package ui

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	maxTranscriptIDBytes          = 128
	transcriptIDEntropy           = 16
	maxTranscriptVisibleTextBytes = 1 << 20
	maxTranscriptLabelBytes       = 512
	maxTranscriptSafeSummaryBytes = 4 << 10
)

// BlockID is the durable identity of one semantic transcript block. It must
// remain stable across streaming updates, reflow, persistence, and restore.
type BlockID string

// TurnID groups causally related blocks without making their slice position
// part of their identity.
type TurnID string

// NewBlockID returns a process-independent 128-bit block identity.
func NewBlockID() (BlockID, error) {
	id, err := newTranscriptID("blk_")
	return BlockID(id), err
}

// NewTurnID returns a process-independent 128-bit transcript turn identity.
func NewTurnID() (TurnID, error) {
	id, err := newTranscriptID("turn_")
	return TurnID(id), err
}

func newTranscriptID(prefix string) (string, error) {
	var entropy [transcriptIDEntropy]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate transcript identity: %w", err)
	}
	return prefix + hex.EncodeToString(entropy[:]), nil
}

// Valid reports whether id is a bounded, canonical opaque identity. IDs are
// intentionally not restricted to the generated prefix so existing durable
// execution identities can be projected without being rewritten.
func (id BlockID) Valid() bool {
	return validTranscriptID(string(id))
}

// Valid reports whether id is a bounded, canonical opaque identity.
func (id TurnID) Valid() bool {
	return validTranscriptID(string(id))
}

func validTranscriptID(id string) bool {
	if id == "" || len(id) > maxTranscriptIDBytes || strings.TrimSpace(id) != id {
		return false
	}
	for index := range len(id) {
		value := id[index]
		if (value < 'a' || value > 'z') &&
			(value < 'A' || value > 'Z') &&
			(value < '0' || value > '9') &&
			value != '_' && value != '-' && value != '.' && value != ':' {
			return false
		}
	}
	return true
}

// BlockKind describes a block's semantic role. Rendering, width, theme, focus,
// and expansion state are deliberately absent from this enum.
type BlockKind uint8

const (
	BlockKindUnknown BlockKind = iota
	BlockKindUserMessage
	BlockKindAssistantMessage
	BlockKindReasoningSummary
	BlockKindToolGroup
	BlockKindToolCall
	BlockKindAgentGroup
	BlockKindAgentEvent
	BlockKindPermissionReceipt
	BlockKindQuestionReceipt
	BlockKindPlanEvent
	BlockKindSystemNotice
	BlockKindErrorNotice
	BlockKindCompactionEvent
	BlockKindSessionBoundary
)

// Valid reports whether kind names a supported semantic block.
func (kind BlockKind) Valid() bool {
	return kind >= BlockKindUserMessage && kind <= BlockKindSessionBoundary
}

// BlockLifecycle is the monotonic semantic lifecycle of a transcript block.
type BlockLifecycle uint8

const (
	BlockPending BlockLifecycle = iota
	BlockLive
	BlockSettling
	BlockSettled
	BlockFailed
	BlockCancelled
)

// Valid reports whether lifecycle is a known lifecycle value.
func (lifecycle BlockLifecycle) Valid() bool {
	return lifecycle <= BlockCancelled
}

// Terminal reports whether the block can no longer make a semantic
// transition. Re-applying the same terminal state remains idempotent.
func (lifecycle BlockLifecycle) Terminal() bool {
	return lifecycle == BlockSettled || lifecycle == BlockFailed || lifecycle == BlockCancelled
}

// CanTransitionTo enforces the lifecycle partial order. It accepts an
// idempotent transition and rejects every transition out of a terminal state.
func (lifecycle BlockLifecycle) CanTransitionTo(next BlockLifecycle) bool {
	if !lifecycle.Valid() || !next.Valid() {
		return false
	}
	if lifecycle == next {
		return true
	}
	switch lifecycle {
	case BlockPending:
		return next == BlockLive || next == BlockSettling || next.Terminal()
	case BlockLive:
		return next == BlockSettling || next.Terminal()
	case BlockSettling:
		return next.Terminal()
	default:
		return false
	}
}

// BlockPayload is the deliberately narrow, provider-neutral content admitted
// to the transcript model. Every field must already be terminal-safe and
// host-approved before construction. In particular, callers must not place raw
// MCP StructuredContent, provider reasoning, internal prompts, credentials, or
// unredacted private paths in these strings.
//
// Tool transport/domain/evidence projections remain separate typed models;
// they must not be smuggled into this payload as arbitrary maps.
type BlockPayload struct {
	visibleText string
	label       string
	safeSummary string
}

// NewVisibleTextBlockPayload admits already projected user/assistant text. It
// does not accept tool envelopes; MCP results must first cross the bounded
// parser/ecosystem projection boundary.
func NewVisibleTextBlockPayload(visibleText string) (BlockPayload, error) {
	payload := BlockPayload{visibleText: visibleText}
	if err := payload.validate(); err != nil {
		return BlockPayload{}, err
	}
	return payload, nil
}

// NewHostProjectedBlockPayload admits bounded host-authored chrome and a
// summary explicitly approved for display. It is the only constructor for
// labels/summaries and must never receive private provider reasoning.
func NewHostProjectedBlockPayload(label, safeSummary string) (BlockPayload, error) {
	payload := BlockPayload{label: label, safeSummary: safeSummary}
	if err := payload.validate(); err != nil {
		return BlockPayload{}, err
	}
	return payload, nil
}

// VisibleText returns terminal-safe projected message text.
func (payload BlockPayload) VisibleText() string {
	return payload.visibleText
}

// Label returns canonical single-line host chrome.
func (payload BlockPayload) Label() string {
	return payload.label
}

