package warden

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPublicS3Address(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "192.168.1.1", "198.18.0.1", "192.0.2.1", "203.0.113.1", "0.0.0.0", "255.255.255.255", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "2001:db8::1", "2002:7f00:1::", "64:ff9b::7f00:1"} {
		if PublicS3Address(netip.MustParseAddr(raw)) {
			t.Errorf("accepted special-use destination %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !PublicS3Address(netip.MustParseAddr(raw)) {
			t.Errorf("refused public destination %s", raw)
		}
	}
}

func TestS3DialRejectsWholeMixedAnswer(t *testing.T) {
	calls := 0
	dial := guardedS3Dial(false, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}, func(context.Context, string, string) (net.Conn, error) { calls++; return nil, io.EOF })
	if _, err := dial(context.Background(), "tcp", "vault.example:443"); err == nil || calls != 0 {
		t.Fatalf("mixed answer dialed: calls=%d err=%v", calls, err)
	}
}

func TestS3DialPinsResolvedDestination(t *testing.T) {
	var destination string
	dial := guardedS3Dial(false, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(_ context.Context, network, address string) (net.Conn, error) {
		destination = address
		return nil, io.EOF
	})
	_, _ = dial(context.Background(), "tcp", "vault.example:443")
	if destination != "8.8.8.8:443" {
		t.Fatalf("unchecked DNS redial: %q", destination)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	destination = ""
	if _, err := dial(ctx, "tcp", "vault.example:443"); err == nil || destination != "" {
		t.Fatalf("canceled request dialed %q: %v", destination, err)
	}
}

func TestS3RejectsRedirectAndProductionLoopback(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "unexpected") }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := (S3{Endpoint: redirect.URL, Dev: true}).get(context.Background(), "vault", nil); err == nil {
		t.Fatal("redirect accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("redirect reached forbidden destination")
	}
	if _, err := (S3{Endpoint: target.URL}).get(context.Background(), "vault", nil); err == nil {
		t.Fatal("production HTTP accepted")
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer tls.Close()
	if _, err := (S3{Endpoint: tls.URL}).get(context.Background(), "vault", nil); err == nil {
		t.Fatal("production loopback accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("production loopback received a request")
	}
}

func TestS3DoesNotUseEnvironmentProxy(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied.Add(1); w.WriteHeader(502) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "direct") }))
	defer origin.Close()
	b, err := (S3{Endpoint: origin.URL, Dev: true}).get(context.Background(), "vault", url.Values{"list-type": {"2"}})
	if err != nil || string(b) != "direct" || proxied.Load() != 0 {
		t.Fatalf("not direct: %q %v proxy=%d", b, err, proxied.Load())
	}
}

func TestS3RejectsReadFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "truncated")
	}))
	defer srv.Close()
	if _, err := (S3{Endpoint: srv.URL, Dev: true}).get(context.Background(), "vault", nil); err == nil {
		t.Fatal("accepted truncated body")
	}
}

func FuzzPublicS3AddressUnmapping(f *testing.F) {
	f.Add(byte(127), byte(0), byte(0), byte(1))
	f.Add(byte(8), byte(8), byte(8), byte(8))
	f.Fuzz(func(t *testing.T, a, b, c, d byte) {
		v4 := netip.AddrFrom4([4]byte{a, b, c, d})
		v6 := netip.MustParseAddr("::ffff:" + v4.String())
		if PublicS3Address(v4) != PublicS3Address(v6) {
			t.Fatalf("mapped address bypass: %s", v6)
		}
		if strings.HasPrefix(v4.String(), "127.") && PublicS3Address(v4) {
			t.Fatal("loopback accepted")
		}
	})
}
