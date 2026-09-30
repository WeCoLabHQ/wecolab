package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/api/v1alpha1"
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
