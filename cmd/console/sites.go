package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/nebula"
	"wecolab.io/wecolab/internal/validate"
	"wecolab.io/wecolab/internal/warden"
)

// Sites and boxes. A site or a box joins with a one-time invite minted here: the invite holds only
// this Console's address and a random code, whose hash waits at the writer for a day. The box calls
// the join endpoint with the code and its public keys, and is added to the Fabric then.

const invitesSecret = "invites"

// invite is what a code admits.
type invite struct {
	Kind    string    `json:"kind"` // site or box
	Site    string    `json:"site"`
	Owner   string    `json:"owner,omitempty"`
	Steward bool      `json:"steward,omitempty"`
	Public  bool      `json:"public,omitempty"`
	Laptop  bool      `json:"laptop,omitempty"`
	By      string    `json:"by"`
	Expires time.Time `json:"expires"`
}

func hash(code string) string {
	s := sha256.Sum256([]byte(code))
	return hex.EncodeToString(s[:])
}

// keepInvite stores an invite under its code's hash, forgetting the expired ones.
func (s *server) keepInvite(ctx context.Context, h string, inv invite) error {
	ctx = s.elevated(ctx)
	sec := &corev1.Secret{}
	err := s.c.Get(ctx, types.NamespacedName{Namespace: warden.SystemNS, Name: invitesSecret}, sec)
	if apierrors.IsNotFound(err) {
		sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: warden.SystemNS, Name: invitesSecret}}
		if err := s.c.Create(ctx, sec); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range sec.Data {
		var old invite
		if json.Unmarshal(v, &old) != nil || old.Expires.Before(time.Now()) {
			delete(sec.Data, k)
		}
	}
	sec.Data[h], _ = json.Marshal(inv)
	return s.c.Update(ctx, sec)
}

// mintInvite keeps an invite's hash and returns the code a box runs with.
func (s *server) mintInvite(ctx context.Context, inv invite) (string, error) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	if err := s.keepInvite(ctx, hash(token), inv); err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]string{"c": s.publicURL, "t": token})
	return "wcl2." + base64.RawURLEncoding.EncodeToString(payload), nil
}

// takeInvite returns an invite and forgets it: codes are single use. The update is conditional on the
// Secret's version, so two requests never both take one.
func (s *server) takeInvite(ctx context.Context, token string) (*invite, error) {
	sec := &corev1.Secret{}
	if err := s.c.Get(s.elevated(ctx), types.NamespacedName{Namespace: warden.SystemNS, Name: invitesSecret}, sec); err != nil {
		return nil, fmt.Errorf("unknown or used invite")
	}
	v, ok := sec.Data[hash(token)]
	if !ok {
		return nil, fmt.Errorf("unknown or used invite")
	}
	delete(sec.Data, hash(token))
	if err := s.c.Update(s.elevated(ctx), sec); err != nil {
		return nil, err
	}
	var inv invite
	if err := json.Unmarshal(v, &inv); err != nil || inv.Expires.Before(time.Now()) {
		return nil, fmt.Errorf("the invite expired")
	}
	return &inv, nil
}

func (s *server) joinCommand(code string) string {
	return fmt.Sprintf("curl -fsSL %s/join.sh | sudo bash -s %s", s.publicURL, code)
}

// inviteSite: an admin invites a new site: its name, owner, whether it is a steward, whether public.
func (s *server) inviteSite(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	var in struct {
		Name, Owner     string
		Steward, Public bool
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "name and owner (a project) required", 400)
		return
	}
	for _, n := range []string{in.Name, in.Owner} {
		if err := validate.Name(n); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	if exists, err := s.fromGit(r.Context(), &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: in.Name}}); err != nil || exists {
		answer(w, cmpErr(err, fail(409, "site %s exists", in.Name)), 502)
		return
	}
	code, err := s.mintInvite(r.Context(), invite{Kind: "site", Site: in.Name, Owner: in.Owner, Steward: in.Steward, Public: in.Public, By: s.who(r).Email, Expires: time.Now().Add(24 * time.Hour)})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"invite": code, "command": s.joinCommand(code), "expires": time.Now().Add(24 * time.Hour)})
}

// cmpErr is err, or else def.
func cmpErr(err, def error) error {
	if err != nil {
		return err
	}
	return def
}

