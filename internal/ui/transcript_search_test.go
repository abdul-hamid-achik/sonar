package ui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func settledSearchEntry(
	id BlockID,
	turn TurnID,
	kind string,
	content string,
) ChatEntry {
	return ChatEntry{
		BlockID:   id,
		TurnID:    turn,
		Revision:  1,
		Lifecycle: BlockSettled,
		Kind:      kind,
		Content:   content,
	}
}

func installSearchTranscript(
	t *testing.T,
	m *Model,
	entries []ChatEntry,
	tools []ToolEntry,
) {
	t.Helper()
	m.entries = entries
	m.toolEntries = tools
	m.resetEntryMemo()
	m.refreshTranscript()
	if len(m.transcriptLayout.Records) == 0 {
		t.Fatal("search fixture did not publish transcript layout")
	}
}

func setTranscriptSearchQuery(
	t *testing.T,
	m *Model,
	query string,
	jump bool,
) {
	t.Helper()
	if m.transcriptSearch == nil {
		t.Fatal("transcript search is not open")
	}
	m.transcriptSearch.input.SetValue(query)
	m.normalizeTranscriptSearchQuery()
	m.recomputeTranscriptSearchMatches(jump)
}

func TestTranscriptSearchIndexesOnlySafeProjectedTranscript(t *testing.T) {
	const (
		answerMarker       = "PUBLIC_ANSWER_MARKER"
		reasoningSecret    = "PRIVATE_REASONING_MUST_NOT_BE_SEARCHABLE"
		privateArgSecret   = "PRIVATE_MCP_ARGUMENT_MUST_NOT_BE_SEARCHABLE"
		rawArgumentSecret  = "RAW_ARGUMENT_MUST_NOT_BE_SEARCHABLE"
		beforeStateSecret  = "BEFORE_STATE_MUST_NOT_BE_SEARCHABLE"
		safeToolResult     = "SAFE_TOOL_RESULT_MARKER"
		repeatedInThinking = "SHARED_VISIBLE_PHRASE"
	)
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	projection := ecosystem.ToolProjection{
		Specialist: "cortex",
		Operation:  "cortex_status",
		Role:       ecosystem.RoleCoordination,
		Transport:  ecosystem.TransportSucceeded,
		Domain:     ecosystem.DomainSucceeded,
		Evidence:   ecosystem.EvidenceNone,
		Route: ecosystem.ToolRoute{
			Gateway: "mcphub",
			Server:  "cortex",
			Tool:    "cortex_status",
			CallID:  "safe-search-call",
			Lazy:    true,
		},
	}.Normalize()
	tools := []ToolEntry{{
		ID:      "safe-search-call",
		Name:    "mcphub__mcphub_call_tool",
		Summary: "safe projected tool summary",
		Args:    `arguments={"token":"` + privateArgSecret + `"}`,
		RawArgs: map[string]any{
			"token": rawArgumentSecret,
		},
		Result:        safeToolResult,
		BeforeContent: beforeStateSecret,
		Status:        ToolStatusDone,
		Collapsed:     false,
		Projection:    projection,
	}}
	assistant := settledSearchEntry(
		"search-assistant-safe",
		"search-turn-safe",
		"assistant",
		answerMarker+" "+repeatedInThinking,
	)
	assistant.ThinkingContent = reasoningSecret + "\n" + repeatedInThinking
	assistant.ThinkingCollapsed = false
	tool := settledSearchEntry(
		"search-tool-safe",
		"search-turn-safe",
		"tool_group",
		"",
	)
	tool.ToolIndex = 0
	installSearchTranscript(t, m, []ChatEntry{
		settledSearchEntry(
			"search-user-safe",
			"search-turn-safe",
			"user",
			"USER_VISIBLE_MARKER",
		),
		assistant,
		tool,
	}, tools)

	_ = m.openTranscriptSearch()
	for _, test := range []struct {
		query       string
		wantMatches int
	}{
		{query: "USER_VISIBLE_MARKER", wantMatches: 1},
		{query: answerMarker, wantMatches: 1},
		{query: safeToolResult, wantMatches: 1},
		{query: reasoningSecret, wantMatches: 0},
		{query: privateArgSecret, wantMatches: 0},
		{query: rawArgumentSecret, wantMatches: 0},
		{query: beforeStateSecret, wantMatches: 0},
		// The same phrase is present in both answer and expanded reasoning.
		// Provenance, not substring coincidence, admits exactly the answer row.
		{query: repeatedInThinking, wantMatches: 1},
	} {
		t.Run(test.query, func(t *testing.T) {
			setTranscriptSearchQuery(t, m, test.query, false)
			if got := len(m.transcriptSearch.matches); got != test.wantMatches {
				t.Fatalf(
					"matches for %q = %d, want %d\nindexed rows: %#v",
					test.query,
					got,
					test.wantMatches,
					m.transcriptSearch.rows,
				)
			}
		})
	}
}

func TestTranscriptSearchSourceProvenanceExcludesChrome(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	installSearchTranscript(t, m, []ChatEntry{
		settledSearchEntry(
			"search-user-chrome",
			"search-chrome-turn",
			"user",
			"you",
		),
		settledSearchEntry(
			"search-system-prefix",
			"search-chrome-turn",
			"system",
			"deployment ready",
		),
		settledSearchEntry(
			"search-error-chrome",
			"search-chrome-turn",
			"error",
			"error",
		),
	}, nil)
	_ = m.openTranscriptSearch()

	for _, test := range []struct {
		name        string
		query       string
		wantMatches int
	}{
		{
			// User chrome no longer paints a "you" role label; content "you"
			// is the only match and must remain searchable.
			name:        "user content you is searchable without role chrome",
			query:       "you",
			wantMatches: 1,
		},
		{
			name:        "system first row validates without host prefix",
			query:       "deployment ready",
			wantMatches: 1,
		},
		{
			name:        "validated system row retains visible host prefix",
			query:       "notice · deployment ready",
			wantMatches: 1,
		},
		{
			name:        "error chip cannot duplicate matching content",
			query:       "error",
			wantMatches: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			setTranscriptSearchQuery(t, m, test.query, false)
			if got := len(m.transcriptSearch.matches); got != test.wantMatches {
				t.Fatalf(
					"matches for %q = %d, want %d\nrows=%#v",
					test.query,
					got,
					test.wantMatches,
					m.transcriptSearch.rows,
				)
			}
		})
	}
}

func TestTranscriptSearchSystemNarrowChromeDoesNotExhaustSource(t *testing.T) {
	const content = "DEPLOYMENT_READY_WITH_A_VERY_LONG_UNBROKEN_TOKEN"
	entry := settledSearchEntry(
		"search-system-narrow",
		"search-system-narrow-turn",
		"system",
		content,
	)
	rendered := transcriptSearchSystemLabel + "\n  " + content

	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.transcriptSearch = newTranscriptSearchState(
		30,
		m.isDark,
		m.themeID,
		m.reducedMotion,
	)
	m.entries = []ChatEntry{entry}
	m.transcriptLayout = TranscriptLayoutSnapshot{
		Records: []TranscriptLayoutRecord{{
			BlockID:  entry.BlockID,
			TurnID:   entry.TurnID,
			Revision: entry.Revision,
			Height:   2,
			StartRow: 0,
			Exact:    true,
			LineMap: LineMap{
				{LogicalOffset: 0, Row: 0},
				{LogicalOffset: 1, Row: 1},
			},
		}},
	}
	m.transcriptPaint.document = transcriptPaintDocument{
		base: []transcriptPaintBlock{
			newTranscriptPaintBlock(0, rendered),
		},
		totalRows: 2,
	}
	m.rebuildTranscriptSearchIndexBounded(2, len(content)+32)

	state := m.transcriptSearch
	if len(state.rows) != 1 ||
		state.rows[0].RenderedText != content {
		t.Fatalf("narrow system rows = %#v, want only source content", state.rows)
	}
	if state.indexSourceScanBytes == 0 {
		t.Fatal("source content was not provenance-checked after chrome-only row")
	}
	state.input.SetValue(content)
	m.recomputeTranscriptSearchMatches(false)
	if got := len(state.matches); got != 1 {
		t.Fatalf("long-token matches = %d, want 1", got)
	}
}

