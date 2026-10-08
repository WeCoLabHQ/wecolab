// Package fabric is the Fabric in Git: commits through a Forgejo, where each object lives, how an
// app's folder is written, and SOPS encryption (docs/architecture.md, "The Fabric").
package fabric

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// Git writes to a site's copy of the Fabric: one commit per change, through its Forgejo API, with the
// person who asked for it as the author. Changes are made with Edit, against what Git holds, and only
// if it has not changed since (docs/plans/2026-09-29-hardening.md, R2); branch protection refuses
// commits anywhere but at the writer.
type Git struct {
	URL   string // the site's Forgejo, e.g. http://forgejo.wecolab-system.svc:3000
	User  string // with a password in Token: basic authentication (another site's mirroring account)
	Token string
	Repo  string // owner/name
	HTTP  *http.Client
}

// GitFromEnv is nil unless WECOLAB_GIT_URL and WECOLAB_GIT_TOKEN are set.
func GitFromEnv() *Git {
	u, t := os.Getenv("WECOLAB_GIT_URL"), os.Getenv("WECOLAB_GIT_TOKEN")
	if u == "" || t == "" {
		return nil
	}
	return &Git{URL: strings.TrimRight(u, "/"), Token: t, Repo: "fabric/fabric", HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// FileChange is one file of a commit; nil Content deletes it.
type FileChange struct {
	Path    string
	Content []byte
}

// Author is who a commit is made for.
type Author struct{ Name, Email string }

func (g *Git) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	return g.doAt(ctx, method, "/api/v1/repos/"+g.Repo+path, body, out)
}

func (g *Git) doAt(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, g.URL+path, rd)
	if g.User != "" {
		req.SetBasicAuth(g.User, g.Token)
	} else {
		req.Header.Set("Authorization", "token "+g.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := g.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		if res.StatusCode == http.StatusNotFound {
			return res.StatusCode, nil
		}
		return res.StatusCode, fmt.Errorf("git %s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return res.StatusCode, json.Unmarshal(b, out)
	}
	return res.StatusCode, nil
}

type gitEntry struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	Content string `json:"content"`
}

func contentsPath(p, ref string) string {
	if !validPath(p) {
		p = "invalid-path" // never reaches another folder; callers check validPath first
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/contents/" + strings.Join(parts, "/") + "?ref=" + url.QueryEscape(ref)
}

// validPath is a path inside the repository: no empty, "." or ".." segment, no leading slash.
func validPath(p string) bool {
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Read is a file's content on main, and whether it exists.
func (g *Git) Read(ctx context.Context, p string) ([]byte, bool, error) {
	if !validPath(p) {
		return nil, false, fmt.Errorf("bad path %q", p)
	}
	var e gitEntry
	code, err := g.do(ctx, http.MethodGet, contentsPath(p, "main"), nil, &e)
	if err != nil || code == http.StatusNotFound {
		return nil, false, err
	}
	b, err := base64.StdEncoding.DecodeString(e.Content)
	return b, true, err
}

// List is the paths of the files in a folder on main (none when it does not exist).
func (g *Git) List(ctx context.Context, dir string) ([]string, error) {
	var es []gitEntry
	code, err := g.do(ctx, http.MethodGet, contentsPath(dir, "main"), nil, &es)
	if err != nil || code == http.StatusNotFound {
		return nil, err
	}
	out := []string{}
	for _, e := range es {
		if e.Type == "file" {
			out = append(out, e.Path)
		}
	}
	return out, nil
}

// ReadAt reads a file from one immutable commit, not the moving main branch.
func (g *Git) ReadAt(ctx context.Context, p, revision string) ([]byte, bool, error) {
	if !validPath(p) || !validRevision(revision) {
		return nil, false, fmt.Errorf("invalid Git path or revision")
	}
	var e gitEntry
	code, err := g.do(ctx, http.MethodGet, contentsPath(p, revision), nil, &e)
	if err != nil || code == http.StatusNotFound {
		return nil, false, err
	}
	b, err := base64.StdEncoding.DecodeString(e.Content)
	return b, true, err
}

// ListAt lists files in a directory at an immutable commit.
func (g *Git) ListAt(ctx context.Context, dir, revision string) ([]string, error) {
	if !validPath(dir) || !validRevision(revision) {
		return nil, fmt.Errorf("invalid Git path or revision")
	}
	var es []gitEntry
	code, err := g.do(ctx, http.MethodGet, contentsPath(dir, revision), nil, &es)
	if err != nil || code == http.StatusNotFound {
		return nil, err
	}
	out := []string{}
	for _, e := range es {
		if e.Type == "file" {
			out = append(out, e.Path)
		}
	}
	return out, nil
}

func validRevision(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, c := range sha {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

// Snapshot is files of the Fabric as Edit read them, each with the hash it had then.
type Snapshot struct{ files map[string]snapFile }

type snapFile struct {
	content []byte
	sha     string
	exists  bool
}

// Get is a file's content as read, and whether it existed.
func (s *Snapshot) Get(p string) ([]byte, bool) {
	f := s.files[p]
	return f.content, f.exists
}

// SHA is the exact Git blob identity observed by this snapshot.
func (s *Snapshot) SHA(p string) string { return s.files[p].sha }

// ErrConflict is an Edit that found the Fabric changed under it on every attempt.
var ErrConflict = errors.New("the Fabric kept changing meanwhile; try again")

func (g *Git) snapshot(ctx context.Context, paths []string) (*Snapshot, error) {
	s := &Snapshot{files: map[string]snapFile{}}
	for _, p := range paths {
		if !validPath(p) {
			return nil, fmt.Errorf("bad path %q", p)
		}
		var e gitEntry
		code, err := g.do(ctx, http.MethodGet, contentsPath(p, "main"), nil, &e)
		if err != nil {
			return nil, err
		}
		f := snapFile{}
		if code != http.StatusNotFound {
			b, err := base64.StdEncoding.DecodeString(e.Content)
			if err != nil {
				return nil, err
			}
			f = snapFile{content: b, sha: e.SHA, exists: true}
		}
		s.files[p] = f
	}
	return s, nil
}

type requestKey struct{}

// WithRequest ties the commits made with ctx to the request that asked for them: each gets a
// "Request-Id:" trailer, the id the Console logs that request under and shows with its errors.
func WithRequest(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestKey{}, id)
}

// Edit reads paths from the Fabric and commits only the changed paths against
// their observed SHAs. Schema-sensitive transactions also change the shared
// placement revision so concurrent authority changes force a retry.
// A migration must use EditMigration while the maintenance gate is active.
func (g *Git) Edit(ctx context.Context, who Author, msg string, paths []string, fn func(*Snapshot) ([]FileChange, error)) (string, error) {
	return g.edit(ctx, who, msg, paths, fn, false)
}

func (g *Git) edit(ctx context.Context, who Author, msg string, paths []string, fn func(*Snapshot) ([]FileChange, error), migration bool) (string, error) {
	if id, _ := ctx.Value(requestKey{}).(string); id != "" {
		msg += "\n\nRequest-Id: " + id
	}
	if !migration && slices.ContainsFunc(paths, protectedPath) {
		paths = append(slices.Clone(paths), UpgradePath, PlacementRevisionPath, MigrationCompletePath)
	}
	for attempt := range 5 {
		snap, err := g.snapshot(ctx, paths)
		if err != nil {
			return "", err
		}
		changes, err := fn(snap)
		if err == nil && !migration && len(changes) > 0 {
			protect, placement := false, false
			for _, c := range changes {
				protect = protect || protectedPath(c.Path)
				placement = placement || placementPath(c.Path)
			}
			if protect {
				err = upgradeAuthorized(snap)
			}
			if err == nil && placement {
				var rev FileChange
				rev, err = incrementPlacement(snap)
				if err == nil {
					changes = append(changes, rev)
				}
			}
		}
		if err != nil {
			return "", err
		}
		files := []map[string]any{}
		for _, c := range changes {
			f, read := snap.files[c.Path]
			if !read {
				return "", fmt.Errorf("edit %q: %s was not read first", msg, c.Path)
			}
			switch {
			case c.Content == nil && !f.exists:
			case c.Content == nil:
				files = append(files, map[string]any{"operation": "delete", "path": c.Path, "sha": f.sha})
			case f.exists && bytes.Equal(f.content, c.Content):
			case f.exists:
				files = append(files, map[string]any{"operation": "update", "path": c.Path, "sha": f.sha, "content": base64.StdEncoding.EncodeToString(c.Content)})
			default:
				files = append(files, map[string]any{"operation": "create", "path": c.Path, "content": base64.StdEncoding.EncodeToString(c.Content)})
			}
		}
		if len(files) == 0 {
			return "", nil
		}
		sha, code, err := g.post(ctx, who, msg, files)
		if err == nil {
			return sha, nil
		}
		if !conflict(code, err) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	return "", ErrConflict
}

// EditMigration is restricted to explicit staged migration commands. It cannot
// silently override the maintenance gate in ordinary API/controller writes.
func (g *Git) EditMigration(ctx context.Context, who Author, msg string, paths []string, fn func(*Snapshot) ([]FileChange, error)) (string, error) {
	paths = append(slices.Clone(paths), UpgradePath)
	return g.edit(ctx, who, msg, paths, func(s *Snapshot) ([]FileChange, error) {
		b, ok := s.Get(UpgradePath)
		if !ok {
			return nil, fmt.Errorf("upgrade maintenance gate missing")
		}
		var r UpgradeReport
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		if r.Phase != "maintenance" {
			return nil, fmt.Errorf("migration requires maintenance")
		}
		return fn(s)
	}, true)
}

// conflict is Forgejo saying the Fabric changed under a commit: a file's sha (409), a file that now
// exists (422), or the branch moved between its clone and its push (500 with a rejected stale ref).
func conflict(code int, err error) bool {
	return code == http.StatusConflict || code == http.StatusUnprocessableEntity ||
		(code == http.StatusInternalServerError && (strings.Contains(err.Error(), "non-fast-forward") ||
			strings.Contains(err.Error(), "out of date") ||
			strings.Contains(err.Error(), "PushRejected Error:") && strings.Contains(err.Error(), "(incorrect old value provided)")))
}

func (g *Git) post(ctx context.Context, who Author, msg string, files []map[string]any) (string, int, error) {
	if who.Email == "" {
		who = Author{Name: "WeCoLab", Email: "fabric@wecolab"}
	}
	if who.Name == "" {
		who.Name = who.Email
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	code, err := g.do(ctx, http.MethodPost, "/contents", map[string]any{
		"branch": "main", "message": msg, "files": files,
		"author":    map[string]string{"name": who.Name, "email": who.Email},
		"committer": map[string]string{"name": "WeCoLab", "email": "fabric@wecolab"},
	}, &out)
	return out.Commit.SHA, code, err
}

// Commit writes changes that are not based on anything read (new files, whole files the caller owns):
// Edit with the changed paths as its reads. A change that depends on what a file holds uses Edit.
func (g *Git) Commit(ctx context.Context, who Author, msg string, changes []FileChange) (string, error) {
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	return g.Edit(ctx, who, msg, paths, func(*Snapshot) ([]FileChange, error) { return changes, nil })
}
