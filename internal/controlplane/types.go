// Package controlplane defines the dependency-free durable contract for
// decisions, deferred approvals, and execution-outcome reconciliation.
package controlplane

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxWorkspaceIDBytes    = 4 * 1024
	MaxIdentityIDBytes     = 128
	MaxIdempotencyKeyBytes = 128
	MaxExternalIDBytes     = 1024
	MaxSummaryBytes        = 4 * 1024
	MaxDetailBytes         = 4 * 1024
	MaxPayloadBytes        = 16 * 1024
	MaxEvidenceBytes       = 16 * 1024
	MaxActorBytes          = 512

	// MaxListLimit is the largest control-plane read the Store accepts. Callers
	// paginate or narrow by identity instead of loading unbounded history.
	MaxListLimit = 100
)

// Kind identifies the authority boundary represented by an item.
type Kind string

const (
	KindCortexDecision          Kind = "cortex_decision"
	KindDeferredApproval        Kind = "deferred_approval"
	KindExecutionReconciliation Kind = "execution_reconciliation"
)

func (k Kind) Valid() bool {
	switch k {
	case KindCortexDecision, KindDeferredApproval, KindExecutionReconciliation:
		return true
	default:
		return false
	}
}

// Outcome is the terminal resolution attached to one immutable item.
type Outcome string

const (
	OutcomeAnswered   Outcome = "answered"
	OutcomeApproved   Outcome = "approved"
	OutcomeDenied     Outcome = "denied"
	OutcomeReconciled Outcome = "reconciled"
	OutcomeDismissed  Outcome = "dismissed"
)

func (o Outcome) Valid() bool {
	switch o {
	case OutcomeAnswered, OutcomeApproved, OutcomeDenied, OutcomeReconciled, OutcomeDismissed:
		return true
	default:
		return false
	}
}

// ValidFor reports whether an outcome can resolve this kind of item. An
// execution reconciliation may only be closed by affirmative reconciliation;
// dismissing it would erase an unknown external effect without evidence.
func (o Outcome) ValidFor(kind Kind) bool {
	switch kind {
	case KindCortexDecision:
		return o == OutcomeAnswered || o == OutcomeDismissed
	case KindDeferredApproval:
		return o == OutcomeApproved || o == OutcomeDenied || o == OutcomeDismissed
	case KindExecutionReconciliation:
		return o == OutcomeReconciled
	default:
		return false
	}
}

// Identity scopes an immutable control item. Goal, execution, and turn IDs are
// optional except that execution reconciliation must identify an execution.
type Identity struct {
	SessionID   int64
	WorkspaceID string
	GoalID      string
	ExecutionID string
	TurnID      string
}

func (i Identity) Validate() error {
	if i.SessionID <= 0 {
		return errors.New("session id must be positive")
	}
	for _, field := range []struct {
		name     string
		value    string
		limit    int
		required bool
	}{
		{"workspace id", i.WorkspaceID, MaxWorkspaceIDBytes, true},
		{"goal id", i.GoalID, MaxIdentityIDBytes, false},
		{"execution id", i.ExecutionID, MaxIdentityIDBytes, false},
		{"turn id", i.TurnID, MaxIdentityIDBytes, false},
	} {
		if err := validateText(field.name, field.value, field.limit, field.required); err != nil {
			return err
		}
	}
	return nil
}

// Item is an immutable request for an authority-bearing decision. PayloadJSON
// is a caller-redacted presentation envelope, not a home for raw secrets.
type Item struct {
	ID             int64
	ItemID         string
	IdempotencyKey string
	Kind           Kind
	Identity       Identity
	ExternalID     string
	Summary        string
	PayloadJSON    string
	PayloadSHA256  string
	CreatedAt      time.Time
	RecordedAt     time.Time
}

// Validate checks an item without requiring store-populated row IDs or times.
func (i Item) Validate() error {
	if err := i.Identity.Validate(); err != nil {
		return err
	}
	if !i.Kind.Valid() {
		return fmt.Errorf("invalid control item kind %q", i.Kind)
	}
	if i.Kind == KindExecutionReconciliation && strings.TrimSpace(i.Identity.ExecutionID) == "" {
		return errors.New("execution reconciliation requires an execution id")
	}
	for _, field := range []struct {
		name     string
		value    string
		limit    int
		required bool
	}{
		{"item id", i.ItemID, MaxIdentityIDBytes, true},
		{"idempotency key", i.IdempotencyKey, MaxIdempotencyKeyBytes, true},
		{"external id", i.ExternalID, MaxExternalIDBytes, false},
		{"summary", i.Summary, MaxSummaryBytes, true},
	} {
		if err := validateText(field.name, field.value, field.limit, field.required); err != nil {
			return err
		}
	}
	return validateJSONDocument("payload", i.PayloadJSON, i.PayloadSHA256, MaxPayloadBytes)
}

