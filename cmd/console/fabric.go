package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/validate"
	"wecolab.io/wecolab/internal/warden"
)

// Writes to the fabric. Every change is a commit to the Fabric in Git, made for the signed-in
// person, and decided against what Git holds, never against the copy here, which lags Git by up to
// a minute (docs/plans/2026-09-29-hardening.md, R2): edit reads the files, the handler decides, and
// the commit is made only if none of them changed meanwhile. The API here still decides whether that
// person may make the change: it is first tried as a dry run as them. After the commit, Flux here is
// asked to fetch it at once; it applies it like at every other site. Changes and removals are never
// written here directly: until Flux fetched the commit it would put back the revision it holds, and
// Warden would act on that meanwhile. New objects are, since the next request may need them and Flux
// never removes what it did not create.

type authorKey struct{}

func withAuthor(ctx context.Context, id *identity) context.Context {
	return context.WithValue(ctx, authorKey{}, id)
}

// author is who a commit is made for; elevated steps keep it.
func author(ctx context.Context) fabric.Author {
	if id, _ := ctx.Value(authorKey{}).(*identity); id != nil && id.Email != "door" {
		return fabric.Author{Name: id.Name, Email: id.Email}
	}
	return fabric.Author{}
}

// httpError is an error a handler answers with its own status.
type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error { return httpError{code, fmt.Sprintf(format, a...)} }

// answer writes an error with its status: its own, the API server's (a dry run refused), 409 for a
// Fabric that kept changing, else def.
func answer(w http.ResponseWriter, err error, def int) {
	code := def
	var he httpError
	var st apierrors.APIStatus
	switch {
	case errors.As(err, &he):
		code = he.code
	case errors.Is(err, fabric.ErrConflict):
		code = http.StatusConflict
	case errors.As(err, &st) && st.Status().Code >= 400:
		code = int(st.Status().Code)
	}
	http.Error(w, err.Error(), code)
}

func (s *server) gvk(obj client.Object) schema.GroupVersionKind {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.GroupVersionKind()
	}
	gvk, _ := apiutil.GVKForObject(obj, s.c.Scheme())
	return gvk
}

// pathOf is where an object lives in the Fabric.
func (s *server) pathOf(obj client.Object) (string, error) {
	gvk := s.gvk(obj)
	p, ok := fabric.Path(gvk, obj.GetNamespace(), obj.GetName())
	if !ok {
		return "", fmt.Errorf("%s is not part of the fabric", gvk.Kind)
	}
	return p, nil
}

// fileOf is the object as a file of the Fabric; nil content when it is being removed.
func (s *server) fileOf(obj client.Object, remove bool) (fabric.FileChange, error) {
	p, err := s.pathOf(obj)
	if err != nil || remove {
		return fabric.FileChange{Path: p}, err
	}
	b, err := fabric.YAML(obj, s.gvk(obj))
	return fabric.FileChange{Path: p, Content: b}, err
}

// decode reads a file of the Fabric into obj, replacing whatever obj held; false when the file is not
// there (obj then holds only its name).
func decode(b []byte, exists bool, obj client.Object) (bool, error) {
	name, ns := obj.GetName(), obj.GetNamespace()
	gvk := obj.GetObjectKind().GroupVersionKind()
	reflect.ValueOf(obj).Elem().SetZero()
	obj.SetName(name)
	obj.SetNamespace(ns)
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	if !exists {
		return false, nil
	}
	return true, yaml.Unmarshal(b, obj)
}

// fromGit reads an object of the Fabric as Git holds it; false when it is not there.
func (s *server) fromGit(ctx context.Context, obj client.Object) (bool, error) {
	p, err := s.pathOf(obj)
	if err != nil {
		return false, err
	}
	b, ok, err := s.git.Read(ctx, p)
	if err != nil {
		return false, err
	}
	return decode(b, ok, obj)
}

// readDir is every object in a folder of the Fabric, as Git holds them.
func readDir[T any](ctx context.Context, g *fabric.Git, dir string) ([]T, error) {
	paths, err := g.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := []T{}
	for _, p := range paths {
		if !strings.HasSuffix(p, ".yaml") {
			continue
		}
		b, ok, err := g.Read(ctx, p)
		if err != nil {
			return nil, err
		}
		var x T
		if ok {
			if err := yaml.Unmarshal(b, &x); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			out = append(out, x)
		}
	}
	return out, nil
}

func (s *server) sitesInGit(ctx context.Context) ([]v1alpha1.Site, error) {
	return readDir[v1alpha1.Site](ctx, s.git, "fabric/sites")
}

