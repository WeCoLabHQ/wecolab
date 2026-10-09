package warden

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"wecolab.io/wecolab/api/v1alpha1"
)

type custodyPair struct {
	source, winner             *custodyRepo
	sourceWriter, winnerWriter *Writer
	sourceHead, winnerHead     string
	routes                     map[string]string
	warden                     *httptest.Server
	intercept                  func(*httptest.ResponseRecorder, *http.Request)
}

func newCustodyPair(t *testing.T) *custodyPair {
	t.Helper()
	p := &custodyPair{source: newCustodyRepo(t), winner: newCustodyRepo(t), routes: map[string]string{}}
	p.sourceHead = p.source.commit("", custodySiteDocs(t, Claim{Writer: "home", Epoch: 1}))
	p.winnerHead = p.winner.commit("", custodySiteDocs(t, Claim{Writer: "pub", Epoch: 2}))
	p.routes["10.77.0.1:30300"] = p.source.server.URL
	p.routes["10.77.0.2:30300"] = p.winner.server.URL
	p.routes[strings.TrimPrefix(p.source.server.URL, "http://")] = p.source.server.URL
	p.routes[strings.TrimPrefix(p.winner.server.URL, "http://")] = p.winner.server.URL
	sourceRollout, winnerRollout := newRolloutFixture(t, nil), newRolloutFixture(t, nil)
	readyCustodyRollout(t, sourceRollout)
	readyCustodyRollout(t, winnerRollout)
	p.sourceWriter = custodyWriter(t, "home", p.source, p.routes, sourceRollout, "home", "pub", "edge")
	p.winnerWriter = custodyWriter(t, "pub", p.winner, p.routes, winnerRollout, "home", "pub", "edge")
	// The fake client does not enforce resourceVersion conflicts. A renewal
	// based on an older Lease can otherwise erase a persisted recreation
	// journal, unlike the API server used by real custody transitions.
	for _, writer := range []*Writer{p.sourceWriter, p.winnerWriter} {
		_, writer.Coordination = newLeaseTestClient(t)
		_, err := writer.Coordination.Leases(SystemNS).Create(context.Background(), &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "wecolab-writer-transition", Namespace: SystemNS},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	p.warden = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.RemoteAddr = "10.77.0.1:49152" // real peer identity, never a forwarded header
		if p.intercept != nil {
			recorder := httptest.NewRecorder()
			p.winnerWriter.ServeHTTP(recorder, req)
			p.intercept(recorder, req)
			for k, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(k, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		p.winnerWriter.ServeHTTP(w, req)
	}))
	t.Cleanup(p.warden.Close)
	p.routes["10.77.0.2:8093"] = p.warden.URL
	return p
}

func (p *custodyPair) follow(t *testing.T) error {
	t.Helper()
	return withWriterLock(custodyContext(t), p.sourceWriter.Coordination, func(ctx context.Context) error {
		return p.sourceWriter.follow(ctx, []v1alpha1.Site{
			testSite("home", true, "10.77.0.1"),
			testSite("pub", true, "10.77.0.2"),
		}, "pub", true, true)
	})
}

func assertCustodyRef(t *testing.T, repo *custodyRepo, name, sha, content string) {
	t.Helper()
	if got := repo.ref(name); got != sha {
		t.Fatalf("preservation ref %s: got %s, want %s", name, got, sha)
	}
	if got := repo.show(sha, SettingsPath); got != strings.TrimSpace(content) {
		t.Fatalf("preservation ref %s did not retain independent Git history", name)
	}
}

// Forgejo's commit lookup can find an object after its last ref was deleted.
// A receiver that cannot give durable custody must not authorize source deletion.
func TestFollowRetainsCopyWithoutPreservationReceipt(t *testing.T) {
	p := newCustodyPair(t)
	custodyGit(t, p.winner.bare, "fetch", "-q", p.source.bare, "main")
	if kind := custodyGit(t, p.winner.bare, "cat-file", "-t", p.sourceHead); kind != "commit" {
		t.Fatalf("missing dangling commit: %s", kind)
	}
	requested := false
	receiver := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/fabric/commits/") {
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"head": p.winnerHead, "has": true, "onMain": false, "claim": Claim{Writer: "pub", Epoch: 2},
			})
			return
		}
		requested = true
		http.Error(rw, "preservation unavailable", http.StatusServiceUnavailable)
	}))
	defer receiver.Close()
	p.routes["10.77.0.2:8093"] = receiver.URL
	if err := p.follow(t); err == nil || !requested {
		t.Fatalf("source did not require a custody receipt: requested=%v err=%v", requested, err)
	}
	if p.source.deleted != 0 || p.source.ref(mainRef) != p.sourceHead {
		t.Fatal("an unreferenced object authorized deleting the only retained history")
	}
}

