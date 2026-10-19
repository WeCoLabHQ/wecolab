package main

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
)

var guestMAC = [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02}

func arpFrame(op uint16, target string) []byte {
	b := make([]byte, 42)
	copy(b[:6], gatewayMAC[:])
	copy(b[6:12], guestMAC[:])
	binary.BigEndian.PutUint16(b[12:14], 0x0806)
	binary.BigEndian.PutUint16(b[14:16], 1)
	binary.BigEndian.PutUint16(b[16:18], 0x0800)
	b[18], b[19] = 6, 4
	binary.BigEndian.PutUint16(b[20:22], op)
	copy(b[22:28], guestMAC[:])
	copy(b[28:32], []byte{192, 168, 127, 2})
	if op == 2 {
		copy(b[32:38], gatewayMAC[:])
	}
	ip := netip.MustParseAddr(target).As4()
	copy(b[38:42], ip[:])
	return b
}

func TestGuestARPConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
		want   bool
	}{
		{"gateway reply", nil, true},
		{"peer reply", func(b []byte) { b[41] = 3 }, false},
		{"broadcast reply", func(b []byte) {
			for i := range 6 {
				b[i] = 255
			}
		}, false},
		{"spoofed sender", func(b []byte) { b[31] = 3 }, false},
		{"spoofed target MAC", func(b []byte) { b[32] = 1 }, false},
		{"unknown operation", func(b []byte) { b[21] = 3 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := arpFrame(2, "192.168.127.1")
			if tc.mutate != nil {
				tc.mutate(b)
			}
			if got := allowedFrame(b, guestMAC, isolated, nil); got != tc.want {
				t.Fatalf("accepted=%v want %v", got, tc.want)
			}
		})
	}
}

func TestAttachedGlobalLANPrefix(t *testing.T) {
	hosts := &hostSnapshot{
		addresses: map[netip.Addr]struct{}{netip.MustParseAddr("11.22.33.4"): {}},
		prefixes:  []netip.Prefix{netip.MustParsePrefix("11.22.33.0/24")},
	}
	peer := frame("192.168.127.2", "11.22.33.5", 17, 4242, 4242)
	if allowedFrame(peer, guestMAC, isolated, hosts) {
		t.Fatal("attached global LAN permitted in isolated mode")
	}
	if !allowedFrame(peer, guestMAC, localNetwork, hosts) {
		t.Fatal("trusted LAN peer rejected")
	}
	if allowedFrame(frame("192.168.127.2", "11.22.33.4", 17, 4242, 4242), guestMAC, localNetwork, hosts) {
		t.Fatal("host address permitted")
	}
	if !allowedFrame(frame("192.168.127.2", "11.22.34.5", 17, 4242, 4242), guestMAC, isolated, hosts) {
		t.Fatal("unattached public destination rejected")
	}
}

func TestInterfacePrefixCollection(t *testing.T) {
	ip := netip.MustParseAddr("11.22.33.4")
	hosts := &hostSnapshot{addresses: make(map[netip.Addr]struct{})}
	if err := collectInterfaceAddresses([]net.Addr{
		&net.IPNet{IP: net.ParseIP(ip.String()).To4(), Mask: net.CIDRMask(24, 32)},
	}, hosts); err != nil {
		t.Fatal(err)
	}
	if _, ok := hosts.addresses[ip]; !ok {
		t.Fatal("interface IP omitted")
	}
	if len(hosts.prefixes) != 1 || hosts.prefixes[0] != netip.MustParsePrefix("11.22.33.0/24") {
		t.Fatalf("interface prefix omitted: %v", hosts.prefixes)
	}
}

