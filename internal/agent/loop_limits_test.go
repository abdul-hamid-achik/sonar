package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/ice"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

type limitedTurnClient struct {
	limit int
	calls atomic.Int64
	block bool
}

func (c *limitedTurnClient) ChatStream(ctx context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.calls.Add(1)
	c.limit = options.MaxEvalTokens
	if c.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return emit(llm.StreamChunk{
		Done: true, EvalCount: options.MaxEvalTokens,
		ToolCalls: []llm.ToolCall{{ID: "must-not-dispatch", Name: "write", Arguments: map[string]any{"path": "nope"}}},
	})
}

func (*limitedTurnClient) Ping() error   { return nil }
func (*limitedTurnClient) Model() string { return "limited-test" }
func (*limitedTurnClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type contextReserveClient struct {
	options llm.ChatOptions
}

func (c *contextReserveClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.options = options
	return emit(llm.StreamChunk{Text: "ok", Done: true, EvalCount: 1, PromptEvalCount: 100})
}

func (*contextReserveClient) Ping() error   { return nil }
func (*contextReserveClient) Model() string { return "context-reserve-test" }
func (*contextReserveClient) NumCtx() int   { return 4_096 }
func (*contextReserveClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type limitOutput struct {
	toolStarts atomic.Int64
	evalTokens atomic.Int64
}

func (*limitOutput) StreamText(string)                                          {}
func (*limitOutput) StreamReasoning(string)                                     {}
func (o *limitOutput) StreamDone(evalTokens, _ int)                             { o.evalTokens.Add(int64(evalTokens)) }
func (o *limitOutput) ToolCallStart(string, string, map[string]any)             { o.toolStarts.Add(1) }
func (*limitOutput) ToolCallResult(string, string, string, bool, time.Duration) {}
func (*limitOutput) SystemMessage(string)                                       {}
func (*limitOutput) Error(string)                                               {}

type expertProgressOutput struct {
	limitOutput
	callIDs []string
	events  []expertteam.ProgressEvent
}

func (output *expertProgressOutput) ExpertProgress(callID string, event expertteam.ProgressEvent) {
	output.callIDs = append(output.callIDs, callID)
	output.events = append(output.events, event)
}

type contextBudgetOutput struct {
	limitOutput
	errors []string
}

func (o *contextBudgetOutput) Error(message string) {
	o.errors = append(o.errors, message)
}

type partialLimitedClient struct {
	limit int
	calls atomic.Int64
}

func (c *partialLimitedClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.calls.Add(1)
	c.limit = options.MaxEvalTokens
	if err := emit(llm.StreamChunk{Text: "partial response without a terminal receipt"}); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func (*partialLimitedClient) Ping() error   { return nil }
func (*partialLimitedClient) Model() string { return "partial-limited-test" }
func (*partialLimitedClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type rejectedLimitedClient struct{}

func (*rejectedLimitedClient) ChatStream(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error {
	return llm.ErrNoModelSelected
}

func (*rejectedLimitedClient) Ping() error   { return llm.ErrNoModelSelected }
func (*rejectedLimitedClient) Model() string { return "" }
func (*rejectedLimitedClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, llm.ErrNoModelSelected
}

type callbackThenNoModelClient struct{}

func (*callbackThenNoModelClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if err := emit(llm.StreamChunk{Reasoning: "provider entered the stream"}); err != nil {
		return err
	}
	return llm.ErrNoModelSelected
}

func (*callbackThenNoModelClient) Ping() error   { return nil }
func (*callbackThenNoModelClient) Model() string { return "callback-no-model-test" }
func (*callbackThenNoModelClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type overflowingParentReceiptClient struct {
	calls       atomic.Int64
	secondLimit atomic.Int64
}

func (client *overflowingParentReceiptClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if client.calls.Add(1) == 1 {
		return emit(llm.StreamChunk{
			Done: true, EvalCount: 2,
			ToolCalls: []llm.ToolCall{{ID: "list-before-overflow", Name: "ls", Arguments: map[string]any{}}},
		})
	}
	client.secondLimit.Store(int64(options.MaxEvalTokens))
	return emit(llm.StreamChunk{Done: true, EvalCount: int(^uint(0) >> 1)})
}

func (*overflowingParentReceiptClient) Ping() error   { return nil }
func (*overflowingParentReceiptClient) Model() string { return "overflowing-parent-receipt-test" }
func (*overflowingParentReceiptClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type boundedSideGenerationClient struct {
	calls         atomic.Int64
	uncappedCalls atomic.Int64
}

func (c *boundedSideGenerationClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.calls.Add(1)
	if options.MaxEvalTokens == 0 {
		c.uncappedCalls.Add(1)
	}
	return emit(llm.StreamChunk{
		Text:      "A sufficiently long direct response that would normally qualify for automatic memory extraction after this turn.",
		Done:      true,
		EvalCount: 1,
	})
}

func (*boundedSideGenerationClient) Ping() error   { return nil }
func (*boundedSideGenerationClient) Model() string { return "bounded-side-generation-test" }
func (*boundedSideGenerationClient) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = []float32{1}
	}
	return result, nil
}

type boundedToolResultClient struct {
	calls atomic.Int64
}

func (c *boundedToolResultClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	call := c.calls.Add(1)
	if call == 1 {
		return emit(llm.StreamChunk{
			Done: true, EvalCount: 1,
			ToolCalls: []llm.ToolCall{{
				ID: "expand-result", Name: "exists", Arguments: map[string]any{"path": "."},
			}},
		})
	}
	return emit(llm.StreamChunk{Text: "must not be requested", Done: true, EvalCount: 1})
}

func (*boundedToolResultClient) Ping() error   { return nil }
func (*boundedToolResultClient) Model() string { return "bounded-tool-result-test" }
func (*boundedToolResultClient) NumCtx() int   { return 1_200 }
func (*boundedToolResultClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type aggregateToolResultClient struct {
	calls atomic.Int64
}

func (c *aggregateToolResultClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	call := c.calls.Add(1)
	if call > 1 {
		return emit(llm.StreamChunk{Text: "must not be requested", Done: true, EvalCount: 1})
	}
	toolCalls := make([]llm.ToolCall, 4)
	for index := range toolCalls {
		toolCalls[index] = llm.ToolCall{
			ID: fmt.Sprintf("expand-result-%d", index), Name: "exists", Arguments: map[string]any{"path": "."},
		}
	}
	return emit(llm.StreamChunk{Done: true, EvalCount: 1, ToolCalls: toolCalls})
}

func (*aggregateToolResultClient) Ping() error   { return nil }
func (*aggregateToolResultClient) Model() string { return "aggregate-tool-result-test" }
func (*aggregateToolResultClient) NumCtx() int   { return 1_200 }
func (*aggregateToolResultClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type authoritativePromptReceiptClient struct {
	calls atomic.Int64
}

func (c *authoritativePromptReceiptClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if call := c.calls.Add(1); call != 1 {
		return fmt.Errorf("unexpected provider request %d", call)
	}
	return emit(llm.StreamChunk{
		Done: true, EvalCount: 1, PromptEvalCount: 3_001,
		ToolCalls: []llm.ToolCall{{ID: "receipt-floor", Name: "exists", Arguments: map[string]any{"path": "."}}},
	})
}

func (*authoritativePromptReceiptClient) Ping() error   { return nil }
func (*authoritativePromptReceiptClient) Model() string { return "authoritative-prompt-receipt-test" }
func (*authoritativePromptReceiptClient) NumCtx() int   { return 4_000 }
func (*authoritativePromptReceiptClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type crossTurnPromptFloorClient struct {
	calls atomic.Int64
}

func (c *crossTurnPromptFloorClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if call := c.calls.Add(1); call != 1 {
		return fmt.Errorf("unexpected provider request %d", call)
	}
	return emit(llm.StreamChunk{
		Text: strings.Repeat("a", 100), Done: true, EvalCount: 25, PromptEvalCount: 2_950,
	})
}

func (*crossTurnPromptFloorClient) Ping() error   { return nil }
func (*crossTurnPromptFloorClient) Model() string { return "cross-turn-prompt-floor-test" }
func (*crossTurnPromptFloorClient) NumCtx() int   { return 4_000 }
func (*crossTurnPromptFloorClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type expandToolResultHook struct{}

func (*expandToolResultHook) Name() string { return "expand-tool-result" }
func (*expandToolResultHook) PreToolUse(context.Context, *llm.ToolCall) (bool, string) {
	return false, ""
}
func (*expandToolResultHook) PostToolUse(_ context.Context, _ llm.ToolCall, result *string, _ bool) {
	*result = strings.Repeat("tool-output ", 8_192)
}

type joinedAutoMemoryClient struct {
	autoStarted chan struct{}
	autoStopped chan struct{}
	mainCalls   atomic.Int64
}

type expertBudgetTurnClient struct {
	calls  atomic.Int64
	limits []int
	repeat bool
}

type expertCorrectionTurnClient struct {
	calls atomic.Int64
}

type cancellationReceiptExpertConsultant struct {
	started chan struct{}
	calls   atomic.Int64
}

func (consultant *cancellationReceiptExpertConsultant) Consult(ctx context.Context, request expertteam.Request) (expertteam.Result, error) {
	consultant.calls.Add(1)
	close(consultant.started)
	<-ctx.Done()
	return expertteam.Result{
		Strategy: request.Strategy,
		Experts: []expertteam.ExpertReceipt{{
			Name: "cancelled", Status: expertteam.ExpertFailed, ErrorCode: "cancelled",
			ChargedEvalTokens: request.MaxTotalEvalTokens, UsageEstimated: true,
		}},
	}, ctx.Err()
}

type postTerminalTurnClient struct{}

func (*postTerminalTurnClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if err := emit(llm.StreamChunk{Done: true, EvalCount: 1, PromptEvalCount: 1}); err != nil {
		return err
	}
	return emit(llm.StreamChunk{Text: "late uncharged parent text"})
}

func (*postTerminalTurnClient) Ping() error   { return nil }
func (*postTerminalTurnClient) Model() string { return "post-terminal-test" }
func (*postTerminalTurnClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type reasoningOnlyTerminalClient struct {
	calls atomic.Int64
}

func (c *reasoningOnlyTerminalClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.calls.Add(1)
	if err := emit(llm.StreamChunk{Reasoning: "I should answer, but emit no visible content."}); err != nil {
		return err
	}
	return emit(llm.StreamChunk{Done: true, EvalCount: 63, PromptEvalCount: 7})
}

func (*reasoningOnlyTerminalClient) Ping() error   { return nil }
func (*reasoningOnlyTerminalClient) Model() string { return "reasoning-only-terminal-test" }
func (*reasoningOnlyTerminalClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type emptyTerminalRepairClient struct {
	calls          atomic.Int64
	emptyResponses int
	systems        []string
}

func (c *emptyTerminalRepairClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	call := int(c.calls.Add(1))
	c.systems = append(c.systems, options.System)
	if call == 1 {
		return emit(llm.StreamChunk{
			Done: true, EvalCount: 1, PromptEvalCount: 10,
			ToolCalls: []llm.ToolCall{{
				ID: "repair-source", Name: "exists", Arguments: map[string]any{"path": "."},
			}},
		})
	}
	if call <= c.emptyResponses+1 {
		if err := emit(llm.StreamChunk{Reasoning: "The tool result is clear, but no answer was emitted."}); err != nil {
			return err
		}
		return emit(llm.StreamChunk{Done: true, EvalCount: call, PromptEvalCount: 20 + call})
	}
	return emit(llm.StreamChunk{Text: "The workspace exists.", Done: true, EvalCount: call, PromptEvalCount: 20 + call})
}

func (*emptyTerminalRepairClient) Ping() error   { return nil }
func (*emptyTerminalRepairClient) Model() string { return "empty-terminal-repair-test" }
func (*emptyTerminalRepairClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type textCountingOutput struct {
	limitOutput
	textChunks atomic.Int64
}

func (output *textCountingOutput) StreamText(string) { output.textChunks.Add(1) }

func (c *expertBudgetTurnClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	call := c.calls.Add(1)
	c.limits = append(c.limits, options.MaxEvalTokens)
	if call == 1 || (c.repeat && call == 2) {
		return emit(llm.StreamChunk{
			Done: true, EvalCount: 2,
			ToolCalls: []llm.ToolCall{{
				ID: "expert-consult", Name: "consult_experts", Arguments: map[string]any{
					"strategy": "team", "objective": "Review the bounded integration.",
				},
			}},
		})
	}
	return emit(llm.StreamChunk{Text: "synthesis", Done: true, EvalCount: options.MaxEvalTokens})
}

func (c *expertCorrectionTurnClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	switch call := c.calls.Add(1); call {
	case 1:
		return emit(llm.StreamChunk{Done: true, EvalCount: 1, ToolCalls: []llm.ToolCall{{
			ID: "invented-experts", Name: "consult_experts", Arguments: map[string]any{
				"strategy": "swarm", "objective": "Compare game engines.",
				"experts": []any{"Game Engine Architect", "Networking Engineer"},
			},
		}}})
	case 2:
		return emit(llm.StreamChunk{Done: true, EvalCount: 1, ToolCalls: []llm.ToolCall{{
			ID: "automatic-experts", Name: "consult_experts", Arguments: map[string]any{
				"strategy": "swarm", "objective": "Compare game engines.",
			},
		}}})
	default:
		return emit(llm.StreamChunk{Text: "synthesis", Done: true, EvalCount: 1})
	}
}

func (*expertCorrectionTurnClient) Ping() error   { return nil }
func (*expertCorrectionTurnClient) Model() string { return "expert-correction-test" }
func (*expertCorrectionTurnClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func (*expertBudgetTurnClient) Ping() error   { return nil }
func (*expertBudgetTurnClient) Model() string { return "expert-budget-test" }
func (*expertBudgetTurnClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func (c *joinedAutoMemoryClient) ChatStream(ctx context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if options.MaxEvalTokens == 0 {
		close(c.autoStarted)
		<-ctx.Done()
		close(c.autoStopped)
		return ctx.Err()
	}
	c.mainCalls.Add(1)
	select {
	case <-c.autoStopped:
	default:
		return errors.New("bounded main generation overlapped auto-memory")
	}
	return emit(llm.StreamChunk{Text: "bounded response", Done: true, EvalCount: 1})
}

func (*joinedAutoMemoryClient) Ping() error   { return nil }
func (*joinedAutoMemoryClient) Model() string { return "joined-auto-memory-test" }
func (*joinedAutoMemoryClient) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = []float32{1}
	}
	return result, nil
}

func TestRunTurnWithLimitsCapsProviderAndStopsBeforeToolDispatch(t *testing.T) {
	client := &limitedTurnClient{}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	output := &limitOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_limited", TurnLimits{MaxEvalTokens: 5})
	if !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	if client.limit != 5 || client.calls.Load() != 1 {
		t.Fatalf("provider limit=%d calls=%d", client.limit, client.calls.Load())
	}
	if output.toolStarts.Load() != 0 {
		t.Fatalf("hard token boundary dispatched %d tools", output.toolStarts.Load())
	}
}

func TestRunTurnWithLimitsCapsProviderRequestToContextReserve(t *testing.T) {
	client := &contextReserveClient{}
	ag := New(client, nil, 4_096)
	ag.SetWorkDir(t.TempDir())
	ag.SetModeContext("", NewToolPolicy(nil, nil, false))
	ag.AddUserMessage("Answer briefly.")

	if err := ag.RunTurnWithLimits(context.Background(), &limitOutput{}, "turn_context_reserve", TurnLimits{MaxEvalTokens: 12_000}); err != nil {
		t.Fatal(err)
	}
	if got := client.options.MaxEvalTokens; got != 1_024 {
		t.Fatalf("provider MaxEvalTokens = %d, want 25%% context reserve 1024", got)
	}
	if client.options.ExpectedContext != 4_096 || client.options.ExpectedModel != client.Model() {
		t.Fatalf("request admission pins = context %d model %q", client.options.ExpectedContext, client.options.ExpectedModel)
	}
}

func TestRunTurnWithLimitsChargesExpertChildrenToParentBudget(t *testing.T) {
	client := &expertBudgetTurnClient{}
	consultant := &fakeExpertConsultant{result: expertteam.Result{
		Strategy: expertselector.StrategyTeam,
		Experts: []expertteam.ExpertReceipt{{
			Name: "critic", Model: "qwen:2b", Status: expertteam.ExpertCompleted,
			Report: "bounded finding", EvalTokens: 3, PromptEvalTokens: 4, ChargedEvalTokens: 3,
		}},
	}, events: []expertteam.ProgressEvent{{Sequence: 1, Phase: expertteam.ProgressPlanned, Total: 1, Queued: 1}}}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.SetExpertConsultant(consultant)
	agent.AddUserMessage("Consult an expert, then synthesize within the goal budget.")
	output := &expertProgressOutput{}

	if err := agent.RunTurnWithLimits(context.Background(), output, "turn_expert_budget", TurnLimits{MaxEvalTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if consultant.request.MaxTotalEvalTokens != 8 {
		t.Fatalf("expert shared cap=%d, want remaining 8", consultant.request.MaxTotalEvalTokens)
	}
	if got := output.evalTokens.Load(); got != 10 {
		t.Fatalf("parent charged usage=%d, want 2 parent + 3 expert + 5 synthesis", got)
	}
	if len(client.limits) != 2 || client.limits[0] != 10 || client.limits[1] != 5 {
		t.Fatalf("parent request limits=%v", client.limits)
	}
	if len(output.callIDs) != 1 || output.callIDs[0] != "expert-consult" || len(output.events) != 1 || output.events[0].Phase != expertteam.ProgressPlanned {
		t.Fatalf("correlated expert progress callIDs=%v events=%#v", output.callIDs, output.events)
	}
}

func TestRunTurnWithLimitsPreservesCancellationWhenExpertReservationExhaustsBudget(t *testing.T) {
	client := &expertBudgetTurnClient{}
	consultant := &cancellationReceiptExpertConsultant{started: make(chan struct{})}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.SetExpertConsultant(consultant)
	agent.AddUserMessage("Cancel while the bounded expert consultation is running.")
	output := &limitOutput{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-consultant.started
		cancel()
	}()

	err := agent.RunTurnWithLimits(ctx, output, "turn_cancelled_expert_budget", TurnLimits{MaxEvalTokens: 10})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error=%v, want joined cancellation and budget exhaustion", err)
	}
	if got := output.evalTokens.Load(); got != 10 {
		t.Fatalf("cancelled parent charge=%d, want 2 parent + 8 expert reservation", got)
	}
	if calls := consultant.calls.Load(); calls != 1 {
		t.Fatalf("expert dispatches=%d, want 1", calls)
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("parent calls=%d, want no synthesis after cancellation", calls)
	}
}

func TestRunTurnDispatchesAtMostOneExpertConsultation(t *testing.T) {
	client := &expertBudgetTurnClient{repeat: true}
	consultant := &fakeExpertConsultant{result: expertteam.Result{
		Strategy: expertselector.StrategyTeam,
		Experts: []expertteam.ExpertReceipt{{
			Name: "critic", Model: "qwen:2b", Status: expertteam.ExpertCompleted, Report: "one consultation",
			EvalTokens: 1, ChargedEvalTokens: 1,
		}},
	}}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.SetExpertConsultant(consultant)
	agent.AddUserMessage("Try consulting twice, then answer.")

	if err := agent.RunTurn(context.Background(), &limitOutput{}, "turn_one_expert_consult"); err != nil {
		t.Fatal(err)
	}
	if consultant.calls != 1 {
		t.Fatalf("expert runtime dispatches=%d, want exactly one", consultant.calls)
	}
}

func TestRunTurnCanCorrectInventedExpertProfilesWithoutConsumingDispatch(t *testing.T) {
	client := &expertCorrectionTurnClient{}
	consultant := &fakeExpertConsultant{
		result: expertteam.Result{
			Strategy: expertselector.StrategySwarm,
			Experts: []expertteam.ExpertReceipt{{
				Name: "architect", Model: "qwen:2b", Status: expertteam.ExpertCompleted,
				Report: "bounded finding", EvalTokens: 1, ChargedEvalTokens: 1,
			}},
		},
		validate: func(request expertteam.Request) error {
			if len(request.ExpertNames) > 0 {
				return errors.Join(expertteam.ErrInvalidRequest, expertselector.ErrUnknownExplicitProfile)
			}
			return nil
		},
	}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.SetExpertConsultant(consultant)
	agent.AddUserMessage("Use a swarm to compare game engines.")

	if err := agent.RunTurn(context.Background(), &limitOutput{}, "turn_correct_expert_catalog"); err != nil {
		t.Fatal(err)
	}
	if consultant.calls != 1 {
		t.Fatalf("expert runtime dispatches=%d, want only the corrected request", consultant.calls)
	}
	if got := client.calls.Load(); got != 3 {
		t.Fatalf("parent iterations=%d, want invalid correction + consultation + synthesis", got)
	}
}

func TestRunTurnWithLimitsChargesReservationForInvalidExpertUsage(t *testing.T) {
	client := &expertBudgetTurnClient{}
	consultant := &fakeExpertConsultant{result: expertteam.Result{
		Strategy: expertselector.StrategyTeam,
		Experts: []expertteam.ExpertReceipt{{
			Name: "invalid", Status: expertteam.ExpertFailed, ChargedEvalTokens: -1,
		}},
	}}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.SetExpertConsultant(consultant)
	agent.AddUserMessage("Consult the invalid fake runtime.")
	output := &limitOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_invalid_expert_usage", TurnLimits{MaxEvalTokens: 10})
	if !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error=%v, want budget exhaustion", err)
	}
	if got := output.evalTokens.Load(); got != 10 {
		t.Fatalf("conservative parent charge=%d, want full 10", got)
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("parent calls=%d, want no synthesis after invalid usage", calls)
	}
}

func TestRunTurnValidatesExpertUsageBeforeEmittingIt(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("64-bit overflow boundary")
	}
	client := &expertBudgetTurnClient{}
	maxInt := int(^uint(0) >> 1)
	consultant := &fakeExpertConsultant{result: expertteam.Result{
		Strategy: expertselector.StrategyTeam,
		Experts: []expertteam.ExpertReceipt{{
			Name: "overflow", Status: expertteam.ExpertCompleted, Report: "invalid huge receipt",
			EvalTokens: maxInt, ChargedEvalTokens: maxInt,
		}},
	}}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.SetExpertConsultant(consultant)
	agent.AddUserMessage("Exercise an overflowing custom usage receipt.")
	output := &limitOutput{}
	maxEval := int64(^uint64(0) >> 1)

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_expert_usage_overflow", TurnLimits{MaxEvalTokens: maxEval})
	if !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error=%v, want budget exhaustion", err)
	}
	if got := output.evalTokens.Load(); got != maxEval {
		t.Fatalf("validated conservative charge=%d, want %d", got, maxEval)
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("parent calls=%d, want no synthesis after overflow", calls)
	}
}

func TestRunTurnValidatesParentUsageBeforeEmittingIt(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("64-bit overflow boundary")
	}
	client := &overflowingParentReceiptClient{}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.AddUserMessage("Exercise an overflowing parent usage receipt after one tool iteration.")
	output := &limitOutput{}
	maxEval := int64(^uint64(0) >> 1)

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_parent_usage_overflow", TurnLimits{MaxEvalTokens: maxEval})
	if !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error=%v, want conservative budget exhaustion", err)
	}
	secondLimit := client.secondLimit.Load()
	if secondLimit <= 0 || secondLimit > 4096/4 {
		t.Fatalf("second request limit=%d, want a positive context-reserved cap <= %d", secondLimit, 4096/4)
	}
	if got, want := output.evalTokens.Load(), int64(2)+secondLimit; got != want {
		t.Fatalf("validated conservative charge=%d, want first receipt plus second reservation=%d", got, want)
	}
	if calls := client.calls.Load(); calls != 2 {
		t.Fatalf("parent calls=%d, want overflow on second request", calls)
	}
}

func TestRunTurnRejectsChunksAfterTerminalReceipt(t *testing.T) {
	agent := New(&postTerminalTurnClient{}, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.AddUserMessage("Reject text after the terminal receipt.")
	output := &textCountingOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_post_terminal", TurnLimits{MaxEvalTokens: 5})
	if !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error=%v, want conservative budget exhaustion", err)
	}
	if output.evalTokens.Load() != 5 {
		t.Fatalf("post-terminal charge=%d, want reservation 5", output.evalTokens.Load())
	}
	if output.textChunks.Load() != 0 {
		t.Fatalf("accepted %d post-terminal text chunks", output.textChunks.Load())
	}
}

func TestRunTurnRejectsReasoningOnlyTerminalResponse(t *testing.T) {
	client := &reasoningOnlyTerminalClient{}
	agent := New(client, nil, 16_384)
	agent.SetWorkDir(t.TempDir())
	agent.AddUserMessage("Return a visible answer.")
	output := &contextBudgetOutput{}

	err := agent.RunTurn(context.Background(), output, "turn_reasoning_only")
	if !errors.Is(err, ErrEmptyTerminalResponse) {
		t.Fatalf("error = %v, want ErrEmptyTerminalResponse", err)
	}
	if got := output.evalTokens.Load(); got != 63 {
		t.Fatalf("charged evaluation tokens = %d, want 63", got)
	}
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want no retry without a preceding tool result", got)
	}
	if len(output.errors) != 1 || !strings.Contains(output.errors[0], "without visible text or a tool call") {
		t.Fatalf("output errors = %q", output.errors)
	}
	for _, message := range agent.Messages() {
		if message.Role == "assistant" {
			t.Fatalf("reasoning-only completion persisted an empty assistant message: %#v", message)
		}
	}
}

func TestRunTurnRepairsOneEmptyTerminalResponseAfterToolResult(t *testing.T) {
	client := &emptyTerminalRepairClient{emptyResponses: 1}
	agent := New(client, nil, 16_384)
	agent.SetWorkDir(t.TempDir())
	agent.SetModeContext("test", NewToolPolicy([]string{"exists"}, nil, false))
	agent.AddUserMessage("Check whether the workspace exists, then answer visibly.")
	output := &contextBudgetOutput{}

	if err := agent.RunTurn(context.Background(), output, "turn_empty_terminal_repair"); err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if got := client.calls.Load(); got != 3 {
		t.Fatalf("provider calls = %d, want tool, empty terminal, and one repair", got)
	}
	if got := output.evalTokens.Load(); got != 6 {
		t.Fatalf("charged evaluation tokens = %d, want all three receipts", got)
	}
	if len(output.errors) != 0 {
		t.Fatalf("output errors = %q", output.errors)
	}
	if len(client.systems) != 3 || strings.Contains(client.systems[0], emptyTerminalRepairPrompt) ||
		strings.Contains(client.systems[1], emptyTerminalRepairPrompt) ||
		!strings.Contains(client.systems[2], emptyTerminalRepairPrompt) {
		t.Fatalf("repair prompt scope = %#v", client.systems)
	}

	messages := agent.Messages()
	if got := messages[len(messages)-1]; got.Role != "assistant" || got.Content != "The workspace exists." {
		t.Fatalf("terminal message = %#v", got)
	}
	assertNoDurableEmptyTerminalAssistant(t, messages)
}

func TestRunTurnStopsAfterOneEmptyTerminalRepair(t *testing.T) {
	client := &emptyTerminalRepairClient{emptyResponses: 2}
	agent := New(client, nil, 16_384)
	agent.SetWorkDir(t.TempDir())
	agent.SetModeContext("test", NewToolPolicy([]string{"exists"}, nil, false))
	agent.AddUserMessage("Check whether the workspace exists, then answer visibly.")
	output := &contextBudgetOutput{}

	err := agent.RunTurn(context.Background(), output, "turn_empty_terminal_repair_once")
	if !errors.Is(err, ErrEmptyTerminalResponse) {
		t.Fatalf("error = %v, want ErrEmptyTerminalResponse", err)
	}
	if got := client.calls.Load(); got != 3 {
		t.Fatalf("provider calls = %d, want exactly one request after the first empty terminal response", got)
	}
	if got := output.evalTokens.Load(); got != 6 {
		t.Fatalf("charged evaluation tokens = %d, want all three receipts", got)
	}
	if len(output.errors) != 1 || !strings.Contains(output.errors[0], "without visible text or a tool call") {
		t.Fatalf("output errors = %q", output.errors)
	}
	if len(client.systems) != 3 || !strings.Contains(client.systems[2], emptyTerminalRepairPrompt) {
		t.Fatalf("repair prompt scope = %#v", client.systems)
	}
	assertNoDurableEmptyTerminalAssistant(t, agent.Messages())
}

func TestRunTurnDoesNotRepairPastEvaluationBudget(t *testing.T) {
	client := &emptyTerminalRepairClient{emptyResponses: 1}
	agent := New(client, nil, 16_384)
	agent.SetWorkDir(t.TempDir())
	agent.SetModeContext("test", NewToolPolicy([]string{"exists"}, nil, false))
	agent.AddUserMessage("Check whether the workspace exists, then answer visibly.")
	output := &contextBudgetOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_empty_terminal_repair_budget", TurnLimits{MaxEvalTokens: 3})
	if !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error = %v, want ErrTurnEvalBudgetExhausted", err)
	}
	if got := client.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want no repair request past the evaluation budget", got)
	}
	if got := output.evalTokens.Load(); got != 3 {
		t.Fatalf("charged evaluation tokens = %d, want the full budget", got)
	}
	if len(client.systems) != 2 || strings.Contains(client.systems[1], emptyTerminalRepairPrompt) {
		t.Fatalf("unexpected repair request systems = %#v", client.systems)
	}
	assertNoDurableEmptyTerminalAssistant(t, agent.Messages())
}

func assertNoDurableEmptyTerminalAssistant(t *testing.T, messages []llm.Message) {
	t.Helper()
	for _, message := range messages {
		if message.Role == "assistant" && strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
			t.Fatalf("empty terminal assistant response was persisted: %#v", message)
		}
	}
}

func TestRunTurnWithLimitsAppliesWallDeadline(t *testing.T) {
	client := &limitedTurnClient{block: true}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	started := time.Now()

	err := agent.RunTurnWithLimits(context.Background(), &limitOutput{}, "turn_deadline", TurnLimits{MaxWallTime: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("wall deadline took %s", elapsed)
	}
}

func TestRunTurnWithLimitsUsesAbsoluteDeadlineWithoutRebasing(t *testing.T) {
	client := &limitedTurnClient{block: true}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())

	err := agent.RunTurnWithLimits(context.Background(), &limitOutput{}, "turn_absolute_deadline", TurnLimits{
		Deadline: time.Now().Add(-time.Second),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if client.calls.Load() != 0 {
		t.Fatalf("expired absolute deadline reached provider %d time(s)", client.calls.Load())
	}
}

func TestRunTurnWithLimitsChargesReservationWhenTerminalUsageIsUnknown(t *testing.T) {
	client := &partialLimitedClient{}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	output := &limitOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_partial_receipt", TurnLimits{MaxEvalTokens: 7})
	if !errors.Is(err, ErrTurnEvalBudgetExhausted) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v", err)
	}
	if client.limit != 7 || client.calls.Load() != 1 {
		t.Fatalf("provider limit=%d calls=%d", client.limit, client.calls.Load())
	}
	if output.evalTokens.Load() != 7 {
		t.Fatalf("conservative token charge=%d, want full 7-token reservation", output.evalTokens.Load())
	}
	if output.toolStarts.Load() != 0 {
		t.Fatalf("partial stream dispatched %d tools", output.toolStarts.Load())
	}
}

func TestRunTurnWithLimitsDoesNotChargeKnownLocalPreflightRejection(t *testing.T) {
	agent := New(&rejectedLimitedClient{}, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	output := &limitOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_local_rejection", TurnLimits{MaxEvalTokens: 7})
	if !errors.Is(err, llm.ErrNoModelSelected) || errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	if output.evalTokens.Load() != 0 {
		t.Fatalf("local preflight rejection charged %d token(s)", output.evalTokens.Load())
	}
	if output.toolStarts.Load() != 0 {
		t.Fatalf("local preflight rejection dispatched %d tools", output.toolStarts.Load())
	}
}

func TestRunTurnWithLimitsChargesReservationWhenNoModelErrorFollowsCallback(t *testing.T) {
	agent := New(&callbackThenNoModelClient{}, nil, 4096)
	agent.SetWorkDir(t.TempDir())
	agent.AddUserMessage("Enter the provider stream before returning no model.")
	output := &limitOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_callback_no_model", TurnLimits{MaxEvalTokens: 7})
	if !errors.Is(err, llm.ErrNoModelSelected) || !errors.Is(err, ErrTurnEvalBudgetExhausted) {
		t.Fatalf("error=%v, want joined no-model and budget exhaustion", err)
	}
	if got := output.evalTokens.Load(); got != 7 {
		t.Fatalf("callback no-model charge=%d, want full 7-token reservation", got)
	}
}

func TestRunTurnWithLimitsSkipsOptionalProviderGenerations(t *testing.T) {
	client := &boundedSideGenerationClient{}
	dir := t.TempDir()
	memories := memory.NewStore(filepath.Join(dir, "memories.json"))
	engine, err := ice.NewEngine(client, memories, ice.EngineConfig{
		StorePath: filepath.Join(dir, "conversations.json"),
		Workspace: dir,
		NumCtx:    16_384,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := New(client, nil, 16_384)
	agent.SetWorkDir(dir)
	agent.SetICEEngine(engine)
	defer agent.Close()
	for index := 0; index < 3; index++ {
		agent.AppendMessage(llm.Message{Role: "user", Content: "A long prior user message that makes compaction eligible under the tiny test context."})
		agent.AppendMessage(llm.Message{Role: "assistant", Content: "A long prior assistant response that also contributes to the compaction threshold."})
	}
	agent.AppendMessage(llm.Message{Role: "user", Content: "Please produce a direct answer long enough for automatic memory detection."})

	if err := agent.RunTurnWithLimits(context.Background(), &limitOutput{}, "turn_no_side_generation", TurnLimits{MaxEvalTokens: 8}); err != nil {
		t.Fatal(err)
	}
	// If auto-memory was scheduled, joining it makes its ChatStream call visible
	// before the assertions without relying on sleeps or scheduler timing.
	engine.StopAutoMemory()
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("bounded turn made %d provider generations, want only the main response", calls)
	}
	if calls := client.uncappedCalls.Load(); calls != 0 {
		t.Fatalf("bounded turn made %d uncapped provider generations", calls)
	}
}

func TestRunTurnWithLimitsRejectsOversizedPromptBeforeProvider(t *testing.T) {
	client := &boundedSideGenerationClient{}
	agent := New(client, nil, 64)
	agent.SetWorkDir(t.TempDir())
	for index := 0; index < 3; index++ {
		agent.AppendMessage(llm.Message{Role: "user", Content: "A long prior user message that pushes this bounded turn beyond its safe context threshold."})
		agent.AppendMessage(llm.Message{Role: "assistant", Content: "A long prior assistant response that must be compacted before any new provider request."})
	}
	agent.AppendMessage(llm.Message{Role: "user", Content: "Continue the bounded goal."})
	output := &contextBudgetOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_context_full", TurnLimits{MaxEvalTokens: 8})
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("error = %v, want context-budget error", err)
	}
	var detail *TurnContextBudgetError
	if !errors.As(err, &detail) || detail.EstimatedPromptTokens <= 0 || detail.ContextWindowTokens != 64 {
		t.Fatalf("typed context error = %#v", detail)
	}
	if calls := client.calls.Load(); calls != 0 {
		t.Fatalf("oversized bounded turn made %d provider call(s), want zero", calls)
	}
	if len(output.errors) != 1 || !strings.Contains(output.errors[0], "compact history") || !strings.Contains(output.errors[0], "retry") {
		t.Fatalf("recovery message = %#v", output.errors)
	}
}

func TestRunRejectsUncompactableOversizedPromptBeforeProvider(t *testing.T) {
	client := &boundedSideGenerationClient{}
	agent := New(client, nil, 1_200)
	agent.SetWorkDir(t.TempDir())
	agent.SetModeContext("", ToolPolicy{})
	agent.AddUserMessage(strings.Repeat("oversized first-message paste ", 300))
	output := &contextBudgetOutput{}

	err := agent.Run(context.Background(), output)
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("error = %v, want context-budget error", err)
	}
	if calls := client.calls.Load(); calls != 0 {
		t.Fatalf("uncompactable oversized prompt made %d provider call(s), want zero", calls)
	}
	if len(output.errors) != 1 || !strings.Contains(output.errors[0], "reduce prompt or tool context") || !strings.Contains(output.errors[0], "larger context window") {
		t.Fatalf("recovery message = %#v", output.errors)
	}
}

func TestContextAdmissionPreservesGenerationReserveBoundary(t *testing.T) {
	for _, test := range []struct {
		name         string
		promptTokens int
		numCtx       int
		wantReject   bool
	}{
		{name: "at threshold", promptTokens: 900, numCtx: 1_200, wantReject: false},
		{name: "above threshold", promptTokens: 901, numCtx: 1_200, wantReject: true},
		{name: "ninety nine percent", promptTokens: 1_188, numCtx: 1_200, wantReject: true},
		{name: "exactly full", promptTokens: 1_200, numCtx: 1_200, wantReject: true},
		{name: "overfull", promptTokens: 1_201, numCtx: 1_200, wantReject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldCompactForContext(test.promptTokens, test.numCtx); got != test.wantReject {
				t.Fatalf("shouldCompactForContext(%d, %d) = %v, want %v", test.promptTokens, test.numCtx, got, test.wantReject)
			}
		})
	}
}

func TestRunTurnWithLimitsRejectsOversizedToolResultBeforeSecondProviderCall(t *testing.T) {
	client := &boundedToolResultClient{}
	agent := New(client, nil, 1_200)
	agent.SetWorkDir(t.TempDir())
	agent.SetModeContext("test", NewToolPolicy([]string{"exists"}, nil, false))
	agent.AddToolHook(&expandToolResultHook{})
	agent.AddUserMessage("Check whether the workspace exists, then continue the bounded goal.")
	output := &contextBudgetOutput{}

	err := agent.RunTurnWithLimits(context.Background(), output, "turn_tool_context_full", TurnLimits{MaxEvalTokens: 8})
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("error = %v, provider calls = %d; want context-budget error", err, client.calls.Load())
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want exactly one before the tool result filled context", calls)
	}
	if starts := output.toolStarts.Load(); starts != 1 {
		t.Fatalf("tool starts = %d, want one", starts)
	}
	var detail *TurnContextBudgetError
	if !errors.As(err, &detail) || detail.EstimatedPromptTokens <= 0 || detail.ContextWindowTokens != 1_200 {
		t.Fatalf("typed context error = %#v", detail)
	}
}

// multiLargeReadClient mimics session 68b9f1a: several large read-like tool
// results on a 16k window, then a final textual answer if admission allows.
type multiLargeReadClient struct {
	calls atomic.Int64
}

func (c *multiLargeReadClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	call := c.calls.Add(1)
	switch call {
	case 1:
		// Distinct paths so the host does not suppress identical read-only
		// builtins; each result still expands to a legacy-sized payload.
		return emit(llm.StreamChunk{
			Done: true, EvalCount: 40, PromptEvalCount: 6_000,
			ToolCalls: []llm.ToolCall{
				{ID: "r1", Name: "exists", Arguments: map[string]any{"path": "."}},
				{ID: "r2", Name: "exists", Arguments: map[string]any{"path": "a"}},
				{ID: "r3", Name: "exists", Arguments: map[string]any{"path": "b"}},
			},
		})
	case 2:
		return emit(llm.StreamChunk{Text: "review complete", Done: true, EvalCount: 20, PromptEvalCount: 9_000})
	default:
		return fmt.Errorf("unexpected provider request %d", call)
	}
}

func (*multiLargeReadClient) Ping() error   { return nil }
func (*multiLargeReadClient) Model() string { return "ornith-like-16k" }
func (*multiLargeReadClient) NumCtx() int   { return 16_384 }
func (*multiLargeReadClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

// expandToLegacyLooseCapHook inflates each tool result to the old 1:1
// tokens≈bytes ceiling (~16KB on a 16k window) that used to trip after_tools.
type expandToLegacyLooseCapHook struct{}

func (*expandToLegacyLooseCapHook) Name() string { return "expand-legacy-loose-cap" }
func (*expandToLegacyLooseCapHook) PreToolUse(context.Context, *llm.ToolCall) (bool, string) {
	return false, ""
}
func (*expandToLegacyLooseCapHook) PostToolUse(_ context.Context, _ llm.ToolCall, result *string, _ bool) {
	*result = strings.Repeat("legacy-large-read-payload ", 700) // ~16KB
}

func TestRunContinuesAfterMultipleLargeToolResultsOn16kWindow(t *testing.T) {
	client := &multiLargeReadClient{}
	ag := New(client, nil, 16_384)
	ag.SetWorkDir(t.TempDir())
	ag.SetModeContext("test", NewToolPolicy([]string{"exists"}, nil, false))
	ag.AddToolHook(&expandToLegacyLooseCapHook{})
	ag.AddUserMessage("review the codebase tests and give a short opinion")
	output := &contextBudgetOutput{}

	err := ag.Run(context.Background(), output)
	if err != nil {
		t.Fatalf("turn failed under proportional tool caps: %v (errors=%#v)", err, output.errors)
	}
	if calls := client.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool round + final answer)", calls)
	}
	// Each inflated result must have been capped well below the old 16KB ceiling.
	capLimit := toolResultByteLimit(16_384)
	large := 0
	for _, msg := range ag.Messages() {
		if msg.Role != "tool" {
			continue
		}
		if len(msg.Content) > capLimit {
			t.Fatalf("tool content %d bytes exceeds proportional cap %d", len(msg.Content), capLimit)
		}
		if strings.Contains(msg.Content, "truncated") {
			large++
		}
	}
	if large < 3 {
		t.Fatalf("truncated large tool results = %d, want 3", large)
	}
}

func TestRunRejectsUncompactableToolResultBeforeSecondProviderCall(t *testing.T) {
	client := &aggregateToolResultClient{}
	agent := New(client, nil, 1_200)
	agent.SetWorkDir(t.TempDir())
	agent.SetModeContext("test", NewToolPolicy([]string{"exists"}, nil, false))
	agent.AddToolHook(&expandToolResultHook{})
	agent.AddUserMessage("Check whether the workspace exists, then continue.")
	output := &contextBudgetOutput{}

	err := agent.Run(context.Background(), output)
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("error = %v, provider calls = %d; want context-budget error", err, client.calls.Load())
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want exactly one before the tool result filled context", calls)
	}
	if starts := output.toolStarts.Load(); starts != 4 {
		t.Fatalf("tool starts = %d, want four", starts)
	}
}

func TestRunPreservesAuthoritativePromptReceiptWhenCompactionIsInapplicable(t *testing.T) {
	client := &authoritativePromptReceiptClient{}
	ag := New(client, nil, 4_000)
	ag.SetWorkDir(t.TempDir())
	ag.SetModeContext("test", NewToolPolicy([]string{"exists"}, nil, false))
	ag.AddUserMessage("Check whether this workspace exists.")
	out := &contextBudgetOutput{}

	err := ag.Run(context.Background(), out)
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("error = %v, want context-budget error", err)
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want exactly one", calls)
	}
	if len(out.errors) != 1 || !strings.Contains(out.errors[0], "estimated prompt uses ") ||
		!strings.Contains(out.errors[0], " of 4000 tokens") {
		t.Fatalf("context admission output = %#v", out.errors)
	}
}