// inviteBox: a site's manager invites another box to it, a Linux box or a Mac.
func (s *server) inviteBox(w http.ResponseWriter, r *http.Request) {
	site := r.PathValue("site")
	if !s.canSite(w, r, site) {
		return
	}
	var in struct{ Laptop bool }
	_ = json.NewDecoder(r.Body).Decode(&in)
	code, err := s.mintInvite(r.Context(), invite{Kind: "box", Site: site, Laptop: in.Laptop, By: s.who(r).Email, Expires: time.Now().Add(24 * time.Hour)})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"invite": code, "command": s.joinCommand(code), "expires": time.Now().Add(24 * time.Hour), "mac": "paste the invite into WeCoLab for Mac"})
}

// joinRequest is what a box sends with its invite.
type joinRequest struct {
	Code   string `json:"code"`
	Host   string `json:"host"`
	Key    string `json:"key"`    // Nebula public key, PEM
	Age    string `json:"age"`    // a new site's age public key
	Public string `json:"public"` // a public site's address, as the box sees it
	Arch   string `json:"arch"`
	Laptop bool   `json:"laptop"` // a Mac: must be what the invite was for
}

// joinResponse is everything the box needs: its Nebula identity and its site's settings.
type joinResponse struct {
	Site        string              `json:"site"`
	Box         string              `json:"box"`
	Role        string              `json:"role"`
	Index       int                 `json:"index"`
	IP          string              `json:"ip"`
	Network     string              `json:"network"`
	CA          string              `json:"ca"`
	Cert        string              `json:"cert"`
	Life        int                 `json:"life"` // the certificate's lifetime, seconds; sets how often the box renews
	Config      string              `json:"config"`
	Stewards    []string            `json:"stewards"`
	Lighthouses []nebula.Lighthouse `json:"lighthouses"`
	Laptop      bool                `json:"laptop"`
	Public      bool                `json:"public"`
	Steward     bool                `json:"steward"`
	// K3sToken is what the box joins k3s with: the agent token for a node, the server token for a
	// manager, which also gets the agent token to give its server (R5).
	K3sToken      string            `json:"k3sToken"`
	K3sAgentToken string            `json:"k3sAgentToken,omitempty"`
	K3sServer     string            `json:"k3sServer,omitempty"`
	Mirror        string            `json:"mirror,omitempty"`
	Version       string            `json:"version"`
	Zone          string            `json:"zone"`
	Dev           bool              `json:"dev"`
	SSHKeys       []string          `json:"sshKeys"` // the keys the box should have now
	Settings      map[string]string `json:"settings"`
}

// One join at a time. Box names must be unique across sites and site indexes across the fabric,
// which no single file's hash guards; only this Console commits joins.
var joins sync.Mutex

var (
	hostRe = regexp.MustCompile(`[^a-z0-9-]+`)
	// ageKey is an age X25519 recipient: "age1" and 58 bech32 characters.
	ageKey = regexp.MustCompile(`^age1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]{58}$`)
)

// boxName is <site>-<host> as a Kubernetes node name: the host lowercased, anything else a dash,
// cut so the whole is at most 63 characters (a Mac's "Anas-MacBook-Pro.local" and long names).
func boxName(site, host string) (string, error) {
	h := strings.Trim(hostRe.ReplaceAllString(strings.ToLower(host), "-"), "-")
	if n := 63 - len(site) - 1; len(h) > n {
		h = strings.TrimRight(h[:n], "-")
	}
	name := site + "-" + h
	if h == "" || validate.Label(name, 63) != nil {
		return "", fail(400, "host %q gives no box name", host)
	}
	return name, nil
}

// nextBox is the host number of a site's next box: never one a box had before, so a removed box's
// address is never anyone else's while its certificate may still be valid. Sites from before nextBox
// start after their highest address.
func nextBox(site *v1alpha1.Site) int {
	n := max(site.Spec.NextBox, 2)
	for _, b := range site.Spec.Boxes {
		if a, err := netip.ParseAddr(b.IP); err == nil && a.Is4() {
			n = max(n, int(a.As4()[3])+1)
		}
	}
	return n
}

// join admits a box: the writer's Console records it in the Fabric, signs its Nebula key and answers
// with what it needs to start. Public: the invite code is the credential.
func (s *server) join(w http.ResponseWriter, r *http.Request) {
	var in joinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || in.Code == "" || in.Key == "" || in.Host == "" {
		http.Error(w, "code, host and key are required", 400)
		return
	}
	joins.Lock()
	defer joins.Unlock()
	// Not cancelled with the request: a join that started finishes, and a refused one puts its invite
	// back, even when the box or the Door dropped the connection meanwhile.
	ctx := s.elevated(context.WithoutCancel(r.Context()))
	inv, err := s.takeInvite(ctx, in.Code)
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	res, err := s.admit(withAuthor(ctx, &identity{Email: inv.By}), inv, in)
	if err != nil {
		// Nothing was committed: the invite stays good, and a retry is not told it was used.
		if err := s.keepInvite(ctx, hash(in.Code), *inv); err != nil {
			log.Printf("join: the invite could not be put back: %v", err)
		}
		answer(w, err, 502)
		return
	}
	writeJSON(w, res)
}

