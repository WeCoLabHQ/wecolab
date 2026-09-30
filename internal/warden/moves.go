package warden

import (
	"fmt"
	"slices"

	"wecolab.io/wecolab/api/v1alpha1"
)

// How a person moves an app's primary, as changes to its spec in Git (docs/plans/2026-09-29-hardening.md,
// R1). Every site derives its database's role from the spec alone; nothing here asks the sites.

// PlannedMove moves the primary to `to` with a handover: the current primary demotes and hands its token
// over. A move while a handover is in flight changes only the target, since the token is not bound to one,
// but only until the token is in Git: then the target may already have promoted with it, and another site
// promoting with the same token would fork the history, so the move is refused until it completes (a
// person can still force). A move back to the handover's origin before any token was minted cancels it.
// An app without a database has nothing to hand over.
// The Console asks MoveGates first.
func PlannedMove(s v1alpha1.AppSpec, to, id string) (v1alpha1.AppSpec, error) {
	if !slices.Contains(s.Sites, to) {
		return s, fmt.Errorf("%s is not one of the app's sites", to)
	}
	if h := s.Handover; h != nil { // never shared with the spec it came from
		h2 := *h
		s.Handover = &h2
	}
	switch h := s.Handover; {
	case to == s.Primary:
		return s, nil
	case s.Database == "":
		s.Primary = to
		return s, nil
	case h == nil:
		s.Handover = &v1alpha1.Handover{ID: id, From: s.Primary}
		s.Archive = nextGeneration(s.Archive, to)
	case to == h.From && h.Token == "":
		s.Handover = nil
		s.Archive = nextGeneration(s.Archive, to)
	case to == h.From:
		return s, fmt.Errorf("%s already demoted with its token; move to a standby, or force", h.From)
	case h.Token != "":
		// The target may already have promoted with the token and taken writes; another site promoting
		// with the same token would fork the history under a planned move. Let this one finish first.
		return s, fmt.Errorf("the move to %s is completing with %s's token; move again once it is done, or force", s.Primary, h.From)
	default:
		s.Archive = nextGeneration(s.Archive, to)
	}
	s.Primary = to
	return s, nil
}

// ForcedMove makes `to` primary at once, without a token, for when the primary is gone. Promotion without
// a token forks the database's history, so every other site's database is rebuilt from the vault under a
// new archive generation (CloudNativePG re-clones a former primary, and refuses to archive into a folder
// that already holds one).
func ForcedMove(s v1alpha1.AppSpec, to string) (v1alpha1.AppSpec, error) {
	if !slices.Contains(s.Sites, to) {
		return s, fmt.Errorf("%s is not one of the app's sites", to)
	}
	others := []string{}
	for _, site := range s.Sites {
		if site != to {
			others = append(others, site)
		}
	}
	s.Primary, s.Handover, s.Archive = to, nil, nextGeneration(s.Archive, to, others...)
	return s, nil
}

// nextGeneration is a copy of archive with to's generation recorded and each of rebuilt's raised. A
// recorded generation is how Git shows the app's database was made, so no site makes it again by initdb
// (NewDatabase).
func nextGeneration(archive map[string]int, to string, rebuilt ...string) map[string]int {
	out := map[string]int{}
	for k, v := range archive {
		out[k] = v
	}
	out[to] = max(out[to], 1)
	for _, site := range rebuilt {
		out[site] = max(out[site], 1) + 1
	}
	return out
}
