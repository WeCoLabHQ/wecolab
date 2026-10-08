package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/yaml"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/warden"
)

// Old site reports cannot prove a completed backup. The operator records actual
// restored-data readback and fences old Forgejo writers before entering maintenance.
type upgradeProof struct {
	SnapshotSHA256    string            `json:"snapshotSHA256"`
	RecoverySHA256    string            `json:"recoverySHA256"`
	WriterFencedAt    time.Time         `json:"writerFencedAt"`
	WriterFenceMethod string            `json:"writerFenceMethod"`
	Sites             []string          `json:"sites"`
	Apps              []upgradeAppProof `json:"apps"`
}

type upgradeAppProof struct {
	Name                  string    `json:"name"`    // namespace/name
	Archive               string    `json:"archive"` // existing primary Barman namespace, NOT a new ArchiveID
	BackupID              string    `json:"backupID"`
	SystemID              string    `json:"systemID"`
	BackupCompletedAt     time.Time `json:"backupCompletedAt"`
	WALObservedAt         time.Time `json:"walObservedAt"`
	RestoredAt            time.Time `json:"restoredAt"`
	RestoreReadbackSHA256 string    `json:"restoreReadbackSHA256"`
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyUpgradeProof(receipt, snapshot, recovery string, now time.Time, sites []string, apps []v1alpha1.App) (*upgradeProof, error) {
	if receipt == "" {
		return nil, fmt.Errorf("backup and restore readback receipt required before beginning upgrade")
	}
	payload, err := os.ReadFile(receipt)
	if err != nil {
		return nil, err
	}
	var proof upgradeProof
	if err = json.Unmarshal(payload, &proof); err != nil {
		return nil, err
	}
	if proof.WriterFencedAt.IsZero() || proof.WriterFencedAt.Before(now.Add(-time.Hour)) || proof.WriterFencedAt.After(now) || strings.TrimSpace(proof.WriterFenceMethod) == "" {
		return nil, fmt.Errorf("old Console/Warden Forgejo writers have no recent fencing receipt")
	}
	observed := slices.Clone(proof.Sites)
	slices.Sort(observed)
	wanted := slices.Clone(sites)
	slices.Sort(wanted)
	if len(observed) != len(wanted) || !slices.Equal(observed, wanted) || len(slices.Compact(observed)) != len(observed) {
		return nil, fmt.Errorf("operator site inventory differs from live Git")
	}
	for label, entry := range map[string]struct{ path, digest string }{"snapshot": {snapshot, proof.SnapshotSHA256}, "recovery material": {recovery, proof.RecoverySHA256}} {
		actual, err := fileDigest(entry.path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		if actual != entry.digest || len(entry.digest) != 64 {
			return nil, fmt.Errorf("%s differs from recovery receipt", label)
		}
	}
	receipts := make(map[string]upgradeAppProof, len(proof.Apps))
	for _, entry := range proof.Apps {
		if entry.Name == "" || receipts[entry.Name].Name != "" {
			return nil, fmt.Errorf("duplicate/empty backup proof app identity")
		}
		receipts[entry.Name] = entry
	}
	count := 0
	for _, app := range apps {
		if app.Spec.Database == "" || app.Spec.Deleted {
			continue
		}
		count++
		name := app.Namespace + "/" + app.Name
		if !slices.Contains(sites, app.Spec.Primary) {
			return nil, fmt.Errorf("%s primary absent from site inventory", name)
		}
		copy := app.DeepCopy()
		if copy.Spec.ArchiveID == "" {
			copy.Spec.ArchiveID = copy.Spec.Database
		} // legacy identity is immutable through migration
		expected := warden.ArchiveName(copy, copy.Spec.Primary)
		got, ok := receipts[name]
		if !ok || expected == "" || got.Archive != expected || got.BackupID == "" || got.SystemID == "" ||
			got.BackupCompletedAt.IsZero() || got.BackupCompletedAt.After(now) || got.BackupCompletedAt.Before(now.Add(-48*time.Hour)) ||
			got.WALObservedAt.Before(now.Add(-15*time.Minute)) || got.WALObservedAt.After(now) ||
			got.RestoredAt.IsZero() || !got.RestoredAt.After(got.BackupCompletedAt) || got.RestoredAt.Before(now.Add(-48*time.Hour)) || got.RestoredAt.After(now) ||
			len(got.RestoreReadbackSHA256) != 64 {
			return nil, fmt.Errorf("%s: missing/stale backup, WAL or actual restore readback proof for legacy archive %s", name, expected)
		}
		if _, err := hex.DecodeString(got.RestoreReadbackSHA256); err != nil {
			return nil, fmt.Errorf("%s: invalid restore SHA-256", name)
		}
	}
	if len(receipts) != count {
		return nil, fmt.Errorf("receipt includes unknown or removed database app")
	}
	return &proof, nil
}

// gitUpgradeApps reads project/App inventory from a single immutable main commit.
func gitUpgradeApps(ctx context.Context, g *fabric.Git, revision string) ([]v1alpha1.App, error) {
	projects, err := g.ListAt(ctx, "fabric/projects", revision)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	namespaces := map[string]bool{}
	for _, project := range projects {
		if !strings.HasSuffix(project, ".yaml") {
			continue
		}
		ns := strings.TrimSuffix(path.Base(project), ".yaml")
		if ns == "" || namespaces[ns] || project != "fabric/projects/"+ns+".yaml" {
			return nil, fmt.Errorf("invalid or duplicate Git project %s", project)
		}
		namespaces[ns] = true
		files, err := g.ListAt(ctx, "fabric/apps/"+ns, revision)
		if err != nil {
			return nil, err
		}
		for _, p := range files {
			if !strings.HasSuffix(p, ".yaml") {
				continue
			}
			if !strings.HasPrefix(p, "fabric/apps/"+ns+"/") || path.Base(p) == ".yaml" {
				return nil, fmt.Errorf("invalid Git app path %s", p)
			}
			paths = append(paths, p)
		}
	}
	apps := make([]v1alpha1.App, 0, len(paths))
	owners := map[string]bool{}
	for _, p := range paths {
		b, ok, err := g.ReadAt(ctx, p, revision)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("Git app missing from pinned upgrade inventory: %s", p)
		}
		var a v1alpha1.App
		if err := yaml.UnmarshalStrict(b, &a); err != nil {
			return nil, fmt.Errorf("invalid Git app %s: %w", p, err)
		}
		identity := a.Namespace + "/" + a.Name
		if a.Kind != "App" || a.APIVersion != v1alpha1.GroupVersion.String() ||
			a.Name == "" || !namespaces[a.Namespace] || p != "fabric/apps/"+a.Namespace+"/"+a.Name+".yaml" ||
			owners[identity] {
			return nil, fmt.Errorf("invalid or duplicate Git app identity at %s", p)
		}
		owners[identity] = true
		apps = append(apps, a)
	}
	return apps, nil
}

func matchLocalUpgradeApps(gitApps, localApps []v1alpha1.App, localSite string) error {
	byName := make(map[string]v1alpha1.App, len(gitApps))
	observed := make(map[string]bool, len(localApps))
	for _, app := range gitApps {
		byName[app.Namespace+"/"+app.Name] = app
	}
	for _, app := range localApps {
		key := app.Namespace + "/" + app.Name
		authoritative, ok := byName[key]
		if !ok {
			if app.Spec.Database != "" && !app.Spec.Deleted && app.Spec.Primary == localSite {
				return fmt.Errorf("%s: local database app absent from writer Git", key)
			}
			continue
		}
		observed[key] = true
		if app.Spec.Database != authoritative.Spec.Database || app.Spec.Deleted != authoritative.Spec.Deleted ||
			app.Spec.Primary != authoritative.Spec.Primary || app.Spec.ArchiveID != authoritative.Spec.ArchiveID {
			return fmt.Errorf("%s: local app differs from writer Git", key)
		}
	}
	for _, app := range gitApps {
		if app.Spec.Database != "" && !app.Spec.Deleted && app.Spec.Primary == localSite && !observed[app.Namespace+"/"+app.Name] {
			return fmt.Errorf("%s/%s: local primary database app missing", app.Namespace, app.Name)
		}
	}
	return nil
}

func localUpgradeBackupMatches(got warden.VaultStatus, receipt upgradeAppProof, archive string, now time.Time) bool {
	return got.Err == "" && got.BackupEvidence != nil && got.BackupEvidence.Archive == archive &&
		got.BackupEvidence.ID == receipt.BackupID && got.BackupEvidence.SystemID == receipt.SystemID &&
		got.BackupEvidence.CompletedAt.Equal(receipt.BackupCompletedAt) &&
		!got.LatestWAL.Before(now.Add(-15*time.Minute))
}

func upgradeBackupPreflight(ctx context.Context, g *fabric.Git, revision string, sites []string, receipt, snapshot, recovery, localSite string) error {
	config, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("backup preflight kubeconfig: %w", err)
	}
	kube, err := client.New(config, client.Options{Scheme: scheme()})
	if err != nil {
		return err
	}
	apps := &v1alpha1.AppList{}
	if err = kube.List(ctx, apps); err != nil {
		return err
	}
	authoritative, err := gitUpgradeApps(ctx, g, revision)
	if err != nil {
		return err
	}
	proof, err := verifyUpgradeProof(receipt, snapshot, recovery, time.Now(), sites, authoritative)
	if err != nil {
		return err
	}
	if err := matchLocalUpgradeApps(authoritative, apps.Items, localSite); err != nil {
		return err
	}
	if !slices.Contains(sites, localSite) {
		return fmt.Errorf("local site %q is not in the Fabric", localSite)
	}
	for _, app := range authoritative {
		if app.Spec.Database == "" || app.Spec.Deleted || app.Spec.Primary != localSite {
			continue
		}
		copy := app.DeepCopy()
		if copy.Spec.ArchiveID == "" {
			copy.Spec.ArchiveID = copy.Spec.Database
		}
		oldArchive := warden.ArchiveName(copy, localSite)
		got := warden.VaultOf(ctx, kube, app.Namespace, app.Spec.Database, oldArchive)
		var receiptApp *upgradeAppProof
		for i := range proof.Apps {
			if proof.Apps[i].Name == app.Namespace+"/"+app.Name {
				receiptApp = &proof.Apps[i]
				break
			}
		}
		if receiptApp == nil || !localUpgradeBackupMatches(got, *receiptApp, oldArchive, time.Now()) {
			return fmt.Errorf("%s/%s: local current archive inspection does not match restore receipt: %s", app.Namespace, app.Name, got.Err)
		}
	}
	return nil
}
