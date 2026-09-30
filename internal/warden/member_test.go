package warden

import (
	"slices"
	"testing"
	"time"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/netbird"
)

func TestMeshGroups(t *testing.T) {
	names := map[string]string{"g1": "users", "g2": "project-old", "g3": "project-vince"}
	got := MeshGroups([]string{"g1", "g2"}, names, []string{"g3"})
	if !slices.Equal(got, []string{"g1", "g3"}) {
		t.Errorf("non-project groups kept, project groups replaced; got %v", got)
	}
	if MeshRole("owner") != "admin" || MeshRole("member") != "user" {
		t.Error("role mapping")
	}
}

func TestRenderBindings(t *testing.T) {
	m := &v1alpha1.Member{}
	m.Name, m.UID = "alice", "u-1"
	m.Spec = v1alpha1.MemberSpec{Email: "Alice@Example.com", Role: "member", Projects: []string{"a", "b"}}
	objs := RenderBindings(m)
	if len(objs) != 3 || objs[0].GetKind() != "ClusterRoleBinding" || objs[1].GetNamespace() != "a" {
		t.Fatalf("member gets a reader binding and one per project: %d", len(objs))
	}
	for _, o := range objs {
		if r := o.GetOwnerReferences(); len(r) != 1 || r[0].Kind != "Member" || r[0].Name != "alice" || r[0].UID != "u-1" {
			t.Fatalf("every binding goes with its Member: %v", r)
		}
	}
	subj := objs[1].Object["subjects"].([]any)[0].(map[string]any)
	if subj["name"] != "oidc:alice@example.com" {
		t.Errorf("subject is the OIDC username, lowercased: %v", subj["name"])
	}
	m.Spec.Role = "admin"
	if o := RenderBindings(m); len(o) != 1 || o[0].Object["roleRef"].(map[string]any)["name"] != "cluster-admin" {
		t.Error("admins are cluster-admin")
	}
	m.Spec.Blocked = true
	if RenderBindings(m) != nil {
		t.Error("blocked members keep nothing")
	}
}

func TestTokenPlan(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	tok := func(id string, expires, created time.Duration) netbird.Token {
		return netbird.Token{ID: id, Expires: now.Add(expires), Created: now.Add(created)}
	}
	old, next := tok("old", 30*day, -335*day), tok("next", 365*day, -2*time.Hour)
	if mint, stale := TokenPlan([]netbird.Token{tok("a", 200*day, -165*day)}, "", now); mint || stale != nil {
		t.Fatal("a token with months left stays")
	}
	if mint, stale := TokenPlan([]netbird.Token{old}, "", now); !mint || stale != nil {
		t.Fatal("the next token is minted two months ahead, and nothing is deleted while the secret names none")
	}
	if mint, _ := TokenPlan([]netbird.Token{old, next}, "old", now); mint {
		t.Fatal("a token minted in the last day is waited for")
	}
	if _, stale := TokenPlan([]netbird.Token{old, next}, "next", now); stale != nil {
		t.Fatal("the old token stays for a day after the secret names the new one, until every steward's Flux has it")
	}
	next.Created = now.Add(-2 * day)
	if mint, stale := TokenPlan([]netbird.Token{old, next}, "old", now); !mint || stale != nil {
		t.Fatal("a token the secret never got is replaced, and the one in use is kept")
	}
	if mint, stale := TokenPlan([]netbird.Token{old, next}, "next", now); mint || !slices.Equal(stale, []string{"old"}) {
		t.Fatal("once the secret holds the new token the old one goes")
	}
}