func frame(src, dst string, proto byte, sourcePort, destPort uint16) []byte {
	transportSize := 8
	if proto == 6 {
		transportSize = 20
	}
	b := make([]byte, 14+20+transportSize)
	copy(b[:6], gatewayMAC[:])
	copy(b[6:12], guestMAC[:])
	binary.BigEndian.PutUint16(b[12:14], 0x0800)
	ip := b[14:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+transportSize))
	ip[8] = 64
	ip[9] = proto
	a := netip.MustParseAddr(src).As4()
	copy(ip[12:16], a[:])
	a = netip.MustParseAddr(dst).As4()
	copy(ip[16:20], a[:])
	binary.BigEndian.PutUint16(ip[20:22], sourcePort)
	binary.BigEndian.PutUint16(ip[22:24], destPort)
	if proto == 6 {
		ip[32] = 0x50
	} else {
		binary.BigEndian.PutUint16(ip[24:26], 8)
	}
	binary.BigEndian.PutUint16(ip[10:12], ipv4Checksum(ip[:20]))
	return b
}
func recalculateChecksum(b []byte) []byte {
	b[24] = 0
	b[25] = 0
	binary.BigEndian.PutUint16(b[24:26], ipv4Checksum(b[14:34]))
	return b
}

func TestBoundary(t *testing.T) {
	cases := []struct {
		name, dst       string
		protocol        byte
		port            uint16
		isolated, local bool
	}{
		{"public HTTPS", "1.1.1.1", 6, 443, true, true},
		{"public Nebula UDP", "8.8.8.8", 17, 4242, true, true},
		{"private LAN", "10.20.30.40", 6, 443, false, true},
		{"other RFC1918", "172.16.30.40", 17, 4242, false, true},
		{"loopback", "127.0.0.1", 6, 80, false, false},
		{"link local metadata", "169.254.169.254", 6, 80, false, false},
		{"cloud metadata", "100.100.100.200", 6, 80, false, false},
		{"CGNAT", "100.64.1.1", 17, 4242, false, false},
		{"multicast", "224.0.0.1", 17, 53, false, false},
		{"documentation", "192.0.2.1", 6, 80, false, false},
		{"subnet peer", "192.168.127.3", 6, 80, false, false},
		{"gateway HTTP", "192.168.127.1", 6, 80, false, false},
		{"gateway DNS UDP", "192.168.127.1", 17, 53, true, true},
		{"gateway DNS TCP", "192.168.127.1", 6, 53, true, true},
		{"host public address", "8.8.8.8", 6, 443, false, false},
		{"host private address", "10.20.30.40", 6, 443, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hosts := &hostSnapshot{addresses: map[netip.Addr]struct{}{}}
			if tc.name == "host public address" || tc.name == "host private address" {
				hosts.addresses[netip.MustParseAddr(tc.dst)] = struct{}{}
			}
			p := frame("192.168.127.2", tc.dst, tc.protocol, 40000, tc.port)
			for _, mode := range []struct {
				access accessMode
				want   bool
			}{{isolated, tc.isolated}, {localNetwork, tc.local}} {
				if got := allowedFrame(p, guestMAC, mode.access, hosts); got != mode.want {
					t.Errorf("mode %s: allowed=%v want %v", mode.access, got, mode.want)
				}
			}
		})
	}
}

