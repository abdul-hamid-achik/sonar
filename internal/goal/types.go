// Package goal defines the dependency-free lifecycle contract for one durable
// user goal bound to one sonar session.
package goal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// SnapshotVersion is the durable Goal Runtime schema version.
	SnapshotVersion = 2
	// LegacySnapshotVersion is the only snapshot schema that can be upgraded in
	// place. Version 1 predates universal turn-admission kinds and typed
	// reconciliation receipts.
	LegacySnapshotVersion = 1

	// ReconciliationReceiptVersion is the machine-verifiable authority binding
	// stored when an outcome-unknown blocker is cleared atomically with its
	// control-plane evidence.
	ReconciliationReceiptVersion = 1

	MaxGoalIDBytes           = 128
	MaxCorrelationIDBytes    = 256
	MaxTurnIDBytes           = 128
	MaxObjectiveBytes        = 16 * 1024
	MaxCriteria              = 64
	MaxCriterionIDBytes      = 128
	MaxCriterionBytes        = 4 * 1024
	MaxReasonBytes           = 4 * 1024
	MaxEvidenceBytes         = 16 * 1024
	MaxReconciliationTargets = 10_000
)

var (
	ErrInvalid                = errors.New("invalid goal runtime value")
	ErrIllegalTransition      = errors.New("illegal goal transition")
	ErrTerminal               = errors.New("goal is terminal")
	ErrBudgetExhausted        = errors.New("goal budget exhausted")
	ErrAutoContinuationDenied = errors.New("automatic continuation denied")
	ErrTurnPending            = errors.New("goal turn is pending")
	ErrTurnConflict           = errors.New("goal turn receipt conflicts with durable state")
	ErrBlocked                = errors.New("goal is blocked")
	ErrOutcomeUnknown         = errors.New("goal has an outcome-unknown blocker")
	ErrAcceptanceIncomplete   = errors.New("goal acceptance criteria are incomplete")
	ErrCorrelationConflict    = errors.New("cortex correlation conflicts with durable state")
)

// State is the host-controlled lifecycle state of a goal.
type State string

const (
	StateActive    State = "active"
	StatePaused    State = "paused"
	StateExhausted State = "exhausted"
	StateCompleted State = "completed"
	StateDropped   State = "dropped"
	StateBlocked   State = "blocked"
)

func (s State) Valid() bool {
	switch s {
	case StateActive, StatePaused, StateExhausted, StateCompleted, StateDropped, StateBlocked:
		return true
	default:
		return false
	}
}

// Terminal reports whether the goal may no longer transition.
func (s State) Terminal() bool { return s == StateCompleted || s == StateDropped }

// AcceptanceCriterion is one durable, independently verifiable success rule.
type AcceptanceCriterion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// Spec is the immutable definition used to create a runtime. ID may be empty;
// New then creates a cryptographically random goal ID.
type Spec struct {
	ID                 string                `json:"id,omitempty"`
	SessionID          int64                 `json:"session_id"`
	Objective          string                `json:"objective"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptance_criteria"`
	Budget             BudgetLimits          `json:"budget"`
	Cortex             CortexCorrelation     `json:"cortex,omitempty"`
}

// BudgetLimits are absolute caps. A zero value disables that dimension.
type BudgetLimits struct {
	MaxContinuationTurns int64         `json:"max_continuation_turns,omitempty"`
	MaxEvalTokens        int64         `json:"max_eval_tokens,omitempty"`
	MaxWallTime          time.Duration `json:"max_wall_time,omitempty"`
}

const (
	// DefaultContinuationTurns / DefaultEvalTokens / DefaultWallTime are the
	// unflagged /goal and `goal open` starting budget. A goal is durable
	// unattended work, so these must not be tighter than a casual AUTO turn.
	DefaultContinuationTurns int64 = 24
	DefaultEvalTokens        int64 = 250_000
	DefaultWallTime                = 90 * time.Minute
)

