package ui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func diffViewerTestFiles() []DiffFileProjection {
	firstHunk := DiffHunk{OldStart: 1, OldCount: 2, NewStart: 1, NewCount: 2}
	secondHunk := DiffHunk{OldStart: 20, OldCount: 1, NewStart: 20, NewCount: 2}
	otherHunk := DiffHunk{OldStart: 5, OldCount: 1, NewStart: 5, NewCount: 1}
	return []DiffFileProjection{
		{
			ID: "file-one", DisplayPath: "internal/one.go", Revision: 7,
			Lines: []DiffLine{
				{Kind: DiffHunkHeader, Content: "@@ stale @@", Hunk: &firstHunk},
				{Kind: DiffRemoved, Content: "old alpha", OldLine: 1},
				{Kind: DiffAdded, Content: "needle alpha", NewLine: 1},
				{Kind: DiffContext, Content: "context", OldLine: 2, NewLine: 2},
				{Kind: DiffHunkHeader, Content: "@@ stale @@", Hunk: &secondHunk},
				{Kind: DiffContext, Content: "needle beta", OldLine: 20, NewLine: 20},
				{Kind: DiffAdded, Content: "new beta", NewLine: 21},
			},
		},
		{
			ID: "file-two", DisplayPath: "internal/two.go", Revision: 3,
			Lines: []DiffLine{
				{Kind: DiffHunkHeader, Content: "@@ stale @@", Hunk: &otherHunk},
				{Kind: DiffRemoved, Content: "second old", OldLine: 5},
				{Kind: DiffAdded, Content: "second new", NewLine: 5},
			},
		},
	}
}

func newDiffViewerForTest(width, height int) *DiffViewer {
	return NewDiffViewer(
		BlockID("block-diff-origin"),
		diffViewerTestFiles(),
		DiffViewerOptions{Width: width, Height: height, IsDark: true, GlyphProfile: GlyphUnicode},
	)
}

func TestDiffViewerModalGeometryIsExactAndClamped(t *testing.T) {
	tests := []struct {
		width, height int
		wantWidth     int
		wantHeight    int
	}{
		{width: 30, height: 12, wantWidth: 30, wantHeight: 12},
		{width: 80, height: 24, wantWidth: 72, wantHeight: 21},
		{width: 160, height: 48, wantWidth: 144, wantHeight: 43},
	}

	for _, test := range tests {
		viewer := newDiffViewerForTest(test.width, test.height)
		layout := viewer.Layout()
		if got := layout.OuterRect.Width(); got != test.wantWidth {
			t.Errorf("%dx%d outer width = %d, want %d", test.width, test.height, got, test.wantWidth)
		}
		if got := layout.OuterRect.Height(); got != test.wantHeight {
			t.Errorf("%dx%d outer height = %d, want %d", test.width, test.height, got, test.wantHeight)
		}
		for name, rect := range map[string]CellRect{
			"outer": layout.OuterRect, "inner": layout.InnerRect,
			"header": layout.HeaderRect, "body": layout.BodyRect, "footer": layout.FooterRect,
		} {
			if rect.MinX < layout.ScreenRect.MinX || rect.MaxX > layout.ScreenRect.MaxX ||
				rect.MinY < layout.ScreenRect.MinY || rect.MaxY > layout.ScreenRect.MaxY {
				t.Errorf("%dx%d %s rect %+v escapes screen %+v", test.width, test.height, name, rect, layout.ScreenRect)
			}
		}

		rendered := viewer.View()
		lines := strings.Split(rendered, "\n")
		if len(lines) != test.wantHeight {
			t.Errorf("%dx%d painted %d rows, want %d", test.width, test.height, len(lines), test.wantHeight)
		}
		for row, line := range lines {
			if got := lipgloss.Width(line); got != test.wantWidth {
				t.Errorf("%dx%d row %d width = %d, want %d", test.width, test.height, row, got, test.wantWidth)
				break
			}
		}
	}
}

func TestDiffViewerSplitThresholdUsesDynamicDigitsAndReadablePanes(t *testing.T) {
	tests := []struct {
		digits    int
		threshold int
	}{
		{digits: 1, threshold: 117},
		{digits: 3, threshold: 121},
		{digits: 6, threshold: 127},
	}

	for _, test := range tests {
		below := resolveDiffViewerSplitPlan(test.threshold-1, test.digits, test.digits)
		if below.available {
			t.Errorf("%d-digit split unexpectedly fit at %d columns", test.digits, test.threshold-1)
		}
		at := resolveDiffViewerSplitPlan(test.threshold, test.digits, test.digits)
		if !at.available {
			t.Fatalf("%d-digit split did not fit at exact threshold %d: %s", test.digits, test.threshold, at.reason)
		}
		if at.oldCodeWidth < diffViewerMinimumSplitCodeColumns ||
			at.newCodeWidth < diffViewerMinimumSplitCodeColumns {
			t.Errorf("%d-digit panes = %d/%d, each must be at least 52", test.digits, at.oldCodeWidth, at.newCodeWidth)
		}
		above := resolveDiffViewerSplitPlan(test.threshold+1, test.digits, test.digits)
		if above.oldCodeWidth+above.newCodeWidth !=
			2*diffViewerMinimumSplitCodeColumns+1 {
			t.Errorf("%d-digit odd residual panes = %d/%d", test.digits, above.oldCodeWidth, above.newCodeWidth)
		}
	}

	if plan := resolveDiffViewerUnifiedPlan(43, 1, 1); plan.available {
		t.Fatal("unified unexpectedly fit below its 40-column code floor")
	}
	if plan := resolveDiffViewerUnifiedPlan(44, 1, 1); !plan.available ||
		plan.codeWidth != 40 || plan.showNumbers {
		t.Fatalf("unified numberless threshold = %+v, want 40 code columns", plan)
	}
	if plan := resolveDiffViewerUnifiedPlan(51, 1, 1); !plan.available ||
		plan.codeWidth != 40 || !plan.showNumbers {
		t.Fatalf("unified numbered threshold = %+v, want numbered 40-column code", plan)
	}
}

