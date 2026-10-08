package fabric

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
)

// The Forgejo administration Warden does on its own site's copy: who may push to main, which other
// copies the writer pushes to, and starting over when an old writer's copy has gone its own way.

// Head is main's commit on this copy ("" while the repository is empty).
func (g *Git) Head(ctx context.Context) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	code, err := g.do(ctx, http.MethodGet, "/branches/main", nil, &b)
	if code == http.StatusNotFound {
		return "", nil
	}
	return b.Commit.ID, err
}

// RepositoryID identifies the repository independently of its reusable name.
// Zero means absent, never an API failure or an invalid identity.
func (g *Git) RepositoryID(ctx context.Context) (int64, error) {
	var repo struct {
		ID int64 `json:"id"`
	}
	code, err := g.do(ctx, http.MethodGet, "", nil, &repo)
	if err != nil || code == http.StatusNotFound {
		return 0, err
	}
	if repo.ID <= 0 {
		return 0, fmt.Errorf("Forgejo returned no repository identity")
	}
	return repo.ID, nil
}

// OnMain is whether a commit is one of main's newest n: a copy whose head it is only lags behind this
// one. Further back counts as not on main; starting over from this copy is then merely unnecessary.
func (g *Git) OnMain(ctx context.Context, sha string, n int) (bool, error) {
	const page = 50 // Forgejo's largest page by default
	for p := 1; (p-1)*page < n; p++ {
		var cs []struct {
			SHA string `json:"sha"`
		}
		code, err := g.do(ctx, http.MethodGet, fmt.Sprintf("/commits?sha=main&limit=%d&page=%d&stat=false&verification=false&files=false", page, p), nil, &cs)
		if err != nil || code == http.StatusNotFound {
			return false, err
		}
		for _, c := range cs {
			if c.SHA == sha {
				return true, nil
			}
		}
		if len(cs) < page {
			break
		}
	}
	return false, nil
}

// Protect sets who may push to main. Never by force, never deleted.
func (g *Git) Protect(ctx context.Context, pushers []string) error {
	return g.protect(ctx, "main", pushers)
}

const preservationRule = "superseded-**"

type branchProtection struct {
	Whitelist  []string `json:"push_whitelist_usernames"`
	Enabled    bool     `json:"enable_push"`
	Restricted bool     `json:"enable_push_whitelist"`
	Force      bool     `json:"enable_force_push"`
	DeployKeys bool     `json:"push_whitelist_deploy_keys"`
	Teams      []string `json:"push_whitelist_teams"`
}

func (p branchProtection) allowsOnly(pushers []string) bool {
	a, b := slices.Clone(p.Whitelist), slices.Clone(pushers)
	slices.Sort(a)
	slices.Sort(b)
	return p.Enabled && p.Restricted && !p.Force && !p.DeployKeys && len(p.Teams) == 0 && slices.Equal(a, b)
}

// PreservationProtected reports whether inbound mirrors are fenced. The caller
// must durably mark an unfinished cutover before changing this protection.
func (g *Git) PreservationProtected(ctx context.Context, owner string) (bool, error) {
	var cur branchProtection
	code, err := g.do(ctx, http.MethodGet, "/branch_protections/"+url.PathEscape(preservationRule), nil, &cur)
	return err == nil && code != http.StatusNotFound && cur.allowsOnly([]string{owner}), err
}

// ProtectPreserved admits only the local receiver's owner credential, not the
// mirror collaborator used by remote sites' legacy scheduled mirrors.
func (g *Git) ProtectPreserved(ctx context.Context, owner string) error {
	return g.protect(ctx, preservationRule, []string{owner})
}

func (g *Git) protect(ctx context.Context, rule string, pushers []string) error {
	var cur branchProtection
	path := "/branch_protections/" + url.PathEscape(rule)
	code, err := g.do(ctx, http.MethodGet, path, nil, &cur)
	if err != nil {
		return err
	}
	if code != http.StatusNotFound && cur.allowsOnly(pushers) {
		return nil
	}
	body := map[string]any{"rule_name": rule, "enable_push": true, "enable_push_whitelist": true,
		"push_whitelist_usernames": pushers, "push_whitelist_teams": []string{},
		"push_whitelist_deploy_keys": false, "enable_force_push": false}
	if code == http.StatusNotFound {
		_, err = g.do(ctx, http.MethodPost, "/branch_protections", body, nil)
	} else {
		_, err = g.do(ctx, http.MethodPatch, path, body, nil)
	}
	return err
}

// PushMirror is legacy scheduled state, read only to quiesce it before exact-ref replication.
type PushMirror struct {
	Name string `json:"remote_name"`
}

func (g *Git) PushMirrors(ctx context.Context) ([]PushMirror, error) {
	out := []PushMirror{}
	_, err := g.do(ctx, http.MethodGet, "/push_mirrors", nil, &out)
	return out, err
}

func (g *Git) DeletePushMirror(ctx context.Context, name string) error {
	_, err := g.do(ctx, http.MethodDelete, "/push_mirrors/"+name, nil, nil)
	return err
}

// Ensure makes whatever is missing of this copy: the repository, and the mirroring account as its
// collaborator. It never deletes, so it finishes a Recreate that stopped halfway, or a Forgejo that
// was installed again, without touching a copy that is whole.
func (g *Git) Ensure(ctx context.Context, mirrorUser string) error {
	code, err := g.do(ctx, http.MethodGet, "", nil, nil)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		owner, name, _ := cutRepo(g.Repo)
		req := map[string]any{"name": name, "private": true, "default_branch": "main"}
		if _, err := g.api(ctx, http.MethodPost, "/user/repos", req, nil); err != nil {
			return fmt.Errorf("create %s/%s: %w", owner, name, err)
		}
	}
	if code, err = g.do(ctx, http.MethodGet, "/collaborators/"+url.PathEscape(mirrorUser), nil, nil); err != nil || code != http.StatusNotFound {
		return err
	}
	_, err = g.do(ctx, http.MethodPut, "/collaborators/"+url.PathEscape(mirrorUser), map[string]any{"permission": "write"}, nil)
	return err
}

// Recreate deletes this copy and makes an empty one that only the mirroring account may push to, for
// the writer to push the whole history into.
func (g *Git) Recreate(ctx context.Context, mirrorUser string) error {
	if _, err := g.do(ctx, http.MethodDelete, "", nil, nil); err != nil {
		return err
	}
	if err := g.Ensure(ctx, mirrorUser); err != nil {
		return err
	}
	return g.Protect(ctx, []string{mirrorUser})
}

// api calls a path outside the repository. It never touches g.Repo: Writer.Run and the status port's
// handler share one Git.
func (g *Git) api(ctx context.Context, method, path string, body, out any) (int, error) {
	return g.doAt(ctx, method, "/api/v1"+path, body, out)
}

func cutRepo(r string) (string, string, bool) {
	for i := range r {
		if r[i] == '/' {
			return r[:i], r[i+1:], true
		}
	}
	return "", r, false
}
