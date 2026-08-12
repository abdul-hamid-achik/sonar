package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type wallCompactClient struct {
	summaryCalls int
	chatCalls    int
}

func (c *wallCompactClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.chatCalls++
	if strings.Contains(options.System, "conversation summarizer") {
		c.summaryCalls++
		return emit(llm.StreamChunk{Text: "older turns summarized", Done: true})
	}
	return emit(llm.StreamChunk{Text: "answer", PromptEvalCount: 100, EvalCount: 5, Done: true})
}

func (*wallCompactClient) Ping() error   { return nil }
func (*wallCompactClient) Model() string { return "wall-compact-test" }
func (*wallCompactClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func oversizedHistory() []llm.Message {
	filler := strings.Repeat("history that must compact before the next request ", 60)
	messages := make([]llm.Message, 0, 30)
	for i := 0; i < 11; i++ {
		messages = append(messages,
			llm.Message{Role: "user", Content: filler},
			llm.Message{Role: "assistant", Content: filler},
		)
	}
	// Small trailing turns keep the retained recent window comfortably inside
	// the context budget once the older filler is summarized away.
	for i := 0; i < 3; i++ {
		messages = append(messages,
			llm.Message{Role: "user", Content: "ok"},
			llm.Message{Role: "assistant", Content: "ok"},
		)
	}
	messages = append(messages, llm.Message{Role: "user", Content: "finish up"})
	return messages
}

func TestWallLimitedTurnCompactsInsteadOfFailingClosed(t *testing.T) {
	client := &wallCompactClient{}
	ag := New(client, nil, 8_192)
	ag.SetWorkDir(t.TempDir())
	ag.ReplaceMessages(oversizedHistory())

	err := ag.RunTurnWithLimits(context.Background(), &outputRecorder{}, "turn_wall_compacts", TurnLimits{
		MaxWallTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("wall-limited turn failed instead of compacting: %v", err)
	}
	if client.summaryCalls < 1 {
		t.Fatalf("wall-limited turn never compacted (summary calls = %d)", client.summaryCalls)
	}
}

func TestEvalBudgetedTurnStillRefusesUntrackedCompaction(t *testing.T) {
	client := &wallCompactClient{}
	ag := New(client, nil, 8_192)
	ag.SetWorkDir(t.TempDir())
	ag.ReplaceMessages(oversizedHistory())

	err := ag.RunTurnWithLimits(context.Background(), &outputRecorder{}, "turn_budget_refuses", TurnLimits{
		MaxEvalTokens: 100_000,
	})
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("eval-budgeted turn error = %v, want ErrTurnContextBudgetExceeded", err)
	}
	if client.summaryCalls != 0 {
		t.Fatalf("eval-budgeted turn spent %d untracked summarization generation(s)", client.summaryCalls)
	}
}

type flakyThenScriptedClient struct {
	failures int
	inner    *scriptedClient
}

func (c *flakyThenScriptedClient) ChatStream(ctx context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if c.failures > 0 {
		c.failures--
		return io.ErrUnexpectedEOF
	}
	return c.inner.ChatStream(ctx, options, emit)
}

func (*flakyThenScriptedClient) Ping() error   { return nil }
func (*flakyThenScriptedClient) Model() string { return "flaky-test" }
func (*flakyThenScriptedClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestBoundedTurnRetriesTransientTransportAfterChargingReservation(t *testing.T) {
	client := &flakyThenScriptedClient{
		failures: 1,
		inner:    &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "recovered", Done: true, EvalCount: 5}}}},
	}
	ag := New(client, nil, 4_096)
	ag.SetWorkDir(t.TempDir())
	ag.AppendMessage(llm.Message{Role: "user", Content: "hola"})
	out := &outputRecorder{}

	err := ag.RunTurnWithLimits(context.Background(), out, "turn_bounded_retry", TurnLimits{
		MaxEvalTokens: 50_000,
	})
	if err != nil {
		t.Fatalf("bounded turn did not survive a transient transport failure: %v", err)
	}
	joined := strings.Join(out.errors, "\n")
	if !strings.Contains(joined, "retrying") || !strings.Contains(joined, "reserved token(s)") {
		t.Fatalf("bounded retry did not surface its charged reservation: %q", joined)
	}
}

func TestBoundedTurnWithoutRemainingBudgetFailsClosedOnTransport(t *testing.T) {
	client := &flakyThenScriptedClient{
		failures: 1,
		inner:    &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "unused", Done: true, EvalCount: 5}}}},
	}
	ag := New(client, nil, 4_096)
	ag.SetWorkDir(t.TempDir())
	ag.AppendMessage(llm.Message{Role: "user", Content: "hola"})

	// The per-request context reservation consumes the entire remaining
	// budget, so the charged failure leaves nothing to retry with.
	err := ag.RunTurnWithLimits(context.Background(), &outputRecorder{}, "turn_bounded_exhausted", TurnLimits{
		MaxEvalTokens: 64,
	})
	if !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("exhausted bounded turn error = %v, want ErrTurnEvalBudgetExhausted", err)
	}
	if client.failures != 0 {
		t.Fatal("provider was never dispatched")
	}
}

func TestAutoProgressCountsEffectfulSuccesses(t *testing.T) {
	progress := newAutoTurnProgress()
	progress.beginIteration(2)
	success := ecosystem.ToolProjection{
		Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainSucceeded, DomainTyped: true,
	}
	progress.settle("read", "hash-read", executionpkg.EffectReadOnly, executionpkg.EventCompleted, success)
	progress.settle("bash", "hash-bash", executionpkg.Effectful, executionpkg.EventCompleted, success)

	checkpoint := progress.checkpoint("turn", 40, 1, time.Second)
	if checkpoint == nil {
		t.Fatal("productive iteration produced no checkpoint")
		return
	}
	if checkpoint.EffectfulSuccessfulCalls != 1 || checkpoint.SuccessfulToolCalls != 2 {
		t.Fatalf("checkpoint counters = %#v", checkpoint)
	}
}