func TestDiffViewerPreferredSplitFallsBackAndRestoresAcrossResize(t *testing.T) {
	viewer := newDiffViewerForTest(160, 48)
	if !viewer.Layout().SplitAvailable {
		t.Fatalf("wide fixture did not admit split: %+v", viewer.Layout())
	}
	_, _ = viewer.Update(charKey('s'))
	if viewer.PreferredMode() != DiffViewerSplit || viewer.EffectiveMode() != DiffViewerSplit {
		t.Fatalf("wide toggle = preferred %s effective %s", viewer.PreferredMode(), viewer.EffectiveMode())
	}

	viewer.SetSize(80, 24)
	if viewer.PreferredMode() != DiffViewerSplit || viewer.EffectiveMode() != DiffViewerUnified {
		t.Fatalf("narrow fallback = preferred %s effective %s", viewer.PreferredMode(), viewer.EffectiveMode())
	}
	if viewer.Layout().DisabledReason == "" || !strings.Contains(viewer.Layout().DisabledReason, "52-column") {
		t.Fatalf("narrow fallback reason = %q", viewer.Layout().DisabledReason)
	}

	viewer.SetSize(160, 48)
	if viewer.PreferredMode() != DiffViewerSplit || viewer.EffectiveMode() != DiffViewerSplit {
		t.Fatalf("wide restore = preferred %s effective %s", viewer.PreferredMode(), viewer.EffectiveMode())
	}
}

func TestDiffViewerIntralineSpansAreConservativeAndGraphemeSafe(t *testing.T) {
	tests := []struct {
		name           string
		before         string
		after          string
		wantOldChanged string
		wantNewChanged string
	}{
		{
			name:           "replacement with shared prefix",
			before:         "const mode = oldValue",
			after:          "const mode = newValue",
			wantOldChanged: "old",
			wantNewChanged: "new",
		},
		{
			name:           "insertion highlights only the inserted side",
			before:         "abc",
			after:          "abXc",
			wantNewChanged: "X",
		},
		{
			name:           "emoji modifier remains one grapheme",
			before:         "status 👍🏽 done",
			after:          "status 👎🏽 done",
			wantOldChanged: "👍🏽",
			wantNewChanged: "👎🏽",
		},
		{
			name:   "unrelated lines keep line-level styling",
			before: "alpha",
			after:  "omega",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldSpan, newSpan := diffViewerPairIntralineSpans(test.before, test.after)
			_, oldChanged, _ := diffViewerSplitGraphemeRange(
				test.before, oldSpan.start, oldSpan.end,
			)
			_, newChanged, _ := diffViewerSplitGraphemeRange(
				test.after, newSpan.start, newSpan.end,
			)
			if oldChanged != test.wantOldChanged || newChanged != test.wantNewChanged {
				t.Fatalf(
					"changed spans = old %q new %q, want old %q new %q",
					oldChanged,
					newChanged,
					test.wantOldChanged,
					test.wantNewChanged,
				)
			}
		})
	}

	tooLong := strings.Repeat("a", diffViewerMaximumIntralineClusters+1)
	oldSpan, newSpan := diffViewerPairIntralineSpans(tooLong, tooLong+"b")
	if oldSpan.valid() || newSpan.valid() {
		t.Fatal("oversized lines should fall back to bounded line-level styling")
	}
}

func TestDiffViewerIntralineRenderingPreservesPlainTextAndWrapGeometry(t *testing.T) {
	content := "prefix oldValue suffix"
	span := diffViewerIntralineSpan{start: 7, end: 15}
	chunks := diffViewerRangedContentChunks(content, 10)
	if len(chunks) < 2 {
		t.Fatalf("fixture did not wrap: %#v", chunks)
	}

	var rendered strings.Builder
	for _, chunk := range chunks {
		styled := renderDiffViewerChunk(chunk, span, lipgloss.NewStyle())
		if lipgloss.Width(styled) != lipgloss.Width(chunk.text) {
			t.Fatalf(
				"styled chunk width = %d, want %d",
				lipgloss.Width(styled),
				lipgloss.Width(chunk.text),
			)
		}
		rendered.WriteString(styled)
	}
	if plain := ansi.Strip(rendered.String()); plain != content {
		t.Fatalf("styled plain text = %q, want %q", plain, content)
	}
	if !strings.Contains(rendered.String(), "\x1b[") {
		t.Fatal("changed span did not receive a distinct terminal attribute")
	}
}

func TestDiffViewerWrappedRowsKeepSemanticAnchorsAndBlankNumberGutters(t *testing.T) {
	hunk := DiffHunk{OldStart: 123, OldCount: 1, NewStart: 456, NewCount: 1}
	viewer := NewDiffViewer(
		"block-wrap",
		[]DiffFileProjection{{
			ID: "wrapped", DisplayPath: "wrapped.go", Revision: 1,
			Lines: []DiffLine{
				{Kind: DiffHunkHeader, Hunk: &hunk},
				{
					Kind: DiffContext, OldLine: 123, NewLine: 456,
					Content: strings.Repeat("long-content-", 20),
				},
			},
		}},
		DiffViewerOptions{Width: 80, Height: 24, GlyphProfile: GlyphUnicode},
	)

	var wrapped []diffViewerRow
	for _, row := range viewer.rows {
		if row.line == 1 {
			wrapped = append(wrapped, row)
		}
	}
	if len(wrapped) < 2 {
		t.Fatalf("long logical line painted %d rows, want wrapping", len(wrapped))
	}
	for continuation, row := range wrapped {
		if row.line != 1 || row.continuation != continuation {
			t.Fatalf("wrapped anchor %d = line %d continuation %d", continuation, row.line, row.continuation)
		}
		if continuation > 0 {
			plain := ansi.Strip(row.rendered)
			prefixWidth := viewer.layout.OldDigits + viewer.layout.NewDigits +
				diffViewerUnifiedChromeColumns
			prefix := truncateDisplay(plain, prefixWidth)
			if strings.Contains(prefix, "123") || strings.Contains(prefix, "456") {
				t.Fatalf("continuation gutter repeated a logical number: %q", prefix)
			}
		}
	}

	viewer.jumpToLine(1, DiffViewerSideUnified)
	viewer.moveCursor(1)
	before := viewer.CurrentAnchor()
	viewer.SetSize(120, 30)
	after := viewer.CurrentAnchor()
	if before.FileID != after.FileID || before.LineIndex != after.LineIndex {
		t.Fatalf("resize moved semantic cursor from %+v to %+v", before, after)
	}
}

