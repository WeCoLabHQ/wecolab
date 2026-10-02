package main

import (
	"os"
	"strings"
	"testing"
)

func TestFillEnv(t *testing.T) {
	items := []catalogEnv{{Name: "DATABASE_URL", DB: "uri"}, {Name: "ADMIN_PASSWORD", Value: "${secret:admin_password}"}, {Name: "URL", Value: "https://${hostname}/"}, {Name: "DSN", Value: "redis://:${secret:redis}@localhost"}}
	gen, sec := map[string]string{}, map[string]string{}
	out := fillEnv(items, "wiki", "wiki-db", map[string]string{"hostname": "wiki.example"}, gen, sec)
	if len(out) != 4 || gen["admin_password"] == "" || gen["redis"] == "" {
		t.Fatalf("secrets not generated: %v", gen)
	}
	if out[2].(map[string]any)["value"] != "https://wiki.example/" {
		t.Errorf("placeholder not filled: %v", out[2])
	}
	if !strings.Contains(sec["env-DSN"], gen["redis"]) {
		t.Errorf("embedded secret must land in the app secret: %v", sec)
	}
	ref := out[0].(map[string]any)["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
	if ref["name"] != "wiki-db-app" || ref["key"] != "uri" {
		t.Errorf("database field wrong: %v", ref)
	}
}

func TestCatalogLoads(t *testing.T) {
	c := loadCatalog()
	if len(c) < 200 || c["miniflux"].Database == nil || c["freshrss"].Port != 80 {
		t.Fatalf("catalog not embedded as expected: %d entries", len(c))
	}
	if _, _, _, err := catalogPod(c["miniflux"], "rss", "rss-db", map[string]string{}, map[string]string{}, map[string]string{}); err != nil {
		t.Error(err)
	}
}

// The owner's Secret is the contract with CNPG (username, password) and with apps (the keys a catalog
// entry's env may read, under the name CNPG would use).
func TestDBOwner(t *testing.T) {
	s := dbOwner("team", "wiki", "pw")
	d := s.Object["stringData"].(map[string]any)
	if s.GetName() != "wiki-db-app" || s.Object["type"] != "kubernetes.io/basic-auth" || d["username"] != "wiki" || d["password"] != "pw" {
		t.Fatalf("owner secret: %v", s.Object)
	}
	for _, k := range []string{"uri", "host", "port", "user", "password", "dbname"} {
		if d[k] == nil {
			t.Fatalf("no %s for catalog env", k)
		}
	}
	if d["uri"] != "postgresql://wiki:pw@wiki-db-rw.team:5432/wiki" {
		t.Fatalf("uri: %v", d["uri"])
	}
}

// A workspace is a desktop in its own VM, reached only from the mesh (decision 26).
func TestWorkspaceEntry(t *testing.T) {
	e, ok := loadCatalog()["workspace"]
	if !ok || e.Runtime != "kata" || !e.MeshOnly || e.Validated != "eligible" {
		t.Fatalf("workspace entry: %+v", e)
	}
	gen, sec := map[string]string{}, map[string]string{}
	containers, volumes, claims, err := catalogPod(e, "desk", "", map[string]string{}, gen, sec)
	if err != nil {
		t.Fatal(err)
	}
	main := containers[0].(map[string]any)
	if main["resources"].(map[string]any)["limits"].(map[string]any)["cpu"] != "4" {
		t.Errorf("the entry's resources replace the defaults: %v", main["resources"])
	}
	mounts := main["volumeMounts"].([]any)
	if len(claims) != 1 || len(volumes) != 2 || mounts[len(mounts)-1].(map[string]any)["mountPath"] != "/dev/shm" ||
		volumes[1].(map[string]any)["emptyDir"].(map[string]any)["medium"] != "Memory" {
		t.Errorf("a home claim and a memory /dev/shm: %v %v", volumes, mounts)
	}
	if gen["admin_password"] == "" || sec["env-PASSWD"] != gen["admin_password"] {
		t.Errorf("the desktop's password is the generated admin password the Console shows once: %v", sec)
	}
}

// The names the Console, the vendored Kata, the userns policy and install.sh must agree on.
func TestKataContract(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tpl := "../../internal/bootstrap/template/system/"
	kata, userns, install := read(tpl+"wecolab/kata.yaml"), read(tpl+"base/userns.yaml"), read("../../install.sh")
	for _, c := range []struct{ in, want, what string }{
		{kata, "kind: RuntimeClass\napiVersion: node.k8s.io/v1\nmetadata:\n  name: " + kataRuntime + "\n", "Kata's RuntimeClass"},
		{userns, "object.spec.runtimeClassName == '" + kataRuntime + "'", "the userns policy admitting it"},
		{kata, "wecolab.io/kvm: \"true\"", "Kata's nodes"},
		{kata, "SHIMS_X86_64\n          value: \"clh\"\n        - name: DEFAULT_SHIM_X86_64\n          value: \"clh\"", "clh as the only shim and the default"},
		{install, "label node \"$node\" wecolab.io/kvm=true", "install.sh labelling them"},
		{read(tpl + "wecolab/kustomization.yaml"), "kata.yaml", "system/wecolab applying Kata"},
	} {
		if !strings.Contains(c.in, c.want) {
			t.Errorf("%s: no %q", c.what, c.want)
		}
	}
}
