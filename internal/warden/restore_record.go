package warden

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// RestoreArtifact is an operator assertion from a completed, isolated recovery drill.
// It is not cryptographic attestation of the actor or the recovered data.
type RestoreArtifact struct {
	Version           int
	Scenario          string
	Outcome           string
	StartedAt         time.Time
	CompletedAt       time.Time
	Namespace         string
	App               string
	ArchiveID         string
	SystemID          string
	Timeline          uint32
	BackupID          string
	ExpectedAppSHA    string
	Scope             string
	ComponentVersions map[string]string
	Operations        []string
	Checks            []RestoreCheck
}

type RestoreCheck struct {
	Name           string
	Passed         bool
	ExpectedSHA256 string
	ActualSHA256   string
}

var restoreHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var restoreGitSHA = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

// ParseRestoreArtifact refuses unknown or duplicate fields, including nested fields;
// neither a file-supplied actor nor a file-supplied digest can be authoritative.
func ParseRestoreArtifact(data []byte) (RestoreArtifact, error) {
	var a RestoreArtifact
	if len(data) == 0 || len(data) > 1<<20 {
		return a, fmt.Errorf("restore evidence must be a nonempty JSON file of at most 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueJSON(d); err != nil {
		return a, fmt.Errorf("restore evidence JSON: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return a, fmt.Errorf("restore evidence must contain exactly one JSON object")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return a, fmt.Errorf("restore evidence: %w", err)
	}
	return a, nil
}

func uniqueJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			fold := strings.ToLower(key)
			if seen[fold] {
				return fmt.Errorf("duplicate field %q", key)
			}
			seen[fold] = true
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case '[':
		for d.More() {
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
}

// ValidateRestoreArtifact requires real named, passed readback comparisons, not
// an Outcome flag or a timestamp alone. Database-only evidence never covers files.
func ValidateRestoreArtifact(a RestoreArtifact, now time.Time) error {
	if a.Version != 1 || a.Scenario == "" || a.Outcome != "passed" {
		return fmt.Errorf("restore scenario has no completed passing result")
	}
	if a.StartedAt.IsZero() || a.CompletedAt.IsZero() || !a.StartedAt.Before(a.CompletedAt) || a.CompletedAt.After(now) {
		return fmt.Errorf("restore timestamps are missing, unordered or in the future")
	}
	if a.Namespace == "" || a.App == "" || a.ArchiveID == "" || a.SystemID == "" || a.BackupID == "" || a.Timeline == 0 || !restoreGitSHA.MatchString(a.ExpectedAppSHA) {
		return fmt.Errorf("restore identity or expected App revision missing")
	}
	if a.Scope != "database" && a.Scope != "database-and-files" {
		return fmt.Errorf("unrecognized restore scope")
	}
	if len(a.ComponentVersions) == 0 || len(a.Operations) == 0 || len(a.Checks) == 0 {
		return fmt.Errorf("restore operation, component versions or readback missing")
	}
	for k, v := range a.ComponentVersions {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("empty component version")
		}
	}
	for _, op := range a.Operations {
		if strings.TrimSpace(op) == "" {
			return fmt.Errorf("empty restore operation")
		}
	}
	seen := map[string]bool{}
	for _, c := range a.Checks {
		if c.Name == "" || seen[c.Name] || !c.Passed || !restoreHash.MatchString(c.ExpectedSHA256) || c.ExpectedSHA256 != c.ActualSHA256 {
			return fmt.Errorf("restore check %q is missing, failed or does not match its SHA-256 readback", c.Name)
		}
		seen[c.Name] = true
	}
	if !seen["database-readback"] || a.Scope == "database-and-files" && !seen["files-readback"] {
		return fmt.Errorf("database and requested file scope require matching data readbacks")
	}
	return nil
}

// ValidateRestoreIdentity compares the runner's assertion with a fresh primary
// report, its independently inspected vault metadata and current database history.
func ValidateRestoreIdentity(a RestoreArtifact, app *v1alpha1.App, primary *SiteStatus, now time.Time) error {
	if app == nil || app.Namespace != a.Namespace || app.Name != a.App || app.Spec.Deleted || app.Spec.Database == "" || app.Spec.ArchiveID == "" || app.Spec.ArchiveID != a.ArchiveID {
		return fmt.Errorf("restore App incarnation differs from Git")
	}
	receiptAge := statusReceiptAge(primary, now)
	if primary == nil || primary.Site != app.Spec.Primary || receiptAge < 0 {
		return fmt.Errorf("current primary report unavailable")
	}
	state, ok := primary.Apps[a.Namespace+"/"+a.App]
	if !ok || state.Active != app.Spec.Primary || state.Vault == nil || state.Vault.Err != "" || state.Vault.BackupEvidence == nil || state.Recovery == nil || state.Recovery.Error != "" || len(state.Recovery.Samples) == 0 {
		return fmt.Errorf("validated current backup and database history unavailable")
	}
	v, e := state.Vault, state.Vault.BackupEvidence
	if e.Archive != ArchiveName(app, app.Spec.Primary) || e.ID != a.BackupID || e.SystemID != a.SystemID || e.Timeline != a.Timeline || e.BeginWAL == "" || e.EndWAL == "" || e.CompletedAt.IsZero() || e.CompletedAt.After(now) || !e.CompletedAt.Equal(v.LatestBackup) || e.ObservedAt.IsZero() || e.ObservedAt.After(now) || now.Sub(e.ObservedAt) > vaultEvery || !a.CompletedAt.After(e.CompletedAt) {
		return fmt.Errorf("restore backup does not match fresh validated vault metadata")
	}
	current := state.Recovery.Samples[len(state.Recovery.Samples)-1]
	if !validRecoverySample(current, app.Spec.ArchiveID, "primary") || sampleAge(current, receiptAge) < 0 || sampleAge(current, receiptAge) > recoveryMaxReplayAge || current.SystemID != a.SystemID || current.Timeline != a.Timeline {
		return fmt.Errorf("restore database system or timeline differs from current primary")
	}
	return nil
}

// RecordRestore writes only after the expected Git blob SHA and fresh primary
// backup/history are checked inside the retried, conditional writer transaction.
// readPrimary must read the current primary's own status, not a follower cache.
func RecordRestore(ctx context.Context, g *fabric.Git, raw []byte, actor string, readPrimary func(context.Context, string) (*SiteStatus, error)) (string, error) {
	if g == nil || readPrimary == nil || actor == "" || actor != strings.TrimSpace(actor) || strings.ContainsAny(actor, "\r\n") {
		return "", fmt.Errorf("writer, primary observer and verified operator identity required")
	}
	a, err := ParseRestoreArtifact(raw)
	if err != nil {
		return "", err
	}
	if err := ValidateRestoreArtifact(a, time.Now()); err != nil {
		return "", err
	}
	if len(validation.IsDNS1123Label(a.Namespace)) != 0 || len(validation.IsDNS1123Label(a.App)) != 0 {
		return "", fmt.Errorf("invalid App name or namespace")
	}
	p, ok := fabric.Path(v1alpha1.GroupVersion.WithKind("App"), a.Namespace, a.App)
	if !ok {
		return "", fmt.Errorf("invalid App path")
	}
	digest := sha256.Sum256(raw)
	return g.Edit(ctx, fabric.Author{Name: actor, Email: "fabric@wecolab"}, "record operator restore for "+a.Namespace+"/"+a.App, []string{p}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
		b, ok := s.Get(p)
		if !ok || s.SHA(p) != a.ExpectedAppSHA {
			return nil, fmt.Errorf("App revision changed since restore drill")
		}
		var app v1alpha1.App
		if err := yaml.UnmarshalStrict(b, &app); err != nil {
			return nil, err
		}
		primary, err := readPrimary(ctx, app.Spec.Primary)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		if err := ValidateRestoreArtifact(a, now); err != nil {
			return nil, err
		}
		if err := ValidateRestoreIdentity(a, &app, primary, now); err != nil {
			return nil, err
		}
		app.Spec.RestoreVerification = &v1alpha1.RestoreVerification{ArchiveID: a.ArchiveID, SystemID: a.SystemID, BackupID: a.BackupID, Timeline: a.Timeline, CompletedAt: metav1.NewTime(a.CompletedAt), Actor: actor, EvidenceSHA256: hex.EncodeToString(digest[:]), Scope: a.Scope}
		next, err := fabric.YAML(&app, v1alpha1.GroupVersion.WithKind("App"))
		if err != nil {
			return nil, err
		}
		return []fabric.FileChange{{Path: p, Content: next}}, nil
	})
}