// appsInGit is every app in Git, found through the projects (the Fabric lists files, not folders).
func (s *server) appsInGit(ctx context.Context) ([]v1alpha1.App, error) {
	projects, err := s.git.List(ctx, "fabric/projects")
	if err != nil {
		return nil, err
	}
	out := []v1alpha1.App{}
	for _, p := range projects {
		ns := strings.TrimSuffix(p[strings.LastIndex(p, "/")+1:], ".yaml")
		apps, err := readDir[v1alpha1.App](ctx, s.git, "fabric/apps/"+ns)
		if err != nil {
			return nil, err
		}
		out = append(out, apps...)
	}
	return out, nil
}

// edit commits what fn decides from the files it was given as Git holds them, only if none of them
// changed meanwhile (fabric.Edit), and asks Flux here to fetch the commit.
func (s *server) edit(ctx context.Context, msg string, paths []string, fn func(*fabric.Snapshot) ([]fabric.FileChange, error)) error {
	// App mutations across handlers must share the vault's changed-path CAS.
	// The deploy handler writes its own vault revision; all other App writers
	// are covered here rather than relying on an unprotected read-only check.
	for _, p := range paths {
		if !strings.HasPrefix(p, "fabric/apps/") || !strings.HasSuffix(p, ".yaml") {
			continue
		}
		parts := strings.Split(p, "/")
		if len(parts) != 4 {
			continue
		}
		vp := "secrets/vault-" + parts[2] + ".sops.yaml"
		paths = append(paths, vp)
		paths = append(paths, fabric.MeshClaimPath(strings.TrimSuffix(parts[3], ".yaml")+"-"+parts[2]))
	}
	sha, err := s.git.Edit(ctx, author(ctx), msg, paths, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		changes, err := fn(snap)
		if err != nil || len(changes) == 0 {
			return changes, err
		}
		for _, c := range changes {
			if !strings.HasPrefix(c.Path, "fabric/apps/") || !strings.HasSuffix(c.Path, ".yaml") || c.Content == nil {
				continue
			}
			var old, next v1alpha1.App
			b, exists := snap.Get(c.Path)
			if exists {
				if err := yaml.Unmarshal(b, &old); err != nil {
					return nil, err
				}
			}
			if err := yaml.Unmarshal(c.Content, &next); err != nil {
				return nil, err
			}
			claimPath := fabric.MeshClaimPath(next.Name + "-" + next.Namespace)
			if old.Spec.Mesh != next.Spec.Mesh && !slices.ContainsFunc(changes, func(f fabric.FileChange) bool { return f.Path == claimPath }) {
				label := next.Name + "-" + next.Namespace
				var claim fabric.FileChange
				if next.Spec.Mesh {
					claim, err = fabric.ClaimRoute(snap, claimPath, label, next.Namespace+"/"+next.Name)
				} else {
					claim, err = fabric.ReleaseRoute(snap, claimPath, label, next.Namespace+"/"+next.Name)
				}
				if err != nil {
					return nil, fail(409, "%v", err)
				}
				changes = append(changes, claim)
			}
			if old.Spec.Database == "" && next.Spec.Database == "" {
				continue
			}
			vp := "secrets/vault-" + next.Namespace + ".sops.yaml"
			if slices.ContainsFunc(changes, func(f fabric.FileChange) bool { return f.Path == vp }) {
				continue
			}
			enc, ok := snap.Get(vp)
			if !ok {
				return nil, fail(409, "database vault missing for %s", next.Namespace)
			}
			v, err := s.openSecret(ctx, enc, vp)
			if err != nil {
				return nil, err
			}
			if err := fabric.BumpVault(v); err != nil {
				return nil, fail(409, "%v", err)
			}
			ageKey, err := s.siteKey(ctx)
			if err != nil {
				return nil, err
			}
			sealed, err := warden.ResealSecret(enc, path.Base(vp), ageKey, func(values map[string]string) error { values["mutation-revision"] = v["mutation-revision"]; return nil })
			if err != nil {
				return nil, err
			}
			changes = append(changes, fabric.FileChange{Path: vp, Content: sealed})
		}
		return changes, nil
	})
	if err == nil && sha != "" {
		s.fetch(ctx)
	}
	return err
}