// admit places the box in the Fabric as Git holds it, with its first certificate recorded, and says
// what the box needs to start.
func (s *server) admit(ctx context.Context, inv *invite, in joinRequest) (*joinResponse, error) {
	if in.Laptop != inv.Laptop {
		return nil, fail(403, "this invite is for %s", map[bool]string{false: "a Linux box", true: "a Mac"}[inv.Laptop])
	}
	name, err := boxName(inv.Site, in.Host)
	if err != nil {
		return nil, err
	}
	ca := &corev1.Secret{}
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: warden.SystemNS, Name: warden.CASecret}, ca); err != nil {
		return nil, fmt.Errorf("the Nebula CA is not at this steward: %w", err)
	}
	sitePath, siteSecret := "fabric/sites/"+inv.Site+".yaml", secretPath(warden.SiteSecret(inv.Site))
	paths, err := s.sitePaths(ctx)
	if err != nil {
		return nil, err
	}
	paths = append(paths, sitePath, siteSecret, settingsPath)
	age := strings.TrimSpace(in.Age)
	if inv.Kind == "site" {
		if !ageKey.MatchString(age) {
			return nil, fail(400, "a new site brings its age public key (age1 and 58 characters)")
		}
		if a, err := netip.ParseAddr(in.Public); inv.Public && (err != nil || !a.Is4()) {
			return nil, fail(400, "a public site needs its public IPv4 address: %q", in.Public)
		}
		paths = append(paths, "keys/"+inv.Site+".age.pub", kustomizationPath)
		if inv.Steward { // a new steward can read everything a steward reads
			sealed, err := s.sealed(ctx)
			if err != nil {
				return nil, err
			}
			paths = append(paths, slices.Collect(maps.Keys(sealed))...)
		}
	}
	paths = compact(paths)
	var res *joinResponse
	owner := inv.Owner
	err = s.edit(ctx, fmt.Sprintf("%s: %s joins", inv.Site, name), paths, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		now := time.Now()
		_, set, err := settingsOf(snap)
		if err != nil {
			return nil, err
		}
		others, err := sitesOf(snap, paths)
		if err != nil {
			return nil, err
		}
		others = slices.DeleteFunc(others, func(x v1alpha1.Site) bool { return x.Name == inv.Site })
		site := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: inv.Site}}
		b, ok := snap.Get(sitePath)
		exists, err := decode(b, ok, site)
		if err != nil {
			return nil, err
		}
		if _, dup := warden.Find(append([]v1alpha1.Site{*site}, others...), name); dup != nil {
			return nil, fail(409, "the fabric already has a box named %s: give this one another host name", name)
		}
		res = &joinResponse{Site: inv.Site, Network: set.Network.String(), Version: s.version, Zone: set.Zone, Dev: set.Dev, Laptop: inv.Laptop}
		box := v1alpha1.Box{Name: name, Key: in.Key, Added: metav1.NewTime(now)}
		changes := []fabric.FileChange{}
		switch inv.Kind {
		case "site":
			if exists {
				return nil, fail(409, "site %s already joined", inv.Site)
			}
			index := 0
			for _, x := range others {
				index = max(index, x.Spec.Index+1)
			}
			ip := nebula.Nth(set.Network, index, 1) // invalid past the network's last /24
			if !ip.IsValid() {
				return nil, fail(409, "the fabric has no site numbers left in %s", set.Network)
			}
			box.IP, box.Role = ip.String(), "manager"
			site.Spec = v1alpha1.SiteSpec{Owner: inv.Owner, Index: index, Steward: inv.Steward, NextBox: 2}
			if inv.Public {
				site.Spec.Public = &v1alpha1.Public{Address: in.Public}
			}
			res.K3sToken, res.K3sAgentToken, res.Mirror = randHexStr(48), randHexStr(48), randHexStr(24)
			all, extra := append(slices.Clone(others), *site), map[string]string{inv.Site: age}
			recips, err := s.recipients(ctx, all, nil, extra)
			if err != nil {
				return nil, err
			}
			sec, err := secretFile(warden.SiteSecret(inv.Site), map[string]string{warden.KeyK3sServer: res.K3sToken, warden.KeyK3sAgent: res.K3sAgentToken, warden.KeyMirror: res.Mirror}, recips)
			if err != nil {
				return nil, err
			}
			kust, err := secretsKustomization(snap, path.Base(siteSecret))
			if err != nil {
				return nil, err
			}
			changes = append(changes, fabric.FileChange{Path: "keys/" + inv.Site + ".age.pub", Content: []byte(age + "\n")}, sec, kust)
			if inv.Steward {
				sealed, err := s.sealedNow(ctx, paths)
				if err != nil {
					return nil, err
				}
				delete(sealed, siteSecret)
				more, err := s.reencrypt(ctx, snap, all, sealed, extra)
				if err != nil {
					return nil, err
				}
				changes = append(changes, more...)
			}
		case "box":
			if !exists || site.Manager() == nil {
				return nil, fail(409, "site %s has not joined", inv.Site)
			}
			n := nextBox(site)
			ip := nebula.Nth(set.Network, site.Spec.Index, n) // invalid past .254
			if !ip.IsValid() {
				return nil, fail(409, "site %s has no addresses left", inv.Site)
			}
			box.IP, box.Role, box.Laptop = ip.String(), "node", inv.Laptop
			site.Spec.NextBox = n + 1
			b, ok := snap.Get(siteSecret)
			if !ok {
				return nil, fail(409, "site %s has no tokens in the Fabric", inv.Site)
			}
			tok, err := s.openSecret(ctx, b, siteSecret)
			if err != nil {
				return nil, err
			}
			if tok[warden.KeyK3sAgent] == "" {
				return nil, fail(409, "site %s has no k3s agent token: nodes never join with the server token", inv.Site)
			}
			res.K3sToken, res.K3sServer = tok[warden.KeyK3sAgent], fmt.Sprintf("https://%s:6443", site.Manager().IP)
			owner = site.Spec.Owner
		default:
			return nil, fmt.Errorf("unknown invite")
		}
		addr, err := nebula.Addr(box, set.Network)
		if err != nil {
			return nil, err
		}
		crt, _, err := nebula.Sign(ca.Data["ca.crt"], ca.Data["ca.key"], nebula.Request{Name: box.Name, Addr: addr, Groups: nebula.Groups(site, box), PubPEM: []byte(box.Key)}, set.CertLife, now)
		if err != nil {
			return nil, err
		}
		fp, notAfter, err := nebula.Fingerprint(crt)
		if err != nil {
			return nil, err
		}
		box.Certs = []v1alpha1.IssuedCert{{Fingerprint: fp, NotAfter: metav1.NewTime(notAfter)}} // what removing the box blocks
		site.Spec.Boxes = append(site.Spec.Boxes, box)
		if err := s.dryRun(ctx, site); err != nil {
			return nil, err
		}
		f, err := s.fileOf(site, false)
		if err != nil {
			return nil, err
		}
		all := append([]v1alpha1.Site{*site}, others...)
		lhs := nebula.Lighthouses(all)
		cfg, err := nebula.FabricConfig(site, box, lhs, set.Blocklist(now))
		if err != nil {
			return nil, err
		}
		res.Box, res.Role, res.IP, res.Index = box.Name, box.Role, box.IP, site.Spec.Index
		res.CA, res.Cert, res.Config, res.Lighthouses, res.Stewards = string(ca.Data["ca.crt"]), string(crt), string(cfg), lhs, warden.Stewards(all)
		res.Life = int(set.CertLife.Seconds())
		res.Public, res.Steward = site.Spec.Public != nil && box.Role == "manager", site.Spec.Steward
		return append(changes, f), nil
	})
	if err != nil {
		return nil, err
	}
	res.SSHKeys = s.managerKeys(ctx, owner)
	return res, nil
}

