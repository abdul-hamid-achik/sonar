package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type outputViewerFixture struct {
	viewer  *OutputViewer
	store   *OutputDetailStore
	receipt OutputDetailReceipt
}

func newOutputViewerFixture(
	t testing.TB,
	source string,
	width, height int,
	limits ...outputDetailLimits,
) outputViewerFixture {
	t.Helper()
	var store *OutputDetailStore
	if len(limits) > 0 {
		store = newOutputDetailStore(limits[0])
	} else {
		store = NewOutputDetailStore()
	}
	receipt, err := store.Admit(source)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	origin := EntityRef{
		Kind:    EntityKindTranscriptBlock,
		BlockID: BlockID("output-viewer-origin"),
	}
	viewer := NewOutputViewer(origin, receipt, width, height, true)
	return outputViewerFixture{viewer: viewer, store: store, receipt: receipt}
}

func settleOutputViewerRequest(
	t testing.TB,
	fixture outputViewerFixture,
	event OutputViewerEvent,
) OutputDetailPage {
	t.Helper()
	if event.Kind != OutputViewerEventRequestPage || !event.Token.Valid() {
		t.Fatalf("event is not a valid page request: %#v", event)
	}
	page, err := fixture.store.Page(context.Background(), event.Request)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if !fixture.viewer.ApplyPage(event.Token, page) {
		t.Fatalf("ApplyPage rejected current token %#v", event.Token)
	}
	return page
}

func loadOutputViewer(t testing.TB, fixture outputViewerFixture) OutputDetailPage {
	t.Helper()
	return settleOutputViewerRequest(t, fixture, fixture.viewer.InitialPageRequest())
}

func TestProjectOutputViewerLayoutRepresentativeSizes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		width, height int
		outerWidth    int
		outerHeight   int
		bodyHeight    int
	}{
		{width: 30, height: 12, outerWidth: 30, outerHeight: 12, bodyHeight: 8},
		{width: 80, height: 24, outerWidth: 72, outerHeight: 21, bodyHeight: 17},
		{width: 160, height: 48, outerWidth: 144, outerHeight: 43, bodyHeight: 39},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("%dx%d", test.width, test.height), func(t *testing.T) {
			t.Parallel()
			layout := ProjectOutputViewerLayout(test.width, test.height, false)
			if got := layout.OuterRect.Width(); got != test.outerWidth {
				t.Fatalf("outer width = %d, want %d", got, test.outerWidth)
			}
			if got := layout.OuterRect.Height(); got != test.outerHeight {
				t.Fatalf("outer height = %d, want %d", got, test.outerHeight)
			}
			if got := layout.BodyRect.Height(); got != test.bodyHeight {
				t.Fatalf("body height = %d, want %d", got, test.bodyHeight)
			}
			if layout.ContentRect.Width() != max(0, test.outerWidth-4) {
				t.Fatalf("content width = %d, want %d", layout.ContentRect.Width(), max(0, test.outerWidth-4))
			}

			withSearch := ProjectOutputViewerLayout(test.width, test.height, true)
			if withSearch.SearchRect.Height() != 1 ||
				withSearch.BodyRect.Height() != layout.BodyRect.Height()-1 {
				t.Fatalf("search geometry is not exact: without=%#v with=%#v", layout, withSearch)
			}
		})
	}
}

func TestProjectOutputViewerLayoutExhaustiveContainment(t *testing.T) {
	t.Parallel()
	for width := 0; width <= 200; width++ {
		for height := 0; height <= 80; height++ {
			for _, searchVisible := range []bool{false, true} {
				layout := ProjectOutputViewerLayout(width, height, searchVisible)
				if !outputViewerRectWithin(layout.OuterRect, layout.ScreenRect) {
					t.Fatalf("%dx%d search=%v outer outside screen: %#v", width, height, searchVisible, layout)
				}
				for name, rect := range map[string]CellRect{
					"content": layout.ContentRect,
					"header":  layout.HeaderRect,
					"search":  layout.SearchRect,
					"body":    layout.BodyRect,
					"footer":  layout.FooterRect,
				} {
					if !outputViewerRectWithin(rect, layout.OuterRect) {
						t.Fatalf("%dx%d search=%v %s outside outer: %#v", width, height, searchVisible, name, layout)
					}
				}
				wantWidth := min(width, max(outputViewerMinimumOuterWidth, outputViewerNinetyPercent(width)))
				wantHeight := min(height, max(outputViewerMinimumOuterHeight, outputViewerNinetyPercent(height)))
				if layout.OuterRect.Width() != wantWidth || layout.OuterRect.Height() != wantHeight {
					t.Fatalf("%dx%d preferred extent mismatch: %#v", width, height, layout)
				}
				searchRows := 0
				if searchVisible {
					searchRows = layout.SearchRect.Height()
				}
				assigned := layout.HeaderRect.Height() + searchRows +
					layout.BodyRect.Height() + layout.FooterRect.Height()
				if assigned != layout.ContentRect.Height() {
					t.Fatalf("%dx%d search=%v assigned rows=%d content=%d: %#v",
						width, height, searchVisible, assigned, layout.ContentRect.Height(), layout)
				}
				if layout.HeaderRect.MaxY > layout.SearchRect.MinY ||
					layout.SearchRect.MaxY > layout.BodyRect.MinY ||
					layout.BodyRect.MaxY > layout.FooterRect.MinY {
					t.Fatalf("%dx%d search=%v rectangles overlap: %#v", width, height, searchVisible, layout)
				}
			}
		}
	}
}

func outputViewerRectWithin(inner, outer CellRect) bool {
	return inner.MinX >= outer.MinX && inner.MaxX <= outer.MaxX &&
		inner.MinY >= outer.MinY && inner.MaxY <= outer.MaxY
}

