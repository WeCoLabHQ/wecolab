package nebula

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/slackhq/nebula/cert"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
)

var network = netip.MustParsePrefix("10.77.0.0/16")

func site(name string, public bool, steward bool) *v1alpha1.Site {
	s := &v1alpha1.Site{}
	s.Name = name
	s.Spec.Steward = steward
	if public {
		s.Spec.Public = &v1alpha1.Public{Address: "203.0.113.10"}
	}
	return s
}

func TestSignAndVerify(t *testing.T) {
	now := time.Now()
	caCrt, caKey, err := NewCA("test", network, 365*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := Keypair()
	if err != nil {
		t.Fatal(err)
	}
	r := Request{Name: "home-n1", Addr: netip.MustParsePrefix("10.77.1.2/16"), Groups: []string{"site-home", "node"}, PubPEM: pub}
	crt, fp, err := Sign(caCrt, caKey, r, 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := cert.NewCAPoolFromPEM(caCrt)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := cert.UnmarshalCertificateFromPEM(crt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.VerifyCertificate(now, c); err != nil {
		t.Fatalf("does not verify against its CA: %v", err)
	}
	if c.Name() != "home-n1" || c.Networks()[0] != r.Addr || c.Version() != cert.Version2 || len(c.Groups()) != 2 {
		t.Fatalf("wrong certificate: %s", c)
	}
	if got, _, _ := Fingerprint(crt); got != fp {
		t.Fatal("fingerprint")
	}
	// Renewal is due only when something changed or a third of the life is left.
	if Due(crt, caCrt, r, now) {
		t.Fatal("a fresh certificate is not due")
	}
	if !Due(crt, caCrt, r, now.Add(21*24*time.Hour)) {
		t.Fatal("due with a third of its life left")
	}
	r2 := r
	r2.Groups = []string{"site-home", "node", "laptop"}
	if !Due(crt, caCrt, r2, now) {
		t.Fatal("due when its groups change")
	}
	other, _, _ := Keypair()
	r3 := r
	r3.PubPEM = other
	if !Due(crt, caCrt, r3, now) {
		t.Fatal("due when the registered key changes")
	}
	if !Due(nil, caCrt, r, now) {
		t.Fatal("due when the box has none")
	}
	newCA, _, _ := NewCA("next", network, 365*24*time.Hour, now)
	if !Due(crt, newCA, r, now) {
		t.Fatal("due when the newest CA did not issue it")
	}
	// A certificate never outlives its CA, and never leaves the CA's network.
	short, shortKey, _ := NewCA("short", network, time.Hour, now)
	crt2, _, err := Sign(short, shortKey, r, 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	c2, _, _ := cert.UnmarshalCertificateFromPEM(crt2)
	if c2.NotAfter().After(now.Add(time.Hour)) {
		t.Fatal("outlives its CA")
	}
	// An expired CA signs nothing, and neither does another CA's key.
	if _, _, err := Sign(short, shortKey, r, time.Hour, now.Add(2*time.Hour)); err == nil {
		t.Fatal("an expired CA signed")
	}
	if _, _, err := Sign(caCrt, shortKey, r, time.Hour, now); err == nil {
		t.Fatal("signed with a key that is not the CA's")
	}
	r.Addr = netip.MustParsePrefix("10.78.0.2/16")
	if _, _, err := Sign(caCrt, caKey, r, time.Hour, now); err == nil {
		t.Fatal("signed an address outside the network")
	}
}

func TestNewest(t *testing.T) {
	now := time.Now()
	caCrt, caKey, _ := NewCA("test", network, 365*24*time.Hour, now)
	pub, _, _ := Keypair()
	r := Request{Name: "home-n1", Addr: netip.MustParsePrefix("10.77.1.2/16"), Groups: []string{"site-home", "node"}, PubPEM: pub}
	old, _, _ := Sign(caCrt, caKey, r, 30*24*time.Hour, now.Add(-10*24*time.Hour))
	cur, fp, _ := Sign(caCrt, caKey, r, 30*24*time.Hour, now)
	other := r
	other.Name = "home-n2"
	foreign, _, _ := Sign(caCrt, caKey, other, 30*24*time.Hour, now.Add(time.Hour))
	if got := Newest(caCrt, nil, "home-n1", now, old, cur, foreign, nil, []byte("junk")); string(got) != string(cur) {
		t.Fatal("the newest valid certificate for the box")
	}
	if got := Newest(caCrt, []string{fp}, "home-n1", now, old, cur); string(got) != string(old) {
		t.Fatal("a blocklisted certificate does not count")
	}
	if Newest(caCrt, nil, "home-n1", now.Add(60*24*time.Hour), old, cur) != nil {
		t.Fatal("expired certificates do not count")
	}
}

func TestNth(t *testing.T) {
	cases := []struct {
		net  string
		i, n int
		want string
	}{
		{"10.77.0.0/16", 0, 1, "10.77.0.1"},
		{"10.77.0.0/16", 3, 254, "10.77.3.254"},
		{"10.77.0.0/16", 256, 1, ""},
		{"10.77.0.0/16", 1, 0, ""},
		{"10.77.0.0/16", 1, 255, ""},
		{"10.64.0.0/12", 300, 2, "10.65.44.2"},
		{"10.77.16.0/20", 15, 1, "10.77.31.1"},
		{"10.77.16.0/20", 16, 1, ""},
		{"10.77.1.0/24", 0, 1, "10.77.1.1"},
		{"10.77.1.0/25", 0, 1, ""},
	}
	for _, c := range cases {
		got := Nth(netip.MustParsePrefix(c.net), c.i, c.n)
		if c.want == "" && got.IsValid() || c.want != "" && got.String() != c.want {
			t.Errorf("Nth(%s, %d, %d) = %v, want %q", c.net, c.i, c.n, got, c.want)
		}
	}
}

func TestLighthousesNeedIPv4(t *testing.T) {
	a := site("a", true, false)
	a.Spec.Boxes = []v1alpha1.Box{{Name: "a-m", IP: "10.77.0.1", Role: "manager"}}
	b := site("b", true, false)
	b.Spec.Public.Address = "2001:db8::1"
	b.Spec.Boxes = []v1alpha1.Box{{Name: "b-m", IP: "10.77.1.1", Role: "manager"}}
	c := site("c", true, false)
	c.Spec.Public.Address = "203.0.113.9:1\nx"
	c.Spec.Boxes = []v1alpha1.Box{{Name: "c-m", IP: "10.77.2.1", Role: "manager"}}
	if l := Lighthouses([]v1alpha1.Site{*a, *b, *c}); len(l) != 1 || l[0].Public != "203.0.113.10:4242" {
		t.Fatalf("only an IPv4 public address makes a lighthouse: %+v", l)
	}
}

func TestGroupsAndRules(t *testing.T) {
	pub := site("pub", true, true)
	mgr := v1alpha1.Box{Name: "pub-a", IP: "10.77.0.1", Role: "manager"}
	if g := strings.Join(Groups(pub, mgr), ","); g != "site-pub,manager,steward,entrance" {
		t.Fatalf("public steward manager: %s", g)
	}
	home := site("home", false, false)
	mac := v1alpha1.Box{Name: "home-mac", IP: "10.77.1.3", Role: "node", Laptop: true}
	if g := strings.Join(Groups(home, mac), ","); g != "site-home,node,laptop" {
		t.Fatalf("laptop node: %s", g)
	}
	has := func(rs []rule, port, from string) bool {
		for _, r := range rs {
			if r.Port == port && (r.Group == from || r.Host == from) {
				return true
			}
		}
		return false
	}
	nodeRules := Inbound(home, mac)
	if has(nodeRules, "6443", "site-home") || has(nodeRules, "8093", "manager") || has(nodeRules, "8094", "any") || has(nodeRules, "30300", "steward") {
		t.Fatal("a node serves neither the Kubernetes API, Warden, certificates nor the Fabric")
	}
	if !has(nodeRules, "30000-30299", "entrance") || !has(nodeRules, "30301-32767", "entrance") {
		t.Fatal("the Door cannot reach an app whose pods run on a node")
	}
	mgrRules := Inbound(home, v1alpha1.Box{Role: "manager"})
	if !has(mgrRules, "6443", "site-home") || !has(mgrRules, "30300", "steward") || !has(mgrRules, "8093", "manager") {
		t.Fatalf("manager rules: %+v", mgrRules)
	}
	if has(mgrRules, "6443", "site-pub") {
		t.Fatal("another site reaches this site's Kubernetes API")
	}
	for _, r := range mgrRules {
		if r.Port == "30300" && r.Group != "steward" || r.Port == "8093" && r.Group != "manager" || strings.Contains(r.Port, "-") && r.Port != "30000-30299" && r.Port != "30301-32767" {
			t.Fatalf("only stewards reach the Fabric and only managers reach Warden's status: %+v", r)
		}
	}
	if has(mgrRules, "8094", "any") {
		t.Fatal("a site that is not a steward serves no certificates")
	}
	if !has(Inbound(pub, mgr), "8094", "any") {
		t.Fatal("every box reaches a steward's certificate service")
	}
}

func TestFabricConfig(t *testing.T) {
	pub := site("pub", true, true)
	pub.Spec.Boxes = []v1alpha1.Box{{Name: "pub-a", IP: "10.77.0.1", Role: "manager"}}
	home := site("home", false, true)
	home.Spec.Boxes = []v1alpha1.Box{{Name: "home-a", IP: "10.77.1.1", Role: "manager"}}
	lhs := Lighthouses([]v1alpha1.Site{*pub, *home})
	if len(lhs) != 1 || lhs[0].Public != "203.0.113.10:4242" {
		t.Fatalf("lighthouses: %+v", lhs)
	}
	var lh, h map[string]any
	b, err := FabricConfig(pub, pub.Spec.Boxes[0], lhs, nil)
	if err != nil || yaml.Unmarshal(b, &lh) != nil {
		t.Fatal(err)
	}
	b, _ = FabricConfig(home, home.Spec.Boxes[0], lhs, []string{"abc"})
	if yaml.Unmarshal(b, &h) != nil {
		t.Fatal("home config")
	}
	if lh["lighthouse"].(map[string]any)["am_lighthouse"] != true || lh["relay"].(map[string]any)["am_relay"] != true {
		t.Fatalf("the public manager is a lighthouse and relay: %v", lh)
	}
	if len(lh["static_host_map"].(map[string]any)) != 0 {
		t.Fatal("a lighthouse does not list itself")
	}
	hl := h["lighthouse"].(map[string]any)
	if hl["am_lighthouse"] != false || len(hl["hosts"].([]any)) != 1 || h["static_host_map"].(map[string]any)["10.77.0.1"] == nil {
		t.Fatalf("a home box finds the lighthouse: %v", h)
	}
	pki := h["pki"].(map[string]any)
	if pki["disconnect_invalid"] != true || len(pki["blocklist"].([]any)) != 1 {
		t.Fatalf("pki: %v", pki)
	}
}
