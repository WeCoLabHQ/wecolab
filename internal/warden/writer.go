package warden

import (
	"context"
	"crypto/sha256"
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
	Client client.Client
	Site   string
	Git    *fabric.Git // this site's copy
	Peers  *Peers
	HTTP   *http.Client // to other sites' Warden; nil: a short timeout

	mu      sync.Mutex
	reseeds map[string]time.Time
	mirrors map[string]mirrorState // push mirrors this Warden set up, by remote
	notes   map[string]string      // what each vault's old keys wait for, as last logged
}

// mirrorState is how this Warden last set up a push mirror: with which password, and when.
type mirrorState struct {
	password [32]byte
	at       time.Time
}

// Claim is who a site follows as writer, and at which epoch.
type Claim struct {
	Writer string
	Epoch  int
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
	// A Recreate cut short, or a Forgejo installed again, leaves a copy that answers 404 to everything
	// below, which would pass for success.
	if err := w.Git.Ensure(ctx, ForgejoMirror); err != nil {
		return fmt.Errorf("this copy of the Fabric: %w", err)
	}
	own, err := GitClaim(ctx, w.Git)
	if err != nil {
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
	if eff.Writer == w.Site {
		return w.lead(ctx, sites.Items, own)
	}
	return w.follow(ctx, sites.Items, eff.Writer, stewards[w.Site], own.Writer == w.Site)
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

func forgejoURL(ip string) string {
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(nebula.PortForgejo)) + "/" + ForgejoOwner + "/fabric.git"
}

// mirrorPassword is a site's Forgejo mirroring password, "" until its secret has reached this site.
func (w *Writer) mirrorPassword(ctx context.Context, site string) string {
	s := &corev1.Secret{}
	if err := w.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: SiteSecret(site)}, s); err != nil {
		return ""
	}
	return string(s.Data[KeyMirror])
}

// fresh is whether a push mirror can stay: this Warden set it up with the site's password as it is
// now, and it has not been failing for long. Anything else is set up again, which is what repairs a
// mirror after a password change or a site's Forgejo installed anew.
func (w *Writer) fresh(pm fabric.PushMirror, password string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.mirrors[pm.Remote]
	return ok && st.password == sha256.Sum256([]byte(password)) && (pm.LastError == "" || now.Sub(st.at) < 10*time.Minute)
}

// lead: admit the Console, push to every other copy, revoke, ask for rebuilds, make the handovers'
// steps, retire vaults' old keys. own is what this copy names the writer: empty while it lacks the
// Fabric's settings.
func (w *Writer) lead(ctx context.Context, sites []v1alpha1.Site, own Claim) error {
	head, err := w.Git.Head(ctx)
	if err != nil {
		return err
	}
	// Only a copy that holds the Fabric admits the Console and pushes: a commit into an emptied copy
	// would be a new root that every other copy starts over from, and Flux would then prune everything.
	holds := head != "" && own.Writer != ""
	pushers := []string{ForgejoMirror}
	if holds {
		pushers = append(pushers, ForgejoOwner)
	}
	if err := w.Git.Protect(ctx, pushers); err != nil {
		return fmt.Errorf("protect main: %w", err)
	}
	// The planned moves' steps first, and whatever else fails: a move waits on neither a site's push
	// mirror nor the stewards' certificate lists, and while it waits the app has no writable primary.
	return errors.Join((&Handovers{Client: w.Client, Site: w.Site, Git: w.Git, Peers: w.Peers}).Steps(ctx),
		w.mirror(ctx, sites, holds), w.revoke(ctx, sites), w.reseed(ctx), w.retire(ctx))
}

