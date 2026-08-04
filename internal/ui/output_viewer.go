package ui

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

const (
	outputViewerMinimumOuterWidth  = 72
	outputViewerMinimumOuterHeight = 12
	outputViewerPageRowLimit       = maxOutputDetailPageRows
	outputViewerPageByteLimit      = maxOutputDetailPageBytes
	outputViewerSearchLimit        = 256
	// Keep a useful navigable prefix while bounding search memory. Bubbles
	// applies highlights before horizontal clipping, so only a small moving
	// window is active in the renderer at once.
	outputViewerSearchMatchLimit   = 4096
	outputViewerHighlightLimit     = 20
	outputViewerCursorHistoryLimit = 256
)

// OutputViewerPageToken fences asynchronous page results by both viewer
// generation and request sequence. A token is intentionally scalar: the
// process-local output capability remains in OutputDetailPageRequest.
type OutputViewerPageToken struct {
	Generation uint64
	Sequence   uint64
}

// Valid reports whether the token can identify one page request.
func (token OutputViewerPageToken) Valid() bool {
	return token.Generation > 0 && token.Sequence > 0
}

// OutputViewerEventKind identifies the only intents a presentation-only
// OutputViewer may emit. The smart parent owns page IO, clipboard integration,
// and modal-stack mutation.
type OutputViewerEventKind uint8

const (
	OutputViewerEventNone OutputViewerEventKind = iota
	OutputViewerEventRequestPage
	OutputViewerEventCopyVisible
	OutputViewerEventClose
)

// OutputViewerEvent is a bounded child-to-parent intent. CopyText contains
// only the currently visible, terminal-safe output cells; it is never written
// to the transcript by this component.
type OutputViewerEvent struct {
	Kind     OutputViewerEventKind
	Origin   EntityRef
	Token    OutputViewerPageToken
	Request  OutputDetailPageRequest
	CopyText string
}

// Empty reports whether no intent was emitted.
func (event OutputViewerEvent) Empty() bool {
	return event.Kind == OutputViewerEventNone
}

// OutputViewerStatus is the presentation state of the one-page cache.
type OutputViewerStatus uint8

const (
	OutputViewerLoading OutputViewerStatus = iota
	OutputViewerReady
	OutputViewerUnavailable
)

// OutputViewerAnchor names the first visible source row rather than a
// presentation offset. Resizing, theme changes, and search-row insertion may
// all change viewport geometry without changing this semantic anchor.
type OutputViewerAnchor struct {
	SourceRow int
	Valid     bool
}

// OutputViewerLayout is the exact screen-space geometry used for both
// rendering and pointer routing. All rectangles are half-open.
type OutputViewerLayout struct {
	ScreenRect  CellRect
	OuterRect   CellRect
	ContentRect CellRect
	HeaderRect  CellRect
	SearchRect  CellRect
	BodyRect    CellRect
	FooterRect  CellRect
}

// ProjectOutputViewerLayout computes a centered modal whose preferred extent
// is 90% of the terminal, with a 72x12 floor that always clamps back to the
// actual screen. One border cell and one horizontal padding cell are deducted
// before assigning the header, optional search row, body, and footer.
func ProjectOutputViewerLayout(screenWidth, screenHeight int, searchVisible bool) OutputViewerLayout {
	screenWidth = max(0, screenWidth)
	screenHeight = max(0, screenHeight)
	screen := NewCellRect(0, 0, screenWidth, screenHeight)

	outerWidth := min(screenWidth, max(outputViewerMinimumOuterWidth, outputViewerNinetyPercent(screenWidth)))
	outerHeight := min(screenHeight, max(outputViewerMinimumOuterHeight, outputViewerNinetyPercent(screenHeight)))
	outerX := max(0, (screenWidth-outerWidth)/2)
	outerY := max(0, (screenHeight-outerHeight)/2)
	outer := NewCellRect(outerX, outerY, outerX+outerWidth, outerY+outerHeight)

	// Lip Gloss owns a one-cell border and horizontal padding of one cell.
	// There is no vertical padding, so header/body/footer consume every inner
	// row exactly once.
	content := Inset(outer, Insets{Top: 1, Right: 2, Bottom: 1, Left: 2})
	header, remain := TakeTop(content, 1)
	search := NewCellRect(remain.MinX, remain.MinY, remain.MinX, remain.MinY)
	if searchVisible {
		search, remain = TakeTop(remain, 1)
	}
	footer, body := TakeBottom(remain, 1)

	return OutputViewerLayout{
		ScreenRect:  screen,
		OuterRect:   outer,
		ContentRect: content,
		HeaderRect:  header,
		SearchRect:  search,
		BodyRect:    body,
		FooterRect:  footer,
	}
}

func outputViewerNinetyPercent(value int) int {
	if value <= 0 {
		return 0
	}
	// Quotient/remainder arithmetic avoids overflowing 9*value when tests or a
	// malformed terminal report exercise extreme integer sizes.
	return (value/10)*9 + (value%10)*9/10
}

type outputViewerPageDirection uint8

const (
	outputViewerPageInitial outputViewerPageDirection = iota
	outputViewerPageForward
	outputViewerPageBackward
)

type outputViewerPendingPage struct {
	token     OutputViewerPageToken
	cursor    OutputDetailCursor
	direction outputViewerPageDirection
}

type outputViewerMatch struct {
	pageRow   int
	startCell int
	endCell   int
}

type outputViewerCellSpan struct {
	start     int
	end       int
	byteStart int
	byteEnd   int
}

