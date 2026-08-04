package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

type scriptedClient struct {
	responses [][]llm.StreamChunk
	calls     int
}

func (c *scriptedClient) ChatStream(_ context.Context, _ llm.ChatOptions, fn func(llm.StreamChunk) error) error {
	if c.calls >= len(c.responses) {
		return nil
	}
	chunks := c.responses[c.calls]
	c.calls++
	for _, chunk := range chunks {
		if err := fn(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (c *scriptedClient) Ping() error   { return nil }
func (c *scriptedClient) Model() string { return "test-model" }
func (c *scriptedClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type outputRecorder struct {
	toolResults []string
	errors      []string
}

func (o *outputRecorder) StreamText(string)                            {}
func (o *outputRecorder) StreamReasoning(string)                       {}
func (o *outputRecorder) StreamDone(int, int)                          {}
func (o *outputRecorder) ToolCallStart(string, string, map[string]any) {}
func (o *outputRecorder) ToolCallResult(_ string, _ string, result string, _ bool, _ time.Duration) {
	o.toolResults = append(o.toolResults, result)
}
func (o *outputRecorder) SystemMessage(string) {}
func (o *outputRecorder) Error(message string) { o.errors = append(o.errors, message) }

type cancelAfterFirstToolHook struct {
	cancel context.CancelFunc
	calls  int
}

type cancelWhenPathExistsHook struct {
	path   string
	cancel context.CancelFunc
}

func (h *cancelWhenPathExistsHook) Name() string { return "cancel-after-dispatch" }
func (h *cancelWhenPathExistsHook) PreToolUse(context.Context, *llm.ToolCall) (bool, string) {
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(h.path); err == nil {
				h.cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return false, ""
}
func (*cancelWhenPathExistsHook) PostToolUse(context.Context, llm.ToolCall, *string, bool) {}

func (h *cancelAfterFirstToolHook) Name() string { return "cancel-after-first" }

func (h *cancelAfterFirstToolHook) PreToolUse(context.Context, *llm.ToolCall) (bool, string) {
	return false, ""
}

func (h *cancelAfterFirstToolHook) PostToolUse(context.Context, llm.ToolCall, *string, bool) {
	h.calls++
	if h.calls == 1 {
		h.cancel()
	}
}

func TestCancellationStopsQueuedMutationWithYolo(t *testing.T) {
	store := memory.NewStore(filepath.Join(t.TempDir(), "memories.json"))
	client := &scriptedClient{
		responses: [][]llm.StreamChunk{
			{{
				ToolCalls: []llm.ToolCall{
					{
						ID:        "call-1",
						Name:      "memory_save",
						Arguments: map[string]any{"content": "first mutation"},
					},
					{
						ID:        "call-2",
						Name:      "memory_save",
						Arguments: map[string]any{"content": "second mutation"},
					},
				},
				Done: true,
			}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hook := &cancelAfterFirstToolHook{cancel: cancel}
	ag := New(client, nil, 0)
	ag.SetMemoryStore(store)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.AddToolHook(hook)
	ag.AddUserMessage("save both memories")

	out := &outputRecorder{}
	err := ag.Run(ctx, out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if hook.calls != 1 {
		t.Fatalf("executed tool calls = %d, want 1", hook.calls)
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("stored memories = %d, want 1", got)
	}
	memories, _ := store.Recent(5)
	if len(memories) != 1 || memories[0].Content != "first mutation" {
		t.Fatalf("cancelled queued mutation executed: %#v", memories)
	}
	if len(out.toolResults) != 2 || !strings.Contains(out.toolResults[0], "Memory saved") {
		t.Fatalf("completed effect lost its tool receipt: %#v", out.toolResults)
	}
	if !strings.Contains(out.toolResults[1], "CANCELLED — NOT DISPATCHED") {
		t.Fatalf("queued effect missing not-dispatched receipt: %#v", out.toolResults)
	}
	messages := ag.Messages()
	if got := messages[len(messages)-1]; got.ToolCallID != "call-2" || !strings.Contains(got.Content, "NOT DISPATCHED") {
		t.Fatalf("durable transcript left queued call unresolved: %#v", got)
	}
}

func TestCancellationDuringBashEmitsUnknownOutcomeReceipt(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "started")
	client := &scriptedClient{
		responses: [][]llm.StreamChunk{
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:   "call-bash",
						Name: "bash",
						Arguments: map[string]any{
							"command": "touch started && exec sleep 5",
						},
					}},
					Done: true,
				},
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag := New(client, nil, 0)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.AddToolHook(&cancelWhenPathExistsHook{path: marker, cancel: cancel})
	ag.AddUserMessage("run the command")
	out := &outputRecorder{}
	err := ag.Run(ctx, out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(out.toolResults) != 1 || !strings.Contains(out.toolResults[0], "OUTCOME UNKNOWN:") {
		t.Fatalf("cancelled effect missing unknown-outcome receipt: %#v", out.toolResults)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("bash was not dispatched before cancellation: %v", err)
	}
}

func TestEffectfulErrorAfterDispatchKeepsBackendReceipt(t *testing.T) {
	workDir := t.TempDir()
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "call-bash", Name: "bash", Arguments: map[string]any{"command": "touch partial && false"},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag := New(client, nil, 0)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.AddUserMessage("run the command")
	out := &outputRecorder{}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "partial")); err != nil {
		t.Fatalf("bash did not partially mutate before failure: %v", err)
	}
	// The backend answered: the exit status is a known outcome, so the raw
	// receipt reaches the model instead of the unverifiable-outcome framing.
	if len(out.toolResults) != 1 || !strings.Contains(out.toolResults[0], "exit status 1") || strings.Contains(out.toolResults[0], "OUTCOME UNKNOWN:") {
		t.Fatalf("effectful failure receipt = %#v", out.toolResults)
	}

	// dispatchedEffectErrorReceipt now frames only genuinely unverifiable
	// outcomes: a dispatch cancelled mid-flight or lost in transport.
	mcpReceipt := dispatchedEffectErrorReceipt("server__mutate", "remote rejected final step", nil)
	if !strings.Contains(mcpReceipt, "OUTCOME UNKNOWN:") || !strings.Contains(mcpReceipt, "partially taken effect") {
		t.Fatalf("unverifiable-outcome receipt = %q", mcpReceipt)
	}
}

func TestMemoryPersistenceFailureFailsAndRollsBackMemory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "memory")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "store.json")
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocks memory directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "call-memory", Name: "memory_save", Arguments: map[string]any{"content": "partial memory"},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag := New(client, nil, 0)
	ag.SetMemoryStore(store)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.AddUserMessage("remember this")
	out := &outputRecorder{}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(out.toolResults) != 1 || !strings.Contains(out.toolResults[0], "error saving memory") || strings.Contains(out.toolResults[0], "OUTCOME UNKNOWN:") {
		t.Fatalf("memory persistence receipt = %#v", out.toolResults)
	}
	if got := store.Count(); got != 0 {
		t.Fatalf("failed persistence left %d phantom in-memory entries", got)
	}
}

func TestCancellationAwaitingApprovalClosesQueuedToolReceipts(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call-1", Name: "write", Arguments: map[string]any{"path": "one", "content": "one"}},
					{ID: "call-2", Name: "write", Arguments: map[string]any{"path": "two", "content": "two"}},
				},
				Done: true,
			},
		},
	}}
	ag := New(client, nil, 0)
	ag.SetWorkDir(t.TempDir())
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	approvalStarted := make(chan struct{})
	ag.SetApprovalCallback(func(permission.ApprovalRequest) { close(approvalStarted) })
	ag.AddUserMessage("write both files")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	out := &outputRecorder{}
	go func() { done <- ag.Run(ctx, out) }()
	<-approvalStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(out.toolResults) != 2 {
		t.Fatalf("tool receipts = %d, want 2: %#v", len(out.toolResults), out.toolResults)
	}
	for _, result := range out.toolResults {
		if !strings.Contains(result, "CANCELLED — NOT DISPATCHED") {
			t.Fatalf("approval cancellation receipt = %q", result)
		}
	}
}

