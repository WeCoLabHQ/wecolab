package warden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/nebula"
)

// SiteSecret is, at stewards, the Secret holding a site's k3s tokens and the password of its Forgejo's
// mirroring account (the keys below).
func SiteSecret(site string) string { return "site-" + site }

// Keys of a site's Secret. The server token never leaves the site's managers: nodes join with the agent
// token, which cannot fetch the cluster's bootstrap data or CA keys (docs/plans/2026-09-29-hardening.md, R5).
const (
	KeyK3sServer = "k3s"
	KeyK3sAgent  = "agent"
	KeyMirror    = "mirror"
)

// Accounts on every site's Forgejo: the owner of the Fabric's repository, which the Console and Flux
// use, and the account the writer pushes with.
const (
	ForgejoOwner  = "fabric"
	ForgejoMirror = "mirror"
)

// SettingsPath is the fabric's settings in the Fabric, which Flux applies as the SettingsName ConfigMap.
const SettingsPath = "system/base/fabric.yaml"

const settingsHeader = "# The fabric's settings (docs/architecture.md). Changing the writer raises the epoch.\n"

// Writer does every site's part in keeping one writer. At the writer, its copy admits the Console and
// pushes every commit to every other copy; everywhere else, the copy admits only the writer's pushes.
// Who is the writer comes from this site's own copy of the Fabric, not from what Flux has applied yet,
// so a steward that has just taken over leads at once. A steward that sees another claim the role with
// a higher epoch steps down, and if its copy went its own way meanwhile, keeps those commits aside as a
// branch on the new writer and takes the new writer's history; any other site simply takes it. At the
// writer it also blocklists the certificates of boxes that left the Fabric, asks for standby databases
// that cannot recover to be rebuilt, makes the planned moves' steps (Handovers), and deletes vaults' old
// keys once every site has the new one (retire).
type Writer struct {
	Client       client.Client
	APIReader    client.Reader // uncached lifecycle observations; nil uses Client
	Site         string
	Git          *fabric.Git // this site's copy
	Peers        *Peers
	Coordination coordinationclient.CoordinationV1Interface // local writer transition lease
	HTTP         *http.Client                               // to other sites' Warden; nil: a short timeout

	syncMu  sync.Mutex // only Run/Sync recovers an uncertain repository deletion
	mu      sync.Mutex
	reseeds map[string]time.Time
	notes   map[string]string // what each vault's old keys wait for, as last logged
}

// Claim is who a site follows as writer, and at which epoch.
type Claim struct {
	Writer string `json:"writer"`
	Epoch  int    `json:"epoch"`
}

// Effective is the writer everyone should follow: the highest epoch claimed by a steward, then the
// lowest site name, starting from what this site's Fabric says. A claim naming a site that is not a
// steward counts for nothing, this site's own included.
func Effective(own Claim, claims []Claim, stewards map[string]bool) Claim {
	best := Claim{}
	for _, c := range append([]Claim{own}, claims...) {
		if !stewards[c.Writer] {
			continue
		}
		if best.Writer == "" || c.Epoch > best.Epoch || c.Epoch == best.Epoch && c.Writer < best.Writer {
			best = c
		}
	}
	return best
}

