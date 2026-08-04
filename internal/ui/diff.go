package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const (
	maxDiffSnapshotBytes = 2 * 1024 * 1024
	maxDiffSnapshotWait  = 50 * time.Millisecond
	maxDiffInputLines    = 50_000
	maxDiffCells         = 4_000_000
	diffContextLines     = 3
	// Below this many terminal cells the old/new number columns are omitted so
	// narrow panes keep their full content budget for the diff text itself.
	diffGutterNumbersMinWidth = 72
)

var diffSnapshotSlots = make(chan struct{}, 1)

// DiffLineKind represents the type of a diff line.
type DiffLineKind int

const (
	DiffContext DiffLineKind = iota
	DiffAdded
	DiffRemoved
	DiffHunkHeader
	DiffEllipsis
	DiffOmitted
	DiffNoNewline
)

const diffNoNewlineContent = `\ No newline at end of file`

// DiffHunk is the structural range represented by one unified-diff hunk.
// Keeping the range typed means rendering and session restore never need to
// reverse-engineer coordinates from already-styled text.
type DiffHunk struct {
	OldStart int `json:"OldStart,omitempty"`
	OldCount int `json:"OldCount,omitempty"`
	NewStart int `json:"NewStart,omitempty"`
	NewCount int `json:"NewCount,omitempty"`
}

// DiffLine is a single line in a unified diff.
type DiffLine struct {
	Kind    DiffLineKind
	Content string
	OldLine int       `json:"OldLine,omitempty"`
	NewLine int       `json:"NewLine,omitempty"`
	Hunk    *DiffHunk `json:"Hunk,omitempty"`
}

type diffSnapshot struct {
	Content   string
	Available bool
}

// diffTextLine retains whether the source line ended in a newline. Comparing
// termination as part of the typed line is what makes a final-newline-only
// edit observable without smuggling sentinels into user-controlled content.
type diffTextLine struct {
	Content    string
	Terminated bool
}

// diffBuildRequest contains only the bounded, immutable data needed by the
// asynchronous patch builder. Raw tool arguments never cross this boundary.
type diffBuildRequest struct {
	Generation      uint64
	ToolID          string
	ToolName        string
	Path            string
	WorkDir         string
	Before          string
	BeforeAvailable bool
}

type diffBuildResultMsg struct {
	Generation uint64
	ToolID     string
	ToolName   string
	Lines      []DiffLine
	Available  bool
}

func buildFileDiffCmd(request diffBuildRequest) tea.Cmd {
	return func() tea.Msg {
		result := diffBuildResultMsg{
			Generation: request.Generation,
			ToolID:     request.ToolID,
			ToolName:   request.ToolName,
		}
		if request.Path == "" || !request.BeforeAvailable {
			return result
		}

		after := readDiffSnapshotForPathAt(request.Path, request.WorkDir)
		if !after.Available {
			return result
		}
		result.Lines = computeDiff(request.Before, after.Content)
		result.Available = true
		return result
	}
}

// readFileForDiff extracts a file path from tool args and reads its content.
func readFileForDiff(rawArgs map[string]any) string {
	workDir, _ := os.Getwd()
	return readFileForDiffAt(rawArgs, workDir)
}

func readFileForDiffAt(rawArgs map[string]any, workDir string) string {
	return readDiffSnapshotForArgsAt(rawArgs, workDir).Content
}

func readDiffSnapshotForArgsAt(rawArgs map[string]any, workDir string) diffSnapshot {
	return readDiffSnapshotForPathAt(diffPathFromArgs(rawArgs), workDir)
}

func diffPathFromArgs(rawArgs map[string]any) string {
	for _, key := range []string{"path", "file_path", "filename", "file"} {
		if path, ok := rawArgs[key].(string); ok {
			return path
		}
	}
	return ""
}

func readDiffSnapshotForPathAt(path, workDir string) diffSnapshot {
	if strings.TrimSpace(path) == "" {
		return diffSnapshot{}
	}
	// Snapshotting is a UI enhancement executed outside Bubble Tea's Update
	// loop. One abandoned network-filesystem syscall is tolerated; later
	// snapshots fail fast while that bounded slot remains occupied.
	select {
	case diffSnapshotSlots <- struct{}{}:
	default:
		return diffSnapshot{}
	}
	done := make(chan diffSnapshot, 1)
	go func() {
		defer func() { <-diffSnapshotSlots }()
		done <- readDiffSnapshotForPathAtUnbounded(path, workDir)
	}()
	timer := time.NewTimer(maxDiffSnapshotWait)
	defer timer.Stop()
	select {
	case snapshot := <-done:
		return snapshot
	case <-timer.C:
		return diffSnapshot{}
	}
}

