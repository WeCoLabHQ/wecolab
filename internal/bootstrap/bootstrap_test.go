package bootstrap

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestOnlyControllers(t *testing.T) {
	m := "apiVersion: v1\nkind: Namespace\nmetadata: { name: flux-system }\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: { name: helm-controller }\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: { name: source-controller }\n"
	out := string(OnlyControllers([]byte(m), "source-controller"))
	if strings.Contains(out, "helm-controller") || !strings.Contains(out, "source-controller") || !strings.Contains(out, "kind: Namespace") {
		t.Fatalf("%s", out)
	}
}

const fluxManifest = `---
apiVersion: v1
kind: Namespace
metadata: { name: flux-system }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: kustomize-controller, namespace: flux-system }
spec:
  template:
    spec:
      containers:
        - name: manager
          image: ghcr.io/fluxcd/kustomize-controller:v1
          args: [--watch-all-namespaces=true, --no-remote-bases=false]
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: source-controller, namespace: flux-system }
spec: { template: { spec: { containers: [{ name: manager, args: [--log-level=info] }] } } }
`

func TestLockdown(t *testing.T) {
	out, err := Lockdown([]byte(fluxManifest))
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(string(out), "\n---")
	if len(docs) != 3 || !strings.Contains(docs[2], "--log-level=info") || strings.Contains(docs[2], "no-remote") {
		t.Fatalf("only the kustomize-controller changes:\n%s", out)
	}
	dep := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(docs[1]), &dep.Object); err != nil {
		t.Fatal(err)
	}
	cs, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	args, _, _ := unstructured.NestedStringSlice(cs[0].(map[string]any), "args")
	want := []string{"--watch-all-namespaces=true", "--no-cross-namespace-refs=true", "--no-remote-bases=true", "--default-service-account=default"}
	if !slices.Equal(args, want) {
		t.Fatalf("args %v, want %v", args, want)
	}
	if _, err := Lockdown([]byte("apiVersion: v1\nkind: Namespace\nmetadata: { name: flux-system }\n")); err == nil {
		t.Fatal("a manifest without the kustomize-controller cannot be locked down")
	}
}

