// Package bootstrap makes a new fabric: the Fabric repository's first commit from the template, with
// its keys, its Nebula CA, the first box's certificate and every generated secret, encrypted
// (docs/install.md, step 2).
package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/nebula"
	"wecolab.io/wecolab/internal/validate"
	"wecolab.io/wecolab/internal/warden"
)

//go:embed all:template
var template_ embed.FS

// Vendored are the upstream manifests the Fabric pins, by folder under system/vendor.
var Vendored = map[string]string{
	"flux":         "https://github.com/fluxcd/flux2/releases/download/v2.9.5/install.yaml",
	"cert-manager": "https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml",
	"cnpg":         "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/2a35abb4628f209d149825ef3c38011e0701ff2f/releases/cnpg-1.30.1.yaml",
	"barman":       "https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/v0.15.0/manifest.yaml",
}

// VendorSHA256 is checked before a fetched manifest becomes deployable platform state.
var VendorSHA256 = map[string]string{
	"flux":         "cc3dcd743af16215838b6937e1fce83745bf24c0dcc6c59737c59df15429caaf",
	"cert-manager": "e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f",
	"cnpg":         "37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb",
	"barman":       "1c483eae12a7424ad28ac66bdfee771b510ee8e234cb8756c3ade7254cef2fad",
}

// Options are what the install step knows.
type Options struct {
	Zone, Email, Site, Project string
	PublicAddress              string // the first box's public IPv4
	Network                    netip.Prefix
	Version                    string // WeCoLab's image tag
	Host                       string // the first box's host name
	BoxKey                     []byte // the first box's Nebula public key, PEM
	Dev                        bool
	LockFlux                   bool // Upgrade: lock down the Flux of a fabric made before the lockdown (keepFlux)
	SchemaOnly                 bool // Upgrade: first commit additive CRDs without changing running controllers
	Now                        time.Time
	Fetch                      func(url string) ([]byte, error) // nil: HTTP
}

// Result is what the install step needs from the new fabric that is not in the repository: every
// private value, to hand to the box and then forget.
type Result struct {
	Box            v1alpha1.Box `json:"box"`
	CA             string       `json:"ca"`
	Cert           string       `json:"cert"`
	SiteKey        string       `json:"siteKey"`       // this site's age identity
	RecoveryKey    string       `json:"recoveryKey"`   // the recovery card
	K3sToken       string       `json:"k3sToken"`      // the site's k3s server token: managers only
	K3sAgentToken  string       `json:"k3sAgentToken"` // what the site's nodes join with (k3s agent-token)
	MirrorPassword string       `json:"mirrorPassword"`
	Config         string       `json:"config"` // the first box's /etc/nebula/config.d/20-fabric.yml
	Life           int          `json:"life"`   // a certificate's lifetime, seconds
}

// CALife is how long the fabric's Nebula CA lasts; it is rotated well before (docs/operations.md).
const CALife = 5 * 365 * 24 * time.Hour

// check is the names the fabric is made with: they become objects, paths, certificates and DNS records.
func (o Options) check() error {
	for _, err := range []error{validate.DNSName(o.Zone), validate.Email(o.Email), validate.Name(o.Site), validate.Name(o.Project),
		validate.Label(o.Host, 0), validate.Label(o.Site+"-"+o.Host, 0)} {
		if err != nil {
			return err
		}
	}
	if a, err := netip.ParseAddr(o.PublicAddress); o.PublicAddress != "" && (err != nil || !a.Is4()) {
		return fmt.Errorf("%q is not an IPv4 address", o.PublicAddress)
	}
	if !nebula.Nth(o.Network, 0, 1).IsValid() { // sites take /24s of it: IPv4, /16 to /24
		return fmt.Errorf("the Nebula network %s cannot hold sites: use an IPv4 prefix from /16 to /24", o.Network)
	}
	return nil
}

