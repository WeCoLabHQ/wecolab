package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/validate"
	"wecolab.io/wecolab/internal/warden"
)

// Storage: a project's vault (the B2 bucket its databases archive to) and the
// volumes its apps hold at each site. Volume data is site-local; database data is
// what the vault protects.

func (s *server) storage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := s.who(r)
	mine := func(p string) bool { return id.Admin || slices.Contains(id.Projects, p) }

	// Vaults: one per project with a vault Secret; each database app's archive state.
	type vaultApp struct {
		App, Database, ObjectLock, Err string
		LatestBackup, LatestWAL        *time.Time
	}
	type vault struct {
		Project, Bucket, Endpoint string
		Apps                      []vaultApp
	}
	nsl := &corev1.NamespaceList{}
	_ = s.c.List(ctx, nsl, client.HasLabels{"wecolab.io/tenant"})
	al := &v1alpha1.AppList{}
	_ = s.listApps(ctx, id, al)
	vaults := []vault{}
	without := []string{}
	for _, n := range nsl.Items {
		if !n.DeletionTimestamp.IsZero() || !mine(n.Name) {
			continue
		}
		sec := &corev1.Secret{}
		if err := s.c.Get(s.elevated(ctx), types.NamespacedName{Namespace: warden.SystemNS, Name: vaultSecret(n.Name)}, sec); err != nil {
			without = append(without, n.Name)
			continue
		}
		v := vault{Project: n.Name, Bucket: string(sec.Data["bucket"]), Endpoint: string(sec.Data["endpoint"]), Apps: []vaultApp{}}
		for _, a := range al.Items {
			if a.Namespace != n.Name || a.Spec.Database == "" {
				continue
			}
			va := vaultApp{App: a.Name, Database: a.Spec.Database, Err: "the primary has not looked at its vault yet"}
			// The site holding the history looks at its vault with the app's own keys and publishes what it saw.
			_, _, states := s.peers.Gather(s.elevated(ctx), &a, "")
			if st := states[warden.Serving(a.Spec)].Vault; st != nil {
				va.Err, va.ObjectLock = st.Err, st.ObjectLock
				if !st.LatestBackup.IsZero() {
					t := st.LatestBackup.UTC()
					va.LatestBackup = &t
				}
				if !st.LatestWAL.IsZero() {
					t := st.LatestWAL.UTC()
					va.LatestWAL = &t
				}
			}
			v.Apps = append(v.Apps, va)
		}
		vaults = append(vaults, v)
	}

	// Volumes: the claims on the Fabric and what each site made of them.
	type siteVol struct {
		Site, Phase, Capacity string
		Applied               bool
	}
	type volume struct {
		Project, App, Name, Size string
		Sites                    []siteVol
	}
	answers, all := s.siteStatuses(ctx)
	byName := map[string]*volume{}
	for _, site := range all {
		a := answers[site.Name]
		if a == nil {
			continue
		}
		for key, vs := range a.Volumes {
			ns, name, _ := strings.Cut(key, "/")
			if !mine(ns) {
				continue
			}
			v := byName[key]
			if v == nil {
				v = &volume{Project: ns, App: vs.App, Name: name, Size: vs.Request, Sites: []siteVol{}}
				byName[key] = v
			}
			v.Sites = append(v.Sites, siteVol{Site: site.Name, Phase: vs.Phase, Capacity: vs.Capacity, Applied: true})
		}
	}
	volumes := []volume{}
	for _, v := range byName {
		volumes = append(volumes, *v)
	}
	acct := &corev1.Secret{}
	hasAccount := s.c.Get(s.elevated(ctx), types.NamespacedName{Namespace: warden.SystemNS, Name: storageSecret}, acct) == nil
	writeJSON(w, map[string]any{"vaults": vaults, "without": without, "volumes": volumes, "accountKey": hasAccount})
}

// createVault makes a project's vault on B2 from the account key in Settings.
func (s *server) createVault(w http.ResponseWriter, r *http.Request) {
	var in struct{ Project string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || validate.Name(in.Project) != nil {
		http.Error(w, "project must be a project's name", 400)
		return
	}
	if !s.can(w, r, in.Project) {
		return
	}
	ctx := r.Context()
	if _, exists, err := s.git.Read(ctx, secretPath(vaultSecret(in.Project))); err != nil || exists {
		answer(w, cmpErr(err, fail(409, "project %s already has a vault", in.Project)), 502)
		return
	}
	acct := &corev1.Secret{}
	if err := s.c.Get(s.elevated(ctx), types.NamespacedName{Namespace: warden.SystemNS, Name: storageSecret}, acct); err != nil {
		http.Error(w, "no B2 account key in Settings; an admin adds it once, or enter a bucket and key by hand when deploying", 409)
		return
	}
	v, err := warden.CreateVault(ctx, string(acct.Data["key-id"]), string(acct.Data["key"]), warden.VaultName(s.domain, in.Project))
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	data := map[string]string{"b2-key-id": v.KeyID, "b2-key": v.Key, "bucket": v.Bucket, "endpoint": v.Endpoint}
	if err := s.putSecret(ctx, "project "+in.Project+": its vault, bucket "+v.Bucket, vaultSecret(in.Project), data, false); err != nil {
		answer(w, err, 502)
		return
	}
	through(s.localSecret(r, vaultSecret(in.Project), data), "vault "+in.Project)
	writeJSON(w, map[string]any{"ok": true, "bucket": v.Bucket, "endpoint": v.Endpoint, "note": fmt.Sprintf("Object Lock compliance 30 days; key %s reaches only this bucket", strings.TrimSpace(v.KeyID))})
}

// vaultEndpoint checks an object storage endpoint someone typed. Every site's databases and Wardens
// connect to it with the vault's keys, so it is https to a public address, never a place inside a
// site or the fabric's networks.
func vaultEndpoint(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fail(400, "the vault endpoint must be an https URL")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(ips) == 0 {
		return fail(400, "the vault endpoint %s does not resolve", u.Hostname())
	}
	for _, ip := range ips {
		if ip = ip.Unmap(); !ip.IsGlobalUnicast() || ip.IsPrivate() || cgnat.Contains(ip) {
			return fail(400, "the vault endpoint %s is not a public address (%s)", u.Hostname(), ip)
		}
	}
	return nil
}

// cgnat is shared address space (RFC 6598), where NetBird puts people's devices.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// rotateVault gives a project's vault a new key (decision 25): made at B2 with the account key in
// Settings, written into the project's vault and every database app's Secret, then the old key deleted
// there, so a copy held anywhere else, such as at a site that left, no longer reaches the backups.
// Archiving and restores retry until each site has the new key.
func (s *server) rotateVault(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if validate.Name(project) != nil {
		http.Error(w, "project must be a project's name", 400)
		return
	}
	if !s.can(w, r, project) {
		return
	}
	bucket, err := s.rotate(r, project)
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "bucket": bucket})
}

