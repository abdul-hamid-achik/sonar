package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPClient manages a single MCP server connection.
type MCPClient struct {
	name          string
	client        *sdkmcp.Client
	session       *sdkmcp.ClientSession
	instructions  string
	processCancel context.CancelFunc
}

// Connect establishes a connection to an MCP server using the specified transport.
func Connect(ctx context.Context, name, command string, args []string, env []string, transport, url string) (*MCPClient, error) {
	return connectWithVersion(ctx, developmentImplementationVersion, name, command, args, env, transport, url, false)
}

func connectWithVersion(ctx context.Context, implementationVersion, name, command string, args []string, env []string, transport, url string, localOnly bool) (*MCPClient, error) {
	return connectWithVersionAndTrust(ctx, implementationVersion, name, command, args, env, transport, url, localOnly, "")
}

func connectWithVersionAndTrust(ctx context.Context, implementationVersion, name, command string, args []string, env []string, transport, url string, localOnly bool, executableSHA256 string) (*MCPClient, error) {
	return connectWithVersionTrustAndToolChanges(
		ctx, implementationVersion, name, command, args, env, transport, url,
		localOnly, executableSHA256, nil,
	)
}

func connectWithVersionTrustAndToolChanges(
	ctx context.Context,
	implementationVersion, name, command string,
	args []string,
	env []string,
	transport, url string,
	localOnly bool,
	executableSHA256 string,
	onToolListChanged func(),
) (*MCPClient, error) {
	var clientOptions *sdkmcp.ClientOptions
	if onToolListChanged != nil {
		clientOptions = &sdkmcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *sdkmcp.ToolListChangedRequest) {
				onToolListChanged()
			},
		}
	}
	client := sdkmcp.NewClient(
		clientImplementation(implementationVersion),
		clientOptions,
	)

	var t sdkmcp.Transport
	var processCancel context.CancelFunc
	var stopConnectCancellation func() bool

	switch transport {
	case "sse":
		if url == "" {
			return nil, fmt.Errorf("sse transport requires url for %s", name)
		}
		var httpClient *http.Client
		if localOnly {
			var err error
			httpClient, err = newLocalOnlyMCPHTTPClient(url)
			if err != nil {
				return nil, fmt.Errorf("sse transport for %s: %w", name, err)
			}
		}
		t = &sdkmcp.SSEClientTransport{Endpoint: url, HTTPClient: httpClient}
	case "streamable-http":
		if url == "" {
			return nil, fmt.Errorf("streamable-http transport requires url for %s", name)
		}
		var httpClient *http.Client
		if localOnly {
			var err error
			httpClient, err = newLocalOnlyMCPHTTPClient(url)
			if err != nil {
				return nil, fmt.Errorf("streamable-http transport for %s: %w", name, err)
			}
		}
		t = &sdkmcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpClient}
	default: // "stdio" or ""
		if command == "" {
			return nil, fmt.Errorf("stdio transport requires command for %s", name)
		}
		resolvedCommand, err := resolveExecutable(command)
		if err != nil {
			return nil, fmt.Errorf("stdio transport command for %s: %w", name, err)
		}
		if err := verifyTrustedExecutable(resolvedCommand, executableSHA256); err != nil {
			return nil, fmt.Errorf("stdio transport command for %s: %w", name, err)
		}
		// The SDK owns cmd.Wait, while MCPClient owns cancellation of the
		// process tree. Use a detached process context so the short connection
		// timeout can be unhooked after successful initialization.
		processCtx, cancelProcess := context.WithCancel(context.Background())
		processCancel = cancelProcess
		stopConnectCancellation = context.AfterFunc(ctx, cancelProcess)
		cmd := exec.CommandContext(processCtx, resolvedCommand, args...)
		configureProcessCancellation(cmd)
		cmd.Env = childEnvironment(cmd.Environ(), env)
		t = &sdkmcp.CommandTransport{Command: cmd}
	}

	session, err := client.Connect(ctx, t, nil)
	if stopConnectCancellation != nil {
		stopConnectCancellation()
	}
	if err != nil {
		if processCancel != nil {
			processCancel()
		}
		return nil, fmt.Errorf("connect to %s: %w", name, err)
	}

	return &MCPClient{
		name:          name,
		client:        client,
		session:       session,
		instructions:  serverInstructionsFromInitializeResult(session.InitializeResult()),
		processCancel: processCancel,
	}, nil
}

func serverInstructionsFromInitializeResult(result *sdkmcp.InitializeResult) string {
	if result == nil {
		return ""
	}
	return boundServerInstruction(result.Instructions, maxServerInstructionBytes)
}

const developmentImplementationVersion = "dev"

func clientImplementation(version string) *sdkmcp.Implementation {
	version = strings.TrimSpace(version)
	if version == "" {
		version = developmentImplementationVersion
	}
	return &sdkmcp.Implementation{Name: "sonar", Version: version}
}

// Name returns the server name.
func (c *MCPClient) Name() string { return c.name }

