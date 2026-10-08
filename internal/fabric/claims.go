package fabric

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type RouteClaim struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

func PublicClaimPath(host string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSuffix(host, "."))))
	return "coordination/route-claims/public/" + hex.EncodeToString(digest[:]) + ".yaml"
}
func MeshClaimPath(label string) string { return "coordination/route-claims/mesh/" + label + ".yaml" }

// ClaimRoute acquires a shared path. A SHA update/create on this same path is
// required by both contenders, including contenders with independent App files.
func ClaimRoute(s *Snapshot, p, name, owner string) (FileChange, error) {
	if b, ok := s.Get(p); ok {
		var prior RouteClaim
		if err := json.Unmarshal(b, &prior); err != nil {
			return FileChange{}, fmt.Errorf("route claim %s: %w", p, err)
		}
		if prior.Name != name {
			return FileChange{}, fmt.Errorf("route claim hash collision for %s", p)
		}
		if prior.Owner != owner {
			return FileChange{}, fmt.Errorf("route %s already claimed by %s", name, prior.Owner)
		}
	}
	b, _ := json.Marshal(RouteClaim{Name: name, Owner: owner})
	return FileChange{Path: p, Content: b}, nil
}

// ReleaseRoute refuses to remove claims that no longer belong to this App.
func ReleaseRoute(s *Snapshot, p, name, owner string) (FileChange, error) {
	b, ok := s.Get(p)
	if !ok {
		return FileChange{}, fmt.Errorf("route claim %s absent; migration required", p)
	}
	var prior RouteClaim
	if err := json.Unmarshal(b, &prior); err != nil {
		return FileChange{}, err
	}
	if prior.Name != name || prior.Owner != owner {
		return FileChange{}, fmt.Errorf("route claim %s belongs to %s, not %s", name, prior.Owner, owner)
	}
	return FileChange{Path: p}, nil
}
