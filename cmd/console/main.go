// Command console serves the WeCoLab Console: a thin view over the Fabric API
// with the few actions that add boxes and peers and move an app.
package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/nebula"
	"wecolab.io/wecolab/internal/validate"
	"wecolab.io/wecolab/internal/warden"

	_ "golang.org/x/crypto/x509roots/fallback" // the image has no CA bundle: NetBird, B2 and Let's Encrypt names
)

//go:embed web
var web embed.FS

type server struct {
	dev       bool          // the development fabric (docs/development.md): its vault is plain http on the stand-in internet
	c         client.Client // this site's API, acting as the signed-in person
	site      string        // this site
	domain    string        // the fabric's zone
	publicURL string        // https://console.<zone>, where boxes join
	version   string        // WeCoLab's image tag, told to joining boxes
	dist      string        // what joining boxes download before a registry holds WeCoLab's images
	meshURL   string
	auth      *auth // nil: no sign-in (the development fabric)
	catalog   map[string]catalogEntry
	git       *fabric.Git // this site's copy of the Fabric; it takes commits only at the writer
	peers     *warden.Peers
}

// meshToken is the NetBird service token as the netbird Secret holds it now, "" when there is none: the
// writer rotates it and deletes the old one (internal/warden/people.go), so a value read at start expires.
func (s *server) meshToken(ctx context.Context) string {
	sec := &corev1.Secret{}
	if err := s.c.Get(s.elevated(ctx), types.NamespacedName{Namespace: warden.SystemNS, Name: "netbird"}, sec); err != nil {
		return ""
	}
	return string(sec.Data["token"])
}