func TestTranscriptSearchSourceIndexWorkIsLinear(t *testing.T) {
	const rowCount = 4096
	lines := make([]string, rowCount)
	lineMap := make(LineMap, rowCount)
	for index := range lines {
		lines[index] = fmt.Sprintf("source row %05d payload", index)
		lineMap[index] = TranscriptLinePoint{
			LogicalOffset: index + 1,
			Row:           index,
		}
	}
	content := strings.Join(lines, "\n")
	entry := settledSearchEntry(
		"search-linear-source",
		"search-linear-turn",
		"user",
		content,
	)

	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.transcriptSearch = newTranscriptSearchState(
		80,
		m.isDark,
		m.themeID,
		m.reducedMotion,
	)
	m.entries = []ChatEntry{entry}
	m.transcriptLayout = TranscriptLayoutSnapshot{
		Records: []TranscriptLayoutRecord{{
			BlockID:  entry.BlockID,
			TurnID:   entry.TurnID,
			Revision: entry.Revision,
			Height:   rowCount,
			StartRow: 0,
			Exact:    true,
			LineMap:  lineMap,
		}},
	}
	m.transcriptPaint.document = transcriptPaintDocument{
		base: []transcriptPaintBlock{
			newTranscriptPaintBlock(0, content),
		},
		totalRows: rowCount,
	}
	m.rebuildTranscriptSearchIndexBounded(rowCount, len(content)+1)

	state := m.transcriptSearch
	if got := len(state.rows); got != rowCount {
		t.Fatalf("indexed rows = %d, want %d", got, rowCount)
	}
	if state.indexSourceSanitizations != 1 {
		t.Fatalf(
			"source sanitizations = %d, want exactly one",
			state.indexSourceSanitizations,
		)
	}
	if state.indexSourceBytes > len(content)+1 {
		t.Fatalf(
			"prepared source bytes = %d, budget = %d",
			state.indexSourceBytes,
			len(content)+1,
		)
	}
	if state.indexSourceScanBytes > state.indexSourceBytes {
		t.Fatalf(
			"source scan bytes = %d, prepared = %d; work is not monotonic",
			state.indexSourceScanBytes,
			state.indexSourceBytes,
		)
	}
}

func TestTranscriptSearchLiveTailExcludesReasoningAndChrome(t *testing.T) {
	const (
		answerMarker    = "LIVE_PUBLIC_ANSWER"
		reasoningSecret = "LIVE_PRIVATE_REASONING"
		sharedPhrase    = "LIVE_SHARED_PHRASE"
	)
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.entries = []ChatEntry{settledSearchEntry(
		"search-live-user",
		"search-live-turn",
		"user",
		"start live search",
	)}
	m.state = StateStreaming
	m.thinkBuf.WriteString(reasoningSecret + "\n" + sharedPhrase)
	m.streamBuf.WriteString(answerMarker + " assistant " + sharedPhrase)
	m.resetEntryMemo()
	m.refreshTranscript()

	_ = m.openTranscriptSearch()
	for _, test := range []struct {
		query       string
		wantMatches int
	}{
		{query: answerMarker, wantMatches: 1},
		{query: reasoningSecret, wantMatches: 0},
		{query: sharedPhrase, wantMatches: 1},
		// "assistant" occurs in the answer, but the role header must not become
		// a second semantic hit.
		{query: "assistant", wantMatches: 1},
	} {
		setTranscriptSearchQuery(t, m, test.query, false)
		if got := len(m.transcriptSearch.matches); got != test.wantMatches {
			t.Fatalf(
				"live matches for %q = %d, want %d\nmatches=%#v\nrows=%#v\nlayout=%#v",
				test.query,
				got,
				test.wantMatches,
				m.transcriptSearch.matches,
				m.transcriptSearch.rows,
				m.transcriptLayout.Records,
			)
		}
	}
}

func TestTranscriptSearchAssistantMarkdownUsesSafeVisibleProjection(t *testing.T) {
	const reasoningSecret = "MARKDOWN_REASONING_MUST_NOT_BE_SEARCHABLE"
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	entry := settledSearchEntry(
		"search-markdown-answer",
		"search-markdown-turn",
		"assistant",
		"# Heading Marker\n\n- alpha marker\n- [linked marker](https://example.com)",
	)
	entry.ThinkingContent = reasoningSecret + "\nalpha marker"
	entry.ThinkingCollapsed = false
	installSearchTranscript(t, m, []ChatEntry{entry}, nil)
	_ = m.openTranscriptSearch()

	for _, test := range []struct {
		query       string
		wantMatches int
	}{
		{query: "Heading Marker", wantMatches: 1},
		{query: "alpha marker", wantMatches: 1},
		{query: "linked marker", wantMatches: 1},
		{query: reasoningSecret, wantMatches: 0},
	} {
		setTranscriptSearchQuery(t, m, test.query, false)
		if got := len(m.transcriptSearch.matches); got != test.wantMatches {
			t.Fatalf(
				"Markdown query %q matches = %d, want %d\nrows=%#v",
				test.query,
				got,
				test.wantMatches,
				m.transcriptSearch.rows,
			)
		}
	}
}

func TestTranscriptSearchRestoresDraftFocusAndManualAnchor(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 64, Height: 12})
	m = updated.(*Model)
	entries := make([]ChatEntry, 0, 48)
	for index := 0; index < 48; index++ {
		entries = append(entries, settledSearchEntry(
			BlockID(fmt.Sprintf("search-restore-%02d", index)),
			TurnID(fmt.Sprintf("search-restore-turn-%02d", index)),
			"user",
			fmt.Sprintf("history %02d RESTORE_MATCH", index),
		))
	}
	installSearchTranscript(t, m, entries, nil)
	m.pauseFollow()
	m.setTranscriptYOffset(m.transcriptLayout.Records[24].StartRow)
	originalTop := m.transcriptYOffset()
	const draft = "draft stays exactly here\nwith a second line"
	m.input.SetValue(draft)
	m.input.CursorEnd()
	_ = m.input.Focus()
	if !m.input.Focused() {
		t.Fatal("fixture composer is not focused")
	}

	_, handled := m.handleKeyPress(ctrlKey('f'))
	if !handled || m.overlay != OverlayTranscriptSearch ||
		m.transcriptSearch == nil {
		t.Fatalf(
			"Ctrl+F did not open search: handled=%t overlay=%d state=%v",
			handled,
			m.overlay,
			m.transcriptSearch != nil,
		)
	}
	if m.input.Value() != draft || m.input.Focused() ||
		!m.transcriptSearch.input.Focused() {
		t.Fatalf(
			"search ownership changed draft/focus: draft=%q composer=%t search=%t",
			m.input.Value(),
			m.input.Focused(),
			m.transcriptSearch.input.Focused(),
		)
	}
	for _, character := range "RESTORE_MATCH" {
		_, owned := m.handleOverlayKey(charKey(character))
		if !owned {
			t.Fatalf("search did not own printable %q", character)
		}
	}
	if m.transcriptYOffset() == originalTop || !m.followPaused() {
		t.Fatalf(
			"search did not navigate observationally: top=%d original=%d paused=%t",
			m.transcriptYOffset(),
			originalTop,
			m.followPaused(),
		)
	}

	_, owned := m.handleOverlayKey(escKey())
	if !owned || m.overlay != OverlayNone || m.transcriptSearch != nil {
		t.Fatalf(
			"Escape did not close search: owned=%t overlay=%d state=%v",
			owned,
			m.overlay,
			m.transcriptSearch != nil,
		)
	}
	if m.input.Value() != draft || !m.input.Focused() {
		t.Fatalf(
			"close did not restore draft/focus: draft=%q focused=%t",
			m.input.Value(),
			m.input.Focused(),
		)
	}
	if !m.followPaused() || m.transcriptYOffset() != originalTop {
		t.Fatalf(
			"manual anchor restore = paused:%t top:%d, want paused:true top:%d",
			m.followPaused(),
			m.transcriptYOffset(),
			originalTop,
		)
	}
}

