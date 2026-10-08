package fabric

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestVaultRevisionRejectsRegressionsAndOverflow(t *testing.T) {
	v := map[string]string{"key-version": "0", "mutation-revision": "0", "b2-key-id": "K0"}
	if err := NextKeyVersion(v); err != nil || v["key-version"] != "1" || v["mutation-revision"] != "1" {
		t.Fatalf("version failed: %v %v", err, v)
	}
	cases := []map[string]string{{"key-version": "0", "b2-key-id": "K0"}, {"key-version": "1", "b2-key-id": "different"}, {"b2-key-id": "K0"}}
	for _, app := range cases {
		if err := CheckKeyVersion(app, v); err == nil && app["key-version"] == "1" {
			t.Fatal("same version with another key accepted")
		}
	}
	v["mutation-revision"] = strconv.FormatUint(^uint64(0), 10)
	if err := BumpVault(v); err == nil {
		t.Fatal("wrapped mutation revision")
	}
	v["mutation-revision"] = "1"
	v["retirement-phase"] = `{"version":"1","keys":["K0"]}`
	if err := BumpVault(v); err == nil {
		t.Fatal("retirement phase did not block app mutation")
	}
	delete(v, "retirement-phase")
	delete(v, "key-version")
	if err := NextKeyVersion(v); err == nil {
		t.Fatal("missing migrated key version accepted")
	}
}

func TestRouteClaimsRequireExactOwnerAndName(t *testing.T) {
	p := PublicClaimPath("App.EXAMPLE.")
	for _, coord := range []string{p, MeshClaimPath("app-p"), UpgradePath, MigrationCompletePath, PlacementRevisionPath} {
		if !strings.HasPrefix(coord, "coordination/") {
			t.Fatalf("non-Kubernetes coordination file inside Flux's fabric folder: %s", coord)
		}
	}
	if p != PublicClaimPath("app.example") {
		t.Fatal("hostname normalization changed claim path")
	}
	initial := &Snapshot{files: map[string]snapFile{p: {}}}
	claim, err := ClaimRoute(initial, p, "app.example", "a/app")
	if err != nil {
		t.Fatal(err)
	}
	existing := &Snapshot{files: map[string]snapFile{p: {exists: true, content: claim.Content}}}
	if _, err := ClaimRoute(existing, p, "app.example", "b/app"); err == nil {
		t.Fatal("cross-project name collision accepted")
	}
	if _, err := ReleaseRoute(existing, p, "app.example", "b/app"); err == nil {
		t.Fatal("stale owner released someone else's claim")
	}
	if _, err := ClaimRoute(existing, p, "other.example", "a/app"); err == nil {
		t.Fatal("hash collision silently acquired")
	}
	if _, err := ReleaseRoute(existing, p, "app.example", "a/app"); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeReviewFailsClosedWithoutGate(t *testing.T) {
	g, _ := fakeForgejo(t, map[string]string{})
	r, err := UpgradeReview(context.Background(), g)
	if err != nil || r.Phase == "ready" || len(r.Blockers) == 0 {
		t.Fatalf("missing gate allowed: %+v %v", r, err)
	}
	if _, err := g.Edit(context.Background(), Author{}, "unsafe", []string{"fabric/apps/a/app.yaml"}, func(*Snapshot) ([]FileChange, error) {
		return []FileChange{{Path: "fabric/apps/a/app.yaml", Content: []byte("app")}}, nil
	}); err == nil || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("app write without migration accepted: %v", err)
	}
	p := PublicClaimPath("wiki.example")
	if _, err := g.Edit(context.Background(), Author{}, "unsafe claim", []string{p}, func(s *Snapshot) ([]FileChange, error) {
		c, err := ClaimRoute(s, p, "wiki.example", "p/wiki")
		if err != nil {
			return nil, err
		}
		return []FileChange{c}, nil
	}); err == nil || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("claim write without migration accepted: %v", err)
	}
}
