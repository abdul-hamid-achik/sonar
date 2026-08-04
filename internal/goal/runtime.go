package goal

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Clock makes wall-time exhaustion deterministic in tests and embeddings.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type runtimeOptions struct {
	clock Clock
}

// Option configures a Runtime without changing its durable schema.
type Option func(*runtimeOptions) error

// WithClock installs the clock used for timestamps and wall-time budgets.
func WithClock(clock Clock) Option {
	return func(options *runtimeOptions) error {
		if clock == nil {
			return fmt.Errorf("%w: clock is nil", ErrInvalid)
		}
		options.clock = clock
		return nil
	}
}

// Runtime is the concurrency-safe host authority for one durable session goal.
// It deliberately has no persistence dependency; callers atomically store the
// Snapshot alongside the owning session.
type Runtime struct {
	mu    sync.RWMutex
	state Snapshot
	clock Clock
}

// New creates one active goal bound to Spec.SessionID.
func New(spec Spec, options ...Option) (*Runtime, error) {
	if err := validateSpec(spec, false); err != nil {
		return nil, err
	}
	if spec.ID == "" {
		var err error
		spec.ID, err = NewGoalID()
		if err != nil {
			return nil, err
		}
	}
	if err := validateText("goal id", spec.ID, MaxGoalIDBytes, true); err != nil {
		return nil, err
	}
	configured, err := applyOptions(options)
	if err != nil {
		return nil, err
	}
	now := configured.clock.Now().UTC()
	if now.IsZero() {
		return nil, fmt.Errorf("%w: clock returned zero time", ErrInvalid)
	}
	return &Runtime{
		clock: configured.clock,
		state: Snapshot{
			Version:            SnapshotVersion,
			ID:                 spec.ID,
			SessionID:          spec.SessionID,
			Objective:          spec.Objective,
			AcceptanceCriteria: cloneCriteria(spec.AcceptanceCriteria),
			State:              StateActive,
			Budget:             spec.Budget,
			Cortex:             spec.Cortex,
			CreatedAt:          now,
			UpdatedAt:          now,
		},
	}, nil
}

// Restore validates and resumes one previously persisted snapshot. If wall
// time elapsed while the process was absent, the restored goal becomes
// exhausted rather than being silently completed or continued.
func Restore(snapshot Snapshot, options ...Option) (*Runtime, error) {
	configured, err := applyOptions(options)
	if err != nil {
		return nil, err
	}
	copy := cloneSnapshot(snapshot)
	if err := migrateLegacySnapshot(&copy); err != nil {
		return nil, err
	}
	if err := validateSnapshot(copy); err != nil {
		return nil, err
	}
	if configured.clock.Now().IsZero() {
		return nil, fmt.Errorf("%w: clock returned zero time", ErrInvalid)
	}
	runtime := &Runtime{state: copy, clock: configured.clock}
	runtime.mu.Lock()
	runtime.refreshBudgetLocked(runtime.nowLocked())
	runtime.mu.Unlock()
	return runtime, nil
}

func applyOptions(options []Option) (runtimeOptions, error) {
	configured := runtimeOptions{clock: systemClock{}}
	for _, option := range options {
		if option == nil {
			return configured, fmt.Errorf("%w: nil runtime option", ErrInvalid)
		}
		if err := option(&configured); err != nil {
			return configured, err
		}
	}
	return configured, nil
}

// Snapshot returns an isolated copy suitable for durable JSON persistence.
func (r *Runtime) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshBudgetLocked(r.nowLocked())
	return cloneSnapshot(r.state), nil
}

