package warden

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"wecolab.io/wecolab/api/v1alpha1"
)

// OfferAt is what a project holds at one site, merged over every offer it holds there.
type OfferAt struct {
	Site, CPU, Memory, Storage string
	Boxes                      []string
	BestEffort                 bool
}

// MergeBySite folds a project's holdings into one per site: sizes add up, boxes are the union (any
// unpinned offer unpins the site), and the site is best effort only when every holding there is.
func MergeBySite(in []OfferAt) []OfferAt {
	add := func(a, b string) string {
		if a == "" {
			return b
		}
		if b == "" {
			return a
		}
		qa, err1 := resource.ParseQuantity(a)
		qb, err2 := resource.ParseQuantity(b)
		if err1 != nil || err2 != nil {
			return a
		}
		qa.Add(qb)
		return qa.String()
	}
	idx := map[string]int{}
	out := []OfferAt{}
	for _, o := range in {
		i, ok := idx[o.Site]
		if !ok {
			idx[o.Site] = len(out)
			o.Boxes = slices.Clone(o.Boxes)
			out = append(out, o)
			continue
		}
		m := &out[i]
		m.CPU, m.Memory, m.Storage = add(m.CPU, o.CPU), add(m.Memory, o.Memory), add(m.Storage, o.Storage)
		if len(m.Boxes) == 0 || len(o.Boxes) == 0 {
			m.Boxes = nil
		} else {
			for _, b := range o.Boxes {
				if !slices.Contains(m.Boxes, b) {
					m.Boxes = append(m.Boxes, b)
				}
			}
		}
		m.BestEffort = m.BestEffort && o.BestEffort
	}
	return out
}

// Holdings is what every project holds, per offer: the offer's size for a direct holder; for a
// project drawing from a pool, the pool's quota where it sets one. Held both ways counts once.
func Holdings(offers []v1alpha1.Offer, pools []v1alpha1.Pool) map[string][]OfferAt {
	pick := func(tier, own string) string {
		if tier != "" {
			return tier
		}
		return own
	}
	held := map[string][]OfferAt{}
	for _, o := range offers {
		base := OfferAt{Site: o.Spec.Site, CPU: o.Spec.CPU, Memory: o.Spec.Memory, Storage: o.Spec.Storage, Boxes: o.Spec.Boxes, BestEffort: o.Spec.BestEffort}
		seen := map[string]bool{}
		for _, p := range o.Spec.To {
			if !seen[p] {
				seen[p] = true
				held[p] = append(held[p], base)
			}
		}
		for _, pool := range pools {
			if !slices.Contains(o.Spec.Pools, pool.Name) {
				continue
			}
			q := pool.Spec.Quota
			at := base
			at.CPU, at.Memory, at.Storage = pick(q.CPU, o.Spec.CPU), pick(q.Memory, o.Spec.Memory), pick(q.Storage, o.Spec.Storage)
			for _, p := range pool.Spec.Projects {
				if !seen[p] {
					seen[p] = true
					held[p] = append(held[p], at)
				}
			}
		}
	}
	return held
}

// OfferHolders are the projects holding an offer: named directly, or drawing from its pools.
func OfferHolders(o v1alpha1.Offer, pools []v1alpha1.Pool) []string {
	out := slices.Clone(o.Spec.To)
	for _, p := range pools {
		if !slices.Contains(o.Spec.Pools, p.Name) {
			continue
		}
		for _, pr := range p.Spec.Projects {
			if !slices.Contains(out, pr) {
				out = append(out, pr)
			}
		}
	}
	return out
}

// Allowed lists the sites a project may place work at: the ones it owns and the ones whose offers
// it holds, directly or through a pool.
func Allowed(project string, sites []v1alpha1.Site, offers []v1alpha1.Offer, pools []v1alpha1.Pool) map[string]bool {
	ok := map[string]bool{}
	for _, s := range sites {
		if s.Spec.Owner == project {
			ok[s.Name] = true
		}
	}
	for _, at := range Holdings(offers, pools)[project] {
		ok[at.Site] = true
	}
	return ok
}

// Standing is a project's position at one site: it owns the site, holds capacity there, or neither.
type Standing struct {
	Owner bool
	Held  *OfferAt
}

// StandingAt is a project's standing at a site, from the Fabric.
func StandingAt(project, site string, sites []v1alpha1.Site, offers []v1alpha1.Offer, pools []v1alpha1.Pool) Standing {
	st := Standing{}
	for _, s := range sites {
		if s.Name == site && s.Spec.Owner == project {
			st.Owner = true
		}
	}
	for _, at := range MergeBySite(Holdings(offers, pools)[project]) {
		if at.Site == site {
			at := at
			st.Held = &at
		}
	}
	return st
}

// BestEffortClass is the PriorityClass of work that yields: the only work meant for laptops and idle time.
const BestEffortClass = "wecolab-best-effort"

