package warden

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/nebula"
	"wecolab.io/wecolab/internal/validate"
)

// Entrance is Warden in the Door's pod at a public site. From the Fabric and the sites' published
// status it writes one Traefik route per app, pointing at where the app's primary answers, the
// Console's and the people mesh's routes, and the fabric's DNS zone for Names. It answers Traefik's
// DNS challenge for the mesh names' wildcard certificate by putting the TXT record in that zone.
//
// A site's status is believed only about that site and only as far as the Fabric allows: an app's
// servers are what the primary Git names publishes, on that site's own boxes, and only stewards'
// writer claims count (docs/plans/2026-09-29-hardening.md, R1). Every name that reaches Traefik or the zone is checked (R3).
type Entrance struct {
	Client    client.Reader
	Peers     *Peers
	Site      string
	RoutesDir string        // Traefik's file provider directory
	ZoneFile  string        // the zone file Names serves
	PeopleNet string        // NetBird's address range: mesh names admit only these
	NetBirdIP func() string // this box's NetBird address, "" until NetBird is up (it may start after the Door)
	TLS       bool          // false in the development fabric: Traefik's own certificate, no ACME
	// User and Password are what Traefik's DNS-challenge requests must carry: anything on the box's
	// network reaches the endpoint's address too.
	User, Password string

	mu     sync.Mutex          // the challenge records, the zone file and what was last written to it
	txt    map[string][]string // ACME challenge records: fqdn -> values
	serial uint32
	zone   string

	skipped     string // what the last Sync left out, logged when it changes (Run's goroutine alone)
	writerClaim Claim  // highest observed authority; Run's goroutine alone
}

// Router priorities. Traefik's default is the rule's length, so a longer tenant rule would outrank the
// Console; explicit ones make the fabric's own routes win whatever an app asks for.
const (
	prioSystem = 1000000
	prioTenant = 1000
)

// MeshName is where people on the mesh reach an app: <app>-<project>.mesh.<zone>. The Door routes it
// and the Console shows it, so both call this. Two apps can share one (a-b in c, a in b-c); the Door
// then serves neither.
func MeshName(app *v1alpha1.App, zone string) string {
	return app.Name + "-" + app.Namespace + ".mesh." + zone
}

// Route is one app's Traefik route: its public name and its mesh name, to its servers. Name is
// <project>.<app> (both labels, so no two apps share it and no system route, which has no dot, does).
type Route struct {
	Name, Hostname, MeshHost string
	Servers                  []string // http://<nebula address>:<port>
	System                   bool     // the fabric's own route (the Console): outranks every app's
}

// Traefik's dynamic configuration, the part the Door writes. It is built as data and marshalled, never
// assembled from strings, because every name in it came from someone's input.
type traefikHTTP struct {
	Routers     map[string]traefikRouter     `json:"routers,omitempty"`
	Services    map[string]traefikService    `json:"services,omitempty"`
	Middlewares map[string]traefikMiddleware `json:"middlewares,omitempty"`
}

type traefikRouter struct {
	Rule          string                `json:"rule"`
	Priority      int                   `json:"priority"`
	EntryPoints   []string              `json:"entryPoints"`
	Middlewares   []string              `json:"middlewares,omitempty"`
	Service       string                `json:"service"`
	TLS           *traefikTLS           `json:"tls"`
	Observability *traefikObservability `json:"observability,omitempty"`
}

// traefikObservability turns the access log off for a router. An app's visitors are its project's
// business: the owner of the site running the Door does not log them.
type traefikObservability struct {
	AccessLogs bool `json:"accessLogs"`
}

type traefikTLS struct {
	CertResolver string              `json:"certResolver,omitempty"`
	Domains      []map[string]string `json:"domains,omitempty"`
}

type traefikService struct {
	LoadBalancer struct {
		Servers []map[string]string `json:"servers"`
	} `json:"loadBalancer"`
}

type traefikMiddleware struct {
	IPAllowList struct {
		SourceRange []string `json:"sourceRange"`
	} `json:"ipAllowList"`
}

func loadBalancer(urls ...string) traefikService {
	s := traefikService{}
	s.LoadBalancer.Servers = []map[string]string{}
	for _, u := range urls {
		s.LoadBalancer.Servers = append(s.LoadBalancer.Servers, map[string]string{"url": u})
	}
	return s
}

func render(h traefikHTTP) string {
	b, _ := yaml.Marshal(map[string]traefikHTTP{"http": h}) // maps and strings: cannot fail
	return "# written by the entrance Warden from the Fabric and the sites' status; do not edit\n" + string(b)
}

