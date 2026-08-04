package agent

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Verify HeadlessOutput satisfies the Output interface at compile time.
var _ Output = (*HeadlessOutput)(nil)

func TestHeadlessOutput_StreamText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)

	out.StreamText("hello ")
	out.StreamText("world")

	if got := stdout.String(); got != "hello world" {
		t.Errorf("StreamText: stdout = %q, want %q", got, "hello world")
	}
	if stderr.Len() != 0 {
		t.Errorf("StreamText: unexpected stderr output: %q", stderr.String())
	}
}

func TestHeadlessOutput_StreamDone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)

	out.StreamText("response")
	out.StreamDone(100, 50)

	if got := stdout.String(); got != "response\n" {
		t.Errorf("StreamDone: stdout = %q, want %q", got, "response\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("StreamDone: unexpected stderr output: %q", stderr.String())
	}
}

func TestHeadlessOutput_GoalTurnStats(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)
	out.StreamText("completed the requested change")
	out.StreamDone(17, 42)
	out.ToolCallStart("call-1", "read", map[string]any{"path": "README.md"})
	out.ToolCallResult("call-1", "read", "contents", false, time.Millisecond)
	summary, evalTokens, productive := out.GoalTurnStats()
	if summary != "settled 1 tool call(s), 1 successful" || evalTokens != 17 || !productive {
		t.Fatalf("goal stats = %q/%d/%t", summary, evalTokens, productive)
	}
}

func TestHeadlessOutput_GoalTurnStatsDoesNotTreatProseAsConcreteProgress(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)
	out.StreamText("I think this is complete")
	summary, _, productive := out.GoalTurnStats()
	if summary != "assistant yielded without a concrete tool receipt" || productive {
		t.Fatalf("goal stats = %q productive=%t", summary, productive)
	}
}

func TestHeadlessOutput_ToolCallStart(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)

	out.ToolCallStart("call-1", "read_file", map[string]any{"path": "/tmp/test.go"})

	if stdout.Len() != 0 {
		t.Errorf("ToolCallStart: unexpected stdout output: %q", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "read_file") {
		t.Errorf("ToolCallStart: stderr = %q, missing tool name", got)
	}
	if !strings.HasPrefix(got, "→ ") {
		t.Errorf("ToolCallStart: stderr = %q, missing arrow prefix", got)
	}
}

func TestHeadlessOutput_ToolCallStartDoesNotPrintMCPArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)
	secret := "HEADLESS_MCP_SECRET"

	out.ToolCallStart("call-1", "mcphub__mcphub_call_tool", map[string]any{
		"server": "cortex",
		"tool":   "cortex__investigate",
		"arguments": map[string]any{
			"query":   secret,
			"api_key": secret,
		},
	})

	got := stderr.String()
	if strings.Contains(got, secret) || strings.Contains(got, "api_key") {
		t.Fatalf("headless diagnostic leaked MCP arguments: %q", got)
	}
	for _, route := range []string{"cortex", "investigate"} {
		if !strings.Contains(got, route) {
			t.Fatalf("headless diagnostic = %q, missing route %q", got, route)
		}
	}
}

func TestHeadlessOutput_ToolCallResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)

	out.ToolCallResult("call-1", "read_file", "file contents here", false, 150*time.Millisecond)

	if stdout.Len() != 0 {
		t.Errorf("ToolCallResult: unexpected stdout output: %q", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "read_file") {
		t.Errorf("ToolCallResult: stderr = %q, missing tool name", got)
	}
	if !strings.Contains(got, "ok") {
		t.Errorf("ToolCallResult: stderr = %q, missing ok status", got)
	}
	if !strings.Contains(got, "file contents here") {
		t.Errorf("ToolCallResult: stderr = %q, missing result content", got)
	}
}

func TestHeadlessOutput_ToolCallResult_Error(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)

	out.ToolCallResult("call-1", "write_file", "permission denied", true, 50*time.Millisecond)

	got := stderr.String()
	if !strings.Contains(got, "ERROR") {
		t.Errorf("ToolCallResult error: stderr = %q, missing ERROR status", got)
	}
}

func TestHeadlessOutput_ToolCallResult_LongResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)

	longResult := strings.Repeat("x", 300)
	out.ToolCallResult("call-1", "search", longResult, false, 100*time.Millisecond)

	got := stderr.String()
	if strings.Contains(got, strings.Repeat("x", 300)) {
		t.Error("ToolCallResult: long result should be truncated")
	}
	if !strings.Contains(got, "...") {
		t.Error("ToolCallResult: truncated result should end with ...")
	}
}

func TestHeadlessOutput_SystemMessage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)

	out.SystemMessage("compacting conversation")

	if stdout.Len() != 0 {
		t.Errorf("SystemMessage: unexpected stdout output: %q", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "[system]") {
		t.Errorf("SystemMessage: stderr = %q, missing [system] prefix", got)
	}
	if !strings.Contains(got, "compacting conversation") {
		t.Errorf("SystemMessage: stderr = %q, missing message", got)
	}
}

func TestHeadlessOutput_Error(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newHeadlessOutput(&stdout, &stderr)

	out.Error("something went wrong")

	if stdout.Len() != 0 {
		t.Errorf("Error: unexpected stdout output: %q", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "[error]") {
		t.Errorf("Error: stderr = %q, missing [error] prefix", got)
	}
	if !strings.Contains(got, "something went wrong") {
		t.Errorf("Error: stderr = %q, missing message", got)
	}
}

func TestNewHeadlessOutput(t *testing.T) {
	out := NewHeadlessOutput()
	if out == nil {
		t.Fatal("NewHeadlessOutput returned nil")
	}
	if out.stdout == nil || out.stderr == nil {
		t.Error("NewHeadlessOutput: writers should not be nil")
	}
}
