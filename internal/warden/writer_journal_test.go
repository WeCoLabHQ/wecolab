package warden

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wecolab.io/wecolab/internal/fabric"
)

func TestRecreationJournalRetainedOnAmbiguousDeleteAndRejectsStaleClear(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/repos/fabric/fabric" {
			http.Error(rw, "unexpected path", http.StatusTeapot)
			return
		}
		if req.Method == http.MethodDelete {
			deletes.Add(1)
			http.Error(rw, "unknown outcome", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"id": 17})
	}))
	defer server.Close()
	_, coordination := newLeaseTestClient(t)
	w := &Writer{Git: &fabric.Git{URL: server.URL, Repo: "fabric/fabric", Token: "token", HTTP: server.Client()}, Coordination: coordination}
	if err := withWriterLock(ctx, coordination, w.recreate); err == nil {
		t.Fatal("ambiguous DELETE must fail")
	}
	if deletes.Load() != 1 {
		t.Fatal("DELETE not exercised")
	}
	journal, err := readRecreation(ctx, coordination)
	if err != nil || journal == nil || journal.RepositoryID != 17 || journal.Nonce == "" {
		t.Fatalf("ambiguous DELETE must retain exact journal: %v %v", journal, err)
	}
	if err := withWriterLock(ctx, coordination, func(ctx context.Context) error {
		stale := *journal
		stale.Nonce = "different"
		return mutateRecreation(ctx, coordination, &stale, nil)
	}); err == nil || !strings.Contains(err.Error(), "stale clear") {
		t.Fatalf("stale clear must be rejected: %v", err)
	}
	still, err := readRecreation(ctx, coordination)
	if err != nil || *still != *journal {
		t.Fatalf("stale clear corrupted durable journal: %v %v", still, err)
	}
	if _, err := TakeOver(ctx, w.Git, "home", nil, fabric.Author{Name: "operator"}, coordination); err == nil || !strings.Contains(err.Error(), "journal pending") {
		t.Fatalf("takeover must refuse before touching Forgejo: %v", err)
	}
	if deletes.Load() != 1 {
		t.Fatal("takeover unexpectedly deleted repository")
	}
	if err := mutateRecreation(ctx, coordination, journal, nil); err == nil {
		t.Fatal("journal may not be cleared outside callback's live holder context")
	}
}

func TestRecoveryDrainsBeforeEnsureAndRetainsJournalOnOrphanError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newRolloutFixture(t, nil)
	var deletes, ensures atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			deletes.Add(1)
		}
		if req.Method == http.MethodGet && req.URL.Path == "/api/v1/repos/fabric/fabric" {
			ensures.Add(1)
			http.Error(rw, "orphan directory", http.StatusInternalServerError)
			return
		}
		http.Error(rw, "unexpected request", http.StatusTeapot)
	}))
	defer server.Close()
	_, coordination := newLeaseTestClient(t)
	w := &Writer{Client: f.client, APIReader: f.client, Git: &fabric.Git{URL: server.URL, Repo: "fabric/fabric", Token: "token", HTTP: server.Client()}, Coordination: coordination}
	journal := recreationJournal{Nonce: "old-operation", RepositoryID: 17}
	finished := make(chan error, 1)
	go func() {
		finished <- withWriterLock(ctx, coordination, func(ctx context.Context) error {
			if err := mutateRecreation(ctx, coordination, nil, &journal); err != nil {
				return err
			}
			return w.recoverRecreation(ctx, journal)
		})
	}()
	marker := awaitRollout(t, ctx, f)
	if ensures.Load() != 0 || deletes.Load() != 0 {
		t.Fatal("recovery touched Forgejo before old process exited")
	}
	if err := f.completeRollout(ctx, marker); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("orphan Ensure failure must surface: %v", err)
	}
	if deletes.Load() != 0 || ensures.Load() != 1 {
		t.Fatalf("recovery may only Ensure, never DELETE: ensure=%d delete=%d", ensures.Load(), deletes.Load())
	}
	still, err := readRecreation(ctx, coordination)
	if err != nil || still == nil || *still != journal {
		t.Fatalf("orphan error must retain journal: %v %v", still, err)
	}
}