// hostRule is Traefik's rule for one host name, "" for anything else: the rule is Traefik's own
// language, and only a host name cannot end it.
func hostRule(h string) string {
	if validate.DNSName(h) != nil {
		return ""
	}
	return "Host(`" + h + "`)"
}

// RouteYAML renders a route for Traefik's file provider. Public names get a certificate of their own;
// mesh names share the wildcard and admit only the people mesh. A name that is not a host name gets
// no router.
func RouteYAML(r Route, zone string, tls bool) string {
	prio := prioTenant
	if r.System {
		prio = prioSystem
	}
	pub, mesh := &traefikTLS{}, &traefikTLS{}
	if tls {
		pub = &traefikTLS{CertResolver: "public"}
		mesh = &traefikTLS{CertResolver: "mesh", Domains: []map[string]string{{"main": "*.mesh." + zone}}}
	}
	var quiet *traefikObservability // the fabric's own routes are logged; apps' are not
	if !r.System {
		quiet = &traefikObservability{AccessLogs: false}
	}
	h := traefikHTTP{Routers: map[string]traefikRouter{}, Services: map[string]traefikService{r.Name: loadBalancer(r.Servers...)}}
	if rule := hostRule(r.Hostname); rule != "" {
		h.Routers[r.Name] = traefikRouter{Rule: rule, Priority: prio, EntryPoints: []string{"websecure"}, Service: r.Name, TLS: pub, Observability: quiet}
	}
	if rule := hostRule(r.MeshHost); rule != "" {
		h.Routers[r.Name+".mesh"] = traefikRouter{Rule: rule, Priority: prio, EntryPoints: []string{"websecure"},
			Middlewares: []string{"people-only@file"}, Service: r.Name, TLS: mesh, Observability: quiet}
	}
	return render(h)
}

// peopleOnly is the middleware mesh names use: NetBird's addresses only.
func peopleOnly(peopleNet string) string {
	m := traefikMiddleware{}
	m.IPAllowList.SourceRange = []string{peopleNet}
	return render(traefikHTTP{Middlewares: map[string]traefikMiddleware{"people-only": m}})
}

// netbirdAPI is NetBird's API without /api/setup, which makes its first caller the owner of NetBird's
// identity provider, and /api/instance, which says whether that is still open. Neither needs
// authentication; the install reaches both on the box itself.
const netbirdAPI = "PathPrefix(`/api`) && !Path(`/api/setup`) && !PathPrefix(`/api/setup/`) && !Path(`/api/instance`) && !PathPrefix(`/api/instance/`)"

// peopleRoutes send NetBird's gRPC, relay, API and sign-in paths to the NetBird server on this box.
func peopleRoutes(zone string, tls bool) string {
	t := &traefikTLS{}
	if tls {
		t.CertResolver = "public"
	}
	host := hostRule("mesh." + zone)
	return render(traefikHTTP{
		Routers: map[string]traefikRouter{
			"netbird-grpc": {Rule: host + " && (PathPrefix(`/signalexchange.SignalExchange/`) || PathPrefix(`/management.ManagementService/`))",
				Priority: prioSystem, EntryPoints: []string{"websecure"}, Service: "netbird-grpc", TLS: t},
			"netbird": {Rule: host + " && (PathPrefix(`/relay`) || PathPrefix(`/ws-proxy/`) || PathPrefix(`/oauth2`) || (" + netbirdAPI + "))",
				Priority: prioSystem, EntryPoints: []string{"websecure"}, Service: "netbird", TLS: t},
		},
		Services: map[string]traefikService{"netbird-grpc": loadBalancer("h2c://127.0.0.1:8081"), "netbird": loadBalancer("http://127.0.0.1:8081")},
	})
}

// Record is one DNS record of the fabric's zone.
type Record struct{ Name, Type, Value string }

