package warden

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"wecolab.io/wecolab/api/v1alpha1"
)

// Every stage of a planned move b -> c at three sites: who each site takes for the primary, what it
// replays, and who carries the token.
func TestRoleAt(t *testing.T) {
	sites := []string{"a", "b", "c"}
	base := v1alpha1.AppSpec{Sites: sites, Primary: "b", Database: "db"}
	demoting, promoting := base, base
	demoting.Primary, demoting.Handover = "c", &v1alpha1.Handover{ID: "m1", From: "b"}
	promoting.Primary, promoting.Handover = "c", &v1alpha1.Handover{ID: "m1", From: "b", Token: "tok"}
	for _, c := range []struct {
		name string
		spec v1alpha1.AppSpec
		want map[string]Role
	}{
		{"steady", base, map[string]Role{"a": {"b", "b", ""}, "b": {"b", "a", ""}, "c": {"b", "b", ""}}},
		{"demoting", demoting, map[string]Role{"a": {"b", "b", ""}, "b": {"c", "b", ""}, "c": {"b", "b", ""}}},
		{"promoting", promoting, map[string]Role{"a": {"c", "b", ""}, "b": {"c", "b", ""}, "c": {"c", "b", "tok"}}},
	} {
		primaries := 0
		for _, s := range sites {
			r := RoleAt(c.spec, s)
			if r != c.want[s] {
				t.Errorf("%s at %s: %+v, want %+v", c.name, s, r, c.want[s])
			}
			if r.Primary == s {
				primaries++
			}
		}
		if primaries > 1 {
			t.Errorf("%s: %d sites may promote", c.name, primaries)
		}
	}
	if Serving(demoting) != "b" || Serving(promoting) != "c" || Serving(base) != "b" {
		t.Error("the history is at the old primary until its token is in Git")
	}
}

// patchesOf maps "<kind>/<name>" to the decoded JSON patch ops of a Kustomization.
func patchesOf(t *testing.T, app *v1alpha1.App, self string, l Local) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	ps, _, _ := unstructured.NestedSlice(AppKustomization(app, self, l, Standing{}).Object, "spec", "patches")
	for _, p := range ps {
		pm := p.(map[string]any)
		tg := pm["target"].(map[string]any)
		var ops []map[string]any
		if err := json.Unmarshal([]byte(pm["patch"].(string)), &ops); err != nil {
			t.Fatal(err)
		}
		out[fmt.Sprint(tg["kind"], "/", tg["name"])] = ops
	}
	return out
}

func opValue(ops []map[string]any, path string) any {
	for _, o := range ops {
		if o["path"] == path {
			return o["value"]
		}
	}
	return nil
}

