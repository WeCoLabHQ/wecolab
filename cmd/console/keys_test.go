package main

import (
	"strings"
	"testing"

	"wecolab.io/wecolab/api/v1alpha1"
)

func TestKeysAreSingleUse(t *testing.T) {
	code, key := mintKey("wclp1", "community")
	parts := strings.Split(code, ".")
	if len(parts) != 3 || parts[0] != "wclp1" || parts[1] != "community" {
		t.Fatalf("code shape: %q", code)
	}
	left, ok := useKey(append([]v1alpha1.OfferKey{}, key), parts[2])
	if !ok || len(left) != 0 {
		t.Fatalf("the right secret redeems once and is removed: ok=%v left=%d", ok, len(left))
	}
	if _, ok := useKey(left, parts[2]); ok {
		t.Error("a used key must not redeem again")
	}
	if _, ok := useKey(append([]v1alpha1.OfferKey{}, key), "wrong"); ok {
		t.Error("a wrong secret must not redeem")
	}
}

func TestSetPath(t *testing.T) {
	o := map[string]any{"spec": map[string]any{"plugins": []any{map[string]any{"parameters": map[string]any{"serverName": "x"}}}}}
	setPath(o, "/spec/plugins/0/parameters/serverName", "db-pub")
	setPath(o, "/spec/replica/self", "pub")
	if o["spec"].(map[string]any)["plugins"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["serverName"] != "db-pub" ||
		o["spec"].(map[string]any)["replica"].(map[string]any)["self"] != "pub" {
		t.Fatalf("%v", o)
	}
}
