package warden

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"encoding/json"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
	"time"
	"wecolab.io/wecolab/internal/fabric"

	"wecolab.io/wecolab/api/v1alpha1"
)

func restoreFixture(now time.Time) (RestoreArtifact, *v1alpha1.App, *SiteStatus) {
	app := sampleApp()
	app.Namespace, app.Name = "team", "docs"
	app.Spec.Primary = "home"
	app.Spec.ArchiveID = "incarnation"
	app.Spec.Database = "docs-db"
	db := strings.Repeat("a", 64)
	artifact := RestoreArtifact{Version: 1, Scenario: "recreate", Outcome: "passed", StartedAt: now.Add(-time.Hour), CompletedAt: now.Add(-time.Minute), Namespace: "team", App: "docs", ArchiveID: "incarnation", SystemID: "system", Timeline: 2, BackupID: "backup-1", ExpectedAppSHA: strings.Repeat("b", 40), Scope: "database", ComponentVersions: map[string]string{"postgres": "17"}, Operations: []string{"restore isolated target", "read sentinel rows"}, Checks: []RestoreCheck{{Name: "database-readback", Passed: true, ExpectedSHA256: db, ActualSHA256: db}}}
	st := &SiteStatus{Site: "home", Time: now.Add(-time.Second), Apps: map[string]AppState{"team/docs": {Active: "home", Vault: &VaultStatus{LatestBackup: now.Add(-2 * time.Hour), BackupEvidence: &BackupEvidence{Archive: ArchiveName(app, "home"), ID: "backup-1", SystemID: "system", Timeline: 2, BeginWAL: "0/1", EndWAL: "0/2", CompletedAt: now.Add(-2 * time.Hour), ObservedAt: now.Add(-time.Second)}}, Recovery: &RecoveryReport{Samples: []RecoverySample{{ArchiveID: "incarnation", SystemID: "system", Timeline: 2, Pod: "db-1", Role: "primary", ObservedAt: now.Add(-time.Second)}}}}}}
	st.ReceivedAt = now
	return artifact, app, st
}

func TestRestoreArtifactRequiresCompletedReadback(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*RestoreArtifact)
	}{
		{"failed", func(a *RestoreArtifact) { a.Outcome = "failed" }},
		{"bare timestamp", func(a *RestoreArtifact) { a.Checks = nil }},
		{"wrong checksum", func(a *RestoreArtifact) { a.Checks[0].ActualSHA256 = strings.Repeat("c", 64) }},
		{"forged passed flag", func(a *RestoreArtifact) { a.Checks[0].Passed = false }},
		{"incomplete operation", func(a *RestoreArtifact) { a.Operations = nil }},
		{"missing version", func(a *RestoreArtifact) { a.ComponentVersions = nil }},
		{"future completion", func(a *RestoreArtifact) { a.CompletedAt = now.Add(time.Second) }},
		{"unordered timestamps", func(a *RestoreArtifact) { a.StartedAt = a.CompletedAt.Add(time.Second) }},
		{"file scope missing readback", func(a *RestoreArtifact) { a.Scope = "database-and-files" }},
		{"files check invalid", func(a *RestoreArtifact) {
			a.Scope = "database-and-files"
			a.Checks = append(a.Checks, RestoreCheck{Name: "files-readback", Passed: true, ExpectedSHA256: strings.Repeat("a", 64), ActualSHA256: strings.Repeat("b", 64)})
		}},
		{"unsupported scope", func(a *RestoreArtifact) { a.Scope = "files" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := restoreFixture(now)
			tc.mutate(&a)
			if err := ValidateRestoreArtifact(a, now); err == nil {
				t.Fatal("invalid restore evidence accepted")
			}
		})
	}
	a, _, _ := restoreFixture(now)
	if err := ValidateRestoreArtifact(a, now); err != nil {
		t.Fatalf("valid database-only readback: %v", err)
	}
	a.Scope = "database-and-files"
	a.Checks = append(a.Checks, RestoreCheck{Name: "files-readback", Passed: true, ExpectedSHA256: strings.Repeat("a", 64), ActualSHA256: strings.Repeat("a", 64)})
	if err := ValidateRestoreArtifact(a, now); err != nil {
		t.Fatalf("valid file readback: %v", err)
	}
}

func TestRestoreArtifactRejectsUnknownAndDuplicateJSON(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	a, _, _ := restoreFixture(now)
	b, _ := json.Marshal(a)
	for _, input := range [][]byte{append(append([]byte{}, b...), []byte(` {}`)...), []byte(`{"Version":1,"Version":1}`), []byte(`{"Version":1,"Actor":"admin"}`), []byte(`{"Version":1,"EvidenceSHA256":"forged"}`)} {
		if _, err := ParseRestoreArtifact(input); err == nil {
			t.Fatalf("accepted untrusted JSON: %s", input)
		}
	}
}

