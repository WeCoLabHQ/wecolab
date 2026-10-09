package warden

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
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
	Client        client.Reader // this site's own API, read directly
	Site          string
	SchemaVersion string      // Warden binary schema version, distinct from the node's kubelet version.
	Git           *fabric.Git // this site's copy of the Fabric; nil without one

	mu      sync.Mutex
	cached  []byte
	at      time.Time
	refresh *statusRefresh
	claim   atomic.Pointer[Claim] // the copy's last answer to GitClaim

	vaultMu sync.Mutex
	vaults  map[string]*vaultAnswer // "<ns>/<archive>": a new archive generation is a new folder

	recoveryMu sync.Mutex
	recoveries map[string]*recoveryAnswer
}

type statusRefresh struct {
	done chan struct{}
	body []byte
	err  error
}

type vaultAnswer struct {
	st   *VaultStatus
	at   time.Time
	busy bool
}

type recoveryAnswer struct {
	report                  RecoveryReport
	at                      time.Time
	pod, role, expectedRole string
	busy                    bool
}

// vaultEvery is how often the primary looks at a database's vault. Base backups are daily and
// the archive is judged by its newest WAL, so minutes are plenty; it also keeps B2's daily free
// transactions for the rest of the fabric.
const vaultEvery = 5 * time.Minute