// OutputViewer is a one-page, presentation-only full-output inspector. It
// retains an ephemeral receipt but never a store, context, clipboard callback,
// transcript pointer, or raw tool payload.
type OutputViewer struct {
	Origin  EntityRef
	Receipt OutputDetailReceipt

	width         int
	height        int
	isDark        bool
	themeID       string
	glyphProfile  GlyphProfile
	reducedMotion bool
	styles        Styles
	layout        OutputViewerLayout
	status        OutputViewerStatus
	generation    uint64
	sequence      uint64

	page          OutputDetailPage
	currentCursor OutputDetailCursor
	pending       outputViewerPendingPage
	history       []OutputDetailCursor

	viewport viewport.Model
	search   textinput.Model

	searchVisible bool
	matches       []outputViewerMatch
	matchBounds   [][2]int
	matchesCapped bool
	selectedMatch int
	highlightFrom int
	highlightTo   int
	selectedStyle lipgloss.Style
	anchor        OutputViewerAnchor
	notice        string
}

// NewOutputViewer creates an isolated viewer. The caller starts page loading
// by forwarding InitialPageRequest to the smart parent.
func NewOutputViewer(
	origin EntityRef,
	receipt OutputDetailReceipt,
	width, height int,
	isDark bool,
	profiles ...GlyphProfile,
) *OutputViewer {
	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "search loaded page"
	search.CharLimit = outputViewerSearchLimit
	search.SetVirtualCursor(false)

	viewer := &OutputViewer{
		Origin:        origin,
		Receipt:       receipt,
		width:         max(0, width),
		height:        max(0, height),
		isDark:        isDark,
		glyphProfile:  resolveGlyphProfile(profiles...),
		status:        OutputViewerLoading,
		generation:    1,
		search:        search,
		selectedMatch: -1,
	}
	viewer.viewport = viewport.New(viewport.WithWidth(1), viewport.WithHeight(1))
	viewer.viewport.FillHeight = true
	viewer.viewport.SoftWrap = false
	viewer.applyTheme()
	viewer.reproject()
	if !viewer.validIdentity() {
		viewer.status = OutputViewerUnavailable
		viewer.notice = "Output is no longer available."
		viewer.refreshViewport(OutputViewerAnchor{})
	}
	return viewer
}

// InitialPageRequest emits the first bounded request exactly once. Repeated
// calls while a request is pending or after a result has settled are no-ops.
func (viewer *OutputViewer) InitialPageRequest() OutputViewerEvent {
	if viewer == nil || viewer.status != OutputViewerLoading ||
		viewer.pending.token.Valid() || viewer.sequence > 0 || !viewer.validIdentity() {
		return OutputViewerEvent{}
	}
	return viewer.beginPageRequest(OutputDetailCursor{}, outputViewerPageInitial)
}

// ReplaceReceipt starts a fresh generation while keeping the same semantic
// origin and modal instance. Late results from the previous receipt cannot
// mutate the replacement page.
func (viewer *OutputViewer) ReplaceReceipt(receipt OutputDetailReceipt) OutputViewerEvent {
	if viewer == nil {
		return OutputViewerEvent{}
	}
	viewer.Receipt = receipt
	viewer.generation++
	if viewer.generation == 0 {
		viewer.generation = 1
	}
	viewer.sequence = 0
	viewer.status = OutputViewerLoading
	viewer.page = OutputDetailPage{}
	viewer.currentCursor = OutputDetailCursor{}
	viewer.pending = outputViewerPendingPage{}
	viewer.history = nil
	viewer.resetMatches()
	viewer.anchor = OutputViewerAnchor{}
	viewer.notice = ""
	viewer.refreshViewport(OutputViewerAnchor{})
	if !viewer.validIdentity() {
		viewer.status = OutputViewerUnavailable
		viewer.notice = "Output is no longer available."
		viewer.refreshViewport(OutputViewerAnchor{})
		return OutputViewerEvent{}
	}
	return viewer.InitialPageRequest()
}

// Status returns the viewer's current presentation state.
func (viewer *OutputViewer) Status() OutputViewerStatus {
	if viewer == nil {
		return OutputViewerUnavailable
	}
	return viewer.status
}

// Layout returns the current immutable geometry snapshot.
func (viewer *OutputViewer) Layout() OutputViewerLayout {
	if viewer == nil {
		return OutputViewerLayout{}
	}
	return viewer.layout
}

// Anchor returns the first visible semantic source row.
func (viewer *OutputViewer) Anchor() OutputViewerAnchor {
	if viewer == nil {
		return OutputViewerAnchor{}
	}
	viewer.captureAnchor()
	return viewer.anchor
}

// CachedRowCount exposes only the cache cardinality for diagnostics and tests.
// Output content remains private to the viewer.
func (viewer *OutputViewer) CachedRowCount() int {
	if viewer == nil {
		return 0
	}
	return len(viewer.page.Rows)
}

// SetSize reprojects geometry while preserving the first visible source row.
func (viewer *OutputViewer) SetSize(width, height int) {
	if viewer == nil {
		return
	}
	width = max(0, width)
	height = max(0, height)
	if viewer.width == width && viewer.height == height {
		return
	}
	anchor := viewer.Anchor()
	viewer.width = width
	viewer.height = height
	viewer.reproject()
	viewer.restoreAnchor(anchor)
}

// SetTheme changes adaptive presentation styles without moving the semantic
// source-row anchor.
func (viewer *OutputViewer) SetTheme(isDark bool, themeID string) {
	if viewer == nil || viewer.isDark == isDark {
		return
	}
	anchor := viewer.Anchor()
	viewer.isDark = isDark
	viewer.applyTheme()
	viewer.restoreAnchor(anchor)
}

