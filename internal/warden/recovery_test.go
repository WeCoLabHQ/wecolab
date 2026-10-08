package warden

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
)

func TestRecoveryMeasuredBound(t *testing.T) {
	now := time.Now()
	sample := func(lsn uint64, age time.Duration, role string) RecoverySample {
		return RecoverySample{ArchiveID: "archive", SystemID: "system", Timeline: 2, Pod: "db-1", Role: role, LSNHigh: uint32(lsn >> 32), LSNLow: uint32(lsn), AgeNanos: int64(age), ObservedAt: now.Add(-age)}
	}
	cases := []struct {
		name            string
		primary, replay RecoverySample
		want            string
	}{
		{"covered write", sample(1<<53+123, 20*time.Second, "primary"), sample(1<<53+123, 5*time.Second, "replica"), "within-objective"},
		{"covered idle", sample(1<<32, 20*time.Second, "primary"), sample(1<<32, 5*time.Second, "replica"), "within-objective"},
		{"stalled replay", sample(1<<32+2, 5*time.Second, "primary"), sample(1<<32+1, 5*time.Second, "replica"), "outside-objective"},
		{"changed timeline", sample(1<<32, 20*time.Second, "primary"), sample(1<<32, 5*time.Second, "replica"), "unknown"},
		{"wrong system", sample(1<<32, 20*time.Second, "primary"), sample(1<<32, 5*time.Second, "replica"), "unknown"},
		{"invalid role", sample(1<<32, 20*time.Second, "replica"), sample(1<<32, 5*time.Second, "replica"), "unknown"},
		{"expired replay", sample(1<<32, 20*time.Second, "primary"), sample(1<<32, 90*time.Second, "replica"), "unknown"},
		{"crosses 32-bit boundary", sample(1<<32-1, 20*time.Second, "primary"), sample(1<<32, 5*time.Second, "replica"), "within-objective"},
		{"replay behind boundary", sample(1<<32, 20*time.Second, "primary"), sample(1<<32-1, 5*time.Second, "replica"), "unknown"},
	}
	cases[3].replay.Timeline = 3
	cases[4].replay.SystemID = "other"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			samples := []RecoverySample{tc.primary}
			if tc.name == "stalled replay" {
				samples = append([]RecoverySample{sample(1<<32+2, 90*time.Second, "primary")}, samples...)
			}
			got := MeasureRecovery(RecoveryReport{Samples: samples}, RecoveryReport{Samples: []RecoverySample{tc.replay}}, "archive", time.Minute, 0, 0)
			if got.State != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
	if got := MeasureRecovery(RecoveryReport{Samples: []RecoverySample{sample(^uint64(0), 0, "primary")}}, RecoveryReport{Samples: []RecoverySample{sample(^uint64(0), 0, "replica")}}, "archive", time.Minute, 0, 0); got.State != "within-objective" {
		t.Fatal(got)
	}
	p := sample(2, 0, "primary")
	r := sample(2, 0, "replica")
	for _, delay := range []time.Duration{30 * time.Second, -time.Second, 16 * time.Second} {
		if got := MeasureRecovery(RecoveryReport{Samples: []RecoverySample{p}}, RecoveryReport{Samples: []RecoverySample{r}}, "archive", time.Minute, delay, 0); delay < 0 || delay > 15*time.Second {
			if got.State != "unknown" {
				t.Fatalf("invalid delivery age %s: %+v", delay, got)
			}
		} else if got.ExposureUpperBoundSeconds == nil || *got.ExposureUpperBoundSeconds < delay.Seconds() {
			t.Fatalf("receipt delay not bounded: %+v", got)
		}
	}
	r.AgeNanos = 1<<63 - 1
	if got := MeasureRecovery(RecoveryReport{Samples: []RecoverySample{p}}, RecoveryReport{Samples: []RecoverySample{r}}, "archive", time.Minute, 0, 0); got.State != "unknown" {
		t.Fatalf("overflowed receipt age cannot certify recovery: %+v", got)
	}
}