func TestRunCarriesPromptReceiptAndSuffixGrowthAcrossTurns(t *testing.T) {
	client := &crossTurnPromptFloorClient{}
	ag := New(client, nil, 4_000)
	ag.SetWorkDir(t.TempDir())
	ag.SetModeContext("", NewToolPolicy(nil, nil, false))
	ag.AddUserMessage("Give a short direct answer.")
	firstOutput := &contextBudgetOutput{}
	if err := ag.Run(context.Background(), firstOutput); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if floor := ag.ContextPromptFloor(); floor.Tokens != 2_950 || floor.MessageTokens <= 0 || floor.Model != client.Model() {
		t.Fatalf("recorded floor = %#v", floor)
	}

	ag.AddUserMessage(strings.Repeat("next ", 80))
	secondOutput := &contextBudgetOutput{}
	err := ag.Run(context.Background(), secondOutput)
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("second turn error = %v, want context-budget error", err)
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want only the admitted first turn", calls)
	}
	if len(secondOutput.errors) != 1 || !strings.Contains(secondOutput.errors[0], "turn context budget exceeded") {
		t.Fatalf("second turn admission output = %#v", secondOutput.errors)
	}
}

func TestRunCarriesPromptReceiptAndHostContextGrowthAcrossTurns(t *testing.T) {
	client := &crossTurnPromptFloorClient{}
	ag := New(client, nil, 4_000)
	ag.SetWorkDir(t.TempDir())
	ag.SetModeContext("", NewToolPolicy(nil, nil, false))
	ag.AddUserMessage("Give a short direct answer.")
	if err := ag.Run(context.Background(), &contextBudgetOutput{}); err != nil {
		t.Fatalf("first turn: %v", err)
	}

	// Grow only host-owned context between turns. The exact receipt dominates
	// the heuristic, so this positive host-component delta must still be charged.
	ag.SetLoadedContext(strings.Repeat("host context ", 40))
	ag.AddUserMessage("Continue.")
	err := ag.Run(context.Background(), &contextBudgetOutput{})
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("second turn error = %v, want context-budget error", err)
	}
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want only the admitted first turn", calls)
	}
}

