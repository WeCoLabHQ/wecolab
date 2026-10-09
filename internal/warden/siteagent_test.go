package warden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/flowcontrol"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

func TestSiteStatus(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	node := func(name string, laptop, away bool) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: map[string]string{"box": "10.77.0.9"}[name]}}
		if laptop {
			n.Labels["wecolab.io/laptop"] = "true"
			if !away {
				n.Spec.Taints = []corev1.Taint{{Key: "wecolab.io/idle", Value: "true", Effect: corev1.TaintEffectNoExecute}}
			}
		}
		return n
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "x"}, Spec: corev1.PodSpec{NodeName: "box", Containers: []corev1.Container{{Name: "c",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	elsewhere := named("elsewhere")
	elsewhere.Spec.Sites = []string{"friend", "home"}
	// wiki answers here: a node port Service, and a ready pod on box.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "vince"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort,
		Selector: map[string]string{"app": "wiki"}, Ports: []corev1.ServicePort{{Port: 80, NodePort: 31234}}}}
	wikiPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "wiki-1", Namespace: "vince", Labels: map[string]string{"app": "wiki"}}, Spec: corev1.PodSpec{NodeName: "box"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	// wiki's own Secret, named after the app (deploy), holds the vault key its database archives with.
	wikiSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "vince"}, Data: map[string][]byte{"b2-key-id": []byte("K2"), "b2-key": []byte("the-key-itself")}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(node("box", false, false), node("mac-in-use", true, false), node("mac-away", true, true),
		pod, named("wiki"), elsewhere, svc, wikiPod, wikiSecret).WithStatusSubresource(&v1alpha1.App{}).Build()
	st, err := (&SiteAgent{Client: c, Site: "vince", SchemaVersion: "v2"}).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != "v2" {
		t.Fatalf("published warden schema version: %q", st.SchemaVersion)
	}
	if st.ReceivedAt.IsZero() || st.ReceivedAt.Before(st.Time) {
		t.Fatalf("local receipt must be stamped after status collection: %+v", st)
	}
	if b, err := json.Marshal(st); err != nil || strings.Contains(string(b), "ReceivedAt") || strings.Contains(string(b), "receivedAt") {
		t.Fatalf("receipt must stay local: %s %v", b, err)
	}
	idle := map[string]bool{}
	for _, n := range st.Nodes {
		idle[n.Name] = n.Idle
	}
	if idle["box"] || idle["mac-in-use"] || !idle["mac-away"] {
		t.Errorf("only a laptop without the idle taint is idle: %v", idle)
	}
	if st.Allocatable["cpu"] != "12" || st.Requested["cpu"] != "500m" {
		t.Errorf("capacity sums: allocatable %v requested %v", st.Allocatable, st.Requested)
	}
	if len(st.Apps) != 1 || st.Apps["vince/wiki"].Active != "vince" {
		t.Errorf("published roles: only apps placed here, from Git: %v", st.Apps)
	}
	if e := st.Apps["vince/wiki"].Endpoints; len(e) != 1 || e[0] != "10.77.0.9:31234" {
		t.Errorf("endpoints: the Nebula address of the box running a ready pod, with the node port: %v", e)
	}
	if id := st.Apps["vince/wiki"].VaultKeyID; id != "K2" || strings.Contains(toJSON(st), "the-key-itself") {
		t.Errorf("the id of the vault key the app's Secret holds, never the key: %q", id)
	}
	if got := st.Apps["vince/wiki"].Recovery; got == nil || got.Error == "" {
		t.Fatalf("missing CNPG exporter is an explicit unknown: %+v", got)
	}
}

type rateLimitedStatusReader struct {
	client.Reader
	limit   flowcontrol.RateLimiter
	latency time.Duration
}

