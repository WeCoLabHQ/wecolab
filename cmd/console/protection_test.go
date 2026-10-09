package main

import (
	"encoding/json"
	"strings"
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

func stateFiles(t *testing.T, s *server) []string {
	t.Helper()
	w := call(s.state, "GET", nil)
	if w.Code != 200 {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
	var out struct {
		Apps []struct {
			Name       string
			Protection warden.Protection
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0, len(out.Apps))
	for _, a := range out.Apps {
		files = append(files, a.Name+":"+a.Protection.Files.State)
	}
	return files
}

func TestStateScopesFollowImmutableGitEvidence(t *testing.T) {
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "files"}, Spec: v1alpha1.AppSpec{Sites: []string{"a"}, Primary: "a", Workload: "files"}}
	folder := fabric.AppFolder("p", "files")
	deploy := folder + "/deployment.yaml"
	pvc := folder + "/pvc.yaml"
	git := inGit(t, a)
	git[deploy] = "kind: Deployment\nspec: {template: {spec: {containers: [{name: app, image: example.org/app}]}}}\n"
	s, f := testServer(t, git, a)
	want := func(expected string) {
		t.Helper()
		if got := strings.Join(stateFiles(t, s), ","); got != expected {
			t.Fatalf("files = %q, want %q", got, expected)
		}
	}
	want("files:not-applicable")
	f.mu.Lock()
	f.archiveUnreadable = true
	f.files[pvc] = "kind: PersistentVolumeClaim\n"
	f.mu.Unlock()
	want("files:unknown")
	f.mu.Lock()
	f.archiveUnreadable = false
	f.mu.Unlock()
	want("files:local-only")
	f.mu.Lock()
	delete(f.files, pvc)
	f.files[deploy] = "kind: Deployment\nspec: {template: {spec: {containers: [{name: app, image: example.org/app}], volumes: [{name: data, persistentVolumeClaim: {claimName: data}}]}}}\n"
	f.mu.Unlock()
	want("files:local-only")
	f.mu.Lock()
	f.files[deploy] = "kind: Deployment\nspec: {template: {spec: {containers: [{name: app, image: example.org/app}]}}}\n"
	f.archiveUnreadable = true
	f.mu.Unlock()
	want("files:unknown")
	f.mu.Lock()
	f.archiveUnreadable = false
	f.mu.Unlock()
	want("files:not-applicable")
	f.mu.Lock()
	f.headUnreadable = true
	f.mu.Unlock()
	want("files:unknown")
	f.mu.Lock()
	f.headUnreadable = false
	f.files[deploy] = "kind: Deployment\nspec: [broken\n"
	f.mu.Unlock()
	want("files:unknown")
}

func TestStateArchiveKeepsImmutableHeadAndRejectsIncompleteSnapshot(t *testing.T) {
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "files"}, Spec: v1alpha1.AppSpec{Sites: []string{"a"}, Primary: "a", Workload: "files"}}
	folder := fabric.AppFolder("p", "files")
	git := inGit(t, a)
	git[folder+"/deployment.yaml"] = "kind: Deployment\n"
	s, f := testServer(t, git, a)
	want := func(expected string) {
		t.Helper()
		if got := strings.Join(stateFiles(t, s), ","); got != expected {
			t.Fatalf("files = %q, want %q", got, expected)
		}
	}
	want("files:not-applicable")
	f.mu.Lock()
	f.archiveBroken = true
	f.files[folder+"/pvc.yaml"] = "kind: PersistentVolumeClaim\n"
	f.mu.Unlock()
	want("files:unknown")
	f.mu.Lock()
	f.archiveBroken = false
	f.mu.Unlock()
	want("files:local-only")
	f.mu.Lock()
	f.archiveUnreadable = true
	f.mu.Unlock()
	want("files:local-only") // verified facts from this exact head need no second archive
	f.mu.Lock()
	f.files[folder+"/pvc.yaml"] = "kind: ConfigMap\n"
	f.mu.Unlock()
	want("files:unknown") // new head cannot inherit old protection
}

func TestStateWantedManifestSymlinkCannotHideFiles(t *testing.T) {
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "files"}, Spec: v1alpha1.AppSpec{Sites: []string{"a"}, Primary: "a", Workload: "files"}}
	folder := fabric.AppFolder("p", "files")
	git := inGit(t, a)
	git[folder+"/deployment.yaml"] = "kind: Deployment\n"
	s, f := testServer(t, git, a)
	if got := strings.Join(stateFiles(t, s), ","); got != "files:not-applicable" {
		t.Fatal(got)
	}
	f.mu.Lock()
	f.symlinkPath = folder + "/pvc.yaml"
	f.files[f.symlinkPath] = "../pvc.yaml"
	f.mu.Unlock()
	if got := strings.Join(stateFiles(t, s), ","); got != "files:unknown" {
		t.Fatal(got)
	}
}

func TestStateArchiveUsesObservedHeadWhenMainMovesDuringTransfer(t *testing.T) {
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "files"}, Spec: v1alpha1.AppSpec{Sites: []string{"a"}, Primary: "a", Workload: "files"}}
	folder := fabric.AppFolder("p", "files")
	git := inGit(t, a)
	git[folder+"/deployment.yaml"] = "kind: Deployment\n"
	s, f := testServer(t, git, a)
	f.mu.Lock()
	f.onArchive = func() { f.files[folder+"/pvc.yaml"] = "kind: PersistentVolumeClaim\n" }
	f.mu.Unlock()
	if got := strings.Join(stateFiles(t, s), ","); got != "files:not-applicable" {
		t.Fatalf("archive did not use observed immutable head: %s", got)
	}
	if got := strings.Join(stateFiles(t, s), ","); got != "files:local-only" {
		t.Fatalf("next head did not observe new PVC: %s", got)
	}
}

func TestStateArchiveRequiresDesiredWorkloadAndExcludesSOPS(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, want string
	}{
		{"missing-folder", "", "unknown"},
		{"encrypted-pvc", "kind: Deployment\n", "not-applicable"},
		{"claim-template", "kind: StatefulSet\nspec: {volumeClaimTemplates: [{metadata: {name: data}}]}\n", "local-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "files"}, Spec: v1alpha1.AppSpec{Sites: []string{"a"}, Primary: "a", Workload: "files"}}
			git := inGit(t, a)
			folder := fabric.AppFolder("p", "files")
			if tc.manifest != "" {
				git[folder+"/workload.yaml"] = tc.manifest
				git[folder+"/secret.sops.yaml"] = "kind: PersistentVolumeClaim\nspec: [encrypted"
			}
			s, _ := testServer(t, git, a)
			if got := strings.Join(stateFiles(t, s), ","); got != "files:"+tc.want {
				t.Fatalf("files = %s, want %s", got, tc.want)
			}
		})
	}
}
