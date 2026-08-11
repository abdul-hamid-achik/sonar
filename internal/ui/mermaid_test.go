package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const mermaidTestTheme = "nord"

func mermaidTestDoc(body string) string {
	return "Before the diagram.\n\n```mermaid\n" + body + "\n```\n\nAfter the diagram.\n"
}

func TestMermaidFenceRendersAsDiagram(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	got := mr.preprocessMermaid(mermaidTestDoc("graph LR\nA --> B"))

	if strings.Contains(got, "```mermaid") {
		t.Fatalf("mermaid fence survived preprocessing:\n%s", got)
	}
	if !strings.Contains(got, "─") || !strings.Contains(got, "►") {
		t.Fatalf("expected box-drawing diagram art, got:\n%s", got)
	}
	if !strings.Contains(got, "Before the diagram.") || !strings.Contains(got, "After the diagram.") {
		t.Fatalf("surrounding prose lost:\n%s", got)
	}
}

func TestMermaidSequenceDiagramRenders(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	got := mr.preprocessMermaid(mermaidTestDoc("sequenceDiagram\nAlice->>Bob: Hello\nBob-->>Alice: Hi"))

	if strings.Contains(got, "```mermaid") {
		t.Fatalf("mermaid fence survived preprocessing:\n%s", got)
	}
	if !strings.Contains(got, "Alice") || !strings.Contains(got, "│") {
		t.Fatalf("expected sequence lifelines, got:\n%s", got)
	}
}

func TestMermaidTooWideFallsBackToOriginalBlock(t *testing.T) {
	mr := NewMarkdownRenderer(20, true, mermaidTestTheme)
	// Three LR nodes draw ~25 columns, wider than the 20-column pane on any
	// rendering path.
	doc := mermaidTestDoc("graph LR\nA --> B --> C")
	if got := mr.preprocessMermaid(doc); got != doc {
		t.Fatalf("diagram wider than the pane must keep the original block, got:\n%s", got)
	}
}

func TestMermaidOversizedSourceFallsBackWithoutLayout(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	var b strings.Builder
	b.WriteString("graph LR\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "N%d --> N%d & N%d & N%d\n", i, i+1, i+2, i+3)
	}
	doc := mermaidTestDoc(b.String())
	if got := mr.preprocessMermaid(doc); got != doc {
		t.Fatalf("dense source must fall back before paying layout, got a rewrite")
	}
}

func TestMermaidListIndentationIsPreserved(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	doc := "1. First step\n2. Second step:\n\n   ```mermaid\n   graph LR\n   A --> B\n   ```\n"
	got := mr.preprocessMermaid(doc)
	if strings.Contains(got, "```mermaid") {
		t.Fatalf("indented mermaid fence survived preprocessing:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "~~~") && !strings.HasPrefix(line, "   ~~~") {
			t.Fatalf("replacement fence lost the list indentation: %q", line)
		}
		if strings.Contains(line, "►") && !strings.HasPrefix(line, "   ") {
			t.Fatalf("diagram art lost the list indentation: %q", line)
		}
	}
}

func TestMermaidAnnotatedInfoStringStillRenders(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	doc := "```mermaid title=Pipeline\ngraph LR\nA --> B\n```\n"
	got := mr.preprocessMermaid(doc)
	if !strings.Contains(got, "►") {
		t.Fatalf("annotated mermaid fence must still render as a diagram:\n%s", got)
	}
}

func TestMermaidCloseGrammarMatchesScanner(t *testing.T) {
	// The scanner accepts only space/tab/CR after a closing fence; Unicode
	// whitespace must not close a fence here either, or the two parsers
	// disagree about where a fence ends.
	for line, want := range map[string]bool{
		"```":       true,
		"```  ":     true,
		"```\t\r":   true,
		"```\v":     false,
		"```\u00a0": false,
		"``` info":  false,
	} {
		if got := mermaidFenceCloses(line, '`', 3); got != want {
			t.Fatalf("mermaidFenceCloses(%q) = %t, scanner says %t", line, got, want)
		}
	}
}

