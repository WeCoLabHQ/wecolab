package fabric

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type refRoundTrip func(*http.Request) (*http.Response, error)

func (f refRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func routeRefRepo(t *testing.T, r *refRepo, logicalURL string) {
	t.Helper()
	actual, err := url.Parse(r.git.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.git.URL = logicalURL
	r.git.HTTP = &http.Client{Transport: refRoundTrip(func(req *http.Request) (*http.Response, error) {
		copyReq := req.Clone(req.Context())
		copyURL := *copyReq.URL
		copyURL.Scheme, copyURL.Host = actual.Scheme, actual.Host
		copyReq.URL = &copyURL
		copyReq.Host = actual.Host
		response, err := http.DefaultTransport.RoundTrip(copyReq)
		if response != nil {
			response.Request = req
		}
		return response, err
	})}
}

func TestRefTransportRoutesLogicalGitURLsThroughClientTransport(t *testing.T) {
	src, dst := newRefRepo(t), newRefRepo(t)
	tip := src.commit(t, "", "routed-object")
	src.set(t, "refs/heads/main", tip)
	routeRefRepo(t, src, "http://logical-source.test:30300")
	routeRefRepo(t, dst, "http://logical-destination.test:30300")
	ctx := context.Background()
	refs, err := src.git.Refs(ctx)
	if err != nil || refs["refs/heads/main"] != tip {
		t.Fatalf("routed refs: %v %v", refs, err)
	}
	snap, err := src.git.FetchRefs(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.PushMain(ctx, dst.git); err != nil {
		t.Fatal(err)
	}
	preserve := "refs/heads/superseded-source-" + tip
	if err := snap.PushPreserved(ctx, dst.git, map[string]string{preserve: "refs/heads/main"}); err != nil {
		t.Fatal(err)
	}
	if dst.ref(t, "refs/heads/main") != tip || dst.ref(t, preserve) != tip {
		t.Fatal("routed pushes did not reach destination")
	}
}

// refRepo serves a real bare Git repository through git-http-backend. The
// server authenticates every HTTP request, not just its first advertisement.
type refRepo struct {
	git   *Git
	dir   string
	token string
}

func newRefRepo(t *testing.T) *refRepo {
	t.Helper()
	dir := t.TempDir()
	r := &refRepo{dir: filepath.Join(dir, "fabric.git"), token: "test-secret-token"}
	gitCommand(t, "", "init", "--bare", r.dir)
	gitCommand(t, r.dir, "config", "http.receivepack", "true")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if u, p, ok := req.BasicAuth(); !ok || u != "mirror" || p != r.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "request body", http.StatusBadRequest)
			return
		}
		cmd := exec.CommandContext(req.Context(), "git", "http-backend")
		cmd.Env = append(os.Environ(), "GIT_PROJECT_ROOT="+dir, "GIT_HTTP_EXPORT_ALL=1", "REQUEST_METHOD="+req.Method, "PATH_INFO="+req.URL.Path, "QUERY_STRING="+req.URL.RawQuery, "CONTENT_TYPE="+req.Header.Get("Content-Type"))
		cmd.Stdin = bytes.NewReader(body)
		out, err := cmd.Output()
		if err != nil {
			http.Error(w, "backend failed", http.StatusInternalServerError)
			return
		}
		sep := bytes.Index(out, []byte("\r\n\r\n"))
		if sep < 0 {
			http.Error(w, "invalid CGI response", http.StatusInternalServerError)
			return
		}
		status := http.StatusOK
		for _, line := range strings.Split(string(out[:sep]), "\r\n") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}
			if strings.EqualFold(parts[0], "Status") {
				if strings.HasPrefix(strings.TrimSpace(parts[1]), "404") {
					status = http.StatusNotFound
				}
				continue
			}
			w.Header().Add(parts[0], strings.TrimSpace(parts[1]))
		}
		w.WriteHeader(status)
		_, _ = w.Write(out[sep+4:])
	}))
	t.Cleanup(server.Close)
	r.git = &Git{URL: server.URL, Repo: "fabric", User: "mirror", Token: r.token, HTTP: server.Client()}
	return r
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fabric", "GIT_AUTHOR_EMAIL=fabric@example.test", "GIT_COMMITTER_NAME=Fabric", "GIT_COMMITTER_EMAIL=fabric@example.test")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func (r *refRepo) commit(t *testing.T, parent, content string) string {
	t.Helper()
	blob := exec.Command("git", "--git-dir="+r.dir, "hash-object", "-w", "--stdin")
	blob.Stdin = strings.NewReader(content)
	out, err := blob.Output()
	if err != nil {
		t.Fatal(err)
	}
	// A commit's content lives in its tree; independent payloads force distinct objects.
	input := "100644 blob " + strings.TrimSpace(string(out)) + "\tfile\n"
	cmd := exec.Command("git", "--git-dir="+r.dir, "mktree")
	cmd.Stdin = strings.NewReader(input)
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"commit-tree", strings.TrimSpace(string(b)), "-m", "fixture"}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return gitCommand(t, r.dir, args...)
}
func (r *refRepo) set(t *testing.T, name, sha string) {
	t.Helper()
	gitCommand(t, r.dir, "update-ref", name, sha)
}
func (r *refRepo) ref(t *testing.T, name string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+r.dir, "rev-parse", "--verify", name)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestRefsEmptyRepository(t *testing.T) {
	r := newRefRepo(t)
	refs, err := r.git.Refs(context.Background())
	if err != nil || len(refs) != 0 {
		t.Fatalf("empty refs: %v %v", refs, err)
	}
}

