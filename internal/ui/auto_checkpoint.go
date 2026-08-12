package ui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

// Each agent segment already has the AUTO iteration watchdog. This second,
// host-owned ceiling bounds a complete conversational turn while allowing long
// productive jobs to cross several invisible implementation segments.
//
// Both dimensions come from tools.auto_max_segments and tools.auto_max_wall_time
// so an unattended job can be given hours. They were constants, which made that
// impossible to express at any value of auto_max_iterations — that setting
// bounds one segment, and the whole-turn ceiling silently overrode it.
// config.DefaultAutoMaxSegments / DefaultAutoMaxWallTime hold the former
// constants, so an unconfigured harness behaves exactly as before.

type autoCheckpointSupervisor struct {
	agent.AutoSegmentState
}

func (s *autoCheckpointSupervisor) reset(logicalTurnID string, startedAt time.Time, maxSegments int, maxElapsed time.Duration) {
	s.Reset(logicalTurnID, startedAt, maxSegments, maxElapsed)
}

func (s *autoCheckpointSupervisor) segmentCeiling() int {
	return s.SegmentCeiling()
}

func (s *autoCheckpointSupervisor) elapsedCeiling() time.Duration {
	return s.ElapsedCeiling()
}

func (s *autoCheckpointSupervisor) clear() {
	s.Clear()
}

func (s *autoCheckpointSupervisor) admit(
	logicalTurnID string,
	checkpoint *agent.AutoIterationCheckpointError,
	now time.Time,
) error {
	return s.Admit(logicalTurnID, checkpoint, now)
}

func defaultPlainAutoTurnLimits(limits agent.TurnLimits, authority Mode, wallTime time.Duration) agent.TurnLimits {
	return agent.ApplyDefaultAutoTurnLimits(limits, authority == ModeAuto, wallTime)
}

func normalizeLogicalTurnLimits(limits agent.TurnLimits, now time.Time) agent.TurnLimits {
	return agent.NormalizeLogicalTurnLimits(limits, now)
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
	continuedLimits, limitsErr := agent.ContinueAutoLimits(m.turnRunOptions.Limits, checkpoint)
	if limitsErr != nil {
		m.entries = append(m.entries, ChatEntry{
			Kind: "error", Content: "AUTO stopped safely at a continuation checkpoint: " + limitsErr.Error() + ".",
		})
		m.invalidateEntryCache()
		return nil, false, fmt.Errorf("AUTO continuation stopped: %w", limitsErr)
	}
	m.turnRunOptions.Limits = continuedLimits
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
			m.autoCheckpoints.SegmentsContinued+1, checkpoint.SuccessfulToolCalls,
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

// autoTurnCeilings resolves the configured whole-turn AUTO budget, falling back
// to the built-in defaults when no agent is attached (tests, early frames).
func (m *Model) autoTurnCeilings() (segments int, wallTime time.Duration) {
	if m != nil && m.agent != nil {
		return m.agent.AutoTurnCeilings()
	}
	return config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime
}