func (r rateLimitedStatusReader) wait(ctx context.Context) error {
	if err := r.limit.Wait(ctx); err != nil {
		return err
	}
	if r.latency != 0 {
		select {
		case <-time.After(r.latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r rateLimitedStatusReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := r.wait(ctx); err != nil {
		return err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func (r rateLimitedStatusReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.wait(ctx); err != nil {
		return err
	}
	return r.Reader.List(ctx, list, opts...)
}

type delayedSettingsReader struct {
	client.Reader
	delay time.Duration
}

func (r delayedSettingsReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Namespace == SystemNS && key.Name == "fabric" {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

type unavailableStatusReader struct{ client.Reader }

func (unavailableStatusReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("inventory unavailable")
}

func TestStatusReportsAppRoutesWithinPeerDeadline(t *testing.T) {
	site := testSite("vince", false, "10.77.0.1")
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "box"}, Status: corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.77.0.9"}},
	}}
	objects := []client.Object{&site, node, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "hidden", Namespace: "system"},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}}
	type expectation struct{ keyID, endpoint string }
	want := map[string]expectation{}
	for project, base := range map[string]int32{"p": 31000, "q": 31100, "r": 31200} {
		replicas := int32(2)
		objects = append(objects,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: project, Labels: map[string]string{"wecolab.io/tenant": project}}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: project},
				Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}})
		for i := range 12 {
			name := fmt.Sprintf("app-%02d", i)
			app := named(name)
			app.Namespace = project
			app.Spec.Primary, app.Spec.Database, app.Spec.Workload = "home", name+"-db", name
			keyID := project + "-" + name
			port := base + int32(i)
			objects = append(objects, app,
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
					Data: map[string][]byte{"b2-key-id": []byte(keyID)}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
					Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: map[string]string{"app": name},
						Ports: []corev1.ServicePort{{Port: 80, NodePort: port}}}},
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project, Labels: map[string]string{"app": name}},
					Spec:   corev1.PodSpec{NodeName: "box"},
					Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}})
			want[project+"/"+name] = expectation{keyID, fmt.Sprintf("10.77.0.9:%d", port)}
		}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
	// API latency and throttling must not make a healthy multi-app site
	// disappear. Inventory reads need both batching and bounded concurrency.
	limit := flowcontrol.NewTokenBucketRateLimiter(1, 12)
	defer limit.Stop()
	agent := &SiteAgent{Client: rateLimitedStatusReader{Reader: c, limit: limit, latency: 300 * time.Millisecond}, Site: "vince"}
	server := httptest.NewServer(agent)
	defer server.Close()
	peers := &Peers{Client: c, HTTP: &http.Client{Timeout: 3 * time.Second, Transport: toServer{server.URL}}}
	report, ok := peers.Get(context.Background(), "vince")
	if !ok {
		t.Fatal("healthy multi-app site missed the peer observation deadline")
	}
	for key, expected := range want {
		app := report.Apps[key]
		if app.VaultKeyID != expected.keyID || len(app.Endpoints) != 1 || app.Endpoints[0] != expected.endpoint {
			t.Errorf("%s: wrong project-scoped key or route: %+v", key, app)
		}
	}
	for _, project := range []string{"p", "q", "r"} {
		if got := report.Workloads[project+"/work"]; got.Ready != 1 || got.Replicas != 2 {
			t.Errorf("%s workload observation: %+v", project, got)
		}
	}
	if _, exposed := report.Workloads["system/hidden"]; exposed {
		t.Fatal("status exposed a workload outside tenant namespaces")
	}
}

func TestShortPeerRequestsDoNotStarveStatusCollection(t *testing.T) {
	site := testSite("vince", false, "10.77.0.1")
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "box"}, Status: corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(&site, node).Build()
	limit := flowcontrol.NewTokenBucketRateLimiter(10, 1)
	defer limit.Stop()
	agent := &SiteAgent{Client: rateLimitedStatusReader{Reader: c, limit: limit}, Site: "vince"}
	server := httptest.NewServer(agent)
	defer server.Close()
	peers := &Peers{Client: c, HTTP: &http.Client{Timeout: 250 * time.Millisecond, Transport: toServer{server.URL}}}
	if _, err := peers.fetch(context.Background(), "vince"); err == nil {
		t.Fatal("a cold inventory cannot fit in this peer's request budget")
	}
	// Each peer request is shorter than a complete inventory. A disconnected
	// caller must not discard shared progress and make every later peer miss.
	for range 6 {
		report, err := peers.fetch(context.Background(), "vince")
		if err != nil {
			continue
		}
		if len(report.Nodes) != 1 || report.Nodes[0].Name != "box" || !report.Nodes[0].Ready {
			t.Fatalf("peer received an incomplete healthy-site observation: %+v", report)
		}
		return
	}
	t.Fatal("short peer requests continually canceled the healthy site's inventory")
}