func readDiffSnapshotForPathAtUnbounded(path, workDir string) diffSnapshot {
	root, relative, err := confinedDiffRelativePath(path, workDir)
	if err != nil {
		return diffSnapshot{}
	}
	file, err := safeio.OpenWithinNoFollow(root, relative)
	if errors.Is(err, os.ErrNotExist) {
		// A missing pre-write file is a valid empty side of a new-file patch.
		return diffSnapshot{Available: true}
	}
	if err != nil {
		return diffSnapshot{}
	}
	defer func() { _ = file.Close() }()
	openedInfo, statErr := file.Stat()
	if statErr != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() > maxDiffSnapshotBytes {
		return diffSnapshot{}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDiffSnapshotBytes+1))
	if err != nil || len(data) > maxDiffSnapshotBytes {
		return diffSnapshot{}
	}
	return diffSnapshot{Content: string(data), Available: true}
}

func confinedDiffRelativePath(path, workDir string) (string, string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", errors.New("empty diff path")
	}
	root, err := filepath.Abs(workDir)
	if err != nil {
		return "", "", err
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", errors.New("diff path escapes workspace")
	}
	return root, rel, nil
}

// computeDiff computes a line-level diff between before and after text.
// Returns nil if the texts are identical.
func computeDiff(before, after string) []DiffLine {
	if before == after {
		return nil
	}
	if !utf8.ValidString(before) || !utf8.ValidString(after) ||
		strings.IndexByte(before, 0) >= 0 || strings.IndexByte(after, 0) >= 0 {
		return omittedBinaryDiffLines(len(before), len(after))
	}

	beforeLines := splitDiffTextLines(before)
	afterLines := splitDiffTextLines(after)
	if len(beforeLines)+len(afterLines) > maxDiffInputLines {
		return omittedDiffLines(len(beforeLines), len(afterLines))
	}
	if len(beforeLines) > 0 && len(afterLines) > maxDiffCells/len(beforeLines) {
		return omittedDiffLines(len(beforeLines), len(afterLines))
	}

	lcs := lcsComparable(beforeLines, afterLines)

	all := make([]DiffLine, 0, len(beforeLines)+len(afterLines))
	bi, ai, li := 0, 0, 0
	oldLine, newLine := 1, 1

	for li < len(lcs) {
		for bi < len(beforeLines) && beforeLines[bi] != lcs[li] {
			all = appendDiffTextLine(all, DiffRemoved, beforeLines[bi], oldLine, 0)
			bi++
			oldLine++
		}
		for ai < len(afterLines) && afterLines[ai] != lcs[li] {
			all = appendDiffTextLine(all, DiffAdded, afterLines[ai], 0, newLine)
			ai++
			newLine++
		}
		all = appendDiffTextLine(all, DiffContext, lcs[li], oldLine, newLine)
		bi++
		ai++
		li++
		oldLine++
		newLine++
	}
	for bi < len(beforeLines) {
		all = appendDiffTextLine(all, DiffRemoved, beforeLines[bi], oldLine, 0)
		bi++
		oldLine++
	}
	for ai < len(afterLines) {
		all = appendDiffTextLine(all, DiffAdded, afterLines[ai], 0, newLine)
		ai++
		newLine++
	}

	return buildDiffHunks(all, diffContextLines)
}

func appendDiffTextLine(lines []DiffLine, kind DiffLineKind, line diffTextLine, oldLine, newLine int) []DiffLine {
	lines = append(lines, DiffLine{
		Kind: kind, Content: line.Content, OldLine: oldLine, NewLine: newLine,
	})
	if !line.Terminated {
		lines = append(lines, DiffLine{Kind: DiffNoNewline, Content: diffNoNewlineContent})
	}
	return lines
}

