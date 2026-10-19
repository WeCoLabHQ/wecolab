package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/sirupsen/logrus"
)

func networkConfig(mac net.HardwareAddr) *types.Configuration {
	return &types.Configuration{
		MTU: 1500, Subnet: "192.168.127.0/24", GatewayIP: "192.168.127.1",
		DeviceIP: "192.168.127.2", GatewayMacAddress: net.HardwareAddr(gatewayMAC[:]).String(),
		DHCPStaticLeases: map[string]string{"192.168.127.2": mac.String()},
		// Never set HostIP, gateway virtual aliases, NAT, port forwards or metadata access.
	}
}

func run(ctx context.Context, args []string, stdin *os.File, ready *os.File) error {
	return runWithParent(ctx, args, stdin, ready, os.Getppid)
}

func checkParent(expected int, current func() int) error {
	if expected <= 1 || current() != expected {
		return errors.New("network worker parent lost")
	}
	return nil
}

func runWithParent(ctx context.Context, args []string, stdin *os.File, ready *os.File, currentParent func() int) error {
	flags := flag.NewFlagSet("wecolab-network", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	macArg := flags.String("mac", "", "guest Ethernet address")
	accessArg := flags.String("access", "", "isolated or localNetwork")
	parentArg := flags.String("parent-pid", "", "owning VM worker PID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*accessArg != string(isolated) && *accessArg != string(localNetwork)) {
		return errors.New("expected --mac <canonical MAC> --access isolated|localNetwork --parent-pid <worker PID>")
	}
	parent, err := strconv.Atoi(*parentArg)
	if err != nil || parent <= 1 || parent > 2147483647 {
		return errors.New("invalid parent PID")
	}
	if err := checkParent(parent, currentParent); err != nil {
		return err
	}
	mac, err := net.ParseMAC(*macArg)
	if err != nil || len(mac) != 6 || mac.String() != *macArg || mac[0]&1 != 0 ||
		bytes.Equal(mac, []byte{0, 0, 0, 0, 0, 0}) {
		return errors.New("invalid canonical unicast MAC")
	}
	var macBytes [6]byte
	copy(macBytes[:], mac)
	raw, err := net.FileConn(stdin)
	if err != nil {
		return fmt.Errorf("fd0 must be a connected Unix datagram socket: %w", err)
	}
	unix, ok := raw.(*net.UnixConn)
	if !ok {
		raw.Close()
		return errors.New("fd0 must be a connected Unix datagram socket")
	}
	defer unix.Close()
	if err := verifyDatagram(unix); err != nil {
		return err
	}
	hosts := new(hostAddresses)
	// Library services may log per guest packet; do not let untrusted guest traffic
	// produce unbounded diagnostics. Startup/transport failures are reported below.
	logrus.SetOutput(io.Discard)
	if err := hosts.refresh(); err != nil {
		return fmt.Errorf("cannot enumerate host interfaces: %w", err)
	}
	if err := checkParent(parent, currentParent); err != nil {
		return err
	}
	network, err := newBackend(networkConfig(mac))
	if err != nil {
		return fmt.Errorf("initialize network: %w", err)
	}
	defer network.Close()
	transport := &filteredConn{UnixConn: unix, mac: macBytes, access: accessMode(*accessArg), hosts: hosts}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- network.AcceptVfkit(runCtx, transport) }()
	// Refresh independently of guest traffic. A stale/failed snapshot blocks all ingress.
	// The Unix datagram peer can die without EOF; Getppid detects reparenting
	// independently. The 250ms watchdog is below the one-second liveness bound.
	refreshTicker := time.NewTicker(time.Second)
	defer refreshTicker.Stop()
	parentTicker := time.NewTicker(250 * time.Millisecond)
	defer parentTicker.Stop()
	if err := checkParent(parent, currentParent); err != nil {
		cancel()
		unix.Close()
		<-finished
		return err
	}
	if _, err := fmt.Fprintln(ready, "READY"); err != nil {
		cancel()
		unix.Close()
		<-finished
		return fmt.Errorf("write READY: %w", err)
	}
	refreshFailed := false
	for {
		select {
		case err := <-finished:
			if err != nil {
				return fmt.Errorf("vfkit transport lost: %w", err)
			}
			return errors.New("vfkit transport closed")
		case <-ctx.Done():
			cancel()
			unix.Close()
			<-finished
			return nil
		case <-parentTicker.C:
			if err := checkParent(parent, currentParent); err != nil {
				cancel()
				unix.Close()
				<-finished
				return err
			}
		case <-refreshTicker.C:
			if err := hosts.refresh(); err != nil {
				if !refreshFailed {
					fmt.Fprintln(os.Stderr, "host interface refresh failed; guest ingress blocked")
				}
				refreshFailed = true
			} else {
				refreshFailed = false
			}
		}
	}
}

func verifyDatagram(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		var kind int
		kind, socketErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_TYPE)
		if socketErr == nil && kind != syscall.SOCK_DGRAM {
			socketErr = errors.New("fd0 must be SOCK_DGRAM")
		}
		if socketErr == nil {
			_, socketErr = syscall.Getpeername(int(fd))
		}
	}); err != nil {
		return err
	}
	return socketErr
}

func main() {
	// VM vCPU admission does not account for host-side backend scheduling.
	// Limit concurrent Go execution; this is not a hard host CPU quota.
	runtime.GOMAXPROCS(1)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
