package ui

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
)

const (
	maxToolResultPreviewLines = 80
	maxToolResultRenderCache  = 128

	readPreviewHeadRows = 5
	readPreviewTailRows = 3
	execPreviewHeadRows = 2
	execPreviewTailRows = 3
	searchPreviewRows   = 12
	editPreviewRows     = 12
	genericPreviewRows  = 12
)

type toolResultRenderKind uint8

const (
	toolResultPlain toolResultRenderKind = iota
	toolResultCode
	toolResultSearch
)

type toolResultRenderCacheKey struct {
	Digest   [sha256.Size]byte
	Kind     toolResultRenderKind
	Language string
	Width    int
	Dark     bool
	NoColor  bool
	Mode     ToolPreviewMode
	// GlyphProfile affects semantic chrome such as omission markers and read
	// gutters. Keep ASCII and Unicode projections in separate cache entries.
	GlyphProfile GlyphProfile
	// Display marks the ANSI-16 remapped variant so a cached plain render can
	// never be served for the colored body or vice versa.
	Display bool
	// Output metadata changes the honest omission receipt even when the bounded
	// preview bytes are identical (for example, after session restore/eviction).
	OutputDigest    OutputDetailDigest
	OutputAvailable bool
}

// Tool result highlighting is an ephemeral presentation cache. Keys contain a
// digest rather than result text, values are derived only after terminal
// sanitization, and the hard entry ceiling prevents a long session from
// retaining an unbounded amount of output.
var semanticToolResultCache = struct {
	sync.RWMutex
	entries map[toolResultRenderCacheKey][]string
}{entries: make(map[toolResultRenderCacheKey][]string)}

var searchResultLocation = regexp.MustCompile(`^(.+?):([0-9]+)(?::([0-9]+))?:(.*)$`)
var genericResultField = regexp.MustCompile(`^([[:alnum:]_. -]{1,40}):[ \t]+(.+)$`)

var trustedResultLanguagesByExtension = map[string]string{
	".bash": "bash", ".sh": "bash", ".zsh": "bash",
	".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp",
	".css": "css", ".scss": "scss",
	".go": "go", ".graphql": "graphql", ".gql": "graphql",
	".html": "html", ".htm": "html",
	".java": "java", ".js": "javascript", ".jsx": "jsx",
	".json": "json", ".jsonl": "json",
	".kt": "kotlin", ".kts": "kotlin",
	".lua": "lua", ".md": "markdown", ".mdx": "markdown",
	".php": "php", ".py": "python", ".rb": "ruby", ".rs": "rust",
	".sql": "sql", ".swift": "swift",
	".toml": "toml", ".ts": "typescript", ".tsx": "tsx",
	".xml": "xml", ".yaml": "yaml", ".yml": "yaml",
}

var trustedResultLanguages = func() map[string]struct{} {
	result := make(map[string]struct{}, len(trustedResultLanguagesByExtension))
	for _, language := range trustedResultLanguagesByExtension {
		result[language] = struct{}{}
	}
	return result
}()

// resultRenderKind classifies the result body exactly as the semantic render
// path does; only the plain kind may use the ANSI-16 display variant.
func (c ToolCard) resultRenderKind() toolResultRenderKind {
	switch {
	case c.Kind == ToolCardSearch:
		return toolResultSearch
	case c.Kind == ToolCardFile && normalizeTrustedResultLanguage(c.ResultLanguage) != "":
		return toolResultCode
	default:
		return toolResultPlain
	}
}