// SetReducedMotion keeps the search input static for users who disable
// animation. It changes only cursor presentation; semantic focus, query,
// geometry, and source-row position remain stable.
func (viewer *OutputViewer) SetReducedMotion(reduced bool) {
	if viewer == nil || viewer.reducedMotion == reduced {
		return
	}
	anchor := viewer.Anchor()
	viewer.reducedMotion = reduced
	viewer.search.SetStyles(semanticTextInputStyles(viewer.isDark, viewer.themeID, reduced))
	viewer.restoreAnchor(anchor)
}

// ApplyPageResult settles only the exact current request token. Any error,
// invalid projection, unavailable/evicted ref, or digest mismatch fails closed
// to the same non-actionable state without exposing transport details.
func (viewer *OutputViewer) ApplyPageResult(
	token OutputViewerPageToken,
	page OutputDetailPage,
	err error,
) bool {
	if viewer == nil || !token.Valid() || token != viewer.pending.token ||
		token.Generation != viewer.generation {
		return false
	}

	pending := viewer.pending
	viewer.pending = outputViewerPendingPage{}
	if err != nil || !viewer.validPage(pending.cursor, page) {
		viewer.status = OutputViewerUnavailable
		viewer.page = OutputDetailPage{}
		viewer.resetMatches()
		viewer.anchor = OutputViewerAnchor{}
		viewer.notice = outputViewerUnavailableNotice(err)
		viewer.refreshViewport(OutputViewerAnchor{})
		return true
	}

	switch pending.direction {
	case outputViewerPageForward:
		viewer.pushHistory(viewer.currentCursor)
	case outputViewerPageBackward:
		if last := len(viewer.history) - 1; last >= 0 && viewer.history[last] == pending.cursor {
			viewer.history = viewer.history[:last]
		}
	}

	viewer.currentCursor = pending.cursor
	viewer.page = cloneOutputDetailPageForViewer(page)
	viewer.status = OutputViewerReady
	viewer.notice = ""
	viewer.resetMatches()
	viewer.refreshViewport(OutputViewerAnchor{})
	viewer.recomputeMatches()
	return true
}

// ApplyPage is the successful-result convenience form.
func (viewer *OutputViewer) ApplyPage(token OutputViewerPageToken, page OutputDetailPage) bool {
	return viewer.ApplyPageResult(token, page, nil)
}

// ApplyPageError is the failed-result convenience form.
func (viewer *OutputViewer) ApplyPageError(token OutputViewerPageToken, err error) bool {
	if err == nil {
		err = ErrOutputDetailUnavailable
	}
	return viewer.ApplyPageResult(token, OutputDetailPage{}, err)
}

// Update handles Bubbles presentation messages and returns typed intent for
// its smart parent. It performs no page IO, clipboard writes, transcript
// mutation, or modal-stack mutation.
func (viewer *OutputViewer) Update(msg tea.Msg) (OutputViewerEvent, tea.Cmd) {
	if viewer == nil {
		return OutputViewerEvent{}, nil
	}
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		viewer.SetSize(size.Width, size.Height)
		return OutputViewerEvent{}, nil
	}
	if wheel, ok := msg.(tea.MouseWheelMsg); ok {
		if viewer.status != OutputViewerReady ||
			!viewer.layout.BodyRect.Contains(wheel.X, wheel.Y) {
			return OutputViewerEvent{}, nil
		}
		beforeX, beforeY := viewer.viewport.XOffset(), viewer.viewport.YOffset()
		var command tea.Cmd
		viewer.viewport, command = viewer.viewport.Update(wheel)
		if viewer.viewport.XOffset() != beforeX || viewer.viewport.YOffset() != beforeY {
			viewer.installHighlightWindow(-1)
		}
		viewer.captureAnchor()
		return OutputViewerEvent{}, command
	}
	if click, ok := msg.(tea.MouseClickMsg); ok {
		if click.Button != tea.MouseLeft {
			return OutputViewerEvent{}, nil
		}
		if viewer.searchVisible && viewer.layout.SearchRect.Contains(click.X, click.Y) {
			return OutputViewerEvent{}, viewer.search.Focus()
		}
		if viewer.layout.BodyRect.Contains(click.X, click.Y) && viewer.search.Focused() {
			viewer.search.Blur()
		}
		// Output rows are copyable viewport content, not a second selection
		// model. The exact modal still owns every click inside its frame.
		return OutputViewerEvent{}, nil
	}

	keyMsg, isKey := msg.(tea.KeyPressMsg)
	if viewer.search.Focused() {
		if isKey && keyMsg.Code == tea.KeyEscape {
			viewer.closeSearch()
			return OutputViewerEvent{}, nil
		}
		if isKey && keyMsg.Code == tea.KeyEnter {
			viewer.search.Blur()
			return OutputViewerEvent{}, nil
		}
		before := viewer.search.Value()
		var command tea.Cmd
		viewer.search, command = viewer.search.Update(msg)
		viewer.normalizeSearchValue()
		if viewer.search.Value() != before {
			viewer.recomputeMatches()
		}
		return OutputViewerEvent{}, command
	}
	if !isKey {
		return OutputViewerEvent{}, nil
	}
	if viewer.status == OutputViewerReady {
		viewer.notice = ""
	}

	switch {
	case keyMsg.Code == tea.KeyEscape:
		if viewer.searchVisible {
			viewer.closeSearch()
			return OutputViewerEvent{}, nil
		}
		return OutputViewerEvent{Kind: OutputViewerEventClose, Origin: viewer.Origin}, nil
	case outputViewerKeyText(keyMsg) == "/":
		if viewer.status != OutputViewerReady || viewer.pending.token.Valid() {
			return OutputViewerEvent{}, nil
		}
		if !viewer.searchVisible {
			anchor := viewer.Anchor()
			viewer.searchVisible = true
			viewer.reproject()
			viewer.restoreAnchor(anchor)
		}
		return OutputViewerEvent{}, viewer.search.Focus()
	case outputViewerKeyText(keyMsg) == "n":
		viewer.navigateMatch(1)
		return OutputViewerEvent{}, nil
	case outputViewerKeyText(keyMsg) == "N":
		viewer.navigateMatch(-1)
		return OutputViewerEvent{}, nil
	case outputViewerKeyText(keyMsg) == "]":
		return viewer.requestNextPage(), nil
	case outputViewerKeyText(keyMsg) == "[":
		return viewer.requestPreviousPage(), nil
	case outputViewerKeyText(keyMsg) == "c":
		if viewer.status != OutputViewerReady || viewer.pending.token.Valid() {
			return OutputViewerEvent{}, nil
		}
		visible := viewer.visibleOutput()
		if visible == "" {
			return OutputViewerEvent{}, nil
		}
		return OutputViewerEvent{
			Kind: OutputViewerEventCopyVisible, Origin: viewer.Origin, CopyText: visible,
		}, nil
	default:
		if viewer.status != OutputViewerReady {
			return OutputViewerEvent{}, nil
		}
		beforeX, beforeY := viewer.viewport.XOffset(), viewer.viewport.YOffset()
		var command tea.Cmd
		viewer.viewport, command = viewer.viewport.Update(keyMsg)
		if viewer.viewport.XOffset() != beforeX || viewer.viewport.YOffset() != beforeY {
			viewer.installHighlightWindow(-1)
		}
		viewer.captureAnchor()
		return OutputViewerEvent{}, command
	}
}