// CanAutoContinue reports whether the host may start a continuation without a
// new user instruction. It also materializes wall-time exhaustion.
func (r *Runtime) CanAutoContinue(ctx context.Context) (ContinuationDecision, error) {
	if err := contextError(ctx); err != nil {
		return ContinuationDecision{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshBudgetLocked(r.nowLocked())
	return r.continuationDecisionLocked(), nil
}

// BeginTurn durably admits one provider turn. Every new turn must settle this
// exact admission through RecordTurn or RecoverPendingContinuation. Initial and
// manual turns do not consume the automatic continuation budget.
func (r *Runtime) BeginTurn(ctx context.Context, turnID string, kind TurnAdmissionKind) (ContinuationPermit, error) {
	if err := contextError(ctx); err != nil {
		return ContinuationPermit{}, err
	}
	if err := validateText("turn id", turnID, MaxTurnIDBytes, true); err != nil {
		return ContinuationPermit{}, err
	}
	if !kind.Valid() {
		return ContinuationPermit{}, fmt.Errorf("%w: invalid turn admission kind %q", ErrInvalid, kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if pending := r.state.PendingContinuation; pending != nil {
		return ContinuationPermit{}, fmt.Errorf("%w: %s requires settlement or recovery", ErrTurnPending, pending.TurnID)
	}
	if last := r.state.LastTurn; last != nil && last.TurnID == turnID {
		return ContinuationPermit{}, fmt.Errorf("%w: turn id %s already has a settled receipt", ErrTurnConflict, turnID)
	}
	if recovered := r.state.LastPendingRecovery; recovered != nil && recovered.Permit.TurnID == turnID {
		return ContinuationPermit{}, fmt.Errorf("%w: recovered turn id %s cannot be redispatched", ErrTurnConflict, turnID)
	}
	now := r.nowLocked()
	r.refreshBudgetLocked(now)

	switch kind {
	case AdmissionInitial:
		if r.state.State == StateExhausted || len(r.state.ExhaustedBy) > 0 {
			return ContinuationPermit{}, fmt.Errorf("%w: %s", ErrBudgetExhausted, strings.Join(budgetDimensionStrings(r.state.ExhaustedBy), ", "))
		}
		if r.state.State != StateActive || r.state.LastTurn != nil {
			return ContinuationPermit{}, fmt.Errorf("%w: initial turn requires an active goal without a prior receipt", ErrIllegalTransition)
		}
	case AdmissionManual:
		if r.state.State == StateExhausted || len(r.state.ExhaustedBy) > 0 {
			return ContinuationPermit{}, fmt.Errorf("%w: %s", ErrBudgetExhausted, strings.Join(budgetDimensionStrings(r.state.ExhaustedBy), ", "))
		}
		if r.state.State != StateActive || r.state.LastTurn == nil {
			return ContinuationPermit{}, fmt.Errorf("%w: manual turn requires an active goal with a prior receipt", ErrIllegalTransition)
		}
	case AdmissionAutomatic:
		decision := r.continuationDecisionLocked()
		if !decision.Allowed {
			if decision.Reason == ContinuationBudget {
				return ContinuationPermit{}, fmt.Errorf("%w: %s", ErrBudgetExhausted, strings.Join(budgetDimensionStrings(r.state.ExhaustedBy), ", "))
			}
			return ContinuationPermit{}, fmt.Errorf("%w: %s", ErrAutoContinuationDenied, decision.Reason)
		}
		if r.state.Usage.ContinuationTurns == math.MaxInt64 {
			return ContinuationPermit{}, fmt.Errorf("%w: continuation-turn usage overflow", ErrInvalid)
		}
		r.state.Usage.ContinuationTurns++
	}

	ordinal := int64(0)
	if kind == AdmissionAutomatic {
		ordinal = r.state.Usage.ContinuationTurns
	}
	permit := &ContinuationPermit{
		TurnID:    turnID,
		Kind:      kind,
		Ordinal:   ordinal,
		GrantedAt: now,
	}
	r.state.PendingContinuation = permit
	r.state.UpdatedAt = now
	r.refreshBudgetLocked(now)
	return *permit, nil
}

// BeginContinuation is the compatibility wrapper for callers that explicitly
// request an automatic continuation.
func (r *Runtime) BeginContinuation(ctx context.Context, turnID string) (ContinuationPermit, error) {
	return r.BeginTurn(ctx, turnID, AdmissionAutomatic)
}

// RecoverPendingContinuation resolves a turn admission left without a receipt.
// The legacy method name remains stable for embeddings. Recovery never
// redispatches and never refunds automatic continuation usage. A proven
// pre-dispatch cancellation lands paused (or exhausted); any uncertainty lands
// blocked until ResolveBlock records reconciliation.
func (r *Runtime) RecoverPendingContinuation(ctx context.Context, recovery PendingRecovery) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validatePendingRecovery(recovery); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.PendingContinuation == nil {
		if previous := r.state.LastPendingRecovery; previous != nil && reflect.DeepEqual(previous.Recovery, recovery) {
			return nil
		}
		return fmt.Errorf("%w: no turn admission is pending", ErrIllegalTransition)
	}
	if recovery.TurnID != r.state.PendingContinuation.TurnID {
		return fmt.Errorf("%w: recovery %s does not match %s", ErrTurnPending, recovery.TurnID, r.state.PendingContinuation.TurnID)
	}
	now := r.nowLocked()
	permit := *r.state.PendingContinuation
	r.state.LastPendingRecovery = &PendingRecoveryRecord{
		Permit:      permit,
		Recovery:    recovery,
		RecoveredAt: now,
	}
	r.state.PendingContinuation = nil
	r.state.UpdatedAt = now
	r.state.ExhaustedBy = exhaustedDimensions(r.state, now)

	if recovery.Kind == PendingOutcomeUnknown {
		r.state.State = StateBlocked
		r.state.StateReason = recovery.Reason
		r.state.Blocker = &Blocker{
			Kind:      BlockOutcomeUnknown,
			Reference: recovery.OutcomeRef,
			Reason:    recovery.Reason,
			BlockedAt: now,
		}
		return nil
	}
	if len(r.state.ExhaustedBy) > 0 {
		r.state.State = StateExhausted
		r.state.StateReason = "turn cancelled before dispatch; budget exhausted"
		return nil
	}
	r.state.State = StatePaused
	r.state.StateReason = "turn cancelled before dispatch: " + recovery.Reason
	return nil
}

// RecordTurn commits the settled receipt for an exactly admitted initial,
// manual, or automatic turn. Duplicate identical receipts are idempotent.
func (r *Runtime) RecordTurn(ctx context.Context, report TurnReport) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateTurnReport(report); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.State.Terminal() {
		return ErrTerminal
	}
	if last := r.state.LastTurn; last != nil && last.TurnID == report.TurnID {
		if pending := r.state.PendingContinuation; pending != nil {
			return fmt.Errorf("%w: receipt %s does not settle %s", ErrTurnPending, report.TurnID, pending.TurnID)
		}
		if reflect.DeepEqual(last.TurnReport, report) {
			return nil
		}
		return fmt.Errorf("%w: duplicate turn id %s", ErrTurnConflict, report.TurnID)
	}
	pending := r.state.PendingContinuation
	if pending == nil {
		return fmt.Errorf("%w: receipt %s has no durable turn admission", ErrTurnPending, report.TurnID)
	}
	if pending.TurnID != report.TurnID {
		return fmt.Errorf("%w: receipt %s does not settle %s", ErrTurnPending, report.TurnID, pending.TurnID)
	}
	if r.state.State == StateBlocked {
		return fmt.Errorf("%w: resolve the current blocker before recording another turn", ErrIllegalTransition)
	}
	if r.state.State == StatePaused {
		return fmt.Errorf("%w: resume the paused goal before recording another turn", ErrIllegalTransition)
	}
	if report.EvalTokens > math.MaxInt64-r.state.Usage.EvalTokens {
		return fmt.Errorf("%w: eval-token usage overflow", ErrInvalid)
	}
	now := r.nowLocked()
	r.state.Usage.EvalTokens += report.EvalTokens
	r.state.LastTurn = &TurnReceipt{TurnReport: report, RecordedAt: now}
	r.state.PendingContinuation = nil
	r.state.UpdatedAt = now

	if report.OutcomeUnknown {
		r.state.State = StateBlocked
		r.state.StateReason = report.Summary
		r.state.Blocker = &Blocker{
			Kind:      BlockOutcomeUnknown,
			Reference: report.OutcomeRef,
			Reason:    report.Summary,
			BlockedAt: now,
		}
		r.state.ExhaustedBy = exhaustedDimensions(r.state, now)
		return nil
	}

	r.state.ExhaustedBy = exhaustedDimensions(r.state, now)
	if len(r.state.ExhaustedBy) > 0 {
		r.state.State = StateExhausted
		r.state.StateReason = "budget exhausted: " + strings.Join(budgetDimensionStrings(r.state.ExhaustedBy), ", ")
		return nil
	}
	if !report.Productive {
		r.state.State = StatePaused
		r.state.StateReason = "unproductive turn: " + report.Summary
		return nil
	}
	r.state.State = StateActive
	r.state.StateReason = ""
	return nil
}

// Pause explicitly suspends an active goal.
func (r *Runtime) Pause(ctx context.Context, reason string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateText("pause reason", reason, MaxReasonBytes, true); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowLocked()
	r.refreshBudgetLocked(now)
	if r.state.State.Terminal() {
		return ErrTerminal
	}
	if r.state.State != StateActive || r.state.PendingContinuation != nil {
		return fmt.Errorf("%w: cannot pause from %s", ErrIllegalTransition, r.state.State)
	}
	r.state.State = StatePaused
	r.state.StateReason = reason
	r.state.UpdatedAt = now
	return nil
}

// Resume explicitly reactivates a paused or replenished exhausted goal. It
// does not make an unproductive last turn eligible for automatic continuation.
func (r *Runtime) Resume(ctx context.Context, reason string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateText("resume reason", reason, MaxReasonBytes, true); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowLocked()
	r.refreshBudgetLocked(now)
	if r.state.State.Terminal() {
		return ErrTerminal
	}
	if r.state.State != StatePaused && r.state.State != StateExhausted {
		return fmt.Errorf("%w: cannot resume from %s", ErrIllegalTransition, r.state.State)
	}
	if len(exhaustedDimensions(r.state, now)) > 0 {
		return ErrBudgetExhausted
	}
	if r.state.Blocker != nil {
		return fmt.Errorf("%w: blocker must be resolved first", ErrIllegalTransition)
	}
	r.state.State = StateActive
	r.state.StateReason = ""
	r.state.ExhaustedBy = nil
	r.state.UpdatedAt = now
	return nil
}

// AmendBudget replaces the absolute caps after an explicit host decision.
// Making room does not reactivate an exhausted goal; Resume remains explicit.
func (r *Runtime) AmendBudget(ctx context.Context, limits BudgetLimits, reason string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := limits.Validate(); err != nil {
		return err
	}
	if err := validateText("budget amendment reason", reason, MaxReasonBytes, true); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.State.Terminal() {
		return ErrTerminal
	}
	now := r.nowLocked()
	r.state.Budget = limits
	r.state.UpdatedAt = now
	r.state.ExhaustedBy = exhaustedDimensions(r.state, now)
	if len(r.state.ExhaustedBy) > 0 && r.state.State != StateBlocked {
		r.state.State = StateExhausted
		r.state.StateReason = "budget exhausted: " + strings.Join(budgetDimensionStrings(r.state.ExhaustedBy), ", ")
	}
	return nil
}

// AttachCortex records a monotonic Cortex revision for this goal.
func (r *Runtime) AttachCortex(ctx context.Context, correlation CortexCorrelation) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := correlation.Validate(); err != nil {
		return err
	}
	if correlation.Empty() {
		return fmt.Errorf("%w: cortex correlation is empty", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.State.Terminal() {
		return ErrTerminal
	}
	current := r.state.Cortex
	if !current.Empty() {
		if current.TaskID != correlation.TaskID || current.Actor != correlation.Actor {
			return ErrCorrelationConflict
		}
		if correlation.Revision < current.Revision {
			return fmt.Errorf("%w: revision regressed from %d to %d", ErrCorrelationConflict, current.Revision, correlation.Revision)
		}
	}
	if current == correlation {
		return nil
	}
	r.state.Cortex = correlation
	r.state.UpdatedAt = r.nowLocked()
	return nil
}

// Block records a host-observed blocker. Outcome-unknown callers should prefer
// a TurnReport so token usage and the terminal turn receipt remain atomic.
func (r *Runtime) Block(ctx context.Context, blocker Blocker) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateBlocker(blocker, false); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.State.Terminal() {
		return ErrTerminal
	}
	if r.state.PendingContinuation != nil {
		return fmt.Errorf("%w: settle the pending turn admission before blocking", ErrTurnPending)
	}
	if r.state.State == StateBlocked {
		if r.state.Blocker != nil && sameBlocker(*r.state.Blocker, blocker) {
			return nil
		}
		return fmt.Errorf("%w: goal is already blocked", ErrIllegalTransition)
	}
	now := r.nowLocked()
	blocker.BlockedAt = now
	r.state.State = StateBlocked
	r.state.StateReason = blocker.Reason
	r.state.Blocker = &blocker
	r.state.ExhaustedBy = exhaustedDimensions(r.state, now)
	r.state.UpdatedAt = now
	return nil
}

// ResolveBlock records explicit recovery for dependency and decision blockers.
// Outcome-unknown recovery is deliberately rejected: only
// ApplyVerifiedReconciliation may apply its typed, atomically persisted control
// receipt. Resolution always lands in paused or exhausted state, so it cannot
// restart work without a separate Resume call.
func (r *Runtime) ResolveBlock(ctx context.Context, resolution BlockResolution) error {
	return r.resolveBlock(ctx, resolution, false)
}

// ApplyVerifiedReconciliation applies one repository-verified recovery receipt
// to an isolated durable snapshot. Persistence coordinators call this pure
// transition inside the same transaction that appends the final control-plane
// resolution. It never mutates a caller-owned Runtime and never resumes work.
func ApplyVerifiedReconciliation(ctx context.Context, snapshot Snapshot, resolution BlockResolution, resolvedAt time.Time) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if resolvedAt.IsZero() {
		return Snapshot{}, fmt.Errorf("%w: reconciliation time is required", ErrInvalid)
	}
	resolvedAt = resolvedAt.UTC()
	if resolvedAt.Before(snapshot.UpdatedAt) {
		return Snapshot{}, fmt.Errorf("%w: reconciliation predates the durable goal snapshot", ErrInvalid)
	}
	runtime, err := Restore(snapshot, WithClock(fixedClock{now: resolvedAt}))
	if err != nil {
		return Snapshot{}, err
	}
	if err := runtime.resolveBlock(ctx, resolution, true); err != nil {
		return Snapshot{}, err
	}
	return runtime.Snapshot(ctx)
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func (r *Runtime) resolveBlock(ctx context.Context, resolution BlockResolution, verified bool) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateText("block resolution reason", resolution.Reason, MaxReasonBytes, true); err != nil {
		return err
	}
	if err := validateText("block resolution reference", resolution.Reference, MaxCorrelationIDBytes, true); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.State != StateBlocked || r.state.Blocker == nil {
		return fmt.Errorf("%w: goal is not blocked", ErrIllegalTransition)
	}
	if resolution.Reference != r.state.Blocker.Reference {
		return fmt.Errorf("%w: blocker reference does not match", ErrIllegalTransition)
	}
	if r.state.Blocker.Kind == BlockOutcomeUnknown {
		if !verified {
			return ErrOutcomeUnknown
		}
		if !resolution.Reconciled {
			return ErrOutcomeUnknown
		}
		if err := validateText("outcome reconciliation evidence", resolution.Evidence, MaxEvidenceBytes, true); err != nil {
			return fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
		}
		if resolution.Reconciliation == nil {
			return fmt.Errorf("%w: typed reconciliation receipt is required", ErrOutcomeUnknown)
		}
		if err := resolution.Reconciliation.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
		}
	} else if resolution.Reconciled || resolution.Reconciliation != nil {
		return fmt.Errorf("%w: reconciliation evidence requires an outcome-unknown blocker", ErrInvalid)
	}
	now := r.nowLocked()
	resolved := *r.state.Blocker
	r.state.LastBlockResolution = &BlockResolutionRecord{
		Blocker:    resolved,
		Resolution: resolution,
		ResolvedAt: now,
	}
	r.state.Blocker = nil
	r.state.ExhaustedBy = exhaustedDimensions(r.state, now)
	if len(r.state.ExhaustedBy) > 0 {
		r.state.State = StateExhausted
		r.state.StateReason = "block resolved; budget exhausted"
	} else {
		r.state.State = StatePaused
		r.state.StateReason = "block resolved: " + resolution.Reason
	}
	r.state.UpdatedAt = now
	return nil
}

