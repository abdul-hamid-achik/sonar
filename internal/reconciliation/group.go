package reconciliation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	GroupEvidenceVersion     = 1
	MaxBlockerReferenceBytes = 256
	// MaxGroupMembers leaves one slot in goal.MaxReconciliationTargets for the
	// required turn-level parent authority.
	MaxGroupMembers = 9_999
)

// TurnConclusion is the authority conclusion for a lost provider turn. It is
// intentionally distinct from an execution-effect disposition.
type TurnConclusion string

const TurnAbandonedAfterInspection TurnConclusion = "turn_abandoned_after_inspection"

func (c TurnConclusion) Valid() bool { return c == TurnAbandonedAfterInspection }

// TurnRequest is the operator-authored evidence for the turn-level parent.
// The coordinator derives every durable target field.
type TurnRequest struct {
	Conclusion TurnConclusion `json:"conclusion"`
	Source     Source         `json:"source"`
	Summary    string         `json:"summary"`
}

func (r TurnRequest) Validate() error {
	if !r.Conclusion.Valid() {
		return fmt.Errorf("invalid reconciliation turn conclusion %q", r.Conclusion)
	}
	if err := r.Source.Validate(); err != nil {
		return err
	}
	return validateText("turn evidence summary", r.Summary, MaxSummaryBytes)
}

// GroupTarget is the repository-derived turn-level authority binding. A group
// exists even when ExecutionMemberCount is zero, covering provider crashes
// that produced no execution lifecycle.
type GroupTarget struct {
	SessionID            int64
	WorkspaceID          string
	GoalID               string
	TurnID               string
	GroupItemID          string
	GroupPayloadSHA256   string
	BlockerReference     string
	GoalSnapshotSHA256   string
	SnapshotCursor       int64
	MemberSetSHA256      string
	ExecutionMemberCount int
	Actor                string
}

func (t GroupTarget) Validate() error {
	if t.SessionID <= 0 {
		return errors.New("group target session id must be positive")
	}
	if err := validateText("group target workspace id", t.WorkspaceID, MaxWorkspaceBytes); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"group target goal id", t.GoalID, MaxIdentityBytes},
		{"group target turn id", t.TurnID, MaxIdentityBytes},
		{"group target item id", t.GroupItemID, MaxIdentityBytes},
		{"group target blocker reference", t.BlockerReference, MaxBlockerReferenceBytes},
		{"group target actor", t.Actor, MaxActorBytes},
	} {
		if err := validateText(field.name, field.value, field.limit); err != nil {
			return err
		}
	}
	for _, digest := range []struct {
		name  string
		value string
	}{
		{"group payload", t.GroupPayloadSHA256},
		{"goal snapshot", t.GoalSnapshotSHA256},
		{"group member set", t.MemberSetSHA256},
	} {
		if !validDigest(digest.value) {
			return fmt.Errorf("%s SHA-256 is invalid", digest.name)
		}
	}
	if t.SnapshotCursor < 0 {
		return errors.New("group target snapshot cursor must not be negative")
	}
	if t.ExecutionMemberCount < 0 || t.ExecutionMemberCount > MaxGroupMembers {
		return fmt.Errorf("group execution member count must be between 0 and %d", MaxGroupMembers)
	}
	return nil
}

// GroupEnvelope is the typed evidence for the required turn-level parent.
// Execution effect dispositions remain on exact member envelopes.
type GroupEnvelope struct {
	Version              int            `json:"version"`
	SessionID            int64          `json:"session_id"`
	WorkspaceID          string         `json:"workspace_id"`
	GoalID               string         `json:"goal_id"`
	TurnID               string         `json:"turn_id"`
	GroupItemID          string         `json:"group_item_id"`
	GroupPayloadSHA256   string         `json:"group_payload_sha256"`
	BlockerReference     string         `json:"blocker_reference"`
	GoalSnapshotSHA256   string         `json:"goal_snapshot_sha256"`
	SnapshotCursor       int64          `json:"snapshot_cursor"`
	MemberSetSHA256      string         `json:"member_set_sha256"`
	ExecutionMemberCount int            `json:"execution_member_count"`
	Actor                string         `json:"actor"`
	Conclusion           TurnConclusion `json:"conclusion"`
	Source               Source         `json:"source"`
	Summary              string         `json:"summary"`
}

