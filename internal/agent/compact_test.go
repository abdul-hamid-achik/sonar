package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

// mockOutput implements the Output interface for testing.
type mockOutput struct {
	texts              []string
	errors             []string
	sysMsgs            []string
	compactions        int
	compactionStarts   int
	compactionFinishes int
}

type failingCompactionClient struct{}

func (*failingCompactionClient) ChatStream(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error {
	return errors.New("summary unavailable")
}
func (*failingCompactionClient) Ping() error   { return nil }
func (*failingCompactionClient) Model() string { return "failing-compaction-test" }
func (*failingCompactionClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type contextWindowClient struct {
	numCtx        int
	numCtxCalls   int
	chatCalls     int
	summaryCalls  int
	expectedCtx   []int
	expectedModel []string
}

type durableRecoveryCompactionClient struct {
	providerMessages []llm.Message
}

type repeatedCompactionClient struct {
	summaryPrompts []llm.ChatOptions
}

func (c *repeatedCompactionClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.summaryPrompts = append(c.summaryPrompts, options)
	recap := "recap one"
	if len(c.summaryPrompts) > 1 {
		recap = "recap two"
	}
	return emit(llm.StreamChunk{Text: recap, Done: true})
}

func (*repeatedCompactionClient) Ping() error   { return nil }
func (*repeatedCompactionClient) Model() string { return "repeated-compaction-test" }
func (*repeatedCompactionClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func (c *durableRecoveryCompactionClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if strings.Contains(options.System, "conversation summarizer") {
		return emit(llm.StreamChunk{Text: "older turns summarized", Done: true})
	}
	c.providerMessages = append([]llm.Message(nil), options.Messages...)
	return emit(llm.StreamChunk{Text: "answer", PromptEvalCount: 1, EvalCount: 1, Done: true})
}

func (*durableRecoveryCompactionClient) Ping() error   { return nil }
func (*durableRecoveryCompactionClient) Model() string { return "durable-recovery-compaction-test" }
func (*durableRecoveryCompactionClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func (c *contextWindowClient) NumCtx() int {
	c.numCtxCalls++
	return c.numCtx
}

func (c *contextWindowClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.chatCalls++
	c.expectedCtx = append(c.expectedCtx, options.ExpectedContext)
	c.expectedModel = append(c.expectedModel, options.ExpectedModel)
	if strings.Contains(options.System, "conversation summarizer") {
		c.summaryCalls++
		return emit(llm.StreamChunk{Text: "older turns summarized", Done: true})
	}
	return emit(llm.StreamChunk{Text: "answer", PromptEvalCount: 20_000, EvalCount: 1, Done: true})
}

func TestCompactionPinsTurnModelIdentity(t *testing.T) {
	client := &contextWindowClient{numCtx: 4_096}
	ag := New(client, nil, 4_096)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "one"}, {Role: "assistant", Content: "one"},
		{Role: "user", Content: "two"}, {Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"}, {Role: "assistant", Content: "three"},
	})
	if !ag.compactForContextAndModel(context.Background(), &mockOutput{}, 4_096, "turn-model") {
		t.Fatal("compaction failed")
	}
	if len(client.expectedModel) != 1 || client.expectedModel[0] != "turn-model" {
		t.Fatalf("compaction expected models = %#v", client.expectedModel)
	}
}

func (*contextWindowClient) Ping() error   { return nil }
func (*contextWindowClient) Model() string { return "dynamic-context-test" }
func (*contextWindowClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestRecentConversationBoundaryKeepsToolPairsIntact(t *testing.T) {
	messages := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read"}}},
		{Role: "tool", ToolCallID: "1", ToolName: "read", Content: "one"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "second"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "2", Name: "read"}}},
		{Role: "tool", ToolCallID: "2", ToolName: "read", Content: "two"},
	}
	if got := recentConversationBoundary(messages, 4); got != 4 {
		t.Fatalf("boundary = %d, want start of second user turn at 4", got)
	}
}