func TestCloseCancelsAndJoinsActiveTurn(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "started")
	client := &scriptedClient{
		responses: [][]llm.StreamChunk{
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID: "call-close", Name: "bash",
						Arguments: map[string]any{"command": "touch started && exec sleep 5"},
					}},
					Done: true,
				},
			},
		},
	}
	ag := New(client, nil, 0)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.AddUserMessage("run")
	out := &outputRecorder{}
	runDone := make(chan error, 1)
	go func() { runDone <- ag.Run(context.Background(), out) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tool backend did not start")
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	ag.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close did not promptly join the active turn: %s", elapsed)
	}
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error after Close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run survived Agent.Close")
	}
	if len(out.toolResults) != 1 || !strings.Contains(out.toolResults[0], "OUTCOME UNKNOWN:") {
		t.Fatalf("shutdown lost dispatched-tool receipt: %#v", out.toolResults)
	}
	if err := ag.Run(context.Background(), &outputRecorder{}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed agent accepted a new turn: %v", err)
	}
	secondClose := time.Now()
	ag.Close()
	if elapsed := time.Since(secondClose); elapsed > 100*time.Millisecond {
		t.Fatalf("repeated Close was not idempotent: %s", elapsed)
	}
}

func TestMCPTransportErrorsAreOutcomeUnknown(t *testing.T) {
	receipt := mcpDispatchErrorReceipt("server__mutate", errors.New("connection reset by peer"))
	if !strings.HasPrefix(receipt, "OUTCOME UNKNOWN:") || !strings.Contains(receipt, "may have taken effect") {
		t.Fatalf("transport error receipt = %q", receipt)
	}
}

