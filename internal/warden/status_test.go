package warden

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"wecolab.io/wecolab/api/v1alpha1"
)

func healthyDB(primary bool, backupAge time.Duration, now time.Time) map[string]any {
	m := map[string]any{
		"phase":          cnpgHealthy,
		"readyInstances": int64(1),
		"conditions":     []any{map[string]any{"type": "ContinuousArchiving", "status": "True"}},
	}
	if primary {
		m["currentPrimary"] = "docs-db-1"
		m["systemIdentifier"] = "system"
		m["timelineID"] = uint32(1)
		m["lastSuccessfulBackup"] = now.Add(-backupAge).Format(time.RFC3339)
	}
	return m
}

func status(conds []metav1.Condition, typ string) metav1.ConditionStatus {
	for _, c := range conds {
		if c.Type == typ {
			return c.Status
		}
	}
	return "missing"
}

func TestComputeHealthyButUnmeasuredRecovery(t *testing.T) {
	now := time.Now()
	app := sampleApp()
	in := Inputs{Now: now, MaxBackupAge: 24 * time.Hour, RPO: 5 * time.Minute,
		ClusterReady: map[string]bool{"vince": true, "friend": true},
		DB:           map[string]map[string]any{"vince": healthyDB(true, time.Hour, now), "friend": healthyDB(false, 0, now)},
		Vault:        &VaultStatus{LatestBackup: now.Add(-time.Hour), BackupEvidence: &BackupEvidence{Archive: ArchiveName(app, "vince"), ID: "backup", SystemID: "system", Timeline: 1, BeginWAL: "0/1", EndWAL: "0/2", CompletedAt: now.Add(-time.Hour), ObservedAt: now}, LatestWAL: now.Add(-40 * time.Second), ObjectLock: "compliance 30 days"}}
	conds := Compute(app, "vince", in)
	for _, typ := range []string{v1alpha1.CondPrimaryHealthy, v1alpha1.CondStandbyStaged, v1alpha1.CondVaultFresh, v1alpha1.CondRouted} {
		if status(conds, typ) != metav1.ConditionTrue {
			t.Errorf("%s should be True", typ)
		}
	}
	if status(conds, v1alpha1.CondWithinRPO) != metav1.ConditionUnknown {
		t.Fatal("healthy archive and WAL freshness cannot prove replay position")
	}
	if r := Ready(conds); r.Status != metav1.ConditionTrue {
		t.Errorf("operational availability must be separate from recovery: %s (%s)", r.Status, r.Message)
	}
}

func TestComputeGates(t *testing.T) {
	now := time.Now()
	app := sampleApp()
	base := func() Inputs {
		return Inputs{Now: now, MaxBackupAge: 24 * time.Hour, RPO: 5 * time.Minute,
			ClusterReady: map[string]bool{"vince": true, "friend": true},
			DB:           map[string]map[string]any{"vince": healthyDB(true, time.Hour, now), "friend": healthyDB(false, 0, now)},
			Vault:        &VaultStatus{LatestBackup: now.Add(-time.Hour), BackupEvidence: &BackupEvidence{Archive: ArchiveName(app, "vince"), ID: "backup", SystemID: "system", Timeline: 1, BeginWAL: "0/1", EndWAL: "0/2", CompletedAt: now.Add(-time.Hour), ObservedAt: now}, LatestWAL: now.Add(-40 * time.Second), ObjectLock: "compliance 30 days"}}
	}
	cases := []struct {
		name   string
		mutate func(*Inputs)
		fails  string
	}{
		{"standby site down", func(i *Inputs) { i.ClusterReady["friend"] = false }, v1alpha1.CondStandbyStaged},
		{"replica unhealthy", func(i *Inputs) { i.DB["friend"]["phase"] = "Setting up primary" }, v1alpha1.CondWithinRPO},
		{"archive stalled", func(i *Inputs) { i.DB["vince"]["conditions"] = []any{} }, v1alpha1.CondWithinRPO},
		{"backup stale", func(i *Inputs) {
			i.Vault.LatestBackup = now.Add(-48 * time.Hour)
			i.Vault.BackupEvidence.CompletedAt = i.Vault.LatestBackup
		}, v1alpha1.CondVaultFresh},
		{"no object lock", func(i *Inputs) { i.Vault.ObjectLock = "" }, v1alpha1.CondVaultFresh},
		{"vault unreachable", func(i *Inputs) { i.Vault.Err = "401" }, v1alpha1.CondVaultFresh},
		{"primary site down", func(i *Inputs) { i.ClusterReady["vince"] = false }, v1alpha1.CondPrimaryHealthy},
		{"dns mismatch", func(i *Inputs) { f := false; i.Routed = &f }, v1alpha1.CondRouted},
	}
	for _, c := range cases {
		in := base()
		c.mutate(&in)
		conds := Compute(app, "vince", in)
		if status(conds, c.fails) != metav1.ConditionFalse {
			t.Errorf("%s: %s should be False", c.name, c.fails)
		}
		wantReady := metav1.ConditionTrue
		if c.fails == v1alpha1.CondPrimaryHealthy || c.fails == v1alpha1.CondRouted {
			wantReady = metav1.ConditionFalse
		}
		if Ready(conds).Status != wantReady {
			t.Errorf("%s: operational Ready should be %s", c.name, wantReady)
		}
	}
}