// Run keeps it until ctx ends.
func (w *Writer) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := w.Sync(ctx); err != nil {
			log.FromContext(ctx).Error(err, "writer")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync does one pass.
func (w *Writer) Sync(ctx context.Context) error {
	w.syncMu.Lock()
	defer w.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var own Claim
	recovered := false
	err := withWriterLock(ctx, w.Coordination, func(ctx context.Context) error {
		journal, err := readRecreation(ctx, w.Coordination)
		if err != nil {
			return err
		}
		if journal != nil {
			recovered = true
			return w.recoverRecreation(ctx, *journal)
		}
		// Ensure must not race an uncertain DELETE or create a replacement
		// while old Forgejo filesystem work may still be running.
		if err := w.Git.Ensure(ctx, ForgejoMirror); err != nil {
			return fmt.Errorf("this copy of the Fabric: %w", err)
		}
		own, err = GitClaim(ctx, w.Git)
		return err
	})
	if err != nil || recovered {
		return err
	}
	sites := &v1alpha1.SiteList{}
	if err := w.Client.List(ctx, sites); err != nil {
		return err
	}
	stewards := map[string]bool{}
	claims := []Claim{}
	for _, site := range sites.Items {
		if site.Spec.Steward {
			stewards[site.Name] = true
			if st, ok := w.Peers.Get(ctx, site.Name); ok && st.Writer != "" {
				claims = append(claims, Claim{Writer: st.Writer, Epoch: st.Epoch})
			}
		}
	}
	eff := Effective(own, claims, stewards)
	lead := false
	err = withWriterLock(ctx, w.Coordination, func(ctx context.Context) error {
		if journal, err := readRecreation(ctx, w.Coordination); err != nil {
			return err
		} else if journal != nil {
			return fmt.Errorf("repository recreation pending")
		}
		if err := w.quiesceMirrors(ctx); err != nil {
			return err
		}
		// Discovery and the rollout barrier may outlive a takeover or push.
		current, err := GitClaim(ctx, w.Git)
		if err != nil || current != own {
			return err
		}
		if eff.Writer == w.Site {
			if err := w.admitLead(ctx, own); err != nil {
				return err
			}
			lead = true
			return nil
		}
		return w.follow(ctx, sites.Items, eff.Writer, stewards[w.Site], own.Writer == w.Site)
	})
	if err != nil || !lead {
		return err
	}
	return w.lead(ctx, sites.Items, own)
}

// GitClaim is who a copy of the Fabric names as the writer; nothing while the copy is empty. It is what
// this site follows, and what its /status should say.
func GitClaim(ctx context.Context, g *fabric.Git) (Claim, error) {
	b, ok, err := g.Read(ctx, SettingsPath)
	if err != nil || !ok {
		return Claim{}, err
	}
	cm := &corev1.ConfigMap{}
	if err := yaml.Unmarshal(b, cm); err != nil {
		return Claim{}, fmt.Errorf("%s: %w", SettingsPath, err)
	}
	c := Claim{Writer: cm.Data["writer"]}
	if v := cm.Data["epoch"]; v != "" {
		if c.Epoch, err = strconv.Atoi(v); err != nil {
			return Claim{}, fmt.Errorf("%s: epoch: %w", SettingsPath, err)
		}
	}
	return c, nil
}

// mirrorPassword is a site's Forgejo mirroring password, "" until its secret has reached this site.
func (w *Writer) mirrorPassword(ctx context.Context, site string) string {
	s := &corev1.Secret{}
	if err := w.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: SiteSecret(site)}, s); err != nil {
		return ""
	}
	return string(s.Data[KeyMirror])
}

// admitLead opens the copy for Console commits only when it holds the Fabric. Call under
// the local writer transition lease, after re-reading the claim.
func (w *Writer) admitLead(ctx context.Context, own Claim) error {
	if journal, err := readRecreation(ctx, w.Coordination); err != nil {
		return err
	} else if journal != nil {
		return fmt.Errorf("repository recreation pending")
	}
	head, err := w.Git.Head(ctx)
	if err != nil {
		return err
	}
	// An emptied copy cannot accept a new root: Flux would prune the Fabric.
	pushers := []string{ForgejoMirror}
	if head != "" && own.Writer == w.Site {
		pushers = append(pushers, ForgejoOwner)
	}
	if err := w.Git.Protect(ctx, pushers); err != nil {
		return fmt.Errorf("protect main: %w", err)
	}
	return nil
}

// lead does maintenance outside the writer transition lease. It must not delay a takeover.
func (w *Writer) lead(ctx context.Context, sites []v1alpha1.Site, own Claim) error {
	head, err := w.Git.Head(ctx)
	if err != nil {
		return err
	}
	// Moves wait on neither replication nor certificate lists.
	return errors.Join((&Handovers{Client: w.Client, Site: w.Site, Git: w.Git, Peers: w.Peers}).Steps(ctx),
		w.replicate(ctx, sites, head, own), w.revoke(ctx, sites), w.reseed(ctx), w.retire(ctx))
}

