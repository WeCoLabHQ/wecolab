package warden

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"wecolab.io/wecolab/internal/fabric"
)

func custodyIP(host byte) netip.Addr { return netip.AddrFrom4([4]byte{10, 77, 0, host}) }

// custodyRepo is a Forgejo REST surface backed by an actual bare Git repository.
// In particular, commit existence and ancestry come from Git, not an API echo.
type custodyRepo struct {
	t                *testing.T
	root, bare, work string
	server           *httptest.Server
	mu               sync.Mutex
	id               int64
	deleted          int
	mirrors          []fabric.PushMirror
	protection       map[string]map[string]any
	beforeDelete     func()
}

func custodyGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newCustodyRepo(t *testing.T) *custodyRepo {
	t.Helper()
	root := t.TempDir()
	r := &custodyRepo{t: t, root: root, bare: filepath.Join(root, "fabric", "fabric.git"), work: filepath.Join(root, "work"), id: 1}
	if err := os.MkdirAll(filepath.Dir(r.bare), 0700); err != nil {
		t.Fatal(err)
	}
	custodyGit(t, root, "init", "--bare", "-q", r.bare)
	custodyGit(t, root, "init", "-q", "-b", "main", r.work)
	custodyGit(t, r.bare, "config", "http.receivepack", "true")
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

func (r *custodyRepo) ref(name string) string {
	r.t.Helper()
	cmd := exec.Command("git", "--git-dir="+r.bare, "rev-parse", "--verify", name)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func (r *custodyRepo) set(name, sha string) {
	r.t.Helper()
	custodyGit(r.t, r.root, "--git-dir="+r.bare, "update-ref", name, sha)
}
func (r *custodyRepo) show(sha, path string) string {
	r.t.Helper()
	return custodyGit(r.t, r.root, "--git-dir="+r.bare, "show", sha+":"+path)
}
func (r *custodyRepo) commit(parent string, files map[string]string) string {
	r.t.Helper()
	if parent != "" {
		custodyGit(r.t, r.work, "fetch", "-q", r.bare, parent)
		custodyGit(r.t, r.work, "checkout", "-q", "-B", "main", "FETCH_HEAD")
	}
	for path, content := range files {
		full := filepath.Join(r.work, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			r.t.Fatal(err)
		}
	}
	custodyGit(r.t, r.work, "add", "-A")
	custodyGit(r.t, r.work, "commit", "-qm", fmt.Sprintf("fixture-%d", len(files)))
	sha := custodyGit(r.t, r.work, "rev-parse", "HEAD")
	custodyGit(r.t, r.bare, "fetch", "-q", r.work, "HEAD")
	r.set("refs/heads/main", sha)
	return sha
}

func (r *custodyRepo) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	const api = "/api/v1/repos/fabric/fabric"
	if req.URL.Path == "/api/v1/user/repos" && req.Method == http.MethodPost {
		if err := os.MkdirAll(filepath.Dir(r.bare), 0700); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if out, err := exec.Command("git", "init", "--bare", "-q", r.bare).CombinedOutput(); err != nil {
			http.Error(w, string(out), 500)
			return
		}
		if out, err := exec.Command("git", "--git-dir="+r.bare, "config", "http.receivepack", "true").CombinedOutput(); err != nil {
			http.Error(w, string(out), 500)
			return
		}
		r.id++
		w.WriteHeader(http.StatusCreated)
		return
	}
	if strings.HasPrefix(req.URL.Path, api) {
		if _, err := os.Stat(r.bare); err != nil {
			http.NotFound(w, req)
			return
		}
		p := strings.TrimPrefix(req.URL.Path, api)
		switch {
		case p == "" && req.Method == http.MethodDelete:
			if r.beforeDelete != nil {
				r.beforeDelete()
			}
			if err := os.RemoveAll(r.bare); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			r.deleted++
			w.WriteHeader(http.StatusNoContent)
			return
		case p == "":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": r.id})
			return
		case p == "/collaborators/mirror":
			w.WriteHeader(http.StatusNoContent)
			return
		case strings.HasPrefix(p, "/branch_protections/") && req.Method == http.MethodGet:
			rule := strings.TrimPrefix(p, "/branch_protections/")
			if r.protection[rule] == nil {
				http.NotFound(w, req)
				return
			}
			_ = json.NewEncoder(w).Encode(r.protection[rule])
			return
		case strings.HasPrefix(p, "/branch_protections"):
			var b map[string]any
			if err := json.NewDecoder(req.Body).Decode(&b); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			rule, _ := b["rule_name"].(string)
			if r.protection == nil {
				r.protection = make(map[string]map[string]any)
			}
			r.protection[rule] = b
			return
		case p == "/push_mirrors" && req.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(r.mirrors)
			return
		case strings.HasPrefix(p, "/push_mirrors/") && req.Method == http.MethodDelete:
			name := strings.TrimPrefix(p, "/push_mirrors/")
			for i, m := range r.mirrors {
				if m.Name == name {
					r.mirrors = append(r.mirrors[:i], r.mirrors[i+1:]...)
					break
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case p == "/branches/main":
			sha := r.ref("refs/heads/main")
			if sha == "" {
				http.NotFound(w, req)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"id": sha}})
			return
		case strings.HasPrefix(p, "/contents/"):
			path := strings.TrimPrefix(p, "/contents/")
			ref := req.URL.Query().Get("ref")
			if ref == "main" {
				ref = r.ref("refs/heads/main")
			}
			if ref == "" {
				http.NotFound(w, req)
				return
			}
			cmd := exec.Command("git", "--git-dir="+r.bare, "show", ref+":"+path)
			content, err := cmd.Output()
			if err != nil {
				http.NotFound(w, req)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "path": path, "content": base64.StdEncoding.EncodeToString(content)})
			return
		case strings.HasPrefix(p, "/git/commits/"):
			sha := strings.TrimPrefix(p, "/git/commits/")
			if _, err := exec.Command("git", "--git-dir="+r.bare, "cat-file", "-e", sha+"^{commit}").Output(); err != nil {
				http.NotFound(w, req)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
			return
		case p == "/commits":
			if r.ref("refs/heads/main") == "" {
				http.NotFound(w, req)
				return
			}
			cmd := exec.Command("git", "--git-dir="+r.bare, "rev-list", "--max-count=50", "main")
			out, err := cmd.Output()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			commits := []map[string]string{}
			for _, sha := range strings.Fields(string(out)) {
				commits = append(commits, map[string]string{"sha": sha})
			}
			_ = json.NewEncoder(w).Encode(commits)
			return
		}
		http.Error(w, "unexpected Forgejo API "+req.Method+" "+p, http.StatusTeapot)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	cmd := exec.CommandContext(req.Context(), "git", "http-backend")
	cmd.Env = append(os.Environ(), "GIT_PROJECT_ROOT="+r.root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=mirror", "REQUEST_METHOD="+req.Method, "PATH_INFO="+req.URL.Path, "QUERY_STRING="+req.URL.RawQuery, "CONTENT_TYPE="+req.Header.Get("Content-Type"))
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, "git http-backend: "+err.Error(), 500)
		return
	}
	head, payload, ok := bytes.Cut(out, []byte("\r\n\r\n"))
	if !ok {
		http.Error(w, "invalid CGI output", 500)
		return
	}
	code := http.StatusOK
	for _, line := range strings.Split(string(head), "\r\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		if key == "Status" {
			_, _ = fmt.Sscanf(value, "%d", &code)
		} else {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(code)
	_, _ = w.Write(payload)
}

// The writer and smart Git use their actual logical manager URLs; the test
// transport maps those URLs to independent local Warden/Forgejo servers.
type custodyRoute func(*http.Request) (*http.Response, error)

func (f custodyRoute) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func custodyHTTP(t *testing.T, routes map[string]string) *http.Client {
	t.Helper()
	return &http.Client{Transport: custodyRoute(func(req *http.Request) (*http.Response, error) {
		actual := routes[req.URL.Host]
		if actual == "" {
			return nil, fmt.Errorf("unrouted custody request: %s", req.URL.Host)
		}
		replaced := req.Clone(req.Context())
		u := *replaced.URL
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(actual, "http://")
		replaced.URL = &u
		replaced.Host = u.Host
		response, err := http.DefaultTransport.RoundTrip(replaced)
		if response != nil {
			response.Request = req
		}
		return response, err
	})}
}

func custodyWriter(t *testing.T, name string, repo *custodyRepo, routes map[string]string, rollout *rolloutFixture, sites ...string) *Writer {
	t.Helper()
	for i, site := range sites {
		s := testSite(site, true, fmt.Sprintf("10.77.0.%d", i+1))
		if err := rollout.client.Create(context.Background(), &s); err != nil {
			t.Fatal(err)
		}
		sec := &corev1.Secret{Data: map[string][]byte{KeyMirror: []byte("fixture")}}
		sec.Name, sec.Namespace = SiteSecret(site), SystemNS
		if err := rollout.client.Create(context.Background(), sec); err != nil {
			t.Fatal(err)
		}
	}
	return &Writer{Site: name, Git: &fabric.Git{URL: repo.server.URL, Repo: "fabric/fabric", Token: "fixture", HTTP: custodyHTTP(t, routes)},
		Client: rollout.client, APIReader: rollout.client, HTTP: custodyHTTP(t, routes), Coordination: testWriterCoordination().CoordinationV1()}
}

func readyCustodyRollout(t *testing.T, fixture *rolloutFixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case nonce := <-fixture.updates:
				if err := fixture.completeRollout(ctx, nonce); err != nil && ctx.Err() == nil {
					t.Errorf("complete Forgejo rollout: %v", err)
				}
			}
		}
	}()
}

func custodyContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func custodySiteDocs(t *testing.T, claim Claim) map[string]string {
	t.Helper()
	return map[string]string{
		SettingsPath:             settingsFile(t, claim.Writer, claim.Epoch, ""),
		"fabric/sites/home.yaml": siteFile(t, testSite("home", true, "10.77.0.1")),
		"fabric/sites/pub.yaml":  siteFile(t, testSite("pub", true, "10.77.0.2")),
		"fabric/sites/edge.yaml": siteFile(t, testSite("edge", true, "10.77.0.3")),
	}
}