// managerKeys are the SSH keys of the members of the project that owns a site, checked and marked as
// the hourly bundle sends them, so the box's key block is the same from its join on.
func (s *server) managerKeys(ctx context.Context, project string) []string {
	ml := &v1alpha1.MemberList{}
	if s.c.List(ctx, ml) != nil {
		return []string{}
	}
	return warden.SSHKeys(ml.Items, project)
}

// recipients are the age keys a fabric-level secret is encrypted to: every steward's and the recovery
// key, plus the named sites'. extra holds keys not yet in the Fabric (a site joining now).
func (s *server) recipients(ctx context.Context, sites []v1alpha1.Site, also []string, extra map[string]string) ([]string, error) {
	want := map[string]bool{"recovery": true}
	for _, x := range sites {
		if x.Spec.Steward {
			want[x.Name] = true
		}
	}
	for _, a := range also {
		want[a] = true
	}
	out := []string{}
	for _, n := range slices.Sorted(maps.Keys(want)) {
		if k, ok := extra[n]; ok {
			out = append(out, k)
			continue
		}
		b, ok, err := s.git.Read(ctx, "keys/"+n+".age.pub")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("no age key for %s in the Fabric", n)
		}
		out = append(out, strings.TrimSpace(string(b)))
	}
	return out, nil
}

