// Package reconciliation defines the dependency-free, privacy-bounded
// evidence contract used to resolve an execution whose external effect is
// unknown. The execution ledger remains immutable; this package only describes
// the independently persisted authority receipt.
package reconciliation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// EvidenceVersion is the current durable reconciliation evidence schema.
	EvidenceVersion = 1

	MaxIdentityBytes  = 128
	MaxWorkspaceBytes = 4 * 1024
	MaxReferenceBytes = 1024
	MaxSummaryBytes   = 4 * 1024
	MaxActorBytes     = 512
	MaxEnvelopeBytes  = 16 * 1024
)

// Disposition is the verified state of the external effect. These values are
// deliberately about the effect, not about the execution ledger's terminal
// event, which remains outcome_unknown.
type Disposition string

const (
	DispositionEffectApplied     Disposition = "effect_applied"
	DispositionEffectNotApplied  Disposition = "effect_not_applied"
	DispositionEffectCompensated Disposition = "effect_compensated"
)

func (d Disposition) Valid() bool {
	switch d {
	case DispositionEffectApplied, DispositionEffectNotApplied, DispositionEffectCompensated:
		return true
	default:
		return false
	}
}

// SourceKind describes the independently inspectable source behind the
// reconciliation claim. It intentionally excludes raw tool arguments and raw
// backend results.
type SourceKind string

const (
	SourceExternalReceipt     SourceKind = "external_receipt"
	SourceWorkspaceArtifact   SourceKind = "workspace_artifact"
	SourceVerificationCheck   SourceKind = "verification_check"
	SourceOperatorObservation SourceKind = "operator_observation"
)

func (k SourceKind) Valid() bool {
	switch k {
	case SourceExternalReceipt, SourceWorkspaceArtifact, SourceVerificationCheck, SourceOperatorObservation:
		return true
	default:
		return false
	}
}

// Source binds the claim to a redacted, inspectable reference and the time it
// was observed. Reference is an identifier or location, not a raw result.
type Source struct {
	Kind       SourceKind `json:"kind"`
	Reference  string     `json:"reference"`
	ObservedAt time.Time  `json:"observed_at"`
}

func (s Source) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("invalid reconciliation source kind %q", s.Kind)
	}
	if err := validateText("source reference", s.Reference, MaxReferenceBytes); err != nil {
		return err
	}
	if s.ObservedAt.IsZero() {
		return errors.New("source observed time is required")
	}
	if s.ObservedAt.Location() != time.UTC {
		return errors.New("source observed time must be UTC")
	}
	if s.ObservedAt.Year() < 1 || s.ObservedAt.Year() > 9999 {
		return errors.New("source observed time is outside the supported range")
	}
	return nil
}

// Request is the host-facing, unbound evidence input. It contains no fields
// for raw arguments or raw results; callers supply a bounded redacted summary.
type Request struct {
	Disposition Disposition `json:"disposition"`
	Source      Source      `json:"source"`
	Summary     string      `json:"summary"`
}

func (r Request) Validate() error {
	if !r.Disposition.Valid() {
		return fmt.Errorf("invalid reconciliation disposition %q", r.Disposition)
	}
	if err := r.Source.Validate(); err != nil {
		return err
	}
	return validateText("evidence summary", r.Summary, MaxSummaryBytes)
}

// Target contains repository-derived immutable correlations. Presentation
// layers must never construct it from user input.
type Target struct {
	SessionID         int64
	WorkspaceID       string
	GoalID            string
	ItemID            string
	ItemPayloadSHA256 string
	ExecutionID       string
	TurnID            string
	LatestEventID     int64
	LatestEventType   string
	LatestEventSHA256 string
	Actor             string
}

func (t Target) Validate() error {
	if t.SessionID <= 0 {
		return errors.New("target session id must be positive")
	}
	if err := validateText("target workspace id", t.WorkspaceID, MaxWorkspaceBytes); err != nil {
		return err
	}
	// GoalID is intentionally optional for ordinary NORMAL/AUTO turns. A Goal
	// Runtime recovery still binds its non-empty goal ID; standalone execution
	// reconciliation must not fabricate one merely to record typed evidence.
	if t.GoalID != "" {
		if err := validateText("target goal id", t.GoalID, MaxIdentityBytes); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"target item id", t.ItemID},
		{"target execution id", t.ExecutionID},
		{"target turn id", t.TurnID},
	} {
		if err := validateText(field.name, field.value, MaxIdentityBytes); err != nil {
			return err
		}
	}
	if !validDigest(t.ItemPayloadSHA256) {
		return errors.New("target item payload SHA-256 is invalid")
	}
	if t.LatestEventID <= 0 {
		return errors.New("target latest event id must be positive")
	}
	if t.LatestEventType != EventTypeStarted && t.LatestEventType != EventTypeOutcomeUnknown {
		return fmt.Errorf("target latest event type %q is not reconcilable", t.LatestEventType)
	}
	if !validDigest(t.LatestEventSHA256) {
		return errors.New("target latest event SHA-256 is invalid")
	}
	return validateText("target actor", t.Actor, MaxActorBytes)
}

