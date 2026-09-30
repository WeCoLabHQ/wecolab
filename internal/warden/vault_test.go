package warden

import (
	"regexp"
	"testing"
)

func TestVaultName(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9-]{6,63}$`)
	for _, c := range [][2]string{{"fab.example.org", "team"}, {"a.b", "x"}, {"very-long-fabric-domain-name.example.org", "a-project-with-a-long-name-too"}} {
		n := VaultName(c[0], c[1])
		if !re.MatchString(n) || len(n) < 6 || len(n) > 63 || n[:3] == "b2-" {
			t.Errorf("bad bucket name %q", n)
		}
	}
}
