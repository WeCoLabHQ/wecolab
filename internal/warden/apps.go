package warden

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// applierRole is what Flux may create in a project's namespace (system/wecolab/rbac.yaml).
const applierRole = "wecolab-apply"

// AppReconciler applies the apps placed at this site. For each one it derives this site's role from the
// App's spec in Git (RoleAt), keeps what it must remember in the App's status here, and hands Flux one
// Kustomization: the app's folder in the Fabric, applied as the project (never as the cluster's admin),
// with this site's role and the project's standing here as patches.
type AppReconciler struct {
	client.Client
	Site   string
	Peers  *Peers
	Resync time.Duration
}

func kustomizationName(ns, app string) string { return "app-" + ns + "-" + app }

func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)
	app := &v1alpha1.App{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		if apierrors.IsNotFound(err) { // removed from the Fabric: it leaves this site, all but the history
			return ctrl.Result{}, r.drop(ctx, req.Namespace, req.Name, true)
		}
		return ctrl.Result{}, err
	}
	if app.Spec.Deleted { // a person deleted it: everything here goes, data included
		return ctrl.Result{}, r.drop(ctx, app.Namespace, app.Name, false)
	}
	if !slices.Contains(app.Spec.Sites, r.Site) {
		if err := MayDestroy(app, r.Site, r.report(ctx, app.Spec.Primary)); err != nil {
			lg.Info("the app left this site; its database stays until the primary proves it holds the data", "why", err.Error())
			return ctrl.Result{RequeueAfter: r.Resync}, nil
		}
		return ctrl.Result{}, r.drop(ctx, app.Namespace, app.Name, false)
	}

	now := time.Now()
	here, local, err := r.here(ctx, app)
	if err != nil {
		return ctrl.Result{}, err
	}
	var primary *SiteStatus
	if here.Exists && here.Archive != "" && here.Archive != ArchiveName(app, r.Site) { // a rebuild needs the primary's word
		primary = r.report(ctx, app.Spec.Primary)
	}
	p := PlanAt(app, r.Site, here, primary)
	app.Status.Active = RoleAt(app.Spec, r.Site).Primary
	if local != nil {
		RememberDemotion(app, r.Site, dbStatus(local))
	}
	SetCondition(&app.Status.Conditions, p.Promotion, now, app.Generation)
	// Kept before it is acted on: the old primary's leftover token must survive a restart. An App CRD
	// older than this Warden drops status.demoting on write, so nothing is acted on until it has it.
	demoting := app.Status.Demoting
	if err := r.Status().Update(ctx, app); err != nil {
		return ctrl.Result{}, err
	}
	if app.Status.Demoting != demoting {
		return ctrl.Result{}, fmt.Errorf("the App CRD here does not keep status.demoting: it needs this Warden's CRDs")
	}
	if err := r.applier(ctx, app.Namespace); err != nil {
		return ctrl.Result{}, err
	}
	st, err := r.standing(ctx, app.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	k := AppKustomization(app, r.Site, p.Local, st)
	if p.Suspend {
		_ = unstructured.SetNestedField(k.Object, true, "spec", "suspend")
	}
	if err := r.Patch(ctx, k, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
		return ctrl.Result{}, fmt.Errorf("kustomization: %w", err)
	}
	if p.Destroy && local.GetDeletionTimestamp() == nil { // suspended first, so Flux leaves it gone
		lg.Info("rebuilding this site's database from the vault", "built", here.Archive, "want", ArchiveName(app, r.Site))
		if err := r.Delete(ctx, local); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	}
	if p.Suspend {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if app.Spec.Primary == r.Site && app.Spec.Handover == nil && str(dbStatus(local), "phase") == cnpgHealthy {
		if err := r.ensureBaseBackup(ctx, app, now); err != nil {
			lg.Error(err, "base backup")
		}
	}
	return ctrl.Result{RequeueAfter: r.Resync}, nil
}

// report is a site's own status, nil when it did not answer.
func (r *AppReconciler) report(ctx context.Context, site string) *SiteStatus {
	st, _ := r.Peers.Get(ctx, site)
	return st
}

// Here is this site's database as it is.
type Here struct {
	Exists  bool
	Created bool   // Flux made one here before, so a missing one was lost, not never made
	Archive string // the archive it was built for
}

// here is this site's database. When there is none, the app's Kustomization tells a lost one from one
// never made: Flux's inventory still lists what it applied.
func (r *AppReconciler) here(ctx context.Context, app *v1alpha1.App) (Here, *unstructured.Unstructured, error) {
	if app.Spec.Database == "" {
		return Here{}, nil, nil
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvkDBCluster)
	err := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Spec.Database}, u)
	if err == nil {
		return Here{Exists: true, Created: true, Archive: serverName(u)}, u, nil
	}
	if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return Here{}, nil, err
	}
	k := &unstructured.Unstructured{}
	k.SetGroupVersionKind(gvkKustomization)
	if err := r.Get(ctx, types.NamespacedName{Namespace: FluxNS, Name: kustomizationName(app.Namespace, app.Name)}, k); err != nil {
		return Here{}, nil, client.IgnoreNotFound(err)
	}
	id := app.Namespace + "_" + app.Spec.Database + "_postgresql.cnpg.io_Cluster"
	entries, _, _ := unstructured.NestedSlice(k.Object, "status", "inventory", "entries")
	for _, e := range entries {
		if m, _ := e.(map[string]any); m["id"] == id {
			return Here{Created: true}, nil, nil
		}
	}
	return Here{}, nil, nil
}

