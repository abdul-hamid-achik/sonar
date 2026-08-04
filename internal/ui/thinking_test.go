package ui

import (
	"strings"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Assistant turns carry no role label. The user gutter already marks whose
// turn a block is, and the footer owns the only motion in the frame.
func TestAssistantTurnsCarryNoRoleLabelOrMotion(t *testing.T) {
	m := newTestModel(t)
	m.reducedMotion = false
	var rendered strings.Builder
	m.renderAssistantMsg(&rendered, ChatEntry{
		Kind:    "assistant",
		Content: "a settled answer",
	}, m.chatContentWidth())
	got := ansi.Strip(rendered.String())
	if strings.Contains(got, "assistant") {
		t.Fatalf("assistant turn reintroduced a role label: %q", got)
	}
	if strings.Contains(rendered.String(), m.spin.View()) {
		t.Fatalf("assistant turn duplicated the footer spinner: %q", got)
	}
}

func TestProcessStreamChunk_PlainText(t *testing.T) {
	main, think, inThinking, buf := processStreamChunk("hello world", false, "")
	if main != "hello world" {
		t.Errorf("main text = %q, want %q", main, "hello world")
	}
	if think != "" {
		t.Errorf("think text = %q, want empty", think)
	}
	if inThinking {
		t.Error("should not be in thinking mode")
	}
	if buf != "" {
		t.Errorf("search buf = %q, want empty", buf)
	}
}

func TestProcessStreamChunk_OpenTag(t *testing.T) {
	main, think, inThinking, _ := processStreamChunk("<think>reasoning here", false, "")
	if main != "" {
		t.Errorf("main text = %q, want empty", main)
	}
	if think != "reasoning here" {
		t.Errorf("think text = %q, want %q", think, "reasoning here")
	}
	if !inThinking {
		t.Error("should be in thinking mode")
	}
}

func TestProcessStreamChunk_CloseTag(t *testing.T) {
	main, think, inThinking, _ := processStreamChunk("end of thought</think>visible text", true, "")
	if think != "end of thought" {
		t.Errorf("think text = %q, want %q", think, "end of thought")
	}
	if main != "visible text" {
		t.Errorf("main text = %q, want %q", main, "visible text")
	}
	if inThinking {
		t.Error("should not be in thinking mode after close tag")
	}
}

func TestProcessStreamChunk_FullCycle(t *testing.T) {
	main, think, inThinking, _ := processStreamChunk("<think>thought</think>response", false, "")
	if think != "thought" {
		t.Errorf("think text = %q, want %q", think, "thought")
	}
	if main != "response" {
		t.Errorf("main text = %q, want %q", main, "response")
	}
	if inThinking {
		t.Error("should not be in thinking mode")
	}
}

func TestProcessStreamChunk_SplitAcrossChunks(t *testing.T) {
	// First chunk ends with partial tag "<thi"
	main1, think1, inThinking1, buf1 := processStreamChunk("text<thi", false, "")
	if main1 != "text" {
		t.Errorf("chunk1 main = %q, want %q", main1, "text")
	}
	if think1 != "" {
		t.Errorf("chunk1 think = %q, want empty", think1)
	}
	if inThinking1 {
		t.Error("chunk1 should not be in thinking")
	}
	if buf1 != "<thi" {
		t.Errorf("chunk1 buf = %q, want %q", buf1, "<thi")
	}

	// Second chunk completes the tag
	main2, think2, inThinking2, _ := processStreamChunk("nk>reasoning", false, buf1)
	if main2 != "" {
		t.Errorf("chunk2 main = %q, want empty", main2)
	}
	if think2 != "reasoning" {
		t.Errorf("chunk2 think = %q, want %q", think2, "reasoning")
	}
	if !inThinking2 {
		t.Error("chunk2 should be in thinking mode")
	}
}

func TestProcessStreamChunk_NestedTags(t *testing.T) {
	// Nested <think> should be treated as text inside thinking.
	main, think, _, _ := processStreamChunk("<think>outer<think>inner</think>after", false, "")
	// The inner <think> should be literal text inside thinking.
	// When we encounter the first </think>, thinking ends.
	if main != "after" {
		t.Errorf("main = %q, want %q", main, "after")
	}
	// The think content should include "outer<think>inner"
	if think != "outer<think>inner" {
		t.Errorf("think = %q, want %q", think, "outer<think>inner")
	}
}

func TestHasPartialTagSuffix(t *testing.T) {
	tests := []struct {
		s, tag string
		want   int
	}{
		{"hello<", "<think>", 1},
		{"hello<t", "<think>", 2},
		{"hello<th", "<think>", 3},
		{"hello<thi", "<think>", 4},
		{"hello<thin", "<think>", 5},
		{"hello<think", "<think>", 6},
		{"hello<think>", "<think>", 0}, // full match, not partial
		{"hello", "<think>", 0},
		{"</thi", "</think>", 5},
		{"<", "</think>", 1},
	}
	for _, tt := range tests {
		got := hasPartialTagSuffix(tt.s, tt.tag)
		if got != tt.want {
			t.Errorf("hasPartialTagSuffix(%q, %q) = %d, want %d", tt.s, tt.tag, got, tt.want)
		}
	}
}

func TestRenderThinkingBoxUsesCompactRail(t *testing.T) {
	m := newTestModel(t)
	m.width = 80

	collapsed := ansi.Strip(m.renderThinkingBox("inspect files\ncompare behavior\nreport result", true))
	if !strings.Contains(collapsed, "│ ▸ Thought") || !strings.Contains(collapsed, "3 lines") {
		t.Fatalf("collapsed reasoning receipt is unclear:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "inspect files") || strings.ContainsAny(collapsed, "╭╮╰╯") ||
		strings.Contains(collapsed, "ctrl+t") {
		t.Fatalf("collapsed reasoning retained heavy chrome or hidden content:\n%s", collapsed)
	}

	expanded := ansi.Strip(m.renderThinkingBox("inspect files\ncompare behavior\nreport result", false))
	for _, want := range []string{"│ ▾ Thought", "│ inspect files", "│ report result"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded reasoning missing %q:\n%s", want, expanded)
		}
	}
	if strings.Contains(expanded, "ctrl+t") {
		t.Fatalf("expanded reasoning repeated global controls:\n%s", expanded)
	}
}

func TestThinkingHeaderKeepsTranscriptQuiet(t *testing.T) {
	got := thinkingHeader("▸", 80)
	if got != "▸ Thought" {
		t.Fatalf("reasoning header = %q", got)
	}
}

func TestRenderThinkingBoxStaysInsideReadableTranscript(t *testing.T) {
	for _, width := range []int{30, 40, 80, 160} {
		m := newTestModel(t)
		m.width = width
		rendered := m.renderThinkingBox(strings.Repeat("long reasoning text ", 10), false)
		maximum := max(4, m.chatContentWidth())
		for lineNumber, line := range strings.Split(rendered, "\n") {
			if got := lipgloss.Width(line); got > maximum {
				t.Fatalf("width %d line %d = %d cells, want <= %d: %q", width, lineNumber+1, got, maximum, line)
			}
		}
	}
}

func TestLiveReasoningStaysInsideAssistantTurnUntilAnswerStarts(t *testing.T) {
	m := newTestModel(t)
	m.reducedMotion = true
	m.thinkBuf.WriteString("inspect files\ncompare behavior")

	plain := ansi.Strip(m.renderEntries())
	// Assistant turns carry no role label, so ownership is structural: the
	// reasoning block is the only thing in the turn until an answer starts.
	if strings.Contains(plain, "assistant") {
		t.Fatalf("assistant turn reintroduced a role label:\n%s", plain)
	}
	for _, want := range []string{"│ Thinking…", "compare behavior"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("live reasoning omitted %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "chars") || strings.Contains(plain, "ctrl+t") {
		t.Fatalf("live reasoning exposed unstable metrics or an unavailable action:\n%s", plain)
	}
}

func TestLiveReasoningTailSanitizesTerminalAndBidiControls(t *testing.T) {
	unsafe := "earlier\n\x1b]52;c;REASONING_SECRET\x07inspect\x1b[2J\tfiles\u202e\u2066"
	tail := liveThinkingTail(unsafe, 60)
	if len(tail) != 2 || tail[0] != "earlier" || !strings.Contains(tail[1], "inspect files") {
		t.Fatalf("sanitized live reasoning tail = %#v", tail)
	}
	for _, line := range tail {
		if strings.Contains(line, "REASONING_SECRET") {
			t.Fatalf("secret survived live reasoning tail: %q", line)
		}
		for _, character := range line {
			if unicode.IsControl(character) || isBidiControl(character) {
				t.Fatalf("unsafe rune %U survived live reasoning tail: %q", character, line)
			}
		}
	}

	m := newTestModel(t)
	rendered := ansi.Strip(m.renderLiveThinkingBox(unsafe))
	if strings.Contains(rendered, "REASONING_SECRET") || !strings.Contains(rendered, "inspect files") {
		t.Fatalf("unsafe live reasoning window = %q", rendered)
	}
}

func TestRenderLiveThinkingBoxSingleLineKeepsInlineSummary(t *testing.T) {
	m := newTestModel(t)
	rendered := ansi.Strip(m.renderLiveThinkingBox("inspect files"))
	if rendered != "│ Thinking… · inspect files" {
		t.Fatalf("single-line live reasoning = %q", rendered)
	}
}

func TestRenderLiveThinkingBoxShowsBoundedTailWindow(t *testing.T) {
	m := newTestModel(t)
	rendered := ansi.Strip(m.renderLiveThinkingBox("one\ntwo\nthree\nfour\nfive"))
	lines := strings.Split(rendered, "\n")
	if len(lines) != 1+liveThinkingTailRows {
		t.Fatalf("live tail window rows = %d, want %d:\n%s", len(lines), 1+liveThinkingTailRows, rendered)
	}
	if lines[0] != "│ Thinking…" {
		t.Fatalf("tail window header duplicated the summary: %q", lines[0])
	}
	for i, want := range []string{"│ three", "│ four", "│ five"} {
		if lines[i+1] != want {
			t.Fatalf("tail row %d = %q, want %q", i+1, lines[i+1], want)
		}
	}
}

func TestRenderLiveThinkingBoxSlicesAfterWrapping(t *testing.T) {
	m := newTestModel(t)
	inner := max(1, max(4, m.chatContentWidth())-2)
	long := strings.TrimSpace(strings.Repeat("steady reasoning stream ", 20))

	rendered := ansi.Strip(m.renderLiveThinkingBox(long))
	lines := strings.Split(rendered, "\n")
	if len(lines) != 1+liveThinkingTailRows {
		t.Fatalf("wrapped tail rows = %d, want %d:\n%s", len(lines), 1+liveThinkingTailRows, rendered)
	}
	wrapped := strings.Split(wrapText(long, inner), "\n")
	for i, want := range wrapped[len(wrapped)-liveThinkingTailRows:] {
		if lines[i+1] != "│ "+want {
			t.Fatalf("tail row %d = %q, want %q", i+1, lines[i+1], "│ "+want)
		}
	}
}

func TestLiveThinkingWindowHeightIsStableOnceGrown(t *testing.T) {
	m := newTestModel(t)
	buffer := "one\ntwo\nthree"
	grown := len(strings.Split(ansi.Strip(m.renderLiveThinkingBox(buffer)), "\n"))
	if grown != 1+liveThinkingTailRows {
		t.Fatalf("grown live window rows = %d, want %d", grown, 1+liveThinkingTailRows)
	}
	for _, chunk := range []string{" and", "\nfour", " tokens keep streaming", "\nfive\nsix\nseven\neight"} {
		buffer += chunk
		if rows := len(strings.Split(ansi.Strip(m.renderLiveThinkingBox(buffer)), "\n")); rows != grown {
			t.Fatalf("live window oscillated to %d rows after %q, want %d", rows, chunk, grown)
		}
	}
}

func TestExpandedReasoningStripsTerminalControlSequences(t *testing.T) {
	m := newTestModel(t)
	rendered := m.renderThinkingBox("inspect\x1b]0;owned\x07\nthen \u202espoof", false)
	for _, forbidden := range []string{"\x1b]", "\x07", "\u202e", "owned"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("expanded reasoning retained terminal control payload %q: %q", forbidden, rendered)
		}
	}
	for _, want := range []string{"inspect", "then", "spoof"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expanded reasoning dropped visible text %q: %q", want, rendered)
		}
	}
}

func TestReasoningOnlyCompletionBelongsToAssistantBlock(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{
		Kind: "assistant", ThinkingContent: "inspect files", ThinkingCollapsed: true,
	}}

	plain := ansi.Strip(m.renderEntries())
	if !strings.Contains(plain, "▸ Thought") {
		t.Fatalf("completed reasoning receipt missing:\n%s", plain)
	}
	if strings.Contains(plain, "assistant") {
		t.Fatalf("assistant turn reintroduced a role label:\n%s", plain)
	}
}