// View renders an exact-size modal frame and intentionally drops the optional
// child hardware cursor. Parents that route focus should use ViewWithCursor.
func (viewer *OutputViewer) View() string {
	view, _ := viewer.ViewWithCursor()
	return view
}

// ViewWithCursor returns one cursor at most, local to the rendered frame. It is
// present only while the search Bubbles input owns focus.
func (viewer *OutputViewer) ViewWithCursor() (string, *tea.Cursor) {
	if viewer == nil || viewer.layout.OuterRect.Empty() {
		return "", nil
	}
	if viewer.layout.OuterRect.Width() < 4 || viewer.layout.OuterRect.Height() < 3 {
		return outputViewerRecoveryFrame(
			viewer.layout.OuterRect.Width(),
			viewer.layout.OuterRect.Height(),
		), nil
	}
	contentWidth := viewer.layout.ContentRect.Width()
	rows := make([]string, 0, viewer.layout.ContentRect.Height())
	rows = append(rows, outputViewerFitRow(viewer.styles.OverlayTitle.Render(viewer.headerText()), contentWidth))
	if viewer.searchVisible {
		rows = append(rows, outputViewerFitRow(viewer.search.View(), contentWidth))
	}

	bodyRows := outputViewerExactRows(viewer.viewport.View(), contentWidth, viewer.layout.BodyRect.Height())
	rows = append(rows, bodyRows...)
	rows = append(rows, outputViewerFitRow(viewer.styles.OverlayDim.Render(viewer.footerText()), contentWidth))
	rows = outputViewerExactRows(strings.Join(rows, "\n"), contentWidth, viewer.layout.ContentRect.Height())

	frame := lipgloss.NewStyle().
		Border(borderForGlyphProfile(viewer.glyphProfile)).
		BorderForeground(viewer.styles.FocusIndicator.GetForeground()).
		Padding(0, 1).
		Width(viewer.layout.OuterRect.Width()).
		Height(viewer.layout.OuterRect.Height()).
		Render(strings.Join(rows, "\n"))

	if !viewer.searchVisible || !viewer.search.Focused() {
		return frame, nil
	}
	cursor := viewer.search.Cursor()
	if cursor == nil || viewer.layout.SearchRect.Empty() {
		return frame, nil
	}
	local := *cursor
	local.X += viewer.layout.ContentRect.MinX - viewer.layout.OuterRect.MinX
	local.Y = viewer.layout.SearchRect.MinY - viewer.layout.OuterRect.MinY
	searchLocal := NewCellRect(
		viewer.layout.SearchRect.MinX-viewer.layout.OuterRect.MinX,
		viewer.layout.SearchRect.MinY-viewer.layout.OuterRect.MinY,
		viewer.layout.SearchRect.MaxX-viewer.layout.OuterRect.MinX,
		viewer.layout.SearchRect.MaxY-viewer.layout.OuterRect.MinY,
	)
	if searchLocal.Empty() {
		return frame, nil
	}
	local.X = min(max(local.X, searchLocal.MinX), searchLocal.MaxX-1)
	local.Y = min(max(local.Y, searchLocal.MinY), searchLocal.MaxY-1)
	return frame, &local
}

func (viewer *OutputViewer) validIdentity() bool {
	return viewer != nil && viewer.Origin.Valid() && viewer.Origin.BlockID.Valid() &&
		viewer.Receipt.Ref.Valid() && viewer.Receipt.Digest.Valid()
}

