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

// HasCommit is whether this copy has a commit, on any branch.
func (g *Git) HasCommit(ctx context.Context, sha string) (bool, error) {
	code, err := g.do(ctx, http.MethodGet, "/git/commits/"+url.PathEscape(sha)+"?stat=false&verification=false&files=false", nil, nil)
	return err == nil && code != http.StatusNotFound, err
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

// Protect sets who may push to main: the mirroring account always, the Console's account only at the
// writer. Never by force, never deleted.
func (g *Git) Protect(ctx context.Context, pushers []string) error {
	var cur struct {
		Whitelist []string `json:"push_whitelist_usernames"`
	}
	code, err := g.do(ctx, http.MethodGet, "/branch_protections/main", nil, &cur)
	if err != nil {
		return err
	}
	body := map[string]any{"rule_name": "main", "enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": pushers}
	if code == http.StatusNotFound {
		_, err = g.do(ctx, http.MethodPost, "/branch_protections", body, nil)
		return err
	}
	a, b := slices.Clone(cur.Whitelist), slices.Clone(pushers)
	slices.Sort(a)
	slices.Sort(b)
	if slices.Equal(a, b) {
		return nil
	}
	_, err = g.do(ctx, http.MethodPatch, "/branch_protections/main", body, nil)
	return err
}

// PushMirror is one other copy the writer pushes to.
type PushMirror struct {
	Name      string `json:"remote_name"`
	Remote    string `json:"remote_address"`
	Filter    string `json:"branch_filter"`
	LastError string `json:"last_error"`
}

func (g *Git) PushMirrors(ctx context.Context) ([]PushMirror, error) {
	out := []PushMirror{}
	_, err := g.do(ctx, http.MethodGet, "/push_mirrors", nil, &out)
	return out, err
}

// AddPushMirror pushes branches matching filter to remote on every commit and every two minutes.
func (g *Git) AddPushMirror(ctx context.Context, remote, user, password, filter string) error {
	_, err := g.do(ctx, http.MethodPost, "/push_mirrors", map[string]any{"remote_address": remote, "remote_username": user, "remote_password": password,
		"branch_filter": filter, "sync_on_commit": true, "interval": "2m"}, nil)
	return err
}

func (g *Git) DeletePushMirror(ctx context.Context, name string) error {
	_, err := g.do(ctx, http.MethodDelete, "/push_mirrors/"+name, nil, nil)
	return err
}

// SyncPushMirrors pushes now.
func (g *Git) SyncPushMirrors(ctx context.Context) error {
	_, err := g.do(ctx, http.MethodPost, "/push_mirrors-sync", nil, nil)
	return err
}

// Branch creates a branch at main's head; one that exists already is left as it is.
func (g *Git) Branch(ctx context.Context, name string) error {
	code, err := g.do(ctx, http.MethodPost, "/branches", map[string]any{"new_branch_name": name, "old_ref_name": "main"}, nil)
	if code == http.StatusConflict {
		return nil
	}
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