// secretPath is where a Secret of wecolab-system lives in the Fabric.
func secretPath(name string) string { return "secrets/" + name + ".sops.yaml" }

const kustomizationPath = "secrets/kustomization.yaml"

// secretFile is a Secret in wecolab-system as the Fabric keeps it: secrets/<name>.sops.yaml.
func secretFile(name string, data map[string]string, recipients []string) (fabric.FileChange, error) {
	sec := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: warden.SystemNS},
		Type: corev1.SecretTypeOpaque, StringData: data}
	plain, err := yaml.Marshal(sec)
	if err != nil {
		return fabric.FileChange{}, err
	}
	enc, err := fabric.Encrypt(plain, name+".sops.yaml", recipients)
	return fabric.FileChange{Path: secretPath(name), Content: enc}, err
}

// secretsKustomization is secrets/kustomization.yaml, as read, with one more file listed.
func secretsKustomization(snap *fabric.Snapshot, add string) (fabric.FileChange, error) {
	b, _ := snap.Get(kustomizationPath)
	var k struct {
		APIVersion string   `json:"apiVersion"`
		Kind       string   `json:"kind"`
		Resources  []string `json:"resources"`
	}
	if err := yaml.Unmarshal(b, &k); err != nil {
		return fabric.FileChange{}, err
	}
	if !slices.Contains(k.Resources, add) {
		k.Resources = append(k.Resources, add)
	}
	slices.Sort(k.Resources)
	k.APIVersion, k.Kind = "kustomize.config.k8s.io/v1beta1", "Kustomization"
	out, err := yaml.Marshal(k)
	return fabric.FileChange{Path: kustomizationPath, Content: out}, err
}

// secretsKustomizationWithout is secrets/kustomization.yaml without one secret's file.
func secretsKustomizationWithout(snap *fabric.Snapshot, drop string) (fabric.FileChange, error) {
	f, err := secretsKustomization(snap, drop)
	if err != nil {
		return f, err
	}
	var k map[string]any
	if err := yaml.Unmarshal(f.Content, &k); err != nil {
		return f, err
	}
	res, _ := k["resources"].([]any)
	k["resources"] = slices.DeleteFunc(res, func(x any) bool { return x == drop })
	f.Content, err = yaml.Marshal(k)
	return f, err
}

// siteKey is this site's age identity, to open what the Fabric encrypted for it.
func (s *server) siteKey(ctx context.Context) (string, error) {
	sec := &corev1.Secret{}
	if err := s.c.Get(s.elevated(ctx), types.NamespacedName{Namespace: warden.FluxNS, Name: "sops-age"}, sec); err != nil {
		return "", fmt.Errorf("this site's age key: %w", err)
	}
	for _, v := range sec.Data {
		return strings.TrimSpace(string(v)), nil
	}
	return "", fmt.Errorf("this site's age key is empty")
}

// openSecret is what a Secret file of the Fabric holds, opened with this steward's key.
func (s *server) openSecret(ctx context.Context, b []byte, name string) (map[string]string, error) {
	key, err := s.siteKey(ctx)
	if err != nil {
		return nil, err
	}
	plain, err := fabric.Decrypt(b, path.Base(name), key)
	if err != nil {
		return nil, err
	}
	sec := &corev1.Secret{}
	if err := yaml.Unmarshal(plain, sec); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range sec.Data {
		out[k] = string(v)
	}
	maps.Copy(out, sec.StringData)
	return out, nil
}

// sealed are the Fabric's encrypted files, each with the sites it is for besides the stewards and the
// recovery key: the fabric's own secrets, and every app's, also for the app's sites.
func (s *server) sealed(ctx context.Context) (map[string][]string, error) {
	out := map[string][]string{}
	files, err := s.git.List(ctx, "secrets")
	if err != nil {
		return nil, err
	}
	for _, p := range files {
		if strings.HasSuffix(p, ".sops.yaml") {
			out[p] = nil
		}
	}
	apps, err := s.appsInGit(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		folder, err := s.git.List(ctx, fabric.AppFolder(a.Namespace, a.Name))
		if err != nil {
			return nil, err
		}
		for _, p := range folder {
			if strings.HasSuffix(p, ".sops.yaml") {
				out[p] = a.Spec.Sites
			}
		}
	}
	return out, nil
}