// remappedDisplayResultLines regenerates the ANSI-16 display variant of a
// plain result body from the raw bytes retained at intake. It returns nil
// when no display variant applies so callers fall back to the sanitized
// plain rendering.
func (c ToolCard) remappedDisplayResultLines(width int) []string {
	if noColor || width <= 0 || c.resultRenderKind() != toolResultPlain {
		return nil
	}
	if len(c.resultDisplayLines) > 0 {
		plan := c.resultPreviewPlan(c.Result, width)
		if len(plan.lines) == 0 {
			return nil
		}
		palette := newSemanticPalette(c.IsDark, c.ThemeID)
		rendered := make([]string, 0, len(plan.lines)+1)
		for _, sourceIndex := range plan.sourceIndexes {
			if sourceIndex < 0 || sourceIndex >= len(c.resultDisplayLines) {
				// The ANSI byte budget retained only a prefix, so rendering a
				// selected semantic tail in color would pair it with the wrong
				// source row. Fall back to the sanitized body.
				return nil
			}
			rendered = append(rendered, renderANSI16SegmentsWithGlyphProfile(
				c.resultDisplayLines[sourceIndex], palette, plan.contentWidth, c.GlyphProfile,
			))
		}
		omission := c.outputDetailOmissionLine(
			len(plan.lines),
			previewSourceBytes(c.Result, c.OutputDigest.TotalBytes),
			max(plan.hiddenRows, c.resultDisplayHidden),
			width,
		)
		rendered = c.renderReadLineNumberGutter(rendered, plan)
		return insertToolPreviewOmission(rendered, omission, plan.omissionIndex)
	}
	display := strings.TrimRight(c.ResultDisplay, "\n")
	if display == "" {
		return nil
	}
	safeDisplay := sanitizeTerminalMultiline(display)
	planSource := c.Result
	if strings.TrimSpace(planSource) == "" {
		planSource = safeDisplay
	}
	plan := c.resultPreviewPlan(planSource, width)
	if len(plan.lines) == 0 {
		return nil
	}
	all := strings.Split(display, "\n")
	palette := newSemanticPalette(c.IsDark, c.ThemeID)
	rendered := make([]string, 0, len(plan.lines)+1)
	for _, sourceIndex := range plan.sourceIndexes {
		if sourceIndex < 0 || sourceIndex >= len(all) {
			return nil
		}
		rendered = append(rendered, remapANSI16LineWithGlyphProfile(
			all[sourceIndex], palette, plan.contentWidth, c.GlyphProfile,
		))
	}
	omission := c.outputDetailOmissionLine(
		len(plan.lines),
		previewSourceBytes(planSource, c.OutputDigest.TotalBytes),
		plan.hiddenRows,
		width,
	)
	rendered = c.renderReadLineNumberGutter(rendered, plan)
	return insertToolPreviewOmission(rendered, omission, plan.omissionIndex)
}

func (c ToolCard) renderSemanticResultLines(result string, width int) []string {
	if width <= 0 || strings.TrimSpace(result) == "" {
		return nil
	}

	language := normalizeTrustedResultLanguage(c.ResultLanguage)
	kind := c.resultRenderKind()
	displayVariant := kind == toolResultPlain && !noColor &&
		(c.ResultDisplay != "" || len(c.resultDisplayLines) > 0)
	key := toolResultRenderCacheKey{
		Digest:          sha256.Sum256([]byte(result)),
		Kind:            kind,
		Language:        language,
		Width:           width,
		Dark:            c.IsDark,
		NoColor:         noColor,
		Mode:            c.resolvedPreviewMode(),
		GlyphProfile:    resolveGlyphProfile(c.GlyphProfile),
		Display:         displayVariant,
		OutputDigest:    c.OutputDigest,
		OutputAvailable: c.OutputAvailable,
	}
	if displayVariant {
		key.Digest = toolDisplayPreviewDigest(c)
	}
	if cached, ok := cachedSemanticToolResult(key); ok {
		return cached
	}

	var rendered []string
	if displayVariant {
		rendered = c.remappedDisplayResultLines(width)
	}
	if len(rendered) == 0 {
		plan := c.resultPreviewPlan(result, width)
		plainLines := plan.lines
		rendered = make([]string, 0, len(plainLines)+1)
		switch kind {
		case toolResultCode:
			if !noColor {
				if highlighted, ok := highlightToolCode(plainLines, language, c.IsDark, c.ThemeID); ok {
					rendered = highlighted
				}
			}
		case toolResultSearch:
			rendered = c.renderSearchPreviewLines(plainLines, width, plan.omissionIndex)
		case toolResultPlain:
			if c.resolvedPreviewMode() == ToolPreviewGeneric {
				for _, line := range plainLines {
					rendered = append(rendered, c.renderGenericResultLine(line, width))
				}
			}
		}
		if len(rendered) == 0 {
			for _, line := range plainLines {
				rendered = append(rendered, c.Styles.Result.Render(line))
			}
		}
		omission := c.outputDetailOmissionLine(
			len(plainLines),
			previewSourceBytes(result, c.OutputDigest.TotalBytes),
			plan.hiddenRows,
			width,
		)
		rendered = c.renderReadLineNumberGutter(rendered, plan)
		rendered = insertToolPreviewOmission(rendered, omission, plan.omissionIndex)
	}
	cacheSemanticToolResult(key, rendered)
	return append([]string(nil), rendered...)
}