func (viewer *OutputViewer) validPage(cursor OutputDetailCursor, page OutputDetailPage) bool {
	if viewer == nil || page.Digest != viewer.Receipt.Digest ||
		cursor.Row < 0 || cursor.ByteOffset < 0 ||
		page.Next.Row < 0 || page.Next.ByteOffset < 0 ||
		len(page.Rows) > outputViewerPageRowLimit ||
		page.Bytes > uint64(outputViewerPageByteLimit) {
		return false
	}
	if uint64(cursor.Row) > viewer.Receipt.Digest.RetainedRows ||
		uint64(page.Next.Row) > viewer.Receipt.Digest.RetainedRows {
		return false
	}
	if page.HasMore && len(page.Rows) == 0 {
		return false
	}
	if page.HasMore && !outputDetailCursorAfter(page.Next, cursor) {
		return false
	}
	if len(page.Rows) == 0 {
		return !page.HasMore &&
			cursor == (OutputDetailCursor{Row: int(viewer.Receipt.Digest.RetainedRows)}) &&
			page.Next == cursor && page.Bytes == 0
	}

	var bytes uint64
	expectedCursor := cursor
	for index, row := range page.Rows {
		if row.Index < 0 || uint64(row.Index) >= viewer.Receipt.Digest.RetainedRows ||
			row.Index != expectedCursor.Row ||
			row.StartsMidRow != (index == 0 && cursor.ByteOffset > 0) ||
			row.SourceRowComplete && !row.EndsRow ||
			strings.ContainsAny(row.Text, "\r\n") ||
			len(row.Text) > outputViewerPageByteLimit-int(bytes) {
			return false
		}
		bytes += uint64(len(row.Text))
		if row.EndsRow {
			expectedCursor = OutputDetailCursor{Row: row.Index + 1}
			continue
		}
		if index != len(page.Rows)-1 || !page.HasMore {
			return false
		}
		expectedCursor.ByteOffset += len(row.Text)
	}
	if page.Next != expectedCursor || bytes != page.Bytes {
		return false
	}
	if page.HasMore {
		return uint64(page.Next.Row) < viewer.Receipt.Digest.RetainedRows
	}
	return page.Next == (OutputDetailCursor{Row: int(viewer.Receipt.Digest.RetainedRows)})
}

func outputDetailCursorAfter(candidate, current OutputDetailCursor) bool {
	return candidate.Row > current.Row ||
		(candidate.Row == current.Row && candidate.ByteOffset > current.ByteOffset)
}

func cloneOutputDetailPageForViewer(page OutputDetailPage) OutputDetailPage {
	clone := page
	clone.Rows = make([]OutputDetailRow, len(page.Rows))
	copy(clone.Rows, page.Rows)
	for index := range clone.Rows {
		clone.Rows[index].Text = sanitizeTerminalLine(clone.Rows[index].Text)
	}
	return clone
}

func outputViewerUnavailableNotice(err error) string {
	switch {
	case errors.Is(err, ErrOutputDetailCursor):
		return "Output page is no longer available."
	default:
		// Unknown, stale, evicted, cancelled, and transport failures deliberately
		// collapse to the same viewer posture.
		return "Output is no longer available."
	}
}

func (viewer *OutputViewer) beginPageRequest(
	cursor OutputDetailCursor,
	direction outputViewerPageDirection,
) OutputViewerEvent {
	if viewer == nil || viewer.pending.token.Valid() || !viewer.validIdentity() {
		return OutputViewerEvent{}
	}
	viewer.sequence++
	if viewer.sequence == 0 {
		viewer.sequence = 1
	}
	token := OutputViewerPageToken{Generation: viewer.generation, Sequence: viewer.sequence}
	viewer.pending = outputViewerPendingPage{token: token, cursor: cursor, direction: direction}
	viewer.notice = "Loading page..."
	return OutputViewerEvent{
		Kind:   OutputViewerEventRequestPage,
		Origin: viewer.Origin,
		Token:  token,
		Request: OutputDetailPageRequest{
			Ref:       viewer.Receipt.Ref,
			Cursor:    cursor,
			RowLimit:  outputViewerPageRowLimit,
			ByteLimit: outputViewerPageByteLimit,
		},
	}
}

func (viewer *OutputViewer) requestNextPage() OutputViewerEvent {
	if viewer == nil || viewer.status != OutputViewerReady ||
		viewer.pending.token.Valid() || !viewer.page.HasMore {
		return OutputViewerEvent{}
	}
	return viewer.beginPageRequest(viewer.page.Next, outputViewerPageForward)
}

func (viewer *OutputViewer) requestPreviousPage() OutputViewerEvent {
	if viewer == nil || viewer.status != OutputViewerReady ||
		viewer.pending.token.Valid() || len(viewer.history) == 0 {
		return OutputViewerEvent{}
	}
	return viewer.beginPageRequest(viewer.history[len(viewer.history)-1], outputViewerPageBackward)
}

func (viewer *OutputViewer) pushHistory(cursor OutputDetailCursor) {
	if len(viewer.history) == outputViewerCursorHistoryLimit {
		copy(viewer.history, viewer.history[1:])
		viewer.history[len(viewer.history)-1] = cursor
		return
	}
	viewer.history = append(viewer.history, cursor)
}

func (viewer *OutputViewer) applyTheme() {
	viewer.styles = NewStyles(viewer.isDark, viewer.themeID)
	viewer.search.SetStyles(semanticTextInputStyles(viewer.isDark, viewer.themeID, viewer.reducedMotion))
	palette := outputSemanticPalette(viewer.isDark, viewer.themeID)
	viewer.viewport.HighlightStyle = lipgloss.NewStyle().
		Foreground(palette.Text).
		Underline(true)
	viewer.selectedStyle = lipgloss.NewStyle().
		Foreground(palette.Accent).
		Bold(true).
		Underline(true)
	if viewer.selectedMatch >= 0 {
		viewer.viewport.SelectedHighlightStyle = viewer.selectedStyle
	} else {
		// Bubbles owns the highlight cursor. Before the user presses n/N its
		// internal first match is intentionally rendered like every other
		// match, preserving the viewer's no-selection state.
		viewer.viewport.SelectedHighlightStyle = viewer.viewport.HighlightStyle
	}
	viewer.viewport.StyleLineFunc = nil
	viewer.configureGutter()
}