// editObj changes one object as Git holds it. obj names it; fn gets it read from Git into obj (exists
// says whether it was there) and returns what to write, obj or another object, or nil to remove it.
// That is tried as a dry run as the person, then committed if the file is still as it was read; on a
// conflict it is read again and fn asked again.
func (s *server) editObj(ctx context.Context, obj client.Object, msg string, fn func(exists bool) (client.Object, error)) error {
	p, err := s.pathOf(obj)
	if err != nil {
		return err
	}
	obj.GetObjectKind().SetGroupVersionKind(s.gvk(obj))
	return s.edit(ctx, msg, []string{p}, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		b, ok := snap.Get(p)
		exists, err := decode(b, ok, obj)
		if err != nil {
			return nil, err
		}
		out, err := fn(exists)
		switch {
		case err != nil:
			return nil, err
		case out == nil:
			if err := s.c.Delete(ctx, obj.DeepCopyObject().(client.Object), client.DryRunAll); client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			return []fabric.FileChange{{Path: p}}, nil
		}
		if err := s.dryRun(ctx, out); err != nil {
			return nil, err
		}
		f, err := s.fileOf(out, false)
		return []fabric.FileChange{f}, err
	})
}

// dryRun tries an object as the person, whether or not it exists here yet: the API server checks
// their rights and the CRD's validation.
func (s *server) dryRun(ctx context.Context, obj client.Object) error {
	o := obj.DeepCopyObject().(client.Object)
	o.GetObjectKind().SetGroupVersionKind(s.gvk(obj))
	o.SetResourceVersion("")
	return s.c.Patch(ctx, o, client.Apply, client.ForceOwnership, client.FieldOwner("wecolab-console"), client.DryRunAll)
}

// commit writes files whatever Git holds (fabric.Git.Commit): only for writes not decided from a read.
func (s *server) commit(ctx context.Context, msg string, changes ...fabric.FileChange) error {
	sha, err := s.git.Commit(ctx, author(ctx), msg, changes)
	if err == nil && sha != "" {
		s.fetch(ctx)
	}
	return err
}

// fetch asks Flux here to fetch the Fabric now rather than within its interval.
func (s *server) fetch(ctx context.Context) {
	repo := &unstructured.Unstructured{}
	repo.SetGroupVersionKind(schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"})
	repo.SetNamespace(warden.FluxNS)
	repo.SetName("fabric")
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"reconcile.fluxcd.io/requestedAt":%q}}}`, time.Now().UTC().Format(time.RFC3339Nano))
	if err := s.c.Patch(s.elevated(ctx), repo, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		log.Printf("asking Flux to fetch the Fabric: %v", err)
	}
}

// through writes a committed change here too. Git is the record: a failure here is logged and
// Flux brings the change anyway.
func through(err error, what string) {
	if err != nil && !apierrors.IsAlreadyExists(err) {
		log.Printf("%s: committed, not yet here: %v", what, err)
	}
}

// create adds a new object to the Fabric, refused if Git has it already, and here.
func (s *server) create(ctx context.Context, obj client.Object, msg string) error {
	want := obj.DeepCopyObject().(client.Object)
	err := s.editObj(ctx, obj.DeepCopyObject().(client.Object), msg, func(exists bool) (client.Object, error) {
		if exists {
			return nil, fail(http.StatusConflict, "%s exists", want.GetName())
		}
		return want, nil
	})
	if err == nil {
		through(s.c.Create(ctx, obj), msg)
	}
	return err
}

// remove takes an object out of the Fabric; its sites' Flux removes it.
func (s *server) remove(ctx context.Context, obj client.Object, msg string) error {
	return s.editObj(ctx, obj.DeepCopyObject().(client.Object), msg, func(exists bool) (client.Object, error) {
		if !exists {
			return nil, fail(http.StatusNotFound, "no such %s", strings.ToLower(s.gvk(obj).Kind))
		}
		return nil, nil
	})
}

// project is a project's namespace as the Fabric keeps it: a tenant, never deleted by a commit.
func project(name string) *unstructured.Unstructured {
	ns := obj("v1", "Namespace", name, "", nil)
	delete(ns.Object, "spec")
	ns.SetLabels(warden.TenantLabels(name))
	ns.SetAnnotations(map[string]string{"kustomize.toolkit.fluxcd.io/prune": "disabled"})
	return ns
}

// applyProject makes sure a project's namespace exists, in Git and here. Every project comes through
// here, so this is where a system namespace is refused as one.
func (s *server) applyProject(ctx context.Context, name string) error {
	if err := validate.Name(name); err != nil {
		return fail(http.StatusBadRequest, "project: %v", err)
	}
	ns := project(name)
	opts := []client.PatchOption{client.ForceOwnership, client.FieldOwner("wecolab-console")}
	if err := s.c.Patch(ctx, ns.DeepCopy(), client.Apply, append(opts, client.DryRunAll)...); err != nil {
		return err
	}
	f, err := s.fileOf(ns, false)
	if err != nil {
		return err
	}
	if err := s.commit(ctx, "project "+name, f); err != nil {
		return err
	}
	through(s.c.Patch(ctx, ns, client.Apply, opts...), "project "+name)
	return nil
}