func TestDiffViewerNavigationSearchAndTypedCopyEvents(t *testing.T) {
	origin := BlockID("block-diff-origin")
	viewer := newDiffViewerForTest(160, 48)

	_, _ = viewer.Update(charKey(']'))
	if got := viewer.CurrentAnchor().LineIndex; got != 4 {
		t.Fatalf("next hunk selected line %d, want 4", got)
	}
	_, _ = viewer.Update(tabKey())
	if viewer.CurrentFileIndex() != 1 {
		t.Fatalf("tab selected file %d, want 1", viewer.CurrentFileIndex())
	}
	_, _ = viewer.Update(shiftTabKey())
	if viewer.CurrentFileIndex() != 0 {
		t.Fatalf("shift-tab selected file %d, want 0", viewer.CurrentFileIndex())
	}

	_, _ = viewer.Update(charKey('/'))
	if !viewer.searching {
		t.Fatal("/ did not focus search")
	}
	_, _ = viewer.Update(tea.PasteMsg{Content: "needle"})
	_, _ = viewer.Update(enterKey())
	if got := viewer.CurrentAnchor().LineIndex; got != 2 {
		t.Fatalf("first search match selected line %d, want 2", got)
	}
	_, _ = viewer.Update(charKey('n'))
	if got := viewer.CurrentAnchor().LineIndex; got != 5 {
		t.Fatalf("next match selected line %d, want 5", got)
	}
	_, _ = viewer.Update(charKey('N'))
	if got := viewer.CurrentAnchor().LineIndex; got != 2 {
		t.Fatalf("previous match selected line %d, want 2", got)
	}

	lineEvent, _ := viewer.Update(charKey('c'))
	if lineEvent.Kind != DiffViewerEventCopyLine || lineEvent.Text != "+needle alpha" {
		t.Fatalf("line copy event = %+v", lineEvent)
	}
	if lineEvent.BlockID != origin {
		t.Fatalf("line copy block = %q, want %q", lineEvent.BlockID, origin)
	}
	if strings.Contains(lineEvent.Text, "\x1b[") || strings.Contains(lineEvent.Text, "  1") {
		t.Fatalf("line copy leaked rendered gutter/ANSI: %q", lineEvent.Text)
	}

	hunkEvent, _ := viewer.Update(charKey('C'))
	if hunkEvent.Kind != DiffViewerEventCopyHunk ||
		!strings.Contains(hunkEvent.Text, "@@ -1,2 +1,2 @@") ||
		!strings.Contains(hunkEvent.Text, "+needle alpha") {
		t.Fatalf("hunk copy event = %+v", hunkEvent)
	}
	if hunkEvent.BlockID != origin || hunkEvent.FileID != lineEvent.FileID {
		t.Fatalf("copy events lost authority: line=%+v hunk=%+v", lineEvent, hunkEvent)
	}

	pathEvent, _ := viewer.Update(charKey('p'))
	if pathEvent.Kind != DiffViewerEventCopyPath || pathEvent.Text != "internal/one.go" {
		t.Fatalf("path copy event = %+v", pathEvent)
	}
	if pathEvent.BlockID != origin {
		t.Fatalf("path copy block = %q, want %q", pathEvent.BlockID, origin)
	}
}

func TestDiffViewerDisabledEventsExplainPendingAndTruncatedState(t *testing.T) {
	hunk := DiffHunk{OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1}
	pending := NewDiffViewer(
		"block-pending",
		[]DiffFileProjection{{
			ID: "pending", DisplayPath: "pending.go", Revision: 0,
			Lines: []DiffLine{{Kind: DiffAdded, Content: "not authoritative", NewLine: 1}},
		}},
		DiffViewerOptions{Width: 120, Height: 30},
	)
	event, _ := pending.Update(charKey('c'))
	if event.Kind != DiffViewerEventUnavailable ||
		event.Action != DiffViewerActionCopyLine ||
		!strings.Contains(strings.ToLower(event.DisabledReason), "loading") ||
		event.Text != "" {
		t.Fatalf("pending line event = %+v", event)
	}
	pending.SetFiles([]DiffFileProjection{{
		ID: "pending", DisplayPath: "pending.go", Revision: 1,
		Lines: []DiffLine{{Kind: DiffAdded, Content: "now authoritative", NewLine: 1}},
	}})
	event, _ = pending.Update(charKey('c'))
	if event.Kind != DiffViewerEventCopyLine || event.Text != "+now authoritative" {
		t.Fatalf("completed projection did not replace pending state: %+v", event)
	}

	truncated := NewDiffViewer(
		"block-truncated",
		[]DiffFileProjection{{
			ID: "truncated", DisplayPath: "truncated.go", Revision: 2, Truncated: true,
			Lines: []DiffLine{
				{Kind: DiffHunkHeader, Hunk: &hunk},
				{Kind: DiffAdded, Content: "partial", NewLine: 1},
			},
		}},
		DiffViewerOptions{Width: 120, Height: 30},
	)
	event, _ = truncated.Update(charKey('C'))
	if event.Kind != DiffViewerEventUnavailable ||
		event.Action != DiffViewerActionCopyHunk ||
		!strings.Contains(strings.ToLower(event.DisabledReason), "truncated") ||
		event.Text != "" || !event.Truncated {
		t.Fatalf("truncated hunk event = %+v", event)
	}
}