func (viewer *OutputViewer) reproject() {
	viewer.layout = ProjectOutputViewerLayout(viewer.width, viewer.height, viewer.searchVisible)
	viewer.viewport.SetWidth(viewer.layout.BodyRect.Width())
	viewer.viewport.SetHeight(viewer.layout.BodyRect.Height())
	searchWidth := max(1, viewer.layout.SearchRect.Width()-lipgloss.Width(viewer.search.Prompt)-1)
	viewer.search.SetWidth(searchWidth)
	viewer.configureGutter()
}

func (viewer *OutputViewer) configureGutter() {
	if viewer == nil {
		return
	}
	digits := len(fmt.Sprintf("%d", max(uint64(1), viewer.Receipt.Digest.RetainedRows)))
	digits = min(8, max(1, digits))
	style := viewer.styles.OverlayDim
	viewer.viewport.LeftGutterFunc = func(context viewport.GutterContext) string {
		if context.Soft || context.Index < 0 || context.Index >= len(viewer.page.Rows) {
			return strings.Repeat(" ", digits+3)
		}
		row := viewer.page.Rows[context.Index]
		label := fmt.Sprintf("%*d | ", digits, row.Index+1)
		return style.Render(label)
	}
}

func (viewer *OutputViewer) refreshViewport(anchor OutputViewerAnchor) {
	switch viewer.status {
	case OutputViewerReady:
		lines := make([]string, len(viewer.page.Rows))
		for index, row := range viewer.page.Rows {
			lines[index] = sanitizeTerminalLine(row.Text)
		}
		viewer.viewport.SetContentLines(lines)
	default:
		viewer.viewport.SetContentLines(nil)
	}
	viewer.configureGutter()
	viewer.restoreAnchor(anchor)
}

func (viewer *OutputViewer) captureAnchor() {
	if viewer == nil || viewer.status != OutputViewerReady || len(viewer.page.Rows) == 0 {
		return
	}
	index := min(max(0, viewer.viewport.YOffset()), len(viewer.page.Rows)-1)
	viewer.anchor = OutputViewerAnchor{SourceRow: viewer.page.Rows[index].Index, Valid: true}
}

func (viewer *OutputViewer) restoreAnchor(anchor OutputViewerAnchor) {
	if viewer == nil || viewer.status != OutputViewerReady || len(viewer.page.Rows) == 0 {
		viewer.anchor = OutputViewerAnchor{}
		viewer.viewport.SetYOffset(0)
		return
	}
	index := 0
	if anchor.Valid {
		index = len(viewer.page.Rows) - 1
		for pageIndex, row := range viewer.page.Rows {
			if row.Index >= anchor.SourceRow {
				index = pageIndex
				break
			}
		}
	}
	viewer.viewport.SetYOffset(index)
	viewer.captureAnchor()
}

func (viewer *OutputViewer) normalizeSearchValue() {
	if viewer == nil {
		return
	}
	value := strings.ReplaceAll(sanitizeTerminalLine(viewer.search.Value()), "\t", " ")
	if value != viewer.search.Value() {
		viewer.search.SetValue(value)
		viewer.search.CursorEnd()
	}
}

func (viewer *OutputViewer) recomputeMatches() {
	if viewer == nil {
		return
	}
	viewer.resetMatches()
	query := viewer.search.Value()
	if strings.TrimSpace(query) == "" || viewer.status != OutputViewerReady {
		return
	}
	expression, err := regexp.Compile("(?i)" + regexp.QuoteMeta(query))
	if err != nil {
		return
	}
	contentByteOffset := 0
	for index, row := range viewer.page.Rows {
		line := sanitizeTerminalLine(row.Text)
		lineByteOffset := contentByteOffset
		contentByteOffset += len(line)
		if index+1 < len(viewer.page.Rows) {
			contentByteOffset++
		}

		remaining := outputViewerSearchMatchLimit - len(viewer.matches)
		if remaining == 0 {
			if expression.FindStringIndex(line) != nil {
				viewer.matchesCapped = true
				break
			}
			continue
		}

		// Ask for one extra match so the cap is reported only when there is
		// known additional content. Dense rows stop after a bounded prefix.
		bounds := expression.FindAllStringIndex(line, remaining+1)
		if len(bounds) > remaining {
			viewer.matchesCapped = true
			bounds = bounds[:remaining]
		}
		if len(bounds) == 0 {
			continue
		}

		for _, span := range outputViewerMatchCellSpans(line, bounds) {
			viewer.matches = append(viewer.matches, outputViewerMatch{
				pageRow: index, startCell: span.start, endCell: span.end,
			})
			viewer.matchBounds = append(viewer.matchBounds, [2]int{
				lineByteOffset + span.byteStart,
				lineByteOffset + span.byteEnd,
			})
		}
		if viewer.matchesCapped {
			break
		}
	}
	viewer.viewport.SetXOffset(0)
	viewer.installHighlightWindow(-1)
}

