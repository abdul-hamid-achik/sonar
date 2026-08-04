package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/charmbracelet/x/ansi"
)

func TestAdapterAdmitsOrdinaryOutputButNotExpertReports(t *testing.T) {
	store := NewOutputDetailStore()
	adapter := NewAdapterWithOutputDetails(nil, store)

	msg := adapter.toolCallResultMsg(
		"call-1",
		"read_file",
		"first\x1b[31m\nsecond",
		false,
		time.Second,
		ecosystemProjectionZero(),
		"first\x1b[31m\nsecond",
		true,
	)
	if !msg.OutputDetail.Ref.Valid() || !msg.OutputDetail.Digest.Valid() {
		t.Fatalf("ordinary output did not receive a valid detail receipt: %#v", msg.OutputDetail)
	}
	page, err := store.Page(context.Background(), OutputDetailPageRequest{Ref: msg.OutputDetail.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if got := flattenedOutputDetailPageText(page); got != "first\nsecond" {
		t.Fatalf("retained output = %q", got)
	}

	expert := adapter.toolCallResultMsg(
		"call-2",
		"consult_experts",
		"private aggregate report",
		false,
		time.Second,
		ecosystemProjectionZero(),
		"private aggregate report",
		true,
	)
	if expert.OutputDetail.Ref.Valid() || expert.OutputDetail.Digest != (OutputDetailDigest{}) {
		t.Fatalf("expert output crossed the detail boundary: %#v", expert.OutputDetail)
	}
	if got, want := store.Len(), 1; got != want {
		t.Fatalf("store entries = %d, want %d", got, want)
	}
}

func TestAdapterSemanticDetailKeepsPreviewSeparateFromCompleteEphemeralOutput(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	adapter := NewAdapterWithOutputDetails(nil, store)
	preview := strings.Repeat("x", 64) + "\n... [output truncated]"
	complete := strings.Repeat("x", 2*maxOutputDetailSourceBytes)

	msg := adapter.toolCallResultMsg(
		"call-complete",
		"bash",
		preview,
		false,
		time.Second,
		ecosystemProjectionZero(),
		complete,
		true,
	)
	if msg.Result != preview {
		t.Fatalf("transcript preview changed: %q", msg.Result)
	}
	if !msg.OutputDetail.Ref.Valid() ||
		msg.OutputDetail.Digest.TotalBytes != uint64(len(complete)) ||
		!msg.OutputDetail.Digest.Truncated {
		t.Fatalf("complete detail digest = %#v", msg.OutputDetail)
	}

	semanticReceipt := adapter.toolCallResultMsg(
		"call-semantic",
		"mcp__typed",
		"domain=succeeded evidence=verified",
		false,
		time.Second,
		ecosystemProjectionZero(),
		"",
		false,
	)
	if semanticReceipt.OutputDetail.Ref.Valid() ||
		semanticReceipt.OutputDetail.Digest != (OutputDetailDigest{}) {
		t.Fatalf("semantic receipt gained false original-output capability: %#v", semanticReceipt.OutputDetail)
	}
}

func TestAdapterDirectSyntheticResultCannotGainOutputCapability(t *testing.T) {
	t.Parallel()

	store := NewOutputDetailStore()
	adapter := NewAdapterWithOutputDetails(nil, store)
	adapter.ToolCallResult(
		"blocked-call",
		"write_file",
		"synthetic permission denial",
		true,
		time.Second,
	)
	if store.Len() != 0 || store.RetainedBytes() != 0 {
		t.Fatalf("direct synthetic result entered output store: refs=%d bytes=%d",
			store.Len(), store.RetainedBytes())
	}
}

func TestToolResultOwnsDetailReceiptAndDropsLateReceipt(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(ToolCallStartMsg{
		ID: "call-live", Name: "read_file", StartTime: time.Now(),
	})
	m = updated.(*Model)

	receipt, err := m.outputDetails.Admit("one\ntwo\nthree")
	if err != nil {
		t.Fatal(err)
	}
	updated, _ = m.Update(ToolCallResultMsg{
		ID: "call-live", Name: "read_file", Result: "one\ntwo\nthree",
		OutputDetail: receipt,
	})
	m = updated.(*Model)
	if got := m.toolEntries[0].OutputDetail; got.Ref != receipt.Ref || got.Digest != receipt.Digest {
		t.Fatalf("settled tool detail = %#v, want %#v", got, receipt)
	}

	late, err := m.outputDetails.Admit("late result")
	if err != nil {
		t.Fatal(err)
	}
	before := m.outputDetails.Len()
	updated, _ = m.Update(ToolCallResultMsg{
		ID: "call-missing", Name: "read_file", Result: "late result",
		OutputDetail: late,
	})
	m = updated.(*Model)
	if got, want := m.outputDetails.Len(), before-1; got != want {
		t.Fatalf("late output was not revoked: entries = %d, want %d", got, want)
	}
}

func TestToolProjectionDoesNotPromiseEvictedOutput(t *testing.T) {
	m := newTestModel(t)
	receipt, err := m.outputDetails.Admit("retained")
	if err != nil {
		t.Fatal(err)
	}
	m.toolEntries = []ToolEntry{{
		ID: "read-1", Name: "read_file", Status: ToolStatusDone,
		OutputDetail: receipt,
	}}
	chat := ChatEntry{
		BlockID: "block-read-1", Revision: 1, Lifecycle: BlockSettled,
		Kind: "tool_group", ToolIndex: 0,
	}
	before, err := m.projectToolRenderModel(chat)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Preview.OutputAvailable {
		t.Fatal("live output ref was projected as unavailable")
	}

	m.outputDetails.Drop(receipt.Ref)
	after, err := m.projectToolRenderModel(chat)
	if err != nil {
		t.Fatal(err)
	}
	if after.Preview.OutputAvailable || after.Preview.OutputDigest != receipt.Digest {
		t.Fatalf("evicted projection = %#v", after.Preview)
	}
}

func TestOutputDetailSessionRoundTripPersistsDigestWithoutCapability(t *testing.T) {
	store := NewOutputDetailStore()
	receipt, err := store.Admit("alpha\nbeta")
	if err != nil {
		t.Fatal(err)
	}
	persisted := persistToolEntries([]ToolEntry{{
		ID: "read-1", Name: "read_file", Status: ToolStatusDone, OutputDetail: receipt,
	}})
	if len(persisted) != 1 || persisted[0].OutputDetail == nil {
		t.Fatalf("output digest was not persisted: %#v", persisted)
	}
	raw, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), receipt.Ref.String()) || strings.Contains(string(raw), `"id":"output_`) {
		t.Fatalf("ephemeral output capability leaked into session JSON: %s", raw)
	}

	restored := restoreToolEntries(persisted)
	if len(restored) != 1 || restored[0].OutputDetail.Digest != receipt.Digest {
		t.Fatalf("restored digest = %#v, want %#v", restored, receipt.Digest)
	}
	if restored[0].OutputDetail.Ref.Valid() {
		t.Fatalf("restore revived an ephemeral output capability: %#v", restored[0].OutputDetail.Ref)
	}
}

