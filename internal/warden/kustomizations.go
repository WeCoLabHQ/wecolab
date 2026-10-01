package warden

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"wecolab.io/wecolab/api/v1alpha1"
)

// siteKustomizationLabel marks the Kustomizations Warden adds for its site's roles.
const siteKustomizationLabel = "wecolab.io/site-role"

// PlatformSA is the service account the platform's own Kustomizations apply as. Flux runs with
// --default-service-account, so a Kustomization that names none (a project's could try) applies as
// nobody.
const PlatformSA = "kustomize-controller"

// SiteKustomizations are the Flux Kustomizations a site's roles call for: secrets/ at a steward,
// system/entrance at a public site, system/people at the site running NetBird. Site-specific values
// reach the manifests through Flux's post-build substitution, except secrets/: Flux would substitute
// inside the decrypted Secrets too, and a value holding ${...} would be changed.
func SiteKustomizations(site *v1alpha1.Site, s *Settings, email string) []*unstructured.Unstructured {
	vars := map[string]any{"SITE": site.Name, "ZONE": s.Zone, "NETWORK": s.Network.String(), "PEOPLE_NET": PeopleNet, "EMAIL": email,
		"TLS": fmt.Sprint(!s.Dev)}
	mk := func(name, path string, decrypt bool, deps []string) *unstructured.Unstructured {
		spec := map[string]any{
			"interval": "10m", "retryInterval": "1m", "path": path, "prune": true, "serviceAccountName": PlatformSA,
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "fabric"},
		}
		if decrypt {
			spec["decryption"] = map[string]any{"provider": "sops", "secretRef": map[string]any{"name": "sops-age"}}
		} else {
			spec["postBuild"] = map[string]any{"substitute": vars}
		}
		d := []any{map[string]any{"name": "system"}}
		for _, n := range deps {
			d = append(d, map[string]any{"name": n})
		}
		spec["dependsOn"] = d
		k := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		k.SetGroupVersionKind(gvkKustomization)
		k.SetNamespace(FluxNS)
		k.SetName(name)
		k.SetLabels(map[string]string{siteKustomizationLabel: name})
		return k
	}
	out := []*unstructured.Unstructured{}
	if site.Spec.Steward {
		out = append(out, mk("secrets", "./secrets", true, nil), mk("steward", "./system/steward", false, []string{"secrets"}))
	}
	if site.Spec.Public != nil {
		out = append(out, mk("entrance", "./system/entrance", false, nil))
	}
	if s.People == site.Name && site.Spec.Steward {
		out = append(out, mk("people", "./system/people", false, []string{"secrets"}))
	}
	return out
}

// Roots are the Kustomizations install.sh makes on a site's manager (flux_up), which nothing applies
// again. Warden names the platform's service account on them, so a site made before Flux's lockdown
// keeps applying under it.
var Roots = []string{"crds", "flux", "cert-manager", "cnpg", "barman", "system", "fabric"}

// nameRoots sets serviceAccountName on the Roots this site has, and nothing else on them.
func nameRoots(ctx context.Context, c client.Client) error {
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"serviceAccountName":"`+PlatformSA+`"}}`))
	for _, n := range Roots {
		k := &unstructured.Unstructured{}
		k.SetGroupVersionKind(gvkKustomization)
		k.SetNamespace(FluxNS)
		k.SetName(n)
		if err := client.IgnoreNotFound(c.Patch(ctx, k, patch)); err != nil {
			return fmt.Errorf("kustomization %s: %w", n, err)
		}
	}
	return nil
}

// PeopleNet is the NetBird network, fixed at install so mesh names can admit exactly it.
const PeopleNet = "100.96.0.0/16"

// SiteReconciler keeps this site's role Kustomizations in step with the Fabric.
type SiteReconciler struct {
	client.Client
	Site   string
	Resync time.Duration
}

func (r *SiteReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := nameRoots(ctx, r); err != nil {
		return ctrl.Result{}, err
	}
	s, err := ReadSettings(ctx, r)
	if err != nil {
		return ctrl.Result{}, err
	}
	site := &v1alpha1.Site{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.Site}, site); err != nil {
		return ctrl.Result{RequeueAfter: r.Resync}, client.IgnoreNotFound(err)
	}
	cm := &corev1.ConfigMap{}
	_ = r.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: SettingsName}, cm)
	want := map[string]bool{}
	for _, k := range SiteKustomizations(site, s, cm.Data["email"]) {
		want[k.GetName()] = true
		if err := r.Patch(ctx, k, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
			return ctrl.Result{}, fmt.Errorf("kustomization %s: %w", k.GetName(), err)
		}
	}
	have := &unstructured.UnstructuredList{}
	have.SetGroupVersionKind(gvkKustomization.GroupVersion().WithKind("KustomizationList"))
	if err := r.List(ctx, have, client.InNamespace(FluxNS), client.HasLabels{siteKustomizationLabel}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range have.Items {
		if !want[have.Items[i].GetName()] {
			if err := client.IgnoreNotFound(r.Delete(ctx, &have.Items[i])); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	return ctrl.Result{RequeueAfter: r.Resync}, r.forgetNodes(ctx, site, time.Now())
}

// nodeGrace is how long a node of a box the Fabric no longer lists must have been unready before it is
// deleted.
const nodeGrace = 10 * time.Minute

// forgetNodes deletes the Kubernetes nodes of boxes this site's copy of the Fabric no longer lists, once
// they have been unready for nodeGrace: a removed box is blocklisted off the mesh, so it stops answering,
// and there is nothing left to drain (its pods go with the node). A joining box answers, and a box that
// is only down is still listed, so neither is touched; nor is anything while the Site names no manager.
func (r *SiteReconciler) forgetNodes(ctx context.Context, site *v1alpha1.Site, now time.Time) error {
	if site.Manager() == nil {
		return nil
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return err
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if _, box := Find([]v1alpha1.Site{*site}, n.Name); box != nil || !unready(n, now) {
			continue
		}
		log.FromContext(ctx).Info("deleting the node of a box the Fabric no longer lists", "node", n.Name)
		if err := client.IgnoreNotFound(r.Delete(ctx, n)); err != nil {
			return err
		}
	}
	return nil
}

// unready says a node has not been Ready for nodeGrace (a node that never reported counts from its creation).
func unready(n *corev1.Node, now time.Time) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status != corev1.ConditionTrue && now.Sub(c.LastTransitionTime.Time) >= nodeGrace
		}
	}
	return now.Sub(n.CreationTimestamp.Time) >= nodeGrace
}

func (r *SiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: r.Site}}}
	})
	return ctrl.NewControllerManagedBy(mgr).Named("site").For(&v1alpha1.Site{}).Watches(&corev1.ConfigMap{}, one).Complete(r)
}
