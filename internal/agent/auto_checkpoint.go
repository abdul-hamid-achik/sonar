package agent

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
)

// ErrAutoIterationCheckpoint identifies a productive AUTO turn that reached
// its per-segment iteration watchdog. It is a scheduler signal, not a failure:
// a host may create a fresh turn identity and continue subject to its own
// segment, time, token, cancellation, and loop budgets.
var ErrAutoIterationCheckpoint = errors.New("AUTO iteration checkpoint")

// AutoIterationCheckpointError is the bounded host-owned receipt returned
// when AUTO reaches its iteration watchdog after concrete, non-repeated
// progress. It deliberately contains no arguments, paths, tool output, model
// prose, or reasoning. UI and headless supervisors may use these counters to
// explain and schedule a continuation without persisting raw execution data.
type AutoIterationCheckpointError struct {
	TurnID                  string
	Iterations              int
	ToolCalls               int
	SuccessfulToolCalls     int
	DistinctSuccessfulCalls int
	// EffectfulSuccessfulCalls counts verified successes whose effect class
	// was not read-only. A supervisor may use it to distinguish a read-only
	// replay (a stall) from a legitimately repeated build/test cycle.
	EffectfulSuccessfulCalls int
	// ProgressDigest is an opaque, order-independent digest over the exact
	// successful tool+argument fingerprints observed in this AUTO segment. It
	// lets a supervisor detect a stalled cross-turn continuation without
	// exposing arguments or paths to the UI/session projection.
	ProgressDigest string
	EvalTokens     int64
	Elapsed        time.Duration
	LastTool       string
	LastEffect     executionpkg.EffectClass
	LastDomain     ecosystem.DomainState
}

func (e *AutoIterationCheckpointError) Error() string {
	if e == nil {
		return ErrAutoIterationCheckpoint.Error()
	}
	return fmt.Sprintf(
		"%v after %d iterations: %d/%d tool calls succeeded (%d distinct)",
		ErrAutoIterationCheckpoint, e.Iterations, e.SuccessfulToolCalls,
		e.ToolCalls, e.DistinctSuccessfulCalls,
	)
}

func (e *AutoIterationCheckpointError) Unwrap() error {
	return ErrAutoIterationCheckpoint
}

// autoTurnProgress is intentionally private and reset for every RunTurn. A
// successful call is productive only once per exact identity+arguments
// fingerprint. This prevents an endlessly repeated read or mutation from
// qualifying for automatic continuation merely because its transport keeps
// answering successfully.
type autoTurnProgress struct {
	toolCalls                int
	successfulToolCalls      int
	distinctSuccessfulCalls  int
	effectfulSuccessfulCalls int
	lastIterationCalls       int
	lastIterationSucceeded   int
	lastIterationDistinct    int
	lastTool                 string
	lastEffect               executionpkg.EffectClass
	lastDomain               ecosystem.DomainState
	seen                     map[string]struct{}
}

func newAutoTurnProgress() *autoTurnProgress {
	return &autoTurnProgress{seen: make(map[string]struct{})}
}

func (p *autoTurnProgress) beginIteration(toolCalls int) {
	if p == nil {
		return
	}
	p.toolCalls += toolCalls
	p.lastIterationCalls = toolCalls
	p.lastIterationSucceeded = 0
	p.lastIterationDistinct = 0
}

func (p *autoTurnProgress) settle(
	toolName string,
	argumentsHash string,
	effect executionpkg.EffectClass,
	terminal executionpkg.EventType,
	projection ecosystem.ToolProjection,
) {
	if p == nil || terminal != executionpkg.EventCompleted ||
		projection.Transport != ecosystem.TransportSucceeded ||
		!projection.DomainTyped || projection.Domain != ecosystem.DomainSucceeded {
		return
	}

	p.successfulToolCalls++
	p.lastIterationSucceeded++
	if effect != executionpkg.EffectReadOnly {
		p.effectfulSuccessfulCalls++
	}
	p.lastTool = toolName
	p.lastEffect = effect
	p.lastDomain = projection.Domain

	fingerprint := toolName + "\x00" + argumentsHash
	if _, duplicate := p.seen[fingerprint]; duplicate {
		return
	}
	p.seen[fingerprint] = struct{}{}
	p.distinctSuccessfulCalls++
	p.lastIterationDistinct++
}

func (p *autoTurnProgress) checkpoint(
	turnID string,
	iterations int,
	evalTokens int64,
	elapsed time.Duration,
) *AutoIterationCheckpointError {
	// The final iteration must contain at least one verified success so a
	// segment that devolved into repeated or refused work stays terminal, but a
	// mixed final iteration (one failure among real progress) may continue. The
	// supervisor's cross-segment progress digest rejects a continuation that
	// merely replays the same distinct work.
	if p == nil || p.lastIterationCalls <= 0 ||
		p.lastIterationSucceeded <= 0 ||
		p.distinctSuccessfulCalls <= 0 {
		return nil
	}
	return p.receipt(turnID, iterations, evalTokens, elapsed)
}

// segmentCheckpoint reports distinct verified progress anywhere in the
// segment, regardless of the final iteration's shape. It backs the AUTO
// breaker fallbacks, where the terminal provider response was degenerate
// (empty or malformed) but earlier work in the segment was real; a fresh
// segment re-prompts the model instead of abandoning that work.
func (p *autoTurnProgress) segmentCheckpoint(
	turnID string,
	iterations int,
	evalTokens int64,
	elapsed time.Duration,
) *AutoIterationCheckpointError {
	if p == nil || p.distinctSuccessfulCalls <= 0 {
		return nil
	}
	return p.receipt(turnID, iterations, evalTokens, elapsed)
}

func (p *autoTurnProgress) receipt(
	turnID string,
	iterations int,
	evalTokens int64,
	elapsed time.Duration,
) *AutoIterationCheckpointError {
	return &AutoIterationCheckpointError{
		TurnID:                   turnID,
		Iterations:               iterations,
		ToolCalls:                p.toolCalls,
		SuccessfulToolCalls:      p.successfulToolCalls,
		DistinctSuccessfulCalls:  p.distinctSuccessfulCalls,
		EffectfulSuccessfulCalls: p.effectfulSuccessfulCalls,
		ProgressDigest:           autoProgressDigest(p.seen),
		EvalTokens:               evalTokens,
		Elapsed:                  elapsed,
		LastTool:                 p.lastTool,
		LastEffect:               p.lastEffect,
		LastDomain:               p.lastDomain,
	}
}

func autoProgressDigest(seen map[string]struct{}) string {
	if len(seen) == 0 {
		return ""
	}
	keys := make([]string, 0, len(seen))
	for fingerprint := range seen {
		keys = append(keys, fingerprint)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, fingerprint := range keys {
		_, _ = hash.Write([]byte(fingerprint))
		// The exact fingerprint consists of a UTF-8 tool name, NUL, and a
		// lowercase hexadecimal arguments digest. 0xff is therefore an
		// unambiguous record separator without retaining either source value.
		_, _ = hash.Write([]byte{0xff})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