func TestCustodyTwoTakeoversRetainEveryHistory(t *testing.T) {
	p := newCustodyPair(t)
	older := p.source.commit(p.sourceHead, map[string]string{"history/older": "otherwise unreachable"})
	p.source.set("refs/heads/superseded-older-"+older, older)
	p.source.set("refs/heads/main", p.sourceHead)
	if err := p.follow(t); err != nil {
		t.Fatal(err)
	}
	if p.source.deleted != 1 || p.source.ref("refs/heads/main") != "" {
		t.Fatalf("first takeover did not recreate source: deletes=%d", p.source.deleted)
	}
	homeRef := "refs/heads/superseded-home-" + p.sourceHead
	oldRef := "refs/heads/superseded-older-" + older
	assertCustodyRef(t, p.winner, homeRef, p.sourceHead, settingsFile(t, "home", 1, ""))
	if got := p.winner.show(older, "history/older"); got != "otherwise unreachable" {
		t.Fatalf("old branch content: %q", got)
	}

	edge := newCustodyRepo(t)
	edgeHead := edge.commit("", custodySiteDocs(t, Claim{Writer: "edge", Epoch: 3}))
	p.routes["10.77.0.3:30300"] = edge.server.URL
	p.routes[strings.TrimPrefix(edge.server.URL, "http://")] = edge.server.URL
	edgeRollout := newRolloutFixture(t, nil)
	readyCustodyRollout(t, edgeRollout)
	edgeWriter := custodyWriter(t, "edge", edge, p.routes, edgeRollout, "home", "pub", "edge")
	edgeWarden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.RemoteAddr = "10.77.0.2:49152"
		edgeWriter.ServeHTTP(w, req)
	}))
	t.Cleanup(edgeWarden.Close)
	p.routes["10.77.0.3:8093"] = edgeWarden.URL
	if err := withWriterLock(custodyContext(t), p.winnerWriter.Coordination, func(ctx context.Context) error {
		return p.winnerWriter.follow(ctx, []v1alpha1.Site{
			testSite("home", true, "10.77.0.1"), testSite("pub", true, "10.77.0.2"), testSite("edge", true, "10.77.0.3"),
		}, "edge", true, true)
	}); err != nil {
		t.Fatal(err)
	}
	assertCustodyRef(t, edge, homeRef, p.sourceHead, settingsFile(t, "home", 1, ""))
	assertCustodyRef(t, edge, oldRef, older, settingsFile(t, "home", 1, ""))
	assertCustodyRef(t, edge, "refs/heads/superseded-pub-"+p.winnerHead, p.winnerHead, settingsFile(t, "pub", 2, ""))
	if got := edge.ref("refs/heads/main"); got != edgeHead {
		t.Fatalf("custody changed winner's main: %s", got)
	}
	if p.winner.deleted != 1 {
		t.Fatalf("second source not recreated: %d", p.winner.deleted)
	}
}

