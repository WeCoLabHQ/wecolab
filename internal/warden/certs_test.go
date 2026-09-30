package warden

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/nebula"
)

const (
	keyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIX9lVzki3cdi5xgTaj1ANUE6umeaZHi/OpV7ejZMNSD"
	keyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIkgMwGJO5QC7wzAvZLolmEkVVV8YPQIzQu3te3oxmK3"
)

func member(name, role string, blocked bool, projects []string, keys ...string) v1alpha1.Member {
	m := v1alpha1.Member{Spec: v1alpha1.MemberSpec{Email: name + "@example.org", Role: role, Projects: projects, SSHKeys: keys, Blocked: blocked}}
	m.Name = name
	return m
}

func TestSSHKeys(t *testing.T) {
	ms := []v1alpha1.Member{
		member("ana", "admin", false, nil, keyA+" ana@laptop"),
		member("bo", "member", false, []string{"home"}, keyB, keyA, `command="sh" `+keyB, keyB+"\n"+keyA, "not a key"),
		member("cy", "member", false, []string{"other"}, keyB+" cy"),
		member("di", "admin", true, nil, keyB+" di"),
	}
	got := SSHKeys(ms, "home")
	want := []string{keyA + " wecolab:ana", keyB + " wecolab:bo", keyA + " wecolab:bo"}
	if !slices.Equal(got, want) {
		t.Fatalf("admins' and the owner project's plain keys, one line each:\n%q", got)
	}
}

type certFixture struct {
	c     client.Client
	svc   *CertService
	boxIP netip.Addr
}

