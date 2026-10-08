package warden

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"wecolab.io/wecolab/internal/fabric"
)

func TestPendingMirrorMarkerSurvivesInterruptionAfterDeletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := newRolloutFixture(t, nil)
	copy := &fakeCopy{exists: true, mirrors: []fabric.PushMirror{{Name: "legacy"}}}
	listed := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var lists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/api/v1/repos/fabric/fabric/push_mirrors" && lists.Add(1) == 2 {
			close(listed)
			select {
			case <-release:
			case <-req.Context().Done():
			}
		}
		copy.ServeHTTP(rw, req)
	}))
	defer server.Close()
	coordination := testWriterCoordination().CoordinationV1()
	w := &Writer{Client: f.client, APIReader: f.client, Git: &fabric.Git{URL: server.URL, Repo: "fabric/fabric", Token: "token", HTTP: server.Client()}, Coordination: coordination}
	firstCtx, stopFirst := context.WithCancel(ctx)
	finished := make(chan error, 1)
	go func() { finished <- withWriterLock(firstCtx, coordination, w.quiesceMirrors) }()
	select {
	case <-listed:
	case <-ctx.Done():
		t.Fatal("mirror deletion was not reached")
	}
	d := &appsv1.Deployment{}
	if err := f.client.Get(ctx, forgejoDeployment, d); err != nil {
		t.Fatal(err)
	}
	if d.Annotations[gitMirrorPendingAnnotation] != "true" || d.Spec.Template.Annotations[gitTransferAnnotation] != "initial" {
		t.Fatalf("pending must be durable before deletion and old success marker is insufficient: %v", d)
	}
	stopFirst()
	if err := <-finished; err == nil {
		t.Fatal("interrupted mirror deletion reported success")
	}
	select {
	case marker := <-f.updates:
		t.Fatalf("rollout %q requested before interrupted deletion finished", marker)
	default:
	}
	retried := make(chan error, 1)
	go func() { retried <- withWriterLock(ctx, coordination, w.quiesceMirrors) }()
	marker := awaitRollout(t, ctx, f)
	if marker == "initial" {
		t.Fatal("retry reused pre-deletion marker")
	}
	if err := f.completeRollout(ctx, marker); err != nil {
		t.Fatal(err)
	}
	if err := <-retried; err != nil {
		t.Fatal(err)
	}
}

func TestProtectionChangeInterruptionCannotReusePreviousCustodyFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := newRolloutFixture(t, nil)
	repository := newCustodyRepo(t)
	first, interrupt := context.WithCancel(ctx)
	defer interrupt()
	httpClient := &http.Client{Transport: custodyRoute(func(req *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(req)
		if req.Method == http.MethodPost && req.URL.Path == "/api/v1/repos/fabric/fabric/branch_protections" {
			interrupt() // protection committed, but no fresh Forgejo drain yet
		}
		return response, err
	})}
	w := &Writer{Client: f.client, APIReader: f.client,
		Git:          &fabric.Git{URL: repository.server.URL, Repo: "fabric/fabric", Token: "fixture", HTTP: httpClient},
		Coordination: testWriterCoordination().CoordinationV1()}
	if err := withWriterLock(first, w.Coordination, w.quiesceMirrors); err == nil {
		t.Fatal("interrupted protection change acknowledged safe custody")
	}
	protected, err := w.Git.PreservationProtected(ctx, ForgejoOwner)
	if err != nil || !protected {
		t.Fatalf("failure point did not follow persisted protection: protected=%v err=%v", protected, err)
	}
	d, err := w.forgejoDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Annotations[gitMirrorPendingAnnotation] != "true" || d.Spec.Template.Annotations[gitTransferAnnotation] != "initial" {
		t.Fatal("interrupted protection change lost its unfinished drain")
	}
	finished := make(chan error, 1)
	go func() { finished <- withWriterLock(ctx, w.Coordination, w.quiesceMirrors) }()
	marker := awaitRollout(t, ctx, f)
	select {
	case err := <-finished:
		t.Fatalf("retry acknowledged custody with an old Forgejo process alive: %v", err)
	default:
	}
	if err := f.completeRollout(ctx, marker); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