func TestRecoveryAllowanceIsNotProofOfLoss(t *testing.T) {
	now := time.Now()
	sample := func(lsn uint32, age time.Duration, role string) RecoverySample {
		return RecoverySample{ArchiveID: "archive", SystemID: "system", Timeline: 2, Pod: "db-1", Role: role,
			LSNLow: lsn, AgeNanos: int64(age), ObservedAt: now.Add(-age)}
	}
	cases := []struct {
		name    string
		primary []RecoverySample
		want    string
	}{
		{"covered at 50s with 15s allowance", []RecoverySample{sample(1, 50*time.Second, "primary")}, "unknown"},
		{"uncovered at 35s with 15s allowance", []RecoverySample{sample(1, 50*time.Second, "primary"), sample(2, 35*time.Second, "primary")}, "unknown"},
		{"older uncovered write definitely before replay", []RecoverySample{sample(2, 90*time.Second, "primary"), sample(3, 5*time.Second, "primary")}, "outside-objective"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MeasureRecovery(RecoveryReport{Samples: tc.primary},
				RecoveryReport{Samples: []RecoverySample{sample(1, 5*time.Second, "replica")}},
				"archive", time.Minute, 0, 0)
			if got.State != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
	// The producer's oldest write may be older than the objective yet newer
	// than the replica observation: independent clocks give no replay proof.
	got := MeasureRecovery(RecoveryReport{Samples: []RecoverySample{sample(2, 65*time.Second, "primary")}},
		RecoveryReport{Samples: []RecoverySample{sample(1, 55*time.Second, "replica")}},
		"archive", time.Minute, 0, 0)
	if got.State != "unknown" {
		t.Fatalf("unordered cross-site samples: %+v", got)
	}
	primary := RecoveryReport{Samples: []RecoverySample{sample(2, 55*time.Second, "primary"), sample(3, 0, "primary")}}
	replica := RecoveryReport{Samples: []RecoverySample{sample(1, 0, "replica")}}
	if got := MeasureRecovery(primary, replica, "archive", time.Minute, 0, 6*time.Second); got.State != "unknown" {
		t.Fatalf("replica receipt delay cannot age primary writes: %+v", got)
	}
	if got := MeasureRecovery(primary, replica, "archive", time.Minute, 6*time.Second, 0); got.State != "outside-objective" {
		t.Fatalf("primary receipt delay contributes to its own lower bound: %+v", got)
	}
}

func TestRecoverySampleRing(t *testing.T) {
	var ring RecoveryReport
	for i := range 80 {
		ring.add(RecoverySample{ArchiveID: "a", SystemID: "s", Timeline: 1, Pod: "db-1", Role: "primary", LSNLow: uint32(i)}, time.Now())
	}
	if len(ring.Samples) != 64 || ring.Samples[0].LSNLow != 16 || ring.Samples[63].LSNLow != 79 {
		t.Fatalf("ring: %d %v", len(ring.Samples), ring.Samples)
	}
	ring.add(RecoverySample{ArchiveID: "a", SystemID: "s", Timeline: 2, Pod: "db-1", Role: "primary", LSNLow: 1}, time.Now())
	if len(ring.Samples) != 1 {
		t.Fatalf("timeline changed: %+v", ring.Samples)
	}
	ring.add(RecoverySample{ArchiveID: "a", SystemID: "s", Timeline: 2, Pod: "db-2", Role: "primary"}, time.Now())
	if len(ring.Samples) != 1 {
		t.Fatalf("primary changed: %+v", ring.Samples)
	}
	ring.add(RecoverySample{ArchiveID: "a", SystemID: "s", Timeline: 2, Pod: "db-2", Role: "primary", LSNLow: 1}, time.Now())
	ring.add(RecoverySample{ArchiveID: "a", SystemID: "s", Timeline: 2, Pod: "db-2", Role: "primary", LSNLow: 0}, time.Now())
	if ring.Error != recoveryRegression || len(ring.Samples) != 2 {
		t.Fatalf("same-history WAL regression must fail closed: %+v", ring)
	}
	ring.add(RecoverySample{ArchiveID: "a", SystemID: "s", Timeline: 2, Pod: "db-2", Role: "primary", LSNLow: 2}, time.Now())
	if ring.Error != recoveryRegression {
		t.Fatal("later samples cannot silently clear a same-history regression")
	}
	ring.add(RecoverySample{ArchiveID: "a", SystemID: "s", Timeline: 3, Pod: "db-2", Role: "primary", LSNLow: 1}, time.Now())
	if ring.Error != "" || len(ring.Samples) != 1 {
		t.Fatal("new timeline starts a fresh history")
	}
}

func TestParseRecoveryMetrics(t *testing.T) {
	text := "# HELP unrelated x\ncnpg_wecolab_recovery_recovering{system_identifier=\"9223372036854775808\",timeline_id=\"4\"} 0\ncnpg_wecolab_recovery_lsn_high{system_identifier=\"9223372036854775808\",timeline_id=\"4\"} 2097152\ncnpg_wecolab_recovery_lsn_low{system_identifier=\"9223372036854775808\",timeline_id=\"4\"} 4.294967295e+09\n"
	s, err := parseRecoveryMetrics([]byte(text))
	if err != nil || s.SystemID != "9223372036854775808" || s.Timeline != 4 || s.LSNHigh != 2097152 || s.LSNLow != ^uint32(0) || s.Role != "primary" {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{"", text + text, "cnpg_wecolab_recovery_lsn_high{system_identifier=\"x\",timeline_id=\"1\"} 9007199254740992", "cnpg_wecolab_recovery_recovering{system_identifier=\"x\",timeline_id=\"1\"} NaN", strings.Replace(text, "4.294967295e+09", "4.25", 1)} {
		if _, err := parseRecoveryMetrics([]byte(bad)); err == nil {
			t.Errorf("accepted invalid metric %q", fmt.Sprintf("%.80s", bad))
		}
	}
}

func TestRecoveryProtectionScopes(t *testing.T) {
	now := time.Now()
	app := sampleApp()
	app.Spec.ArchiveID = "archive"
	app.Spec.RPO = metav1.Duration{Duration: time.Minute}
	s := RecoverySample{ArchiveID: "archive", SystemID: "system", Timeline: 2, Pod: "docs-db-1", Role: "primary", LSNHigh: 1, LSNLow: 3, ObservedAt: now}
	backup := now.Add(-time.Hour)
	evidence := &BackupEvidence{Archive: ArchiveName(app, "vince"), ID: "20261006T090000", SystemID: "system", Timeline: 2, BeginWAL: "0/1", EndWAL: "0/2", CompletedAt: backup, ObservedAt: now.Add(-time.Minute)}
	source := &SiteStatus{Site: "vince", ReceivedAt: now, Apps: map[string]AppState{"vince/docs": {Recovery: &RecoveryReport{Samples: []RecoverySample{s}}, Vault: &VaultStatus{LatestBackup: backup, BackupEvidence: evidence, ObjectLock: "compliance 30 days"}}}}
	replay := s
	replay.Role = "replica"
	replay.Pod = "docs-db-2"
	target := &SiteStatus{Site: "friend", ReceivedAt: now, Apps: map[string]AppState{"vince/docs": {Recovery: &RecoveryReport{Samples: []RecoverySample{replay}}}}}
	sites := map[string]*SiteStatus{"vince": source, "friend": target}
	p := ProtectionOf(app, sites, now, 24*time.Hour, true, true)
	if p.Database.State != "protected" || p.Files.State != "local-only" || p.RecoveryPoint.State != "within-objective" || p.LastVerifiedRestore != nil {
		t.Fatalf("scopes: %+v", p)
	}
	source.ReceivedAt = now.Add(-recoveryDeliveryAllowance - time.Second)
	if p = ProtectionOf(app, sites, now, 24*time.Hour, true, true); p.Database.State != "unknown" || p.RecoveryPoint.State != "unknown" || p.LastVerifiedRestore != nil {
		t.Fatalf("expired primary receipt cannot certify scopes: %+v", p)
	}
	source.ReceivedAt = now
	target.ReceivedAt = now.Add(-recoveryDeliveryAllowance - time.Second)
	if p = ProtectionOf(app, sites, now, 24*time.Hour, true, true); p.RecoveryPoint.State != "unknown" {
		t.Fatalf("expired replay receipt cannot certify recovery: %+v", p)
	}
	target.ReceivedAt = now
	target.ReceivedAt = time.Time{}
	if p = ProtectionOf(app, sites, now, 24*time.Hour, true, true); p.RecoveryPoint.State != "unknown" {
		t.Fatalf("missing receipt cannot certify recovery: %+v", p)
	}
	target.ReceivedAt = now
	target.Apps["vince/docs"] = AppState{Archive: "obsolete-generation", Recovery: &RecoveryReport{Samples: []RecoverySample{replay}}}
	if p = ProtectionOf(app, sites, now, 24*time.Hour, true, true); p.RecoveryPoint.State != "unknown" {
		t.Fatal("replay from obsolete archive must not certify objective")
	}
	target.Apps["vince/docs"] = AppState{Archive: ArchiveName(app, "friend"), Recovery: &RecoveryReport{Samples: []RecoverySample{replay}}}
	app.Spec.RestoreVerification = &v1alpha1.RestoreVerification{ArchiveID: "archive", SystemID: "system", Timeline: 2, BackupID: evidence.ID, Actor: "operator", EvidenceSHA256: strings.Repeat("a", 64), Scope: "database", CompletedAt: metav1.NewTime(now.Add(-time.Minute))}
	p = ProtectionOf(app, sites, now, 24*time.Hour, true, true)
	if p.LastVerifiedRestore == nil {
		t.Fatal("matching completed restore record must show separately")
	}
	app.Spec.RestoreVerification.Timeline = 1
	if p = ProtectionOf(app, sites, now, 24*time.Hour, true, true); p.LastVerifiedRestore != nil {
		t.Fatal("restore on another timeline cannot certify this history")
	}
	evidence.SystemID = "old"
	if p = ProtectionOf(app, sites, now, 24*time.Hour, true, true); p.Database.State != "unknown" {
		t.Fatal("wrong backup system identity must be unknown")
	}
	evidence.SystemID = "system"
	evidence.ObservedAt = now.Add(-10 * time.Minute)
	if p = ProtectionOf(app, sites, now, 24*time.Hour, true, true); p.Database.State != "unknown" {
		t.Fatal("old inspection cannot prove current vault")
	}
	app.Spec.Database = ""
	if p = ProtectionOf(app, sites, now, 24*time.Hour, true, false); p.Database.State != "not-applicable" || p.Files.State != "not-applicable" || p.RecoveryPoint.State != "not-applicable" {
		t.Fatal(p)
	}
	if p = ProtectionOf(app, sites, now, 24*time.Hour, false, false); p.Files.State != "unknown" {
		t.Fatal(p)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	database, ok := payload["Database"].(map[string]any)
	if !ok {
		t.Fatalf("missing Database object: %s", encoded)
	}
	if value, exists := database["ObservedAt"]; !exists || value != nil {
		t.Fatalf("unknown timestamp must be explicit null: %s", encoded)
	}
}