func TestWhitespaceOnlyThinkingDoesNotCreateLiveOrCompletedBlock(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	updated, _ := m.Update(StreamThinkingMsg{Text: " \n\t "})
	m = updated.(*Model)
	if m.hasLiveTurn() {
		t.Fatal("whitespace-only reasoning created a live turn")
	}
	m.flushStream()
	if len(m.entries) != 0 {
		t.Fatalf("whitespace-only reasoning created entries: %#v", m.entries)
	}
}

func TestToolCallSettlesReasoningBeforeReceipt(t *testing.T) {
	m := newTestModel(t)
	m.reducedMotion = true
	m.state = StateStreaming
	m.thinkBuf.WriteString("inspect the target")

	updated, _ := m.Update(ToolCallStartMsg{
		ID: "call-1", Name: "read", Args: map[string]any{"path": "README.md"}, StartTime: time.Now(),
	})
	m = updated.(*Model)
	if len(m.entries) != 2 || m.entries[0].Kind != "assistant" || m.entries[1].Kind != "tool_group" {
		t.Fatalf("reasoning/tool entry order = %#v", m.entries)
	}
	plain := ansi.Strip(m.renderEntries())
	reasoningAt := strings.Index(plain, "Thought")
	toolAt := strings.Index(strings.ToLower(plain), "read")
	if reasoningAt < 0 || toolAt < 0 || reasoningAt > toolAt {
		t.Fatalf("reasoning did not precede tool receipt:\n%s", plain)
	}
}

