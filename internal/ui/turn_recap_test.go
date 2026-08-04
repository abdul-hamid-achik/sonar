package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// longMultiSectionAnswer is long enough to justify a one-line digest and has
// later sections so the recap is not a near-duplicate of the full on-screen
// reply.
const longMultiSectionAnswer = "" +
	"I'll inspect the boundary, then run the focused checks across the host " +
	"policy surface and the tool admission path before proposing a fix. " +
	"More detail follows after the first pass through the repository layout.\n\n" +
	"The root cause was a stale cache key that survived concurrent reloads, " +
	"so later turns could observe a half-written entry and re-render it. " +
	"We should also verify the transcript paint cache invalidation path."

func TestBuildTurnRecapFirstSentence(t *testing.T) {
	got := buildTurnRecap(longMultiSectionAnswer)
	if !strings.Contains(got, "inspect the boundary") {
		t.Fatalf("recap lost the lead sentence: %q", got)
	}
	if strings.Contains(got, "root cause") {
		t.Fatalf("recap leaked past first section: %q", got)
	}
}

func TestBuildTurnRecapSkipsFences(t *testing.T) {
	content := "```go\nfunc main() {}\n```\n\n" + longMultiSectionAnswer
	got := buildTurnRecap(content)
	if !strings.Contains(got, "inspect the boundary") && !strings.Contains(got, "I'll inspect") {
		t.Fatalf("recap should prefer prose after fences: %q", got)
	}
	if strings.Contains(got, "func main") {
		t.Fatalf("recap included fenced code: %q", got)
	}
}

func TestBuildTurnRecapSuppressesNonCompressingDigest(t *testing.T) {
	// A digest identical to the whole answer restates a short reply verbatim
	// right underneath it — noise, not scannability. Suppress it.
	for _, content := range []string{
		"Buenos días — what are we working on today?",
		"Done.",
		"Ready when you are",
	} {
		if got := buildTurnRecap(content); got != "" {
			t.Fatalf("recap %q duplicates the entire short answer %q", got, content)
		}
	}
	// Multi-section long answers still produce a digest.
	if got := buildTurnRecap(longMultiSectionAnswer); got == "" {
		t.Fatal("multi-section answer lost its recap")
	}
}

func TestFormatTurnRecapLineASCII(t *testing.T) {
	// Short phrases are suppressed — they only echo the answer.
	if line := formatTurnRecapLine("Ready when you are.", 40, true, defaultThemeID, GlyphASCII); line != "" {
		t.Fatalf("short recap should be suppressed, got %q", ansi.Strip(line))
	}
	long := "Patched the loader race across concurrent reloads of the cache key and host policy path."
	line := formatTurnRecapLine(long, 80, true, defaultThemeID, GlyphASCII)
	plain := ansi.Strip(line)
	if !strings.HasPrefix(plain, "* ") {
		t.Fatalf("ASCII recap marker missing: %q", plain)
	}
	if !strings.Contains(plain, "Patched the loader race") {
		t.Fatalf("recap body missing: %q", plain)
	}
}

func TestBuildTurnRecapRejectsSpanishGreetingOnlyCut(t *testing.T) {
	// Short single-paragraph Spanish reply must not restate under itself.
	content := "¡Claro! Voy a echar un vistazo al repositorio para entender su estructura y qué hace este proyecto."
	if got := buildTurnRecap(content); got != "" {
		t.Fatalf("short Spanish reply should not get a recap echo, got %q", got)
	}
}

func TestBuildTurnRecapRejectsNearDuplicateFirstLine(t *testing.T) {
	// The live bug: answer + recap that is just a truncated copy of the same line.
	content := "¡Claro! Voy a explorar el código para entender cómo funciona el sistema de temas y qué opciones tenemos para crear nuevos temas personalizados."
	if got := buildTurnRecap(content); got != "" {
		t.Fatalf("near-duplicate first-line recap should be suppressed, got %q", got)
	}
}

func TestRenderAssistantIncludesRecap(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.height = 24
	m.ready = true
	var b strings.Builder
	m.renderAssistantMsg(&b, ChatEntry{
		Kind:    "assistant",
		Content: longMultiSectionAnswer,
	}, m.chatContentWidth())
	plain := ansi.Strip(b.String())
	if !strings.Contains(plain, "inspect the boundary") {
		t.Fatalf("answer body missing:\n%s", plain)
	}
	// Compact digest marker (unicode diamond or historical "recap:").
	if !strings.Contains(plain, "✳ ") && !strings.Contains(plain, "recap:") && !strings.Contains(plain, "* ") {
		// Multi-section long answers should get a digest line.
		t.Fatalf("assistant render missing recap marker:\n%s", plain)
	}

	// Short single-sentence replies stay recap-free — the digest would only
	// repeat the answer verbatim one line below it.
	b.Reset()
	m.renderAssistantMsg(&b, ChatEntry{
		Kind:    "assistant",
		Content: "Buenos días — what are we working on today?",
	}, m.chatContentWidth())
	short := ansi.Strip(b.String())
	// Only one occurrence of the answer body (no digest echo).
	if strings.Count(short, "Buenos días") != 1 {
		t.Fatalf("short answer rendered a duplicating recap:\n%s", short)
	}
}
