package warden

import (
	"context"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"net/http/httptest"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

func TestMigrateLegacyRefusesDuplicateRouteWithoutAssigningWinner(t *testing.T) {
	app := func(project string) *v1alpha1.App {
		return &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: project}, Spec: v1alpha1.AppSpec{Hostname: "wiki.example.org", Workload: "wiki"}}
	}
	docs := map[string]string{}
	for _, project := range []string{"a", "b"} {
		a := app(project)
		b, err := fabric.YAML(a, v1alpha1.GroupVersion.WithKind("App"))
		if err != nil {
			t.Fatal(err)
		}
		docs["fabric/projects/"+project+".yaml"] = "project"
		docs["fabric/apps/"+project+"/wiki.yaml"] = string(b)
	}
	docs[fabric.UpgradePath] = `{"phase":"maintenance","sites":["home"]}`
	cp := &fakeCopy{exists: true, files: docs}
	srv := httptest.NewServer(cp)
	defer srv.Close()
	g := &fabric.Git{URL: srv.URL, Repo: "fabric/fabric", Token: "x", HTTP: srv.Client()}
	if err := MigrateLegacy(context.Background(), g, "AGE-SECRET-KEY-1"); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("ambiguous legacy route not blocked: %v", err)
	}
	if cp.commits != 0 || cp.files[fabric.PublicClaimPath("wiki.example.org")] != "" {
		t.Fatal("migration assigned an arbitrary owner before detecting collision")
	}
}