func TestStatusRefreshFailureDoesNotServeExpiredSuccess(t *testing.T) {
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "box"}, Status: corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(node).WithStatusSubresource(node).Build()
	agent := &SiteAgent{Client: c, Site: "vince"}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		agent.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/status", nil))
		return r
	}
	if got := request(); got.Code != http.StatusOK {
		t.Fatalf("initial inventory: %d %s", got.Code, got.Body)
	}
	agent.mu.Lock()
	agent.at = time.Now().Add(-6 * time.Second)
	agent.mu.Unlock()
	agent.Client = unavailableStatusReader{c}
	if got := request(); got.Code != http.StatusInternalServerError {
		t.Fatalf("failed refresh served an expired success: %d %s", got.Code, got.Body)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "box"}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := c.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	agent.Client = c
	got := request()
	var report SiteStatus
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &report) != nil {
		t.Fatalf("inventory after API recovery: %d %s", got.Code, got.Body)
	}
	if len(report.Nodes) != 1 || report.Nodes[0].Ready {
		t.Fatalf("API recovery resurrected the old ready-node observation: %+v", report.Nodes)
	}
}

func TestPeerReceiptPreservedAcrossCachedPolls(t *testing.T) {
	body := &SiteStatus{Site: "home", ReceivedAt: time.Now().Add(-time.Hour)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	site := testSite("home", false, "10.77.2.1")
	p := &Peers{Client: fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(&site).Build(),
		HTTP: &http.Client{Transport: toServer{server.URL}}}
	first, ok := p.Get(context.Background(), "home")
	if !ok || first.ReceivedAt.IsZero() || time.Since(first.ReceivedAt) > time.Second {
		t.Fatalf("receipt must be local to HTTP decode, not the remote JSON: %+v", first)
	}
	received := first.ReceivedAt
	for range 3 {
		again, ok := p.Get(context.Background(), "home")
		if !ok || again.ReceivedAt != received {
			t.Fatalf("cached poll renewed receipt: %+v", again)
		}
	}
}

// named is sampleApp named name.
func named(name string) *v1alpha1.App {
	a := sampleApp()
	a.Name = name
	return a
}

// A vault look belongs to one archive: after a new generation the old folder's answer is not the new one's.
func TestVaultLookPerArchive(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	a := &SiteAgent{Client: fake.NewClientBuilder().WithScheme(s).Build(), Site: "vince"}
	app := sampleApp()
	look := func() *VaultStatus {
		for i := 0; i < 200; i++ {
			if v := a.vault(app, nil); v != nil {
				return v
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("no look at the vault")
		return nil
	}
	if v := look(); v.Err == "" {
		t.Fatalf("no ObjectStore, no vault: %+v", v)
	}
	app.Spec.Archive = map[string]int{"vince": 2}
	if v := a.vault(app, nil); v != nil {
		t.Fatalf("the new archive's first look is its own, not the old folder's: %+v", v)
	}
	app.Spec.Archive = nil
	app.Spec.ArchiveID = "new-identity"
	if v := a.vault(app, nil); v != nil {
		t.Fatalf("a recreated database with the same archive label cannot reuse old vault evidence: %+v", v)
	}
}

// Publication ages samples through slow collection; pod role changes invalidate them.
func TestRecoveryPodSelectionAndPublicationAge(t *testing.T) {
	app := sampleApp()
	app.Spec.ArchiveID = "archive"
	a := &SiteAgent{Site: "vince"}
	now := time.Now()
	observedAt := now.Add(-(recoveryMaxReplayAge - recoveryDeliveryAllowance - time.Second))
	key := app.Namespace + "/" + app.Spec.Database + "/archive/" + ArchiveName(app, "vince")
	a.recoveries = map[string]*recoveryAnswer{key: {pod: "docs-db-1", role: "primary", expectedRole: "primary", at: now, report: RecoveryReport{Samples: []RecoverySample{
		{ArchiveID: "archive", SystemID: "system", Timeline: 1, Pod: "docs-db-1", Role: "primary", LSNLow: 1, ObservedAt: observedAt},
	}}}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "docs-db-1", Namespace: app.Namespace, Labels: map[string]string{
		"cnpg.io/cluster": app.Spec.Database, "app.kubernetes.io/managed-by": "cloudnative-pg",
		"app.kubernetes.io/component": "database", "cnpg.io/instanceName": "docs-db-1", "cnpg.io/instanceRole": "primary",
	}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: app.Spec.Database, UID: "owner"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "invalid-ip"}}
	db := map[string]any{"currentPrimary": "docs-db-1"}
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "status": db,
		"metadata": map[string]any{"namespace": app.Namespace, "name": app.Spec.Database, "uid": "owner"},
	}}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app, &pod, cluster).Build()
	a.Client = delayedSettingsReader{Reader: c, delay: 2 * time.Second}
	status, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := status.Apps[app.Namespace+"/"+app.Name].Recovery
	if got == nil || got.Error != "" || len(got.Samples) != 1 || !got.Samples[0].ObservedAt.Equal(observedAt) {
		t.Fatalf("publication must preserve the original successful observation: %+v", got)
	}
	replica := got.Samples[0]
	replica.Role, replica.Pod, replica.ObservedAt, replica.AgeNanos = "replica", "replica-1", time.Now(), 0
	point := MeasureRecovery(*got, RecoveryReport{Samples: []RecoverySample{replica}}, "archive", 5*time.Minute, 0, 0)
	if point.State != "unknown" {
		t.Fatalf("slow collection made expired evidence pass recovery gates: %+v; sample=%+v", point, got.Samples[0])
	}
	if db := status.DB[app.Namespace+"/"+app.Spec.Database]; db["systemIdentifier"] != nil || db["timelineID"] != nil || db["recovering"] != nil {
		t.Fatalf("expired recovery evidence supplied current database identity: %+v", db)
	}
	a.recoveries[key].report.Samples[0].ObservedAt = time.Now()
	a.Client = c
	status, err = a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if db := status.DB[app.Namespace+"/"+app.Spec.Database]; db["systemIdentifier"] != "system" || db["timelineID"] != uint32(1) || db["recovering"] != false {
		t.Fatalf("fresh verified recovery evidence did not supply database identity: %+v", db)
	}
	pod.Labels["cnpg.io/instanceRole"] = "replica"
	got = a.recovery(app, db, []corev1.Pod{pod}, "owner")
	if len(got.Samples) != 0 || got.Error == "" {
		t.Fatalf("role change must invalidate previous primary history: %+v", got)
	}
	got = a.recovery(app, db, []corev1.Pod{pod}, "different-cluster")
	if got.Error == "" {
		t.Fatal("a pod owned by another CNPG Cluster cannot supply telemetry")
	}
}

