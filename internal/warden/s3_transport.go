package warden

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// PublicS3Address excludes special-use networks, including transition mechanisms
// that could translate a seemingly public IPv6 address to a private IPv4 target.
func PublicS3Address(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.Zone() != "" {
		return false
	}
	if a.Is6() && !publicIPv6.Contains(a) {
		return false
	}
	for _, p := range s3SpecialNetworks {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var publicIPv6 = netip.MustParsePrefix("2000::/3")
var s3SpecialNetworks = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

type lookupS3 func(context.Context, string, string) ([]netip.Addr, error)
type dialS3 func(context.Context, string, string) (net.Conn, error)

func guardedS3Dial(dev bool, lookup lookupS3, dial dialS3) dialS3 {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("unsupported S3 network %q", network)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := lookup(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve S3 destination: %w", err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("S3 destination has no addresses")
		}
		for _, ip := range ips {
			if !ip.IsValid() || ip.Zone() != "" || (!dev && !PublicS3Address(ip)) {
				return nil, fmt.Errorf("S3 destination is not public: %s", ip)
			}
		}
		for _, ip := range ips {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ip = ip.Unmap()
			if network == "tcp4" && !ip.Is4() || network == "tcp6" && !ip.Is6() {
				continue
			}
			var conn net.Conn
			conn, err = dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		if err == nil {
			err = fmt.Errorf("S3 destination has no address in requested family")
		}
		return nil, err
	}
}

func newS3Client(dev bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           guardedS3Dial(dev, net.DefaultResolver.LookupNetIP, dialer.DialContext),
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          32,
		},
	}
}

var productionS3Client = newS3Client(false)
var developmentS3Client = newS3Client(true)
var b2HTTPClient = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return b, nil
}