func TestAppKustomization(t *testing.T) {
	a := sampleApp()
	a.Spec.Archive = map[string]int{"friend": 2}
	prim, stby := patchesOf(t, a, "vince", Local{Created: true}), patchesOf(t, a, "friend", Local{Created: true})
	if opValue(prim["Cluster/docs-db"], "/spec/replica/self") != "vince" || opValue(stby["Cluster/docs-db"], "/spec/replica/source") != "vince" {
		t.Fatalf("db roles: %v / %v", prim["Cluster/docs-db"], stby["Cluster/docs-db"])
	}
	if s := toJSON(opValue(stby["Cluster/docs-db"], "/spec/externalClusters")); s != `[{"name":"vince","plugin":{"name":"barman-cloud.cloudnative-pg.io","parameters":{"barmanObjectName":"docs-db-vault","serverName":"docs-db-vince"}}},{"name":"friend","plugin":{"name":"barman-cloud.cloudnative-pg.io","parameters":{"barmanObjectName":"docs-db-vault","serverName":"docs-db-friend-g2"}}}]` {
		t.Fatalf("every site's current archive, and no older one: %s", s)
	}
	if opValue(prim["ScheduledBackup/docs-db-backup"], "/spec/suspend") != false || opValue(stby["ScheduledBackup/docs-db-backup"], "/spec/suspend") != true {
		t.Fatal("backups must run at the primary only")
	}
	if _, stopped := prim["Deployment/docs"]; stopped {
		t.Fatal("app stopped at its primary")
	}
	if opValue(stby["Deployment/docs"], "/spec/replicas") != float64(0) {
		t.Fatalf("app must not run beside a read-only standby: %v", stby["Deployment/docs"])
	}

	// A new database starts empty at the primary only, and only while Git and Flux's record here say it is
	// new; once made, never again.
	boot := func(ps map[string][]map[string]any) string {
		return toJSON(opValue(ps["Cluster/docs-db"], "/spec/bootstrap"))
	}
	if b := boot(patchesOf(t, sampleApp(), "vince", Local{})); b != `{"initdb":{"database":"docs","owner":"docs","secret":{"name":"docs-db-app"}}}` {
		t.Fatalf("new primary: %s", b)
	}
	if b := boot(patchesOf(t, sampleApp(), "vince", Local{Created: true})); b != `{"recovery":{"database":"docs","owner":"docs","secret":{"name":"docs-db-app"},"source":"vince"}}` {
		t.Fatalf("a primary's database, once made here, comes back only from its own archive: %s", b)
	}
	if b := boot(patchesOf(t, a, "vince", Local{})); b != `{"recovery":{"database":"docs","owner":"docs","secret":{"name":"docs-db-app"},"source":"vince"}}` {
		t.Fatalf("a primary Git knows made its database never starts empty, even with no record here: %s", b)
	}
	if b := boot(patchesOf(t, a, "friend", Local{})); b != `{"recovery":{"database":"docs","owner":"docs","secret":{"name":"docs-db-app"},"source":"vince"}}` {
		t.Fatalf("standby: %s", b)
	}
	if s := opValue(patchesOf(t, a, "friend", Local{Created: true, Archive: "docs-db-friend"})["Cluster/docs-db"], "/spec/plugins/0/parameters/serverName"); s != "docs-db-friend" {
		t.Fatalf("a database waiting to be rebuilt keeps its own archive: %v", s)
	}

	// During a handover the app stops everywhere, and the token goes to the primary only.
	a.Spec.Primary, a.Spec.Handover = "friend", &v1alpha1.Handover{ID: "m", From: "vince", Token: "tok"}
	for _, site := range a.Spec.Sites {
		ps := patchesOf(t, a, site, Local{Created: true})
		if _, stopped := ps["Deployment/docs"]; !stopped {
			t.Errorf("app runs at %s while its database changes hands", site)
		}
		if tok := opValue(ps["Cluster/docs-db"], "/spec/replica/promotionToken"); (tok == "tok") != (site == "friend") {
			t.Errorf("token at %s: %v", site, tok)
		}
	}

	a.Spec.Database, a.Spec.Handover = "", nil
	ps := patchesOf(t, a, "friend", Local{})
	if len(ps) != 1 || opValue(ps["Service/docs"], "/spec/type") != "NodePort" || opValue(ps["Service/docs"], "/spec/externalTrafficPolicy") != "Local" {
		t.Fatalf("an app without a database runs everywhere, reachable from the Door only: %v", ps)
	}
	// Capacity a project holds here decides where its pods go: best effort tolerates laptops and
	// idle time; pinned offers pin to their boxes. The owner's own site is never patched.
	held := Standing{Held: &OfferAt{Site: "friend", BestEffort: true, Boxes: []string{"friend-mac"}}}
	k := AppKustomization(a, "friend", Local{}, held)
	patches, _, _ := unstructured.NestedSlice(k.Object, "spec", "patches")
	var pod []map[string]any
	for _, p := range patches {
		pm := p.(map[string]any)
		if tg := pm["target"].(map[string]any); tg["kind"] == "Deployment" && tg["name"] == nil {
			_ = json.Unmarshal([]byte(pm["patch"].(string)), &pod)
		}
	}
	if opValue(pod, "/spec/template/spec/priorityClassName") != BestEffortClass || opValue(pod, "/spec/template/spec/affinity") == nil {
		t.Fatalf("held capacity: %v", pod)
	}
	held.Owner = true
	owned, _, _ := unstructured.NestedSlice(AppKustomization(a, "friend", Local{}, held).Object, "spec", "patches")
	if len(owned) != 1 {
		t.Fatalf("the owner's site is not patched for capacity: %v", owned)
	}
	k = AppKustomization(a, "friend", Local{}, Standing{})
	if sa, _, _ := unstructured.NestedString(k.Object, "spec", "serviceAccountName"); sa != "tenant-vince" {
		t.Fatal("Flux must apply as the project, not as the cluster's admin")
	}
}

