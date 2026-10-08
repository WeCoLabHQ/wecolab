package warden

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"sigs.k8s.io/yaml"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// MigrateLegacy runs only under the durable maintenance gate. ageKey is a raw
// writer/recovery SOPS age identity, never a path or provider credential. It
// preserves pre-existing archive namespaces and encrypted Secret recipients.
func MigrateLegacy(ctx context.Context, g *fabric.Git, ageKey string) error {
	if strings.TrimSpace(ageKey) == "" {
		return fmt.Errorf("migration requires a SOPS age identity")
	}
	review, err := fabric.UpgradeReview(ctx, g)
	if err != nil {
		return err
	}
	if review.Phase != "maintenance" {
		return fmt.Errorf("migration requires maintenance")
	}
	projects, err := g.List(ctx, "fabric/projects")
	if err != nil {
		return err
	}
	type item struct {
		p string
		a v1alpha1.App
	}
	apps := []item{}
	owners := map[string]string{}
	for _, project := range projects {
		ns := strings.TrimSuffix(path.Base(project), ".yaml")
		files, err := g.List(ctx, "fabric/apps/"+ns)
		if err != nil {
			return err
		}
		for _, p := range files {
			if !strings.HasSuffix(p, ".yaml") {
				continue
			}
			b, ok, err := g.Read(ctx, p)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("app disappeared during migration: %s", p)
			}
			var a v1alpha1.App
			if err := yaml.Unmarshal(b, &a); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if a.Name == "" || a.Namespace != ns {
				return fmt.Errorf("app %s has invalid identity", p)
			}
			owner := ns + "/" + a.Name
			for _, claim := range []struct{ path, name string }{{fabric.PublicClaimPath(a.Spec.Hostname), a.Spec.Hostname}, {fabric.MeshClaimPath(a.Name + "-" + ns), a.Name + "-" + ns}} {
				if claim.name == "" || claim.path == fabric.MeshClaimPath(a.Name+"-"+ns) && !a.Spec.Mesh {
					continue
				}
				if prev := owners[claim.path]; prev != "" && prev != owner {
					return fmt.Errorf("legacy route collision %s: %s and %s; resolve before migration", claim.name, prev, owner)
				}
				owners[claim.path] = owner
			}
			apps = append(apps, item{p, a})
		}
	}
	for _, it := range apps {
		if it.a.Spec.Database == "" {
			continue
		}
		vp := "secrets/vault-" + it.a.Namespace + ".sops.yaml"
		if _, ok, err := g.Read(ctx, vp); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("database app %s has no project vault", it.p)
		}
	}
	// The inventory and collision check precede any write. Each subsequent edit is
	// idempotent, so interruption never allocates a different identity or owner.
	for _, it := range apps {
		a := it.a
		appPath := it.p
		ns, name := a.Namespace, a.Name
		claimPaths := []string{}
		if a.Spec.Hostname != "" {
			claimPaths = append(claimPaths, fabric.PublicClaimPath(a.Spec.Hostname))
		}
		if a.Spec.Mesh {
			claimPaths = append(claimPaths, fabric.MeshClaimPath(name+"-"+ns))
		}
		paths := append([]string{appPath}, claimPaths...)
		if a.Spec.Database != "" {
			dir := fabric.AppFolder(ns, name)
			paths = append(paths, dir+"/cluster-"+a.Spec.Database+".yaml", dir+"/kustomization.yaml", dir+"/configmap-"+a.Spec.Database+"-recovery-metrics.yaml")
		}
		_, err = g.EditMigration(ctx, fabric.Author{}, "migrate legacy archive, route identity and recovery metrics", paths, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
			b, ok := s.Get(appPath)
			if !ok {
				return nil, fmt.Errorf("app %s disappeared", appPath)
			}
			var current v1alpha1.App
			if err := yaml.Unmarshal(b, &current); err != nil {
				return nil, err
			}
			if current.Spec.Database != a.Spec.Database {
				return nil, fmt.Errorf("app %s database identity changed during migration", appPath)
			}
			if current.Spec.Database != "" {
				if current.Spec.ArchiveID == "" {
					current.Spec.ArchiveID = current.Spec.Database
				}
				if a.Spec.ArchiveID != "" && current.Spec.ArchiveID != a.Spec.ArchiveID {
					return nil, fmt.Errorf("app %s archive identity changed during migration", appPath)
				}
			}
			out := []fabric.FileChange{}
			if current.Spec.Database != "" && a.Spec.ArchiveID == "" {
				updated, err := fabric.YAML(&current, v1alpha1.GroupVersion.WithKind("App"))
				if err != nil {
					return nil, err
				}
				out = append(out, fabric.FileChange{Path: appPath, Content: updated})
			}
			if current.Spec.Database != "" {
				changes, err := migrateRecoveryMetrics(s, ns, name, current.Spec.Database)
				if err != nil {
					return nil, err
				}
				out = append(out, changes...)
			}
			if current.Spec.Hostname != "" {
				f, err := fabric.ClaimRoute(s, fabric.PublicClaimPath(current.Spec.Hostname), current.Spec.Hostname, ns+"/"+name)
				if err != nil {
					return nil, err
				}
				out = append(out, f)
			}
			if current.Spec.Mesh {
				label := name + "-" + ns
				f, err := fabric.ClaimRoute(s, fabric.MeshClaimPath(label), label, ns+"/"+name)
				if err != nil {
					return nil, err
				}
				out = append(out, f)
			}
			return out, nil
		})
		if err != nil {
			return err
		}
	}
	for _, project := range projects {
		ns := strings.TrimSuffix(path.Base(project), ".yaml")
		vpath := "secrets/vault-" + ns + ".sops.yaml"
		_, ok, err := g.Read(ctx, vpath)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		_, err = g.EditMigration(ctx, fabric.Author{}, "migrate vault credential revision", []string{vpath}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
			enc, ok := s.Get(vpath)
			if !ok {
				return nil, fmt.Errorf("vault %s disappeared", vpath)
			}
			data, err := openVaultFile(enc, path.Base(vpath), ageKey)
			if err != nil {
				return nil, err
			}
			if data["key-version"] != "" && data["mutation-revision"] != "" {
				return nil, nil
			}
			out, err := reseal(enc, path.Base(vpath), ageKey, func(v map[string]string) error {
				if v["key-version"] == "" {
					v["key-version"] = "0"
				}
				if v["mutation-revision"] == "" {
					v["mutation-revision"] = "0"
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			return []fabric.FileChange{{Path: vpath, Content: out}}, nil
		})
		if err != nil {
			return err
		}
	}
	for _, it := range apps {
		if it.a.Spec.Database == "" {
			continue
		}
		p := fabric.AppFolder(it.a.Namespace, it.a.Name) + "/secret-" + it.a.Name + ".sops.yaml"
		vp := "secrets/vault-" + it.a.Namespace + ".sops.yaml"
		_, err = g.EditMigration(ctx, fabric.Author{}, "migrate app credential version", []string{p, vp}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
			enc, ok := s.Get(p)
			if !ok {
				return nil, fmt.Errorf("database app secret missing: %s", p)
			}
			vb, ok := s.Get(vp)
			if !ok {
				return nil, fmt.Errorf("vault missing: %s", vp)
			}
			vault, err := openVaultFile(vb, path.Base(vp), ageKey)
			if err != nil {
				return nil, err
			}
			current, err := openVaultFile(enc, path.Base(p), ageKey)
			if err != nil {
				return nil, err
			}
			if current["key-version"] != "" {
				return nil, fabric.CheckKeyVersion(current, vault)
			}
			if vault["key-version"] != "0" || current["b2-key-id"] != vault["b2-key-id"] {
				return nil, fmt.Errorf("legacy app %s key version cannot be inferred; reconcile credentials before upgrade", p)
			}
			out, err := reseal(enc, path.Base(p), ageKey, func(v map[string]string) error {
				v["key-version"] = "0"
				return fabric.CheckKeyVersion(v, vault)
			})
			if err != nil {
				return nil, err
			}
			return []fabric.FileChange{{Path: p, Content: out}}, nil
		})
		if err != nil {
			return err
		}
	}
	_, err = g.EditMigration(ctx, fabric.Author{}, "record completed legacy migration", []string{fabric.PlacementRevisionPath, fabric.MigrationCompletePath}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
		changes := []fabric.FileChange{}
		if _, ok := s.Get(fabric.PlacementRevisionPath); !ok {
			changes = append(changes, fabric.FileChange{Path: fabric.PlacementRevisionPath, Content: []byte(`{"revision":"0"}`)})
		}
		if _, ok := s.Get(fabric.MigrationCompletePath); !ok {
			b, _ := json.Marshal(map[string]any{"archiveIDs": true, "routeClaims": true, "keyVersions": true})
			changes = append(changes, fabric.FileChange{Path: fabric.MigrationCompletePath, Content: b})
		}
		return changes, nil
	})
	return err
}