func dbStatus(u *unstructured.Unstructured) map[string]any {
	if u == nil {
		return nil
	}
	m, _ := u.Object["status"].(map[string]any)
	return m
}

// Plan is what a site does with an app's database now.
type Plan struct {
	Local     Local
	Suspend   bool // Flux leaves the app alone
	Destroy   bool // delete this site's database; once it is gone, Flux makes it again from the vault
	Promotion metav1.Condition
}

// PlanAt decides from the App as Git has it here, this site's database, and the primary's own report
// (nil when it did not answer, or was not needed):
//   - where the app's history is held (Serving) a database that is not there is made only while Git says
//     it is new and Flux never made one here; otherwise it would start empty under the app's history, so
//     the app is held until a person moves the primary;
//   - a database built for an older archive generation than the spec's is rebuilt from the vault once
//     MayDestroy allows; until then it follows its role but keeps writing its own archive, never the
//     new one its rebuilt self will need empty.
func PlanAt(app *v1alpha1.App, self string, here Here, primary *SiteStatus) Plan {
	p := Plan{Local: Local{Created: here.Exists || here.Created}, Promotion: Promotion(app.Spec)}
	if app.Spec.Database == "" {
		return p
	}
	want := ArchiveName(app, self)
	holds := Serving(app.Spec) == self
	switch {
	case holds && !here.Exists && (here.Created || !NewDatabase(app.Spec)):
		p.Suspend = true
		p.Promotion = cond(v1alpha1.CondPromotion, false, "DatabaseMissing",
			self+" holds the app's database and has none; it is never made again empty. Move the primary, forced, to a site that has one")
	case !holds && here.Exists && here.Archive != "" && here.Archive != want:
		if err := MayDestroy(app, self, primary); err != nil {
			p.Local.Archive = here.Archive
			p.Promotion = cond(v1alpha1.CondPromotion, false, "RebuildWaiting", fmt.Sprintf("rebuilt from the vault as %s once safe: %v", want, err))
			break
		}
		p.Suspend, p.Destroy = true, true
		p.Promotion = cond(v1alpha1.CondPromotion, false, "Rebuilding", "rebuilding from the vault as "+want)
	}
	return p
}

