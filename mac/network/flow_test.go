package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestUDPResourceAdmissionAndRelease(t *testing.T) {
	sockets, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	guestFile := os.NewFile(uintptr(sockets[0]), "guest")
	backendFile := os.NewFile(uintptr(sockets[1]), "backend")
	defer guestFile.Close()
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
	backend, err := newBackendWithLimits(networkConfig(net.HardwareAddr(guestMAC[:])), 1, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- backend.AcceptVfkit(ctx, backendRaw) }()
	defer func() { cancel(); backendRaw.Close(); <-finished }()
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: nonLoopbackIPv4(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := uint16(listener.LocalAddr().(*net.UDPAddr).Port)
	first := udpPayloadFrame(listener.LocalAddr().(*net.UDPAddr).IP.String(), 40001, port)
	second := udpPayloadFrame(listener.LocalAddr().(*net.UDPAddr).IP.String(), 40002, port)
	if _, err := guestRaw.Write(first); err != nil {
		t.Fatal(err)
	}
	waitFlows(t, backend.flows, 1)
	if _, err := guestRaw.Write(second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if active := backend.flows.active(); active != 1 {
		t.Fatalf("capacity exceeded: %d", active)
	}
	waitFlows(t, backend.flows, 0)
	if _, err := guestRaw.Write(second); err != nil {
		t.Fatal(err)
	}
	waitFlows(t, backend.flows, 1)
}

func udpPayloadFrame(destination string, sourcePort, destinationPort uint16) []byte {
	b := frame("192.168.127.2", destination, 17, sourcePort, destinationPort)
	b = append(b, 1)
	binary.BigEndian.PutUint16(b[16:18], 29)
	binary.BigEndian.PutUint16(b[38:40], 9)
	return recalculateChecksum(b)
}

func nonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		for _, addr := range addrs {
			subnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := subnet.IP.To4()
			if ip != nil && !ip.IsLinkLocalUnicast() {
				return ip
			}
		}
	}
	t.Skip("no active non-loopback IPv4 host interface for real UDP flow")
	return nil
}

func waitFlows(t *testing.T, budget *flowBudget, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if budget.active() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("active flows=%d, want %d", budget.active(), want)
}

