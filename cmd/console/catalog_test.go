package main

import (
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

func TestCatalogPodDatabaseReferencesBothContainers(t *testing.T) {
	fields := []string{"host", "dbname", "user", "password", "port", "uri"}
	env := make([]catalogEnv, 0, len(fields)+2)
	for _, field := range fields {
		env = append(env, catalogEnv{Name: "DB_" + field, DB: field})
	}
	env = append(env, catalogEnv{Name: "SHARED", Value: "${secret:shared}"})
	e := catalogEntry{Title: "Sample", Image: "sample:1", Port: 8080, Env: env,
		Sidecars: []catalogSidecar{{Name: "worker", Image: "worker:1", Env: append(append([]catalogEnv{}, env...), catalogEnv{Name: "CACHE", Value: "localhost"})}}}
	generated, secrets := map[string]string{}, map[string]string{}
	containers, _, _, err := catalogPod(e, "sample", "sample-db", nil, generated, secrets)
	if err != nil {
		t.Fatal(err)
	}
	owner := dbOwner("team", "sample", "pw").Object["stringData"].(map[string]any)
	for _, container := range containers {
		for _, item := range container.(map[string]any)["env"].([]any) {
			env := item.(map[string]any)
			if !strings.HasPrefix(env["name"].(string), "DB_") {
				continue
			}
			field := strings.TrimPrefix(env["name"].(string), "DB_")
			key := field
			if key == "user" {
				key = "username"
			}
			ref := env["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
			if ref["name"] != "sample-db-app" || ref["key"] != key || ref["optional"] != nil || owner[key] == nil {
				t.Errorf("%v has wrong owner-secret reference: %v", container.(map[string]any)["name"], env)
			}
		}
	}
	worker := containers[1].(map[string]any)["env"].([]any)
	if worker[len(worker)-1].(map[string]any)["value"] != "localhost" || generated["shared"] == "" ||
		secrets["env-SHARED"] != generated["shared"] {
		t.Fatalf("sidecar literals or shared secret: %v %v", worker, generated)
	}
	if _, _, _, err := catalogPod(e, "sample", "", nil, map[string]string{}, map[string]string{}); err == nil {
		t.Fatal("database references without a database must fail")
	}
}

func TestCatalogLoads(t *testing.T) {
	c := loadCatalog()
	if len(c) < 200 || c["listmonk"].Database == nil || c["freshrss"].Port != 80 {
		t.Fatalf("catalog not embedded as expected: %d entries", len(c))
	}
	if _, _, _, err := catalogPod(c["listmonk"], "listmonk", "listmonk-db", map[string]string{}, map[string]string{}, map[string]string{}); err != nil {
		t.Error(err)
	}
	if len(c["miniflux"].Unsupported) == 0 {
		t.Fatal("nonportable imported database URL must not be deployed silently")
	}
}

func TestCatalogAuthentikWorkerUsesDatabaseOwner(t *testing.T) {
	e := loadCatalog()["authentik"]
	containers, _, claims, err := catalogPod(e, "authentik", "authentik-db", nil, map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) < 2 || len(claims) == 0 {
		t.Fatalf("worker or claims missing: %v %v", containers, claims)
	}
	expected := map[string]string{
		"AUTHENTIK_POSTGRESQL__HOST": "host", "AUTHENTIK_POSTGRESQL__NAME": "dbname",
		"AUTHENTIK_POSTGRESQL__PASSWORD": "password", "AUTHENTIK_POSTGRESQL__USER": "username",
	}
	for _, container := range containers[:2] {
		found := map[string]bool{}
		for _, value := range container.(map[string]any)["env"].([]any) {
			item := value.(map[string]any)
			key, ok := expected[item["name"].(string)]
			if !ok {
				continue
			}
			valueFrom, ok := item["valueFrom"].(map[string]any)
			if !ok {
				t.Errorf("literal database field in %s: %v", container.(map[string]any)["name"], item)
				continue
			}
			ref, ok := valueFrom["secretKeyRef"].(map[string]any)
			if !ok || ref["name"] != "authentik-db-app" || ref["key"] != key || ref["optional"] != nil {
				t.Errorf("wrong %s reference in %s: %v", item["name"], container.(map[string]any)["name"], item)
			}
			found[item["name"].(string)] = true
		}
		if len(found) != len(expected) {
			t.Errorf("%s missing database refs: %v", container.(map[string]any)["name"], found)
		}
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