func TestRestoreIdentityRequiresMatchingFreshObservedBackup(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*RestoreArtifact, *v1alpha1.App, *SiteStatus)
	}{
		{"other app", func(a *RestoreArtifact, app *v1alpha1.App, _ *SiteStatus) { a.App = "other" }},
		{"other incarnation", func(a *RestoreArtifact, app *v1alpha1.App, _ *SiteStatus) { a.ArchiveID = "old" }},
		{"other backup", func(a *RestoreArtifact, _ *v1alpha1.App, _ *SiteStatus) { a.BackupID = "old" }},
		{"other timeline", func(a *RestoreArtifact, _ *v1alpha1.App, _ *SiteStatus) { a.Timeline = 1 }},
		{"other system", func(a *RestoreArtifact, _ *v1alpha1.App, _ *SiteStatus) { a.SystemID = "old" }},
		{"stale backup", func(_ *RestoreArtifact, _ *v1alpha1.App, s *SiteStatus) {
			s.Apps["team/docs"].Vault.BackupEvidence.ObservedAt = now.Add(-time.Hour)
		}},
		{"missing history", func(_ *RestoreArtifact, _ *v1alpha1.App, s *SiteStatus) {
			state := s.Apps["team/docs"]
			state.Recovery = nil
			s.Apps["team/docs"] = state
		}},
		{"old site report", func(_ *RestoreArtifact, _ *v1alpha1.App, s *SiteStatus) { s.ReceivedAt = now.Add(-time.Minute) }},
		{"missing receipt", func(_ *RestoreArtifact, _ *v1alpha1.App, s *SiteStatus) { s.ReceivedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, app, s := restoreFixture(now)
			tc.mutate(&a, app, s)
			if err := ValidateRestoreIdentity(a, app, s, now); err == nil {
				t.Fatal("mismatched restore history accepted")
			}
		})
	}
	a, app, s := restoreFixture(now)
	if err := ValidateRestoreIdentity(a, app, s, now); err != nil {
		t.Fatalf("current evidence: %v", err)
	}
}

func TestRecordRestoreConditionalWriterMutation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		race, stale, files bool
		wantSuccess        bool
	}{
		{"matching database drill", false, false, false, true},
		{"matching file drill", false, false, true, true},
		{"wrong revision", false, true, false, false},
		{"concurrent App replacement", true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			a, app, st := restoreFixture(now)
			if tc.files {
				a.Scope = "database-and-files"
				a.Checks = append(a.Checks, RestoreCheck{Name: "files-readback", Passed: true, ExpectedSHA256: strings.Repeat("a", 64), ActualSHA256: strings.Repeat("a", 64)})
			}
			app.Spec.Sites = []string{"home"}
			initial, err := fabric.YAML(app, v1alpha1.GroupVersion.WithKind("App"))
			if err != nil {
				t.Fatal(err)
			}
			appPath, _ := fabric.Path(v1alpha1.GroupVersion.WithKind("App"), a.Namespace, a.App)
			files := map[string][]byte{appPath: initial, fabric.UpgradePath: []byte(`{"phase":"ready"}`), fabric.PlacementRevisionPath: []byte(`{"revision":"1"}`)}
			files[fabric.MigrationCompletePath] = []byte(`{}`)
			var mu sync.Mutex
			raced := false
			sha := func(p string) string {
				if p == appPath {
					if tc.race && raced {
						return strings.Repeat("c", 40)
					}
					return strings.Repeat("b", 40)
				}
				return strings.Repeat("d", 40)
			}
			if tc.stale {
				a.ExpectedAppSHA = strings.Repeat("e", 40)
			}
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			var posts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case http.MethodGet:
					p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/fabric/fabric/contents/")
					content, ok := files[p]
					if !ok {
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "sha": sha(p), "content": base64.StdEncoding.EncodeToString(content)})
				case http.MethodPost:
					posts++
					if tc.race && !raced {
						raced = true
						http.Error(w, "changed while committing", http.StatusConflict)
						return
					}
					var body struct {
						Files []struct {
							Path    string `json:"path"`
							SHA     string `json:"sha"`
							Content string `json:"content"`
						} `json:"files"`
						Author map[string]string `json:"author"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if body.Author["name"] != "operator@example.org" {
						t.Errorf("author not recorded: %v", body.Author)
					}
					for _, f := range body.Files {
						if f.SHA != sha(f.Path) {
							t.Errorf("writer bypassed conditional SHA on %s", f.Path)
						}
						content, err := base64.StdEncoding.DecodeString(f.Content)
						if err != nil {
							t.Error(err)
						}
						files[f.Path] = content
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "commit-sha"}})
				default:
					http.Error(w, "unsupported", 405)
				}
			}))
			defer srv.Close()
			g := &fabric.Git{URL: srv.URL, Token: "test", Repo: "fabric/fabric", HTTP: srv.Client()}
			_, err = RecordRestore(context.Background(), g, raw, "operator@example.org", func(_ context.Context, site string) (*SiteStatus, error) {
				if site != "home" {
					return nil, fmt.Errorf("wrong primary %s", site)
				}
				return st, nil
			})
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("commit=%v posts=%d", err, posts)
			}
			if !tc.wantSuccess {
				if tc.stale && posts != 0 {
					t.Fatal("stale drill committed")
				}
				return
			}
			if posts != 1 {
				t.Fatalf("expected one conditional commit, got %d", posts)
			}
			var stored v1alpha1.App
			if err := yaml.Unmarshal(files[appPath], &stored); err != nil {
				t.Fatal(err)
			}
			got := stored.Spec.RestoreVerification
			digest := sha256.Sum256(raw)
			if got == nil || got.Actor != "operator@example.org" || got.EvidenceSHA256 != fmt.Sprintf("%x", digest) || got.Scope != a.Scope || got.BackupID != a.BackupID {
				t.Fatalf("wrong durable evidence: %+v", got)
			}
		})
	}
}
