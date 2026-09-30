package warden

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/nebula"
)

// CASecret holds the fabric's Nebula CA at stewards (from secrets/ in the Fabric): ca.crt, the bundle
// with the newest CA first, and ca.key, that CA's key.
const CASecret = "nebula-ca"

// issuedMap keeps, per box, the certificates this steward signed that have not expired (PEM, newest
// last, at most keepIssued): what the writer blocklists once the box has left the Fabric, and what a
// box that asks again is given instead of yet another certificate. joinedMap keeps the join
// certificates the Fabric recorded on its boxes (Box.Certs) until they expire, so they are still known
// once a box has left by any path, not only the Console's removal.
//
// ponytail: keepIssued caps the record's size; a box signed more than keepIssued times within a
// certificate's life (key or group changes, blocklist replacements) has its oldest still-valid ones
// dropped unpublished. Keep fingerprints apart from the PEM if that ever happens in practice.
const (
	issuedMap  = "nebula-issued"
	joinedMap  = "nebula-joined"
	keepIssued = 3
)

// Bundle is what a box gets from the certificate service.
type Bundle struct {
	Name     string   `json:"name"`
	IP       string   `json:"ip"`
	CA       string   `json:"ca"`
	Cert     string   `json:"cert,omitempty"` // only when the box should hold another one
	Config   string   `json:"config"`         // /etc/nebula/config.d/20-fabric.yml
	Stewards []string `json:"stewards"`       // where to ask next time
	Life     int      `json:"life"`           // a certificate's lifetime, seconds
	SSHKeys  []string `json:"sshKeys"`        // root's authorized keys from the Fabric, all of them
}

// Issued is one certificate a steward signed.
type Issued struct {
	Fingerprint string    `json:"fingerprint"`
	NotAfter    time.Time `json:"notAfter"`
}

// CertService is a steward's certificate service, on its own port over Nebula (nebula.PortCerts). A box
// posts the certificate it holds and gets its CA bundle, its share of the Nebula configuration, its SSH
// keys and, when one is due, a new certificate. Nebula authenticates source addresses (a packet from
// outside its sender's certificate is dropped), so a request for a box counts only from that box's
// address; and it only ever signs the key the Fabric registered for that box.
type CertService struct {
	Client client.Client
	mu     sync.Mutex
}

var (
	errNoBox   = errors.New("no such box in the Fabric")
	errNotFrom = errors.New("a box asks for itself, from its own Nebula address")
)

func (c *CertService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/nebula/")
	switch {
	case ok && (name == "issued" || strings.HasPrefix(name, "issued/")) && r.Method == http.MethodGet:
		out, err := c.issued(r.Context(), time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if box, one := strings.CutPrefix(name, "issued/"); one {
			writeJSONResponse(w, append([]Issued{}, out[box]...))
			return
		}
		writeJSONResponse(w, out)
	case ok && name != "" && r.Method == http.MethodPost:
		from, _ := netip.ParseAddrPort(r.RemoteAddr)
		current, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		b, err := c.Bundle(r.Context(), name, from.Addr(), current, time.Now())
		switch {
		case errors.Is(err, errNoBox):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, errNotFrom):
			http.Error(w, err.Error(), http.StatusForbidden)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			writeJSONResponse(w, b)
		}
	default:
		http.NotFound(w, r)
	}
}

// Find is the site and box of a box name in the Fabric.
func Find(sites []v1alpha1.Site, name string) (*v1alpha1.Site, *v1alpha1.Box) {
	for i := range sites {
		for j := range sites[i].Spec.Boxes {
			if sites[i].Spec.Boxes[j].Name == name {
				return &sites[i], &sites[i].Spec.Boxes[j]
			}
		}
	}
	return nil, nil
}

