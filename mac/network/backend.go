package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// One shared budget covers TCP/UDP forwarding, DNS queries/connections, and
// DHCP handlers. 64 flows may hold up to 256KiB per TCP direction in gVisor,
// plus two 32KiB TCP copy buffers. UDP buffers hold complete datagrams.
const maxHostFlows = 64
const proxyPacketSize = 65535
const tcpBufferLimit = 256 << 10
const udpIdleTimeout = 90 * time.Second
const dnsBacklog = 16

type flowBudget struct{ slots chan struct{} }

func newFlowBudget(capacity int) *flowBudget {
	return &flowBudget{slots: make(chan struct{}, capacity)}
}
func (b *flowBudget) acquire() (func(), bool) {
	select {
	case b.slots <- struct{}{}:
		return func() { <-b.slots }, true
	default:
		return nil, false
	}
}
func (b *flowBudget) active() int { return len(b.slots) }

type backend struct {
	stack    *stack.Stack
	switcher *tap.Switch
	flows    *flowBudget
	udpIdle  time.Duration
	cancel   context.CancelFunc
	dnsUDP   net.PacketConn
	dnsTCP   net.Listener
	dhcp     *dhcp.Server
}

func newBackend(config *types.Configuration) (*backend, error) {
	return newBackendWithLimits(config, maxHostFlows, udpIdleTimeout)
}

// Build on maintained gVisor/tap/DHCP/DNS APIs instead of VirtualNetwork.New:
// its stack and transport handlers are private and cannot support flow admission.
func newBackendWithLimits(config *types.Configuration, capacity int, idle time.Duration) (_ *backend, err error) {
	pool, err := tap.NewIPPool(config.Subnet)
	if err != nil {
		return nil, err
	}
	if err := pool.Reserve(config.GatewayIP, config.GatewayMacAddress); err != nil {
		return nil, err
	}
	for ip, mac := range config.DHCPStaticLeases {
		if err := pool.Reserve(ip, mac); err != nil {
			return nil, err
		}
	}
	link, err := tap.NewLinkEndpoint(false, uint32(config.MTU), config.GatewayMacAddress, config.GatewayIP, nil)
	if err != nil {
		return nil, err
	}
	sw := tap.NewSwitch(false)
	link.Connect(sw)
	sw.Connect(link)
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	b := &backend{stack: s, switcher: sw, flows: newFlowBudget(capacity), udpIdle: idle}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	defer func() {
		if err != nil {
			b.Close()
		}
	}()
	if terr := s.CreateNIC(1, link); terr != nil {
		return nil, fmt.Errorf("create NIC: %s", terr)
	}
	ip := net.ParseIP(config.GatewayIP).To4()
	if ip == nil {
		return nil, errors.New("invalid gateway IP")
	}
	address := tcpip.AddrFrom4Slice(ip)
	if terr := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: address.WithPrefix()}, stack.AddressProperties{}); terr != nil {
		return nil, fmt.Errorf("configure gateway: %s", terr)
	}
	s.SetSpoofing(1, true)
	s.SetPromiscuousMode(1, true)
	_, cidr, err := net.ParseCIDR(config.Subnet)
	if err != nil {
		return nil, err
	}
	subnet, terr := tcpip.NewSubnet(tcpip.AddrFromSlice(cidr.IP), tcpip.MaskFromBytes(cidr.Mask))
	if terr != nil {
		return nil, fmt.Errorf("configure subnet: %s", terr)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: 1}})
	// TCP autogrowth and per-endpoint buffer limits are separate from the number
	// of flows. Never let an admitted flow grow to the upstream 4MiB ceiling.
	send := tcpip.TCPSendBufferSizeRangeOption{Min: 4096, Default: 64 << 10, Max: tcpBufferLimit}
	receive := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4096, Default: 64 << 10, Max: tcpBufferLimit}
	if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &send); terr != nil {
		return nil, fmt.Errorf("TCP send buffer: %s", terr)
	}
	if terr := s.SetTransportProtocolOption(tcp.ProtocolNumber, &receive); terr != nil {
		return nil, fmt.Errorf("TCP receive buffer: %s", terr)
	}
	if terr := s.SetOption(tcpip.SendBufferSizeOption{Min: 4096, Default: 32 << 10, Max: 64 << 10}); terr != nil {
		return nil, fmt.Errorf("UDP send buffer: %s", terr)
	}
	if terr := s.SetOption(tcpip.ReceiveBufferSizeOption{Min: 4096, Default: 32 << 10, Max: 64 << 10}); terr != nil {
		return nil, fmt.Errorf("UDP receive buffer: %s", terr)
	}
	tcpForwarder := tcp.NewForwarder(s, 64<<10, capacity, func(r *tcp.ForwarderRequest) { b.forwardTCP(ctx, r) })
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)
	udpForwarder := udp.NewForwarder(s, func(r *udp.ForwarderRequest) bool { return b.forwardUDP(ctx, r) })
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)
	dnsUDP, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: 1, Addr: address, Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	b.dnsUDP = dnsUDP
	dnsTCP, err := listenDNS(s, address, b.flows)
	if err != nil {
		return nil, err
	}
	b.dnsTCP = dnsTCP
	dnsServer, err := dns.New(newDNSPacketConn(dnsUDP, b.flows), dnsTCP, config.DNS)
	if err != nil {
		return nil, err
	}
	dhcpServer, err := dhcp.New(config, s, pool)
	if err != nil {
		return nil, err
	}
	b.dhcp = dhcpServer
	dhcpServer.Underlying.Handler = budgetedDHCPHandler(dhcpServer.Underlying.Handler, b.flows)
	go dnsServer.Serve()
	go dnsServer.ServeTCP()
	go dhcpServer.Serve()
	return b, nil
}

