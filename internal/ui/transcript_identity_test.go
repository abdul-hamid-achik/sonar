package ui

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestTranscriptIDsAreCanonicalAndDistinct(t *testing.T) {
	firstBlock, err := NewBlockID()
	if err != nil {
		t.Fatalf("NewBlockID: %v", err)
	}
	secondBlock, err := NewBlockID()
	if err != nil {
		t.Fatalf("NewBlockID second: %v", err)
	}
	turn, err := NewTurnID()
	if err != nil {
		t.Fatalf("NewTurnID: %v", err)
	}

	if !firstBlock.Valid() || !secondBlock.Valid() || !turn.Valid() {
		t.Fatalf("generated IDs are not valid: %q %q %q", firstBlock, secondBlock, turn)
	}
	if firstBlock == secondBlock {
		t.Fatalf("generated duplicate block ID %q", firstBlock)
	}
	if !strings.HasPrefix(string(firstBlock), "blk_") || !strings.HasPrefix(string(turn), "turn_") {
		t.Fatalf("generated IDs lack type prefixes: block=%q turn=%q", firstBlock, turn)
	}
}

func TestTranscriptIDValidationRejectsUnsafeValues(t *testing.T) {
	for _, id := range []BlockID{
		"",
		" leading",
		"trailing ",
		"has\nnewline",
		"unicode_é",
		"bidi_\u202e",
		BlockID(strings.Repeat("x", maxTranscriptIDBytes+1)),
	} {
		if id.Valid() {
			t.Errorf("BlockID(%q).Valid() = true, want false", id)
		}
	}
	if !BlockID("legacy:block-17").Valid() {
		t.Fatal("bounded opaque legacy identity should remain admissible")
	}
}

func TestBlockLifecycleTransitionsAreMonotonic(t *testing.T) {
	tests := []struct {
		from BlockLifecycle
		to   BlockLifecycle
		want bool
	}{
		{BlockPending, BlockLive, true},
		{BlockPending, BlockSettled, true},
		{BlockLive, BlockSettling, true},
		{BlockSettling, BlockFailed, true},
		{BlockSettled, BlockSettled, true},
		{BlockFailed, BlockLive, false},
		{BlockCancelled, BlockPending, false},
		{BlockSettling, BlockLive, false},
		{BlockLifecycle(255), BlockSettled, false},
	}
	for _, test := range tests {
		if got := test.from.CanTransitionTo(test.to); got != test.want {
			t.Errorf("%d.CanTransitionTo(%d) = %v, want %v", test.from, test.to, got, test.want)
		}
	}
}

func TestTranscriptBlockValidationAndCollectionIntegrity(t *testing.T) {
	turn := TurnID("turn_test")
	userPayload, err := NewVisibleTextBlockPayload("hello")
	if err != nil {
		t.Fatalf("NewVisibleTextBlockPayload: %v", err)
	}
	assistantPayload, err := NewVisibleTextBlockPayload("working")
	if err != nil {
		t.Fatalf("NewVisibleTextBlockPayload assistant: %v", err)
	}
	parent := TranscriptBlock{
		ID:        BlockID("block_parent"),
		TurnID:    turn,
		Kind:      BlockKindUserMessage,
		Revision:  1,
		Lifecycle: BlockSettled,
		Payload:   userPayload,
	}
	child := TranscriptBlock{
		ID:        BlockID("block_child"),
		TurnID:    turn,
		ParentID:  parent.ID,
		Kind:      BlockKindAssistantMessage,
		Revision:  3,
		Lifecycle: BlockLive,
		Payload:   assistantPayload,
	}
	if err := ValidateTranscriptBlocks([]TranscriptBlock{parent, child}); err != nil {
		t.Fatalf("valid blocks rejected: %v", err)
	}

	tests := []struct {
		name   string
		blocks []TranscriptBlock
	}{
		{"duplicate ID", []TranscriptBlock{parent, parent}},
		{"missing parent", []TranscriptBlock{child}},
		{"self parent", []TranscriptBlock{{
			ID: BlockID("same"), TurnID: turn, ParentID: BlockID("same"),
			Kind: BlockKindAssistantMessage, Revision: 1, Lifecycle: BlockLive,
		}}},
		{"zero revision", []TranscriptBlock{{
			ID: BlockID("block"), TurnID: turn,
			Kind: BlockKindAssistantMessage, Lifecycle: BlockLive,
		}}},
		{"missing turn", []TranscriptBlock{{
			ID: BlockID("block"), Kind: BlockKindAssistantMessage,
			Revision: 1, Lifecycle: BlockLive,
		}}},
		{"parent cycle", []TranscriptBlock{
			{
				ID: BlockID("cycle_a"), TurnID: turn, ParentID: BlockID("cycle_b"),
				Kind: BlockKindAgentEvent, Revision: 1, Lifecycle: BlockSettled,
			},
			{
				ID: BlockID("cycle_b"), TurnID: turn, ParentID: BlockID("cycle_a"),
				Kind: BlockKindAgentEvent, Revision: 1, Lifecycle: BlockSettled,
			},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateTranscriptBlocks(test.blocks); err == nil {
				t.Fatal("invalid transcript blocks were accepted")
			}
		})
	}

	system := TranscriptBlock{
		ID: BlockID("system"), Kind: BlockKindSystemNotice,
		Revision: 1, Lifecycle: BlockSettled,
	}
	if err := system.Validate(); err != nil {
		t.Fatalf("turn-independent system notice rejected: %v", err)
	}
	errorNotice := TranscriptBlock{
		ID: BlockID("startup_error"), Kind: BlockKindErrorNotice,
		Revision: 1, Lifecycle: BlockSettled,
	}
	if err := errorNotice.Validate(); err != nil {
		t.Fatalf("turn-independent startup error rejected: %v", err)
	}
}

