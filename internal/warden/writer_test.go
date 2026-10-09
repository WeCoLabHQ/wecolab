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

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

func testWriterCoordination() *kubefake.Clientset {
	return kubefake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: "wecolab-writer-transition", Namespace: SystemNS,
	}})
}

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

// fakeCopy covers Forgejo REST administration and edits. Smart-HTTP custody
// tests use real bare Git repositories instead.
type fakeCopy struct {
	mu        sync.Mutex
	head      string
	exists    bool
	collab    bool
	pushers   []string
	mirrors   []fabric.PushMirror
	files     map[string]string
	recreated int
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
		f.exists, f.collab, f.head, f.mirrors, f.pushers = false, false, "", nil, nil
		f.recreated++
	case p == "":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	case p == "/collaborators/mirror" && r.Method == http.MethodPut:
		f.collab = true
	case p == "/collaborators/mirror" && !f.collab:
		http.NotFound(w, r)
	case p == "/collaborators/mirror":
		w.WriteHeader(http.StatusNoContent)
	case p == "/branch_protections/superseded-**" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"push_whitelist_usernames": []string{ForgejoOwner}, "enable_push": true, "enable_push_whitelist": true})
	case p == "/branch_protections/main" && r.Method == http.MethodGet && f.pushers == nil:
		http.NotFound(w, r)
	case p == "/branch_protections/main" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"push_whitelist_usernames": f.pushers, "enable_push": true, "enable_push_whitelist": true})
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
	case p == "/push_mirrors" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(append([]fabric.PushMirror{}, f.mirrors...))
	case strings.HasPrefix(p, "/push_mirrors/") && r.Method == http.MethodDelete:
		f.mirrors = slices.DeleteFunc(f.mirrors, func(m fabric.PushMirror) bool { return m.Name == strings.TrimPrefix(p, "/push_mirrors/") })
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

func TestLeadAdmissionRequiresFabric(t *testing.T) {
	for _, tc := range []struct {
		name  string
		head  string
		claim Claim
		open  bool
	}{
		{"empty repository", "", Claim{Writer: "pub", Epoch: 1}, false},
		{"missing settings", strings.Repeat("1", 40), Claim{}, false},
		{"another writer", strings.Repeat("1", 40), Claim{Writer: "home", Epoch: 2}, false},
		{"owns populated repository", strings.Repeat("1", 40), Claim{Writer: "pub", Epoch: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := &fakeCopy{head: tc.head, exists: true, collab: true}
			fj := httptest.NewServer(cp)
			defer fj.Close()
			w := &Writer{Site: "pub", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()},
				Coordination: testWriterCoordination().CoordinationV1()}
			if err := withWriterLock(context.Background(), w.Coordination, func(ctx context.Context) error {
				return w.admitLead(ctx, tc.claim)
			}); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(cp.pushers, ForgejoOwner) != tc.open {
				t.Fatalf("Console admission: pushers=%v, want open=%v", cp.pushers, tc.open)
			}
		})
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
	home, pub := testSite("home", true, "10.77.1.1"), testSite("pub", true, "10.77.0.1")
	cp := &fakeCopy{head: "1111111111111111111111111111111111111111", exists: true, collab: true,
		files: map[string]string{
			SettingsPath:             settingsFile(t, "home", 2, ""),
			"fabric/sites/home.yaml": siteFile(t, home),
			"fabric/sites/pub.yaml":  siteFile(t, pub),
		}}
	fj := httptest.NewServer(cp)
	defer fj.Close()
	pr := peer(SiteStatus{Writer: "pub", Epoch: 1}, nil)
	defer pr.Close()
	cluster := &corev1.ConfigMap{Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": "pub", "epoch": "1"}}
	cluster.Name, cluster.Namespace = SettingsName, SystemNS
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cluster, &home, &pub).WithObjects(testReadyForgejo(t)...).Build()
	hc := &http.Client{Transport: toServer{pr.URL}}
	w := &Writer{Client: c, Site: "home", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()},
		Peers: &Peers{Client: c, HTTP: hc}, HTTP: hc, Coordination: testWriterCoordination().CoordinationV1()}
	if err := w.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cp.pushers, []string{"mirror", "fabric"}) {
		t.Fatalf("home leads by its own copy of the Fabric, not by what Flux has applied: %v", cp.pushers)
	}
}

