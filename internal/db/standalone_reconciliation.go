package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

var ErrStandaloneReconciliationGoalOwned = errors.New("execution belongs to a session with a durable goal")

// StandaloneExecutionReconciliationInspection is a redacted, read-only view
// of one ordinary (goal-less) execution hazard. It deliberately excludes raw
// arguments, results, and receipts. Revision and EventID are review tokens
// that an apply request must echo so stale observations fail closed.
type StandaloneExecutionReconciliationInspection struct {
	SessionID       int64
	WorkspaceID     string
	SessionRevision int64
	ExecutionID     string
	TurnID          string
	ToolName        string
	EventID         int64
	EventType       execution.EventType
	EffectClass     execution.EffectClass
	ArgumentsSHA256 string
	ItemID          string
	Resolved        bool
	ResolutionID    string
	Context         StandaloneReconciliationContext
}

// StandaloneReconciliationContext is the closed, privacy-bounded projection a
// host may add to model context after validating durable reconciliation. It
// intentionally excludes the operator-authored summary/reference, raw tool
// arguments/results, control-plane detail, and actor. The immutable execution
// ID, tool name, and argument digest are retained so the provider cannot apply
// the receipt's disposition to a different tool effect.
type StandaloneReconciliationContext struct {
	ResolutionID    string
	EvidenceSHA256  string
	ExecutionID     string
	ToolName        string
	ArgumentsSHA256 string
	Disposition     reconciliation.Disposition
	SourceKind      reconciliation.SourceKind
}

func (c StandaloneReconciliationContext) Validate() error {
	if strings.TrimSpace(c.ResolutionID) == "" || strings.TrimSpace(c.ResolutionID) != c.ResolutionID ||
		len(c.ResolutionID) > controlplane.MaxIdentityIDBytes || !utf8.ValidString(c.ResolutionID) {
		return errors.New("standalone reconciliation context resolution id is invalid")
	}
	if !validStandaloneReconciliationDigest(c.EvidenceSHA256) {
		return errors.New("standalone reconciliation context evidence digest is invalid")
	}
	if strings.TrimSpace(c.ExecutionID) == "" || strings.TrimSpace(c.ExecutionID) != c.ExecutionID ||
		len(c.ExecutionID) > execution.MaxExecutionIDBytes || !utf8.ValidString(c.ExecutionID) {
		return errors.New("standalone reconciliation context execution id is invalid")
	}
	if strings.TrimSpace(c.ToolName) == "" || strings.TrimSpace(c.ToolName) != c.ToolName ||
		len(c.ToolName) > execution.MaxToolNameBytes || !utf8.ValidString(c.ToolName) {
		return errors.New("standalone reconciliation context tool name is invalid")
	}
	if !validStandaloneReconciliationDigest(c.ArgumentsSHA256) {
		return errors.New("standalone reconciliation context arguments digest is invalid")
	}
	if !c.Disposition.Valid() {
		return fmt.Errorf("standalone reconciliation context disposition %q is invalid", c.Disposition)
	}
	if !c.SourceKind.Valid() {
		return fmt.Errorf("standalone reconciliation context source kind %q is invalid", c.SourceKind)
	}
	return nil
}

type ResolveStandaloneExecutionReconciliationRequest struct {
	SessionID               int64
	WorkspaceID             string
	ExecutionID             string
	ExpectedSessionRevision int64
	ExpectedEventID         int64
	Actor                   string
	Evidence                reconciliation.Request
}

type StandaloneExecutionReconciliationReceipt struct {
	SessionID       int64
	WorkspaceID     string
	SessionRevision int64
	ExecutionID     string
	EventID         int64
	ItemID          string
	ResolutionID    string
	Inserted        bool
	Context         StandaloneReconciliationContext
}