func TestTranscriptSearchRestoresFollowLatestAfterHistoricalJump(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 56, Height: 12})
	m = updated.(*Model)
	entries := make([]ChatEntry, 0, 40)
	for index := 0; index < 40; index++ {
		entries = append(entries, settledSearchEntry(
			BlockID(fmt.Sprintf("search-follow-%02d", index)),
			TurnID(fmt.Sprintf("search-follow-turn-%02d", index)),
			"user",
			fmt.Sprintf("follow history %02d", index),
		))
	}
	installSearchTranscript(t, m, entries, nil)
	m.resumeFollow()
	if m.followPaused() || !m.transcriptAtBottom() {
		t.Fatal("fixture is not following latest")
	}

	_ = m.openTranscriptSearch()
	setTranscriptSearchQuery(t, m, "follow history 00", true)
	if !m.followPaused() || m.transcriptAtBottom() {
		t.Fatal("historical search did not temporarily pause follow")
	}
	_ = m.closeTranscriptSearch(true)
	if m.followPaused() || !m.transcriptAtBottom() {
		t.Fatalf(
			"close did not restore follow latest: paused=%t top=%d max=%d",
			m.followPaused(),
			m.transcriptYOffset(),
			m.transcriptMaxTop(),
		)
	}
}

func TestTranscriptSearchPreservesComposerFocusFromBackgroundLoad(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.fileLoading = true
	m.fileOpToken = 41
	m.input.Blur()
	_ = m.openTranscriptSearch()
	if m.transcriptSearch == nil || m.transcriptSearch.composerFocused {
		t.Fatal("search fixture did not capture the busy blurred composer")
	}

	updated, _ := m.Update(ContextLoadResultMsg{
		Token: 41,
		Path:  "context.md",
		Data:  "loaded context",
	})
	m = updated.(*Model)
	if m.fileLoading || !m.input.Focused() ||
		m.overlay != OverlayTranscriptSearch {
		t.Fatalf(
			"background load state = loading:%t composer:%t overlay:%d",
			m.fileLoading,
			m.input.Focused(),
			m.overlay,
		)
	}

	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayNone || !m.input.Focused() {
		t.Fatalf(
			"search close lost newer focus: overlay=%d focused=%t",
			m.overlay,
			m.input.Focused(),
		)
	}
	updated, _ = m.Update(charKey('x'))
	m = updated.(*Model)
	if got := m.input.Value(); got != "x" {
		t.Fatalf("first printable after background load = %q, want %q", got, "x")
	}
}

func TestOpenTranscriptSearchReplacesOrphanedState(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 52, Height: 12})
	m = updated.(*Model)
	entries := make([]ChatEntry, 0, 30)
	for index := 0; index < 30; index++ {
		entries = append(entries, settledSearchEntry(
			BlockID(fmt.Sprintf("search-orphan-%02d", index)),
			TurnID(fmt.Sprintf("search-orphan-turn-%02d", index)),
			"user",
			fmt.Sprintf("fresh searchable transcript %02d", index),
		))
	}
	installSearchTranscript(t, m, entries, nil)
	m.pauseFollow()
	m.setTranscriptYOffset(m.transcriptLayout.Records[10].StartRow)
	originalTop := m.transcriptYOffset()
	previous := func(int) lipgloss.Style {
		return lipgloss.NewStyle().Italic(true)
	}
	orphan := newTranscriptSearchState(20, m.isDark, m.themeID, m.reducedMotion)
	orphan.restore = m.captureTranscriptReflowAnchor()
	orphan.composerFocused = m.input.Focused()
	_ = orphan.input.Focus()
	m.transcriptSearch = orphan
	m.overlay = OverlayNone
	m.viewport.StyleLineFunc = previous
	m.resumeFollow()
	if !m.transcriptAtBottom() {
		t.Fatal("orphan fixture did not simulate a historical navigation jump")
	}

	_ = m.openTranscriptSearch()
	if m.overlay != OverlayTranscriptSearch ||
		m.transcriptSearch == nil ||
		m.transcriptSearch == orphan {
		t.Fatalf(
			"orphan recovery = overlay:%d state:%p orphan:%p",
			m.overlay,
			m.transcriptSearch,
			orphan,
		)
	}
	if orphan.input.Focused() {
		t.Fatal("orphaned search input retained keyboard focus")
	}
	if !m.followPaused() || m.transcriptYOffset() != originalTop {
		t.Fatalf(
			"orphan recovery lost captured anchor: paused=%t top=%d want=%d",
			m.followPaused(),
			m.transcriptYOffset(),
			originalTop,
		)
	}
	if m.viewport.StyleLineFunc == nil ||
		reflect.ValueOf(m.viewport.StyleLineFunc).Pointer() !=
			reflect.ValueOf(previous).Pointer() {
		t.Fatal("fresh search replaced the existing viewport style")
	}
	if len(m.transcriptSearch.rows) == 0 {
		t.Fatal("fresh search did not rebuild its safe transcript index")
	}
}

func TestTranscriptSearchNextPreviousWrapBySemanticIdentity(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 52, Height: 12})
	m = updated.(*Model)
	entries := make([]ChatEntry, 0, 24)
	var matchIDs []BlockID
	for index := 0; index < 24; index++ {
		id := BlockID(fmt.Sprintf("search-nav-%02d", index))
		content := fmt.Sprintf("ordinary row %02d", index)
		if index == 2 || index == 11 || index == 20 {
			content += " NAVIGATE_NEEDLE"
			matchIDs = append(matchIDs, id)
		}
		entries = append(entries, settledSearchEntry(
			id,
			TurnID(fmt.Sprintf("search-nav-turn-%02d", index)),
			"user",
			content,
		))
	}
	installSearchTranscript(t, m, entries, nil)
	_ = m.openTranscriptSearch()
	setTranscriptSearchQuery(t, m, "navigate_needle", true)
	state := m.transcriptSearch
	if len(state.matches) != len(matchIDs) ||
		state.matches[0].BlockID != matchIDs[0] ||
		state.active != 0 {
		t.Fatalf("initial matches = %#v active=%d", state.matches, state.active)
	}

	m.navigateTranscriptSearch(1)
	if state.active != 1 || state.matches[state.active].BlockID != matchIDs[1] {
		t.Fatalf("next active=%d match=%s", state.active, state.matches[state.active].BlockID)
	}
	m.navigateTranscriptSearch(-1)
	if state.active != 0 || state.matches[state.active].BlockID != matchIDs[0] {
		t.Fatalf("previous active=%d match=%s", state.active, state.matches[state.active].BlockID)
	}
	m.navigateTranscriptSearch(-1)
	if state.active != 2 || state.matches[state.active].BlockID != matchIDs[2] {
		t.Fatalf("wrapped previous active=%d match=%s", state.active, state.matches[state.active].BlockID)
	}
	m.navigateTranscriptSearch(1)
	if state.active != 0 || state.matches[state.active].BlockID != matchIDs[0] {
		t.Fatalf("wrapped next active=%d match=%s", state.active, state.matches[state.active].BlockID)
	}
	if !m.followPaused() || state.activeRow < 0 {
		t.Fatalf("navigation lost paused/highlight state: paused=%t row=%d", m.followPaused(), state.activeRow)
	}
}

func TestTranscriptSearchMatchRowUsesBoundedIndexCoordinate(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	installSearchTranscript(t, m, []ChatEntry{
		settledSearchEntry(
			"search-coordinate-a",
			"search-coordinate-turn-a",
			"user",
			"ordinary history",
		),
		settledSearchEntry(
			"search-coordinate-b",
			"search-coordinate-turn-b",
			"user",
			"BOUNDED_COORDINATE_NEEDLE",
		),
	}, nil)
	_ = m.openTranscriptSearch()
	setTranscriptSearchQuery(t, m, "bounded_coordinate_needle", false)
	state := m.transcriptSearch
	if len(state.matches) != 1 {
		t.Fatalf("coordinate matches = %d, want 1", len(state.matches))
	}
	match := state.matches[0]
	want := match.AbsoluteRow

	// Navigation is tied to the generation-stamped absolute coordinate stored
	// in the bounded index; it must not scan the full layout on every key.
	m.transcriptLayout.Records = nil
	if got, ok := m.transcriptSearchMatchRow(match); !ok || got != want {
		t.Fatalf(
			"bounded match coordinate = row:%d ok:%t, want row:%d",
			got,
			ok,
			want,
		)
	}
}