func TestDiffViewerSanitizesDisplayPathAndNeverReturnsAbsolutePath(t *testing.T) {
	viewer := NewDiffViewer(
		"block-path",
		[]DiffFileProjection{{
			ID:          "bad\x1b[31m-id",
			DisplayPath: "\x1b[31m/Users/person/private/secret.go\x1b[0m\n",
			Revision:    1,
			Lines:       []DiffLine{{Kind: DiffAdded, Content: "safe", NewLine: 1}},
		}},
		DiffViewerOptions{Width: 120, Height: 30},
	)
	if got := viewer.files[0].DisplayPath; got != "secret.go" {
		t.Fatalf("sanitized display path = %q, want relative basename", got)
	}
	if strings.Contains(viewer.View(), "/Users/") || strings.Contains(viewer.View(), "\x1b[31m/Users") {
		t.Fatalf("view leaked absolute/control-bearing path:\n%s", viewer.View())
	}
	event, _ := viewer.Update(charKey('p'))
	if event.Text != "secret.go" || strings.HasPrefix(event.Text, "/") ||
		strings.ContainsAny(event.Text, "\r\n") {
		t.Fatalf("path event leaked unsafe path: %+v", event)
	}

	traversal := sanitizeDiffViewerDisplayPath("../../outside/file.go")
	if traversal != "file.go" {
		t.Fatalf("traversal display path = %q, want basename", traversal)
	}
	windows := sanitizeDiffViewerDisplayPath(`C:\Users\person\secret.go`)
	if windows != "secret.go" {
		t.Fatalf("Windows absolute display path = %q, want basename", windows)
	}
}

func TestDiffViewerCopyIsTerminalSafeUTF8AndBounded(t *testing.T) {
	content := strings.Repeat("界", diffViewerMaximumCopyBytes)
	viewer := NewDiffViewer(
		"block-copy-bound",
		[]DiffFileProjection{{
			ID: "large", DisplayPath: "large.go", Revision: 1,
			Lines: []DiffLine{{Kind: DiffAdded, Content: "\x1b[31m" + content, NewLine: 1}},
		}},
		DiffViewerOptions{Width: 120, Height: 30},
	)
	event, _ := viewer.Update(charKey('c'))
	if len(event.Text) > diffViewerMaximumCopyBytes {
		t.Fatalf("copy bytes = %d, max %d", len(event.Text), diffViewerMaximumCopyBytes)
	}
	if !utf8.ValidString(event.Text) || strings.Contains(event.Text, "\x1b") {
		t.Fatalf("copy is not terminal-safe UTF-8")
	}
}

func TestDiffViewerNoColorAndGlyphProfileAreIndependent(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })

	asciiFiles := diffViewerTestFiles()[:1]
	asciiFiles[0].Lines = append(asciiFiles[0].Lines, DiffLine{
		Kind: DiffEllipsis, Content: "… unchanged lines",
	})
	asciiViewer := NewDiffViewer(
		"block-ascii",
		asciiFiles,
		DiffViewerOptions{Width: 120, Height: 30, IsDark: true, GlyphProfile: GlyphASCII},
	)
	asciiView := asciiViewer.View()
	if strings.Contains(asciiView, "\x1b[38;") || strings.Contains(asciiView, "\x1b[48;") {
		t.Fatalf("NO_COLOR viewer emitted ANSI color: %q", asciiView)
	}
	if !strings.HasPrefix(asciiView, "+") ||
		strings.ContainsAny(asciiView, "╭╮╰╯│●○…·") {
		t.Fatalf("ASCII glyph profile leaked Unicode chrome:\n%s", asciiView)
	}

	unicodeViewer := NewDiffViewer(
		"block-unicode",
		diffViewerTestFiles()[:1],
		DiffViewerOptions{Width: 120, Height: 30, IsDark: true, GlyphProfile: GlyphUnicode},
	)
	unicodeView := unicodeViewer.View()
	if strings.Contains(unicodeView, "\x1b[38;") || strings.Contains(unicodeView, "\x1b[48;") {
		t.Fatalf("NO_COLOR Unicode viewer emitted ANSI color: %q", unicodeView)
	}
	if !strings.HasPrefix(unicodeView, "╭") {
		t.Fatalf("NO_COLOR unexpectedly forced ASCII glyphs:\n%s", unicodeView)
	}
}

func TestDiffViewerSearchCursorStaysInsideFrameAndEscapeIsLayered(t *testing.T) {
	viewer := newDiffViewerForTest(80, 24)
	_, _ = viewer.Update(charKey('/'))
	view, cursor := viewer.ViewWithCursor()
	if cursor == nil {
		t.Fatal("focused search omitted hardware cursor")
	}
	layout := viewer.Layout()
	if cursor.X < 0 || cursor.X >= layout.OuterRect.Width() ||
		cursor.Y < 0 || cursor.Y >= layout.OuterRect.Height() {
		t.Fatalf("search cursor %+v escapes frame %dx%d", cursor, layout.OuterRect.Width(), layout.OuterRect.Height())
	}
	if len(strings.Split(view, "\n")) != layout.OuterRect.Height() {
		t.Fatal("search row changed exact modal height")
	}
	if !viewer.Back() || viewer.Back() {
		t.Fatal("Escape layering did not consume search once then release modal close")
	}
}

