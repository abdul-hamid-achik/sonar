package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLocalOnlyMCPHTTPClientRejectsCrossOriginRedirect(t *testing.T) {
	var redirectedHits atomic.Int64
	const origin = "http://127.0.0.1:19765"
	client, err := newLocalOnlyMCPHTTPClient(origin + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	policy := client.Transport.(*localOnlyMCPRoundTripper)
	policy.next = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Host == "127.0.0.1:29765":
			redirectedHits.Add(1)
			return responseFor(request, http.StatusNoContent, ""), nil
		case request.URL.Path == "/same-origin":
			return responseFor(request, http.StatusTemporaryRedirect, origin+"/ok"), nil
		case request.URL.Path == "/ok":
			return responseFor(request, http.StatusNoContent, ""), nil
		default:
			return responseFor(request, http.StatusTemporaryRedirect, "http://127.0.0.1:29765/escaped"), nil
		}
	})

	response, err := client.Get(origin + "/same-origin")
	if err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("same-origin status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	response, err = client.Get(origin + "/cross-origin")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "cross-origin MCP request") {
		t.Fatalf("cross-origin redirect error = %v", err)
	}
	if hits := redirectedHits.Load(); hits != 0 {
		t.Fatalf("cross-origin target received %d requests", hits)
	}
}

func TestLocalOnlyMCPHTTPClientDisablesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client, err := newLocalOnlyMCPHTTPClient("http://127.0.0.1:9876/mcp")
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := client.Transport.(*localOnlyMCPRoundTripper)
	if !ok {
		t.Fatalf("transport = %T", client.Transport)
	}
	transport, ok := policy.next.(*http.Transport)
	if !ok {
		t.Fatalf("inner transport = %T", policy.next)
	}
	if transport.Proxy != nil {
		t.Fatal("local-only MCP transport retained an environment proxy")
	}
}

func TestLocalOnlyMCPDialRejectsPoisonedLocalhostResolution(t *testing.T) {
	resolver := mcpResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("127.0.0.1")},
			{IP: net.ParseIP("203.0.113.10")},
		}, nil
	})
	var dialed atomic.Bool
	dial := localOnlyMCPDialContext(resolver, func(context.Context, string, string) (net.Conn, error) {
		dialed.Store(true)
		return nil, errors.New("unexpected dial")
	})

	_, err := dial(context.Background(), "tcp", "localhost:8080")
	if err == nil || !strings.Contains(err.Error(), "non-loopback address") {
		t.Fatalf("poisoned DNS error = %v", err)
	}
	if dialed.Load() {
		t.Fatal("poisoned localhost resolution reached the network dialer")
	}
}

func TestLocalOnlyMCPDialPinsVerifiedLoopbackAddress(t *testing.T) {
	resolver := mcpResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	})
	var gotAddress string
	stop := errors.New("dial seam reached")
	dial := localOnlyMCPDialContext(resolver, func(_ context.Context, _ string, address string) (net.Conn, error) {
		gotAddress = address
		return nil, stop
	})

	_, err := dial(context.Background(), "tcp", "localhost:8080")
	if !errors.Is(err, stop) {
		t.Fatalf("dial error = %v, want seam error", err)
	}
	if gotAddress != "127.0.0.1:8080" {
		t.Fatalf("verified dial address = %q, want loopback IP", gotAddress)
	}
}

func TestLocalOnlyMCPDialFallsBackAcrossVerifiedLoopbackAddresses(t *testing.T) {
	resolver := mcpResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.IPv6loopback}, {IP: net.ParseIP("127.0.0.1")}}, nil
	})
	var targets []string
	dial := localOnlyMCPDialContext(resolver, func(_ context.Context, _ string, address string) (net.Conn, error) {
		targets = append(targets, address)
		if strings.HasPrefix(address, "[") {
			return nil, errors.New("IPv6 listener unavailable")
		}
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	})

	connection, err := dial(context.Background(), "tcp", "localhost:8080")
	if err != nil {
		t.Fatalf("verified loopback fallback: %v", err)
	}
	_ = connection.Close()
	if len(targets) != 2 || targets[0] != "[::1]:8080" || targets[1] != "127.0.0.1:8080" {
		t.Fatalf("dial targets = %#v", targets)
	}
}

func TestLocalOnlyMCPHTTPClientGuardsServerSuppliedSSEEndpoint(t *testing.T) {
	const origin = "http://127.0.0.1:19765"
	client, err := newLocalOnlyMCPHTTPClient(origin + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	policy := client.Transport.(*localOnlyMCPRoundTripper)
	policy.next = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != origin+"/sse" {
			return nil, fmt.Errorf("unexpected unguarded request: %s %s", request.Method, request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       reader,
			Request:    request,
		}, nil
	})
	go func() {
		_, _ = fmt.Fprint(writer, "event: endpoint\ndata: http://192.0.2.10/messages\n\n")
	}()

	transport := &sdkmcp.SSEClientTransport{Endpoint: origin + "/sse", HTTPClient: client}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("connect SSE fixture: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	err = connection.Write(ctx, &jsonrpc.Request{Method: "notifications/initialized"})
	if err == nil || !strings.Contains(err.Error(), "non-local MCP request") {
		t.Fatalf("server-supplied absolute endpoint error = %v", err)
	}
}

func TestLocalOnlyMCPHTTPClientRejectsRemoteInitialEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"https://example.com/mcp",
		"ftp://127.0.0.1/mcp",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := newLocalOnlyMCPHTTPClient(endpoint); err == nil || !strings.Contains(err.Error(), "privacy.local_only") {
				t.Fatalf("endpoint %q error = %v", endpoint, err)
			}
		})
	}
}

func TestConnectWiresLocalOnlyClientIntoBothHTTPTransports(t *testing.T) {
	for _, transport := range []string{"sse", "streamable-http"} {
		t.Run(transport, func(t *testing.T) {
			_, err := connectWithVersion(
				context.Background(),
				"test",
				"remote",
				"",
				nil,
				nil,
				transport,
				"https://example.com/mcp",
				true,
			)
			if err == nil || !strings.Contains(err.Error(), "privacy.local_only") {
				t.Fatalf("%s initial endpoint error = %v", transport, err)
			}
		})
	}
}

type mcpResolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f mcpResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func responseFor(request *http.Request, status int, location string) *http.Response {
	header := make(http.Header)
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    request,
	}
}