// Create writes the new fabric's repository into dir.
func Create(o Options, dir string) (*Result, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	files, err := Platform(o)
	if err != nil {
		return nil, err
	}
	for p, b := range files {
		if err := write(filepath.Join(dir, filepath.FromSlash(p)), b); err != nil {
			return nil, err
		}
	}
	// Fresh fabrics start schema-aware. Legacy fabrics must use the gated staged migration;
	// an absent gate never silently authorizes old writers.
	ready, _ := json.Marshal(fabric.UpgradeReport{Phase: "ready", Sites: []string{o.Site}})
	for name, content := range map[string][]byte{
		fabric.UpgradePath:           ready,
		fabric.PlacementRevisionPath: []byte(`{"revision":"0"}`),
		fabric.MigrationCompletePath: []byte(`{"archiveIDs":true,"routeClaims":true,"keyVersions":true}`),
	} {
		if err := write(filepath.Join(dir, filepath.FromSlash(name)), content); err != nil {
			return nil, err
		}
	}

	res := &Result{}
	siteID, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	recID, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	res.SiteKey, res.RecoveryKey = siteID.String(), recID.String()
	recipients := []string{siteID.Recipient().String(), recID.Recipient().String()}
	for n, v := range map[string]string{o.Site: recipients[0], "recovery": recipients[1]} {
		if err := write(filepath.Join(dir, "keys", n+".age.pub"), []byte(v+"\n")); err != nil {
			return nil, err
		}
	}

	// Nebula: the CA and the first box, a public steward's manager.
	caCrt, caKey, err := nebula.NewCA(o.Zone, o.Network, CALife, o.Now)
	if err != nil {
		return nil, err
	}
	res.Box = v1alpha1.Box{Name: o.Site + "-" + o.Host, IP: nebula.Nth(o.Network, 0, 1).String(), Role: "manager", Key: string(o.BoxKey), Added: metav1.NewTime(o.Now)}
	site := &v1alpha1.Site{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Site"}, ObjectMeta: metav1.ObjectMeta{Name: o.Site},
		Spec: v1alpha1.SiteSpec{Owner: o.Project, Index: 0, Steward: true, Boxes: []v1alpha1.Box{res.Box}}}
	if o.PublicAddress != "" {
		site.Spec.Public = &v1alpha1.Public{Address: o.PublicAddress}
	}
	addr, err := nebula.Addr(res.Box, o.Network)
	if err != nil {
		return nil, err
	}
	life := 720 * time.Hour
	if o.Dev {
		life = time.Hour
	}
	crt, _, err := nebula.Sign(caCrt, caKey, nebula.Request{Name: res.Box.Name, Addr: addr, Groups: nebula.Groups(site, res.Box), PubPEM: o.BoxKey}, life, o.Now)
	if err != nil {
		return nil, err
	}
	cfg, err := nebula.FabricConfig(site, res.Box, nebula.Lighthouses([]v1alpha1.Site{*site}), nil)
	if err != nil {
		return nil, err
	}
	res.CA, res.Cert, res.Config, res.Life = string(caCrt), string(crt), string(cfg), int(life.Seconds())

	// Generated secrets, encrypted to this site and the recovery key.
	res.K3sToken, res.K3sAgentToken, res.MirrorPassword = random(24), random(24), random(24)
	secrets := map[string]map[string]string{
		"nebula-ca":      {"ca.crt": string(caCrt), "ca.key": string(caKey)},
		"site-" + o.Site: {warden.KeyK3sServer: res.K3sToken, warden.KeyK3sAgent: res.K3sAgentToken, warden.KeyMirror: res.MirrorPassword},
		"console":        {"session-key": base64.StdEncoding.EncodeToString([]byte(random(32)))},
	}
	if !o.Dev {
		secrets["netbird"] = map[string]string{"url": "https://mesh." + o.Zone, "config.yaml": netbirdConfig(o)}
	}
	names := []string{}
	for name, data := range secrets {
		s := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: warden.SystemNS},
			Type: corev1.SecretTypeOpaque, StringData: data}
		plain, err := yaml.Marshal(s)
		if err != nil {
			return nil, err
		}
		file := name + ".sops.yaml"
		enc, err := fabric.Encrypt(plain, file, recipients)
		if err != nil {
			return nil, err
		}
		if err := write(filepath.Join(dir, "secrets", file), enc); err != nil {
			return nil, err
		}
		names = append(names, file)
	}
	if err := write(filepath.Join(dir, "secrets/kustomization.yaml"), kustomization(names)); err != nil {
		return nil, err
	}

	// The fabric's first objects and settings.
	owner := &v1alpha1.Member{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Member"}, ObjectMeta: metav1.ObjectMeta{Name: "owner"},
		Spec: v1alpha1.MemberSpec{Email: o.Email, Role: "owner", Projects: []string{o.Project}}}
	ns := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: o.Project,
		Labels: warden.TenantLabels(o.Project), Annotations: map[string]string{fabric.PruneAnnotation: "disabled"}}}
	for _, obj := range []runtime.Object{site, owner, ns} {
		gvk := obj.GetObjectKind().GroupVersionKind()
		m, _ := obj.(interface{ GetName() string })
		p, _ := fabric.Path(gvk, "", m.GetName())
		b, err := fabric.YAML(obj, gvk)
		if err != nil {
			return nil, err
		}
		if err := write(filepath.Join(dir, p), b); err != nil {
			return nil, err
		}
	}
	people := o.Site
	certLife := "720h"
	if o.Dev {
		people, certLife = "", "1h"
	}
	settings := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: warden.SettingsName, Namespace: warden.SystemNS},
		Data: map[string]string{"zone": o.Zone, "email": o.Email, "network": o.Network.String(), "writer": o.Site, "epoch": "1", "people": people,
			"certLife": certLife, "dev": fmt.Sprint(o.Dev), "blocklist": ""}}
	b, _ := yaml.Marshal(settings)
	if err := write(filepath.Join(dir, settingsFile), append([]byte("# The fabric's settings (docs/architecture.md). Changing the writer raises the epoch.\n"), b...)); err != nil {
		return nil, err
	}
	sops := fmt.Sprintf("# Who each file is encrypted to; the writer keeps this in step (docs/secrets.md).\ncreation_rules:\n  - path_regex: ^secrets/\n    encrypted_regex: ^(data|stringData)$\n    age: %s\n", strings.Join(recipients, ","))
	if err := write(filepath.Join(dir, ".sops.yaml"), []byte(sops)); err != nil {
		return nil, err
	}
	return res, nil
}

