package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestFormatToolArgsForToolKeepsMCPPayloadsPrivate(t *testing.T) {
	secret := "SECRET_MCP_ARGUMENT"
	tests := []struct {
		name     string
		tool     string
		args     map[string]any
		contains []string
	}{
		{
			name:     "direct MCP is opaque",
			tool:     "cortex__cortex_start_task",
			args:     map[string]any{"goal": secret, "token": secret},
			contains: []string{hiddenToolArguments},
		},
		{
			name:     "security alias is opaque",
			tool:     "tinyvault_get",
			args:     map[string]any{"key": secret},
			contains: []string{hiddenToolArguments},
		},
		{
			name: "gateway retains route",
			tool: "mcphub__mcphub_call_tool",
			args: map[string]any{
				"server": "cortex", "tool": "cortex__investigate",
				"arguments": map[string]any{"query": secret, "authorization": secret},
			},
			contains: []string{`server="cortex"`, `tool="investigate"`},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := FormatToolArgsForTool(test.tool, test.args)
			if strings.Contains(got, secret) {
				t.Fatalf("formatted arguments leaked secret: %q", got)
			}
			for _, fragment := range test.contains {
				if !strings.Contains(got, fragment) {
					t.Fatalf("formatted arguments = %q, missing %q", got, fragment)
				}
			}
		})
	}
}

func TestSafeToolArgsForPersistenceRetainsOnlyMCPHubRoute(t *testing.T) {
	secret := "SECRET_NESTED_ARGUMENT"
	original := map[string]any{
		"server": "cortex",
		"tool":   "cortex__investigate",
		"arguments": map[string]any{
			"query": secret,
			"token": secret,
		},
	}
	safe := SafeToolArgsForPersistence("mcphub__mcphub_call_tool", original)
	encoded, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("safe gateway arguments leaked secret: %s", encoded)
	}
	if got, want := safe["server"], "cortex"; got != want {
		t.Fatalf("safe server = %#v, want %#v", got, want)
	}
	if got, want := safe["tool"], "investigate"; got != want {
		t.Fatalf("safe tool = %#v, want %#v", got, want)
	}
	if got := safe["arguments"]; !reflect.DeepEqual(got, map[string]any{"redacted": true}) {
		t.Fatalf("nested arguments = %#v", got)
	}
	if original["arguments"].(map[string]any)["query"] != secret {
		t.Fatal("sanitization mutated provider-owned arguments")
	}
}

func TestSanitizeMessagesForPersistenceRedactsSensitiveArguments(t *testing.T) {
	secret := "SESSION_TOOL_SECRET"
	messages := []llm.Message{{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{
			{ID: "mcp", Name: "monitor__monitor_snapshot", Arguments: map[string]any{"token": secret, "filter": secret}},
			{ID: "write", Name: "write", Arguments: map[string]any{"path": "README.md", "content": secret}},
		},
	}}

	safe := SanitizeMessagesForPersistence(messages)
	encoded, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("persisted messages leaked tool arguments: %s", encoded)
	}
	if got := safe[0].ToolCalls[0].Arguments["redacted"]; got != true {
		t.Fatalf("direct MCP marker = %#v, want true", got)
	}
	if got := safe[0].ToolCalls[1].Arguments["path"]; got != "README.md" {
		t.Fatalf("non-sensitive local path = %#v", got)
	}
	if got := safe[0].ToolCalls[1].Arguments["content"]; got != "[hidden]" {
		t.Fatalf("local content = %#v, want hidden", got)
	}
	if messages[0].ToolCalls[1].Arguments["content"] != secret {
		t.Fatal("message sanitization mutated live model history")
	}
}

func TestSanitizeMessagesForPersistenceStripsTransientImages(t *testing.T) {
	secret := []byte("SESSION_IMAGE_SECRET")
	messages := []llm.Message{{
		Role:    "user",
		Content: "inspect this",
		Images:  []llm.ImageData{{MediaType: "image/png", Data: secret}},
	}}

	safe := SanitizeMessagesForPersistence(messages)
	if len(safe[0].Images) != 0 {
		t.Fatalf("sanitized images = %#v, want none", safe[0].Images)
	}
	if len(messages[0].Images) != 1 || string(messages[0].Images[0].Data) != string(secret) {
		t.Fatal("image sanitization mutated live model history")
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(secret)) || strings.Contains(string(encoded), "images") {
		t.Fatalf("persisted messages leaked image data: %s", encoded)
	}
}

func TestSanitizeMessagesForPersistenceRetainsOnlyDurableImageMetadata(t *testing.T) {
	data := []byte("DURABLE_IMAGE_BYTES")
	referenced, err := llm.NewReferencedImageData("capture.png", "image/png", 320, 200, data)
	if err != nil {
		t.Fatal(err)
	}
	invalid := referenced
	invalid.SHA256 = strings.Repeat("0", 64)
	messages := []llm.Message{{
		Role: "user",
		Images: []llm.ImageData{
			referenced,
			{MediaType: "image/png", Data: []byte("transient")},
			invalid,
		},
	}}

	safe := SanitizeMessagesForPersistence(messages)
	if len(safe[0].Images) != 1 {
		t.Fatalf("durable images = %#v, want one valid reference", safe[0].Images)
	}
	if len(safe[0].Images[0].Data) != 0 || safe[0].Images[0].SHA256 != referenced.SHA256 {
		t.Fatalf("sanitized reference = %#v", safe[0].Images[0])
	}
	if len(messages[0].Images[0].Data) == 0 || &safe[0].Images[0] == &messages[0].Images[0] {
		t.Fatal("sanitization mutated or aliased live image history")
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(data)) || !strings.Contains(string(encoded), referenced.SHA256) {
		t.Fatalf("durable image JSON = %s", encoded)
	}
}

func TestSanitizeMessagesForPersistenceDropsImagesFromNonUserRoles(t *testing.T) {
	image, err := llm.NewReferencedImageData("capture.png", "image/png", 10, 10, []byte("image bytes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"system", "assistant", "tool"} {
		messages := SanitizeMessagesForPersistence([]llm.Message{{Role: role, Content: "content", Images: []llm.ImageData{image}}})
		if len(messages) != 1 || len(messages[0].Images) != 0 {
			t.Fatalf("role %q retained image metadata: %#v", role, messages)
		}
	}
}