// Complete records explicit host validation of every acceptance criterion.
func (r *Runtime) Complete(ctx context.Context, request CompletionRequest) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.State.Terminal() {
		return ErrTerminal
	}
	if r.state.State == StateBlocked || r.state.Blocker != nil {
		if r.state.Blocker != nil && r.state.Blocker.Kind == BlockOutcomeUnknown {
			return ErrOutcomeUnknown
		}
		return ErrBlocked
	}
	if r.state.PendingContinuation != nil {
		return ErrTurnPending
	}
	ordered, err := validateCompletion(r.state.AcceptanceCriteria, request)
	if err != nil {
		return err
	}
	now := r.nowLocked()
	request.Results = ordered
	r.state.State = StateCompleted
	r.state.StateReason = request.Summary
	r.state.Completion = &CompletionRecord{CompletionRequest: request, CompletedAt: now}
	r.state.UpdatedAt = now
	return nil
}

// Drop explicitly abandons a non-terminal goal. Existing blocker information
// remains in the snapshot as audit context.
func (r *Runtime) Drop(ctx context.Context, reason string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateText("drop reason", reason, MaxReasonBytes, true); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.State.Terminal() {
		return ErrTerminal
	}
	if r.state.PendingContinuation != nil {
		return ErrTurnPending
	}
	r.state.State = StateDropped
	r.state.StateReason = reason
	r.state.UpdatedAt = r.nowLocked()
	return nil
}