// ZoneFile renders the fabric's zone: every public site is a name server and a Door for every name;
// mesh names go to the Door the people mesh reaches; ACME challenges are served while they last.
// Addresses that are not IPv4 are left out; zone and challenge names are the caller's to check.
func ZoneFile(zone string, serial uint32, doors []string, people, peopleDoor string, txt map[string][]string) string {
	doors = slices.DeleteFunc(slices.Clone(doors), func(d string) bool { return !ipv4(d) })
	recs := []Record{}
	for i, d := range doors {
		recs = append(recs, Record{fmt.Sprintf("ns%d", i+1), "A", d}, Record{"@", "A", d}, Record{"*", "A", d})
	}
	if ipv4(people) {
		recs = append(recs, Record{"mesh", "A", people})
	}
	if ipv4(peopleDoor) {
		recs = append(recs, Record{"*.mesh", "A", peopleDoor})
	}
	names := make([]string, 0, len(txt))
	for n := range txt {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rel := strings.TrimSuffix(strings.TrimSuffix(n, "."), "."+zone)
		for _, v := range txt[n] {
			recs = append(recs, Record{rel, "TXT", fmt.Sprintf("%q", v)})
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "$ORIGIN %s.\n$TTL 60\n@ IN SOA ns1.%s. hostmaster.%s. %d 3600 600 1209600 60\n", zone, zone, zone, serial)
	for i := range doors {
		fmt.Fprintf(&b, "@ IN NS ns%d.%s.\n", i+1, zone)
	}
	for _, r := range recs {
		fmt.Fprintf(&b, "%s IN %s %s\n", r.Name, r.Type, r.Value)
	}
	return b.String()
}

func ipv4(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4()
}

var ifaceName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// Corefile is Names' configuration: the zone from zoneFile, answered on the addresses of the named
// interface. CoreDNS's bind takes interface names, which is what works where a cloud NATs the public
// address 1:1 and it is on no interface of the box.
func Corefile(zone, iface, zoneFile string) (string, error) {
	if err := validate.DNSName(zone); err != nil {
		return "", err
	}
	if !ifaceName.MatchString(iface) {
		return "", fmt.Errorf("%q is not an interface name", iface)
	}
	return fmt.Sprintf("%s:53 {\n    bind %s\n    file %s {\n        reload 3s\n    }\n    errors\n}\n", zone, iface, zoneFile), nil
}

// DefaultRouteInterface is the interface of the IPv4 default route with the lowest metric, from the
// contents of /proc/net/route.
func DefaultRouteInterface(procNetRoute string) (string, error) {
	best, metric := "", -1
	for _, line := range strings.Split(procNetRoute, "\n") {
		f := strings.Fields(line) // Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		if m, err := strconv.Atoi(f[6]); err == nil && (metric < 0 || m < metric) {
			best, metric = f[0], m
		}
	}
	if best == "" {
		return "", errors.New("no IPv4 default route")
	}
	return best, nil
}

// Run writes routes and the zone every few seconds until ctx ends, and serves the ACME challenge
// endpoint (lego's httpreq provider) on addr.
func (e *Entrance) Run(ctx context.Context, addr string) error {
	e.txt = map[string][]string{}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("acme endpoint: %w", err)
	}
	srv := &http.Server{Handler: e, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	defer srv.Close()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		if err := e.Sync(ctx); err != nil {
			log.FromContext(ctx).Error(err, "entrance sync")
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-served:
			return fmt.Errorf("acme endpoint: %w", err)
		case <-t.C:
		}
	}
}

// read is the Fabric's settings and sites. The zone is checked here: it becomes Traefik rules and
// zone records.
func (e *Entrance) read(ctx context.Context) (*Settings, []v1alpha1.Site, error) {
	s, err := ReadSettings(ctx, e.Client)
	if err != nil {
		return nil, nil, err
	}
	if err := validate.DNSName(s.Zone); err != nil {
		return nil, nil, fmt.Errorf("fabric settings: zone: %w", err)
	}
	sites := &v1alpha1.SiteList{}
	if err := e.Client.List(ctx, sites); err != nil {
		return nil, nil, err
	}
	return s, sites.Items, nil
}

// Sync writes every route and the zone once. Only Run calls it.
func (e *Entrance) Sync(ctx context.Context) error {
	s, sites, err := e.read(ctx)
	if err != nil {
		return err
	}
	apps := &v1alpha1.AppList{}
	if err := e.Client.List(ctx, apps); err != nil {
		return err
	}
	byName := map[string]*v1alpha1.Site{}
	for i := range sites {
		byName[sites[i].Name] = &sites[i]
	}
	files := map[string]string{"people-only": peopleOnly(e.PeopleNet)} // name (no ".yml") -> body

	// The fabric's own names: the Console at the writer, the people mesh where it runs.
	if w := byName[e.writer(ctx, s, sites)]; w != nil && w.Manager() != nil && ipv4(w.Manager().IP) {
		files["console"] = RouteYAML(Route{Name: "console", Hostname: "console." + s.Zone, System: true,
			Servers: []string{fmt.Sprintf("http://%s:%d", w.Manager().IP, nebula.PortConsole)}}, s.Zone, e.TLS)
	}
	if s.People == e.Site {
		files["people"] = peopleRoutes(s.Zone, e.TLS)
	}

	// Every app, at its primary.
	routes, skipped := e.appRoutes(ctx, s.Zone, apps.Items, byName)
	for _, r := range routes {
		files[r.Name] = RouteYAML(r, s.Zone, e.TLS)
	}
	if why := strings.Join(skipped, "; "); why != e.skipped {
		if why != "" {
			log.FromContext(ctx).Info("left out of the Door", "why", skipped)
		}
		e.skipped = why
	}
	for name, body := range files {
		if err := writeIfChanged(filepath.Join(e.RoutesDir, name+".yml"), body); err != nil {
			return err
		}
	}
	if err := removeOthers(e.RoutesDir, files); err != nil {
		return err
	}
	return e.writeZone(s, sites)
}