func TestRefSnapshotPreservesOnlyExactRefsAndObjects(t *testing.T) {
	ctx := context.Background()
	src, dst := newRefRepo(t), newRefRepo(t)
	root := src.commit(t, "", "root")
	main := src.commit(t, root, "main")
	sibling := src.commit(t, root, "orphaned-branch")
	src.set(t, "refs/heads/main", main)
	src.set(t, "refs/heads/superseded-old-"+sibling, sibling)
	src.set(t, "refs/tags/v1", root)
	unrelated := dst.commit(t, "", "unrelated")
	dst.set(t, "refs/heads/unrelated", unrelated)
	refs, err := src.git.Refs(ctx)
	if err != nil || refs["refs/heads/main"] != main || refs["refs/tags/v1"] != root || refs["refs/heads/superseded-old-"+sibling] != sibling || len(refs) != 3 {
		t.Fatalf("refs: %v %v", refs, err)
	}
	snap, err := src.git.FetchRefs(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]string{"refs/heads/superseded-site-" + main: "refs/heads/main", "refs/heads/superseded-old-" + sibling: "refs/heads/superseded-old-" + sibling}
	if err := snap.PushPreserved(ctx, dst.git, targets); err != nil {
		t.Fatal(err)
	}
	if err := snap.PushPreserved(ctx, dst.git, targets); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if dst.ref(t, "refs/heads/superseded-site-"+main) != main || dst.ref(t, "refs/heads/superseded-old-"+sibling) != sibling || dst.ref(t, "refs/heads/unrelated") != unrelated {
		t.Fatal("incorrect preservation refs")
	}
	if got := gitCommand(t, dst.dir, "show", sibling+":file"); got != "orphaned-branch" {
		t.Fatalf("unreachable blob missing: %s", got)
	}
	if got := dst.ref(t, "refs/heads/main"); got != "" {
		t.Fatalf("preservation touched main: %s", got)
	}
}

func TestPreservedRejectsAncestorCollision(t *testing.T) {
	ctx := context.Background()
	src, dst := newRefRepo(t), newRefRepo(t)
	root := src.commit(t, "", "root")
	tip := src.commit(t, root, "child")
	src.set(t, "refs/heads/main", tip)
	// Create shared ancestor in the destination, then collide on the exact
	// preservation name. Ordinary non-force push would incorrectly fast-forward.
	gitCommand(t, dst.dir, "fetch", src.dir, "refs/heads/main:refs/heads/temporary")
	name := "refs/heads/superseded-site-" + tip
	dst.set(t, name, root)
	snap, err := src.git.FetchRefs(ctx, map[string]string{"refs/heads/main": tip})
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.PushPreserved(ctx, dst.git, map[string]string{name: "refs/heads/main"}); err == nil {
		t.Fatal("ancestor collision accepted")
	}
	if got := dst.ref(t, name); got != root {
		t.Fatalf("collision overwritten: %s", got)
	}
}