func TestAuthorizeToolCallRejectsCancelledYolo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ag := New(nil, nil, 0)
	ag.SetPermissionChecker(permission.NewChecker(nil, true))

	if ag.authorizeToolCall(ctx, llm.ToolCall{Name: "write"}, &outputRecorder{}) {
		t.Fatal("cancelled context was authorized by yolo")
	}
}

func TestEnsureToolCallIDsFillsMissingAndDuplicateIDs(t *testing.T) {
	calls := []llm.ToolCall{
		{Name: "read", ID: "provider-id"},
		{Name: "read", ID: "provider-id"},
		{Name: "write"},
	}
	ensureToolCallIDs(calls, "turn-7", 2)

	if calls[0].ID != "provider-id" {
		t.Fatalf("unique provider ID changed to %q", calls[0].ID)
	}
	seen := map[string]bool{}
	for _, call := range calls {
		if call.ID == "" {
			t.Fatal("tool call ID remained empty")
		}
		if seen[call.ID] {
			t.Fatalf("duplicate tool call ID %q", call.ID)
		}
		seen[call.ID] = true
	}
}

func TestEnsureToolCallIDsAvoidsRestoredTranscriptCollisions(t *testing.T) {
	ag := New(nil, nil, 0)
	ag.ReplaceMessages([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "t1-tool-1-1", Name: "read"}}},
		{Role: "tool", ToolCallID: "t1-tool-1-1", ToolName: "read", Content: "old"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "provider-id", Name: "read"}}},
		{Role: "tool", ToolCallID: "provider-id", ToolName: "read", Content: "old explicit"},
	})
	calls := []llm.ToolCall{
		{Name: "read"},
		{Name: "read", ID: "provider-id"},
	}
	ag.ensureToolCallIDs(calls, "t1", 1)
	used := map[string]bool{"t1-tool-1-1": true, "provider-id": true}
	for _, call := range calls {
		if call.ID == "" || used[call.ID] {
			t.Fatalf("restored transcript ID collision: %#v", calls)
		}
		used[call.ID] = true
	}
}

func TestMCPServerScopeFailsClosedOutsideProfile(t *testing.T) {
	ag := New(nil, nil, 0)
	ag.SetMCPServerScope([]string{"mcphub"})
	if !ag.allowsMCPTool("mcphub__cortex__investigate") {
		t.Fatal("scoped MCPHub tool was denied")
	}
	for _, name := range []string{"obsidian__create", "unqualified_tool"} {
		if ag.allowsMCPTool(name) {
			t.Fatalf("out-of-scope tool %q was allowed", name)
		}
	}
	ag.SetMCPServerScope(nil)
	if !ag.allowsMCPTool("obsidian__create") {
		t.Fatal("empty scope should restore all configured servers")
	}
	ag.DenyAllMCPTools()
	if ag.allowsMCPTool("obsidian__create") {
		t.Fatal("explicit empty MCP scope allowed a tool")
	}
}

func TestToolResultContextCapIsBoundedAndUTF8Safe(t *testing.T) {
	result := strings.Repeat("界", 10_000)
	got := capToolResultForContext(result, 4096)
	// 4k windows use the tighter small-window budget (numCtx/12 tokens * 4 bytes).
	if want := toolResultByteLimit(4096); len(got) > want {
		t.Fatalf("tool result cap = %d bytes, want <= %d", len(got), want)
	}
	if !utf8.ValidString(got) || !strings.Contains(got, "truncated") {
		t.Fatalf("capped result is invalid or undisclosed: %q", got[len(got)-80:])
	}
}