func TestDiffViewerWheelRoutesOnceInsideBodyAndKeepsCopyTargetVisible(t *testing.T) {
	lines := make([]DiffLine, 80)
	for index := range lines {
		lines[index] = DiffLine{
			Kind: DiffContext, Content: "row-" + strconv.Itoa(index),
			OldLine: index + 1, NewLine: index + 1,
		}
	}
	viewer := NewDiffViewer(
		"block-wheel",
		[]DiffFileProjection{{
			ID: "wheel", DisplayPath: "wheel.go", Revision: 1, Lines: lines,
		}},
		DiffViewerOptions{Width: 80, Height: 24},
	)
	body := viewer.Layout().BodyRect
	beforeAnchor := viewer.CurrentAnchor()

	outside := tea.MouseWheelMsg{
		X: body.MinX, Y: max(viewer.Layout().OuterRect.MinY, body.MinY-1),
		Button: tea.MouseWheelDown,
	}
	event, _ := viewer.Update(outside)
	if event.Kind != DiffViewerEventNone || viewer.viewport.YOffset() != 0 ||
		viewer.CurrentAnchor() != beforeAnchor {
		t.Fatalf("outside wheel changed state: event=%+v offset=%d anchor=%+v",
			event, viewer.viewport.YOffset(), viewer.CurrentAnchor())
	}

	wheel := tea.MouseWheelMsg{
		X: body.MinX, Y: body.MinY, Button: tea.MouseWheelDown,
	}
	expected := viewer.viewport
	expected, _ = expected.Update(wheel)
	event, _ = viewer.Update(wheel)
	if event.Kind != DiffViewerEventNone {
		t.Fatalf("wheel emitted parent action: %+v", event)
	}
	if got, want := viewer.viewport.YOffset(), expected.YOffset(); got != want {
		t.Fatalf("one wheel moved to offset %d, one Bubbles update wants %d", got, want)
	}
	selected := viewer.currentRowIndex()
	top := viewer.viewport.YOffset()
	if selected < top || selected >= top+viewer.viewport.Height() {
		t.Fatalf("wheel copy target row %d is outside visible rows [%d,%d)",
			selected, top, top+viewer.viewport.Height())
	}
	copyEvent, _ := viewer.Update(charKey('c'))
	if copyEvent.Kind != DiffViewerEventCopyLine ||
		copyEvent.LineIndex != viewer.CurrentAnchor().LineIndex {
		t.Fatalf("copy target does not match visible selection: event=%+v anchor=%+v",
			copyEvent, viewer.CurrentAnchor())
	}

	offset := viewer.viewport.YOffset()
	anchor := viewer.CurrentAnchor()
	_, _ = viewer.Update(tea.MouseWheelMsg{
		X: body.MaxX, Y: body.MinY, Button: tea.MouseWheelDown,
	})
	if viewer.viewport.YOffset() != offset || viewer.CurrentAnchor() != anchor {
		t.Fatalf("half-open right boundary changed offset/anchor: offset=%d anchor=%+v",
			viewer.viewport.YOffset(), viewer.CurrentAnchor())
	}
}

func TestDiffViewerPointerSelectsExactSplitSideAndIgnoresChrome(t *testing.T) {
	viewer := newDiffViewerForTest(160, 48)
	_, _ = viewer.Update(charKey('s'))
	layout := viewer.Layout()
	if layout.EffectiveMode != DiffViewerSplit {
		t.Fatalf("wide fixture did not enter split mode: %+v", layout)
	}

	targetRow := -1
	for index, row := range viewer.rows {
		if row.line == 1 && row.peer == 2 {
			targetRow = index
			break
		}
	}
	if targetRow < 0 {
		t.Fatal("paired removed/added row was not projected")
	}
	y := layout.BodyRect.MinY + targetRow - viewer.viewport.YOffset()
	oldPaneWidth := layout.OldDigits + diffViewerSplitPaneChromeColumns + layout.OldCodeWidth
	separatorX := layout.BodyRect.MinX + oldPaneWidth
	newPaneX := separatorX + diffViewerSplitGapColumns

	event, _ := viewer.Update(tea.MouseClickMsg{
		X: newPaneX, Y: y, Button: tea.MouseLeft,
	})
	anchor := viewer.CurrentAnchor()
	if event.Kind != DiffViewerEventNone ||
		anchor.Side != DiffViewerSideNew || anchor.LineIndex != 2 {
		t.Fatalf("new-pane click selected %+v and emitted %+v", anchor, event)
	}

	before := anchor
	_, _ = viewer.Update(tea.MouseClickMsg{
		X: separatorX, Y: y, Button: tea.MouseLeft,
	})
	if got := viewer.CurrentAnchor(); got != before {
		t.Fatalf("separator chrome changed selection from %+v to %+v", before, got)
	}

	_, _ = viewer.Update(tea.MouseClickMsg{
		X: layout.BodyRect.MinX, Y: y, Button: tea.MouseLeft,
	})
	anchor = viewer.CurrentAnchor()
	if anchor.Side != DiffViewerSideOld || anchor.LineIndex != 1 {
		t.Fatalf("old-pane click selected %+v", anchor)
	}
}

func TestDiffViewerBodyClickFinishesSearchWithoutRowDrift(t *testing.T) {
	viewer := newDiffViewerForTest(80, 24)
	_, _ = viewer.Update(charKey('/'))
	if !viewer.searching {
		t.Fatal("search did not start")
	}
	layout := viewer.Layout()
	targetRow := min(2, len(viewer.rows)-1)
	want := viewer.rows[targetRow]
	_, _ = viewer.Update(tea.MouseClickMsg{
		X:      layout.BodyRect.MinX,
		Y:      layout.BodyRect.MinY + targetRow - viewer.viewport.YOffset(),
		Button: tea.MouseLeft,
	})
	if viewer.searching {
		t.Fatal("body click retained search editing")
	}
	anchor := viewer.CurrentAnchor()
	if !diffViewerRowContainsLine(want, anchor.LineIndex) {
		t.Fatalf("body click drifted from row %+v to anchor %+v", want, anchor)
	}
}

