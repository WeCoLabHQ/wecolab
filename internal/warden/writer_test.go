package warden

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

func TestEffectiveIgnoresNonStewards(t *testing.T) {
	stewards := map[string]bool{"pub": true}
	if e := Effective(Claim{"home", 5}, []Claim{{"pub", 3}}, stewards); e.Writer != "pub" {
		t.Fatalf("a site that is no longer a steward is not the writer, even by its own copy: %v", e)
	}
	if e := Effective(Claim{"home", 5}, nil, stewards); e.Writer != "" {
		t.Fatalf("no steward claims: no writer: %v", e)
	}
}

// fp is a certificate fingerprint made of one hex digit.
func fp(c string) string { return strings.Repeat(c, 64) }

func TestRevoke(t *testing.T) {
	now := time.Now()
	later, past := now.Add(time.Hour), now.Add(-time.Hour)
	list := []Revoked{{fp("0"), past}, {fp("1"), later}, {fp("1"), later}}
	issued := map[string][]Issued{
		"gone": {{fp("2"), later}, {fp("3"), past}, {fp("1"), later}, {fp("4") + " 2099-01-01T00:00:00Z\n" + fp("5"), later}},
		"here": {{fp("6"), later}},
	}
	got, changed := Revoke(list, map[string]bool{"here": true}, issued, now)
	fps := []string{}
	for _, r := range got {
		fps = append(fps, r.Fingerprint)
	}
	if !changed || !slices.Equal(fps, []string{fp("1"), fp("2")}) {
		t.Fatalf("a gone box's live certificates are added once, expired entries and non-fingerprints dropped: %v", fps)
	}
	if _, changed := Revoke(got, map[string]bool{"here": true}, issued, now); changed {
		t.Fatal("nothing more to do")
	}
}

// fakeCopy is a site's Forgejo as the Writer uses it: branch protection, push mirrors, main's head,
// branches, files on main and commits to them, and deleting and creating the repository.
type fakeCopy struct {
	mu        sync.Mutex
	head      string
	exists    bool
	collab    bool
	pushers   []string
	mirrors   []fabric.PushMirror
	branches  []string
	files     map[string]string
	recreated int
	syncs     int
	commits   int
}