// mirror keeps a push mirror to every other site's copy while this copy holds the Fabric, and none
// otherwise.
func (w *Writer) mirror(ctx context.Context, sites []v1alpha1.Site, holds bool) error {
	want := map[string]string{} // remote -> site
	for _, site := range sites {
		if m := site.Manager(); m != nil && site.Name != w.Site && holds {
			want[forgejoURL(m.IP)] = site.Name
		}
	}
	have, err := w.Git.PushMirrors(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, pm := range have {
		if site, ok := want[pm.Remote]; ok && pm.Filter == "main" {
			if pw := w.mirrorPassword(ctx, site); pw == "" || w.fresh(pm, pw, now) {
				delete(want, pm.Remote)
				continue
			}
		}
		if err := w.Git.DeletePushMirror(ctx, pm.Name); err != nil {
			return err
		}
	}
	remotes := make([]string, 0, len(want))
	for r := range want {
		remotes = append(remotes, r)
	}
	sort.Strings(remotes)
	added, errs := false, []error{}
	for _, r := range remotes {
		pw := w.mirrorPassword(ctx, want[r])
		if pw == "" {
			continue // the site's password has not reached this copy yet
		}
		if err := w.Git.AddPushMirror(ctx, r, ForgejoMirror, pw, "main"); err != nil {
			errs = append(errs, fmt.Errorf("push to %s: %w", want[r], err)) // the other sites still get theirs
			continue
		}
		w.mu.Lock()
		if w.mirrors == nil {
			w.mirrors = map[string]mirrorState{}
		}
		w.mirrors[r] = mirrorState{password: sha256.Sum256([]byte(pw)), at: now}
		w.mu.Unlock()
		log.FromContext(ctx).Info("pushing the Fabric to a site", "site", want[r])
		added = true
	}
	if added { // a new or returning copy gets the history now, not at the next interval
		errs = append(errs, w.Git.SyncPushMirrors(ctx))
	}
	return errors.Join(errs...)
}

const supersededFilter = "superseded-*"

// follow: admit only the writer's pushes. A copy whose head is not on the writer's main cannot take
// them (Forgejo refuses a force push to a protected branch), so it starts over from the writer's
// history: a steward first keeps its commits on the writer as a branch; any other site's were never the
// fabric's, since only stewards write. Nothing starts over from a writer whose copy is empty, nor, unless
// the writer holds its head, a copy that names this site the writer (claimed): its history may be the
// fabric's newest, whatever the steward flags say.
func (w *Writer) follow(ctx context.Context, sites []v1alpha1.Site, writer string, steward, claimed bool) error {
	if err := w.Git.Protect(ctx, []string{ForgejoMirror}); err != nil {
		return fmt.Errorf("protect main: %w", err)
	}
	var wm *v1alpha1.Box
	for i := range sites {
		if sites[i].Name == writer {
			wm = sites[i].Manager()
		}
	}
	aside := "" // where a steward keeps its commits aside: the writer's copy
	if wm != nil && steward {
		aside = forgejoURL(wm.IP)
	}
	have, err := w.Git.PushMirrors(ctx)
	if err != nil {
		return err
	}
	kept := false
	for _, pm := range have {
		if pm.Remote == aside && pm.Filter == supersededFilter {
			kept = true
			continue
		}
		if err := w.Git.DeletePushMirror(ctx, pm.Name); err != nil {
			return err
		}
	}
	head, err := w.Git.Head(ctx)
	if err != nil || wm == nil || head == "" {
		return err
	}
	at, err := w.ask(ctx, wm.IP, head)
	if err != nil || at.Head == "" || at.OnMain {
		return err // on the writer's main, its pushes fast-forward this copy
	}
	if steward && !at.Has {
		branch := "superseded-" + w.Site + "-" + head[:min(12, len(head))] // the same branch every pass
		if err := w.Git.Branch(ctx, branch); err != nil || kept {
			return err
		}
		pw := w.mirrorPassword(ctx, writer)
		if pw == "" {
			return nil
		}
		log.FromContext(ctx).Info("this copy went its own way; keeping its commits on the writer as a branch", "branch", branch, "writer", writer)
		if err := w.Git.AddPushMirror(ctx, aside, ForgejoMirror, pw, supersededFilter); err != nil {
			return err
		}
		return w.Git.SyncPushMirrors(ctx)
	}
	if claimed && !at.Has {
		log.FromContext(ctx).Info("this copy names this site the writer, but a steward's claim wins and lacks its commits; keeping them until a person decides", "head", head, "writer", writer)
		return nil
	}
	if now, err := w.Git.Head(ctx); err != nil || now != head {
		return err // a push arrived meanwhile: look again next pass
	}
	log.FromContext(ctx).Info("this copy is not on the writer's history; starting over from it", "head", head, "writer", writer)
	return w.Git.Recreate(ctx, ForgejoMirror)
}

// CopyAnswer is what a site's Warden says about its copy of the Fabric, asked about a commit.
type CopyAnswer struct {
	Head   string `json:"head"`   // main's commit, "" while the copy is empty
	OnMain bool   `json:"onMain"` // the commit is one of main's (newest) commits
	Has    bool   `json:"has"`    // the commit is anywhere in the copy, a kept-aside branch included
}

// onMainDepth is how far back a follower's head counts as merely behind.
const onMainDepth = 200

var (
	commitID          = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	nebulaFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`) // a certificate's SHA-256
)

// ServeHTTP answers GET /fabric/commits/<sha> on the status port, from this site's copy: its head, and
// whether the commit is on main or anywhere. Followers ask the writer this before they keep their
// commits aside or start over; they need no account on the writer's Forgejo for it.
func (w *Writer) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	sha, ok := strings.CutPrefix(r.URL.Path, "/fabric/commits/")
	if !ok || r.Method != http.MethodGet || !commitID.MatchString(sha) {
		http.NotFound(rw, r)
		return
	}
	ctx := r.Context()
	var a CopyAnswer
	var err error
	if a.Head, err = w.Git.Head(ctx); err == nil {
		if a.OnMain, err = w.Git.OnMain(ctx, sha, onMainDepth); err == nil {
			a.Has, err = w.Git.HasCommit(ctx, sha)
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
// expiry (docs/plans/2026-09-29-hardening.md, R7). The cluster's copy only says whether to look; what is
// committed is decided against the Sites and settings in Git.
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
	set, err := ReadSettings(ctx, w.Client)
	if err != nil {
		return err
	}
	now := time.Now()
	boxes := map[string]bool{}
	for _, s := range sites {
		for _, b := range s.Spec.Boxes {
			boxes[b.Name] = true
		}
	}
	if _, changed := Revoke(set.Blocklisted, boxes, issued, now); !changed {
		return nil
	}
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