func TestOutputViewerRendersExactFrameAndSingleContainedCursor(t *testing.T) {
	t.Parallel()
	for _, size := range []struct{ width, height int }{
		{30, 12}, {80, 24}, {160, 48},
	} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			t.Parallel()
			fixture := newOutputViewerFixture(t, "alpha\nbeta\ngamma", size.width, size.height)
			loadOutputViewer(t, fixture)

			view, cursor := fixture.viewer.ViewWithCursor()
			outer := fixture.viewer.Layout().OuterRect
			if width, height := lipgloss.Size(view); width != outer.Width() || height != outer.Height() {
				t.Fatalf("frame size = %dx%d, want %dx%d\n%s", width, height, outer.Width(), outer.Height(), ansi.Strip(view))
			}
			for index, row := range strings.Split(view, "\n") {
				if got := lipgloss.Width(row); got != outer.Width() {
					t.Fatalf("row %d width = %d, want %d: %q", index, got, outer.Width(), ansi.Strip(row))
				}
			}
			if cursor != nil {
				t.Fatalf("unfocused viewer exposed cursor: %#v", cursor)
			}

			event, _ := fixture.viewer.Update(charKey('/'))
			if !event.Empty() {
				t.Fatalf("opening search emitted parent intent: %#v", event)
			}
			view, cursor = fixture.viewer.ViewWithCursor()
			if cursor == nil {
				t.Fatal("focused search omitted hardware cursor")
			}
			search := fixture.viewer.Layout().SearchRect
			localSearch := NewCellRect(
				search.MinX-outer.MinX,
				search.MinY-outer.MinY,
				search.MaxX-outer.MinX,
				search.MaxY-outer.MinY,
			)
			if !localSearch.Contains(cursor.X, cursor.Y) {
				t.Fatalf("cursor %#v outside local search rect %#v", cursor, localSearch)
			}
			if width, height := lipgloss.Size(view); width != outer.Width() || height != outer.Height() {
				t.Fatalf("search frame size = %dx%d, want %dx%d", width, height, outer.Width(), outer.Height())
			}
		})
	}
}

func TestOutputViewerSearchBodyAndFooterOccupyTheirExactRows(t *testing.T) {
	t.Parallel()
	for _, size := range []struct{ width, height int }{
		{80, 24},
		{160, 48},
	} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			t.Parallel()
			sourceRows := make([]string, 100)
			for index := range sourceRows {
				sourceRows[index] = fmt.Sprintf("BODY_ROW_%02d", index)
			}
			fixture := newOutputViewerFixture(
				t,
				strings.Join(sourceRows, "\n"),
				size.width,
				size.height,
			)
			loadOutputViewer(t, fixture)
			fixture.viewer.Update(charKey('/'))

			plainRows := strings.Split(ansi.Strip(fixture.viewer.View()), "\n")
			layout := fixture.viewer.Layout()
			outer := layout.OuterRect
			if len(plainRows) != outer.Height() {
				t.Fatalf("rendered rows = %d, want outer height %d", len(plainRows), outer.Height())
			}
			localRow := func(rect CellRect) string {
				t.Helper()
				index := rect.MinY - outer.MinY
				if index < 0 || index >= len(plainRows) {
					t.Fatalf("rect %#v maps outside %d rendered rows", rect, len(plainRows))
				}
				return plainRows[index]
			}

			if searchRow := localRow(layout.SearchRect); !strings.Contains(searchRow, "search loaded page") {
				t.Fatalf("search row does not occupy SearchRect: %q", searchRow)
			}
			for offset := range layout.BodyRect.Height() {
				rowRect := NewCellRect(
					layout.BodyRect.MinX,
					layout.BodyRect.MinY+offset,
					layout.BodyRect.MaxX,
					layout.BodyRect.MinY+offset+1,
				)
				want := fmt.Sprintf("BODY_ROW_%02d", offset)
				if row := localRow(rowRect); !strings.Contains(row, want) {
					t.Fatalf("body offset %d does not occupy BodyRect: got %q want %q", offset, row, want)
				}
			}
			if footerRow := localRow(layout.FooterRect); !strings.Contains(footerRow, "copy visible") {
				t.Fatalf("footer does not occupy FooterRect: %q", footerRow)
			}
			if count := strings.Count(strings.Join(plainRows, "\n"), "search loaded page"); count != 1 {
				t.Fatalf("search control rendered %d times, want exactly once", count)
			}
		})
	}
}

func TestOutputViewerASCIIProfileChangesBorderWithoutChangingGeometry(t *testing.T) {
	t.Parallel()
	store := NewOutputDetailStore()
	receipt, err := store.Admit("ascii output\nsecond row")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	origin := EntityRef{
		Kind:    EntityKindTranscriptBlock,
		BlockID: BlockID("output-viewer-ascii-origin"),
	}
	unicodeViewer := NewOutputViewer(origin, receipt, 80, 24, true, GlyphUnicode)
	asciiViewer := NewOutputViewer(origin, receipt, 80, 24, true, GlyphASCII)
	for _, viewer := range []*OutputViewer{unicodeViewer, asciiViewer} {
		event := viewer.InitialPageRequest()
		page, pageErr := store.Page(context.Background(), event.Request)
		if pageErr != nil {
			t.Fatalf("Page: %v", pageErr)
		}
		if !viewer.ApplyPage(event.Token, page) {
			t.Fatal("ApplyPage rejected current page")
		}
		viewer.Update(charKey('/'))
	}

	unicodeView := ansi.Strip(unicodeViewer.View())
	asciiView := ansi.Strip(asciiViewer.View())
	if strings.ContainsAny(asciiView, "╭╮╰╯─│") {
		t.Fatalf("ASCII profile emitted Unicode border glyphs:\n%s", asciiView)
	}
	asciiRows := strings.Split(asciiView, "\n")
	if len(asciiRows) == 0 ||
		!strings.HasPrefix(asciiRows[0], "+") ||
		!strings.HasSuffix(asciiRows[0], "+") {
		t.Fatalf("ASCII border is not terminal-safe: %q", asciiRows)
	}
	if !strings.ContainsAny(unicodeView, "╭╮╰╯") {
		t.Fatalf("Unicode profile omitted rounded border:\n%s", unicodeView)
	}
	unicodeWidth, unicodeHeight := lipgloss.Size(unicodeViewer.View())
	asciiWidth, asciiHeight := lipgloss.Size(asciiViewer.View())
	if unicodeViewer.Layout() != asciiViewer.Layout() ||
		unicodeWidth != asciiWidth || unicodeHeight != asciiHeight {
		t.Fatalf(
			"glyph profile changed geometry: unicode=%#v %dx%d ascii=%#v %dx%d",
			unicodeViewer.Layout(), unicodeWidth, unicodeHeight,
			asciiViewer.Layout(), asciiWidth, asciiHeight,
		)
	}
}

