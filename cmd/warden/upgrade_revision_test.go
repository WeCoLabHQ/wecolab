package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// A real git-http-backend receives the maintenance push; the API side serves
// contents at the requested immutable SHA. The other writer can add an App
// after listing or between the push advertisement and its receive-pack update.
func TestUpgradeInventoryPinnedThroughMaintenancePush(t *testing.T) {
	for _, race := range []string{"", "after-list", "during-push"} {
		name := "unchanged"
		if race != "" {
			name = "remote app added " + race
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			bare, work := filepath.Join(root, "fabric", "fabric.git"), filepath.Join(root, "work")
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %s: %v: %s", args[0], err, out)
				}
				return strings.TrimSpace(string(out))
			}
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "fabric"), 0700); err != nil {
				t.Fatal(err)
			}
			git(root, "init", "--bare", "-q", bare)
			git(bare, "config", "http.receivepack", "true")
			git(work, "init", "-q", "-b", "main")
			put := func(p string, b []byte) {
				t.Helper()
				full := filepath.Join(work, filepath.FromSlash(p))
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			put("fabric/projects/p.yaml", []byte("kind: Project\n"))
			put("fabric/sites/home.yaml", []byte("kind: Site\n"))
			put("fabric/sites/away.yaml", []byte("kind: Site\n"))
			put(fabric.UpgradePath, []byte(`{"phase":"ready","sites":["away","home"]}`))
			app := v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}}
			app.Spec.Database, app.Spec.Primary = "wiki-db", "home"
			b, err := fabric.YAML(&app, v1alpha1.GroupVersion.WithKind("App"))
			if err != nil {
				t.Fatal(err)
			}
			put("fabric/apps/p/wiki.yaml", b)
			commit := func() {
				t.Helper()
				git(work, "add", "-A")
				git(work, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "fixture")
				git(work, "push", "-q", bare, "main")
			}
			commit()
			base := git(work, "rev-parse", "HEAD")
			requests, moved := 0, false
			addRemote := func() {
				moved = true
				other := app.DeepCopy()
				other.Name, other.Spec.Database, other.Spec.Primary = "remote", "remote-db", "away"
				newDoc, err := fabric.YAML(other, v1alpha1.GroupVersion.WithKind("App"))
				if err != nil {
					t.Error(err)
					return
				}
				put("fabric/apps/p/remote.yaml", newDoc)
				commit()
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				const api = "/api/v1/repos/fabric/fabric/"
				if strings.HasPrefix(r.URL.Path, api) {
					if r.Method != http.MethodGet {
						requests++
						http.Error(w, "contents mutation forbidden", http.StatusMethodNotAllowed)
						return
					}
					if r.URL.Path == api+"branches/main" {
						_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"id": git(work, "rev-parse", "HEAD")}})
						return
					}
					p := strings.TrimPrefix(r.URL.Path, api+"contents/")
					if r.URL.Query().Get("ref") != base {
						http.Error(w, "unpinned read", http.StatusBadRequest)
						return
					}
					// A file is read via git show, a directory via ls-tree.
					if kind := git(work, "cat-file", "-t", base+":"+p); kind == "tree" {
						listing := git(work, "ls-tree", "--name-only", base+":"+p)
						entries := []map[string]string{}
						for _, entry := range strings.Split(listing, "\n") {
							if entry != "" {
								entries = append(entries, map[string]string{"type": "file", "path": p + "/" + entry})
							}
						}
						_ = json.NewEncoder(w).Encode(entries)
						if race == "after-list" && !moved && p == "fabric/projects" {
							addRemote()
						}
						return
					}
					data := git(work, "show", base+":"+p)
					_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(data))})
					return
				}
				if race == "during-push" && !moved && r.Method == http.MethodPost &&
					strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
					addRemote()
				}
				cmd := exec.Command("git", "http-backend")
				cmd.Env = append(os.Environ(),
					"GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=fixture",
					"REQUEST_METHOD="+r.Method, "PATH_INFO="+r.URL.Path, "QUERY_STRING="+r.URL.RawQuery,
					"CONTENT_TYPE="+r.Header.Get("Content-Type"), fmt.Sprintf("CONTENT_LENGTH=%d", r.ContentLength))
				cmd.Stdin = r.Body
				out, err := cmd.Output()
				if err != nil {
					t.Errorf("git http-backend: %v", err)
					http.Error(w, "backend error", 500)
					return
				}
				header, body, ok := bytes.Cut(out, []byte("\r\n\r\n"))
				separator := "\r\n"
				if !ok {
					header, body, ok = bytes.Cut(out, []byte("\n\n"))
					separator = "\n"
				}
				if !ok {
					t.Error("missing git CGI headers")
					http.Error(w, "bad CGI", 500)
					return
				}
				code := http.StatusOK
				for _, line := range strings.Split(string(header), separator) {
					key, value, found := strings.Cut(line, ": ")
					if !found {
						continue
					}
					if key == "Status" {
						_, _ = fmt.Sscanf(value, "%d", &code)
					} else {
						w.Header().Set(key, value)
					}
				}
				w.WriteHeader(code)
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			g := &fabric.Git{URL: srv.URL, Repo: "fabric/fabric", Token: "fixture", HTTP: srv.Client()}
			ctx := context.Background()
			revision, err := g.Head(ctx)
			if err != nil || revision != base {
				t.Fatalf("head %s: %v", revision, err)
			}
			sites, err := g.ListAt(ctx, "fabric/sites", revision)
			if err != nil || len(sites) != 2 {
				t.Fatalf("sites %v: %v", sites, err)
			}
			apps, err := gitUpgradeApps(ctx, g, revision)
			if err != nil || len(apps) != 1 {
				t.Fatalf("pinned apps %v: %v", apps, err)
			}
			err = fabric.BeginUpgrade(ctx, g, []string{"home", "away"}, revision)
			gate := git(work, "--git-dir="+bare, "rev-parse", "main")
			if race != "" {
				if err == nil || gate == base || !moved {
					t.Fatalf("changed branch accepted or not moved: err=%v moved=%v", err, moved)
				}
				if contents := git(work, "--git-dir="+bare, "show", "main:"+fabric.UpgradePath); strings.Contains(contents, "maintenance") {
					t.Fatal("maintenance accepted after remote App addition")
				}
			} else {
				if err != nil || gate == base {
					t.Fatalf("unchanged revision refused: %v", err)
				}
				if contents := git(work, "--git-dir="+bare, "show", "main:"+fabric.UpgradePath); !strings.Contains(contents, "maintenance") {
					t.Fatal("gate not committed")
				}
			}
			if requests != 0 {
				t.Fatalf("unexpected contents mutations: %d", requests)
			}
		})
	}
}
