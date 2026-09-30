package warden

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// SiteAgent publishes what is true at this site, on its manager's Nebula address, for the other
// sites, the Door and the Console: its nodes and capacity, and for every app here its database, its
// workload, its volumes, the role it applied from Git and where it answers. Database status is
// CloudNativePG's own .status.
type SiteAgent struct {
	Client client.Reader // this site's own API, read directly
	Site   string
	Git    *fabric.Git // this site's copy of the Fabric; nil without one

	mu     sync.Mutex
	cached []byte
	at     time.Time
	claim  atomic.Pointer[Claim] // the copy's last answer to GitClaim

	vaultMu sync.Mutex
	vaults  map[string]*vaultAnswer // "<ns>/<archive>": a new archive generation is a new folder
}

type vaultAnswer struct {
	st   *VaultStatus
	at   time.Time
	busy bool
}

// vaultEvery is how often the primary looks at a database's vault. Base backups are daily and
// the archive is judged by its newest WAL, so minutes are plenty; it also keeps B2's daily free
// transactions for the rest of the fabric.
const vaultEvery = 5 * time.Minute

// vault is the last look at this site's archive of the app's database, refreshed in the background so
// /status never waits on the object store. Nil until the first look finishes.
func (a *SiteAgent) vault(app *v1alpha1.App) *VaultStatus {
	ns, db, server := app.Namespace, app.Spec.Database, ArchiveName(app, a.Site)
	key := ns + "/" + server
	a.vaultMu.Lock()
	defer a.vaultMu.Unlock()
	if a.vaults == nil {
		a.vaults = map[string]*vaultAnswer{}
	}
	v := a.vaults[key]
	if v == nil {
		v = &vaultAnswer{}
		a.vaults[key] = v
	}
	every := vaultEvery
	if v.st != nil && v.st.Err != "" {
		every = 30 * time.Second // a failed look is retried soon
	}
	if !v.busy && time.Since(v.at) > every {
		v.busy = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			st := VaultOf(ctx, a.Client, ns, db, server)
			a.vaultMu.Lock()
			v.st, v.at, v.busy = &st, time.Now(), false
			a.vaultMu.Unlock()
		}()
	}
	return v.st
}

type SiteStatus struct {
	Site        string                    `json:"site"`
	Time        time.Time                 `json:"time"`
	Version     string                    `json:"version"`
	Nodes       []NodeStatus              `json:"nodes"`
	Allocatable map[string]string         `json:"allocatable"`
	Requested   map[string]string         `json:"requested"`
	DB          map[string]map[string]any `json:"db"`        // "<ns>/<cluster>": CloudNativePG .status
	Workloads   map[string]WorkloadStatus `json:"workloads"` // "<ns>/<deployment>", project namespaces only
	Volumes     map[string]VolumeStatus   `json:"volumes"`   // "<ns>/<claim>", claims labeled wecolab.io/app
	Apps        map[string]AppState       `json:"apps"`      // "<ns>/<app>": the role this site applied, and what it saw
	// Writer and Epoch are who this site's own copy of the Fabric names the writer (GitClaim): a steward
	// that took over publishes its higher epoch here at once, before Flux applies it anywhere.
	Writer string `json:"writer"`
	Epoch  int    `json:"epoch"`
}

type NodeStatus struct {
	Name        string            `json:"name"`
	Ready       bool              `json:"ready"`
	Laptop      bool              `json:"laptop"`
	Idle        bool              `json:"idle"` // a laptop whose person is away: best-effort work may run
	Allocatable map[string]string `json:"allocatable"`
}

type WorkloadStatus struct {
	Replicas int32 `json:"replicas"`
	Ready    int32 `json:"ready"`
}

type VolumeStatus struct {
	App      string `json:"app"` // its wecolab.io/app label
	Request  string `json:"request"`
	Phase    string `json:"phase"`
	Capacity string `json:"capacity"`
}