// Consecutive reasoning segments of one turn read as one block: they stack
// without a chapter break, and the answer follows them.
func TestReasoningSegmentsOfOneTurnStayTogether(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{
		{Kind: "user", Content: "inspect and explain"},
		{Kind: "assistant", ThinkingContent: "inspect files", ThinkingCollapsed: true},
		{Kind: "assistant", ThinkingContent: "compare behavior", ThinkingCollapsed: true},
		{Kind: "assistant", Content: "Here is the result."},
	}

	// Sticky owns the latest user prompt; body must still show the assistant turn.
	m.settleChromeSpringForTest()
	plain := ansi.Strip(m.renderEntries())
	if strings.Contains(plain, "assistant") {
		t.Fatalf("assistant turn reintroduced a role label:\n%s", plain)
	}
	if answerAt, reasoningAt := strings.Index(plain, "Here is the result."), strings.Index(plain, "▸ Thought"); answerAt < reasoningAt {
		t.Fatalf("answer preceded the reasoning that produced it:\n%s", plain)
	}
	if sticky := ansi.Strip(m.projectFrame().Header.Content); !strings.Contains(sticky, "inspect and explain") {
		t.Fatalf("sticky user omitted prompt:\n%s", sticky)
	}
	if strings.Contains(plain, "inspect and explain") {
		t.Fatalf("body re-printed sticky-owned user prompt:\n%s", plain)
	}
	for _, want := range []string{"▸ Thought", "Here is the result."} {
		if !strings.Contains(plain, want) {
			t.Fatalf("assistant turn omitted %q:\n%s", want, plain)
		}
	}
}