// rotate is rotateVault's work, for one project; the old key is deleted only once the new one is in the
// Fabric everywhere it was.
func (s *server) rotate(r *http.Request, project string) (string, error) {
	ctx := r.Context()
	p := secretPath(vaultSecret(project))
	b, exists, err := s.git.Read(ctx, p)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fail(404, "project %s has no vault", project)
	}
	vault, err := s.openSecret(ctx, b, p)
	if err != nil {
		return "", err
	}
	acct := &corev1.Secret{}
	if err := s.c.Get(s.elevated(ctx), types.NamespacedName{Namespace: warden.SystemNS, Name: storageSecret}, acct); err != nil {
		return "", fail(409, "no B2 account key in Settings: rotate the key of %s at its provider", vault["bucket"])
	}
	id, key := string(acct.Data["key-id"]), string(acct.Data["key"])
	v, err := warden.NewVaultKey(ctx, id, key, vault["bucket"])
	if err != nil {
		return "", err
	}
	old := vault["b2-key-id"]
	vault["b2-key-id"], vault["b2-key"] = v.KeyID, v.Key
	if err := s.putSecret(ctx, "project "+project+": its vault's key rotated", vaultSecret(project), vault, true); err != nil {
		return "", err
	}
	through(s.localSecret(r, vaultSecret(project), vault), "vault "+project)
	if err := s.rekeyApps(ctx, project, v); err != nil {
		return "", fmt.Errorf("the vault has its new key, but its apps do not yet (the old key still works; rotate again): %w", err)
	}
	if err := warden.DeleteVaultKey(ctx, id, key, old); err != nil {
		return "", fmt.Errorf("the new key is in place, but the old one (%s) could not be deleted at B2 and still works: %w", old, err)
	}
	return vault["bucket"], nil
}

// rekeyApps writes a vault's new key into the Secret of every database app of the project: each carries
// a copy (deploy), encrypted for the app's sites and the stewards.
func (s *server) rekeyApps(ctx context.Context, project string, v warden.Vault) error {
	apps, err := readDir[v1alpha1.App](ctx, s.git, "fabric/apps/"+project)
	if err != nil {
		return err
	}
	paths, err := s.sitePaths(ctx)
	if err != nil {
		return err
	}
	files := map[string]v1alpha1.App{}
	for _, a := range apps {
		if a.Spec.Database != "" && !a.Spec.Deleted {
			f := fabric.AppFolder(project, a.Name) + "/secret-" + a.Name + ".sops.yaml" // as fabric.WorkloadFiles names it
			files[f] = a
			paths = append(paths, f)
		}
	}
	if len(files) == 0 {
		return nil
	}
	return s.edit(ctx, "project "+project+": its apps take the vault's new key", compact(paths), func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		sites, err := sitesOf(snap, paths)
		if err != nil {
			return nil, err
		}
		out := []fabric.FileChange{}
		for _, f := range slices.Sorted(maps.Keys(files)) {
			b, ok := snap.Get(f)
			if !ok {
				continue
			}
			data, err := s.openSecret(ctx, b, f)
			if err != nil {
				return nil, err
			}
			data["b2-key-id"], data["b2-key"] = v.KeyID, v.Key
			recips, err := s.recipients(ctx, sites, files[f].Spec.Sites, nil)
			if err != nil {
				return nil, err
			}
			sec := obj("v1", "Secret", files[f].Name, "", nil)
			sec.Object["type"] = "Opaque"
			sd := map[string]any{}
			for k, x := range data {
				sd[k] = x
			}
			sec.Object["stringData"] = sd
			plain, err := fabric.YAML(sec, sec.GroupVersionKind())
			if err != nil {
				return nil, err
			}
			enc, err := fabric.Encrypt(plain, path.Base(f), recips)
			if err != nil {
				return nil, err
			}
			out = append(out, fabric.FileChange{Path: f, Content: enc})
		}
		return out, nil
	})
}