func TestMermaidArtSurvivesRendererRebuild(t *testing.T) {
	previous := NewMarkdownRenderer(100, true, mermaidTestTheme)
	previous.preprocessMermaid(mermaidTestDoc("graph LR\nA --> B"))
	if len(previous.mermaidCache) == 0 {
		t.Fatal("expected the render to populate the art cache")
	}

	rebuilt := NewMarkdownRenderer(90, true, mermaidTestTheme)
	rebuilt.inheritMermaidArt(previous)
	if len(rebuilt.mermaidCache) != len(previous.mermaidCache) {
		t.Fatal("rebuilt renderer must inherit the art cache")
	}
	if rebuilt.mermaidBudgetSet {
		t.Fatal("the paint budget is width-dependent and must not be inherited")
	}
}

func TestMermaidPaintBudgetIsMeasuredSane(t *testing.T) {
	const width = 80
	mr := NewMarkdownRenderer(width, true, mermaidTestTheme)
	budget := mr.mermaidPaintBudget()
	if budget < width-8 || budget > width {
		t.Fatalf("measured budget %d outside sane range [%d, %d] — either Glamour reflows code lines or the measurement broke", budget, width-8, width)
	}
}

func TestUnclosedMermaidFenceIsLeftAlone(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	doc := "Streaming...\n\n```mermaid\ngraph LR\nA --> B"
	if got := mr.preprocessMermaid(doc); got != doc {
		t.Fatalf("unclosed fence must stay untouched, got:\n%s", got)
	}
}

func TestMermaidQuotedInsideLongerFenceIsNotRendered(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	doc := "````markdown\n```mermaid\ngraph LR\nA --> B\n```\n````\n"
	if got := mr.preprocessMermaid(doc); got != doc {
		t.Fatalf("a quoted mermaid example must stay verbatim, got:\n%s", got)
	}
}

func TestNonMermaidFencesAreUntouched(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	doc := "```go\nfunc main() { /* mermaid */ }\n```\n"
	if got := mr.preprocessMermaid(doc); got != doc {
		t.Fatalf("non-mermaid fence changed:\n%s", got)
	}
}

func TestMultipleMermaidBlocksAllRender(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	doc := mermaidTestDoc("graph LR\nA --> B") + "\n" + mermaidTestDoc("graph TD\nC --> D")
	got := mr.preprocessMermaid(doc)
	if strings.Contains(got, "```mermaid") {
		t.Fatalf("a mermaid fence survived preprocessing:\n%s", got)
	}
}

func TestMermaidReplacementCarriesNoAnsi(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	source := "graph LR\nclassDef hot color:#ff0000\nA:::hot --> B"
	got := mr.preprocessMermaid(mermaidTestDoc(source))
	if strings.Contains(got, "\x1b") {
		t.Fatalf("diagram art leaked ANSI escapes into markdown source:\n%q", got)
	}
}

// The property the budget exists for: whatever Glamour adds around the art,
// a rendered document with a diagram must never paint wider than the pane.
func TestRenderFullMermaidFitsPane(t *testing.T) {
	const width = 80
	mr := NewMarkdownRenderer(width, true, mermaidTestTheme)
	rendered := mr.RenderFull(mermaidTestDoc("graph LR\nA --> B --> C"))
	if !strings.Contains(ansi.Strip(rendered), "─") {
		t.Fatalf("expected the rendered document to contain diagram art:\n%s", rendered)
	}
	for _, line := range strings.Split(rendered, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("rendered line paints %d cells in an %d-wide pane: %q", w, width, line)
		}
	}
}

func TestBrokenMermaidFallsBackToOriginalBlock(t *testing.T) {
	mr := NewMarkdownRenderer(100, true, mermaidTestTheme)
	doc := mermaidTestDoc("sequenceDiagram\nAlice->>")
	if got := mr.preprocessMermaid(doc); got != doc {
		t.Fatalf("unparseable mermaid must keep the original block, got:\n%s", got)
	}
}