// InspectStandaloneExecutionReconciliation never creates control-plane state.
// It returns the exact immutable target tokens required by the apply path.
func (s *Store) InspectStandaloneExecutionReconciliation(ctx context.Context, sessionID int64, workspaceID, executionID string) (StandaloneExecutionReconciliationInspection, error) {
	if err := validateStandaloneExecutionIdentity(sessionID, workspaceID, executionID); err != nil {
		return StandaloneExecutionReconciliationInspection{}, err
	}
	record, err := s.GetSessionStateRecord(ctx, sessionID)
	if err != nil {
		return StandaloneExecutionReconciliationInspection{}, err
	}
	if err := requireGoalLessSessionState(record.StateJSON); err != nil {
		return StandaloneExecutionReconciliationInspection{}, err
	}
	state, err := s.GetExecutionState(ctx, sessionID, workspaceID, executionID)
	if err != nil {
		return StandaloneExecutionReconciliationInspection{}, err
	}
	if !executionStateCanBeReconciled(state) {
		return StandaloneExecutionReconciliationInspection{}, fmt.Errorf("%w: execution %q latest event is %s/%s", ErrControlExecutionNotHazardous, executionID, state.Latest.Type, state.Identity.EffectClass)
	}
	expected, err := standaloneExecutionReconciliationItem(state)
	if err != nil {
		return StandaloneExecutionReconciliationInspection{}, err
	}
	inspection := StandaloneExecutionReconciliationInspection{
		SessionID: sessionID, WorkspaceID: workspaceID, SessionRevision: record.Revision,
		ExecutionID: executionID, TurnID: state.Identity.TurnID, ToolName: state.Identity.ToolName,
		EventID: state.Latest.ID, EventType: state.Latest.Type, EffectClass: state.Identity.EffectClass,
		ArgumentsSHA256: state.Latest.ArgumentsSHA256, ItemID: expected.ItemID,
	}
	states, err := s.ListControlStates(ctx, controlplane.Query{
		SessionID: sessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindExecutionReconciliation, ExecutionID: executionID,
		Limit: 2,
	})
	if err != nil {
		return StandaloneExecutionReconciliationInspection{}, err
	}
	if len(states) == 0 {
		return inspection, nil
	}
	if len(states) != 1 || !controlItemsEquivalent(states[0].Item, expected) {
		return StandaloneExecutionReconciliationInspection{}, fmt.Errorf("%w: execution %q has a non-canonical control item", ErrExecutionReconciliationCorrupt, executionID)
	}
	if states[0].Resolution == nil {
		return inspection, nil
	}
	resolution := *states[0].Resolution
	target, err := executionReconciliationTarget(states[0].Item, state.Latest, resolution.ResolvedBy)
	if err != nil {
		return StandaloneExecutionReconciliationInspection{}, fmt.Errorf("%w: %v", ErrExecutionReconciliationCorrupt, err)
	}
	envelope, err := reconciliation.Parse(resolution.EvidenceJSON, resolution.EvidenceSHA256)
	if err != nil || !envelope.MatchesTarget(target) {
		return StandaloneExecutionReconciliationInspection{}, fmt.Errorf("%w: execution %q has invalid typed evidence", ErrExecutionReconciliationCorrupt, executionID)
	}
	recoveryContext, err := standaloneReconciliationContext(resolution, envelope, state)
	if err != nil {
		return StandaloneExecutionReconciliationInspection{}, fmt.Errorf("%w: execution %q has invalid host context: %v", ErrExecutionReconciliationCorrupt, executionID, err)
	}
	inspection.Resolved = true
	inspection.ResolutionID = resolution.ResolutionID
	inspection.Context = recoveryContext
	return inspection, nil
}