func TestToggleThinkingAppliesToEveryVisibleDisclosure(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{
		{Kind: "assistant", Content: "first", ThinkingContent: "first reasoning", ThinkingCollapsed: true},
		{Kind: "assistant", Content: "second", ThinkingContent: "second reasoning", ThinkingCollapsed: true},
	}

	updated, _ := m.Update(ctrlKey('t'))
	m = updated.(*Model)
	for i, entry := range m.entries {
		if entry.ThinkingCollapsed {
			t.Fatalf("entry %d remained collapsed after shared disclosure toggle", i)
		}
	}

	updated, _ = m.Update(ctrlKey('t'))
	m = updated.(*Model)
	for i, entry := range m.entries {
		if !entry.ThinkingCollapsed {
			t.Fatalf("entry %d remained expanded after shared disclosure toggle", i)
		}
	}
}

func TestCompletedThoughtHeadersToggleOnlyClickedEntry(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{
		{Kind: "assistant", Content: "first", ThinkingContent: "inspect files", ThinkingCollapsed: true},
		{Kind: "assistant", Content: "second", ThinkingContent: "compare behavior", ThinkingCollapsed: true},
	}
	m.invalidateEntryCache()
	m.refreshTranscript()

	if len(m.thinkingHitRegions) != 2 {
		t.Fatalf("thinking hit regions = %#v", m.thinkingHitRegions)
	}
	second := m.thinkingHitRegions[1]
	visibleRow := second.Row - m.transcriptYOffset()
	if second.StartCol <= 0 || second.StartCol >= second.EndCol {
		t.Fatalf("Thought header does not exclude its left margin: %#v", second)
	}
	m.handleMouseClick(second.EndCol, visibleRow)
	if !m.entries[0].ThinkingCollapsed || !m.entries[1].ThinkingCollapsed {
		t.Fatal("click immediately beyond Thought header toggled an entry")
	}
	m.handleMouseClick(second.StartCol-1, visibleRow)
	if !m.entries[0].ThinkingCollapsed || !m.entries[1].ThinkingCollapsed {
		t.Fatal("click immediately before Thought header toggled an entry")
	}
	m.handleMouseClick(second.StartCol, visibleRow)
	if !m.entries[0].ThinkingCollapsed || m.entries[1].ThinkingCollapsed {
		t.Fatalf("click on first Thought header cell toggled the wrong entry: %#v", m.entries)
	}
	m.entries[1].ThinkingCollapsed = true
	m.invalidateEntryCache()
	m.refreshTranscript()
	second = m.thinkingHitRegions[1]
	visibleRow = second.Row - m.transcriptYOffset()
	m.handleMouseClick(second.EndCol-1, visibleRow)
	if !m.entries[0].ThinkingCollapsed || m.entries[1].ThinkingCollapsed {
		t.Fatalf("click on last Thought header cell toggled the wrong entry: %#v", m.entries)
	}
	m.entries[1].ThinkingCollapsed = true
	m.invalidateEntryCache()
	m.refreshTranscript()
	second = m.thinkingHitRegions[1]
	visibleRow = second.Row - m.transcriptYOffset()
	m.handleMouseClick(second.StartCol, visibleRow-1)
	if !m.entries[0].ThinkingCollapsed || !m.entries[1].ThinkingCollapsed {
		t.Fatal("click above Thought header toggled an entry")
	}
	m.handleMouseClick(second.StartCol, visibleRow+1)
	if !m.entries[0].ThinkingCollapsed || !m.entries[1].ThinkingCollapsed {
		t.Fatal("click below Thought header toggled an entry")
	}
}