func TestStaleFollowerPassCannotCloseSuccessfulTakeover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	home, pub := testSite("home", true, "10.77.1.1"), testSite("pub", true, "10.77.0.1")
	cp := &fakeCopy{head: strings.Repeat("1", 40), exists: true, collab: true,
		pushers: []string{ForgejoMirror}, files: map[string]string{
			SettingsPath:             settingsFile(t, "pub", 1, ""),
			"fabric/sites/home.yaml": siteFile(t, home),
			"fabric/sites/pub.yaml":  siteFile(t, pub),
		}}
	fj := httptest.NewServer(cp)
	t.Cleanup(fj.Close)
	g := &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}
	paused, resume := make(chan struct{}), make(chan struct{})
	var pauseOnce, resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	pr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			pauseOnce.Do(func() {
				close(paused)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			})
			_ = json.NewEncoder(w).Encode(SiteStatus{Site: "pub", Writer: "pub", Epoch: 1})
			return
		}
		_ = json.NewEncoder(w).Encode(CopyAnswer{Head: strings.Repeat("1", 40), OnMain: true, Claim: Claim{Writer: "pub", Epoch: 1}})
	}))
	t.Cleanup(pr.Close)
	t.Cleanup(release)
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(&home, &pub).WithObjects(testReadyForgejo(t)...).Build()
	hc := &http.Client{Transport: toServer{pr.URL}}
	w := &Writer{Client: c, Site: "home", Git: g, Peers: &Peers{Client: c, HTTP: hc}, HTTP: hc,
		Coordination: testWriterCoordination().CoordinationV1()}
	synced := make(chan error, 1)
	go func() { synced <- w.Sync(ctx) }()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal("follower did not reach peer observation")
	}
	if epoch, err := TakeOver(ctx, g, "home", nil, fabric.Author{Name: "operator"}, w.Coordination); err != nil || epoch != 2 {
		t.Fatalf("takeover: epoch=%d err=%v", epoch, err)
	}
	release()
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	claim, err := GitClaim(ctx, g)
	if err != nil || claim != (Claim{Writer: "home", Epoch: 2}) {
		t.Fatalf("successful takeover was lost: %+v %v", claim, err)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if !slices.Contains(cp.pushers, ForgejoOwner) || cp.recreated != 0 {
		t.Fatalf("stale follower revoked the new writer or recreated its repository: pushers=%v recreated=%d", cp.pushers, cp.recreated)
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

func TestRevokeDoesNotWaitForAppliedBoxRemoval(t *testing.T) {
	later := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	pr := peer(SiteStatus{}, map[string][]Issued{"home-b": {{fp("b"), later}}})
	defer pr.Close()
	cluster := &corev1.ConfigMap{Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": "home"}}
	cluster.Name, cluster.Namespace = SettingsName, SystemNS
	home := testSite("home", true, "10.77.1.1", "home-b")
	cp := &fakeCopy{head: "1111111111111111111111111111111111111111", exists: true, collab: true, files: map[string]string{
		SettingsPath:             settingsFile(t, "home", 1, ""),
		"fabric/sites/home.yaml": siteFile(t, testSite("home", true, "10.77.1.1")),
	}}
	fj := httptest.NewServer(cp)
	defer fj.Close()
	w := &Writer{Client: fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cluster, &home).Build(), Site: "home",
		Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}, HTTP: &http.Client{Transport: toServer{pr.URL}}}
	if err := w.revoke(context.Background(), []v1alpha1.Site{home}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := yaml.Unmarshal([]byte(cp.file(SettingsPath)), cm); err != nil {
		t.Fatal(err)
	}
	settings, err := ParseSettings(cm.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Blocklisted) != 1 || settings.Blocklisted[0].Fingerprint != fp("b") || !settings.Blocklisted[0].Until.Equal(later) {
		t.Fatalf("a Git-deleted box's renewal must be revoked before Flux applies its removal: %+v", settings.Blocklisted)
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
		files: map[string]string{SettingsPath: settingsFile(t, "pub", 1, ""), p: string(b),
			fabric.UpgradePath: `{"phase":"ready"}`, fabric.MigrationCompletePath: `{}`, fabric.PlacementRevisionPath: `{"revision":"0"}`}}
	fj := httptest.NewServer(cp)
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
	replicationAttempted := false
	gitHTTP := &http.Client{Transport: custodyRoute(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "10.77.2.1:30300" {
			replicationAttempted = true
			return nil, fmt.Errorf("unreachable fixture")
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	w := &Writer{Client: c, Site: "pub", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: gitHTTP},
		Peers: &Peers{Client: c, HTTP: hc}, HTTP: hc}
	if err := w.lead(context.Background(), []v1alpha1.Site{vince, pub}, Claim{Writer: "pub", Epoch: 1}); err == nil || !replicationAttempted {
		t.Fatalf("replication failure did not surface: attempted=%v err=%v", replicationAttempted, err)
	}
	got := &v1alpha1.App{}
	if err := yaml.Unmarshal([]byte(cp.file(p)), got); err != nil || got.Spec.Handover == nil || got.Spec.Handover.Token != "tok" {
		t.Fatalf("the demoted primary's token is committed: %v %+v", err, got.Spec.Handover)
	}
}