// outputViewerMatchCellSpans projects sorted, non-overlapping regexp byte
// ranges into terminal cells in one grapheme pass. A regexp boundary inside a
// combining or ZWJ sequence expands to the whole rendered grapheme, so search
// navigation never asks the viewport to reveal half of a visible glyph.
func outputViewerMatchCellSpans(value string, bounds [][]int) []outputViewerCellSpan {
	if len(bounds) == 0 {
		return nil
	}

	spans := make([]outputViewerCellSpan, len(bounds))
	for index := range spans {
		spans[index].start = -1
		spans[index].end = -1
	}

	matchIndex := 0
	cellOffset := 0
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() && matchIndex < len(bounds) {
		byteStart, byteEnd := graphemes.Positions()
		cellEnd := cellOffset + lipgloss.Width(graphemes.Str())

		for matchIndex < len(bounds) && bounds[matchIndex][1] <= byteStart {
			matchIndex++
		}
		for index := matchIndex; index < len(bounds) && bounds[index][0] < byteEnd; index++ {
			if bounds[index][1] <= byteStart {
				continue
			}
			if spans[index].start < 0 {
				spans[index].start = cellOffset
				spans[index].byteStart = byteStart
			}
			spans[index].end = cellEnd
			spans[index].byteEnd = byteEnd
		}
		for matchIndex < len(bounds) && bounds[matchIndex][1] <= byteEnd {
			matchIndex++
		}
		cellOffset = cellEnd
	}

	// regexp ranges over value are expected to intersect a grapheme. Keep the
	// helper total for malformed internal input without rescanning the string.
	for index := range spans {
		if spans[index].start < 0 {
			spans[index] = outputViewerCellSpan{
				start: cellOffset, end: cellOffset,
				byteStart: len(value), byteEnd: len(value),
			}
		}
	}
	return spans
}

func (viewer *OutputViewer) installHighlightWindow(preferred int) {
	if viewer == nil || len(viewer.matchBounds) == 0 {
		return
	}

	anchor := preferred
	if anchor < 0 || anchor >= len(viewer.matches) {
		yOffset := viewer.viewport.YOffset()
		xOffset := viewer.viewport.XOffset()
		anchor = sort.Search(len(viewer.matches), func(index int) bool {
			match := viewer.matches[index]
			return match.pageRow > yOffset ||
				(match.pageRow == yOffset && match.endCell > xOffset)
		})
		if anchor == len(viewer.matches) {
			anchor = len(viewer.matches) - 1
		}
	}

	start := anchor
	if preferred >= 0 && preferred < len(viewer.matches) {
		start = max(0, preferred-outputViewerHighlightLimit/2)
	}
	end := min(len(viewer.matchBounds), start+outputViewerHighlightLimit)
	start = max(0, end-outputViewerHighlightLimit)
	bounds := make([][]int, end-start)
	for index := range bounds {
		match := viewer.matchBounds[start+index]
		bounds[index] = []int{match[0], match[1]}
	}

	// SetHighlights normally selects the nearest match. Give it a temporary
	// zero-height viewport positioned after the content so its private cursor
	// starts at -1, matching the viewer's no-selection state. Restore geometry
	// and position immediately; no transient projection reaches View.
	yOffset := viewer.viewport.YOffset()
	xOffset := viewer.viewport.XOffset()
	height := viewer.viewport.Height()
	viewer.viewport.ClearHighlights()
	viewer.viewport.SetHeight(0)
	viewer.viewport.SetYOffset(len(viewer.page.Rows))
	viewer.viewport.SetHighlights(bounds)
	viewer.viewport.SetHeight(height)
	viewer.viewport.SetYOffset(yOffset)
	viewer.viewport.SetXOffset(xOffset)
	viewer.highlightFrom = start
	viewer.highlightTo = end

	if viewer.selectedMatch >= start && viewer.selectedMatch < end {
		viewer.viewport.SelectedHighlightStyle = viewer.selectedStyle
		for range viewer.selectedMatch - start + 1 {
			viewer.viewport.HighlightNext()
		}
		return
	}
	viewer.viewport.SelectedHighlightStyle = viewer.viewport.HighlightStyle
}

func (viewer *OutputViewer) resetMatches() {
	if viewer == nil {
		return
	}
	viewer.matches = nil
	viewer.matchBounds = nil
	viewer.matchesCapped = false
	viewer.selectedMatch = -1
	viewer.highlightFrom = 0
	viewer.highlightTo = 0
	viewer.viewport.ClearHighlights()
	viewer.viewport.SelectedHighlightStyle = viewer.viewport.HighlightStyle
}

func (viewer *OutputViewer) navigateMatch(delta int) {
	if viewer == nil || viewer.status != OutputViewerReady || len(viewer.matches) == 0 || delta == 0 {
		return
	}
	previous := viewer.selectedMatch
	if viewer.selectedMatch < 0 {
		if delta < 0 {
			viewer.selectedMatch = len(viewer.matches) - 1
		} else {
			viewer.selectedMatch = 0
		}
	} else {
		steps := delta % len(viewer.matches)
		viewer.selectedMatch = (viewer.selectedMatch + steps + len(viewer.matches)) % len(viewer.matches)
	}

	// The common adjacent path stays inside Bubbles' active highlight window,
	// so n/N advances its cursor without reparsing content. Crossing a window
	// edge installs a new bounded window centered on the target.
	switch {
	case previous >= viewer.highlightFrom && previous < viewer.highlightTo &&
		viewer.selectedMatch == previous+1 && viewer.selectedMatch < viewer.highlightTo:
		viewer.viewport.HighlightNext()
		viewer.viewport.SelectedHighlightStyle = viewer.selectedStyle
	case previous >= viewer.highlightFrom && previous < viewer.highlightTo &&
		viewer.selectedMatch == previous-1 && viewer.selectedMatch >= viewer.highlightFrom:
		viewer.viewport.HighlightPrevious()
		viewer.viewport.SelectedHighlightStyle = viewer.selectedStyle
	case len(viewer.matches) <= outputViewerHighlightLimit &&
		previous == len(viewer.matches)-1 && viewer.selectedMatch == 0:
		viewer.viewport.HighlightNext()
		viewer.viewport.SelectedHighlightStyle = viewer.selectedStyle
	case len(viewer.matches) <= outputViewerHighlightLimit &&
		previous == 0 && viewer.selectedMatch == len(viewer.matches)-1:
		viewer.viewport.HighlightPrevious()
		viewer.viewport.SelectedHighlightStyle = viewer.selectedStyle
	default:
		viewer.installHighlightWindow(viewer.selectedMatch)
	}
	viewer.captureAnchor()
}

