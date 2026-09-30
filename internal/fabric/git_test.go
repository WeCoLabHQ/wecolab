package fabric

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"wecolab.io/wecolab/api/v1alpha1"
)

// fakeForgejo holds files in memory and answers the two calls FabricGit makes.
func fakeForgejo(t *testing.T, files map[string]string) (*Git, *[]map[string]any) {
	var ops []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/fabric/fabric/contents")
		switch {
		case r.Method == http.MethodGet:
			c, ok := files[strings.TrimPrefix(p, "/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "sha": "sha-" + c, "content": base64.StdEncoding.EncodeToString([]byte(c))})
		case r.Method == http.MethodPost && p == "":
			var body struct {
				Files  []map[string]any
				Author map[string]string
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Author["email"] != "ana@example.org" {
				t.Errorf("author %v", body.Author)
			}
			ops = append(ops, body.Files...)
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "abc"}})
		}
	}))
	t.Cleanup(srv.Close)
	return &Git{URL: srv.URL, Token: "t", Repo: "fabric/fabric", HTTP: srv.Client()}, &ops
}

func TestFabricGitCommit(t *testing.T) {
	g, ops := fakeForgejo(t, map[string]string{"a.yaml": "old", "same.yaml": "same", "gone.yaml": "x"})
	sha, err := g.Commit(context.Background(), Author{Email: "ana@example.org"}, "m", []FileChange{
		{Path: "a.yaml", Content: []byte("new")}, {Path: "same.yaml", Content: []byte("same")},
		{Path: "b.yaml", Content: []byte("b")}, {Path: "gone.yaml"}, {Path: "never.yaml"},
	})
	if err != nil || sha != "abc" {
		t.Fatal(sha, err)
	}
	got := map[string]string{}
	for _, o := range *ops {
		got[o["path"].(string)] = o["operation"].(string)
		if o["operation"] != "create" && o["sha"] == nil {
			t.Errorf("%v needs the file's sha", o)
		}
	}
	if len(got) != 3 || got["a.yaml"] != "update" || got["b.yaml"] != "create" || got["gone.yaml"] != "delete" {
		t.Fatalf("ops: %v", got)
	}
}

func TestYAML(t *testing.T) {
	m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "ana", ResourceVersion: "7", UID: "u", Finalizers: []string{"x"},
		Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}", "wecolab.io/keep": "yes"}}}
	m.Status.Invite = "secret-link"
	b, err := YAML(m, v1alpha1.GroupVersion.WithKind("Member"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, bad := range []string{"resourceVersion", "uid", "finalizers", "last-applied", "status", "secret-link", "creationTimestamp"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q kept:\n%s", bad, s)
		}
	}
	if !strings.Contains(s, "kind: Member") || !strings.Contains(s, "wecolab.io/keep") {
		t.Errorf("identity lost:\n%s", s)
	}
	if p, ok := Path(v1alpha1.GroupVersion.WithKind("App"), "vince", "docs"); !ok || p != "fabric/apps/vince/docs.yaml" {
		t.Error(p)
	}
	if _, ok := Path(v1alpha1.GroupVersion.WithKind("Secret"), "vince", "docs"); ok {
		t.Error("only fabric objects have a place in Git")
	}
}