func TestTranscriptSearchBoundsIndexMatchesAndQuery(t *testing.T) {
	t.Run("bounded index rows", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		m.transcriptSearch = newTranscriptSearchState(40, m.isDark, m.themeID, m.reducedMotion)
		m.transcriptLayout = TranscriptLayoutSnapshot{}
		m.transcriptPaint.document = transcriptPaintDocument{}
		for index := 0; index < 4; index++ {
			id := BlockID(fmt.Sprintf("search-bound-row-%d", index))
			turn := TurnID(fmt.Sprintf("search-bound-turn-%d", index))
			m.entries = append(m.entries, settledSearchEntry(id, turn, "user", "row"))
			m.transcriptLayout.Records = append(
				m.transcriptLayout.Records,
				TranscriptLayoutRecord{
					BlockID:  id,
					TurnID:   turn,
					Revision: 1,
					Height:   1,
					StartRow: index,
					Exact:    true,
					LineMap: LineMap{{
						LogicalOffset: 1,
						Row:           0,
					}},
				},
			)
			m.transcriptPaint.document.base = append(
				m.transcriptPaint.document.base,
				transcriptPaintBlock{
					startRow:   index,
					content:    "row",
					lineStarts: []int{0},
				},
			)
		}
		m.transcriptPaint.document.totalRows = 4
		m.rebuildTranscriptSearchIndexBounded(3, 100)
		if got := len(m.transcriptSearch.rows); got != 3 ||
			!m.transcriptSearch.indexCapped {
			t.Fatalf(
				"row-bounded index = %d capped=%t, want 3/true",
				got,
				m.transcriptSearch.indexCapped,
			)
		}
		for index, row := range m.transcriptSearch.rows {
			want := BlockID(fmt.Sprintf("search-bound-row-%d", index+1))
			if row.BlockID != want {
				t.Fatalf(
					"recent row %d BlockID = %s, want %s; rows=%#v",
					index,
					row.BlockID,
					want,
					m.transcriptSearch.rows,
				)
			}
		}
	})

	t.Run("bounded index bytes", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		m.transcriptSearch = newTranscriptSearchState(40, m.isDark, m.themeID, m.reducedMotion)
		m.transcriptLayout = TranscriptLayoutSnapshot{}
		m.transcriptPaint.document = transcriptPaintDocument{}
		for index := 0; index < 3; index++ {
			id := BlockID(fmt.Sprintf("search-bound-byte-%d", index))
			turn := TurnID(fmt.Sprintf("search-byte-turn-%d", index))
			m.entries = append(m.entries, settledSearchEntry(id, turn, "user", "abcd"))
			m.transcriptLayout.Records = append(
				m.transcriptLayout.Records,
				TranscriptLayoutRecord{
					BlockID:  id,
					TurnID:   turn,
					Revision: 1,
					Height:   1,
					StartRow: index,
					Exact:    true,
					LineMap: LineMap{{
						LogicalOffset: 1,
						Row:           0,
					}},
				},
			)
			m.transcriptPaint.document.base = append(
				m.transcriptPaint.document.base,
				transcriptPaintBlock{
					startRow:   index,
					content:    "abcd",
					lineStarts: []int{0},
				},
			)
		}
		m.transcriptPaint.document.totalRows = 3
		m.rebuildTranscriptSearchIndexBounded(100, 7)
		if got := len(m.transcriptSearch.rows); got != 1 ||
			m.transcriptSearch.indexBytes != 4 ||
			!m.transcriptSearch.indexCapped {
			t.Fatalf(
				"byte-bounded index = rows:%d bytes:%d capped:%t",
				got,
				m.transcriptSearch.indexBytes,
				m.transcriptSearch.indexCapped,
			)
		}
		if got, want := m.transcriptSearch.rows[0].BlockID,
			BlockID("search-bound-byte-2"); got != want {
			t.Fatalf("byte-bounded row = %s, want recent %s", got, want)
		}
	})

	t.Run("oversized older record cannot block latest", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		m.transcriptSearch = newTranscriptSearchState(
			40,
			m.isDark,
			m.themeID,
			m.reducedMotion,
		)
		const latest = "LATEST_NEEDLE"
		older := strings.Repeat("o", 1024)
		entries := []ChatEntry{
			settledSearchEntry(
				"search-byte-oversized-old",
				"search-byte-oversized-turn",
				"user",
				older,
			),
			settledSearchEntry(
				"search-byte-latest",
				"search-byte-latest-turn",
				"user",
				latest,
			),
		}
		m.entries = entries
		m.transcriptLayout.Records = []TranscriptLayoutRecord{
			{
				BlockID:  entries[0].BlockID,
				TurnID:   entries[0].TurnID,
				Revision: 1,
				Height:   1,
				StartRow: 0,
				Exact:    true,
				LineMap: LineMap{{
					LogicalOffset: 1,
					Row:           0,
				}},
			},
			{
				BlockID:  entries[1].BlockID,
				TurnID:   entries[1].TurnID,
				Revision: 1,
				Height:   1,
				StartRow: 1,
				Exact:    true,
				LineMap: LineMap{{
					LogicalOffset: 1,
					Row:           0,
				}},
			},
		}
		m.transcriptPaint.document = transcriptPaintDocument{
			base: []transcriptPaintBlock{
				newTranscriptPaintBlock(0, older),
				newTranscriptPaintBlock(1, latest),
			},
			totalRows: 2,
		}

		m.rebuildTranscriptSearchIndexBounded(2, 64)
		state := m.transcriptSearch
		if len(state.rows) != 1 ||
			state.rows[0].BlockID != entries[1].BlockID ||
			state.rows[0].RenderedText != latest ||
			!state.indexCapped {
			t.Fatalf(
				"byte suffix = rows:%#v capped:%t",
				state.rows,
				state.indexCapped,
			)
		}

		var invalid strings.Builder
		for range 20 {
			invalid.WriteByte(0xff)
			invalid.WriteByte(' ')
		}
		entries[0].Content = invalid.String()
		m.entries = entries
		m.transcriptPaint.document = transcriptPaintDocument{
			base: []transcriptPaintBlock{
				newTranscriptPaintBlock(0, "OLDER_SAFE_ROW"),
				newTranscriptPaintBlock(1, latest),
			},
			totalRows: 2,
		}
		m.rebuildTranscriptSearchIndexBounded(2, 64)
		state = m.transcriptSearch
		if len(state.rows) != 1 ||
			state.rows[0].BlockID != entries[1].BlockID {
			t.Fatalf(
				"sanitization expansion blocked latest row: %#v",
				state.rows,
			)
		}
	})

	t.Run("tool projection does not consume assistant source budget", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		tools := []ToolEntry{{
			ID:        "search-budget-tool",
			Name:      "read_file",
			Summary:   "safe projected tool summary",
			Result:    strings.Repeat("safe projected result ", 8),
			Status:    ToolStatusDone,
			Collapsed: false,
		}}
		tool := settledSearchEntry(
			"search-budget-tool-block",
			"search-budget-turn",
			"tool_group",
			"",
		)
		tool.ToolIndex = 0
		const latest = "LATEST_ASSISTANT_BUDGET_NEEDLE"
		assistant := settledSearchEntry(
			"search-budget-assistant",
			"search-budget-turn",
			"assistant",
			latest+strings.Repeat("\n", 4096),
		)
		installSearchTranscript(
			t,
			m,
			[]ChatEntry{tool, assistant},
			tools,
		)
		_ = m.openTranscriptSearch()

		renderedBytes := 0
		for _, record := range m.transcriptLayout.Records {
			rows, _ := m.transcriptPaint.document.materializeRows(
				record.StartRow,
				record.StartRow+record.Height,
			)
			for _, row := range rows {
				renderedBytes += len(row)
			}
		}
		byteLimit := max(renderedBytes, len(assistant.Content))
		if byteLimit != len(assistant.Content) {
			t.Fatalf(
				"fixture source=%d bytes does not dominate rendered=%d",
				len(assistant.Content),
				renderedBytes,
			)
		}
		m.rebuildTranscriptSearchIndexBounded(
			m.transcriptPaint.document.totalRows,
			byteLimit,
		)
		setTranscriptSearchQuery(t, m, latest, false)
		if got := len(m.transcriptSearch.matches); got != 1 {
			t.Fatalf(
				"recent assistant matches = %d, want 1; rows=%#v",
				got,
				m.transcriptSearch.rows,
			)
		}
	})

	t.Run("oversized newest record fails closed at row budget", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		m.transcriptSearch = newTranscriptSearchState(40, m.isDark, m.themeID, m.reducedMotion)
		m.transcriptLayout = TranscriptLayoutSnapshot{}
		m.transcriptPaint.document = transcriptPaintDocument{}
		lines := []string{"first", "second", "third", "fourth"}
		content := strings.Join(lines, "\n")
		entry := settledSearchEntry(
			"search-bound-record",
			"search-bound-record-turn",
			"user",
			content,
		)
		m.entries = []ChatEntry{entry}
		m.transcriptLayout.Records = []TranscriptLayoutRecord{{
			BlockID:  entry.BlockID,
			TurnID:   entry.TurnID,
			Revision: 1,
			Height:   len(lines),
			StartRow: 0,
			Exact:    true,
			LineMap: LineMap{
				{LogicalOffset: 1, Row: 0},
				{LogicalOffset: 2, Row: 1},
				{LogicalOffset: 3, Row: 2},
				{LogicalOffset: 4, Row: 3},
			},
		}}
		m.transcriptPaint.document.base = []transcriptPaintBlock{
			newTranscriptPaintBlock(0, content),
		}
		m.transcriptPaint.document.totalRows = len(lines)

		m.rebuildTranscriptSearchIndexBounded(2, 100)
		if got := len(m.transcriptSearch.rows); got != 0 ||
			!m.transcriptSearch.indexCapped {
			t.Fatalf(
				"record-bounded index = %d capped=%t, want 0/true",
				got,
				m.transcriptSearch.indexCapped,
			)
		}
	})

	t.Run("oversized rendered row fails closed before semantic strip", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		m.transcriptSearch = newTranscriptSearchState(40, m.isDark, m.themeID, m.reducedMotion)
		m.transcriptLayout = TranscriptLayoutSnapshot{}
		m.transcriptPaint.document = transcriptPaintDocument{}
		content := strings.Repeat("x", 1024)
		entry := settledSearchEntry(
			"search-bound-giant-row",
			"search-bound-giant-turn",
			"user",
			content,
		)
		m.entries = []ChatEntry{entry}
		m.transcriptLayout.Records = []TranscriptLayoutRecord{{
			BlockID:  entry.BlockID,
			TurnID:   entry.TurnID,
			Revision: 1,
			Height:   1,
			StartRow: 0,
			Exact:    true,
			LineMap: LineMap{{
				LogicalOffset: 1,
				Row:           0,
			}},
		}}
		m.transcriptPaint.document.base = []transcriptPaintBlock{
			newTranscriptPaintBlock(0, content),
		}
		m.transcriptPaint.document.totalRows = 1

		m.rebuildTranscriptSearchIndexBounded(10, 16)
		if len(m.transcriptSearch.rows) != 0 ||
			m.transcriptSearch.indexBytes != 0 ||
			!m.transcriptSearch.indexCapped {
			t.Fatalf(
				"giant-row index = rows:%d bytes:%d capped:%t",
				len(m.transcriptSearch.rows),
				m.transcriptSearch.indexBytes,
				m.transcriptSearch.indexCapped,
			)
		}
	})

	t.Run("bounded matches", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		m.transcriptSearch = newTranscriptSearchState(40, m.isDark, m.themeID, m.reducedMotion)
		m.transcriptSearch.generation = m.transcriptPaint.documentGeneration
		m.transcriptSearch.rows = []transcriptSearchRow{{
			BlockID:      "search-match-bound",
			TurnID:       "search-match-bound-turn",
			Revision:     1,
			RenderedText: strings.Repeat("x", transcriptSearchMatchLimit+7),
		}}
		m.transcriptSearch.input.SetValue("x")
		m.recomputeTranscriptSearchMatches(false)
		if got := len(m.transcriptSearch.matches); got != transcriptSearchMatchLimit ||
			!m.transcriptSearch.matchesCapped {
			t.Fatalf(
				"bounded matches = %d capped=%t, want %d/true",
				got,
				m.transcriptSearch.matchesCapped,
				transcriptSearchMatchLimit,
			)
		}
	})

	t.Run("bounded query", func(t *testing.T) {
		state := newTranscriptSearchState(40, false, defaultThemeID, false)
		state.input.SetValue(strings.Repeat("界", transcriptSearchQueryLimit+8))
		if got := len([]rune(state.input.Value())); got != transcriptSearchQueryLimit {
			t.Fatalf("query runes = %d, want %d", got, transcriptSearchQueryLimit)
		}
	})
}