// settingsFile is the fabric's settings: written by Create, changed by the Console, never by Upgrade.
const settingsFile = "system/base/fabric.yaml"

// imageTag is what a version may be: it goes into every site's manifests as an image tag, so it can carry
// no YAML (the grammar of an OCI tag).
var imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// Platform is what this version of WeCoLab puts in system/ and crds/, by path in the Fabric: the
// template, filled in with what is fixed for the whole fabric (its version), and the upstream
// manifests it pins. Create writes it into a new fabric; Upgrade commits it to one that exists.
func Platform(o Options) (map[string][]byte, error) {
	if !imageTag.MatchString(o.Version) {
		return nil, fmt.Errorf("%q is not a version WeCoLab's images can have", o.Version)
	}
	if o.Fetch == nil {
		o.Fetch = httpGet
	}
	files := map[string][]byte{}
	err := fs.WalkDir(template_, "template", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := template_.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".yaml") && strings.Contains(string(b), "{{") {
			t, err := template.New(p).Parse(string(b))
			if err != nil {
				return err
			}
			var sb strings.Builder
			if err := t.Execute(&sb, o); err != nil {
				return err
			}
			b = []byte(sb.String())
		}
		files[strings.TrimPrefix(p, "template/")] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	if o.SchemaOnly {
		for name := range files {
			if !strings.HasPrefix(name, "crds/") {
				delete(files, name)
			}
		}
		return files, nil
	}
	for name, url := range Vendored {
		b, err := o.Fetch(url)
		if err != nil {
			return nil, fmt.Errorf("vendor %s: %w", name, err)
		}
		if !o.Dev {
			if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != VendorSHA256[name] {
				return nil, fmt.Errorf("vendor %s: SHA-256 mismatch", name)
			}
		}
		if name == "flux" {
			if b, err = Lockdown(OnlyControllers(b, "source-controller", "kustomize-controller")); err != nil {
				return nil, fmt.Errorf("vendor flux: %w", err)
			}
		}
		files["system/vendor/"+name+"/manifest.yaml"] = b
		files["system/vendor/"+name+"/kustomization.yaml"] = []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [manifest.yaml]\n# " + url + "\n")
	}
	return files, nil
}