// DefaultBudget is the unflagged /goal and `goal open` starting budget.
var DefaultBudget = BudgetLimits{
	MaxContinuationTurns: DefaultContinuationTurns,
	MaxEvalTokens:        DefaultEvalTokens,
	MaxWallTime:          DefaultWallTime,
}

func (b BudgetLimits) Validate() error {
	if b.MaxContinuationTurns < 0 {
		return fmt.Errorf("%w: continuation-turn budget must not be negative", ErrInvalid)
	}
	if b.MaxEvalTokens < 0 {
		return fmt.Errorf("%w: eval-token budget must not be negative", ErrInvalid)
	}
	if b.MaxWallTime < 0 {
		return fmt.Errorf("%w: wall-time budget must not be negative", ErrInvalid)
	}
	return nil
}

// BudgetUsage records work already admitted or observed.
type BudgetUsage struct {
	ContinuationTurns int64 `json:"continuation_turns,omitempty"`
	EvalTokens        int64 `json:"eval_tokens,omitempty"`
}

// BudgetDimension identifies why a goal exhausted its budget.
type BudgetDimension string

const (
	BudgetContinuationTurns BudgetDimension = "continuation_turns"
	BudgetEvalTokens        BudgetDimension = "eval_tokens"
	BudgetWallTime          BudgetDimension = "wall_time"
)

// CortexCorrelation binds the local goal to one Cortex case revision and
// actor. TaskID and Actor are immutable after the initial attachment; Revision
// is monotonic.
type CortexCorrelation struct {
	TaskID   string `json:"task_id,omitempty"`
	Revision int64  `json:"revision,omitempty"`
	Actor    string `json:"actor,omitempty"`
}

func (c CortexCorrelation) Empty() bool {
	return c.TaskID == "" && c.Revision == 0 && c.Actor == ""
}

func (c CortexCorrelation) Validate() error {
	if c.Empty() {
		return nil
	}
	if err := validateText("cortex task id", c.TaskID, MaxCorrelationIDBytes, true); err != nil {
		return err
	}
	if err := validateText("cortex actor", c.Actor, MaxCorrelationIDBytes, true); err != nil {
		return err
	}
	if c.Revision < 0 {
		return fmt.Errorf("%w: cortex revision must not be negative", ErrInvalid)
	}
	return nil
}

// TurnReport is the host's settled receipt for one model turn. Productive must
// reflect concrete progress toward an acceptance criterion, not token output.
type TurnReport struct {
	TurnID         string `json:"turn_id"`
	EvalTokens     int64  `json:"eval_tokens,omitempty"`
	Productive     bool   `json:"productive"`
	Summary        string `json:"summary"`
	OutcomeUnknown bool   `json:"outcome_unknown,omitempty"`
	OutcomeRef     string `json:"outcome_ref,omitempty"`
}

// TurnReceipt is the durable, timestamped form of TurnReport.
type TurnReceipt struct {
	TurnReport
	RecordedAt time.Time `json:"recorded_at"`
}

// TurnAdmissionKind identifies why one provider turn was admitted. Every turn
// is durably admitted before dispatch, but only automatic admissions consume
// the continuation-turn budget.
type TurnAdmissionKind string

const (
	AdmissionInitial   TurnAdmissionKind = "initial"
	AdmissionManual    TurnAdmissionKind = "manual"
	AdmissionAutomatic TurnAdmissionKind = "automatic"
)

func (k TurnAdmissionKind) Valid() bool {
	switch k {
	case AdmissionInitial, AdmissionManual, AdmissionAutomatic:
		return true
	default:
		return false
	}
}

// ContinuationPermit is the legacy name of the durable provider-turn
// admission. Keeping the type and JSON field names stable preserves existing
// session snapshots while Kind extends the permit to initial and manual turns.
// While an admission is pending, every later begin attempt fails closed;
// callers must settle or explicitly recover it.
type ContinuationPermit struct {
	TurnID    string            `json:"turn_id"`
	Kind      TurnAdmissionKind `json:"kind,omitempty"`
	Ordinal   int64             `json:"ordinal"`
	GrantedAt time.Time         `json:"granted_at"`
}