func TestToolResultByteLimitScalesWithContextWindow(t *testing.T) {
	// 16k must not equate tokens to bytes 1:1 (that allowed ~16KB ≈ 4k tokens
	// per result and blew after_tools admission on ornith-sized windows).
	if got := toolResultByteLimit(16_384); got >= 16_384 {
		t.Fatalf("16k byte limit = %d, want a fraction of the window", got)
	}
	if got := toolResultByteLimit(16_384); got != 8_192 {
		t.Fatalf("16k byte limit = %d, want 8192 (numCtx/8 tokens * 4)", got)
	}
	if got := toolResultByteLimit(4_096); got != 2_048 {
		t.Fatalf("4k byte limit = %d, want 2048 floor/small-window budget", got)
	}
	if got := toolResultByteLimit(131_072); got != 65_536 {
		t.Fatalf("128k byte limit = %d, want 65536", got)
	}
}

func TestShrinkToolResultsForContextRecoversOversizedHistory(t *testing.T) {
	ag := New(nil, nil, 16_384)
	// Simulate the 68b9f1a failure shape: several large recent tool receipts
	// already past the ordinary per-result cap floor of a prior looser policy.
	large := strings.Repeat("x", 16_000)
	ag.AppendMessage(llm.Message{Role: "user", Content: "review the tests"})
	ag.AppendMessage(llm.Message{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read"}}})
	ag.AppendMessage(llm.Message{Role: "tool", Content: large, ToolName: "read", ToolCallID: "c1"})
	ag.AppendMessage(llm.Message{Role: "tool", Content: large, ToolName: "read", ToolCallID: "c2"})

	if !ag.shrinkToolResultsForContext(16_384) {
		t.Fatal("expected tool results to shrink")
	}
	limit := emergencyToolResultByteLimit(16_384)
	for _, msg := range ag.Messages() {
		if msg.Role != "tool" {
			continue
		}
		if len(msg.Content) > limit {
			t.Fatalf("tool content still %d bytes, want <= %d", len(msg.Content), limit)
		}
		if !strings.Contains(msg.Content, "truncated") {
			t.Fatalf("shrunk tool result missing marker: %q", msg.Content[len(msg.Content)-80:])
		}
	}
	// A second pass is a no-op once content is at the emergency ceiling.
	if ag.shrinkToolResultsForContext(16_384) {
		t.Fatal("second shrink should not change already-capped results")
	}
}

func TestModeToolPolicies_BlockMutationOutsideBuild(t *testing.T) {
	cases := []struct {
		name        string
		policy      ToolPolicy
		expectWrite bool
	}{
		{name: "ask blocks write", policy: AskToolPolicy()},
		{name: "plan blocks write", policy: PlanToolPolicy()},
		{name: "build allows write", policy: BuildToolPolicy(), expectWrite: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			target := filepath.Join(tmpDir, "out.txt")

			client := &scriptedClient{
				responses: [][]llm.StreamChunk{
					{{
						ToolCalls: []llm.ToolCall{{
							ID:   "call-1",
							Name: "write",
							Arguments: map[string]any{
								"path":    "out.txt",
								"content": "hello from policy test",
							},
						}},
						Done: true,
					}},
					{{Text: "done", Done: true}},
				},
			}

			ag := New(client, nil, 0)
			ag.SetWorkDir(tmpDir)
			ag.SetModeContext("test", tt.policy)
			ag.AddUserMessage("write the output file")

			out := &outputRecorder{}
			if err := ag.Run(context.Background(), out); err != nil {
				t.Fatalf("run agent: %v", err)
			}

			data, err := os.ReadFile(target)
			if tt.expectWrite {
				if err != nil {
					t.Fatalf("expected write to succeed: %v", err)
				}
				if string(data) != "hello from policy test" {
					t.Fatalf("unexpected file content: %q", string(data))
				}
				return
			}

			if err == nil {
				t.Fatalf("expected write to be blocked, file content=%q", string(data))
			}
			if !strings.Contains(strings.Join(out.toolResults, "\n"), "blocked in current mode") {
				t.Fatalf("expected blocked tool result, got %v", out.toolResults)
			}
		})
	}
}

func TestRiskyBuiltinFailsClosedWithoutApprovalUI(t *testing.T) {
	tmpDir := t.TempDir()
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID:   "call-1",
			Name: "write",
			Arguments: map[string]any{
				"path": "out.txt", "content": "must not be written",
			},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag := New(client, nil, 0)
	ag.SetWorkDir(tmpDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	ag.AddUserMessage("write the output")
	out := &outputRecorder{}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatalf("run agent: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("unapproved write executed, stat err=%v", err)
	}
	result := strings.Join(out.toolResults, "\n")
	if !strings.Contains(result, "approval_ui_unavailable") || strings.Contains(result, "user denied") {
		t.Fatalf("missing typed host refusal result: %v", out.toolResults)
	}
}

func TestLegacyPersistedAllowRequiresNewExactApproval(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "permissions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.UpsertToolPermission(context.Background(), db.UpsertToolPermissionParams{
		ToolName: "bash", Policy: "allow",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertToolPermission(context.Background(), db.UpsertToolPermissionParams{
		ToolName: "server__mutate", Policy: "allow",
	}); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "call-bash", Name: "bash", Arguments: map[string]any{"command": "touch forbidden"},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag := New(client, nil, 4096)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(store, false))
	ag.AddUserMessage("run it")
	out := &outputRecorder{}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "forbidden")); !os.IsNotExist(err) {
		t.Fatalf("persisted allow dispatched headless bash: %v", err)
	}
	if len(out.toolResults) != 1 || !strings.Contains(out.toolResults[0], "approval_ui_unavailable") {
		t.Fatalf("headless denial receipt = %#v", out.toolResults)
	}

	// The gate is shared by MCP and local effects before dispatch.
	mcpOut := &outputRecorder{}
	if ag.authorizeToolCall(context.Background(), llm.ToolCall{ID: "mcp", Name: "server__mutate"}, mcpOut) {
		t.Fatal("persisted MCP allow bypassed missing interactive capability")
	}
	if len(mcpOut.toolResults) != 1 || !strings.Contains(mcpOut.toolResults[0], "approval_ui_unavailable") {
		t.Fatalf("MCP headless denial receipt = %#v", mcpOut.toolResults)
	}
}