// Upgrade is "an upgrade is a commit": this version's Platform committed to an existing fabric, with
// the files the template no longer has removed from its folders. The fabric's settings stay as they are.
// keptFlux is whether the fabric's Flux stayed as it was (keepFlux).
func Upgrade(ctx context.Context, g *fabric.Git, o Options, who fabric.Author) (sha string, keptFlux bool, err error) {
	b, ok, err := g.Read(ctx, settingsFile)
	if err != nil || !ok {
		return "", false, fmt.Errorf("%s is not in this Fabric (%v): is this the writer's copy?", settingsFile, err)
	}
	cm := &corev1.ConfigMap{}
	if err := yaml.Unmarshal(b, cm); err != nil {
		return "", false, err
	}
	want, err := Platform(o)
	if err != nil {
		return "", false, err
	}
	keptFlux = false
	if !o.SchemaOnly {
		flux, _, err := g.Read(ctx, fluxFile)
		if err != nil {
			return "", false, err
		}
		keptFlux = keepFlux(want, flux, o.LockFlux)
	}
	have := []string{}
	for _, d := range folders(want) {
		l, err := g.List(ctx, d)
		if err != nil {
			return "", false, err
		}
		have = append(have, l...)
	}
	paths := slices.Sorted(maps.Keys(want))
	if !o.SchemaOnly {
		paths = append(paths, stale(want, have)...)
	}
	action := "system/ and crds/"
	if o.SchemaOnly {
		action = "additive crds/"
	}
	sha, err = g.Edit(ctx, who, fmt.Sprintf("upgrade %s to WeCoLab %s: %s", cm.Data["zone"], o.Version, action), paths,
		func(*fabric.Snapshot) ([]fabric.FileChange, error) {
			out := make([]fabric.FileChange, len(paths))
			for i, p := range paths {
				out[i] = fabric.FileChange{Path: p, Content: want[p]} // nil: gone from the template; Edit skips what is equal
			}
			return out, nil
		})
	return sha, keptFlux, err
}

// fluxFile is the vendored Flux in the Fabric.
const fluxFile = "system/vendor/flux/manifest.yaml"

// keepFlux takes Flux out of an upgrade when the Fabric's Flux is not locked down yet and lock is not
// asked for. A fabric made before the lockdown has install.sh's own Kustomizations without a service
// account; one commit with both, and a site's Flux could come up locked before its new Warden named one
// on them (warden.Roots), and then apply nothing again, that Warden included. So the lockdown comes in
// an upgrade of its own, once every site runs a Warden that names it.
func keepFlux(want map[string][]byte, have []byte, lock bool) bool {
	if lock || bytes.Contains(have, []byte("--default-service-account=")) {
		return false
	}
	delete(want, fluxFile)
	delete(want, path.Dir(fluxFile)+"/kustomization.yaml")
	return true
}

