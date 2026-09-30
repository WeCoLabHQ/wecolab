package nebula

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"

	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
)

// Port is where Nebula listens on lighthouses (and relays). Other boxes let the OS choose.
const Port = 4242

// Ports every box of the fabric is reached on over Nebula (docs/architecture.md, "Networking").
const (
	PortWarden   = 8093  // Warden's status, on managers, for managers
	PortCerts    = 8094  // the certificate service, on stewards' managers, for every box
	PortForgejo  = 30300 // a site's copy of the Fabric, for the stewards that push to it
	PortConsole  = 30800
	NodePortLow  = 30000
	NodePortHigh = 32767
)

// Groups are what a box's certificate says it is; firewall rules match on them.
func Groups(site *v1alpha1.Site, box v1alpha1.Box) []string {
	g := []string{"site-" + site.Name, box.Role}
	if box.Role == "manager" {
		if site.Spec.Steward {
			g = append(g, "steward")
		}
		if site.Spec.Public != nil {
			g = append(g, "entrance")
		}
	}
	if box.Laptop {
		g = append(g, "laptop")
	}
	return g
}

// Addr is a box's address with the network's prefix length, as its certificate carries it: the box
// then routes the whole network over Nebula.
func Addr(box v1alpha1.Box, network netip.Prefix) (netip.Prefix, error) {
	a, err := netip.ParseAddr(box.IP)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !network.Contains(a) {
		return netip.Prefix{}, fmt.Errorf("%s is outside the fabric's network %s", box.IP, network)
	}
	return netip.PrefixFrom(a, network.Bits()), nil
}

// Lighthouse is a public site's manager: its Nebula address and where the internet reaches it.
type Lighthouse struct {
	IP     string
	Public string // host:port
}

// Lighthouses are the managers of every public site with a public IPv4 address.
func Lighthouses(sites []v1alpha1.Site) []Lighthouse {
	out := []Lighthouse{}
	for i := range sites {
		s := &sites[i]
		m := s.Manager()
		if m == nil || s.Spec.Public == nil {
			continue
		}
		if a, err := netip.ParseAddr(s.Spec.Public.Address); err == nil && a.Is4() {
			out = append(out, Lighthouse{IP: m.IP, Public: net.JoinHostPort(a.String(), strconv.Itoa(Port))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

type rule struct {
	Port  string `json:"port"`
	Proto string `json:"proto"`
	Host  string `json:"host,omitempty"`
	Group string `json:"group,omitempty"`
}

// Inbound is what a box admits over Nebula: nothing but what its role needs. Only stewards push to a
// site's copy of the Fabric, so Forgejo's node port is left out of the range the Door reaches.
func Inbound(site *v1alpha1.Site, box v1alpha1.Box) []rule {
	own := "site-" + site.Name
	r := []rule{
		{Port: "any", Proto: "icmp", Host: "any"},
		{Port: "10250", Proto: "tcp", Group: own},
		{Port: "8472", Proto: "udp", Group: own},
		// the Door reaches an app on whichever box runs its pods (externalTrafficPolicy: Local)
		{Port: fmt.Sprintf("%d-%d", NodePortLow, PortForgejo-1), Proto: "tcp", Group: "entrance"},
		{Port: fmt.Sprintf("%d-%d", PortForgejo+1, NodePortHigh), Proto: "tcp", Group: "entrance"},
	}
	if box.Role == "manager" {
		r = append(r,
			rule{Port: "6443", Proto: "tcp", Group: own},
			rule{Port: fmt.Sprint(PortWarden), Proto: "tcp", Group: "manager"},
			rule{Port: fmt.Sprint(PortForgejo), Proto: "tcp", Group: "steward"},
		)
		if site.Spec.Steward {
			r = append(r, rule{Port: fmt.Sprint(PortCerts), Proto: "tcp", Host: "any"})
		}
	}
	return r
}

// FabricConfig is the part of a box's Nebula configuration that comes from the Fabric
// (/etc/nebula/config.d/20-fabric.yml): its PKI, its lighthouses and relays, and its firewall. The
// rest (listen port, tun device, preferred ranges) is the box's own, written when it joined.
func FabricConfig(site *v1alpha1.Site, box v1alpha1.Box, lighthouses []Lighthouse, blocklist []string) ([]byte, error) {
	lighthouse := box.Role == "manager" && site.Spec.Public != nil
	hosts, static := []string{}, map[string][]string{}
	for _, l := range lighthouses {
		if l.IP == box.IP {
			continue
		}
		hosts = append(hosts, l.IP)
		static[l.IP] = []string{l.Public}
	}
	cfg := map[string]any{
		"pki": map[string]any{"ca": "/etc/nebula/ca.crt", "cert": "/etc/nebula/host.crt", "key": "/etc/nebula/host.key",
			"disconnect_invalid": true, "blocklist": nonNil(blocklist)},
		"static_host_map": static,
		"punchy":          map[string]any{"punch": true, "respond": true},
		"firewall": map[string]any{
			"outbound": []rule{{Port: "any", Proto: "any", Host: "any"}},
			"inbound":  Inbound(site, box),
		},
	}
	if lighthouse {
		cfg["lighthouse"] = map[string]any{"am_lighthouse": true, "hosts": []string{}}
		cfg["relay"] = map[string]any{"am_relay": true}
	} else {
		cfg["lighthouse"] = map[string]any{"am_lighthouse": false, "hosts": hosts}
		cfg["relay"] = map[string]any{"relays": hosts, "use_relays": true}
	}
	b, err := yaml.Marshal(cfg)
	return append([]byte("# written by WeCoLab from the Fabric; the next sync replaces it\n"), b...), err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Nth is the address of box n (1-254) of the site with index i: host n of the network's i-th /24. It is
// the zero Addr, which is not valid, when either number does not fit.
func Nth(network netip.Prefix, i, n int) netip.Addr {
	bits := network.Bits()
	if !network.Addr().Is4() || bits < 0 || bits > 24 || i < 0 || i >= 1<<(24-bits) || n < 1 || n > 254 {
		return netip.Addr{}
	}
	b := network.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) | uint32(i)<<8 | uint32(n)
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}