func (r *Runtime) continuationDecisionLocked() ContinuationDecision {
	if r.state.State == StateBlocked || r.state.Blocker != nil {
		if r.state.Blocker != nil && r.state.Blocker.Kind == BlockOutcomeUnknown {
			return ContinuationDecision{Reason: ContinuationOutcomeUnknown}
		}
		return ContinuationDecision{Reason: ContinuationBlocked}
	}
	if r.state.PendingContinuation != nil {
		return ContinuationDecision{Reason: ContinuationTurnPending}
	}
	if r.state.State == StateExhausted || len(r.state.ExhaustedBy) > 0 {
		return ContinuationDecision{Reason: ContinuationBudget}
	}
	if r.state.LastTurn == nil {
		return ContinuationDecision{Reason: ContinuationNoTurnReceipt}
	}
	if !r.state.LastTurn.Productive {
		return ContinuationDecision{Reason: ContinuationUnproductive}
	}
	if r.state.State != StateActive {
		return ContinuationDecision{Reason: ContinuationNotActive}
	}
	return ContinuationDecision{Allowed: true, Reason: ContinuationAllowed}
}

func (r *Runtime) refreshBudgetLocked(now time.Time) {
	dimensions := exhaustedDimensions(r.state, now)
	r.state.ExhaustedBy = dimensions
	if len(dimensions) == 0 || r.state.State.Terminal() || r.state.State == StateBlocked {
		return
	}
	if r.state.State != StateExhausted {
		r.state.State = StateExhausted
		r.state.StateReason = "budget exhausted: " + strings.Join(budgetDimensionStrings(dimensions), ", ")
		r.state.UpdatedAt = now
	}
}