// ResolveStandaloneExecutionReconciliation atomically creates/replays the
// canonical control item and appends one typed evidence receipt. It never
// mutates the execution ledger or session snapshot and never schedules work.
func (s *Store) ResolveStandaloneExecutionReconciliation(ctx context.Context, lease *ExecutionSessionLease, request ResolveStandaloneExecutionReconciliationRequest) (StandaloneExecutionReconciliationReceipt, error) {
	if err := validateStandaloneExecutionIdentity(request.SessionID, request.WorkspaceID, request.ExecutionID); err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	if request.ExpectedSessionRevision < 0 {
		return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("invalid expected session revision %d", request.ExpectedSessionRevision)
	}
	if request.ExpectedEventID <= 0 {
		return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("invalid expected execution event id %d", request.ExpectedEventID)
	}
	if strings.TrimSpace(request.Actor) == "" {
		return StandaloneExecutionReconciliationReceipt{}, errors.New("standalone reconciliation actor is required")
	}
	if err := request.Evidence.Validate(); err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	release, err := s.holdControlLease(ctx, lease, request.SessionID, request.WorkspaceID)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("begin standalone execution reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := getSessionStateRecord(ctx, tx, request.SessionID)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	if record.Revision != request.ExpectedSessionRevision {
		return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("%w: durable session revision is %d, expected %d", ErrSessionStateConflict, record.Revision, request.ExpectedSessionRevision)
	}
	if err := requireGoalLessSessionState(record.StateJSON); err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	state, err := getExecutionStateTx(ctx, tx, request.SessionID, request.WorkspaceID, request.ExecutionID)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	if !executionStateCanBeReconciled(state) {
		return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("%w: execution %q latest event is %s/%s", ErrControlExecutionNotHazardous, request.ExecutionID, state.Latest.Type, state.Identity.EffectClass)
	}
	if state.Latest.ID != request.ExpectedEventID {
		return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("%w: execution %q latest event is %d, expected %d", ErrReconciliationStaleEvidence, request.ExecutionID, state.Latest.ID, request.ExpectedEventID)
	}
	expected, err := standaloneExecutionReconciliationItem(state)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	item, _, err := appendControlItemTx(ctx, tx, expected)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	target, err := executionReconciliationTarget(item, state.Latest, request.Actor)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	envelope, err := request.Evidence.Bind(target)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	evidenceJSON, evidenceSHA, err := envelope.Marshal()
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	identitySHA := controlplane.HashText("standalone-execution-reconciliation\x00" + item.ItemID + "\x00" + evidenceSHA)
	candidate := controlplane.Resolution{
		ResolutionID:   "ctrlres_standalone_" + identitySHA[:32],
		IdempotencyKey: "ctrlresidem_standalone_" + identitySHA[:32],
		ItemID:         item.ItemID, SessionID: request.SessionID, WorkspaceID: request.WorkspaceID,
		Outcome:      controlplane.OutcomeReconciled,
		EvidenceJSON: evidenceJSON, EvidenceSHA256: evidenceSHA,
		ResolvedBy: request.Actor, Detail: "operator supplied typed standalone execution reconciliation evidence",
	}
	resolution, inserted, err := resolveControlItemTx(ctx, tx, candidate, true)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, err
	}
	recoveryContext, err := standaloneReconciliationContext(resolution, envelope, state)
	if err != nil {
		return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("build standalone recovery context: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return s.readBackStandaloneExecutionReconciliation(candidate, request.ExpectedSessionRevision, state.Latest.ID, err)
	}
	return StandaloneExecutionReconciliationReceipt{
		SessionID: request.SessionID, WorkspaceID: request.WorkspaceID,
		SessionRevision: record.Revision, ExecutionID: request.ExecutionID,
		EventID: state.Latest.ID, ItemID: item.ItemID,
		ResolutionID: resolution.ResolutionID, Inserted: inserted,
		Context: recoveryContext,
	}, nil
}

func standaloneExecutionReconciliationItem(state execution.State) (controlplane.Item, error) {
	identitySHA := controlplane.HashText(fmt.Sprintf("execution-reconciliation\x00%d\x00%s\x00%s", state.Identity.SessionID, "", state.Identity.ExecutionID))
	payload, payloadSHA, err := controlplane.MarshalDocument(map[string]any{
		"execution_id": state.Identity.ExecutionID, "turn_id": state.Identity.TurnID,
		"tool": state.Identity.ToolName, "event_type": state.Latest.Type,
		"effect_class": state.Identity.EffectClass,
	})
	if err != nil {
		return controlplane.Item{}, err
	}
	return controlplane.Item{
		ItemID: "ctrl_execution_" + identitySHA[:32], IdempotencyKey: "ctrlidem_execution_" + identitySHA[:32],
		Kind: controlplane.KindExecutionReconciliation,
		Identity: controlplane.Identity{
			SessionID: state.Identity.SessionID, WorkspaceID: state.Identity.WorkspaceID,
			ExecutionID: state.Identity.ExecutionID, TurnID: state.Identity.TurnID,
		},
		ExternalID: state.Identity.CanonicalCallID, Summary: "Reconcile outcome of " + state.Identity.ToolName,
		PayloadJSON: payload, PayloadSHA256: payloadSHA,
	}, nil
}

