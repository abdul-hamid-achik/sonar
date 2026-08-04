// Package supervisor defines UI-independent scheduling decisions for durable
// goals. It does not dispatch providers or render presentation state.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

var ErrInvalid = errors.New("invalid goal supervisor observation")

// Action is the only next operation a controller may perform for an observed
// goal. A decision never performs the action itself.
type Action string

const (
	ActionStop                 Action = "stop"
	ActionWaitTurn             Action = "wait_turn"
	ActionEvaluate             Action = "evaluate"
	ActionDispatchInitial      Action = "dispatch_initial"
	ActionDispatchManual       Action = "dispatch_manual"
	ActionDispatchContinuation Action = "dispatch_continuation"
)

// StopReason is a stable explanation shared by CLI and TUI controllers.
type StopReason string

const (
	StopNone                   StopReason = ""
	StopLeaseRequired          StopReason = "lease_required"
	StopPersistenceUnavailable StopReason = "persistence_unavailable"
	StopApproval               StopReason = "approval_required"
	StopDecision               StopReason = "decision_required"
	StopDependency             StopReason = "dependency_blocked"
	StopOutcomeUnknown         StopReason = "outcome_unknown"
	StopRecoveryRequired       StopReason = "recovery_required"
	StopTurnIdentityConflict   StopReason = "turn_identity_conflict"
	StopPaused                 StopReason = "paused"
	StopExhausted              StopReason = "exhausted"
	StopCompleted              StopReason = "completed"
	StopDropped                StopReason = "dropped"
	StopBlocked                StopReason = "blocked"
	StopCortexUnavailable      StopReason = "cortex_unavailable"
	StopNoProgress             StopReason = "no_progress"
	StopUnproductive           StopReason = "unproductive"
	StopBudget                 StopReason = "budget_exhausted"
	StopContinuationDenied     StopReason = "continuation_denied"
	StopObservationStale       StopReason = "observation_stale"
)

// IssueKind is the supervisor-facing projection of one unresolved durable
// control-plane item. It deliberately does not depend on a database package.
type IssueKind string

const (
	IssueApproval       IssueKind = "approval"
	IssueDecision       IssueKind = "decision"
	IssueDependency     IssueKind = "dependency"
	IssueOutcomeUnknown IssueKind = "outcome_unknown"
)

func (k IssueKind) Valid() bool {
	switch k {
	case IssueApproval, IssueDecision, IssueDependency, IssueOutcomeUnknown:
		return true
	default:
		return false
	}
}

// Issue is a bounded identity/explanation supplied by a durable control-plane
// adapter. Decide returns all issue IDs and stops at the highest-risk class.
type Issue struct {
	ID      string    `json:"id"`
	Kind    IssueKind `json:"kind"`
	Summary string    `json:"summary,omitempty"`
}

// EvaluationStatus tells the supervisor what Cortex established for one exact
// settled turn and durable correlation revision.
type EvaluationStatus string

const (
	EvaluationProgressed EvaluationStatus = "progressed"
	EvaluationNoProgress EvaluationStatus = "no_progress"
)

func (s EvaluationStatus) Valid() bool {
	return s == EvaluationProgressed || s == EvaluationNoProgress
}

// EvaluationBasis is the committed supervisor record captured before one turn
// was dispatched. Its revision is not supplied by the evaluation response.
type EvaluationBasis struct {
	RecordID       string    `json:"record_id"`
	GoalID         string    `json:"goal_id"`
	TurnID         string    `json:"turn_id"`
	CortexTaskID   string    `json:"cortex_task_id"`
	CortexRevision int64     `json:"cortex_revision"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// EvaluationBasisReader is the narrow persistence seam needed to establish
// semantic progress. Implementations must return the committed pre-dispatch
// basis for the exact goal and turn; transient UI state is not authoritative.
type EvaluationBasisReader interface {
	EvaluationBasis(ctx context.Context, goalID, turnID string) (EvaluationBasis, error)
}

// EvaluationReceipt binds a Cortex result to the exact last settled turn and
// task. Revision must already be attached to, and durably persisted with, the
// Goal Runtime before PersistenceReady may be true. Progress is measured
// against the separately loaded durable pre-turn EvaluationBasis.
type EvaluationReceipt struct {
	Status       EvaluationStatus `json:"status"`
	TurnID       string           `json:"turn_id"`
	CortexTaskID string           `json:"cortex_task_id"`
	Revision     int64            `json:"revision"`
}

// Observation contains only the external facts needed to choose a next
// operation. LeaseOwned and PersistenceReady must both be true before any
// dispatch/evaluation action is returned.
type Observation struct {
	LeaseOwned       bool                  `json:"lease_owned"`
	PersistenceReady bool                  `json:"persistence_ready"`
	AdvisorAvailable bool                  `json:"advisor_available"`
	RunningTurnID    string                `json:"running_turn_id,omitempty"`
	Manual           bool                  `json:"manual,omitempty"`
	Evaluation       *EvaluationReceipt    `json:"evaluation,omitempty"`
	EvaluationBases  EvaluationBasisReader `json:"-"`
	Issues           []Issue               `json:"issues,omitempty"`
}

// Decision is a non-mutating scheduling receipt. Goal is the exact refreshed
// snapshot on which Action and Reason were based. Goal is zero only when the
// caller did not establish lease ownership and durable persistence first.
type Decision struct {
	Action   Action        `json:"action"`
	Reason   StopReason    `json:"reason,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	IssueIDs []string      `json:"issue_ids,omitempty"`
	Goal     goal.Snapshot `json:"goal"`
}