func TestMainPushFastForwardAndRefusal(t *testing.T) {
	ctx := context.Background()
	src, dst := newRefRepo(t), newRefRepo(t)
	root := src.commit(t, "", "root")
	src.set(t, "refs/heads/main", root)
	snap, err := src.git.FetchRefs(ctx, map[string]string{"refs/heads/main": root})
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.PushMain(ctx, dst.git); err != nil {
		t.Fatal(err)
	}
	if err := snap.PushMain(ctx, dst.git); err != nil {
		t.Fatalf("idempotence: %v", err)
	}
	next := src.commit(t, root, "next")
	src.set(t, "refs/heads/main", next)
	advance, err := src.git.FetchRefs(ctx, map[string]string{"refs/heads/main": next})
	if err != nil {
		t.Fatal(err)
	}
	if err := advance.PushMain(ctx, dst.git); err != nil {
		t.Fatalf("fast forward: %v", err)
	}
	if err := snap.PushMain(ctx, dst.git); err == nil {
		t.Fatal("non-fast-forward main accepted")
	}
	if got := dst.ref(t, "refs/heads/main"); got != next {
		t.Fatalf("main changed: %s", got)
	}
}

func TestFetchRefsRejectsChangedOrMissingSource(t *testing.T) {
	ctx := context.Background()
	src := newRefRepo(t)
	root := src.commit(t, "", "root")
	newHead := src.commit(t, root, "new")
	src.set(t, "refs/heads/main", newHead)
	if _, err := src.git.FetchRefs(ctx, map[string]string{"refs/heads/main": root}); err == nil {
		t.Fatal("moved source accepted")
	}
	if _, err := src.git.FetchRefs(ctx, map[string]string{"refs/heads/removed": root}); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestPreservedRefRequiresPinnedSourceAndSupersededDestination(t *testing.T) {
	ctx := context.Background()
	src, dst := newRefRepo(t), newRefRepo(t)
	root := src.commit(t, "", "root")
	src.set(t, "refs/heads/main", root)
	snap, err := src.git.FetchRefs(ctx, map[string]string{"refs/heads/main": root})
	if err != nil {
		t.Fatal(err)
	}
	for _, targets := range []map[string]string{
		{"refs/heads/main": "refs/heads/main"},
		{"refs/heads/superseded-other": "refs/heads/not-fetched"},
		{"refs/heads/superseded-": "refs/heads/main"},
	} {
		if err := snap.PushPreserved(ctx, dst.git, targets); err == nil {
			t.Fatalf("accepted invalid targets: %v", targets)
		}
	}
	if got, err := dst.git.Refs(ctx); err != nil || len(got) != 0 {
		t.Fatalf("invalid push changed destination: %v %v", got, err)
	}
}

func TestTransportRejectsInvalidInputsAndSanitizesErrors(t *testing.T) {
	ctx := context.Background()
	r := newRefRepo(t)
	for _, raw := range []string{"file:///tmp/repo", r.git.URL + "?token=secret", "http://user:password@127.0.0.1/", r.git.URL + "#fragment"} {
		g := *r.git
		g.URL = raw
		if _, err := g.Refs(ctx); err == nil {
			t.Errorf("accepted invalid URL: %s", raw)
		}
	}
	g := *r.git
	g.Token = "wrong-secret"
	if _, err := g.Refs(ctx); err == nil || strings.Contains(err.Error(), "wrong-secret") || strings.Contains(err.Error(), r.git.Token) {
		t.Fatalf("credential error: %v", err)
	}
	cancel, done := context.WithCancel(ctx)
	done()
	if _, err := r.git.Refs(cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestTransportRejectsCredentialBearingRedirect(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		followed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.URL+"/unexpected", http.StatusFound)
	}))
	defer redirect.Close()
	g := &Git{URL: redirect.URL, Repo: "fabric", User: "mirror", Token: "secret"}
	if _, err := g.Refs(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if followed {
		t.Fatal("credential-bearing request reached redirected endpoint")
	}
}
