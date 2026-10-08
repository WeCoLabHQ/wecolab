package main

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
)

func TestForceRequiresCurrentFencingConfirmation(t *testing.T) {
	age := fakeSops(t)
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}, Spec: v1alpha1.AppSpec{Sites: []string{"a", "b"}, Primary: "a", Workload: "wiki", Database: "wiki-db", ArchiveID: "id-00000000000000000000000000000001"}}
	git := inGit(t, a, testSite("a", 1, true), testSite("b", 2, false))
	git["keys/a.age.pub"], git["keys/recovery.age.pub"] = testAge+"\n", testAge+"\n"
	vault, err := secretFile(vaultSecret("p"), map[string]string{"bucket": "wcl-x", "endpoint": "https://s3.example.org", "b2-key-id": "k1", "b2-key": "disposable", "key-version": "1", "mutation-revision": "0"}, []string{testAge})
	if err != nil {
		t.Fatal(err)
	}
	git[vault.Path] = string(vault.Content)
	s, f := testServer(t, git, age)
	before := f.commits
	if w := call(s.setPrimary, "POST", map[string]any{"To": "b", "Force": true}, "ns", "p", "app", "wiki"); w.Code != 400 || f.commits != before {
		t.Fatalf("unacknowledged force: %d %s", w.Code, w.Body)
	}
	preview := call(s.forcePreview, "GET", nil, "ns", "p", "app", "wiki")
	if preview.Code != 200 {
		t.Fatalf("preview %d: %s", preview.Code, preview.Body)
	}
	var fencing fencingConfirmation
	if err := json.Unmarshal(preview.Body.Bytes(), &fencing); err != nil {
		t.Fatal(err)
	}
	fencing.Method, fencing.Evidence = "power-off", "disposable host powered off and checked by operator"
	bad := fencing
	bad.ArchiveID = "other"
	if w := call(s.setPrimary, "POST", map[string]any{"To": "b", "Force": true, "Fencing": bad}, "ns", "p", "app", "wiki"); w.Code != 409 {
		t.Fatalf("wrong incarnation: %d %s", w.Code, w.Body)
	}
	if w := call(s.setPrimary, "POST", map[string]any{"To": "b", "Force": true, "Fencing": fencing}, "ns", "p", "app", "wiki"); w.Code != 200 {
		t.Fatalf("acknowledged force: %d %s", w.Code, w.Body)
	}
	got := fromFake(t, s, &v1alpha1.App{ObjectMeta: a.ObjectMeta})
	if got.Spec.Primary != "b" || got.Spec.Force == nil || got.Spec.Force.Actor == "" || got.Spec.Force.Method != "power-off" || got.Spec.Force.From != "a" {
		t.Fatalf("force audit missing: %+v", got.Spec)
	}
	if w := call(s.setPrimary, "POST", map[string]any{"To": "a", "Force": true, "Fencing": fencing}, "ns", "p", "app", "wiki"); w.Code != 409 {
		t.Fatalf("replayed revision: %d %s", w.Code, w.Body)
	}
}
