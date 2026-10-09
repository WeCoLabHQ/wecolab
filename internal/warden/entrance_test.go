package warden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
)

func TestRouteYAML(t *testing.T) {
	y := RouteYAML(Route{Name: "vince.docs", Hostname: "docs.fab.example.org", MeshHost: "docs-vince.mesh.fab.example.org", Servers: []string{"http://10.77.1.1:31000"}}, "fab.example.org", true)
	for _, want := range []string{"rule: Host(`docs.fab.example.org`)", "certResolver: public", "people-only@file", "main: '*.mesh.fab.example.org'", "url: http://10.77.1.1:31000", "vince.docs.mesh:", "priority: 1000\n"} {
		if !strings.Contains(y, want) {
			t.Errorf("route lacks %q:\n%s", want, y)
		}
	}
	if y := RouteYAML(Route{Name: "x.y", Hostname: "x.z"}, "z", false); !strings.Contains(y, "servers: []") || strings.Contains(y, "certResolver") {
		t.Errorf("no servers and no ACME in development:\n%s", y)
	}
	if y := RouteYAML(Route{Name: "console", Hostname: "console.z", System: true}, "z", false); !strings.Contains(y, "priority: 1000000") || strings.Contains(y, "accessLogs") {
		t.Errorf("the fabric's own routes outrank every app's, and are logged:\n%s", y)
	}
	// An app's visitors are not the Door's to log, by its public name or its mesh name.
	if n := strings.Count(y, "accessLogs: false"); n != 2 {
		t.Errorf("app routers with the access log off: %d of 2:\n%s", n, y)
	}
	// The sink itself refuses what is not a host name: a rule is Traefik's language.
	for _, bad := range []string{"a`) || Host(`console.z", "a.z\n    evil:\n      rule: PathPrefix(`/`)", "{{ .Env }}.z"} {
		if y := RouteYAML(Route{Name: "p.a", Hostname: bad, MeshHost: bad}, "z", true); strings.Contains(y, "rule:") {
			t.Errorf("%q became a rule:\n%s", bad, y)
		}
	}
}

func TestZoneFile(t *testing.T) {
	z := ZoneFile("fab.example.org", 7, []string{"203.0.113.10", "198.51.100.2", "6.6.6.6\n* IN A 6.6.6.7"}, "203.0.113.10", "100.96.0.5",
		map[string][]string{"_acme-challenge.mesh.fab.example.org.": {"tok"}})
	for _, want := range []string{"@ IN SOA ns1.fab.example.org. hostmaster.fab.example.org. 7 ", "@ IN NS ns2.fab.example.org.", "ns2 IN A 198.51.100.2",
		"* IN A 203.0.113.10", "mesh IN A 203.0.113.10", "*.mesh IN A 100.96.0.5", `_acme-challenge.mesh IN TXT "tok"`} {
		if !strings.Contains(z, want) {
			t.Errorf("zone lacks %q:\n%s", want, z)
		}
	}
	if strings.Contains(z, "6.6.6") || strings.Contains(z, "ns3") {
		t.Errorf("an address that is not one reached the zone:\n%s", z)
	}
}

