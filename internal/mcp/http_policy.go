package mcp

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/netpolicy"
)

// newLocalOnlyMCPHTTPClient creates an HTTP client whose authority is limited
// to the exact local origin selected by configuration. The policy is enforced
// at both the request and dial boundaries: redirects, SDK reconnects and an
// absolute message endpoint supplied by an SSE server all reuse this client.
func newLocalOnlyMCPHTTPClient(endpoint string) (*http.Client, error) {
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return nil, fmt.Errorf("invalid MCP HTTP endpoint %q", endpoint)
	}
	if err := validateLocalMCPURL(origin); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	}
	// A local-only request must never be handed to HTTP_PROXY/HTTPS_PROXY.
	transport.Proxy = nil
	transport.DialContext = localOnlyMCPDialContext(net.DefaultResolver, dialer.DialContext)

	policy := &localOnlyMCPRoundTripper{
		origin: originKey(origin),
		next:   transport,
	}
	return &http.Client{
		Transport: policy,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 MCP redirects")
			}
			if err := policy.validate(req.URL); err != nil {
				return err
			}
			return nil
		},
	}, nil
}

type localOnlyMCPRoundTripper struct {
	origin string
	next   http.RoundTripper
}

func (t *localOnlyMCPRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("refusing MCP request without a target URL")
	}
	if err := t.validate(req.URL); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(req)
}

func (t *localOnlyMCPRoundTripper) validate(target *url.URL) error {
	if err := validateLocalMCPURL(target); err != nil {
		return err
	}
	if got := originKey(target); got != t.origin {
		return fmt.Errorf("privacy.local_only refuses cross-origin MCP request to %s", target.Redacted())
	}
	return nil
}

func validateLocalMCPURL(target *url.URL) error {
	if target == nil || target.Host == "" {
		return errors.New("privacy.local_only refuses MCP request without a host")
	}
	switch strings.ToLower(target.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("privacy.local_only refuses MCP URL scheme %q", target.Scheme)
	}
	host := target.Hostname()
	if !netpolicy.IsLocalHost(host) {
		return fmt.Errorf("privacy.local_only refuses non-local MCP request to %s", target.Redacted())
	}
	return nil
}

func originKey(target *url.URL) string {
	port := target.Port()
	if port == "" {
		switch strings.ToLower(target.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return strings.ToLower(target.Scheme) + "://" + strings.ToLower(target.Hostname()) + ":" + port
}

type mcpIPResolver = netpolicy.IPResolver

type mcpDialContextFunc = netpolicy.DialContextFunc

// localOnlyMCPDialContext resolves the target itself, verifies every returned
// address and dials a verified IP rather than the hostname. That closes the
// DNS check/use race and fails closed if a localhost lookup is poisoned with a
// non-loopback answer.
func localOnlyMCPDialContext(resolver mcpIPResolver, dial mcpDialContextFunc) mcpDialContextFunc {
	return netpolicy.LocalOnlyDialContext(resolver, dial, "MCP")
}
