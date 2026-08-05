package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

const (
	// Each agent segment already has the AUTO iteration watchdog. This second,
	// host-owned ceiling bounds a complete conversational turn while allowing
	// long productive jobs to cross several invisible implementation segments.
	maxAutoCheckpointSegments = 8
	maxAutoCheckpointElapsed  = 90 * time.Minute
)

type autoCheckpointSupervisor struct {
	logicalTurnID     string
	startedAt         time.Time
	segmentsContinued int
	lastDigest        string
}

func (s *autoCheckpointSupervisor) reset(logicalTurnID string, startedAt time.Time) {
	*s = autoCheckpointSupervisor{
		logicalTurnID: strings.TrimSpace(logicalTurnID),
		startedAt:     startedAt,
	}
}

func (s *autoCheckpointSupervisor) clear() {
	*s = autoCheckpointSupervisor{}
}

func (s *autoCheckpointSupervisor) admit(
	logicalTurnID string,
	checkpoint *agent.AutoIterationCheckpointError,
	now time.Time,
) error {
	if s == nil || checkpoint == nil {
		return errors.New("checkpoint receipt is unavailable")
	}
	if strings.TrimSpace(logicalTurnID) == "" || logicalTurnID != s.logicalTurnID {
		return errors.New("checkpoint does not belong to the active turn")
	}
	digest := strings.TrimSpace(checkpoint.ProgressDigest)
	if digest == "" {
		return errors.New("checkpoint has no progress identity")
	}
	if digest == s.lastDigest && checkpoint.EffectfulSuccessfulCalls == 0 {
		// A read-only replay of the previous segment's exact work set is a
		// stall. A repeated set that includes verified effects (for example a
		// build/test cycle run again after new edits landed earlier in the
		// logical turn) may legitimately recur; the segment and time budgets
		// still bound it.
		return errors.New("the last AUTO segment repeated without new progress")
	}
	if s.segmentsContinued >= maxAutoCheckpointSegments {
		return fmt.Errorf("the %d-segment AUTO continuation budget was exhausted", maxAutoCheckpointSegments)
	}
	if !s.startedAt.IsZero() && now.Sub(s.startedAt) >= maxAutoCheckpointElapsed {
		return fmt.Errorf("the %s AUTO continuation time budget was exhausted", maxAutoCheckpointElapsed)
	}
	s.segmentsContinued++
	s.lastDigest = digest
	return nil
}

// defaultPlainAutoTurnLimits gives an otherwise-unbounded AUTO turn the same
// wall ceiling the checkpoint supervisor already enforces across segments.
// Without it, one wedged segment — for example an approval prompt that never
// receives an answer — could hold the logical turn open forever. Goals and
// other bounded callers keep their own budgets; interactive NORMAL and PLAN
// turns stay unbounded because the user is watching them.
func defaultPlainAutoTurnLimits(limits agent.TurnLimits, authority Mode) agent.TurnLimits {
	if authority != ModeAuto {
		return limits
	}
	if limits.MaxEvalTokens > 0 || !limits.Deadline.IsZero() || limits.MaxWallTime > 0 {
		return limits
	}
	limits.MaxWallTime = maxAutoCheckpointElapsed
	return limits
}

func normalizeLogicalTurnLimits(limits agent.TurnLimits, now time.Time) agent.TurnLimits {
	if limits.MaxWallTime <= 0 {
		return limits
	}
	deadline := now.Add(limits.MaxWallTime)
	if limits.Deadline.IsZero() || deadline.Before(limits.Deadline) {
		limits.Deadline = deadline
	}
	// A relative limit must not restart at every AUTO segment.
	limits.MaxWallTime = 0
	return limits
}

func newAgentSegmentCmd(
	agentInstance *agent.Agent,
	program *tea.Program,
	outputDetails *OutputDetailStore,
	ctx context.Context,
	logicalTurnID string,
	segmentTurnID string,
	options agent.TurnOptions,
) tea.Cmd {
	workDir := ""
	if agentInstance != nil {
		workDir = agentInstance.WorkDir()
	}
	return func() tea.Msg {
		if agentInstance == nil {
			return AgentDoneMsg{
				TurnID: logicalTurnID, SegmentTurnID: segmentTurnID,
				Err: errors.New("agent is unavailable"),
			}
		}
		adapter := NewAdapterWithOutputDetails(program, outputDetails, workDir)
		err := agentInstance.RunTurnWithOptions(ctx, adapter, segmentTurnID, options)
		return AgentDoneMsg{TurnID: logicalTurnID, SegmentTurnID: segmentTurnID, Err: err}
	}
}