func TestBlockPayloadHasNoOpaqueStructuredCarrier(t *testing.T) {
	payloadType := reflect.TypeFor[BlockPayload]()
	for index := range payloadType.NumField() {
		field := payloadType.Field(index)
		if field.IsExported() {
			t.Fatalf("BlockPayload field %s is exported and bypasses admission constructors", field.Name)
		}
		if field.Type.Kind() != reflect.String {
			t.Fatalf("BlockPayload field %s has raw-capable type %s", field.Name, field.Type)
		}
	}
}

func TestTranscriptBlockRejectsUnsafeOrUnboundedPayload(t *testing.T) {
	tests := []struct {
		name  string
		build func() error
	}{
		{"terminal escape", func() error {
			_, err := NewVisibleTextBlockPayload("\x1b[31mraw")
			return err
		}},
		{"multiline label", func() error {
			_, err := NewHostProjectedBlockPayload("line one\nline two", "")
			return err
		}},
		{"noncanonical summary", func() error {
			_, err := NewHostProjectedBlockPayload("", "  padded summary  ")
			return err
		}},
		{"oversized visible text", func() error {
			_, err := NewVisibleTextBlockPayload(strings.Repeat("x", maxTranscriptVisibleTextBytes+1))
			return err
		}},
		{"oversized label", func() error {
			_, err := NewHostProjectedBlockPayload(strings.Repeat("x", maxTranscriptLabelBytes+1), "")
			return err
		}},
		{"oversized summary", func() error {
			_, err := NewHostProjectedBlockPayload("", strings.Repeat("x", maxTranscriptSafeSummaryBytes+1))
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.build(); err == nil {
				t.Fatal("unsafe transcript payload was accepted")
			}
		})
	}
}

func TestBlockPayloadRequiresExplicitDurableProjection(t *testing.T) {
	payload, err := NewVisibleTextBlockPayload("credential=must-not-persist")
	if err != nil {
		t.Fatalf("NewVisibleTextBlockPayload: %v", err)
	}
	block := TranscriptBlock{
		ID: BlockID("block"), TurnID: TurnID("turn"),
		Kind: BlockKindAssistantMessage, Revision: 1, Lifecycle: BlockSettled,
		Payload: payload,
	}
	encoded, err := json.Marshal(block)
	if err == nil {
		t.Fatalf("direct transcript persistence unexpectedly succeeded: %s", encoded)
	}
	if strings.Contains(string(encoded), "must-not-persist") {
		t.Fatalf("failed direct persistence leaked payload: %s", encoded)
	}

	var restored TranscriptBlock
	if err := json.Unmarshal([]byte(`{
		"ID":"block",
		"TurnID":"turn",
		"Kind":2,
		"Revision":1,
		"Lifecycle":3,
		"Payload":{"visibleText":"forged"}
	}`), &restored); err == nil {
		t.Fatal("direct transcript restore unexpectedly succeeded")
	}
	if restored.ID != "" || restored.Payload.VisibleText() != "" {
		t.Fatalf("failed direct restore partially installed state: %+v", restored)
	}
}

func TestTranscriptRevisionExhaustionFailsWithoutWrapping(t *testing.T) {
	m := newTestModel(t)
	entry := ChatEntry{
		BlockID: "block-max", TurnID: "turn-max",
		Revision: math.MaxUint64, Lifecycle: BlockSettled,
		Kind: "assistant", Content: "new content",
	}
	old := entry
	old.Content = "old content"
	entry.semanticDigest = m.chatEntrySemanticDigest(old)
	m.entries = []ChatEntry{entry}

	err := m.reconcileTranscriptEntries()
	if err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("revision exhaustion error = %v", err)
	}
	if got := m.entries[0].Revision; got != math.MaxUint64 {
		t.Fatalf("revision wrapped or mutated to %d", got)
	}
}