// gonet.ListenTCP hardcodes a 4096-entry backlog, which would allocate many
// gVisor TCP endpoints before the DNS admission wrapper's Accept can run.
func listenDNS(s *stack.Stack, address tcpip.Address, budget *flowBudget) (*dnsListener, error) {
	var wq waiter.Queue
	ep, terr := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		return nil, errors.New(terr.String())
	}
	if terr = ep.Bind(tcpip.FullAddress{NIC: 1, Addr: address, Port: 53}); terr != nil {
		ep.Close()
		return nil, errors.New(terr.String())
	}
	if terr = ep.Listen(dnsBacklog); terr != nil {
		ep.Close()
		return nil, errors.New(terr.String())
	}
	return &dnsListener{
		Listener: gonet.NewTCPListener(s, &wq, ep),
		endpoint: ep,
		queue:    &wq,
		budget:   budget,
		closed:   make(chan struct{}),
	}, nil
}

func budgetedDHCPHandler(handler server4.Handler, budget *flowBudget) server4.Handler {
	return func(conn net.PacketConn, peer net.Addr, message *dhcpv4.DHCPv4) {
		release, ok := budget.acquire()
		if !ok {
			return
		}
		defer release()
		handler(conn, peer, message)
	}
}

func (b *backend) AcceptVfkit(ctx context.Context, conn net.Conn) error {
	return b.switcher.Accept(ctx, conn, types.VfkitProtocol)
}
func (b *backend) Close() error {
	b.cancel()
	if b.dhcp != nil {
		b.dhcp.Underlying.Close()
	}
	if b.dnsTCP != nil {
		b.dnsTCP.Close()
	}
	if b.dnsUDP != nil {
		b.dnsUDP.Close()
	}
	if b.stack != nil {
		b.stack.Close()
	}
	return nil
}

func hostAddress(id stack.TransportEndpointID) string {
	return net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))
}

type hostBufferSocket interface {
	SetReadBuffer(int) error
	SetWriteBuffer(int) error
}

func configureHostBuffers(conn hostBufferSocket, bytes int) error {
	if err := conn.SetReadBuffer(bytes); err != nil {
		return err
	}
	return conn.SetWriteBuffer(bytes)
}