// outputDetailOmissionLine describes the complete sanitized source, not merely
// the already-bounded ToolEntry preview. When the ephemeral capability has
// expired (including after session restore), the durable digest remains honest
// without implying that the missing bytes can still be loaded.
func (c ToolCard) outputDetailOmissionLine(
	visibleRows int,
	visibleBytes uint64,
	fallbackHiddenRows int,
	width int,
) string {
	if width <= 0 {
		return ""
	}
	digest := c.OutputDigest
	if digest != (OutputDetailDigest{}) && digest.Valid() {
		shownRows := uint64(max(0, visibleRows))
		if shownRows > digest.TotalRows {
			shownRows = digest.TotalRows
		}
		hiddenRows := digest.TotalRows - shownRows
		if visibleBytes > digest.TotalBytes {
			visibleBytes = digest.TotalBytes
		}
		hiddenBytes := digest.TotalBytes - visibleBytes
		if hiddenRows == 0 && hiddenBytes == 0 {
			return ""
		}

		var count string
		if hiddenRows > 0 {
			count = fmt.Sprintf("%d %s hidden", hiddenRows, pluralWord(hiddenRows, "line", "lines"))
		} else {
			count = fmt.Sprintf("%d %s hidden", hiddenBytes, pluralWord(hiddenBytes, "byte", "bytes"))
		}
		action := "full output unavailable"
		if c.OutputAvailable {
			action = "open output"
			if digest.Truncated {
				action = "open retained output"
			}
		}
		prefix := "…"
		separator := " · "
		if c.GlyphProfile == GlyphASCII {
			prefix = "..."
			separator = " - "
		}
		return c.Styles.Dimmed.Render(
			truncateDisplayWithGlyphProfile(prefix+" "+count+separator+action, width, c.GlyphProfile),
		)
	}
	if fallbackHiddenRows <= 0 {
		return ""
	}
	prefix := "…"
	if c.GlyphProfile == GlyphASCII {
		prefix = "..."
	}
	return c.Styles.Dimmed.Render(
		truncateDisplayWithGlyphProfile(
			fmt.Sprintf("%s %d more lines", prefix, fallbackHiddenRows),
			width,
			c.GlyphProfile,
		),
	)
}

func previewSourceBytes(result string, totalBytes uint64) uint64 {
	result = sanitizeTerminalMultiline(result)
	if totalBytes > uint64(len(result)) && strings.HasSuffix(result, "...") {
		return uint64(len(result) - len("..."))
	}
	return uint64(len(result))
}

