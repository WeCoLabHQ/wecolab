package warden

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wecolab.io/wecolab/internal/fabric"
)

func TestRecreationCancellationAfterDeleteStartsRetainsJournal(t *testing.T) {
	ctx, timeout := context.WithTimeout(context.Background(), 5*time.Second)
	defer timeout()
	deleting := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			close(deleting)
			<-req.Context().Done()
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"id": 42})
	}))
	defer server.Close()
	_, coordination := newLeaseTestClient(t)
	w := &Writer{Git: &fabric.Git{URL: server.URL, Repo: "fabric/fabric", Token: "token", HTTP: server.Client()}, Coordination: coordination}
	canceled, cancel := context.WithCancel(ctx)
	finished := make(chan error, 1)
	go func() { finished <- withWriterLock(canceled, coordination, w.recreate) }()
	select {
	case <-deleting:
	case <-ctx.Done():
		t.Fatal("DELETE not reached")
	}
	cancel()
	if err := <-finished; err == nil {
		t.Fatal("canceled recreation reported success")
	}
	journal, err := readRecreation(ctx, coordination)
	if err != nil || journal == nil || journal.RepositoryID != 42 || journal.Nonce == "" {
		t.Fatalf("cancellation must keep durable journal: %v %v", journal, err)
	}
}