func TestOutputViewerRenderExtentIsBoundedAcrossSmallScreens(t *testing.T) {
	t.Parallel()
	fixture := newOutputViewerFixture(t, "alpha\nbeta\ngamma", 30, 12)
	loadOutputViewer(t, fixture)
	for width := 0; width <= 40; width++ {
		for height := 0; height <= 20; height++ {
			fixture.viewer.SetSize(width, height)
			view := fixture.viewer.View()
			outer := fixture.viewer.Layout().OuterRect
			gotWidth, gotHeight := lipgloss.Size(view)
			if outer.Empty() {
				if view != "" {
					t.Fatalf("%dx%d empty outer rendered %dx%d: %q",
						width, height, gotWidth, gotHeight, ansi.Strip(view))
				}
				continue
			}
			if gotWidth != outer.Width() || gotHeight != outer.Height() {
				t.Fatalf("%dx%d rendered %dx%d, want bounded %dx%d: %q",
					width, height, gotWidth, gotHeight,
					outer.Width(), outer.Height(), ansi.Strip(view))
			}
		}
	}
}

func TestOutputViewerPageTokensRejectStaleResultsAndCacheOnePage(t *testing.T) {
	t.Parallel()
	limits := defaultOutputDetailLimits
	limits.pageRows = 3
	fixture := newOutputViewerFixture(
		t,
		"row-0\nrow-1\nrow-2\nrow-3\nrow-4\nrow-5\nrow-6",
		80,
		24,
		limits,
	)

	initial := fixture.viewer.InitialPageRequest()
	if initial.Kind != OutputViewerEventRequestPage ||
		initial.Request.RowLimit != outputViewerPageRowLimit ||
		initial.Request.ByteLimit != outputViewerPageByteLimit ||
		initial.Request.Ref != fixture.receipt.Ref {
		t.Fatalf("unexpected initial request: %#v", initial)
	}
	if duplicate := fixture.viewer.InitialPageRequest(); !duplicate.Empty() {
		t.Fatalf("duplicate initial request was not suppressed: %#v", duplicate)
	}
	first, err := fixture.store.Page(context.Background(), initial.Request)
	if err != nil {
		t.Fatalf("first Page: %v", err)
	}
	stale := initial.Token
	stale.Sequence++
	if fixture.viewer.ApplyPage(stale, first) {
		t.Fatal("stale sequence was accepted")
	}
	if fixture.viewer.Status() != OutputViewerLoading {
		t.Fatalf("stale result changed status: %v", fixture.viewer.Status())
	}
	if !fixture.viewer.ApplyPage(initial.Token, first) {
		t.Fatal("current initial token was rejected")
	}
	if fixture.viewer.CachedRowCount() != 3 {
		t.Fatalf("first cache rows = %d, want 3", fixture.viewer.CachedRowCount())
	}

	next, _ := fixture.viewer.Update(charKey(']'))
	if next.Kind != OutputViewerEventRequestPage ||
		next.Token.Generation != initial.Token.Generation ||
		next.Token.Sequence <= initial.Token.Sequence ||
		next.Request.Cursor != first.Next {
		t.Fatalf("unexpected next request: first=%#v next=%#v", initial, next)
	}
	fixture.viewer.search.SetValue("row")
	fixture.viewer.recomputeMatches()
	beforeMatches := append([]outputViewerMatch(nil), fixture.viewer.matches...)
	beforeBounds := append([][2]int(nil), fixture.viewer.matchBounds...)
	beforeCapped := fixture.viewer.matchesCapped
	beforeView := fixture.viewer.viewport.View()
	if fixture.viewer.ApplyPage(initial.Token, first) {
		t.Fatal("settled token was accepted while a newer page was pending")
	}
	if !slices.Equal(fixture.viewer.matches, beforeMatches) ||
		!slices.Equal(fixture.viewer.matchBounds, beforeBounds) ||
		fixture.viewer.matchesCapped != beforeCapped ||
		fixture.viewer.viewport.View() != beforeView {
		t.Fatalf("stale page result changed search state: matches=%v capped=%v",
			fixture.viewer.matches, fixture.viewer.matchesCapped)
	}
	second := settleOutputViewerRequest(t, fixture, next)
	if fixture.viewer.CachedRowCount() != 3 ||
		strings.Contains(fixture.viewer.visibleOutput(), "row-0") ||
		!strings.Contains(fixture.viewer.visibleOutput(), "row-3") {
		t.Fatalf("cache did not replace the first page: rows=%d copy=%q",
			fixture.viewer.CachedRowCount(), fixture.viewer.visibleOutput())
	}

	previous, _ := fixture.viewer.Update(charKey('['))
	if previous.Kind != OutputViewerEventRequestPage || previous.Request.Cursor != (OutputDetailCursor{}) {
		t.Fatalf("unexpected previous-page request: %#v", previous)
	}
	settleOutputViewerRequest(t, fixture, previous)
	if !strings.Contains(fixture.viewer.visibleOutput(), "row-0") {
		t.Fatalf("previous page did not restore first page: %q", fixture.viewer.visibleOutput())
	}

	oldNext, _ := fixture.viewer.Update(charKey(']'))
	replacementReceipt, err := fixture.store.Admit("replacement-0\nreplacement-1")
	if err != nil {
		t.Fatalf("replacement Admit: %v", err)
	}
	replacement := fixture.viewer.ReplaceReceipt(replacementReceipt)
	if replacement.Kind != OutputViewerEventRequestPage ||
		replacement.Token.Generation <= oldNext.Token.Generation {
		t.Fatalf("replacement did not advance generation: old=%#v new=%#v", oldNext, replacement)
	}
	if fixture.viewer.ApplyPage(oldNext.Token, second) {
		t.Fatal("previous-generation page mutated replacement viewer")
	}
	replacementPage, err := fixture.store.Page(context.Background(), replacement.Request)
	if err != nil {
		t.Fatalf("replacement Page: %v", err)
	}
	if !fixture.viewer.ApplyPage(replacement.Token, replacementPage) ||
		!strings.Contains(fixture.viewer.visibleOutput(), "replacement-0") {
		t.Fatalf("replacement result did not settle: %q", fixture.viewer.visibleOutput())
	}
}

