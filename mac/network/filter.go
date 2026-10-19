package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"time"
)

type accessMode string

const (
	isolated     accessMode = "isolated"
	localNetwork accessMode = "localNetwork"
)

var gatewayMAC = [6]byte{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xee}
var guestIP = netip.MustParseAddr("192.168.127.2")
var gatewayIP = netip.MustParseAddr("192.168.127.1")
var zeroIP = netip.IPv4Unspecified()
var broadcastIP = netip.MustParseAddr("255.255.255.255")

// IPv4 checksums are verified before inspecting destinations. Neither options nor fragmented
// datagrams are supported, so the transport header always starts at byte 34.
func ipv4Checksum(h []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(h); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(h[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func isPrivateLAN(ip netip.Addr) bool {
	b := ip.As4()
	return b[0] == 10 || b[0] == 172 && b[1] >= 16 && b[1] <= 31 || b[0] == 192 && b[1] == 168
}

// Cached addresses/prefixes are defense-in-depth only: polling can race interface
// changes and forwarding. Strict isolation requires an installed, verified OS firewall.
func permittedDestination(ip netip.Addr, mode accessMode, hosts *hostSnapshot) bool {
	if hosts != nil {
		if _, present := hosts.addresses[ip]; present {
			return false
		}
		if mode == isolated {
			for _, prefix := range hosts.prefixes {
				if prefix.Contains(ip) {
					return false
				}
			}
		}
	}
	b := ip.As4()
	if b[0] == 0 || b[0] == 127 || b[0] >= 224 || b[0] == 169 && b[1] == 254 ||
		b[0] == 100 && b[1] >= 64 && b[1] <= 127 || b[0] == 192 && b[1] == 0 && b[2] == 0 ||
		b[0] == 192 && b[1] == 0 && b[2] == 2 || b[0] == 198 && b[1] == 18 || b[0] == 198 && b[1] == 19 ||
		b[0] == 198 && b[1] == 51 && b[2] == 100 || b[0] == 203 && b[1] == 0 && b[2] == 113 ||
		b[0] == 192 && b[1] == 88 && b[2] == 99 || b[0] == 192 && b[1] == 168 && b[2] == 127 {
		return false
	}
	if isPrivateLAN(ip) {
		return mode == localNetwork
	}
	return true
}

func allowedFrame(b []byte, mac [6]byte, mode accessMode, hosts *hostSnapshot) bool {
	if len(b) < 14 || len(b) > 1514 || !bytes.Equal(b[6:12], mac[:]) {
		return false
	}
	broadcast := bytes.Equal(b[:6], []byte{255, 255, 255, 255, 255, 255})
	toGateway := bytes.Equal(b[:6], gatewayMAC[:])
	if !broadcast && !toGateway {
		return false
	}
	etherType := binary.BigEndian.Uint16(b[12:14])
	switch etherType {
	case 0x0806:
		// Only guest-to-gateway ARP requests and confirmations; never relay peer ARP.
		if len(b) != 42 && len(b) != 60 {
			return false
		}
		if binary.BigEndian.Uint16(b[14:16]) != 1 || binary.BigEndian.Uint16(b[16:18]) != 0x0800 ||
			b[18] != 6 || b[19] != 4 || !bytes.Equal(b[22:28], mac[:]) ||
			!bytes.Equal(b[38:42], []byte{192, 168, 127, 1}) {
			return false
		}
		src := netip.AddrFrom4([4]byte(b[28:32]))
		switch binary.BigEndian.Uint16(b[20:22]) {
		case 1:
			return src == guestIP || src == zeroIP
		case 2:
			return toGateway && src == guestIP && bytes.Equal(b[32:38], gatewayMAC[:])
		default:
			return false
		}
	case 0x0800:
	default:
		return false
	}
	if len(b) < 14+20 || b[14] != 0x45 || ipv4Checksum(b[14:34]) != 0 ||
		b[20]&0xbf != 0 || b[21] != 0 {
		return false
	}
	total := int(binary.BigEndian.Uint16(b[16:18]))
	if total < 20 || total+14 != len(b) && !(len(b) == 60 && total+14 < 60) {
		return false
	}
	src := netip.AddrFrom4([4]byte(b[26:30]))
	dst := netip.AddrFrom4([4]byte(b[30:34]))
	proto := b[23]
	if proto != 6 && proto != 17 {
		return false
	}
	n := total - 20
	if n < 8 {
		return false
	}
	sourcePort := binary.BigEndian.Uint16(b[34:36])
	destPort := binary.BigEndian.Uint16(b[36:38])
	if proto == 6 {
		if n < 20 || b[46]>>4 < 5 || int(b[46]>>4)*4 > n {
			return false
		}
	} else if binary.BigEndian.Uint16(b[38:40]) != uint16(n) {
		return false
	}
	if src == zeroIP && dst == broadcastIP && broadcast && proto == 17 && sourcePort == 68 && destPort == 67 {
		return validDHCP(b[42:14+total], mac)
	}
	// DHCP binds wildcard UDP/67 inside gVisor. Reserve that destination port
	// for authenticated gateway/broadcast DHCP even when an external address
	// would otherwise be allowed: those packets cannot reach the host forwarder.
	if proto == 17 && destPort == 67 && dst != gatewayIP && dst != broadcastIP {
		return false
	}
	if src != guestIP {
		return false
	}
	if dst == gatewayIP {
		if !toGateway {
			return false
		}
		if destPort == 53 {
			return true
		}
		return proto == 17 && sourcePort == 68 && destPort == 67 && validDHCP(b[42:14+total], mac)
	}
	if dst == broadcastIP {
		return broadcast && proto == 17 && sourcePort == 68 && destPort == 67 && validDHCP(b[42:14+total], mac)
	}
	return toGateway && permittedDestination(dst, mode, hosts)
}

func validDHCP(payload []byte, mac [6]byte) bool {
	return len(payload) >= 240 && payload[0] == 1 && payload[1] == 1 && payload[2] == 6 &&
		bytes.Equal(payload[28:34], mac[:]) && payload[236] == 99 && payload[237] == 130 && payload[238] == 83 && payload[239] == 99
}

type hostSnapshot struct {
	addresses map[netip.Addr]struct{}
	prefixes  []netip.Prefix
	until     time.Time
}
type hostAddresses struct{ snapshot atomic.Pointer[hostSnapshot] }

func collectInterfaceAddresses(addrs []net.Addr, snapshot *hostSnapshot) error {
	for _, addr := range addrs {
		var ip net.IP
		switch a := addr.(type) {
		case *net.IPNet:
			ip = a.IP
			if ipv4 := ip.To4(); ipv4 != nil {
				bits, size := a.Mask.Size()
				if size != 32 || bits < 0 {
					return &net.AddrError{Err: "invalid IPv4 interface prefix", Addr: addr.String()}
				}
				snapshot.prefixes = append(snapshot.prefixes, netip.PrefixFrom(netip.AddrFrom4([4]byte(ipv4)), bits).Masked())
			}
		case *net.IPAddr:
			ip = a.IP
		default:
			return &net.AddrError{Err: "unsupported interface address", Addr: addr.String()}
		}
		if ipv4 := ip.To4(); ipv4 != nil {
			snapshot.addresses[netip.AddrFrom4([4]byte(ipv4))] = struct{}{}
		}
	}
	return nil
}

func (h *hostAddresses) refresh() error {
	ifaces, err := net.Interfaces()
	if err != nil {
		h.snapshot.Store(nil)
		return err
	}
	snapshot := &hostSnapshot{addresses: make(map[netip.Addr]struct{}), prefixes: make([]netip.Prefix, 0, len(ifaces))}
	for _, iface := range ifaces {
		ips, err := iface.Addrs()
		if err != nil {
			h.snapshot.Store(nil)
			return err
		}
		if err := collectInterfaceAddresses(ips, snapshot); err != nil {
			h.snapshot.Store(nil)
			return err
		}
	}
	snapshot.until = time.Now().Add(2 * time.Second)
	h.snapshot.Store(snapshot)
	return nil
}
func (h *hostAddresses) current() (*hostSnapshot, bool) {
	snapshot := h.snapshot.Load()
	if snapshot == nil || time.Now().After(snapshot.until) {
		return nil, false
	}
	return snapshot, true
}

// DHCP service starts a goroutine for each valid request. Bound ingress before
// dispatch so even a scheduler backlog cannot accumulate unbounded goroutines.
type dhcpLimiter struct {
	last   time.Time
	tokens int
}

func (l *dhcpLimiter) allow(now time.Time) bool {
	if l.last.IsZero() {
		l.last, l.tokens = now, 8
	}
	if elapsed := now.Sub(l.last); elapsed >= 125*time.Millisecond {
		n := int(elapsed / (125 * time.Millisecond))
		l.tokens += n
		if l.tokens > 8 {
			l.tokens = 8
		}
		l.last = l.last.Add(time.Duration(n) * 125 * time.Millisecond)
	}
	if l.tokens == 0 {
		return false
	}
	l.tokens--
	return true
}

// ReadMsgUnix retains datagram boundaries and detects truncation, unlike UnixConn.Read.
// Dropped frames never reach gvisor. The only write path returns stack frames to the VM.
type filteredConn struct {
	*net.UnixConn
	mac    [6]byte
	access accessMode
	hosts  *hostAddresses
	dhcp   dhcpLimiter
}

func (c *filteredConn) Read(buf []byte) (int, error) {
	for {
		n, _, flags, _, err := c.UnixConn.ReadMsgUnix(buf, nil)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		if flags&syscall.MSG_TRUNC != 0 {
			continue
		}
		hosts, fresh := c.hosts.current()
		if !fresh {
			continue
		}
		if allowedFrame(buf[:n], c.mac, c.access, hosts) {
			if buf[12] == 0x08 && buf[13] == 0x00 && buf[23] == 17 &&
				binary.BigEndian.Uint16(buf[34:36]) == 68 && binary.BigEndian.Uint16(buf[36:38]) == 67 &&
				!c.dhcp.allow(time.Now()) {
				continue
			}
			return n, nil
		}
	}
}

// Upstream retries ENOBUFS without a cancellation check; translate that error
// into terminal transport loss instead of allowing a stuck send loop.
func (c *filteredConn) Write(b []byte) (int, error) {
	n, err := c.UnixConn.Write(b)
	if errors.Is(err, syscall.ENOBUFS) {
		return n, errors.New("guest receive buffer full")
	}
	return n, err
}