// Stewards are the managers' addresses of every steward site.
func Stewards(sites []v1alpha1.Site) []string {
	out := []string{}
	for i := range sites {
		if m := sites[i].Manager(); m != nil && sites[i].Spec.Steward {
			out = append(out, m.IP)
		}
	}
	slices.Sort(out)
	return out
}

// SSHKeys are the keys that may log in as root on a box of a site owned by project: those of the
// fabric's admins and of the project's members, unless blocked. Only plain keys pass (one line, no
// options), marked with their member, since each box writes them into authorized_keys.
func SSHKeys(members []v1alpha1.Member, project string) []string {
	out := []string{}
	for _, m := range members {
		if m.Spec.Blocked || !m.Admin() && !slices.Contains(m.Spec.Projects, project) {
			continue
		}
		for _, k := range m.Spec.SSHKeys {
			if strings.ContainsAny(k, "\r\n") {
				continue
			}
			pk, _, opts, rest, err := ssh.ParseAuthorizedKey([]byte(k))
			if err != nil || len(opts) > 0 || strings.TrimSpace(string(rest)) != "" {
				continue
			}
			line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + " wecolab:" + m.Name
			if !slices.Contains(out, line) {
				out = append(out, line)
			}
		}
	}
	return out
}

// Bundle is a box's answer. It signs only when the newest valid certificate the box could hold (the one
// it sent, or the newest this steward signed for it) is due; when this steward's is newer than the one
// sent, the box gets that one again. So a box that asks often gets nothing more, and a blocklisted
// certificate is due at once.
func (c *CertService) Bundle(ctx context.Context, name string, from netip.Addr, current []byte, now time.Time) (*Bundle, error) {
	s, err := ReadSettings(ctx, c.Client)
	if err != nil {
		return nil, err
	}
	sites := &v1alpha1.SiteList{}
	if err := c.Client.List(ctx, sites); err != nil {
		return nil, err
	}
	site, box := Find(sites.Items, name)
	if box == nil {
		return nil, errNoBox
	}
	if ip, err := netip.ParseAddr(box.IP); err != nil || from.Unmap() != ip {
		return nil, errNotFrom
	}
	members := &v1alpha1.MemberList{}
	if err := c.Client.List(ctx, members); err != nil {
		return nil, err
	}
	ca := &corev1.Secret{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: CASecret}, ca); err != nil {
		return nil, fmt.Errorf("the Nebula CA is not here: %w", err)
	}
	addr, err := nebula.Addr(*box, s.Network)
	if err != nil {
		return nil, err
	}
	req := nebula.Request{Name: box.Name, Addr: addr, Groups: nebula.Groups(site, *box), PubPEM: []byte(box.Key)}
	blocklist := s.Blocklist(now)
	cfg, err := nebula.FabricConfig(site, *box, nebula.Lighthouses(sites.Items), blocklist)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Name: box.Name, IP: box.IP, CA: string(ca.Data["ca.crt"]), Config: string(cfg), Stewards: Stewards(sites.Items),
		Life: int(s.CertLife.Seconds()), SSHKeys: SSHKeys(members.Items, site.Spec.Owner)}
	mine, err := c.signed(ctx, box.Name)
	if err != nil {
		return nil, err
	}
	best := nebula.Newest(ca.Data["ca.crt"], blocklist, box.Name, now, append(mine, current)...)
	if best != nil && !nebula.Due(best, ca.Data["ca.crt"], req, now) {
		if fingerprint(best) != fingerprint(current) {
			b.Cert = string(best)
		}
		return b, nil
	}
	crt, _, err := nebula.Sign(ca.Data["ca.crt"], ca.Data["ca.key"], req, s.CertLife, now)
	if err != nil {
		return nil, err
	}
	if err := c.record(ctx, box.Name, crt, now); err != nil {
		return nil, err
	}
	b.Cert = string(crt)
	return b, nil
}

func fingerprint(crt []byte) string {
	fp, _, _ := nebula.Fingerprint(crt)
	return fp
}

