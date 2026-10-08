package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/warden"
)

func TestUpgradeProofRequiresExactLegacyArchiveAndRecovery(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	app := v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}}
	app.Spec.Database = "wiki-db"
	app.Spec.Primary = "home"
	dir := t.TempDir()
	snapshot, recovery := filepath.Join(dir, "snapshot"), filepath.Join(dir, "recovery")
	for _, path := range []string{snapshot, recovery} {
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := fileDigest(snapshot)
	r, _ := fileDigest(recovery)
	data := upgradeProof{SnapshotSHA256: s, RecoverySHA256: r, WriterFencedAt: now.Add(-time.Minute), WriterFenceMethod: "old writer pods stopped and Forgejo write grants revoked", Sites: []string{"home"}, Apps: []upgradeAppProof{{Name: "p/wiki", Archive: "wiki-db-home", BackupID: "completed-backup", SystemID: "database-system", BackupCompletedAt: now.Add(-time.Hour), WALObservedAt: now.Add(-time.Minute), RestoredAt: now.Add(-time.Minute), RestoreReadbackSHA256: strings.Repeat("a", 64)}}}
	receipt := filepath.Join(dir, "proof.json")
	verify := func(p upgradeProof) error {
		t.Helper()
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(receipt, b, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = verifyUpgradeProof(receipt, snapshot, recovery, now, []string{"home"}, []v1alpha1.App{app})
		return err
	}
	if err := verify(data); err != nil {
		t.Fatalf("valid operator receipt rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*upgradeProof)
	}{
		{"wrong archive", func(p *upgradeProof) { p.Apps[0].Archive = "wiki-db-other" }},
		{"stale backup", func(p *upgradeProof) { p.Apps[0].BackupCompletedAt = now.Add(-72 * time.Hour) }},
		{"restore predates backup", func(p *upgradeProof) { p.Apps[0].RestoredAt = now.Add(-2 * time.Hour) }},
		{"no restore readback", func(p *upgradeProof) { p.Apps[0].RestoreReadbackSHA256 = "" }},
		{"unfenced writers", func(p *upgradeProof) { p.WriterFenceMethod = "" }},
		{"wrong site", func(p *upgradeProof) { p.Sites = []string{"other"} }},
		{"wrong snapshot", func(p *upgradeProof) { p.SnapshotSHA256 = hex.EncodeToString(sha256.New().Sum(nil)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := data
			p.Sites = append([]string(nil), data.Sites...)
			p.Apps = append([]upgradeAppProof(nil), data.Apps...)
			tc.change(&p)
			if err := verify(p); err == nil {
				t.Fatal("unsafe proof accepted")
			}
		})
	}
}

func TestLocalUpgradeBackupRequiresExactCompletionTime(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	completed := now.Add(-time.Hour)
	got := warden.VaultStatus{
		BackupEvidence: &warden.BackupEvidence{
			Archive: "wiki-db-home", ID: "backup", SystemID: "system", CompletedAt: completed,
		},
		LatestWAL: now.Add(-time.Minute),
	}
	receipt := upgradeAppProof{
		BackupID: "backup", SystemID: "system", BackupCompletedAt: completed,
	}
	if !localUpgradeBackupMatches(got, receipt, "wiki-db-home", now) {
		t.Fatal("matching inspected backup refused")
	}
	receipt.BackupCompletedAt = completed.Add(-time.Second)
	if localUpgradeBackupMatches(got, receipt, "wiki-db-home", now) {
		t.Fatal("receipt completion time differs from inspected backup")
	}
}

func TestUpgradeInventoryUsesGitIncludingRemoteOnlyApps(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	local := v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}}
	local.Spec.Database = "wiki-db"
	local.Spec.Primary = "home"
	remote := v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "remote"}}
	remote.Spec.Database = "remote-db"
	remote.Spec.Primary = "away"
	dir := t.TempDir()
	snapshot, recovery := filepath.Join(dir, "snapshot"), filepath.Join(dir, "recovery")
	for _, p := range []string{snapshot, recovery} {
		if err := os.WriteFile(p, []byte(p), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := fileDigest(snapshot)
	r, _ := fileDigest(recovery)
	receipt := filepath.Join(dir, "proof.json")
	proof := upgradeProof{SnapshotSHA256: s, RecoverySHA256: r, WriterFencedAt: now.Add(-time.Minute), WriterFenceMethod: "old writers fenced", Sites: []string{"home", "away"}, Apps: []upgradeAppProof{{Name: "p/wiki", Archive: "wiki-db-home", BackupID: "backup", SystemID: "system", BackupCompletedAt: now.Add(-time.Hour), WALObservedAt: now.Add(-time.Minute), RestoredAt: now.Add(-time.Minute), RestoreReadbackSHA256: strings.Repeat("a", 64)}}}
	writeProof := func() {
		t.Helper()
		b, err := json.Marshal(proof)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(receipt, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProof()
	docs := map[string]string{}
	for _, a := range []v1alpha1.App{local, remote} {
		b, err := fabric.YAML(&a, v1alpha1.GroupVersion.WithKind("App"))
		if err != nil {
			t.Fatal(err)
		}
		docs["fabric/apps/p/"+a.Name+".yaml"] = string(b)
	}
	git := upgradeGitFixture(t, docs)
	apps, err := gitUpgradeApps(context.Background(), git, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyUpgradeProof(receipt, snapshot, recovery, now, []string{"home", "away"}, apps); err == nil {
		t.Fatal("remote-only Git database app has no receipt")
	}
	stale := local.DeepCopy()
	stale.Name = "remote"
	stale.Spec.Database = "stale-db"
	stale.Spec.Primary = "home"
	if err := matchLocalUpgradeApps(apps, []v1alpha1.App{local, *stale}, "home"); err == nil {
		t.Fatal("stale local app replaced Git inventory")
	}
	proof.Apps = append(proof.Apps, upgradeAppProof{Name: "p/remote", Archive: "remote-db-away", BackupID: "remote-backup", SystemID: "remote-system", BackupCompletedAt: now.Add(-time.Hour), WALObservedAt: now.Add(-time.Minute), RestoredAt: now.Add(-time.Minute), RestoreReadbackSHA256: strings.Repeat("b", 64)})
	writeProof()
	if _, err := verifyUpgradeProof(receipt, snapshot, recovery, now, []string{"home", "away"}, apps); err != nil {
		t.Fatalf("all Git apps proven: %v", err)
	}
}

func TestUpgradeInventoryRejectsMalformedAndDuplicateGitApps(t *testing.T) {
	app := v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}}
	app.Spec.Database = "wiki-db"
	app.Spec.Primary = "home"
	b, err := fabric.YAML(&app, v1alpha1.GroupVersion.WithKind("App"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		docs map[string]string
	}{
		{"missing", map[string]string{"fabric/apps/p/wiki.yaml": "<missing>"}},
		{"malformed", map[string]string{"fabric/apps/p/wiki.yaml": "kind: App\nmetadata: [broken"}},
		{"wrong identity", map[string]string{"fabric/apps/p/other.yaml": string(b)}},
		{"duplicate identity", map[string]string{"fabric/apps/p/wiki.yaml": string(b), "fabric/apps/p/other.yaml": string(b)}},
		{"duplicate database", map[string]string{"fabric/apps/p/wiki.yaml": strings.Replace(string(b), "spec:\n", "spec:\n  database: remote-db\n  database: \"\"\n", 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := gitUpgradeApps(context.Background(), upgradeGitFixture(t, tc.docs), strings.Repeat("a", 40)); err == nil {
				t.Fatal("unsafe Git inventory accepted")
			}
		})
	}
}

func upgradeGitFixture(t *testing.T, docs map[string]string) *fabric.Git {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/fabric/fabric/contents/")
		if r.Method != http.MethodGet {
			t.Errorf("unexpected Git mutation %s", r.Method)
			http.Error(w, "no mutation", 405)
			return
		}
		if r.URL.Query().Get("ref") != strings.Repeat("a", 40) {
			t.Errorf("inventory read was not pinned: %s", r.URL.RawQuery)
			http.Error(w, "unpinned inventory read", http.StatusBadRequest)
			return
		}
		if p == "fabric/projects" {
			fmt.Fprint(w, `[{"type":"file","path":"fabric/projects/p.yaml"}]`)
			return
		}
		if p == "fabric/apps/p" {
			entries := []map[string]string{}
			for name := range docs {
				entries = append(entries, map[string]string{"type": "file", "path": name})
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		if data, ok := docs[path.Clean(p)]; ok {
			if data == "<missing>" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(data)), "sha": "fixture"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &fabric.Git{URL: srv.URL, Token: "t", Repo: "fabric/fabric", HTTP: srv.Client()}
}
