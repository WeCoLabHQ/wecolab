package warden

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wecolab.io/wecolab/api/v1alpha1"
)

// RecoveryQuery is the documented CNPG 1.30.1 customQueriesConfigMap data
// entry ("queries"), using its standard cnpg_metrics_exporter pg_monitor role.
const RecoveryQuery = `wecolab_recovery:
  query: |
    WITH position AS (
      SELECT pg_catalog.pg_is_in_recovery() AS recovering,
             CASE WHEN pg_catalog.pg_is_in_recovery()
                  THEN pg_catalog.pg_last_wal_replay_lsn()
                  ELSE pg_catalog.pg_current_wal_lsn() END AS lsn
    ), checkpoint AS (
      SELECT timeline_id FROM pg_catalog.pg_control_checkpoint()
    ), identity AS (
      SELECT system_identifier FROM pg_catalog.pg_control_system()
    )
    SELECT identity.system_identifier::text AS system_identifier,
           checkpoint.timeline_id::text AS timeline_id,
           CASE WHEN position.recovering THEN 1 ELSE 0 END AS recovering,
           floor(pg_catalog.pg_wal_lsn_diff(position.lsn, '0/0') / 4294967296)::bigint AS lsn_high,
           mod(pg_catalog.pg_wal_lsn_diff(position.lsn, '0/0'), 4294967296)::bigint AS lsn_low
    FROM position, checkpoint, identity
  metrics:
    - system_identifier:
        usage: LABEL
        description: PostgreSQL system identifier
    - timeline_id:
        usage: LABEL
        description: WAL checkpoint timeline
    - recovering:
        usage: GAUGE
        description: One if the instance is in recovery
    - lsn_high:
        usage: GAUGE
        description: WAL position high 32 bits
    - lsn_low:
        usage: GAUGE
        description: WAL position low 32 bits
`

// RecoverySample is one read-only CNPG exporter observation. AgeNanos is measured
// using the producer's monotonic clock when the site report is produced; wall time
// is for display only and is never used to compare clocks between sites.
type RecoverySample struct {
	ArchiveID  string    `json:"archiveID"`
	SystemID   string    `json:"systemID"`
	Timeline   uint32    `json:"timeline"`
	Pod        string    `json:"pod"`
	Role       string    `json:"role"`
	LSNHigh    uint32    `json:"lsnHigh"`
	LSNLow     uint32    `json:"lsnLow"`
	ObservedAt time.Time `json:"observedAt"`
	AgeNanos   int64     `json:"ageNanos"`
}

type RecoveryReport struct {
	Samples []RecoverySample `json:"samples,omitempty"`
	Error   string           `json:"error,omitempty"`
}

const (
	recoverySamples      = 64
	recoveryInterval     = 15 * time.Second
	recoveryMaxReplayAge = 45 * time.Second
	// Peer cache is five seconds, /status cache is five seconds and peer HTTP
	// timeout is three seconds. Include a further two seconds of scheduling slack.
	recoveryDeliveryAllowance = 15 * time.Second
	recoveryRegression        = "WAL position regressed without a new database history"
)

func (r *RecoveryReport) add(s RecoverySample, at time.Time) {
	if n := len(r.Samples); n > 0 {
		old := r.Samples[n-1]
		if old.ArchiveID != s.ArchiveID || old.SystemID != s.SystemID || old.Timeline != s.Timeline || old.Pod != s.Pod || old.Role != s.Role || at.Before(old.ObservedAt) {
			r.Samples = nil
		} else {
			if old.LSNHigh > s.LSNHigh || old.LSNHigh == s.LSNHigh && old.LSNLow > s.LSNLow {
				r.Error = recoveryRegression
			}
			if r.Error == recoveryRegression {
				return
			}
		}
	}
	if len(r.Samples) == recoverySamples {
		copy(r.Samples, r.Samples[1:])
		r.Samples = r.Samples[:recoverySamples-1]
	}
	r.Samples = append(r.Samples, s)
	r.Error = ""
}

// RecoveryPoint is a measured upper bound on exposure, not a guarantee of restore.
type RecoveryPoint struct {
	State                     string     `json:"State"`
	ObservedAt                *time.Time `json:"ObservedAt"`
	ExposureUpperBoundSeconds *float64   `json:"ExposureUpperBoundSeconds"`
	Reason                    string     `json:"Reason"`
}