// Resolution is the single append-only terminal record for an Item. Evidence
// is required and hash-bound so a resolution can never be a bare status flip.
type Resolution struct {
	ID             int64
	ResolutionID   string
	IdempotencyKey string
	ItemID         string
	SessionID      int64
	WorkspaceID    string
	Outcome        Outcome
	EvidenceJSON   string
	EvidenceSHA256 string
	ResolvedBy     string
	Detail         string
	ResolvedAt     time.Time
	RecordedAt     time.Time
}

// Validate checks a resolution without needing its parent item. Outcome/kind
// compatibility is checked atomically by the Store after loading that item.
func (r Resolution) Validate() error {
	if r.SessionID <= 0 {
		return errors.New("session id must be positive")
	}
	if !r.Outcome.Valid() {
		return fmt.Errorf("invalid control resolution outcome %q", r.Outcome)
	}
	for _, field := range []struct {
		name     string
		value    string
		limit    int
		required bool
	}{
		{"workspace id", r.WorkspaceID, MaxWorkspaceIDBytes, true},
		{"resolution id", r.ResolutionID, MaxIdentityIDBytes, true},
		{"idempotency key", r.IdempotencyKey, MaxIdempotencyKeyBytes, true},
		{"item id", r.ItemID, MaxIdentityIDBytes, true},
		{"resolved by", r.ResolvedBy, MaxActorBytes, true},
		{"detail", r.Detail, MaxDetailBytes, false},
	} {
		if err := validateText(field.name, field.value, field.limit, field.required); err != nil {
			return err
		}
	}
	return validateJSONDocument("evidence", r.EvidenceJSON, r.EvidenceSHA256, MaxEvidenceBytes)
}

// State is the current projection derived from one item and its optional
// immutable resolution.
type State struct {
	Item       Item
	Resolution *Resolution
}

func (s State) Pending() bool { return s.Resolution == nil }

// Query is a bounded, session-scoped control-plane query. Empty identity and
// kind fields are wildcards.
type Query struct {
	SessionID   int64
	WorkspaceID string
	Kind        Kind
	GoalID      string
	ExecutionID string
	TurnID      string
	PendingOnly bool
	Limit       int
}

func (q Query) Validate() error {
	if err := (Identity{
		SessionID: q.SessionID, WorkspaceID: q.WorkspaceID,
		GoalID: q.GoalID, ExecutionID: q.ExecutionID, TurnID: q.TurnID,
	}).Validate(); err != nil {
		return err
	}
	if q.Kind != "" && !q.Kind.Valid() {
		return fmt.Errorf("invalid control item kind %q", q.Kind)
	}
	if q.Limit <= 0 || q.Limit > MaxListLimit {
		return fmt.Errorf("control-plane list limit must be between 1 and %d", MaxListLimit)
	}
	return nil
}

// MarshalDocument returns deterministic JSON and the hash callers bind into
// Item.PayloadSHA256 or Resolution.EvidenceSHA256. The top level must be an
// object so documents can evolve through named fields.
func MarshalDocument(value any) (string, string, error) {
	if value == nil {
		value = map[string]any{}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", "", fmt.Errorf("marshal control-plane document: %w", err)
	}
	document := string(encoded)
	if err := validateJSONObject(document, MaxPayloadBytes); err != nil {
		return "", "", err
	}
	return document, HashText(document), nil
}

// HashText returns the lowercase hexadecimal SHA-256 of text.
func HashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validateJSONDocument(name, document, digest string, limit int) error {
	if err := validateJSONObject(document, limit); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := validateSHA256(name, digest); err != nil {
		return err
	}
	if got := HashText(document); got != digest {
		return fmt.Errorf("%s SHA-256 does not match document", name)
	}
	return nil
}

func validateJSONObject(document string, limit int) error {
	if !utf8.ValidString(document) {
		return errors.New("document is not valid UTF-8")
	}
	if len(document) == 0 {
		return errors.New("document is required")
	}
	if len(document) > limit {
		return fmt.Errorf("document exceeds %d bytes", limit)
	}
	trimmed := strings.TrimSpace(document)
	if len(trimmed) < 2 || trimmed[0] != '{' || !json.Valid([]byte(trimmed)) {
		return errors.New("document must be a valid JSON object")
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("%s SHA-256 must be 64 lowercase hexadecimal characters", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return fmt.Errorf("%s SHA-256 must be 64 lowercase hexadecimal characters", name)
	}
	return nil
}

func validateText(name, value string, limit int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	return nil
}

// NewItemID returns a cryptographically random process-independent item ID.
func NewItemID() (string, error) { return newRandomID("ctrl_") }

// NewResolutionID returns a cryptographically random resolution ID.
func NewResolutionID() (string, error) { return newRandomID("ctrlres_") }

// NewIdempotencyKey returns a stable key callers retain for exact replay.
func NewIdempotencyKey() (string, error) { return newRandomID("ctrlidem_") }

func newRandomID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate control-plane id: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}