// handleAutoIterationCheckpoint consumes the agent's non-terminal scheduler
// signal before ordinary AgentDone settlement. A successful continuation does
// not save, increment the session turn, evaluate a Goal, clear the queued
// follow-up, or render a red error: it is still the same logical user turn.
func (m *Model) handleAutoIterationCheckpoint(message AgentDoneMsg) (tea.Cmd, bool, error) {
	var checkpoint *agent.AutoIterationCheckpointError
	if !errors.As(message.Err, &checkpoint) || !errors.Is(message.Err, agent.ErrAutoIterationCheckpoint) {
		return nil, false, nil
	}
	// A goal turn settles through the durable Goal Runtime: RecordTurn,
	// budget accounting, and Cortex evaluation own its continuation. Plain-AUTO
	// segment chaining must never bypass that per-turn re-admission.
	if m.goalRuntime != nil && m.goalTurnID != "" {
		return nil, false, nil
	}
	logicalTurnID := message.TurnID
	if logicalTurnID == "" {
		logicalTurnID = m.turnLogicalID
	}
	segmentTurnID := message.SegmentTurnID
	if segmentTurnID == "" {
		segmentTurnID = message.TurnID
	}
	if m.turnAuthority != ModeAuto || m.turnRunContext == nil ||
		logicalTurnID == "" || logicalTurnID != m.turnLogicalID ||
		segmentTurnID == "" || segmentTurnID != m.turnSegmentID {
		return nil, false, fmt.Errorf("AUTO stopped safely: stale or unauthorized continuation checkpoint")
	}
	if m.shuttingDown || (m.turnRunContext != nil && m.turnRunContext.Err() != nil) {
		return nil, false, context.Canceled
	}
	if err := m.autoCheckpoints.admit(logicalTurnID, checkpoint, m.nowTime()); err != nil {
		m.entries = append(m.entries, ChatEntry{
			Kind: "error", Content: "AUTO stopped safely at a continuation checkpoint: " + err.Error() + ".",
		})
		m.invalidateEntryCache()
		return nil, false, fmt.Errorf("AUTO continuation stopped: %w", err)
	}

	// Preserve a logical eval budget across segment boundaries. The agent owns
	// the exact usage receipt; no provider prose or tool result crosses here.
	if remaining := m.turnRunOptions.Limits.MaxEvalTokens; remaining > 0 {
		remaining -= checkpoint.EvalTokens
		if remaining <= 0 {
			err := errors.New("the logical turn evaluation-token budget was exhausted")
			m.entries = append(m.entries, ChatEntry{
				Kind: "error", Content: "AUTO stopped safely at a continuation checkpoint: " + err.Error() + ".",
			})
			m.invalidateEntryCache()
			return nil, false, fmt.Errorf("AUTO continuation stopped: %w", err)
		}
		m.turnRunOptions.Limits.MaxEvalTokens = remaining
	}
	// Host continuations are one-shot capabilities. Re-presenting one on a later
	// segment would be stale even if the agent's claim guard rejected it.
	m.turnRunOptions.Continuation = nil

	newSegmentID, err := execution.NewTurnID()
	if err != nil {
		return nil, false, fmt.Errorf("AUTO continuation identity: %w", err)
	}
	// Settle the finished segment's stream into the transcript before the
	// boundary saves it, so the durable snapshot describes the work the cursor
	// is about to certify as projected.
	m.flushStream()
	// A segment boundary is a projection boundary. Without it the next segment
	// scans the execution ledger from the logical turn's original cursor, where
	// this segment's own completed non-read-only effects read as unprojected
	// hazards and stop the run before any provider work. Fail closed: stopping
	// the continuation here is strictly better than launching a segment that
	// cannot survive its own pre-provider scan.
	if err := m.advanceExecutionProjectionBoundary(); err != nil {
		m.entries = append(m.entries, ChatEntry{
			Kind: "error",
			Content: "AUTO stopped safely at a continuation checkpoint: the execution projection boundary could not be advanced · " +
				sanitizeTerminalSingleLine(err.Error()) + ".",
		})
		m.invalidateEntryCache()
		return nil, false, fmt.Errorf("AUTO continuation stopped: %w", err)
	}
	// A continuation is invisible provider plumbing, but a long autonomous run
	// must stay legible: leave one bounded counters-only receipt in the
	// transcript. No arguments, paths, tool output, or prose cross this line.
	m.entries = append(m.entries, ChatEntry{
		Kind: "system",
		Content: fmt.Sprintf(
			"AUTO checkpoint · continuing segment %d · %d/%d tools ok · %s",
			m.autoCheckpoints.segmentsContinued+1, checkpoint.SuccessfulToolCalls,
			checkpoint.ToolCalls, formatWorkingElapsed(checkpoint.Elapsed),
		),
	})
	m.compactingContext = false
	m.capabilityRoute = nil
	m.clearContinuationAction()
	m.turnSegmentID = newSegmentID
	m.beginContinuationTurn(newSegmentID)
	m.state = StateWaiting
	m.scramble.Reset()
	m.recalcViewportHeight()
	m.refreshTranscript()
	m.gotoBottomIfFollowing()

	command := newAgentSegmentCmd(
		m.agent, m.program, m.outputDetails, m.turnRunContext, logicalTurnID, newSegmentID, m.turnRunOptions,
	)
	return tea.Batch(m.startActivityCmd(), command), true, nil
}
