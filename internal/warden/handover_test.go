package warden

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

func TestRememberDemotion(t *testing.T) {
	a := sampleApp()
	a.Spec.Primary, a.Spec.Handover = "friend", &v1alpha1.Handover{ID: "m1", From: "vince"}
	RememberDemotion(a, "vince", dbReport(true, "old", ""))
	RememberDemotion(a, "vince", dbReport(false, "new", "")) // the same handover: what it first saw stays
	if a.Status.Demoting != (v1alpha1.Demoting{Handover: "m1", Stale: "old"}) {
		t.Fatalf("%+v", a.Status.Demoting)
	}
	if d := Demoted(a, "vince", dbReport(false, "new", "")); d == nil || d.Token != "new" {
		t.Fatalf("demoted: %+v", d)
	}
	if Demoted(a, "vince", dbReport(true, "newer", "")) != nil {
		t.Fatal("a writable database's token is always from before its last promotion")
	}
	a.Spec.Handover.ID = "m2"
	RememberDemotion(a, "vince", dbReport(true, "new", ""))
	if a.Status.Demoting.Stale != "new" || Demoted(a, "vince", dbReport(false, "new", "")) != nil {
		t.Fatalf("a new handover: the token already there is its leftover: %+v", a.Status.Demoting)
	}
	// Demoted for m2, which was cancelled unseen and planned again as m3: it has not moved since.
	a.Spec.Handover.ID = "m3"
	RememberDemotion(a, "vince", dbReport(false, "new", ""))
	if d := Demoted(a, "vince", dbReport(false, "new", "")); d == nil || d.Handover != "m3" || d.Token != "new" {
		t.Fatalf("an already demoted database's token is the new handover's: %+v %+v", a.Status.Demoting, d)
	}
	a.Spec.Handover = nil
	RememberDemotion(a, "vince", dbReport(true, "new", ""))
	if a.Status.Demoting != (v1alpha1.Demoting{}) {
		t.Fatal("forgotten once no handover starts here")
	}
}

// A new database is recorded as made in Git once its primary proves it holds it with a base backup.
func TestWriterRecordsTheDatabase(t *testing.T) {
	now := time.Now()
	a := sampleApp()
	reports := map[string]*SiteStatus{"vince": provenReport("vince", now)}
	report := func(s string) *SiteStatus { return reports[s] }
	reports["vince"].Apps["vince/docs"] = AppState{Active: "vince", Vault: &VaultStatus{}}
	if WriterStep(a, report) != "" || !NewDatabase(a.Spec) {
		t.Fatal("not before its first base backup")
	}
	reports["vince"] = provenReport("vince", now)
	if WriterStep(a, report) == "" || NewDatabase(a.Spec) || ArchiveName(a, "vince") != "docs-db-vince" {
		t.Fatalf("recorded, under the same archive: %+v", a.Spec.Archive)
	}
	if WriterStep(a, report) != "" {
		t.Fatal("once only")
	}
}

func TestWriterStep(t *testing.T) {
	a := sampleApp()
	a.Spec.Primary, a.Spec.Handover = "friend", &v1alpha1.Handover{ID: "m1", From: "vince"}
	reports := map[string]*SiteStatus{}
	report := func(s string) *SiteStatus { return reports[s] }
	demoted := func(site, id, tok string) *SiteStatus {
		return &SiteStatus{Site: site, Apps: map[string]AppState{"vince/docs": {Demoted: &Demotion{Handover: id, Token: tok}}}}
	}
	for _, c := range []*SiteStatus{nil, demoted("vince", "m0", "earlier"), demoted("friend", "m1", "tok")} {
		reports["vince"] = c
		if WriterStep(a, report) != "" || a.Spec.Handover.Token != "" {
			t.Fatalf("no token without the old primary's own word for this handover: %+v", c)
		}
	}
	reports["vince"] = demoted("vince", "m1", "tok")
	if WriterStep(a, report) == "" || a.Spec.Handover.Token != "tok" || a.Spec.Handover.From != "vince" {
		t.Fatalf("token: %+v", a.Spec.Handover)
	}
	for _, c := range []*SiteStatus{nil, {Site: "friend", DB: map[string]map[string]any{"vince/docs-db": dbReport(true, "", "older")}},
		{Site: "friend", DB: map[string]map[string]any{"vince/docs-db": {"phase": "Promoting", "lastPromotionToken": "tok"}}},
		{Site: "vince", DB: map[string]map[string]any{"vince/docs-db": dbReport(true, "", "tok")}}} {
		reports["friend"] = c
		if WriterStep(a, report) != "" || a.Spec.Handover == nil {
			t.Fatalf("the handover stays until the primary itself promoted with the token: %+v", c)
		}
	}
	reports["friend"] = &SiteStatus{Site: "friend", DB: map[string]map[string]any{"vince/docs-db": dbReport(true, "", "tok")}}
	if WriterStep(a, report) == "" || a.Spec.Handover != nil || a.Spec.Primary != "friend" {
		t.Fatalf("done: %+v", a.Spec)
	}
}