func TestCompactUsesSystemSummaryAndCompleteRecentTurn(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "first turn recap", Done: true}}}}
	ag := New(client, nil, 4096)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read"}}},
		{Role: "tool", ToolCallID: "1", ToolName: "read", Content: "one"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "second"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "2", Name: "read"}}},
		{Role: "tool", ToolCallID: "2", ToolName: "read", Content: "two"},
	})
	out := &mockOutput{}
	if !ag.compact(context.Background(), out) {
		t.Fatal("expected compaction")
	}
	if out.compactionStarts != 1 || out.compactionFinishes != 1 {
		t.Fatalf("compaction lifecycle = %d starts, %d finishes", out.compactionStarts, out.compactionFinishes)
	}

	got := ag.Messages()
	if got[0].Role != "system" || !got[0].HostOwned || !strings.Contains(got[0].Content, "first turn recap") {
		t.Fatalf("summary message = %#v", got[0])
	}
	if got[1].Role != "user" || got[1].Content != "second" {
		t.Fatalf("recent history did not start at complete turn: %#v", got)
	}
	if got[2].ToolCalls[0].ID != got[3].ToolCallID {
		t.Fatalf("tool call/result pair was broken: %#v", got)
	}
}

func TestRepeatedCompactionCarriesOnlyHostOwnedPriorSummary(t *testing.T) {
	client := &repeatedCompactionClient{}
	ag := New(client, nil, 4_096)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "question one"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "question two"},
		{Role: "assistant", Content: "answer two"},
		{Role: "user", Content: "question three"},
		{Role: "assistant", Content: "answer three"},
		{Role: "user", Content: "question four"},
		{Role: "assistant", Content: "answer four"},
	})
	out := &mockOutput{}
	if !ag.compactForContext(context.Background(), out, 4_096) {
		t.Fatal("first compaction failed")
	}
	ag.AppendMessage(llm.Message{Role: "system", Content: "arbitrary system text must not be summarized", HostOwned: true})
	ag.AddUserMessage("question five")
	ag.AppendMessage(llm.Message{Role: "assistant", Content: "answer five"})
	if !ag.compactForContext(context.Background(), out, 4_096) {
		t.Fatal("second compaction failed")
	}
	if len(client.summaryPrompts) != 2 {
		t.Fatalf("summary requests = %d, want 2", len(client.summaryPrompts))
	}
	second := client.summaryPrompts[1]
	if len(second.Messages) != 1 || !strings.Contains(second.Messages[0].Content, "Previous conversation summary: recap one") {
		t.Fatalf("second summary prompt lost first recap: %#v", second.Messages)
	}
	if strings.Contains(second.Messages[0].Content, "arbitrary system text") {
		t.Fatalf("second summary prompt included unrelated system text: %q", second.Messages[0].Content)
	}
	got := ag.Messages()
	if got[0].Role != "system" || !got[0].HostOwned || !strings.HasPrefix(got[0].Content, conversationSummaryPrefix+"recap two") {
		t.Fatalf("second summary projection = %#v", got[0])
	}
}

func TestCompactionDisablesReasoning(t *testing.T) {
	client := &repeatedCompactionClient{}
	ag := New(client, nil, 4_096)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "question one"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "question two"},
		{Role: "assistant", Content: "answer two"},
		{Role: "user", Content: "question three"},
		{Role: "assistant", Content: "answer three"},
		{Role: "user", Content: "question four"},
		{Role: "assistant", Content: "answer four"},
	})
	if !ag.compactForContext(context.Background(), &mockOutput{}, 4_096) {
		t.Fatal("compaction failed")
	}
	if len(client.summaryPrompts) != 1 {
		t.Fatalf("summary requests = %d", len(client.summaryPrompts))
	}
	if !client.summaryPrompts[0].DisableReasoning {
		t.Fatal("compaction paid for chain-of-thought; host-owned summaries must opt out")
	}
}

func TestRepeatedCompactionCarriesSummaryAfterPersistedRoundTrip(t *testing.T) {
	client := &repeatedCompactionClient{}
	ag := New(client, nil, 4_096)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "question one"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "question two"},
		{Role: "assistant", Content: "answer two"},
		{Role: "user", Content: "question three"},
		{Role: "assistant", Content: "answer three"},
		{Role: "user", Content: "question four"},
		{Role: "assistant", Content: "answer four"},
	})
	out := &mockOutput{}
	if !ag.compactForContext(context.Background(), out, 4_096) {
		t.Fatal("first compaction failed")
	}

	persisted, err := json.Marshal(SanitizeMessagesForPersistence(ag.Messages()))
	if err != nil {
		t.Fatal(err)
	}
	var restored []llm.Message
	if err := json.Unmarshal(persisted, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored) == 0 || restored[0].HostOwned {
		t.Fatalf("JSON round trip unexpectedly preserved host marker: %#v", restored)
	}
	ag.ReplaceMessages(restored)
	if got := ag.Messages(); len(got) == 0 || !got[0].HostOwned {
		t.Fatalf("restore did not re-authorize bounded recap: %#v", got)
	}

	ag.AddUserMessage("question five")
	ag.AppendMessage(llm.Message{Role: "assistant", Content: "answer five"})
	if !ag.compactForContext(context.Background(), out, 4_096) {
		t.Fatal("second compaction failed")
	}
	if len(client.summaryPrompts) != 2 || !strings.Contains(client.summaryPrompts[1].Messages[0].Content, "Previous conversation summary: recap one") {
		t.Fatalf("post-restore compaction lost prior recap: %#v", client.summaryPrompts)
	}
}