// sealedNow are the sealed files as Git holds them now, each with its app's sites now. One added since
// the edit read its files cannot be encrypted in its commit: ErrConflict, and the person tries again.
func (s *server) sealedNow(ctx context.Context, read []string) (map[string][]string, error) {
	sealed, err := s.sealed(ctx)
	if err != nil {
		return nil, err
	}
	for p := range sealed {
		if !slices.Contains(read, p) {
			return nil, fabric.ErrConflict
		}
	}
	return sealed, nil
}

// sitePaths are the files of the sites in Git. An edit that encrypts for the stewards or counts them
// reads them all, so it commits only if the stewards are still the ones it saw.
func (s *server) sitePaths(ctx context.Context) ([]string, error) {
	l, err := s.git.List(ctx, "fabric/sites")
	return slices.DeleteFunc(l, func(p string) bool { return !strings.HasSuffix(p, ".yaml") }), err
}

// sitesOf are the sites among the files an edit read.
func sitesOf(snap *fabric.Snapshot, paths []string) ([]v1alpha1.Site, error) {
	out := []v1alpha1.Site{}
	for _, p := range paths {
		b, ok := snap.Get(p)
		if !ok || !strings.HasPrefix(p, "fabric/sites/") {
			continue
		}
		var x v1alpha1.Site
		if err := yaml.Unmarshal(b, &x); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, x)
	}
	return out, nil
}

// compact is paths sorted, each once.
func compact(paths []string) []string {
	slices.Sort(paths)
	return slices.Compact(paths)
}

// reencrypt encrypts the sealed files, as read, again for the stewards among sites (a steward was
// added or removed); extra holds age keys not yet in the Fabric.
func (s *server) reencrypt(ctx context.Context, snap *fabric.Snapshot, sites []v1alpha1.Site, sealed map[string][]string, extra map[string]string) ([]fabric.FileChange, error) {
	out := []fabric.FileChange{}
	if len(sealed) == 0 {
		return out, nil
	}
	key, err := s.siteKey(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range slices.Sorted(maps.Keys(sealed)) {
		b, ok := snap.Get(p)
		if !ok {
			continue
		}
		recips, err := s.recipients(ctx, sites, sealed[p], extra)
		if err != nil {
			return nil, err
		}
		enc, err := fabric.Reencrypt(b, path.Base(p), key, recips)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, fabric.FileChange{Path: p, Content: enc})
	}
	return out, nil
}

// setSteward makes a site a steward or not: it commits the flag and every secret encrypted anew. The
// writer stays a steward: only a steward's copy may take the fabric's changes.
func (s *server) setSteward(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	var in struct{ Steward *bool }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Steward == nil {
		http.Error(w, "steward (true or false) required", 400)
		return
	}
	name, ctx := r.PathValue("site"), r.Context()
	sealed, err := s.sealed(ctx)
	if err != nil {
		answer(w, err, 502)
		return
	}
	paths, err := s.sitePaths(ctx)
	if err != nil {
		answer(w, err, 502)
		return
	}
	sitePath := "fabric/sites/" + name + ".yaml"
	// Also read: the list of the fabric's secrets, so one added meanwhile makes this read again.
	paths = compact(append(append(paths, sitePath, settingsPath, kustomizationPath), slices.Collect(maps.Keys(sealed))...))
	n := 0
	err = s.edit(ctx, fmt.Sprintf("site %s: steward %v; secrets encrypted for the stewards", name, *in.Steward), paths, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		site := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
		b, ok := snap.Get(sitePath)
		if exists, err := decode(b, ok, site); err != nil || !exists {
			return nil, cmpErr(err, fail(404, "no such site"))
		}
		_, set, err := settingsOf(snap)
		if err != nil {
			return nil, err
		}
		if !*in.Steward && set.Writer == name {
			return nil, fail(409, "%s is the writer: take over at another steward first", name)
		}
		site.Spec.Steward = *in.Steward
		sites, err := sitesOf(snap, paths)
		if err != nil {
			return nil, err
		}
		stewards := 0
		for i := range sites {
			if sites[i].Name == name {
				sites[i] = *site
			}
			if sites[i].Spec.Steward {
				stewards++
			}
		}
		if stewards == 0 {
			return nil, fail(409, "the fabric needs at least one steward")
		}
		sealed, err := s.sealedNow(ctx, paths)
		if err != nil {
			return nil, err
		}
		changes, err := s.reencrypt(ctx, snap, sites, sealed, nil)
		if err != nil {
			return nil, err
		}
		n = len(changes)
		if err := s.dryRun(ctx, site); err != nil {
			return nil, err
		}
		f, err := s.fileOf(site, false)
		return append(changes, f), err
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "reencrypted": n})
}