func TestOutputViewerSearchIsScopedHonestlyToLoadedPage(t *testing.T) {
	t.Parallel()
	lines := make([]string, 40)
	for index := range lines {
		lines[index] = fmt.Sprintf("ordinary row %02d", index)
	}
	lines[2] = "Alpha first"
	lines[30] = "second ALPHA"
	fixture := newOutputViewerFixture(t, strings.Join(lines, "\n"), 160, 48)
	loadOutputViewer(t, fixture)

	fixture.viewer.Update(charKey('/'))
	for _, character := range "alpha" {
		fixture.viewer.Update(charKey(character))
	}
	fixture.viewer.Update(enterKey())
	if len(fixture.viewer.matches) != 2 ||
		!strings.Contains(fixture.viewer.footerText(), "2 matches in complete output") {
		t.Fatalf("search projection mismatch: matches=%v footer=%q",
			fixture.viewer.matches, fixture.viewer.footerText())
	}
	fixture.viewer.Update(charKey('n'))
	if selected := fixture.viewer.matches[fixture.viewer.selectedMatch].pageRow; selected != 2 {
		t.Fatalf("first match row = %d, want 2", selected)
	}
	fixture.viewer.Update(charKey('n'))
	if selected := fixture.viewer.matches[fixture.viewer.selectedMatch].pageRow; selected != 30 {
		t.Fatalf("second match row = %d, want 30", selected)
	}
	fixture.viewer.Update(charKey('N'))
	if selected := fixture.viewer.matches[fixture.viewer.selectedMatch].pageRow; selected != 2 {
		t.Fatalf("previous match row = %d, want 2", selected)
	}

	event, _ := fixture.viewer.Update(escKey())
	if !event.Empty() || fixture.viewer.searchVisible {
		t.Fatalf("first escape did not close search only: event=%#v visible=%v", event, fixture.viewer.searchVisible)
	}
	event, _ = fixture.viewer.Update(escKey())
	if event.Kind != OutputViewerEventClose || event.Origin != fixture.viewer.Origin {
		t.Fatalf("second escape did not request modal close: %#v", event)
	}

	limits := defaultOutputDetailLimits
	limits.pageRows = 3
	paged := newOutputViewerFixture(t, "alpha\nx\nalpha\ny\nalpha", 160, 48, limits)
	loadOutputViewer(t, paged)
	paged.viewer.Update(charKey('/'))
	for _, character := range "alpha" {
		paged.viewer.Update(charKey(character))
	}
	paged.viewer.Update(enterKey())
	if footer := paged.viewer.footerText(); !strings.Contains(footer, "loaded page") ||
		strings.Contains(footer, "complete output") {
		t.Fatalf("paged search overstated its scope: %q", footer)
	}
}

func TestOutputViewerDenseSearchIsBoundedWithExactCellOffsets(t *testing.T) {
	t.Parallel()
	source := strings.Repeat("a", outputViewerPageByteLimit)
	fixture := newOutputViewerFixture(t, source, 160, 48)
	loadOutputViewer(t, fixture)

	fixture.viewer.searchVisible = true
	fixture.viewer.search.SetValue("a")
	fixture.viewer.recomputeMatches()

	if got := len(fixture.viewer.matches); got != outputViewerSearchMatchLimit {
		t.Fatalf("dense search retained %d matches, want cap %d",
			got, outputViewerSearchMatchLimit)
	}
	if !fixture.viewer.matchesCapped {
		t.Fatal("dense search did not report known matches beyond the cap")
	}
	first := fixture.viewer.matches[0]
	last := fixture.viewer.matches[len(fixture.viewer.matches)-1]
	if first != (outputViewerMatch{pageRow: 0, startCell: 0, endCell: 1}) ||
		last != (outputViewerMatch{
			pageRow:   0,
			startCell: outputViewerSearchMatchLimit - 1,
			endCell:   outputViewerSearchMatchLimit,
		}) {
		t.Fatalf("dense cell offsets mismatch: first=%#v last=%#v", first, last)
	}
	if footer := fixture.viewer.footerText(); !strings.Contains(
		footer,
		fmt.Sprintf("at least %d matches in complete output", outputViewerSearchMatchLimit),
	) || !strings.Contains(
		footer,
		fmt.Sprintf("%d nearby highlighted", outputViewerHighlightLimit),
	) {
		t.Fatalf("capped search footer was not honest: %q", footer)
	}

	if fixture.viewer.viewport.StyleLineFunc != nil {
		t.Fatal("dense search retained whole-row styling instead of exact ranges")
	}
	if got := fixture.viewer.highlightTo - fixture.viewer.highlightFrom; got != outputViewerHighlightLimit {
		t.Fatalf("active dense highlights = %d, want render cap %d",
			got, outputViewerHighlightLimit)
	}
	fixture.viewer.navigateMatch(1)
	fixture.viewer.navigateMatch(1)
	if fixture.viewer.selectedMatch != 1 {
		t.Fatalf("selected match = %d, want 1", fixture.viewer.selectedMatch)
	}
	if got := fixture.viewer.matches[fixture.viewer.selectedMatch]; got.startCell != 1 || got.endCell != 2 {
		t.Fatalf("selected dense range = %#v, want cells [1,2)", got)
	}
}