// CopyAnswer is an ancestry observation, never a custody receipt.
type CopyAnswer struct {
	Head   string `json:"head"`
	OnMain bool   `json:"onMain"`
	Claim  Claim  `json:"claim"`
}

// onMainDepth is how far back a follower's head counts as merely behind.
const onMainDepth = 200

var (
	commitID          = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	nebulaFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`) // a certificate's SHA-256
)

// ServeHTTP exposes ancestry observations and receiver-serialized preservation
// on the manager-only Nebula status surface.
func (w *Writer) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/fabric/preserve" && r.Method == http.MethodPost {
		w.servePreservation(rw, r)
		return
	}
	sha, ok := strings.CutPrefix(r.URL.Path, "/fabric/commits/")
	if !ok || r.Method != http.MethodGet || !commitID.MatchString(sha) {
		http.NotFound(rw, r)
		return
	}
	ctx := r.Context()
	var a CopyAnswer
	var err error
	if a.Head, err = w.Git.Head(ctx); err == nil && a.Head != "" {
		if a.Claim, err = claimAt(ctx, w.Git, a.Head); err == nil {
			a.OnMain, err = w.Git.OnMain(ctx, sha, onMainDepth)
		}
	}
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSONResponse(rw, a)
}

func (w *Writer) ask(ctx context.Context, ip, sha string) (*CopyAnswer, error) {
	a := &CopyAnswer{}
	return a, w.getJSON(ctx, "http://"+net.JoinHostPort(ip, strconv.Itoa(nebula.PortWarden))+"/fabric/commits/"+sha, a)
}

func (w *Writer) getJSON(ctx context.Context, url string, out any) error {
	hc := w.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// Revoke is the blocklist with every certificate issued for a box not in boxes added, and entries past
// their expiry dropped; changed says whether it differs from list. Anything issued that is not a
// fingerprint is skipped: each becomes a line of the blocklist.
func Revoke(list []Revoked, boxes map[string]bool, issued map[string][]Issued, now time.Time) (out []Revoked, changed bool) {
	out = []Revoked{}
	seen := map[string]bool{}
	for _, r := range list {
		if r.Until.After(now) && !seen[r.Fingerprint] {
			out = append(out, r)
			seen[r.Fingerprint] = true
		}
	}
	changed = len(out) != len(list)
	names := make([]string, 0, len(issued))
	for box := range issued {
		names = append(names, box)
	}
	sort.Strings(names)
	for _, box := range names {
		for _, it := range issued[box] {
			if !boxes[box] && it.NotAfter.After(now) && !seen[it.Fingerprint] && nebulaFingerprint.MatchString(it.Fingerprint) {
				out = append(out, Revoked{Fingerprint: it.Fingerprint, Until: it.NotAfter})
				seen[it.Fingerprint], changed = true, true
			}
		}
	}
	return out, changed
}

// revoke blocklists in the Fabric every certificate issued for a box that has left it (renewals and the
// join certificates the stewards have seen recorded, as they publish them), and drops entries past their
// expiry (docs/plans/2026-09-29-hardening.md, R7). Git decides membership and the current blocklist;
// waiting for an applied Site to lose a box would delay revocation by another Flux reconciliation.
func (w *Writer) revoke(ctx context.Context, sites []v1alpha1.Site) error {
	issued := map[string][]Issued{}
	for _, ip := range Stewards(sites) {
		got := map[string][]Issued{}
		if err := w.getJSON(ctx, "http://"+net.JoinHostPort(ip, strconv.Itoa(nebula.PortCerts))+"/nebula/issued", &got); err != nil {
			log.FromContext(ctx).Info("a steward's certificates are not known yet", "steward", ip, "error", err.Error())
			continue
		}
		for box, l := range got {
			issued[box] = append(issued[box], l...)
		}
	}
	now := time.Now()
	files, err := w.Git.List(ctx, "fabric/sites")
	if err != nil {
		return err
	}
	_, err = w.Git.Edit(ctx, fabric.Author{Name: "Warden", Email: "warden@" + w.Site}, "blocklist the certificates of boxes that left the Fabric",
		append([]string{SettingsPath}, files...), func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
			inGit, self := map[string]bool{}, false
			for _, p := range files {
				b, _ := snap.Get(p)
				s := &v1alpha1.Site{}
				if err := yaml.Unmarshal(b, s); err != nil {
					return nil, fmt.Errorf("%s: %w", p, err)
				}
				self = self || s.Name == w.Site
				for _, x := range s.Spec.Boxes {
					inGit[x.Name] = true
				}
			}
			if !self { // the Sites were not all read: revoking now could cut off every box
				return nil, fmt.Errorf("this site is not among the Sites in the Fabric; nothing revoked")
			}
			b, _ := snap.Get(SettingsPath)
			cm := &corev1.ConfigMap{}
			if err := yaml.Unmarshal(b, cm); err != nil {
				return nil, err
			}
			cur, err := ParseSettings(cm.Data)
			if err != nil {
				return nil, err
			}
			list, changed := Revoke(cur.Blocklisted, inGit, issued, now)
			if !changed {
				return nil, nil
			}
			lines := ""
			for _, r := range list {
				lines += r.Fingerprint + " " + r.Until.UTC().Format(time.RFC3339) + "\n"
			}
			cm.Data["blocklist"] = lines
			out, err := yaml.Marshal(cm)
			return []fabric.FileChange{{Path: SettingsPath, Content: append([]byte(settingsHeader), out...)}}, err
		})
	return err
}

// reseed asks for a standby that reports it cannot recover to be rebuilt from the vault: a new archive
// generation for its site. At most once per half hour per database and site.
func (w *Writer) reseed(ctx context.Context) error {
	apps := &v1alpha1.AppList{}
	if err := w.Client.List(ctx, apps); err != nil {
		return err
	}
	for _, app := range apps.Items {
		if app.Spec.Database == "" {
			continue
		}
		_, db, _ := w.Peers.Gather(ctx, &app, "")
		for _, site := range app.Spec.Sites {
			if site == app.Spec.Primary || !strings.EqualFold(str(db[site], "phase"), cnpgUnrecoverable) {
				continue
			}
			key := app.Namespace + "/" + app.Name + "@" + site
			w.mu.Lock()
			if w.reseeds == nil {
				w.reseeds = map[string]time.Time{}
			}
			recent := time.Since(w.reseeds[key]) < 30*time.Minute
			w.mu.Unlock()
			if recent {
				continue
			}
			sha, err := w.rebuild(ctx, app.Namespace, app.Name, site)
			if err != nil {
				return err
			}
			w.mu.Lock()
			w.reseeds[key] = time.Now()
			w.mu.Unlock()
			if sha != "" {
				log.FromContext(ctx).Info("standby cannot recover: rebuild from the vault committed", "app", key)
			}
		}
	}
	return nil
}

// rebuild commits a new archive generation for one of an app's standbys, decided against the App in
// Git: never for its primary, nor for the site a planned move is taking the primary from
// (docs/plans/2026-09-29-hardening.md, R4). "" when nothing was committed.
func (w *Writer) rebuild(ctx context.Context, ns, name, site string) (string, error) {
	gvk := v1alpha1.GroupVersion.WithKind("App")
	p, _ := fabric.Path(gvk, ns, name)
	return w.Git.Edit(ctx, fabric.Author{Name: "Warden", Email: "warden@" + w.Site}, fmt.Sprintf("%s/%s: rebuild %s's database from the vault", ns, name, site),
		[]string{p}, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
			b, ok := snap.Get(p)
			if !ok {
				return nil, nil
			}
			a := &v1alpha1.App{}
			if err := yaml.Unmarshal(b, a); err != nil {
				return nil, err
			}
			if !slices.Contains(a.Spec.Sites, site) || site == a.Spec.Primary || a.Spec.Handover != nil && site == a.Spec.Handover.From {
				return nil, nil
			}
			archive := map[string]int{}
			for k, v := range a.Spec.Archive {
				archive[k] = v
			}
			archive[site] = max(archive[site], 1) + 1
			a.Spec.Archive = archive
			out, err := fabric.YAML(a, gvk)
			return []fabric.FileChange{{Path: p, Content: out}}, err
		})
}

const cnpgUnrecoverable = "Cluster is unrecoverable and needs manual intervention"