// mesh reads one NetBird API listing; the console only reads the mesh, Warden writes it.
func (s *server) mesh(ctx context.Context, tok, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.meshURL+"/api"+path, nil)
	req.Header.Set("Authorization", "Token "+tok)
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// network: people's devices on the people mesh and what they may reach. Boxes are not on it (they talk
// over Nebula); a device belongs to the person who signed in with it, so it goes when they do. A peer
// with no owner joined with a setup key and outlives everyone: the page flags it.
func (s *server) network(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	ctx := r.Context()
	tok := s.meshToken(ctx)
	if tok == "" {
		writeJSON(w, map[string]any{"configured": false})
		return
	}
	var peers, users, groups, policies []map[string]any
	for path, out := range map[string]*[]map[string]any{"/peers": &peers, "/users": &users, "/groups": &groups, "/policies": &policies} {
		if err := s.mesh(ctx, tok, path, out); err != nil {
			http.Error(w, "mesh: "+err.Error(), 502)
			return
		}
	}
	owner := map[string]string{}
	for _, u := range users {
		owner[str(u, "id")] = str(u, "email") // service users have none
	}
	type device struct {
		Name, IP, OS, Owner, LastSeen string
		Connected                     bool
	}
	devices := []device{}
	for _, p := range peers {
		d := device{Name: str(p, "name"), IP: str(p, "ip"), OS: str(p, "os"), Owner: owner[str(p, "user_id")], LastSeen: str(p, "last_seen"), Connected: p["connected"] == true}
		// The Door joins with a setup key into its own group (install.sh): a system peer, not a stray one.
		if gs, _ := p["groups"].([]any); d.Owner == "" && slices.ContainsFunc(gs, func(g any) bool { m, _ := g.(map[string]any); return str(m, "name") == "door" }) {
			d.Owner = "the Door"
		}
		devices = append(devices, d)
	}
	gname := map[string]string{}
	for _, g := range groups {
		gname[str(g, "id")] = str(g, "name")
	}
	names := func(v any) string { // a rule's groups, as ids or as objects
		out := []string{}
		l, _ := v.([]any)
		for _, x := range l {
			switch t := x.(type) {
			case string:
				out = append(out, cmp.Or(gname[t], t))
			case map[string]any:
				out = append(out, cmp.Or(str(t, "name"), gname[str(t, "id")]))
			}
		}
		return strings.Join(out, ", ")
	}
	type rule struct {
		Policy, From, To, Proto, Ports string
		Enabled                        bool
	}
	rules := []rule{}
	for _, p := range policies {
		rl, _ := p["rules"].([]any)
		for _, x := range rl {
			m, _ := x.(map[string]any)
			ports := []string{}
			pl, _ := m["ports"].([]any)
			for _, q := range pl {
				if q, ok := q.(string); ok {
					ports = append(ports, q)
				}
			}
			rules = append(rules, rule{Policy: str(p, "name"), From: names(m["sources"]), To: names(m["destinations"]), Proto: str(m, "protocol"), Ports: strings.Join(ports, ","), Enabled: p["enabled"] == true})
		}
	}
	writeJSON(w, map[string]any{"configured": true, "devices": devices, "rules": rules})
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

type box struct {
	Name, IP, Role string
	Laptop, Ready  bool
	Added          time.Time
}

type site struct {
	Name, Owner, Public, Version string
	Nebula                       string // the site's own network, <prefix>.<index>.0/24
	Index                        int
	Steward, Writer, Ready       bool
	Boxes                        []box
}

type app struct {
	Namespace, Name, Primary, Active, Hostname, Workload, Database, Ready, Reason string
	Mesh                                                                          bool
	MeshName                                                                      string
	Sites                                                                         []string
	RPO                                                                           string
	Conditions                                                                    []metav1.Condition
	Handover                                                                      *v1alpha1.Handover // a planned move in flight, from Git, without its token
	Deleted                                                                       bool               // a person deleted it; its sites are removing their part
	Archive                                                                       map[string]int
}

// listApps lists what the person may see: everything for admins, else each project's namespace.
func (s *server) listApps(ctx context.Context, id *identity, out *v1alpha1.AppList) error {
	if id == nil || id.Admin {
		return s.c.List(ctx, out)
	}
	for _, p := range id.Projects {
		l := &v1alpha1.AppList{}
		if err := s.c.List(ctx, l, client.InNamespace(p)); err != nil {
			return err
		}
		out.Items = append(out.Items, l.Items...)
	}
	return nil
}

type pool struct {
	Name                 string
	Projects, Sites      []string
	CPU, Memory, Storage string
	Keys                 int
}

type offer struct {
	Name, Site, CPU, Memory, Storage string
	Boxes, To, Pools                 []string
	Keys, Holders                    int
	BestEffort                       bool
	Conditions                       []metav1.Condition
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sl := &v1alpha1.SiteList{}
	if err := s.c.List(ctx, sl); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	ol := &v1alpha1.OfferList{}
	_ = s.c.List(ctx, ol)
	offers := []offer{}
	for _, x := range ol.Items {
		offers = append(offers, offer{Name: x.Name, Site: x.Spec.Site, CPU: x.Spec.CPU, Memory: x.Spec.Memory, Storage: x.Spec.Storage, Boxes: x.Spec.Boxes, To: x.Spec.To, Pools: x.Spec.Pools, Keys: len(x.Spec.Keys), Holders: x.Status.Holders, BestEffort: x.Spec.BestEffort, Conditions: x.Status.Conditions})
	}
	// Pools are visible to everyone signed in: site managers choose what to pool with.
	pl := &v1alpha1.PoolList{}
	_ = s.c.List(ctx, pl)
	pools := []pool{}
	for _, x := range pl.Items {
		pools = append(pools, pool{Name: x.Name, Projects: x.Spec.Projects, Sites: x.Status.Sites, CPU: x.Spec.Quota.CPU, Memory: x.Spec.Quota.Memory, Storage: x.Spec.Quota.Storage, Keys: len(x.Spec.Keys)})
	}
	al := &v1alpha1.AppList{}
	if err := s.listApps(ctx, s.who(r), al); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	set, _ := warden.ReadSettings(s.elevated(ctx), s.c) // everyone sees who the writer is; not everyone may read the ConfigMap
	answers, _ := s.siteStatuses(ctx)
	sites := []site{}
	for _, x := range sl.Items {
		st := site{Name: x.Name, Owner: x.Spec.Owner, Index: x.Spec.Index, Steward: x.Spec.Steward, Writer: set != nil && set.Writer == x.Name}
		if set != nil {
			if a := nebula.Nth(set.Network, x.Spec.Index, 1); a.IsValid() {
				st.Nebula = netip.PrefixFrom(a, 24).Masked().String()
			}
		}
		if x.Spec.Public != nil {
			st.Public = x.Spec.Public.Address
		}
		a := answers[x.Name] // the site's own Warden answered
		ready := map[string]bool{}
		if a != nil {
			st.Ready, st.Version = true, a.Version
			for _, n := range a.Nodes {
				ready[n.Name] = n.Ready
			}
		}
		for _, b := range x.Spec.Boxes {
			st.Boxes = append(st.Boxes, box{Name: b.Name, IP: b.IP, Role: b.Role, Laptop: b.Laptop, Ready: ready[b.Name], Added: b.Added.Time})
		}
		sites = append(sites, st)
	}
	apps := []app{}
	for _, x := range al.Items {
		active, conds := warden.AppView(&x, answers, time.Now(), 24*time.Hour)
		rd := warden.Ready(conds)
		var h *v1alpha1.Handover // the demotion token is the promotion's credential: never on the page
		if x.Spec.Handover != nil {
			c := *x.Spec.Handover
			c.Token = ""
			h = &c
		}
		apps = append(apps, app{Namespace: x.Namespace, Name: x.Name, Primary: x.Spec.Primary, Active: active, Hostname: x.Spec.Hostname, Workload: x.Spec.Workload, Database: x.Spec.Database,
			Ready: string(rd.Status), Reason: rd.Reason, Sites: x.Spec.Sites, RPO: x.Spec.RPO.Duration.String(), Conditions: append(conds, rd), Handover: h, Deleted: x.Spec.Deleted,
			Archive: x.Spec.Archive, Mesh: x.Spec.Mesh, MeshName: warden.MeshName(&x, s.domain)})
	}
	if id := s.who(r); id != nil && !id.Admin {
		mine := func(p string) bool { return slices.Contains(id.Projects, p) }
		owns := func(site string) bool {
			for _, x := range sl.Items {
				if x.Name == site && mine(x.Spec.Owner) {
					return true
				}
			}
			return false
		}
		apps = slices.DeleteFunc(apps, func(a app) bool { return !mine(a.Namespace) })
		drawsFrom := func(name string) bool {
			for _, p := range pools {
				if p.Name == name && slices.ContainsFunc(p.Projects, mine) {
					return true
				}
			}
			return false
		}
		offers = slices.DeleteFunc(offers, func(o offer) bool {
			return !owns(o.Site) && !slices.ContainsFunc(o.To, mine) && !slices.ContainsFunc(o.Pools, drawsFrom)
		})
	}
	writeJSON(w, map[string]any{"domain": s.domain, "now": time.Now().UTC(), "sites": sites, "apps": apps, "offers": offers, "pools": pools})
}

// createOffer: a site manager shares boxes at their site with a size per holder and an audience.
func (s *server) createOffer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name, Site, CPU, Memory, Storage string
		Boxes, To, Pools                 []string
		BestEffort                       bool
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !nameRe.MatchString(in.Name) {
		http.Error(w, "name must be a DNS label", 400)
		return
	}
	if in.CPU == "" || in.Memory == "" {
		http.Error(w, "cpu and memory are required", 400)
		return
	}
	if err := projectNames(in.To); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ctx := r.Context()
	if exists, err := s.fromGit(ctx, &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: in.Site}}); err != nil || !exists {
		answer(w, cmpErr(err, fail(404, "no such site")), 502)
		return
	}
	if !s.canSite(w, r, in.Site) {
		return
	}
	for _, p := range clean(in.Pools) {
		if exists, err := s.fromGit(ctx, &v1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: p}}); err != nil || !exists {
			answer(w, cmpErr(err, fail(404, "no such pool %s", p)), 502)
			return
		}
	}
	o := &v1alpha1.Offer{ObjectMeta: metav1.ObjectMeta{Name: in.Name}, Spec: v1alpha1.OfferSpec{Site: in.Site, Boxes: clean(in.Boxes), CPU: in.CPU, Memory: in.Memory, Storage: in.Storage, To: clean(in.To), Pools: clean(in.Pools), BestEffort: in.BestEffort}}
	if err := s.create(s.elevated(ctx), o, "offer "+o.Name+" at "+o.Spec.Site); err != nil {
		answer(w, err, 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// offerOf reads an offer from Git for someone who manages its site; false once it has answered.
func (s *server) offerOf(w http.ResponseWriter, r *http.Request) (*v1alpha1.Offer, bool) {
	o := &v1alpha1.Offer{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("name")}}
	if exists, err := s.fromGit(r.Context(), o); err != nil || !exists {
		answer(w, cmpErr(err, fail(404, "no such offer")), 502)
		return nil, false
	}
	return o, s.canSite(w, r, o.Spec.Site)
}

func (s *server) deleteOffer(w http.ResponseWriter, r *http.Request) {
	o, ok := s.offerOf(w, r)
	if !ok {
		return
	}
	if err := s.remove(s.elevated(r.Context()), o, "offer "+o.Name+" withdrawn"); err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// mintKey makes a single-use code "<prefix>.<name>.<secret>" valid 24 h; only its hash is kept.
func mintKey(prefix, name string) (string, v1alpha1.OfferKey) {
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	enc := base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(enc))
	return prefix + "." + name + "." + enc, v1alpha1.OfferKey{Hash: hex.EncodeToString(sum[:]), Expires: metav1.NewTime(time.Now().Add(24 * time.Hour).UTC())}
}

// useKey removes the key the secret matches (and any expired ones); false when none matches.
func useKey(keys []v1alpha1.OfferKey, secret string) ([]v1alpha1.OfferKey, bool) {
	sum := sha256.Sum256([]byte(secret))
	want := hex.EncodeToString(sum[:])
	found, out := false, []v1alpha1.OfferKey{}
	for _, k := range keys {
		switch {
		case k.Hash == want && k.Expires.After(time.Now()):
			found = true
		case k.Expires.After(time.Now()):
			out = append(out, k)
		}
	}
	return out, found
}

// offerKey mints a single-use code for an offer, valid 24 h: whoever redeems it holds the offer.
func (s *server) offerKey(w http.ResponseWriter, r *http.Request) {
	o, ok := s.offerOf(w, r)
	if !ok {
		return
	}
	code, key := mintKey("wclo1", o.Name)
	err := s.editObj(s.elevated(r.Context()), o, "offer "+o.Name+": a one-time key", func(exists bool) (client.Object, error) {
		if !exists {
			return nil, fail(404, "no such offer")
		}
		o.Spec.Keys = append(o.Spec.Keys, key)
		return o, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"code": code, "expires": key.Expires.Time, "offer": o.Name, "site": o.Spec.Site})
}

// Pools: admins create them and set the tier; site managers offer into them; projects
// join by an admin's choice or by redeeming a pool key.

func (s *server) createPool(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	var in struct {
		Name, CPU, Memory, Storage string
		Projects                   []string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !nameRe.MatchString(in.Name) {
		http.Error(w, "name must be a DNS label", 400)
		return
	}
	if err := projectNames(in.Projects); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	p := &v1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: in.Name}, Spec: v1alpha1.PoolSpec{Projects: clean(in.Projects),
		Quota: v1alpha1.PoolQuota{CPU: strings.TrimSpace(in.CPU), Memory: strings.TrimSpace(in.Memory), Storage: strings.TrimSpace(in.Storage)}}}
	if err := s.create(r.Context(), p, "pool "+p.Name); err != nil {
		answer(w, err, 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) deletePool(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	if err := s.remove(r.Context(), &v1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("name")}}, "pool "+r.PathValue("name")+" removed"); err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) poolKey(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	p := &v1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("name")}}
	code, key := mintKey("wclp1", p.Name)
	err := s.editObj(r.Context(), p, "pool "+p.Name+": a one-time key", func(exists bool) (client.Object, error) {
		if !exists {
			return nil, fail(404, "no such pool")
		}
		p.Spec.Keys = append(p.Spec.Keys, key)
		return p, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"code": code, "expires": key.Expires.Time, "pool": p.Name})
}