func TestOutputViewerDenseHighlightWindowFollowsViewportAndWrapsNavigation(t *testing.T) {
	t.Parallel()
	fixture := newOutputViewerFixture(
		t,
		strings.Repeat("a", outputViewerPageByteLimit),
		160,
		48,
	)
	loadOutputViewer(t, fixture)
	fixture.viewer.search.SetValue("a")
	fixture.viewer.recomputeMatches()

	fixture.viewer.viewport.SetXOffset(100)
	fixture.viewer.installHighlightWindow(-1)
	if fixture.viewer.highlightFrom != 100 ||
		fixture.viewer.highlightTo != 100+outputViewerHighlightLimit {
		t.Fatalf("horizontal highlight window = [%d,%d), want [100,%d)",
			fixture.viewer.highlightFrom,
			fixture.viewer.highlightTo,
			100+outputViewerHighlightLimit)
	}

	fixture.viewer.navigateMatch(-1)
	if fixture.viewer.selectedMatch != outputViewerSearchMatchLimit-1 ||
		fixture.viewer.highlightTo != outputViewerSearchMatchLimit ||
		fixture.viewer.highlightTo-fixture.viewer.highlightFrom != outputViewerHighlightLimit {
		t.Fatalf("reverse wrap did not install final bounded window: selected=%d window=[%d,%d)",
			fixture.viewer.selectedMatch,
			fixture.viewer.highlightFrom,
			fixture.viewer.highlightTo)
	}
	fixture.viewer.navigateMatch(1)
	if fixture.viewer.selectedMatch != 0 ||
		fixture.viewer.highlightFrom != 0 ||
		fixture.viewer.highlightTo != outputViewerHighlightLimit {
		t.Fatalf("forward wrap did not restore first bounded window: selected=%d window=[%d,%d)",
			fixture.viewer.selectedMatch,
			fixture.viewer.highlightFrom,
			fixture.viewer.highlightTo)
	}
}

func TestOutputViewerSearchRendersAndSelectsExactRangesOnSameLine(t *testing.T) {
	t.Parallel()
	const line = "alpha gap alpha"
	fixture := newOutputViewerFixture(t, line, 160, 48)
	loadOutputViewer(t, fixture)
	fixture.viewer.search.SetValue("alpha")
	fixture.viewer.recomputeMatches()
	fixture.viewer.viewport.LeftGutterFunc = nil

	if got, want := fixture.viewer.matches, []outputViewerMatch{
		{pageRow: 0, startCell: 0, endCell: 5},
		{pageRow: 0, startCell: 10, endCell: 15},
	}; !slices.Equal(got, want) {
		t.Fatalf("same-line ranges = %#v, want %#v", got, want)
	}
	if fixture.viewer.viewport.StyleLineFunc != nil {
		t.Fatal("search uses whole-line style instead of Bubbles highlights")
	}

	highlighted := lipgloss.StyleRanges(
		line,
		lipgloss.NewRange(0, 5, fixture.viewer.viewport.HighlightStyle),
		lipgloss.NewRange(10, 15, fixture.viewer.viewport.HighlightStyle),
	)
	firstViewportLine := func() string {
		return strings.TrimRight(strings.SplitN(fixture.viewer.viewport.View(), "\n", 2)[0], " ")
	}

	if got := firstViewportLine(); got != highlighted {
		t.Fatalf("initial exact highlights mismatch:\n got %q\nwant %q", got, highlighted)
	}

	fixture.viewer.navigateMatch(1)
	wantFirst := lipgloss.StyleRanges(
		highlighted,
		lipgloss.NewRange(0, 5, fixture.viewer.selectedStyle),
	)
	if got := firstViewportLine(); fixture.viewer.selectedMatch != 0 || got != wantFirst {
		t.Fatalf("first exact selection mismatch: selected=%d\n got %q\nwant %q",
			fixture.viewer.selectedMatch, got, wantFirst)
	}

	fixture.viewer.navigateMatch(1)
	wantSecond := lipgloss.StyleRanges(
		highlighted,
		lipgloss.NewRange(10, 15, fixture.viewer.selectedStyle),
	)
	if got := firstViewportLine(); fixture.viewer.selectedMatch != 1 || got != wantSecond {
		t.Fatalf("second exact selection mismatch: selected=%d\n got %q\nwant %q",
			fixture.viewer.selectedMatch, got, wantSecond)
	}
}