// A deployed app's folder: the Secret is encrypted to the recipients and decrypts back; data
// objects are kept from pruning; nothing carries a namespace. Needs sops and age-keygen.
func TestWorkloadFiles(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops not installed")
	}
	if _, err := exec.LookPath("age-keygen"); err != nil {
		t.Skip("age-keygen not installed")
	}
	key, err := exec.Command("age-keygen").Output()
	if err != nil {
		t.Fatal(err)
	}
	pub := ""
	for _, l := range strings.Split(string(key), "\n") {
		if strings.HasPrefix(l, "# public key: ") {
			pub = strings.TrimPrefix(l, "# public key: ")
		}
	}
	sec := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": "wiki", "namespace": "vince"}, "stringData": map[string]any{"password": "hunter2"}}}
	pvc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "wiki-data", "namespace": "vince"}, "spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}}}}
	files, err := WorkloadFiles("vince", "wiki", []*unstructured.Unstructured{sec, pvc}, []string{pub})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = string(f.Content)
	}
	enc := got["projects/vince/wiki/secret-wiki.sops.yaml"]
	if strings.Contains(enc, "hunter2") || !strings.Contains(enc, pub) {
		t.Fatalf("secret not encrypted to the recipient:\n%s", enc)
	}
	dec := exec.Command("sops", "decrypt", "--input-type", "yaml", "--output-type", "yaml", "/dev/stdin")
	dec.Stdin = strings.NewReader(enc)
	dec.Env = append(os.Environ(), "SOPS_AGE_KEY="+string(key))
	plain, err := dec.Output()
	if err != nil || !strings.Contains(string(plain), "hunter2") {
		t.Fatalf("does not decrypt with the site's key: %v\n%s", err, plain)
	}
	if v := got["projects/vince/wiki/persistentvolumeclaim-wiki-data.yaml"]; !strings.Contains(v, "prune: disabled") || strings.Contains(v, "namespace") {
		t.Fatalf("volume:\n%s", v)
	}
	if k := got["projects/vince/wiki/kustomization.yaml"]; !strings.Contains(k, "secret-wiki.sops.yaml") {
		t.Fatalf("kustomization:\n%s", k)
	}
}

// Edit commits against the hashes it read; when Forgejo says a file changed meanwhile (409), it reads
// again and asks again, so a change is never made on a stale read.
func TestEditRetriesOnConflict(t *testing.T) {
	files := map[string]string{"sites/a.yaml": "v1"}
	posts, calls := 0, 0
	var lastSHA any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/fabric/fabric/contents"), "/")
		switch r.Method {
		case http.MethodGet:
			c, ok := files[p]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "sha": "sha-" + c, "content": base64.StdEncoding.EncodeToString([]byte(c))})
		case http.MethodPost:
			var body struct{ Files []map[string]any }
			_ = json.NewDecoder(r.Body).Decode(&body)
			posts++
			lastSHA = body.Files[0]["sha"]
			if posts == 1 { // someone else committed in between
				files["sites/a.yaml"] = "v2"
				http.Error(w, "sha does not match", http.StatusConflict)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c2"}})
		}
	}))
	defer srv.Close()
	g := &Git{URL: srv.URL, Token: "t", Repo: "fabric/fabric", HTTP: srv.Client()}
	sha, err := g.Edit(context.Background(), Author{}, "m", []string{"sites/a.yaml"}, func(s *Snapshot) ([]FileChange, error) {
		calls++
		old, _ := s.Get("sites/a.yaml")
		return []FileChange{{Path: "sites/a.yaml", Content: append(old, '+')}}, nil
	})
	if err != nil || sha != "c2" || calls != 2 || lastSHA != "sha-v2" {
		t.Fatalf("sha=%q err=%v calls=%d lastSHA=%v", sha, err, calls, lastSHA)
	}
	if _, err := g.Edit(context.Background(), Author{}, "m", []string{"sites/a.yaml"}, func(*Snapshot) ([]FileChange, error) {
		return []FileChange{{Path: "sites/b.yaml", Content: []byte("x")}}, nil
	}); err == nil || !strings.Contains(err.Error(), "was not read") {
		t.Fatalf("a change to a file not read must be refused: %v", err)
	}
	for _, bad := range []string{"../etc/passwd", "keys/../x", "/abs", "a//b", ""} {
		if _, err := g.Edit(context.Background(), Author{}, "m", []string{bad}, func(*Snapshot) ([]FileChange, error) { return nil, nil }); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
}

// A commit made for a request says which: the Console logs that request under the same id.
func TestEditRequestTrailer(t *testing.T) {
	var msg string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct{ Message string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		msg = body.Message
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "abc"}})
	}))
	defer srv.Close()
	g := &Git{URL: srv.URL, Token: "t", Repo: "fabric/fabric", HTTP: srv.Client()}
	ctx := WithRequest(context.Background(), "r1d")
	if _, err := g.Commit(ctx, Author{Email: "ana@example.org"}, "add a", []FileChange{{Path: "a.yaml", Content: []byte("a")}}); err != nil {
		t.Fatal(err)
	}
	if msg != "add a\n\nRequest-Id: r1d" {
		t.Fatalf("message %q", msg)
	}
}
