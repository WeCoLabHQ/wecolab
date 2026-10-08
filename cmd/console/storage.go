package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
		Retiring, Awaiting        []string // old key ids still valid at B2, and the sites not yet reporting the new key
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
		if v.Retiring = strings.Fields(string(sec.Data["retiring"])); len(v.Retiring) > 0 {
			apps := slices.DeleteFunc(slices.Clone(al.Items), func(a v1alpha1.App) bool { return a.Namespace != n.Name })
			v.Awaiting = warden.Retirable(string(sec.Data["b2-key-id"]), string(sec.Data["key-version"]), apps, func(site string) *warden.SiteStatus {
				st, _ := s.peers.Get(s.elevated(ctx), site)
				return st
			})
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
	data := map[string]string{"b2-key-id": v.KeyID, "b2-key": v.Key, "bucket": v.Bucket, "endpoint": v.Endpoint, "key-version": "0", "mutation-revision": "0"}
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
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fail(400, "the vault endpoint must be an https URL")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(ips) == 0 {
		return fail(400, "the vault endpoint %s does not resolve", u.Hostname())
	}
	for _, ip := range ips {
		if !warden.PublicS3Address(ip) {
			return fail(400, "the vault endpoint %s is not a public address (%s)", u.Hostname(), ip)
		}
	}
	return nil
}

// rotateVault gives a project's vault a new key (decision 25): made at B2 with the account key in
// Settings and written into the project's vault and every database app's Secret. The old key is marked
// retiring in the vault: the writer's Warden deletes it at B2 once every site of the project's database
// apps reports the new one, so a copy held anywhere else, such as at a site that left, no longer reaches
// the backups, and no site still archiving with it is cut off.
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
	writeJSON(w, map[string]any{"ok": true, "bucket": bucket, "note": "the old key stays valid until every site has the new one; then it is deleted at B2"})
}

// rotate is rotateVault's work, for one project. The old key joins the vault's retiring keys, in the same
// commit as the new one.
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
	v, err := warden.NewVaultKey(ctx, string(acct.Data["key-id"]), string(acct.Data["key"]), vault["bucket"])
	if err != nil {
		return "", err
	}
	// The version and revision are persisted before any app rekey. A crash after
	// this commit is repaired by the writer; an orphan key from a failed commit
	// must be inventoried at the provider rather than guessed and deleted here.
	err = s.editSecret(ctx, "project "+project+": its vault's key rotated", vaultSecret(project), func(b []byte, exists bool) (map[string]string, error) {
		if !exists {
			return nil, fail(404, "project %s has no vault", project)
		}
		now, err := s.openSecret(ctx, b, p)
		if err != nil {
			return nil, err
		}
		if err := fabric.NextKeyVersion(now); err != nil {
			return nil, fail(409, "%v", err)
		}
		if old := now["b2-key-id"]; old != "" && !slices.Contains(strings.Fields(now["retiring"]), old) {
			now["retiring"] = strings.TrimSpace(now["retiring"] + " " + old)
		}
		now["b2-key-id"], now["b2-key"] = v.KeyID, v.Key
		vault = now
		return now, nil
	})
	if err != nil {
		return "", err
	}
	// Never apply a request's captured key directly to the cluster: an older
	// request can resume after a newer Git rotation. Flux applies Git's winner.
	ageKey, err := s.siteKey(ctx)
	if err != nil {
		return "", err
	}
	if err := warden.ConvergeVaultApps(ctx, s.git, ageKey, project); err != nil {
		return "", fmt.Errorf("vault committed; writer will repair outstanding app keys: %w", err)
	}
	return vault["bucket"], nil
}
