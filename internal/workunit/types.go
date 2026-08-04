// Package workunit defines the dependency-free contract for constrained
// specialist work attached to one durable goal.
package workunit

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// SnapshotVersion is the durable specialist work-graph schema version.
	SnapshotVersion = 1

	MaxGraphIDBytes      = 128
	MaxGoalIDBytes       = 128
	MaxWorkspaceIDBytes  = 4 * 1024
	MaxUnitIDBytes       = 128
	MaxTitleBytes        = 512
	MaxPromptBytes       = 32 * 1024
	MaxProfileBytes      = 256
	MaxReasonBytes       = 4 * 1024
	MaxSummaryBytes      = 16 * 1024
	MaxEvidenceBytes     = 16 * 1024
	MaxCriterionIDBytes  = 128
	MaxProofBytes        = 4 * 1024
	MaxUnits             = 64
	MaxDependencies      = 32
	MaxCriteria          = 64
	MaxProofExpectations = 32
	MaxEvidence          = 64
)

var (
	ErrInvalid           = errors.New("invalid specialist work graph value")
	ErrNotFound          = errors.New("specialist work unit not found")
	ErrNotReady          = errors.New("specialist work unit is not ready")
	ErrWriterBusy        = errors.New("specialist writer lane is busy")
	ErrIllegalTransition = errors.New("illegal specialist work unit transition")
	ErrConflict          = errors.New("specialist work unit receipt conflicts with durable state")
)

// Role is a bounded specialist responsibility. Roles are intentionally small:
// coordination remains the supervisor's responsibility.
type Role string

const (
	RoleExplorer    Role = "explorer"
	RoleImplementer Role = "implementer"
	RoleVerifier    Role = "verifier"
)

func (r Role) Valid() bool {
	switch r {
	case RoleExplorer, RoleImplementer, RoleVerifier:
		return true
	default:
		return false
	}
}

// EffectPolicy declares whether a unit may mutate the shared workspace.
type EffectPolicy string

const (
	EffectReadOnly EffectPolicy = "read_only"
	EffectWriter   EffectPolicy = "writer"
)

func (p EffectPolicy) Valid() bool { return p == EffectReadOnly || p == EffectWriter }

// State is the host-controlled lifecycle of one work unit.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateBlocked   State = "blocked"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

func (s State) Valid() bool {
	switch s {
	case StateQueued, StateRunning, StateBlocked, StateCompleted, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// Terminal reports whether the unit requires an explicit retry to run again.
func (s State) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

// BudgetSlice caps one specialist independently from the owning goal. Zero
// disables a dimension; the supervisor remains responsible for the goal-wide
// budget.
type BudgetSlice struct {
	MaxTurns      int64         `json:"max_turns,omitempty"`
	MaxEvalTokens int64         `json:"max_eval_tokens,omitempty"`
	MaxWallTime   time.Duration `json:"max_wall_time,omitempty"`
}

func (b BudgetSlice) Validate() error {
	if b.MaxTurns < 0 || b.MaxEvalTokens < 0 || b.MaxWallTime < 0 {
		return fmt.Errorf("%w: work-unit budgets must not be negative", ErrInvalid)
	}
	return nil
}

// UnitSpec is the immutable definition of one specialist. Dependencies refer
// only to unit IDs in the same graph.
type UnitSpec struct {
	ID                     string       `json:"id"`
	Title                  string       `json:"title"`
	Role                   Role         `json:"role"`
	Effect                 EffectPolicy `json:"effect"`
	Prompt                 string       `json:"prompt"`
	DependsOn              []string     `json:"depends_on,omitempty"`
	AcceptanceCriterionIDs []string     `json:"acceptance_criterion_ids,omitempty"`
	ProofExpectations      []string     `json:"proof_expectations,omitempty"`
	ModelProfile           string       `json:"model_profile,omitempty"`
	Budget                 BudgetSlice  `json:"budget,omitempty"`
}

// GraphSpec binds a set of specialists to one exact goal/session/workspace.
// ID may be empty; New then creates a cryptographically random graph ID.
type GraphSpec struct {
	ID          string     `json:"id,omitempty"`
	GoalID      string     `json:"goal_id"`
	SessionID   int64      `json:"session_id"`
	WorkspaceID string     `json:"workspace_id"`
	Units       []UnitSpec `json:"units"`
}

// Evidence is one bounded proof retained with a completion receipt.
type Evidence struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	Ref     string `json:"ref,omitempty"`
}

// CompletionReceipt is the settled output of one specialist. Verifiers must
// supply evidence; prose alone is not accepted as verification.
type CompletionReceipt struct {
	Attempt  int64      `json:"attempt"`
	Summary  string     `json:"summary"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// FailureReceipt is a bounded, retry-oriented failure explanation.
type FailureReceipt struct {
	Attempt   int64  `json:"attempt"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable,omitempty"`
}

// Unit is the durable projection of one immutable spec and its current state.
type Unit struct {
	Spec       UnitSpec           `json:"spec"`
	State      State              `json:"state"`
	Attempt    int64              `json:"attempt"`
	Reason     string             `json:"reason,omitempty"`
	Completion *CompletionReceipt `json:"completion,omitempty"`
	Failure    *FailureReceipt    `json:"failure,omitempty"`
	QueuedAt   time.Time          `json:"queued_at"`
	StartedAt  time.Time          `json:"started_at,omitempty"`
	SettledAt  time.Time          `json:"settled_at,omitempty"`
}

// Snapshot is the durable, JSON-safe projection of a complete work graph.
type Snapshot struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	GoalID      string    `json:"goal_id"`
	SessionID   int64     `json:"session_id"`
	WorkspaceID string    `json:"workspace_id"`
	Units       []Unit    `json:"units"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ReadyReason is a stable explanation that supervisors and TUIs can render.
type ReadyReason string

const (
	ReadyAllowed             ReadyReason = "ready"
	ReadyState               ReadyReason = "state"
	ReadyDependencyPending   ReadyReason = "dependency_pending"
	ReadyDependencyUnsettled ReadyReason = "dependency_unsettled"
	ReadyWriterBusy          ReadyReason = "writer_busy"
)

// Readiness explains whether one unit may start without mutating the graph.
type Readiness struct {
	Ready      bool        `json:"ready"`
	Reason     ReadyReason `json:"reason"`
	Dependency string      `json:"dependency,omitempty"`
	Writer     string      `json:"writer,omitempty"`
}

// Admission is the exact attempt authorized by Start. Started is true only
// for the call that consumed a queued unit; a retry of Start returns the same
// identity with Started false so a controller cannot mistake an idempotent
// replay for permission to dispatch the specialist again.
type Admission struct {
	UnitID    string    `json:"unit_id"`
	Attempt   int64     `json:"attempt"`
	StartedAt time.Time `json:"started_at"`
	Started   bool      `json:"started"`
}

// ReadySet is a deterministic scheduling view. Every eligible read-only unit
// is returned; at most one writer is admitted to the serialized writer lane.
type ReadySet struct {
	ReadOnly []Unit `json:"read_only,omitempty"`
	Writer   *Unit  `json:"writer,omitempty"`
}

func validateText(name, value string, limit int, required bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalid, name)
	}
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, name)
	}
	if len(value) > limit {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalid, name, limit)
	}
	return nil
}