func TestMalformedAndBypassFrames(t *testing.T) {
	base := frame("192.168.127.2", "1.1.1.1", 17, 40000, 4242)
	variants := map[string]func([]byte) []byte{
		"short Ethernet":      func(b []byte) []byte { return b[:13] },
		"short IP":            func(b []byte) []byte { return b[:30] },
		"trailing bytes":      func(b []byte) []byte { return append(b, 0) },
		"wrong source MAC":    func(b []byte) []byte { b[6] ^= 1; return b },
		"spoofed source IP":   func(b []byte) []byte { b[26] = 10; return recalculateChecksum(b) },
		"VLAN":                func(b []byte) []byte { b[12] = 0x81; return b },
		"IPv6":                func(b []byte) []byte { b[12] = 0x86; b[13] = 0xdd; return b },
		"IP options":          func(b []byte) []byte { b[14] = 0x46; return recalculateChecksum(b) },
		"fragment offset":     func(b []byte) []byte { b[21] = 1; return recalculateChecksum(b) },
		"more fragments":      func(b []byte) []byte { b[20] = 0x20; return recalculateChecksum(b) },
		"unsupported ICMP":    func(b []byte) []byte { b[23] = 1; return recalculateChecksum(b) },
		"invalid checksum":    func(b []byte) []byte { b[24] ^= 1; return b },
		"truncated transport": func(b []byte) []byte { return b[:len(b)-1] },
		"broadcast to public": func(b []byte) []byte {
			for i := range 6 {
				b[i] = 255
			}
			return b
		},
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			if allowedFrame(mutate(append([]byte(nil), base...)), guestMAC, localNetwork, nil) {
				t.Fatal("bypass accepted")
			}
		})
	}
}
func TestEthernetMinimumPadding(t *testing.T) {
	p := frame("192.168.127.2", "8.8.8.8", 17, 40000, 4242)
	p = append(p, make([]byte, 60-len(p))...)
	if !allowedFrame(p, guestMAC, isolated, nil) {
		t.Fatal("standard Ethernet padding rejected")
	}
	p = append(p, 0)
	if allowedFrame(p, guestMAC, isolated, nil) {
		t.Fatal("excess padding accepted")
	}
}

func TestDHCPRequiresBootpIdentity(t *testing.T) {
	p := frame("0.0.0.0", "255.255.255.255", 17, 68, 67)
	binary.BigEndian.PutUint16(p[16:18], uint16(20+8+236+4))
	p = append(p, make([]byte, 236+4)...)
	for i := range 6 {
		p[i] = 255
	}
	binary.BigEndian.PutUint16(p[38:40], uint16(8+236+4))
	p[42] = 1
	p[43] = 1
	p[44] = 6
	copy(p[42+28:42+34], guestMAC[:])
	copy(p[42+236:], []byte{99, 130, 83, 99})
	binary.BigEndian.PutUint16(p[24:26], 0)
	binary.BigEndian.PutUint16(p[24:26], ipv4Checksum(p[14:34]))
	if !allowedFrame(p, guestMAC, isolated, nil) {
		t.Fatal("valid DHCP rejected")
	}
	copy(p[:6], gatewayMAC[:])
	copy(p[26:30], []byte{192, 168, 127, 2})
	copy(p[30:34], []byte{192, 168, 127, 1})
	binary.BigEndian.PutUint16(p[24:26], 0)
	binary.BigEndian.PutUint16(p[24:26], ipv4Checksum(p[14:34]))
	if !allowedFrame(p, guestMAC, isolated, nil) {
		t.Fatal("valid unicast renewal rejected")
	}
	// The upstream DHCP service binds wildcard UDP/67. Packets to an allowed
	// public address would hit it before the UDP forwarder, so reserve UDP/67
	// exclusively for validated gateway/broadcast DHCP.
	remote := append([]byte(nil), p...)
	copy(remote[30:34], []byte{1, 1, 1, 1})
	binary.BigEndian.PutUint16(remote[34:36], 40000)
	if allowedFrame(recalculateChecksum(remote), guestMAC, localNetwork, nil) {
		t.Fatal("public destination UDP/67 reached wildcard DHCP service")
	}
	binary.BigEndian.PutUint16(remote[34:36], 68)
	if allowedFrame(recalculateChecksum(remote), guestMAC, localNetwork, nil) {
		t.Fatal("public destination DHCP reached wildcard DHCP service")
	}
	gatewayWrongPort := append([]byte(nil), p...)
	binary.BigEndian.PutUint16(gatewayWrongPort[34:36], 40000)
	if allowedFrame(gatewayWrongPort, guestMAC, isolated, nil) {
		t.Fatal("non-DHCP source port reached gateway DHCP service")
	}
	p[42+28] ^= 1
	if allowedFrame(p, guestMAC, isolated, nil) {
		t.Fatal("spoofed DHCP identity accepted")
	}
}