func (viewer *OutputViewer) closeSearch() {
	if viewer == nil || !viewer.searchVisible {
		return
	}
	anchor := viewer.Anchor()
	viewer.search.Blur()
	viewer.search.Reset()
	viewer.searchVisible = false
	viewer.resetMatches()
	viewer.reproject()
	viewer.restoreAnchor(anchor)
}

func (viewer *OutputViewer) visibleOutput() string {
	if viewer == nil || viewer.status != OutputViewerReady || len(viewer.page.Rows) == 0 ||
		viewer.layout.BodyRect.Empty() {
		return ""
	}
	start := min(max(0, viewer.viewport.YOffset()), len(viewer.page.Rows))
	end := min(len(viewer.page.Rows), start+viewer.layout.BodyRect.Height())
	if start >= end {
		return ""
	}
	xOffset := viewer.viewport.XOffset()
	gutterWidth := outputViewerGutterWidth(viewer)
	lineWidth := max(0, viewer.layout.BodyRect.Width()-gutterWidth)
	if lineWidth == 0 {
		return ""
	}
	lines := make([]string, 0, end-start)
	for _, row := range viewer.page.Rows[start:end] {
		line := ansi.Cut(sanitizeTerminalLine(row.Text), xOffset, xOffset+lineWidth)
		lines = append(lines, line)
	}
	return sanitizeTerminalMultiline(strings.Join(lines, "\n"))
}

func outputViewerGutterWidth(viewer *OutputViewer) int {
	if viewer == nil || viewer.viewport.LeftGutterFunc == nil {
		return 0
	}
	return lipgloss.Width(viewer.viewport.LeftGutterFunc(viewport.GutterContext{}))
}

func (viewer *OutputViewer) headerText() string {
	if viewer == nil {
		return "Output"
	}
	switch viewer.status {
	case OutputViewerUnavailable:
		return "Output | unavailable"
	case OutputViewerLoading:
		return "Output | loading"
	}
	if len(viewer.page.Rows) == 0 {
		return "Output | empty"
	}
	first := viewer.page.Rows[0].Index + 1
	last := viewer.page.Rows[len(viewer.page.Rows)-1].Index + 1
	total := viewer.Receipt.Digest.TotalRows
	if viewer.Receipt.Digest.Truncated {
		return fmt.Sprintf("Output | rows %d-%d of %d | retained prefix", first, last, total)
	}
	return fmt.Sprintf("Output | rows %d-%d of %d", first, last, total)
}

func (viewer *OutputViewer) footerText() string {
	if viewer == nil {
		return ""
	}
	if viewer.status == OutputViewerUnavailable {
		return outputViewerFallback(viewer.notice, "Output is no longer available.") + " | esc close"
	}
	if viewer.status == OutputViewerLoading {
		return "Loading page... | esc close"
	}
	if viewer.pending.token.Valid() {
		return "Loading page... | c unavailable | esc close"
	}
	if notice := sanitizeTerminalSingleLine(viewer.notice); notice != "" {
		return notice + " | esc close"
	}

	scope := "loaded page"
	if viewer.currentCursor == (OutputDetailCursor{}) &&
		!viewer.page.HasMore && !viewer.Receipt.Digest.Truncated {
		scope = "complete output"
	}
	search := "/ search"
	if query := strings.TrimSpace(viewer.search.Value()); viewer.searchVisible && query != "" {
		noun := "matches"
		if len(viewer.matches) == 1 {
			noun = "match"
		}
		if viewer.matchesCapped {
			search = fmt.Sprintf("at least %d %s in %s", len(viewer.matches), noun, scope)
		} else {
			search = fmt.Sprintf("%d %s in %s", len(viewer.matches), noun, scope)
		}
		if len(viewer.matches) > outputViewerHighlightLimit {
			search += fmt.Sprintf(" | %d nearby highlighted", outputViewerHighlightLimit)
		}
		search += " | n/N move"
	}
	page := ""
	if len(viewer.history) > 0 || viewer.page.HasMore {
		page = "[/] page | "
	}
	return page + search + " | c copy visible | esc close"
}

func outputViewerFallback(value, fallback string) string {
	value = sanitizeTerminalSingleLine(value)
	if value == "" {
		return fallback
	}
	return value
}

func outputViewerKeyText(message tea.KeyPressMsg) string {
	if message.Mod&^tea.ModShift != 0 {
		return ""
	}
	if message.Text != "" {
		return message.Text
	}
	if message.Code > 0 && message.Code <= 0x10ffff {
		return string(message.Code)
	}
	return ""
}

func outputViewerFitRow(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = ansi.Truncate(value, width, "")
	return lipgloss.NewStyle().Width(width).MaxWidth(width).Render(value)
}

func outputViewerExactRows(value string, width, height int) []string {
	if height <= 0 {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(value, "\n"), "\n")
	if value == "" {
		lines = nil
	}
	rows := make([]string, height)
	for index := range height {
		if index < len(lines) {
			rows[index] = outputViewerFitRow(lines[index], width)
		} else {
			rows[index] = outputViewerFitRow("", width)
		}
	}
	return rows
}

func outputViewerRecoveryFrame(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxWidth(width).
		MaxHeight(height).
		Render(outputViewerFitRow("Output", width))
}
