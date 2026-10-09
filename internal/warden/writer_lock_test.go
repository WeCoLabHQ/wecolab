package warden

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
)

// leaseServer enforces the resourceVersion compare-and-swap that a real API
// server uses. The generated client exercises the actual LeaseLock HTTP path.
type leaseServer struct {
	mu            sync.Mutex
	lease         *coordinationv1.Lease
	deny          bool
	failRenewals  bool
	serial        int
	beforeRelease func(*coordinationv1.Lease)
}

func leaseFailure(w http.ResponseWriter, code int, reason metav1.StatusReason) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(&metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure, Reason: reason, Code: int32(code),
	})
}

func (s *leaseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const collection = "/apis/coordination.k8s.io/v1/namespaces/wecolab-system/leases"
	if r.URL.Path != collection && r.URL.Path != collection+"/wecolab-writer-transition" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if s.deny {
		leaseFailure(w, http.StatusForbidden, metav1.StatusReasonForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if s.lease == nil {
			leaseFailure(w, http.StatusNotFound, metav1.StatusReasonNotFound)
			return
		}
	case http.MethodPost, http.MethodPut:
		var next coordinationv1.Lease
		if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if next.Name != "wecolab-writer-transition" || next.Namespace != "wecolab-system" {
			http.Error(w, "wrong lock", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPut && s.lease != nil && next.Spec.HolderIdentity != nil && *next.Spec.HolderIdentity == "" && s.beforeRelease != nil {
			change := s.beforeRelease
			s.beforeRelease = nil
			change(s.lease)
			s.serial++
			s.lease.ResourceVersion = strconv.Itoa(s.serial)
		}
		if r.Method == http.MethodPost && s.lease != nil || r.Method == http.MethodPut && (s.lease == nil || next.ResourceVersion != s.lease.ResourceVersion) {
			leaseFailure(w, http.StatusConflict, metav1.StatusReasonConflict)
			return
		}
		if s.failRenewals && next.Spec.HolderIdentity != nil && *next.Spec.HolderIdentity != "" {
			leaseFailure(w, http.StatusConflict, metav1.StatusReasonConflict)
			return
		}
		s.serial++
		next.ResourceVersion = strconv.Itoa(s.serial)
		s.lease = next.DeepCopy()
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(s.lease)
}

func newLeaseTestClient(t *testing.T) (*leaseServer, coordinationclient.CoordinationV1Interface) {
	t.Helper()
	fixture := &leaseServer{}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client, err := coordinationclient.NewForConfig(&rest.Config{
		Host: server.URL,
		ContentConfig: rest.ContentConfig{
			ContentType: "application/json", AcceptContentTypes: "application/json",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, client
}

func awaitLeaseEvent(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for lease operation")
	}
}

func TestWriterLockSerializesCompetingClients(t *testing.T) {
	fixture, client := newLeaseTestClient(t)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withWriterLock(context.Background(), client, func(ctx context.Context) error {
			close(firstEntered)
			<-releaseFirst
			return ctx.Err()
		})
	}()
	awaitLeaseEvent(t, firstEntered)

	fixture.mu.Lock()
	firstIdentity := *fixture.lease.Spec.HolderIdentity
	fixture.mu.Unlock()
	secondEntered := make(chan struct{})
	secondIdentity := make(chan string, 1)
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- withWriterLock(context.Background(), client, func(ctx context.Context) error {
			fixture.mu.Lock()
			secondIdentity <- *fixture.lease.Spec.HolderIdentity
			fixture.mu.Unlock()
			close(secondEntered)
			return ctx.Err()
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second mutation started while first still held the lease")
	case <-time.After(3 * time.Second): // long enough for at least one acquisition attempt
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first mutation: %v", err)
	}
	awaitLeaseEvent(t, secondEntered)
	if err := <-secondDone; err != nil {
		t.Fatalf("second mutation: %v", err)
	}
	if identity := <-secondIdentity; identity == firstIdentity || identity == "" {
		t.Fatalf("separate invocations used the same or empty identity: first=%q second=%q", firstIdentity, identity)
	}
}

func TestWriterLockCancellationWaitsForMutationCleanup(t *testing.T) {
	_, client := newLeaseTestClient(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstEntered := make(chan struct{})
	firstCanceled := make(chan struct{})
	finishCleanup := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withWriterLock(parent, client, func(ctx context.Context) error {
			close(firstEntered)
			<-ctx.Done()
			close(firstCanceled)
			<-finishCleanup
			return nil
		})
	}()
	awaitLeaseEvent(t, firstEntered)
	cancel()
	awaitLeaseEvent(t, firstCanceled)
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- withWriterLock(context.Background(), client, func(context.Context) error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("parent cancellation released ownership before cleanup completed")
	case <-time.After(3 * time.Second):
	}
	close(finishCleanup)
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mutation returned %v", err)
	}
	awaitLeaseEvent(t, secondEntered)
	if err := <-secondDone; err != nil {
		t.Fatalf("second mutation: %v", err)
	}
}

func TestWriterLockReleaseRechecksOwnershipAfterConflict(t *testing.T) {
	for _, tc := range []struct{ name, holder string }{
		{"late-renewal", ""},
		{"successor", "successor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, client := newLeaseTestClient(t)
			fixture.beforeRelease = func(lease *coordinationv1.Lease) {
				// A canceled HTTP request may still commit at the API server
				// after release reads its resourceVersion.
				lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
				if tc.holder != "" {
					lease.Spec.HolderIdentity = &tc.holder
				}
			}
			if err := withWriterLock(context.Background(), client, func(context.Context) error { return nil }); err != nil {
				t.Fatalf("completed mutation failed to release safely: %v", err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if got := *fixture.lease.Spec.HolderIdentity; got != tc.holder {
				t.Fatalf("released the wrong owner: got %q, want %q", got, tc.holder)
			}
		})
	}
}

func TestWriterLockRefusesCanceledAndDeniedAcquisition(t *testing.T) {
	fixture, client := newLeaseTestClient(t)
	invoked := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withWriterLock(ctx, client, func(context.Context) error { invoked = true; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled acquisition: %v", err)
	}
	if invoked {
		t.Fatal("canceled acquisition ran the mutation")
	}
	if err := withWriterLock(context.Background(), nil, func(context.Context) error { invoked = true; return nil }); err == nil {
		t.Fatal("nil coordination client was accepted")
	}
	fixture.mu.Lock()
	fixture.deny = true
	fixture.mu.Unlock()
	deadline, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stop()
	if err := withWriterLock(deadline, client, func(context.Context) error { invoked = true; return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("denied acquisition: %v", err)
	}
	if invoked {
		t.Fatal("refused acquisition ran the mutation")
	}
}

func TestWriterLockLeaseLossInterruptsMutation(t *testing.T) {
	fixture, client := newLeaseTestClient(t)
	entered := make(chan struct{})
	interrupted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withWriterLock(context.Background(), client, func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			close(interrupted)
			return nil // lease loss must override even a nil callback result
		})
	}()
	awaitLeaseEvent(t, entered)
	fixture.mu.Lock()
	fixture.deny = true // block all renewals; real election expires after RenewDeadline
	fixture.mu.Unlock()
	awaitLeaseEvent(t, interrupted)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lease") {
			t.Fatalf("lost lease returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("lease-loss mutation did not return")
	}
}

func TestWriterLockDoesNotReleaseLostLeaseBeforeCleanup(t *testing.T) {
	fixture, client := newLeaseTestClient(t)
	entered := make(chan struct{})
	interrupted := make(chan struct{})
	finishCleanup := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withWriterLock(context.Background(), client, func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			close(interrupted)
			<-finishCleanup
			return nil
		})
	}()
	awaitLeaseEvent(t, entered)
	defer func() {
		close(finishCleanup)
		if err := <-done; err == nil {
			t.Error("lease loss returned success")
		}
	}()
	fixture.mu.Lock()
	fixture.failRenewals = true // releases still succeed, unlike an API outage
	fixture.mu.Unlock()
	awaitLeaseEvent(t, interrupted)
	fixture.mu.Lock()
	held := fixture.lease.Spec.HolderIdentity != nil && *fixture.lease.Spec.HolderIdentity != ""
	fixture.mu.Unlock()
	if !held {
		t.Fatal("renewal failure released the lease before mutation cleanup finished")
	}
}

func TestWriterLockReleasesAfterMutationPanic(t *testing.T) {
	_, client := newLeaseTestClient(t)
	func() {
		defer func() {
			if value := recover(); value != "mutation failed" {
				t.Fatalf("mutation panic was changed: %v", value)
			}
		}()
		_ = withWriterLock(context.Background(), client, func(context.Context) error {
			panic("mutation failed")
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := withWriterLock(ctx, client, func(ctx context.Context) error {
		return ctx.Err()
	}); err != nil {
		t.Fatalf("panicked mutation kept the next writer locked out: %v", err)
	}
}

func TestWriterLockPreservesContextValues(t *testing.T) {
	_, client := newLeaseTestClient(t)
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "elevation")
	if err := withWriterLock(ctx, client, func(ctx context.Context) error {
		if ctx.Value(key{}) != "elevation" {
			t.Fatal("mutation lost caller context values")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
