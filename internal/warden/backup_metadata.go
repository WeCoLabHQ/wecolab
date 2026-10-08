package warden

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// backupMetadata reads Barman's FieldListFile format, never Python literals/code.
// backup_id is the directory name, not a serialized BackupInfo field.
func backupMetadata(data []byte, key, prefix string, now time.Time) (*BackupEvidence, error) {
	rel := strings.TrimPrefix(key, prefix)
	parts := strings.Split(rel, "/")
	if !strings.HasPrefix(key, prefix) || len(parts) != 3 || parts[0] != "base" || parts[2] != "backup.info" {
		return nil, fmt.Errorf("backup key outside archive: %q", key)
	}
	if _, err := time.Parse("20060102T150405", parts[1]); err != nil {
		return nil, fmt.Errorf("invalid backup id %q", parts[1])
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("malformed backup metadata")
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("duplicate backup field %s", name)
		}
		fields[name] = strings.TrimSpace(value)
	}
	switch fields["status"] {
	case "STARTED", "WAITING_FOR_WALS", "FAILED", "EMPTY":
		return nil, nil
	case "DONE":
	default:
		return nil, fmt.Errorf("unknown or missing backup status")
	}
	completed, err := backupTime(fields["end_time"])
	if err != nil {
		return nil, err
	}
	started, err := backupTime(fields["begin_time"])
	if err != nil || started.After(completed) || completed.After(now) {
		return nil, fmt.Errorf("inconsistent backup timestamps")
	}
	system, err := strconv.ParseUint(fields["systemid"], 10, 64)
	if err != nil || system == 0 {
		return nil, fmt.Errorf("invalid backup system identifier")
	}
	timeline, err := strconv.ParseUint(fields["timeline"], 10, 32)
	if err != nil || timeline == 0 {
		return nil, fmt.Errorf("invalid backup timeline")
	}
	begin, end := fields["begin_wal"], fields["end_wal"]
	for _, wal := range []string{begin, end} {
		if len(wal) != 24 || strings.ToUpper(wal) != wal {
			return nil, fmt.Errorf("invalid WAL bound")
		}
		for i := 0; i < 24; i += 8 {
			if _, err := strconv.ParseUint(wal[i:i+8], 16, 32); err != nil {
				return nil, fmt.Errorf("invalid WAL bound")
			}
		}
		walTimeline, _ := strconv.ParseUint(wal[:8], 16, 32)
		if walTimeline != timeline {
			return nil, fmt.Errorf("backup WAL timeline mismatch")
		}
	}
	if begin > end {
		return nil, fmt.Errorf("reversed WAL bounds")
	}
	// Barman Cloud writes server_name=cloud; the validated object key binds
	// this metadata to the archive, not that internal label.
	archive := path.Base(strings.TrimSuffix(prefix, "/"))
	return &BackupEvidence{Archive: archive, ID: parts[1], SystemID: fields["systemid"], Timeline: uint32(timeline), BeginWAL: begin, EndWAL: end, CompletedAt: completed, ObservedAt: now}, nil
}

func backupTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timezone-aware backup timestamp %q", s)
}
