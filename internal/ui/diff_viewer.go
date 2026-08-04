package ui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

const (
	diffViewerMinimumOuterWidth     = 72
	diffViewerMinimumOuterHeight    = 12
	diffViewerOuterScaleNumerator   = 9
	diffViewerOuterScaleDenominator = 10

	diffViewerMinimumUnifiedCodeColumns = 40
	diffViewerMinimumSplitCodeColumns   = 52
	// A split pane paints: selection, space, right-aligned number, space,
	// separator, space. The number itself is measured separately.
	diffViewerSplitPaneChromeColumns = 5
	diffViewerSplitGapColumns        = 1
	// Unified paints selection, both number columns, a semantic separator,
	// and the diff marker. Number columns are omitted when doing so is the only
	// way to retain the minimum readable code measure.
	diffViewerUnifiedChromeColumns     = 9
	diffViewerUnifiedNumberlessChrome  = 4
	diffViewerMaximumSearchRunes       = 256
	diffViewerMaximumCopyBytes         = 64 * 1024
	diffViewerMaximumDisplayPathBytes  = 4 * 1024
	diffViewerMaximumIntralineClusters = 4096
	diffViewerUnavailablePending       = "Diff is still loading."
	diffViewerUnavailableTruncatedHunk = "Hunk copy is unavailable because this diff is truncated."
	diffViewerUnavailableNoSelection   = "No diff line is selected."
	diffViewerUnavailableNoHunk        = "No diff hunk is available."
	diffViewerUnavailableNarrow        = "Diff viewer needs at least 40 readable code columns."
	diffViewerUnavailableNoFiles       = "Diff details are unavailable."
)

// DiffFileProjection is the immutable, presentation-safe input for one file.
// DisplayPath is re-sanitized and confined to a workspace-relative label by
// NewDiffViewer. Revision zero represents a producer that has not published a
// complete first projection yet.
type DiffFileProjection struct {
	ID          string
	DisplayPath string
	Lines       []DiffLine
	Truncated   bool
	Revision    uint64
}

// DiffViewerMode separates the user's durable presentation preference from
// the mode that fits the current BodyRect.
type DiffViewerMode uint8

const (
	DiffViewerUnified DiffViewerMode = iota
	DiffViewerSplit
)

func (mode DiffViewerMode) String() string {
	if mode == DiffViewerSplit {
		return "split"
	}
	return "unified"
}

// DiffViewerSide identifies the logical side selected within a split row.
// Unified rows use DiffViewerSideUnified.
type DiffViewerSide uint8

const (
	DiffViewerSideUnified DiffViewerSide = iota
	DiffViewerSideOld
	DiffViewerSideNew
)

// DiffViewerAnchor binds a physical wrapped row to stable source identity.
// Continuation zero is the first painted row of a logical DiffLine. Split rows
// retain both source indices because removed and added lines may share a row.
type DiffViewerAnchor struct {
	FileID        string
	LineIndex     int
	PeerLineIndex int
	Continuation  int
	Side          DiffViewerSide
}

// DiffViewerEventKind names a parent-owned side effect request. The component
// never calls a clipboard implementation and never appends a transcript row.
type DiffViewerEventKind uint8

const (
	DiffViewerEventNone DiffViewerEventKind = iota
	DiffViewerEventCopyLine
	DiffViewerEventCopyHunk
	DiffViewerEventCopyPath
	DiffViewerEventUnavailable
)

// DiffViewerAction identifies which user request was unavailable.
type DiffViewerAction uint8

const (
	DiffViewerActionNone DiffViewerAction = iota
	DiffViewerActionCopyLine
	DiffViewerActionCopyHunk
	DiffViewerActionCopyPath
)

// DiffViewerEvent is a bounded scalar request for the smart parent. BlockID
// remains the original transcript authority across file, hunk, and line
// navigation. Text contains terminal-safe plain text only.
type DiffViewerEvent struct {
	Kind           DiffViewerEventKind
	Action         DiffViewerAction
	BlockID        BlockID
	FileID         string
	FileIndex      int
	HunkIndex      int
	LineIndex      int
	Text           string
	Truncated      bool
	DisabledReason string
}

// DiffViewerLayout is the exact half-open geometry used for both capability
// decisions and paint. Rectangles are expressed in screen coordinates.
type DiffViewerLayout struct {
	ScreenRect CellRect
	OuterRect  CellRect
	InnerRect  CellRect
	HeaderRect CellRect
	SearchRect CellRect
	BodyRect   CellRect
	FooterRect CellRect

	PreferredMode    DiffViewerMode
	EffectiveMode    DiffViewerMode
	ShowNumbers      bool
	UnifiedCodeWidth int
	OldCodeWidth     int
	NewCodeWidth     int
	OldDigits        int
	NewDigits        int
	SplitAvailable   bool
	UnifiedAvailable bool
	DisabledReason   string
}

// DiffViewerOptions supplies presentation-only host state.
type DiffViewerOptions struct {
	Width         int
	Height        int
	IsDark        bool
	ThemeID       string
	ReducedMotion bool
	GlyphProfile  GlyphProfile
}

type diffViewerRow struct {
	rendered     string
	line         int
	peer         int
	continuation int
	side         DiffViewerSide
}

type diffViewerCursor struct {
	fileID        string
	lineIndex     int
	peerLineIndex int
	continuation  int
	side          DiffViewerSide
}

type diffViewerLocation struct {
	fileIndex int
	lineIndex int
}

type diffViewerFileCursor struct {
	lineIndex     int
	peerLineIndex int
	continuation  int
	side          DiffViewerSide
}

// DiffViewer is a presentation-only Charm component. Its files are cloned on
// admission; Bubbles owns scrolling and search editing while the parent owns
// every side effect described by DiffViewerEvent.
type DiffViewer struct {
	origin        BlockID
	files         []DiffFileProjection
	fileIndex     int
	preferredMode DiffViewerMode
	effectiveMode DiffViewerMode
	screen        CellRect
	layout        DiffViewerLayout
	isDark        bool
	themeID       string
	reducedMotion bool
	glyphProfile  GlyphProfile
	styles        Styles
	viewport      viewport.Model
	search        textinput.Model
	searching     bool
	searchQuery   string
	searchMatches []diffViewerLocation
	searchMatch   int
	rows          []diffViewerRow
	intraline     []map[int]diffViewerIntralineSpan
	cursor        diffViewerCursor
	cursorRow     int
	fileCursors   map[string]diffViewerFileCursor
	notice        string
}

// NewDiffViewer constructs a viewer in unified mode. Invalid or absolute
// display paths are reduced to a safe relative basename before entering state.
func NewDiffViewer(
	origin BlockID,
	files []DiffFileProjection,
	options DiffViewerOptions,
) *DiffViewer {
	profile := resolveGlyphProfile(options.GlyphProfile)
	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "search diff"
	search.CharLimit = diffViewerMaximumSearchRunes
	search.ShowSuggestions = false
	search.SetVirtualCursor(false)
	search.SetStyles(diffViewerSearchStyles(options.IsDark, resolveThemeID(options.ThemeID), options.ReducedMotion))

	viewer := &DiffViewer{
		origin:        origin,
		files:         cloneDiffViewerFiles(files),
		preferredMode: DiffViewerUnified,
		effectiveMode: DiffViewerUnified,
		isDark:        options.IsDark,
		reducedMotion: options.ReducedMotion,
		glyphProfile:  profile,
		styles:        NewStyles(options.IsDark, resolveThemeID(options.ThemeID)),
		viewport: viewport.New(
			viewport.WithWidth(1),
			viewport.WithHeight(1),
		),
		search:      search,
		searchMatch: -1,
		cursorRow:   -1,
		fileCursors: make(map[string]diffViewerFileCursor),
	}
	viewer.rebuildIntralineSpans()
	viewer.disableViewportKeyMap()
	viewer.screen = NewCellRect(0, 0, max(0, options.Width), max(0, options.Height))
	viewer.resetCursorForCurrentFile()
	viewer.rebuild(false)
	return viewer
}

func cloneDiffViewerFiles(files []DiffFileProjection) []DiffFileProjection {
	cloned := make([]DiffFileProjection, len(files))
	for index, file := range files {
		file.ID = sanitizeDiffViewerID(file.ID, index)
		file.DisplayPath = sanitizeDiffViewerDisplayPath(file.DisplayPath)
		file.Lines = cloneDiffViewerLines(file.Lines)
		for lineIndex := range file.Lines {
			file.Lines[lineIndex].Content = sanitizeTerminalLine(file.Lines[lineIndex].Content)
		}
		cloned[index] = file
	}
	return cloned
}

func cloneDiffViewerLines(lines []DiffLine) []DiffLine {
	cloned := make([]DiffLine, len(lines))
	for index, line := range lines {
		cloned[index] = line
		if line.Hunk != nil {
			hunk := *line.Hunk
			cloned[index].Hunk = &hunk
		}
	}
	return cloned
}

func sanitizeDiffViewerID(value string, index int) string {
	value = sanitizeTerminalSingleLine(value)
	if validOpaqueUIID(value) {
		return value
	}
	return fmt.Sprintf("diff-file-%d", index+1)
}