func TestUDPHostSocketBuffersBounded(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := configureHostBuffers(conn, 64<<10); err != nil {
		t.Fatal(err)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Control(func(fd uintptr) {
		for _, option := range []int{syscall.SO_RCVBUF, syscall.SO_SNDBUF} {
			size, getErr := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, option)
			if getErr != nil {
				t.Error(getErr)
				continue
			}
			if size < 64<<10 || size > 256<<10 {
				t.Errorf("socket buffer %d out of 64-256KiB bound: %d", option, size)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUDPReceiverFailureClosesFlowDespiteGuestTraffic(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	outbound, err := net.DialUDP("udp4", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	frontend, guest := net.Pipe()
	defer guest.Close()
	budget := newFlowBudget(1)
	release, ok := budget.acquire()
	if !ok {
		t.Fatal("UDP flow not admitted")
	}
	b := &backend{udpIdle: 5 * time.Second}
	done := make(chan struct{})
	go func() { b.proxyUDP(context.Background(), frontend, outbound, release); close(done) }()
	if _, err := guest.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var packet [1]byte
	listener.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := listener.ReadFromUDP(packet[:]); err != nil {
		t.Fatal(err)
	}
	outbound.Close() // a real terminal socket error in the host-receive pump
	guest.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := guest.Read(packet[:]); err == nil {
		t.Fatal("failed host receiver left frontend open")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("UDP sender failed to stop after receiver error")
	}
	if budget.active() != 0 {
		t.Fatal("failed UDP flow retained slot")
	}
}

func TestTCPHalfClosePreservesGuestUpload(t *testing.T) {
	guestListener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer guestListener.Close()
	guest, err := net.DialTCP("tcp4", nil, guestListener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	frontend, err := guestListener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	hostListener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer hostListener.Close()
	outbound, err := net.DialTCP("tcp4", nil, hostListener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	host, err := hostListener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	guest.SetDeadline(time.Now().Add(3 * time.Second))
	host.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan struct{})
	go func() { proxyTCP(context.Background(), frontend, outbound); close(done) }()
	if _, err := host.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	if err := host.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var greeting [5]byte
	if _, err := io.ReadFull(guest, greeting[:]); err != nil {
		t.Fatal(err)
	}
	if string(greeting[:]) != "ready" {
		t.Fatalf("greeting=%q", greeting)
	}
	if _, err := guest.Write([]byte("complete-upload")); err != nil {
		t.Fatal(err)
	}
	if err := guest.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	upload, err := io.ReadAll(host)
	if err != nil {
		t.Fatal(err)
	}
	if string(upload) != "complete-upload" {
		t.Fatalf("upload truncated: %q", upload)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("TCP proxy did not finish after both half-closes")
	}
}

func TestDHCPHandlerAdmissionTracksCompletion(t *testing.T) {
	budget := newFlowBudget(1)
	entered, resume := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	handler := budgetedDHCPHandler(func(net.PacketConn, net.Addr, *dhcpv4.DHCPv4) {
		calls.Add(1)
		close(entered)
		<-resume
	}, budget)
	done := make(chan struct{})
	go func() { handler(nil, nil, nil); close(done) }()
	<-entered
	handler(nil, nil, nil)
	if calls.Load() != 1 || budget.active() != 1 {
		t.Fatal("DHCP handler escaped capacity limit")
	}
	close(resume)
	<-done
	if budget.active() != 0 {
		t.Fatal("DHCP handler retained admission after completion")
	}
}

func TestEmptyUDPDatagramsConsumeReceiveBudget(t *testing.T) {
	b, err := newBackend(networkConfig(net.HardwareAddr(guestMAC[:])))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var queue waiter.Queue
	receiver, terr := b.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &queue)
	if terr != nil {
		t.Fatal(terr)
	}
	defer receiver.Close()
	if terr := receiver.Bind(tcpip.FullAddress{NIC: 1, Port: 50000}); terr != nil {
		t.Fatal(terr)
	}
	frame := frame("192.168.127.2", "1.1.1.1", 17, 40000, 50000)
	if !allowedFrame(frame, guestMAC, localNetwork, nil) {
		t.Fatal("valid empty UDP datagrams must remain supported")
	}
	id := stack.TransportEndpointID{
		LocalAddress:  tcpip.AddrFrom4([4]byte{1, 1, 1, 1}),
		LocalPort:     50000,
		RemoteAddress: tcpip.AddrFrom4([4]byte{192, 168, 127, 2}),
		RemotePort:    40000,
	}
	inject := func() int {
		packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame[14:])})
		defer packet.DecRef()
		packet.NetworkProtocolNumber = ipv4.ProtocolNumber
		packet.TransportProtocolNumber = udp.ProtocolNumber
		packet.NetworkHeader().Consume(20)
		packet.TransportHeader().Consume(8)
		receiver.(stack.TransportEndpoint).HandlePacket(id, packet)
		return packet.MemSize()
	}
	const attempts = 4096
	var cost int
	// The application deliberately does not read until the entire burst has arrived.
	for range attempts {
		cost = inject()
	}
	dropped := b.stack.Stats().UDP.ReceiveBufferErrors.Value()
	var sink bytes.Buffer
	accepted := 0
	for {
		result, err := receiver.Read(&sink, tcpip.ReadOptions{})
		if _, blocked := err.(*tcpip.ErrWouldBlock); blocked {
			break
		}
		if err != nil || result.Count != 0 {
			t.Fatalf("empty UDP read: result=%+v error=%v", result, err)
		}
		accepted++
	}
	if dropped == 0 || accepted == 0 || uint64(accepted)+dropped != attempts {
		t.Fatalf("receive admission: accepted=%d dropped=%d total=%d", accepted, dropped, attempts)
	}
	if int64(accepted*cost) > receiver.SocketOptions().GetReceiveBufferSize()+int64(cost) {
		t.Fatalf("empty datagrams exceeded the configured receive memory budget: %d packets", accepted)
	}
	inject()
	if _, err := receiver.Read(&sink, tcpip.ReadOptions{}); err != nil {
		t.Fatalf("draining did not release receive budget: %v", err)
	}
}
