package main

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRejectInvalidLaunchBeforeReady(t *testing.T) {
	cases := [][]string{
		nil, {"--mac", "02:00:00:00:00:02", "--access", "default", "--parent-pid", "42"},
		{"--mac", "02:00:00:00:00:0Z", "--access", "isolated", "--parent-pid", "42"},
		{"--mac", "ff:ff:ff:ff:ff:ff", "--access", "isolated", "--parent-pid", "42"},
		{"--mac", "00:00:00:00:00:00", "--access", "isolated", "--parent-pid", "42"},
		{"--mac", "02:00:00:00:00:02", "--access", "isolated", "--parent-pid", "42", "extra"},
		{"--mac", "02:00:00:00:00:02", "--access", "isolated"},
		{"--mac", "02:00:00:00:00:02", "--access", "isolated", "--parent-pid", "0"},
		{"--mac", "02:00:00:00:00:02", "--access", "isolated", "--parent-pid", "-1"},
		{"--mac", "02:00:00:00:00:02", "--access", "isolated", "--parent-pid", "nope"},
		{"--mac", "02:00:00:00:00:02", "--access", "isolated", "--parent-pid", "2147483648"},
		{"--mac", "02:00:00:00:00:02", "--access", "isolated", "--parent-pid", "1"},
	}
	for _, args := range cases {
		t.Run("invalid", func(t *testing.T) {
			input, err := os.CreateTemp(t.TempDir(), "invalid-fd")
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := os.CreateTemp(t.TempDir(), "ready")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			if run(context.Background(), args, input, output) == nil {
				t.Fatal("invalid launch accepted")
			}
			size, err := output.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if size.Size() != 0 {
				t.Fatal("READY emitted on failure")
			}
		})
	}
}

func TestParentLiveness(t *testing.T) {
	expected := os.Getppid()
	if err := checkParent(expected, func() int { return expected }); err != nil {
		t.Fatal(err)
	}
	if err := checkParent(expected, func() int { return 1 }); err == nil {
		t.Fatal("reparented backend accepted")
	}
}

func TestParentLossStopsBackend(t *testing.T) {
	sockets, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	guestFile := os.NewFile(uintptr(sockets[0]), "guest")
	backendFile := os.NewFile(uintptr(sockets[1]), "backend")
	defer guestFile.Close()
	defer backendFile.Close()
	ready, err := os.CreateTemp(t.TempDir(), "ready")
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	parent := os.Getppid()
	var observed atomic.Int64
	observed.Store(int64(parent))
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- runWithParent(ctx, []string{"--mac", "02:00:00:00:00:02", "--access", "isolated", "--parent-pid", strconv.Itoa(parent)}, backendFile, ready, func() int { return int(observed.Load()) })
	}()
	deadline := time.After(5 * time.Second)
	for {
		stat, err := ready.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if stat.Size() > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("backend exited before READY: %v", err)
		case <-deadline:
			t.Fatal("backend did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	observed.Store(1)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("parent loss reported clean exit")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("backend stayed alive after parent loss")
	}
}

func TestDHCPIngressBurstAndRefill(t *testing.T) {
	var limiter dhcpLimiter
	now := time.Unix(12345, 0)
	for i := range 8 {
		if !limiter.allow(now) {
			t.Fatalf("initial request %d rejected", i)
		}
	}
	if limiter.allow(now) {
		t.Fatal("unbounded DHCP ingress")
	}
	if !limiter.allow(now.Add(125 * time.Millisecond)) {
		t.Fatal("DHCP token not replenished")
	}
	if limiter.allow(now.Add(125 * time.Millisecond)) {
		t.Fatal("DHCP refill exceeded")
	}
}

func TestHostSnapshotRefreshAndExpiry(t *testing.T) {
	hosts := new(hostAddresses)
	address := netip.MustParseAddr("8.8.8.8")
	hosts.snapshot.Store(&hostSnapshot{addresses: map[netip.Addr]struct{}{address: {}}, until: time.Now().Add(time.Minute)})
	addresses, fresh := hosts.current()
	if !fresh || permittedDestination(address, localNetwork, addresses) {
		t.Fatal("host address allowed")
	}
	hosts.snapshot.Store(&hostSnapshot{addresses: map[netip.Addr]struct{}{}, until: time.Now().Add(time.Minute)})
	addresses, fresh = hosts.current()
	if !fresh || !permittedDestination(address, localNetwork, addresses) {
		t.Fatal("updated address set not observed")
	}
	hosts.snapshot.Store(&hostSnapshot{addresses: map[netip.Addr]struct{}{}, until: time.Now().Add(-time.Second)})
	if _, fresh := hosts.current(); fresh {
		t.Fatal("stale address snapshot accepted")
	}
}

func TestTruncatedDatagramNeverAccepted(t *testing.T) {
	sockets, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	guestFile := os.NewFile(uintptr(sockets[0]), "guest")
	defer guestFile.Close()
	backendFile := os.NewFile(uintptr(sockets[1]), "backend")
	defer backendFile.Close()
	guestRaw, err := net.FileConn(guestFile)
	if err != nil {
		t.Fatal(err)
	}
	defer guestRaw.Close()
	backendRaw, err := net.FileConn(backendFile)
	if err != nil {
		t.Fatal(err)
	}
	defer backendRaw.Close()
	hosts := new(hostAddresses)
	hosts.snapshot.Store(&hostSnapshot{addresses: map[netip.Addr]struct{}{}, until: time.Now().Add(time.Minute)})
	conn := &filteredConn{UnixConn: backendRaw.(*net.UnixConn), mac: guestMAC, access: isolated, hosts: hosts}
	guest := guestRaw.(*net.UnixConn)
	good := frame("192.168.127.2", "8.8.8.8", 17, 4242, 4242)
	if _, err := guest.Write(append(append([]byte(nil), good...), make([]byte, 128)...)); err != nil {
		t.Fatal(err)
	}
	if _, err := guest.Write(good); err != nil {
		t.Fatal(err)
	}
	backendRaw.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, len(good))
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(good) {
		t.Fatalf("accepted truncated datagram: %d", n)
	}
}