func TestOutputViewerSearchCapIsExactNotSpeculative(t *testing.T) {
	t.Parallel()
	fixture := newOutputViewerFixture(
		t,
		strings.Repeat("a", outputViewerSearchMatchLimit),
		160,
		48,
	)
	loadOutputViewer(t, fixture)
	fixture.viewer.searchVisible = true
	fixture.viewer.search.SetValue("a")
	fixture.viewer.recomputeMatches()

	if len(fixture.viewer.matches) != outputViewerSearchMatchLimit ||
		fixture.viewer.matchesCapped {
		t.Fatalf("exact-cap search was mislabeled: matches=%d capped=%v",
			len(fixture.viewer.matches), fixture.viewer.matchesCapped)
	}
	if footer := fixture.viewer.footerText(); !strings.Contains(
		footer,
		fmt.Sprintf("%d matches in complete output", outputViewerSearchMatchLimit),
	) || strings.Contains(footer, "at least") {
		t.Fatalf("exact-cap footer was not exact: %q", footer)
	}
}

func TestOutputViewerSearchExpandsPartialUnicodeMatchesToGraphemeCells(t *testing.T) {
	t.Parallel()
	combiningPrefix := "lead "
	combining := "e\u0301"
	emojiPrefix := combiningPrefix + combining + " mid "
	emoji := "👩‍💻"
	fixture := newOutputViewerFixture(
		t,
		emojiPrefix+emoji+" tail",
		160,
		48,
	)
	loadOutputViewer(t, fixture)

	fixture.viewer.search.SetValue("\u0301")
	fixture.viewer.recomputeMatches()
	if len(fixture.viewer.matches) != 1 {
		t.Fatalf("combining-mark matches = %v, want one", fixture.viewer.matches)
	}
	wantCombining := outputViewerMatch{
		pageRow:   0,
		startCell: lipgloss.Width(combiningPrefix),
		endCell:   lipgloss.Width(combiningPrefix + combining),
	}
	if got := fixture.viewer.matches[0]; got != wantCombining {
		t.Fatalf("combining-mark cell span = %#v, want %#v", got, wantCombining)
	}

	// The laptop rune starts inside the ZWJ grapheme. Navigation should reveal
	// the complete visible emoji rather than an impossible half-glyph span.
	fixture.viewer.search.SetValue("💻")
	fixture.viewer.recomputeMatches()
	if len(fixture.viewer.matches) != 1 {
		t.Fatalf("ZWJ-member matches = %v, want one", fixture.viewer.matches)
	}
	wantEmoji := outputViewerMatch{
		pageRow:   0,
		startCell: lipgloss.Width(emojiPrefix),
		endCell:   lipgloss.Width(emojiPrefix + emoji),
	}
	if got := fixture.viewer.matches[0]; got != wantEmoji {
		t.Fatalf("ZWJ-member cell span = %#v, want %#v", got, wantEmoji)
	}
	fixture.viewer.viewport.LeftGutterFunc = nil
	rendered := strings.TrimRight(
		strings.SplitN(fixture.viewer.viewport.View(), "\n", 2)[0],
		" ",
	)
	wantRendered := lipgloss.StyleRanges(
		emojiPrefix+emoji+" tail",
		lipgloss.NewRange(wantEmoji.startCell, wantEmoji.endCell, fixture.viewer.viewport.HighlightStyle),
	)
	if rendered != wantRendered {
		t.Fatalf("ZWJ-member exact render mismatch:\n got %q\nwant %q", rendered, wantRendered)
	}
}

func BenchmarkOutputViewerDenseSearch64KiB(b *testing.B) {
	source := strings.Repeat("a", outputViewerPageByteLimit)
	fixture := newOutputViewerFixture(b, source, 160, 48)
	loadOutputViewer(b, fixture)
	fixture.viewer.search.SetValue("a")

	b.Run("Recompute", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(source)))
		for range b.N {
			fixture.viewer.recomputeMatches()
		}
	})

	if len(fixture.viewer.matches) != outputViewerSearchMatchLimit ||
		!fixture.viewer.matchesCapped {
		b.Fatalf("benchmark lost bounded semantics: matches=%d capped=%v",
			len(fixture.viewer.matches), fixture.viewer.matchesCapped)
	}

	b.Run("Render", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = fixture.viewer.viewport.View()
		}
	})

	fixture.viewer.navigateMatch(1)
	b.Run("RenderSelected", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = fixture.viewer.viewport.View()
		}
	})

	b.Run("NavigateWithinWindow", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			fixture.viewer.navigateMatch(1)
			fixture.viewer.navigateMatch(-1)
		}
	})
}

func TestOutputViewerCopyVisibleIsTypedBoundedAndTerminalSafe(t *testing.T) {
	t.Parallel()
	lines := make([]string, 20)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%02d payload", index)
	}
	lines[5] = "\x1b]0;spoof\x07visible \x1b[31mred\x1b[0m"
	fixture := newOutputViewerFixture(t, strings.Join(lines, "\n"), 30, 12)
	loadOutputViewer(t, fixture)
	for range 3 {
		fixture.viewer.Update(downKey())
	}

	event, command := fixture.viewer.Update(charKey('c'))
	if command != nil {
		t.Fatal("copy-visible child attempted clipboard command")
	}
	if event.Kind != OutputViewerEventCopyVisible || event.Origin != fixture.viewer.Origin {
		t.Fatalf("unexpected copy event: %#v", event)
	}
	if strings.ContainsAny(event.CopyText, "\x1b\r") || strings.Contains(event.CopyText, "spoof") {
		t.Fatalf("copy leaked terminal control content: %q", event.CopyText)
	}
	copyLines := strings.Split(event.CopyText, "\n")
	if len(copyLines) > fixture.viewer.Layout().BodyRect.Height() ||
		!strings.HasPrefix(copyLines[0], "line-03") {
		t.Fatalf("copy is not the visible body window: %q", event.CopyText)
	}
	if strings.Contains(event.CopyText, "3 |") {
		t.Fatalf("copy included presentation gutter: %q", event.CopyText)
	}
}