// provenReport is a primary's report that proves R4: promoted, healthy, a base backup in its vault.
func provenReport(site string, now time.Time) *SiteStatus {
	return &SiteStatus{Site: site,
		DB:   map[string]map[string]any{"vince/docs-db": dbReport(true, "", "")},
		Apps: map[string]AppState{"vince/docs": {Active: site, Vault: &VaultStatus{LatestBackup: now}}}}
}

// dbReport is a CloudNativePG status as a site reports it.
func dbReport(primary bool, demotion, promotion string) map[string]any {
	return map[string]any{"phase": cnpgHealthy, "currentPrimary": "db-1", "readyInstances": int64(1), "demotionToken": demotion,
		"lastPromotionToken": promotion, "instancesReportedState": map[string]any{"db-1": map[string]any{"isPrimary": primary}}}
}

// R4: a database is destroyed only on proof from Git and from the primary.
func TestMayDestroy(t *testing.T) {
	now := time.Now()
	app := func(primary string, h *v1alpha1.Handover) *v1alpha1.App {
		a := sampleApp()
		a.Spec.Sites, a.Spec.Primary, a.Spec.Handover = []string{"vince", "friend", "third"}, primary, h
		return a
	}
	mod := func(f func(*SiteStatus)) *SiteStatus { st := provenReport("friend", now); f(st); return st }
	for _, c := range []struct {
		name    string
		app     *v1alpha1.App
		self    string
		primary *SiteStatus
		ok      bool
	}{
		{"proven", app("friend", nil), "vince", provenReport("friend", now), true},
		{"this site is the primary", app("vince", nil), "vince", provenReport("vince", now), false},
		{"this site hands over", app("friend", &v1alpha1.Handover{ID: "m", From: "vince", Token: "t"}), "vince", provenReport("friend", now), false},
		{"no token yet", app("friend", &v1alpha1.Handover{ID: "m", From: "third"}), "vince", provenReport("friend", now), false},
		{"the primary does not answer", app("friend", nil), "vince", nil, false},
		{"another site answers", app("friend", nil), "vince", provenReport("third", now), false},
		{"the primary lags behind Git", app("friend", nil), "vince", mod(func(s *SiteStatus) {
			s.Apps["vince/docs"] = AppState{Active: "vince", Vault: s.Apps["vince/docs"].Vault}
		}), false},
		{"still a replica", app("friend", nil), "vince", mod(func(s *SiteStatus) { s.DB["vince/docs-db"] = dbReport(false, "", "") }), false},
		{"not healthy", app("friend", nil), "vince", mod(func(s *SiteStatus) { s.DB["vince/docs-db"]["phase"] = "Setting up primary" }), false},
		{"no base backup", app("friend", nil), "vince", mod(func(s *SiteStatus) { s.Apps["vince/docs"] = AppState{Active: "friend", Vault: &VaultStatus{}} }), false},
		{"vault unreadable", app("friend", nil), "vince", mod(func(s *SiteStatus) {
			s.Apps["vince/docs"] = AppState{Active: "friend", Vault: &VaultStatus{Err: "401"}}
		}), false},
		{"promoted without the handover's token", app("friend", &v1alpha1.Handover{ID: "m", From: "third", Token: "t"}), "vince", provenReport("friend", now), false},
		{"promoted with it", app("friend", &v1alpha1.Handover{ID: "m", From: "third", Token: "t"}), "vince", mod(func(s *SiteStatus) { s.DB["vince/docs-db"]["lastPromotionToken"] = "t" }), true},
	} {
		if err := MayDestroy(c.app, c.self, c.primary); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	a := app("friend", nil)
	a.Spec.Database = ""
	if MayDestroy(a, "vince", nil) != nil {
		t.Error("an app without a database has nothing to keep")
	}
}

// An App gone from the Fabric without the deleted mark leaves all its data at every site.
func TestDropKeepsTheHistory(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	mine := map[string]string{"kustomize.toolkit.fluxcd.io/name": kustomizationName("vince", "docs"), "kustomize.toolkit.fluxcd.io/namespace": FluxNS}
	// An App gone from Git without the deleted mark deletes nothing: not the history, not a standby's
	// copy, not a volume (decision 12).
	for primary, kept := range map[string]bool{"vince": true, "friend": true} {
		db := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"replica": map[string]any{"self": "vince", "primary": primary, "source": "friend"}}}}
		db.SetGroupVersionKind(gvkDBCluster)
		db.SetNamespace("vince")
		db.SetName("docs-db")
		db.SetLabels(mine)
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "docs-data", Namespace: "vince", Labels: mine}}
		k := &unstructured.Unstructured{}
		k.SetGroupVersionKind(gvkKustomization)
		k.SetNamespace(FluxNS)
		k.SetName(kustomizationName("vince", "docs"))
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(db, pvc, k).Build()
		if err := (&AppReconciler{Client: c, Site: "vince"}).drop(ctx, "vince", "docs", true); err != nil {
			t.Fatal(err)
		}
		gotDB, gotPVC := db.DeepCopy(), pvc.DeepCopy()
		dbErr, pvcErr, kErr := c.Get(ctx, client.ObjectKeyFromObject(db), gotDB), c.Get(ctx, client.ObjectKeyFromObject(pvc), gotPVC), c.Get(ctx, client.ObjectKeyFromObject(k), k.DeepCopy())
		if (dbErr == nil) != kept || (pvcErr == nil) != kept || !apierrors.IsNotFound(kErr) {
			t.Errorf("primary %s: database %v, volume %v, kustomization %v", primary, dbErr, pvcErr, kErr)
		}
		// Flux's finalizer prunes the Kustomization's inventory: kept data must be marked for it to skip.
		if kept && (gotDB.GetAnnotations()[fluxPrune] != "disabled" || gotPVC.Annotations[fluxPrune] != "disabled") {
			t.Errorf("kept data is not excluded from Flux's pruning: %v %v", gotDB.GetAnnotations(), gotPVC.Annotations)
		}
	}
}

