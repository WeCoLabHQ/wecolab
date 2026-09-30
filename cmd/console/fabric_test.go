package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/nebula"
	"wecolab.io/wecolab/internal/warden"
)

// fakeGit is a Forgejo holding the Fabric in memory. Like the real one, it refuses a commit whose sha
// is not the file's current one (409) and a create where the file exists (422).
type fakeGit struct {
	mu      sync.Mutex
	files   map[string]string
	commits int
	failing bool     // every commit fails
	refuse  int      // the next commits are refused as pushes to a protected branch
	pushers []string // branch protection's push whitelist; nil: none
}

func sha(c string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(c))) }

func (f *fakeGit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/fabric/fabric")
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/contents/"):
		p = strings.TrimPrefix(p, "/contents/")
		if c, ok := f.files[p]; ok {
			_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "path": p, "sha": sha(c), "content": base64.StdEncoding.EncodeToString([]byte(c))})
			return
		}
		list := []map[string]string{}
		for k := range f.files {
			if pathpkg.Dir(k) == p {
				list = append(list, map[string]string{"type": "file", "path": k})
			}
		}
		if len(list) == 0 {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPost && p == "/contents":
		var body struct {
			Files []struct{ Operation, Path, SHA, Content string }
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.failing {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if f.refuse > 0 {
			f.refuse--
			http.Error(w, "protected branch", http.StatusForbidden)
			return
		}
		for _, c := range body.Files {
			cur, ok := f.files[c.Path]
			if c.Operation == "create" && ok {
				http.Error(w, "exists", http.StatusUnprocessableEntity)
				return
			}
			if c.Operation != "create" && (!ok || sha(cur) != c.SHA) {
				http.Error(w, "sha does not match", http.StatusConflict)
				return
			}
		}
		for _, c := range body.Files {
			if c.Operation == "delete" {
				delete(f.files, c.Path)
				continue
			}
			b, _ := base64.StdEncoding.DecodeString(c.Content)
			f.files[c.Path] = string(b)
		}
		f.commits++
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": fmt.Sprint(f.commits)}})
	case p == "/branch_protections/main" && r.Method == http.MethodGet:
		if f.pushers == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"push_whitelist_usernames": f.pushers})
	case strings.HasPrefix(p, "/branch_protections"):
		var body struct {
			Pushers []string `json:"push_whitelist_usernames"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.pushers = body.Pushers
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGit) get(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[p]
}

const testSettings = `apiVersion: v1
kind: ConfigMap
metadata: {name: fabric, namespace: wecolab-system}
data: {zone: fab.example, network: 10.77.0.0/16, writer: a, epoch: "3"}
`

var testScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}()

// inGit is objects as the Fabric keeps them, by path, with the settings.
func inGit(t *testing.T, objs ...client.Object) map[string]string {
	t.Helper()
	out := map[string]string{settingsPath: testSettings}
	for _, o := range objs {
		gvk, _ := apiutil.GVKForObject(o, testScheme)
		p, _ := fabric.Path(gvk, o.GetNamespace(), o.GetName())
		b, err := fabric.YAML(o, gvk)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = string(b)
	}
	return out
}

// testServer is a Console at site a over a fake Fabric in Git and a fake API server here.
func testServer(t *testing.T, git map[string]string, objs ...client.Object) (*server, *fakeGit) {
	t.Helper()
	f := &fakeGit{files: git}
	hs := httptest.NewServer(f)
	t.Cleanup(hs.Close)
	cm := &corev1.ConfigMap{}
	_ = yaml.Unmarshal([]byte(testSettings), cm)
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(append(objs, cm)...).Build()
	return &server{c: c, site: "a", domain: "fab.example", publicURL: "https://console.fab.example",
		git: &fabric.Git{URL: hs.URL, Token: "t", Repo: "fabric/fabric", HTTP: hs.Client()}, peers: &warden.Peers{Client: c}}, f
}

// fromFake reads an object back from the fake Fabric.
func fromFake[T client.Object](t *testing.T, s *server, obj T) T {
	t.Helper()
	if ok, err := s.fromGit(context.Background(), obj); err != nil || !ok {
		t.Fatalf("%s: in Git %v, %v", obj.GetName(), ok, err)
	}
	return obj
}

func call(h http.HandlerFunc, method string, body any, pathValues ...string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "/", strings.NewReader(string(b)))
	for i := 0; i+1 < len(pathValues); i += 2 {
		r.SetPathValue(pathValues[i], pathValues[i+1])
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// fakeSops puts a sops on the PATH that passes files through as they are: the tests check what is
// written and read, not the encryption. The Console opens files with this site's age key.
func fakeSops(t *testing.T) client.Object {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: warden.FluxNS, Name: "sops-age"}, Data: map[string][]byte{"age.agentkey": []byte("AGE-SECRET-KEY-1")}}
}

// code is the invite code inside what mintInvite gives a person.
func code(t *testing.T, s *server, inv invite) string {
	t.Helper()
	c, err := s.mintInvite(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ T string }
	b, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(c, "wcl2."))
	_ = json.Unmarshal(b, &out)
	return out.T
}

func testCA(t *testing.T) client.Object {
	t.Helper()
	caCrt, caKey, err := nebula.NewCA("test", netip.MustParsePrefix("10.77.0.0/16"), 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: warden.SystemNS, Name: warden.CASecret}, Data: map[string][]byte{"ca.crt": caCrt, "ca.key": caKey}}
}

const testAge = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"

func testSite(name string, index int, steward bool, boxes ...v1alpha1.Box) *v1alpha1.Site {
	return &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.SiteSpec{Owner: "p", Index: index, Steward: steward, Boxes: boxes}}
}

// A box joins against the Fabric in Git: its address from nextBox (never a removed box's), a name no
// other site's box has, the agent token, and its certificate recorded. An invite is used only by a
// join that was committed: a refused one can be retried.
func TestJoinBox(t *testing.T) {
	now := time.Now()
	sopsAge := fakeSops(t)
	pub, _, _ := nebula.Keypair()
	a := testSite("a", 1, true, v1alpha1.Box{Name: "a-m", IP: "10.77.1.1", Role: "manager", Key: "k"}, v1alpha1.Box{Name: "a-n", IP: "10.77.1.4", Role: "node", Key: "k"})
	a.Spec.NextBox = 7 // boxes 5 and 6 were removed
	ab := testSite("a-b", 2, false, v1alpha1.Box{Name: "a-b-x", IP: "10.77.2.1", Role: "manager", Key: "k"})
	git := inGit(t, a, ab)
	tokens, _ := secretFile(warden.SiteSecret("a"), map[string]string{warden.KeyK3sServer: "server", warden.KeyK3sAgent: "agent"}, []string{testAge})
	git[tokens.Path] = string(tokens.Content) // the tokens come from Git: here, a site that joined a moment ago has none yet
	s, _ := testServer(t, git, testCA(t), sopsAge)
	req := joinRequest{Code: code(t, s, invite{Kind: "box", Site: "a", By: "ana@example.org", Expires: now.Add(time.Hour)}), Host: "b-x", Key: string(pub)}

	if w := call(s.join, "POST", req); w.Code != 409 {
		t.Fatalf("a-b-x is site a-b's box: %d %s", w.Code, w.Body)
	}
	req.Host, req.Laptop = "Anas-MacBook-Pro.local", true
	if w := call(s.join, "POST", req); w.Code != 403 {
		t.Fatalf("a Mac took a Linux box's invite: %d %s", w.Code, w.Body)
	}
	req.Laptop = false
	w := call(s.join, "POST", req)
	if w.Code != 200 {
		t.Fatalf("the invite must survive refused joins: %d %s", w.Code, w.Body)
	}
	var res joinResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Box != "a-anas-macbook-pro-local" || res.IP != "10.77.1.7" || res.K3sToken != "agent" || res.K3sAgentToken != "" || res.K3sServer != "https://10.77.1.1:6443" {
		t.Fatalf("response: box %s ip %s token %q agent %q server %s", res.Box, res.IP, res.K3sToken, res.K3sAgentToken, res.K3sServer)
	}
	got := fromFake(t, s, &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}})
	fp, _, _ := nebula.Fingerprint([]byte(res.Cert))
	box := got.Spec.Boxes[len(got.Spec.Boxes)-1]
	if got.Spec.NextBox != 8 || box.Name != res.Box || len(box.Certs) != 1 || box.Certs[0].Fingerprint != fp {
		t.Fatalf("in Git: nextBox %d, box %+v, cert %s", got.Spec.NextBox, box, fp)
	}
	if w := call(s.join, "POST", req); w.Code != 403 {
		t.Fatalf("an invite is used once: %d", w.Code)
	}
}

// A site joins with its age key checked before anything is written; its manager gets the server token
// and the agent token, both kept in the site's Secret in Git; a new steward can read every sealed file.
func TestJoinSite(t *testing.T) {
	a := testSite("a", 1, true, v1alpha1.Box{Name: "a-m", IP: "10.77.1.1", Role: "manager", Key: "k"})
	git := inGit(t, a)
	git["keys/a.age.pub"], git["keys/recovery.age.pub"] = testAge+"\n", testAge+"\n"
	git["secrets/storage.sops.yaml"] = "apiVersion: v1\nkind: Secret\nmetadata: {name: storage}\n"
	s, f := testServer(t, git, testCA(t), fakeSops(t))
	pub, _, _ := nebula.Keypair()
	req := joinRequest{Code: code(t, s, invite{Kind: "site", Site: "c", Owner: "p", Steward: true, By: "ana@example.org", Expires: time.Now().Add(time.Hour)}), Host: "m", Key: string(pub), Age: "age1bad"}
	if w := call(s.join, "POST", req); w.Code != 400 {
		t.Fatalf("a bad age key: %d %s", w.Code, w.Body)
	}
	req.Age = testAge
	w := call(s.join, "POST", req)
	if w.Code != 200 {
		t.Fatalf("the invite must survive a refused join: %d %s", w.Code, w.Body)
	}
	var res joinResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	sec := &corev1.Secret{}
	_ = yaml.Unmarshal([]byte(f.get(secretPath(warden.SiteSecret("c")))), sec)
	if res.Box != "c-m" || res.IP != "10.77.2.1" || res.Role != "manager" || res.K3sToken == "" || res.K3sAgentToken == "" || res.K3sToken == res.K3sAgentToken ||
		sec.StringData[warden.KeyK3sServer] != res.K3sToken || sec.StringData[warden.KeyK3sAgent] != res.K3sAgentToken {
		t.Fatalf("response %+v, secret %v", res, sec.StringData)
	}
	if f.get("keys/c.age.pub") != testAge+"\n" || !strings.Contains(f.get(kustomizationPath), pathpkg.Base(secretPath(warden.SiteSecret("c")))) {
		t.Fatal("the age key and the Secret's listing")
	}
	if _, err := s.sealedNow(context.Background(), nil); err != fabric.ErrConflict {
		t.Fatalf("a sealed file the edit did not read: %v", err)
	}
}

// A box past .254 or a site past the network's last /24 is refused, never committed as "invalid IP".
func TestJoinOutOfAddresses(t *testing.T) {
	a := testSite("a", 15, false, v1alpha1.Box{Name: "a-m", IP: "10.77.15.1", Role: "manager", Key: "k"})
	a.Spec.NextBox = 255
	git := inGit(t, a)
	git[settingsPath] = strings.Replace(testSettings, "10.77.0.0/16", "10.77.0.0/20", 1) // sixteen sites
	s, _ := testServer(t, git, testCA(t), fakeSops(t))
	pub, _, _ := nebula.Keypair()
	for _, inv := range []invite{{Kind: "box", Site: "a"}, {Kind: "site", Site: "c", Owner: "p"}} {
		inv.By, inv.Expires = "ana@example.org", time.Now().Add(time.Hour)
		req := joinRequest{Code: code(t, s, inv), Host: "n", Key: string(pub), Age: testAge}
		if w := call(s.join, "POST", req); w.Code != 409 {
			t.Errorf("%s %s: %d %s", inv.Kind, inv.Site, w.Code, w.Body)
		}
	}
}

// An invite is for a new, valid name, and a Git that cannot be read is not a site that does not exist.
func TestInviteSite(t *testing.T) {
	s, _ := testServer(t, inGit(t, testSite("a", 1, true)))
	for name, want := range map[string]int{"a": 409, "recovery": 400, "Bad": 400} {
		if w := call(s.inviteSite, "POST", map[string]string{"Name": name, "Owner": "p"}); w.Code != want {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	s.git.URL = "http://127.0.0.1:1"
	if w := call(s.inviteSite, "POST", map[string]string{"Name": "c", "Owner": "p"}); w.Code != 502 {
		t.Fatalf("Git unreadable: %d %s", w.Code, w.Body)
	}
}

// Removing a box blocklists the certificate it joined with, in the same commit (the writer's loop
// blocklists renewals). Its address is never given again.
func TestRemoveBoxBlocklistsItsCertificates(t *testing.T) {
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	fp := strings.Repeat("ab", 32)
	a := testSite("a", 1, true, v1alpha1.Box{Name: "a-m", IP: "127.0.0.1", Role: "manager", Key: "k"},
		v1alpha1.Box{Name: "a-n", IP: "10.77.1.2", Role: "node", Key: "k", Certs: []v1alpha1.IssuedCert{{Fingerprint: fp, NotAfter: metav1.NewTime(until)}, {Fingerprint: "x notatime", NotAfter: metav1.NewTime(until)}}})
	s, f := testServer(t, inGit(t, a), a)
	if w := call(s.removeBox, "DELETE", nil, "site", "a", "box", "a-m"); w.Code != 409 {
		t.Fatalf("a manager leaves with its site: %d", w.Code)
	}
	w := call(s.removeBox, "DELETE", nil, "site", "a", "box", "a-n")
	var out struct{ Blocklisted int }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Blocklisted != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	_, set, err := parseSettings([]byte(f.get(settingsPath)))
	if err != nil || len(set.Blocklisted) != 1 || set.Blocklisted[0].Fingerprint != fp || !set.Blocklisted[0].Until.Equal(until) {
		t.Fatalf("blocklist: %+v %v", set, err)
	}
	got := fromFake(t, s, &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}})
	if _, b := warden.Find([]v1alpha1.Site{*got}, "a-n"); b != nil || got.Spec.NextBox != 3 {
		t.Fatalf("the box is still in Git, or its address is free again: nextBox %d", got.Spec.NextBox)
	}
}

// Moves are spec changes in Git: planned only when the sites report it can complete (MoveGates), forced
// with every other site rebuilt; the target is one of the app's sites.
func TestSetPrimary(t *testing.T) {
	app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}, Spec: v1alpha1.AppSpec{Sites: []string{"a", "b"}, Primary: "a", Workload: "wiki", Database: "wiki-db"}}
	moving := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "docs"}, Spec: v1alpha1.AppSpec{Sites: []string{"a", "b"}, Primary: "b", Workload: "docs", Database: "docs-db", Handover: &v1alpha1.Handover{ID: "h", From: "a"}}}
	s, _ := testServer(t, inGit(t, app, moving))
	if w := call(s.setPrimary, "POST", map[string]any{"To": "c"}, "ns", "p", "app", "wiki"); w.Code != 409 {
		t.Fatalf("c is not one of the app's sites: %d", w.Code)
	}
	if w := call(s.setPrimary, "POST", map[string]any{"To": "b"}, "ns", "p", "app", "wiki"); w.Code != 409 {
		t.Fatalf("a planned move while no site answers: %d %s", w.Code, w.Body)
	}
	if got := fromFake(t, s, &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}}); got.Spec.Primary != "a" || got.Spec.Handover != nil {
		t.Fatalf("a refused move changed Git: %+v", got.Spec)
	}
	if w := call(s.setPrimary, "POST", map[string]any{"To": "a"}, "ns", "p", "app", "docs"); w.Code != 200 {
		t.Fatalf("a cancel before the token needs no gate: %d %s", w.Code, w.Body)
	}
	if got := fromFake(t, s, &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "docs"}}); got.Spec.Primary != "a" || got.Spec.Handover != nil {
		t.Fatalf("cancelled: %+v", got.Spec)
	}
	if w := call(s.setMesh, "POST", map[string]any{}, "ns", "p", "app", "wiki"); w.Code != 400 {
		t.Fatalf("a body without the flag: %d", w.Code)
	}
	if w := call(s.setPrimary, "POST", map[string]any{"To": "b", "Force": true}, "ns", "p", "app", "wiki"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := fromFake(t, s, &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}})
	if got.Spec.Primary != "b" || got.Spec.Handover != nil || got.Spec.Archive["a"] != 2 {
		t.Fatalf("forced: %+v", got.Spec)
	}
}

// A key is taken out of the file as Git holds it: two redeems at once, one wins.
func TestRedeemOnce(t *testing.T) {
	code, key := mintKey("wclp1", "community")
	pool := &v1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "community"}, Spec: v1alpha1.PoolSpec{Keys: []v1alpha1.OfferKey{key}}}
	s, _ := testServer(t, inGit(t, pool))
	if w := call(s.redeemOffer, "POST", map[string]string{"Code": code, "Project": "kube-system"}); w.Code != 400 {
		t.Fatalf("a system namespace as a project: %d", w.Code)
	}
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, p := range []string{"p1", "p2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- call(s.redeemOffer, "POST", map[string]string{"Code": code, "Project": p}).Code
		}()
	}
	wg.Wait()
	close(codes)
	got := []int{}
	for c := range codes {
		got = append(got, c)
	}
	slices.Sort(got)
	pool = fromFake(t, s, &v1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "community"}})
	if !slices.Equal(got, []int{200, 403}) || len(pool.Spec.Projects) != 1 || len(pool.Spec.Keys) != 0 {
		t.Fatalf("answers %v, pool %+v", got, pool.Spec)
	}
}

// The writer stays a steward; a body without the flag is refused, never read as false.
func TestSetSteward(t *testing.T) {
	a, b := testSite("a", 1, true), testSite("b", 2, true)
	s, _ := testServer(t, inGit(t, a, b))
	if w := call(s.setSteward, "POST", map[string]any{}, "site", "b"); w.Code != 400 {
		t.Fatalf("no flag: %d", w.Code)
	}
	if w := call(s.setSteward, "POST", map[string]any{"Steward": false}, "site", "a"); w.Code != 409 {
		t.Fatalf("a is the writer: %d %s", w.Code, w.Body)
	}
	if w := call(s.setSteward, "POST", map[string]any{"Steward": false}, "site", "b"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if fromFake(t, s, &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "b"}}).Spec.Steward {
		t.Fatal("b is still a steward")
	}
}

// takeover raises the epoch in Git and opens this copy only for a commit that succeeds.
func TestTakeover(t *testing.T) {
	git := inGit(t, testSite("a", 1, true))
	git[settingsPath] = strings.Replace(testSettings, "writer: a", "writer: b", 1)
	s, f := testServer(t, git)
	f.failing, f.pushers = true, []string{warden.ForgejoMirror}
	if w := call(s.takeover, "POST", nil); w.Code == 200 || !slices.Equal(f.pushers, []string{warden.ForgejoMirror}) {
		t.Fatalf("a failed takeover leaves the copy open: %d %v", w.Code, f.pushers)
	}
	f.failing, f.refuse = false, 1 // this site's Warden closed the copy again between opening and committing
	if w := call(s.takeover, "POST", nil); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	_, set, _ := parseSettings([]byte(f.get(settingsPath)))
	if set.Writer != "a" || set.Epoch != 4 || len(f.pushers) != 2 {
		t.Fatalf("writer %s epoch %d pushers %v", set.Writer, set.Epoch, f.pushers)
	}
	var got map[string]any
	_ = json.Unmarshal(call(s.getSettings, "GET", nil).Body.Bytes(), &got)
	if got["isSteward"] != true || got["isWriter"] != true {
		t.Fatalf("settings: %v", got)
	}
}

// Members change as Git holds them, only in the fields sent: a stale copy here never undoes an
// admin's change, and a person blocked in Git cannot save keys before the copy here knows.
func TestMemberEditsReadGit(t *testing.T) {
	m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "m"}, Spec: v1alpha1.MemberSpec{Email: "m@example.org", Role: "member", Projects: []string{"p"}}}
	owner := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "o"}, Spec: v1alpha1.MemberSpec{Email: "o@example.org", Role: "owner"}}
	s, _ := testServer(t, inGit(t, m, owner), m.DeepCopy(), owner.DeepCopy()) // the copy here never catches up
	keys := func() int {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"SSHKeys":["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB m@laptop"]}`))
		r = r.WithContext(withIdentity(r.Context(), &identity{Email: "m@example.org", Projects: []string{"p"}}))
		w := httptest.NewRecorder()
		s.myKeys(w, r)
		return w.Code
	}
	if w := call(s.setMember, "POST", map[string]any{"Projects": []string{"q"}}, "id", "m"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c := keys(); c != 200 {
		t.Fatalf("keys: %d", c)
	}
	got := fromFake(t, s, &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "m"}})
	if !slices.Equal(got.Spec.Projects, []string{"q"}) || len(got.Spec.SSHKeys) != 1 {
		t.Fatalf("both edits survive: %+v", got.Spec)
	}
	if w := call(s.setMember, "POST", map[string]any{"Blocked": true}, "id", "m"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c := keys(); c != 403 || !fromFake(t, s, &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "m"}}).Spec.Blocked {
		t.Fatalf("a blocked member saved keys (%d) or was unblocked", c)
	}
	if call(s.setMember, "POST", map[string]any{"Role": "member"}, "id", "o").Code != 403 || call(s.deleteMember, "DELETE", nil, "id", "o").Code != 403 {
		t.Fatal("the owner is neither edited nor removed here")
	}
}