// redeemOffer turns a code into membership: an offer key makes the project a holder of
// that offer; a pool key makes it draw from that pool. The key is taken out of the file as Git holds
// it, in the same conditional commit, so it is never redeemed twice.
func (s *server) redeemOffer(w http.ResponseWriter, r *http.Request) {
	var in struct{ Code, Project string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || validate.Name(in.Project) != nil {
		http.Error(w, "project must be a project's name", 400)
		return
	}
	if !s.can(w, r, in.Project) {
		return
	}
	parts := strings.Split(strings.TrimSpace(in.Code), ".")
	if len(parts) != 3 || (parts[0] != "wclo1" && parts[0] != "wclp1") || validate.Label(parts[1], 0) != nil {
		http.Error(w, "not an offer or pool key", 400)
		return
	}
	ctx := s.elevated(r.Context()) // checked above: the person may act for this project
	redeem := func(keys *[]v1alpha1.OfferKey, members *[]string) func(bool) error {
		return func(exists bool) error {
			left, ok := useKey(*keys, parts[2])
			if !exists || !ok {
				return fail(403, "unknown, used or expired key")
			}
			*keys = left
			if !slices.Contains(*members, in.Project) {
				*members = append(*members, in.Project)
			}
			return nil
		}
	}
	if parts[0] == "wclp1" {
		p := &v1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: parts[1]}}
		use := redeem(&p.Spec.Keys, &p.Spec.Projects)
		if err := s.editObj(ctx, p, "pool "+p.Name+": project "+in.Project+" joins with a key", func(exists bool) (client.Object, error) { return p, use(exists) }); err != nil {
			answer(w, err, 502)
			return
		}
		writeJSON(w, map[string]any{"pool": p.Name, "project": in.Project})
		return
	}
	o := &v1alpha1.Offer{ObjectMeta: metav1.ObjectMeta{Name: parts[1]}}
	use := redeem(&o.Spec.Keys, &o.Spec.To)
	if err := s.editObj(ctx, o, "offer "+o.Name+": project "+in.Project+" holds it with a key", func(exists bool) (client.Object, error) { return o, use(exists) }); err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"offer": o.Name, "site": o.Spec.Site, "project": in.Project})
}