func TestCompactionBoundsProviderOvershootToGenerationReserve(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{
		Text: strings.Repeat("界", 2_000), Done: true,
	}}}}
	ag := New(client, nil, 1_200)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "question one"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "question two"},
		{Role: "assistant", Content: "answer two"},
		{Role: "user", Content: "question three"},
		{Role: "assistant", Content: "answer three"},
	})
	if !ag.compactForContext(context.Background(), &mockOutput{}, 1_200) {
		t.Fatal("compaction failed")
	}
	got := ag.Messages()
	if len(got) == 0 || !strings.HasPrefix(got[0].Content, conversationSummaryPrefix) {
		t.Fatalf("summary projection = %#v", got)
	}
	recap := strings.TrimPrefix(got[0].Content, conversationSummaryPrefix)
	if tokens := estimateTextPromptTokens(recap); tokens > 300 {
		t.Fatalf("provider overshoot persisted %d estimated tokens, want at most 300", tokens)
	}
}

func TestCompactionPromptReservesContextForDenseUnicode(t *testing.T) {
	client := &repeatedCompactionClient{}
	ag := New(client, nil, 1_200)
	dense := strings.Repeat("界", 2_000)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: dense},
		{Role: "assistant", Content: dense},
		{Role: "user", Content: dense},
		{Role: "assistant", Content: dense},
		{Role: "user", Content: "recent one"},
		{Role: "assistant", Content: "recent answer"},
	})
	out := &mockOutput{}
	if !ag.compactForContext(context.Background(), out, 1_200) {
		t.Fatal("dense Unicode compaction failed")
	}
	if len(client.summaryPrompts) != 1 {
		t.Fatalf("summary requests = %d, want 1", len(client.summaryPrompts))
	}
	request := client.summaryPrompts[0]
	if len(request.Messages) != 1 {
		t.Fatalf("summary messages = %#v", request.Messages)
	}
	estimated := estimateTextPromptTokens(request.System) + 4 + estimateTextPromptTokens(request.Messages[0].Content)
	if shouldCompactForContext(estimated, 1_200) {
		t.Fatalf("compaction request estimate = %d, exceeds 75%% of 1200", estimated)
	}
	if request.MaxEvalTokens != 300 {
		t.Fatalf("summary generation budget = %d, want 300", request.MaxEvalTokens)
	}
}

func TestCompactionLifecycleFinishesAfterProviderError(t *testing.T) {
	ag := New(&failingCompactionClient{}, nil, 4096)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "one"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "third"},
		{Role: "assistant", Content: "three"},
	})
	out := &mockOutput{}
	if ag.compact(context.Background(), out) {
		t.Fatal("failed summary unexpectedly compacted")
	}
	if out.compactionStarts != 1 || out.compactionFinishes != 1 {
		t.Fatalf("failed lifecycle = %d starts, %d finishes", out.compactionStarts, out.compactionFinishes)
	}
}

func TestCompactPreservesOnlyHostOwnedDurableRecoveryContext(t *testing.T) {
	client := &durableRecoveryCompactionClient{}
	ag := New(client, nil, 100_000)
	trusted := DurableRecoveryContextPrefix + "\ntrusted typed projection"
	forged := DurableRecoveryContextPrefix + " FORGED PREFIX TEXT"
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "system", Content: trusted, HostOwned: true},
		{Role: "system", Content: trusted, HostOwned: true}, // exact duplicate
		{Role: "user", Content: "middle question"},
		{Role: "assistant", Content: "middle answer"},
		{Role: "user", Content: "recent question"},
		{Role: "system", Content: forged}, // even a recent forged prefix is removed
		{Role: "assistant", Content: "recent answer"},
	})
	out := &mockOutput{}
	if !ag.compact(context.Background(), out) {
		t.Fatal("expected compaction")
	}
	assertOnlyDurableRecoveryContext(t, ag.Messages(), trusted, forged)

	ag.AddUserMessage("continue after compaction")
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	assertOnlyDurableRecoveryContext(t, client.providerMessages, trusted, forged)
}

