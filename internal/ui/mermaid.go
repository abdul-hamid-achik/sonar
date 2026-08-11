package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/x/ansi"
	mermaiddiagram "github.com/pgavlin/mermaid-ascii/pkg/diagram"
	mermaidrender "github.com/pgavlin/mermaid-ascii/pkg/render"
	"github.com/sirupsen/logrus"
)

// Mermaid fences render as ASCII/Unicode diagrams before Glamour sees the
// document. Glamour has no per-language code block hook — its goldmark
// pipeline is internal — so the block is replaced in the markdown source,
// the same approach glow planned for the same reason. The replacement is a
// plain fence, which keeps the work-width classification and the code-block
// framing that every other structural block already gets.
//
// The replacement only happens when it cannot make the document worse: a
// parse error, a panic, source too dense to lay out inside a frame, or art
// wider than the pane all leave the original block exactly as written,
// which renders as plain code the way it does today.

// Layout cost is paid on the Update goroutine, so it is bounded before it
// is spent. Graph layout is superlinear in edges: 25 nodes with 5
// successors each measured 17.5 ms, 100/6 measured 216 ms, 150/8 measured
// 732 ms — against a 16 ms frame. The caps are a heuristic on the source,
// not a promise about layout time: lines and bytes bound the input, and
// '>' plus '&' counts approximate edges (every arrow form carries '>', and
// '&' is the fan-out syntax that multiplies edges per line). A diagram this
// large could not fit a terminal pane anyway, so the fallback costs the
// reader nothing.
const (
	mermaidMaxSourceLines = 64
	mermaidMaxSourceBytes = 4096
	mermaidMaxEdgeMarks   = 128
)

func mermaidSourceWithinBudget(source string) bool {
	return len(source) <= mermaidMaxSourceBytes &&
		strings.Count(source, "\n") < mermaidMaxSourceLines &&
		strings.Count(source, ">")+strings.Count(source, "&") <= mermaidMaxEdgeMarks
}

// mermaidCacheLimit bounds the art cache. AUTO runs are bounded at 512
// segments over 24 hours, and a model iteratively revising diagrams mints a
// new cache key per revision; the sibling stream cache holds one entry by
// design, so this one does not get to grow forever. Eviction is an
// arbitrary entry: the cache exists to survive resize storms, not to be an
// LRU, and any entry evicted in error costs one re-render.
const mermaidCacheLimit = 256

// mermaidResult memoizes one diagram render. ok=false records a failed
// parse so a streaming prefix does not re-parse the same broken source on
// every repaint.
type mermaidResult struct {
	art string
	ok  bool
}

// silenceMermaidLogging runs before the first render. The library's graph
// package logs at Warn level to stderr when edge pathing fails; sonar does
// not use logrus anywhere, so the global logger has exactly one writer and
// discarding it cannot swallow anyone else's output. Writing to stderr
// mid-frame corrupts the Bubble Tea render, which is worse than losing a
// diagnostic about a diagram that still rendered.
var silenceMermaidLogging = sync.OnceFunc(func() {
	logrus.SetOutput(io.Discard)
})

// renderMermaidArt renders mermaid source to plain Unicode art. The library
// is third-party layout code driven by model-authored input, so a panic is
// contained here rather than taking down the frame loop. Output is stripped
// of ANSI sequences: `classDef` colors would otherwise carry model-chosen
// escapes into the transcript, bypassing the scheme entirely — the art
// answers the code block's own colors instead.
func renderMermaidArt(source string) (art string, err error) {
	if !mermaidSourceWithinBudget(source) {
		return "", fmt.Errorf("mermaid source over layout budget")
	}
	silenceMermaidLogging()
	defer func() {
		if r := recover(); r != nil {
			art, err = "", fmt.Errorf("mermaid render panic: %v", r)
		}
	}()

	out, err := mermaidrender.Render(source, mermaiddiagram.DefaultConfig())
	if err != nil {
		return "", err
	}
	out = ansi.Strip(out)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n"), nil
}

