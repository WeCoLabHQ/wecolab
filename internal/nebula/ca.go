// Package nebula is the fabric's Nebula certificate authority: making it, signing boxes' keys, and
// each box's share of the Nebula configuration (docs/architecture.md, "Networking").
package nebula

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/slackhq/nebula/cert"
	"golang.org/x/crypto/curve25519"
)

// NewCA makes a certificate authority for the network: an ed25519 signing key and a v2 CA
// certificate valid for d. Host certificates it signs must fit inside the network.
func NewCA(name string, network netip.Prefix, d time.Duration, now time.Time) (crtPEM, keyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	t := &cert.TBSCertificate{Version: cert.Version2, Name: name, Networks: []netip.Prefix{network},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(d), PublicKey: pub, IsCA: true, Curve: cert.Curve_CURVE25519}
	c, err := t.Sign(nil, cert.Curve_CURVE25519, priv)
	if err != nil {
		return nil, nil, err
	}
	crtPEM, err = c.MarshalPEM()
	return crtPEM, cert.MarshalSigningPrivateKeyToPEM(cert.Curve_CURVE25519, priv), err
}

// Keypair makes a box's X25519 key pair, PEM-encoded as nebula-cert keygen writes it. Used by the
// install step for the first box and by tests; every other box makes its own with nebula-cert.
func Keypair() (pubPEM, keyPEM []byte, err error) {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return nil, nil, err
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, nil, err
	}
	return cert.MarshalPublicKeyToPEM(cert.Curve_CURVE25519, pub), cert.MarshalPrivateKeyToPEM(cert.Curve_CURVE25519, priv), nil
}

// Request is what a box gets a certificate for.
type Request struct {
	Name   string
	Addr   netip.Prefix // the box's address with the network's prefix length: the routes it installs
	Groups []string
	PubPEM []byte
}

// Sign signs a box's public key with the newest CA in the bundle. The certificate lasts d, but never
// past its CA. An expired CA signs nothing (its certificates would be dead and due forever), and neither
// does a key that is not that CA's.
func Sign(caBundlePEM, caKeyPEM []byte, r Request, d time.Duration, now time.Time) (crtPEM []byte, fingerprint string, err error) {
	ca, _, err := cert.UnmarshalCertificateFromPEM(caBundlePEM)
	if err != nil {
		return nil, "", fmt.Errorf("ca certificate: %w", err)
	}
	if ca.Expired(now) {
		return nil, "", fmt.Errorf("the CA %q is not valid now (until %s)", ca.Name(), ca.NotAfter().UTC().Format(time.RFC3339))
	}
	key, _, curve, err := cert.UnmarshalSigningPrivateKeyFromPEM(caKeyPEM)
	if err != nil {
		return nil, "", fmt.Errorf("ca key: %w", err)
	}
	if err := ca.VerifyPrivateKey(curve, key); err != nil {
		return nil, "", fmt.Errorf("the CA key is not the newest CA's: %w", err)
	}
	pub, _, pubCurve, err := cert.UnmarshalPublicKeyFromPEM(r.PubPEM)
	if err != nil {
		return nil, "", fmt.Errorf("box key: %w", err)
	}
	if pubCurve != curve {
		return nil, "", fmt.Errorf("box key is %s, the CA is %s", pubCurve, curve)
	}
	notAfter := now.Add(d)
	if last := ca.NotAfter().Add(-time.Second); notAfter.After(last) {
		notAfter = last
	}
	t := &cert.TBSCertificate{Version: cert.Version2, Name: r.Name, Networks: []netip.Prefix{r.Addr}, Groups: r.Groups,
		NotBefore: now.Add(-time.Minute), NotAfter: notAfter, PublicKey: pub, Curve: curve}
	c, err := t.Sign(ca, curve, key)
	if err != nil {
		return nil, "", err
	}
	if crtPEM, err = c.MarshalPEM(); err != nil {
		return nil, "", err
	}
	fingerprint, err = c.Fingerprint()
	return crtPEM, fingerprint, err
}

// Due says whether a box holding current (PEM, possibly empty) should get a new certificate: none
// yet, a third of its life left, not issued by the newest CA, or not what the Fabric says it is now
// (another name, address or groups, or another key).
func Due(current, caBundlePEM []byte, r Request, now time.Time) bool {
	c, _, err := cert.UnmarshalCertificateFromPEM(current)
	if err != nil {
		return true
	}
	ca, _, err := cert.UnmarshalCertificateFromPEM(caBundlePEM)
	if err != nil {
		return true
	}
	caFP, _ := ca.Fingerprint()
	pub, _, _, err := cert.UnmarshalPublicKeyFromPEM(r.PubPEM)
	if err != nil {
		return true
	}
	life := c.NotAfter().Sub(c.NotBefore())
	groups := slices.Clone(c.Groups())
	want := slices.Clone(r.Groups)
	slices.Sort(groups)
	slices.Sort(want)
	return c.NotAfter().Sub(now) < life/3 || c.Issuer() != caFP || c.Name() != r.Name ||
		!slices.Equal(c.Networks(), []netip.Prefix{r.Addr}) || !slices.Equal(groups, want) || string(c.PublicKey()) != string(pub)
}

// Newest is, of crts, the newest the CA bundle verifies at now for the box called name and that is not
// blocklisted: the certificate that box should hold. Nil when none is.
func Newest(caBundlePEM []byte, blocklist []string, name string, now time.Time, crts ...[]byte) []byte {
	pool, err := cert.NewCAPoolFromPEM(caBundlePEM)
	if err != nil && !errors.Is(err, cert.ErrExpired) { // an expired old CA still leaves the others
		return nil
	}
	for _, fp := range blocklist {
		pool.BlocklistFingerprint(fp)
	}
	var best []byte
	var at time.Time
	for _, b := range crts {
		c, _, err := cert.UnmarshalCertificateFromPEM(b)
		if err != nil || c.Name() != name {
			continue
		}
		if _, err := pool.VerifyCertificate(now, c); err == nil && (best == nil || c.NotBefore().After(at)) {
			best, at = b, c.NotBefore()
		}
	}
	return best
}

// Fingerprint is a certificate's fingerprint, as Nebula's blocklist takes it.
func Fingerprint(crtPEM []byte) (string, time.Time, error) {
	c, _, err := cert.UnmarshalCertificateFromPEM(crtPEM)
	if err != nil {
		return "", time.Time{}, err
	}
	fp, err := c.Fingerprint()
	return fp, c.NotAfter(), err
}
