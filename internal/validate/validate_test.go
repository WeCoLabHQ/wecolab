package validate

import (
	"regexp"
	"testing"
)

func TestNames(t *testing.T) {
	for _, bad := range []string{"", "A", "a_b", "-a", "a-", "a.b", "a b", "a'b", "a`b", "a\nb", "<img>", "recovery", "console", "door", "abcdefghijabcdefghijabcdefghijabc"} {
		if Name(bad) == nil {
			t.Errorf("Name(%q) accepted", bad)
		}
	}
	for _, good := range []string{"a", "vince", "home-lab", "x1", "abcdefghijabcdefghijabcdefghijab"} {
		if err := Name(good); err != nil {
			t.Errorf("Name(%q): %v", good, err)
		}
	}
}

func TestDNSName(t *testing.T) {
	for _, bad := range []string{"", "*.example.org", "a`) || Host(`console.x", "a.example.org\n", "A.example.org", "a..b", ".a", "a.", "-a.b", "a_b.c"} {
		if DNSName(bad) == nil {
			t.Errorf("DNSName(%q) accepted", bad)
		}
	}
	for _, good := range []string{"a", "docs.fab.example.org", "x-1.y.z"} {
		if err := DNSName(good); err != nil {
			t.Errorf("DNSName(%q): %v", good, err)
		}
	}
}

func TestEmail(t *testing.T) {
	if Email("owner@example.org") != nil || Email("o'brien@example.org") == nil || Email("a@b") == nil {
		t.Fatal("email rules")
	}
}

// The CRD patterns are the same rules as the functions.
func TestPatternsMatchFunctions(t *testing.T) {
	l, d := regexp.MustCompile(LabelPattern), regexp.MustCompile(DNSNamePattern)
	for _, s := range []string{"a", "a-b", "-a", "a_b", "a.b", "*.a", "a..b", "docs.example.org", "A"} {
		if l.MatchString(s) != (Label(s, 63) == nil) {
			t.Errorf("label pattern and Label disagree on %q", s)
		}
		if d.MatchString(s) != (DNSName(s) == nil) {
			t.Errorf("DNS name pattern and DNSName disagree on %q", s)
		}
	}
}