// mermaidArt is the memoized render. The cache is inherited across the
// renderer rebuilds a resize or theme switch performs — the art is
// width-independent and monochrome, so only the fits-or-falls-back
// decision needs the current width, and that is re-taken per paint.
func (mr *MarkdownRenderer) mermaidArt(source string) (string, bool) {
	if cached, hit := mr.mermaidCache[source]; hit {
		return cached.art, cached.ok
	}
	art, err := renderMermaidArt(source)
	result := mermaidResult{art: art, ok: err == nil}
	if mr.mermaidCache == nil {
		mr.mermaidCache = make(map[string]mermaidResult)
	}
	if len(mr.mermaidCache) >= mermaidCacheLimit {
		for key := range mr.mermaidCache {
			delete(mr.mermaidCache, key)
			break
		}
	}
	mr.mermaidCache[source] = result
	return result.art, result.ok
}

// inheritMermaidArt carries rendered art across a renderer rebuild. Both
// resize paths replace the renderer wholesale, and without this every
// diagram in the transcript re-paid full layout on every width-changing
// WindowSizeMsg — a drag gesture producing dozens of events multiplied a
// cost measured in tens to hundreds of milliseconds per diagram.
func (mr *MarkdownRenderer) inheritMermaidArt(previous *MarkdownRenderer) {
	if previous != nil && previous.mermaidCache != nil {
		mr.mermaidCache = previous.mermaidCache
	}
}

// mermaidPaintBudget is how wide a code-block line may be before the
// rendered document paints past the pane. It is measured through the actual
// work renderer rather than derived: a hand-maintained copy of Glamour's
// wrap arithmetic was wrong by a different amount on the color and NO_COLOR
// paths — the two diverge on document margin — and silently rejected
// diagrams that fit. Measured once per renderer, on the first diagram.
func (mr *MarkdownRenderer) mermaidPaintBudget() int {
	if !mr.mermaidBudgetSet {
		mr.mermaidBudgetSet = true
		mr.mermaidBudgetCols = measureCodeBlockBudget(mr.renderer, mr.width)
	}
	return mr.mermaidBudgetCols
}

// measureCodeBlockBudget binary-searches the widest code-block line the
// renderer paints within width columns. Rendering is monotone in line width
// — Glamour does not wrap code lines, it overflows them — which the width+8
// probe double-checks: if even that fits, something reflows and no
// measurement is trustworthy, so nothing is.
func measureCodeBlockBudget(renderer *glamour.TermRenderer, width int) int {
	if renderer == nil || width <= 0 {
		return 0
	}
	fits := func(cols int) bool {
		rendered, err := renderer.Render("~~~\n" + strings.Repeat("M", cols) + "\n~~~\n")
		if err != nil {
			return false
		}
		for _, line := range strings.Split(rendered, "\n") {
			if ansi.StringWidth(line) > width {
				return false
			}
		}
		return true
	}
	low, high := 0, width+8
	if fits(high) {
		return 0
	}
	for high-low > 1 {
		mid := (low + high) / 2
		if fits(mid) {
			low = mid
		} else {
			high = mid
		}
	}
	return low
}

// mermaidFenceOpens parses a fence-opening line through the same marker
// grammar as the streaming boundary scanner (markdownFenceMarker), so the
// two cannot disagree about what opens a fence. The info string is
// lowercased for the language match; CommonMark's language word is the
// first whitespace-separated token, so annotated fences like
// "```mermaid title=Pipeline" still declare mermaid.
func mermaidFenceOpens(line string) (char byte, length int, language string, ok bool) {
	char, length, rest, ok := markdownFenceMarker(line)
	if !ok || (char == '`' && strings.ContainsRune(rest, '`')) {
		return 0, 0, "", false
	}
	fields := strings.Fields(rest)
	if len(fields) > 0 {
		language = strings.ToLower(fields[0])
	}
	return char, length, language, true
}

