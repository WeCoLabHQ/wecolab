package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	dnswire "github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Upstream dns.Server starts a goroutine for every UDP datagram. Reserve a
// shared slot before it sees a valid query, then release when its actual reply
// is written. An upstream resolver which never returns retains its slot, and
// exhaustion drops subsequent queries instead of growing goroutines/sockets.
// Only complete standard queries are passed through: miekg/dns ignores QR
// replies without writing a response, which otherwise would strand a slot.
type dnsKey struct {
	remote string
	id     uint16
}
type dnsPacketConn struct {
	net.PacketConn
	budget  *flowBudget
	mu      sync.Mutex
	pending map[dnsKey][]func()
}

func newDNSPacketConn(conn net.PacketConn, budget *flowBudget) *dnsPacketConn {
	return &dnsPacketConn{PacketConn: conn, budget: budget, pending: make(map[dnsKey][]func())}
}
func (c *dnsPacketConn) ReadFrom(buf []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(buf)
		if err != nil {
			return n, addr, err
		}
		if n < 12 {
			continue
		}
		var request dnswire.Msg
		if err := request.Unpack(buf[:n]); err != nil || request.Response {
			continue
		}
		release, ok := c.budget.acquire()
		if !ok {
			continue
		}
		key := dnsKey{remote: addr.String(), id: binary.BigEndian.Uint16(buf[:2])}
		c.mu.Lock()
		c.pending[key] = append(c.pending[key], release)
		c.mu.Unlock()
		return n, addr, nil
	}
}
func (c *dnsPacketConn) WriteTo(buf []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(buf, addr)
	if len(buf) < 2 {
		return n, err
	}
	key := dnsKey{remote: addr.String(), id: binary.BigEndian.Uint16(buf[:2])}
	c.mu.Lock()
	releases := c.pending[key]
	var release func()
	if len(releases) != 0 {
		release = releases[0]
		if len(releases) == 1 {
			delete(c.pending, key)
		} else {
			c.pending[key] = releases[1:]
		}
	}
	c.mu.Unlock()
	if release != nil {
		release()
	}
	return n, err
}

// Accept needs the actual gVisor endpoint: gonet.TCPListener.Accept hides it
// behind net.Conn, making graceful FIN_WAIT/TIME_WAIT teardown unaccountable.
type dnsListener struct {
	net.Listener
	endpoint tcpip.Endpoint
	queue    *waiter.Queue
	budget   *flowBudget
	closed   chan struct{}
	once     sync.Once
}

func (l *dnsListener) Accept() (net.Conn, error) {
	entry, ready := waiter.NewChannelEntry(waiter.ReadableEvents)
	l.queue.EventRegister(&entry)
	defer l.queue.EventUnregister(&entry)
	for {
		ep, queue, terr := l.endpoint.Accept(nil)
		if terr == nil {
			release, ok := l.budget.acquire()
			if !ok {
				// Rejected DNS connections have sent no user data. Only these
				// may abort immediately, avoiding unaccounted FIN_WAIT_2.
				ep.SocketOptions().SetLinger(tcpip.LingerOption{Enabled: true})
				ep.Close()
				continue
			}
			return &dnsConn{Conn: gonet.NewTCPConn(queue, ep), endpoint: ep, release: release}, nil
		}
		if _, ok := terr.(*tcpip.ErrWouldBlock); !ok {
			return nil, fmt.Errorf("DNS accept: %s", terr)
		}
		select {
		case <-l.closed:
			return nil, net.ErrClosed
		case <-ready:
		}
	}
}

func (l *dnsListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

type dnsConn struct {
	net.Conn
	once     sync.Once
	endpoint tcpip.Endpoint
	release  func()
}

func (c *dnsConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { go waitTCPTerminal(c.endpoint, c.release) })
	return err
}