// entranceFixture is a fabric of a public steward (pub), a second steward (home) and a friend's site,
// whose statuses the peers answer with.
func entranceFixture(t *testing.T, statuses map[string]*SiteStatus, objs ...client.Object) *Entrance {
	t.Helper()
	dir := t.TempDir()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	site := func(name string, idx int, steward bool) *v1alpha1.Site {
		st := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.SiteSpec{Index: idx, Steward: steward,
			Boxes: []v1alpha1.Box{{Name: name + "-a", IP: fmt.Sprintf("10.77.%d.1", idx), Role: "manager"}, {Name: name + "-b", IP: fmt.Sprintf("10.77.%d.2", idx), Role: "node"}}}}
		if name == "pub" {
			st.Spec.Public = &v1alpha1.Public{Address: "203.0.113.10"}
		}
		return st
	}
	settings := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: SettingsName, Namespace: SystemNS},
		Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": "pub", "epoch": "1", "people": "pub"}}
	objs = append(objs, site("pub", 0, true), site("home", 1, true), site("friend", 2, false), settings)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	byIP := map[string]*SiteStatus{}
	for name, st := range statuses {
		st.Site = name // a site's status names it; Peers refuses one that answers for another
		byIP[map[string]string{"pub": "10.77.0.1", "home": "10.77.1.1", "friend": "10.77.2.1"}[name]] = st
	}
	peers := &Peers{Client: c, HTTP: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		st, ok := byIP[r.URL.Hostname()]
		if !ok {
			return nil, fmt.Errorf("unreachable")
		}
		b, _ := json.Marshal(st)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}}, nil
	})}}
	return &Entrance{Client: c, Peers: peers, Site: "pub", RoutesDir: dir, ZoneFile: filepath.Join(dir, "db.zone"), PeopleNet: "100.96.0.0/16",
		NetBirdIP: func() string { return "100.96.0.5" }, TLS: true, User: "u", Password: "p", txt: map[string][]string{}}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Hostile names and endpoints never reach Traefik, and no two apps share a route.
func TestEntranceRoutes(t *testing.T) {
	app := func(ns, name, host string, mesh bool) *v1alpha1.App {
		return &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: v1alpha1.AppSpec{Sites: []string{"home", "friend"}, Primary: "home", Hostname: host, Mesh: mesh, Workload: name}}
	}
	active := func(eps ...string) AppState { return AppState{Active: "home", Endpoints: eps} }
	statuses := map[string]*SiteStatus{
		"pub": {Writer: "pub", Epoch: 1},
		// A friend's site claims a writer with a high epoch: it is no steward, so it does not count.
		"friend": {Writer: "home", Epoch: 99, Apps: map[string]AppState{"vince/docs": {Active: "friend", Endpoints: []string{"10.77.2.1:31000"}}}},
		"home": {Writer: "pub", Epoch: 1, Apps: map[string]AppState{
			"vince/docs": active("10.77.1.2:31000", "127.0.0.1:8095", "10.77.2.1:31000", "10.77.1.1:22", "[::ffff:10.77.1.1]:31000", "10.77.1.1:31000\n"),
			"a-b/c":      active("10.77.1.1:31001"), "a/b-c": active("10.77.1.1:31002"),
		}},
	}
	e := entranceFixture(t, statuses,
		app("vince", "docs", "docs.fab.example.org", true),
		app("evil", "console", "console.fab.example.org", false),
		app("evil", "mesh", "mesh.fab.example.org", false),
		app("evil", "deep", "x.mesh.fab.example.org", false),
		app("evil", "apex", "fab.example.org", false),
		app("evil", "inject", "x.fab.example.org`) || Host(`console.fab.example.org", false),
		app("one", "shared", "shared.fab.example.org", false), app("two", "shared", "shared.fab.example.org", false),
		app("c", "a-b", "", true), app("b-c", "a", "", true), // both a-b-c.mesh.<zone>: neither is served
		app("a-b", "c", "c.fab.example.org", false), app("a", "b-c", "bc.fab.example.org", false), // were both a-b-c
	)
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	es, _ := os.ReadDir(e.RoutesDir)
	for _, f := range es {
		if strings.HasSuffix(f.Name(), ".yml") {
			b, _ := os.ReadFile(filepath.Join(e.RoutesDir, f.Name()))
			files[strings.TrimSuffix(f.Name(), ".yml")] = string(b)
			var v map[string]any
			if err := yaml.Unmarshal(b, &v); err != nil {
				t.Errorf("%s is not YAML: %v", f.Name(), err)
			}
		}
	}
	docs := files["vince.docs"]
	if !strings.Contains(docs, "url: http://10.77.1.2:31000") || strings.Count(docs, "url:") != 1 {
		t.Errorf("only the primary's own boxes on a NodePort are servers:\n%s", docs)
	}
	if !strings.Contains(docs, "Host(`docs-vince.mesh.fab.example.org`)") {
		t.Errorf("mesh names are <app>-<project>.mesh.<zone>:\n%s", docs)
	}
	if c := files["console"]; !strings.Contains(c, "http://10.77.0.1:30800") || !strings.Contains(c, "priority: 1000000") {
		t.Errorf("the Console stays at the Fabric's writer, outranking apps:\n%s", c)
	}
	if files["a-b.c"] == "" || files["a.b-c"] == "" {
		t.Errorf("apps whose names joined with a dash collide get routes of their own: %v", keys(files))
	}
	for name, body := range files {
		if name != "console" && strings.Contains(body, "console.fab") {
			t.Errorf("%s routes the Console's name:\n%s", name, body)
		}
		if name != "people" && strings.Contains(body, "Host(`mesh.fab") {
			t.Errorf("%s routes NetBird's name:\n%s", name, body)
		}
		for _, bad := range []string{"127.0.0.1:8095", "10.77.2.1", ":22", "x.mesh.fab", "`fab.example.org`", "shared.", "a-b-c.mesh"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s carries %q:\n%s", name, bad, body)
			}
		}
	}
	for _, name := range []string{"evil.console", "evil.mesh", "evil.deep", "evil.apex", "evil.inject", "one.shared", "two.shared", "c.a-b", "b-c.a"} {
		if _, ok := files[name]; ok {
			t.Errorf("%s was routed", name)
		}
	}
	if !strings.Contains(files["people"], "!Path(`/api/setup`)") || !strings.Contains(files["people"], "!PathPrefix(`/api/instance/`)") {
		t.Errorf("NetBird's unauthenticated setup is not passed on:\n%s", files["people"])
	}
}