func TestOutputViewerUnavailableAndPendingStatesEmitNoFalseActions(t *testing.T) {
	t.Parallel()
	limits := defaultOutputDetailLimits
	limits.pageRows = 2
	fixture := newOutputViewerFixture(t, "one\ntwo\nthree", 80, 24, limits)
	loadOutputViewer(t, fixture)
	next, _ := fixture.viewer.Update(charKey(']'))
	if next.Kind != OutputViewerEventRequestPage {
		t.Fatalf("next did not request a page: %#v", next)
	}
	for _, key := range []tea.KeyPressMsg{charKey('c'), charKey(']'), charKey('['), charKey('/')} {
		if event, _ := fixture.viewer.Update(key); !event.Empty() {
			t.Fatalf("pending viewer emitted false action for %q: %#v", key.String(), event)
		}
	}

	if !fixture.store.Drop(fixture.receipt.Ref) {
		t.Fatal("Drop did not revoke fixture receipt")
	}
	_, err := fixture.store.Page(context.Background(), next.Request)
	if !errors.Is(err, ErrOutputDetailUnavailable) {
		t.Fatalf("revoked Page error = %v, want unavailable", err)
	}
	if !fixture.viewer.ApplyPageError(next.Token, err) ||
		fixture.viewer.Status() != OutputViewerUnavailable {
		t.Fatalf("unavailable result did not settle: status=%v", fixture.viewer.Status())
	}
	for _, key := range []tea.KeyPressMsg{charKey('c'), charKey(']'), charKey('['), charKey('/'), charKey('n')} {
		if event, _ := fixture.viewer.Update(key); !event.Empty() {
			t.Fatalf("unavailable viewer emitted false action for %q: %#v", key.String(), event)
		}
	}
	footer := fixture.viewer.footerText()
	if !strings.Contains(strings.ToLower(footer), "available") ||
		strings.Contains(footer, "copy") || strings.Contains(footer, "page") {
		t.Fatalf("unavailable footer advertises false action: %q", footer)
	}
	if event, _ := fixture.viewer.Update(escKey()); event.Kind != OutputViewerEventClose {
		t.Fatalf("unavailable viewer cannot close: %#v", event)
	}
}

func TestOutputViewerWheelRoutesOnlyInsideBody(t *testing.T) {
	t.Parallel()
	lines := make([]string, 60)
	for index := range lines {
		lines[index] = fmt.Sprintf("row-%02d", index)
	}
	fixture := newOutputViewerFixture(t, strings.Join(lines, "\n"), 80, 24)
	loadOutputViewer(t, fixture)
	body := fixture.viewer.Layout().BodyRect

	fixture.viewer.Update(tea.MouseWheelMsg{
		X:      body.MinX,
		Y:      max(0, body.MinY-1),
		Button: tea.MouseWheelDown,
	})
	if got := fixture.viewer.viewport.YOffset(); got != 0 {
		t.Fatalf("wheel outside body moved viewport to %d", got)
	}
	fixture.viewer.Update(tea.MouseWheelMsg{
		X:      body.MinX,
		Y:      body.MinY,
		Button: tea.MouseWheelDown,
	})
	if got := fixture.viewer.viewport.YOffset(); got == 0 {
		t.Fatal("wheel inside body did not move viewport")
	}
	offset := fixture.viewer.viewport.YOffset()
	fixture.viewer.Update(tea.MouseWheelMsg{
		X:      body.MaxX,
		Y:      body.MinY,
		Button: tea.MouseWheelDown,
	})
	if got := fixture.viewer.viewport.YOffset(); got != offset {
		t.Fatalf("half-open right boundary moved viewport: got %d want %d", got, offset)
	}
}

func TestOutputViewerPointerFocusUsesExactHalfOpenRects(t *testing.T) {
	fixture := newOutputViewerFixture(t, "alpha\nbeta\ngamma", 80, 24)
	loadOutputViewer(t, fixture)
	_, _ = fixture.viewer.Update(charKey('/'))
	if !fixture.viewer.search.Focused() {
		t.Fatal("search did not own focus")
	}
	layout := fixture.viewer.Layout()

	event, _ := fixture.viewer.Update(tea.MouseClickMsg{
		X:      layout.BodyRect.MinX,
		Y:      layout.BodyRect.MinY,
		Button: tea.MouseLeft,
	})
	if !event.Empty() || fixture.viewer.search.Focused() {
		t.Fatalf("body click emitted an action or retained search focus: %#v", event)
	}

	_, _ = fixture.viewer.Update(tea.MouseClickMsg{
		X:      layout.SearchRect.MinX,
		Y:      layout.SearchRect.MinY,
		Button: tea.MouseLeft,
	})
	if !fixture.viewer.search.Focused() {
		t.Fatal("search-row click did not restore search focus")
	}
	fixture.viewer.search.Blur()
	_, _ = fixture.viewer.Update(tea.MouseClickMsg{
		X:      layout.SearchRect.MaxX,
		Y:      layout.SearchRect.MinY,
		Button: tea.MouseLeft,
	})
	if fixture.viewer.search.Focused() {
		t.Fatal("half-open search right boundary stole focus")
	}
}

func TestOutputViewerPreservesSemanticAnchorAcrossResizeThemeAndSearch(t *testing.T) {
	t.Parallel()
	lines := make([]string, 100)
	for index := range lines {
		lines[index] = fmt.Sprintf("source row %03d", index)
	}
	fixture := newOutputViewerFixture(t, strings.Join(lines, "\n"), 80, 24)
	loadOutputViewer(t, fixture)
	for range 50 {
		fixture.viewer.Update(downKey())
	}
	before := fixture.viewer.Anchor()
	if !before.Valid || before.SourceRow != 50 {
		t.Fatalf("precondition anchor = %#v, want source row 50", before)
	}

	fixture.viewer.SetSize(160, 48)
	if got := fixture.viewer.Anchor(); got != before {
		t.Fatalf("resize changed semantic anchor: before=%#v after=%#v", before, got)
	}
	fixture.viewer.SetTheme(false, defaultThemeID)
	if got := fixture.viewer.Anchor(); got != before {
		t.Fatalf("theme changed semantic anchor: before=%#v after=%#v", before, got)
	}
	fixture.viewer.Update(charKey('/'))
	if got := fixture.viewer.Anchor(); got != before {
		t.Fatalf("search insertion changed semantic anchor: before=%#v after=%#v", before, got)
	}
	fixture.viewer.Update(escKey())
	if got := fixture.viewer.Anchor(); got != before {
		t.Fatalf("search removal changed semantic anchor: before=%#v after=%#v", before, got)
	}
}