func requireGoalLessSessionState(raw string) error {
	if !utf8.ValidString(raw) || !json.Valid([]byte(raw)) {
		return errors.New("standalone reconciliation session state is not valid UTF-8 JSON")
	}
	if err := validateUniqueTopLevelJSONKeys([]byte(raw)); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return fmt.Errorf("decode standalone reconciliation session: %w", err)
	}
	if goalRaw := bytes.TrimSpace(fields["goal"]); len(goalRaw) > 0 && !bytes.Equal(goalRaw, []byte("null")) {
		return ErrStandaloneReconciliationGoalOwned
	}
	return nil
}

func validateStandaloneExecutionIdentity(sessionID int64, workspaceID, executionID string) error {
	if sessionID <= 0 {
		return fmt.Errorf("invalid execution session id %d", sessionID)
	}
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(workspaceID) != workspaceID || len(workspaceID) > execution.MaxWorkspaceIDBytes {
		return errors.New("standalone reconciliation workspace id is invalid")
	}
	if strings.TrimSpace(executionID) == "" || strings.TrimSpace(executionID) != executionID || len(executionID) > execution.MaxExecutionIDBytes || !utf8.ValidString(executionID) {
		return errors.New("standalone reconciliation execution id is invalid")
	}
	return nil
}

func (s *Store) readBackStandaloneExecutionReconciliation(candidate controlplane.Resolution, expectedRevision, eventID int64, commitErr error) (StandaloneExecutionReconciliationReceipt, error) {
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, stateErr := s.GetControlState(readCtx, candidate.SessionID, candidate.WorkspaceID, candidate.ItemID)
	record, recordErr := s.GetSessionStateRecord(readCtx, candidate.SessionID)
	if stateErr == nil && state.Resolution != nil && controlResolutionsEquivalent(*state.Resolution, candidate) &&
		recordErr == nil && record.Revision == expectedRevision {
		executionState, executionErr := s.GetExecutionState(readCtx, candidate.SessionID, candidate.WorkspaceID, state.Item.Identity.ExecutionID)
		if executionErr != nil {
			return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("commit standalone execution reconciliation: %w (read-back execution=%v)", commitErr, executionErr)
		}
		target, targetErr := executionReconciliationTarget(state.Item, executionState.Latest, state.Resolution.ResolvedBy)
		if targetErr != nil {
			return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("commit standalone execution reconciliation: %w (read-back target=%v)", commitErr, targetErr)
		}
		envelope, evidenceErr := reconciliation.Parse(state.Resolution.EvidenceJSON, state.Resolution.EvidenceSHA256)
		if evidenceErr != nil || !envelope.MatchesTarget(target) {
			return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("commit standalone execution reconciliation: %w (read-back evidence=%v)", commitErr, evidenceErr)
		}
		recoveryContext, contextErr := standaloneReconciliationContext(*state.Resolution, envelope, executionState)
		if contextErr != nil {
			return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("commit standalone execution reconciliation: %w (read-back context=%v)", commitErr, contextErr)
		}
		return StandaloneExecutionReconciliationReceipt{
			SessionID: candidate.SessionID, WorkspaceID: candidate.WorkspaceID,
			SessionRevision: record.Revision, ExecutionID: state.Item.Identity.ExecutionID,
			EventID: eventID, ItemID: state.Item.ItemID,
			ResolutionID: state.Resolution.ResolutionID, Inserted: false,
			Context: recoveryContext,
		}, nil
	}
	return StandaloneExecutionReconciliationReceipt{}, fmt.Errorf("commit standalone execution reconciliation: %w (read-back state=%v session=%v)", commitErr, stateErr, recordErr)
}

func standaloneReconciliationContext(resolution controlplane.Resolution, envelope reconciliation.Envelope, state execution.State) (StandaloneReconciliationContext, error) {
	context := StandaloneReconciliationContext{
		ResolutionID: resolution.ResolutionID, EvidenceSHA256: resolution.EvidenceSHA256,
		ExecutionID: state.Identity.ExecutionID, ToolName: state.Identity.ToolName,
		ArgumentsSHA256: state.Latest.ArgumentsSHA256,
		Disposition:     envelope.Disposition, SourceKind: envelope.Source.Kind,
	}
	if err := context.Validate(); err != nil {
		return StandaloneReconciliationContext{}, err
	}
	return context, nil
}

func validStandaloneReconciliationDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