// A site's manager is who its owner project is in Git, not in the copy here.
func TestCanSiteReadsGit(t *testing.T) {
	stale := testSite("a", 1, false)
	stale.Spec.Owner = "q"
	b := testSite("b", 2, false)
	b.Spec.Owner = "q"
	s, _ := testServer(t, inGit(t, testSite("a", 1, false), b), stale)
	for site, want := range map[string]int{"a": 200, "b": 403, "../a": 403} {
		r := httptest.NewRequest("GET", "/", nil)
		r = r.WithContext(withIdentity(r.Context(), &identity{Email: "m@example.org", Projects: []string{"p"}}))
		w := httptest.NewRecorder()
		if ok := s.canSite(w, r, site); ok != (want == 200) || !ok && w.Code != want {
			t.Errorf("%s: %v %d", site, ok, w.Code)
		}
	}
}

// The page gets each site's Nebula /24 and a move's origin, never the demotion token.
func TestStateForThePage(t *testing.T) {
	app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}, Spec: v1alpha1.AppSpec{Sites: []string{"a", "b"}, Primary: "b", Database: "wiki-db",
		Handover: &v1alpha1.Handover{ID: "h", From: "a", Token: "the-demotion-token"}}}
	s, _ := testServer(t, inGit(t), testSite("a", 3, true), app)
	w := call(s.state, "GET", nil)
	var out struct {
		Sites []struct{ Nebula string }
		Apps  []struct{ Handover *v1alpha1.Handover }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || strings.Contains(w.Body.String(), "the-demotion-token") || len(out.Apps) != 1 || out.Apps[0].Handover.From != "a" ||
		len(out.Sites) != 1 || out.Sites[0].Nebula != "10.77.3.0/24" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// A domain verified for one project cannot be claimed by another, nor a name under it; an unverified
// claim blocks nobody.
func TestDomainClaims(t *testing.T) {
	verified := &v1alpha1.Domain{ObjectMeta: metav1.ObjectMeta{Name: "p1.example.org"}, Spec: v1alpha1.DomainSpec{Name: "example.org", Project: "p1"}}
	squat := &v1alpha1.Domain{ObjectMeta: metav1.ObjectMeta{Name: "p1.other.org"}, Spec: v1alpha1.DomainSpec{Name: "other.org", Project: "p1"}}
	here := verified.DeepCopy()
	here.Status.Verified = true
	s, _ := testServer(t, inGit(t, verified, squat), here)
	for _, c := range []struct {
		domain string
		code   int
	}{{"apps.example.org", 409}, {"example.org", 409}, {"bad_name.org", 400}, {"x.fab.example", 400}, {"other.org", 200}} {
		if w := call(s.addDomain, "POST", map[string]string{"Domain": c.domain, "Project": "p2"}); w.Code != c.code {
			t.Errorf("%s: %d %s", c.domain, w.Code, w.Body)
		}
	}
	fromFake(t, s, &v1alpha1.Domain{ObjectMeta: metav1.ObjectMeta{Name: "p2.other.org"}})
	// The zone itself is not an app's, even for a project that verified a domain above it.
	parent := &v1alpha1.Domain{ObjectMeta: metav1.ObjectMeta{Name: "p.example"}, Spec: v1alpha1.DomainSpec{Name: "example", Project: "p"}}
	here = parent.DeepCopy()
	here.Status.Verified = true
	s, _ = testServer(t, inGit(t, parent), here)
	if err := s.hostnameAllowed(context.Background(), "p", "wiki", "fab.example", nil); err == nil {
		t.Error("the fabric's own name was allowed")
	}
	if err := s.hostnameAllowed(context.Background(), "p", "wiki", "wiki.fab.example", nil); err != nil {
		t.Error(err)
	}
}

// Names: a Mac's host name makes a valid, bounded node name; the next box never reuses a number.
func TestBoxNames(t *testing.T) {
	for host, want := range map[string]string{"Anas-MacBook-Pro.local": "home-anas-macbook-pro-local", "--": "", strings.Repeat("x", 80): "home-" + strings.Repeat("x", 58)} {
		got, err := boxName("home", host)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q %v", host, got, err)
		}
	}
	s := testSite("a", 1, false, v1alpha1.Box{IP: "10.77.1.1"}, v1alpha1.Box{IP: "10.77.1.3"})
	if n := nextBox(s); n != 4 {
		t.Errorf("after .3: %d", n)
	}
	s.Spec.NextBox = 9
	if n := nextBox(s); n != 9 {
		t.Errorf("nextBox kept: %d", n)
	}
	if !ageKey.MatchString("age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p") || ageKey.MatchString("age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p\n# x") || ageKey.MatchString("AGE-SECRET-KEY-1") {
		t.Error("age key pattern")
	}
}