func newCertFixture(t *testing.T) *certFixture {
	now := time.Now()
	network := netip.MustParsePrefix("10.77.0.0/16")
	caCrt, caKey, err := nebula.NewCA("test", network, 365*24*time.Hour, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := nebula.Keypair()
	site := &v1alpha1.Site{Spec: v1alpha1.SiteSpec{Owner: "home", Index: 1, Boxes: []v1alpha1.Box{
		{Name: "home-a", IP: "10.77.1.1", Role: "manager", Key: string(pub)},
		{Name: "home-b", IP: "10.77.1.2", Role: "node", Key: string(pub)},
	}}}
	site.Name = "home"
	settings := &corev1.ConfigMap{Data: map[string]string{"zone": "fab.example.org", "writer": "home", "network": network.String(), "certLife": "720h"}}
	settings.Name, settings.Namespace = SettingsName, SystemNS
	ca := &corev1.Secret{Data: map[string][]byte{"ca.crt": caCrt, "ca.key": caKey}}
	ca.Name, ca.Namespace = CASecret, SystemNS
	ana := member("ana", "admin", false, nil, keyA)
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(site, settings, ca, &ana).Build()
	return &certFixture{c: c, svc: &CertService{Client: c}, boxIP: netip.MustParseAddr("10.77.1.2")}
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

// fakeWithSecret is a cluster holding one site's Secret with its mirroring password.
func fakeWithSecret(t *testing.T, site, password string) client.Client {
	sec := &corev1.Secret{Data: map[string][]byte{KeyMirror: []byte(password)}}
	sec.Name, sec.Namespace = SiteSecret(site), SystemNS
	return fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(sec).Build()
}

func (f *certFixture) blocklist(t *testing.T, crt string) {
	fp, notAfter, err := nebula.Fingerprint([]byte(crt))
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	_ = f.c.Get(context.Background(), types.NamespacedName{Namespace: SystemNS, Name: SettingsName}, cm)
	cm.Data["blocklist"] += fp + " " + notAfter.UTC().Format(time.RFC3339) + "\n"
	if err := f.c.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
}

func TestCertServiceSignsOnlyWhenDue(t *testing.T) {
	ctx := context.Background()
	f := newCertFixture(t)
	now := time.Now()
	if _, err := f.svc.Bundle(ctx, "home-b", netip.MustParseAddr("10.77.1.1"), nil, now); err != errNotFrom {
		t.Fatalf("another box's address asks for home-b: %v", err)
	}
	first, err := f.svc.Bundle(ctx, "home-b", f.boxIP, nil, now)
	if err != nil || first.Cert == "" {
		t.Fatalf("a box with no certificate of this steward's gets one: %v", err)
	}
	if !slices.Equal(first.SSHKeys, []string{keyA + " wecolab:ana"}) {
		t.Fatalf("the bundle carries the box's SSH keys: %q", first.SSHKeys)
	}
	again, _ := f.svc.Bundle(ctx, "home-b", f.boxIP, nil, now)
	if again.Cert != first.Cert {
		t.Fatal("asking again without a certificate gets the same one, not another")
	}
	if held, _ := f.svc.Bundle(ctx, "home-b", f.boxIP, []byte(first.Cert), now); held.Cert != "" {
		t.Fatal("a box holding its newest certificate gets none")
	}
	if late, _ := f.svc.Bundle(ctx, "home-b", f.boxIP, []byte(first.Cert), now.Add(21*24*time.Hour)); late.Cert == "" || late.Cert == first.Cert {
		t.Fatal("renewed with a third of its life left")
	}
	// A blocklisted certificate is due at once (a second later: signing is deterministic); the record
	// keeps the newest three.
	last := first.Cert
	for i := range 4 {
		f.blocklist(t, last)
		b, err := f.svc.Bundle(ctx, "home-b", f.boxIP, []byte(last), now.Add(time.Duration(i+1)*time.Second))
		if err != nil || b.Cert == "" || b.Cert == last {
			t.Fatalf("a blocklisted certificate is replaced: %v", err)
		}
		last = b.Cert
	}
	issued, _ := f.svc.issued(ctx, now)
	if n := len(issued["home-b"]); n != keepIssued {
		t.Fatalf("the record is bounded: %d", n)
	}
	if issued["home-b"][keepIssued-1].Fingerprint != fingerprint([]byte(last)) {
		t.Fatal("the newest is last")
	}
	// Expired records go whenever anything is recorded.
	if _, err := f.svc.Bundle(ctx, "home-a", netip.MustParseAddr("10.77.1.1"), nil, now.Add(40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if issued, _ := f.svc.issued(ctx, now.Add(40*24*time.Hour)); len(issued["home-b"]) != 0 || len(issued["home-a"]) != 1 {
		t.Fatalf("expired records are pruned: %v", issued)
	}
}

func TestCertServiceHTTP(t *testing.T) {
	f := newCertFixture(t)
	srv := httptest.NewServer(f.svc)
	defer srv.Close()
	// The test client's address is 127.0.0.1, which is no box's.
	res, err := http.Post(srv.URL+"/nebula/home-b", "application/x-pem-file", strings.NewReader(""))
	if err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("a request from elsewhere is refused: %v %v", err, res.Status)
	}
	if _, err := f.svc.Bundle(context.Background(), "home-b", f.boxIP, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(srv.URL + "/nebula/issued")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatal(err)
	}
	var all map[string][]Issued
	if err := json.NewDecoder(res.Body).Decode(&all); err != nil || len(all["home-b"]) != 1 {
		t.Fatalf("stewards publish what they issued per box: %v %v", all, err)
	}
}

// A join certificate recorded on a box stays published after the box has left the Fabric by any path,
// until it expires, so the writer can revoke it.
func TestCertServiceKeepsJoinCertificates(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	f := newCertFixture(t)
	site := &v1alpha1.Site{}
	if err := f.c.Get(ctx, types.NamespacedName{Name: "home"}, site); err != nil {
		t.Fatal(err)
	}
	site.Spec.Boxes[1].Certs = []v1alpha1.IssuedCert{{Fingerprint: fp("d"), NotAfter: metav1.NewTime(now.Add(time.Hour))}}
	if err := f.c.Update(ctx, site); err != nil {
		t.Fatal(err)
	}
	if got, err := f.svc.issued(ctx, now); err != nil || len(got["home-b"]) != 1 {
		t.Fatalf("join certificates are published: %v %v", got, err)
	}
	site.Spec.Boxes = site.Spec.Boxes[:1]
	if err := f.c.Update(ctx, site); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.issued(ctx, now); len(got["home-b"]) != 1 || got["home-b"][0].Fingerprint != fp("d") {
		t.Fatalf("still published once the box has left: %v", got)
	}
	if got, _ := f.svc.issued(ctx, now.Add(2*time.Hour)); len(got) != 0 {
		t.Fatalf("until it expires: %v", got)
	}
}