// MayDestroy is R4 of docs/plans/2026-09-29-hardening.md, the one guard before a site deletes its copy of
// an app's database, to rebuild it or because the app left the site: nil only on positive proof, from
// Git that this site holds nothing a move still needs, and from the primary that it holds the data. app
// is the App as Git has it here; primary is the primary's own report, nil when it did not answer.
func MayDestroy(app *v1alpha1.App, self string, primary *SiteStatus) error {
	s := app.Spec
	if s.Database == "" {
		return nil
	}
	switch h := s.Handover; {
	case s.Primary == self:
		return fmt.Errorf("Git says %s is the primary", self)
	case h != nil && h.From == self:
		return fmt.Errorf("%s hands over to %s", self, s.Primary)
	case h != nil && h.Token == "":
		return fmt.Errorf("%s has not handed over yet", h.From)
	}
	return proven(app, primary)
}

// proven is nil when the primary, by its own report (nil when it did not answer), holds the app's data:
// it takes itself for the primary, is writable and healthy, promoted with the handover's token if Git has
// one, and has a base backup in its vault.
func proven(app *v1alpha1.App, primary *SiteStatus) error {
	s := app.Spec
	if primary == nil || primary.Site != s.Primary {
		return fmt.Errorf("%s does not answer", s.Primary)
	}
	st, db := primary.Apps[app.Namespace+"/"+app.Name], primary.DB[app.Namespace+"/"+s.Database]
	switch {
	case st.Active != s.Primary || !writable(db) || str(db, "phase") != cnpgHealthy:
		return fmt.Errorf("%s is not a healthy primary yet", s.Primary)
	case s.Handover != nil && str(db, "lastPromotionToken") != s.Handover.Token:
		return fmt.Errorf("%s has not promoted with the handover's token yet", s.Primary)
	case st.Vault == nil || st.Vault.Err != "" || st.Vault.LatestBackup.IsZero():
		// ponytail: any base backup under the primary's current archive counts, even one from before a
		// forced move made it primary again; a backup since its promotion needs the backup's timeline.
		return fmt.Errorf("%s has no base backup in its vault yet", s.Primary)
	}
	return nil
}

// writable says the database's current primary instance has left recovery: a promoted primary, not the
// designated primary of a replica cluster.
func writable(db map[string]any) bool {
	states, _ := db["instancesReportedState"].(map[string]any)
	st, _ := states[str(db, "currentPrimary")].(map[string]any)
	ok, _ := st["isPrimary"].(bool)
	return ok
}

// serverName is the archive a CloudNativePG Cluster was built for (its first plugin's serverName).
func serverName(u *unstructured.Unstructured) string {
	ps, _, _ := unstructured.NestedSlice(u.Object, "spec", "plugins")
	if len(ps) == 0 {
		return ""
	}
	p, _ := ps[0].(map[string]any)
	s, _, _ := unstructured.NestedString(p, "parameters", "serverName")
	return s
}

// drop removes an app from this site, once MayDestroy allows: its data (databases and volumes Flux
// applied for it), then its Kustomization, whose finalizer prunes the rest. When the App itself is gone
// without the deleted mark, Flux pruned it because its file left Git, which deletes no data (decision
// 12): keep then leaves every database and volume here for a person to delete. Volumes are in no vault,
// and a standby's database costs only its disk.
func (r *AppReconciler) drop(ctx context.Context, ns, name string, keep bool) error {
	mine := client.MatchingLabels{"kustomize.toolkit.fluxcd.io/name": kustomizationName(ns, name), "kustomize.toolkit.fluxcd.io/namespace": FluxNS}
	dbs := &unstructured.UnstructuredList{}
	dbs.SetGroupVersionKind(gvkCNPGCluster)
	if err := r.List(ctx, dbs, client.InNamespace(ns), mine); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(ns), mine); err != nil {
		return err
	}
	objs := []client.Object{}
	for i := range dbs.Items {
		objs = append(objs, &dbs.Items[i])
	}
	for i := range pvcs.Items {
		objs = append(objs, &pvcs.Items[i])
	}
	if keep && len(objs) > 0 {
		log.FromContext(ctx).Info("the app left the Fabric without being deleted; its data here stays until a person deletes it", "app", ns+"/"+name)
	}
	for _, o := range objs {
		var err error
		if keep { // the Kustomization's finalizer would prune it with the rest; Flux skips what carries this
			err = r.Patch(ctx, o, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+fluxPrune+`":"disabled"}}}`)))
		} else {
			err = r.Delete(ctx, o)
		}
		if err := client.IgnoreNotFound(err); err != nil {
			return err
		}
	}
	k := &unstructured.Unstructured{}
	k.SetGroupVersionKind(gvkKustomization)
	k.SetNamespace(FluxNS)
	k.SetName(kustomizationName(ns, name))
	return client.IgnoreNotFound(r.Delete(ctx, k))
}