func TestMigrateLegacyPreservesArchiveAndCredentialVersions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "p"}, Spec: v1alpha1.AppSpec{Database: "wiki-db", Primary: "home", Sites: []string{"home"}, Archive: map[string]int{"home": 3}}}
	before := "wiki-db-home-g3"
	b, err := fabric.YAML(app, v1alpha1.GroupVersion.WithKind("App"))
	if err != nil {
		t.Fatal(err)
	}
	dirPath := fabric.AppFolder("p", "wiki")
	sp := dirPath + "/secret-wiki.sops.yaml"
	vp := "secrets/vault-p.sops.yaml"
	clusterPath := dirPath + "/cluster-wiki-db.yaml"
	kustomPath := dirPath + "/kustomization.yaml"
	docs := map[string]string{"fabric/projects/p.yaml": "project", "fabric/apps/p/wiki.yaml": string(b),
		sp:                 "apiVersion: v1\nkind: Secret\nmetadata: {name: wiki, namespace: p}\nstringData: {b2-key-id: K0, b2-key: secret, db-password: keep}\nsops:\n  age:\n    - recipient: age1writer\n",
		vp:                 "apiVersion: v1\nkind: Secret\nmetadata: {name: vault-p, namespace: wecolab-system}\nstringData: {b2-key-id: K0, b2-key: secret, bucket: wcl-test}\nsops:\n  age:\n    - recipient: age1writer\n",
		clusterPath:        "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: wiki-db\n  annotations: {custom: retained}\nspec:\n  storage: {size: 40Gi}\n  plugins: [{name: barman-cloud.cloudnative-pg.io, parameters: {serverName: wiki-db-home-g3}}]\n  bootstrap: {recovery: {source: historical}}\n  monitoring:\n    customQueriesConfigMap: [{name: custom-monitor, key: other}]\n    enablePodMonitor: true\n",
		kustomPath:         "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [cluster-wiki-db.yaml, secret-wiki.sops.yaml, custom.yaml]\n",
		fabric.UpgradePath: `{"phase":"maintenance","sites":["home"]}`}
	cp := &fakeCopy{exists: true, files: docs}
	srv := httptest.NewServer(cp)
	defer srv.Close()
	g := &fabric.Git{URL: srv.URL, Repo: "fabric/fabric", Token: "x", HTTP: srv.Client()}
	if err := MigrateLegacy(context.Background(), g, "AGE-SECRET-KEY-1"); err != nil {
		t.Fatal(err)
	}
	commits := cp.commits
	if err := MigrateLegacy(context.Background(), g, "AGE-SECRET-KEY-1"); err != nil {
		t.Fatal(err)
	}
	if cp.commits != commits {
		t.Fatalf("repeat migration committed again: %d -> %d", commits, cp.commits)
	}
	var got v1alpha1.App
	if err := yaml.Unmarshal([]byte(cp.file("fabric/apps/p/wiki.yaml")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.ArchiveID != "wiki-db" || ArchiveName(&got, "home") != before || got.Spec.Archive["home"] != 3 {
		t.Fatalf("legacy namespace changed: %s archive=%v", got.Spec.ArchiveID, got.Spec.Archive)
	}
	var sec, vault corev1.Secret
	if err := yaml.Unmarshal([]byte(cp.file(sp)), &sec); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(cp.file(vp)), &vault); err != nil {
		t.Fatal(err)
	}
	if sec.StringData["key-version"] != "0" || sec.StringData["db-password"] != "keep" || vault.StringData["key-version"] != "0" || vault.StringData["mutation-revision"] != "0" {
		t.Fatalf("legacy credential version or owner password lost: app=%v vault=%v", sec.StringData, vault.StringData)
	}
	if cp.file(fabric.MigrationCompletePath) == "" || cp.file(fabric.PlacementRevisionPath) == "" {
		t.Fatal("migration marker or placement revision missing")
	}
	var cluster struct {
		Metadata struct {
			Name        string            `json:"name"`
			Namespace   string            `json:"namespace"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Storage    map[string]any   `json:"storage"`
			Plugins    []map[string]any `json:"plugins"`
			Bootstrap  map[string]any   `json:"bootstrap"`
			Monitoring struct {
				CustomQueriesConfigMap []struct {
					Name string `json:"name"`
					Key  string `json:"key"`
				} `json:"customQueriesConfigMap"`
				MetricsQueriesTTL *metav1.Duration `json:"metricsQueriesTTL"`
				EnablePodMonitor  bool             `json:"enablePodMonitor"`
			} `json:"monitoring"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(cp.file(clusterPath)), &cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Metadata.Name != "wiki-db" || cluster.Metadata.Namespace != "" || cluster.Metadata.Annotations["custom"] != "retained" || cluster.Spec.Storage["size"] != "40Gi" || len(cluster.Spec.Plugins) != 1 {
		t.Fatalf("cluster identity or storage/archive changed: %+v", cluster)
	}
	pluginParams, ok := cluster.Spec.Plugins[0]["parameters"].(map[string]any)
	if !ok || pluginParams["serverName"] != "wiki-db-home-g3" {
		t.Fatalf("archive plugin changed: %+v", cluster.Spec.Plugins)
	}
	recovery, ok := cluster.Spec.Bootstrap["recovery"].(map[string]any)
	if !ok || recovery["source"] != "historical" {
		t.Fatalf("bootstrap changed: %+v", cluster.Spec.Bootstrap)
	}
	refs := cluster.Spec.Monitoring.CustomQueriesConfigMap
	if !cluster.Spec.Monitoring.EnablePodMonitor || cluster.Spec.Monitoring.MetricsQueriesTTL == nil || cluster.Spec.Monitoring.MetricsQueriesTTL.Duration != 0 || len(refs) != 2 || refs[0].Name != "custom-monitor" || refs[0].Key != "other" || refs[1].Name != "wiki-db-recovery-metrics" || refs[1].Key != "queries" {
		t.Fatalf("cluster monitoring changed incorrectly: %+v", cluster.Spec.Monitoring)
	}
}

func TestMigrateLegacyRejectsMissingOrMalformedDatabaseWorkload(t *testing.T) {
	for _, tc := range []struct{ name, cluster, kustom string }{
		{"missing cluster", "", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [cluster-wiki-db.yaml]\n"},
		{"malformed cluster", "kind: Service\nmetadata: {name: wiki-db}\n", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [cluster-wiki-db.yaml]\n"},
		{"missing kustomization", "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata: {name: wiki-db}\nspec: {}\n", ""},
		{"unlisted cluster", "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata: {name: wiki-db}\nspec: {}\n", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [other.yaml]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "p"}, Spec: v1alpha1.AppSpec{Database: "wiki-db"}}
			b, err := fabric.YAML(app, v1alpha1.GroupVersion.WithKind("App"))
			if err != nil {
				t.Fatal(err)
			}
			dir := fabric.AppFolder("p", "wiki")
			docs := map[string]string{"fabric/projects/p.yaml": "project", "fabric/apps/p/wiki.yaml": string(b), "secrets/vault-p.sops.yaml": "vault", fabric.UpgradePath: `{"phase":"maintenance","sites":["home"]}`}
			if tc.cluster != "" {
				docs[dir+"/cluster-wiki-db.yaml"] = tc.cluster
			}
			if tc.kustom != "" {
				docs[dir+"/kustomization.yaml"] = tc.kustom
			}
			cp := &fakeCopy{exists: true, files: docs}
			srv := httptest.NewServer(cp)
			defer srv.Close()
			g := &fabric.Git{URL: srv.URL, Repo: "fabric/fabric", Token: "x", HTTP: srv.Client()}
			if err := MigrateLegacy(context.Background(), g, "AGE-SECRET-KEY-1"); err == nil {
				t.Fatal("invalid workload was migrated")
			}
			if cp.file(fabric.MigrationCompletePath) != "" || cp.file(dir+"/configmap-wiki-db-recovery-metrics.yaml") != "" {
				t.Fatal("invalid workload received completed migration or metrics")
			}
		})
	}
}
