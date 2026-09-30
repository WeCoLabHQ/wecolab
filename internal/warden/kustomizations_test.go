package warden

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