// Instructions returns bounded usage guidance supplied by the MCP server
// during initialization. Callers must continue to treat it as untrusted
// server-authored content rather than host policy.
func (c *MCPClient) Instructions() string {
	if c == nil {
		return ""
	}
	return c.instructions
}

// ListTools returns all tools from this server.
func (c *MCPClient) ListTools(ctx context.Context) ([]*sdkmcp.Tool, error) {
	caps := c.session.InitializeResult()
	if caps == nil || caps.Capabilities.Tools == nil {
		return nil, nil
	}

	var tools []*sdkmcp.Tool
	for tool, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return tools, fmt.Errorf("list tools from %s: %w", c.name, err)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// CallTool invokes a tool on this server.
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error) {
	result, err := c.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("call tool %s on %s: %w", name, c.name, err)
	}

	structured := marshalBoundedMCPValue(result.StructuredContent)
	content := renderToolResult(result)
	// StructuredContent is host-only parser input. Never stringify it into the
	// model/UI text channel: typed payloads can contain secrets, large evidence,
	// or fields whose meaning depends on an exact companion-tool contract. The
	// agent derives a bounded semantic receipt after projection when no safe
	// unstructured content was supplied.
	var errorMeta json.RawMessage
	if result.Meta != nil {
		errorMeta = marshalBoundedMCPValue(result.Meta["error"])
	}
	return &ToolResult{
		Content:    content,
		Structured: structured,
		ErrorMeta:  errorMeta,
		IsError:    result.IsError,
	}, nil
}

const maxRenderedMCPResultBytes = 96 * 1024

// renderToolResult renders only unstructured MCP content and emits bounded
// receipts for binary/resource blocks. StructuredContent has its own typed
// boundary and must not be concatenated here: doing so duplicates typed MCP
// handlers' JSON and makes exact semantic parsing impossible.
func renderToolResult(result *sdkmcp.CallToolResult) string {
	const truncatedMarker = "\n... [MCP result truncated]"
	var b strings.Builder
	truncated := false
	appendPart := func(part string) {
		if truncated || part == "" {
			return
		}
		if b.Len() > 0 {
			part = "\n" + part
		}
		remaining := maxRenderedMCPResultBytes - len(truncatedMarker) - b.Len()
		if remaining <= 0 {
			truncated = true
			return
		}
		if len(part) > remaining {
			b.WriteString(part[:remaining])
			truncated = true
			return
		}
		b.WriteString(part)
	}

	for _, content := range result.Content {
		switch value := content.(type) {
		case *sdkmcp.TextContent:
			appendPart(value.Text)
		case *sdkmcp.ImageContent:
			appendPart(fmt.Sprintf("[image: mime=%s encoded_bytes=%d]", value.MIMEType, len(value.Data)))
		case *sdkmcp.AudioContent:
			appendPart(fmt.Sprintf("[audio: mime=%s encoded_bytes=%d]", value.MIMEType, len(value.Data)))
		case *sdkmcp.ResourceLink:
			appendPart(fmt.Sprintf("[resource: uri=%s name=%s mime=%s]", value.URI, value.Name, value.MIMEType))
		case *sdkmcp.EmbeddedResource:
			if value.Resource == nil {
				appendPart("[embedded resource: empty]")
			} else if value.Resource.Text != "" {
				appendPart(fmt.Sprintf("[embedded resource: uri=%s mime=%s]\n%s", value.Resource.URI, value.Resource.MIMEType, value.Resource.Text))
			} else {
				appendPart(fmt.Sprintf("[embedded resource: uri=%s mime=%s encoded_bytes=%d]", value.Resource.URI, value.Resource.MIMEType, len(value.Resource.Blob)))
			}
		default:
			appendPart(fmt.Sprintf("[MCP content: %T]", content))
		}
	}

	if truncated {
		b.WriteString(truncatedMarker)
	}
	return b.String()
}

// marshalBoundedMCPValue preserves a JSON value atomically. Oversized or
// invalid non-nil values become a JSON null marker instead of disappearing.
// The marker preserves the fact that typed content was present, so callers do
// not mistake a duplicated TextContent payload for genuinely unstructured
// output and accidentally bypass the semantic parser boundary.
func marshalBoundedMCPValue(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maxRenderedMCPResultBytes {
		return json.RawMessage("null")
	}
	return append(json.RawMessage(nil), encoded...)
}

// Close shuts down the MCP server connection.
func (c *MCPClient) Close() error {
	if c.processCancel != nil {
		c.processCancel()
	}
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}

// IsConnected returns true if the client has an active session.
func (c *MCPClient) IsConnected() bool {
	return c.session != nil
}

// Ping checks if the server is still responsive.
// Returns nil if the server responds, error otherwise.
func (c *MCPClient) Ping(ctx context.Context) error {
	if c.session == nil {
		return fmt.Errorf("no session")
	}

	// Try listing tools as a health check
	_, err := c.ListTools(ctx)
	return err
}