func TestDiffViewerThemeAndReducedMotionPreserveAnchorSelectionAndGeometry(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	viewer := newDiffViewerForTest(160, 48)
	_, _ = viewer.Update(charKey(']'))
	_, _ = viewer.Update(charKey('/'))
	_, _ = viewer.Update(tea.PasteMsg{Content: "needle"})
	beforeAnchor := viewer.CurrentAnchor()
	beforeLayout := viewer.Layout()
	beforeValue := viewer.search.Value()
	if !viewer.search.Focused() || !viewer.search.Styles().Cursor.Blink {
		t.Fatal("theme fixture search is not focused with motion enabled")
	}

	viewer.SetTheme(false, defaultThemeID)
	if got := viewer.CurrentAnchor(); got != beforeAnchor {
		t.Fatalf("theme changed anchor from %+v to %+v", beforeAnchor, got)
	}
	if got := viewer.Layout(); got != beforeLayout {
		t.Fatalf("theme changed geometry/mode:\nbefore %+v\nafter  %+v", beforeLayout, got)
	}
	if !viewer.search.Focused() || viewer.search.Value() != beforeValue {
		t.Fatalf("theme lost search focus/value: focused=%v value=%q",
			viewer.search.Focused(), viewer.search.Value())
	}

	viewer.SetReducedMotion(true)
	if viewer.search.Styles().Cursor.Blink {
		t.Fatal("reduced motion left the search cursor blinking")
	}
	if viewer.CurrentAnchor() != beforeAnchor || viewer.Layout() != beforeLayout ||
		!viewer.search.Focused() || viewer.search.Value() != beforeValue {
		t.Fatal("reduced motion changed anchor, geometry, selection, or search state")
	}

	viewer.SetTheme(true, defaultThemeID)
	if viewer.search.Styles().Cursor.Blink {
		t.Fatal("theme refresh re-enabled blink under reduced motion")
	}
	viewer.SetReducedMotion(false)
	if !viewer.search.Styles().Cursor.Blink {
		t.Fatal("disabling reduced motion did not restore search cursor blink")
	}
}

func TestDiffViewerSplitRowsUseExactBodyWidthAndSemanticAuthority(t *testing.T) {
	viewer := newDiffViewerForTest(160, 48)
	_, _ = viewer.Update(charKey('s'))
	if viewer.EffectiveMode() != DiffViewerSplit {
		t.Fatal("wide viewer did not enter split")
	}
	for index, row := range viewer.rows {
		if got := lipgloss.Width(row.rendered); got != viewer.Layout().BodyRect.Width() {
			t.Fatalf("split row %d width = %d, want body width %d", index, got, viewer.Layout().BodyRect.Width())
		}
	}

	origin := BlockID("block-diff-origin")
	for _, key := range []tea.KeyPressMsg{charKey('c'), charKey('C'), charKey('p')} {
		event, _ := viewer.Update(key)
		if event.BlockID != origin {
			t.Fatalf("%q event block = %q, want %q", key.String(), event.BlockID, origin)
		}
	}
}

func TestDiffViewerSplitContinuationAnchorsOnlyTheSideStillPainting(t *testing.T) {
	hunk := DiffHunk{OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1}
	viewer := NewDiffViewer(
		"block-split-wrap",
		[]DiffFileProjection{{
			ID: "split-wrap", DisplayPath: "split.go", Revision: 1,
			Lines: []DiffLine{
				{Kind: DiffHunkHeader, Hunk: &hunk},
				{Kind: DiffRemoved, Content: "short", OldLine: 1},
				{Kind: DiffAdded, Content: strings.Repeat("new-content-", 40), NewLine: 1},
			},
		}},
		DiffViewerOptions{Width: 160, Height: 48},
	)
	_, _ = viewer.Update(charKey('s'))
	if viewer.EffectiveMode() != DiffViewerSplit {
		t.Fatal("split wrap fixture did not admit split")
	}
	var continuation diffViewerRow
	found := false
	for _, row := range viewer.rows {
		if row.continuation > 0 {
			continuation = row
			found = true
			break
		}
	}
	if !found {
		t.Fatal("split fixture did not wrap")
	}
	if continuation.line != 2 || continuation.peer != -1 ||
		continuation.side != DiffViewerSideNew {
		t.Fatalf("new-only continuation anchor = %+v", continuation)
	}
}

func diffViewerLongLines(count int) []DiffLine {
	lines := make([]DiffLine, count)
	for index := range lines {
		lines[index] = DiffLine{
			Kind:    DiffContext,
			Content: fmt.Sprintf("row-%04d stable content", index),
			OldLine: index + 1,
			NewLine: index + 1,
		}
	}
	return lines
}

func TestDiffViewerRebuildPreservesCursorScreenRowAndOffset(t *testing.T) {
	viewer := NewDiffViewer(
		"block-stable-screen-row",
		[]DiffFileProjection{{
			ID: "stable", DisplayPath: "stable.go", Revision: 1,
			Lines: diffViewerLongLines(120),
		}},
		DiffViewerOptions{Width: 80, Height: 24, IsDark: true},
	)
	viewer.jumpToPhysicalRow(60)
	viewer.moveCursor(-5)

	beforeAnchor := viewer.CurrentAnchor()
	beforeOffset := viewer.viewport.YOffset()
	beforeScreenRow := viewer.currentRowIndex() - beforeOffset
	if beforeOffset <= 0 || beforeScreenRow <= 0 {
		t.Fatalf("fixture was not scrolled to an interior screen row: offset=%d row=%d",
			beforeOffset, beforeScreenRow)
	}

	viewer.SetTheme(false, defaultThemeID)
	if got := viewer.viewport.YOffset(); got != beforeOffset {
		t.Fatalf("theme rebuild offset = %d, want %d", got, beforeOffset)
	}
	if got := viewer.currentRowIndex() - viewer.viewport.YOffset(); got != beforeScreenRow {
		t.Fatalf("theme rebuild screen row = %d, want %d", got, beforeScreenRow)
	}

	viewer.SetReducedMotion(true)
	if got := viewer.viewport.YOffset(); got != beforeOffset {
		t.Fatalf("motion rebuild offset = %d, want %d", got, beforeOffset)
	}
	if got := viewer.currentRowIndex() - viewer.viewport.YOffset(); got != beforeScreenRow {
		t.Fatalf("motion rebuild screen row = %d, want %d", got, beforeScreenRow)
	}

	viewer.SetSize(100, 30)
	if got := viewer.currentRowIndex() - viewer.viewport.YOffset(); got != beforeScreenRow {
		t.Fatalf("larger resize screen row = %d, want %d", got, beforeScreenRow)
	}
	if got := viewer.CurrentAnchor(); got != beforeAnchor {
		t.Fatalf("rebuilds moved semantic anchor from %+v to %+v", beforeAnchor, got)
	}

	viewer.SetSize(80, 12)
	wantClampedRow := min(beforeScreenRow, viewer.viewport.Height()-1)
	if got := viewer.currentRowIndex() - viewer.viewport.YOffset(); got != wantClampedRow {
		t.Fatalf("smaller resize screen row = %d, want clamped %d", got, wantClampedRow)
	}
}

