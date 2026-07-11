// Package security contains the network-boundary checks used before the bot
// connects to URLs discovered in feeds or HTML documents.
package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

var (
	// ErrUnsafeURL is returned when a URL or one of its resolved addresses is
	// not suitable for an outbound Internet request.
	ErrUnsafeURL = errors.New("unsafe URL")
	// ErrNoAddresses is returned when DNS succeeds without yielding an address.
	ErrNoAddresses = errors.New("host resolved to no addresses")
)

// Resolver is the subset of net.Resolver used by Policy. It is deliberately
// small so DNS changes and rebinding attempts can be exercised in tests.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Dialer is the subset of net.Dialer used by Policy.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Policy validates URLs both before a request and again while dialing. DNS is
// intentionally performed on both paths: the second lookup closes the usual
// validation-to-connect DNS rebinding gap.
type Policy struct {
	resolver Resolver
	dialer   Dialer
}

// NewPolicy constructs a policy. Nil arguments select the system resolver and
// a net.Dialer with the timeout supplied by the caller through its context.
func NewPolicy(resolver Resolver, dialer Dialer) *Policy {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return &Policy{resolver: resolver, dialer: dialer}
}

// DefaultPolicy returns a policy backed by the system resolver and dialer.
func DefaultPolicy() *Policy { return NewPolicy(nil, nil) }

// ValidateURL parses raw and verifies that it is an absolute public HTTP(S)
// URL. Hostnames are resolved and every returned address must be public.
func (p *Policy) ValidateURL(ctx context.Context, raw string) (*url.URL, error) {
	if p == nil {
		p = DefaultPolicy()
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, unsafe("URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, unsafe("parse URL: %v", err)
	}
	if err := p.CheckURL(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// CheckURL verifies an already parsed URL and resolves its hostname. It does
// not mutate the URL.
func (p *Policy) CheckURL(ctx context.Context, u *url.URL) error {
	if p == nil {
		p = DefaultPolicy()
	}
	host, err := checkURLSyntax(u)
	if err != nil {
		return err
	}
	return p.checkHost(ctx, host)
}

// DialContext is suitable for http.Transport.DialContext. It resolves the
// requested hostname again, rejects the entire answer if it contains a
// non-public address, and dials only the validated IP literals.
func (p *Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if p == nil {
		p = DefaultPolicy()
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("split dial address: %w", err)
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if err := checkHostname(host); err != nil {
		return nil, err
	}

	addrs, err := p.lookupPublic(ctx, host)
	if err != nil {
		return nil, err
	}
	var dialErrs []error
	for _, ip := range addrs {
		conn, dialErr := p.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		dialErrs = append(dialErrs, dialErr)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("dial validated addresses for %q: %w", host, errors.Join(dialErrs...))
}

func checkURLSyntax(u *url.URL) (string, error) {
	if u == nil {
		return "", unsafe("URL is nil")
	}
	if u.Opaque != "" {
		return "", unsafe("opaque URLs are not allowed")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", unsafe("scheme %q is not allowed", u.Scheme)
	}
	if u.User != nil {
		return "", unsafe("userinfo is not allowed")
	}
	if u.Host == "" {
		return "", unsafe("host is missing")
	}
	if u.Hostname() == "" {
		return "", unsafe("hostname is missing")
	}
	if u.Port() != "" {
		if _, err := net.LookupPort("tcp", u.Port()); err != nil {
			return "", unsafe("invalid port %q", u.Port())
		}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if err := checkHostname(host); err != nil {
		return "", err
	}
	return host, nil
}

func checkHostname(host string) error {
	if host == "" {
		return unsafe("hostname is empty")
	}
	if strings.Contains(host, "%") {
		return unsafe("IPv6 zones are not allowed")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return unsafe("localhost is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && !IsPublicIP(ip) {
		return unsafe("IP address %s is not public", ip)
	}
	return nil
}

func (p *Policy) checkHost(ctx context.Context, host string) error {
	_, err := p.lookupPublic(ctx, host)
	return err
}

func (p *Policy) lookupPublic(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !IsPublicIP(ip) {
			return nil, unsafe("IP address %s is not public", ip)
		}
		return []net.IP{ip}, nil
	}
	resolved, err := p.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("resolve %q: %w", host, ErrNoAddresses)
	}

	addrs := make([]net.IP, 0, len(resolved))
	seen := make(map[string]struct{}, len(resolved))
	for _, item := range resolved {
		if !IsPublicIP(item.IP) {
			return nil, unsafe("host %q resolved to non-public IP %s", host, item.IP)
		}
		key := item.IP.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		addrs = append(addrs, item.IP)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %q: %w", host, ErrNoAddresses)
	}
	return addrs, nil
}

// IsPublicIP reports whether ip is appropriate for arbitrary Internet access.
// In addition to Go's private/link-local checks, documentation, benchmarking,
// carrier-grade NAT, reserved, multicast, and unspecified ranges are denied.
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range deniedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

var deniedPrefixes = mustPrefixes(
	"0.0.0.0/8",
	"100.64.0.0/10",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
	"64:ff9b::/96",
	"100::/64",
	"2001:db8::/32",
)

func mustPrefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}

func unsafe(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsafeURL, fmt.Sprintf(format, args...))
}