// fluxPrune "disabled" on an object keeps Flux from pruning it, its Kustomization's deletion included.
const fluxPrune = "kustomize.toolkit.fluxcd.io/prune"

// applier is the project's identity for Flux: a ServiceAccount allowed only what a project member
// may create, and only in the project's namespace.
func (r *AppReconciler) applier(ctx context.Context, ns string) error {
	sa := &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-" + ns, Namespace: FluxNS}}
	rb := &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: applierRole, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: applierRole},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa.Name, Namespace: FluxNS}}}
	for _, o := range []client.Object{sa, rb} {
		if err := r.Patch(ctx, o, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
			return fmt.Errorf("applier %s: %w", o.GetName(), err)
		}
	}
	return nil
}

// standing is the project's standing at this site, from the Fabric's sites, offers and pools.
func (r *AppReconciler) standing(ctx context.Context, project string) (Standing, error) {
	sites, offers, pools := &v1alpha1.SiteList{}, &v1alpha1.OfferList{}, &v1alpha1.PoolList{}
	for _, l := range []client.ObjectList{sites, offers, pools} {
		if err := r.List(ctx, l); err != nil {
			return Standing{}, err
		}
	}
	return StandingAt(project, r.Site, sites.Items, offers.Items, pools.Items), nil
}

// AppKustomization is the Flux Kustomization that applies an app's folder at one site, with that
// site's part as JSON patches: its database's place in the topology, backups and the app itself only at
// the primary and never while its database changes hands, its Service reachable from the Door (a node
// port that keeps the caller's address), and, where the project holds best-effort or box-pinned
// capacity here, its pods placed accordingly.
func AppKustomization(app *v1alpha1.App, self string, l Local, st Standing) *unstructured.Unstructured {
	patches := []any{
		jsonPatch("", "Service", app.Spec.Workload, add("/spec/type", "NodePort"), add("/spec/externalTrafficPolicy", "Local")),
	}
	if db := app.Spec.Database; db != "" {
		runs := self == app.Spec.Primary && app.Spec.Handover == nil
		ops := []any{}
		for _, op := range DBPatch(app, self, l) {
			ops = append(ops, op)
		}
		patches = append(patches,
			jsonPatch("postgresql.cnpg.io", "Cluster", db, ops...),
			jsonPatch("postgresql.cnpg.io", "ScheduledBackup", db+"-backup", add("/spec/suspend", !runs)))
		if !runs {
			patches = append(patches, jsonPatch("apps", "Deployment", app.Spec.Workload, add("/spec/replicas", 0)))
		}
	}
	if h := st.Held; h != nil && !st.Owner {
		pod := []any{}
		if h.BestEffort && app.Spec.Database == "" { // databases never land on best-effort capacity
			pod = append(pod,
				add("/spec/template/spec/priorityClassName", BestEffortClass),
				map[string]any{"op": "add", "path": "/spec/template/spec/tolerations", "value": []any{
					map[string]any{"key": "wecolab.io/laptop", "operator": "Exists", "effect": "NoSchedule"},
					map[string]any{"key": "wecolab.io/idle", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": int64(60)},
				}})
		}
		if len(h.Boxes) > 0 {
			boxes := make([]any, 0, len(h.Boxes))
			for _, b := range h.Boxes {
				boxes = append(boxes, b)
			}
			pod = append(pod, map[string]any{"op": "add", "path": "/spec/template/spec/affinity", "value": map[string]any{"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{
					map[string]any{"key": "kubernetes.io/hostname", "operator": "In", "values": boxes}}}}}}}})
		}
		if len(pod) > 0 {
			patches = append(patches, jsonPatch("apps", "Deployment", "", pod...))
		}
	}
	k := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"interval":           "10m",
		"retryInterval":      "1m",
		"path":               "./" + fabric.AppFolder(app.Namespace, app.Name),
		"prune":              true,
		"sourceRef":          map[string]any{"kind": "GitRepository", "name": "fabric"},
		"targetNamespace":    app.Namespace,
		"serviceAccountName": "tenant-" + app.Namespace,
		"decryption":         map[string]any{"provider": "sops", "secretRef": map[string]any{"name": "sops-age"}},
		"dependsOn":          []any{map[string]any{"name": "fabric"}},
		"patches":            patches,
	}}}
	k.SetGroupVersionKind(gvkKustomization)
	k.SetNamespace(FluxNS)
	k.SetName(kustomizationName(app.Namespace, app.Name))
	k.SetLabels(map[string]string{"wecolab.io/project": app.Namespace, "wecolab.io/app": app.Name})
	return k
}

