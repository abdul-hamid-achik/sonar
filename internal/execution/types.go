// Package execution defines the dependency-free lifecycle contract shared by
// the agent runtime and durable stores.
package execution

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
	// MaxResultReceiptBytes bounds the human-readable backend receipt retained
	// in the ledger. The complete result is represented by ResultSHA256.
	MaxResultReceiptBytes = 16 * 1024
	// MaxDetailBytes bounds lifecycle explanations and recovery reason text.
	MaxDetailBytes = 4 * 1024

	MaxWorkspaceIDBytes     = 4 * 1024
	MaxRunIDBytes           = 128
	MaxTurnIDBytes          = 128
	MaxExecutionIDBytes     = 128
	MaxIdempotencyKeyBytes  = 128
	MaxProviderCallIDBytes  = 1024
	MaxCanonicalCallIDBytes = 1024
	MaxToolNameBytes        = 512
)

// Kind identifies the execution backend family without coupling the ledger to
// an agent or provider implementation.
type Kind string

const (
	KindBuiltin Kind = "builtin"
	KindMemory  Kind = "memory"
	KindMCP     Kind = "mcp"
)

func (k Kind) Valid() bool {
	switch k {
	case KindBuiltin, KindMemory, KindMCP:
		return true
	default:
		return false
	}
}

// EffectClass controls conservative recovery behavior after a started event.
type EffectClass string

const (
	EffectReadOnly EffectClass = "read_only"
	Effectful      EffectClass = "effectful"
	EffectUnknown  EffectClass = "unknown"
)

func (c EffectClass) Valid() bool {
	switch c {
	case EffectReadOnly, Effectful, EffectUnknown:
		return true
	default:
		return false
	}
}

// EventType is one immutable transition in an execution lifecycle.
type EventType string

const (
	EventRequested         EventType = "requested"
	EventApprovalRequested EventType = "approval_requested"
	EventApproved          EventType = "approved"
	EventDenied            EventType = "denied"
	EventStarted           EventType = "started"
	EventCompleted         EventType = "completed"
	EventFailed            EventType = "failed"
	EventCancelled         EventType = "cancelled"
	EventOutcomeUnknown    EventType = "outcome_unknown"
)

func (t EventType) Valid() bool {
	switch t {
	case EventRequested, EventApprovalRequested, EventApproved, EventDenied,
		EventStarted, EventCompleted, EventFailed, EventCancelled,
		EventOutcomeUnknown:
		return true
	default:
		return false
	}
}

// Terminal reports whether no later lifecycle event may be appended.
func (t EventType) Terminal() bool {
	switch t {
	case EventDenied, EventCompleted, EventFailed, EventCancelled, EventOutcomeUnknown:
		return true
	default:
		return false
	}
}

// IsTerminal is the predicate form used by runtime lifecycle code.
func (t EventType) IsTerminal() bool { return t.Terminal() }

// Approval records how an approval event was reached. EventType remains the
// state-machine authority; this value provides a bounded audit explanation.
type Approval string

const (
	ApprovalNotApplicable Approval = "not_applicable"
	ApprovalRequested     Approval = "requested"
	ApprovalPolicy        Approval = "policy"
	ApprovalYolo          Approval = "yolo"
	ApprovalEmbedding     Approval = "embedding"
	ApprovalOnce          Approval = "once"
	// ApprovalSession reuses the historical "always" wire value so existing
	// append-only ledgers remain readable. Runtime semantics are now bounded to
	// this Agent session and exact request; no global policy is persisted.
	ApprovalSession Approval = "always"
	// ApprovalAlways is retained only to decode and compile legacy callers.
	ApprovalAlways       Approval = ApprovalSession
	ApprovalUserDenied   Approval = "denied"
	ApprovalPolicyDenied Approval = ApprovalUserDenied
	// Host refusals and cancellations are distinguished by EventFailed and
	// EventCancelled respectively. Reusing not_applicable preserves the stable
	// database wire contract while ensuring neither is EventDenied.
	ApprovalHostRefused Approval = ApprovalNotApplicable
	ApprovalCancelled   Approval = ApprovalNotApplicable
	// ApprovalDenied is retained as a source-compatible alias for old callers.
	// New lifecycle code must choose user_denied or policy_denied explicitly.
	ApprovalDenied Approval = ApprovalUserDenied
)

func (a Approval) Valid() bool {
	switch a {
	case ApprovalNotApplicable, ApprovalRequested, ApprovalPolicy, ApprovalYolo,
		ApprovalEmbedding,
		ApprovalOnce, ApprovalSession, ApprovalUserDenied:
		return true
	default:
		return false
	}
}

// Identity is immutable for every event belonging to one execution.
type Identity struct {
	SessionID       int64
	WorkspaceID     string
	RunID           string
	TurnID          string
	ExecutionID     string
	IdempotencyKey  string
	ProviderCallID  string
	CanonicalCallID string
	ToolName        string
	Iteration       int
	Ordinal         int
	Kind            Kind
	EffectClass     EffectClass
}