func TestEntranceKeepsHighestWriterClaim(t *testing.T) {
	statuses := map[string]*SiteStatus{
		"pub":  {Writer: "pub", Epoch: 1},
		"home": {Writer: "home", Epoch: 2},
	}
	e := entranceFixture(t, statuses)
	transport := e.Peers.HTTP.Transport
	unreachable := map[string]bool{}
	e.Peers.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if unreachable[r.URL.Hostname()] {
			return nil, fmt.Errorf("peer unavailable")
		}
		return transport.RoundTrip(r)
	})
	check := func(want string) {
		t.Helper()
		e.Peers.cache = nil // each round observes the changed peer availability
		if err := e.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(e.RoutesDir, "console.yml"))
		if err != nil {
			t.Fatal(err)
		}
		var route struct {
			HTTP traefikHTTP `json:"http"`
		}
		if err := yaml.Unmarshal(b, &route); err != nil {
			t.Fatal(err)
		}
		servers := route.HTTP.Services["console"].LoadBalancer.Servers
		if len(servers) != 1 || servers[0]["url"] != want {
			t.Fatalf("Console route: got %v, want %s", servers, want)
		}
	}

	check("http://10.77.1.1:30800")
	unreachable["10.77.1.1"] = true
	check("http://10.77.1.1:30800") // pub still reports the superseded epoch
	unreachable["10.77.0.1"] = true
	check("http://10.77.1.1:30800") // Flux still says pub/1; neither peer answers

	statuses["pub"].Epoch = 3
	delete(unreachable, "10.77.0.1")
	check("http://10.77.0.1:30800") // a newer claim must still move the route
	unreachable["10.77.0.1"] = true
	delete(unreachable, "10.77.1.1")
	check("http://10.77.0.1:30800") // home's now-stale epoch cannot take it back

	statuses["home"].Epoch = 3
	check("http://10.77.1.1:30800") // the lower name wins equal epochs
	unreachable["10.77.1.1"] = true
	delete(unreachable, "10.77.0.1")
	check("http://10.77.1.1:30800") // keep the winning tie when that peer is silent

	home := &v1alpha1.Site{}
	if err := e.Client.Get(context.Background(), client.ObjectKey{Name: "home"}, home); err != nil {
		t.Fatal(err)
	}
	home.Spec.Steward = false
	if err := e.Client.(client.Client).Update(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	check("http://10.77.0.1:30800") // remembered authority cannot outlive stewardship
}