// jsonPatch targets every object of a kind when name is empty.
func jsonPatch(group, kind, name string, ops ...any) any {
	b, _ := json.Marshal(ops)
	t := map[string]any{"group": group, "kind": kind}
	if name != "" {
		t["name"] = name
	}
	return map[string]any{"target": t, "patch": string(b)}
}

func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.App{}).Named("app").Complete(r)
}

var gvkBackup = gvkDBCluster.GroupVersion().WithKind("Backup")

// NeedsBackup says whether the primary's database needs a base backup now: its vault, as the primary
// last saw it, holds none under the current archive (a new database, or one just promoted or rebuilt
// under a new generation), none asked for in the last ten minutes is still going, and none failed in the
// last hour: while backups fail (failed, or ended because WAL archiving fails) they are retried hourly,
// since every attempt leaves Object-Locked files. Without a clean look at the vault it waits.
func NeedsBackup(vault *VaultStatus, backups []unstructured.Unstructured, db string, now time.Time) bool {
	if vault == nil || vault.Err != "" || !vault.LatestBackup.IsZero() {
		return false
	}
	for _, b := range backups {
		if name, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name"); name != db {
			continue
		}
		phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
		failed, age := phase == "failed" || phase == "walArchivingFailing", now.Sub(b.GetCreationTimestamp().Time)
		if failed && age < time.Hour || !failed && age < 10*time.Minute {
			return false
		}
	}
	return true
}

// ensureBaseBackup asks CloudNativePG for a base backup at the primary when its database has none: a
// standby elsewhere can only start from one, and the schedule may be a day away.
func (r *AppReconciler) ensureBaseBackup(ctx context.Context, app *v1alpha1.App, now time.Time) error {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(gvkBackup.GroupVersion().WithKind("BackupList"))
	if err := r.List(ctx, l, client.InNamespace(app.Namespace)); err != nil {
		return err
	}
	var vault *VaultStatus
	if st := r.report(ctx, r.Site); st != nil { // what this site's own status says of the vault
		vault = st.Apps[app.Namespace+"/"+app.Name].Vault
	}
	if !NeedsBackup(vault, l.Items, app.Spec.Database, now) {
		return nil
	}
	b := newObj(gvkBackup.GroupVersion().String(), "Backup", fmt.Sprintf("%s-base-%d", app.Spec.Database, now.Unix()), app.Namespace, map[string]any{
		"cluster": map[string]any{"name": app.Spec.Database}, "method": "plugin",
		"pluginConfiguration": map[string]any{"name": "barman-cloud.cloudnative-pg.io"}})
	log.FromContext(ctx).Info("no base backup yet: asking for one", "database", app.Namespace+"/"+app.Spec.Database)
	return r.Create(ctx, b)
}
