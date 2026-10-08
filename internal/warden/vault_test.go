package warden

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

func TestVaultName(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9-]{6,63}$`)
	for _, c := range [][2]string{{"fab.example.org", "team"}, {"a.b", "x"}, {"very-long-fabric-domain-name.example.org", "a-project-with-a-long-name-too"}} {
		n := VaultName(c[0], c[1])
		if !re.MatchString(n) || len(n) < 6 || len(n) > 63 || n[:3] == "b2-" {
			t.Errorf("bad bucket name %q", n)
		}
	}
}

// Rotating a vault's key: sign in with the account key, find the bucket, make a key for it alone; then
// delete the old one, where a key B2 no longer has counts as deleted if the account holds the bucket. A
// bucket the account does not hold is refused.
func TestVaultKeyRotation(t *testing.T) {
	calls := []string{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		calls = append(calls, name)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch name {
		case "b2_authorize_account":
			if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("acct:secret")) {
				t.Errorf("authorize with %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"accountId": "A", "authorizationToken": "T",
				"apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": srv.URL, "s3ApiUrl": "https://s3.example"}}})
		case "b2_list_buckets":
			if body["bucketName"] == "wcl-x" {
				_ = json.NewEncoder(w).Encode(map[string]any{"buckets": []any{map[string]any{"bucketId": "B"}}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"buckets": []any{}})
			}
		case "b2_create_key":
			if body["bucketId"] != "B" || body["accountId"] != "A" || r.Header.Get("Authorization") != "T" {
				t.Errorf("a key for the bucket alone: %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"applicationKeyId": "new", "applicationKey": "k"})
		case "b2_delete_key":
			if body["applicationKeyId"] != "old" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "bad_request", "message": "no such key"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case "b2_list_keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"applicationKeyId": "held"}}})
		}
	}))
	defer srv.Close()
	B2API = srv.URL
	defer func() { B2API = "https://api.backblazeb2.com" }()
	ctx := context.Background()
	v, err := NewVaultKey(ctx, "acct", "secret", "wcl-x")
	if err != nil || v.KeyID != "new" || v.Key != "k" || v.Bucket != "wcl-x" || v.Endpoint != "https://s3.example" {
		t.Fatalf("%+v %v", v, err)
	}
	if err := DeleteVaultKey(ctx, "acct", "secret", "wcl-x", "old"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteVaultKey(ctx, "acct", "secret", "wcl-x", "gone"); err != nil {
		t.Fatalf("a key B2 no longer has counts as deleted: %v", err)
	}
	if err := DeleteVaultKey(ctx, "acct", "secret", "wcl-x", "held"); err == nil {
		t.Fatal("a key B2 still has but would not delete")
	}
	if err := DeleteVaultKey(ctx, "acct", "secret", "someone-elses", "gone"); err == nil {
		t.Fatal("a key of a bucket the account does not hold: this account cannot see it, so it may still work")
	}
	if _, err := NewVaultKey(ctx, "acct", "secret", "someone-elses"); err == nil {
		t.Fatal("a bucket the account does not hold")
	}
	if !slices.Contains(calls, "b2_delete_key") {
		t.Fatalf("calls: %v", calls)
	}
}

// Retirement waits for every database App still in Git, even while deletion
// waits for sites, and for exact report versions rather than key IDs alone.
func TestRetirable(t *testing.T) {
	app := func(name string, sites ...string) v1alpha1.App {
		a := v1alpha1.App{Spec: v1alpha1.AppSpec{Sites: sites, Primary: sites[0], Database: name + "-db"}}
		a.Name, a.Namespace = name, "p"
		return a
	}
	wiki, docs, gone, web := app("wiki", "a", "b"), app("docs", "b", "home"), app("gone", "sam"), app("web", "sam")
	gone.Spec.Deleted, web.Spec.Database = true, ""
	reports := map[string]*SiteStatus{
		"a":    {Site: "a", Apps: map[string]AppState{"p/wiki": {VaultKeyID: "new", VaultKeyVersion: "2"}}},
		"b":    {Site: "b", Apps: map[string]AppState{"p/wiki": {VaultKeyID: "new", VaultKeyVersion: "2"}, "p/docs": {VaultKeyID: "new", VaultKeyVersion: "2"}}},
		"home": {Site: "home", Apps: map[string]AppState{"p/docs": {VaultKeyID: "new", VaultKeyVersion: "2"}}},
		"sam":  {Site: "sam", Apps: map[string]AppState{"p/gone": {VaultKeyID: "new", VaultKeyVersion: "2"}}},
	}
	report := func(site string) *SiteStatus { return reports[site] }
	apps := []v1alpha1.App{wiki, docs, gone, web}
	for _, c := range []struct {
		why    string
		change func()
		want   []string
	}{
		{"every current Git app reports the new key and version", func() {}, nil},
		{"b still archives docs with the old key", func() { reports["b"].Apps["p/docs"] = AppState{VaultKeyID: "old", VaultKeyVersion: "1"} }, []string{"b"}},
		{"home does not answer", func() { delete(reports, "home") }, []string{"b", "home"}},
	} {
		c.change()
		if got := Retirable("new", "2", apps, report); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.why, got, c.want)
		}
	}
	if got := Retirable("new", "2", nil, report); len(got) != 0 {
		t.Errorf("no apps, nothing to wait for: %v", got)
	}
	if got := Retirable("", "2", apps, func(site string) *SiteStatus { return &SiteStatus{Site: site} }); len(got) == 0 {
		t.Error("a vault without a current key or reports cannot retire keys")
	}
	reports["a"].Apps["p/wiki"] = AppState{VaultKeyID: "new"}
	if got := Retirable("new", "2", apps, report); !slices.Contains(got, "a") {
		t.Fatal("missing site key version authorized retirement")
	}
}

// The writer inspects current Git App Secrets and fresh site reports before
// provider calls; retirement is checkpointed separately from those calls.
func TestRetire(t *testing.T) {
	dir := t.TempDir() // a sops that passes files through and says how it was called
	script := `#!/bin/sh