func clean(in []string) []string {
	out := []string{}
	for _, x := range in {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// projectNames checks names given as projects: a system namespace never becomes a tenant.
func projectNames(names []string) error {
	for _, p := range clean(names) {
		if err := validate.Name(p); err != nil {
			return fmt.Errorf("project: %w", err)
		}
	}
	return nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

// setPrimary moves an app's primary, as a change to its spec in Git: planned, the old primary hands
// over with a token; forced, every other site's database is rebuilt from the vault
// (docs/plans/2026-09-29-hardening.md, R1).
func (s *server) setPrimary(w http.ResponseWriter, r *http.Request) {
	if !s.can(w, r, r.PathValue("ns")) {
		return
	}
	var in struct {
		To    string
		Force bool
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.To == "" {
		http.Error(w, "to required", 400)
		return
	}
	ctx := r.Context()
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: r.PathValue("ns"), Name: r.PathValue("app")}}
	msg := fmt.Sprintf("%s/%s: primary %s", a.Namespace, a.Name, in.To)
	if in.Force {
		msg += ", forced; the other sites' databases are rebuilt from the vault"
	}
	id := randHexStr(16)
	answers, _ := s.siteStatuses(ctx)
	err := s.editObj(ctx, a, msg, func(exists bool) (client.Object, error) {
		if !exists {
			return nil, fail(404, "no such app")
		}
		spec, err := move(a.Spec, in.To, id, in.Force)
		if err != nil {
			return nil, fail(409, "%v", err)
		}
		// Roles come from Git alone, so whether a planned move can complete is checked here, against what
		// the sites report. A forced move is for when they cannot; a target without the database holds.
		if !in.Force && spec.Primary != a.Spec.Primary {
			if err := warden.MoveGates(a, answers, in.To, time.Now()); err != nil {
				return nil, fail(409, "%v", err)
			}
		}
		a.Spec = spec
		return a, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// move is an app's spec with a new primary. Without a database there is nothing to hand over.
func move(spec v1alpha1.AppSpec, to, id string, force bool) (v1alpha1.AppSpec, error) {
	switch {
	case spec.Database == "" && !slices.Contains(spec.Sites, to):
		return spec, fmt.Errorf("%s is not one of the app's sites", to)
	case spec.Database == "":
		spec.Primary, spec.Handover = to, nil
		return spec, nil
	case force:
		return warden.ForcedMove(spec, to)
	}
	return warden.PlannedMove(spec, to, id)
}

func (s *server) setMesh(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("app")
	if !s.can(w, r, ns) {
		return
	}
	var in struct{ Mesh *bool }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Mesh == nil {
		http.Error(w, "mesh (true or false) required", 400)
		return
	}
	ctx := r.Context()
	if *in.Mesh {
		apps, err := s.appsInGit(ctx)
		if err == nil {
			err = meshFree(apps, ns, name)
		}
		if err != nil {
			answer(w, err, 502)
			return
		}
	}
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	err := s.editObj(ctx, a, fmt.Sprintf("%s/%s: on the mesh %v", ns, name, *in.Mesh), func(exists bool) (client.Object, error) {
		if !exists {
			return nil, fail(404, "no such app")
		}
		a.Spec.Mesh = *in.Mesh
		return a, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// resources: what each site has and uses, and which workloads run where, as each site's Warden
// publishes it.
func (s *server) resources(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	answers, all := s.siteStatuses(r.Context())
	type site struct {
		Name                               string
		Ready                              bool
		Version                            string
		Nodes, NodesReady                  int64
		Allocatable, Allocated, Allocating map[string]string
	}
	sites := []site{}
	type placement struct {
		Namespace, Kind, Name string
		Sites                 map[string]string // site -> health
	}
	byName := map[string]*placement{}
	for _, x := range all {
		st := site{Name: x.Name}
		a := answers[x.Name]
		if a == nil {
			sites = append(sites, st)
			continue
		}
		st.Ready, st.Version, st.Allocatable, st.Allocated = true, a.Version, a.Allocatable, a.Requested
		for _, n := range a.Nodes {
			st.Nodes++
			if n.Ready {
				st.NodesReady++
			}
		}
		sites = append(sites, st)
		for key, wl := range a.Workloads {
			p := byName[key]
			if p == nil {
				ns, name, _ := strings.Cut(key, "/")
				p = &placement{Namespace: ns, Kind: "Deployment", Name: name, Sites: map[string]string{}}
				byName[key] = p
			}
			switch {
			case wl.Replicas == 0:
				p.Sites[x.Name] = "Stopped"
			case wl.Ready >= wl.Replicas:
				p.Sites[x.Name] = "Healthy"
			default:
				p.Sites[x.Name] = "Unhealthy"
			}
		}
	}
	placements := []placement{}
	for _, p := range byName {
		placements = append(placements, *p)
	}
	writeJSON(w, map[string]any{"sites": sites, "placements": placements})
}

// settings: integrations whose secrets are stored on the fabric and never shown back.
const settingsNS = "wecolab-system"

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Version is set at build time.
var Version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil))) // log.Printf's lines too
	listen := flag.String("listen", ":8080", "address to serve on")
	site := flag.String("site", os.Getenv("WECOLAB_SITE"), "this site")
	dist := flag.String("dist", "/dist", "what joining boxes download")
	flag.Parse()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	cfg := ctrl.GetConfigOrDie()
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return impersonating{rt} })
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatal(err)
	}
	set, err := warden.ReadSettings(context.Background(), c)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{c: c, site: *site, domain: set.Zone, publicURL: "https://console." + set.Zone, version: Version, dist: *dist,
		meshURL: os.Getenv("NETBIRD_URL"), git: fabric.GitFromEnv(), peers: &warden.Peers{Client: c}}
	if s.git == nil {
		log.Fatal("WECOLAB_GIT_URL and WECOLAB_GIT_TOKEN are required")
	}
	s.catalog = loadCatalog()
	s.dev = set.Dev
	if s.meshURL != "" {
		key, err := base64.StdEncoding.DecodeString(os.Getenv("WECOLAB_SESSION_KEY"))
		if err != nil || len(key) < 32 {
			log.Fatal("WECOLAB_SESSION_KEY must be 32 bytes or more, base64")
		}
		s.auth = newAuth(s.meshURL+"/oauth2", "", "", s.publicURL+"/oauth/callback", key)
		log.Printf("sign-in at %s", s.auth.issuer)
	} else if !set.Dev {
		log.Fatal("NETBIRD_URL is missing: outside the development fabric the Console never runs without sign-in")
	} else {
		log.Printf("no people mesh: everyone is the owner (development fabric only)")
	}
	log.Printf("console %s at site %s on %s", Version, s.site, *listen)
	srv := &http.Server{Addr: *listen, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// handler is everything the Console serves. Cross-origin protection refuses state-changing requests a
// browser marks as coming from another origin: a member's app at <app>.<zone> is same-site, not
// same-origin, so it cannot act for an admin who visits it. Requests without Origin or Sec-Fetch-Site
// (install.sh's POST /join, the Mac) are not from a browser and pass. Nor may that app frame the
// Console: clicks in a framed page are same-origin, and the session cookie goes along (SameSite=Lax).
func (s *server) handler() http.Handler {
	next := http.NewCrossOriginProtection().Handler(s.protect(s.routes()))
	return logged(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	}))
}

// request is what the log line of one request says; protect and the sign-in fill in who and event.
type request struct{ id, who, event string }

type requestKey struct{}

func noted(ctx context.Context) *request {
	if q, _ := ctx.Value(requestKey{}).(*request); q != nil {
		return q
	}
	return &request{}
}

// logged gives every request an id, returned as X-Request-Id, shown with the page's errors and written
// into the commits it makes, and logs one JSON line for each request that changes something or is
// refused: who, what, the status, and for a refusal the reason the person saw. Reads that succeed, and
// the 404s of whoever probes a public address, are not logged. Never logged: query strings (invite
// tokens, sign-in codes), headers, cookies, bodies. Events use OWASP's logging vocabulary.
func logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := &request{id: strings.ToLower(rand.Text()[:12])}
		w.Header().Set("X-Request-Id", q.id)
		rec := &recorder{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(fabric.WithRequest(context.WithValue(r.Context(), requestKey{}, q), q.id)))
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		switch {
		case q.event != "":
		case rec.status == 403:
			q.event = "authz_fail"
		case rec.status == 400:
			q.event = "input_validation_fail"
		}
		if read && q.event == "" && rec.status < 500 {
			return
		}
		attrs := []any{"id", q.id, "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", time.Since(start).Milliseconds(), "who", q.who, "ip", r.Header.Get("X-Forwarded-For")}
		if q.event != "" {
			attrs = append(attrs, "event", q.event)
		}
		level := slog.LevelInfo
		if rec.status >= 400 {
			level = slog.LevelWarn
			attrs = append(attrs, "reason", strings.TrimSpace(rec.reason.String()))
		}
		if rec.status >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request", attrs...)
	})
}