func (f *fakeCopy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/fabric/fabric")
	switch {
	case r.URL.Path == "/api/v1/user/repos" && r.Method == http.MethodPost:
		f.exists = true
	case !f.exists:
		http.NotFound(w, r)
	case p == "" && r.Method == http.MethodDelete:
		f.exists, f.collab, f.head, f.mirrors, f.branches, f.pushers = false, false, "", nil, nil, nil
		f.recreated++
	case p == "":
	case p == "/collaborators/mirror" && r.Method == http.MethodPut:
		f.collab = true
	case p == "/collaborators/mirror" && !f.collab:
		http.NotFound(w, r)
	case p == "/collaborators/mirror":
		w.WriteHeader(http.StatusNoContent)
	case p == "/branch_protections/main" && r.Method == http.MethodGet && f.pushers == nil:
		http.NotFound(w, r)
	case p == "/branch_protections/main" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"push_whitelist_usernames": f.pushers})
	case strings.HasPrefix(p, "/branch_protections"):
		var b struct {
			Pushers []string `json:"push_whitelist_usernames"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.pushers = b.Pushers
	case p == "/branches/main" && f.head == "":
		http.NotFound(w, r)
	case p == "/branches/main":
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"id": f.head}})
	case p == "/branches" && r.Method == http.MethodPost:
		var b struct {
			Name string `json:"new_branch_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if slices.Contains(f.branches, b.Name) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.branches = append(f.branches, b.Name)
	case p == "/push_mirrors" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(append([]fabric.PushMirror{}, f.mirrors...))
	case p == "/push_mirrors" && r.Method == http.MethodPost:
		var b struct {
			Remote string `json:"remote_address"`
			Filter string `json:"branch_filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mirrors = append(f.mirrors, fabric.PushMirror{Name: "m" + b.Filter, Remote: b.Remote, Filter: b.Filter})
	case strings.HasPrefix(p, "/push_mirrors/") && r.Method == http.MethodDelete:
		f.mirrors = slices.DeleteFunc(f.mirrors, func(m fabric.PushMirror) bool { return m.Name == strings.TrimPrefix(p, "/push_mirrors/") })
	case p == "/push_mirrors-sync":
		f.syncs++
	case strings.HasPrefix(p, "/contents/") && r.Method == http.MethodGet:
		f.contents(w, r, strings.TrimPrefix(p, "/contents/"))
	case p == "/contents" && r.Method == http.MethodPost:
		var b struct {
			Files []struct{ Operation, Path, Content string }
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, c := range b.Files {
			d, _ := base64.StdEncoding.DecodeString(c.Content)
			if f.files[c.Path] = string(d); c.Operation == "delete" {
				delete(f.files, c.Path)
			}
		}
		f.commits++
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c" + strconv.Itoa(f.commits)}})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusTeapot)
	}
}

// contents answers the contents API: a file, or a folder's files.
func (f *fakeCopy) contents(w http.ResponseWriter, r *http.Request, p string) {
	if c, ok := f.files[p]; ok {
		_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "path": p, "sha": fmt.Sprintf("%x", sha1.Sum([]byte(c))),
			"content": base64.StdEncoding.EncodeToString([]byte(c))})
		return
	}
	list := []map[string]string{}
	for k := range f.files {
		if strings.HasPrefix(k, p+"/") {
			list = append(list, map[string]string{"type": "file", "path": k})
		}
	}
	if len(list) == 0 {
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(list)
}

func (f *fakeCopy) file(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[p]
}

// toServer sends every request to one test server, whatever its address: the writer's Warden here.
type toServer struct{ url string }

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(s.url, "http://")
	return http.DefaultTransport.RoundTrip(r)
}

func TestFollow(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	sites := []v1alpha1.Site{{Spec: v1alpha1.SiteSpec{Steward: true, Boxes: []v1alpha1.Box{{Name: "pub-a", IP: "10.77.0.1", Role: "manager"}}}}}
	sites[0].Name = "pub"
	const other = "2222222222222222222222222222222222222222"
	cases := []struct {
		name      string
		steward   bool
		claimed   bool // this copy names this site the writer
		pushed    bool // a push reaches this copy while the writer is asked
		answer    CopyAnswer
		recreated bool
		aside     bool
	}{
		{"behind the writer", false, false, false, CopyAnswer{Head: other, OnMain: true, Has: true}, false, false},
		{"a non-steward that went its own way", false, false, false, CopyAnswer{Head: other}, true, false},
		{"never from an empty writer", false, false, false, CopyAnswer{}, false, false},
		{"not when a push arrived meanwhile", false, false, true, CopyAnswer{Head: other}, false, false},
		{"not a copy naming itself the writer", false, true, false, CopyAnswer{Head: other}, false, false},
		{"a copy naming itself the writer, once the writer holds it", false, true, false, CopyAnswer{Head: other, Has: true}, true, false},
		{"a steward keeps its commits aside first", true, true, false, CopyAnswer{Head: other}, false, true},
		{"a steward whose commits the writer holds", true, true, false, CopyAnswer{Head: other, Has: true}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cp := &fakeCopy{head: head, exists: true, collab: true, pushers: []string{"mirror", "fabric"},
				mirrors: []fabric.PushMirror{{Name: "old", Remote: "http://10.77.1.1:30300/fabric/fabric.git", Filter: "main"}}}
			fj := httptest.NewServer(cp)
			defer fj.Close()
			writer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/fabric/commits/"+head {
					t.Errorf("asked %s", r.URL.Path)
				}
				if c.pushed {
					cp.mu.Lock()
					cp.head = "3333333333333333333333333333333333333333"
					cp.mu.Unlock()
				}
				_ = json.NewEncoder(w).Encode(c.answer)
			}))
			defer writer.Close()
			w := &Writer{Site: "home", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()},
				HTTP: &http.Client{Transport: toServer{writer.URL}}}
			w.mirrors = map[string]mirrorState{}
			if c.steward { // a steward has the writer's mirroring password
				w.Client = fakeWithSecret(t, "pub", "pw")
			}
			if err := w.follow(context.Background(), sites, "pub", c.steward, c.claimed); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cp.pushers, []string{"mirror"}) {
				t.Fatalf("main takes the writer's pushes only: %v", cp.pushers)
			}
			if (cp.recreated > 0) != c.recreated || !cp.exists || !cp.collab {
				t.Fatalf("recreated %d, want %v; the copy is whole: %v %v", cp.recreated, c.recreated, cp.exists, cp.collab)
			}
			aside := len(cp.branches) == 1 && strings.HasPrefix(cp.branches[0], "superseded-home-111111111111") &&
				len(cp.mirrors) == 1 && cp.mirrors[0].Filter == supersededFilter && cp.mirrors[0].Remote == "http://10.77.0.1:30300/fabric/fabric.git"
			if aside != c.aside || !c.aside && len(cp.mirrors) != 0 {
				t.Fatalf("aside %v, want %v: %v %v", aside, c.aside, cp.branches, cp.mirrors)
			}
			if c.aside { // the next pass pushes nothing new while the branch is on its way
				syncs := cp.syncs
				if err := w.follow(context.Background(), sites, "pub", true, true); err != nil || cp.syncs != syncs || len(cp.branches) != 1 {
					t.Fatalf("the same branch and mirror stay: %v %d %v", err, cp.syncs, cp.branches)
				}
			}
		})
	}
}

func TestEnsureFinishesAHalfDoneRecreate(t *testing.T) {
	cp := &fakeCopy{}
	fj := httptest.NewServer(cp)
	defer fj.Close()
	g := &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}
	if err := g.Ensure(context.Background(), ForgejoMirror); err != nil || !cp.exists || !cp.collab {
		t.Fatalf("a missing copy is made again: %v %v %v", err, cp.exists, cp.collab)
	}
	cp.collab = false
	if err := g.Ensure(context.Background(), ForgejoMirror); err != nil || !cp.collab || cp.recreated != 0 {
		t.Fatal("a missing collaborator is added back, and nothing is deleted")
	}
}

func TestLeadNeverPushesAnEmptyCopy(t *testing.T) {
	cp := &fakeCopy{exists: true, collab: true, mirrors: []fabric.PushMirror{{Name: "m", Remote: forgejoURL("10.77.1.1"), Filter: "main"}}}
	fj := httptest.NewServer(cp)
	defer fj.Close()
	sites := []v1alpha1.Site{{Spec: v1alpha1.SiteSpec{Boxes: []v1alpha1.Box{{Name: "home-a", IP: "10.77.1.1", Role: "manager"}}}}}
	sites[0].Name = "home"
	w := &Writer{Site: "pub", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}, Client: fakeWithSecret(t, "home", "pw")}
	holds := Claim{Writer: "pub", Epoch: 1}          // what a copy holding the Fabric's settings names
	_ = w.lead(context.Background(), sites, Claim{}) // revoke and reseed need more than this fake has
	if len(cp.mirrors) != 0 || !slices.Equal(cp.pushers, []string{"mirror"}) {
		t.Fatalf("an empty writer's copy pushes to nobody and admits no Console: %v %v", cp.mirrors, cp.pushers)
	}
	cp.head = "1111111111111111111111111111111111111111"
	_ = w.lead(context.Background(), sites, Claim{})
	if len(cp.mirrors) != 0 || !slices.Equal(cp.pushers, []string{"mirror"}) {
		t.Fatalf("a copy without the Fabric's settings pushes to nobody and admits no Console: %v %v", cp.mirrors, cp.pushers)
	}
	_ = w.lead(context.Background(), sites, holds)
	if len(cp.mirrors) != 1 || cp.syncs != 1 || !slices.Equal(cp.pushers, []string{"mirror", "fabric"}) {
		t.Fatalf("with the Fabric, it pushes and admits the Console: %v %v", cp.mirrors, cp.pushers)
	}
	_ = w.lead(context.Background(), sites, holds)
	if cp.syncs != 1 {
		t.Fatal("a mirror set up with the current password stays")
	}
	w.Client = fakeWithSecret(t, "home", "changed")
	_ = w.lead(context.Background(), sites, holds)
	if len(cp.mirrors) != 1 || cp.syncs != 2 {
		t.Fatalf("a changed password sets the mirror up again: %v %d", cp.mirrors, cp.syncs)
	}
}

// peer is every other site's Warden at once: its status, the certificates it issued, and its copy.
func peer(st SiteStatus, issued map[string][]Issued) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/status":
			_ = json.NewEncoder(w).Encode(st)
		case r.URL.Path == "/nebula/issued":
			_ = json.NewEncoder(w).Encode(issued)
		default:
			http.NotFound(w, r)
		}
	}))
}

func settingsFile(t *testing.T, writer string, epoch int, blocklist string) string {
	cm := &corev1.ConfigMap{Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": writer,
		"epoch": strconv.Itoa(epoch), "blocklist": blocklist}}
	cm.APIVersion, cm.Kind, cm.Name, cm.Namespace = "v1", "ConfigMap", SettingsName, SystemNS
	b, err := yaml.Marshal(cm)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func siteFile(t *testing.T, s v1alpha1.Site) string {
	b, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func testSite(name string, steward bool, ip string, boxes ...string) v1alpha1.Site {
	s := v1alpha1.Site{Spec: v1alpha1.SiteSpec{Steward: steward, Boxes: []v1alpha1.Box{{Name: name + "-a", IP: ip, Role: "manager"}}}}
	for _, b := range boxes {
		s.Spec.Boxes = append(s.Spec.Boxes, v1alpha1.Box{Name: b, Role: "node"})
	}
	s.Name = name
	return s
}

// Taking over is a commit at the new writer; until its Flux applies it, the cluster there (and every
// other site) still names the old writer. The new writer leads from its own copy at once.
func TestSyncLeadsByItsOwnCopy(t *testing.T) {
	cp := &fakeCopy{head: "1111111111111111111111111111111111111111", exists: true, collab: true,
		files: map[string]string{SettingsPath: settingsFile(t, "home", 2, "")}}
	fj := httptest.NewServer(cp)
	defer fj.Close()
	pr := peer(SiteStatus{Writer: "pub", Epoch: 1}, nil)
	defer pr.Close()
	cluster := &corev1.ConfigMap{Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": "pub", "epoch": "1"}}
	cluster.Name, cluster.Namespace = SettingsName, SystemNS
	home, pub := testSite("home", true, "10.77.1.1"), testSite("pub", true, "10.77.0.1")
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cluster, &home, &pub).Build()
	hc := &http.Client{Transport: toServer{pr.URL}}
	w := &Writer{Client: c, Site: "home", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()},
		Peers: &Peers{Client: c, HTTP: hc}, HTTP: hc}
	if err := w.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cp.pushers, []string{"mirror", "fabric"}) {
		t.Fatalf("home leads by its own copy of the Fabric, not by what Flux has applied: %v", cp.pushers)
	}
}

// What is revoked is decided in Git: a box the cluster's copy has dropped but Git still holds keeps its
// certificates, and nothing is revoked when this site's own Site was not read.
func TestRevokeDecidesInGit(t *testing.T) {
	later := time.Now().Add(time.Hour)
	issued := map[string][]Issued{"home-b": {{fp("b"), later}}, "home-c": {{fp("c"), later}}}
	pr := peer(SiteStatus{}, issued)
	defer pr.Close()
	cluster := &corev1.ConfigMap{Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": "home"}}
	cluster.Name, cluster.Namespace = SettingsName, SystemNS
	home := testSite("home", true, "10.77.1.1") // the cluster's copy has neither home-b nor home-c
	cp := &fakeCopy{head: "1111111111111111111111111111111111111111", exists: true, collab: true, files: map[string]string{
		SettingsPath: settingsFile(t, "home", 1, ""), "fabric/sites/home.yaml": siteFile(t, testSite("home", true, "10.77.1.1", "home-b"))}}
	fj := httptest.NewServer(cp)
	defer fj.Close()
	w := &Writer{Client: fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cluster, &home).Build(), Site: "home",
		Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}, HTTP: &http.Client{Transport: toServer{pr.URL}}}
	ctx := context.Background()
	if err := w.revoke(ctx, []v1alpha1.Site{home}); err != nil {
		t.Fatal(err)
	}
	set := &corev1.ConfigMap{}
	if err := yaml.Unmarshal([]byte(cp.file(SettingsPath)), set); err != nil {
		t.Fatal(err)
	}
	if bl := set.Data["blocklist"]; !strings.Contains(bl, fp("c")) || strings.Contains(bl, fp("b")) {
		t.Fatalf("only the box that left Git is revoked: %q", bl)
	}
	cp.mu.Lock()
	cp.files = map[string]string{SettingsPath: settingsFile(t, "home", 1, ""), "fabric/sites/pub.yaml": siteFile(t, testSite("pub", true, "10.77.0.1"))}
	commits := cp.commits
	cp.mu.Unlock()
	if err := w.revoke(ctx, []v1alpha1.Site{home}); err == nil || cp.commits != commits {
		t.Fatalf("without this site's own Site, nothing is revoked: %v", err)
	}
}

// lead makes the planned moves' steps, even while a site's push mirror cannot be set up.
func TestLeadMakesHandoverSteps(t *testing.T) {
	gvk := v1alpha1.GroupVersion.WithKind("App")
	app := sampleApp()
	app.Spec.Primary, app.Spec.Handover = "friend", &v1alpha1.Handover{ID: "m1", From: "vince"}
	b, err := fabric.YAML(app, gvk)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := fabric.Path(gvk, app.Namespace, app.Name)
	cp := &fakeCopy{head: "1111111111111111111111111111111111111111", exists: true, collab: true,
		files: map[string]string{SettingsPath: settingsFile(t, "pub", 1, ""), p: string(b)}}
	fj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/push_mirrors") && r.Method == http.MethodPost {
			http.Error(w, "vince's copy is unreachable", http.StatusInternalServerError)
			return
		}
		cp.ServeHTTP(w, r)
	}))
	defer fj.Close()
	pr := peer(SiteStatus{Site: "vince", Apps: map[string]AppState{"vince/docs": {Demoted: &Demotion{Handover: "m1", Token: "tok"}}}}, nil)
	defer pr.Close()
	cluster := &corev1.ConfigMap{Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": "pub", "epoch": "1"}}
	cluster.Name, cluster.Namespace = SettingsName, SystemNS
	sec := &corev1.Secret{Data: map[string][]byte{KeyMirror: []byte("pw")}}
	sec.Name, sec.Namespace = SiteSecret("vince"), SystemNS
	vince, pub := testSite("vince", false, "10.77.2.1"), testSite("pub", true, "10.77.0.1")
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app, cluster, sec, &vince, &pub).Build()
	hc := &http.Client{Transport: toServer{pr.URL}}
	w := &Writer{Client: c, Site: "pub", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()},
		Peers: &Peers{Client: c, HTTP: hc}, HTTP: hc}
	if err := w.lead(context.Background(), []v1alpha1.Site{vince, pub}, Claim{Writer: "pub", Epoch: 1}); err == nil || !strings.Contains(err.Error(), "push to vince") {
		t.Fatalf("the mirror's failure is reported: %v", err)
	}
	got := &v1alpha1.App{}
	if err := yaml.Unmarshal([]byte(cp.file(p)), got); err != nil || got.Spec.Handover == nil || got.Spec.Handover.Token != "tok" {
		t.Fatalf("the demoted primary's token is committed: %v %+v", err, got.Spec.Handover)
	}
}