func exhaustedDimensions(snapshot Snapshot, now time.Time) []BudgetDimension {
	result := make([]BudgetDimension, 0, 3)
	if snapshot.Budget.MaxContinuationTurns > 0 && snapshot.Usage.ContinuationTurns >= snapshot.Budget.MaxContinuationTurns {
		result = append(result, BudgetContinuationTurns)
	}
	if snapshot.Budget.MaxEvalTokens > 0 && snapshot.Usage.EvalTokens >= snapshot.Budget.MaxEvalTokens {
		result = append(result, BudgetEvalTokens)
	}
	if snapshot.Budget.MaxWallTime > 0 {
		elapsed := now.Sub(snapshot.CreatedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		if elapsed >= snapshot.Budget.MaxWallTime {
			result = append(result, BudgetWallTime)
		}
	}
	return result
}

func budgetDimensionStrings(values []BudgetDimension) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}

func (r *Runtime) nowLocked() time.Time {
	now := r.clock.Now().UTC()
	if !r.state.UpdatedAt.IsZero() && now.Before(r.state.UpdatedAt) {
		return r.state.UpdatedAt
	}
	return now
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalid)
	}
	return ctx.Err()
}

func validateTurnReport(report TurnReport) error {
	if err := validateText("turn id", report.TurnID, MaxTurnIDBytes, true); err != nil {
		return err
	}
	if report.EvalTokens < 0 {
		return fmt.Errorf("%w: eval tokens must not be negative", ErrInvalid)
	}
	if err := validateText("turn summary", report.Summary, MaxReasonBytes, true); err != nil {
		return err
	}
	if report.OutcomeUnknown {
		if report.Productive {
			return fmt.Errorf("%w: outcome-unknown turn cannot claim productive progress", ErrInvalid)
		}
		if err := validateText("outcome reference", report.OutcomeRef, MaxCorrelationIDBytes, true); err != nil {
			return err
		}
	} else if report.OutcomeRef != "" {
		return fmt.Errorf("%w: outcome reference requires outcome_unknown", ErrInvalid)
	}
	return nil
}

func validatePendingRecovery(recovery PendingRecovery) error {
	if err := validateText("pending recovery turn id", recovery.TurnID, MaxTurnIDBytes, true); err != nil {
		return err
	}
	if !recovery.Kind.Valid() {
		return fmt.Errorf("%w: invalid pending recovery kind %q", ErrInvalid, recovery.Kind)
	}
	if err := validateText("pending recovery reason", recovery.Reason, MaxReasonBytes, true); err != nil {
		return err
	}
	if err := validateText("pending recovery evidence", recovery.Evidence, MaxEvidenceBytes, true); err != nil {
		return err
	}
	if recovery.Kind == PendingOutcomeUnknown {
		return validateText("pending outcome reference", recovery.OutcomeRef, MaxCorrelationIDBytes, true)
	}
	if recovery.OutcomeRef != "" {
		return fmt.Errorf("%w: outcome reference requires outcome-unknown recovery", ErrInvalid)
	}
	return nil
}