func omittedDiffLines(beforeLines, afterLines int) []DiffLine {
	return []DiffLine{
		{Kind: DiffOmitted, Content: fmt.Sprintf("[large diff omitted: %d lines before]", beforeLines)},
		{Kind: DiffOmitted, Content: fmt.Sprintf("[large diff omitted: %d lines after]", afterLines)},
	}
}

func omittedBinaryDiffLines(beforeBytes, afterBytes int) []DiffLine {
	return []DiffLine{
		{Kind: DiffOmitted, Content: fmt.Sprintf("[binary diff omitted: %d bytes before]", beforeBytes)},
		{Kind: DiffOmitted, Content: fmt.Sprintf("[binary diff omitted: %d bytes after]", afterBytes)},
	}
}

type diffRange struct {
	start int
	end   int
}

// buildDiffHunks groups changed operations with bounded context and emits
// typed headers and omission markers. The body retains exact old/new line
// coordinates for gutters and durable session restore.
func buildDiffHunks(lines []DiffLine, contextLines int) []DiffLine {
	if len(lines) == 0 {
		return nil
	}
	if contextLines < 0 {
		contextLines = 0
	}

	var ranges []diffRange
	for index, line := range lines {
		if line.Kind != DiffAdded && line.Kind != DiffRemoved {
			continue
		}
		candidate := diffRange{
			start: max(0, index-contextLines),
			end:   min(len(lines)-1, index+contextLines),
		}
		if len(ranges) > 0 && candidate.start <= ranges[len(ranges)-1].end+1 {
			ranges[len(ranges)-1].end = max(ranges[len(ranges)-1].end, candidate.end)
			continue
		}
		ranges = append(ranges, candidate)
	}
	if len(ranges) == 0 {
		return nil
	}

	result := make([]DiffLine, 0, len(lines)+len(ranges)*2)
	previousEnd := -1
	for _, span := range ranges {
		if span.start > previousEnd+1 {
			result = append(result, DiffLine{Kind: DiffEllipsis, Content: "… unchanged lines"})
		}
		hunk := diffHunkForLines(lines[span.start : span.end+1])
		result = append(result, DiffLine{
			Kind:    DiffHunkHeader,
			Content: formatDiffHunk(hunk),
			Hunk:    &hunk,
		})
		result = append(result, lines[span.start:span.end+1]...)
		previousEnd = span.end
	}
	if previousEnd < len(lines)-1 {
		result = append(result, DiffLine{Kind: DiffEllipsis, Content: "… unchanged lines"})
	}
	return result
}

func diffHunkForLines(lines []DiffLine) DiffHunk {
	var hunk DiffHunk
	for _, line := range lines {
		if line.OldLine > 0 {
			if hunk.OldStart == 0 {
				hunk.OldStart = line.OldLine
			}
			hunk.OldCount++
		}
		if line.NewLine > 0 {
			if hunk.NewStart == 0 {
				hunk.NewStart = line.NewLine
			}
			hunk.NewCount++
		}
	}
	if hunk.OldCount == 0 && hunk.NewStart > 0 {
		hunk.OldStart = max(0, hunk.NewStart-1)
	}
	if hunk.NewCount == 0 && hunk.OldStart > 0 {
		hunk.NewStart = max(0, hunk.OldStart-1)
	}
	return hunk
}

func formatDiffHunk(hunk DiffHunk) string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@", hunk.OldStart, hunk.OldCount, hunk.NewStart, hunk.NewCount)
}

// renderDiff renders diff lines with styles, capping output at maxLines.
func renderDiff(lines []DiffLine, styles Styles, maxLines int) string {
	return renderUnifiedDiffAtWidth("", lines, styles, maxLines, 0)
}

// renderDiffAtWidth renders a diff within a concrete terminal-cell boundary.
// A zero width preserves the historical unbounded behavior used by callers
// that do not own a viewport.
func renderDiffAtWidth(lines []DiffLine, styles Styles, maxLines, width int) string {
	return renderUnifiedDiffAtWidth("", lines, styles, maxLines, width)
}

