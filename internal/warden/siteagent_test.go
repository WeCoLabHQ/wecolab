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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
			if v := a.vault(app); v != nil {
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
	if v := a.vault(app); v != nil {
		t.Fatalf("the new archive's first look is its own, not the old folder's: %+v", v)
	}
	app.Spec.Archive = nil
	app.Spec.ArchiveID = "new-identity"
	if v := a.vault(app); v != nil {
		t.Fatalf("a recreated database with the same archive label cannot reuse old vault evidence: %+v", v)
	}
}

// Report age increases from the producer's receipt; pod role changes invalidate it.
func TestRecoveryPodSelectionAndReceiptAge(t *testing.T) {
	app := sampleApp()
	app.Spec.ArchiveID = "archive"
	a := &SiteAgent{Site: "vince"}
	now := time.Now()
	key := app.Namespace + "/" + app.Spec.Database + "/archive/" + ArchiveName(app, "vince")
	a.recoveries = map[string]*recoveryAnswer{key: {pod: "docs-db-1", role: "primary", expectedRole: "primary", at: now, report: RecoveryReport{Samples: []RecoverySample{
		{ArchiveID: "archive", SystemID: "system", Timeline: 1, Pod: "docs-db-1", Role: "primary", LSNLow: 1, ObservedAt: now.Add(-time.Second)},
	}}}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "docs-db-1", Namespace: app.Namespace, Labels: map[string]string{
		"cnpg.io/cluster": app.Spec.Database, "app.kubernetes.io/managed-by": "cloudnative-pg",
		"app.kubernetes.io/component": "database", "cnpg.io/instanceName": "docs-db-1", "cnpg.io/instanceRole": "primary",
	}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: app.Spec.Database, UID: "owner"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "invalid-ip"}}
	db := map[string]any{"currentPrimary": "docs-db-1"}
	got := a.recovery(app, db, []corev1.Pod{pod}, "owner")
	if got.Error != "" || len(got.Samples) != 1 || got.Samples[0].AgeNanos < int64(time.Second) {
		t.Fatalf("sample receipt age must increase without resampling: %+v", got)
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