// SafeSummary returns a canonical single-line, host-approved summary.
func (payload BlockPayload) SafeSummary() string {
	return payload.safeSummary
}

// MarshalJSON fails closed. Durable transcript envelopes need an explicit
// projection so adding a field here cannot silently persist private content.
func (BlockPayload) MarshalJSON() ([]byte, error) {
	return nil, errors.New("BlockPayload requires an explicit durable projection")
}

// UnmarshalJSON fails closed for the same reason as MarshalJSON. Restore must
// validate an explicit, versioned durable DTO before constructing this type.
func (*BlockPayload) UnmarshalJSON([]byte) error {
	return errors.New("BlockPayload requires an explicit durable projection")
}

func (payload BlockPayload) validate() error {
	if len(payload.visibleText) > maxTranscriptVisibleTextBytes {
		return fmt.Errorf("visible text exceeds %d bytes", maxTranscriptVisibleTextBytes)
	}
	if payload.visibleText != sanitizeTerminalMultiline(payload.visibleText) {
		return errors.New("visible text is not terminal-safe")
	}
	if len(payload.label) > maxTranscriptLabelBytes {
		return fmt.Errorf("label exceeds %d bytes", maxTranscriptLabelBytes)
	}
	if payload.label != sanitizeTerminalSingleLine(payload.label) {
		return errors.New("label is not a canonical terminal-safe line")
	}
	if len(payload.safeSummary) > maxTranscriptSafeSummaryBytes {
		return fmt.Errorf("safe summary exceeds %d bytes", maxTranscriptSafeSummaryBytes)
	}
	if payload.safeSummary != sanitizeTerminalSingleLine(payload.safeSummary) {
		return errors.New("safe summary is not a canonical terminal-safe line")
	}
	return nil
}

// TranscriptBlock is one semantic item in causal transcript order. Revision
// changes only when Payload or Lifecycle changes; layout and presentation
// changes must not increment it.
type TranscriptBlock struct {
	ID        BlockID
	TurnID    TurnID
	ParentID  BlockID
	Kind      BlockKind
	Revision  uint64
	Lifecycle BlockLifecycle
	Payload   BlockPayload
}

// MarshalJSON prevents the semantic runtime model from becoming an accidental
// persistence schema. A versioned durable DTO must select safe fields.
func (TranscriptBlock) MarshalJSON() ([]byte, error) {
	return nil, errors.New("TranscriptBlock requires an explicit durable projection")
}

// UnmarshalJSON prevents a manipulated or incomplete envelope from silently
// replacing private payload fields with a valid-looking zero value.
func (*TranscriptBlock) UnmarshalJSON([]byte) error {
	return errors.New("TranscriptBlock requires an explicit durable projection")
}

// Validate checks identity and state invariants that must hold before a block
// enters a transcript store or durable envelope.
func (block TranscriptBlock) Validate() error {
	if !block.ID.Valid() {
		return errors.New("transcript block ID is required and must be canonical")
	}
	if block.ParentID != "" && !block.ParentID.Valid() {
		return errors.New("transcript parent block ID must be canonical")
	}
	if block.ParentID == block.ID {
		return errors.New("transcript block cannot parent itself")
	}
	if !block.Kind.Valid() {
		return errors.New("transcript block kind is invalid")
	}
	if block.TurnID == "" {
		if !block.Kind.turnOptional() {
			return errors.New("transcript turn ID is required for this block kind")
		}
	} else if !block.TurnID.Valid() {
		return errors.New("transcript turn ID must be canonical")
	}
	if block.Revision == 0 {
		return errors.New("transcript block revision must start at one")
	}
	if !block.Lifecycle.Valid() {
		return errors.New("transcript block lifecycle is invalid")
	}
	if err := block.Payload.validate(); err != nil {
		return fmt.Errorf("transcript block payload: %w", err)
	}
	return nil
}

func (kind BlockKind) turnOptional() bool {
	return kind == BlockKindSystemNotice || kind == BlockKindErrorNotice || kind == BlockKindSessionBoundary
}

// ValidateTranscriptBlocks checks block-local invariants plus collection
// identity and parent-reference integrity. It expects a complete block set;
// parent order is intentionally not constrained, so forward references work.
func ValidateTranscriptBlocks(blocks []TranscriptBlock) error {
	ids := make(map[BlockID]struct{}, len(blocks))
	parents := make(map[BlockID]BlockID, len(blocks))
	for index, block := range blocks {
		if err := block.Validate(); err != nil {
			return fmt.Errorf("transcript block %d: %w", index, err)
		}
		if _, exists := ids[block.ID]; exists {
			return fmt.Errorf("transcript block %d: duplicate ID %q", index, block.ID)
		}
		ids[block.ID] = struct{}{}
		parents[block.ID] = block.ParentID
	}
	for index, block := range blocks {
		if block.ParentID == "" {
			continue
		}
		if _, exists := ids[block.ParentID]; !exists {
			return fmt.Errorf("transcript block %d: parent ID %q is outside the block set", index, block.ParentID)
		}
	}
	// Iterative coloring avoids recursion depth becoming input-controlled.
	const (
		parentUnseen uint8 = iota
		parentVisiting
		parentDone
	)
	state := make(map[BlockID]uint8, len(blocks))
	for _, block := range blocks {
		if state[block.ID] == parentDone {
			continue
		}
		path := make([]BlockID, 0, 4)
		current := block.ID
		for current != "" && state[current] == parentUnseen {
			state[current] = parentVisiting
			path = append(path, current)
			current = parents[current]
		}
		if current != "" && state[current] == parentVisiting {
			return fmt.Errorf("transcript parent cycle reaches block ID %q", current)
		}
		for _, id := range path {
			state[id] = parentDone
		}
	}
	return nil
}