func BenchmarkTranscriptSearchMatchesAtMaximumIndex(b *testing.B) {
	rowText := strings.Repeat("x", 64) + " COMMON_NEEDLE"
	rows := make([]transcriptSearchRow, transcriptSearchRowLimit)
	totalBytes := 0
	for index := range rows {
		rows[index] = transcriptSearchRow{
			BlockID:      BlockID(fmt.Sprintf("benchmark-search-%06d", index)),
			TurnID:       "benchmark-search-turn",
			Revision:     1,
			LocalRow:     index,
			RenderedText: rowText,
		}
		totalBytes += len(rowText)
	}
	if totalBytes > transcriptSearchByteLimit {
		b.Fatalf(
			"benchmark fixture = %d bytes, index limit = %d",
			totalBytes,
			transcriptSearchByteLimit,
		)
	}

	for _, benchmark := range []struct {
		name  string
		query string
	}{
		{name: "rare_no_match", query: "ABSENT_NEEDLE"},
		{name: "common_match_capped", query: "COMMON_NEEDLE"},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			m := newTestModel(b)
			m.transcriptPaint.documentGeneration = 1
			m.transcriptSearch = newTranscriptSearchState(
				80,
				m.isDark,
				m.themeID,
				m.reducedMotion,
			)
			m.transcriptSearch.rows = rows
			m.transcriptSearch.generation = 1
			m.transcriptSearch.input.SetValue(benchmark.query)
			m.recomputeTranscriptSearchMatches(false)
			b.ReportAllocs()
			b.SetBytes(int64(totalBytes))
			b.ResetTimer()
			for b.Loop() {
				m.recomputeTranscriptSearchMatches(false)
			}
		})
	}
}

func TestTranscriptSearchUnicodeCaseAndCombiningMarks(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	installSearchTranscript(t, m, []ChatEntry{
		settledSearchEntry(
			"search-unicode",
			"search-unicode-turn",
			"user",
			"CAFÉ 你 e\u0301 👩🏽‍💻",
		),
	}, nil)
	_ = m.openTranscriptSearch()
	for _, query := range []string{"café", "你", "e\u0301", "👩🏽‍💻"} {
		setTranscriptSearchQuery(t, m, query, false)
		if got := len(m.transcriptSearch.matches); got != 1 {
			t.Fatalf("Unicode query %q matches = %d, want 1", query, got)
		}
	}
}

func TestTranscriptSearchTypingPreservesWordSeparators(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	installSearchTranscript(t, m, []ChatEntry{
		settledSearchEntry(
			"search-space",
			"search-space-turn",
			"assistant",
			"interrupted effect remains searchable",
		),
	}, nil)
	_ = m.openTranscriptSearch()

	for _, character := range "interrupted effect" {
		m.handleTranscriptSearchKey(charKey(character))
	}
	if got, want := m.transcriptSearch.input.Value(), "interrupted effect"; got != want {
		t.Fatalf("typed query = %q, want %q", got, want)
	}
	if got := len(m.transcriptSearch.matches); got != 1 {
		t.Fatalf("word-separated query matches = %d, want 1", got)
	}

	m.handleTranscriptSearchKey(charKey(' '))
	if got, want := m.transcriptSearch.input.Value(), "interrupted effect "; got != want {
		t.Fatalf("trailing-space query = %q, want %q", got, want)
	}
	if got := len(m.transcriptSearch.matches); got != 1 {
		t.Fatalf("trimmed search semantics after trailing space = %d matches, want 1", got)
	}
}