func TestRunTurnWithLimitsJoinsPriorAutoMemoryBeforeProvider(t *testing.T) {
	client := &joinedAutoMemoryClient{
		autoStarted: make(chan struct{}),
		autoStopped: make(chan struct{}),
	}
	dir := t.TempDir()
	memories := memory.NewStore(filepath.Join(dir, "memories.json"))
	engine, err := ice.NewEngine(client, memories, ice.EngineConfig{
		StorePath: filepath.Join(dir, "conversations.json"),
		Workspace: dir,
		NumCtx:    4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := New(client, nil, 4096)
	agent.SetWorkDir(dir)
	agent.SetICEEngine(engine)
	defer agent.Close()

	engine.DetectAutoMemory(context.Background(),
		"A prior user exchange long enough to trigger automatic memory extraction.",
		"A prior assistant exchange long enough to trigger automatic memory extraction and remain in flight.",
	)
	<-client.autoStarted
	agent.AppendMessage(llm.Message{Role: "user", Content: "Start the bounded goal turn only after optional inference is joined."})

	if err := agent.RunTurnWithLimits(context.Background(), &limitOutput{}, "turn_join_auto_memory", TurnLimits{MaxEvalTokens: 8}); err != nil {
		t.Fatal(err)
	}
	if client.mainCalls.Load() != 1 {
		t.Fatalf("bounded main provider calls=%d", client.mainCalls.Load())
	}
	select {
	case <-client.autoStopped:
	default:
		t.Fatal("bounded turn returned before prior auto-memory stopped")
	}
}
