package warden

import (
	"testing"

	"wecolab.io/wecolab/api/v1alpha1"
)

func TestMoves(t *testing.T) {
	s := v1alpha1.AppSpec{Sites: []string{"a", "b", "c"}, Primary: "a", Database: "db"}
	p, err := PlannedMove(s, "b", "m1")
	if err != nil || p.Primary != "b" || p.Handover == nil || p.Handover.From != "a" || p.Handover.ID != "m1" {
		t.Fatalf("planned: %+v %v", p, err)
	}
	r, _ := PlannedMove(p, "c", "m2") // retarget keeps the handover
	if r.Primary != "c" || r.Handover.From != "a" || r.Handover.ID != "m1" {
		t.Fatalf("retarget: %+v", r)
	}
	for _, m := range []v1alpha1.AppSpec{p, r} {
		if NewDatabase(m) || m.Archive[m.Primary] != 1 {
			t.Fatalf("a move records its target, so Git shows the database was made: %+v", m)
		}
	}
	c, _ := PlannedMove(p, "a", "m3") // back to the origin before a token: cancelled
	if c.Primary != "a" || c.Handover != nil || NewDatabase(c) || s.Archive != nil {
		t.Fatalf("cancel: %+v", c)
	}
	if same, _ := PlannedMove(p, "b", "m6"); same.Handover.ID != "m1" || same.Primary != "b" {
		t.Fatalf("a move to the current target changes nothing: %+v", same)
	}
	p.Handover.Token = "tok"
	if r.Handover.Token != "" {
		t.Fatal("a move shares its handover with the spec it came from")
	}
	if _, err := PlannedMove(p, "a", "m4"); err == nil {
		t.Fatal("back to a demoted origin must be refused")
	}
	// Once the token is out the old target may have promoted with it: it is rebuilt from the new one.
	if _, err := PlannedMove(p, "c", "m7"); err == nil { // the target may have promoted with the token already
		t.Fatal("a retarget after the token must be refused: two sites could promote with it")
	}
	if n, _ := PlannedMove(v1alpha1.AppSpec{Sites: []string{"a", "b"}, Primary: "a"}, "b", "m8"); n.Primary != "b" || n.Handover != nil {
		t.Fatalf("an app without a database has nothing to hand over: %+v", n)
	}
	if _, err := PlannedMove(s, "z", "m5"); err == nil {
		t.Fatal("a target outside the app's sites must be refused")
	}
	f, _ := ForcedMove(v1alpha1.AppSpec{Sites: []string{"a", "b", "c"}, Primary: "a", Archive: map[string]int{"c": 3},
		Handover: &v1alpha1.Handover{ID: "x", From: "a"}}, "b")
	if f.Primary != "b" || f.Handover != nil || f.Archive["a"] != 2 || f.Archive["c"] != 4 || f.Archive["b"] != 1 {
		t.Fatalf("forced: %+v", f)
	}
}