func TestTranscriptSearchActiveRowSignalPreservesGeometryAndRestoresStyle(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	previous := func(index int) lipgloss.Style {
		if index%2 == 0 {
			return lipgloss.NewStyle().Italic(true)
		}
		return lipgloss.NewStyle()
	}
	m.viewport.StyleLineFunc = previous
	previousPointer := reflect.ValueOf(previous).Pointer()
	installSearchTranscript(t, m, []ChatEntry{
		settledSearchEntry(
			"search-highlight",
			"search-highlight-turn",
			"user",
			"ACTIVE_ROW_MARKER",
		),
	}, nil)
	_ = m.openTranscriptSearch()
	setTranscriptSearchQuery(t, m, "active_row_marker", true)
	state := m.transcriptSearch
	if state.activeRow < 0 {
		t.Fatalf("active signal was not installed: row=%d", state.activeRow)
	}
	if m.viewport.StyleLineFunc == nil ||
		reflect.ValueOf(m.viewport.StyleLineFunc).Pointer() != previousPointer {
		t.Fatal("search replaced the viewport's existing line-style hook")
	}
	style := m.transcriptSearchActiveRowStyle()
	if !style.GetBold() || style.GetUnderline() {
		t.Fatalf("active row style = %#v, want bold without grapheme-splitting underline", style)
	}
	if !noColor {
		assertSameColor(
			t,
			"active transcript search row",
			style.GetForeground(),
			outputSemanticPalette(m.isDark, m.themeID).Accent,
		)
	}
	localRow := state.activeRow - m.transcriptPaint.windowStart
	stagedRows := strings.Split(m.viewport.GetContent(), "\n")
	if localRow < 0 || localRow >= len(stagedRows) {
		t.Fatalf("active local row = %d, staged rows = %d", localRow, len(stagedRows))
	}
	originalRows, _ := m.transcriptPaint.document.materializeRows(
		state.activeRow,
		state.activeRow+1,
	)
	styled := stagedRows[localRow]
	if len(originalRows) != 1 ||
		ansi.Strip(styled) != ansi.Strip(originalRows[0]) ||
		lipgloss.Width(styled) != lipgloss.Width(originalRows[0]) ||
		lipgloss.Height(styled) != 1 {
		t.Fatalf(
			"active signal changed geometry/content: %q width=%d/%d height=%d",
			ansi.Strip(styled),
			lipgloss.Width(styled),
			lipgloss.Width(originalRows[0]),
			lipgloss.Height(styled),
		)
	}
	generation := m.transcriptPaint.documentGeneration
	m.transcriptPaint.documentGeneration++
	m.syncTranscriptPaintWindow()
	stagedRows = strings.Split(m.viewport.GetContent(), "\n")
	if got := stagedRows[localRow]; got != originalRows[0] {
		t.Fatalf("stale search generation retained highlight: %q", got)
	}
	m.transcriptPaint.documentGeneration = generation

	_ = m.closeTranscriptSearch(true)
	if m.viewport.StyleLineFunc == nil ||
		reflect.ValueOf(m.viewport.StyleLineFunc).Pointer() != previousPointer {
		t.Fatal("closing search did not restore the previous viewport line style")
	}
}

func TestTranscriptSearchActiveRowPreservesANSIStyledMarkdown(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	entry := settledSearchEntry(
		"search-highlight-ansi",
		"search-highlight-ansi-turn",
		"assistant",
		"ANSI_ACTIVE_ROW_MARKER",
	)
	entry.RenderedContent = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#d8dee9")).
		Render(entry.Content)
	installSearchTranscript(t, m, []ChatEntry{entry}, nil)
	_ = m.openTranscriptSearch()
	setTranscriptSearchQuery(t, m, "ansi_active_row_marker", true)
	state := m.transcriptSearch
	if len(state.matches) != 1 || state.activeRow < 0 {
		t.Fatalf(
			"ANSI fixture matches=%d activeRow=%d rows=%#v",
			len(state.matches),
			state.activeRow,
			state.rows,
		)
	}
	originalRows, _ := m.transcriptPaint.document.materializeRows(
		state.activeRow,
		state.activeRow+1,
	)
	if len(originalRows) != 1 ||
		!strings.Contains(originalRows[0], "\x1b[") {
		t.Fatalf("fixture is not ANSI styled: %#v", originalRows)
	}
	localRow := state.activeRow - m.transcriptPaint.windowStart
	stagedRows := strings.Split(m.viewport.GetContent(), "\n")
	if localRow < 0 || localRow >= len(stagedRows) {
		t.Fatalf("active local row = %d, staged rows = %d", localRow, len(stagedRows))
	}
	staged := stagedRows[localRow]
	if got, want := ansi.Strip(staged), ansi.Strip(originalRows[0]); got != want {
		t.Fatalf(
			"ANSI highlight exposed or changed control bytes:\n got %q\nwant %q",
			got,
			want,
		)
	}
	if strings.Contains(ansi.Strip(staged), "[38;") ||
		strings.Contains(ansi.Strip(staged), "[0m") {
		t.Fatalf("ANSI highlight exposed SGR fragments: %q", ansi.Strip(staged))
	}
	if lipgloss.Width(staged) != lipgloss.Width(originalRows[0]) {
		t.Fatalf(
			"ANSI highlight width = %d, want %d",
			lipgloss.Width(staged),
			lipgloss.Width(originalRows[0]),
		)
	}
}

func TestTranscriptSearchCompactLayoutFitsRowsAndOwnsCursor(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	updated, _ := m.Update(tea.WindowSizeMsg{
		Width:  minTerminalWidth,
		Height: minTerminalHeight,
	})
	m = updated.(*Model)
	installSearchTranscript(t, m, []ChatEntry{
		settledSearchEntry(
			"search-compact",
			"search-compact-turn",
			"user",
			"compact marker",
		),
	}, nil)
	_ = m.openTranscriptSearch()
	setTranscriptSearchQuery(t, m, "界界界界界界界界界界", false)

	rendered, cursor := m.renderTranscriptSearchView()
	rows := strings.Split(rendered, "\n")
	if len(rows) != 2 {
		t.Fatalf("search footer rows = %d, want 2\n%s", len(rows), ansi.Strip(rendered))
	}
	for index, row := range rows {
		if got := lipgloss.Width(row); got != m.chatPaneWidth() {
			t.Fatalf(
				"compact row %d width = %d, want %d: %q",
				index,
				got,
				m.chatPaneWidth(),
				ansi.Strip(row),
			)
		}
	}
	if cursor == nil || cursor.X < 0 ||
		cursor.X >= m.chatPaneWidth() || cursor.Y != 0 {
		t.Fatalf("compact local cursor = %#v", cursor)
	}

	frame := m.projectFrame()
	if frame.Cursor == nil ||
		!frame.Footer.Rect.Contains(frame.Cursor.X, frame.Cursor.Y) {
		t.Fatalf(
			"projected cursor %#v outside footer %+v",
			frame.Cursor,
			frame.Footer.Rect,
		)
	}
	view := m.View()
	if view.Cursor == nil || view.Cursor.X < 0 || view.Cursor.X >= m.width ||
		view.Cursor.Y < 0 || view.Cursor.Y >= m.height {
		t.Fatalf("compact view cursor = %#v for %dx%d", view.Cursor, m.width, m.height)
	}
	for index, row := range strings.Split(view.Content, "\n") {
		if got := lipgloss.Width(row); got > m.width {
			t.Fatalf("view row %d width = %d, terminal = %d", index, got, m.width)
		}
	}
}