func TestPlanAt(t *testing.T) {
	now := time.Now()
	a := sampleApp()
	a.Spec.Primary, a.Spec.Archive = "friend", map[string]int{"vince": 2} // forced away from vince
	stale := Here{Exists: true, Created: true, Archive: "docs-db-vince"}
	if p := PlanAt(a, "vince", stale, nil); p.Destroy || p.Suspend || p.Local.Archive != "docs-db-vince" || p.Promotion.Reason != "RebuildWaiting" {
		t.Fatalf("without the primary's proof the old database stays, writing only its own archive: %+v", p)
	}
	if p := PlanAt(a, "vince", stale, provenReport("friend", now)); !p.Destroy || !p.Suspend || p.Local.Archive != "" {
		t.Fatalf("with it, it is rebuilt: %+v", p)
	}
	if p := PlanAt(a, "vince", Here{Exists: true, Created: true, Archive: "docs-db-vince-g2"}, nil); p.Destroy || p.Suspend || p.Promotion.Reason != "Applied" {
		t.Fatalf("current generation: %+v", p)
	}
	if p := PlanAt(a, "vince", Here{Created: true}, nil); p.Suspend || !p.Local.Created {
		t.Fatalf("a standby's lost database is made again from the vault: %+v", p)
	}

	// The primary's database: made once, never again empty.
	if p := PlanAt(sampleApp(), "vince", Here{}, nil); p.Suspend || p.Local.Created {
		t.Fatalf("a new app's database is made: %+v", p)
	}
	for name, h := range map[string]Here{"lost": {Created: true}, "never had one here": {}} {
		if p := PlanAt(a, "friend", h, nil); !p.Suspend || p.Promotion.Reason != "DatabaseMissing" {
			t.Fatalf("a primary with no database (%s) holds the app: %+v", name, p)
		}
	}
	fresh := sampleApp()
	if p := PlanAt(fresh, "vince", Here{Created: true}, nil); !p.Suspend {
		t.Fatalf("a new database Flux made here and lost is not made again: %+v", p)
	}
	forced := sampleApp()
	forced.Spec, _ = ForcedMove(forced.Spec, "friend") // to a site that never had the database
	if p := PlanAt(forced, "friend", Here{}, nil); !p.Suspend || p.Promotion.Reason != "DatabaseMissing" {
		t.Fatalf("a forced move to a site without the database holds the app: %+v", p)
	}
	a.Spec.Archive = map[string]int{"friend": 3}
	if p := PlanAt(a, "friend", Here{Exists: true, Created: true, Archive: "docs-db-friend"}, provenReport("friend", now)); p.Destroy || p.Local.Archive != "" {
		t.Fatalf("the primary is never rebuilt; it moves to its new archive: %+v", p)
	}
	// During a handover the old primary holds the history.
	a.Spec.Primary, a.Spec.Handover = "vince", &v1alpha1.Handover{ID: "m", From: "friend"}
	if p := PlanAt(a, "friend", Here{Created: true}, nil); !p.Suspend {
		t.Fatalf("the old primary's lost database holds the move: %+v", p)
	}
	if p := PlanAt(a, "vince", Here{Created: true}, nil); p.Suspend {
		t.Fatalf("the target's lost replica is made again from the old primary's archive: %+v", p)
	}
	a.Spec.Handover.Token = "tok"
	if p := PlanAt(a, "vince", Here{}, nil); !p.Suspend || p.Promotion.Reason != "DatabaseMissing" {
		t.Fatalf("a target the token reached without a database holds the move: %+v", p)
	}
}