// removeBox takes a box out of the Fabric and blocklists, in the same commit, the certificates it
// joined with as Git records them. Renewals are the stewards' to report: the writer's revocation loop
// blocklists those once the box is gone from its Site (the Console's pod cannot reach the stewards'
// certificate service, and should not).
func (s *server) removeBox(w http.ResponseWriter, r *http.Request) {
	name, boxName := r.PathValue("site"), r.PathValue("box")
	if !s.canSite(w, r, name) {
		return
	}
	ctx := s.elevated(r.Context())
	sites, err := s.sitesInGit(ctx)
	if err != nil {
		answer(w, err, 502)
		return
	}
	if site, box := warden.Find(sites, boxName); box == nil || site.Name != name {
		http.Error(w, "no such box", 404)
		return
	}
	sitePath := "fabric/sites/" + name + ".yaml"
	n := 0
	err = s.edit(ctx, fmt.Sprintf("%s: box %s removed, its certificates blocklisted", name, boxName), []string{sitePath, settingsPath}, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		site := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
		b, ok := snap.Get(sitePath)
		if _, err := decode(b, ok, site); err != nil {
			return nil, err
		}
		i := slices.IndexFunc(site.Spec.Boxes, func(b v1alpha1.Box) bool { return b.Name == boxName })
		if i < 0 {
			return nil, fail(404, "no such box")
		}
		if site.Spec.Boxes[i].Role == "manager" {
			return nil, fail(409, "a site's manager leaves with its site")
		}
		revoked := []warden.Revoked{}
		for _, c := range site.Spec.Boxes[i].Certs {
			revoked = append(revoked, warden.Revoked{Fingerprint: c.Fingerprint, Until: c.NotAfter.Time})
		}
		site.Spec.NextBox = nextBox(site) // before the box goes: a site from before nextBox may have counted from its address
		site.Spec.Boxes = slices.Delete(site.Spec.Boxes, i, i+1)
		cm, _, err := settingsOf(snap)
		if err != nil {
			return nil, err
		}
		cm.Data["blocklist"], n = blocklistWith(cm.Data["blocklist"], revoked)
		fs, err := settingsFile(cm)
		if err != nil {
			return nil, err
		}
		f, err := s.fileOf(site, false)
		return []fabric.FileChange{f, fs}, err
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "blocklisted": n})
}