func TestPersistedOutputDetailValidationFailsClosed(t *testing.T) {
	invalid := OutputDetailDigest{
		TotalRows: 1, RetainedRows: 2, TotalBytes: 1, RetainedBytes: 1,
	}
	state := persistedSessionState{
		ToolEntries: []persistedToolEntry{{
			ID: "read-1", Name: "read_file", Status: ToolStatusDone, OutputDetail: &invalid,
		}},
	}
	if err := validatePersistedToolTranscriptState(state); err == nil {
		t.Fatal("invalid output digest was accepted")
	}

	valid := OutputDetailDigest{TotalRows: 1, RetainedRows: 1, TotalBytes: 1, RetainedBytes: 1}
	state.ToolEntries[0].Name = "consult_experts"
	state.ToolEntries[0].OutputDetail = &valid
	if err := validatePersistedToolTranscriptState(state); err == nil {
		t.Fatal("expert output digest was accepted")
	}
}

func TestToolCardUsesExactOutputDigestForOmissionReceipt(t *testing.T) {
	store := NewOutputDetailStore()
	source := strings.Repeat("x\n", 237) + "x"
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatal(err)
	}
	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Lifecycle = ToolLifecycleSucceeded
	card.Expanded = true
	card.Result = boundedToolCardResult(source)
	card.OutputDigest = receipt.Digest
	card.OutputAvailable = true

	view := ansi.Strip(card.View(96))
	if !strings.Contains(view, "230 lines hidden · open output") {
		t.Fatalf("card omitted exact source-row receipt:\n%s", view)
	}

	card.OutputAvailable = false
	view = ansi.Strip(card.View(96))
	if !strings.Contains(view, "230 lines hidden · full output unavailable") {
		t.Fatalf("evicted/restored card implied loadability:\n%s", view)
	}
}

func TestToolCardReportsBytesHiddenInsideOneLongRow(t *testing.T) {
	store := NewOutputDetailStore()
	source := strings.Repeat("z", 3_000)
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatal(err)
	}
	card := NewToolCard("bash", ToolCardBash, true, defaultThemeID, GlyphASCII)
	card.State = ToolCardSuccess
	card.Lifecycle = ToolLifecycleSucceeded
	card.Expanded = true
	card.Result = boundedToolCardResult(source)
	card.OutputDigest = receipt.Digest
	card.OutputAvailable = true

	view := ansi.Strip(card.View(96))
	if !strings.Contains(view, "... 1003 bytes hidden - open output") {
		t.Fatalf("long-row omission receipt was not exact or ASCII-safe:\n%s", view)
	}
	if strings.ContainsAny(view, "…·") {
		t.Fatalf("ASCII omission receipt leaked Unicode punctuation:\n%s", view)
	}
}

func flattenedOutputDetailPageText(page OutputDetailPage) string {
	var builder strings.Builder
	for index, row := range page.Rows {
		if index > 0 && !row.StartsMidRow {
			builder.WriteByte('\n')
		}
		builder.WriteString(row.Text)
	}
	return builder.String()
}

// ecosystemProjectionZero keeps the adapter test focused on the output
// boundary while preserving a named return type at the call site.
func ecosystemProjectionZero() ecosystem.ToolProjection {
	return ecosystem.ToolProjection{}
}