// renderUnifiedDiffAtWidth renders a full-width unified patch with a file
// header, typed hunk headers, and old/new line-number gutters.
func renderUnifiedDiffAtWidth(
	path string,
	lines []DiffLine,
	styles Styles,
	maxLines,
	width int,
	profiles ...GlyphProfile,
) string {
	if len(lines) == 0 {
		return ""
	}
	profile := resolveGlyphProfile(profiles...)

	var b strings.Builder
	b.WriteString(renderDiffFileHeader(path, lines, styles, width, profile))
	b.WriteByte('\n')

	displayed := 0
	gutter := resolveDiffGutter(lines, styles, width)
	for index, line := range lines {
		var renderedRows []string
		switch line.Kind {
		case DiffAdded:
			renderedRows = renderDiffBodyRows(line, "+", gutter.numberAt(index), gutter, styles.DiffAdded, width, profiles...)
		case DiffRemoved:
			renderedRows = renderDiffBodyRows(line, "-", gutter.numberAt(index), gutter, styles.DiffRemoved, width, profiles...)
		case DiffContext:
			renderedRows = renderDiffBodyRows(line, " ", gutter.numberAt(index), gutter, styles.DiffContext, width, profiles...)
		case DiffHunkHeader:
			header := line.Content
			if line.Hunk != nil {
				header = formatDiffHunk(*line.Hunk)
			}
			renderedRows = []string{renderDiffMetaLine(header, styles, width, profile)}
		case DiffEllipsis:
			content := line.Content
			if content == "" {
				content = "… unchanged lines"
			}
			renderedRows = []string{renderDiffMetaLine(content, styles, width, profile)}
		case DiffOmitted:
			renderedRows = []string{renderDiffMetaLine(line.Content, styles, width, profile)}
		case DiffNoNewline:
			renderedRows = []string{renderDiffMetaLine(diffNoNewlineContent, styles, width, profile)}
		default:
			renderedRows = []string{renderDiffMetaLine(line.Content, styles, width, profile)}
		}

		if maxLines > 0 {
			remaining := maxLines - displayed
			needsMarker := len(renderedRows) > remaining ||
				(len(renderedRows) == remaining && index < len(lines)-1)
			if needsMarker {
				visible := max(0, remaining-1)
				visible = min(visible, len(renderedRows))
				for _, row := range renderedRows[:visible] {
					b.WriteString(row)
					b.WriteByte('\n')
					displayed++
				}
				if remaining > 0 {
					b.WriteString(renderDiffMetaLine(
						diffContinuationLabel(visible, len(renderedRows), len(lines)-index-1, profile),
						styles,
						width,
						profile,
					))
					b.WriteByte('\n')
				}
				break
			}
		}
		for _, row := range renderedRows {
			b.WriteString(row)
			b.WriteByte('\n')
			displayed++
		}
	}

	return b.String()
}

func renderDiffLoadingAtWidth(path string, styles Styles, width int, profiles ...GlyphProfile) string {
	return renderDiffHeaderStatus(path, "diff loading", styles, width, profiles...)
}

func renderDiffFileHeader(
	path string,
	lines []DiffLine,
	styles Styles,
	width int,
	profiles ...GlyphProfile,
) string {
	added, removed, known := diffTotals(lines)
	status := fmt.Sprintf("+%d -%d", added, removed)
	if !known {
		status = "+? -?"
	}
	return renderDiffHeaderStatus(path, status, styles, width, profiles...)
}

func renderDiffHeaderStatus(
	path, status string,
	styles Styles,
	width int,
	profiles ...GlyphProfile,
) string {
	profile := resolveGlyphProfile(profiles...)
	path = sanitizeTerminalSingleLine(path)
	if path == "" {
		path = "patch"
	}
	status = sanitizeTerminalSingleLine(status)
	pathStyle := styles.DiffHeader.PaddingLeft(0)

	if width <= 0 {
		return pathStyle.Render(path) + "  " + renderDiffStatus(status, styles)
	}
	statusWidth := lipglossWidth(status)
	if statusWidth >= width {
		return pathStyle.Render(truncateDisplayWithGlyphProfile(status, width, profile))
	}
	pathBudget := max(1, width-statusWidth-1)
	path = truncateDisplayWithGlyphProfile(path, pathBudget, profile)
	gap := max(1, width-lipglossWidth(path)-statusWidth)
	return pathStyle.Render(path) + strings.Repeat(" ", gap) + renderDiffStatus(status, styles)
}