// removeSite takes a site out of the fabric in one commit (decision 25): the site, its age key and its
// secret, its place in every app it held a standby for, the offers it made, and every certificate of its
// boxes onto the blocklist. From that commit the writer stops pushing to it and every box drops it within
// the hour. Then the vault key of every project whose database apps it held is rotated, since each of
// those apps' Secrets carried a copy. Refused for the writer, a steward (Stop being a steward first: that
// re-encrypts the fabric's secrets without it), the site running NetBird, and a site that is an app's
// primary or the origin of a move.
func (s *server) removeSite(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	name, ctx := r.PathValue("site"), r.Context()
	apps, err := s.appsInGit(ctx)
	if err != nil {
		answer(w, err, 502)
		return
	}
	offers, err := readDir[v1alpha1.Offer](ctx, s.git, "fabric/offers")
	if err != nil {
		answer(w, err, 502)
		return
	}
	sitePath, siteSecret, siteKey := "fabric/sites/"+name+".yaml", secretPath(warden.SiteSecret(name)), "keys/"+name+".age.pub"
	paths := []string{sitePath, siteSecret, siteKey, kustomizationPath, settingsPath}
	held := map[string]types.NamespacedName{} // the apps and offers it had, by path
	for i := range apps {
		if slices.Contains(apps[i].Spec.Sites, name) {
			p, _ := s.pathOf(&apps[i])
			held[p] = types.NamespacedName{Namespace: apps[i].Namespace, Name: apps[i].Name}
		}
	}
	for i := range offers {
		if offers[i].Spec.Site == name {
			p, _ := s.pathOf(&offers[i])
			held[p] = types.NamespacedName{Name: offers[i].Name}
		}
	}
	paths = append(paths, slices.Sorted(maps.Keys(held))...)
	var blocked, standbys, withdrawn int
	var rekey map[string]bool
	err = s.edit(ctx, fmt.Sprintf("site %s removed: its boxes' certificates blocklisted, its standbys and offers gone", name), paths, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		standbys, withdrawn, rekey = 0, 0, map[string]bool{}
		site := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
		b, ok := snap.Get(sitePath)
		if exists, err := decode(b, ok, site); err != nil || !exists {
			return nil, cmpErr(err, fail(404, "no such site"))
		}
		cm, set, err := settingsOf(snap)
		if err != nil {
			return nil, err
		}
		switch {
		case set.Writer == name:
			return nil, fail(409, "%s is the writer: take over at another steward first", name)
		case site.Spec.Steward:
			return nil, fail(409, "%s is a steward: Stop being a steward first, which re-encrypts the fabric's secrets without it", name)
		case set.People == name:
			return nil, fail(409, "%s runs NetBird, the people mesh, which cannot move yet", name)
		}
		changes := []fabric.FileChange{}
		for _, p := range slices.Sorted(maps.Keys(held)) {
			b, ok := snap.Get(p)
			if held[p].Namespace == "" { // an offer it made
				o := &v1alpha1.Offer{ObjectMeta: metav1.ObjectMeta{Name: held[p].Name}}
				if exists, err := decode(b, ok, o); err != nil || !exists || o.Spec.Site != name {
					if err != nil {
						return nil, err
					}
					continue
				}
				f, err := s.fileOf(o, true)
				if err != nil {
					return nil, err
				}
				changes, withdrawn = append(changes, f), withdrawn+1
				continue
			}
			a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: held[p].Namespace, Name: held[p].Name}}
			if exists, err := decode(b, ok, a); err != nil || !exists || !slices.Contains(a.Spec.Sites, name) {
				if err != nil {
					return nil, err
				}
				continue
			}
			if a.Spec.Primary == name || a.Spec.Handover != nil && a.Spec.Handover.From == name {
				return nil, fail(409, "%s/%s is served from %s: move its primary first", a.Namespace, a.Name, name)
			}
			a.Spec.Sites = slices.DeleteFunc(slices.Clone(a.Spec.Sites), func(x string) bool { return x == name })
			delete(a.Spec.Archive, name)
			if a.Spec.Database != "" && !a.Spec.Deleted {
				rekey[a.Namespace] = true
			}
			f, err := s.fileOf(a, false)
			if err != nil {
				return nil, err
			}
			changes, standbys = append(changes, f), standbys+1
		}
		revoked := []warden.Revoked{}
		for _, box := range site.Spec.Boxes {
			for _, c := range box.Certs {
				revoked = append(revoked, warden.Revoked{Fingerprint: c.Fingerprint, Until: c.NotAfter.Time})
			}
		}
		cm.Data["blocklist"], blocked = blocklistWith(cm.Data["blocklist"], revoked)
		fs, err := settingsFile(cm)
		if err != nil {
			return nil, err
		}
		gone, err := s.fileOf(site, true)
		if err != nil {
			return nil, err
		}
		changes = append(changes, fs, gone)
		for _, p := range []string{siteKey, siteSecret} {
			if _, ok := snap.Get(p); ok {
				changes = append(changes, fabric.FileChange{Path: p})
			}
		}
		k, err := secretsKustomizationWithout(snap, path.Base(siteSecret))
		return append(changes, k), err
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	rotated, failed := []string{}, map[string]string{}
	for _, p := range slices.Sorted(maps.Keys(rekey)) {
		if _, err := s.rotate(r, p); err != nil {
			failed[p] = err.Error()
		} else {
			rotated = append(rotated, p)
		}
	}
	writeJSON(w, map[string]any{"ok": true, "blocklisted": blocked, "standbys": standbys, "offers": withdrawn, "rotated": rotated, "notRotated": failed})
}

// fingerprintRe is a Nebula certificate fingerprint: the hex of a SHA-256.
var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// blocklistWith is the settings' blocklist with more certificates, each once, and how many it added.
// Only fingerprints go in: the list is text every site parses.
func blocklistWith(list string, add []warden.Revoked) (string, int) {
	list, n := strings.TrimSpace(list), 0
	for _, r := range add {
		if fingerprintRe.MatchString(r.Fingerprint) && !strings.Contains(list, r.Fingerprint) {
			list += "\n" + r.Fingerprint + " " + r.Until.UTC().Format(time.RFC3339)
			n++
		}
	}
	return strings.TrimSpace(list) + "\n", n
}

func randHexStr(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// siteStatuses is what each site's Warden publishes, asked in parallel; an unreachable site is absent.
func (s *server) siteStatuses(ctx context.Context) (map[string]*warden.SiteStatus, []v1alpha1.Site) {
	ctx = s.elevated(ctx)
	sl := &v1alpha1.SiteList{}
	_ = s.c.List(ctx, sl)
	out := map[string]*warden.SiteStatus{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, site := range sl.Items {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if st, ok := s.peers.Get(ctx, name); ok {
				mu.Lock()
				out[name] = st
				mu.Unlock()
			}
		}(site.Name)
	}
	wg.Wait()
	return out, sl.Items
}