// recorder keeps a response's status and the start of a refusal's text.
type recorder struct {
	http.ResponseWriter
	status int
	reason bytes.Buffer
}

func (w *recorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.status >= 400 && w.reason.Len() < 300 {
		w.reason.Write(b[:min(len(b), 300-w.reason.Len())])
	}
	return w.ResponseWriter.Write(b)
}

// Flush lets streamed responses through the recorder.
func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *server) routes() *http.ServeMux {
	static, _ := fs.Sub(web, "web")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	// Public: joining boxes, and people accepting their invite.
	mux.HandleFunc("POST /join", s.join)
	mux.HandleFunc("GET /join.sh", s.joinScript)
	mux.HandleFunc("GET /dl/{file}", s.download)
	mux.HandleFunc("GET /fabric/{path...}", s.fabricFile)
	mux.HandleFunc("/invite", s.invitePage)
	if s.auth != nil {
		mux.HandleFunc("GET /oauth/login", s.login)
		mux.HandleFunc("GET /oauth/callback", s.callback)
		mux.HandleFunc("GET /oauth/logout", s.logout)
	}
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("POST /api/me/keys", s.myKeys)
	mux.HandleFunc("GET /api/members", s.members)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("POST /api/members/invite", s.inviteMember)
	mux.HandleFunc("POST /api/members/{id}", s.setMember)
	mux.HandleFunc("DELETE /api/members/{id}", s.deleteMember)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/sites", s.inviteSite)
	mux.HandleFunc("POST /api/sites/{site}/boxes", s.inviteBox)
	mux.HandleFunc("DELETE /api/sites/{site}/boxes/{box}", s.removeBox)
	mux.HandleFunc("DELETE /api/sites/{site}", s.removeSite)
	mux.HandleFunc("POST /api/sites/{site}/steward", s.setSteward)
	mux.HandleFunc("GET /api/network", s.network)
	mux.HandleFunc("POST /api/offers", s.createOffer)
	mux.HandleFunc("DELETE /api/offers/{name}", s.deleteOffer)
	mux.HandleFunc("POST /api/offers/{name}/key", s.offerKey)
	mux.HandleFunc("POST /api/offers/redeem", s.redeemOffer)
	mux.HandleFunc("POST /api/pools", s.createPool)
	mux.HandleFunc("DELETE /api/pools/{name}", s.deletePool)
	mux.HandleFunc("POST /api/pools/{name}/key", s.poolKey)
	mux.HandleFunc("POST /api/apps/{ns}/{app}/primary", s.setPrimary)
	mux.HandleFunc("POST /api/apps/{ns}/{app}/mesh", s.setMesh)
	mux.HandleFunc("POST /api/deploy", s.deploy)
	mux.HandleFunc("DELETE /api/apps/{ns}/{app}", s.deleteApp)
	mux.HandleFunc("GET /api/domains", s.domains)
	mux.HandleFunc("POST /api/domains", s.addDomain)
	mux.HandleFunc("DELETE /api/domains/{domain}", s.removeDomain)
	mux.HandleFunc("GET /api/resources", s.resources)
	mux.HandleFunc("GET /api/storage", s.storage)
	mux.HandleFunc("POST /api/storage/vault", s.createVault)
	mux.HandleFunc("POST /api/storage/vault/{project}/rotate", s.rotateVault)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("POST /api/settings/storage", s.setStorage)
	mux.HandleFunc("POST /api/settings/takeover", s.takeover)
	mux.Handle("/", http.FileServer(http.FS(static)))
	return mux
}
