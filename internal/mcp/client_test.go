package mcp

import (
	"strings"
	"testing"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClientImplementationVersion(t *testing.T) {
	if got := clientImplementation("0.3.0"); got.Name != "sonar" || got.Version != "0.3.0" {
		t.Fatalf("release implementation = %#v", got)
	}
	if got := clientImplementation("  "); got.Version != developmentImplementationVersion {
		t.Fatalf("development implementation = %#v", got)
	}
}

func TestServerInstructionsFromInitializeResult(t *testing.T) {
	if got := serverInstructionsFromInitializeResult(nil); got != "" {
		t.Fatalf("nil initialize guidance = %q", got)
	}
	got := serverInstructionsFromInitializeResult(&sdkmcp.InitializeResult{
		Instructions: "  use mcphub_search_tools before guessing  ",
	})
	if got != "use mcphub_search_tools before guessing" {
		t.Fatalf("initialize guidance = %q", got)
	}
	got = serverInstructionsFromInitializeResult(&sdkmcp.InitializeResult{
		Instructions: string([]byte{'a', 0xff, 'b'}),
	})
	if !utf8.ValidString(got) || got != "a�b" {
		t.Fatalf("invalid UTF-8 guidance was not repaired: %q", got)
	}
}

func TestRenderToolResultKeepsStructuredContentOutOfDisplayText(t *testing.T) {
	result := &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.TextContent{Text: "summary"},
			&sdkmcp.ImageContent{MIMEType: "image/png", Data: []byte("encoded")},
			&sdkmcp.ResourceLink{URI: "file:///tmp/evidence.json", Name: "evidence", MIMEType: "application/json"},
		},
		StructuredContent: map[string]any{"count": 2, "ok": true},
	}

	got := renderToolResult(result)
	for _, want := range []string{
		"summary",
		"[image: mime=image/png encoded_bytes=7]",
		"[resource: uri=file:///tmp/evidence.json name=evidence mime=application/json]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered result %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "structured:") || strings.Contains(got, `"count":2`) {
		t.Fatalf("structured content was flattened into display text: %q", got)
	}
	structured := marshalBoundedMCPValue(result.StructuredContent)
	if string(structured) != `{"count":2,"ok":true}` {
		t.Fatalf("structured content = %s", structured)
	}
}

func TestRenderToolResultIsBounded(t *testing.T) {
	got := renderToolResult(&sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: strings.Repeat("x", maxRenderedMCPResultBytes*2)}},
	})
	if len(got) > maxRenderedMCPResultBytes {
		t.Fatalf("rendered result exceeded cap: %d", len(got))
	}
	if !strings.HasSuffix(got, "... [MCP result truncated]") {
		t.Fatalf("rendered result did not disclose truncation: %q", got[len(got)-80:])
	}
}

func TestMarshalBoundedMCPValueIsAtomic(t *testing.T) {
	if got := marshalBoundedMCPValue(map[string]any{"ok": true}); string(got) != `{"ok":true}` {
		t.Fatalf("bounded value = %s", got)
	}
	if got := marshalBoundedMCPValue(map[string]any{"payload": strings.Repeat("x", maxRenderedMCPResultBytes)}); string(got) != "null" {
		t.Fatalf("oversized structured value = %q, want rejection marker", got)
	}
	if got := marshalBoundedMCPValue(func() {}); string(got) != "null" {
		t.Fatalf("unencodable structured value = %q, want rejection marker", got)
	}
}