func renderDiffStatus(status string, styles Styles) string {
	parts := strings.Fields(status)
	if len(parts) == 2 && strings.HasPrefix(parts[0], "+") && strings.HasPrefix(parts[1], "-") {
		return styles.DiffAdded.PaddingLeft(0).Render(parts[0]) + " " +
			styles.DiffRemoved.PaddingLeft(0).Render(parts[1])
	}
	return styles.DiffHeader.PaddingLeft(0).Render(status)
}

func renderDiffBodyRows(
	line DiffLine,
	marker string,
	numbers diffGutterNumbers,
	gutter diffGutter,
	style lipgloss.Style,
	width int,
	profiles ...GlyphProfile,
) []string {
	prefix := ""
	if gutter.withNumbers {
		oldNumber := ""
		if numbers.old > 0 {
			oldNumber = strconv.Itoa(numbers.old)
		}
		newNumber := ""
		if numbers.new > 0 {
			newNumber = strconv.Itoa(numbers.new)
		}
		prefix = fmt.Sprintf("%*s %*s ", gutter.oldWidth, oldNumber, gutter.newWidth, newNumber)
	}
	glyphs := glyphSet(resolveGlyphProfile(profiles...))
	body := glyphs.Vertical + " " + marker + " "
	// Literal tabs do not have an intrinsic cell width: the terminal expands
	// them from the current cursor column. Normalize them before measuring so
	// wrapping has the same geometry in every terminal and code indentation
	// remains visible.
	content := strings.ReplaceAll(sanitizeTerminalLine(line.Content), "\t", "    ")
	contentWidth := 0
	if width > 0 {
		contentWidth = max(1, width-lipglossWidth(prefix+body))
	}
	chunks := []string{content}
	if contentWidth > 0 && lipglossWidth(content) > contentWidth {
		chunks = splitDisplayChunks(content, contentWidth)
	}
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	rows := make([]string, 0, len(chunks))
	for index, chunk := range chunks {
		rowPrefix := prefix
		rowBody := body
		if index > 0 {
			rowPrefix = strings.Repeat(" ", lipglossWidth(prefix))
			rowBody = glyphs.Vertical + " " + glyphs.Continuation + " "
		}
		rendered := style.PaddingLeft(0).Render(rowBody + chunk)
		if rowPrefix != "" {
			rendered = gutter.numberStyle.PaddingLeft(0).Render(rowPrefix) + rendered
		}
		rows = append(rows, rendered)
	}
	return rows
}

func diffContinuationLabel(
	visibleRows,
	currentRows,
	remainingSourceLines int,
	profiles ...GlyphProfile,
) string {
	prefix := "…"
	separator := " · "
	if resolveGlyphProfile(profiles...) == GlyphASCII {
		prefix = "..."
		separator = " | "
	}
	switch {
	case visibleRows > 0 && visibleRows < currentRows && remainingSourceLines > 0:
		return fmt.Sprintf(
			"%s line continues%s%d more lines",
			prefix,
			separator,
			remainingSourceLines,
		)
	case visibleRows > 0 && visibleRows < currentRows:
		return prefix + " line continues"
	default:
		// No part of the current source line was shown. Count it alongside
		// later source lines rather than claiming that an unseen line
		// "continues".
		return fmt.Sprintf("%s %d more lines", prefix, remainingSourceLines+1)
	}
}

func renderDiffMetaLine(
	content string,
	styles Styles,
	width int,
	profiles ...GlyphProfile,
) string {
	profile := resolveGlyphProfile(profiles...)
	content = sanitizeTerminalSingleLine(content)
	if profile == GlyphASCII {
		// Diff meta is host-owned chrome, unlike body content. Translate the
		// semantic punctuation here so a typed omission restored from an older
		// Unicode session still respects the active terminal profile.
		content = strings.ReplaceAll(content, "…", "...")
		content = strings.ReplaceAll(content, "·", "|")
	}
	if width > 0 {
		content = truncateDisplayWithGlyphProfile(content, width, profile)
	}
	return styles.DiffHeader.PaddingLeft(0).Render(content)
}

// diffGutterNumbers is the resolved old/new coordinate pair for one body line.
// A zero side renders as a blank column (the line has no counterpart there).
type diffGutterNumbers struct {
	old int
	new int
}

// diffGutter is the per-render gutter plan: whether number columns are shown,
// their right-aligned widths, and the dimmed style applied to numbers only.
type diffGutter struct {
	withNumbers bool
	numbers     []diffGutterNumbers
	oldWidth    int
	newWidth    int
	numberStyle lipgloss.Style
}