// writer is the site whose Console the Door routes to: the Fabric's writer, or a steward that took
// over and whose commit has not reached this site yet. As at the writer itself, only stewards' own
// reports count and only a steward can be claimed (Effective). Missing peer answers cannot restore
// a superseded writer while Flux catches up; remembered authority still requires stewardship.
func (e *Entrance) writer(ctx context.Context, s *Settings, sites []v1alpha1.Site) string {
	stewards, claims := map[string]bool{}, []Claim{e.writerClaim}
	for _, site := range sites {
		if site.Spec.Steward {
			stewards[site.Name] = true
			if st, ok := e.Peers.Get(ctx, site.Name); ok && st.Writer != "" {
				claims = append(claims, Claim{Writer: st.Writer, Epoch: st.Epoch})
			}
		}
	}
	e.writerClaim = Effective(Claim{Writer: s.Writer, Epoch: s.Epoch}, claims, stewards)
	return e.writerClaim.Writer
}

// appRoutes are the apps' routes, each to where its primary answers, and what was left out and why:
// a name that is not a host name, one of the fabric's own names, or a name two apps claim (Traefik
// would pick one of them by chance); a server that is not the primary site's own.
//
// The primary is spec.primary and nothing else: a site that says it is active, while the primary is
// out of reach or a move is under way, draws no user (their sign-ins, their sessions) to its boxes.
func (e *Entrance) appRoutes(ctx context.Context, zone string, apps []v1alpha1.App, sites map[string]*v1alpha1.Site) ([]Route, []string) {
	names := func(a *v1alpha1.App) (public, mesh string) {
		if a.Spec.Mesh {
			mesh = MeshName(a, zone)
		}
		return a.Spec.Hostname, mesh
	}
	live := []v1alpha1.App{} // a deleted app is served nowhere, and claims no name, while it is removed
	for _, a := range apps {
		if !a.Spec.Deleted {
			live = append(live, a)
		}
	}
	apps = live
	claimed := map[string]int{}
	for i := range apps {
		p, m := names(&apps[i])
		claimed[p]++
		claimed[m]++
	}
	routes, skipped := []Route{}, []string{}
	for i := range apps {
		app := &apps[i]
		key := app.Namespace + "/" + app.Name
		if validate.Label(app.Namespace, 0) != nil || validate.Label(app.Name, 0) != nil {
			skipped = append(skipped, key+": its project or name is not a label")
			continue
		}
		r := Route{Name: app.Namespace + "." + app.Name}
		public, mesh := names(app)
		if public != "" {
			if err := publicName(public, zone); err != nil {
				skipped = append(skipped, key+": "+err.Error())
			} else if claimed[public] > 1 {
				skipped = append(skipped, key+": "+public+" is claimed by another app too")
			} else {
				r.Hostname = public
			}
		}
		if mesh != "" {
			if err := validate.DNSName(mesh); err != nil {
				skipped = append(skipped, key+": "+err.Error())
			} else if claimed[mesh] > 1 {
				skipped = append(skipped, key+": "+mesh+" is claimed by another app too")
			} else {
				r.MeshHost = mesh
			}
		}
		if r.Hostname == "" && r.MeshHost == "" {
			continue
		}
		p, eps := app.Spec.Primary, []string(nil)
		if st, ok := e.Peers.Get(ctx, p); ok {
			eps = st.Apps[key].Endpoints
		}
		for _, ep := range eps {
			if u, ok := endpointURL(sites[p], ep); ok {
				r.Servers = append(r.Servers, u)
			} else {
				skipped = append(skipped, fmt.Sprintf("%s: %s published %q, which is not one of its boxes on a NodePort", key, p, ep))
			}
		}
		routes = append(routes, r)
	}
	return routes, skipped
}