// vault is the last look at this site's archive of the app's database, refreshed in the background so
// /status never waits on the object store. Nil until the first look finishes.
func (a *SiteAgent) vault(app *v1alpha1.App, sec *corev1.Secret) *VaultStatus {
	ns, db, server := app.Namespace, app.Spec.Database, ArchiveName(app, a.Site)
	key := ns + "/" + server + "/" + app.Spec.ArchiveID + "/" + a.Site
	// An archive survives key rotation; a cached inspection made with an old
	// credential must not be reused after the local Secret changes.
	if sec != nil {
		digest := sha256.New()
		digest.Write(sec.Data["b2-key-id"])
		digest.Write([]byte{0})
		digest.Write(sec.Data["b2-key"])
		key += fmt.Sprintf("/%s/%x", sec.Data["key-version"], digest.Sum(nil))
	} else {
		key += "/credential-unavailable"
	}
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

// recovery starts one bounded exporter scrape per database at most every
// controller cadence, without putting network I/O on /status' critical path.
// A failed scrape is published as an error, never as the previous good sample.
func (a *SiteAgent) recovery(app *v1alpha1.App, db map[string]any, pods []corev1.Pod, owner types.UID) *RecoveryReport {
	key := app.Namespace + "/" + app.Spec.Database + "/" + app.Spec.ArchiveID + "/" + ArchiveName(app, a.Site)
	var selected *corev1.Pod
	primary := str(db, "currentPrimary")
	expectedRole := "replica"
	if RoleAt(app.Spec, a.Site).Primary == a.Site {
		expectedRole = "primary"
	}
	for i := range pods {
		p := &pods[i]
		if p.Namespace != app.Namespace || p.Labels["cnpg.io/cluster"] != app.Spec.Database ||
			p.Labels["app.kubernetes.io/managed-by"] != "cloudnative-pg" ||
			p.Labels["app.kubernetes.io/component"] != "database" ||
			p.Labels["cnpg.io/instanceName"] != p.Name ||
			p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		owned := false
		for _, ref := range p.OwnerReferences {
			if ref.APIVersion == "postgresql.cnpg.io/v1" && ref.Kind == "Cluster" &&
				ref.Name == app.Spec.Database && ref.UID == owner && owner != "" {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		role := p.Labels["cnpg.io/instanceRole"]
		if p.Name != primary || role != "primary" && role != "replica" {
			continue
		}
		selected = p
		break
	}
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	if a.recoveries == nil {
		a.recoveries = map[string]*recoveryAnswer{}
	}
	r := a.recoveries[key]
	if r == nil {
		r = &recoveryAnswer{}
		a.recoveries[key] = r
	}
	now := time.Now()
	if selected == nil {
		r.report.Error = "No CNPG instance pod with a verified role and address"
	} else if r.pod != selected.Name || r.role != selected.Labels["cnpg.io/instanceRole"] || r.expectedRole != expectedRole {
		r.report = RecoveryReport{Error: "CNPG instance role changed; awaiting fresh observation"}
		r.pod, r.role, r.expectedRole, r.at, r.busy = selected.Name, selected.Labels["cnpg.io/instanceRole"], expectedRole, time.Time{}, false
	}
	if selected != nil && !r.busy && (r.at.IsZero() || now.Sub(r.at) >= recoveryInterval) {
		r.busy = true
		pod := *selected
		archiveID := app.Spec.ArchiveID
		go func() {
			s, err := scrapeRecovery(context.Background(), pod.Status.PodIP)
			at := time.Now()
			a.recoveryMu.Lock()
			defer a.recoveryMu.Unlock()
			if r.pod != pod.Name || r.role != pod.Labels["cnpg.io/instanceRole"] || r.expectedRole != expectedRole {
				return
			}
			r.busy, r.at = false, at
			if err != nil {
				if r.report.Error != recoveryRegression {
					r.report.Error = err.Error()
				}
				return
			}
			s.Pod, s.ArchiveID, s.ObservedAt = pod.Name, archiveID, at
			if s.Role != expectedRole || pod.Name != primary {
				if r.report.Error != recoveryRegression {
					r.report.Error = "Exporter role disagrees with desired PostgreSQL role or CNPG primary pod"
				}
				return
			}
			r.report.add(s, at)
		}()
	}
	out := r.report
	out.Samples = append([]RecoverySample(nil), r.report.Samples...)
	return &out
}

type SiteStatus struct {
	Site string    `json:"site"`
	Time time.Time `json:"time"`
	// ReceivedAt is the local monotonic receipt of this report. A wire timestamp
	// cannot establish freshness across independently clocked sites.
	ReceivedAt    time.Time                 `json:"-"`
	Version       string                    `json:"version"`
	SchemaVersion string                    `json:"schemaVersion"`
	Nodes         []NodeStatus              `json:"nodes"`
	Allocatable   map[string]string         `json:"allocatable"`
	Requested     map[string]string         `json:"requested"`
	DB            map[string]map[string]any `json:"db"`        // "<ns>/<cluster>": CloudNativePG .status
	Workloads     map[string]WorkloadStatus `json:"workloads"` // "<ns>/<deployment>", project namespaces only
	Volumes       map[string]VolumeStatus   `json:"volumes"`   // "<ns>/<claim>", claims labeled wecolab.io/app
	Apps          map[string]AppState       `json:"apps"`      // "<ns>/<app>": the role this site applied, and what it saw
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
	Kata        bool              `json:"kata"` // Kata Containers is installed and ready: a workspace may run here
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
	st := &SiteStatus{Site: a.Site, Time: time.Now().UTC(), SchemaVersion: a.SchemaVersion, DB: map[string]map[string]any{}, Workloads: map[string]WorkloadStatus{}, Volumes: map[string]VolumeStatus{}, Apps: map[string]AppState{}}
	nodes := &corev1.NodeList{}
	pods := &corev1.PodList{}
	dbs := &unstructured.UnstructuredList{}
	dbs.SetGroupVersionKind(gvkCNPGCluster)
	tenants := &corev1.NamespaceList{}
	deps := &appsv1.DeploymentList{}
	pvcs := &corev1.PersistentVolumeClaimList{}
	apps := &v1alpha1.AppList{}
	// Independent API latency must not accumulate across the whole inventory.
	// Bound pressure on the API server and publish only after reads finish.
	reads, readCtx := errgroup.WithContext(ctx)
	reads.SetLimit(4)
	reads.Go(func() error { return a.Client.List(readCtx, nodes) })
	reads.Go(func() error { return a.Client.List(readCtx, pods) })
	reads.Go(func() error {
		if err := a.Client.List(readCtx, dbs); err != nil { // absent operator: no databases
			dbs.Items = nil
		}
		return nil
	})
	reads.Go(func() error {
		if err := a.Client.List(readCtx, tenants, client.HasLabels{"wecolab.io/tenant"}); err != nil {
			return err
		}
		if len(tenants.Items) != 0 {
			return a.Client.List(readCtx, deps)
		}
		return nil
	})
	reads.Go(func() error { return a.Client.List(readCtx, pvcs, client.HasLabels{"wecolab.io/app"}) })
	reads.Go(func() error { return a.Client.List(readCtx, apps) })
	if err := reads.Wait(); err != nil {
		return nil, err
	}
	alloc := corev1.ResourceList{}
	for _, n := range nodes.Items {
		ns := NodeStatus{Name: n.Name, Laptop: n.Labels["wecolab.io/laptop"] == "true", Kata: n.Labels["katacontainers.io/kata-runtime"] == "true", Idle: true, Allocatable: map[string]string{}}
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

	built, owners := map[string]string{}, map[string]types.UID{} // "<ns>/<cluster>"
	for i, d := range dbs.Items {
		if s, ok := d.Object["status"].(map[string]any); ok {
			st.DB[d.GetNamespace()+"/"+d.GetName()] = s
		}
		built[d.GetNamespace()+"/"+d.GetName()] = serverName(&dbs.Items[i])
		owners[d.GetNamespace()+"/"+d.GetName()] = d.GetUID()
	}
	if len(tenants.Items) != 0 {
		tenantNames := make(map[string]struct{}, len(tenants.Items))
		for _, ns := range tenants.Items {
			tenantNames[ns.Name] = struct{}{}
		}
		for _, d := range deps.Items {
			if _, tenant := tenantNames[d.Namespace]; !tenant {
				continue
			}
			w := WorkloadStatus{Ready: d.Status.ReadyReplicas}
			if d.Spec.Replicas != nil {
				w.Replicas = *d.Spec.Replicas
			}
			st.Workloads[d.Namespace+"/"+d.Name] = w
		}
	}
	for _, p := range pvcs.Items {
		q, want := p.Status.Capacity[corev1.ResourceStorage], p.Spec.Resources.Requests[corev1.ResourceStorage]
		st.Volumes[p.Namespace+"/"+p.Name] = VolumeStatus{App: p.Labels["wecolab.io/app"], Request: want.String(), Phase: string(p.Status.Phase), Capacity: q.String()}
	}
	// Read each project's app resources in batches. Per-app Get calls can
	// exhaust the API rate limit and make /status miss its peers' deadline.
	services := make(map[types.NamespacedName]*corev1.Service, len(apps.Items))
	secrets := make(map[types.NamespacedName]*corev1.Secret, len(apps.Items))
	namespaces := map[string]bool{}
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.Deleted || !slices.Contains(app.Spec.Sites, a.Site) {
			continue
		}
		namespaces[app.Namespace] = namespaces[app.Namespace] || app.Spec.Database != ""
		services[types.NamespacedName{Namespace: app.Namespace, Name: app.Spec.Workload}] = nil
		if app.Spec.Database != "" {
			secrets[types.NamespacedName{Namespace: app.Namespace, Name: app.Name}] = nil
		}
	}
	resources := make([]struct {
		services corev1.ServiceList
		secrets  corev1.SecretList
	}, len(namespaces))
	var resourceReads errgroup.Group
	resourceReads.SetLimit(4)
	i := 0
	for ns, database := range namespaces {
		batch := &resources[i]
		i++
		resourceReads.Go(func() error {
			if err := a.Client.List(ctx, &batch.services, client.InNamespace(ns)); err != nil {
				batch.services.Items = nil // no route evidence from a failed read
			}
			return nil
		})
		if database {
			resourceReads.Go(func() error {
				if err := a.Client.List(ctx, &batch.secrets, client.InNamespace(ns)); err != nil {
					batch.secrets.Items = nil // no vault evidence from a failed read
				}
				return nil
			})
		}
	}
	_ = resourceReads.Wait()
	for j := range resources {
		batch := &resources[j]
		for i := range batch.services.Items {
			svc := &batch.services.Items[i]
			key := client.ObjectKeyFromObject(svc)
			if _, needed := services[key]; needed {
				services[key] = svc
			}
		}
		for i := range batch.secrets.Items {
			sec := &batch.secrets.Items[i]
			key := client.ObjectKeyFromObject(sec)
			if _, needed := secrets[key]; needed {
				secrets[key] = sec
			}
		}
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
		if app.Spec.Database != "" {
			if s.Archive != "" && s.Archive != ArchiveName(&app, a.Site) {
				s.Recovery = &RecoveryReport{Error: "Database belongs to a previous archive generation"}
			} else {
				s.Recovery = a.recovery(&app, st.DB[dbKey], pods.Items, owners[dbKey])
			}
		}
		sec := secrets[types.NamespacedName{Namespace: app.Namespace, Name: app.Name}]
		if app.Spec.Database != "" && Serving(app.Spec) == a.Site {
			s.Vault = a.vault(&app, sec)
		}
		if sec != nil {
			s.VaultKeyID = string(sec.Data["b2-key-id"])
			s.VaultKeyVersion = string(sec.Data["key-version"])
		}
		s.Endpoints = appEndpoints(services[types.NamespacedName{Namespace: app.Namespace, Name: app.Spec.Workload}], pods.Items, nodeIP)
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
	now := time.Now()
	// Include all inventory I/O in producer age. A slow later read must not
	// make an earlier sample appear fresh when peers receive this report.
	for _, app := range apps.Items {
		s := st.Apps[app.Namespace+"/"+app.Name]
		if s.Recovery == nil {
			continue
		}
		for i := range s.Recovery.Samples {
			sample := &s.Recovery.Samples[i]
			age := now.Sub(sample.ObservedAt)
			if age < 0 {
				s.Recovery.Error = "Monotonic sample age invalid"
				break
			}
			sample.AgeNanos = int64(age)
		}
		if s.Recovery.Error == "" && len(s.Recovery.Samples) != 0 {
			sample := s.Recovery.Samples[len(s.Recovery.Samples)-1]
			if (sample.Role == "primary" || sample.Role == "replica") &&
				validRecoverySample(sample, app.Spec.ArchiveID, sample.Role) &&
				sampleAge(sample, 0) >= 0 && sampleAge(sample, 0) <= recoveryMaxReplayAge {
				dbKey := app.Namespace + "/" + app.Spec.Database
				if st.DB[dbKey] == nil {
					st.DB[dbKey] = map[string]any{}
				}
				st.DB[dbKey]["systemIdentifier"] = sample.SystemID
				st.DB[dbKey]["timelineID"] = sample.Timeline
				st.DB[dbKey]["recovering"] = sample.Role == "replica"
			}
		}
	}
	st.ReceivedAt = now
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

// appEndpoints are where the Door reaches an app here: the Nebula address of every box running a ready
// pod behind the app's Service, with the Service's node port. The Service keeps the caller's address
// (externalTrafficPolicy Local), so it answers only on those boxes.
func appEndpoints(svc *corev1.Service, pods []corev1.Pod, nodeIP map[string]string) []string {
	if svc == nil || svc.Spec.Type != corev1.ServiceTypeNodePort || len(svc.Spec.Ports) == 0 {
		return nil
	}
	port := svc.Spec.Ports[0].NodePort
	sel := labels.SelectorFromSet(svc.Spec.Selector)
	out := []string{}
	for _, p := range pods {
		if p.Namespace != svc.Namespace || !sel.Matches(labels.Set(p.Labels)) || p.Spec.NodeName == "" || !podReady(p) {
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
	body := a.cached
	if time.Since(a.at) > 5*time.Second || body == nil {
		refresh := a.refresh
		if refresh == nil {
			refresh = &statusRefresh{done: make(chan struct{})}
			a.refresh = refresh
			go func() {
				// A short-lived peer must not repeatedly cancel a slower
				// inventory. Share one bounded collection, not stale success.
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				st, err := a.Status(ctx)
				if err == nil {
					refresh.body, err = json.Marshal(st)
				}
				refresh.err = err
				a.mu.Lock()
				if err == nil {
					a.cached, a.at = refresh.body, time.Now()
				}
				a.refresh = nil
				close(refresh.done)
				a.mu.Unlock()
			}()
		}
		a.mu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-refresh.done:
		}
		if refresh.err != nil {
			http.Error(w, refresh.err.Error(), http.StatusInternalServerError)
			return
		}
		body = refresh.body
	} else {
		a.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
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