func TestTranscriptSearchResizeRecomputesTextInputOverflowWindow(t *testing.T) {
	state := newTranscriptSearchState(24, false, defaultThemeID, true)
	query := "resize preserves the complete visible search query"
	state.input.SetValue(query)
	state.input.CursorEnd()
	state.setWidth(24)
	narrow := ansi.Strip(state.input.View())
	if strings.Contains(narrow, query) {
		t.Fatal("narrow fixture did not create a clipped overflow window")
	}

	state.setWidth(80)
	wide := ansi.Strip(state.input.View())
	if !strings.Contains(wide, query) {
		t.Fatalf("expanded input retained stale narrow overflow window: %q", wide)
	}
	fitted := transcriptSearchFitRow(state.input.View(), state.width)
	if got := lipgloss.Width(fitted); got != state.width {
		t.Fatalf("expanded input width = %d, want %d", got, state.width)
	}
}

func TestTranscriptSearchRefreshesStaleIndexOnNavigationWithoutSkippingFirst(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.entries = []ChatEntry{settledSearchEntry(
		"search-refresh-user",
		"search-refresh-turn",
		"user",
		"start",
	)}
	m.state = StateStreaming
	m.resetEntryMemo()
	m.refreshTranscript()
	_ = m.openTranscriptSearch()
	setTranscriptSearchQuery(t, m, "arrived_later", false)
	if len(m.transcriptSearch.matches) != 0 {
		t.Fatal("fixture unexpectedly matched before stream update")
	}

	updated, _ := m.Update(StreamTextMsg{
		Text: "ARRIVED_LATER and ARRIVED_LATER",
	})
	m = updated.(*Model)
	if m.transcriptSearch == nil {
		t.Fatal("stream update closed transcript search")
	}
	if m.transcriptSearch.generation == m.transcriptPaint.documentGeneration {
		t.Fatal("stream update eagerly rescanned the global index")
	}
	status, _ := m.renderTranscriptSearchView()
	if !strings.Contains(ansi.Strip(status), "transcript changed") {
		t.Fatalf("stale search status is not visible:\n%s", ansi.Strip(status))
	}

	m.navigateTranscriptSearch(1)
	if m.transcriptSearch.generation != m.transcriptPaint.documentGeneration {
		t.Fatal("navigation did not refresh the stale index")
	}
	if got := len(m.transcriptSearch.matches); got != 2 {
		t.Fatalf("navigation-refreshed matches = %d, want 2", got)
	}
	if m.transcriptSearch.active != 0 {
		t.Fatalf(
			"first navigation after zero matches selected %d, want first match",
			m.transcriptSearch.active,
		)
	}
}

func TestDismissOverlayRestoresSearchStateAndViewportStyle(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	previous := func(int) lipgloss.Style {
		return lipgloss.NewStyle().Italic(true)
	}
	m.viewport.StyleLineFunc = previous
	previousPointer := reflect.ValueOf(previous).Pointer()
	installSearchTranscript(t, m, []ChatEntry{
		settledSearchEntry(
			"search-dismiss",
			"search-dismiss-turn",
			"user",
			"dismiss marker",
		),
	}, nil)
	_ = m.openTranscriptSearch()
	m.dismissOverlay()

	if m.overlay != OverlayNone || m.transcriptSearch != nil ||
		!m.input.Focused() {
		t.Fatalf(
			"dismiss state = overlay:%d search:%v focus:%t",
			m.overlay,
			m.transcriptSearch != nil,
			m.input.Focused(),
		)
	}
	if m.viewport.StyleLineFunc == nil ||
		reflect.ValueOf(m.viewport.StyleLineFunc).Pointer() != previousPointer {
		t.Fatal("generic dismiss leaked the search viewport style")
	}
}

func TestTranscriptSearchANSINavigationRoundTripLeavesNoGhostHighlight(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	m = updated.(*Model)

	first := settledSearchEntry(
		"search-ansi-navigation-a",
		"search-ansi-navigation-turn-a",
		"assistant",
		"ANSI_NAVIGATION_NEEDLE alpha",
	)
	first.RenderedContent = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#88c0d0")).
		Render(first.Content)
	second := settledSearchEntry(
		"search-ansi-navigation-b",
		"search-ansi-navigation-turn-b",
		"assistant",
		"ANSI_NAVIGATION_NEEDLE beta",
	)
	second.RenderedContent = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#d08770")).
		Render(second.Content)
	installSearchTranscript(t, m, []ChatEntry{first, second}, nil)
	baseline, _ := m.transcriptPaint.document.materializeRows(
		0,
		m.transcriptPaint.document.totalRows,
	)

	_ = m.openTranscriptSearch()
	setTranscriptSearchQuery(t, m, "ansi_navigation_needle", true)
	state := m.transcriptSearch
	if len(state.matches) != 2 {
		t.Fatalf("ANSI navigation matches = %d, want 2", len(state.matches))
	}
	rowA := state.matches[0].AbsoluteRow
	rowB := state.matches[1].AbsoluteRow
	if rowA == rowB {
		t.Fatalf("ANSI navigation fixture collapsed onto row %d", rowA)
	}
	assertTranscriptSearchHighlightRows(t, m, baseline, rowA, rowB, rowA)

	m.navigateTranscriptSearch(1)
	if state.active != 1 || state.activeRow != rowB {
		t.Fatalf("A→B navigation = active:%d row:%d, want active:1 row:%d",
			state.active, state.activeRow, rowB)
	}
	assertTranscriptSearchHighlightRows(t, m, baseline, rowA, rowB, rowB)

	m.navigateTranscriptSearch(-1)
	if state.active != 0 || state.activeRow != rowA {
		t.Fatalf("B→A navigation = active:%d row:%d, want active:0 row:%d",
			state.active, state.activeRow, rowA)
	}
	assertTranscriptSearchHighlightRows(t, m, baseline, rowA, rowB, rowA)

	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayNone || m.transcriptSearch != nil {
		t.Fatalf("Escape retained search state: overlay=%d state=%t",
			m.overlay, m.transcriptSearch != nil)
	}
	m.syncTranscriptPaintWindow()
	for _, row := range []int{rowA, rowB} {
		if got := stagedTranscriptSearchRow(t, m, row); got != baseline[row] {
			t.Fatalf("row %d retained a ghost highlight:\n got %q\nwant %q",
				row, got, baseline[row])
		}
	}
	assertNoLiteralANSIEscapeFragments(t, m.View().Content)
}