func (b *backend) forwardTCP(ctx context.Context, r *tcp.ForwarderRequest) {
	release, ok := b.flows.acquire()
	if !ok {
		r.Complete(true)
		return
	}
	retained := false
	defer func() {
		if !retained {
			release()
		}
	}()
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	outbound, err := (&net.Dialer{}).DialContext(dialCtx, "tcp4", hostAddress(r.ID()))
	cancel()
	if err != nil {
		r.Complete(true)
		return
	}
	defer outbound.Close()
	socket, ok := outbound.(*net.TCPConn)
	if !ok {
		r.Complete(true)
		return
	}
	if err := configureHostBuffers(socket, 64<<10); err != nil {
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	r.Complete(terr != nil)
	if terr != nil {
		return
	}
	frontend := gonet.NewTCPConn(&wq, ep)
	// Close only starts gVisor's graceful FIN_WAIT/TIME_WAIT state machine.
	// Keep the slot until terminal cleanup removes the endpoint.
	retained = true
	proxyTCP(ctx, frontend, socket)
	go waitTCPTerminal(ep, release)
}

func waitTCPTerminal(ep tcpip.Endpoint, release func()) {
	defer release()
	tcpEP, ok := ep.(*tcp.Endpoint)
	if !ok {
		ep.Abort()
		return
	}
	for {
		state := tcp.EndpointState(tcpEP.State())
		if state == tcp.StateClose || state == tcp.StateError {
			return
		}
		tcpEP.Wait()
	}
}

type tcpHalfConn interface {
	net.Conn
	CloseWrite() error
}

func proxyTCP(ctx context.Context, frontend, outbound tcpHalfConn) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { frontend.Close(); outbound.Close() }) }
	defer closeBoth()
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-ctx.Done():
			closeBoth()
		case <-sessionDone:
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		if _, err := io.CopyBuffer(outbound, frontend, buf); err != nil {
			closeBoth()
			return
		}
		if err := outbound.CloseWrite(); err != nil {
			closeBoth()
		}
	}()
	buf := make([]byte, 32<<10)
	if _, err := io.CopyBuffer(frontend, outbound, buf); err != nil {
		closeBoth()
	} else if err := frontend.CloseWrite(); err != nil {
		closeBoth()
	}
	// EOF means a half-close, not a full close. Wait for the opposite direction
	// to finish naturally unless a real I/O error or cancellation closes both.
	<-done
}

func (b *backend) forwardUDP(ctx context.Context, r *udp.ForwarderRequest) bool {
	release, ok := b.flows.acquire()
	if !ok {
		return false
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		release()
		return false
	}
	frontend := gonet.NewUDPConn(&wq, ep)
	addr, err := net.ResolveUDPAddr("udp4", hostAddress(r.ID()))
	if err != nil {
		frontend.Close()
		release()
		return false
	}
	outbound, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		frontend.Close()
		release()
		return false
	}
	if err := configureHostBuffers(outbound, 64<<10); err != nil {
		outbound.Close()
		frontend.Close()
		release()
		return false
	}
	go b.proxyUDP(ctx, frontend, outbound, release)
	return true
}

func (b *backend) proxyUDP(ctx context.Context, frontend net.Conn, outbound *net.UDPConn, release func()) {
	defer release()
	defer frontend.Close()
	defer outbound.Close()
	done := make(chan struct{})
	go func() {
		// If either direction fails, stop both pumps. Otherwise a guest
		// continuously sending can keep a deaf receive-side mapping alive.
		defer close(done)
		defer outbound.Close()
		defer frontend.Close()
		var buf [proxyPacketSize]byte
		for {
			outbound.SetReadDeadline(time.Now().Add(b.udpIdle))
			n, _, flags, _, err := outbound.ReadMsgUDP(buf[:], nil)
			if err != nil {
				return
			}
			if flags&syscall.MSG_TRUNC != 0 {
				continue
			}
			frontend.SetReadDeadline(time.Now().Add(b.udpIdle))
			if _, err := frontend.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	var buf [proxyPacketSize]byte
	for {
		frontend.SetReadDeadline(time.Now().Add(b.udpIdle))
		n, err := frontend.Read(buf[:])
		if err != nil {
			break
		}
		outbound.SetReadDeadline(time.Now().Add(b.udpIdle))
		if _, err := outbound.Write(buf[:n]); err != nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	frontend.Close()
	outbound.Close()
	<-done
}
