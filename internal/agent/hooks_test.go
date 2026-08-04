package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestSizeCapHookTruncatesOversizedResult(t *testing.T) {
	h := NewSizeCapHook(100)
	result := strings.Repeat("x", 500)
	h.PostToolUse(context.Background(), llm.ToolCall{Name: "read"}, &result, false)

	if len(result) <= 100 {
		t.Fatalf("expected a truncation notice appended, got len=%d", len(result))
	}
	if !strings.HasPrefix(result, strings.Repeat("x", 100)) {
		t.Fatal("expected the first 100 bytes preserved")
	}
	if !strings.Contains(result, "output truncated") {
		t.Fatalf("expected truncation notice, got: %q", result)
	}
}

func TestSizeCapHookLeavesSmallResult(t *testing.T) {
	h := NewSizeCapHook(100)
	result := "small output"
	h.PostToolUse(context.Background(), llm.ToolCall{Name: "read"}, &result, false)
	if result != "small output" {
		t.Fatalf("small result should be untouched, got: %q", result)
	}
}

func TestSizeCapHookDisabledWithZero(t *testing.T) {
	h := NewSizeCapHook(0)
	result := strings.Repeat("y", 1000)
	h.PostToolUse(context.Background(), llm.ToolCall{Name: "read"}, &result, false)
	if len(result) != 1000 {
		t.Fatalf("zero max should disable capping, got len=%d", len(result))
	}
}

func TestSizeCapHookPreservesUTF8AtByteBoundary(t *testing.T) {
	h := NewSizeCapHook(5)
	result := "界界界"
	h.PostToolUse(context.Background(), llm.ToolCall{Name: "read"}, &result, false)
	if !utf8.ValidString(result) || !strings.HasPrefix(result, "界") ||
		!strings.Contains(result, "3 of 9 bytes shown") {
		t.Fatalf("UTF-8 cap produced %q", result)
	}
}

func TestSizeCapHookNormalizesInvalidUTF8InOneBoundedPass(t *testing.T) {
	h := NewSizeCapHook(12)
	result := "safe\xfftext" + strings.Repeat("界", 100_000)
	h.PostToolUse(context.Background(), llm.ToolCall{Name: "read"}, &result, false)
	if !utf8.ValidString(result) ||
		!strings.Contains(result, "safe�text") ||
		!strings.Contains(result, "output truncated") {
		t.Fatalf("invalid UTF-8 cap produced %q", result)
	}
	if len(result) > 256 {
		t.Fatalf("bounded invalid UTF-8 cap retained %d bytes", len(result))
	}
}

// blockHook blocks any tool whose name matches and records post calls.
type blockHook struct {
	blockName string
	postCalls int
}

type redactResultHook struct{ secret string }

func (redactResultHook) Name() string { return "test-redact" }
func (redactResultHook) PreToolUse(context.Context, *llm.ToolCall) (bool, string) {
	return false, ""
}
func (hook redactResultHook) PostToolUse(
	_ context.Context,
	_ llm.ToolCall,
	result *string,
	_ bool,
) {
	*result = strings.ReplaceAll(*result, hook.secret, "[redacted]")
}

func (b *blockHook) Name() string { return "test-block" }
func (b *blockHook) PreToolUse(_ context.Context, call *llm.ToolCall) (bool, string) {
	if call.Name == b.blockName {
		return true, "blocked by test hook"
	}
	return false, ""
}
func (b *blockHook) PostToolUse(_ context.Context, _ llm.ToolCall, _ *string, _ bool) {
	b.postCalls++
}

func TestPreHookBlocks(t *testing.T) {
	a := New(nil, nil, 8192)
	bh := &blockHook{blockName: "bash"}
	a.AddToolHook(bh)

	call := llm.ToolCall{Name: "bash"}
	block, reason := a.runPreHooks(context.Background(), &call)
	if !block {
		t.Fatal("expected bash to be blocked")
	}
	if reason != "blocked by test hook" {
		t.Fatalf("unexpected reason: %q", reason)
	}

	allowed := llm.ToolCall{Name: "read"}
	if block, _ := a.runPreHooks(context.Background(), &allowed); block {
		t.Fatal("expected read to be allowed")
	}
}

func TestPostHooksRunInOrderAndMutate(t *testing.T) {
	a := New(nil, nil, 8192)
	a.AddToolHook(NewSizeCapHook(5))

	result := "0123456789"
	a.runPostHooks(context.Background(), llm.ToolCall{Name: "read"}, &result, false)
	if !strings.HasPrefix(result, "01234") || !strings.Contains(result, "truncated") {
		t.Fatalf("expected size cap applied via runPostHooks, got: %q", result)
	}
}

func TestPostHooksCaptureCompleteResultAfterRedactionBeforeContextCap(t *testing.T) {
	a := New(nil, nil, 8192)
	// Registering the limiter first must not let it destroy bytes before a
	// later policy hook has redacted the complete result.
	a.AddToolHook(NewSizeCapHook(16))
	a.AddToolHook(redactResultHook{secret: "SECRET"})

	result := strings.Repeat("safe-", 20) + "SECRET"
	complete := a.runPostHooks(
		context.Background(),
		llm.ToolCall{Name: "read"},
		&result,
		false,
	)
	if strings.Contains(complete, "SECRET") || !strings.HasSuffix(complete, "[redacted]") {
		t.Fatalf("ephemeral detail was captured before redaction: %q", complete)
	}
	if len(complete) <= 16 {
		t.Fatalf("complete detail was context-capped: len=%d", len(complete))
	}
	if strings.Contains(result, "SECRET") ||
		!strings.Contains(result, "output truncated") ||
		len(result) >= len(complete) {
		t.Fatalf("durable/model result was not safely capped: %q", result)
	}
}
