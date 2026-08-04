package workunit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

// Clock makes specialist lifecycle tests and embeddings deterministic.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type graphOptions struct{ clock Clock }

// Option configures a Graph without changing its durable schema.
type Option func(*graphOptions) error

// WithClock installs the clock used for lifecycle timestamps.
func WithClock(clock Clock) Option {
	return func(options *graphOptions) error {
		if clock == nil {
			return fmt.Errorf("%w: clock is nil", ErrInvalid)
		}
		options.clock = clock
		return nil
	}
}

// Graph is the concurrency-safe host authority for one constrained set of
// specialists. It schedules state only; process execution belongs to a goal
// supervisor.
type Graph struct {
	mu    sync.RWMutex
	state Snapshot
	clock Clock
}

// New constructs a queued graph after validating scope, dependencies, role
// capabilities, and the independent-verifier contract.
func New(spec GraphSpec, options ...Option) (*Graph, error) {
	configured, err := applyOptions(options)
	if err != nil {
		return nil, err
	}
	if spec.ID == "" {
		spec.ID, err = NewGraphID()
		if err != nil {
			return nil, err
		}
	}
	if err := validateGraphSpec(spec); err != nil {
		return nil, err
	}
	now := configured.clock.Now().UTC()
	if now.IsZero() {
		return nil, fmt.Errorf("%w: clock returned zero time", ErrInvalid)
	}
	units := make([]Unit, len(spec.Units))
	for index, unit := range spec.Units {
		units[index] = Unit{
			Spec:     cloneUnitSpec(unit),
			State:    StateQueued,
			QueuedAt: now,
		}
	}
	return &Graph{
		clock: configured.clock,
		state: Snapshot{
			Version:     SnapshotVersion,
			ID:          spec.ID,
			GoalID:      spec.GoalID,
			SessionID:   spec.SessionID,
			WorkspaceID: spec.WorkspaceID,
			Units:       units,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}, nil
}

// Restore validates and resumes a previously persisted graph snapshot.
func Restore(snapshot Snapshot, options ...Option) (*Graph, error) {
	configured, err := applyOptions(options)
	if err != nil {
		return nil, err
	}
	copy := cloneSnapshot(snapshot)
	if err := validateSnapshot(copy); err != nil {
		return nil, err
	}
	now := configured.clock.Now().UTC()
	if now.IsZero() {
		return nil, fmt.Errorf("%w: clock returned zero time", ErrInvalid)
	}
	if now.Before(copy.UpdatedAt) {
		return nil, fmt.Errorf("%w: snapshot is newer than the host clock", ErrInvalid)
	}
	return &Graph{state: copy, clock: configured.clock}, nil
}

func applyOptions(options []Option) (graphOptions, error) {
	configured := graphOptions{clock: systemClock{}}
	for _, option := range options {
		if option == nil {
			return configured, fmt.Errorf("%w: nil graph option", ErrInvalid)
		}
		if err := option(&configured); err != nil {
			return configured, err
		}
	}
	return configured, nil
}

// NewGraphID returns a cryptographically random durable graph identifier.
func NewGraphID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate specialist graph id: %w", err)
	}
	return "work_" + hex.EncodeToString(bytes[:]), nil
}

// Snapshot returns an isolated copy suitable for durable JSON persistence.
func (g *Graph) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	return cloneSnapshot(g.state), nil
}