func TestDiffViewerRedundantPresentationSettersDoNotRebuild(t *testing.T) {
	viewer := NewDiffViewer(
		"block-noop-rebuild",
		[]DiffFileProjection{{
			ID: "noop", DisplayPath: "noop.go", Revision: 1,
			Lines: diffViewerLongLines(20),
		}},
		DiffViewerOptions{Width: 80, Height: 24, IsDark: true},
	)
	beforeRows := &viewer.rows[0]
	beforeContent := viewer.viewport.GetContent()

	viewer.SetSize(80, 24)
	viewer.SetScreenRect(NewCellRect(0, 0, 80, 24))
	viewer.SetTheme(true, defaultThemeID)
	viewer.SetReducedMotion(false)

	if got := &viewer.rows[0]; got != beforeRows {
		t.Fatal("idempotent presentation setters replaced cached rows")
	}
	if got := viewer.viewport.GetContent(); got != beforeContent {
		t.Fatal("idempotent presentation setters replaced cached content")
	}
}

func TestDiffViewerNavigationDoesNotRebuildCachedDocument(t *testing.T) {
	viewer := NewDiffViewer(
		"block-incremental-navigation",
		[]DiffFileProjection{{
			ID: "incremental", DisplayPath: "incremental.go", Revision: 1,
			Lines: diffViewerLongLines(200),
		}},
		DiffViewerOptions{Width: 80, Height: 24},
	)
	beforeRows := &viewer.rows[0]
	beforeContent := viewer.viewport.GetContent()
	body := viewer.Layout().BodyRect

	_, _ = viewer.Update(charKey('j'))
	_, _ = viewer.Update(charKey('k'))
	_, _ = viewer.Update(tea.MouseClickMsg{
		X: body.MinX, Y: body.MinY + 2, Button: tea.MouseLeft,
	})
	_, _ = viewer.Update(tea.MouseWheelMsg{
		X: body.MinX, Y: body.MinY, Button: tea.MouseWheelDown,
	})
	_, _ = viewer.Update(charKey('G'))
	_, _ = viewer.Update(charKey('c'))

	if got := &viewer.rows[0]; got != beforeRows {
		t.Fatal("navigation replaced the physical row projection")
	}
	if got := viewer.viewport.GetContent(); got != beforeContent {
		t.Fatal("navigation reset or repainted the cached viewport document")
	}
	for index, row := range viewer.rows {
		if got := ansi.Cut(ansi.Strip(row.rendered), 0, 1); got != glyphSet(viewer.glyphProfile).Unselected {
			t.Fatalf("cached row %d embeds selection marker %q", index, got)
		}
	}
}

func TestDiffViewerVisibleSelectionOverlaysExactUnifiedAndSplitCells(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })

	unified := newDiffViewerForTest(80, 24)
	unifiedBody := strings.Split(ansi.Strip(unified.renderVisibleBody()), "\n")
	if got := ansi.Cut(unifiedBody[0], 0, 1); got != glyphSet(GlyphUnicode).Selected {
		t.Fatalf("unified selected cell = %q", got)
	}
	unified.moveCursor(1)
	unifiedBody = strings.Split(ansi.Strip(unified.renderVisibleBody()), "\n")
	if got := ansi.Cut(unifiedBody[0], 0, 1); got != glyphSet(GlyphUnicode).Unselected {
		t.Fatalf("old unified row retained selection = %q", got)
	}
	if got := ansi.Cut(unifiedBody[1], 0, 1); got != glyphSet(GlyphUnicode).Selected {
		t.Fatalf("new unified selected cell = %q", got)
	}

	split := newDiffViewerForTest(160, 48)
	_, _ = split.Update(charKey('s'))
	if !split.jumpToLine(1, DiffViewerSideOld) {
		t.Fatal("could not select paired split row")
	}
	split.moveSplitSide(DiffViewerSideNew)
	rowIndex := split.currentRowIndex()
	screenRow := rowIndex - split.viewport.YOffset()
	rows := strings.Split(ansi.Strip(split.renderVisibleBody()), "\n")
	oldColumn := 0
	newColumn := split.layout.OldDigits +
		diffViewerSplitPaneChromeColumns +
		split.layout.OldCodeWidth +
		diffViewerSplitGapColumns
	if got := ansi.Cut(rows[screenRow], oldColumn, oldColumn+1); got != glyphSet(GlyphUnicode).Unselected {
		t.Fatalf("old split cell = %q, want unselected", got)
	}
	if got := ansi.Cut(rows[screenRow], newColumn, newColumn+1); got != glyphSet(GlyphUnicode).Selected {
		t.Fatalf("new split cell = %q, want selected", got)
	}
	if got := lipgloss.Width(rows[screenRow]); got != split.layout.BodyRect.Width() {
		t.Fatalf("selection overlay changed row width to %d, want %d", got, split.layout.BodyRect.Width())
	}

	split.SetSize(80, 24)
	if split.EffectiveMode() != DiffViewerUnified ||
		split.CurrentAnchor().Side != DiffViewerSideNew {
		t.Fatalf("fallback lost split preference/side: mode=%s anchor=%+v",
			split.EffectiveMode(), split.CurrentAnchor())
	}
	screenRow = split.currentRowIndex() - split.viewport.YOffset()
	rows = strings.Split(ansi.Strip(split.renderVisibleBody()), "\n")
	if got := ansi.Cut(rows[screenRow], 0, 1); got != glyphSet(GlyphUnicode).Selected {
		t.Fatalf("unified fallback selected cell = %q", got)
	}

	noColor = false
	styledSplit := newDiffViewerForTest(160, 48)
	_, _ = styledSplit.Update(charKey('s'))
	if !styledSplit.jumpToLine(1, DiffViewerSideOld) {
		t.Fatal("could not select styled paired split row")
	}
	styledSplit.moveSplitSide(DiffViewerSideNew)
	screenRow = styledSplit.currentRowIndex() - styledSplit.viewport.YOffset()
	styledRows := strings.Split(styledSplit.renderVisibleBody(), "\n")
	if !strings.Contains(styledRows[screenRow], "\x1b[") {
		t.Fatal("styled split fixture did not contain ANSI attributes")
	}
	newColumn = styledSplit.layout.OldDigits +
		diffViewerSplitPaneChromeColumns +
		styledSplit.layout.OldCodeWidth +
		diffViewerSplitGapColumns
	styledPlainRow := ansi.Strip(styledRows[screenRow])
	if got := ansi.Cut(styledPlainRow, newColumn, newColumn+1); got != glyphSet(GlyphUnicode).Selected {
		t.Fatalf("ANSI-styled new split cell = %q, want selected", got)
	}
}