// A redeploy keeps what only moves and rebuilds change, and the secrets generated before.
func TestRedeployKeeps(t *testing.T) {
	app := &v1alpha1.App{Spec: v1alpha1.AppSpec{Primary: "a", Archive: map[string]int{"b": 3}, Handover: &v1alpha1.Handover{ID: "h", From: "b"}}}
	merge(app, deployRequest{Name: "wiki", Sites: []string{"a", "b"}, Primary: "a", Hostname: "wiki.fab.example", Database: true})
	if app.Spec.Archive["b"] != 3 || app.Spec.Handover == nil || app.Spec.Database != "wiki-db" || app.Spec.Hostname != "wiki.fab.example" {
		t.Fatalf("%+v", app.Spec)
	}
	if got := reusable(map[string]string{"admin_password": "x", "env-DSN": "redis://:x@", "b2-key": "k"}); len(got) != 1 || got["admin_password"] != "x" {
		t.Fatalf("%v", got)
	}
	apps := []v1alpha1.App{{ObjectMeta: metav1.ObjectMeta{Namespace: "c", Name: "a-b"}, Spec: v1alpha1.AppSpec{Mesh: true}}}
	if meshFree(apps, "b-c", "a") == nil || meshFree(apps, "c", "a-b") != nil || meshFree(nil, strings.Repeat("p", 32), strings.Repeat("a", 32)) == nil {
		t.Error("mesh names: a-b-c is taken; the app itself may keep its own; at most 63 characters")
	}
}

