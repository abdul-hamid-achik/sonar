// Package netpolicy provides shared network authority primitives.
package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// IPResolver is the resolver surface required by LocalOnlyDialContext.
type IPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// DialContextFunc is the dial surface wrapped by LocalOnlyDialContext.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)

// IsLocalHost accepts only the localhost alias and literal loopback or
// unspecified addresses. Unspecified addresses are useful Ollama bind aliases;
// LocalOnlyDialContext maps them to the corresponding loopback address before
// dialing.
func IsLocalHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return IsLocalIP(net.ParseIP(host))
}

// IsLocalIP reports whether ip is confined to the local machine.
func IsLocalIP(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// LocalOnlyDialContext resolves the target itself, validates every answer, and
// dials a verified IP instead of resolving the hostname a second time. This
// closes DNS rebinding/check-use gaps for local-only HTTP clients.
//
// service is used only in diagnostics (for example, "MCP" or "Ollama").
func LocalOnlyDialContext(resolver IPResolver, dial DialContextFunc, service string) DialContextFunc {
	service = strings.TrimSpace(service)
	if service == "" {
		service = "service"
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("privacy.local_only cannot validate %s dial target %q: %w", service, address, err)
		}

		var addresses []net.IPAddr
		if literal := net.ParseIP(host); literal != nil {
			addresses = []net.IPAddr{{IP: literal}}
		} else {
			if !strings.EqualFold(host, "localhost") {
				return nil, fmt.Errorf("privacy.local_only refuses %s DNS target %q", service, host)
			}
			if resolver == nil {
				return nil, fmt.Errorf("privacy.local_only cannot resolve local %s host %q: resolver is unavailable", service, host)
			}
			addresses, err = resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve local %s host %q: %w", service, host, err)
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("local %s host %q resolved to no addresses", service, host)
		}
		for _, resolved := range addresses {
			if !IsLocalIP(resolved.IP) {
				return nil, fmt.Errorf("privacy.local_only refuses %s host %q resolving to non-loopback address %s", service, host, resolved.IP)
			}
		}
		if dial == nil {
			return nil, fmt.Errorf("privacy.local_only cannot dial local %s host %q: dialer is unavailable", service, host)
		}

		var dialErrors []error
		for _, selected := range addresses {
			ip := selected.IP
			if ip.IsUnspecified() {
				if ip.To4() != nil {
					ip = net.IPv4(127, 0, 0, 1)
				} else {
					ip = net.IPv6loopback
				}
			}
			dialHost := ip.String()
			if selected.Zone != "" {
				dialHost += "%" + selected.Zone
			}
			verifiedTarget := net.JoinHostPort(dialHost, port)
			connection, dialErr := dial(ctx, network, verifiedTarget)
			if dialErr == nil {
				return connection, nil
			}
			dialErrors = append(dialErrors, fmt.Errorf("%s: %w", verifiedTarget, dialErr))
		}
		return nil, fmt.Errorf("dial verified local %s target: %w", service, errors.Join(dialErrors...))
	}
}