// MeasureRecovery accepts only samples of the same PostgreSQL history and role.
// Each delivery age is elapsed local monotonic time since receiving its site's
// report. Callers that cannot bound both receipts must not claim an objective.
func MeasureRecovery(primary, replica RecoveryReport, archiveID string, objective, primaryDeliveryAge, replicaDeliveryAge time.Duration) RecoveryPoint {
	unknown := func(reason string) RecoveryPoint {
		return RecoveryPoint{State: "unknown", Reason: reason}
	}
	if archiveID == "" || objective <= 0 || primaryDeliveryAge < 0 || primaryDeliveryAge > recoveryDeliveryAllowance ||
		replicaDeliveryAge < 0 || replicaDeliveryAge > recoveryDeliveryAllowance {
		return unknown("No bounded current-history observation")
	}
	if primary.Error != "" || replica.Error != "" {
		return unknown("Exporter observation failed: " + primary.Error + " " + replica.Error)
	}
	if len(primary.Samples) == 0 || len(replica.Samples) == 0 {
		return unknown("No comparable primary and replay samples")
	}
	if len(primary.Samples) > recoverySamples || len(replica.Samples) > recoverySamples {
		return unknown("Recovery sample history exceeds bound")
	}
	latest := primary.Samples[len(primary.Samples)-1]
	if age := sampleAge(latest, primaryDeliveryAge); !validRecoverySample(latest, archiveID, "primary") || age < 0 || age > recoveryMaxReplayAge {
		return unknown("Primary observation is invalid or expired")
	}
	target := replica.Samples[len(replica.Samples)-1]
	if age := sampleAge(target, replicaDeliveryAge); !validRecoverySample(target, archiveID, "replica") || age < 0 || age > recoveryMaxReplayAge {
		return unknown("Replica observation is invalid or expired")
	}
	var covered, oldestUncovered *RecoverySample
	for i := range primary.Samples {
		p := &primary.Samples[i]
		if i > 0 {
			previous := primary.Samples[i-1]
			if previous.AgeNanos < p.AgeNanos ||
				previous.LSNHigh > p.LSNHigh ||
				previous.LSNHigh == p.LSNHigh && previous.LSNLow > p.LSNLow {
				return unknown("Primary sample order or WAL position is inconsistent")
			}
		}
		if !validRecoverySample(*p, archiveID, "primary") || p.SystemID != target.SystemID || p.Timeline != target.Timeline {
			return unknown("Primary and replica history or role differ")
		}
		if sampleAge(*p, primaryDeliveryAge) < 0 {
			return unknown("Invalid primary observation age")
		}
		if p.LSNHigh < target.LSNHigh || p.LSNHigh == target.LSNHigh && p.LSNLow <= target.LSNLow {
			covered = p
		} else if oldestUncovered == nil {
			oldestUncovered = p
		}
	}
	if covered != nil {
		age := sampleAge(*covered, primaryDeliveryAge)
		if age <= objective {
			seconds, observed := age.Seconds(), covered.ObservedAt
			return RecoveryPoint{State: "within-objective", ObservedAt: &observed, ExposureUpperBoundSeconds: &seconds,
				Reason: "Target replay covers a same-history primary write position"}
		}
	}
	// An upper bound beyond the objective says nothing about actual loss. A
	// definite outside result requires an uncovered write whose *lower* age
	// exceeds the objective and whose latest possible occurrence precedes
	// the replica sample's earliest possible occurrence. Neither producer
	// clock is compared.
	if oldestUncovered != nil {
		lower := sampleAgeLower(*oldestUncovered, primaryDeliveryAge)
		replicaUpper := sampleAge(target, replicaDeliveryAge)
		if lower > objective && lower > replicaUpper {
			return RecoveryPoint{State: "outside-objective", Reason: "Fresh replay is behind a primary write position older than the objective"}
		}
	}
	return unknown("No bounded proof that target replay meets or exceeds the objective")
}

func sampleAgeLower(s RecoverySample, delivery time.Duration) time.Duration {
	if s.AgeNanos < 0 || delivery < 0 || time.Duration(s.AgeNanos) > time.Duration(1<<63-1)-delivery {
		return -1
	}
	return time.Duration(s.AgeNanos) + delivery
}