func TestCustodyIncompleteReceiptRetainsSourceAndRetryIsIdempotent(t *testing.T) {
	p := newCustodyPair(t)
	previous := p.source.commit(p.sourceHead, map[string]string{"history/previous": "kept independently"})
	name := "refs/heads/superseded-previous-" + previous
	p.source.set(name, previous)
	p.source.set("refs/heads/main", p.sourceHead)
	p.intercept = func(r *httptest.ResponseRecorder, req *http.Request) {
		if req.URL.Path != "/fabric/preserve" || r.Code != http.StatusOK {
			return
		}
		r.Code = http.StatusServiceUnavailable // receiver persisted refs, reply lost on the wire
	}
	if err := p.follow(t); err == nil {
		t.Fatal("lost receipt was accepted")
	}
	if p.source.deleted != 0 || p.winner.ref(name) != previous {
		t.Fatal("lost receipt deleted source or failed to preserve source history")
	}
	p.intercept = func(r *httptest.ResponseRecorder, req *http.Request) {
		if req.URL.Path != "/fabric/preserve" || r.Code != http.StatusOK {
			return
		}
		var receipt preservationReceipt
		if err := json.Unmarshal(r.Body.Bytes(), &receipt); err != nil {
			t.Error(err)
			return
		}
		delete(receipt.Refs, name)
		r.Body.Reset()
		if err := json.NewEncoder(r.Body).Encode(receipt); err != nil {
			t.Error(err)
		}
	}
	if err := p.follow(t); err == nil {
		t.Fatal("incomplete receipt was accepted")
	}
	if p.source.deleted != 0 || p.source.ref("refs/heads/main") != p.sourceHead {
		t.Fatal("partial receipt authorized source deletion")
	}
	if p.winner.ref(name) != previous {
		t.Fatal("receiver never persisted prior preservation ref")
	}
	p.intercept = nil
	if err := p.follow(t); err != nil {
		t.Fatal(err)
	}
	if p.source.deleted != 1 || p.winner.ref(name) != previous || p.winner.ref("refs/heads/superseded-home-"+p.sourceHead) != p.sourceHead {
		t.Fatal("retry failed to verify the already-installed exact refs and recreate source")
	}
	journal, err := readRecreation(custodyContext(t), p.sourceWriter.Coordination)
	if err != nil || journal != nil {
		t.Fatalf("completed retry did not clear the recreation journal: %v %v", journal, err)
	}
}

func TestCustodySourceMovementOrUnmanagedRefPreventsDeletion(t *testing.T) {
	for _, change := range []string{"move main", "add unmanaged ref"} {
		t.Run(change, func(t *testing.T) {
			p := newCustodyPair(t)
			mutated := false
			p.intercept = func(r *httptest.ResponseRecorder, req *http.Request) {
				if req.URL.Path != "/fabric/preserve" || r.Code != http.StatusOK {
					return
				}
				mutated = true
				if change == "move main" {
					moved := p.source.commit(p.sourceHead, map[string]string{"history/moved": "a write after snapshot"})
					p.source.set("refs/heads/main", moved)
				} else {
					p.source.set("refs/tags/unmanaged", p.sourceHead)
				}
			}
			if err := p.follow(t); err == nil {
				t.Fatal("source change was not detected")
			}
			if !mutated {
				t.Fatal("receiver did not install custody before the source moved")
			}
			if p.source.deleted != 0 {
				t.Fatal("changed source deleted after receipt")
			}
		})
	}
}

func TestCustodyRejectsForgedSourceAndStaleClaim(t *testing.T) {
	p := newCustodyPair(t)
	claim := Claim{Writer: "pub", Epoch: 2}
	req := preservationRequest{Source: "home", Claim: claim, Refs: map[string]string{"refs/heads/main": p.sourceHead}, PreserveMain: true}
	ctx := custodyContext(t)
	if _, err := p.winnerWriter.receivePreservation(ctx, req, custodyIP(3)); err == nil {
		t.Fatal("forged source manager IP authorized a custody receipt")
	}
	req.Claim.Epoch--
	if _, err := p.winnerWriter.receivePreservation(ctx, req, custodyIP(1)); err == nil {
		t.Fatal("stale winning claim authorized a custody receipt")
	}
	req.Claim = claim
	req.Refs["refs/heads/main"] = strings.Repeat("a", 40)
	if _, err := p.winnerWriter.receivePreservation(ctx, req, custodyIP(1)); err == nil {
		t.Fatal("invented source commit authorized a custody receipt")
	}
	if p.winner.ref("refs/heads/superseded-home-"+p.sourceHead) != "" || p.source.deleted != 0 {
		t.Fatal("rejected receipt modified custody repositories")
	}
}

