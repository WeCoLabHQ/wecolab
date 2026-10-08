package fabric

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// beginUpgradeAt uses Git's receive-pack ref comparison, rather than the
// Forgejo contents API's per-file SHA checks. Forgejo v15's public
// ChangeFilesOptions has no expected branch commit field: a file SHA is not a
// precondition on the inventory tree. A push of a child of the pinned commit
// with an explicit lease cannot succeed after main moves.
func (g *Git) beginUpgradeAt(ctx context.Context, revision string, gate []byte) error {
	u, err := url.Parse(g.URL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || !validPath(g.Repo) || strings.ContainsAny(g.Repo, "?#") {
		return fmt.Errorf("upgrade: invalid Forgejo Git remote")
	}
	remote := strings.TrimRight(g.URL, "/") + "/" + g.Repo + ".git"
	dir, err := os.MkdirTemp("", "fabric-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// No credentials in argv, URLs, git config files, or error output. Disable
	// ambient credential helpers, URL rewrites, prompts, and local hooks.
	header := "Authorization: token " + g.Token
	if g.User != "" {
		header = "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(g.User+":"+g.Token))
	}
	env := make([]string, 0, len(os.Environ())+11)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") && !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") {
			env = append(env, entry)
		}
	}
	env = append(env,
		"HOME="+dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "XDG_CONFIG_HOME="+dir,
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraheader", "GIT_CONFIG_VALUE_0="+header,
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/dev/null",
	)
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := run("init", "-q", "-b", "main"); err != nil {
		return err
	}
	if _, err := run("fetch", "-q", "--no-tags", remote, "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return err
	}
	fetched, err := run("rev-parse", "refs/remotes/origin/main")
	if err != nil {
		return err
	}
	if fetched != revision {
		return fmt.Errorf("upgrade: %w: main moved since inventory preflight", ErrConflict)
	}
	if _, err := run("checkout", "-q", "-b", "main", revision); err != nil {
		return err
	}
	coord := filepath.Join(dir, "coordination")
	if info, err := os.Lstat(coord); err == nil && !info.IsDir() {
		return fmt.Errorf("upgrade: coordination path is not a directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(coord, 0700); err != nil {
		return err
	}
	gatePath := filepath.Join(dir, filepath.FromSlash(UpgradePath))
	if info, err := os.Lstat(gatePath); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("upgrade: gate is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(gatePath, gate, 0600); err != nil {
		return err
	}
	migration := filepath.Join(dir, filepath.FromSlash(MigrationCompletePath))
	markerTracked := false
	if info, err := os.Lstat(migration); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("upgrade: migration marker is not a regular file")
		}
		markerTracked = true
		if err := os.Remove(migration); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := run("add", "--", UpgradePath); err != nil {
		return err
	}
	if markerTracked {
		if _, err := run("add", "-u", "--", MigrationCompletePath); err != nil {
			return err
		}
	}
	if _, err := run("-c", "user.name=WeCoLab", "-c", "user.email=fabric@wecolab", "commit", "-q", "-m", "begin schema migration"); err != nil {
		return err
	}
	// Explicit expected-old lease, plus ordinary fast-forward receive-pack
	// semantics: no API read/check can substitute for this atomic ref update.
	if _, err := run("push", "-q", "--force-with-lease=refs/heads/main:"+revision, remote, "HEAD:refs/heads/main"); err != nil {
		return fmt.Errorf("upgrade: %w: pinned main push refused", ErrConflict)
	}
	return nil
}