// Decide chooses one safe next operation. The caller remains responsible for
// committing a continuation permit before dispatch and for persisting every
// later transition.
func Decide(ctx context.Context, runtime *goal.Runtime, observation Observation) (Decision, error) {
	if ctx == nil {
		return Decision{}, fmt.Errorf("%w: context is nil", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if runtime == nil {
		return Decision{}, fmt.Errorf("%w: goal runtime is nil", ErrInvalid)
	}
	if err := validateObservation(observation); err != nil {
		return Decision{}, err
	}
	decision := Decision{Action: ActionStop}
	// Snapshot and CanAutoContinue can materialize wall-time exhaustion inside
	// Goal Runtime. Establish the mutation/persistence authority boundary before
	// either call so a read-only observer cannot create unpersisted goal state.
	if !observation.LeaseOwned {
		return stopped(decision, StopLeaseRequired, "the scoped execution lease is not owned"), nil
	}
	if !observation.PersistenceReady {
		return stopped(decision, StopPersistenceUnavailable, "durable goal persistence is unavailable"), nil
	}
	snapshot, err := runtime.Snapshot(ctx)
	if err != nil {
		return Decision{}, err
	}
	decision.Goal = snapshot
	if issue, ids := highestPriorityIssue(observation.Issues); issue != nil {
		decision.IssueIDs = ids
		switch issue.Kind {
		case IssueOutcomeUnknown:
			return stopped(decision, StopOutcomeUnknown, issue.Summary), nil
		case IssueDecision:
			return stopped(decision, StopDecision, issue.Summary), nil
		case IssueApproval:
			return stopped(decision, StopApproval, issue.Summary), nil
		case IssueDependency:
			return stopped(decision, StopDependency, issue.Summary), nil
		}
	}

	if observation.RunningTurnID != "" {
		if pending := snapshot.PendingContinuation; pending != nil && pending.TurnID != observation.RunningTurnID {
			return stopped(decision, StopTurnIdentityConflict, "running turn does not match the durable continuation permit"), nil
		}
		decision.Action = ActionWaitTurn
		return decision, nil
	}
	if snapshot.PendingContinuation != nil {
		return stopped(decision, StopRecoveryRequired, "a durable continuation permit has no active turn"), nil
	}

	switch snapshot.State {
	case goal.StatePaused:
		if snapshot.LastTurn != nil && !snapshot.LastTurn.Productive {
			return stopped(decision, StopUnproductive, snapshot.StateReason), nil
		}
		return stopped(decision, StopPaused, snapshot.StateReason), nil
	case goal.StateExhausted:
		return stopped(decision, StopExhausted, snapshot.StateReason), nil
	case goal.StateCompleted:
		return stopped(decision, StopCompleted, snapshot.StateReason), nil
	case goal.StateDropped:
		return stopped(decision, StopDropped, snapshot.StateReason), nil
	case goal.StateBlocked:
		reason := StopBlocked
		if snapshot.Blocker != nil {
			switch snapshot.Blocker.Kind {
			case goal.BlockDecision:
				reason = StopDecision
			case goal.BlockDependency:
				reason = StopDependency
			case goal.BlockOutcomeUnknown:
				reason = StopOutcomeUnknown
			}
		}
		return stopped(decision, reason, snapshot.StateReason), nil
	case goal.StateActive:
		// Continue below.
	default:
		return Decision{}, fmt.Errorf("%w: unsupported goal state %q", ErrInvalid, snapshot.State)
	}

	if snapshot.LastTurn == nil {
		decision.Action = ActionDispatchInitial
		return decision, nil
	}
	if observation.Manual && snapshot.Cortex.TaskID == "" {
		decision.Action = ActionDispatchManual
		return decision, nil
	}
	if snapshot.Cortex.TaskID == "" || !observation.AdvisorAvailable {
		return stopped(decision, StopCortexUnavailable, "goal progress requires a linked, available Cortex advisor"), nil
	}
	if observation.Evaluation == nil {
		decision.Action = ActionEvaluate
		return decision, nil
	}
	if err := validateEvaluationBinding(ctx, snapshot, *observation.Evaluation, observation.EvaluationBases); err != nil {
		return Decision{}, err
	}
	if observation.Manual {
		decision.Action = ActionDispatchManual
		return decision, nil
	}
	switch observation.Evaluation.Status {
	case EvaluationNoProgress:
		return stopped(decision, StopNoProgress, "Cortex recorded no semantic progress for the last turn"), nil
	case EvaluationProgressed:
		// The refreshed Goal Runtime decides whether the host may continue.
	default:
		return Decision{}, fmt.Errorf("%w: unsupported evaluation status %q", ErrInvalid, observation.Evaluation.Status)
	}

	continuation, err := runtime.CanAutoContinue(ctx)
	if err != nil {
		return Decision{}, err
	}
	refreshed, err := runtime.Snapshot(ctx)
	if err != nil {
		return Decision{}, err
	}
	if !reflect.DeepEqual(snapshot, refreshed) {
		decision.Goal = refreshed
		return stopped(decision, StopObservationStale, "goal changed while choosing an automatic continuation; observe it again"), nil
	}
	decision.Goal = refreshed
	if continuation.Allowed {
		decision.Action = ActionDispatchContinuation
		return decision, nil
	}
	switch continuation.Reason {
	case goal.ContinuationUnproductive:
		return stopped(decision, StopUnproductive, "the last settled turn made no concrete progress"), nil
	case goal.ContinuationBudget:
		return stopped(decision, StopBudget, snapshot.StateReason), nil
	case goal.ContinuationOutcomeUnknown:
		return stopped(decision, StopOutcomeUnknown, snapshot.StateReason), nil
	case goal.ContinuationBlocked:
		return stopped(decision, StopBlocked, snapshot.StateReason), nil
	case goal.ContinuationTurnPending:
		return stopped(decision, StopRecoveryRequired, "a continuation permit requires settlement or recovery"), nil
	default:
		return stopped(decision, StopContinuationDenied, string(continuation.Reason)), nil
	}
}

func stopped(decision Decision, reason StopReason, detail string) Decision {
	decision.Action = ActionStop
	decision.Reason = reason
	decision.Detail = strings.TrimSpace(detail)
	return decision
}

func validateObservation(observation Observation) error {
	if observation.RunningTurnID != "" {
		if !utf8.ValidString(observation.RunningTurnID) || strings.TrimSpace(observation.RunningTurnID) != observation.RunningTurnID {
			return fmt.Errorf("%w: running turn id is not canonical UTF-8", ErrInvalid)
		}
		if len(observation.RunningTurnID) > goal.MaxTurnIDBytes {
			return fmt.Errorf("%w: running turn id exceeds %d bytes", ErrInvalid, goal.MaxTurnIDBytes)
		}
	}
	if len(observation.Issues) > MaxIssues {
		return fmt.Errorf("%w: issues exceed %d items", ErrInvalid, MaxIssues)
	}
	seen := make(map[string]struct{}, len(observation.Issues))
	for _, issue := range observation.Issues {
		if !utf8.ValidString(issue.ID) || strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(issue.ID) != issue.ID || len(issue.ID) > goal.MaxCorrelationIDBytes {
			return fmt.Errorf("%w: issue id is missing or too long", ErrInvalid)
		}
		if _, exists := seen[issue.ID]; exists {
			return fmt.Errorf("%w: duplicate issue id %q", ErrInvalid, issue.ID)
		}
		seen[issue.ID] = struct{}{}
		if !issue.Kind.Valid() {
			return fmt.Errorf("%w: invalid issue kind %q", ErrInvalid, issue.Kind)
		}
		if !utf8.ValidString(issue.Summary) || strings.TrimSpace(issue.Summary) == "" || len(issue.Summary) > goal.MaxReasonBytes {
			return fmt.Errorf("%w: issue summary exceeds %d bytes", ErrInvalid, goal.MaxReasonBytes)
		}
	}
	if observation.Evaluation != nil {
		if err := validateEvaluationReceipt(*observation.Evaluation); err != nil {
			return err
		}
	}
	return nil
}

func validateEvaluationReceipt(receipt EvaluationReceipt) error {
	if !receipt.Status.Valid() {
		return fmt.Errorf("%w: invalid evaluation status %q", ErrInvalid, receipt.Status)
	}
	if !utf8.ValidString(receipt.TurnID) || strings.TrimSpace(receipt.TurnID) == "" || strings.TrimSpace(receipt.TurnID) != receipt.TurnID || len(receipt.TurnID) > goal.MaxTurnIDBytes {
		return fmt.Errorf("%w: evaluation turn id is invalid", ErrInvalid)
	}
	if !utf8.ValidString(receipt.CortexTaskID) || strings.TrimSpace(receipt.CortexTaskID) == "" || strings.TrimSpace(receipt.CortexTaskID) != receipt.CortexTaskID || len(receipt.CortexTaskID) > goal.MaxCorrelationIDBytes {
		return fmt.Errorf("%w: evaluation Cortex task id is invalid", ErrInvalid)
	}
	if receipt.Revision < 0 {
		return fmt.Errorf("%w: evaluation revision must not be negative", ErrInvalid)
	}
	return nil
}

func validateEvaluationBinding(ctx context.Context, snapshot goal.Snapshot, receipt EvaluationReceipt, reader EvaluationBasisReader) error {
	if snapshot.LastTurn == nil {
		return fmt.Errorf("%w: evaluation has no settled goal turn", ErrInvalid)
	}
	if receipt.TurnID != snapshot.LastTurn.TurnID {
		return fmt.Errorf("%w: evaluation turn %q does not match last settled turn %q", ErrInvalid, receipt.TurnID, snapshot.LastTurn.TurnID)
	}
	if receipt.CortexTaskID != snapshot.Cortex.TaskID {
		return fmt.Errorf("%w: evaluation Cortex task does not match durable goal correlation", ErrInvalid)
	}
	if receipt.Revision != snapshot.Cortex.Revision {
		return fmt.Errorf("%w: evaluation revision %d is not the durable Cortex revision %d", ErrInvalid, receipt.Revision, snapshot.Cortex.Revision)
	}
	if reader == nil {
		return fmt.Errorf("%w: durable pre-turn evaluation basis is unavailable", ErrInvalid)
	}
	basis, err := reader.EvaluationBasis(ctx, snapshot.ID, snapshot.LastTurn.TurnID)
	if err != nil {
		return fmt.Errorf("read durable evaluation basis: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateEvaluationBasis(snapshot, basis); err != nil {
		return err
	}
	switch receipt.Status {
	case EvaluationProgressed:
		if receipt.Revision <= basis.CortexRevision {
			return fmt.Errorf("%w: progressed evaluation did not advance durable Cortex revision", ErrInvalid)
		}
	case EvaluationNoProgress:
		if receipt.Revision != basis.CortexRevision {
			return fmt.Errorf("%w: no-progress evaluation differs from durable Cortex revision", ErrInvalid)
		}
	}
	return nil
}

func validateEvaluationBasis(snapshot goal.Snapshot, basis EvaluationBasis) error {
	if snapshot.LastTurn == nil {
		return fmt.Errorf("%w: durable evaluation basis has no settled turn", ErrInvalid)
	}
	for name, value := range map[string]string{
		"record id": basis.RecordID, "goal id": basis.GoalID,
		"turn id": basis.TurnID, "Cortex task id": basis.CortexTaskID,
	} {
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || len(value) > goal.MaxCorrelationIDBytes {
			return fmt.Errorf("%w: durable evaluation basis %s is invalid", ErrInvalid, name)
		}
	}
	if len(basis.TurnID) > goal.MaxTurnIDBytes || basis.CortexRevision < 0 || basis.RecordedAt.IsZero() {
		return fmt.Errorf("%w: durable evaluation basis metadata is invalid", ErrInvalid)
	}
	if basis.GoalID != snapshot.ID || basis.TurnID != snapshot.LastTurn.TurnID || basis.CortexTaskID != snapshot.Cortex.TaskID {
		return fmt.Errorf("%w: durable evaluation basis does not match the goal turn", ErrInvalid)
	}
	if basis.RecordedAt.After(snapshot.LastTurn.RecordedAt) {
		return fmt.Errorf("%w: durable evaluation basis was recorded after the settled turn", ErrInvalid)
	}
	return nil
}

func highestPriorityIssue(issues []Issue) (*Issue, []string) {
	if len(issues) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(issues))
	priority := func(kind IssueKind) int {
		switch kind {
		case IssueOutcomeUnknown:
			return 4
		case IssueDecision:
			return 3
		case IssueApproval:
			return 2
		case IssueDependency:
			return 1
		default:
			return 0
		}
	}
	selected := 0
	for index := range issues {
		ids = append(ids, issues[index].ID)
		if priority(issues[index].Kind) > priority(issues[selected].Kind) {
			selected = index
		}
	}
	issue := issues[selected]
	return &issue, ids
}
