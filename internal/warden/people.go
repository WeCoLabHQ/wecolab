package warden

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/netbird"
)

// PeopleReconciler turns Members into what they may do at this site: Kubernetes RBAC, which the
// Console's dry runs are checked against. At the writer it also keeps the people mesh in step: a
// NetBird user per member, a single-use invite that sets their password, their projects as groups,
// and the blocked flag; and the mesh's service token, renewed before it expires.
type PeopleReconciler struct {
	client.Client
	Site   string
	Mesh   *netbird.Client // nil: no people mesh here
	Resync time.Duration

	mu      sync.Mutex
	checked time.Time // the service token's last look
}

const memberFinalizer = "wecolab.io/mesh-user"

// MeshRole is the mesh role a fabric role maps to.
func MeshRole(role string) string {
	if role == "owner" || role == "admin" {
		return "admin"
	}
	return "user"
}

// MeshGroups is the auto_groups a mirrored user should have: the member's project
// groups plus whatever non-project groups the mesh already gave them.
func MeshGroups(current []string, names map[string]string, projectGroups []string) []string {
	out := []string{}
	for _, g := range current {
		if !strings.HasPrefix(names[g], "project-") {
			out = append(out, g)
		}
	}
	for _, g := range projectGroups {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// Subject is how a site's API names a member: the name the Console impersonates.
func Subject(email string) string { return "oidc:" + strings.ToLower(email) }

// RenderBindings turns a Member into RBAC at a site: cluster-admin for owners and
// admins; for members, wecolab-reader cluster-wide and wecolab-project-member in each
// project namespace. A blocked member keeps no binding at all. Every binding is owned by
// its Member, so Kubernetes deletes it with the Member whether or not Warden sees it go.
func RenderBindings(m *v1alpha1.Member) []*unstructured.Unstructured {
	if m.Spec.Blocked {
		return nil
	}
	lab := map[string]string{"wecolab.io/member": m.Name}
	owner := []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Member", Name: m.Name, UID: m.UID}}
	subj := []any{map[string]any{"kind": "User", "apiGroup": "rbac.authorization.k8s.io", "name": Subject(m.Spec.Email)}}
	crb := func(name, role string) *unstructured.Unstructured {
		o := newObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", name, "", nil)
		delete(o.Object, "spec")
		o.SetLabels(lab)
		o.SetOwnerReferences(owner)
		o.Object["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": role}
		o.Object["subjects"] = subj
		return o
	}
	if m.Admin() {
		return []*unstructured.Unstructured{crb("wecolab-admin-"+m.Name, "cluster-admin")}
	}
	out := []*unstructured.Unstructured{crb("wecolab-reader-"+m.Name, "wecolab-reader")}
	for _, p := range m.Spec.Projects {
		o := newObj("rbac.authorization.k8s.io/v1", "RoleBinding", "wecolab-member-"+m.Name, p, nil)
		delete(o.Object, "spec")
		o.SetLabels(lab)
		o.SetOwnerReferences(owner)
		o.Object["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "wecolab-project-member"}
		o.Object["subjects"] = subj
		out = append(out, o)
	}
	return out
}

// bindings applies the member's RBAC and removes what this member had before and no longer should.
func (r *PeopleReconciler) bindings(ctx context.Context, m *v1alpha1.Member) error {
	want := map[string]bool{}
	for _, o := range RenderBindings(m) {
		want[o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName()] = true
		if err := r.Patch(ctx, o, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
			if o.GetKind() == "RoleBinding" && strings.Contains(err.Error(), "not found") {
				continue // the project namespace does not exist yet; next pass
			}
			return err
		}
	}
	for _, kind := range []string{"ClusterRoleBinding", "RoleBinding"} {
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: kind + "List"})
		if err := r.List(ctx, l, client.MatchingLabels{"wecolab.io/member": m.Name}); err != nil {
			return err
		}
		for i := range l.Items {
			it := &l.Items[i]
			if !want[kind+"/"+it.GetNamespace()+"/"+it.GetName()] {
				if err := client.IgnoreNotFound(r.Delete(ctx, it)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *PeopleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)
	m := &v1alpha1.Member{}
	if err := r.Get(ctx, req.NamespacedName, m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := time.Now()
	if m.DeletionTimestamp.IsZero() {
		if err := r.bindings(ctx, m); err != nil {
			lg.Error(err, "member rbac", "member", m.Name)
		}
	}
	if !m.DeletionTimestamp.IsZero() {
		blocked := *m
		blocked.Spec.Blocked = true // renders nothing, so every binding is removed
		if err := r.bindings(ctx, &blocked); err != nil {
			return ctrl.Result{}, err
		}
	}
	s, err := ReadSettings(ctx, r)
	if err != nil {
		return ctrl.Result{}, err
	}
	if r.Mesh == nil || s.Writer != r.Site {
		// Only the writer keeps the people mesh; elsewhere a member being deleted just goes.
		if !m.DeletionTimestamp.IsZero() {
			if controllerutil.RemoveFinalizer(m, memberFinalizer) {
				return ctrl.Result{}, r.Update(ctx, m)
			}
			return ctrl.Result{}, nil
		}
		m.Status.Phase = "Active"
		if m.Spec.Blocked {
			m.Status.Phase = "Blocked"
		}
		SetCondition(&m.Status.Conditions, cond("Mirrored", true, "NotHere", "the writer keeps the people mesh"), now, m.Generation)
		return ctrl.Result{RequeueAfter: r.Resync}, r.Status().Update(ctx, m)
	}
	if err := r.keepToken(ctx, now); err != nil {
		lg.Error(err, "the people mesh's service token")
	}
	if r.Mesh.Token == "" { // the people step has not given the fabric its token yet
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	users, err := r.Mesh.Users(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	var user map[string]any
	for _, u := range users {
		if strings.EqualFold(str(u, "email"), m.Spec.Email) && u["is_service_user"] != true {
			user = u
		}
	}
	if !m.DeletionTimestamp.IsZero() {
		if user != nil {
			if str(user, "role") == "owner" {
				lg.Info("member removed but the mesh owner stays", "email", m.Spec.Email)
			} else if err := r.Mesh.DeleteUser(ctx, str(user, "id")); err != nil {
				return ctrl.Result{}, err
			}
		}
		if invites, err := r.Mesh.Invites(ctx); err == nil {
			for _, i := range invites {
				if strings.EqualFold(str(i, "email"), m.Spec.Email) {
					_ = r.Mesh.DeleteInvite(ctx, str(i, "id"))
				}
			}
		}
		if controllerutil.RemoveFinalizer(m, memberFinalizer) {
			return ctrl.Result{}, r.Update(ctx, m)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(m, memberFinalizer) {
		if err := r.Update(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
	}
	names, err := r.Mesh.GroupNames(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	people, err := r.Mesh.EnsureGroup(ctx, "people") // every person's devices; the Door admits this group
	if err != nil {
		return ctrl.Result{}, err
	}
	projectGroups := []string{people}
	for _, p := range m.Spec.Projects {
		id, err := r.Mesh.EnsureGroup(ctx, "project-"+p)
		if err != nil {
			return ctrl.Result{}, err
		}
		projectGroups = append(projectGroups, id)
		names[id] = "project-" + p
	}

	if user == nil {
		// Not on the mesh yet: keep one live invite. Its token sets the password once.
		expired := m.Status.InviteExpires == nil || m.Status.InviteExpires.Time.Before(now)
		if m.Status.Invite == "" || expired {
			tok, err := r.Mesh.Invite(ctx, m.Spec.Email, m.Spec.Name, MeshRole(m.Spec.Role), projectGroups, 3*86400)
			if err != nil {
				SetCondition(&m.Status.Conditions, cond("Mirrored", false, "InviteError", err.Error()), now, m.Generation)
				return ctrl.Result{RequeueAfter: r.Resync}, r.Status().Update(ctx, m)
			}
			m.Status.Invite = tok
			m.Status.InviteExpires = &metav1.Time{Time: now.Add(3 * 86400 * time.Second)}
			lg.Info("member invited on the mesh", "email", m.Spec.Email)
		}
		m.Status.Phase, m.Status.Mesh = "Invited", ""
		SetCondition(&m.Status.Conditions, cond("Mirrored", true, "Invited", "invite minted; the link sets the password"), now, m.Generation)
		return ctrl.Result{RequeueAfter: r.Resync}, r.Status().Update(ctx, m)
	}

	// On the mesh: role, groups and blocked follow the Member. The mesh owner's role is its own.
	cur := []string{}
	if ag, ok := user["auto_groups"].([]any); ok {
		for _, g := range ag {
			cur = append(cur, fmt.Sprint(g))
		}
	}
	want := MeshGroups(cur, names, projectGroups)
	role := MeshRole(m.Spec.Role)
	if str(user, "role") == "owner" {
		role = "owner"
	}
	slices.Sort(cur)
	sorted := slices.Clone(want)
	slices.Sort(sorted)
	if role != str(user, "role") || !slices.Equal(cur, sorted) || (user["is_blocked"] == true) != m.Spec.Blocked {
		if err := r.Mesh.UpdateUser(ctx, str(user, "id"), role, want, m.Spec.Blocked); err != nil {
			SetCondition(&m.Status.Conditions, cond("Mirrored", false, "UpdateError", err.Error()), now, m.Generation)
			return ctrl.Result{RequeueAfter: r.Resync}, r.Status().Update(ctx, m)
		}
		lg.Info("mesh user updated", "email", m.Spec.Email)
	}
	m.Status.Invite, m.Status.InviteExpires = "", nil
	m.Status.Mesh = str(user, "id")
	m.Status.Phase = "Active"
	if m.Spec.Blocked {
		m.Status.Phase = "Blocked"
	}
	SetCondition(&m.Status.Conditions, cond("Mirrored", true, "Synced", "mesh user "+str(user, "id")+" follows this member"), now, m.Generation)
	return ctrl.Result{RequeueAfter: r.Resync}, r.Status().Update(ctx, m)
}

// The people mesh's service token is in the Fabric's netbird secret. NetBird tokens last at most a year,
// so the writer mints the next one two months before the one in use expires, commits it, and deletes
// older ones once this site runs with it (docs/plans/2026-09-29-hardening.md, R7).
const (
	meshServiceUser = "wecolab"
	meshSecret      = "netbird"
	meshSecretFile  = meshSecret + ".sops.yaml"
	tokenRenewal    = 60 * 24 * time.Hour
)

// TokenPlan decides from the service user's tokens and the id of the one this site's secret holds
// ("" when it does not say): whether to mint the next token, and which older ones to delete. Nothing is
// deleted until the secret names its token and that token is a day old, so every steward's Flux has
// applied it; a token minted in the last day but not yet in the secret is waited for rather than minted
// again.
func TokenPlan(tokens []netbird.Token, current string, now time.Time) (mint bool, stale []string) {
	var cur *netbird.Token
	for i := range tokens {
		if tokens[i].ID == current {
			cur = &tokens[i]
		}
	}
	known := cur != nil
	if !known { // the secret does not say: the newest is the one in use
		for i := range tokens {
			if cur == nil || tokens[i].Expires.After(cur.Expires) {
				cur = &tokens[i]
			}
		}
	}
	if cur == nil {
		return false, nil
	}
	pending := false
	for _, t := range tokens {
		pending = pending || t.Expires.After(cur.Expires) && now.Sub(t.Created) < 24*time.Hour
		if known && t.Expires.Before(cur.Expires) && now.Sub(cur.Created) > 24*time.Hour {
			stale = append(stale, t.ID)
		}
	}
	return !pending && cur.Expires.Sub(now) < tokenRenewal, stale
}

// keepToken uses the token this site's secret holds now, and at most hourly renews it.
func (r *PeopleReconciler) keepToken(ctx context.Context, now time.Time) error {
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: meshSecret}, sec); err != nil {
		return client.IgnoreNotFound(err)
	}
	if t := string(sec.Data["token"]); t != "" {
		r.Mesh.Token = t // Flux updates the Secret; the environment would keep the old one
	}
	r.mu.Lock()
	due := now.Sub(r.checked) > time.Hour
	if due {
		r.checked = now
	}
	r.mu.Unlock()
	if !due {
		return nil
	}
	user, err := r.Mesh.ServiceUser(ctx, meshServiceUser)
	if err != nil || user == "" {
		return err
	}
	tokens, err := r.Mesh.Tokens(ctx, user)
	if err != nil {
		return err
	}
	mint, stale := TokenPlan(tokens, string(sec.Data["tokenId"]), now)
	for _, id := range stale {
		if err := r.Mesh.DeleteToken(ctx, user, id); err != nil {
			return err
		}
		log.FromContext(ctx).Info("the people mesh's old service token deleted", "token", id)
	}
	if !mint {
		return nil
	}
	git := fabric.GitFromEnv() // this site's copy of the Fabric, as its Writer has it
	if git == nil {
		return fmt.Errorf("no copy of the Fabric here to renew the token in")
	}
	key, err := siteAgeKey(ctx, r, r.Site)
	if err != nil {
		return err
	}
	path := "secrets/" + meshSecretFile
	// Open the secret before minting: a Warden that cannot (no sops, not a recipient) would otherwise
	// mint a token every hour only to delete it again.
	enc, ok, err := git.Read(ctx, path)
	if err == nil && !ok {
		err = fmt.Errorf("no %s in the Fabric", path)
	}
	if err == nil {
		_, err = fabric.Decrypt(enc, meshSecretFile, key)
	}
	if err != nil {
		return err
	}
	plain, tok, err := r.Mesh.CreateToken(ctx, user, meshServiceUser, 365)
	if err != nil {
		return err
	}
	var ours error // fn failed: nothing was sent
	_, err = git.Edit(ctx, fabric.Author{Name: "Warden", Email: "warden@" + r.Site}, "the people mesh's next service token", []string{path},
		func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
			enc, ok := s.Get(path)
			if !ok {
				ours = fmt.Errorf("no %s in the Fabric", path)
				return nil, ours
			}
			out, err := reseal(enc, meshSecretFile, key, func(v map[string]string) error {
				v["token"], v["tokenId"] = plain, tok.ID
				return nil
			})
			ours = err
			return []fabric.FileChange{{Path: path, Content: out}}, err
		})
	if err != nil {
		// Delete the token only when no commit can hold it: any other failure may have landed, and a
		// token nobody holds is deleted anyway once the secret names a newer one.
		if ours != nil || errors.Is(err, fabric.ErrConflict) {
			_ = r.Mesh.DeleteToken(ctx, user, tok.ID)
		}
		return err
	}
	log.FromContext(ctx).Info("the people mesh's next service token committed", "token", tok.ID, "expires", tok.Expires)
	return nil
}

// siteAgeKey is this site's age identity, what Flux decrypts the Fabric's secrets with here.
func siteAgeKey(ctx context.Context, c client.Reader, site string) (string, error) {
	age := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: FluxNS, Name: "sops-age"}, age); err != nil {
		return "", err
	}
	return string(age.Data[site+".agekey"]), nil
}

// reseal is a sealed Secret's file (name is its file name) with its values changed by change, which sees
// data and stringData as one map, encrypted again to whoever could read it.
func reseal(enc []byte, name, ageKey string, change func(map[string]string) error) ([]byte, error) {
	var meta struct {
		Sops struct {
			Age []struct {
				Recipient string `json:"recipient"`
			} `json:"age"`
		} `json:"sops"`
	}
	if err := yaml.Unmarshal(enc, &meta); err != nil {
		return nil, err
	}
	recipients := []string{}
	for _, a := range meta.Sops.Age {
		recipients = append(recipients, a.Recipient)
	}
	plain, err := fabric.Decrypt(enc, name, ageKey)
	if err != nil {
		return nil, err
	}
	sec := &corev1.Secret{}
	if err := yaml.Unmarshal(plain, sec); err != nil {
		return nil, err
	}
	v := map[string]string{}
	for k, b := range sec.Data {
		v[k] = string(b)
	}
	maps.Copy(v, sec.StringData)
	if err := change(v); err != nil {
		return nil, err
	}
	sec.Data, sec.StringData = nil, v
	out, err := yaml.Marshal(sec)
	if err != nil {
		return nil, err
	}
	return fabric.Encrypt(out, name, recipients)
}

func (r *PeopleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("people").For(&v1alpha1.Member{}).Complete(r)
}