func pluralWord(count uint64, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func toolDisplayPreviewDigest(card ToolCard) [sha256.Size]byte {
	if len(card.resultDisplayLines) == 0 {
		return sha256.Sum256([]byte(card.ResultDisplay))
	}
	var b strings.Builder
	for _, line := range card.resultDisplayLines {
		for _, segment := range line {
			fmt.Fprintf(&b, "%t:%d:%d:", segment.bold, segment.fg, len(segment.text))
			b.WriteString(segment.text)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "|hidden:%d", card.resultDisplayHidden)
	return sha256.Sum256([]byte(b.String()))
}

type toolResultPreviewPlan struct {
	lines         []string
	sourceIndexes []int
	hiddenRows    int
	omissionIndex int
	lineNumCells  int
	contentWidth  int
}

type toolPreviewBudget struct {
	head int
	tail int
}

func (c ToolCard) resultPreviewPlan(result string, width int) toolResultPreviewPlan {
	safeSource := sanitizeTerminalMultiline(result)
	sourceComplete := c.previewSourceComplete(safeSource)
	result = strings.TrimRight(safeSource, "\n")
	if result == "" || width <= 0 {
		return toolResultPreviewPlan{}
	}
	all := strings.Split(result, "\n")
	budget := previewBudgetForMode(c.resolvedPreviewMode())
	limit := budget.head + budget.tail
	if limit <= 0 {
		limit = genericPreviewRows
	}

	indexes := make([]int, 0, min(len(all), limit))
	omissionIndex := 0
	if len(all) <= limit {
		for index := range all {
			indexes = append(indexes, index)
		}
		omissionIndex = len(indexes)
	} else if budget.tail > 0 && sourceComplete {
		for index := 0; index < budget.head; index++ {
			indexes = append(indexes, index)
		}
		omissionIndex = len(indexes)
		for index := len(all) - budget.tail; index < len(all); index++ {
			indexes = append(indexes, index)
		}
	} else {
		// A retained prefix is not the source tail. Showing its last few rows
		// in a "head + tail" layout would falsely imply completion.
		prefixRows := budget.head
		if budget.tail == 0 {
			prefixRows = limit
		}
		for index := 0; index < min(len(all), prefixRows); index++ {
			indexes = append(indexes, index)
		}
		omissionIndex = len(indexes)
	}

	lines := make([]string, 0, len(indexes))
	contentWidth := width
	lineNumCells := 0
	if c.resolvedPreviewMode() == ToolPreviewRead && len(indexes) > 0 {
		lineNumCells = len(strconv.Itoa(indexes[len(indexes)-1] + 1))
		const readGutterSeparatorCells = 3 // " │ " or " | "
		gutterCells := lineNumCells + readGutterSeparatorCells
		gutterFits := width > gutterCells
		for _, sourceIndex := range indexes {
			line := strings.ReplaceAll(sanitizeTerminalLine(all[sourceIndex]), "\t", "    ")
			if lipgloss.Width(line)+gutterCells > width {
				gutterFits = false
				break
			}
		}
		if !gutterFits {
			// Line numbers are navigation metadata. If adding them would
			// truncate code that otherwise fits, preserve the source row and
			// drop the gutter as an adaptive narrow-width degradation.
			lineNumCells = 0
		} else {
			contentWidth -= gutterCells
		}
	}
	for _, sourceIndex := range indexes {
		line := strings.ReplaceAll(sanitizeTerminalLine(all[sourceIndex]), "\t", "    ")
		lines = append(
			lines,
			truncateDisplayWithGlyphProfile(line, contentWidth, c.GlyphProfile),
		)
	}
	return toolResultPreviewPlan{
		lines:         lines,
		sourceIndexes: indexes,
		hiddenRows:    max(0, len(all)-len(indexes)),
		omissionIndex: omissionIndex,
		lineNumCells:  lineNumCells,
		contentWidth:  contentWidth,
	}
}

func (c ToolCard) renderStyledResultPreview(
	result string,
	width int,
	renderLine func(string) string,
) []string {
	plan := c.resultPreviewPlan(result, width)
	if len(plan.lines) == 0 {
		return nil
	}
	rendered := make([]string, 0, len(plan.lines)+1)
	for _, line := range plan.lines {
		rendered = append(rendered, renderLine(line))
	}
	omission := c.outputDetailOmissionLine(
		len(plan.lines),
		previewSourceBytes(result, c.OutputDigest.TotalBytes),
		plan.hiddenRows,
		width,
	)
	rendered = c.renderReadLineNumberGutter(rendered, plan)
	return insertToolPreviewOmission(rendered, omission, plan.omissionIndex)
}

func (c ToolCard) renderReadLineNumberGutter(
	lines []string,
	plan toolResultPreviewPlan,
) []string {
	if plan.lineNumCells <= 0 || len(lines) != len(plan.sourceIndexes) {
		return lines
	}
	vertical := glyphSet(c.GlyphProfile).Vertical
	rendered := make([]string, len(lines))
	for index, line := range lines {
		gutter := fmt.Sprintf(
			"%*d %s ",
			plan.lineNumCells,
			plan.sourceIndexes[index]+1,
			vertical,
		)
		rendered[index] = c.Styles.Dimmed.Render(gutter) + line
	}
	return rendered
}

func (c ToolCard) resolvedPreviewMode() ToolPreviewMode {
	if c.PreviewMode.Valid() {
		return c.PreviewMode
	}
	switch c.Kind {
	case ToolCardBash:
		return ToolPreviewExec
	case ToolCardSearch:
		return ToolPreviewSearch
	case ToolCardFile:
		if classifyTool(c.Name) == ToolTypeFileWrite {
			return ToolPreviewEdit
		}
		return ToolPreviewRead
	default:
		return ToolPreviewGeneric
	}
}

func previewBudgetForMode(mode ToolPreviewMode) toolPreviewBudget {
	switch mode {
	case ToolPreviewRead:
		return toolPreviewBudget{head: readPreviewHeadRows, tail: readPreviewTailRows}
	case ToolPreviewExec:
		return toolPreviewBudget{head: execPreviewHeadRows, tail: execPreviewTailRows}
	case ToolPreviewSearch:
		return toolPreviewBudget{head: searchPreviewRows}
	case ToolPreviewEdit:
		return toolPreviewBudget{head: editPreviewRows}
	default:
		return toolPreviewBudget{head: genericPreviewRows}
	}
}

func (c ToolCard) previewSourceComplete(result string) bool {
	result = sanitizeTerminalMultiline(result)
	if c.OutputDigest != (OutputDetailDigest{}) && c.OutputDigest.Valid() {
		return uint64(len(result)) >= c.OutputDigest.TotalBytes
	}
	// Legacy/restored entries can lack a digest. A preview at the byte ceiling
	// with the truncation suffix is known to be a prefix, even though the exact
	// missing row count is unavailable.
	return !strings.HasSuffix(result, "...") ||
		len(result) < maxToolCardResultBytes-len("...")-utf8.UTFMax
}

func insertToolPreviewOmission(lines []string, omission string, index int) []string {
	if omission == "" {
		return lines
	}
	index = max(0, min(index, len(lines)))
	result := make([]string, 0, len(lines)+1)
	result = append(result, lines[:index]...)
	result = append(result, omission)
	result = append(result, lines[index:]...)
	return result
}

func highlightToolCode(lines []string, language string, isDark bool, themeID string) ([]string, bool) {
	lexer := lexers.Get(language)
	if lexer == nil || lexer == lexers.Fallback {
		return nil, false
	}
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, strings.Join(lines, "\n"))
	if err != nil {
		return nil, false
	}
	var output bytes.Buffer
	if err := formatters.TTY16m.Format(&output, toolCodeStyle(isDark, themeID), iterator); err != nil {
		return nil, false
	}
	highlighted := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(highlighted) != len(lines) {
		return nil, false
	}
	return highlighted, true
}

func toolCodeStyle(isDark bool, themeID string) *chroma.Style {
	palette := newSemanticPalette(isDark, themeID)
	text := colorHex(palette.Text)
	muted := colorHex(palette.Muted)
	dim := colorHex(palette.Dim)
	accent := colorHex(palette.Accent)
	accent2 := colorHex(palette.Accent2)
	errorColor := colorHex(palette.Error)
	success := colorHex(palette.Success)
	special := colorHex(palette.Special)
	warning := colorHex(palette.Warning)

	// This is the same LightDark-derived Nord vocabulary as the rest of Local
	// Agent. There is deliberately no background token: the user's terminal
	// remains responsible for its own background and contrast mode.
	return chroma.MustNewStyle("sonar-nord", chroma.StyleEntries{
		chroma.Background:          text,
		chroma.Text:                text,
		chroma.TextWhitespace:      muted,
		chroma.Error:               errorColor,
		chroma.Keyword:             "bold " + accent2,
		chroma.KeywordPseudo:       accent2,
		chroma.KeywordType:         accent2,
		chroma.Name:                text,
		chroma.NameAttribute:       accent,
		chroma.NameBuiltin:         accent2,
		chroma.NameClass:           accent,
		chroma.NameConstant:        special,
		chroma.NameDecorator:       warning,
		chroma.NameException:       errorColor,
		chroma.NameFunction:        accent,
		chroma.NameLabel:           accent,
		chroma.NameNamespace:       accent,
		chroma.NameProperty:        accent,
		chroma.NameTag:             accent2,
		chroma.NameVariable:        text,
		chroma.LiteralString:       success,
		chroma.LiteralStringDoc:    "italic " + dim,
		chroma.LiteralStringEscape: warning,
		chroma.LiteralNumber:       special,
		chroma.Operator:            accent2,
		chroma.OperatorWord:        "bold " + accent2,
		chroma.Punctuation:         text,
		chroma.Comment:             "italic " + dim,
		chroma.CommentPreproc:      accent2,
		chroma.GenericDeleted:      errorColor,
		chroma.GenericError:        errorColor,
		chroma.GenericHeading:      "bold " + accent,
		chroma.GenericInserted:     success,
		chroma.GenericOutput:       muted,
		chroma.GenericPrompt:       "bold " + dim,
		chroma.GenericSubheading:   "bold " + accent,
		chroma.GenericTraceback:    errorColor,
	})
}

func (c ToolCard) renderSearchResultLine(line string, width int) string {
	match := searchResultLocation.FindStringSubmatch(line)
	if len(match) == 0 {
		if looksLikeSearchResultPath(line) {
			return c.Styles.SearchPath.Render(line)
		}
		return c.Styles.Result.Render(line)
	}
	path := match[1]
	location := match[2]
	if match[3] != "" {
		location += ":" + match[3]
	}
	prefixWidth := lipgloss.Width(path) + 1 + lipgloss.Width(location) + 1
	if prefixWidth >= width {
		return c.Styles.SearchPath.Render(
			truncateDisplayWithGlyphProfile(line, width, c.GlyphProfile),
		)
	}
	body := truncateDisplayWithGlyphProfile(match[4], max(1, width-prefixWidth), c.GlyphProfile)
	return c.Styles.SearchPath.Render(path) +
		c.Styles.SearchLocation.Render(":"+location+":") +
		c.Styles.SearchMatch.Render(body)
}

func (c ToolCard) renderSearchPreviewLines(
	lines []string,
	width int,
	gapIndex int,
) []string {
	rendered := make([]string, 0, len(lines))
	lastPath := ""
	for index, line := range lines {
		if index == gapIndex && gapIndex < len(lines) {
			// A head/tail omission is a real discontinuity. The first tail row
			// must restate its file instead of inheriting grouping context from
			// a source row that is no longer adjacent.
			lastPath = ""
		}
		match := searchResultLocation.FindStringSubmatch(line)
		if len(match) == 0 || match[1] != lastPath {
			rendered = append(rendered, c.renderSearchResultLine(line, width))
			if len(match) > 0 {
				lastPath = match[1]
			}
			continue
		}
		location := match[2]
		if match[3] != "" {
			location += ":" + match[3]
		}
		prefix := "  " + location + ":"
		bodyWidth := max(1, width-lipgloss.Width(prefix))
		body := truncateDisplayWithGlyphProfile(match[4], bodyWidth, c.GlyphProfile)
		rendered = append(
			rendered,
			c.Styles.SearchLocation.Render(prefix)+c.Styles.SearchMatch.Render(body),
		)
	}
	return rendered
}

func (c ToolCard) renderGenericResultLine(line string, width int) string {
	match := genericResultField.FindStringSubmatch(line)
	if len(match) == 0 {
		return c.Styles.Result.Render(line)
	}
	key := match[1] + ":"
	keyWidth := lipgloss.Width(key)
	if keyWidth+1 >= width {
		return c.Styles.Result.Render(
			truncateDisplayWithGlyphProfile(line, width, c.GlyphProfile),
		)
	}
	value := truncateDisplayWithGlyphProfile(match[2], width-keyWidth-1, c.GlyphProfile)
	return c.Styles.Dimmed.Render(key) + " " + c.Styles.Result.Render(value)
}

func looksLikeSearchResultPath(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, " \t") {
		return false
	}
	return trustedResultLanguageFromPath(line) != ""
}