// migrateRecoveryMetrics updates only the deployed Cluster's monitoring and its
// Kustomization; the Cluster's storage, archive and bootstrap remain untouched.
func migrateRecoveryMetrics(s *fabric.Snapshot, ns, app, db string) ([]fabric.FileChange, error) {
	dir := fabric.AppFolder(ns, app)
	clusterPath := dir + "/cluster-" + db + ".yaml"
	kustomPath := dir + "/kustomization.yaml"
	metricsName := db + "-recovery-metrics"
	metricsFile := "configmap-" + metricsName + ".yaml"
	metricsPath := dir + "/" + metricsFile
	load := func(p string) (map[string]any, error) {
		b, ok := s.Get(p)
		if !ok {
			return nil, fmt.Errorf("database workload missing: %s", p)
		}
		var doc map[string]any
		if err := yaml.UnmarshalStrict(b, &doc); err != nil {
			return nil, fmt.Errorf("invalid workload %s: %w", p, err)
		}
		if doc == nil {
			return nil, fmt.Errorf("invalid workload %s: empty document", p)
		}
		return doc, nil
	}
	field := func(doc map[string]any, key, p string) (map[string]any, error) {
		value, ok := doc[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid workload %s: missing or malformed %s", p, key)
		}
		return value, nil
	}
	cluster, err := load(clusterPath)
	if err != nil {
		return nil, err
	}
	metadata, err := field(cluster, "metadata", clusterPath)
	if err != nil {
		return nil, err
	}
	if version, _ := cluster["apiVersion"].(string); version != "postgresql.cnpg.io/v1" {
		return nil, fmt.Errorf("invalid database Cluster apiVersion: %s", clusterPath)
	}
	if kind, _ := cluster["kind"].(string); kind != "Cluster" {
		return nil, fmt.Errorf("invalid database Cluster kind: %s", clusterPath)
	}
	clusterName, _ := metadata["name"].(string)
	clusterNS, _ := metadata["namespace"].(string)
	if clusterName != db || metadata["namespace"] != nil && clusterNS != ns {
		return nil, fmt.Errorf("invalid database Cluster identity: %s", clusterPath)
	}
	spec, err := field(cluster, "spec", clusterPath)
	if err != nil {
		return nil, err
	}
	kustom, err := load(kustomPath)
	if err != nil {
		return nil, err
	}
	if kind, _ := kustom["kind"].(string); kind != "Kustomization" {
		return nil, fmt.Errorf("invalid workload Kustomization: %s", kustomPath)
	}
	resources, ok := kustom["resources"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid workload %s: malformed resources", kustomPath)
	}
	listed := false
	metricsListed := false
	for _, entry := range resources {
		resource, ok := entry.(string)
		if !ok {
			return nil, fmt.Errorf("invalid workload %s: non-string resource", kustomPath)
		}
		if resource == "cluster-"+db+".yaml" {
			listed = true
		}
		if resource == metricsFile {
			metricsListed = true
		}
	}
	if !listed {
		return nil, fmt.Errorf("database Cluster not listed in %s", kustomPath)
	}
	var monitoring map[string]any
	if existing, ok := spec["monitoring"]; ok {
		monitoring, ok = existing.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid workload %s: malformed monitoring", clusterPath)
		}
	} else {
		monitoring = map[string]any{}
		spec["monitoring"] = monitoring
	}
	refs := []any{}
	if existing, ok := monitoring["customQueriesConfigMap"]; ok {
		refs, ok = existing.([]any)
		if !ok {
			return nil, fmt.Errorf("invalid workload %s: malformed customQueriesConfigMap", clusterPath)
		}
	}
	clusterChanged := false
	found := false
	for _, entry := range refs {
		ref, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid workload %s: malformed custom query reference", clusterPath)
		}
		refName, _ := ref["name"].(string)
		if refName == metricsName {
			if found {
				return nil, fmt.Errorf("duplicate recovery metrics reference in %s", clusterPath)
			}
			found = true
			if key, _ := ref["key"].(string); key != "queries" {
				ref["key"] = "queries"
				clusterChanged = true
			}
		}
	}
	if !found {
		monitoring["customQueriesConfigMap"] = append(refs, map[string]any{"name": metricsName, "key": "queries"})
		clusterChanged = true
	}
	if ttl, ok := monitoring["metricsQueriesTTL"].(string); !ok || ttl != "0s" {
		monitoring["metricsQueriesTTL"] = "0s"
		clusterChanged = true
	}
	changes := []fabric.FileChange{}
	if clusterChanged {
		b, err := yaml.Marshal(cluster)
		if err != nil {
			return nil, err
		}
		changes = append(changes, fabric.FileChange{Path: clusterPath, Content: b})
	}
	if !metricsListed {
		kustom["resources"] = append(resources, metricsFile)
		b, err := yaml.Marshal(kustom)
		if err != nil {
			return nil, err
		}
		changes = append(changes, fabric.FileChange{Path: kustomPath, Content: b})
	}
	var metrics map[string]any
	_, exists := s.Get(metricsPath)
	if exists {
		metrics, err = load(metricsPath)
		if err != nil {
			return nil, err
		}
		md, err := field(metrics, "metadata", metricsPath)
		if err != nil {
			return nil, err
		}
		version, _ := metrics["apiVersion"].(string)
		kind, _ := metrics["kind"].(string)
		configName, _ := md["name"].(string)
		configNS, _ := md["namespace"].(string)
		if version != "v1" || kind != "ConfigMap" || configName != metricsName || md["namespace"] != nil && configNS != ns {
			return nil, fmt.Errorf("invalid recovery ConfigMap identity: %s", metricsPath)
		}
	} else {
		metrics = map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": metricsName}}
	}
	md := metrics["metadata"].(map[string]any)
	metricsChanged := !exists
	labels := map[string]any{}
	if existing, ok := md["labels"]; ok {
		labels, ok = existing.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid workload %s: malformed labels", metricsPath)
		}
	}
	if label, _ := labels["cnpg.io/reload"].(string); label != "true" {
		labels["cnpg.io/reload"] = "true"
		md["labels"] = labels
		metricsChanged = true
	}
	data := map[string]any{}
	if existing, ok := metrics["data"]; ok {
		data, ok = existing.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid workload %s: malformed data", metricsPath)
		}
	}
	if query, _ := data["queries"].(string); query != RecoveryQuery {
		data["queries"] = RecoveryQuery
		metrics["data"] = data
		metricsChanged = true
	}
	if metricsChanged {
		b, err := yaml.Marshal(metrics)
		if err != nil {
			return nil, err
		}
		changes = append(changes, fabric.FileChange{Path: metricsPath, Content: b})
	}
	return changes, nil
}
