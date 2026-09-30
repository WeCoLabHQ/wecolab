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
