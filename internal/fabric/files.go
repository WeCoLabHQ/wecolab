package fabric

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
)

// Path is where an object of the fabric lives in Git; false for anything that is not one.
func Path(gvk schema.GroupVersionKind, ns, name string) (string, bool) {
	if gvk.Group == "" && gvk.Kind == "Namespace" {
		return "fabric/projects/" + name + ".yaml", true
	}
	if gvk.Group != v1alpha1.GroupVersion.Group {
		return "", false
	}
	switch gvk.Kind {
	case "App":
		return "fabric/apps/" + ns + "/" + name + ".yaml", true
	case "Site", "Member", "Offer", "Pool", "Domain":
		return "fabric/" + strings.ToLower(gvk.Kind) + "s/" + name + ".yaml", true
	}
	return "", false
}

// AppFolder is where an app's workload lives.
func AppFolder(ns, app string) string { return "projects/" + ns + "/" + app }

// clusterOnly are the keys only one API server's copy has: never in Git.
var clusterOnly = []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink", "ownerReferences", "finalizers", "deletionTimestamp", "deletionGracePeriodSeconds"}

// localKey is a label or annotation that belongs to one API server or tool, not to the object.
func localKey(k string) bool {
	host, _, _ := strings.Cut(k, "/")
	return host == "kubectl.kubernetes.io" || host == "deployment.kubernetes.io" || host == "pv.kubernetes.io" ||
		strings.HasPrefix(host, "volume.") && strings.HasSuffix(host, "kubernetes.io") || strings.HasPrefix(host, "kustomize.toolkit.fluxcd.io") && k != PruneAnnotation
}

// PruneAnnotation keeps an object from being deleted by Flux when its file disappears.
const PruneAnnotation = "kustomize.toolkit.fluxcd.io/prune"

// YAML is an object as the Fabric keeps it: no status and nothing one API server added.
// gvk is required for typed objects, whose TypeMeta is usually empty.
func YAML(obj runtime.Object, gvk schema.GroupVersionKind) ([]byte, error) {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u["apiVersion"], u["kind"] = gvk.GroupVersion().String(), gvk.Kind
	delete(u, "status")
	md, _ := u["metadata"].(map[string]any)
	for _, k := range clusterOnly {
		delete(md, k)
	}
	for _, f := range []string{"labels", "annotations"} {
		m, _ := md[f].(map[string]any)
		for k := range m {
			if localKey(k) {
				delete(m, k)
			}
		}
		if len(m) == 0 {
			delete(md, f)
		}
	}
	return yaml.Marshal(u)
}

// WorkloadFiles is an app's folder: every object without its namespace (the site's Kustomization
// sets it), volumes and databases kept if a commit ever drops them, Secrets encrypted with SOPS to the
// recipients, and a kustomization.yaml listing them.
func WorkloadFiles(ns, app string, objs []*unstructured.Unstructured, recipients []string) ([]FileChange, error) {
	dir := AppFolder(ns, app)
	out, names := []FileChange{}, []string{}
	for _, o := range objs {
		o = o.DeepCopy()
		o.SetNamespace("")
		kind := o.GetKind()
		if kind == "PersistentVolumeClaim" || kind == "Cluster" {
			a := o.GetAnnotations()
			if a == nil {
				a = map[string]string{}
			}
			a[PruneAnnotation] = "disabled"
			o.SetAnnotations(a)
		}
		b, err := YAML(o, o.GroupVersionKind())
		if err != nil {
			return nil, err
		}
		name := strings.ToLower(kind) + "-" + o.GetName() + ".yaml"
		if kind == "Secret" {
			name = "secret-" + o.GetName() + ".sops.yaml"
			if b, err = Encrypt(b, name, recipients); err != nil {
				return nil, err
			}
		}
		names = append(names, name)
		out = append(out, FileChange{Path: dir + "/" + name, Content: b})
	}
	sort.Strings(names)
	k, _ := yaml.Marshal(map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": names})
	return append(out, FileChange{Path: dir + "/kustomization.yaml", Content: k}), nil
}

// Encrypt runs the sops binary the way Flux documents: only data and stringData are encrypted, to
// every recipient. The plaintext goes through a pipe, never a file.
func Encrypt(plain []byte, name string, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients for %s", name)
	}
	return sops(plain, nil, "encrypt", "--age", strings.Join(recipients, ","), "--encrypted-regex", "^(data|stringData)$", "--filename-override", name)
}

// Decrypt opens a SOPS file with an age private key.
func Decrypt(enc []byte, name, ageKey string) ([]byte, error) {
	return sops(enc, []string{"SOPS_AGE_KEY=" + ageKey}, "decrypt", "--filename-override", name)
}

// Reencrypt opens a file with one key and encrypts it again to new recipients.
func Reencrypt(enc []byte, name, ageKey string, recipients []string) ([]byte, error) {
	plain, err := Decrypt(enc, name, ageKey)
	if err != nil {
		return nil, err
	}
	return Encrypt(plain, name, recipients)
}

func sops(in []byte, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("sops", args...)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = os.TempDir() // never pick up a .sops.yaml from wherever we happen to run
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sops %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Recipients are the age public keys a secret for these sites is encrypted to: those sites', every
// steward's, and the recovery key's, read from keys/ in the Fabric.
func Recipients(ctx context.Context, g *Git, sites []v1alpha1.Site, for_ []string) ([]string, error) {
	want := map[string]bool{"recovery": true}
	for _, s := range for_ {
		want[s] = true
	}
	for _, s := range sites {
		if s.Spec.Steward {
			want[s.Name] = true
		}
	}
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []string{}
	for _, n := range names {
		b, ok, err := g.Read(ctx, "keys/"+n+".age.pub")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("no age key for %s in the Fabric (keys/%s.age.pub)", n, n)
		}
		out = append(out, strings.TrimSpace(string(b)))
	}
	return out, nil
}
