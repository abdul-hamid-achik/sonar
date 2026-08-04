package netpolicy

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestIsLocalHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{host: "localhost", want: true},
		{host: "LOCALHOST", want: true},
		{host: "127.0.0.1", want: true},
		{host: "::1", want: true},
		{host: "0.0.0.0", want: true},
		{host: "::", want: true},
		{host: "192.168.1.10", want: false},
		{host: "example.com", want: false},
		{host: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.host, func(t *testing.T) {
			if got := IsLocalHost(test.host); got != test.want {
				t.Fatalf("IsLocalHost(%q) = %v, want %v", test.host, got, test.want)
			}
		})
	}
}

func TestLocalOnlyDialContextRejectsPoisonedLocalhostBeforeDial(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("127.0.0.1")},
			{IP: net.ParseIP("203.0.113.10")},
		}, nil
	})
	dialed := false
	dial := LocalOnlyDialContext(resolver, func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}, "test")

	_, err := dial(context.Background(), "tcp", "localhost:8080")
	if err == nil || !strings.Contains(err.Error(), "non-loopback address") {
		t.Fatalf("poisoned localhost error = %v", err)
	}
	if dialed {
		t.Fatal("poisoned localhost resolution reached the network dialer")
	}
}

func TestLocalOnlyDialContextPinsVerifiedIPv4AndIPv6(t *testing.T) {
	tests := []struct {
		name string
		ip   net.IP
		want string
	}{
		{name: "IPv4", ip: net.ParseIP("127.0.0.1"), want: "127.0.0.1:8080"},
		{name: "IPv6", ip: net.IPv6loopback, want: "[::1]:8080"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: test.ip}}, nil
			})
			var target string
			stop := errors.New("dial seam reached")
			dial := LocalOnlyDialContext(resolver, func(_ context.Context, _ string, address string) (net.Conn, error) {
				target = address
				return nil, stop
			}, "test")

			_, err := dial(context.Background(), "tcp", "localhost:8080")
			if !errors.Is(err, stop) {
				t.Fatalf("dial error = %v, want seam error", err)
			}
			if target != test.want {
				t.Fatalf("verified dial target = %q, want %q", target, test.want)
			}
		})
	}
}

func TestLocalOnlyDialContextMapsUnspecifiedAliasesToLoopback(t *testing.T) {
	tests := []struct {
		address string
		want    string
	}{
		{address: "0.0.0.0:11434", want: "127.0.0.1:11434"},
		{address: "[::]:11434", want: "[::1]:11434"},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			var target string
			stop := errors.New("dial seam reached")
			dial := LocalOnlyDialContext(nil, func(_ context.Context, _ string, address string) (net.Conn, error) {
				target = address
				return nil, stop
			}, "test")

			_, err := dial(context.Background(), "tcp", test.address)
			if !errors.Is(err, stop) {
				t.Fatalf("dial error = %v, want seam error", err)
			}
			if target != test.want {
				t.Fatalf("mapped dial target = %q, want %q", target, test.want)
			}
		})
	}
}

type resolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}
