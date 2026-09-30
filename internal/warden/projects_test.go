package warden

import (
	"strings"
	"testing"
)

// Only the site's owner and holders of its best-effort capacity may run best-effort pods, the ones that
// tolerate laptops and idle time.
func TestBestEffortQuota(t *testing.T) {
	quota := func(st Standing) map[string]any {
		for _, o := range ProjectObjects("p", st, "10.77.0.0/16") {
			if o.GetKind() == "ResourceQuota" && o.GetName() == "best-effort" {
				return o.Object["spec"].(map[string]any)
			}
		}
		return nil
	}
	for _, st := range []Standing{{}, {Held: &OfferAt{CPU: "1"}}} {
		q := quota(st)
		if q == nil || q["hard"].(map[string]any)["pods"] != "0" || !strings.Contains(toJSON(q["scopeSelector"]), `"values":["`+BestEffortClass+`"]`) {
			t.Fatalf("%+v: %v", st, q)
		}
	}
	for _, st := range []Standing{{Owner: true}, {Held: &OfferAt{BestEffort: true}}} {
		if q := quota(st); q != nil {
			t.Fatalf("%+v: %v", st, q)
		}
	}
}

// Pods reach the Kubernetes API at their own site's managers only; with none known there is no rule, since
// a rule without "to" would admit every address.
func TestAPIEgress(t *testing.T) {
	egress := func(servers ...string) string {
		for _, o := range ProjectObjects("p", Standing{}, "10.77.0.0/16", servers...) {
			if o.GetName() == "allow-app-egress" {
				return toJSON(o.Object["spec"])
			}
		}
		return ""
	}
	if e := egress("10.77.1.1", "not-an-ip"); !strings.Contains(e, `{"ports":[{"port":6443,"protocol":"TCP"}],"to":[{"ipBlock":{"cidr":"10.77.1.1/32"}}]}`) {
		t.Fatal(e)
	}
	if e := egress(); strings.Contains(e, "6443") {
		t.Fatal(e)
	}
}