func TestDiffViewerSplitNavigationAlwaysSelectsAPaintedSide(t *testing.T) {
	hunk := DiffHunk{OldStart: 1, OldCount: 4, NewStart: 1, NewCount: 4}
	viewer := NewDiffViewer(
		"block-split-painted-side",
		[]DiffFileProjection{{
			ID: "painted-side", DisplayPath: "painted-side.go", Revision: 1,
			Lines: []DiffLine{
				{Kind: DiffHunkHeader, Hunk: &hunk},
				{Kind: DiffRemoved, Content: "paired old", OldLine: 1},
				{Kind: DiffAdded, Content: "paired new", NewLine: 1},
				{Kind: DiffContext, Content: "between", OldLine: 2, NewLine: 2},
				{Kind: DiffRemoved, Content: "old only", OldLine: 3},
				{Kind: DiffContext, Content: "between again", OldLine: 4, NewLine: 3},
				{Kind: DiffAdded, Content: "new only", NewLine: 4},
			},
		}},
		DiffViewerOptions{Width: 160, Height: 48},
	)

	if !viewer.jumpToLine(1, DiffViewerSideUnified) {
		t.Fatal("could not select unified removed line")
	}
	_, _ = viewer.Update(charKey('s'))
	if anchor := viewer.CurrentAnchor(); anchor.Side != DiffViewerSideOld ||
		anchor.LineIndex != 1 {
		t.Fatalf("unified to split resolved %+v, want painted old side", anchor)
	}
	if _, ok := viewer.selectionColumn(viewer.currentRowIndex()); !ok {
		t.Fatal("unified to split left no visible selector")
	}

	viewer.moveSplitSide(DiffViewerSideNew)
	viewer.moveCursor(2)
	if anchor := viewer.CurrentAnchor(); anchor.Side != DiffViewerSideOld ||
		anchor.LineIndex != 4 {
		t.Fatalf("new side to old-only row resolved %+v", anchor)
	}
	if column, ok := viewer.selectionColumn(viewer.currentRowIndex()); !ok || column != 0 {
		t.Fatalf("old-only selector column = %d, visible=%t", column, ok)
	}

	viewer.moveCursor(2)
	if anchor := viewer.CurrentAnchor(); anchor.Side != DiffViewerSideNew ||
		anchor.LineIndex != 6 {
		t.Fatalf("old side to new-only row resolved %+v", anchor)
	}
	newColumn := viewer.layout.OldDigits +
		diffViewerSplitPaneChromeColumns +
		viewer.layout.OldCodeWidth +
		diffViewerSplitGapColumns
	if column, ok := viewer.selectionColumn(viewer.currentRowIndex()); !ok || column != newColumn {
		t.Fatalf("new-only selector column = %d, want %d visible=%t", column, newColumn, ok)
	}
}

func TestDiffViewerSingletonChromeAndASCIITruncation(t *testing.T) {
	single := NewDiffViewer(
		"block-singleton",
		[]DiffFileProjection{{
			ID:          "single",
			DisplayPath: strings.Repeat("nested-segment/", 20) + "file.go",
			Revision:    1,
			Lines:       diffViewerLongLines(2),
		}},
		DiffViewerOptions{Width: 80, Height: 24, GlyphProfile: GlyphASCII},
	)
	plain := ansi.Strip(single.View())
	if strings.Contains(plain, "1/1") || strings.Contains(plain, "tab file") {
		t.Fatalf("singleton rendered redundant file chrome:\n%s", plain)
	}
	if !strings.Contains(plain, "~") {
		t.Fatalf("long ASCII header did not use ASCII truncation:\n%s", plain)
	}
	if strings.ContainsAny(plain, "…·↑↓←→╭╮╰╯│●○") {
		t.Fatalf("ASCII singleton leaked Unicode chrome:\n%s", plain)
	}

	multiple := newDiffViewerForTest(160, 48)
	multiplePlain := ansi.Strip(multiple.View())
	if !strings.Contains(multiplePlain, "1/2") ||
		!strings.Contains(multiplePlain, "tab file") {
		t.Fatalf("multi-file viewer lost file chrome:\n%s", multiplePlain)
	}
}

func BenchmarkDiffViewerIncrementalNavigationNearMaximum(b *testing.B) {
	viewer := NewDiffViewer(
		"block-benchmark",
		[]DiffFileProjection{{
			ID: "benchmark", DisplayPath: "benchmark.go", Revision: 1,
			Lines: diffViewerLongLines(maxPersistedDiffLines),
		}},
		DiffViewerOptions{Width: 160, Height: 48},
	)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if index%2 == 0 {
			viewer.moveCursor(1)
		} else {
			viewer.moveCursor(-1)
		}
	}
	if viewer.currentRowIndex() < 0 {
		b.Fatal("navigation lost the physical cursor row")
	}
}