// What a site says of an app: its role from Git, the archive its database was built for, and a demotion
// token only when it is the old primary's new one.
func TestReportApp(t *testing.T) {
	app := sampleApp()
	app.Spec.Primary, app.Spec.Handover = "friend", &v1alpha1.Handover{ID: "m", From: "vince"}
	RememberDemotion(app, "vince", dbReport(true, "old", ""))
	if r := ReportApp(app, "vince", dbReport(false, "old", ""), "docs-db-vince"); r.Active != "friend" || r.Archive != "docs-db-vince" || r.Demoted != nil {
		t.Fatalf("a leftover token is never handed over: %+v", r)
	}
	if r := ReportApp(app, "vince", dbReport(false, "new", ""), ""); r.Demoted == nil || *r.Demoted != (Demotion{Handover: "m", Token: "new"}) {
		t.Fatalf("the new one is: %+v", r.Demoted)
	}
	if r := ReportApp(app, "friend", dbReport(false, "new", ""), ""); r.Demoted != nil || r.Active != "vince" {
		t.Fatalf("only the old primary hands over: %+v", r)
	}
}

// /status names the writer this site's own copy of the Fabric names, ahead of what Flux applied; the
// cluster's copy only while the Git copy is empty. A copy that stalls does not hold /status up.
func TestStatusWriterFromGit(t *testing.T) {
	cluster := &corev1.ConfigMap{Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": "pub", "epoch": "1"}}
	cluster.Name, cluster.Namespace = SettingsName, SystemNS
	cp := &fakeCopy{head: "1111111111111111111111111111111111111111", exists: true, files: map[string]string{SettingsPath: settingsFile(t, "home", 2, "")}}
	var stall atomic.Bool
	fj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stall.Load() {
			<-r.Context().Done()
			return
		}
		cp.ServeHTTP(w, r)
	}))
	defer fj.Close()
	a := &SiteAgent{Client: fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cluster).Build(), Site: "home",
		Git: &fabric.Git{URL: fj.URL, Token: "t", Repo: "fabric/fabric", HTTP: fj.Client()}}
	if st, err := a.Status(context.Background()); err != nil || st.Writer != "home" || st.Epoch != 2 {
		t.Fatalf("the Git copy's claim: %+v %v", st, err)
	}
	stall.Store(true)
	start := time.Now()
	if st, err := a.Status(context.Background()); err != nil || st.Writer != "home" || st.Epoch != 2 || time.Since(start) > 2*time.Second {
		t.Fatalf("a stalled copy: its last claim, at once: %+v %v after %v", st, err, time.Since(start))
	}
	stall.Store(false)
	cp.mu.Lock()
	cp.head, cp.files = "", nil
	cp.mu.Unlock()
	if st, err := a.Status(context.Background()); err != nil || st.Writer != "pub" || st.Epoch != 1 {
		t.Fatalf("an empty copy: the cluster's claim: %+v %v", st, err)
	}
}