func validRecoverySample(s RecoverySample, archiveID, role string) bool {
	return s.ArchiveID == archiveID && s.SystemID != "" && s.Timeline != 0 && s.Pod != "" && s.Role == role && s.AgeNanos >= 0 && !s.ObservedAt.IsZero()
}
func sampleAge(s RecoverySample, delivery time.Duration) time.Duration {
	if s.AgeNanos < 0 || delivery < 0 || time.Duration(s.AgeNanos) > time.Duration(1<<63-1)-delivery-recoveryDeliveryAllowance {
		return -1
	}
	return time.Duration(s.AgeNanos) + delivery + recoveryDeliveryAllowance
}

// parseRecoveryMetrics requires one complete exporter query row. Prometheus
// labels carry exact identifiers, while each 32-bit gauge is an exact integer.
func parseRecoveryMetrics(body []byte) (RecoverySample, error) {
	const prefix = "cnpg_wecolab_recovery_"
	var sample RecoverySample
	var seen uint8
	labels := ""
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		opening := strings.IndexByte(line, '{')
		closing := strings.IndexByte(line, '}')
		if opening < 0 || closing <= opening || closing+2 >= len(line) || line[closing+1] != ' ' {
			return sample, errors.New("invalid recovery metric")
		}
		name := line[len(prefix):opening]
		var bit uint8
		switch name {
		case "recovering":
			bit = 1
		case "lsn_high":
			bit = 2
		case "lsn_low":
			bit = 4
		default:
			continue
		}
		if seen&bit != 0 {
			return sample, errors.New("duplicate recovery metric")
		}
		seen |= bit
		if labels != "" && labels != line[opening:closing+1] {
			return sample, errors.New("mismatched metric identity")
		}
		labels = line[opening : closing+1]
		number := strings.TrimSpace(line[closing+2:])
		gauge, err := strconv.ParseFloat(number, 64)
		if err != nil || math.IsNaN(gauge) || math.IsInf(gauge, 0) || gauge < 0 || gauge > float64(^uint32(0)) || math.Trunc(gauge) != gauge {
			return sample, fmt.Errorf("invalid 32-bit recovery gauge %q", number)
		}
		value := uint32(gauge)
		switch bit {
		case 1:
			if value > 1 {
				return sample, errors.New("invalid recovery role")
			}
			if value == 1 {
				sample.Role = "replica"
			} else {
				sample.Role = "primary"
			}
		case 2:
			sample.LSNHigh = uint32(value)
		case 4:
			sample.LSNLow = uint32(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return sample, err
	}
	if seen != 7 {
		return sample, errors.New("missing recovery metric")
	}
	for _, part := range strings.Split(strings.Trim(labels, "{}"), ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return sample, errors.New("invalid metric labels")
		}
		s, err := strconv.Unquote(value)
		if err != nil {
			return sample, err
		}
		switch key {
		case "system_identifier":
			sample.SystemID = s
		case "timeline_id":
			n, err := strconv.ParseUint(s, 10, 32)
			if err != nil {
				return sample, err
			}
			sample.Timeline = uint32(n)
		}
	}
	if sample.SystemID == "" || sample.Timeline == 0 {
		return sample, errors.New("missing recovery identity")
	}
	return sample, nil
}