const (
	EventTypeStarted        = "started"
	EventTypeOutcomeUnknown = "outcome_unknown"
)

// EventFingerprint is the canonical redacted projection used to bind evidence
// to the exact latest immutable event. It contains hashes, never raw arguments,
// results, receipts, or event detail.
type EventFingerprint struct {
	EventID         int64     `json:"event_id"`
	SessionID       int64     `json:"session_id"`
	WorkspaceID     string    `json:"workspace_id"`
	RunID           string    `json:"run_id"`
	ExecutionID     string    `json:"execution_id"`
	IdempotencyKey  string    `json:"idempotency_key"`
	TurnID          string    `json:"turn_id"`
	CanonicalCallID string    `json:"canonical_call_id"`
	ToolName        string    `json:"tool_name"`
	Kind            string    `json:"kind"`
	Iteration       int       `json:"iteration"`
	Ordinal         int       `json:"ordinal"`
	EventType       string    `json:"event_type"`
	EffectClass     string    `json:"effect_class"`
	Approval        string    `json:"approval"`
	ArgumentsSHA256 string    `json:"arguments_sha256"`
	ResultSHA256    string    `json:"result_sha256,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
}

func (f EventFingerprint) Validate() error {
	if f.EventID <= 0 || f.SessionID <= 0 {
		return errors.New("event fingerprint ids must be positive")
	}
	if err := validateText("event fingerprint workspace id", f.WorkspaceID, MaxWorkspaceBytes); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"event fingerprint run id", f.RunID, MaxIdentityBytes},
		{"event fingerprint execution id", f.ExecutionID, MaxIdentityBytes},
		{"event fingerprint idempotency key", f.IdempotencyKey, MaxIdentityBytes},
		{"event fingerprint turn id", f.TurnID, MaxIdentityBytes},
		{"event fingerprint canonical call id", f.CanonicalCallID, MaxReferenceBytes},
		{"event fingerprint tool name", f.ToolName, MaxActorBytes},
	} {
		if err := validateText(field.name, field.value, field.limit); err != nil {
			return err
		}
	}
	if f.EventType != EventTypeStarted && f.EventType != EventTypeOutcomeUnknown {
		return fmt.Errorf("event fingerprint type %q is not reconcilable", f.EventType)
	}
	if f.EffectClass != "read_only" && f.EffectClass != "effectful" && f.EffectClass != "unknown" {
		return fmt.Errorf("event fingerprint effect class %q is invalid", f.EffectClass)
	}
	if f.EventType == EventTypeStarted && f.EffectClass == "read_only" {
		return errors.New("started read-only event fingerprint is not hazardous")
	}
	if f.Kind != "builtin" && f.Kind != "memory" && f.Kind != "mcp" {
		return fmt.Errorf("event fingerprint kind %q is invalid", f.Kind)
	}
	if f.Iteration <= 0 || f.Ordinal <= 0 {
		return errors.New("event fingerprint iteration and ordinal must be positive")
	}
	if !validApproval(f.Approval) {
		return fmt.Errorf("event fingerprint approval %q is invalid", f.Approval)
	}
	if !validDigest(f.ArgumentsSHA256) {
		return errors.New("event fingerprint arguments SHA-256 is invalid")
	}
	if f.ResultSHA256 != "" && !validDigest(f.ResultSHA256) {
		return errors.New("event fingerprint result SHA-256 is invalid")
	}
	if f.OccurredAt.IsZero() || f.OccurredAt.Location() != time.UTC {
		return errors.New("event fingerprint occurrence time must be non-zero UTC")
	}
	return nil
}

// Digest returns the canonical SHA-256 binding for an immutable event.
func (f EventFingerprint) Digest() (string, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}
	document, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("marshal reconciliation event fingerprint: %w", err)
	}
	return Hash(string(document)), nil
}

// Envelope is the immutable, versioned evidence document stored in a control
// resolution. Its repository-derived target binds the human evidence to one
// exact scoped item and immutable execution event.
type Envelope struct {
	Version           int         `json:"version"`
	SessionID         int64       `json:"session_id"`
	WorkspaceID       string      `json:"workspace_id"`
	GoalID            string      `json:"goal_id"`
	ItemID            string      `json:"item_id"`
	ItemPayloadSHA256 string      `json:"item_payload_sha256"`
	ExecutionID       string      `json:"execution_id"`
	TurnID            string      `json:"turn_id"`
	LatestEventID     int64       `json:"latest_event_id"`
	LatestEventType   string      `json:"latest_event_type"`
	LatestEventSHA256 string      `json:"latest_event_sha256"`
	Actor             string      `json:"actor"`
	Disposition       Disposition `json:"disposition"`
	Source            Source      `json:"source"`
	Summary           string      `json:"summary"`
}

// Bind validates a request and binds it to one immutable control item and
// execution lifecycle.
func (r Request) Bind(target Target) (Envelope, error) {
	envelope := Envelope{
		Version:   EvidenceVersion,
		SessionID: target.SessionID, WorkspaceID: target.WorkspaceID,
		GoalID: target.GoalID, ItemID: target.ItemID,
		ItemPayloadSHA256: target.ItemPayloadSHA256,
		ExecutionID:       target.ExecutionID, TurnID: target.TurnID,
		LatestEventID: target.LatestEventID, LatestEventType: target.LatestEventType,
		LatestEventSHA256: target.LatestEventSHA256, Actor: target.Actor,
		Disposition: r.Disposition, Source: r.Source, Summary: r.Summary,
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func (e Envelope) Validate() error {
	if e.Version != EvidenceVersion {
		return fmt.Errorf("unsupported reconciliation evidence version %d", e.Version)
	}
	target := e.Target()
	if err := target.Validate(); err != nil {
		return err
	}
	request := Request{Disposition: e.Disposition, Source: e.Source, Summary: e.Summary}
	return request.Validate()
}

// Target returns the repository binding represented by the envelope.
func (e Envelope) Target() Target {
	return Target{
		SessionID: e.SessionID, WorkspaceID: e.WorkspaceID,
		GoalID: e.GoalID, ItemID: e.ItemID, ItemPayloadSHA256: e.ItemPayloadSHA256,
		ExecutionID: e.ExecutionID, TurnID: e.TurnID,
		LatestEventID: e.LatestEventID, LatestEventType: e.LatestEventType,
		LatestEventSHA256: e.LatestEventSHA256, Actor: e.Actor,
	}
}

// MatchesTarget reports whether the evidence is bound to the exact immutable
// control/execution correlation loaded by the repository.
func (e Envelope) MatchesTarget(target Target) bool {
	return e.Target() == target
}

// Marshal returns canonical JSON and its lowercase SHA-256 digest.
func (e Envelope) Marshal() (string, string, error) {
	if err := e.Validate(); err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return "", "", fmt.Errorf("marshal reconciliation evidence: %w", err)
	}
	if len(encoded) > MaxEnvelopeBytes {
		return "", "", fmt.Errorf("reconciliation evidence exceeds %d bytes", MaxEnvelopeBytes)
	}
	document := string(encoded)
	return document, Hash(document), nil
}

// Parse verifies the digest, rejects unknown fields and non-canonical JSON,
// and returns a fully validated typed envelope.
func Parse(document, digest string) (Envelope, error) {
	if !utf8.ValidString(document) {
		return Envelope{}, errors.New("reconciliation evidence is not valid UTF-8")
	}
	if len(document) == 0 || len(document) > MaxEnvelopeBytes {
		return Envelope{}, fmt.Errorf("reconciliation evidence size must be between 1 and %d bytes", MaxEnvelopeBytes)
	}
	if !validDigest(digest) || Hash(document) != digest {
		return Envelope{}, errors.New("reconciliation evidence SHA-256 does not match document")
	}
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode reconciliation evidence: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Envelope{}, err
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	canonical, _, err := envelope.Marshal()
	if err != nil {
		return Envelope{}, err
	}
	if !bytes.Equal([]byte(document), []byte(canonical)) {
		return Envelope{}, errors.New("reconciliation evidence JSON is not canonical")
	}
	return envelope, nil
}

func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validateText(name, value string, limit int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be canonical without surrounding whitespace", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validApproval(value string) bool {
	switch value {
	case "not_applicable", "requested", "policy", "yolo", "embedding", "once", "always", "denied":
		return true
	default:
		return false
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing reconciliation evidence: %w", err)
	}
	return errors.New("reconciliation evidence contains multiple JSON values")
}
