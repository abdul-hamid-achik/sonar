package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// RemoteInferenceFailureCopy is the only provider-failure prose allowed to
// cross from a dispatched remote inference request into transcript/session
// state. Raw HTTP bodies, SSE error messages, endpoints, and provider paths
// remain transient diagnostics inside the agent.
const RemoteInferenceFailureCopy = "Remote model response failed. Check the provider profile and credential environment, then retry or switch providers."

// ErrRemoteInferenceFailed is the stable error identity for a safely projected
// remote failure. Its technical identity stays independent from presentation
// copy and punctuation.
var ErrRemoteInferenceFailed = errors.New("remote inference failed")

type remoteInferenceBoundaryError struct{}

func (remoteInferenceBoundaryError) Error() string {
	return RemoteInferenceFailureCopy
}

func (remoteInferenceBoundaryError) Is(target error) bool {
	return target == ErrRemoteInferenceFailed
}

// providerStage runs one provider request for iteration i: eval-budget
// reservation, streaming, token-receipt validation, retry after a charged
// reservation, and empty-terminal repair. It returns the assistant text, the
// ordered tool calls, and a control-flow signal for the iteration loop. A
// queued host continuation replaces the provider request with its exact
// detached call.
func (t *turnRuntime) providerStage(ctx context.Context, i int) (string, []llm.ToolCall, turnStageSignal, error) {
	const maxRetries = 2
	remainingEvalTokens := int64(0)
	requestEvalLimit := 0
	if t.limits.MaxEvalTokens > 0 {
		remainingEvalTokens = t.limits.MaxEvalTokens - t.totalEvalTokens
		if remainingEvalTokens <= 0 {
			return "", nil, stageProceed, fmt.Errorf("%w: used %d of %d", ErrTurnEvalBudgetExhausted, t.totalEvalTokens, t.limits.MaxEvalTokens)
		}
		effectivePromptTokens := t.estimatedPromptTokens()
		requestEvalLimit = contextReservedEvalLimit(remainingEvalTokens, effectivePromptTokens, t.turnNumCtx)
		if requestEvalLimit <= 0 {
			return "", nil, stageProceed, t.rejectContextPrompt(effectivePromptTokens, true, "phase", "before_provider", "iter", i)
		}
	}

	// Stream LLM response.
	var textBuf strings.Builder
	var reasoningBuf strings.Builder
	var toolCalls []llm.ToolCall
	t.lastReasoning = ""
	t.hostContinuationBatch = t.queuedAutoContinuation != nil
	t.activeAutoContinuation = t.queuedAutoContinuation
	t.lastEvalTokens = 0
	doneSeen := false
	callbackSeen := false
	reportedEvalTokens := int64(0)
	requestHostTokens := 0
	requestMessageTokens := 0
	repairRequest := t.emptyTerminalRepairPending

	if t.hostContinuationBatch {
		toolCalls = []llm.ToolCall{t.queuedAutoContinuation.detachedCall()}
		t.queuedAutoContinuation = nil
	} else {
		llmStart := time.Now()
		// Snapshot the message history under the lock: ChatStream runs while
		// other goroutines may AppendMessage, and passing the live slice would
		// race on the backing array (and could realloc mid-stream).
		t.a.mu.RLock()
		msgsSnapshot := make([]llm.Message, len(t.a.messages))
		copy(msgsSnapshot, t.a.messages)
		t.a.mu.RUnlock()
		requestHostTokens = estimateHostPromptTokens(t.system, t.tools)
		requestMessageTokens = estimateMessagesPromptTokens(msgsSnapshot)

		err := t.a.chatStreamWithResolvedImages(ctx, llm.ChatOptions{
			Messages:        msgsSnapshot,
			Tools:           t.tools,
			System:          t.system,
			MaxEvalTokens:   requestEvalLimit,
			ExpectedContext: t.turnNumCtx,
			ExpectedModel:   t.turnModel,
		}, func(chunk llm.StreamChunk) error {
			callbackSeen = true
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if doneSeen {
				return fmt.Errorf("provider streamed data after its terminal usage receipt")
			}

			if chunk.Text != "" {
				textBuf.WriteString(chunk.Text)
				t.out.StreamText(chunk.Text)
			}
			if chunk.Reasoning != "" {
				reasoningBuf.WriteString(chunk.Reasoning)
				t.lastReasoning = reasoningBuf.String()
				t.out.StreamReasoning(chunk.Reasoning)
			}
			if len(chunk.ToolCalls) > 0 {
				if len(chunk.ToolCalls) > maxToolCallsPerResponse-len(toolCalls) {
					return fmt.Errorf("model streamed at least %d tool calls; maximum per response is %d", len(toolCalls)+len(chunk.ToolCalls), maxToolCallsPerResponse)
				}
				toolCalls = append(toolCalls, chunk.ToolCalls...)
			}
			if chunk.Done {
				if chunk.EvalCount < 0 || chunk.PromptEvalCount < 0 {
					return fmt.Errorf("invalid provider token receipt: eval=%d prompt=%d", chunk.EvalCount, chunk.PromptEvalCount)
				}
				if int64(chunk.EvalCount) > math.MaxInt64-t.totalEvalTokens {
					return fmt.Errorf("provider evaluation-token receipt %d overflows the parent turn counter", chunk.EvalCount)
				}
				doneSeen = true
				t.lastPromptTokens = chunk.PromptEvalCount
				t.lastEvalTokens = chunk.EvalCount
				reportedEvalTokens = int64(chunk.EvalCount)
				t.out.StreamDone(chunk.EvalCount, chunk.PromptEvalCount)
				if receiptOut, ok := t.out.(ProviderReceiptOutput); ok {
					receiptOut.ProviderReceipt(chunk.FinishReason, chunk.Timing)
				}
			}
			return nil
		})

		if err != nil {
			boundaryErr := providerBoundaryError(err)
			if errors.Is(err, llm.ErrInferenceNotStarted) && !callbackSeen {
				if t.lg != nil {
					t.lg.Warn("llm request rejected before dispatch", "iter", i, "err", boundaryErr)
				}
				// boundaryErr already reads "inference not started: <cause>", so a
				// "LLM request not started:" prefix only stutters at the user.
				t.out.Error(capitalizeFirst(boundaryErr.Error()))
				return "", nil, stageProceed, boundaryErr
			}
			reservedEvalTokens := int64(0)
			if (!errors.Is(err, llm.ErrNoModelSelected) && !errors.Is(err, llm.ErrInferenceNotStarted)) || callbackSeen {
				reservedEvalTokens = chargeUnknownEvalReservation(t.out, requestEvalLimit, reportedEvalTokens)
			}
			var reservationErr error
			if reservedEvalTokens > 0 {
				// The unknown portion of the reservation is durably charged either
				// way; count it against this turn before deciding whether the
				// remaining budget can absorb a retry of the same request.
				t.totalEvalTokens += reservedEvalTokens
				if ctx.Err() == nil && !repairRequest && t.retryCount < maxRetries &&
					isRetryableError(err) && t.totalEvalTokens < t.limits.MaxEvalTokens {
					t.retryCount++
					if t.lg != nil {
						t.lg.Warn("llm retry after charged reservation", "iter", i, "attempt", t.retryCount, "reserved", reservedEvalTokens, "err", boundaryErr)
					}
					t.out.Error(fmt.Sprintf("LLM transport failed, retrying (%d/%d) after charging %d reserved token(s)...", t.retryCount, maxRetries, reservedEvalTokens))
					return "", nil, stageNextIteration, nil
				}
				reservationErr = fmt.Errorf(
					"%w: provider stream ended without a trustworthy terminal usage receipt; conservatively charged %d reserved token(s)",
					ErrTurnEvalBudgetExhausted, reservedEvalTokens,
				)
			}
			if ctx.Err() != nil {
				return "", nil, stageProceed, errors.Join(ctx.Err(), reservationErr)
			}
			if reservationErr != nil {
				t.out.Error(reservationErr.Error())
				return "", nil, stageProceed, errors.Join(reservationErr, fmt.Errorf("LLM response: %w", boundaryErr))
			}
			// Retry on transient provider errors from small models.
			if !repairRequest && t.limits.MaxEvalTokens == 0 && t.retryCount < maxRetries && isRetryableError(err) {
				t.retryCount++
				if t.lg != nil {
					t.lg.Warn("llm retry", "iter", i, "attempt", t.retryCount, "err", boundaryErr)
				}
				t.out.Error(fmt.Sprintf("LLM produced malformed output, retrying (%d/%d)...", t.retryCount, maxRetries))
				return "", nil, stageNextIteration, nil
			}
			if t.lg != nil {
				t.lg.Error("llm error", "iter", i, "err", boundaryErr)
			}
			// Show error and provide a fallback response
			t.out.Error(fmt.Sprintf("LLM error: %v", boundaryErr))
			if errors.Is(boundaryErr, ErrRemoteInferenceFailed) {
				t.out.SystemMessage("⚠️ " + RemoteInferenceFailureCopy + "\n\nTool results are still available above.")
			} else {
				// Exact local provenance keeps Ollama diagnostics actionable.
				t.out.SystemMessage(fmt.Sprintf("⚠️ Model response failed: %v\n\nYou can try:\n- Checking if Ollama is running (`ollama ps`)\n- Switching to a different model (ctrl+o)\n- Reducing context size\n\nTool results are still available above.", boundaryErr))
			}
			return "", nil, stageProceed, fmt.Errorf("LLM response: %w", boundaryErr)
		}
		if !doneSeen {
			reservedEvalTokens := chargeUnknownEvalReservation(t.out, requestEvalLimit, 0)
			receiptErr := fmt.Errorf("provider stream ended without a terminal usage receipt")
			if reservedEvalTokens > 0 {
				budgetErr := fmt.Errorf(
					"%w: provider stream ended without a terminal usage receipt; conservatively charged %d reserved token(s)",
					ErrTurnEvalBudgetExhausted, reservedEvalTokens,
				)
				t.out.Error(budgetErr.Error())
				return "", nil, stageProceed, errors.Join(budgetErr, receiptErr)
			}
			t.out.Error(receiptErr.Error())
			return "", nil, stageProceed, receiptErr
		}
		t.retryCount = 0 // reset on success
		if t.lastEvalTokens < 0 || int64(t.lastEvalTokens) > math.MaxInt64-t.totalEvalTokens {
			return "", nil, stageProceed, fmt.Errorf("invalid provider evaluation-token receipt %d", t.lastEvalTokens)
		}
		t.totalEvalTokens += int64(t.lastEvalTokens)
		t.a.recordContextPromptFloor(t.lastPromptTokens, requestHostTokens, requestMessageTokens, t.turnModel)
		if repairRequest {
			// The correction belongs to exactly one provider request. Rebuild the
			// ordinary system prompt before any tool call from the repaired response
			// can lead to another iteration.
			t.emptyTerminalRepairPending = false
			t.rebuildSystem(ctx)
		}
		if t.lg != nil {
			t.lg.Info("llm response", "iter", i, "ms", time.Since(llmStart).Milliseconds(),
				"prompt_tokens", t.lastPromptTokens, "eval_tokens", t.lastEvalTokens, "tool_calls", len(toolCalls))
		}
	}
	if len(toolCalls) > maxToolCallsPerResponse {
		err := fmt.Errorf("model returned %d tool calls; maximum per response is %d", len(toolCalls), maxToolCallsPerResponse)
		t.out.Error(err.Error())
		return "", nil, stageProceed, err
	}
	t.autoProgress.beginIteration(len(toolCalls))
	if t.limits.MaxEvalTokens > 0 && t.totalEvalTokens > t.limits.MaxEvalTokens {
		// A provider that violates num_predict has already crossed the requested
		// generation boundary. Preserve only its text and stop before creating
		// any durable execution intents.
		t.a.AppendMessage(llm.Message{Role: "assistant", Content: textBuf.String()})
		err := fmt.Errorf("%w: provider reported %d used token(s) for a %d-token limit", ErrTurnEvalBudgetExhausted, t.totalEvalTokens, t.limits.MaxEvalTokens)
		t.out.Error(err.Error())
		return "", nil, stageProceed, err
	}
	if requestEvalLimit > 0 && t.lastEvalTokens > requestEvalLimit {
		t.a.AppendMessage(llm.Message{Role: "assistant", Content: textBuf.String()})
		err := fmt.Errorf("%w: provider reported %d used token(s) for a %d-token context-reserved request limit", ErrTurnEvalBudgetExhausted, t.lastEvalTokens, requestEvalLimit)
		t.out.Error(err.Error())
		return "", nil, stageProceed, err
	}
	if t.limits.MaxEvalTokens > 0 && t.totalEvalTokens == t.limits.MaxEvalTokens && len(toolCalls) > 0 {
		// The provider may return tool requests on the response that consumes
		// the final token allowance. Keep its text, but never create durable
		// dispatch intents or execute effects after the hard boundary.
		t.a.AppendMessage(llm.Message{Role: "assistant", Content: textBuf.String()})
		err := fmt.Errorf("%w before tool dispatch: used %d of %d", ErrTurnEvalBudgetExhausted, t.totalEvalTokens, t.limits.MaxEvalTokens)
		t.out.Error(err.Error())
		return "", nil, stageProceed, err
	}
	if !t.hostContinuationBatch && len(toolCalls) == 0 && strings.TrimSpace(textBuf.String()) == "" {
		if t.previousIterationEndedWithToolResult && t.emptyTerminalRepairs < maxEmptyTerminalRepairs && i < t.maxIters-1 {
			if err := ctx.Err(); err != nil {
				return "", nil, stageProceed, err
			}
			if t.limits.MaxEvalTokens > 0 && t.totalEvalTokens >= t.limits.MaxEvalTokens {
				err := fmt.Errorf("%w before empty-terminal repair: used %d of %d", ErrTurnEvalBudgetExhausted, t.totalEvalTokens, t.limits.MaxEvalTokens)
				t.out.Error(err.Error())
				return "", nil, stageProceed, err
			}
			t.emptyTerminalRepairs++
			t.previousIterationEndedWithToolResult = false
			t.emptyTerminalRepairPending = true
			t.rebuildSystem(ctx)
			if err := t.admitSystemPrompt(ctx); err != nil {
				return "", nil, stageProceed, err
			}
			if err := ctx.Err(); err != nil {
				return "", nil, stageProceed, err
			}
			if t.lg != nil {
				t.lg.Warn("empty terminal response after tool result; retrying once", "iter", i, "eval_tokens", t.lastEvalTokens)
			}
			t.out.SystemMessage("Model returned no visible answer after the tool result; retrying once.")
			return "", nil, stageNextIteration, nil
		}
		err := fmt.Errorf(
			"%w: provider completed after %d evaluation token(s) without visible text or a tool call",
			ErrEmptyTerminalResponse, t.lastEvalTokens,
		)
		if t.lg != nil {
			t.lg.Warn("empty terminal response", "iter", i, "eval_tokens", t.lastEvalTokens)
		}
		// A degenerate terminal response must not abandon a segment that made
		// verified distinct progress. The host supervisor re-prompts under its
		// own segment, digest, and time budgets instead of surfacing a failure.
		if checkpoint := t.autoProgress.segmentCheckpoint(t.turnID, i+1, t.totalEvalTokens, time.Since(t.turnStart)); checkpoint != nil {
			if t.lg != nil {
				t.lg.Info("AUTO segment checkpoint after empty terminal response", "iter", i)
			}
			return "", nil, stageProceed, checkpoint
		}
		t.out.Error(err.Error())
		return "", nil, stageProceed, err
	}
	return textBuf.String(), toolCalls, stageProceed, nil
}

func providerBoundaryError(err error) error {
	if llm.IsRemoteInferenceError(err) {
		return remoteInferenceBoundaryError{}
	}
	return err
}

// capitalizeFirst makes a Go error string presentable as UI copy without
// prefixing it with another sentence, which is how "LLM request not started:
// inference not started: no model selected" happened.
func capitalizeFirst(value string) string {
	if value == "" {
		return value
	}
	runes := []rune(value)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