// The writer's step is decided again on the App as Git has it: a handover cancelled in Git meanwhile is
// left alone, whatever the cluster copy said, and a step other than the one the message names waits.
func TestHandoverCommitsAgainstGit(t *testing.T) {
	file := func(h *v1alpha1.Handover) string {
		a := sampleApp()
		a.Spec.Primary, a.Spec.Handover = "friend", h
		b, _ := fabric.YAML(a, v1alpha1.GroupVersion.WithKind("App"))
		return string(b)
	}
	copied := sampleApp()
	copied.Spec.Primary, copied.Spec.Handover = "friend", &v1alpha1.Handover{ID: "m1", From: "vince"}
	content := file(&v1alpha1.Handover{ID: "m1", From: "vince"})
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "sha": "s", "content": base64.StdEncoding.EncodeToString([]byte(content))})
		case http.MethodPost:
			var body struct{ Files []map[string]any }
			_ = json.NewDecoder(r.Body).Decode(&body)
			b, _ := base64.StdEncoding.DecodeString(body.Files[0]["content"].(string))
			posted = append(posted, string(b))
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": "c"}})
		}
	}))
	defer srv.Close()
	h := &Handovers{Site: "vince", Git: &fabric.Git{URL: srv.URL, Token: "t", Repo: "fabric/fabric", HTTP: srv.Client()}}
	report := func(string) *SiteStatus {
		return &SiteStatus{Site: "vince", Apps: map[string]AppState{"vince/docs": {Demoted: &Demotion{Handover: "m1", Token: "tok"}}}}
	}
	if _, err := h.step(context.Background(), "vince", "docs", "another step", report); err != nil || len(posted) != 0 {
		t.Fatalf("a commit under a message that is not its step: %v %d", err, len(posted))
	}
	msg := WriterStep(copied, report)
	if done, err := h.step(context.Background(), "vince", "docs", msg, report); err != nil || !done || len(posted) != 1 {
		t.Fatalf("%v %d", err, len(posted))
	}
	got := &v1alpha1.App{}
	if err := yaml.Unmarshal([]byte(posted[0]), got); err != nil || got.Spec.Handover == nil || got.Spec.Handover.Token != "tok" || strings.Contains(posted[0], "status") {
		t.Fatalf("%v\n%s", err, posted[0])
	}
	content = file(nil) // cancelled in Git
	if done, err := h.step(context.Background(), "vince", "docs", msg, report); err != nil || done || len(posted) != 1 {
		t.Fatalf("a step Git no longer calls for is not made: %v %d", err, len(posted))
	}
}

// A deleted app leaves the Fabric only once every site of it answers and none still holds anything of it;
// a site no longer in the fabric is not waited for.
func TestDeletionDone(t *testing.T) {
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "wiki"}, Spec: v1alpha1.AppSpec{Sites: []string{"a", "b", "gone"}, Primary: "a", Deleted: true}}
	sites := []v1alpha1.Site{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}, {ObjectMeta: metav1.ObjectMeta{Name: "b"}}}
	clean := func(site string) *SiteStatus { return &SiteStatus{Site: site, Apps: map[string]AppState{}} }
	holding := func(site string) *SiteStatus {
		return &SiteStatus{Site: site, Apps: map[string]AppState{"p/wiki": {Deleting: true}}}
	}
	for _, c := range []struct {
		name   string
		report func(string) *SiteStatus
		done   bool
	}{
		{"every site clean", clean, true},
		{"b down", func(s string) *SiteStatus { return map[string]*SiteStatus{"a": clean("a")}[s] }, false},
		{"b still removing", func(s string) *SiteStatus {
			if s == "b" {
				return holding(s)
			}
			return clean(s)
		}, false},
		{"b answers as another site", func(s string) *SiteStatus { return clean("a") }, false},
	} {
		if got := DeletionDone(a, sites, c.report); got != c.done {
			t.Errorf("%s: done=%v", c.name, got)
		}
	}
}
