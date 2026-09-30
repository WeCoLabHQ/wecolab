// Package validate is what a name in the fabric may be. Names become Kubernetes objects, Git paths,
// certificate names, Traefik rules, zone records and page text, so they are checked once, here, against
// rules that leave no room for syntax, and the same rules are the CRDs' validation
// (docs/plans/2026-09-29-hardening.md, R3).
package validate

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	email = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
)

// LabelPattern and DNSNamePattern are the CRDs' patterns for the same rules.
const (
	LabelPattern   = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	DNSNamePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
)

// Label is a DNS label (RFC 1123): lowercase letters, digits and inner dashes, at most max characters
// (63 when max is 0). Sites, projects, apps, boxes and hosts are labels.
func Label(s string, max int) error {
	if max == 0 {
		max = 63
	}
	if len(s) == 0 || len(s) > max || !label.MatchString(s) {
		return fmt.Errorf("%q is not a name: lowercase letters, digits and inner dashes, at most %d characters", s, max)
	}
	return nil
}

// DNSName is a host name: labels joined by dots, at most 253 characters, no wildcard, lowercase.
func DNSName(s string) error {
	if len(s) == 0 || len(s) > 253 {
		return fmt.Errorf("%q is not a host name", s)
	}
	for _, l := range strings.Split(s, ".") {
		if Label(l, 63) != nil {
			return fmt.Errorf("%q is not a host name: lowercase labels of letters, digits and dashes, joined by dots", s)
		}
	}
	return nil
}

// Email is a plain address, enough to invite someone and to name them in RBAC.
func Email(s string) error {
	if len(s) > 254 || !email.MatchString(s) {
		return fmt.Errorf("%q is not an email address", s)
	}
	return nil
}

// reserved are names the fabric uses itself: the recovery key's file, system namespaces, and the Door's
// own host names. No site, project or app may take them.
var reserved = []string{
	"recovery", "default", "kube-system", "kube-public", "kube-node-lease", "flux-system", "wecolab-system",
	"cert-manager", "cnpg-system", "console", "door", "mesh", "ns1", "ns2", "www", "people",
}

// Reserved reports whether a name belongs to the fabric itself.
func Reserved(name string) bool { return slices.Contains(reserved, name) }

// Name is a label that is not reserved: what the Console accepts for new sites, projects and apps.
func Name(s string) error {
	if err := Label(s, 32); err != nil {
		return err
	}
	if Reserved(s) {
		return fmt.Errorf("%q is reserved by the fabric", s)
	}
	return nil
}
