package warden

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func completedMetadata(status string, at time.Time) string {
	return fmt.Sprintf("status=%s\nserver_name=cloud\nsystemid=7284954224128361771\ntimeline=1\nbegin_time=%s\nend_time=%s\nbegin_wal=000000010000000000000002\nend_wal=000000010000000000000003\n", status, at.Add(-time.Minute).Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
}

func TestInspectS3QualifiesCompletedBackup(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	for _, status := range []string{"FAILED", "STARTED", "DONE"} {
		t.Run(status, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Has("object-lock") {
					http.Error(w, "unsupported", 404)
					return
				}
				if strings.HasSuffix(r.URL.Path, "backup.info") {
					fmt.Fprint(w, completedMetadata(status, now))
					return
				}
				fmt.Fprint(w, `<ListBucketResult><Contents><Key>db-site/base/20261005T110000/backup.info</Key><LastModified>2026-10-05T12:00:00Z</LastModified></Contents></ListBucketResult>`)
			}))
			defer srv.Close()
			got := InspectS3(context.Background(), S3{Endpoint: srv.URL, Dev: true}, "bucket", "db-site/")
			if got.Err != "" {
				t.Fatal(got.Err)
			}
			if status == "DONE" {
				if got.BackupEvidence == nil || !got.LatestBackup.Equal(now) {
					t.Fatalf("completion evidence: %+v", got)
				}
			} else {
				if got.BackupEvidence != nil || !got.LatestBackup.IsZero() {
					t.Fatalf("incomplete backup qualified: %+v", got)
				}
				if !NeedsBackup(&got, nil, "db", "db-site", RecoverySample{SystemID: "7284954224128361771", Timeline: 1}, time.Now()) {
					t.Fatal("failed/incomplete metadata prevented due retry")
				}
			}
		})
	}
}

func TestBackupMetadataRejectsUncertainHistory(t *testing.T) {
	now := time.Now().UTC()
	valid := completedMetadata("DONE", now.Add(-time.Minute))
	for _, mutate := range []func(string) string{
		func(s string) string { return strings.Replace(s, "status=DONE", "status=", 1) },
		func(s string) string {
			return strings.Replace(s, "end_wal=000000010000000000000003", "end_wal=None", 1)
		},
		func(s string) string { return strings.Replace(s, "timeline=1", "timeline=2", 1) },
		func(s string) string { return strings.Replace(s, "systemid=7284954224128361771", "systemid=None", 1) },
		func(s string) string { return s + "status=DONE\n" },
		func(s string) string { return completedMetadata("DONE", now.Add(time.Hour)) },
	} {
		if _, err := backupMetadata([]byte(mutate(valid)), "db-site/base/20261005T110000/backup.info", "db-site/", now); err == nil {
			t.Fatal("uncertain history qualified")
		}
	}
	for _, key := range []string{
		"other-site/base/20261005T110000/backup.info",
		"db-site-other/base/20261005T110000/backup.info",
		"db-site/base/20261005T110000/nested/backup.info",
	} {
		if _, err := backupMetadata([]byte(valid), key, "db-site/", now); err == nil {
			t.Fatalf("backup outside the selected archive qualified: %s", key)
		}
	}
	if e, err := backupMetadata([]byte(valid), "db-site/base/20261005T110000/backup.info", "db-site/", now); err != nil || e.Archive != "db-site" || e.SystemID != "7284954224128361771" {
		t.Fatalf("valid history rejected: %v %v", e, err)
	}
}