func TestNeedsBackup(t *testing.T) {
	now := time.Now()
	bk := func(db, phase string, age time.Duration) unstructured.Unstructured {
		u := unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"cluster": map[string]any{"name": db}}, "status": map[string]any{"phase": phase}}}
		u.SetCreationTimestamp(metav1.NewTime(now.Add(-age)))
		return u
	}
	empty := &VaultStatus{}
	full := &VaultStatus{LatestBackup: now.Add(-time.Hour)}
	for i, c := range []struct {
		vault   *VaultStatus
		backups []unstructured.Unstructured
		want    bool
	}{
		{empty, nil, true},
		{nil, nil, false}, // no look at the vault yet: wait
		{&VaultStatus{Err: "unreachable"}, nil, false}, // a vault that did not answer: wait
		{full, nil, false}, // the current archive has one
		{empty, []unstructured.Unstructured{bk("docs-db", "completed", 48*time.Hour)}, true}, // an old database's
		{empty, []unstructured.Unstructured{bk("docs-db", "running", time.Minute)}, false},   // in flight
		{empty, []unstructured.Unstructured{bk("docs-db", "failed", time.Minute)}, false},    // failing: hourly
		{empty, []unstructured.Unstructured{bk("docs-db", "failed", 61*time.Minute)}, true},
		{empty, []unstructured.Unstructured{bk("docs-db", "walArchivingFailing", 20*time.Minute)}, false},
		{empty, []unstructured.Unstructured{bk("other-db", "running", time.Minute)}, true},
	} {
		if got := NeedsBackup(c.vault, c.backups, "docs-db", now); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

// An app left at one site (a site removed, decision 25) still names a replica source, its own: CNPG
// refuses an empty one, and the app's whole Kustomization with it, a new vault key included.
func TestOneSiteApp(t *testing.T) {
	a := sampleApp()
	a.Spec.Sites, a.Spec.Primary, a.Spec.Archive = []string{"vince"}, "vince", map[string]int{"vince": 1}
	if r := RoleAt(a.Spec, "vince"); r.Source != "vince" || r.Primary != "vince" {
		t.Fatalf("role: %+v", r)
	}
	if s := opValue(patchesOf(t, a, "vince", Local{Created: true})["Cluster/docs-db"], "/spec/replica/source"); s != "vince" {
		t.Fatalf("replica.source %v", s)
	}
}