func platform(t *testing.T) map[string][]byte {
	files, err := Platform(Options{Version: "v9.9.9", Fetch: func(url string) ([]byte, error) {
		if strings.Contains(url, "flux") {
			return []byte(fluxManifest), nil
		}
		return []byte("apiVersion: v1\nkind: Namespace\nmetadata: { name: x }\n"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestPlatform(t *testing.T) {
	files := platform(t)
	if !strings.Contains(string(files["system/wecolab/warden.yaml"]), "ghcr.io/wecolabhq/warden:v9.9.9") {
		t.Fatal("the template is rendered for the version")
	}
	if !strings.Contains(string(files["system/vendor/flux/manifest.yaml"]), "--default-service-account=default") {
		t.Fatal("the vendored Flux is locked down")
	}
	if _, ok := files[settingsFile]; ok {
		t.Fatal("the platform never carries the fabric's settings")
	}
	for p, b := range files { // every manifest parses, so a broken template fails here and not at a site
		for _, d := range strings.Split(string(b), "\n---") {
			if err := yaml.Unmarshal([]byte(d), &map[string]any{}); err != nil && strings.HasSuffix(p, ".yaml") {
				t.Errorf("%s: %v", p, err)
			}
		}
	}
	have := []string{"crds/wecolab.io_apps.yaml", "crds/wecolab.io_gone.yaml", settingsFile, "system/base/old.yaml"}
	if got := stale(files, have); !slices.Equal(got, []string{"crds/wecolab.io_gone.yaml", "system/base/old.yaml"}) {
		t.Fatalf("an upgrade removes what the template no longer has, and never the settings: %v", got)
	}
}

// The admission policies and RBAC the template ships, as they must stay (docs/plans/2026-09-29-hardening.md, R5).
func TestTemplatePolicies(t *testing.T) {
	files := platform(t)
	userns := string(files["system/base/userns.yaml"])
	for _, want := range []string{"pods/ephemeralcontainers", "object.spec.initContainers.all(", "object.spec.ephemeralContainers.all(", "runAsUser == 0"} {
		if !strings.Contains(userns, want) {
			t.Errorf("the userns policy lost %q", want)
		}
	}
	// A keyless toleration tolerates the laptop and idle taints too; the binding must cover tenants.
	placement := string(files["system/base/placement.yaml"])
	for _, want := range []string{"!has(t.key) || t.key == '' || t.key.startsWith('wecolab.io/')", "priorityClassName == 'wecolab-best-effort'",
		"request.operation != 'CREATE' || !has(object.spec.nodeName)", "validationActions: [Deny]", "key: wecolab.io/tenant, operator: Exists"} {
		if !strings.Contains(placement, want) {
			t.Errorf("the placement policy lost %q", want)
		}
	}
	if !strings.Contains(string(files["system/base/kustomization.yaml"]), "placement.yaml") {
		t.Error("system/base does not apply the placement policy")
	}
	agent := string(files["system/wecolab/node-agent.yaml"])
	for _, want := range []string{"request.userInfo.extra['authentication.kubernetes.io/node-name'][0] == object.metadata.name", "mountPropagation: HostToContainer"} {
		if !strings.Contains(agent, want) {
			t.Errorf("the node agent lost %q", want)
		}
	}
	var role struct {
		Rules []struct{ Resources, Verbs []string }
	}
	console := strings.Split(string(files["system/steward/console.yaml"]), "\n---")
	for _, d := range console {
		if !strings.Contains(d, "kind: ClusterRole\n") {
			continue
		}
		if err := yaml.Unmarshal([]byte(d), &role); err != nil {
			t.Fatal(err)
		}
		for _, r := range role.Rules {
			if slices.Contains(r.Verbs, "impersonate") && !slices.Equal(r.Resources, []string{"users"}) || slices.Contains(r.Resources, "secrets") {
				t.Errorf("the Console's ClusterRole may impersonate users only and reads Secrets through Roles: %v", r)
			}
		}
	}
}

func TestCreateChecksNames(t *testing.T) {
	ok := Options{Zone: "fab.example.org", Email: "owner@example.org", Site: "home", Project: "vince", Host: "box1", PublicAddress: "203.0.113.7",
		Network: netip.MustParsePrefix("10.77.0.0/16")}
	if err := ok.check(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*Options){
		func(o *Options) { o.Zone = "fab.example.org\nevil" },
		func(o *Options) { o.Site = "Home" },
		func(o *Options) { o.Network = netip.MustParsePrefix("fd00::/64") },
		func(o *Options) { o.Network = netip.MustParsePrefix("10.77.0.0/25") },
		func(o *Options) { o.Site = "recovery" },
		func(o *Options) { o.Project = "a b" },
		func(o *Options) { o.Host = "-box" },
		func(o *Options) { o.Host = strings.Repeat("h", 60) },
		func(o *Options) { o.Email = "owner" },
		func(o *Options) { o.PublicAddress = "::1" },
	} {
		o := ok
		bad(&o)
		if o.check() == nil {
			t.Errorf("accepted %+v", o)
		}
	}
}

// The version goes into every site's manifests: it can be a tag only.
func TestPlatformVersion(t *testing.T) {
	for _, v := range []string{"", "v1\nkind: Secret", "v1: x", "-v1", "v1 x"} {
		if _, err := Platform(Options{Version: v, Fetch: func(string) ([]byte, error) { return nil, nil }}); err == nil {
			t.Errorf("rendered version %q", v)
		}
	}
}

// A fabric whose Flux is not locked down keeps its Flux until asked; one that is gets this version's.
func TestKeepFlux(t *testing.T) {
	locked, _ := Lockdown([]byte(fluxManifest))
	for _, c := range []struct {
		have       []byte
		lock, kept bool
	}{
		{[]byte(fluxManifest), false, true},
		{[]byte(fluxManifest), true, false},
		{locked, false, false},
	} {
		want := platform(t)
		if kept := keepFlux(want, c.have, c.lock); kept != c.kept {
			t.Errorf("lock %v: kept %v", c.lock, kept)
		}
		if _, ok := want[fluxFile]; ok == c.kept {
			t.Errorf("lock %v, kept %v: the upgrade writes Flux %v", c.lock, c.kept, ok)
		}
		if _, ok := want["system/wecolab/warden.yaml"]; !ok {
			t.Error("the rest is upgraded")
		}
	}
}