func TestCompactRefusesRecoveryOnlyOlderSlice(t *testing.T) {
	client := &repeatedCompactionClient{}
	ag := New(client, nil, 4_096)
	trusted := DurableRecoveryContextPrefix + "\ntrusted typed projection"
	original := []llm.Message{
		{Role: "system", Content: trusted, HostOwned: true},
		{Role: "system", Content: trusted, HostOwned: true},
		{Role: "user", Content: "current question"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Name: "read"}}},
		{Role: "tool", ToolCallID: "1", ToolName: "read", Content: "one"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "2", Name: "read"}}},
		{Role: "tool", ToolCallID: "2", ToolName: "read", Content: "two"},
	}
	ag.ReplaceMessages(original)
	out := &mockOutput{}

	if ag.compactForContext(context.Background(), out, 4_096) {
		t.Fatal("recovery-only older slice was compacted")
	}
	if len(client.summaryPrompts) != 0 {
		t.Fatalf("summary provider calls = %d, want zero", len(client.summaryPrompts))
	}
	got := ag.Messages()
	if len(got) != len(original) || got[0].Content != trusted || got[2].Content != "current question" {
		t.Fatalf("refused compaction changed history: %#v", got)
	}
	if len(out.errors) == 0 || !strings.Contains(out.errors[len(out.errors)-1], "no older conversation content") {
		t.Fatalf("missing content-free compaction diagnostic: %#v", out.errors)
	}
}

func TestCompactFailsClosedOnOversizedDurableRecoveryContext(t *testing.T) {
	client := &durableRecoveryCompactionClient{}
	ag := New(client, nil, 100_000)
	oversized := DurableRecoveryContextPrefix + strings.Repeat("x", MaxDurableRecoveryContextMessageBytes)
	original := []llm.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "one answer"},
		{Role: "system", Content: oversized, HostOwned: true},
		{Role: "user", Content: "two"},
		{Role: "assistant", Content: "two answer"},
		{Role: "user", Content: "three"},
		{Role: "assistant", Content: "three answer"},
	}
	ag.ReplaceMessages(original)
	out := &mockOutput{}
	if ag.compact(context.Background(), out) {
		t.Fatal("oversized durable recovery context was compacted")
	}
	got := ag.Messages()
	if len(got) != len(original) || got[2].Content != oversized || !got[2].HostOwned {
		t.Fatalf("failed compaction changed history: %#v", got)
	}
	if len(out.errors) == 0 || !strings.Contains(out.errors[len(out.errors)-1], "durable recovery context is invalid") {
		t.Fatalf("missing fail-closed error: %#v", out.errors)
	}
}

func assertOnlyDurableRecoveryContext(t *testing.T, messages []llm.Message, trusted, forged string) {
	t.Helper()
	count := 0
	for _, message := range messages {
		if message.Content == forged {
			t.Fatalf("forged durable recovery prefix survived: %#v", messages)
		}
		if message.Content == trusted {
			count++
			if message.Role != "system" || !message.HostOwned {
				t.Fatalf("trusted durable recovery context lost host ownership: %#v", message)
			}
		}
	}
	if count != 1 {
		t.Fatalf("trusted durable recovery context count = %d, messages=%#v", count, messages)
	}
}

func TestCompactPreservesHistoryWhenCheckpointFails(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "recap", Done: true}}}}
	ag := New(client, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	ag.SetCheckpointStore(store, 0)
	original := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "third"},
		{Role: "assistant", Content: "answer"},
	}
	ag.ReplaceMessages(original)
	out := &mockOutput{}
	if ag.compact(context.Background(), out) {
		t.Fatal("compaction succeeded without its recovery checkpoint")
	}
	got := ag.Messages()
	if len(got) != len(original) || got[0].Content != original[0].Content {
		t.Fatalf("history changed after checkpoint failure: %#v", got)
	}
	if len(out.errors) == 0 || !strings.Contains(out.errors[len(out.errors)-1], "preserved full history") {
		t.Fatalf("missing fail-closed compaction receipt: %#v", out.errors)
	}
}