func (g diffGutter) numberAt(index int) diffGutterNumbers {
	if !g.withNumbers || index >= len(g.numbers) {
		return diffGutterNumbers{}
	}
	return g.numbers[index]
}

func resolveDiffGutter(lines []DiffLine, styles Styles, width int) diffGutter {
	if width > 0 && width < diffGutterNumbersMinWidth {
		return diffGutter{}
	}
	numbers, ok := resolveDiffGutterNumbers(lines)
	if !ok {
		return diffGutter{}
	}
	gutter := diffGutter{withNumbers: true, numbers: numbers, numberStyle: styles.Dimmed}
	maxOld, maxNew := 1, 1
	for _, n := range numbers {
		maxOld = max(maxOld, n.old)
		maxNew = max(maxNew, n.new)
	}
	gutter.oldWidth = len(strconv.Itoa(maxOld))
	gutter.newWidth = len(strconv.Itoa(maxNew))
	return gutter
}

var diffHunkHeaderPattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseDiffHunkHeader recovers a typed hunk range from unified-diff header
// text. Counts default to 1 when the short `@@ -a +c @@` form omits them.
func parseDiffHunkHeader(content string) (DiffHunk, bool) {
	groups := diffHunkHeaderPattern.FindStringSubmatch(strings.TrimSpace(content))
	if groups == nil {
		return DiffHunk{}, false
	}
	numbers := [4]int{0, 1, 0, 1}
	for i, group := range groups[1:] {
		if group == "" {
			continue
		}
		value, err := strconv.Atoi(group)
		if err != nil {
			return DiffHunk{}, false
		}
		numbers[i] = value
	}
	return DiffHunk{
		OldStart: numbers[0], OldCount: numbers[1], NewStart: numbers[2], NewCount: numbers[3],
	}, true
}

func diffHunkFromHeader(line DiffLine) (DiffHunk, bool) {
	if line.Hunk != nil {
		hunk := *line.Hunk
		if hunk.OldStart < 0 || hunk.OldCount < 0 || hunk.NewStart < 0 || hunk.NewCount < 0 {
			return DiffHunk{}, false
		}
		return hunk, true
	}
	return parseDiffHunkHeader(line.Content)
}

// resolveDiffGutterNumbers assigns old/new coordinates to every body line.
// Typed per-line coordinates win; lines without them (older persisted
// sessions) are counted forward from the enclosing hunk header. Any line
// whose coordinates cannot be derived — a missing or garbled header, or more
// counted lines than the header budgeted — fails the whole resolution so the
// renderer falls back to numberless output instead of miscounting silently.
func resolveDiffGutterNumbers(lines []DiffLine) ([]diffGutterNumbers, bool) {
	numbers := make([]diffGutterNumbers, len(lines))
	oldNext, newNext := 0, 0
	oldLeft, newLeft := 0, 0
	counters := false

	takeOld := func(typed int) (int, bool) {
		if typed > 0 {
			if counters && (oldLeft == 0 || oldNext <= 0 || typed != oldNext) {
				return 0, false
			}
			oldNext = typed + 1
			if counters && oldLeft > 0 {
				oldLeft--
			}
			return typed, true
		}
		if !counters || oldNext <= 0 || oldLeft == 0 {
			return 0, false
		}
		value := oldNext
		oldNext++
		if oldLeft > 0 {
			oldLeft--
		}
		return value, true
	}
	takeNew := func(typed int) (int, bool) {
		if typed > 0 {
			if counters && (newLeft == 0 || newNext <= 0 || typed != newNext) {
				return 0, false
			}
			newNext = typed + 1
			if counters && newLeft > 0 {
				newLeft--
			}
			return typed, true
		}
		if !counters || newNext <= 0 || newLeft == 0 {
			return 0, false
		}
		value := newNext
		newNext++
		if newLeft > 0 {
			newLeft--
		}
		return value, true
	}

	for index, line := range lines {
		if line.OldLine < 0 || line.NewLine < 0 {
			return nil, false
		}
		switch line.Kind {
		case DiffHunkHeader:
			hunk, ok := diffHunkFromHeader(line)
			if !ok {
				return nil, false
			}
			oldNext, newNext = hunk.OldStart, hunk.NewStart
			oldLeft, newLeft = hunk.OldCount, hunk.NewCount
			counters = true
		case DiffEllipsis, DiffOmitted:
			// Skipped or truncated regions consume an unknown number of lines;
			// counting may only resume from a later header or typed coordinates.
			counters = false
		case DiffContext:
			oldNumber, okOld := takeOld(line.OldLine)
			newNumber, okNew := takeNew(line.NewLine)
			if !okOld || !okNew {
				return nil, false
			}
			numbers[index] = diffGutterNumbers{old: oldNumber, new: newNumber}
		case DiffAdded:
			if line.OldLine != 0 {
				return nil, false
			}
			newNumber, ok := takeNew(line.NewLine)
			if !ok {
				return nil, false
			}
			numbers[index] = diffGutterNumbers{new: newNumber}
		case DiffRemoved:
			if line.NewLine != 0 {
				return nil, false
			}
			oldNumber, ok := takeOld(line.OldLine)
			if !ok {
				return nil, false
			}
			numbers[index] = diffGutterNumbers{old: oldNumber}
		}
	}
	return numbers, true
}