// Validate checks identity shape. It intentionally accepts any non-empty ID
// format so deterministic fixtures and imported provider identifiers remain
// possible; New*ID produces the recommended random form.
func (i Identity) Validate() error {
	if i.SessionID <= 0 {
		return fmt.Errorf("session id must be positive")
	}
	for _, field := range []struct {
		name     string
		value    string
		limit    int
		required bool
	}{
		{"workspace id", i.WorkspaceID, MaxWorkspaceIDBytes, true},
		{"run id", i.RunID, MaxRunIDBytes, true},
		{"turn id", i.TurnID, MaxTurnIDBytes, true},
		{"execution id", i.ExecutionID, MaxExecutionIDBytes, true},
		{"idempotency key", i.IdempotencyKey, MaxIdempotencyKeyBytes, true},
		{"provider call id", i.ProviderCallID, MaxProviderCallIDBytes, false},
		{"canonical call id", i.CanonicalCallID, MaxCanonicalCallIDBytes, true},
		{"tool name", i.ToolName, MaxToolNameBytes, true},
	} {
		if field.required && strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
		if len(field.value) > field.limit {
			return fmt.Errorf("%s exceeds %d bytes", field.name, field.limit)
		}
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("%s is not valid UTF-8", field.name)
		}
	}
	if i.Iteration <= 0 {
		return fmt.Errorf("iteration must be positive")
	}
	if i.Ordinal <= 0 {
		return fmt.Errorf("ordinal must be positive")
	}
	if !i.Kind.Valid() {
		return fmt.Errorf("invalid execution kind %q", i.Kind)
	}
	if !i.EffectClass.Valid() {
		return fmt.Errorf("invalid effect class %q", i.EffectClass)
	}
	return nil
}

// Event is the durable, append-only representation of one lifecycle edge.
// Argument bodies are deliberately absent; ArgumentsSHA256 proves identity
// without copying secrets into the ledger.
type Event struct {
	ID              int64
	Identity        Identity
	Type            EventType
	Approval        Approval
	ArgumentsSHA256 string
	ResultSHA256    string
	ResultReceipt   string
	Detail          string
	OccurredAt      time.Time
	RecordedAt      time.Time
}

// Validate checks the event without requiring store-populated IDs or times.
func (e Event) Validate() error {
	if err := e.Identity.Validate(); err != nil {
		return err
	}
	if !e.Type.Valid() {
		return fmt.Errorf("invalid event type %q", e.Type)
	}
	if !e.Approval.Valid() {
		return fmt.Errorf("invalid approval value %q", e.Approval)
	}
	if err := validateSHA256("arguments", e.ArgumentsSHA256, true); err != nil {
		return err
	}
	if err := validateSHA256("result", e.ResultSHA256, false); err != nil {
		return err
	}
	if len(e.ResultReceipt) > MaxResultReceiptBytes {
		return fmt.Errorf("result receipt exceeds %d bytes", MaxResultReceiptBytes)
	}
	if len(e.Detail) > MaxDetailBytes {
		return fmt.Errorf("detail exceeds %d bytes", MaxDetailBytes)
	}
	if !utf8.ValidString(e.ResultReceipt) {
		return errors.New("result receipt is not valid UTF-8")
	}
	if !utf8.ValidString(e.Detail) {
		return errors.New("detail is not valid UTF-8")
	}
	return nil
}

func validateSHA256(name, value string, required bool) error {
	if value == "" && !required {
		return nil
	}
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("%s SHA-256 must be 64 lowercase hexadecimal characters", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return fmt.Errorf("%s SHA-256 must be 64 lowercase hexadecimal characters", name)
	}
	return nil
}

// State is the read model derived from immutable events.
type State struct {
	Identity   Identity
	Latest     Event
	EventCount int
}

func (s State) Terminal() bool { return s.Latest.Type.Terminal() }

// NewTurnID returns a cryptographically random, process-independent turn ID.
func NewTurnID() (string, error) { return newRandomID("turn_") }

// NewRunID returns a cryptographically random identity for one runtime owner.
func NewRunID() (string, error) { return newRandomID("run_") }

// NewExecutionID returns a cryptographically random execution ID.
func NewExecutionID() (string, error) { return newRandomID("exec_") }

// NewIdempotencyKey returns the stable key callers should retain for the
// complete execution and pass to a backend when that backend supports it.
func NewIdempotencyKey() (string, error) { return newRandomID("idem_") }

func newRandomID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate %s id: %w", strings.TrimSuffix(prefix, "_"), err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

// HashCanonicalArguments returns the SHA-256 of deterministic JSON without
// returning or retaining the argument body. encoding/json sorts map keys.
func HashCanonicalArguments(arguments map[string]any) (string, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	canonical, err := json.Marshal(arguments)
	if err != nil {
		return "", fmt.Errorf("canonicalize execution arguments: %w", err)
	}
	return HashBytes(canonical), nil
}

// HashText returns the lowercase hexadecimal SHA-256 of text.
func HashText(value string) string { return HashBytes([]byte(value)) }

// HashBytes returns the lowercase hexadecimal SHA-256 of bytes.
func HashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// BoundResultReceipt returns a valid UTF-8 receipt within the durable limit.
func BoundResultReceipt(value string) string {
	return boundUTF8(value, MaxResultReceiptBytes)
}

// BoundDetail returns valid UTF-8 detail within the durable limit.
func BoundDetail(value string) string { return boundUTF8(value, MaxDetailBytes) }

func boundUTF8(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	const marker = "\n...[truncated]"
	cut := limit - len(marker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + marker
}