func TestThoughtHitRegionRejectsStaleEntryDigest(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{
		Kind: "assistant", Content: "answer", ThinkingContent: "original reasoning", ThinkingCollapsed: true,
	}}
	m.invalidateEntryCache()
	m.refreshTranscript()
	if len(m.thinkingHitRegions) != 1 {
		t.Fatalf("thinking hit regions = %#v", m.thinkingHitRegions)
	}
	region := m.thinkingHitRegions[0]
	m.entries[0].ThinkingContent = "replacement reasoning"
	m.handleMouseClick(region.StartCol, region.Row-m.transcriptYOffset())
	if !m.entries[0].ThinkingCollapsed {
		t.Fatal("stale Thought region toggled replacement content")
	}
}

func TestLiveReasoningHasNoClickableDisclosureRegion(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.thinkBuf.WriteString("live reasoning")
	m.invalidateEntryCache()
	m.refreshTranscript()
	if len(m.thinkingHitRegions) != 0 {
		t.Fatalf("live reasoning created click regions: %#v", m.thinkingHitRegions)
	}
}

func TestThoughtClickPreservesPausedScrollAnchorAtNarrowWidth(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = updated.(*Model)
	m.entries = []ChatEntry{
		{Kind: "assistant", Content: "answer", ThinkingContent: strings.Repeat("reasoning detail ", 20), ThinkingCollapsed: true},
		{Kind: "system", Content: strings.Repeat("later transcript rows ", 30)},
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	if len(m.thinkingHitRegions) != 1 {
		t.Fatalf("thinking hit regions = %#v", m.thinkingHitRegions)
	}
	region := m.thinkingHitRegions[0]
	if region.StartCol <= 0 || region.StartCol >= region.EndCol ||
		region.EndCol > m.viewport.Width() {
		t.Fatalf("narrow Thought region escaped viewport: %#v width=%d", region, m.viewport.Width())
	}
	m.setTranscriptYOffset(0)
	m.pauseFollow()
	before := m.transcriptYOffset()
	m.handleMouseClick(region.EndCol-1, region.Row-before)
	if m.entries[0].ThinkingCollapsed {
		t.Fatal("narrow Thought header did not expand")
	}
	if !m.followPaused() || m.transcriptYOffset() != before {
		t.Fatalf("Thought expansion moved paused anchor: paused=%v before=%d after=%d", m.followPaused(), before, m.transcriptYOffset())
	}
}

func TestCtrlTTogglesSettledThoughtsDuringActiveTurnButLeavesDraftOwned(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{
		Kind: "assistant", Content: "earlier answer", ThinkingContent: "settled reasoning", ThinkingCollapsed: true,
	}}
	m.state = StateStreaming
	m.thinkBuf.WriteString("current live reasoning")

	updated, _ := m.Update(ctrlKey('t'))
	m = updated.(*Model)
	if m.entries[0].ThinkingCollapsed {
		t.Fatal("Ctrl+T did not inspect settled reasoning during active turn")
	}
	if got := m.thinkBuf.String(); got != "current live reasoning" {
		t.Fatalf("Ctrl+T changed live reasoning: %q", got)
	}

	m.entries[0].ThinkingCollapsed = true
	m.input.SetValue("draft stays owned")
	updated, _ = m.Update(ctrlKey('t'))
	m = updated.(*Model)
	if !m.entries[0].ThinkingCollapsed {
		t.Fatal("Ctrl+T stole a non-empty composer draft")
	}
	if got := m.input.Value(); got != "draft stays owned" {
		t.Fatalf("Ctrl+T changed draft: %q", got)
	}
}
