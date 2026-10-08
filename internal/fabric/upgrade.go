package fabric

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const UpgradePath = "coordination/upgrade-review.json"
const MigrationCompletePath = "coordination/migration-complete.json"
const PlacementRevisionPath = "coordination/placement-revision.json"

type UpgradeReport struct {
	Phase    string   `json:"phase"`
	Sites    []string `json:"sites"`
	Blockers []string `json:"blockers,omitempty"`
}

// UpgradeReview is the durable gate for transactions requiring the archive, claim and
// credential schema. An absent or unreadable gate never authorizes an old controller.
func UpgradeReview(ctx context.Context, g *Git) (UpgradeReport, error) {
	b, ok, err := g.Read(ctx, UpgradePath)
	if err != nil {
		return UpgradeReport{}, err
	}
	if !ok {
		return UpgradeReport{Blockers: []string{"upgrade gate not initialized"}}, nil
	}
	var r UpgradeReport
	if err = json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("upgrade gate: %w", err)
	}
	if r.Phase != "ready" {
		r.Blockers = append(r.Blockers, "upgrade maintenance is active")
	}
	if r.Phase == "ready" {
		if _, ok, err := g.Read(ctx, MigrationCompletePath); err != nil {
			return r, err
		} else if !ok {
			r.Blockers = append(r.Blockers, "legacy migration marker missing")
		}
		if _, ok, err := g.Read(ctx, PlacementRevisionPath); err != nil {
			return r, err
		} else if !ok {
			r.Blockers = append(r.Blockers, "placement revision missing")
		}
	}
	return r, nil
}

// BeginUpgrade starts maintenance only if main still equals the pinned, proven
// inventory revision. A moved branch is a conflict, never an automatic retry.
func BeginUpgrade(ctx context.Context, g *Git, sites []string, revision string) error {
	if !validRevision(revision) {
		return fmt.Errorf("upgrade: invalid pinned main revision")
	}
	sites = slices.Clone(sites)
	slices.Sort(sites)
	sites = slices.Compact(sites)
	if len(sites) == 0 {
		return fmt.Errorf("upgrade: site inventory is empty")
	}
	b, ok, err := g.ReadAt(ctx, UpgradePath, revision)
	if err != nil {
		return err
	}
	if ok {
		var old UpgradeReport
		if err := json.Unmarshal(b, &old); err != nil {
			return err
		}
		if old.Phase == "maintenance" {
			return fmt.Errorf("maintenance already active; begin requires a new preflight")
		}
		if old.Phase != "ready" {
			return fmt.Errorf("invalid upgrade phase %q", old.Phase)
		}
	}
	b, _ = json.Marshal(UpgradeReport{Phase: "maintenance", Sites: sites})
	return g.beginUpgradeAt(ctx, revision, b)
}

// CompleteUpgrade requires every site in the original inventory to report a
// schema-aware binary. The caller must finish and validate all migrations first.
func CompleteUpgrade(ctx context.Context, g *Git, siteVersions map[string]bool) error {
	_, err := g.Edit(ctx, Author{}, "complete schema migration", []string{UpgradePath, PlacementRevisionPath, MigrationCompletePath}, func(s *Snapshot) ([]FileChange, error) {
		b, ok := s.Get(UpgradePath)
		if !ok {
			return nil, fmt.Errorf("upgrade maintenance was not started")
		}
		if _, ok := s.Get(MigrationCompletePath); !ok {
			return nil, fmt.Errorf("legacy migration not complete")
		}
		var r UpgradeReport
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		if r.Phase == "ready" {
			return nil, nil
		}
		if r.Phase != "maintenance" {
			return nil, fmt.Errorf("invalid upgrade phase %q", r.Phase)
		}
		for _, site := range r.Sites {
			if !siteVersions[site] {
				return nil, fmt.Errorf("site %s has not reported a schema-aware version", site)
			}
		}
		if _, ok := s.Get(PlacementRevisionPath); !ok {
			return nil, fmt.Errorf("placement revision not migrated")
		}
		r.Phase = "ready"
		r.Blockers = nil
		b, _ = json.Marshal(r)
		return []FileChange{{Path: UpgradePath, Content: b}}, nil
	})
	return err
}

func protectedPath(p string) bool {
	return strings.HasPrefix(p, "fabric/apps/") || strings.HasPrefix(p, "fabric/sites/") || strings.HasPrefix(p, "fabric/offers/") || strings.HasPrefix(p, "fabric/pools/") || strings.HasPrefix(p, "coordination/route-claims/") || strings.HasPrefix(p, "secrets/vault-")
}
func placementPath(p string) bool {
	return strings.HasPrefix(p, "fabric/apps/") || strings.HasPrefix(p, "fabric/sites/") || strings.HasPrefix(p, "fabric/offers/") || strings.HasPrefix(p, "fabric/pools/")
}
func upgradeAuthorized(s *Snapshot) error {
	b, ok := s.Get(UpgradePath)
	if !ok {
		return fmt.Errorf("schema migration required: upgrade gate missing")
	}
	var r UpgradeReport
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	if r.Phase != "ready" {
		return fmt.Errorf("schema migration in maintenance: deployment and placement mutations paused")
	}
	if _, ok := s.Get(MigrationCompletePath); !ok {
		return fmt.Errorf("legacy migration marker missing")
	}
	return nil
}
func incrementPlacement(s *Snapshot) (FileChange, error) {
	b, ok := s.Get(PlacementRevisionPath)
	if !ok {
		return FileChange{}, fmt.Errorf("placement revision migration required")
	}
	var r struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return FileChange{}, err
	}
	n, err := strconv.ParseUint(r.Revision, 10, 64)
	if err != nil || n == ^uint64(0) {
		return FileChange{}, fmt.Errorf("invalid or exhausted placement revision")
	}
	r.Revision = strconv.FormatUint(n+1, 10)
	b, _ = json.Marshal(r)
	return FileChange{Path: PlacementRevisionPath, Content: b}, nil
}