// Git alone names the site an app's users go to: with its primary silent, a site of the app that says
// it is active, and publishes its own boxes, gets no server.
func TestEntranceFollowsGitPrimary(t *testing.T) {
	e := entranceFixture(t, map[string]*SiteStatus{
		"pub":    {Writer: "pub", Epoch: 1},
		"friend": {Apps: map[string]AppState{"vince/docs": {Active: "friend", Endpoints: []string{"10.77.2.1:31000"}}}},
	}, &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "vince", Name: "docs"},
		Spec: v1alpha1.AppSpec{Sites: []string{"home", "friend"}, Primary: "home", Hostname: "docs.fab.example.org", Workload: "docs"}})
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(e.RoutesDir, "vince.docs.yml"))
	if err != nil || !strings.Contains(string(b), "servers: []") {
		t.Fatalf("a replica's claim drew the app's users: %v\n%s", err, b)
	}
}

func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The ACME challenge endpoint puts the record in the zone and takes it out again: locally, with the
// pod's credentials, and only for a challenge name inside the zone.
func TestEntranceChallenge(t *testing.T) {
	e := entranceFixture(t, nil)
	const tok = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJ0123-_x" // 43
	post := func(path, remote, user, password, body string) int {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		r.RemoteAddr = remote
		if user != "" {
			r.SetBasicAuth(user, password)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w.Code
	}
	ok := `{"fqdn":"_acme-challenge.mesh.fab.example.org.","value":"` + tok + `"}`
	if post("/present", "192.0.2.1:5000", "u", "p", ok) != http.StatusForbidden {
		t.Fatal("only this box may answer challenges")
	}
	if post("/present", "127.0.0.1:5000", "", "", ok) != http.StatusUnauthorized || post("/present", "127.0.0.1:5000", "u", "wrong", ok) != http.StatusUnauthorized {
		t.Fatal("the pod's credentials are required")
	}
	for _, bad := range []string{
		`{"fqdn":"_acme-challenge.example.com.","value":"` + tok + `"}`,
		`{"fqdn":"_acme-challenge.evilfab.example.org.","value":"` + tok + `"}`,
		`{"fqdn":"mesh.fab.example.org.","value":"` + tok + `"}`,
		`{"fqdn":"_acme-challenge.mesh.fab.example.org","value":"` + tok + `"}`,
		`{"fqdn":"_acme-challenge.x\n* IN A 6.6.6.6\n.fab.example.org.","value":"` + tok + `"}`,
		`{"fqdn":"_acme-challenge.mesh.fab.example.org.","value":"` + tok + `\n"}`,
		`{"fqdn":"_acme-challenge.mesh.fab.example.org.","value":"short"}`,
	} {
		if code := post("/present", "127.0.0.1:5000", "u", "p", bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code := post("/present", "127.0.0.1:5000", "u", "p", ok); code != http.StatusOK {
		t.Fatalf("present: %d", code)
	}
	zone, _ := os.ReadFile(e.ZoneFile)
	if !strings.Contains(string(zone), `_acme-challenge.mesh IN TXT "`+tok+`"`) || !strings.Contains(string(zone), "*.mesh IN A 100.96.0.5") || strings.Count(string(zone), "TXT") != 1 {
		t.Fatalf("zone after present:\n%s", zone)
	}
	if post("/cleanup", "127.0.0.1:5000", "u", "p", ok) != http.StatusOK {
		t.Fatal("cleanup")
	}
	zone, _ = os.ReadFile(e.ZoneFile)
	if strings.Contains(string(zone), "TXT") {
		t.Fatalf("record left after cleanup:\n%s", zone)
	}
	e.User, e.Password = "", ""
	if post("/present", "127.0.0.1:5000", "", "", ok) != http.StatusUnauthorized {
		t.Fatal("without credentials of its own the endpoint refuses everyone")
	}
}

func TestNamesConfig(t *testing.T) {
	routes := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wt0\t00006064\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"ens3\t00000000\t0100000A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	if i, err := DefaultRouteInterface(routes); err != nil || i != "ens3" {
		t.Fatalf("the default route with the lowest metric: %q %v", i, err)
	}
	if _, err := DefaultRouteInterface("Iface\tDestination\n"); err == nil {
		t.Fatal("no default route is an error")
	}
	c, err := Corefile("fab.example.org", "ens3", "/zones/db.zone")
	if err != nil || !strings.Contains(c, "fab.example.org:53 {\n    bind ens3\n    file /zones/db.zone {") {
		t.Fatalf("%v\n%s", err, c)
	}
	for _, bad := range [][2]string{{"fab.example.org {\n", "ens3"}, {"fab.example.org", "ens3 {\n"}, {"fab.example.org", ""}} {
		if _, err := Corefile(bad[0], bad[1], "/zones/db.zone"); err == nil {
			t.Errorf("Corefile(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

// The Door's pod holds only what it uses: its own read-only account, the token in the Warden alone,
// no capability but binding low ports; NetBird's pod no capability at all.
func TestDoorManifests(t *testing.T) {
	docs := []string{}
	for _, f := range []string{"entrance/door.yaml", "people/netbird.yaml"} {
		b, err := os.ReadFile("../bootstrap/template/system/" + f)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, strings.Split(strings.ReplaceAll(string(b), "{{ .Version }}", "v0"), "\n---\n")...)
	}
	deployments := 0
	for _, doc := range docs {
		var kind struct{ Kind string }
		_ = yaml.Unmarshal([]byte(doc), &kind)
		switch kind.Kind {
		case "ClusterRole", "Role":
			var r rbacv1.ClusterRole
			if err := yaml.UnmarshalStrict([]byte(doc), &r); err != nil {
				t.Fatal(err)
			}
			for _, rule := range r.Rules {
				for _, v := range rule.Verbs {
					if v != "get" && v != "list" && v != "watch" {
						t.Errorf("%s may %s %v", r.Name, v, rule.Resources)
					}
				}
			}
		case "Deployment":
			deployments++
			var d appsv1.Deployment
			if err := yaml.UnmarshalStrict([]byte(doc), &d); err != nil {
				t.Fatal(err)
			}
			p := d.Spec.Template.Spec
			if p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken || p.SecurityContext == nil ||
				p.SecurityContext.SeccompProfile == nil || p.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Errorf("%s: no automatic token, RuntimeDefault seccomp", d.Name)
			}
			if d.Name == "door" && p.ServiceAccountName != "wecolab-door" {
				t.Errorf("the Door runs as wecolab-door, not %q", p.ServiceAccountName)
			}
			for _, c := range append(p.InitContainers, p.Containers...) {
				sc := c.SecurityContext
				if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.Capabilities == nil ||
					!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || slices.ContainsFunc(sc.Capabilities.Add, func(c corev1.Capability) bool { return c != "NET_BIND_SERVICE" }) {
					t.Errorf("%s/%s: drop every capability but NET_BIND_SERVICE, no escalation", d.Name, c.Name)
				}
				for _, m := range c.VolumeMounts {
					if m.Name == "token" && c.Name != "warden" {
						t.Errorf("%s/%s mounts the Kubernetes token", d.Name, c.Name)
					}
				}
			}
		}
	}
	if deployments != 2 {
		t.Fatalf("found %d deployments", deployments)
	}
}
func TestSettings(t *testing.T) {
	s, err := ParseSettings(map[string]string{"zone": "z", "network": "10.77.0.0/16", "writer": "pub", "epoch": "3", "certLife": "1h",
		"blocklist": "old 2020-01-01T00:00:00Z\nlive 2099-01-01T00:00:00Z\n"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Epoch != 3 || s.CertLife != time.Hour {
		t.Fatalf("%+v", s)
	}
	if b := s.Blocklist(time.Now()); len(b) != 1 || b[0] != "live" {
		t.Fatalf("expired entries leave the blocklist: %v", b)
	}
	if _, err := ParseSettings(map[string]string{"zone": "z", "network": "nope", "writer": "pub"}); err == nil {
		t.Fatal("a bad network is refused")
	}
}

func TestProjectStanding(t *testing.T) {
	sites := []v1alpha1.Site{{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: v1alpha1.SiteSpec{Owner: "ana"}}}
	offers := []v1alpha1.Offer{{Spec: v1alpha1.OfferSpec{Site: "home", CPU: "2", Memory: "4Gi", To: []string{"bo"}}}}
	quota := func(st Standing) map[string]any {
		for _, o := range ProjectObjects("p", st, "10.77.0.0/16") {
			if o.GetKind() == "ResourceQuota" {
				return o.Object["spec"].(map[string]any)["hard"].(map[string]any)
			}
		}
		return nil
	}
	if q := quota(StandingAt("ana", "home", sites, offers, nil)); q != nil {
		t.Fatalf("the owner's own site has no quota: %v", q)
	}
	if q := quota(StandingAt("bo", "home", sites, offers, nil)); q["requests.cpu"] != "2" || q["requests.memory"] != "4Gi" {
		t.Fatalf("a holder gets what it holds: %v", q)
	}
	if q := quota(StandingAt("cy", "home", sites, offers, nil)); q["requests.cpu"] != "0" {
		t.Fatalf("anyone else gets nothing: %v", q)
	}
	for _, o := range ProjectObjects("p", Standing{}, "10.77.0.0/16") {
		if o.GetName() == "allow-app-ingress" && !strings.Contains(strings.ReplaceAll(toJSON(o.Object), " ", ""), `"cidr":"10.77.0.0/16"`) {
			t.Fatal("apps admit the Door over Nebula")
		}
	}
}

func TestEffectiveWriter(t *testing.T) {
	stewards := map[string]bool{"pub": true, "home": true}
	own := Claim{Writer: "pub", Epoch: 1}
	if e := Effective(own, []Claim{{"pub", 1}, {"pub", 1}}, stewards); e != own {
		t.Fatalf("steady: everyone follows the Fabric's word: %v", e)
	}
	if e := Effective(own, []Claim{{"home", 2}}, stewards); e.Writer != "home" || e.Epoch != 2 {
		t.Fatalf("a steward's higher epoch wins: %v", e)
	}
	if e := Effective(own, []Claim{{"friend", 9}}, stewards); e.Writer != "pub" {
		t.Fatalf("only stewards may claim: %v", e)
	}
	if e := Effective(Claim{"pub", 2}, []Claim{{"home", 2}}, stewards); e.Writer != "home" {
		t.Fatalf("two claims at one epoch settle on the lower name: %v", e)
	}
}

func TestPeopleKustomizationSubstitutesNothingFromSecrets(t *testing.T) {
	// Flux reads substituteFrom in its own namespace; the people mesh's Secret lives in wecolab-system.
	site := &v1alpha1.Site{}
	site.Name, site.Spec.Steward = "pub", true
	for _, k := range SiteKustomizations(site, &Settings{Zone: "fab.example.org", People: "pub"}, "owner@example.org") {
		if _, found, _ := unstructured.NestedFieldNoCopy(k.Object, "spec", "postBuild", "substituteFrom"); found {
			t.Fatalf("%s substitutes from a Secret Flux cannot see", k.GetName())
		}
	}
}