func cachedSemanticToolResult(key toolResultRenderCacheKey) ([]string, bool) {
	semanticToolResultCache.RLock()
	value, ok := semanticToolResultCache.entries[key]
	semanticToolResultCache.RUnlock()
	if !ok {
		return nil, false
	}
	return append([]string(nil), value...), true
}

func cacheSemanticToolResult(key toolResultRenderCacheKey, value []string) {
	semanticToolResultCache.Lock()
	if len(semanticToolResultCache.entries) >= maxToolResultRenderCache {
		clear(semanticToolResultCache.entries)
	}
	semanticToolResultCache.entries[key] = append([]string(nil), value...)
	semanticToolResultCache.Unlock()
}

func trustedResultLanguageForTool(name string, args map[string]any) string {
	if classifyTool(name) != ToolTypeFileRead {
		return ""
	}
	for _, key := range []string{"path", "file_path", "filename", "file"} {
		if path, ok := args[key].(string); ok {
			return trustedResultLanguageFromPath(path)
		}
	}
	return ""
}

func trustedResultLanguageFromPath(path string) string {
	extension := strings.ToLower(filepath.Ext(strings.TrimSpace(path)))
	return trustedResultLanguagesByExtension[extension]
}

func normalizeTrustedResultLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if _, ok := trustedResultLanguages[language]; !ok {
		return ""
	}
	return language
}