// Status reads the site's state now.
func (a *SiteAgent) Status(ctx context.Context) (*SiteStatus, error) {
	st := &SiteStatus{Site: a.Site, Time: time.Now().UTC(), DB: map[string]map[string]any{}, Workloads: map[string]WorkloadStatus{}, Volumes: map[string]VolumeStatus{}, Apps: map[string]AppState{}}
	nodes := &corev1.NodeList{}
	if err := a.Client.List(ctx, nodes); err != nil {
		return nil, err
	}
	alloc := corev1.ResourceList{}
	for _, n := range nodes.Items {
		ns := NodeStatus{Name: n.Name, Laptop: n.Labels["wecolab.io/laptop"] == "true", Idle: true, Allocatable: map[string]string{}}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ns.Ready = c.Status == corev1.ConditionTrue
			}
		}
		for _, t := range n.Spec.Taints {
			if t.Key == "wecolab.io/idle" {
				ns.Idle = false // the taint is present while the person is at the laptop
			}
		}
		if !ns.Laptop {
			ns.Idle = false
		}
		for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			q := n.Status.Allocatable[r]
			ns.Allocatable[string(r)] = q.String()
			if ns.Ready {
				sum := alloc[r]
				sum.Add(q)
				alloc[r] = sum
			}
		}
		if st.Version == "" {
			st.Version = n.Status.NodeInfo.KubeletVersion
		}
		st.Nodes = append(st.Nodes, ns)
	}
	st.Allocatable = quantities(alloc)
	pods := &corev1.PodList{}
	if err := a.Client.List(ctx, pods); err != nil {
		return nil, err
	}
	req := corev1.ResourceList{}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || p.Spec.NodeName == "" {
			continue
		}
		for _, c := range p.Spec.Containers {
			for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
				if q, ok := c.Resources.Requests[r]; ok {
					sum := req[r]
					sum.Add(q)
					req[r] = sum
				}
			}
		}
	}
	st.Requested = quantities(req)

	dbs := &unstructured.UnstructuredList{}
	dbs.SetGroupVersionKind(gvkCNPGCluster)
	built := map[string]string{}                    // "<ns>/<cluster>": the archive it was built for
	if err := a.Client.List(ctx, dbs); err == nil { // absent operator: no databases
		for i, d := range dbs.Items {
			if s, ok := d.Object["status"].(map[string]any); ok {
				st.DB[d.GetNamespace()+"/"+d.GetName()] = s
			}
			built[d.GetNamespace()+"/"+d.GetName()] = serverName(&dbs.Items[i])
		}
	}
	tenants := &corev1.NamespaceList{}
	if err := a.Client.List(ctx, tenants, client.HasLabels{"wecolab.io/tenant"}); err != nil {
		return nil, err
	}
	for _, ns := range tenants.Items {
		deps := &appsv1.DeploymentList{}
		if err := a.Client.List(ctx, deps, client.InNamespace(ns.Name)); err != nil {
			return nil, err
		}
		for _, d := range deps.Items {
			w := WorkloadStatus{Ready: d.Status.ReadyReplicas}
			if d.Spec.Replicas != nil {
				w.Replicas = *d.Spec.Replicas
			}
			st.Workloads[d.Namespace+"/"+d.Name] = w
		}
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := a.Client.List(ctx, pvcs, client.HasLabels{"wecolab.io/app"}); err != nil {
		return nil, err
	}
	for _, p := range pvcs.Items {
		q, want := p.Status.Capacity[corev1.ResourceStorage], p.Spec.Resources.Requests[corev1.ResourceStorage]
		st.Volumes[p.Namespace+"/"+p.Name] = VolumeStatus{App: p.Labels["wecolab.io/app"], Request: want.String(), Phase: string(p.Status.Phase), Capacity: q.String()}
	}
	apps := &v1alpha1.AppList{}
	if err := a.Client.List(ctx, apps); err != nil {
		return nil, err
	}
	nodeIP := map[string]string{}
	for _, n := range nodes.Items {
		for _, ad := range n.Status.Addresses {
			if ad.Type == corev1.NodeInternalIP {
				nodeIP[n.Name] = ad.Address
			}
		}
	}
	for _, app := range apps.Items {
		if !slices.Contains(app.Spec.Sites, a.Site) {
			continue
		}
		dbKey := app.Namespace + "/" + app.Spec.Database
		if app.Spec.Deleted {
			if a.holds(ctx, &app, built[dbKey] != "") {
				st.Apps[app.Namespace+"/"+app.Name] = AppState{Deleting: true}
			}
			continue
		}
		s := ReportApp(&app, a.Site, st.DB[dbKey], built[dbKey])
		if app.Spec.Database != "" && Serving(app.Spec) == a.Site {
			s.Vault = a.vault(&app)
		}
		s.Endpoints = a.endpoints(ctx, app.Namespace, app.Spec.Workload, pods.Items, nodeIP)
		st.Apps[app.Namespace+"/"+app.Name] = s
	}
	own := a.gitClaim(ctx)
	if own.Writer == "" { // no copy, or an empty one: what Flux applied last
		if set, err := ReadSettings(ctx, a.Client); err == nil {
			own = Claim{Writer: set.Writer, Epoch: set.Epoch}
		}
	}
	if err := ctx.Err(); err != nil { // the caller left: the claims above may be missing, so cache nothing
		return nil, err
	}
	st.Writer, st.Epoch = own.Writer, own.Epoch
	return st, nil
}