func migrateLegacySnapshot(snapshot *Snapshot) error {
	if snapshot == nil {
		return fmt.Errorf("%w: goal snapshot is nil", ErrInvalid)
	}
	switch snapshot.Version {
	case SnapshotVersion:
		return nil
	case LegacySnapshotVersion:
		// Version 1 could contain host-authored outcome evidence with no durable
		// control receipt. Prose alone cannot be upgraded into authority.
		if record := snapshot.LastBlockResolution; record != nil && record.Kind == BlockOutcomeUnknown && record.Resolution.Reconciliation == nil {
			return fmt.Errorf("%w: legacy outcome reconciliation has no durable control receipt", ErrInvalid)
		}
		migrateLegacyTurnAdmissions(snapshot)
		snapshot.Version = SnapshotVersion
		return nil
	default:
		return fmt.Errorf("%w: unsupported goal snapshot version %d", ErrInvalid, snapshot.Version)
	}
}

func migrateLegacyTurnAdmissions(snapshot *Snapshot) {
	// Before admission kinds existed, the only persisted permit was an
	// automatic continuation and therefore always had a positive continuation
	// ordinal. Do not infer a kind for zero-ordinal data: that would let a
	// stripped new initial/manual kind pass as legacy.
	if pending := snapshot.PendingContinuation; pending != nil && pending.Kind == "" && pending.Ordinal > 0 {
		pending.Kind = AdmissionAutomatic
	}
	if recovery := snapshot.LastPendingRecovery; recovery != nil && recovery.Permit.Kind == "" && recovery.Permit.Ordinal > 0 {
		recovery.Permit.Kind = AdmissionAutomatic
	}
}

// Validate checks the shape of a typed control-plane binding without claiming
// that its referenced rows exist. The atomic reconciliation coordinator owns
// that database cross-check.
func (r ReconciliationReceipt) Validate() error {
	if r.Version != ReconciliationReceiptVersion {
		return fmt.Errorf("%w: unsupported reconciliation receipt version %d", ErrInvalid, r.Version)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"reconciliation group item id", r.GroupItemID},
		{"reconciliation final item id", r.FinalItemID},
		{"reconciliation final resolution id", r.FinalResolutionID},
	} {
		if err := validateText(field.name, field.value, MaxGoalIDBytes, true); err != nil {
			return err
		}
		if strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("%w: %s is not canonical", ErrInvalid, field.name)
		}
	}
	if !validLowerSHA256(r.ResolutionSetSHA256) {
		return fmt.Errorf("%w: reconciliation resolution-set SHA-256 is invalid", ErrInvalid)
	}
	if r.TargetCount <= 0 || r.TargetCount > MaxReconciliationTargets {
		return fmt.Errorf("%w: reconciliation target count must be between 1 and %d", ErrInvalid, MaxReconciliationTargets)
	}
	return nil
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func validateTurnAdmission(name string, admission ContinuationPermit, continuationUsage int64) error {
	if err := validateText(name+" turn id", admission.TurnID, MaxTurnIDBytes, true); err != nil {
		return err
	}
	if !admission.Kind.Valid() {
		return fmt.Errorf("%w: invalid %s kind %q", ErrInvalid, name, admission.Kind)
	}
	switch admission.Kind {
	case AdmissionAutomatic:
		if admission.Ordinal <= 0 || admission.Ordinal > continuationUsage {
			return fmt.Errorf("%w: invalid %s continuation ordinal", ErrInvalid, name)
		}
	case AdmissionInitial, AdmissionManual:
		if admission.Ordinal != 0 {
			return fmt.Errorf("%w: %s %s admission cannot consume continuation budget", ErrInvalid, name, admission.Kind)
		}
	}
	if admission.GrantedAt.IsZero() {
		return fmt.Errorf("%w: %s timestamp is required", ErrInvalid, name)
	}
	return nil
}

func validateBlocker(blocker Blocker, persisted bool) error {
	if !blocker.Kind.Valid() {
		return fmt.Errorf("%w: invalid blocker kind %q", ErrInvalid, blocker.Kind)
	}
	if err := validateText("blocker reference", blocker.Reference, MaxCorrelationIDBytes, true); err != nil {
		return err
	}
	if err := validateText("blocker reason", blocker.Reason, MaxReasonBytes, true); err != nil {
		return err
	}
	if persisted && blocker.BlockedAt.IsZero() {
		return fmt.Errorf("%w: blocker timestamp is required", ErrInvalid)
	}
	return nil
}

func sameBlocker(left, right Blocker) bool {
	return left.Kind == right.Kind && left.Reference == right.Reference && left.Reason == right.Reason
}

func validateCompletion(criteria []AcceptanceCriterion, request CompletionRequest) ([]AcceptanceResult, error) {
	if err := validateText("completion validator", request.ValidatedBy, MaxCorrelationIDBytes, true); err != nil {
		return nil, err
	}
	if err := validateText("completion summary", request.Summary, MaxReasonBytes, true); err != nil {
		return nil, err
	}
	if len(request.Results) != len(criteria) || len(request.Results) > MaxCriteria {
		return nil, fmt.Errorf("%w: got %d results for %d criteria", ErrAcceptanceIncomplete, len(request.Results), len(criteria))
	}
	byID := make(map[string]AcceptanceResult, len(request.Results))
	for _, result := range request.Results {
		if err := validateText("completion criterion id", result.CriterionID, MaxCriterionIDBytes, true); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrAcceptanceIncomplete, err)
		}
		if _, duplicate := byID[result.CriterionID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate result for %q", ErrAcceptanceIncomplete, result.CriterionID)
		}
		byID[result.CriterionID] = result
	}
	ordered := make([]AcceptanceResult, 0, len(criteria))
	for _, criterion := range criteria {
		result, exists := byID[criterion.ID]
		if !exists || !result.Satisfied {
			return nil, fmt.Errorf("%w: criterion %q is not satisfied", ErrAcceptanceIncomplete, criterion.ID)
		}
		if err := validateText("acceptance evidence", result.Evidence, MaxEvidenceBytes, true); err != nil {
			return nil, fmt.Errorf("%w: criterion %q: %v", ErrAcceptanceIncomplete, criterion.ID, err)
		}
		ordered = append(ordered, result)
		delete(byID, criterion.ID)
	}
	if len(byID) > 0 {
		return nil, fmt.Errorf("%w: results contain unknown criteria", ErrAcceptanceIncomplete)
	}
	return ordered, nil
}

