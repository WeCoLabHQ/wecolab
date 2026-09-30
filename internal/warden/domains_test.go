package warden

import "testing"

func TestDomainVerdict(t *testing.T) {
	doors := []string{"1.2.3.4"}
	if ok, _ := DomainVerdict(doors, []string{"1.2.3.4"}, []string{"wecolab=project:alice"}, "alice"); !ok {
		t.Error("both records right must verify")
	}
	if ok, why := DomainVerdict(doors, []string{"9.9.9.9"}, []string{"wecolab=project:alice"}, "alice"); ok || why == "" {
		t.Error("wildcard elsewhere must not verify")
	}
	if ok, _ := DomainVerdict(doors, []string{"1.2.3.4"}, []string{"wecolab=project:bob"}, "alice"); ok {
		t.Error("another project's ownership record must not verify")
	}
	if ok, _ := DomainVerdict(doors, nil, nil, "alice"); ok {
		t.Error("nothing resolving must not verify")
	}
}