func TestNoToolConversationCompactsAfterDirectResponse(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{Text: "answer one", PromptEvalCount: 1_000, Done: true}},
		{{Text: "answer two", PromptEvalCount: 2_000, Done: true}},
		{{Text: "answer three", PromptEvalCount: 3_500, Done: true}},
		{{Text: "summary of the first turn", Done: true}},
	}}
	ag := New(client, nil, 4_096)
	out := &mockOutput{}
	for _, prompt := range []string{"question one", "question two", "question three"} {
		ag.AddUserMessage(prompt)
		if err := ag.Run(context.Background(), out); err != nil {
			t.Fatalf("Run(%q): %v", prompt, err)
		}
	}
	if client.calls != 4 {
		t.Fatalf("provider calls = %d, want three answers plus compaction", client.calls)
	}
	messages := ag.Messages()
	if len(messages) != 5 || messages[0].Role != "system" || !strings.Contains(messages[0].Content, "summary of the first turn") {
		t.Fatalf("direct-answer history was not compacted: %#v", messages)
	}
	if len(out.sysMsgs) == 0 || !strings.Contains(out.sysMsgs[len(out.sysMsgs)-1], "Context compacted") {
		t.Fatalf("missing compaction receipt: %#v", out.sysMsgs)
	}
	if out.compactions != 1 {
		t.Fatalf("typed compaction notifications = %d, want 1", out.compactions)
	}
}

func TestRunSnapshotsProviderContextWindowForCompaction(t *testing.T) {
	tests := []struct {
		name          string
		providerCtx   int
		configuredCtx int
		wantCompacted bool
	}{
		{
			name:          "20K prompt stays intact with 1M provider",
			providerCtx:   1_000_000,
			configuredCtx: 16_000,
		},
		{
			name:          "20K prompt compacts with 16K provider",
			providerCtx:   16_000,
			configuredCtx: 1_000_000,
			wantCompacted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &contextWindowClient{numCtx: tt.providerCtx}
			ag := New(client, nil, tt.configuredCtx)
			ag.SetModeContext("", ToolPolicy{})
			ag.ReplaceMessages([]llm.Message{
				{Role: "user", Content: "first question"},
				{Role: "assistant", Content: "first answer"},
				{Role: "user", Content: "second question"},
				{Role: "assistant", Content: "second answer"},
				{Role: "user", Content: "third question"},
				{Role: "assistant", Content: "third answer"},
				{Role: "user", Content: "current question"},
			})
			out := &mockOutput{}

			if err := ag.Run(context.Background(), out); err != nil {
				t.Fatal(err)
			}
			if got := client.summaryCalls > 0; got != tt.wantCompacted {
				t.Fatalf("compacted = %v, want %v (summary calls=%d)", got, tt.wantCompacted, client.summaryCalls)
			}
			wantCalls := 1
			if tt.wantCompacted {
				wantCalls++
			}
			if client.chatCalls != wantCalls {
				t.Fatalf("provider calls = %d, want %d", client.chatCalls, wantCalls)
			}
			if client.numCtxCalls != 1 {
				t.Fatalf("NumCtx calls = %d, want one immutable turn snapshot", client.numCtxCalls)
			}
			for request, got := range client.expectedCtx {
				if got != tt.providerCtx {
					t.Fatalf("request %d expected context = %d, want turn snapshot %d", request, got, tt.providerCtx)
				}
			}
		})
	}
}

func TestNumCtxFallsBackWhenProviderHasNoEffectiveWindow(t *testing.T) {
	client := &contextWindowClient{}
	ag := New(client, nil, 32_768)
	if got := ag.NumCtx(); got != 32_768 {
		t.Fatalf("NumCtx() = %d, want configured fallback", got)
	}
}

func (m *mockOutput) StreamText(text string)                                               { m.texts = append(m.texts, text) }
func (m *mockOutput) StreamReasoning(_ string)                                             {}
func (m *mockOutput) StreamDone(_, _ int)                                                  {}
func (m *mockOutput) ToolCallStart(_ string, _ string, _ map[string]any)                   {}
func (m *mockOutput) ToolCallResult(_ string, _ string, _ string, _ bool, _ time.Duration) {}
func (m *mockOutput) SystemMessage(msg string)                                             { m.sysMsgs = append(m.sysMsgs, msg) }
func (m *mockOutput) ContextCompacted()                                                    { m.compactions++ }
func (m *mockOutput) ContextCompactionStarted()                                            { m.compactionStarts++ }
func (m *mockOutput) ContextCompactionFinished()                                           { m.compactionFinishes++ }
func (m *mockOutput) Error(msg string)                                                     { m.errors = append(m.errors, msg) }