func validateSnapshot(snapshot Snapshot) error {
	if snapshot.Version != SnapshotVersion {
		return fmt.Errorf("%w: unsupported goal snapshot version %d", ErrInvalid, snapshot.Version)
	}
	spec := Spec{
		ID:                 snapshot.ID,
		SessionID:          snapshot.SessionID,
		Objective:          snapshot.Objective,
		AcceptanceCriteria: snapshot.AcceptanceCriteria,
		Budget:             snapshot.Budget,
		Cortex:             snapshot.Cortex,
	}
	if err := validateSpec(spec, true); err != nil {
		return err
	}
	if !snapshot.State.Valid() {
		return fmt.Errorf("%w: invalid goal state %q", ErrInvalid, snapshot.State)
	}
	if snapshot.Usage.ContinuationTurns < 0 || snapshot.Usage.EvalTokens < 0 {
		return fmt.Errorf("%w: budget usage must not be negative", ErrInvalid)
	}
	if snapshot.CreatedAt.IsZero() || snapshot.UpdatedAt.IsZero() || snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		return fmt.Errorf("%w: invalid goal timestamps", ErrInvalid)
	}
	if err := validateText("state reason", snapshot.StateReason, MaxReasonBytes*2, false); err != nil {
		return err
	}
	seenExhaustion := make(map[BudgetDimension]struct{}, len(snapshot.ExhaustedBy))
	for _, dimension := range snapshot.ExhaustedBy {
		switch dimension {
		case BudgetContinuationTurns, BudgetEvalTokens, BudgetWallTime:
		default:
			return fmt.Errorf("%w: invalid exhausted budget dimension %q", ErrInvalid, dimension)
		}
		if _, duplicate := seenExhaustion[dimension]; duplicate {
			return fmt.Errorf("%w: duplicate exhausted budget dimension %q", ErrInvalid, dimension)
		}
		seenExhaustion[dimension] = struct{}{}
	}
	if snapshot.LastTurn != nil {
		if err := validateTurnReport(snapshot.LastTurn.TurnReport); err != nil {
			return err
		}
		if snapshot.LastTurn.RecordedAt.IsZero() {
			return fmt.Errorf("%w: turn receipt timestamp is required", ErrInvalid)
		}
		if snapshot.LastTurn.RecordedAt.Before(snapshot.CreatedAt) || snapshot.LastTurn.RecordedAt.After(snapshot.UpdatedAt) {
			return fmt.Errorf("%w: turn receipt timestamp is outside the snapshot lifetime", ErrInvalid)
		}
		if snapshot.LastTurn.OutcomeUnknown && snapshot.State != StateBlocked && snapshot.State != StateDropped && snapshot.LastBlockResolution == nil {
			return fmt.Errorf("%w: unreconciled outcome-unknown turn must block the goal", ErrInvalid)
		}
	}
	if snapshot.PendingContinuation != nil {
		if err := validateTurnAdmission("pending", *snapshot.PendingContinuation, snapshot.Usage.ContinuationTurns); err != nil {
			return err
		}
		if snapshot.PendingContinuation.GrantedAt.Before(snapshot.CreatedAt) || snapshot.PendingContinuation.GrantedAt.After(snapshot.UpdatedAt) {
			return fmt.Errorf("%w: pending turn admission timestamp is outside the snapshot lifetime", ErrInvalid)
		}
		switch snapshot.PendingContinuation.Kind {
		case AdmissionInitial:
			if snapshot.LastTurn != nil {
				return fmt.Errorf("%w: pending initial admission cannot follow a turn receipt", ErrInvalid)
			}
		case AdmissionManual, AdmissionAutomatic:
			if snapshot.LastTurn == nil {
				return fmt.Errorf("%w: pending %s admission requires a prior turn receipt", ErrInvalid, snapshot.PendingContinuation.Kind)
			}
		}
		if snapshot.State.Terminal() || snapshot.State == StateBlocked || snapshot.State == StatePaused {
			return fmt.Errorf("%w: pending turn admission cannot exist in %s", ErrInvalid, snapshot.State)
		}
	}
	if snapshot.LastPendingRecovery != nil {
		record := snapshot.LastPendingRecovery
		if err := validateTurnAdmission("recovered permit", record.Permit, snapshot.Usage.ContinuationTurns); err != nil {
			return err
		}
		if record.Permit.GrantedAt.Before(snapshot.CreatedAt) {
			return fmt.Errorf("%w: recovered permit predates the goal", ErrInvalid)
		}
		if err := validatePendingRecovery(record.Recovery); err != nil {
			return err
		}
		if record.Recovery.TurnID != record.Permit.TurnID {
			return fmt.Errorf("%w: pending recovery turn does not match its permit", ErrInvalid)
		}
		if record.RecoveredAt.IsZero() || record.RecoveredAt.Before(record.Permit.GrantedAt) || record.RecoveredAt.After(snapshot.UpdatedAt) {
			return fmt.Errorf("%w: invalid pending recovery timestamp", ErrInvalid)
		}
		if record.Recovery.Kind == PendingOutcomeUnknown && snapshot.State != StateBlocked && snapshot.State != StateDropped && snapshot.LastBlockResolution == nil {
			return fmt.Errorf("%w: unresolved recovered outcome must block the goal", ErrInvalid)
		}
	}
	if snapshot.Blocker != nil {
		if err := validateBlocker(*snapshot.Blocker, true); err != nil {
			return err
		}
		if snapshot.State != StateBlocked && snapshot.State != StateDropped {
			return fmt.Errorf("%w: blocker requires blocked or dropped state", ErrInvalid)
		}
		if snapshot.Blocker.BlockedAt.Before(snapshot.CreatedAt) || snapshot.Blocker.BlockedAt.After(snapshot.UpdatedAt) {
			return fmt.Errorf("%w: blocker timestamp is outside the snapshot lifetime", ErrInvalid)
		}
	} else if snapshot.State == StateBlocked {
		return fmt.Errorf("%w: blocked state requires a blocker", ErrInvalid)
	}
	if snapshot.LastBlockResolution != nil {
		record := snapshot.LastBlockResolution
		if err := validateBlocker(record.Blocker, true); err != nil {
			return err
		}
		if record.BlockedAt.Before(snapshot.CreatedAt) || record.ResolvedAt.IsZero() || record.ResolvedAt.Before(record.BlockedAt) || record.ResolvedAt.After(snapshot.UpdatedAt) {
			return fmt.Errorf("%w: invalid block resolution timestamp", ErrInvalid)
		}
		if err := validateText("block resolution reason", record.Resolution.Reason, MaxReasonBytes, true); err != nil {
			return err
		}
		if record.Resolution.Reference != record.Reference {
			return fmt.Errorf("%w: block resolution reference does not match", ErrInvalid)
		}
		if record.Kind == BlockOutcomeUnknown {
			if !record.Resolution.Reconciled {
				return fmt.Errorf("%w: outcome-unknown resolution is not reconciled", ErrInvalid)
			}
			if err := validateText("outcome reconciliation evidence", record.Resolution.Evidence, MaxEvidenceBytes, true); err != nil {
				return err
			}
			if record.Resolution.Reconciliation == nil {
				return fmt.Errorf("%w: outcome reconciliation is missing its typed control receipt", ErrInvalid)
			}
			if err := record.Resolution.Reconciliation.Validate(); err != nil {
				return err
			}
		} else if record.Resolution.Reconciled || record.Resolution.Reconciliation != nil {
			return fmt.Errorf("%w: non-outcome block resolution contains reconciliation evidence", ErrInvalid)
		}
	}
	if snapshot.State == StateCompleted {
		if snapshot.Completion == nil || snapshot.Completion.CompletedAt.IsZero() {
			return fmt.Errorf("%w: completed state requires a completion record", ErrInvalid)
		}
		if _, err := validateCompletion(snapshot.AcceptanceCriteria, snapshot.Completion.CompletionRequest); err != nil {
			return err
		}
		if snapshot.Completion.CompletedAt.Before(snapshot.CreatedAt) || snapshot.Completion.CompletedAt.After(snapshot.UpdatedAt) {
			return fmt.Errorf("%w: completion timestamp is outside the snapshot lifetime", ErrInvalid)
		}
	} else if snapshot.Completion != nil {
		return fmt.Errorf("%w: completion record requires completed state", ErrInvalid)
	}
	return nil
}