func TestRiskyBuiltinExecutesWithExplicitApproval(t *testing.T) {
	tmpDir := t.TempDir()
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID:   "call-1",
			Name: "write",
			Arguments: map[string]any{
				"path": "out.txt", "content": "approved",
			},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag := New(client, nil, 0)
	ag.SetWorkDir(tmpDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	ag.SetApprovalCallback(permission.AlwaysAllow)
	ag.AddUserMessage("write the output")
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatalf("run agent: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "out.txt"))
	if err != nil {
		t.Fatalf("approved write failed: %v", err)
	}
	if string(data) != "approved" {
		t.Fatalf("content = %q, want approved", data)
	}
}

func TestHandleRead_TruncationCount(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := New(nil, nil, 0)
	ag.SetWorkDir(tmpDir)

	got, isErr := ag.handleRead(map[string]any{
		"path":  path,
		"limit": 2,
	})
	if isErr {
		t.Fatalf("handleRead returned error: %s", got)
	}
	if !strings.Contains(got, "... (3 more lines)") {
		t.Fatalf("expected remaining line count, got %q", got)
	}
}

func TestHandleFind_ShellWildcards(t *testing.T) {
	tmpDir := t.TempDir()
	for _, name := range []string{"main.go", "mainxgo", "file1.txt", "file12.txt"} {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ag := New(nil, nil, 0)
	ag.SetWorkDir(tmpDir)

	got, isErr := ag.handleFind(context.Background(), map[string]any{"name": "*.go"})
	if isErr {
		t.Fatalf("handleFind returned error: %s", got)
	}
	if !strings.Contains(got, "main.go") {
		t.Fatalf("expected literal dot match, got %q", got)
	}
	if strings.Contains(got, "mainxgo") {
		t.Fatalf("pattern should not treat '.' as wildcard, got %q", got)
	}

	got, isErr = ag.handleFind(context.Background(), map[string]any{"name": "file?.txt"})
	if isErr {
		t.Fatalf("handleFind returned error: %s", got)
	}
	if !strings.Contains(got, "file1.txt") {
		t.Fatalf("expected single-character wildcard match, got %q", got)
	}
	if strings.Contains(got, "file12.txt") {
		t.Fatalf("single-character wildcard should not match multiple chars, got %q", got)
	}
}

func TestHandleGlob_RecursiveDoubleStar(t *testing.T) {
	tmpDir := t.TempDir()
	files := []string{
		filepath.Join(tmpDir, "main.go"),
		filepath.Join(tmpDir, "nested", "inner.go"),
		filepath.Join(tmpDir, "nested", "note.txt"),
	}
	for _, name := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ag := New(nil, nil, 0)
	ag.SetWorkDir(tmpDir)

	got, isErr := ag.handleGlob(context.Background(), map[string]any{"pattern": "**/*.go"})
	if isErr {
		t.Fatalf("handleGlob returned error: %s", got)
	}
	if !strings.Contains(got, "main.go") || !strings.Contains(got, "nested/inner.go") {
		t.Fatalf("expected recursive glob matches, got %q", got)
	}
	if strings.Contains(got, "note.txt") {
		t.Fatalf("unexpected non-matching file in output: %q", got)
	}
}