// folders are the folders the platform's files are in.
func folders(files map[string][]byte) []string {
	out := []string{}
	for p := range files {
		if d := path.Dir(p); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return out
}

// stale is what an upgrade removes: files in the platform's folders it no longer has, but never the
// fabric's settings.
func stale(want map[string][]byte, have []string) []string {
	out := []string{}
	for _, p := range have {
		if _, ok := want[p]; !ok && p != settingsFile {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// netbirdConfig is the people mesh's server configuration: listening on the box's loopback behind the
// Door, the Console's sign-in allowed back, its own secrets generated here.
func netbirdConfig(o Options) string {
	return fmt.Sprintf(`server:
  listenAddress: "127.0.0.1:8081"
  exposedAddress: "https://mesh.%[1]s:443"
  stunPorts: [3478]
  metricsPort: 9091  # a port, not an address: install.sh's WECOLAB-HOST chain keeps it from the outside
  healthcheckAddress: "127.0.0.1:9001"
  logLevel: "info"
  logFile: "console"
  authSecret: %[2]q
  dataDir: "/var/lib/netbird/"
  disableAnonymousMetrics: true
  auth:
    issuer: "https://mesh.%[1]s/oauth2"
    localAuthDisabled: false
    sessionCookieEncryptionKey: %[3]q
    dashboardRedirectURIs: ["https://console.%[1]s/oauth/callback"]
    dashboardPostLogoutRedirectURIs: ["https://console.%[1]s/"]
    cliRedirectURIs: ["http://localhost:53000/"]
  store:
    engine: "sqlite"
    encryptionKey: %[4]q
  reverseProxy:
    trustedHTTPProxies: ["127.0.0.1/32"]
`, o.Zone, random(32), base64.StdEncoding.EncodeToString(randomBytes(32)), base64.StdEncoding.EncodeToString(randomBytes(32)))
}

func kustomization(resources []string) []byte {
	b, _ := yaml.Marshal(map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources})
	return b
}

func write(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func random(n int) string { return hex.EncodeToString(randomBytes(n))[:n] }

func httpGet(url string) ([]byte, error) {
	c := &http.Client{Timeout: 2 * time.Minute}
	r, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, r.Status)
	}
	return io.ReadAll(r.Body)
}

// eachDoc rewrites a multi-document manifest one object at a time: fn returns the document to keep, ""
// to drop it.
func eachDoc(manifest []byte, fn func(kind, name, doc string) (string, error)) ([]byte, error) {
	out := []string{}
	for _, d := range strings.Split(string(manifest), "\n---") {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		_ = yaml.Unmarshal([]byte(d), &meta)
		d, err := fn(meta.Kind, meta.Metadata.Name, d)
		if err != nil {
			return nil, err
		}
		if d != "" {
			out = append(out, d)
		}
	}
	return []byte(strings.Join(out, "\n---")), nil
}

// OnlyControllers keeps, of a multi-document manifest, every object except the Deployments not named:
// Flux's install manifest carries five controllers WeCoLab does not use.
func OnlyControllers(manifest []byte, keep ...string) []byte {
	b, _ := eachDoc(manifest, func(kind, name, d string) (string, error) {
		if kind == "Deployment" && !slices.Contains(keep, name) {
			return "", nil
		}
		return d, nil
	})
	return b
}

// fluxLockdown are Flux's multi-tenancy flags (fluxcd.io, "Flux multi-tenancy lockdown"): no reference
// to a source in another namespace, no remote bases, and a Kustomization that names no service account
// applies as flux-system's default one, which may do nothing.
var fluxLockdown = []string{"--no-cross-namespace-refs=true", "--no-remote-bases=true", "--default-service-account=default"}

// Lockdown adds fluxLockdown to the kustomize-controller of Flux's install manifest.
func Lockdown(manifest []byte) ([]byte, error) {
	found := false
	b, err := eachDoc(manifest, func(kind, name, d string) (string, error) {
		if kind != "Deployment" || name != "kustomize-controller" {
			return d, nil
		}
		dep := map[string]any{} // untyped: whatever upstream puts in the manifest stays in it
		if err := yaml.Unmarshal([]byte(d), &dep); err != nil {
			return "", err
		}
		cs, _, _ := unstructured.NestedSlice(dep, "spec", "template", "spec", "containers")
		for _, c := range cs {
			c, _ := c.(map[string]any)
			if c == nil || c["name"] != "manager" {
				continue
			}
			args, _, _ := unstructured.NestedStringSlice(c, "args")
			for _, f := range fluxLockdown {
				flag, _, _ := strings.Cut(f, "=")
				args = append(slices.DeleteFunc(args, func(a string) bool { return a == flag || strings.HasPrefix(a, flag+"=") }), f)
			}
			c["args"], found = toAny(args), true
		}
		if err := unstructured.SetNestedSlice(dep, cs, "spec", "template", "spec", "containers"); err != nil {
			return "", err
		}
		out, err := yaml.Marshal(dep)
		return "\n" + string(out), err
	})
	if err == nil && !found {
		err = fmt.Errorf("no kustomize-controller manager container in Flux's manifest")
	}
	return b, err
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