func cloneCriteria(criteria []AcceptanceCriterion) []AcceptanceCriterion {
	return append([]AcceptanceCriterion(nil), criteria...)
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	copy := snapshot
	copy.AcceptanceCriteria = cloneCriteria(snapshot.AcceptanceCriteria)
	copy.ExhaustedBy = append([]BudgetDimension(nil), snapshot.ExhaustedBy...)
	if snapshot.LastTurn != nil {
		value := *snapshot.LastTurn
		copy.LastTurn = &value
	}
	if snapshot.PendingContinuation != nil {
		value := *snapshot.PendingContinuation
		copy.PendingContinuation = &value
	}
	if snapshot.LastPendingRecovery != nil {
		value := *snapshot.LastPendingRecovery
		copy.LastPendingRecovery = &value
	}
	if snapshot.Blocker != nil {
		value := *snapshot.Blocker
		copy.Blocker = &value
	}
	if snapshot.LastBlockResolution != nil {
		value := *snapshot.LastBlockResolution
		if snapshot.LastBlockResolution.Resolution.Reconciliation != nil {
			receipt := *snapshot.LastBlockResolution.Resolution.Reconciliation
			value.Resolution.Reconciliation = &receipt
		}
		copy.LastBlockResolution = &value
	}
	if snapshot.Completion != nil {
		value := *snapshot.Completion
		value.Results = append([]AcceptanceResult(nil), snapshot.Completion.Results...)
		copy.Completion = &value
	}
	return copy
}
