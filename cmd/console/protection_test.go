package main

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/warden"
)

func TestProtectionUsesDesiredFileScopeAndNeverInventsRestore(t *testing.T) {
	for _, hasFiles := range []bool{false, true} {
		a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "files"}, Spec: v1alpha1.AppSpec{Sites: []string{"a"}, Primary: "a", Workload: "files"}}
		git := inGit(t, a)
		folder := fabric.AppFolder("p", "files")
		git[folder+"/deployment.yaml"] = "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: files}\nspec: {template: {spec: {containers: [{name: app, image: example.org/app}]}}}\n"
		if hasFiles {
			git[folder+"/pvc.yaml"] = "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: files}\nspec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 5Gi}}}\n"
		}
		s, _ := testServer(t, git, a)
		w := call(s.state, "GET", nil)
		if w.Code != 200 {
			t.Fatalf("state: %d %s", w.Code, w.Body)
		}
		var out struct {
			Apps []struct{ Protection warden.Protection }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Apps) != 1 {
			t.Fatalf("unexpected apps: %s", w.Body)
		}
		p := out.Apps[0].Protection
		want := "not-applicable"
		if hasFiles {
			want = "local-only"
		}
		if p.Database.State != "not-applicable" || p.Files.State != want || p.RecoveryPoint.State != "not-applicable" || p.LastVerifiedRestore != nil {
			t.Fatalf("protection conflates scopes: %+v", p)
		}
	}
}