// gitClaim is who this site's copy of the Fabric names the writer, waiting at most a second: every
// peer's /status queues behind this one, and a slow Forgejo must not make the site look gone to the
// Door. A copy that does not answer in time gets its last answer.
func (a *SiteAgent) gitClaim(ctx context.Context) Claim {
	if a.Git == nil {
		return Claim{}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if c, err := GitClaim(ctx, a.Git); err == nil {
		a.claim.Store(&c)
	}
	if c := a.claim.Load(); c != nil {
		return *c
	}
	return Claim{}
}

// ReportApp is what this site says of an app from the App as it has it (spec from Git, status its own),
// its database's CloudNativePG status and the archive that database was built for.
func ReportApp(app *v1alpha1.App, self string, db map[string]any, built string) AppState {
	s := AppState{Active: RoleAt(app.Spec, self).Primary, Archive: built, Demoted: Demoted(app, self, db)}
	if c := meta.FindStatusCondition(app.Status.Conditions, v1alpha1.CondPromotion); c != nil {
		s.Promotion = c
	}
	return s
}

// endpoints are where the Door reaches an app here: the Nebula address of every box running a ready
// pod behind the app's Service, with the Service's node port. The Service keeps the caller's address
// (externalTrafficPolicy Local), so it answers only on those boxes.
func (a *SiteAgent) endpoints(ctx context.Context, ns, workload string, pods []corev1.Pod, nodeIP map[string]string) []string {
	svc := &corev1.Service{}
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: workload}, svc); err != nil || svc.Spec.Type != corev1.ServiceTypeNodePort || len(svc.Spec.Ports) == 0 {
		return nil
	}
	port := svc.Spec.Ports[0].NodePort
	sel := labels.SelectorFromSet(svc.Spec.Selector)
	out := []string{}
	for _, p := range pods {
		if p.Namespace != ns || !sel.Matches(labels.Set(p.Labels)) || p.Spec.NodeName == "" || !podReady(p) {
			continue
		}
		if ip := nodeIP[p.Spec.NodeName]; ip != "" {
			if e := fmt.Sprintf("%s:%d", ip, port); !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	slices.Sort(out)
	return out
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func quantities(l corev1.ResourceList) map[string]string {
	out := map[string]string{}
	for k, v := range l {
		q := v.DeepCopy()
		if k == corev1.ResourceMemory {
			q = *resource.NewQuantity(q.Value(), resource.BinarySI)
		}
		out[string(k)] = q.String()
	}
	return out
}

// ServeHTTP answers GET /status, computed at most every five seconds.
func (a *SiteAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusNoContent)
		return
	case "/status":
	default:
		http.NotFound(w, r)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.at) > 5*time.Second || a.cached == nil {
		st, err := a.Status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.cached, _ = json.Marshal(st)
		a.at = time.Now()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(a.cached)
}

// holds is whether anything of an app is still at this site: its database, or the Kustomization that
// delivers it (Warden deletes that last). A lookup that fails counts as still here.
func (a *SiteAgent) holds(ctx context.Context, app *v1alpha1.App, db bool) bool {
	if db {
		return true
	}
	k := &unstructured.Unstructured{}
	k.SetGroupVersionKind(gvkKustomization)
	err := a.Client.Get(ctx, types.NamespacedName{Namespace: FluxNS, Name: kustomizationName(app.Namespace, app.Name)}, k)
	return !apierrors.IsNotFound(err)
}