func sanitizeDiffViewerDisplayPath(value string) string {
	value = sanitizeTerminalSingleLine(value)
	value = strings.ReplaceAll(value, `\`, "/")
	if len(value) > diffViewerMaximumDisplayPathBytes {
		value = boundedDiffViewerText(value, diffViewerMaximumDisplayPathBytes)
	}
	if value == "" {
		return "patch"
	}
	absolute := strings.HasPrefix(value, "/") ||
		(len(value) >= 3 && value[1] == ':' && value[2] == '/')
	clean := filepath.ToSlash(filepath.Clean(value))
	if absolute || clean == ".." || strings.HasPrefix(clean, "../") {
		clean = filepath.Base(clean)
	}
	clean = strings.TrimLeft(clean, "/")
	if clean == "" || clean == "." || clean == ".." {
		return "patch"
	}
	return sanitizeTerminalSingleLine(clean)
}

func diffViewerSearchStyles(isDark bool, themeID string, reducedMotion bool) textinput.Styles {
	if noColor {
		plain := lipgloss.NoColor{}
		style := lipgloss.NewStyle().Foreground(plain)
		return textinput.Styles{
			Focused: textinput.StyleState{
				Text: style, Placeholder: style, Suggestion: style, Prompt: style.Bold(true),
			},
			Blurred: textinput.StyleState{
				Text: style, Placeholder: style, Suggestion: style, Prompt: style,
			},
			Cursor: textinput.CursorStyle{
				Color: plain, Shape: tea.CursorBar, Blink: !reducedMotion,
			},
		}
	}
	palette := newSemanticPalette(isDark, themeID)
	return textinput.Styles{
		Focused: textinput.StyleState{
			Text:        lipgloss.NewStyle().Foreground(palette.Text),
			Placeholder: lipgloss.NewStyle().Foreground(palette.Dim),
			Suggestion:  lipgloss.NewStyle().Foreground(palette.Dim),
			Prompt:      lipgloss.NewStyle().Foreground(palette.Accent).Bold(true),
		},
		Blurred: textinput.StyleState{
			Text:        lipgloss.NewStyle().Foreground(palette.Muted),
			Placeholder: lipgloss.NewStyle().Foreground(palette.Dim),
			Suggestion:  lipgloss.NewStyle().Foreground(palette.Dim),
			Prompt:      lipgloss.NewStyle().Foreground(palette.Dim),
		},
		Cursor: textinput.CursorStyle{
			Color: palette.Accent,
			Shape: tea.CursorBar,
			Blink: !reducedMotion,
		},
	}
}

func (viewer *DiffViewer) disableViewportKeyMap() {
	if viewer == nil {
		return
	}
	viewer.viewport.KeyMap.Up.SetEnabled(false)
	viewer.viewport.KeyMap.Down.SetEnabled(false)
	viewer.viewport.KeyMap.PageUp.SetEnabled(false)
	viewer.viewport.KeyMap.PageDown.SetEnabled(false)
	viewer.viewport.KeyMap.HalfPageUp.SetEnabled(false)
	viewer.viewport.KeyMap.HalfPageDown.SetEnabled(false)
}

// SetSize recomputes modal, BodyRect, capability, and wrapped-row geometry.
// Preferred split survives a narrow fallback and is restored when it fits.
func (viewer *DiffViewer) SetSize(width, height int) {
	if viewer == nil {
		return
	}
	screen := NewCellRect(0, 0, max(0, width), max(0, height))
	if viewer.screen == screen {
		return
	}
	viewer.screen = screen
	viewer.rebuild(true)
}

// SetScreenRect is the non-zero-origin counterpart used by a parent that has
// already allocated a screen sub-rectangle.
func (viewer *DiffViewer) SetScreenRect(rect CellRect) {
	if viewer == nil {
		return
	}
	screen := rect.canonical()
	if viewer.screen == screen {
		return
	}
	viewer.screen = screen
	viewer.rebuild(true)
}

// SetTheme refreshes every adaptive style while preserving mode, semantic
// selection, search state, and geometry.
func (viewer *DiffViewer) SetTheme(isDark bool, themeID string) {
	if viewer == nil {
		return
	}
	themeID = resolveThemeID(themeID)
	if viewer.isDark == isDark && resolveThemeID(viewer.themeID) == themeID {
		return
	}
	viewer.isDark, viewer.themeID = isDark, themeID
	viewer.styles = NewStyles(isDark, viewer.themeID)
	viewer.search.SetStyles(diffViewerSearchStyles(isDark, viewer.themeID, viewer.reducedMotion))
	viewer.rebuild(true)
}

// SetReducedMotion disables the search cursor blink without conflating motion,
// glyph capability, or color. The semantic anchor and frame allocation do not
// change.
func (viewer *DiffViewer) SetReducedMotion(reduced bool) {
	if viewer == nil || viewer.reducedMotion == reduced {
		return
	}
	viewer.reducedMotion = reduced
	viewer.search.SetStyles(diffViewerSearchStyles(viewer.isDark, viewer.themeID, reduced))
}

// SetFiles replaces an immutable producer projection without resetting the
// user's mode preference or semantic cursor. A live pending diff can therefore
// become complete in place; stale per-file cursors are discarded.
func (viewer *DiffViewer) SetFiles(files []DiffFileProjection) {
	if viewer == nil {
		return
	}
	currentID := viewer.currentFileID()
	viewer.rememberCurrentFileCursor()
	viewer.files = cloneDiffViewerFiles(files)
	viewer.rebuildIntralineSpans()
	viewer.fileIndex = 0
	for index := range viewer.files {
		if viewer.files[index].ID == currentID {
			viewer.fileIndex = index
			break
		}
	}
	liveCursors := make(map[string]diffViewerFileCursor, len(viewer.files))
	for _, file := range viewer.files {
		if saved, ok := viewer.fileCursors[file.ID]; ok {
			liveCursors[file.ID] = saved
		}
	}
	viewer.fileCursors = liveCursors
	if _, ok := viewer.currentFile(); !ok {
		viewer.cursor = diffViewerCursor{lineIndex: -1, peerLineIndex: -1}
		viewer.cursorRow = -1
	} else if saved, ok := viewer.fileCursors[viewer.currentFileID()]; ok {
		viewer.cursor = diffViewerCursor{
			fileID: viewer.currentFileID(), lineIndex: saved.lineIndex,
			peerLineIndex: saved.peerLineIndex, continuation: saved.continuation,
			side: saved.side,
		}
	} else {
		viewer.resetCursorForCurrentFile()
	}
	viewer.searchMatches = viewer.findMatches(viewer.searchQuery)
	viewer.searchMatch = -1
	viewer.notice = ""
	viewer.rebuild(true)
}

// Layout returns the exact geometry and resolved mode consumed by View.
func (viewer *DiffViewer) Layout() DiffViewerLayout {
	if viewer == nil {
		return DiffViewerLayout{}
	}
	return viewer.layout
}

func (viewer *DiffViewer) PreferredMode() DiffViewerMode {
	if viewer == nil {
		return DiffViewerUnified
	}
	return viewer.preferredMode
}

func (viewer *DiffViewer) EffectiveMode() DiffViewerMode {
	if viewer == nil {
		return DiffViewerUnified
	}
	return viewer.effectiveMode
}

func (viewer *DiffViewer) CurrentFileIndex() int {
	if viewer == nil {
		return 0
	}
	return viewer.fileIndex
}

func (viewer *DiffViewer) CurrentAnchor() DiffViewerAnchor {
	if viewer == nil {
		return DiffViewerAnchor{LineIndex: -1, PeerLineIndex: -1}
	}
	return DiffViewerAnchor{
		FileID:        viewer.cursor.fileID,
		LineIndex:     viewer.cursor.lineIndex,
		PeerLineIndex: viewer.cursor.peerLineIndex,
		Continuation:  viewer.cursor.continuation,
		Side:          viewer.cursor.side,
	}
}

func (viewer *DiffViewer) rebuild(preserveCursor bool) {
	if viewer == nil {
		return
	}
	anchor := viewer.cursor
	oldCursorRow := viewer.currentRowIndex()
	oldYOffset := viewer.viewport.YOffset()
	oldScreenRow := oldCursorRow - oldYOffset
	oldCursorVisible := oldCursorRow >= 0 &&
		oldScreenRow >= 0 &&
		oldScreenRow < max(1, viewer.viewport.Height())
	viewer.layout = viewer.deriveLayout()
	viewer.effectiveMode = viewer.layout.EffectiveMode
	if viewer.effectiveMode == DiffViewerSplit &&
		strings.HasPrefix(viewer.notice, "Split needs ") {
		viewer.notice = ""
	}

	width := max(1, viewer.layout.BodyRect.Width())
	height := max(1, viewer.layout.BodyRect.Height())
	vp := viewport.New(viewport.WithWidth(width), viewport.WithHeight(height))
	viewer.viewport = vp
	viewer.disableViewportKeyMap()

	if viewer.layout.UnifiedAvailable && len(viewer.files) > 0 {
		if viewer.effectiveMode == DiffViewerSplit {
			viewer.rows = viewer.buildSplitRows()
		} else {
			viewer.rows = viewer.buildUnifiedRows()
		}
	} else {
		viewer.rows = nil
	}
	viewer.cursorRow = -1

	content := viewer.renderedBody()
	viewer.viewport.SetContent(content)
	if preserveCursor && viewer.restoreCursor(anchor) {
		if oldCursorVisible {
			targetScreenRow := min(
				max(0, oldScreenRow),
				max(0, viewer.viewport.Height()-1),
			)
			viewer.viewport.SetYOffset(viewer.cursorRow - targetScreenRow)
		}
		viewer.ensureCursorVisible()
	} else if preserveCursor && len(viewer.rows) == 0 {
		// Bubbles owns the final clamp because the recovery document may have a
		// different physical height from the semantic row projection.
		viewer.viewport.SetYOffset(oldYOffset)
	} else {
		viewer.selectFirstRow()
	}
	viewer.search.SetWidth(max(1, viewer.layout.SearchRect.Width()-2))
}

func (viewer *DiffViewer) deriveLayout() DiffViewerLayout {
	screen := viewer.screen.canonical()
	outer := diffViewerModalRect(screen)
	inner := Inset(outer, Insets{Top: 1, Right: 1, Bottom: 1, Left: 1})
	work := Inset(inner, Insets{Right: 1, Left: 1})
	header, remain := TakeTop(work, min(1, work.Height()))
	footer, remain := TakeBottom(remain, min(1, remain.Height()))
	search := NewCellRect(remain.MinX, remain.MinY, remain.MaxX, remain.MinY)
	if viewer.searching {
		search, remain = TakeTop(remain, min(1, remain.Height()))
	}

	oldDigits, newDigits := viewer.currentDigitWidths()
	unified := resolveDiffViewerUnifiedPlan(remain.Width(), oldDigits, newDigits)
	split := resolveDiffViewerSplitPlan(remain.Width(), oldDigits, newDigits)
	effective := DiffViewerUnified
	if viewer.preferredMode == DiffViewerSplit && split.available {
		effective = DiffViewerSplit
	}
	reason := ""
	if !unified.available {
		reason = diffViewerUnavailableNarrow
	} else if viewer.preferredMode == DiffViewerSplit && !split.available {
		reason = split.reason
	}

	return DiffViewerLayout{
		ScreenRect:       screen,
		OuterRect:        outer,
		InnerRect:        inner,
		HeaderRect:       header,
		SearchRect:       search,
		BodyRect:         remain,
		FooterRect:       footer,
		PreferredMode:    viewer.preferredMode,
		EffectiveMode:    effective,
		ShowNumbers:      unified.showNumbers,
		UnifiedCodeWidth: unified.codeWidth,
		OldCodeWidth:     split.oldCodeWidth,
		NewCodeWidth:     split.newCodeWidth,
		OldDigits:        oldDigits,
		NewDigits:        newDigits,
		SplitAvailable:   split.available,
		UnifiedAvailable: unified.available,
		DisabledReason:   reason,
	}
}

func diffViewerModalRect(screen CellRect) CellRect {
	screen = screen.canonical()
	width := min(
		screen.Width(),
		max(diffViewerMinimumOuterWidth,
			screen.Width()*diffViewerOuterScaleNumerator/diffViewerOuterScaleDenominator),
	)
	height := min(
		screen.Height(),
		max(diffViewerMinimumOuterHeight,
			screen.Height()*diffViewerOuterScaleNumerator/diffViewerOuterScaleDenominator),
	)
	minX := screen.MinX + max(0, (screen.Width()-width)/2)
	minY := screen.MinY + max(0, (screen.Height()-height)/2)
	return NewCellRect(minX, minY, minX+width, minY+height)
}

type diffViewerUnifiedPlan struct {
	available   bool
	showNumbers bool
	codeWidth   int
}

func resolveDiffViewerUnifiedPlan(width, oldDigits, newDigits int) diffViewerUnifiedPlan {
	if width <= 0 {
		return diffViewerUnifiedPlan{}
	}
	numbered := width - max(1, oldDigits) - max(1, newDigits) -
		diffViewerUnifiedChromeColumns
	if numbered >= diffViewerMinimumUnifiedCodeColumns {
		return diffViewerUnifiedPlan{
			available: true, showNumbers: true, codeWidth: numbered,
		}
	}
	numberless := width - diffViewerUnifiedNumberlessChrome
	return diffViewerUnifiedPlan{
		available: numberless >= diffViewerMinimumUnifiedCodeColumns,
		codeWidth: max(0, numberless),
	}
}

type diffViewerSplitPlan struct {
	available    bool
	oldCodeWidth int
	newCodeWidth int
	reason       string
}

func resolveDiffViewerSplitPlan(width, oldDigits, newDigits int) diffViewerSplitPlan {
	oldDigits = max(1, oldDigits)
	newDigits = max(1, newDigits)
	residual := width - oldDigits - newDigits -
		2*diffViewerSplitPaneChromeColumns - diffViewerSplitGapColumns
	oldCode := residual / 2
	newCode := residual - oldCode
	available := oldCode >= diffViewerMinimumSplitCodeColumns &&
		newCode >= diffViewerMinimumSplitCodeColumns
	reason := ""
	if !available {
		required := oldDigits + newDigits + 2*diffViewerSplitPaneChromeColumns +
			diffViewerSplitGapColumns + 2*diffViewerMinimumSplitCodeColumns
		reason = fmt.Sprintf(
			"Split needs %d body columns for two 52-column code panes; %d available.",
			required,
			max(0, width),
		)
	}
	return diffViewerSplitPlan{
		available:    available,
		oldCodeWidth: max(0, oldCode),
		newCodeWidth: max(0, newCode),
		reason:       reason,
	}
}

func (viewer *DiffViewer) currentDigitWidths() (oldDigits, newDigits int) {
	oldDigits, newDigits = 1, 1
	file, ok := viewer.currentFile()
	if !ok {
		return oldDigits, newDigits
	}
	for _, line := range file.Lines {
		oldDigits = max(oldDigits, len(strconv.Itoa(max(0, line.OldLine))))
		newDigits = max(newDigits, len(strconv.Itoa(max(0, line.NewLine))))
		if line.Hunk != nil {
			oldDigits = max(oldDigits, len(strconv.Itoa(max(0, line.Hunk.OldStart+line.Hunk.OldCount))))
			newDigits = max(newDigits, len(strconv.Itoa(max(0, line.Hunk.NewStart+line.Hunk.NewCount))))
		}
	}
	return oldDigits, newDigits
}

func (viewer *DiffViewer) currentFile() (DiffFileProjection, bool) {
	if viewer == nil || viewer.fileIndex < 0 || viewer.fileIndex >= len(viewer.files) {
		return DiffFileProjection{}, false
	}
	return viewer.files[viewer.fileIndex], true
}

func (viewer *DiffViewer) resetCursorForCurrentFile() {
	file, ok := viewer.currentFile()
	if !ok {
		viewer.cursor = diffViewerCursor{lineIndex: -1, peerLineIndex: -1}
		viewer.cursorRow = -1
		return
	}
	viewer.cursor = diffViewerCursor{
		fileID: file.ID, lineIndex: firstSelectableDiffLine(file.Lines),
		peerLineIndex: -1, side: DiffViewerSideUnified,
	}
	viewer.cursorRow = -1
}

func firstSelectableDiffLine(lines []DiffLine) int {
	if len(lines) == 0 {
		return -1
	}
	for index, line := range lines {
		if line.Kind != DiffEllipsis && line.Kind != DiffOmitted {
			return index
		}
	}
	return 0
}

func (viewer *DiffViewer) renderedBody() string {
	switch {
	case len(viewer.files) == 0:
		return viewer.styles.OverlayDim.Render(diffViewerUnavailableNoFiles)
	case !viewer.layout.UnifiedAvailable:
		return viewer.styles.OverlayDim.Render(viewer.layout.DisabledReason)
	case len(viewer.rows) == 0:
		file, _ := viewer.currentFile()
		if file.Revision == 0 {
			return viewer.styles.OverlayDim.Render(diffViewerUnavailablePending)
		}
		return viewer.styles.OverlayDim.Render("No changed lines.")
	default:
		rendered := make([]string, len(viewer.rows))
		for index := range viewer.rows {
			rendered[index] = viewer.rows[index].rendered
		}
		return strings.Join(rendered, "\n")
	}
}

func (viewer *DiffViewer) buildUnifiedRows() []diffViewerRow {
	file, ok := viewer.currentFile()
	if !ok {
		return nil
	}
	oldDigits, newDigits := viewer.layout.OldDigits, viewer.layout.NewDigits
	showNumbers := viewer.layout.ShowNumbers
	codeWidth := max(1, viewer.layout.UnifiedCodeWidth)
	intraline := viewer.currentIntralineSpans()
	rows := make([]diffViewerRow, 0, len(file.Lines))
	for lineIndex, line := range file.Lines {
		lineRows := viewer.renderUnifiedLogicalLine(
			line, lineIndex, oldDigits, newDigits, showNumbers, codeWidth,
			intraline[lineIndex],
		)
		rows = append(rows, lineRows...)
	}
	return rows
}

func (viewer *DiffViewer) renderUnifiedLogicalLine(
	line DiffLine,
	lineIndex int,
	oldDigits int,
	newDigits int,
	showNumbers bool,
	codeWidth int,
	intraline diffViewerIntralineSpan,
) []diffViewerRow {
	content := diffViewerDisplayContent(line, viewer.glyphProfile)
	chunks := diffViewerRangedContentChunks(content, codeWidth)
	rows := make([]diffViewerRow, 0, len(chunks))
	for continuation, chunk := range chunks {
		prefix := diffViewerUnifiedPrefix(
			line,
			glyphSet(viewer.glyphProfile).Unselected,
			continuation,
			oldDigits,
			newDigits,
			showNumbers,
			viewer.glyphProfile,
		)
		style := viewer.diffLineStyle(line.Kind)
		rendered := prefix + renderDiffViewerChunk(chunk, intraline, style)
		rendered = fitDiffViewerRow(rendered, viewer.layout.BodyRect.Width())
		rows = append(rows, diffViewerRow{
			rendered:     rendered,
			line:         lineIndex,
			peer:         -1,
			continuation: continuation,
			side:         DiffViewerSideUnified,
		})
	}
	return rows
}

func diffViewerLogicalContent(line DiffLine) string {
	content := sanitizeTerminalLine(line.Content)
	if line.Kind == DiffHunkHeader && line.Hunk != nil {
		content = formatDiffHunk(*line.Hunk)
	}
	if line.Kind == DiffNoNewline && content == "" {
		content = diffNoNewlineContent
	}
	return strings.ReplaceAll(content, "\t", "    ")
}

func diffViewerContentChunks(content string, width int) []string {
	width = max(1, width)
	if content == "" {
		return []string{""}
	}
	if lipglossWidth(content) <= width {
		return []string{content}
	}
	return splitDisplayChunks(content, width)
}

type diffViewerContentChunk struct {
	text  string
	start int
	end   int
}

// diffViewerRangedContentChunks preserves the source grapheme interval for
// each wrapped row. Intraline emphasis can therefore be applied after wrapping
// without injecting ANSI before measurement or risking a split control
// sequence.
func diffViewerRangedContentChunks(content string, width int) []diffViewerContentChunk {
	chunks := diffViewerContentChunks(content, width)
	ranged := make([]diffViewerContentChunk, 0, len(chunks))
	offset := 0
	for _, chunk := range chunks {
		count := diffViewerGraphemeCount(chunk)
		ranged = append(ranged, diffViewerContentChunk{
			text: chunk, start: offset, end: offset + count,
		})
		offset += count
	}
	return ranged
}

func diffViewerGraphemeCount(value string) int {
	count := 0
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		count++
	}
	return count
}

type diffViewerIntralineSpan struct {
	start int
	end   int
}

func (span diffViewerIntralineSpan) valid() bool {
	return span.start >= 0 && span.end > span.start
}

func (span diffViewerIntralineSpan) shifted(offset int) diffViewerIntralineSpan {
	if !span.valid() {
		return diffViewerIntralineSpan{}
	}
	return diffViewerIntralineSpan{start: span.start + offset, end: span.end + offset}
}

// diffViewerIntralineSpans pairs only the same conservative removed/added
// units used by split view. It never guesses across unmatched lines, metadata,
// or non-adjacent hunks.
func diffViewerIntralineSpans(lines []DiffLine) map[int]diffViewerIntralineSpan {
	spans := make(map[int]diffViewerIntralineSpan)
	for _, unit := range diffViewerSplitUnits(lines) {
		if unit.oldIndex < 0 || unit.newIndex < 0 {
			continue
		}
		oldLine, newLine := lines[unit.oldIndex], lines[unit.newIndex]
		if oldLine.Kind != DiffRemoved || newLine.Kind != DiffAdded {
			continue
		}
		oldSpan, newSpan := diffViewerPairIntralineSpans(
			diffViewerDisplayContent(oldLine, GlyphUnicode),
			diffViewerDisplayContent(newLine, GlyphUnicode),
		)
		if oldSpan.valid() {
			spans[unit.oldIndex] = oldSpan
		}
		if newSpan.valid() {
			spans[unit.newIndex] = newSpan
		}
	}
	return spans
}

func (viewer *DiffViewer) rebuildIntralineSpans() {
	if viewer == nil {
		return
	}
	viewer.intraline = make([]map[int]diffViewerIntralineSpan, len(viewer.files))
	for index, file := range viewer.files {
		viewer.intraline[index] = diffViewerIntralineSpans(file.Lines)
	}
}

func (viewer *DiffViewer) currentIntralineSpans() map[int]diffViewerIntralineSpan {
	if viewer == nil || viewer.fileIndex < 0 || viewer.fileIndex >= len(viewer.intraline) {
		return nil
	}
	return viewer.intraline[viewer.fileIndex]
}

func diffViewerPairIntralineSpans(
	oldContent string,
	newContent string,
) (diffViewerIntralineSpan, diffViewerIntralineSpan) {
	oldClusters, oldOK := diffViewerBoundedGraphemes(oldContent)
	newClusters, newOK := diffViewerBoundedGraphemes(newContent)
	if !oldOK || !newOK || len(oldClusters) == 0 || len(newClusters) == 0 {
		return diffViewerIntralineSpan{}, diffViewerIntralineSpan{}
	}

	prefix := 0
	for prefix < len(oldClusters) && prefix < len(newClusters) &&
		oldClusters[prefix] == newClusters[prefix] {
		prefix++
	}
	oldRemain := len(oldClusters) - prefix
	newRemain := len(newClusters) - prefix
	suffix := 0
	for suffix < oldRemain && suffix < newRemain &&
		oldClusters[len(oldClusters)-1-suffix] == newClusters[len(newClusters)-1-suffix] {
		suffix++
	}

	// With less than two shared graphemes, emphasizing almost the whole line is
	// usually an accidental affix match rather than useful local context.
	if prefix+suffix < 2 {
		return diffViewerIntralineSpan{}, diffViewerIntralineSpan{}
	}
	oldSpan := diffViewerIntralineSpan{start: prefix, end: len(oldClusters) - suffix}
	newSpan := diffViewerIntralineSpan{start: prefix, end: len(newClusters) - suffix}
	return oldSpan, newSpan
}

func diffViewerBoundedGraphemes(value string) ([]string, bool) {
	clusters := make([]string, 0, min(len(value), diffViewerMaximumIntralineClusters))
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		if len(clusters) >= diffViewerMaximumIntralineClusters {
			return nil, false
		}
		clusters = append(clusters, graphemes.Str())
	}
	return clusters, true
}

func renderDiffViewerChunk(
	chunk diffViewerContentChunk,
	span diffViewerIntralineSpan,
	base lipgloss.Style,
) string {
	if !span.valid() || span.end <= chunk.start || span.start >= chunk.end {
		return base.Render(chunk.text)
	}
	start := max(span.start, chunk.start) - chunk.start
	end := min(span.end, chunk.end) - chunk.start
	before, changed, after := diffViewerSplitGraphemeRange(chunk.text, start, end)
	if changed == "" {
		return base.Render(chunk.text)
	}
	var rendered strings.Builder
	if before != "" {
		rendered.WriteString(base.Render(before))
	}
	rendered.WriteString(base.Bold(true).Underline(true).Render(changed))
	if after != "" {
		rendered.WriteString(base.Render(after))
	}
	return rendered.String()
}

func diffViewerSplitGraphemeRange(
	value string,
	start int,
	end int,
) (string, string, string) {
	var before, changed, after strings.Builder
	index := 0
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		switch {
		case index < start:
			before.WriteString(graphemes.Str())
		case index < end:
			changed.WriteString(graphemes.Str())
		default:
			after.WriteString(graphemes.Str())
		}
		index++
	}
	return before.String(), changed.String(), after.String()
}

func diffViewerUnifiedPrefix(
	line DiffLine,
	selector string,
	continuation int,
	oldDigits int,
	newDigits int,
	showNumbers bool,
	profile GlyphProfile,
) string {
	glyphs := glyphSet(profile)
	marker := diffViewerLineMarker(line.Kind, profile)
	if continuation > 0 {
		if showNumbers {
			return fmt.Sprintf(
				"%s %*s %*s %s %s ",
				selector, oldDigits, "", newDigits, "", glyphs.Vertical, glyphs.Continuation,
			)
		}
		return fmt.Sprintf("%s %s ", selector, glyphs.Continuation)
	}
	if showNumbers {
		oldNumber := ""
		if line.OldLine > 0 {
			oldNumber = strconv.Itoa(line.OldLine)
		}
		newNumber := ""
		if line.NewLine > 0 {
			newNumber = strconv.Itoa(line.NewLine)
		}
		return fmt.Sprintf(
			"%s %*s %*s %s %s ",
			selector, oldDigits, oldNumber, newDigits, newNumber, glyphs.Vertical, marker,
		)
	}
	return fmt.Sprintf("%s %s ", selector, marker)
}

func diffViewerLineMarker(kind DiffLineKind, profiles ...GlyphProfile) string {
	switch kind {
	case DiffAdded:
		return "+"
	case DiffRemoved:
		return "-"
	case DiffContext:
		return " "
	case DiffHunkHeader:
		return "@"
	case DiffEllipsis, DiffOmitted:
		if resolveGlyphProfile(profiles...) == GlyphASCII {
			return "."
		}
		return "…"
	case DiffNoNewline:
		return `\`
	default:
		return " "
	}
}

func diffViewerDisplayContent(line DiffLine, profile GlyphProfile) string {
	content := diffViewerLogicalContent(line)
	if resolveGlyphProfile(profile) == GlyphASCII && line.Kind == DiffEllipsis {
		content = strings.TrimPrefix(content, "…")
		content = "." + content
	}
	return content
}

type diffViewerSplitUnit struct {
	oldIndex  int
	newIndex  int
	metaIndex int
}

func (viewer *DiffViewer) buildSplitRows() []diffViewerRow {
	file, ok := viewer.currentFile()
	if !ok {
		return nil
	}
	units := diffViewerSplitUnits(file.Lines)
	rows := make([]diffViewerRow, 0, len(units))
	for _, unit := range units {
		rows = append(rows, viewer.renderSplitUnit(file, unit)...)
	}
	return rows
}

func diffViewerSplitUnits(lines []DiffLine) []diffViewerSplitUnit {
	units := make([]diffViewerSplitUnit, 0, len(lines))
	for index := 0; index < len(lines); {
		line := lines[index]
		switch line.Kind {
		case DiffRemoved:
			removed := make([]int, 0, 2)
			for index < len(lines) && lines[index].Kind == DiffRemoved {
				removed = append(removed, index)
				index++
			}
			added := make([]int, 0, 2)
			for index < len(lines) && lines[index].Kind == DiffAdded {
				added = append(added, index)
				index++
			}
			for pair := range max(len(removed), len(added)) {
				unit := diffViewerSplitUnit{oldIndex: -1, newIndex: -1, metaIndex: -1}
				if pair < len(removed) {
					unit.oldIndex = removed[pair]
				}
				if pair < len(added) {
					unit.newIndex = added[pair]
				}
				units = append(units, unit)
			}
		case DiffAdded:
			units = append(units, diffViewerSplitUnit{
				oldIndex: -1, newIndex: index, metaIndex: -1,
			})
			index++
		case DiffContext:
			units = append(units, diffViewerSplitUnit{
				oldIndex: index, newIndex: index, metaIndex: -1,
			})
			index++
		default:
			units = append(units, diffViewerSplitUnit{
				oldIndex: -1, newIndex: -1, metaIndex: index,
			})
			index++
		}
	}
	return units
}

func (viewer *DiffViewer) renderSplitUnit(
	file DiffFileProjection,
	unit diffViewerSplitUnit,
) []diffViewerRow {
	if unit.metaIndex >= 0 {
		line := file.Lines[unit.metaIndex]
		content := diffViewerDisplayContent(line, viewer.glyphProfile)
		codeWidth := max(1, viewer.layout.BodyRect.Width()-diffViewerUnifiedNumberlessChrome)
		chunks := diffViewerContentChunks(content, codeWidth)
		rows := make([]diffViewerRow, 0, len(chunks))
		for continuation, chunk := range chunks {
			prefix := fmt.Sprintf(
				"%s %s ",
				glyphSet(viewer.glyphProfile).Unselected,
				diffViewerLineMarker(line.Kind, viewer.glyphProfile),
			)
			rows = append(rows, diffViewerRow{
				rendered: fitDiffViewerRow(
					prefix+viewer.diffLineStyle(line.Kind).Render(chunk),
					viewer.layout.BodyRect.Width(),
				),
				line: unit.metaIndex, peer: -1, continuation: continuation,
				side: DiffViewerSideUnified,
			})
		}
		return rows
	}

	var oldChunks, newChunks []diffViewerContentChunk
	var oldSpan, newSpan diffViewerIntralineSpan
	if unit.oldIndex >= 0 && unit.newIndex >= 0 {
		intraline := viewer.currentIntralineSpans()
		oldSpan, newSpan = intraline[unit.oldIndex], intraline[unit.newIndex]
		// Split content includes the semantic marker and one separating space.
		oldSpan = oldSpan.shifted(2)
		newSpan = newSpan.shifted(2)
	}
	if unit.oldIndex >= 0 {
		oldChunks = diffViewerRangedContentChunks(
			diffViewerSplitContent(file.Lines[unit.oldIndex], viewer.glyphProfile),
			max(1, viewer.layout.OldCodeWidth),
		)
	}
	if unit.newIndex >= 0 {
		newChunks = diffViewerRangedContentChunks(
			diffViewerSplitContent(file.Lines[unit.newIndex], viewer.glyphProfile),
			max(1, viewer.layout.NewCodeWidth),
		)
	}
	rowCount := max(len(oldChunks), len(newChunks))
	rows := make([]diffViewerRow, 0, rowCount)
	for continuation := range rowCount {
		oldChunk, newChunk := diffViewerContentChunk{}, diffViewerContentChunk{}
		oldIndex, newIndex := -1, -1
		if continuation < len(oldChunks) {
			oldChunk = oldChunks[continuation]
			oldIndex = unit.oldIndex
		}
		if continuation < len(newChunks) {
			newChunk = newChunks[continuation]
			newIndex = unit.newIndex
		}
		old := viewer.renderSplitPane(
			file, oldIndex, DiffViewerSideOld, continuation, oldChunk,
			oldSpan, viewer.layout.OldDigits, viewer.layout.OldCodeWidth,
		)
		newSide := viewer.renderSplitPane(
			file, newIndex, DiffViewerSideNew, continuation, newChunk,
			newSpan, viewer.layout.NewDigits, viewer.layout.NewCodeWidth,
		)
		gap := glyphSet(viewer.glyphProfile).Vertical
		primary, peer, side := oldIndex, newIndex, DiffViewerSideOld
		if primary < 0 {
			primary, peer, side = newIndex, oldIndex, DiffViewerSideNew
		}
		rows = append(rows, diffViewerRow{
			rendered: old + gap + newSide,
			line:     primary, peer: peer, continuation: continuation, side: side,
		})
	}
	return rows
}

func diffViewerSplitContent(line DiffLine, profile GlyphProfile) string {
	return diffViewerLineMarker(line.Kind, profile) + " " + diffViewerDisplayContent(line, profile)
}

func (viewer *DiffViewer) renderSplitPane(
	file DiffFileProjection,
	lineIndex int,
	side DiffViewerSide,
	continuation int,
	content diffViewerContentChunk,
	intraline diffViewerIntralineSpan,
	digits int,
	codeWidth int,
) string {
	selector := glyphSet(viewer.glyphProfile).Unselected
	number := ""
	kind := DiffContext
	if lineIndex >= 0 && lineIndex < len(file.Lines) {
		line := file.Lines[lineIndex]
		kind = line.Kind
		value := line.OldLine
		if side == DiffViewerSideNew {
			value = line.NewLine
		}
		if value > 0 && continuation == 0 {
			number = strconv.Itoa(value)
		}
	}
	prefix := fmt.Sprintf(
		"%s %*s %s ",
		selector,
		max(1, digits),
		number,
		glyphSet(viewer.glyphProfile).Vertical,
	)
	styled := renderDiffViewerChunk(content, intraline, viewer.diffLineStyle(kind))
	return fitDiffViewerRow(prefix+styled, max(0, digits)+diffViewerSplitPaneChromeColumns+max(0, codeWidth))
}

func (viewer *DiffViewer) diffLineStyle(kind DiffLineKind) lipgloss.Style {
	switch kind {
	case DiffAdded:
		return viewer.styles.DiffAdded.PaddingLeft(0)
	case DiffRemoved:
		return viewer.styles.DiffRemoved.PaddingLeft(0)
	case DiffContext:
		return viewer.styles.DiffContext.PaddingLeft(0)
	default:
		return viewer.styles.DiffHeader.PaddingLeft(0)
	}
}

func fitDiffViewerRow(value string, width int) string {
	width = max(0, width)
	if lipglossWidth(value) > width {
		// Unlike the general plain-text helper, viewer rows may already carry
		// adaptive Lip Gloss styles. ANSI-aware truncation preserves complete
		// control sequences and never turns a clipped row into broken chrome.
		value = ansi.Truncate(value, width, "")
	}
	return value + strings.Repeat(" ", max(0, width-lipglossWidth(value)))
}

func (viewer *DiffViewer) restoreCursor(anchor diffViewerCursor) bool {
	if len(viewer.rows) == 0 {
		return false
	}
	best := -1
	for index, row := range viewer.rows {
		if viewer.currentFileID() != anchor.fileID {
			continue
		}
		if diffViewerRowContainsLine(row, anchor.lineIndex) {
			if row.continuation == anchor.continuation {
				best = index
				break
			}
			if best < 0 {
				best = index
			}
		}
	}
	if best < 0 {
		return false
	}
	viewer.setCursorFromRow(best, anchor.side, anchor.lineIndex)
	return true
}

func diffViewerRowContainsLine(row diffViewerRow, lineIndex int) bool {
	return row.line == lineIndex || row.peer == lineIndex
}

func (viewer *DiffViewer) selectFirstRow() {
	if len(viewer.rows) == 0 {
		return
	}
	viewer.setCursorFromRow(0, viewer.rows[0].side, viewer.rows[0].line)
	viewer.viewport.GotoTop()
}

func (viewer *DiffViewer) setCursorFromRow(index int, preferredSide DiffViewerSide, preferredLine int) {
	if index < 0 || index >= len(viewer.rows) {
		return
	}
	row := viewer.rows[index]
	side := row.side
	line := row.line
	peer := row.peer
	if preferredLine >= 0 && diffViewerRowContainsLine(row, preferredLine) {
		if viewer.effectiveMode != DiffViewerSplit {
			// A narrow unified fallback retains the prior split side so it can
			// be restored when two panes fit again.
			line = preferredLine
			if row.peer == preferredLine {
				peer = row.line
			}
			side = preferredSide
		} else {
			switch {
			case row.side == DiffViewerSideUnified:
				side = DiffViewerSideUnified
			case row.peer >= 0 &&
				preferredLine == row.peer &&
				(row.peer != row.line || preferredSide == DiffViewerSideNew):
				line, peer, side = row.peer, row.line, DiffViewerSideNew
			default:
				// The preferred pane is blank on this physical row. Select the
				// side that actually paints instead of hiding the cursor.
				line, peer, side = row.line, row.peer, row.side
			}
		}
	}
	viewer.cursor = diffViewerCursor{
		fileID: viewer.currentFileID(), lineIndex: line, peerLineIndex: peer,
		continuation: row.continuation, side: side,
	}
	viewer.cursorRow = index
}

func (viewer *DiffViewer) currentRowIndex() int {
	if viewer == nil || viewer.cursor.fileID != viewer.currentFileID() {
		return -1
	}
	if viewer.cursorRow >= 0 && viewer.cursorRow < len(viewer.rows) {
		row := viewer.rows[viewer.cursorRow]
		if diffViewerRowContainsLine(row, viewer.cursor.lineIndex) &&
			row.continuation == viewer.cursor.continuation {
			return viewer.cursorRow
		}
	}
	for index, row := range viewer.rows {
		if diffViewerRowContainsLine(row, viewer.cursor.lineIndex) &&
			row.continuation == viewer.cursor.continuation {
			viewer.cursorRow = index
			return index
		}
	}
	viewer.cursorRow = -1
	return -1
}

func (viewer *DiffViewer) ensureCursorVisible() {
	index := viewer.currentRowIndex()
	if index < 0 {
		return
	}
	top := viewer.viewport.YOffset()
	height := max(1, viewer.viewport.Height())
	switch {
	case index < top:
		viewer.viewport.SetYOffset(index)
	case index >= top+height:
		viewer.viewport.SetYOffset(index - height + 1)
	}
}

func (viewer *DiffViewer) moveCursor(delta int) {
	if len(viewer.rows) == 0 || delta == 0 {
		return
	}
	index := viewer.currentRowIndex()
	if index < 0 {
		index = 0
	}
	index = min(max(0, index+delta), len(viewer.rows)-1)
	row := viewer.rows[index]
	preferredSide := viewer.cursor.side
	preferredLine := row.line
	if preferredSide == DiffViewerSideNew && row.peer >= 0 {
		preferredLine = row.peer
	}
	viewer.setCursorFromRow(index, preferredSide, preferredLine)
	viewer.ensureCursorVisible()
}

func (viewer *DiffViewer) moveSplitSide(side DiffViewerSide) {
	if viewer.effectiveMode != DiffViewerSplit || len(viewer.rows) == 0 {
		return
	}
	index := viewer.currentRowIndex()
	if index < 0 {
		return
	}
	row := viewer.rows[index]
	switch side {
	case DiffViewerSideOld:
		if row.line >= 0 {
			viewer.setCursorFromRow(index, side, row.line)
		}
	case DiffViewerSideNew:
		target := row.peer
		if row.side == DiffViewerSideNew {
			target = row.line
		}
		if target >= 0 {
			viewer.setCursorFromRow(index, side, target)
		}
	}
}

func (viewer *DiffViewer) currentFileID() string {
	file, ok := viewer.currentFile()
	if !ok {
		return ""
	}
	return file.ID
}

func (viewer *DiffViewer) rememberCurrentFileCursor() {
	if viewer.cursor.fileID == "" {
		return
	}
	viewer.fileCursors[viewer.cursor.fileID] = diffViewerFileCursor{
		lineIndex: viewer.cursor.lineIndex, peerLineIndex: viewer.cursor.peerLineIndex,
		continuation: viewer.cursor.continuation, side: viewer.cursor.side,
	}
}

func (viewer *DiffViewer) switchFile(delta int) {
	if len(viewer.files) < 2 || delta == 0 {
		return
	}
	viewer.rememberCurrentFileCursor()
	viewer.fileIndex = (viewer.fileIndex + delta + len(viewer.files)) % len(viewer.files)
	viewer.cursorRow = -1
	file := viewer.files[viewer.fileIndex]
	if saved, ok := viewer.fileCursors[file.ID]; ok {
		viewer.cursor = diffViewerCursor{
			fileID: file.ID, lineIndex: saved.lineIndex, peerLineIndex: saved.peerLineIndex,
			continuation: saved.continuation, side: saved.side,
		}
	} else {
		viewer.resetCursorForCurrentFile()
	}
	viewer.searchMatches = viewer.findMatches(viewer.searchQuery)
	viewer.searchMatch = -1
	viewer.notice = ""
	viewer.rebuild(true)
}

func (viewer *DiffViewer) togglePreferredMode() {
	if viewer.preferredMode == DiffViewerSplit {
		viewer.preferredMode = DiffViewerUnified
		viewer.notice = ""
	} else {
		viewer.preferredMode = DiffViewerSplit
	}
	viewer.rebuild(true)
	if viewer.preferredMode == DiffViewerSplit && viewer.effectiveMode != DiffViewerSplit {
		viewer.notice = viewer.layout.DisabledReason
	}
}

func (viewer *DiffViewer) hunkIndices() []int {
	file, ok := viewer.currentFile()
	if !ok {
		return nil
	}
	var indices []int
	for index, line := range file.Lines {
		if line.Kind == DiffHunkHeader {
			indices = append(indices, index)
		}
	}
	return indices
}

func (viewer *DiffViewer) currentHunkIndex() int {
	hunks := viewer.hunkIndices()
	if len(hunks) == 0 {
		return -1
	}
	current := viewer.cursor.lineIndex
	result := -1
	for index, lineIndex := range hunks {
		if lineIndex > current {
			break
		}
		result = index
	}
	return result
}

func (viewer *DiffViewer) moveHunk(delta int) {
	hunks := viewer.hunkIndices()
	if len(hunks) == 0 {
		viewer.notice = diffViewerUnavailableNoHunk
		return
	}
	current := viewer.currentHunkIndex()
	if delta > 0 {
		current = (current + 1) % len(hunks)
	} else {
		if current < 0 {
			current = 0
		}
		current = (current - 1 + len(hunks)) % len(hunks)
	}
	viewer.jumpToLine(hunks[current], DiffViewerSideUnified)
	viewer.notice = ""
}

func (viewer *DiffViewer) jumpToLine(lineIndex int, side DiffViewerSide) bool {
	if lineIndex < 0 {
		return false
	}
	for index, row := range viewer.rows {
		if !diffViewerRowContainsLine(row, lineIndex) {
			continue
		}
		resolvedSide := side
		if viewer.effectiveMode == DiffViewerUnified {
			resolvedSide = DiffViewerSideUnified
		} else if row.peer == lineIndex && side == DiffViewerSideUnified {
			resolvedSide = DiffViewerSideNew
		} else if side == DiffViewerSideUnified {
			resolvedSide = row.side
		}
		viewer.setCursorFromRow(index, resolvedSide, lineIndex)
		viewer.ensureCursorVisible()
		return true
	}
	return false
}

func (viewer *DiffViewer) startSearch() tea.Cmd {
	viewer.searching = true
	viewer.search.SetValue(viewer.searchQuery)
	viewer.search.CursorEnd()
	cmd := viewer.search.Focus()
	viewer.rebuild(true)
	return cmd
}

func (viewer *DiffViewer) finishSearch() {
	viewer.searchQuery = sanitizeTerminalSingleLine(viewer.search.Value())
	viewer.searchMatches = viewer.findMatches(viewer.searchQuery)
	viewer.searchMatch = -1
	viewer.searching = false
	viewer.search.Blur()
	viewer.rebuild(true)
	if len(viewer.searchMatches) == 0 {
		if viewer.searchQuery != "" {
			viewer.notice = "No matches."
		}
		return
	}
	viewer.nextMatch(1)
}

func (viewer *DiffViewer) cancelSearch() bool {
	if !viewer.searching {
		return false
	}
	viewer.searching = false
	viewer.search.Blur()
	viewer.search.SetValue(viewer.searchQuery)
	viewer.rebuild(true)
	return true
}

func (viewer *DiffViewer) findMatches(query string) []diffViewerLocation {
	query = strings.ToLower(sanitizeTerminalSingleLine(query))
	if query == "" {
		return nil
	}
	file, ok := viewer.currentFile()
	if !ok {
		return nil
	}
	matches := make([]diffViewerLocation, 0, 8)
	for lineIndex, line := range file.Lines {
		content := strings.ToLower(diffViewerLogicalContent(line))
		if strings.Contains(content, query) {
			matches = append(matches, diffViewerLocation{
				fileIndex: viewer.fileIndex,
				lineIndex: lineIndex,
			})
		}
	}
	return matches
}

func (viewer *DiffViewer) nextMatch(delta int) {
	if viewer.searchQuery == "" {
		viewer.notice = "Enter a search with / first."
		return
	}
	if len(viewer.searchMatches) == 0 {
		viewer.searchMatches = viewer.findMatches(viewer.searchQuery)
	}
	if len(viewer.searchMatches) == 0 {
		viewer.notice = "No matches."
		return
	}
	if viewer.searchMatch < 0 {
		if delta < 0 {
			viewer.searchMatch = 0
		} else {
			viewer.searchMatch = -1
		}
	}
	viewer.searchMatch = (viewer.searchMatch + delta + len(viewer.searchMatches)) %
		len(viewer.searchMatches)
	match := viewer.searchMatches[viewer.searchMatch]
	if match.fileIndex != viewer.fileIndex {
		viewer.fileIndex = match.fileIndex
		viewer.rebuild(false)
	}
	viewer.jumpToLine(match.lineIndex, DiffViewerSideUnified)
	viewer.notice = fmt.Sprintf(
		"Match %d/%d",
		viewer.searchMatch+1,
		len(viewer.searchMatches),
	)
}

// Back consumes Escape while search owns focus. False means the parent may
// close the modal.
func (viewer *DiffViewer) Back() bool {
	return viewer.cancelSearch()
}

// Update handles presentation input and returns a bounded parent intent.
func (viewer *DiffViewer) Update(msg tea.Msg) (DiffViewerEvent, tea.Cmd) {
	if viewer == nil {
		return DiffViewerEvent{}, nil
	}
	if click, ok := msg.(tea.MouseClickMsg); ok {
		return viewer.updatePointer(click), nil
	}
	if viewer.searching {
		if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
			switch keyMsg.String() {
			case "esc":
				viewer.cancelSearch()
				return DiffViewerEvent{}, nil
			case "enter":
				viewer.finishSearch()
				return DiffViewerEvent{}, nil
			}
		}
		var cmd tea.Cmd
		viewer.search, cmd = viewer.search.Update(msg)
		return DiffViewerEvent{}, cmd
	}

	switch typed := msg.(type) {
	case tea.KeyPressMsg:
		switch typed.String() {
		case "j", "down":
			viewer.moveCursor(1)
		case "k", "up":
			viewer.moveCursor(-1)
		case "pgdown", "ctrl+d":
			viewer.moveCursor(max(1, viewer.layout.BodyRect.Height()-1))
		case "pgup", "ctrl+u":
			viewer.moveCursor(-max(1, viewer.layout.BodyRect.Height()-1))
		case "home", "g":
			viewer.jumpToPhysicalRow(0)
		case "end", "G":
			viewer.jumpToPhysicalRow(len(viewer.rows) - 1)
		case "left", "h":
			viewer.moveSplitSide(DiffViewerSideOld)
		case "right", "l":
			viewer.moveSplitSide(DiffViewerSideNew)
		case "[":
			viewer.moveHunk(-1)
		case "]":
			viewer.moveHunk(1)
		case "tab":
			viewer.switchFile(1)
		case "shift+tab":
			viewer.switchFile(-1)
		case "/":
			return DiffViewerEvent{}, viewer.startSearch()
		case "n":
			viewer.nextMatch(1)
		case "N":
			viewer.nextMatch(-1)
		case "s":
			viewer.togglePreferredMode()
		case "c":
			return viewer.copyLineEvent(), nil
		case "C":
			return viewer.copyHunkEvent(), nil
		case "p":
			return viewer.copyPathEvent(), nil
		}
	case tea.MouseWheelMsg:
		if !viewer.layout.BodyRect.Contains(typed.X, typed.Y) {
			return DiffViewerEvent{}, nil
		}
		var cmd tea.Cmd
		viewer.viewport, cmd = viewer.viewport.Update(typed)
		viewer.keepCursorInsideVisibleBody()
		return DiffViewerEvent{}, cmd
	default:
		var cmd tea.Cmd
		viewer.viewport, cmd = viewer.viewport.Update(msg)
		return DiffViewerEvent{}, cmd
	}
	return DiffViewerEvent{}, nil
}

func (viewer *DiffViewer) updatePointer(click tea.MouseClickMsg) DiffViewerEvent {
	if click.Button != tea.MouseLeft {
		return DiffViewerEvent{}
	}
	if viewer.searching && viewer.layout.SearchRect.Contains(click.X, click.Y) {
		return DiffViewerEvent{}
	}
	if !viewer.layout.BodyRect.Contains(click.X, click.Y) || len(viewer.rows) == 0 {
		return DiffViewerEvent{}
	}

	physicalRow := viewer.viewport.YOffset() + click.Y - viewer.layout.BodyRect.MinY
	if physicalRow < 0 || physicalRow >= len(viewer.rows) {
		return DiffViewerEvent{}
	}
	if viewer.searching {
		// Resolve the clicked semantic row before removing the search row from
		// layout; finishing search may move BodyRect by one screen cell.
		viewer.finishSearch()
		physicalRow = min(physicalRow, len(viewer.rows)-1)
	}
	viewer.selectPointerRow(physicalRow, click.X)
	return DiffViewerEvent{}
}

func (viewer *DiffViewer) selectPointerRow(index int, screenX int) {
	if index < 0 || index >= len(viewer.rows) {
		return
	}
	row := viewer.rows[index]
	side, line := row.side, row.line
	if viewer.effectiveMode == DiffViewerSplit {
		oldPaneWidth := viewer.layout.OldDigits +
			diffViewerSplitPaneChromeColumns +
			viewer.layout.OldCodeWidth
		newPaneStart := viewer.layout.BodyRect.MinX +
			oldPaneWidth +
			diffViewerSplitGapColumns
		switch {
		case screenX < viewer.layout.BodyRect.MinX+oldPaneWidth:
			if row.side != DiffViewerSideOld {
				// This is the intentionally blank old side of an added-only
				// row, so it has no semantic selection target.
				return
			}
			side, line = DiffViewerSideOld, row.line
		case screenX >= newPaneStart:
			switch {
			case row.side == DiffViewerSideOld && row.peer >= 0:
				side, line = DiffViewerSideNew, row.peer
			case row.side == DiffViewerSideNew:
				side, line = DiffViewerSideNew, row.line
			default:
				return
			}
		default:
			// The separator is chrome, not either source side.
			return
		}
	}
	viewer.setCursorFromRow(index, side, line)
}

func (viewer *DiffViewer) keepCursorInsideVisibleBody() {
	if viewer == nil || len(viewer.rows) == 0 {
		return
	}
	current := viewer.currentRowIndex()
	top := viewer.viewport.YOffset()
	bottom := min(len(viewer.rows), top+max(1, viewer.viewport.Height()))
	target := current
	switch {
	case current < top:
		target = top
	case current >= bottom:
		target = max(top, bottom-1)
	}
	if target == current || target < 0 || target >= len(viewer.rows) {
		return
	}
	row := viewer.rows[target]
	viewer.setCursorFromRow(target, row.side, row.line)
}

func (viewer *DiffViewer) jumpToPhysicalRow(index int) {
	if len(viewer.rows) == 0 {
		return
	}
	index = min(max(0, index), len(viewer.rows)-1)
	row := viewer.rows[index]
	viewer.setCursorFromRow(index, row.side, row.line)
	viewer.ensureCursorVisible()
}

func (viewer *DiffViewer) copyLineEvent() DiffViewerEvent {
	file, ok := viewer.currentFile()
	lineIndex := viewer.cursor.lineIndex
	if !ok || file.Revision == 0 {
		return viewer.unavailableEvent(DiffViewerActionCopyLine, diffViewerUnavailablePending)
	}
	if lineIndex < 0 || lineIndex >= len(file.Lines) {
		return viewer.unavailableEvent(DiffViewerActionCopyLine, diffViewerUnavailableNoSelection)
	}
	return viewer.baseEvent(DiffViewerEventCopyLine, DiffViewerActionCopyLine, lineIndex, boundedDiffViewerText(
		diffViewerCopyLine(file.Lines[lineIndex]),
		diffViewerMaximumCopyBytes,
	))
}

func (viewer *DiffViewer) copyHunkEvent() DiffViewerEvent {
	file, ok := viewer.currentFile()
	if !ok || file.Revision == 0 {
		return viewer.unavailableEvent(DiffViewerActionCopyHunk, diffViewerUnavailablePending)
	}
	if file.Truncated {
		return viewer.unavailableEvent(DiffViewerActionCopyHunk, diffViewerUnavailableTruncatedHunk)
	}
	hunks := viewer.hunkIndices()
	hunkIndex := viewer.currentHunkIndex()
	if hunkIndex < 0 || hunkIndex >= len(hunks) {
		return viewer.unavailableEvent(DiffViewerActionCopyHunk, diffViewerUnavailableNoHunk)
	}
	start := hunks[hunkIndex]
	end := len(file.Lines)
	if hunkIndex+1 < len(hunks) {
		end = hunks[hunkIndex+1]
	}
	lines := make([]string, 0, end-start)
	for _, line := range file.Lines[start:end] {
		lines = append(lines, diffViewerCopyLine(line))
	}
	event := viewer.baseEvent(
		DiffViewerEventCopyHunk,
		DiffViewerActionCopyHunk,
		viewer.cursor.lineIndex,
		boundedDiffViewerText(strings.Join(lines, "\n"), diffViewerMaximumCopyBytes),
	)
	event.HunkIndex = hunkIndex
	return event
}

func (viewer *DiffViewer) copyPathEvent() DiffViewerEvent {
	file, ok := viewer.currentFile()
	if !ok {
		return viewer.unavailableEvent(DiffViewerActionCopyPath, diffViewerUnavailableNoFiles)
	}
	return viewer.baseEvent(
		DiffViewerEventCopyPath,
		DiffViewerActionCopyPath,
		viewer.cursor.lineIndex,
		boundedDiffViewerText(file.DisplayPath, diffViewerMaximumCopyBytes),
	)
}

func diffViewerCopyLine(line DiffLine) string {
	content := diffViewerLogicalContent(line)
	switch line.Kind {
	case DiffAdded:
		return "+" + content
	case DiffRemoved:
		return "-" + content
	case DiffContext:
		return " " + content
	default:
		return content
	}
}

func boundedDiffViewerText(value string, limit int) string {
	value = sanitizeTerminalMultiline(value)
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func (viewer *DiffViewer) baseEvent(
	kind DiffViewerEventKind,
	action DiffViewerAction,
	lineIndex int,
	text string,
) DiffViewerEvent {
	file, _ := viewer.currentFile()
	return DiffViewerEvent{
		Kind: kind, Action: action, BlockID: viewer.origin,
		FileID: file.ID, FileIndex: viewer.fileIndex,
		HunkIndex: max(0, viewer.currentHunkIndex()), LineIndex: max(0, lineIndex),
		Text: text, Truncated: file.Truncated,
	}
}

func (viewer *DiffViewer) unavailableEvent(
	action DiffViewerAction,
	reason string,
) DiffViewerEvent {
	viewer.notice = reason
	event := viewer.baseEvent(DiffViewerEventUnavailable, action, viewer.cursor.lineIndex, "")
	event.DisabledReason = sanitizeTerminalSingleLine(reason)
	return event
}

// View renders a frame-local modal. Its exact placement is described by
// Layout().OuterRect; the parent may center/overlay this frame without asking
// the child to infer the base transcript geometry.
func (viewer *DiffViewer) View() string {
	view, _ := viewer.ViewWithCursor()
	return view
}

// ViewWithCursor renders the modal and a cursor local to the returned frame.
func (viewer *DiffViewer) ViewWithCursor() (string, *tea.Cursor) {
	if viewer == nil || viewer.layout.OuterRect.Empty() {
		return "", nil
	}
	outerWidth := viewer.layout.OuterRect.Width()
	outerHeight := viewer.layout.OuterRect.Height()
	innerWidth := max(0, outerWidth-2)
	innerHeight := max(0, outerHeight-2)
	innerRows := make([]string, innerHeight)
	for index := range innerRows {
		innerRows[index] = strings.Repeat(" ", innerWidth)
	}

	viewer.placeFrameRect(innerRows, viewer.layout.HeaderRect, viewer.renderHeader())
	var cursor *tea.Cursor
	if viewer.searching && !viewer.layout.SearchRect.Empty() {
		search := viewer.search
		search.SetWidth(max(1, viewer.layout.SearchRect.Width()-2))
		search.SetVirtualCursor(false)
		searchView := search.View()
		viewer.placeFrameRect(innerRows, viewer.layout.SearchRect, searchView)
		cursor = search.Cursor()
		if cursor != nil {
			cursor = offsetCursor(
				cursor,
				viewer.layout.SearchRect.MinX-viewer.layout.OuterRect.MinX,
				viewer.layout.SearchRect.MinY-viewer.layout.OuterRect.MinY,
			)
		}
	}
	viewer.placeFrameRect(innerRows, viewer.layout.BodyRect, viewer.renderVisibleBody())
	viewer.placeFrameRect(innerRows, viewer.layout.FooterRect, viewer.renderFooter())

	glyphs := diffViewerBorderGlyphs(viewer.glyphProfile)
	borderStyle := viewer.styles.ThinkingBorder.PaddingLeft(0)
	top := borderStyle.Render(glyphs.topLeft +
		strings.Repeat(glyphs.horizontal, max(0, outerWidth-2)) + glyphs.topRight)
	bottom := borderStyle.Render(glyphs.bottomLeft +
		strings.Repeat(glyphs.horizontal, max(0, outerWidth-2)) + glyphs.bottomRight)
	lines := make([]string, 0, outerHeight)
	lines = append(lines, fitDiffViewerRow(top, outerWidth))
	for _, row := range innerRows {
		lines = append(lines,
			borderStyle.Render(glyphs.vertical)+
				fitDiffViewerRow(row, innerWidth)+
				borderStyle.Render(glyphs.vertical),
		)
	}
	if outerHeight > 1 {
		lines = append(lines, fitDiffViewerRow(bottom, outerWidth))
	}
	if len(lines) > outerHeight {
		lines = lines[:outerHeight]
	}
	return strings.Join(lines, "\n"), cursor
}

// renderVisibleBody overlays selection onto the bounded viewport frame. The
// full document remains cached with neutral markers, so navigation changes one
// visible cell instead of rewrapping, restyling, and resetting every diff row.
func (viewer *DiffViewer) renderVisibleBody() string {
	body := viewer.viewport.View()
	rowIndex := viewer.currentRowIndex()
	screenRow := rowIndex - viewer.viewport.YOffset()
	if rowIndex < 0 || screenRow < 0 || screenRow >= viewer.viewport.Height() {
		return body
	}
	column, ok := viewer.selectionColumn(rowIndex)
	if !ok {
		return body
	}
	lines := strings.Split(body, "\n")
	if screenRow >= len(lines) {
		return body
	}
	selected := viewer.styles.FocusIndicator.Bold(true).Render(
		glyphSet(viewer.glyphProfile).Selected,
	)
	lines[screenRow] = replaceDiffViewerCells(lines[screenRow], column, selected, 1)
	return strings.Join(lines, "\n")
}

func (viewer *DiffViewer) selectionColumn(rowIndex int) (int, bool) {
	if rowIndex < 0 || rowIndex >= len(viewer.rows) {
		return 0, false
	}
	row := viewer.rows[rowIndex]
	if viewer.effectiveMode != DiffViewerSplit {
		// Preserve the semantic split side through a narrow fallback so it can
		// be restored later, while painting the single unified selector.
		return 0, true
	}
	if row.side == DiffViewerSideUnified {
		return 0, viewer.cursor.side == DiffViewerSideUnified
	}
	switch viewer.cursor.side {
	case DiffViewerSideOld:
		return 0, row.side == DiffViewerSideOld
	case DiffViewerSideNew:
		hasNewSide := row.side == DiffViewerSideNew || row.peer >= 0
		return viewer.layout.OldDigits +
			diffViewerSplitPaneChromeColumns +
			viewer.layout.OldCodeWidth +
			diffViewerSplitGapColumns, hasNewSide
	default:
		return 0, false
	}
}

func (viewer *DiffViewer) placeFrameRect(
	innerRows []string,
	rect CellRect,
	content string,
) {
	if rect.Empty() || len(innerRows) == 0 {
		return
	}
	localX := rect.MinX - viewer.layout.OuterRect.MinX - 1
	localY := rect.MinY - viewer.layout.OuterRect.MinY - 1
	contentRows := strings.Split(content, "\n")
	for rowIndex := 0; rowIndex < rect.Height() && rowIndex < len(contentRows); rowIndex++ {
		target := localY + rowIndex
		if target < 0 || target >= len(innerRows) {
			continue
		}
		innerRows[target] = replaceDiffViewerCells(
			innerRows[target],
			localX,
			fitDiffViewerRow(contentRows[rowIndex], rect.Width()),
			rect.Width(),
		)
	}
}

func replaceDiffViewerCells(base string, x int, value string, width int) string {
	baseWidth := lipglossWidth(base)
	if x < 0 || x >= baseWidth || width <= 0 {
		return base
	}
	left := ansi.Cut(base, 0, x)
	rightStart := min(baseWidth, x+width)
	right := ""
	if rightStart < baseWidth {
		right = ansi.Cut(base, rightStart, baseWidth)
	}
	return left + fitDiffViewerRow(value, min(width, baseWidth-x)) + right
}

type diffViewerBorderSet struct {
	topLeft, topRight, bottomLeft, bottomRight string
	horizontal, vertical                       string
}

func diffViewerBorderGlyphs(profile GlyphProfile) diffViewerBorderSet {
	if resolveGlyphProfile(profile) == GlyphASCII {
		return diffViewerBorderSet{
			topLeft: "+", topRight: "+", bottomLeft: "+", bottomRight: "+",
			horizontal: "-", vertical: "|",
		}
	}
	return diffViewerBorderSet{
		topLeft: "╭", topRight: "╮", bottomLeft: "╰", bottomRight: "╯",
		horizontal: "─", vertical: "│",
	}
}

func (viewer *DiffViewer) renderHeader() string {
	file, ok := viewer.currentFile()
	if !ok {
		return viewer.styles.OverlayTitle.Render(diffViewerUnavailableNoFiles)
	}
	state := viewer.effectiveMode.String()
	if viewer.preferredMode == DiffViewerSplit && viewer.effectiveMode != DiffViewerSplit {
		state = "unified" + viewer.chromeSeparator() + "split paused"
	}
	flags := []string{state}
	if len(viewer.files) > 1 {
		flags = append(flags, fmt.Sprintf("%d/%d", viewer.fileIndex+1, len(viewer.files)))
	}
	switch {
	case file.Revision == 0:
		flags = append(flags, "loading")
	case file.Truncated:
		flags = append(flags, "truncated")
	default:
		flags = append(flags, "r"+strconv.FormatUint(file.Revision, 10))
	}
	status := strings.Join(flags, viewer.chromeSeparator())
	width := viewer.layout.HeaderRect.Width()
	statusWidth := lipglossWidth(status)
	pathWidth := max(1, width-statusWidth-1)
	path := truncateDisplayWithGlyphProfile(file.DisplayPath, pathWidth, viewer.glyphProfile)
	gap := strings.Repeat(" ", max(1, width-lipglossWidth(path)-statusWidth))
	return viewer.styles.OverlayTitle.Render(path) +
		gap + viewer.styles.OverlayDim.Render(status)
}

func (viewer *DiffViewer) renderFooter() string {
	width := viewer.layout.FooterRect.Width()
	message := viewer.notice
	if message == "" {
		switch {
		case len(viewer.files) == 0:
			message = diffViewerUnavailableNoFiles
		case !viewer.layout.UnifiedAvailable:
			message = viewer.layout.DisabledReason
		case viewer.preferredMode == DiffViewerSplit && viewer.effectiveMode != DiffViewerSplit:
			message = viewer.layout.DisabledReason
		default:
			separator := viewer.chromeSeparator()
			hints := []string{"[ ] hunk"}
			if len(viewer.files) > 1 {
				hints = append(hints, "tab file")
			}
			hints = append(
				hints,
				"/ search",
				"n/N match",
				"s view",
				"c/C copy",
				"p path",
			)
			message = strings.Join(hints, separator)
		}
	}
	return viewer.styles.OverlayDim.Render(truncateDisplayWithGlyphProfile(
		message,
		max(0, width),
		viewer.glyphProfile,
	))
}

func (viewer *DiffViewer) chromeSeparator() string {
	if viewer != nil && viewer.glyphProfile == GlyphASCII {
		return " | "
	}
	return " · "
}