func TestCustodyPendingRecreationBlocksReceipts(t *testing.T) {
	p := newCustodyPair(t)
	ctx := custodyContext(t)
	lease, err := p.winnerWriter.Coordination.Leases(SystemNS).Get(ctx, "wecolab-writer-transition", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lease.Annotations = map[string]string{recreationAnnotation: `{"nonce":"prior-delete","repositoryID":1}`}
	if _, err := p.winnerWriter.Coordination.Leases(SystemNS).Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	req := preservationRequest{Source: "home", Claim: Claim{Writer: "pub", Epoch: 2},
		Refs: map[string]string{"refs/heads/main": p.sourceHead}, PreserveMain: true}
	if _, err := p.winnerWriter.receivePreservation(ctx, req, custodyIP(1)); err == nil {
		t.Fatal("pending recreation gave a custody receipt")
	}
	if p.winner.ref("refs/heads/superseded-home-"+p.sourceHead) != "" {
		t.Fatal("journal-pending receiver installed a ref")
	}
}

func TestCustodyReceiptCannotCrossReceiverTransition(t *testing.T) {
	p := newCustodyPair(t)
	ctx := custodyContext(t)
	req := preservationRequest{Source: "home", Claim: Claim{Writer: "pub", Epoch: 2},
		Refs: map[string]string{"refs/heads/main": p.sourceHead}, PreserveMain: true}
	started := make(chan struct{})
	done := make(chan error, 1)
	err := withWriterLock(ctx, p.winnerWriter.Coordination, func(context.Context) error {
		go func() {
			close(started)
			_, err := p.winnerWriter.receivePreservation(ctx, req, custodyIP(1))
			done <- err
		}()
		<-started
		select {
		case err := <-done:
			t.Fatalf("custody request entered receiver transition lease: %v", err)
		default:
		}
		// Simulate an intervening writer transition while this lease is held.
		p.winner.commit(p.winnerHead, custodySiteDocs(t, Claim{Writer: "edge", Epoch: 3}))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stale receipt issued after receiver changed writer claim")
		}
	case <-ctx.Done():
		t.Fatalf("blocked custody request never completed: %v", ctx.Err())
	}
	if p.winner.ref("refs/heads/superseded-home-"+p.sourceHead) != "" {
		t.Fatal("receiver installed custody after its writer claim changed")
	}
}

func TestCustodyRejectsCollidingPreservationHistory(t *testing.T) {
	p := newCustodyPair(t)
	older := p.source.commit(p.sourceHead, map[string]string{"history/collision": "must not disappear"})
	name := preservedPrefix + "home-" + p.sourceHead
	p.source.set(name, older)
	p.source.set(mainRef, p.sourceHead)
	if err := p.follow(t); err == nil {
		t.Fatal("different histories collapsed onto one preservation name")
	}
	if p.source.deleted != 0 || p.source.ref(name) != older || p.source.ref(mainRef) != p.sourceHead {
		t.Fatal("colliding destination names lost source history")
	}
}

func TestCustodyIdenticalRetryDoesNotRestartReceiver(t *testing.T) {
	p := newCustodyPair(t)
	ctx := custodyContext(t)
	req := preservationRequest{Source: "home", Claim: Claim{Writer: "pub", Epoch: 2},
		Refs: map[string]string{mainRef: p.sourceHead}, PreserveMain: true}
	receipt, err := p.winnerWriter.receivePreservation(ctx, req, custodyIP(1))
	if err != nil {
		t.Fatal(err)
	}
	before, err := p.winnerWriter.forgejoDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := p.winnerWriter.receivePreservation(ctx, req, custodyIP(1))
	if err != nil {
		t.Fatal(err)
	}
	after, err := p.winnerWriter.forgejoDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Spec.Template.Annotations[gitTransferAnnotation] != after.Spec.Template.Annotations[gitTransferAnnotation] {
		t.Fatal("an unchanged custody retry restarted the writer's Forgejo")
	}
	name := preservedPrefix + "home-" + p.sourceHead
	if receipt.Refs[name] != p.sourceHead || repeated.Refs[name] != p.sourceHead || p.winner.ref(name) != p.sourceHead {
		t.Fatal("idempotent receipt lost reachable history")
	}
}
