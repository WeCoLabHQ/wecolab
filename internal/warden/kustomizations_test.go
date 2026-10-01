package warden

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"wecolab.io/wecolab/api/v1alpha1"
)

func TestSiteKustomizationsLockdown(t *testing.T) {
	site := &v1alpha1.Site{}
	site.Name, site.Spec.Steward, site.Spec.Public = "pub", true, &v1alpha1.Public{Address: "203.0.113.7"}
	ks := SiteKustomizations(site, &Settings{Zone: "fab.example.org", People: "pub"}, "owner@example.org")
	if len(ks) != 4 {
		t.Fatalf("a public steward running NetBird has secrets, steward, entrance and people: %d", len(ks))
	}
	for _, k := range ks {
		// Flux's --default-service-account makes one without a name apply as nobody.
		if sa, _, _ := unstructured.NestedString(k.Object, "spec", "serviceAccountName"); sa != PlatformSA {
			t.Errorf("%s applies as %q", k.GetName(), sa)
		}
		_, subst, _ := unstructured.NestedFieldNoCopy(k.Object, "spec", "postBuild")
		_, decrypt, _ := unstructured.NestedFieldNoCopy(k.Object, "spec", "decryption")
		if decrypt == subst {
			t.Errorf("%s: decrypts %v, substitutes %v; Flux would substitute inside decrypted Secrets", k.GetName(), decrypt, subst)
		}
	}
}

// install.sh's Kustomizations get the platform's service account whatever made them, and only that.
func TestNameRoots(t *testing.T) {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(gvkKustomization, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gvkKustomization.GroupVersion().WithKind("KustomizationList"), &unstructured.UnstructuredList{})
	root := func(name string) *unstructured.Unstructured {
		k := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"path": "./" + name, "interval": "1h"}}}
		k.SetGroupVersionKind(gvkKustomization)
		k.SetNamespace(FluxNS)
		k.SetName(name)
		return k
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(root("system"), root("flux")).Build() // a site without the others
	if err := nameRoots(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, n := range Roots {
		k := &unstructured.Unstructured{}
		k.SetGroupVersionKind(gvkKustomization)
		err := c.Get(context.Background(), types.NamespacedName{Namespace: FluxNS, Name: n}, k)
		if n != "system" && n != "flux" {
			if err == nil {
				t.Errorf("%s was made", n)
			}
			continue
		}
		sa, _, _ := unstructured.NestedString(k.Object, "spec", "serviceAccountName")
		p, _, _ := unstructured.NestedString(k.Object, "spec", "path")
		if err != nil || sa != PlatformSA || p != "./"+n {
			t.Errorf("%s: %v, serviceAccountName %q, path %q", n, err, sa, p)
		}
	}
}

// A site deletes the node of a box its Fabric no longer lists once it has been unready for nodeGrace:
// never a listed box's, nor a joining box's (ready), nor anything while the Site names no manager.
func TestForgetNodes(t *testing.T) {
	now := time.Now()
	node := func(name string, ready corev1.ConditionStatus, since time.Duration) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready, LastTransitionTime: metav1.NewTime(now.Add(-since))}}}}
	}
	site := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: v1alpha1.SiteSpec{Boxes: []v1alpha1.Box{{Name: "a-m", Role: "manager"}, {Name: "a-down", Role: "node"}}}}
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		node("a-m", corev1.ConditionTrue, time.Hour),
		node("a-down", corev1.ConditionUnknown, time.Hour),      // listed, only down
		node("a-left", corev1.ConditionUnknown, time.Hour),      // removed, off the mesh
		node("a-leaving", corev1.ConditionUnknown, time.Minute), // removed a minute ago
		node("a-joining", corev1.ConditionTrue, time.Minute),    // not in this copy yet
	).Build()
	r := &SiteReconciler{Client: c, Site: "a"}
	names := func() (out []string) {
		l := &corev1.NodeList{}
		_ = c.List(context.Background(), l)
		for _, n := range l.Items {
			out = append(out, n.Name)
		}
		slices.Sort(out)
		return out
	}
	if err := r.forgetNodes(context.Background(), &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "a"}}, now); err != nil || len(names()) != 5 {
		t.Fatalf("a Site without its manager deletes nothing: %v %v", names(), err)
	}
	if err := r.forgetNodes(context.Background(), site, now); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"a-down", "a-joining", "a-leaving", "a-m"}) {
		t.Fatalf("nodes left: %v", got)
	}
}