func TestOutputViewerReducedMotionDisablesBlinkWithoutChangingViewerState(t *testing.T) {
	t.Parallel()
	lines := make([]string, 100)
	for index := range lines {
		lines[index] = fmt.Sprintf("source row %03d", index)
	}
	fixture := newOutputViewerFixture(t, strings.Join(lines, "\n"), 80, 24)
	loadOutputViewer(t, fixture)
	for range 30 {
		fixture.viewer.Update(downKey())
	}
	fixture.viewer.Update(charKey('/'))
	for _, character := range "row 030" {
		fixture.viewer.Update(charKey(character))
	}

	beforeLayout := fixture.viewer.Layout()
	beforeAnchor := fixture.viewer.Anchor()
	beforeQuery := fixture.viewer.search.Value()
	_, beforeCursor := fixture.viewer.ViewWithCursor()
	if beforeCursor == nil || !beforeCursor.Blink {
		t.Fatalf("animated search cursor precondition failed: %#v", beforeCursor)
	}

	fixture.viewer.SetReducedMotion(true)
	_, reducedCursor := fixture.viewer.ViewWithCursor()
	if reducedCursor == nil || reducedCursor.Blink ||
		fixture.viewer.search.Styles().Cursor.Blink {
		t.Fatalf("reduced motion left search cursor blinking: cursor=%#v styles=%#v",
			reducedCursor, fixture.viewer.search.Styles().Cursor)
	}
	if fixture.viewer.Layout() != beforeLayout ||
		fixture.viewer.Anchor() != beforeAnchor ||
		fixture.viewer.search.Value() != beforeQuery ||
		!fixture.viewer.searchVisible ||
		!fixture.viewer.search.Focused() {
		t.Fatalf(
			"reduced motion changed viewer state: layout=%v/%v anchor=%v/%v query=%q/%q visible=%v focused=%v",
			beforeLayout, fixture.viewer.Layout(),
			beforeAnchor, fixture.viewer.Anchor(),
			beforeQuery, fixture.viewer.search.Value(),
			fixture.viewer.searchVisible, fixture.viewer.search.Focused(),
		)
	}

	// Theme replacement must continue honoring the independent motion axis.
	fixture.viewer.SetTheme(false, defaultThemeID)
	_, themedCursor := fixture.viewer.ViewWithCursor()
	if themedCursor == nil || themedCursor.Blink ||
		fixture.viewer.search.Styles().Cursor.Blink {
		t.Fatalf("theme change re-enabled blink under reduced motion: %#v", themedCursor)
	}

	fixture.viewer.SetReducedMotion(false)
	_, restoredCursor := fixture.viewer.ViewWithCursor()
	if restoredCursor == nil || !restoredCursor.Blink ||
		!fixture.viewer.search.Styles().Cursor.Blink {
		t.Fatalf("animated cursor was not restored: %#v", restoredCursor)
	}
}

func TestOutputViewerSanitizesForgedPageBeforeRendering(t *testing.T) {
	t.Parallel()
	raw := "\x1b]0;spoof\x07visible \x1b[31mred\x1b[0m"
	fixture := newOutputViewerFixture(t, strings.Repeat("x", len(raw)), 80, 24)
	request := fixture.viewer.InitialPageRequest()
	forged := OutputDetailPage{
		Rows: []OutputDetailRow{{
			Index: 0, Text: raw, EndsRow: true, SourceRowComplete: true,
		}},
		Next:   OutputDetailCursor{Row: 1},
		Bytes:  uint64(len(raw)),
		Digest: fixture.receipt.Digest,
	}
	if !fixture.viewer.ApplyPage(request.Token, forged) {
		t.Fatal("valid-shape forged page was rejected before sanitization test")
	}
	view := fixture.viewer.View()
	if strings.Contains(view, "\x1b]") || strings.Contains(ansi.Strip(view), "spoof") {
		t.Fatalf("render leaked raw OSC content: %q", view)
	}
	if plain := ansi.Strip(view); !strings.Contains(plain, "visible red") {
		t.Fatalf("sanitization removed safe text: %q", plain)
	}
}

func TestOutputViewerRejectsOversizedOrMismatchedPage(t *testing.T) {
	t.Parallel()
	fixture := newOutputViewerFixture(t, "safe", 80, 24)
	request := fixture.viewer.InitialPageRequest()
	rows := make([]OutputDetailRow, outputViewerPageRowLimit+1)
	for index := range rows {
		rows[index] = OutputDetailRow{Index: index, Text: "x", EndsRow: true, SourceRowComplete: true}
	}
	oversized := OutputDetailPage{
		Rows: rows, Next: OutputDetailCursor{Row: len(rows)},
		Bytes: uint64(len(rows)), Digest: fixture.receipt.Digest,
	}
	if !fixture.viewer.ApplyPage(request.Token, oversized) ||
		fixture.viewer.Status() != OutputViewerUnavailable ||
		fixture.viewer.CachedRowCount() != 0 {
		t.Fatalf("oversized page did not fail closed: status=%v rows=%d",
			fixture.viewer.Status(), fixture.viewer.CachedRowCount())
	}
}