func TestComputeRecoveryEvidenceUnknown(t *testing.T) {
	now := time.Now()
	app := sampleApp()
	app.Spec.ArchiveID = "archive"
	db := healthyDB(true, time.Hour, now)
	in := Inputs{Now: now, MaxBackupAge: 24 * time.Hour, RPO: time.Minute,
		ClusterReady: map[string]bool{"vince": true, "friend": true},
		DB:           map[string]map[string]any{"vince": db, "friend": healthyDB(false, 0, now)},
		Vault:        &VaultStatus{LatestBackup: now.Add(-time.Hour), LatestWAL: now, ObjectLock: "compliance 30 days"}}
	if got := status(Compute(app, "vince", in), v1alpha1.CondVaultFresh); got != metav1.ConditionUnknown {
		t.Fatalf("timestamp alone is not a validated backup: %s", got)
	}
	in.Vault.BackupEvidence = &BackupEvidence{Archive: ArchiveName(app, "vince"), ID: "backup", SystemID: "system", Timeline: 2, BeginWAL: "0/1", EndWAL: "0/2", CompletedAt: in.Vault.LatestBackup, ObservedAt: now}
	if got := status(Compute(app, "vince", in), v1alpha1.CondVaultFresh); got != metav1.ConditionUnknown {
		t.Fatalf("backup from a different timeline is not current-history: %s", got)
	}
}

// The standby gates are judged for the site a move goes to, not the first site that is not the primary.
func TestGatesForTheMovesTarget(t *testing.T) {
	now := time.Now()
	app := sampleApp()
	app.Spec.Sites = []string{"vince", "friend", "third"}
	app.Spec.Primary, app.Spec.Handover = "third", &v1alpha1.Handover{ID: "m", From: "vince"}
	rep := func(site string, db map[string]any) *SiteStatus {
		return &SiteStatus{Site: site, DB: map[string]map[string]any{"vince/docs-db": db},
			Apps: map[string]AppState{"vince/docs": {Active: "vince", Archive: ArchiveName(app, site)}}}
	}
	sites := map[string]*SiteStatus{"vince": rep("vince", healthyDB(true, time.Hour, now)), "friend": rep("friend", healthyDB(false, 0, now)), "third": rep("third", map[string]any{"phase": "Setting up primary"})}
	active, conds := AppView(app, sites, now, 24*time.Hour)
	if active != "vince" {
		t.Fatalf("the history is at the old primary until its token is in Git: %s", active)
	}
	if status(conds, v1alpha1.CondStandbyStaged) != metav1.ConditionFalse || status(conds, v1alpha1.CondWithinRPO) != metav1.ConditionFalse {
		t.Fatalf("the target's replica is not ready, whatever friend's is: %+v", conds)
	}
	sites["third"] = rep("third", healthyDB(false, 0, now))
	sites["third"].Apps["vince/docs"] = AppState{Archive: "docs-db-third-old"}
	if _, conds = AppView(app, sites, now, 24*time.Hour); status(conds, v1alpha1.CondStandbyStaged) != metav1.ConditionFalse {
		t.Fatalf("a target waiting to be rebuilt is not staged: %+v", conds)
	}
	sites["third"].Apps["vince/docs"] = AppState{Archive: ArchiveName(app, "third")}
	if _, conds = AppView(app, sites, now, 24*time.Hour); status(conds, v1alpha1.CondStandbyStaged) != metav1.ConditionTrue || status(conds, v1alpha1.CondWithinRPO) != metav1.ConditionUnknown {
		t.Fatalf("staged target without measured replay remains unknown: %+v", conds)
	}
	if status(conds, v1alpha1.CondPromotion) != metav1.ConditionFalse {
		t.Fatal("a move in flight shows in Promotion")
	}
	// Why the site holding the history holds back, in its own words.
	app.Spec.Handover = nil
	app.Spec.Primary = "vince"
	missing := cond(v1alpha1.CondPromotion, false, "DatabaseMissing", "gone")
	sites["vince"].Apps["vince/docs"] = AppState{Promotion: &missing}
	if _, conds = AppView(app, sites, now, 24*time.Hour); conds[len(conds)-1].Reason != "DatabaseMissing" {
		t.Fatalf("%+v", conds[len(conds)-1])
	}
}