// mermaidFenceCloses applies the scanner's own closing rule: same marker
// grammar, at least the opening run length, and markdownFenceClosingSuffix
// for the trailer — not TrimSpace, which accepts Unicode whitespace the
// scanner (and CommonMark) reject and once made the two parsers disagree
// about whether a fence was closed.
func mermaidFenceCloses(line string, char byte, length int) bool {
	closeChar, closeLength, rest, ok := markdownFenceMarker(line)
	return ok && closeChar == char && closeLength >= length && markdownFenceClosingSuffix(rest)
}

// mermaidReplacementFence wraps art in a tilde fence long enough that no
// run of tildes in a label can close it early. Tildes rather than backticks
// because tilde fences may contain backticks unescaped, and diagram labels
// come from the model. Every line carries the original fence's indentation
// so a diagram nested in a list item stays nested — the fallback path keeps
// the indent by reusing the original lines, and success must not render
// worse structure than failure.
func mermaidReplacementFence(art, indent string) []string {
	run, longest := 0, 0
	for _, r := range art {
		if r == '~' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	fence := indent + strings.Repeat("~", max(3, longest+1))
	artLines := strings.Split(art, "\n")
	out := make([]string, 0, len(artLines)+2)
	out = append(out, fence)
	for _, line := range artLines {
		out = append(out, indent+line)
	}
	return append(out, fence)
}

// preprocessMermaid replaces every closed mermaid fence whose rendered art
// fits the pane. Unclosed fences are left alone — during streaming the
// stable-prefix boundary never includes an open fence, so a diagram appears
// exactly once, when its fence closes. Fences inside a longer enclosing
// fence (a markdown example quoting a mermaid block) are skipped by
// honoring the closing rules the boundary scanner applies.
func (mr *MarkdownRenderer) preprocessMermaid(content string) string {
	// This runs on every streaming stable-prefix advance, so the gate must
	// not allocate; the scanner lowercases fence languages, so the gate
	// folds case the same way.
	if !asciiContainsFold(content, "mermaid") {
		return content
	}

	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))

	var openChar byte
	var openLength int
	var openIndent string
	inOther := false
	inMermaid := false
	// block holds the opening fence line plus every content line verbatim,
	// so the fallback and unclosed-at-EOF paths are a plain append.
	var block []string

	for _, line := range lines {
		switch {
		case inOther:
			out = append(out, line)
			if mermaidFenceCloses(line, openChar, openLength) {
				inOther = false
			}
		case inMermaid:
			if !mermaidFenceCloses(line, openChar, openLength) {
				block = append(block, line)
				continue
			}
			inMermaid = false
			source := strings.Join(block[1:], "\n")
			art, ok := mr.mermaidArt(source)
			if ok && mermaidArtFits(art, mr.mermaidPaintBudget()-len(openIndent)) {
				out = append(out, mermaidReplacementFence(art, openIndent)...)
			} else {
				out = append(out, block...)
				out = append(out, line)
			}
		default:
			char, length, language, ok := mermaidFenceOpens(line)
			if !ok {
				out = append(out, line)
				continue
			}
			openChar, openLength = char, length
			if language == "mermaid" {
				inMermaid = true
				indent, _ := markdownFenceMarkerStart(line)
				openIndent = line[:indent]
				block = []string{line}
				continue
			}
			inOther = true
			out = append(out, line)
		}
	}
	if inMermaid {
		out = append(out, block...)
	}
	return strings.Join(out, "\n")
}

func mermaidArtFits(art string, budget int) bool {
	if budget <= 0 {
		return false
	}
	for _, line := range strings.Split(art, "\n") {
		if ansi.StringWidth(line) > budget {
			return false
		}
	}
	return true
}

// asciiContainsFold reports whether s contains needle, folding ASCII case,
// without allocating. The needle must be lowercase ASCII letters; the
// |0x20 fold maps other bytes onto letters too, but a false hit only costs
// the scan the gate exists to skip.
func asciiContainsFold(s, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	first := needle[0]
outer:
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i]|0x20 != first {
			continue
		}
		for j := 1; j < len(needle); j++ {
			if s[i+j]|0x20 != needle[j] {
				continue outer
			}
		}
		return true
	}
	return false
}