func (r TurnRequest) Bind(target GroupTarget) (GroupEnvelope, error) {
	envelope := GroupEnvelope{
		Version:   GroupEvidenceVersion,
		SessionID: target.SessionID, WorkspaceID: target.WorkspaceID,
		GoalID: target.GoalID, TurnID: target.TurnID, GroupItemID: target.GroupItemID,
		GroupPayloadSHA256: target.GroupPayloadSHA256, BlockerReference: target.BlockerReference,
		GoalSnapshotSHA256: target.GoalSnapshotSHA256, SnapshotCursor: target.SnapshotCursor,
		MemberSetSHA256: target.MemberSetSHA256, ExecutionMemberCount: target.ExecutionMemberCount,
		Actor: target.Actor, Conclusion: r.Conclusion, Source: r.Source, Summary: r.Summary,
	}
	if err := envelope.Validate(); err != nil {
		return GroupEnvelope{}, err
	}
	return envelope, nil
}

func (e GroupEnvelope) Target() GroupTarget {
	return GroupTarget{
		SessionID: e.SessionID, WorkspaceID: e.WorkspaceID,
		GoalID: e.GoalID, TurnID: e.TurnID, GroupItemID: e.GroupItemID,
		GroupPayloadSHA256: e.GroupPayloadSHA256, BlockerReference: e.BlockerReference,
		GoalSnapshotSHA256: e.GoalSnapshotSHA256, SnapshotCursor: e.SnapshotCursor,
		MemberSetSHA256: e.MemberSetSHA256, ExecutionMemberCount: e.ExecutionMemberCount,
		Actor: e.Actor,
	}
}

func (e GroupEnvelope) Validate() error {
	if e.Version != GroupEvidenceVersion {
		return fmt.Errorf("unsupported reconciliation group evidence version %d", e.Version)
	}
	if err := e.Target().Validate(); err != nil {
		return err
	}
	return (TurnRequest{Conclusion: e.Conclusion, Source: e.Source, Summary: e.Summary}).Validate()
}

func (e GroupEnvelope) MatchesTarget(target GroupTarget) bool {
	return e.Target() == target
}

func (e GroupEnvelope) Marshal() (string, string, error) {
	if err := e.Validate(); err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return "", "", fmt.Errorf("marshal reconciliation group evidence: %w", err)
	}
	if len(encoded) > MaxEnvelopeBytes {
		return "", "", fmt.Errorf("reconciliation group evidence exceeds %d bytes", MaxEnvelopeBytes)
	}
	document := string(encoded)
	return document, Hash(document), nil
}

func ParseGroup(document, digest string) (GroupEnvelope, error) {
	if !utf8.ValidString(document) {
		return GroupEnvelope{}, errors.New("reconciliation group evidence is not valid UTF-8")
	}
	if len(document) == 0 || len(document) > MaxEnvelopeBytes {
		return GroupEnvelope{}, fmt.Errorf("reconciliation group evidence size must be between 1 and %d bytes", MaxEnvelopeBytes)
	}
	if !validDigest(digest) || Hash(document) != digest {
		return GroupEnvelope{}, errors.New("reconciliation group evidence SHA-256 does not match document")
	}
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()
	var envelope GroupEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return GroupEnvelope{}, fmt.Errorf("decode reconciliation group evidence: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return GroupEnvelope{}, err
	}
	if err := envelope.Validate(); err != nil {
		return GroupEnvelope{}, err
	}
	canonical, _, err := envelope.Marshal()
	if err != nil {
		return GroupEnvelope{}, err
	}
	if !bytes.Equal([]byte(document), []byte(canonical)) {
		return GroupEnvelope{}, errors.New("reconciliation group evidence JSON is not canonical")
	}
	return envelope, nil
}
