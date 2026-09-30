package warden

import (
	"bufio"
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SettingsName is the ConfigMap in SystemNS holding the fabric's settings, from system/base.
const SettingsName = "fabric"

// Settings are the fabric's settings: one ConfigMap, the same at every site.
type Settings struct {
	Zone        string        // the fabric's DNS zone, e.g. fab.example.org
	Network     netip.Prefix  // the Nebula network
	Writer      string        // the site whose copy of the Fabric takes commits
	Epoch       int           // raised every time the writer changes
	People      string        // the site running NetBird; empty for none
	CertLife    time.Duration // how long a box's Nebula certificate lasts
	Blocklisted []Revoked     // Nebula certificates no box may use
	Dev         bool          // the development fabric (docs/development.md)
}

// Revoked is a certificate fingerprint and when that certificate would have expired anyway.
type Revoked struct {
	Fingerprint string
	Until       time.Time
}

// Blocklist is what every box's Nebula refuses: the revoked certificates still valid at now.
func (s *Settings) Blocklist(now time.Time) []string {
	out := []string{}
	for _, r := range s.Blocklisted {
		if r.Until.After(now) {
			out = append(out, r.Fingerprint)
		}
	}
	return out
}

// ParseSettings reads the ConfigMap's data.
func ParseSettings(d map[string]string) (*Settings, error) {
	s := &Settings{Zone: d["zone"], Writer: d["writer"], People: d["people"], Dev: d["dev"] == "true", CertLife: 720 * time.Hour}
	var err error
	if s.Network, err = netip.ParsePrefix(d["network"]); err != nil {
		return nil, fmt.Errorf("fabric settings: network: %w", err)
	}
	if v := d["epoch"]; v != "" {
		if s.Epoch, err = strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("fabric settings: epoch: %w", err)
		}
	}
	if v := d["certLife"]; v != "" {
		if s.CertLife, err = time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("fabric settings: certLife: %w", err)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(d["blocklist"]))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		t, err := time.Parse(time.RFC3339, f[1])
		if err != nil {
			return nil, fmt.Errorf("fabric settings: blocklist %q: %w", sc.Text(), err)
		}
		s.Blocklisted = append(s.Blocklisted, Revoked{Fingerprint: f[0], Until: t})
	}
	if s.Zone == "" || s.Writer == "" {
		return nil, fmt.Errorf("fabric settings: zone and writer are required")
	}
	return s, nil
}

// ReadSettings reads the fabric's settings at this site.
func ReadSettings(ctx context.Context, c client.Reader) (*Settings, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: SettingsName}, cm); err != nil {
		return nil, fmt.Errorf("fabric settings: %w", err)
	}
	return ParseSettings(cm.Data)
}