// ProjectObjects are a project's boundary at a site: default limits; network policies that let its pods
// talk among themselves, to DNS, out to the internet on 80 and 443, to the site's own API server (at
// apiServers, its managers' addresses), and in only from the fabric's Nebula network (the Door); a
// ResourceQuota sized by what it holds here (none where it owns the site, zero where it holds nothing);
// and no best-effort pods unless it owns the site or holds best-effort capacity here.
func ProjectObjects(project string, st Standing, network string, apiServers ...string) []*unstructured.Unstructured {
	lab := map[string]string{"wecolab.io/boundary": project}
	mk := func(apiVersion, kind, name string, spec map[string]any) *unstructured.Unstructured {
		o := newObj(apiVersion, kind, name, project, spec)
		o.SetLabels(lab)
		return o
	}
	private := []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}
	egress := []any{
		map[string]any{"to": []any{map[string]any{"podSelector": map[string]any{}}}},
		map[string]any{"to": []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": map[string]any{"matchLabels": map[string]any{"k8s-app": "kube-dns"}}}},
			"ports": []any{map[string]any{"port": 53, "protocol": "UDP"}, map[string]any{"port": 53, "protocol": "TCP"}}},
		map[string]any{"to": []any{map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0", "except": private}}},
			"ports": []any{map[string]any{"port": 80, "protocol": "TCP"}, map[string]any{"port": 443, "protocol": "TCP"}}},
	}
	// CloudNativePG's instance manager talks to the API server. A rule without "to" would admit every
	// address, so with none known there is no rule.
	api := []any{}
	for _, ip := range apiServers {
		if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
			api = append(api, map[string]any{"ipBlock": map[string]any{"cidr": netip.PrefixFrom(a, 32).String()}})
		}
	}
	if len(api) > 0 {
		egress = append(egress, map[string]any{"to": api, "ports": []any{map[string]any{"port": 6443, "protocol": "TCP"}}})
	}
	out := []*unstructured.Unstructured{
		mk("v1", "LimitRange", "defaults", map[string]any{"limits": []any{map[string]any{
			"type": "Container", "defaultRequest": map[string]any{"cpu": "25m", "memory": "32Mi"}, "default": map[string]any{"memory": "256Mi"}}}}),
		mk("networking.k8s.io/v1", "NetworkPolicy", "default-deny", map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"}}),
		mk("networking.k8s.io/v1", "NetworkPolicy", "allow-app-egress", map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Egress"}, "egress": egress}),
		mk("networking.k8s.io/v1", "NetworkPolicy", "allow-app-ingress", map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress"}, "ingress": []any{
			map[string]any{"from": []any{map[string]any{"podSelector": map[string]any{}}}},
			map[string]any{"from": []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "cnpg-system"}}}}, "ports": []any{map[string]any{"port": 8000, "protocol": "TCP"}}},
			map[string]any{"from": []any{map[string]any{"ipBlock": map[string]any{"cidr": network}}}},
		}}),
	}
	switch {
	case st.Owner:
	case st.Held != nil:
		storage := st.Held.Storage
		if storage == "" {
			storage = "0"
		}
		out = append(out, mk("v1", "ResourceQuota", "offer", map[string]any{"hard": map[string]any{
			"requests.cpu": st.Held.CPU, "requests.memory": st.Held.Memory, "requests.storage": storage}}))
	default:
		out = append(out, mk("v1", "ResourceQuota", "offer", map[string]any{"hard": map[string]any{"requests.cpu": "0", "requests.memory": "0", "requests.storage": "0"}}))
	}
	// Best-effort pods are the ones that tolerate laptops and idle time; only the site's owner and
	// holders of its best-effort capacity may run them. The quota counts only pods of that class: a pod
	// of another class that tolerates wecolab.io/* or sets nodeName is refused by an admission policy on
	// tenant namespaces (system/base), not here.
	if !st.Owner && (st.Held == nil || !st.Held.BestEffort) {
		out = append(out, mk("v1", "ResourceQuota", "best-effort", map[string]any{
			"hard": map[string]any{"pods": "0"},
			"scopeSelector": map[string]any{"matchExpressions": []any{
				map[string]any{"scopeName": "PriorityClass", "operator": "In", "values": []any{BestEffortClass}}}},
		}))
	}
	return out
}

// ProjectReconciler keeps every project's boundary at this site.
type ProjectReconciler struct {
	client.Client
	Site   string
	Resync time.Duration
}

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name}, ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	project := ns.Labels[TenantLabel]
	if project == "" || !ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	s, err := ReadSettings(ctx, r)
	if err != nil {
		return ctrl.Result{}, err
	}
	sites, offers, pools := &v1alpha1.SiteList{}, &v1alpha1.OfferList{}, &v1alpha1.PoolList{}
	if err := r.List(ctx, sites); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.List(ctx, offers); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.List(ctx, pools); err != nil {
		return ctrl.Result{}, err
	}
	st := StandingAt(project, r.Site, sites.Items, offers.Items, pools.Items)
	var managers []string
	for _, site := range sites.Items {
		for _, b := range site.Spec.Boxes {
			if site.Name == r.Site && b.Role == "manager" {
				managers = append(managers, b.IP)
			}
		}
	}
	want := map[string]bool{}
	for _, o := range ProjectObjects(project, st, s.Network.String(), managers...) {
		want[o.GetKind()+"/"+o.GetName()] = true
		if err := r.Patch(ctx, o, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s %s/%s: %w", o.GetKind(), project, o.GetName(), err)
		}
	}
	for _, name := range []string{"offer", "best-effort"} {
		if want["ResourceQuota/"+name] {
			continue
		}
		q := &corev1.ResourceQuota{}
		q.Name, q.Namespace = name, project
		if err := client.IgnoreNotFound(r.Delete(ctx, q)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: r.Resync}, nil
}

func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		l := &corev1.NamespaceList{}
		if r.List(ctx, l, client.HasLabels{TenantLabel}) != nil {
			return nil
		}
		out := []reconcile.Request{}
		for _, n := range l.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: n.Name}})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).Named("project").For(&corev1.Namespace{}).
		Watches(&v1alpha1.Offer{}, all).Watches(&v1alpha1.Pool{}, all).Watches(&v1alpha1.Site{}, all).Complete(r)
}
