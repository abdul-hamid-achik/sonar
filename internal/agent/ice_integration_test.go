package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/ice"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type iceIntegrationClient struct {
	chatCalls  atomic.Int64
	embedCalls atomic.Int64
}

func (c *iceIntegrationClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.chatCalls.Add(1)
	return emit(llm.StreamChunk{Text: "done", Done: true, EvalCount: 1, PromptEvalCount: 1})
}

func (*iceIntegrationClient) Ping() error   { return nil }
func (*iceIntegrationClient) Model() string { return "ice-integration" }

func (c *iceIntegrationClient) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	c.embedCalls.Add(1)
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = []float32{1}
	}
	return result, nil
}

type blockingICESessionClient struct {
	chatCalls atomic.Int64
	started   chan struct{}
	release   chan struct{}
}

func (c *blockingICESessionClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if c.chatCalls.Add(1) == 1 {
		close(c.started)
		<-c.release
	}
	return emit(llm.StreamChunk{Text: "done", Done: true, EvalCount: 1, PromptEvalCount: 1})
}

func (*blockingICESessionClient) Ping() error   { return nil }
func (*blockingICESessionClient) Model() string { return "blocking-ice-session" }

func (*blockingICESessionClient) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = []float32{1}
	}
	return result, nil
}

func newAgentICEEngine(t *testing.T, client llm.Client, workspace string) *ice.Engine {
	t.Helper()
	engine, err := ice.NewEngine(client, nil, ice.EngineConfig{
		StorePath: filepath.Join(workspace, "ice-conversations.json"),
		Workspace: workspace,
		NumCtx:    16_384,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestExecutionSessionIDBindsDurableAndFreshTransientICEScopes(t *testing.T) {
	client := &iceIntegrationClient{}
	workspace := t.TempDir()
	ag := New(client, nil, 16_384)
	ag.SetWorkDir(workspace)
	ag.SetExecutionSessionID(42, "")
	engine := newAgentICEEngine(t, client, workspace)
	ag.SetICEEngine(engine)
	ag.SetModeContext("", NewToolPolicy(nil, nil, false))
	t.Cleanup(ag.Close)

	if got := ag.ICESessionID(); got != "db:42" {
		t.Fatalf("desired ICE status scope = %q, want db:42", got)
	}
	ag.AddUserMessage("bind the first durable ICE scope")
	if err := ag.RunTurnWithLimits(context.Background(), &outputRecorder{}, "turn_bind_42", TurnLimits{MaxEvalTokens: 8}); err != nil {
		t.Fatal(err)
	}
	if got := engine.SessionID(); got != "db:42" {
		t.Fatalf("ICE session = %q, want db:42", got)
	}

	ag.SetExecutionSessionID(0, "")
	if got := ag.ICESessionID(); !strings.HasPrefix(got, "transient:") {
		t.Fatalf("cleared ICE status scope = %q, want transient", got)
	}
	ag.AddUserMessage("bind a fresh transient ICE scope")
	if err := ag.RunTurnWithLimits(context.Background(), &outputRecorder{}, "turn_bind_transient", TurnLimits{MaxEvalTokens: 8}); err != nil {
		t.Fatal(err)
	}
	transient := engine.SessionID()
	if !strings.HasPrefix(transient, "transient:") {
		t.Fatalf("cleared durable session reused %q, want fresh transient scope", transient)
	}
	ag.SetExecutionSessionID(0, "")
	if got := engine.SessionID(); got != transient {
		t.Fatalf("idempotent session clear changed ICE scope from %q to %q", transient, got)
	}

	ag.SetExecutionSessionID(43, "")
	ag.AddUserMessage("bind the resumed durable ICE scope")
	if err := ag.RunTurnWithLimits(context.Background(), &outputRecorder{}, "turn_bind_43", TurnLimits{MaxEvalTokens: 8}); err != nil {
		t.Fatal(err)
	}
	if got := engine.SessionID(); got != "db:43" {
		t.Fatalf("resumed ICE session = %q, want db:43", got)
	}
}

func TestRunUsesAdmittedPromptBudgetBeforeOptionalICE(t *testing.T) {
	client := &iceIntegrationClient{}
	workspace := t.TempDir()
	ag := New(client, nil, 16_384)
	ag.SetWorkDir(workspace)
	ag.SetModeContext("", NewToolPolicy(nil, nil, false))
	engine := newAgentICEEngine(t, client, workspace)
	ag.SetICEEngine(engine)
	t.Cleanup(ag.Close)
	ag.AddUserMessage(strings.Repeat("oversized prompt content ", 2_500))

	out := &outputRecorder{}
	err := ag.RunTurnWithLimits(context.Background(), out, "turn_prompt_aware_ice", TurnLimits{MaxEvalTokens: 8})
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("RunTurnWithLimits error = %v, want context-budget rejection", err)
	}
	if got := client.chatCalls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want zero before rejected prompt", got)
	}
	if got := client.embedCalls.Load(); got != 0 {
		t.Fatalf("embedding calls = %d, want no ICE work before a rejected prompt", got)
	}
	if got := engine.ScopedEntryCount(); got != 0 {
		t.Fatalf("rejected prompt left %d durable ICE entries, want zero", got)
	}
}

func TestExecutionSessionChangeDuringTurnAppliesOnlyToNextICEBoundTurn(t *testing.T) {
	client := &blockingICESessionClient{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	workspace := t.TempDir()
	ag := New(client, nil, 16_384)
	ag.SetWorkDir(workspace)
	ag.SetModeContext("", NewToolPolicy(nil, nil, false))
	ag.SetExecutionSessionID(42, "")
	engine := newAgentICEEngine(t, client, workspace)
	ag.SetICEEngine(engine)
	t.Cleanup(ag.Close)
	ag.AddUserMessage("finish this response inside session 42")

	done := make(chan error, 1)
	go func() {
		done <- ag.RunTurnWithLimits(context.Background(), &outputRecorder{}, "turn_stable_42", TurnLimits{MaxEvalTokens: 8})
	}()
	<-client.started

	ag.SetExecutionSessionID(43, "")
	if got := engine.SessionID(); got != "db:42" {
		t.Fatalf("mid-turn setter rebound ICE to %q, want immutable db:42", got)
	}
	close(client.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := engine.SessionID(); got != "db:42" {
		t.Fatalf("completed turn changed ICE scope to %q, want db:42", got)
	}

	ag.AddUserMessage("start the next response inside session 43")
	if err := ag.RunTurnWithLimits(context.Background(), &outputRecorder{}, "turn_stable_43", TurnLimits{MaxEvalTokens: 8}); err != nil {
		t.Fatal(err)
	}
	if got := engine.SessionID(); got != "db:43" {
		t.Fatalf("next turn ICE scope = %q, want db:43", got)
	}
}

func TestSessionChangeCancellationDoesNotReachUserOutput(t *testing.T) {
	out := &outputRecorder{}
	reportOptionalICEError(context.Background(), out, "indexing", ice.ErrICESessionChanged)
	if len(out.errors) != 0 {
		t.Fatalf("normal ICE session change reached user output: %#v", out.errors)
	}

	reportOptionalICEError(context.Background(), out, "indexing", errors.New("embedding unavailable"))
	if len(out.errors) != 1 || !strings.Contains(out.errors[0], "embedding unavailable") {
		t.Fatalf("real ICE failure was not reported: %#v", out.errors)
	}
}
