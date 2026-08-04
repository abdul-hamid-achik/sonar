package ui

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
)

const (
	goalControlPlaneTimeout               = 2 * time.Second
	cortexDecisionControlBindingVersion   = 1
	cortexDecisionControlSummary          = "Cortex decision awaiting response"
	minCortexDecisionControlOptionIDs     = 2
	maxCortexDecisionControlOptionIDs     = 16
	maxCortexDecisionControlStableIDBytes = 256
	cortexDecisionDropResolutionSource    = "local_goal_drop"
	cortexDecisionDropResolutionDetail    = "Goal dropped by user"
)

type cortexDecisionControlBinding struct {
	Version       int      `json:"version"`
	TaskID        string   `json:"task_id"`
	DecisionID    string   `json:"decision_id"`
	RequestedAt   string   `json:"requested_at"`
	OptionIDs     []string `json:"option_ids"`
	Sensitive     bool     `json:"sensitive"`
	RequestSHA256 string   `json:"request_sha256"`
}

type cortexDecisionControlBindingWire struct {
	Version       int      `json:"version"`
	TaskID        string   `json:"task_id"`
	DecisionID    string   `json:"decision_id"`
	RequestedAt   string   `json:"requested_at"`
	OptionIDs     []string `json:"option_ids"`
	Sensitive     *bool    `json:"sensitive"`
	RequestSHA256 string   `json:"request_sha256"`
}