func TestShouldCompact(t *testing.T) {
	tests := []struct {
		name         string
		numCtx       int
		promptTokens int
		want         bool
	}{
		{"below 75%", 1000, 749, false},
		{"above 75%", 1000, 751, true},
		{"exactly 75% (strict >)", 1000, 750, false},
		{"numCtx zero", 0, 500, false},
		{"promptTokens zero", 1000, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ag := &Agent{
				numCtx:   tt.numCtx,
				registry: mcp.NewRegistry(),
			}
			got := ag.shouldCompact(tt.promptTokens)
			if got != tt.want {
				t.Errorf("shouldCompact(%d) with numCtx=%d = %v, want %v",
					tt.promptTokens, tt.numCtx, got, tt.want)
			}
		})
	}
}

func TestEstimatePromptTokensCountsMessageContentOnce(t *testing.T) {
	base := New(nil, nil, 4096)
	base.ReplaceMessages([]llm.Message{{Role: "user"}})
	withContent := New(nil, nil, 4096)
	withContent.ReplaceMessages([]llm.Message{{Role: "user", Content: strings.Repeat("x", 400)}})

	delta := withContent.estimatePromptTokens("system", nil) - base.estimatePromptTokens("system", nil)
	if delta != 100 {
		t.Fatalf("400 ASCII message bytes changed estimate by %d tokens, want 100", delta)
	}
}

func TestEstimatePromptTokensChargesDenseUnicodeAndImagePatches(t *testing.T) {
	base := New(nil, nil, 16_384)
	base.ReplaceMessages([]llm.Message{{Role: "user"}})
	withInputs := New(nil, nil, 16_384)
	withInputs.ReplaceMessages([]llm.Message{{
		Role:    "user",
		Content: strings.Repeat("界", 100),
		Images:  []llm.ImageData{{Width: 1_120, Height: 840}},
	}})

	delta := withInputs.estimatePromptTokens("system", nil) - base.estimatePromptTokens("system", nil)
	// 100 three-byte Unicode runes plus 40x30 vision patches.
	if delta != 1_500 {
		t.Fatalf("dense Unicode plus image changed estimate by %d tokens, want 1500", delta)
	}
}

func TestSummarizeMessages(t *testing.T) {
	tests := []struct {
		name     string
		msgs     []llm.Message
		contains []string
	}{
		{
			name: "user message",
			msgs: []llm.Message{
				{Role: "user", Content: "hello"},
			},
			contains: []string{"User: hello"},
		},
		{
			name: "assistant message",
			msgs: []llm.Message{
				{Role: "assistant", Content: "hi there"},
			},
			contains: []string{"Assistant: hi there"},
		},
		{
			name: "tool message",
			msgs: []llm.Message{
				{Role: "tool", Content: "result data", ToolName: "read_file"},
			},
			contains: []string{"Tool read_file result: result data"},
		},
		{
			name: "tool content truncation at 300 chars",
			msgs: []llm.Message{
				{Role: "tool", Content: strings.Repeat("x", 400), ToolName: "big_tool"},
			},
			contains: []string{"Tool big_tool result: " + strings.Repeat("x", 297) + "..."},
		},
		{
			name:     "empty slice",
			msgs:     []llm.Message{},
			contains: []string{"Summarize this conversation:"},
		},
		{
			name: "assistant with tool calls",
			msgs: []llm.Message{
				{
					Role: "assistant",
					ToolCalls: []llm.ToolCall{
						{Name: "search", Arguments: map[string]any{"q": "test"}},
					},
				},
			},
			contains: []string{"Assistant called tool search("},
		},
		{
			name: "mixed messages",
			msgs: []llm.Message{
				{Role: "user", Content: "find files"},
				{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
					{Name: "glob", Arguments: map[string]any{"pattern": "*.go"}},
				}},
				{Role: "tool", Content: "file1.go\nfile2.go", ToolName: "glob"},
				{Role: "assistant", Content: "Found 2 files"},
			},
			contains: []string{
				"User: find files",
				"Assistant called tool glob(",
				"Tool glob result:",
				"Assistant: Found 2 files",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := summarizeMessages(tt.msgs)
			for _, want := range tt.contains {
				if !strings.Contains(result, want) {
					t.Errorf("summarizeMessages() missing %q in:\n%s", want, result)
				}
			}
		})
	}
}
