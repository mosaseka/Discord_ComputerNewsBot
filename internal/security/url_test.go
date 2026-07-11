package security

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
)

type resolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

type dialerFunc func(context.Context, string, string) (net.Conn, error)

func (f dialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestValidateURLRejectsUnsafeTargets(t *testing.T) {
	t.Parallel()
	publicResolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	})
	policy := NewPolicy(publicResolver, nil)
	tests := []string{
		"",
		"ftp://example.test/file",
		"https://user:secret@example.test/",
		"https://localhost/",
		"https://api.localhost/",
		"http://127.0.0.1/",
		"http://10.1.2.3/",
		"http://172.20.1.1/",
		"http://192.168.1.1/",
		"http://169.254.1.1/",
		"http://0.0.0.0/",
		"http://224.0.0.1/",
		"http://[::1]/",
		"http://[fe80::1]/",
		"http://[fc00::1]/",
	}
	for _, raw := range tests {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if _, err := policy.ValidateURL(context.Background(), raw); !errors.Is(err, ErrUnsafeURL) {
				t.Fatalf("ValidateURL(%q) error = %v, want ErrUnsafeURL", raw, err)
			}
		})
	}
}

func TestValidateURLRejectsMixedDNSAnswer(t *testing.T) {
	t.Parallel()
	policy := NewPolicy(resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("8.8.8.8")},
			{IP: net.ParseIP("127.0.0.1")},
		}, nil
	}), nil)
	_, err := policy.ValidateURL(context.Background(), "https://mixed.example/article")
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("error = %v, want ErrUnsafeURL", err)
	}
}

func TestDialContextResolvesAgainAndRejectsRebinding(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	lookups := 0
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		mu.Lock()
		defer mu.Unlock()
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	})
	dialCalls := 0
	policy := NewPolicy(resolver, dialerFunc(func(context.Context, string, string) (net.Conn, error) {
		dialCalls++
		return nil, errors.New("must not dial")
	}))
	if _, err := policy.ValidateURL(context.Background(), "https://rebind.example/article"); err != nil {
		t.Fatalf("initial validation failed: %v", err)
	}
	if _, err := policy.DialContext(context.Background(), "tcp", "rebind.example:443"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("dial error = %v, want ErrUnsafeURL", err)
	}
	if dialCalls != 0 {
		t.Fatalf("dialer called %d times, want 0", dialCalls)
	}
}

func TestDialContextUsesValidatedIPLiteral(t *testing.T) {
	t.Parallel()
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.4.4")}}, nil
	})
	var gotAddress string
	policy := NewPolicy(resolver, dialerFunc(func(_ context.Context, _, address string) (net.Conn, error) {
		gotAddress = address
		client, server := net.Pipe()
		go server.Close()
		return client, nil
	}))
	conn, err := policy.DialContext(context.Background(), "tcp", "public.example:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	conn.Close()
	if gotAddress != "8.8.4.4:443" {
		t.Fatalf("dial address = %q, want 8.8.4.4:443", gotAddress)
	}
}

func TestIsPublicIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"100.64.0.1", false},
		{"192.0.2.1", false},
		{"198.18.0.1", false},
		{"203.0.113.1", false},
		{"2001:db8::1", false},
		{"::ffff:127.0.0.1", false},
		{"255.255.255.255", false},
	}
	for _, test := range tests {
		if got := IsPublicIP(net.ParseIP(test.ip)); got != test.want {
			t.Errorf("IsPublicIP(%s) = %v, want %v", test.ip, got, test.want)
		}
	}
}