// publicName is whether an app may be served at h: a host name, and inside the zone one label the
// fabric does not use itself, so no app can stand in for the Console, NetBird or a mesh name.
func publicName(h, zone string) error {
	if err := validate.DNSName(h); err != nil {
		return err
	}
	if label, in := strings.CutSuffix(h, "."+zone); h == zone || in && (strings.Contains(label, ".") || validate.Reserved(label)) {
		return fmt.Errorf("%s is the fabric's own name", h)
	}
	return nil
}

// NodePorts are where a site's apps answer the Door.
const nodePortMin, nodePortMax = 30000, 32767

// endpointURL is the server URL for ep, published by site, when ep is ip:port with the IP one of the
// site's boxes (from the Fabric) and the port a NodePort: a site speaks only for itself, so it cannot
// point the Door at anything else, such as this box's loopback.
func endpointURL(site *v1alpha1.Site, ep string) (string, bool) {
	ap, err := netip.ParseAddrPort(ep)
	if err != nil || site == nil || ap.Port() < nodePortMin || ap.Port() > nodePortMax {
		return "", false
	}
	for _, b := range site.Spec.Boxes {
		if ip, err := netip.ParseAddr(b.IP); err == nil && ip == ap.Addr() {
			return "http://" + ap.String(), true
		}
	}
	return "", false
}

func (e *Entrance) writeZone(s *Settings, sites []v1alpha1.Site) error {
	people := ""
	for _, site := range sites {
		if site.Name == s.People && site.Spec.Public != nil {
			people = site.Spec.Public.Address
		}
	}
	peopleDoor := ""
	if s.People == e.Site && e.NetBirdIP != nil {
		peopleDoor = e.NetBirdIP()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	body := ZoneFile(s.Zone, 0, Doors(sites), people, peopleDoor, e.txt)
	if body == e.zone {
		return nil
	}
	serial := max(e.serial+1, uint32(time.Now().Unix()))
	if err := writeIfChanged(e.ZoneFile, ZoneFile(s.Zone, serial, Doors(sites), people, peopleDoor, e.txt)); err != nil {
		return err
	}
	e.serial, e.zone = serial, body
	return nil
}

// acmeValue is what ACME's DNS challenge puts in the record: base64url of a SHA-256 digest.
var acmeValue = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// challengeRecord checks a DNS-challenge record: _acme-challenge.<name>. for a name in the zone, with
// an ACME value.
func challengeRecord(fqdn, value, zone string) error {
	name, prefixed := strings.CutPrefix(fqdn, "_acme-challenge.")
	name, rooted := strings.CutSuffix(name, ".")
	if !prefixed || !rooted || validate.DNSName(name) != nil || name != zone && !strings.HasSuffix(name, "."+zone) {
		return fmt.Errorf("%q is not an ACME challenge in %s", fqdn, zone)
	}
	if !acmeValue.MatchString(value) {
		return errors.New("the value is not an ACME challenge's")
	}
	return nil
}

// ServeHTTP is lego's httpreq DNS provider: POST /present and /cleanup with {"fqdn", "value"}, from
// Traefik on this box with the pod's credentials.
func (e *Entrance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || (r.URL.Path != "/present" && r.URL.Path != "/cleanup") {
		http.NotFound(w, r)
		return
	}
	if host, _, _ := net.SplitHostPort(r.RemoteAddr); host != "127.0.0.1" && host != "::1" {
		http.Error(w, "local only", http.StatusForbidden)
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || e.User == "" || e.Password == "" ||
		subtle.ConstantTimeCompare([]byte(u), []byte(e.User))&subtle.ConstantTimeCompare([]byte(p), []byte(e.Password)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="door"`)
		http.Error(w, "credentials required", http.StatusUnauthorized)
		return
	}
	var in struct{ FQDN, Value string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		http.Error(w, "fqdn and value required", http.StatusBadRequest)
		return
	}
	s, sites, err := e.read(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := challengeRecord(in.FQDN, in.Value, s.Zone); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	vs := e.txt[in.FQDN]
	if r.URL.Path == "/present" {
		if !slices.Contains(vs, in.Value) {
			e.txt[in.FQDN] = append(vs, in.Value)
		}
	} else {
		vs = slices.DeleteFunc(vs, func(v string) bool { return v == in.Value })
		if len(vs) == 0 {
			delete(e.txt, in.FQDN)
		} else {
			e.txt[in.FQDN] = vs
		}
	}
	e.mu.Unlock()
	if err := e.writeZone(s, sites); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeIfChanged(path, body string) error {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeOthers deletes route files this entrance no longer writes.
func removeOthers(dir string, keep map[string]string) error {
	es, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range es {
		name, ok := strings.CutSuffix(e.Name(), ".yml")
		if _, kept := keep[name]; ok && !kept {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