// A project deploys only where it owns a site or holds an offer, with its primary among its sites.
func TestDeployPlacement(t *testing.T) {
	b := testSite("b", 2, false)
	b.Spec.Owner = "q"
	s, _ := testServer(t, inGit(t, testSite("a", 1, true), b))
	in := deployRequest{Name: "web", Project: "p", Image: "nginx", Sites: []string{"a", "b"}, Primary: "c"}
	if w := call(s.deploy, "POST", in); w.Code != 400 {
		t.Fatalf("primary outside the sites: %d %s", w.Code, w.Body)
	}
	in.Primary = "a"
	if w := call(s.deploy, "POST", in); w.Code != 403 {
		t.Fatalf("site b was never offered to p: %d %s", w.Code, w.Body)
	}
	in.Sites, in.Database, in.Vault.KeyID, in.Vault.Key, in.Vault.Endpoint, in.Vault.Bucket = []string{"a"}, true, "k", "k", "https://1.1.1.1", "b/../x"
	if w := call(s.deploy, "POST", in); w.Code != 400 {
		t.Fatalf("a bucket with a path in it: %d %s", w.Code, w.Body)
	}
}

// A vault endpoint is https to a public address: never inside a site or the fabric's networks.
func TestVaultEndpoint(t *testing.T) {
	for raw, ok := range map[string]bool{"https://1.1.1.1": true, "http://1.1.1.1": false, "https://10.0.0.1": false, "https://127.0.0.1:9000": false,
		"https://169.254.169.254": false, "https://[::1]": false, "https://100.64.1.1": false, "https://user:pw@1.1.1.1": false, "s3.example.org": false} {
		if err := vaultEndpoint(context.Background(), raw); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// Every project comes through applyProject: system namespaces are not projects.
func TestProjectNames(t *testing.T) {
	s, _ := testServer(t, inGit(t))
	for _, n := range []string{"kube-system", "wecolab-system", "recovery", "Bad"} {
		if err := s.applyProject(context.Background(), n); err == nil {
			t.Errorf("%s accepted", n)
		}
	}
}

// Deleting an app only marks it in Git: the sites remove their part, data included, and the writer then
// removes the App and its folder. Nothing is removed from Git by the Console.
func TestDeleteMarksTheApp(t *testing.T) {
	app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}, Spec: v1alpha1.AppSpec{Sites: []string{"a", "b"}, Primary: "a", Workload: "wiki", Database: "wiki-db"}}
	git := inGit(t, app)
	git[fabric.AppFolder("p", "wiki")+"/deployment.yaml"] = "kind: Deployment\n"
	s, f := testServer(t, git)
	for range 2 { // deleting twice is fine
		if w := call(s.deleteApp, "DELETE", nil, "ns", "p", "app", "wiki"); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if got := fromFake(t, s, &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}}); !got.Spec.Deleted || got.Spec.Primary != "a" {
		t.Fatalf("the App is marked, not gone: %+v", got.Spec)
	}
	if _, ok := f.files[fabric.AppFolder("p", "wiki")+"/deployment.yaml"]; !ok {
		t.Fatal("the folder stays until every site removed its part")
	}
	if w := call(s.deleteApp, "DELETE", nil, "ns", "p", "app", "nope"); w.Code != 404 {
		t.Fatalf("an unknown app: %d", w.Code)
	}
}

// One line per change or refusal, with who, the reason and an id the page shows; nothing for a read
// that succeeds, and never a query string.
func TestLogged(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	h := logged(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noted(r.Context()).who = "ana@example.org"
		if r.Method == http.MethodPost {
			http.Error(w, "project x owns nothing at site y", 403)
		}
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/apps?token=secret", nil))
	if out.Len() != 0 || w.Header().Get("X-Request-Id") == "" {
		t.Fatalf("a read that succeeds: %q, id %q", out.String(), w.Header().Get("X-Request-Id"))
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/deploy?code=secret", nil))
	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatal(out.String(), err)
	}
	if line["id"] != w.Header().Get("X-Request-Id") || line["who"] != "ana@example.org" || line["status"] != 403.0 ||
		line["event"] != "authz_fail" || line["reason"] != "project x owns nothing at site y" || line["path"] != "/api/deploy" {
		t.Fatalf("refusal: %v", line)
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("a query string was logged: %s", out.String())
	}
}