func TestTranscriptSearchFullViewGeometryAcrossWidths(t *testing.T) {
	for _, size := range []struct {
		name       string
		width      int
		widthClass WidthClass
		glyphs     GlyphProfile
		ascii      bool
	}{
		{name: "narrow", width: 40, widthClass: WidthNarrow, glyphs: GlyphUnicode},
		{name: "narrow_ascii", width: 40, widthClass: WidthNarrow, glyphs: GlyphASCII, ascii: true},
		{name: "medium", width: 80, widthClass: WidthRegular, glyphs: GlyphUnicode},
		{name: "wide", width: 120, widthClass: WidthWide, glyphs: GlyphUnicode},
	} {
		t.Run(size.name, func(t *testing.T) {
			m := newTestModel(t)
			m.height = 13 // sticky off: layout/search fixtures need every entry painted
			m.glyphProfile = size.glyphs
			updated, _ := m.Update(tea.WindowSizeMsg{
				Width:  size.width,
				Height: 24,
			})
			m = updated.(*Model)
			entry := settledSearchEntry(
				BlockID("search-full-view-"+size.name),
				TurnID("search-full-view-turn-"+size.name),
				"assistant",
				"responsive search query 界e\u0301🙂",
			)
			entry.RenderedContent = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#a3be8c")).
				Render(entry.Content)
			installSearchTranscript(t, m, []ChatEntry{entry}, nil)
			_ = m.openTranscriptSearch()
			setTranscriptSearchQuery(t, m, "界e\u0301🙂", true)

			searchView, localCursor := m.renderTranscriptSearchView()
			searchRows := strings.Split(searchView, "\n")
			if got := lipgloss.Height(searchView); got != 2 ||
				len(searchRows) != 2 {
				t.Fatalf("search owner = %d rows (%d split), want 2:\n%s",
					got, len(searchRows), ansi.Strip(searchView))
			}
			for row, line := range searchRows {
				if got := lipgloss.Width(line); got != m.chatPaneWidth() {
					t.Fatalf("search row %d width = %d, want %d: %q",
						row, got, m.chatPaneWidth(), ansi.Strip(line))
				}
			}
			if localCursor == nil || localCursor.Y != 0 ||
				localCursor.X < 0 || localCursor.X >= m.chatPaneWidth() {
				t.Fatalf("local search cursor = %#v for pane width %d",
					localCursor, m.chatPaneWidth())
			}

			frame := m.projectFrame()
			if frame.WidthClass != size.widthClass {
				t.Fatalf("frame width class = %d, want %d",
					frame.WidthClass, size.widthClass)
			}
			if frame.Cursor == nil ||
				!frame.Footer.Rect.Contains(frame.Cursor.X, frame.Cursor.Y) {
				t.Fatalf("frame cursor %#v outside footer %+v",
					frame.Cursor, frame.Footer.Rect)
			}
			footerRows := strings.Split(frame.Footer.Content, "\n")
			if len(footerRows) < len(searchRows) {
				t.Fatalf("projected footer omitted search owner:\n%s",
					ansi.Strip(frame.Footer.Content))
			}
			projectedSearchRows := footerRows[len(footerRows)-len(searchRows):]
			if got, want := strings.Join(projectedSearchRows, "\n"), searchView; got != want {
				t.Fatalf("projected two-row search owner changed:\n got %q\nwant %q",
					ansi.Strip(got), ansi.Strip(want))
			}

			view := m.View()
			if view.Cursor == nil ||
				!frame.SafeScreen.Contains(view.Cursor.X, view.Cursor.Y) ||
				!frame.Footer.Rect.Contains(view.Cursor.X, view.Cursor.Y) {
				t.Fatalf("full View cursor %#v outside safe footer geometry: safe=%+v footer=%+v",
					view.Cursor, frame.SafeScreen, frame.Footer.Rect)
			}
			if got := lipgloss.Height(view.Content); got > m.height {
				t.Fatalf("full View height = %d, terminal = %d", got, m.height)
			}
			for row, line := range strings.Split(view.Content, "\n") {
				if got := lipgloss.Width(line); got > m.width {
					t.Fatalf("full View row %d width = %d, terminal = %d",
						row, got, m.width)
				}
			}
			plain := ansi.Strip(view.Content)
			if !strings.Contains(plain, "界e\u0301🙂") {
				t.Fatalf("full View omitted Unicode query:\n%s", plain)
			}
			if size.ascii {
				for _, forbidden := range []string{"…", "·", "↑", "↓"} {
					if strings.Contains(ansi.Strip(searchView), forbidden) {
						t.Fatalf("ASCII search owner emitted %q:\n%s",
							forbidden, ansi.Strip(searchView))
					}
				}
			}
			assertNoLiteralANSIEscapeFragments(t, view.Content)
		})
	}
}

func TestTranscriptSearchUnicodeRowsPreserveMatchAndCellGeometry(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 64, Height: 20})
	m = updated.(*Model)
	entry := settledSearchEntry(
		"search-unicode-geometry",
		"search-unicode-geometry-turn",
		"assistant",
		"wide 界界 marker\ncombining e\u0301 marker\nemoji 👩🏽‍💻 marker",
	)
	entry.RenderedContent = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#b48ead")).
		Render(entry.Content)
	installSearchTranscript(t, m, []ChatEntry{entry}, nil)
	baseline, _ := m.transcriptPaint.document.materializeRows(
		0,
		m.transcriptPaint.document.totalRows,
	)
	_ = m.openTranscriptSearch()

	visitedRows := make(map[int]struct{}, 3)
	for _, test := range []struct {
		name      string
		query     string
		wantCells int
	}{
		{name: "wide", query: "界界", wantCells: 4},
		{name: "combining", query: "e\u0301", wantCells: 1},
		{name: "emoji", query: "👩🏽‍💻", wantCells: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			setTranscriptSearchQuery(t, m, test.query, true)
			state := m.transcriptSearch
			if got := len(state.matches); got != 1 {
				t.Fatalf("query %q matches = %d, want 1", test.query, got)
			}
			match := state.matches[0]
			if match.StartByte < 0 || match.EndByte > len(match.RenderedText) ||
				match.StartByte >= match.EndByte {
				t.Fatalf("query %q byte bounds = [%d,%d) for %d bytes",
					test.query, match.StartByte, match.EndByte, len(match.RenderedText))
			}
			if got := match.RenderedText[match.StartByte:match.EndByte]; got != test.query {
				t.Fatalf("query %q matched byte slice %q", test.query, got)
			}
			if got := lipgloss.Width(test.query); got != test.wantCells {
				t.Fatalf("query %q width = %d, want %d",
					test.query, got, test.wantCells)
			}
			if state.activeRow != match.AbsoluteRow {
				t.Fatalf("query %q active row = %d, want %d",
					test.query, state.activeRow, match.AbsoluteRow)
			}
			visitedRows[match.AbsoluteRow] = struct{}{}

			staged := stagedTranscriptSearchRow(t, m, match.AbsoluteRow)
			original := baseline[match.AbsoluteRow]
			if staged == original ||
				ansi.Strip(staged) != ansi.Strip(original) ||
				lipgloss.Width(staged) != lipgloss.Width(original) ||
				!strings.Contains(ansi.Strip(staged), test.query) {
				t.Fatalf(
					"query %q changed Unicode row geometry/content:\n got %q (%d)\nwant %q (%d)",
					test.query,
					ansi.Strip(staged),
					lipgloss.Width(staged),
					ansi.Strip(original),
					lipgloss.Width(original),
				)
			}
			view := m.View()
			frame := m.projectFrame()
			if view.Cursor == nil ||
				!frame.Footer.Rect.Contains(view.Cursor.X, view.Cursor.Y) {
				t.Fatalf("query %q cursor %#v outside footer %+v",
					test.query, view.Cursor, frame.Footer.Rect)
			}
			assertNoLiteralANSIEscapeFragments(t, view.Content)
		})
	}
	if len(visitedRows) != 3 {
		t.Fatalf("Unicode fixtures resolved to %d rows, want 3: %#v",
			len(visitedRows), visitedRows)
	}

	_ = m.closeTranscriptSearch(true)
	m.syncTranscriptPaintWindow()
	for row := range visitedRows {
		if got := stagedTranscriptSearchRow(t, m, row); got != baseline[row] {
			t.Fatalf("Unicode row %d retained highlight after close", row)
		}
	}
}

func stagedTranscriptSearchRow(t *testing.T, m *Model, absoluteRow int) string {
	t.Helper()
	m.syncTranscriptPaintWindow()
	localRow := absoluteRow - m.transcriptPaint.windowStart
	rows := strings.Split(m.viewport.GetContent(), "\n")
	if localRow < 0 || localRow >= len(rows) {
		t.Fatalf("absolute row %d outside staged window [%d,%d)",
			absoluteRow,
			m.transcriptPaint.windowStart,
			m.transcriptPaint.windowEnd,
		)
	}
	return rows[localRow]
}

func assertTranscriptSearchHighlightRows(
	t *testing.T,
	m *Model,
	baseline []string,
	rowA int,
	rowB int,
	activeRow int,
) {
	t.Helper()
	for _, row := range []int{rowA, rowB} {
		staged := stagedTranscriptSearchRow(t, m, row)
		original := baseline[row]
		if got, want := ansi.Strip(staged), ansi.Strip(original); got != want {
			t.Fatalf("row %d content changed:\n got %q\nwant %q", row, got, want)
		}
		if got, want := lipgloss.Width(staged), lipgloss.Width(original); got != want {
			t.Fatalf("row %d width = %d, want %d", row, got, want)
		}
		if row == activeRow && staged == original {
			t.Fatalf("active row %d has no highlight", row)
		}
		if row != activeRow && staged != original {
			t.Fatalf("inactive row %d retained ghost styling", row)
		}
		assertNoLiteralANSIEscapeFragments(t, staged)
	}
}

func assertNoLiteralANSIEscapeFragments(t *testing.T, rendered string) {
	t.Helper()
	plain := ansi.Strip(rendered)
	for _, fragment := range []string{
		"\x1b",
		"[0m",
		"[1m",
		"[4m",
		"[38;",
		"[48;",
	} {
		if strings.Contains(plain, fragment) {
			t.Fatalf("render exposed literal ANSI fragment %q in %q", fragment, plain)
		}
	}
}