// Readiness returns a stable, non-mutating explanation for one unit.
func (g *Graph) Readiness(ctx context.Context, id string) (Readiness, error) {
	if err := contextError(ctx); err != nil {
		return Readiness{}, err
	}
	if err := validateText("unit id", id, MaxUnitIDBytes, true); err != nil {
		return Readiness{}, err
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := contextError(ctx); err != nil {
		return Readiness{}, err
	}
	index := g.unitIndexLocked(id)
	if index < 0 {
		return Readiness{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return g.readinessLocked(index), nil
}

// Ready returns every startable read-only unit in declaration order and at
// most one startable writer. Running units are not returned.
func (g *Graph) Ready(ctx context.Context) (ReadySet, error) {
	if err := contextError(ctx); err != nil {
		return ReadySet{}, err
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := contextError(ctx); err != nil {
		return ReadySet{}, err
	}
	var ready ReadySet
	for index := range g.state.Units {
		if !g.readinessLocked(index).Ready {
			continue
		}
		unit := cloneUnit(g.state.Units[index])
		if unit.Spec.Effect == EffectWriter {
			if ready.Writer == nil {
				ready.Writer = &unit
			}
			continue
		}
		ready.ReadOnly = append(ready.ReadOnly, unit)
	}
	return ready, nil
}

// Start admits one ready unit and returns its exact attempt identity. Repeating
// Start for the same running unit is state-idempotent but returns Started=false,
// so a controller never treats a replay as authority to dispatch twice. Only
// one writer may be running at a time.
func (g *Graph) Start(ctx context.Context, id string) (Admission, error) {
	if err := contextError(ctx); err != nil {
		return Admission{}, err
	}
	if err := validateText("unit id", id, MaxUnitIDBytes, true); err != nil {
		return Admission{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Admission{}, err
	}
	index := g.unitIndexLocked(id)
	if index < 0 {
		return Admission{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	unit := &g.state.Units[index]
	if unit.State == StateRunning {
		return Admission{UnitID: id, Attempt: unit.Attempt, StartedAt: unit.StartedAt}, nil
	}
	readiness := g.readinessLocked(index)
	if !readiness.Ready {
		if readiness.Reason == ReadyWriterBusy {
			return Admission{}, fmt.Errorf("%w: %s", ErrWriterBusy, readiness.Writer)
		}
		return Admission{}, fmt.Errorf("%w: %s (%s)", ErrNotReady, id, readiness.Reason)
	}
	now, err := g.nowLocked()
	if err != nil {
		return Admission{}, err
	}
	if unit.Attempt == math.MaxInt64 {
		return Admission{}, fmt.Errorf("%w: attempt counter overflow for %s", ErrInvalid, id)
	}
	unit.State = StateRunning
	unit.Attempt++
	unit.Reason = ""
	unit.Failure = nil
	unit.Completion = nil
	unit.StartedAt = now
	unit.SettledAt = time.Time{}
	g.state.UpdatedAt = now
	return Admission{UnitID: id, Attempt: unit.Attempt, StartedAt: now, Started: true}, nil
}

// Complete settles one running unit with bounded proof. Exact replay is
// idempotent; a different receipt conflicts.
func (g *Graph) Complete(ctx context.Context, id string, receipt CompletionReceipt) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateCompletion(receipt); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	index := g.unitIndexLocked(id)
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	unit := &g.state.Units[index]
	if receipt.Attempt != unit.Attempt {
		return fmt.Errorf("%w: completion attempt %d does not match %s attempt %d", ErrConflict, receipt.Attempt, id, unit.Attempt)
	}
	if unit.State == StateCompleted {
		if unit.Completion != nil && reflect.DeepEqual(*unit.Completion, receipt) {
			return nil
		}
		return fmt.Errorf("%w: completion for %s", ErrConflict, id)
	}
	if unit.State != StateRunning {
		return fmt.Errorf("%w: cannot complete %s from %s", ErrIllegalTransition, id, unit.State)
	}
	if unit.Spec.Role == RoleVerifier && len(receipt.Evidence) == 0 {
		return fmt.Errorf("%w: verifier %s requires evidence", ErrInvalid, id)
	}
	now, err := g.nowLocked()
	if err != nil {
		return err
	}
	copy := cloneCompletion(receipt)
	unit.State = StateCompleted
	unit.Reason = ""
	unit.Completion = &copy
	unit.Failure = nil
	unit.SettledAt = now
	g.state.UpdatedAt = now
	return nil
}

// Fail settles a running unit without silently making dependents eligible.
// Retry is the only transition back to the queue.
func (g *Graph) Fail(ctx context.Context, id string, receipt FailureReceipt) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if receipt.Attempt <= 0 {
		return fmt.Errorf("%w: failure attempt must be positive", ErrInvalid)
	}
	if err := validateText("failure reason", receipt.Reason, MaxReasonBytes, true); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	index := g.unitIndexLocked(id)
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	unit := &g.state.Units[index]
	if receipt.Attempt != unit.Attempt {
		return fmt.Errorf("%w: failure attempt %d does not match %s attempt %d", ErrConflict, receipt.Attempt, id, unit.Attempt)
	}
	if unit.State == StateFailed {
		if unit.Failure != nil && *unit.Failure == receipt {
			return nil
		}
		return fmt.Errorf("%w: failure for %s", ErrConflict, id)
	}
	if unit.State != StateRunning {
		return fmt.Errorf("%w: cannot fail %s from %s", ErrIllegalTransition, id, unit.State)
	}
	now, err := g.nowLocked()
	if err != nil {
		return err
	}
	unit.State = StateFailed
	unit.Reason = receipt.Reason
	failure := receipt
	unit.Failure = &failure
	unit.Completion = nil
	unit.SettledAt = now
	g.state.UpdatedAt = now
	return nil
}

// Block pauses a queued or running unit while retaining a bounded reason. A
// controller blocking running work must cancel and join its driver first: this
// transition releases any writer lane held by the unit.
func (g *Graph) Block(ctx context.Context, id, reason string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateText("block reason", reason, MaxReasonBytes, true); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	index := g.unitIndexLocked(id)
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	unit := &g.state.Units[index]
	if unit.State == StateBlocked && unit.Reason == reason {
		return nil
	}
	if unit.State != StateQueued && unit.State != StateRunning {
		return fmt.Errorf("%w: cannot block %s from %s", ErrIllegalTransition, id, unit.State)
	}
	now, err := g.nowLocked()
	if err != nil {
		return err
	}
	unit.State = StateBlocked
	unit.Reason = reason
	unit.SettledAt = now
	g.state.UpdatedAt = now
	return nil
}

// Retry explicitly requeues a blocked or retryable failed unit. The reason is
// retained as the host-authored recovery explanation until Start clears it.
func (g *Graph) Retry(ctx context.Context, id, reason string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateText("retry reason", reason, MaxReasonBytes, true); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	index := g.unitIndexLocked(id)
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	unit := &g.state.Units[index]
	if unit.State == StateFailed && (unit.Failure == nil || !unit.Failure.Retryable) {
		return fmt.Errorf("%w: failure for %s is not retryable", ErrIllegalTransition, id)
	}
	if unit.State != StateBlocked && unit.State != StateFailed {
		return fmt.Errorf("%w: cannot retry %s from %s", ErrIllegalTransition, id, unit.State)
	}
	now, err := g.nowLocked()
	if err != nil {
		return err
	}
	unit.State = StateQueued
	unit.Reason = reason
	unit.Completion = nil
	unit.Failure = nil
	unit.StartedAt = time.Time{}
	unit.SettledAt = time.Time{}
	unit.QueuedAt = now
	g.state.UpdatedAt = now
	return nil
}

// Cancel explicitly prevents queued, blocked, or running work from starting
// or continuing. A controller cancelling running work must join its driver
// before calling Cancel because this transition releases the writer lane.
// Exact replay is idempotent.
func (g *Graph) Cancel(ctx context.Context, id, reason string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateText("cancellation reason", reason, MaxReasonBytes, true); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	index := g.unitIndexLocked(id)
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	unit := &g.state.Units[index]
	if unit.State == StateCancelled && unit.Reason == reason {
		return nil
	}
	if unit.State.Terminal() {
		return fmt.Errorf("%w: cannot cancel %s from %s", ErrIllegalTransition, id, unit.State)
	}
	now, err := g.nowLocked()
	if err != nil {
		return err
	}
	unit.State = StateCancelled
	unit.Reason = reason
	unit.SettledAt = now
	g.state.UpdatedAt = now
	return nil
}

func (g *Graph) readinessLocked(index int) Readiness {
	unit := g.state.Units[index]
	if unit.State != StateQueued {
		return Readiness{Reason: ReadyState}
	}
	for _, dependencyID := range unit.Spec.DependsOn {
		dependencyIndex := g.unitIndexLocked(dependencyID)
		if dependencyIndex < 0 {
			return Readiness{Reason: ReadyDependencyUnsettled, Dependency: dependencyID}
		}
		state := g.state.Units[dependencyIndex].State
		if state == StateFailed || state == StateCancelled {
			return Readiness{Reason: ReadyDependencyUnsettled, Dependency: dependencyID}
		}
		if state != StateCompleted {
			return Readiness{Reason: ReadyDependencyPending, Dependency: dependencyID}
		}
	}
	if unit.Spec.Effect == EffectWriter {
		for otherIndex, other := range g.state.Units {
			if otherIndex != index && other.State == StateRunning && other.Spec.Effect == EffectWriter {
				return Readiness{Reason: ReadyWriterBusy, Writer: other.Spec.ID}
			}
		}
	}
	return Readiness{Ready: true, Reason: ReadyAllowed}
}

func (g *Graph) unitIndexLocked(id string) int {
	for index := range g.state.Units {
		if g.state.Units[index].Spec.ID == id {
			return index
		}
	}
	return -1
}

func (g *Graph) nowLocked() (time.Time, error) {
	now := g.clock.Now().UTC()
	if now.IsZero() {
		return time.Time{}, fmt.Errorf("%w: clock returned zero time", ErrInvalid)
	}
	if now.Before(g.state.UpdatedAt) {
		return time.Time{}, fmt.Errorf("%w: clock moved backwards", ErrInvalid)
	}
	return now, nil
}

func validateGraphSpec(spec GraphSpec) error {
	if err := validateText("graph id", spec.ID, MaxGraphIDBytes, true); err != nil {
		return err
	}
	if err := validateText("goal id", spec.GoalID, MaxGoalIDBytes, true); err != nil {
		return err
	}
	if spec.SessionID <= 0 {
		return fmt.Errorf("%w: session id must be positive", ErrInvalid)
	}
	if err := validateText("workspace id", spec.WorkspaceID, MaxWorkspaceIDBytes, true); err != nil {
		return err
	}
	if len(spec.Units) == 0 || len(spec.Units) > MaxUnits {
		return fmt.Errorf("%w: graph must contain 1..%d units", ErrInvalid, MaxUnits)
	}
	units := make(map[string]UnitSpec, len(spec.Units))
	for _, unit := range spec.Units {
		if err := validateUnitSpec(unit); err != nil {
			return err
		}
		if _, exists := units[unit.ID]; exists {
			return fmt.Errorf("%w: duplicate unit id %s", ErrInvalid, unit.ID)
		}
		units[unit.ID] = unit
	}
	for _, unit := range spec.Units {
		seenDependencies := make(map[string]struct{}, len(unit.DependsOn))
		for _, dependency := range unit.DependsOn {
			if dependency == unit.ID {
				return fmt.Errorf("%w: unit %s depends on itself", ErrInvalid, unit.ID)
			}
			if _, exists := units[dependency]; !exists {
				return fmt.Errorf("%w: unit %s has unknown dependency %s", ErrInvalid, unit.ID, dependency)
			}
			if _, exists := seenDependencies[dependency]; exists {
				return fmt.Errorf("%w: unit %s repeats dependency %s", ErrInvalid, unit.ID, dependency)
			}
			seenDependencies[dependency] = struct{}{}
		}
		if unit.Role == RoleVerifier {
			if err := validateVerifier(unit, units); err != nil {
				return err
			}
		}
	}
	if err := validateAcyclic(spec.Units); err != nil {
		return err
	}
	return nil
}

func validateUnitSpec(spec UnitSpec) error {
	if err := validateText("unit id", spec.ID, MaxUnitIDBytes, true); err != nil {
		return err
	}
	if err := validateText("unit title", spec.Title, MaxTitleBytes, true); err != nil {
		return err
	}
	if !spec.Role.Valid() {
		return fmt.Errorf("%w: invalid role %q", ErrInvalid, spec.Role)
	}
	if !spec.Effect.Valid() {
		return fmt.Errorf("%w: invalid effect policy %q", ErrInvalid, spec.Effect)
	}
	if spec.Effect == EffectWriter && spec.Role != RoleImplementer {
		return fmt.Errorf("%w: only implementers may use the writer lane", ErrInvalid)
	}
	if spec.Role == RoleVerifier && spec.Effect != EffectReadOnly {
		return fmt.Errorf("%w: verifiers must be read-only", ErrInvalid)
	}
	if err := validateText("unit prompt", spec.Prompt, MaxPromptBytes, true); err != nil {
		return err
	}
	if err := validateText("model profile", spec.ModelProfile, MaxProfileBytes, false); err != nil {
		return err
	}
	if err := spec.Budget.Validate(); err != nil {
		return err
	}
	if len(spec.DependsOn) > MaxDependencies {
		return fmt.Errorf("%w: unit %s exceeds %d dependencies", ErrInvalid, spec.ID, MaxDependencies)
	}
	if err := validateStringSet("acceptance criterion", spec.AcceptanceCriterionIDs, MaxCriteria, MaxCriterionIDBytes); err != nil {
		return err
	}
	if err := validateStringSet("proof expectation", spec.ProofExpectations, MaxProofExpectations, MaxProofBytes); err != nil {
		return err
	}
	return nil
}

func validateVerifier(verifier UnitSpec, units map[string]UnitSpec) error {
	if len(verifier.DependsOn) == 0 {
		return fmt.Errorf("%w: verifier %s must depend on an implementer", ErrInvalid, verifier.ID)
	}
	if len(verifier.AcceptanceCriterionIDs) == 0 {
		return fmt.Errorf("%w: verifier %s requires acceptance criteria", ErrInvalid, verifier.ID)
	}
	if len(verifier.ProofExpectations) == 0 {
		return fmt.Errorf("%w: verifier %s requires proof expectations", ErrInvalid, verifier.ID)
	}
	for _, dependencyID := range verifier.DependsOn {
		dependency := units[dependencyID]
		if dependency.Role != RoleImplementer {
			continue
		}
		for _, criterion := range verifier.AcceptanceCriterionIDs {
			if slices.Contains(dependency.AcceptanceCriterionIDs, criterion) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: verifier %s must share an acceptance criterion with an implementer dependency", ErrInvalid, verifier.ID)
}

func validateAcyclic(units []UnitSpec) error {
	const (
		unseen = iota
		visiting
		done
	)
	states := make(map[string]int, len(units))
	byID := make(map[string]UnitSpec, len(units))
	for _, unit := range units {
		byID[unit.ID] = unit
	}
	var visit func(string) error
	visit = func(id string) error {
		switch states[id] {
		case visiting:
			return fmt.Errorf("%w: dependency cycle includes %s", ErrInvalid, id)
		case done:
			return nil
		}
		states[id] = visiting
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		states[id] = done
		return nil
	}
	for _, unit := range units {
		if err := visit(unit.ID); err != nil {
			return err
		}
	}
	return nil
}

func validateSnapshot(snapshot Snapshot) error {
	if snapshot.Version != SnapshotVersion {
		return fmt.Errorf("%w: unsupported snapshot version %d", ErrInvalid, snapshot.Version)
	}
	spec := GraphSpec{
		ID:          snapshot.ID,
		GoalID:      snapshot.GoalID,
		SessionID:   snapshot.SessionID,
		WorkspaceID: snapshot.WorkspaceID,
		Units:       make([]UnitSpec, len(snapshot.Units)),
	}
	if snapshot.CreatedAt.IsZero() || snapshot.UpdatedAt.IsZero() || snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		return fmt.Errorf("%w: invalid graph timestamps", ErrInvalid)
	}
	for index, unit := range snapshot.Units {
		spec.Units[index] = unit.Spec
	}
	if err := validateGraphSpec(spec); err != nil {
		return err
	}
	states := make(map[string]State, len(snapshot.Units))
	for _, unit := range snapshot.Units {
		states[unit.Spec.ID] = unit.State
	}
	writers := 0
	for _, unit := range snapshot.Units {
		if !unit.State.Valid() || unit.Attempt < 0 || unit.QueuedAt.IsZero() {
			return fmt.Errorf("%w: invalid projection for unit %s", ErrInvalid, unit.Spec.ID)
		}
		if err := validateUnitTimestamps(snapshot, unit); err != nil {
			return err
		}
		if err := validateText("unit reason", unit.Reason, MaxReasonBytes, false); err != nil {
			return err
		}
		switch unit.State {
		case StateQueued:
			if unit.Completion != nil || unit.Failure != nil || !unit.StartedAt.IsZero() || !unit.SettledAt.IsZero() {
				return fmt.Errorf("%w: invalid queued projection for unit %s", ErrInvalid, unit.Spec.ID)
			}
			if (unit.Attempt == 0 && unit.Reason != "") || (unit.Attempt > 0 && strings.TrimSpace(unit.Reason) == "") {
				return fmt.Errorf("%w: queued unit %s has an invalid retry reason", ErrInvalid, unit.Spec.ID)
			}
		case StateRunning:
			if unit.Attempt == 0 || unit.StartedAt.IsZero() || !unit.SettledAt.IsZero() || unit.Completion != nil || unit.Failure != nil || unit.Reason != "" {
				return fmt.Errorf("%w: invalid running projection for unit %s", ErrInvalid, unit.Spec.ID)
			}
			if err := validateStartedDependencies(unit, states); err != nil {
				return err
			}
			if unit.Spec.Effect == EffectWriter {
				writers++
			}
		case StateBlocked:
			if strings.TrimSpace(unit.Reason) == "" || unit.Completion != nil || unit.Failure != nil || unit.SettledAt.IsZero() {
				return fmt.Errorf("%w: invalid blocked projection for unit %s", ErrInvalid, unit.Spec.ID)
			}
			if unit.Attempt == 0 && !unit.StartedAt.IsZero() {
				return fmt.Errorf("%w: unstarted blocked unit %s has a start time", ErrInvalid, unit.Spec.ID)
			}
			if unit.Attempt > 0 {
				if unit.StartedAt.IsZero() {
					return fmt.Errorf("%w: started blocked unit %s lacks a start time", ErrInvalid, unit.Spec.ID)
				}
				if err := validateStartedDependencies(unit, states); err != nil {
					return err
				}
			}
		case StateCompleted:
			if unit.Attempt == 0 || unit.StartedAt.IsZero() || unit.Completion == nil || unit.Failure != nil || unit.SettledAt.IsZero() || unit.Reason != "" {
				return fmt.Errorf("%w: completed unit %s lacks receipt", ErrInvalid, unit.Spec.ID)
			}
			if err := validateStartedDependencies(unit, states); err != nil {
				return err
			}
			if err := validateCompletion(*unit.Completion); err != nil {
				return err
			}
			if unit.Completion.Attempt != unit.Attempt {
				return fmt.Errorf("%w: completed unit %s receipt attempt differs from projection", ErrInvalid, unit.Spec.ID)
			}
			if unit.Spec.Role == RoleVerifier && len(unit.Completion.Evidence) == 0 {
				return fmt.Errorf("%w: verifier %s lacks evidence", ErrInvalid, unit.Spec.ID)
			}
		case StateFailed:
			if unit.Attempt == 0 || unit.StartedAt.IsZero() || unit.Failure == nil || unit.Completion != nil || unit.SettledAt.IsZero() {
				return fmt.Errorf("%w: failed unit %s lacks receipt", ErrInvalid, unit.Spec.ID)
			}
			if err := validateStartedDependencies(unit, states); err != nil {
				return err
			}
			if err := validateText("failure reason", unit.Failure.Reason, MaxReasonBytes, true); err != nil {
				return err
			}
			if unit.Failure.Attempt <= 0 || unit.Failure.Attempt != unit.Attempt {
				return fmt.Errorf("%w: failed unit %s receipt attempt differs from projection", ErrInvalid, unit.Spec.ID)
			}
			if unit.Reason != unit.Failure.Reason {
				return fmt.Errorf("%w: failed unit %s reason conflicts with receipt", ErrInvalid, unit.Spec.ID)
			}
		case StateCancelled:
			if strings.TrimSpace(unit.Reason) == "" || unit.Completion != nil || unit.Failure != nil || unit.SettledAt.IsZero() {
				return fmt.Errorf("%w: invalid cancelled projection for unit %s", ErrInvalid, unit.Spec.ID)
			}
			if unit.Attempt == 0 && !unit.StartedAt.IsZero() {
				return fmt.Errorf("%w: unstarted cancelled unit %s has a start time", ErrInvalid, unit.Spec.ID)
			}
			if unit.Attempt > 0 {
				if unit.StartedAt.IsZero() {
					return fmt.Errorf("%w: started cancelled unit %s lacks a start time", ErrInvalid, unit.Spec.ID)
				}
				if err := validateStartedDependencies(unit, states); err != nil {
					return err
				}
			}
		}
	}
	if writers > 1 {
		return fmt.Errorf("%w: multiple running writers", ErrInvalid)
	}
	return nil
}

func validateUnitTimestamps(snapshot Snapshot, unit Unit) error {
	withinGraph := func(name string, value time.Time, required bool) error {
		if value.IsZero() {
			if required {
				return fmt.Errorf("%w: unit %s lacks %s", ErrInvalid, unit.Spec.ID, name)
			}
			return nil
		}
		if value.Before(snapshot.CreatedAt) || value.After(snapshot.UpdatedAt) {
			return fmt.Errorf("%w: unit %s %s is outside graph history", ErrInvalid, unit.Spec.ID, name)
		}
		return nil
	}
	if err := withinGraph("queue time", unit.QueuedAt, true); err != nil {
		return err
	}
	if err := withinGraph("start time", unit.StartedAt, false); err != nil {
		return err
	}
	if err := withinGraph("settlement time", unit.SettledAt, false); err != nil {
		return err
	}
	if !unit.StartedAt.IsZero() && unit.StartedAt.Before(unit.QueuedAt) {
		return fmt.Errorf("%w: unit %s started before it was queued", ErrInvalid, unit.Spec.ID)
	}
	baseline := unit.QueuedAt
	if !unit.StartedAt.IsZero() {
		baseline = unit.StartedAt
	}
	if !unit.SettledAt.IsZero() && unit.SettledAt.Before(baseline) {
		return fmt.Errorf("%w: unit %s settled before its active lifecycle", ErrInvalid, unit.Spec.ID)
	}
	return nil
}

func validateStartedDependencies(unit Unit, states map[string]State) error {
	for _, dependency := range unit.Spec.DependsOn {
		if states[dependency] != StateCompleted {
			return fmt.Errorf("%w: started unit %s has unsettled dependency %s", ErrInvalid, unit.Spec.ID, dependency)
		}
	}
	return nil
}

func validateCompletion(receipt CompletionReceipt) error {
	if receipt.Attempt <= 0 {
		return fmt.Errorf("%w: completion attempt must be positive", ErrInvalid)
	}
	if err := validateText("completion summary", receipt.Summary, MaxSummaryBytes, true); err != nil {
		return err
	}
	if len(receipt.Evidence) > MaxEvidence {
		return fmt.Errorf("%w: completion exceeds %d evidence records", ErrInvalid, MaxEvidence)
	}
	for _, evidence := range receipt.Evidence {
		if err := validateText("evidence kind", evidence.Kind, MaxTitleBytes, true); err != nil {
			return err
		}
		if err := validateText("evidence summary", evidence.Summary, MaxEvidenceBytes, true); err != nil {
			return err
		}
		if err := validateText("evidence ref", evidence.Ref, MaxWorkspaceIDBytes, false); err != nil {
			return err
		}
	}
	return nil
}

func validateStringSet(name string, values []string, maximum, byteLimit int) error {
	if len(values) > maximum {
		return fmt.Errorf("%w: %s list exceeds %d entries", ErrInvalid, name, maximum)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateText(name, value, byteLimit, true); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%w: duplicate %s %s", ErrInvalid, name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalid)
	}
	return ctx.Err()
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	copy := snapshot
	copy.Units = make([]Unit, len(snapshot.Units))
	for index, unit := range snapshot.Units {
		copy.Units[index] = cloneUnit(unit)
	}
	return copy
}

func cloneUnit(unit Unit) Unit {
	copy := unit
	copy.Spec = cloneUnitSpec(unit.Spec)
	if unit.Completion != nil {
		completion := cloneCompletion(*unit.Completion)
		copy.Completion = &completion
	}
	if unit.Failure != nil {
		failure := *unit.Failure
		copy.Failure = &failure
	}
	return copy
}

func cloneUnitSpec(spec UnitSpec) UnitSpec {
	copy := spec
	copy.DependsOn = append([]string(nil), spec.DependsOn...)
	copy.AcceptanceCriterionIDs = append([]string(nil), spec.AcceptanceCriterionIDs...)
	copy.ProofExpectations = append([]string(nil), spec.ProofExpectations...)
	return copy
}

func cloneCompletion(receipt CompletionReceipt) CompletionReceipt {
	copy := receipt
	copy.Evidence = append([]Evidence(nil), receipt.Evidence...)
	return copy
}