// A planned move is judged for the site it goes to, before it is made (the Console asks MoveGates).
func TestMoveGates(t *testing.T) {
	now := time.Now()
	app := sampleApp()
	app.Spec.Sites = []string{"vince", "friend", "third"}
	app.Spec.ArchiveID = "archive"
	app.Spec.RPO = metav1.Duration{Duration: 5 * time.Minute}
	rep := func(site string, db map[string]any) *SiteStatus {
		role := "replica"
		if site == "vince" {
			role = "primary"
		}
		sample := RecoverySample{ArchiveID: "archive", SystemID: "system", Timeline: 1, Pod: "docs-db-1", Role: role, LSNLow: 42, ObservedAt: now}
		return &SiteStatus{Site: site, ReceivedAt: now, DB: map[string]map[string]any{"vince/docs-db": db},
			Apps: map[string]AppState{"vince/docs": {Archive: ArchiveName(app, site), Recovery: &RecoveryReport{Samples: []RecoverySample{sample}}}}}
	}
	primary := func() *SiteStatus {
		db := healthyDB(true, time.Hour, now)
		db["instancesReportedState"] = map[string]any{"docs-db-1": map[string]any{"isPrimary": true}}
		return rep("vince", db)
	}
	sites := map[string]*SiteStatus{"vince": primary(), "friend": rep("friend", healthyDB(false, 0, now)), "third": rep("third", map[string]any{"phase": "Setting up primary"})}
	saved := sites["friend"].Apps["vince/docs"]
	saved.Recovery = nil
	sites["friend"].Apps["vince/docs"] = saved
	if err := MoveGates(app, sites, "friend", now); err == nil {
		t.Fatal("unmeasured replica must not pass move gates")
	}
	sites["friend"] = rep("friend", healthyDB(false, 0, now))
	if err := MoveGates(app, sites, "friend", now); err != nil {
		t.Fatalf("measured replay should pass: %v", err)
	}
	sites["friend"].ReceivedAt = now.Add(-recoveryDeliveryAllowance - time.Second)
	if err := MoveGates(app, sites, "friend", now); err == nil {
		t.Fatal("expired replica receipt must block a move")
	}
	sites["friend"] = rep("friend", healthyDB(false, 0, now))
	sites["vince"].ReceivedAt = now.Add(-recoveryDeliveryAllowance - time.Second)
	if err := MoveGates(app, sites, "friend", now); err == nil {
		t.Fatal("expired primary receipt must block a move")
	}
	sites["vince"] = primary()
	for name, change := range map[string]func(){
		"the target's replica is not ready":   func() { sites["friend"] = rep("friend", map[string]any{"phase": "Setting up primary"}) },
		"the target waits to be rebuilt":      func() { sites["friend"].Apps["vince/docs"] = AppState{Archive: "docs-db-friend-old"} },
		"the target does not answer":          func() { delete(sites, "friend") },
		"the primary does not archive":        func() { sites["vince"].DB["vince/docs-db"]["conditions"] = []any{} },
		"the primary is not writable":         func() { sites["vince"].DB["vince/docs-db"]["instancesReportedState"] = map[string]any{} },
		"the primary does not answer":         func() { delete(sites, "vince") },
		"the target's replica is not healthy": func() { sites["friend"].DB["vince/docs-db"]["phase"] = "Setting up primary" },
	} {
		change()
		if MoveGates(app, sites, "friend", now) == nil {
			t.Errorf("%s: the move goes ahead", name)
		}
		sites["vince"], sites["friend"] = primary(), rep("friend", healthyDB(false, 0, now))
	}
	// In flight, every site replays the old primary's archive: a retarget needs only its target staged,
	// though the old target is gone, and a cancel nothing.
	app.Spec.Primary, app.Spec.Handover = "third", &v1alpha1.Handover{ID: "m", From: "vince", Token: "tok"}
	delete(sites, "third")
	if err := MoveGates(app, sites, "friend", now); err != nil {
		t.Fatalf("retarget: %v", err)
	}
	if MoveGates(app, sites, "third", now) == nil {
		t.Fatal("retarget to a site that does not answer")
	}
	app.Spec.Handover.Token = ""
	delete(sites, "vince")
	if err := MoveGates(app, sites, "vince", now); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestSetConditionKeepsTransitionTime(t *testing.T) {
	var list []metav1.Condition
	t0 := time.Now().Add(-time.Hour)
	SetCondition(&list, cond("X", true, "a", "m1"), t0, 1)
	SetCondition(&list, cond("X", true, "a", "m2"), time.Now(), 2)
	if !list[0].LastTransitionTime.Time.Equal(t0) || list[0].Message != "m2" || list[0].ObservedGeneration != 2 {
		t.Error("unchanged status must keep its transition time but update message and generation")
	}
	SetCondition(&list, cond("X", false, "b", "m3"), time.Now(), 3)
	if list[0].LastTransitionTime.Time.Equal(t0) || len(list) != 1 {
		t.Error("status change must bump transition time in place")
	}
}