// PendingRecoveryKind identifies the only safe host conclusions after a turn
// admission survives without a settled receipt.
type PendingRecoveryKind string

const (
	// PendingCancelledBeforeDispatch requires host evidence that no backend work
	// began. The consumed continuation budget is intentionally not refunded.
	PendingCancelledBeforeDispatch PendingRecoveryKind = "cancelled_before_dispatch"
	// PendingOutcomeUnknown means dispatch may have occurred and continuation is
	// blocked until the external effect is reconciled.
	PendingOutcomeUnknown PendingRecoveryKind = "outcome_unknown"
)

func (k PendingRecoveryKind) Valid() bool {
	return k == PendingCancelledBeforeDispatch || k == PendingOutcomeUnknown
}

// PendingRecovery is the host-only conclusion for an orphaned permit.
type PendingRecovery struct {
	TurnID     string              `json:"turn_id"`
	Kind       PendingRecoveryKind `json:"kind"`
	Reason     string              `json:"reason"`
	Evidence   string              `json:"evidence"`
	OutcomeRef string              `json:"outcome_ref,omitempty"`
}

// PendingRecoveryRecord retains both the consumed admission and the evidence
// that made it safe to leave the in-flight state.
type PendingRecoveryRecord struct {
	Permit      ContinuationPermit `json:"permit"`
	Recovery    PendingRecovery    `json:"recovery"`
	RecoveredAt time.Time          `json:"recovered_at"`
}

// ContinuationReason explains why automatic continuation is unavailable.
type ContinuationReason string

const (
	ContinuationAllowed        ContinuationReason = "allowed"
	ContinuationNotActive      ContinuationReason = "not_active"
	ContinuationNoTurnReceipt  ContinuationReason = "no_turn_receipt"
	ContinuationUnproductive   ContinuationReason = "unproductive_turn"
	ContinuationBlocked        ContinuationReason = "blocked"
	ContinuationOutcomeUnknown ContinuationReason = "outcome_unknown"
	ContinuationBudget         ContinuationReason = "budget_exhausted"
	ContinuationTurnPending    ContinuationReason = "turn_pending"
)

// ContinuationDecision is a non-mutating explanation suitable for a TUI.
type ContinuationDecision struct {
	Allowed bool               `json:"allowed"`
	Reason  ContinuationReason `json:"reason"`
}

// BlockKind identifies the recovery authority required before work can resume.
type BlockKind string

const (
	BlockDependency     BlockKind = "dependency"
	BlockDecision       BlockKind = "decision"
	BlockOutcomeUnknown BlockKind = "outcome_unknown"
)

func (k BlockKind) Valid() bool {
	switch k {
	case BlockDependency, BlockDecision, BlockOutcomeUnknown:
		return true
	default:
		return false
	}
}

// Blocker is the durable reason a goal cannot proceed.
type Blocker struct {
	Kind      BlockKind `json:"kind"`
	Reference string    `json:"reference"`
	Reason    string    `json:"reason"`
	BlockedAt time.Time `json:"blocked_at"`
}

// BlockResolution is a host-authored reconciliation receipt. Outcome-unknown
// blockers require Reconciled=true and non-empty Evidence.
type BlockResolution struct {
	Reference      string                 `json:"reference"`
	Reason         string                 `json:"reason"`
	Reconciled     bool                   `json:"reconciled,omitempty"`
	Evidence       string                 `json:"evidence,omitempty"`
	Reconciliation *ReconciliationReceipt `json:"reconciliation,omitempty"`
}

// ReconciliationReceipt binds a Goal Runtime transition to one exact durable
// control-plane resolution set. It contains no user evidence or raw tool data;
// the evidence remains in the append-only control plane and is addressed by
// these identities and digest.
type ReconciliationReceipt struct {
	Version             int    `json:"version"`
	GroupItemID         string `json:"group_item_id"`
	FinalItemID         string `json:"final_item_id"`
	FinalResolutionID   string `json:"final_resolution_id"`
	ResolutionSetSHA256 string `json:"resolution_set_sha256"`
	TargetCount         int    `json:"target_count"`
}