// signed is what this steward signed for a box, newest last.
func (c *CertService) signed(ctx context.Context, box string) ([][]byte, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: issuedMap}, cm); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return splitPEM(cm.Data[box]), nil
}

// record keeps a certificate signed for a box, and drops every box's expired ones and any beyond the
// newest few, so the record stays small however often boxes ask.
func (c *CertService) record(ctx context.Context, box string, crt []byte, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cm := &corev1.ConfigMap{}
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: issuedMap}, cm)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return err
	}
	cm.Name, cm.Namespace = issuedMap, SystemNS
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[box] += string(crt)
	for k, v := range cm.Data {
		keep := []string{}
		for _, p := range splitPEM(v) {
			if _, notAfter, err := nebula.Fingerprint(p); err == nil && notAfter.After(now) {
				keep = append(keep, string(p))
			}
		}
		if keep = keep[max(0, len(keep)-keepIssued):]; len(keep) == 0 {
			delete(cm.Data, k)
		} else {
			cm.Data[k] = strings.Join(keep, "")
		}
	}
	if create {
		return c.Client.Create(ctx, cm)
	}
	return c.Client.Update(ctx, cm)
}

// issued is, per box, the certificates this steward signed and the join certificates it has seen in
// the Fabric, that are still valid.
func (c *CertService) issued(ctx context.Context, now time.Time) (map[string][]Issued, error) {
	out, err := c.joined(ctx, now)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: issuedMap}, cm); err != nil {
		return out, client.IgnoreNotFound(err)
	}
	boxes := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		boxes = append(boxes, k)
	}
	sort.Strings(boxes)
	for _, box := range boxes {
		for _, p := range splitPEM(cm.Data[box]) {
			if fp, notAfter, err := nebula.Fingerprint(p); err == nil && notAfter.After(now) {
				out[box] = append(out[box], Issued{Fingerprint: fp, NotAfter: notAfter})
			}
		}
	}
	return out, nil
}

// joined adds the join certificates on the boxes of this cluster's copy of the Fabric to joinedMap,
// drops expired ones, and is the record. The copy only adds certificates to look at; whether their box
// has left the Fabric is decided in Git by the writer.
func (c *CertService) joined(ctx context.Context, now time.Time) (map[string][]Issued, error) {
	sites := &v1alpha1.SiteList{}
	if err := c.Client.List(ctx, sites); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cm := &corev1.ConfigMap{}
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: joinedMap}, cm)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return nil, err
	}
	out := map[string][]Issued{}
	for box, v := range cm.Data {
		var l []Issued
		_ = json.Unmarshal([]byte(v), &l)
		out[box] = l
	}
	for _, s := range sites.Items {
		for _, b := range s.Spec.Boxes {
			for _, ct := range b.Certs {
				if !slices.ContainsFunc(out[b.Name], func(i Issued) bool { return i.Fingerprint == ct.Fingerprint }) {
					out[b.Name] = append(out[b.Name], Issued{Fingerprint: ct.Fingerprint, NotAfter: ct.NotAfter.Time})
				}
			}
		}
	}
	data := map[string]string{}
	for box, l := range out {
		if l = slices.DeleteFunc(l, func(i Issued) bool { return !i.NotAfter.After(now) }); len(l) == 0 {
			delete(out, box)
			continue
		}
		out[box] = l
		j, _ := json.Marshal(l)
		data[box] = string(j)
	}
	if maps.Equal(data, cm.Data) {
		return out, nil
	}
	cm.Name, cm.Namespace, cm.Data = joinedMap, SystemNS, data
	if create {
		return out, c.Client.Create(ctx, cm)
	}
	return out, c.Client.Update(ctx, cm)
}

// splitPEM is each PEM block of s on its own.
func splitPEM(s string) [][]byte {
	out := [][]byte{}
	for rest := []byte(s); ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			return out
		}
		out = append(out, pem.EncodeToMemory(b))
	}
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