// scrapeRecovery reads only the metrics port of an operator-owned Pod IP. No
// redirects, ambient proxy, external hostnames or credentials are involved.
func scrapeRecovery(ctx context.Context, ip string) (RecoverySample, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return RecoverySample{}, errors.New("CNPG pod has no valid IP")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(parsed.String(), "9187"))
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(parsed.String(), "9187")+"/metrics", nil)
	if err != nil {
		return RecoverySample{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return RecoverySample{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return RecoverySample{}, fmt.Errorf("CNPG exporter returned %s", res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20+1))
	if err != nil {
		return RecoverySample{}, err
	}
	if len(b) > 2<<20 {
		return RecoverySample{}, errors.New("CNPG exporter response exceeds limit")
	}
	return parseRecoveryMetrics(b)
}

// Protection presents database, local files, measured recovery and genuine
// restore evidence independently. filesKnown is false if desired manifests
// could not be inspected; absence of a running PVC is not proof of no files.
type ProtectionScope struct {
	State             string     `json:"State"`
	ObservedAt        *time.Time `json:"ObservedAt"`
	BackupCompletedAt *time.Time `json:"BackupCompletedAt"`
	Reason            string     `json:"Reason"`
}
type FileProtection struct {
	State  string `json:"State"`
	Reason string `json:"Reason"`
}
type Protection struct {
	Database                 ProtectionScope `json:"Database"`
	Files                    FileProtection  `json:"Files"`
	RecoveryPoint            RecoveryPoint   `json:"RecoveryPoint"`
	LastVerifiedRestore      *time.Time      `json:"LastVerifiedRestore"`
	LastVerifiedRestoreScope string          `json:"LastVerifiedRestoreScope,omitempty"`
}

func ProtectionOf(app *v1alpha1.App, sites map[string]*SiteStatus, now time.Time, maxBackupAge time.Duration, filesKnown, hasPersistentFiles bool) Protection {
	p := Protection{
		Database:      ProtectionScope{State: "unknown", Reason: "No validated current-history backup observation"},
		Files:         FileProtection{State: "unknown", Reason: "Desired file manifests unavailable"},
		RecoveryPoint: RecoveryPoint{State: "unknown", Reason: "No fresh comparable primary and replay samples"},
	}
	if filesKnown {
		if hasPersistentFiles {
			p.Files = FileProtection{State: "local-only", Reason: "Persistent volumes do not replicate or move between sites"}
		} else {
			p.Files = FileProtection{State: "not-applicable", Reason: "No persistent file mounts in desired workload"}
		}
	}
	if app.Spec.Database == "" {
		p.Database = ProtectionScope{State: "not-applicable", Reason: "No database"}
		p.RecoveryPoint = RecoveryPoint{State: "not-applicable", Reason: "No database"}
		return p
	}
	active, in, states := observe(app, sites, now, maxBackupAge)
	if active == "" {
		return p
	}
	source := states[active].Recovery
	var current *RecoverySample
	if source != nil && source.Error == "" && len(source.Samples) > 0 {
		s := &source.Samples[len(source.Samples)-1]
		if age := sampleAge(*s, receiptAge(in.ReceiptAge, active)); validRecoverySample(*s, app.Spec.ArchiveID, "primary") &&
			age >= 0 && age <= recoveryMaxReplayAge {
			current = s
		}
	}
	if v := in.Vault; v != nil && v.Err == "" && v.BackupEvidence != nil {
		e := v.BackupEvidence
		if current != nil && e.Archive == ArchiveName(app, active) && e.SystemID == current.SystemID &&
			e.Timeline == current.Timeline && e.CompletedAt.Equal(v.LatestBackup) &&
			!e.ObservedAt.After(now) && e.ObservedAt.After(now.Add(-vaultEvery)) &&
			!e.CompletedAt.After(now) && e.ID != "" && e.BeginWAL != "" && e.EndWAL != "" {
			observed, completed := e.ObservedAt, e.CompletedAt
			p.Database.ObservedAt, p.Database.BackupCompletedAt = &observed, &completed
			if v.ObjectLock == "" || maxBackupAge <= 0 || now.Sub(completed) > maxBackupAge {
				p.Database.State, p.Database.Reason = "degraded", "Validated backup is stale or immutable retention is absent"
			} else {
				p.Database.State, p.Database.Reason = "protected", "Validated current-history backup and Object Lock observed"
			}
		}
	}
	target := app.Spec.Standby(active)
	if source != nil && states[target].Recovery != nil &&
		(states[active].Archive == "" || states[active].Archive == ArchiveName(app, active)) &&
		(states[target].Archive == "" || states[target].Archive == ArchiveName(app, target)) {
		p.RecoveryPoint = MeasureRecovery(*source, *states[target].Recovery, app.Spec.ArchiveID, app.Spec.RPO.Duration,
			receiptAge(in.ReceiptAge, active), receiptAge(in.ReceiptAge, target))
	}
	if record := app.Spec.RestoreVerification; record != nil && current != nil &&
		record.ArchiveID == app.Spec.ArchiveID && record.SystemID == current.SystemID &&
		record.Timeline == current.Timeline && validRestoreRecord(record, now) {
		verified := record.CompletedAt.Time
		p.LastVerifiedRestore = &verified
		p.LastVerifiedRestoreScope = record.Scope
	}
	return p
}

func validRestoreRecord(record *v1alpha1.RestoreVerification, now time.Time) bool {
	if record.Actor == "" || record.CompletedAt.Time.IsZero() || record.CompletedAt.After(now) ||
		record.Scope != "database" && record.Scope != "database-and-files" || len(record.EvidenceSHA256) != 64 {
		return false
	}
	for _, c := range record.EvidenceSHA256 {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	_, err := time.Parse("20060102T150405", record.BackupID)
	return err == nil
}
