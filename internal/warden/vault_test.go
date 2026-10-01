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

// A vault's old keys wait for every site of each live database app of the project to report the new one;
// a site that does not answer is waited for.
func TestRetirable(t *testing.T) {
	app := func(name string, sites ...string) v1alpha1.App {
		a := v1alpha1.App{Spec: v1alpha1.AppSpec{Sites: sites, Primary: sites[0], Database: name + "-db"}}
		a.Name, a.Namespace = name, "p"
		return a
	}
	wiki, docs, gone, web := app("wiki", "a", "b"), app("docs", "b", "home"), app("gone", "sam"), app("web", "sam")
	gone.Spec.Deleted, web.Spec.Database = true, ""
	reports := map[string]*SiteStatus{
		"a":    {Site: "a", Apps: map[string]AppState{"p/wiki": {VaultKeyID: "new"}}},
		"b":    {Site: "b", Apps: map[string]AppState{"p/wiki": {VaultKeyID: "new"}, "p/docs": {VaultKeyID: "new"}}},
		"home": {Site: "home", Apps: map[string]AppState{"p/docs": {VaultKeyID: "new"}}},
	}
	report := func(site string) *SiteStatus { return reports[site] }
	apps := []v1alpha1.App{wiki, docs, gone, web}
	for _, c := range []struct {
		why    string
		change func()
		want   []string
	}{
		{"every site has the new key; sam holds only a deleted app and one without a database", func() {}, nil},
		{"b still archives docs with the old key", func() { reports["b"].Apps["p/docs"] = AppState{VaultKeyID: "old"} }, []string{"b"}},
		{"home does not answer", func() { delete(reports, "home") }, []string{"b", "home"}},
	} {
		c.change()
		if got := Retirable("new", apps, report); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.why, got, c.want)
		}
	}
	if got := Retirable("new", nil, report); len(got) != 0 {
		t.Errorf("no apps, nothing to wait for: %v", got)
	}
	if got := Retirable("", apps, func(site string) *SiteStatus { return &SiteStatus{Site: site} }); len(got) == 0 {
		t.Error("a vault without a current key, sites whose Secrets hold none: nothing says the old keys are unused")
	}
}

// The writer deletes a vault's old keys at B2 only once the sites report the new one, and only the keys
// they were checked against: Git may already retire a newer one the cluster's copy does not know yet. It
// takes them out of the vault in Git, encrypted to the recipients the file has.
func TestRetire(t *testing.T) {
	dir := t.TempDir() // a sops that passes files through and says how it was called
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte("#!/bin/sh\necho \"# sops $*\"\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	deleted := []string{}
	var b2 *httptest.Server
	b2 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "b2_authorize_account":
			_ = json.NewEncoder(w).Encode(map[string]any{"accountId": "A", "authorizationToken": "T", "apiInfo": map[string]any{"storageApi": map[string]any{"apiUrl": b2.URL}}})
		case "b2_delete_key":
			deleted = append(deleted, body["applicationKeyId"].(string))
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer b2.Close()
	B2API = b2.URL
	defer func() { B2API = "https://api.backblazeb2.com" }()

	p := "secrets/vault-p.sops.yaml"
	cp := &fakeCopy{exists: true, files: map[string]string{p: "apiVersion: v1\nkind: Secret\nmetadata: {name: vault-p, namespace: wecolab-system}\n" +
		"stringData: {b2-key-id: K3, b2-key: k3, bucket: wcl-x, retiring: K1 K2}\nsops:\n  age:\n    - recipient: age1a\n    - recipient: age1recovery\n"}}
	fj := httptest.NewServer(cp)
	defer fj.Close()
	st := &SiteStatus{Site: "home", Apps: map[string]AppState{"p/wiki": {VaultKeyID: "K1"}}}
	pr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(st) }))
	defer pr.Close()
	home := testSite("home", false, "10.77.2.1")
	app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}, Spec: v1alpha1.AppSpec{Sites: []string{"home"}, Primary: "home", Database: "wiki-db"}}
	vault := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNS, Name: "vault-p"}, Data: map[string][]byte{"b2-key-id": []byte("K2"), "retiring": []byte("K1")}}
	acct := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNS, Name: StorageSecret}, Data: map[string][]byte{"key-id": []byte("acct"), "key": []byte("secret")}}
	age := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: FluxNS, Name: "sops-age"}, Data: map[string][]byte{"pub.agekey": []byte("AGE-SECRET-KEY-1")}}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(&home, app, vault, acct, age).Build()
	w := &Writer{Client: c, Site: "pub", Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}}
	retire := func() error {
		w.Peers = &Peers{Client: c, HTTP: &http.Client{Transport: toServer{pr.URL}}} // nothing cached
		return w.retire(context.Background())
	}

	if err := retire(); err != nil || len(deleted) != 0 || cp.commits != 0 {
		t.Fatalf("home still holds K1: %v, deleted %v, %d commits", err, deleted, cp.commits)
	}
	st.Apps["p/wiki"] = AppState{VaultKeyID: "K2"}
	if err := retire(); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := yaml.Unmarshal([]byte(cp.file(p)), got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"K1"}) || got.StringData["retiring"] != "K2" || got.StringData["b2-key-id"] != "K3" || got.StringData["b2-key"] != "k3" ||
		!strings.Contains(cp.file(p), "--age age1a,age1recovery ") {
		t.Fatalf("deleted %v; in Git:\n%s", deleted, cp.file(p))
	}
}