// BlockResolutionRecord preserves the reconciliation that made a blocker safe
// to leave. The original blocker remains available through this receipt.
type BlockResolutionRecord struct {
	Blocker
	Resolution BlockResolution `json:"resolution"`
	ResolvedAt time.Time       `json:"resolved_at"`
}

// AcceptanceResult is the host's evidence for one acceptance criterion.
type AcceptanceResult struct {
	CriterionID string `json:"criterion_id"`
	Satisfied   bool   `json:"satisfied"`
	Evidence    string `json:"evidence"`
}

// CompletionRequest is explicit host validation. Budget exhaustion alone can
// never produce one.
type CompletionRequest struct {
	ValidatedBy string             `json:"validated_by"`
	Summary     string             `json:"summary"`
	Results     []AcceptanceResult `json:"results"`
}

// CompletionRecord is the durable form of CompletionRequest.
type CompletionRecord struct {
	CompletionRequest
	CompletedAt time.Time `json:"completed_at"`
}

// Snapshot is the complete durable representation of one session goal.
type Snapshot struct {
	Version             int                    `json:"version"`
	ID                  string                 `json:"id"`
	SessionID           int64                  `json:"session_id"`
	Objective           string                 `json:"objective"`
	AcceptanceCriteria  []AcceptanceCriterion  `json:"acceptance_criteria"`
	State               State                  `json:"state"`
	StateReason         string                 `json:"state_reason,omitempty"`
	Budget              BudgetLimits           `json:"budget"`
	Usage               BudgetUsage            `json:"usage"`
	ExhaustedBy         []BudgetDimension      `json:"exhausted_by,omitempty"`
	Cortex              CortexCorrelation      `json:"cortex,omitempty"`
	LastTurn            *TurnReceipt           `json:"last_turn,omitempty"`
	PendingContinuation *ContinuationPermit    `json:"pending_continuation,omitempty"`
	LastPendingRecovery *PendingRecoveryRecord `json:"last_pending_recovery,omitempty"`
	Blocker             *Blocker               `json:"blocker,omitempty"`
	LastBlockResolution *BlockResolutionRecord `json:"last_block_resolution,omitempty"`
	Completion          *CompletionRecord      `json:"completion,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	UpdatedAt           time.Time              `json:"updated_at"`
}

// NewGoalID returns a process-independent 128-bit goal identifier.
func NewGoalID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate goal id: %w", err)
	}
	return "goal_" + hex.EncodeToString(raw[:]), nil
}

func validateSpec(spec Spec, requireID bool) error {
	if spec.SessionID <= 0 {
		return fmt.Errorf("%w: session id must be positive", ErrInvalid)
	}
	if err := validateText("goal id", spec.ID, MaxGoalIDBytes, requireID); err != nil {
		return err
	}
	if err := validateText("objective", spec.Objective, MaxObjectiveBytes, true); err != nil {
		return err
	}
	if len(spec.AcceptanceCriteria) == 0 || len(spec.AcceptanceCriteria) > MaxCriteria {
		return fmt.Errorf("%w: acceptance criteria count must be between 1 and %d", ErrInvalid, MaxCriteria)
	}
	seen := make(map[string]struct{}, len(spec.AcceptanceCriteria))
	for _, criterion := range spec.AcceptanceCriteria {
		if err := validateText("acceptance criterion id", criterion.ID, MaxCriterionIDBytes, true); err != nil {
			return err
		}
		if err := validateText("acceptance criterion", criterion.Description, MaxCriterionBytes, true); err != nil {
			return err
		}
		if _, exists := seen[criterion.ID]; exists {
			return fmt.Errorf("%w: duplicate acceptance criterion %q", ErrInvalid, criterion.ID)
		}
		seen[criterion.ID] = struct{}{}
	}
	if err := spec.Budget.Validate(); err != nil {
		return err
	}
	return spec.Cortex.Validate()
}

func validateText(name, value string, limit int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, name)
	}
	if len(value) > limit {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalid, name, limit)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalid, name)
	}
	return nil
}