// prepareCortexDecisionDropResolutions validates every pending decision bound
// to this goal and constructs fixed, prose-free user-drop evidence. It performs
// no mutation; the caller commits these receipts with the dropped session
// envelope in one database transaction.
func (m *Model) prepareCortexDecisionDropResolutions(snapshot goal.Snapshot) (string, []controlplane.Resolution, error) {
	if m.sessionStore == nil || m.executionLease == nil {
		return "", nil, fmt.Errorf("durable Cortex decision dismissal requires the active session lease")
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return "", nil, fmt.Errorf("resolve Cortex decision drop workspace: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), goalControlPlaneTimeout)
	defer cancel()
	states, err := m.sessionStore.ListControlStates(ctx, controlplane.Query{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: snapshot.ID,
		PendingOnly: true, Limit: controlplane.MaxListLimit,
	})
	if err != nil {
		return "", nil, fmt.Errorf("list Cortex decisions before goal drop: %w", err)
	}
	if len(states) == 0 {
		return workspaceID, nil, nil
	}
	taskID, err := cortexDecisionTaskBinding(snapshot, "")
	if err != nil {
		return "", nil, err
	}
	resolutions := make([]controlplane.Resolution, 0, len(states))
	for _, state := range states {
		if err := state.Item.Validate(); err != nil {
			return "", nil, fmt.Errorf("validate pending Cortex decision %s before goal drop: %w", state.Item.ItemID, err)
		}
		if state.Resolution != nil || state.Item.Kind != controlplane.KindCortexDecision ||
			state.Item.Identity.SessionID != snapshot.SessionID ||
			state.Item.Identity.WorkspaceID != workspaceID || state.Item.Identity.GoalID != snapshot.ID {
			return "", nil, fmt.Errorf("pending Cortex decision %s has inconsistent durable identity", state.Item.ItemID)
		}
		binding, parseErr := parseCortexDecisionControlBinding(state.Item.PayloadJSON)
		if parseErr != nil {
			return "", nil, fmt.Errorf("validate pending Cortex decision %s binding before goal drop: %w", state.Item.ItemID, parseErr)
		}
		itemID, idempotencyKey := cortexDecisionControlIdentity(
			snapshot.SessionID, snapshot.ID, binding.TaskID, binding.DecisionID,
		)
		if binding.TaskID != taskID || state.Item.ExternalID != binding.DecisionID ||
			state.Item.ItemID != itemID || state.Item.IdempotencyKey != idempotencyKey {
			return "", nil, fmt.Errorf("pending Cortex decision %s has an inconsistent stable binding", state.Item.ItemID)
		}
		evidence, evidenceDigest, encodeErr := controlplane.MarshalDocument(map[string]any{
			"decision_id":             binding.DecisionID,
			"decision_payload_sha256": state.Item.PayloadSHA256,
			"goal_dropped":            true,
			"item_id":                 state.Item.ItemID,
			"request_sha256":          binding.RequestSHA256,
			"resolution_source":       cortexDecisionDropResolutionSource,
			"task_id":                 binding.TaskID,
			"version":                 cortexDecisionControlBindingVersion,
		})
		if encodeErr != nil {
			return "", nil, fmt.Errorf("encode Cortex decision drop resolution: %w", encodeErr)
		}
		resolutionHash := controlplane.HashText(fmt.Sprintf(
			"cortex-drop-resolution\x00%s\x00%s", state.Item.ItemID, evidenceDigest,
		))
		resolutions = append(resolutions, controlplane.Resolution{
			ResolutionID:   "ctrlres_cortex_" + resolutionHash[:32],
			IdempotencyKey: "ctrlidem_cortex_resolution_" + resolutionHash[:32],
			ItemID:         state.Item.ItemID,
			SessionID:      snapshot.SessionID,
			WorkspaceID:    workspaceID,
			Outcome:        controlplane.OutcomeDismissed,
			EvidenceJSON:   evidence,
			EvidenceSHA256: evidenceDigest,
			ResolvedBy:     goalActor,
			Detail:         cortexDecisionDropResolutionDetail,
		})
	}
	return workspaceID, resolutions, nil
}

func (m *Model) persistDroppedGoalWithCortexDismissals(workspaceID string, resolutions []controlplane.Resolution) (err error) {
	if len(resolutions) == 0 {
		return m.persistGoalSession()
	}
	defer func() { m.goalPersistenceDirty = err != nil }()
	stateJSON, err := encodeSessionState(m)
	if err != nil {
		m.sessionStateMu.Lock()
		m.sessionStatePersistenceDirty = true
		m.sessionStateMu.Unlock()
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), goalControlPlaneTimeout)
	defer cancel()
	m.sessionStateMu.Lock()
	defer m.sessionStateMu.Unlock()
	if m.sessionStore == nil || m.sessionID <= 0 || m.executionLease == nil {
		m.sessionStatePersistenceDirty = true
		return fmt.Errorf("durable session is unavailable")
	}
	if !m.sessionStateRevisionKnown {
		m.sessionStatePersistenceDirty = true
		return ErrSessionStateRevisionUnknown
	}
	expectedRevision := m.sessionStateRevision
	record, err := m.sessionStore.DismissCortexDecisionsAndSaveSessionStateCAS(ctx, m.executionLease,
		db.DismissCortexDecisionsSessionRequest{
			SessionID: m.sessionID, WorkspaceID: workspaceID,
			ExpectedSessionRevision: expectedRevision,
			StateJSON:               stateJSON, Resolutions: resolutions,
		})
	if err != nil {
		m.sessionStatePersistenceDirty = true
		if errors.Is(err, db.ErrSessionStateConflict) {
			m.sessionStateRevisionKnown = false
		}
		return err
	}
	if record.SessionID != m.sessionID || record.Revision <= expectedRevision {
		m.sessionStateRevisionKnown = false
		m.sessionStatePersistenceDirty = true
		return fmt.Errorf("session state CAS returned invalid record session=%d revision=%d", record.SessionID, record.Revision)
	}
	m.sessionStateRevision = record.Revision
	m.sessionStatePersistenceDirty = false
	return nil
}

func (m *Model) recordCortexDecisionControlItem(snapshot goal.Snapshot, advice goaladvisor.Advice) error {
	if m.sessionStore == nil {
		// Pure UI embeddings may omit persistence. The production application
		// requires it before a Goal Runtime can be created.
		return nil
	}
	if m.executionLease == nil {
		return fmt.Errorf("durable Cortex decision requires the active session lease")
	}
	if advice.Decision == nil {
		return fmt.Errorf("durable Cortex decision requires the typed pending decision")
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return fmt.Errorf("resolve Cortex decision workspace: %w", err)
	}
	taskID, err := cortexDecisionTaskBinding(snapshot, advice.TaskID)
	if err != nil {
		return err
	}
	decision := *advice.Decision
	requestSHA256, err := decision.RequestBindingSHA256(taskID)
	if err != nil {
		return fmt.Errorf("validate typed Cortex decision request: %w", err)
	}
	optionIDs := make([]string, 0, len(decision.Options))
	for _, option := range decision.Options {
		optionIDs = append(optionIDs, option.ID)
	}
	binding := cortexDecisionControlBinding{
		Version:       cortexDecisionControlBindingVersion,
		TaskID:        taskID,
		DecisionID:    decision.ID,
		RequestedAt:   decision.RequestedAt.UTC().Format(time.RFC3339Nano),
		OptionIDs:     optionIDs,
		Sensitive:     decision.Sensitive,
		RequestSHA256: requestSHA256,
	}
	if err := validateCortexDecisionControlBinding(binding); err != nil {
		return fmt.Errorf("validate Cortex decision binding: %w", err)
	}
	itemID, idempotencyKey := cortexDecisionControlIdentity(snapshot.SessionID, snapshot.ID, taskID, decision.ID)
	payload, payloadDigest, err := controlplane.MarshalDocument(binding)
	if err != nil {
		return fmt.Errorf("encode Cortex decision: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), goalControlPlaneTimeout)
	defer cancel()
	_, _, err = m.sessionStore.AppendControlItem(ctx, m.executionLease, controlplane.Item{
		ItemID:         itemID,
		IdempotencyKey: idempotencyKey,
		Kind:           controlplane.KindCortexDecision,
		Identity: controlplane.Identity{
			SessionID: snapshot.SessionID, WorkspaceID: workspaceID,
			GoalID: snapshot.ID,
		},
		ExternalID:    decision.ID,
		Summary:       cortexDecisionControlSummary,
		PayloadJSON:   payload,
		PayloadSHA256: payloadDigest,
	})
	if err != nil {
		return fmt.Errorf("persist Cortex decision: %w", err)
	}
	return nil
}

func (m *Model) resolveCortexDecisionControlItems(snapshot goal.Snapshot, advice goaladvisor.Advice) error {
	if m.sessionStore == nil {
		return nil
	}
	if m.executionLease == nil {
		return fmt.Errorf("durable Cortex decision resolution requires the active session lease")
	}
	if advice.PendingDecision || advice.Decision != nil ||
		strings.EqualFold(strings.TrimSpace(advice.Phase), "needs_human_decision") || advice.Degraded {
		return fmt.Errorf("cortex decision resolution requires status with no pending decision")
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return fmt.Errorf("resolve Cortex decision workspace: %w", err)
	}
	taskID, err := cortexDecisionTaskBinding(snapshot, advice.TaskID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), goalControlPlaneTimeout)
	defer cancel()
	states, err := m.sessionStore.ListControlStates(ctx, controlplane.Query{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: snapshot.ID,
		PendingOnly: true, Limit: controlplane.MaxListLimit,
	})
	if err != nil {
		return fmt.Errorf("list pending Cortex decisions: %w", err)
	}
	if len(states) == 0 {
		return nil
	}
	type pendingDecisionState struct {
		state   controlplane.State
		binding cortexDecisionControlBinding
	}
	pending := make([]pendingDecisionState, 0, len(states))
	for _, state := range states {
		if err := state.Item.Validate(); err != nil {
			return fmt.Errorf("validate pending Cortex decision %s: %w", state.Item.ItemID, err)
		}
		if state.Item.Kind != controlplane.KindCortexDecision ||
			state.Item.Identity.SessionID != snapshot.SessionID ||
			state.Item.Identity.WorkspaceID != workspaceID ||
			state.Item.Identity.GoalID != snapshot.ID {
			return fmt.Errorf("pending Cortex decision %s has inconsistent durable identity", state.Item.ItemID)
		}
		binding, parseErr := parseCortexDecisionControlBinding(state.Item.PayloadJSON)
		if parseErr != nil {
			return fmt.Errorf("validate pending Cortex decision %s binding: %w", state.Item.ItemID, parseErr)
		}
		if binding.TaskID != taskID {
			return fmt.Errorf("pending Cortex decision %s has a different task binding", state.Item.ItemID)
		}
		if state.Item.ExternalID != binding.DecisionID {
			return fmt.Errorf("pending Cortex decision %s has a different decision binding", state.Item.ItemID)
		}
		itemID, idempotencyKey := cortexDecisionControlIdentity(
			snapshot.SessionID, snapshot.ID, binding.TaskID, binding.DecisionID,
		)
		if state.Item.ItemID != itemID || state.Item.IdempotencyKey != idempotencyKey {
			return fmt.Errorf("pending Cortex decision %s has an inconsistent stable identity", state.Item.ItemID)
		}
		pending = append(pending, pendingDecisionState{state: state, binding: binding})
	}
	for _, candidate := range pending {
		state := candidate.state
		binding := candidate.binding
		evidence, evidenceDigest, encodeErr := controlplane.MarshalDocument(map[string]any{
			"item_id":                    state.Item.ItemID,
			"decision_payload_sha256":    state.Item.PayloadSHA256,
			"decision_id":                binding.DecisionID,
			"decision_no_longer_pending": true,
			"request_sha256":             binding.RequestSHA256,
			"resolution_source":          "cortex_status",
			"task_id":                    binding.TaskID,
			"version":                    cortexDecisionControlBindingVersion,
		})
		if encodeErr != nil {
			return fmt.Errorf("encode Cortex decision resolution: %w", encodeErr)
		}
		resolutionHash := controlplane.HashText(fmt.Sprintf(
			"cortex-resolution\x00%s\x00%s",
			state.Item.ItemID, evidenceDigest,
		))
		_, _, err := m.sessionStore.ResolveControlItem(ctx, m.executionLease, controlplane.Resolution{
			ResolutionID:   "ctrlres_cortex_" + resolutionHash[:32],
			IdempotencyKey: "ctrlidem_cortex_resolution_" + resolutionHash[:32],
			ItemID:         state.Item.ItemID,
			SessionID:      snapshot.SessionID,
			WorkspaceID:    workspaceID,
			// Fresh status proves only that this request is no longer pending. It
			// does not identify an answer, so reserve OutcomeAnswered for the exact
			// local AnswerDecision receipt below.
			Outcome:        controlplane.OutcomeDismissed,
			EvidenceJSON:   evidence,
			EvidenceSHA256: evidenceDigest,
			ResolvedBy:     goalActor,
			Detail:         "Cortex decision settled",
		})
		if err != nil {
			return fmt.Errorf("resolve Cortex decision %s: %w", state.Item.ItemID, err)
		}
	}
	return nil
}

// supersedeCortexDecisionControlItem settles the exact fenced request when a
// fresh Cortex status has advanced directly to a different pending decision.
// The new request's stable ID and hash are evidence of supersession; its
// question, labels, requester, and consequences never enter durable state.
// A same-ID request with a changed hash is not supersession and stays
// fail-closed at the caller.
func (m *Model) supersedeCortexDecisionControlItem(
	snapshot goal.Snapshot,
	attempt *cortexDecisionAttempt,
	nextDecisionID, nextRequestSHA256 string,
) error {
	if m.sessionStore == nil {
		return fmt.Errorf("durable Cortex decision supersession requires session storage")
	}
	if m.executionLease == nil {
		return fmt.Errorf("durable Cortex decision supersession requires the active session lease")
	}
	if err := validateRestoredCortexDecisionAttempt(attempt, &snapshot); err != nil {
		return err
	}
	if nextDecisionID == attempt.DecisionID {
		return fmt.Errorf("cortex decision supersession requires a different decision id")
	}
	if err := validateCortexDecisionBindingText("next decision id", nextDecisionID, maxCortexDecisionControlStableIDBytes); err != nil {
		return err
	}
	nextHash, err := hex.DecodeString(nextRequestSHA256)
	if err != nil || len(nextHash) != 32 || hex.EncodeToString(nextHash) != nextRequestSHA256 {
		return fmt.Errorf("next Cortex decision request SHA-256 is invalid")
	}

	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return fmt.Errorf("resolve superseded Cortex decision workspace: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), goalControlPlaneTimeout)
	defer cancel()
	states, err := m.sessionStore.ListControlStates(ctx, controlplane.Query{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: snapshot.ID,
		Limit: controlplane.MaxListLimit,
	})
	if err != nil {
		return fmt.Errorf("list Cortex decisions for supersession: %w", err)
	}

	var candidate *controlplane.State
	var candidateBinding cortexDecisionControlBinding
	for index := range states {
		state := &states[index]
		if err := state.Item.Validate(); err != nil {
			return fmt.Errorf("validate Cortex decision %s: %w", state.Item.ItemID, err)
		}
		if state.Item.Kind != controlplane.KindCortexDecision ||
			state.Item.Identity.SessionID != snapshot.SessionID ||
			state.Item.Identity.WorkspaceID != workspaceID ||
			state.Item.Identity.GoalID != snapshot.ID {
			return fmt.Errorf("cortex decision %s has inconsistent durable identity", state.Item.ItemID)
		}
		binding, parseErr := parseCortexDecisionControlBinding(state.Item.PayloadJSON)
		if parseErr != nil {
			return fmt.Errorf("validate Cortex decision %s binding: %w", state.Item.ItemID, parseErr)
		}
		itemID, idempotencyKey := cortexDecisionControlIdentity(
			snapshot.SessionID, snapshot.ID, binding.TaskID, binding.DecisionID,
		)
		if state.Item.ItemID != itemID || state.Item.IdempotencyKey != idempotencyKey ||
			state.Item.ExternalID != binding.DecisionID {
			return fmt.Errorf("cortex decision %s has an inconsistent stable identity", state.Item.ItemID)
		}
		if binding.TaskID != attempt.TaskID || binding.DecisionID != attempt.DecisionID {
			continue
		}
		if binding.RequestSHA256 != attempt.RequestSHA256 {
			return fmt.Errorf("fenced Cortex decision request binding changed")
		}
		if candidate != nil {
			return fmt.Errorf("exact fenced Cortex decision binding was not unique")
		}
		candidate = state
		candidateBinding = binding
	}
	if candidate == nil {
		return fmt.Errorf("exact fenced Cortex decision binding was not found")
	}
	if candidate.Resolution != nil {
		if err := candidate.Resolution.Validate(); err != nil {
			return fmt.Errorf("validate resolved Cortex decision %s: %w", candidate.Item.ItemID, err)
		}
		if candidate.Resolution.ItemID != candidate.Item.ItemID ||
			candidate.Resolution.SessionID != snapshot.SessionID ||
			candidate.Resolution.WorkspaceID != workspaceID ||
			!candidate.Resolution.Outcome.ValidFor(controlplane.KindCortexDecision) {
			return fmt.Errorf("resolved Cortex decision %s has inconsistent durable identity", candidate.Item.ItemID)
		}
		return nil
	}

	evidence, evidenceDigest, err := controlplane.MarshalDocument(map[string]any{
		"decision_id":                  candidateBinding.DecisionID,
		"decision_payload_sha256":      candidate.Item.PayloadSHA256,
		"item_id":                      candidate.Item.ItemID,
		"request_sha256":               candidateBinding.RequestSHA256,
		"resolution_source":            "cortex_status_superseded",
		"superseded_by_decision_id":    nextDecisionID,
		"superseded_by_request_sha256": nextRequestSHA256,
		"task_id":                      candidateBinding.TaskID,
		"version":                      cortexDecisionControlBindingVersion,
	})
	if err != nil {
		return fmt.Errorf("encode superseded Cortex decision resolution: %w", err)
	}
	resolutionHash := controlplane.HashText(fmt.Sprintf(
		"cortex-superseded-resolution\x00%s\x00%s",
		candidate.Item.ItemID, evidenceDigest,
	))
	_, _, err = m.sessionStore.ResolveControlItem(ctx, m.executionLease, controlplane.Resolution{
		ResolutionID:   "ctrlres_cortex_" + resolutionHash[:32],
		IdempotencyKey: "ctrlidem_cortex_resolution_" + resolutionHash[:32],
		ItemID:         candidate.Item.ItemID,
		SessionID:      snapshot.SessionID,
		WorkspaceID:    workspaceID,
		Outcome:        controlplane.OutcomeDismissed,
		EvidenceJSON:   evidence,
		EvidenceSHA256: evidenceDigest,
		ResolvedBy:     goalActor,
		Detail:         "Cortex decision superseded",
	})
	if err != nil {
		return fmt.Errorf("resolve superseded Cortex decision %s: %w", candidate.Item.ItemID, err)
	}
	return nil
}

// resolveAnsweredCortexDecisionControlItem resolves only the exact durable
// request that authorized a successful local AnswerDecision call. The selected
// option ID is the sole answer value retained in evidence; display labels,
// question text, and consequences never cross the control-plane boundary.
func (m *Model) resolveAnsweredCortexDecisionControlItem(
	snapshot goal.Snapshot,
	taskID, decisionID, selectedOptionID, requestSHA256 string,
) error {
	if m.sessionStore == nil {
		return nil
	}
	if m.executionLease == nil {
		return fmt.Errorf("durable Cortex answer resolution requires the active session lease")
	}
	boundTaskID, err := cortexDecisionTaskBinding(snapshot, taskID)
	if err != nil {
		return err
	}
	if err := validateCortexDecisionBindingText("decision id", decisionID, maxCortexDecisionControlStableIDBytes); err != nil {
		return err
	}
	if err := validateCortexDecisionBindingText("selected option id", selectedOptionID, maxCortexDecisionControlStableIDBytes); err != nil {
		return err
	}
	if len(requestSHA256) != 64 {
		return fmt.Errorf("cortex answer request binding is invalid")
	}
	if _, err := hex.DecodeString(requestSHA256); err != nil {
		return fmt.Errorf("cortex answer request binding is invalid")
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return fmt.Errorf("resolve Cortex answer workspace: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), goalControlPlaneTimeout)
	defer cancel()
	states, err := m.sessionStore.ListControlStates(ctx, controlplane.Query{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: snapshot.ID,
		PendingOnly: true, Limit: controlplane.MaxListLimit,
	})
	if err != nil {
		return fmt.Errorf("list pending Cortex decisions: %w", err)
	}

	type exactAnswerCandidate struct {
		state   controlplane.State
		binding cortexDecisionControlBinding
	}
	var candidates []exactAnswerCandidate
	for _, state := range states {
		if err := state.Item.Validate(); err != nil {
			return fmt.Errorf("validate pending Cortex decision %s: %w", state.Item.ItemID, err)
		}
		if state.Item.Kind != controlplane.KindCortexDecision ||
			state.Item.Identity.SessionID != snapshot.SessionID ||
			state.Item.Identity.WorkspaceID != workspaceID ||
			state.Item.Identity.GoalID != snapshot.ID {
			return fmt.Errorf("pending Cortex decision %s has inconsistent durable identity", state.Item.ItemID)
		}
		binding, parseErr := parseCortexDecisionControlBinding(state.Item.PayloadJSON)
		if parseErr != nil {
			return fmt.Errorf("validate pending Cortex decision %s binding: %w", state.Item.ItemID, parseErr)
		}
		itemID, idempotencyKey := cortexDecisionControlIdentity(
			snapshot.SessionID, snapshot.ID, binding.TaskID, binding.DecisionID,
		)
		if state.Item.ItemID != itemID || state.Item.IdempotencyKey != idempotencyKey ||
			state.Item.ExternalID != binding.DecisionID {
			return fmt.Errorf("pending Cortex decision %s has an inconsistent stable identity", state.Item.ItemID)
		}
		if binding.TaskID == boundTaskID && binding.DecisionID == decisionID {
			candidates = append(candidates, exactAnswerCandidate{state: state, binding: binding})
		}
	}
	if len(candidates) != 1 {
		return fmt.Errorf("exact pending Cortex decision binding was not unique")
	}
	candidate := candidates[0]
	if candidate.binding.RequestSHA256 != requestSHA256 {
		return fmt.Errorf("pending Cortex decision request binding changed")
	}
	selectedIsBound := false
	for _, optionID := range candidate.binding.OptionIDs {
		if optionID == selectedOptionID {
			selectedIsBound = true
			break
		}
	}
	if !selectedIsBound {
		return fmt.Errorf("selected Cortex option is not bound to the pending request")
	}

	evidence, evidenceDigest, err := controlplane.MarshalDocument(map[string]any{
		"decision_id":             candidate.binding.DecisionID,
		"decision_payload_sha256": candidate.state.Item.PayloadSHA256,
		"item_id":                 candidate.state.Item.ItemID,
		"request_sha256":          candidate.binding.RequestSHA256,
		"resolution_source":       "local_answer_decision",
		"selected_option_id":      selectedOptionID,
		"task_id":                 candidate.binding.TaskID,
		"version":                 cortexDecisionControlBindingVersion,
	})
	if err != nil {
		return fmt.Errorf("encode Cortex answer resolution: %w", err)
	}
	resolutionHash := controlplane.HashText(fmt.Sprintf(
		"cortex-answer-resolution\x00%s\x00%s\x00%s",
		candidate.state.Item.ItemID, selectedOptionID, evidenceDigest,
	))
	_, _, err = m.sessionStore.ResolveControlItem(ctx, m.executionLease, controlplane.Resolution{
		ResolutionID:   "ctrlres_cortex_" + resolutionHash[:32],
		IdempotencyKey: "ctrlidem_cortex_resolution_" + resolutionHash[:32],
		ItemID:         candidate.state.Item.ItemID,
		SessionID:      snapshot.SessionID,
		WorkspaceID:    workspaceID,
		Outcome:        controlplane.OutcomeAnswered,
		EvidenceJSON:   evidence,
		EvidenceSHA256: evidenceDigest,
		ResolvedBy:     goalActor,
		Detail:         "Cortex decision answered",
	})
	if err != nil {
		return fmt.Errorf("resolve Cortex decision %s: %w", candidate.state.Item.ItemID, err)
	}
	return nil
}

func cortexDecisionControlIdentity(sessionID int64, goalID, taskID, decisionID string) (string, string) {
	identityHash := controlplane.HashText(fmt.Sprintf(
		"cortex-decision\x00%d\x00%s\x00%s\x00%s",
		sessionID, goalID, taskID, decisionID,
	))
	return "ctrl_cortex_" + identityHash[:32], "ctrlidem_cortex_" + identityHash[:32]
}

func cortexDecisionTaskBinding(snapshot goal.Snapshot, adviceTaskID string) (string, error) {
	taskID := adviceTaskID
	if taskID == "" {
		taskID = snapshot.Cortex.TaskID
	}
	if err := validateCortexDecisionBindingText("task id", taskID, goal.MaxCorrelationIDBytes); err != nil {
		return "", fmt.Errorf("cortex decision has an invalid task binding: %w", err)
	}
	if snapshot.Cortex.TaskID != "" && taskID != snapshot.Cortex.TaskID {
		return "", fmt.Errorf("cortex decision task does not match the durable goal task")
	}
	return taskID, nil
}

func parseCortexDecisionControlBinding(document string) (cortexDecisionControlBinding, error) {
	if err := rejectDuplicateCortexDecisionBindingFields(document); err != nil {
		return cortexDecisionControlBinding{}, fmt.Errorf("cortex decision binding is not a valid safe object")
	}
	var wire cortexDecisionControlBindingWire
	decoder := json.NewDecoder(bytes.NewBufferString(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return cortexDecisionControlBinding{}, fmt.Errorf("cortex decision binding does not match the safe schema")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return cortexDecisionControlBinding{}, fmt.Errorf("binding contains trailing JSON")
		}
		return cortexDecisionControlBinding{}, fmt.Errorf("cortex decision binding has invalid trailing JSON")
	}
	if wire.Sensitive == nil {
		return cortexDecisionControlBinding{}, fmt.Errorf("binding sensitive flag is required")
	}
	binding := cortexDecisionControlBinding{
		Version:       wire.Version,
		TaskID:        wire.TaskID,
		DecisionID:    wire.DecisionID,
		RequestedAt:   wire.RequestedAt,
		OptionIDs:     wire.OptionIDs,
		Sensitive:     *wire.Sensitive,
		RequestSHA256: wire.RequestSHA256,
	}
	if err := validateCortexDecisionControlBinding(binding); err != nil {
		return cortexDecisionControlBinding{}, err
	}
	return binding, nil
}

func rejectDuplicateCortexDecisionBindingFields(document string) error {
	decoder := json.NewDecoder(bytes.NewBufferString(document))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode binding object: %w", err)
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return fmt.Errorf("binding must be a JSON object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return fmt.Errorf("decode binding field: %w", err)
		}
		field, ok := token.(string)
		if !ok {
			return fmt.Errorf("binding field name is invalid")
		}
		if _, duplicate := seen[field]; duplicate {
			return fmt.Errorf("binding contains duplicate fields")
		}
		seen[field] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode binding field value: %w", err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("decode binding object close: %w", err)
	}
	return nil
}

func validateCortexDecisionControlBinding(binding cortexDecisionControlBinding) error {
	if binding.Version != cortexDecisionControlBindingVersion {
		return fmt.Errorf("unsupported Cortex decision binding version")
	}
	if err := validateCortexDecisionBindingText("task id", binding.TaskID, goal.MaxCorrelationIDBytes); err != nil {
		return err
	}
	if err := validateCortexDecisionBindingText("decision id", binding.DecisionID, maxCortexDecisionControlStableIDBytes); err != nil {
		return err
	}
	requestedAt, err := time.Parse(time.RFC3339Nano, binding.RequestedAt)
	if err != nil || requestedAt.IsZero() || requestedAt.UTC().Format(time.RFC3339Nano) != binding.RequestedAt {
		return fmt.Errorf("cortex decision binding requested_at is not canonical UTC")
	}
	if len(binding.OptionIDs) < minCortexDecisionControlOptionIDs || len(binding.OptionIDs) > maxCortexDecisionControlOptionIDs {
		return fmt.Errorf("cortex decision binding option_ids count is invalid")
	}
	seen := make(map[string]struct{}, len(binding.OptionIDs))
	for _, optionID := range binding.OptionIDs {
		if err := validateCortexDecisionBindingText("option id", optionID, maxCortexDecisionControlStableIDBytes); err != nil {
			return err
		}
		if _, duplicate := seen[optionID]; duplicate {
			return fmt.Errorf("cortex decision binding option_ids must be unique")
		}
		seen[optionID] = struct{}{}
	}
	decoded, err := hex.DecodeString(binding.RequestSHA256)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != binding.RequestSHA256 {
		return fmt.Errorf("cortex decision request SHA-256 is invalid")
	}
	return nil
}

func validateCortexDecisionBindingText(field, value string, limit int) error {
	if !utf8.ValidString(value) || value == "" || value != strings.TrimSpace(value) || len(value) > limit {
		return fmt.Errorf("cortex decision binding %s is invalid", field)
	}
	for _, r := range value {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.In(r, unicode.Cf) || r == '\u2028' || r == '\u2029' {
			return fmt.Errorf("cortex decision binding %s is invalid", field)
		}
	}
	return nil
}

func (m *Model) protectControlPlaneFailure(operation string, controlErr error) {
	if controlErr == nil || m.goalRuntime == nil {
		return
	}
	current, snapshotErr := m.goalRuntime.Snapshot(context.Background())
	var blockErr error
	if snapshotErr == nil && !current.State.Terminal() {
		blockErr = ensureGoalBlock(m.goalRuntime, goal.BlockDecision,
			fallbackGoalText(current.Cortex.TaskID, current.ID),
			"durable Cortex decision evidence could not be recorded",
		)
	}
	persistErr := m.persistGoalSession()
	m.appendGoalError(operation + ": " + errors.Join(controlErr, snapshotErr, blockErr, persistErr).Error())
}