if [ "$1" = decrypt ]; then
  awk '/^sops:/{exit} {print}'
else
  awk -v recipients="$3" '{print} END {n=split(recipients,a,","); print "sops:"; print "  age:"; for(i=1;i<=n;i++) print "    - recipient: " a[i]}'
fi
`
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	deleted := []string{}
	failDelete := false
	var b2 *httptest.Server
	b2 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "b2_authorize_account":
			_ = json.NewEncoder(w).Encode(map[string]any{"accountId": "A", "authorizationToken": "T", "apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": b2.URL}}})
		case "b2_delete_key":
			if failDelete && body["applicationKeyId"] == "K1" {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "unavailable", "message": "try later"})
				return
			}
			deleted = append(deleted, body["applicationKeyId"].(string))
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case "b2_list_keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"applicationKeyId": "K1"}}})
		case "b2_list_buckets":
			_ = json.NewEncoder(w).Encode(map[string]any{"buckets": []any{map[string]string{"bucketId": "B"}}})
		}
	}))
	defer b2.Close()
	B2API = b2.URL
	defer func() { B2API = "https://api.backblazeb2.com" }()

	p := "secrets/vault-p.sops.yaml"
	appPath := "fabric/apps/p/wiki.yaml"
	sp := fabric.AppFolder("p", "wiki") + "/secret-wiki.sops.yaml"
	app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}, Spec: v1alpha1.AppSpec{Sites: []string{"home"}, Primary: "home", Database: "wiki-db", ArchiveID: "wiki-db"}}
	appYAML, err := fabric.YAML(app, v1alpha1.GroupVersion.WithKind("App"))
	if err != nil {
		t.Fatal(err)
	}
	cp := &fakeCopy{exists: true, files: map[string]string{
		p:                            "apiVersion: v1\nkind: Secret\nmetadata: {name: vault-p, namespace: wecolab-system}\nstringData: {b2-key-id: K3, b2-key: k3, bucket: wcl-x, retiring: K1 K2, key-version: '2', mutation-revision: '3'}\nsops:\n  age:\n    - recipient: age1a\n    - recipient: age1recovery\n",
		appPath:                      string(appYAML),
		sp:                           "apiVersion: v1\nkind: Secret\nmetadata: {name: wiki, namespace: p}\nstringData: {b2-key-id: K3, b2-key: k3, key-version: '2'}\nsops:\n  age:\n    - recipient: age1a\n",
		fabric.UpgradePath:           `{"phase":"ready","sites":["home"]}`,
		fabric.MigrationCompletePath: `{}`,
		fabric.PlacementRevisionPath: `{"revision":"0"}`,
	}}
	raceDeletion := false
	appDeleted := app.DeepCopy()
	appDeleted.Spec.Deleted = true
	deletedYAML, err := fabric.YAML(appDeleted, v1alpha1.GroupVersion.WithKind("App"))
	if err != nil {
		t.Fatal(err)
	}
	fj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raceDeletion && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/contents") {
			raceDeletion = false
			cp.mu.Lock()
			cp.files[appPath] = string(deletedYAML)
			cp.files[p] = strings.Replace(cp.files[p], "mutation-revision: '3'", "mutation-revision: '4'", 1)
			cp.mu.Unlock()
			http.Error(w, "vault changed during deletion", http.StatusConflict)
			return
		}
		cp.ServeHTTP(w, r)
	}))
	defer fj.Close()
	st := &SiteStatus{Site: "home", Apps: map[string]AppState{"p/wiki": {VaultKeyID: "K1", VaultKeyVersion: "1"}}}
	pr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(st) }))
	defer pr.Close()
	home := testSite("home", false, "10.77.2.1")
	acct := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNS, Name: StorageSecret}, Data: map[string][]byte{"key-id": []byte("acct"), "key": []byte("secret")}}
	age := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: FluxNS, Name: "sops-age"}, Data: map[string][]byte{"pub.agekey": []byte("AGE-SECRET-KEY-1")}}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(&home, acct, age).Build()
	w := &Writer{Client: c, Site: "pub", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}}
	retire := func() error {
		w.Peers = &Peers{Client: c, HTTP: &http.Client{Transport: toServer{pr.URL}}} // nothing cached
		return w.retire(context.Background())
	}

	if err := retire(); err != nil || len(deleted) != 0 || cp.commits != 0 {
		t.Fatalf("home still holds K1: %v, deleted %v, %d commits", err, deleted, cp.commits)
	}
	st.Apps["p/wiki"] = AppState{VaultKeyID: "K3", VaultKeyVersion: "2"}
	cp.mu.Lock()
	cp.files[appPath] = string(deletedYAML)
	cp.mu.Unlock()
	if err := retire(); err != nil || cp.commits != 0 || len(deleted) != 0 {
		t.Fatalf("pending database deletion acquired retirement: %v, commits %d, deleted %v", err, cp.commits, deleted)
	}
	cp.mu.Lock()
	cp.files[appPath] = string(appYAML)
	cp.mu.Unlock()
	raceDeletion = true
	if err := retire(); err == nil || cp.commits != 0 || len(deleted) != 0 {
		t.Fatalf("concurrent deletion failed to fence retirement: %v, commits %d, deleted %v", err, cp.commits, deleted)
	}
	heldBefore := &corev1.Secret{}
	if err := yaml.Unmarshal([]byte(cp.file(p)), heldBefore); err != nil {
		t.Fatal(err)
	}
	if heldBefore.StringData["retirement-phase"] != "" {
		t.Fatal("concurrent deletion persisted retirement phase")
	}
	cp.mu.Lock()
	cp.files[appPath] = string(appYAML)
	cp.mu.Unlock()
	failDelete = true
	if err := retire(); err == nil || len(deleted) != 0 {
		t.Fatalf("provider failure must retain retirement phase: %v %v", err, deleted)
	}
	held := &corev1.Secret{}
	if err := yaml.Unmarshal([]byte(cp.file(p)), held); err != nil {
		t.Fatal(err)
	}
	if held.StringData["retirement-phase"] == "" || held.StringData["b2-key-id"] != "K3" {
		t.Fatalf("failed deletion cleared claim or live key: %v", held.StringData)
	}
	failDelete = false
	w = &Writer{Client: c, Site: "pub", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}}
	if err := retire(); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := yaml.Unmarshal([]byte(cp.file(p)), got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"K1", "K2"}) || got.StringData["retiring"] != "" || got.StringData["retirement-phase"] != "" || got.StringData["b2-key-id"] != "K3" ||
		strings.Count(cp.file(p), "recipient: age1") != 2 {
		t.Fatalf("deleted %v; in Git:\n%s", deleted, cp.file(p))
	}
}