func diffTotals(lines []DiffLine) (added, removed int, known bool) {
	known = true
	for _, line := range lines {
		switch line.Kind {
		case DiffAdded:
			added++
		case DiffRemoved:
			removed++
		case DiffOmitted:
			known = false
		}
	}
	return added, removed, known
}

// lipglossWidth is kept behind a tiny wrapper so all width arithmetic in this
// file is visibly terminal-cell based rather than byte or rune based.
func lipglossWidth(value string) int {
	return lipgloss.Width(value)
}

// lcsLines computes the longest common subsequence of two string slices.
func lcsLines(a, b []string) []string {
	return lcsComparable(a, b)
}

func lcsComparable[T comparable](a, b []T) []T {
	m, n := len(a), len(b)
	if m == 0 || n == 0 {
		return nil
	}

	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	result := make([]T, dp[m][n])
	k := dp[m][n] - 1
	i, j := m, n
	for i > 0 && j > 0 {
		if a[i-1] == b[j-1] {
			result[k] = a[i-1]
			k--
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			i--
		} else {
			j--
		}
	}

	return result
}

// filterContext keeps only diff lines near changes, with contextLines of context.
func filterContext(lines []DiffLine, contextLines int) []DiffLine {
	if len(lines) == 0 {
		return nil
	}

	keep := make([]bool, len(lines))
	for i, line := range lines {
		if line.Kind != DiffContext {
			lo := i - contextLines
			if lo < 0 {
				lo = 0
			}
			hi := i + contextLines
			if hi >= len(lines) {
				hi = len(lines) - 1
			}
			for j := lo; j <= hi; j++ {
				keep[j] = true
			}
		}
	}

	var result []DiffLine
	for i, line := range lines {
		if keep[i] {
			result = append(result, line)
		}
	}

	return result
}

// splitLines splits text into lines, removing a trailing empty line from a trailing newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func splitDiffTextLines(s string) []diffTextLine {
	lines := splitLines(s)
	if len(lines) == 0 {
		return nil
	}
	terminatedFinalLine := strings.HasSuffix(s, "\n")
	result := make([]diffTextLine, len(lines))
	for index, line := range lines {
		result[index] = diffTextLine{
			Content:    line,
			Terminated: index < len(lines)-1 || terminatedFinalLine,
		}
	}
	return result
}

// handleDiffBuildResult applies an asynchronously built diff receipt to its
// matching pending tool entry.
func (m *Model) handleDiffBuildResult(msg diffBuildResultMsg) {
	matched := false
	for i := len(m.toolEntries) - 1; i >= 0; i-- {
		entry := &m.toolEntries[i]
		if !toolCallMatches(msg.ToolID, msg.ToolName, entry.ID, entry.Name) ||
			!entry.DiffPending || entry.DiffGeneration != msg.Generation {
			continue
		}
		entry.DiffPending = false
		entry.DiffGeneration = 0
		if msg.Available {
			// The live card and persisted session share one explicit bound. This
			// keeps every retained row inspectable while oversized patches end in
			// a typed omission marker instead of a renderer-only dead end.
			entry.DiffLines = persistDiffLines(msg.Lines)
		}
		matched = true
		break
	}
	if !matched {
		return
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
}
