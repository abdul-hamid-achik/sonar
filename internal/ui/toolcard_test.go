package ui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestToolCardViewWithActivityIsPureAndShowsSummary(t *testing.T) {
	card := NewToolCard("write_file", ToolCardFile, true, defaultThemeID)
	card.SetSummary("write src/部署🙂/main.go")
	before := card

	first := card.ViewWithActivity(56, "◐", 1500*time.Millisecond)
	second := card.ViewWithActivity(56, "◐", 1500*time.Millisecond)

	if first != second {
		t.Fatalf("pure rendering changed between calls:\nfirst: %q\nsecond: %q", first, second)
	}
	if !reflect.DeepEqual(card, before) {
		t.Fatalf("rendering mutated the card:\nbefore: %#v\nafter:  %#v", before, card)
	}
	for _, want := range []string{"◐", "Writing", "write src/", "1.5s"} {
		if !strings.Contains(first, want) {
			t.Fatalf("running card missing %q:\n%s", want, first)
		}
	}
	if card.Name != "write_file" {
		t.Fatalf("presentation changed raw correlation name: %q", card.Name)
	}
	assertToolCardLinesFit(t, first, 56)
}

func TestToolCardSummaryIsBoundedAndUnicodeSafe(t *testing.T) {
	card := NewToolCard("read_file", ToolCardFile, false, defaultThemeID)
	card.SetSummary(strings.Repeat("部署🙂\n", 300))

	if !utf8.ValidString(card.Summary) {
		t.Fatalf("summary is invalid UTF-8: %q", card.Summary)
	}
	if strings.Contains(card.Summary, "\n") {
		t.Fatalf("summary was not normalized to one line: %q", card.Summary)
	}
	if got := lipgloss.Width(card.Summary); got > maxToolCardSummaryWidth {
		t.Fatalf("summary width = %d, want <= %d", got, maxToolCardSummaryWidth)
	}

	card.State = ToolCardSuccess
	card.Duration = 42 * time.Millisecond
	view := card.View(32)
	if !strings.Contains(view, "部署") {
		t.Fatalf("collapsed card omitted its semantic summary:\n%s", view)
	}
	assertToolCardLinesFit(t, view, 32)
}

func TestToolCardStripsTerminalControlsFromUntrustedFields(t *testing.T) {
	card := NewToolCard("remote__read", ToolCardGeneric, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.SetSummary("summary\x1b]8;;https://example.invalid\x07link\x1b]8;;\x07\u202espoof")
	card.Args = "{\"path\":\"safe\x1b]0;owned\x07\"}"
	card.Result = "line\x1b]52;c;payload\x07\nnext\u202espoof"
	if strings.Contains(card.Summary, "\x1b]") || strings.Contains(card.Summary, "\x07") || strings.Contains(card.Summary, "\u202e") ||
		!strings.Contains(card.Summary, "summary") {
		t.Fatalf("tool card stored unsafe summary: %q", card.Summary)
	}

	rendered := card.View(72)
	for _, forbidden := range []string{"\x1b]", "\x07", "\u202e", "https://example.invalid"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("tool card retained terminal control payload %q: %q", forbidden, rendered)
		}
	}
	for _, want := range []string{"line", "next", "spoof"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("tool card dropped visible content %q: %q", want, rendered)
		}
	}
}

func TestCollapsedToolBodyFooterGrammar(t *testing.T) {
	// Multi-line hidden bodies advertise size with the shared "N lines hidden"
	// grammar (matches the output-viewer receipt) and name the primary key that
	// reveals them. Header clicks still expand; hidden content with no stated
	// way to reach it is a dead end for anyone who has not already read help.
	if got := collapsedToolBodyFooter("one\ntwo\nthree", "ctrl+t"); got != "3 lines hidden · ctrl+t" {
		t.Fatalf("collapsed body footer = %q, want \"3 lines hidden · ctrl+t\"", got)
	}
	if got := collapsedToolBodyFooter("one\ntwo", ""); got != "2 lines hidden · ctrl+t" {
		t.Fatalf("empty chord must fall back to ToggleFocusedTool default, got %q", got)
	}
	for name, body := range map[string]string{
		"empty":       "",
		"blank":       " \n\t\n",
		"single line": "only line\n",
	} {
		if got := collapsedToolBodyFooter(body, "ctrl+t"); got != "" {
			t.Fatalf("%s body produced footer %q, want none", name, got)
		}
	}
}

func TestToolCardCollapsedErrorAlwaysShowsResult(t *testing.T) {
	card := NewToolCard("write_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardError
	card.Expanded = false
	card.Duration = 250 * time.Millisecond
	card.SetSummary("write 配置.yaml")
	card.Result = "permission denied for 配置.yaml"

	view := card.View(38)
	if !strings.Contains(view, "permission denied") {
		t.Fatalf("collapsed error hid its result:\n%s", view)
	}
	assertToolCardLinesFit(t, view, 38)

	card.Result = ""
	if fallback := card.View(38); !strings.Contains(fallback, "no error details") {
		t.Fatalf("empty error did not expose a diagnostic fallback:\n%s", fallback)
	}
}

func TestToolCardHeaderUsesSpaceBetweenVerbAndObject(t *testing.T) {
	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Duration = 42 * time.Millisecond
	card.SetSummary("path/to/file")
	collapsed := ansi.Strip(card.View(56))
	// Collapsed success: status + verb + space + object; no disclosure; no
	// duration under the calm-width threshold (inner < 72).
	if !strings.Contains(collapsed, "✓ Read path/to/file") {
		t.Fatalf("header missing space-separated verb/object:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "▸") || strings.Contains(collapsed, "▾") {
		t.Fatalf("collapsed success showed a disclosure mark:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "Read · ") || strings.Contains(collapsed, "Read | ") {
		t.Fatalf("header still uses middle-dot/pipe between verb and object:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "(42ms)") {
		t.Fatalf("collapsed success kept tertiary duration on a calm width:\n%s", collapsed)
	}

	// Wide collapsed success may surface duration once the content column is
	// roomy enough; expanded always prefers showing measured duration.
	wideCollapsed := ansi.Strip(card.View(contentLeftColumns + toolCardCollapsedDurationMinInner))
	if !strings.Contains(wideCollapsed, "✓ Read path/to/file") || !strings.Contains(wideCollapsed, "(42ms)") {
		t.Fatalf("wide collapsed success missing verb/object or duration:\n%s", wideCollapsed)
	}
	if strings.Contains(wideCollapsed, "▸") || strings.Contains(wideCollapsed, "▾") {
		t.Fatalf("wide collapsed success showed a disclosure mark:\n%s", wideCollapsed)
	}
	card.Expanded = true
	expanded := ansi.Strip(card.View(56))
	// Expanded headers drop the object summary (body holds detail) but keep
	// disclosure + duration on the status line.
	if !strings.Contains(expanded, "▾ ✓ Read") || !strings.Contains(expanded, "(42ms)") {
		t.Fatalf("expanded header missing disclosure or duration:\n%s", expanded)
	}
	if strings.Contains(expanded, "▸") {
		t.Fatalf("expanded header kept collapsed disclosure:\n%s", expanded)
	}

	card.Expanded = false
	card.State = ToolCardRunning
	card.SetSummary("path")
	running := ansi.Strip(card.ViewWithActivity(48, "…", 0))
	if !strings.Contains(running, "… Reading path") && !strings.Contains(running, "Reading path") {
		t.Fatalf("running header missing space-separated verb/object:\n%s", running)
	}
	if strings.Contains(running, "Reading · ") || strings.Contains(running, "▸") || strings.Contains(running, "▾") {
		t.Fatalf("running header used separator or unexpected disclosure:\n%s", running)
	}
}

func TestToolCardCompletedReceiptShowsDisclosureState(t *testing.T) {
	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Duration = 42 * time.Millisecond
	card.Result = "package ui"

	// Calm collapsed: no disclosure glyph; lifecycle glyph remains.
	collapsed := ansi.Strip(card.View(48))
	if !strings.Contains(collapsed, "✓ Read") {
		t.Fatalf("collapsed receipt lost its success glyph:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "▸") || strings.Contains(collapsed, "▾") {
		t.Fatalf("collapsed receipt showed a disclosure mark:\n%s", collapsed)
	}

	card.Expanded = true
	expanded := ansi.Strip(card.View(48))
	if !strings.Contains(expanded, "▾ ✓ Read") || strings.Contains(expanded, "▸") {
		t.Fatalf("expanded receipt did not expose its disclosure state:\n%s", expanded)
	}
	if !strings.Contains(expanded, "(42ms)") {
		t.Fatalf("expanded receipt omitted measured duration:\n%s", expanded)
	}

	card.State = ToolCardRunning
	running := ansi.Strip(card.View(48))
	if strings.Contains(running, "▸") || strings.Contains(running, "▾") {
		t.Fatalf("non-expandable running receipt showed a disclosure mark:\n%s", running)
	}

	card = NewToolCard("bash", ToolCardBash, true, defaultThemeID)
	card.State = ToolCardError
	card.Duration = 310 * time.Millisecond
	card.Result = "exit status 1"
	failed := ansi.Strip(card.View(48))
	// Collapsed error: bright status, no disclosure; duration may remain as diagnostic.
	if !strings.Contains(failed, "✗ Run failed") || !strings.Contains(failed, "exit status 1") {
		t.Fatalf("failed receipt lost its error state:\n%s", failed)
	}
	if strings.Contains(failed, "▸") || strings.Contains(failed, "▾") {
		t.Fatalf("collapsed error kept disclosure noise:\n%s", failed)
	}

	card.State = ToolCardSuccess
	card.Expanded = false
	for _, width := range []int{4, 5, 12} {
		assertToolCardLinesFit(t, card.View(width), width)
	}
	if tiny := ansi.Strip(card.View(4)); strings.Contains(tiny, "▸") || strings.Contains(tiny, "▾") {
		t.Fatalf("extreme-width receipt kept a disclosure mark that cannot fit: %q", tiny)
	}
}

func TestExpandedBashUnifiedDiffUsesAdaptiveSemanticColors(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	card := NewToolCard("bash", ToolCardBash, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.Result = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n context"

	view := card.View(72)
	plain := ansi.Strip(view)
	for _, want := range []string{"diff --git", "-old", "+new", " context"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("colored diff omitted %q:\n%s", want, plain)
		}
	}
	if !hasANSIColor(view) {
		t.Fatalf("expanded bash diff did not render semantic colors:\n%s", view)
	}
	if got := card.renderUnifiedDiffResultLine("+new"); got == card.renderUnifiedDiffResultLine("-old") {
		t.Fatal("added and removed lines used the same semantic rendering")
	}
}

func TestBashNonDiffOutputKeepsOrdinaryResultStyle(t *testing.T) {
	if looksLikeUnifiedDiff("+ one line\n- another") {
		t.Fatal("unstructured plus/minus output was mistaken for a unified diff")
	}
}

func TestExpandedFileReadUsesTrustedAdaptiveChroma(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	const source = "package ui\n\nfunc answer() int { return 42 }"
	views := make([]string, 0, 2)
	for _, isDark := range []bool{false, true} {
		card := NewToolCard("read_file", ToolCardFile, isDark, defaultThemeID)
		card.State = ToolCardSuccess
		card.Expanded = true
		card.ResultLanguage = trustedResultLanguageFromPath("internal/ui/model.go")
		card.Result = source
		view := card.View(34)
		views = append(views, view)
		plain := ansi.Strip(view)
		for _, want := range []string{"package ui", "func answer", "return 42"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("theme dark=%v highlighted code omitted %q:\n%s", isDark, want, plain)
			}
		}
		if !hasANSIColor(view) {
			t.Fatalf("theme dark=%v emitted no semantic color:\n%s", isDark, view)
		}
		assertToolCardLinesFit(t, view, 34)
	}
	if views[0] == views[1] {
		t.Fatal("light and dark code styles rendered identically")
	}
}

func TestToolResultLanguageComesOnlyFromBoundedHostMetadata(t *testing.T) {
	if got, want := trustedResultLanguageForTool("read_file", map[string]any{"path": "src/main.ts"}), "typescript"; got != want {
		t.Fatalf("trusted language = %q, want %q", got, want)
	}
	for name, args := range map[string]map[string]any{
		"write_file": {"path": "src/main.ts"},
		"read_file":  {"path": "src/unknown.private"},
		"bash":       {"path": "src/main.ts"},
	} {
		if got := trustedResultLanguageForTool(name, args); got != "" {
			t.Fatalf("%s admitted untrusted language %q", name, got)
		}
	}
	if got := normalizeTrustedResultLanguage("../../bash"); got != "" {
		t.Fatalf("arbitrary lexer alias survived normalization: %q", got)
	}
}

func TestExpandedSearchResultUsesSemanticPathLocationAndMatch(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	card := NewToolCard("vecgrep_search", ToolCardSearch, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.Result = "internal/ui/model.go:42:7:func (m *Model) Update()\ninternal/ui/view.go:18:render transcript"

	view := card.View(72)
	plain := ansi.Strip(view)
	for _, want := range []string{"internal/ui/model.go", ":42:7:", "func (m *Model)", "internal/ui/view.go", ":18:"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("semantic search output omitted %q:\n%s", want, plain)
		}
	}
	if got, ordinary := card.renderSearchResultLine("internal/ui/model.go:42:match", 72), card.Styles.Result.Render("internal/ui/model.go:42:match"); got == ordinary {
		t.Fatal("search path/location/match retained the ordinary single-style renderer")
	}
	assertToolCardLinesFit(t, view, 72)
}

func TestSemanticToolResultPreviewIsBoundedAndVisible(t *testing.T) {
	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.ResultLanguage = "go"
	var result strings.Builder
	for line := 0; line < maxToolResultPreviewLines+9; line++ {
		result.WriteString("x\n")
	}
	card.Result = result.String()

	plain := ansi.Strip(card.View(24))
	if !strings.Contains(plain, "… 81 more lines") {
		t.Fatalf("bounded preview omitted hidden-line receipt:\n%s", plain)
	}
	if got := strings.Count(plain, "\nx"); got > readPreviewHeadRows+readPreviewTailRows {
		t.Fatalf(
			"preview rendered %d content lines, want <= %d",
			got,
			readPreviewHeadRows+readPreviewTailRows,
		)
	}
}

func TestSemanticToolResultHonorsNoColorAndSanitizesBeforeHighlighting(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })

	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.ResultLanguage = "go"
	card.Result = "package ui\n\x1b]0;owned\x07func safe() {}\u202e"
	view := card.View(32)
	if hasANSIColor(view) {
		t.Fatalf("NO_COLOR semantic result emitted ANSI color: %q", view)
	}
	for _, forbidden := range []string{"owned", "\x1b]", "\u202e"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("unsafe result payload %q survived: %q", forbidden, view)
		}
	}
	assertToolCardLinesFit(t, view, 32)
}

func TestSemanticToolResultUnknownLanguageFallsBackToPlainText(t *testing.T) {
	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.ResultLanguage = "../../go"
	got := card.renderSemanticResultLines("plain output", 40)
	if len(got) != 1 || ansi.Strip(got[0]) != "1 │ plain output" {
		t.Fatalf("numbered plain fallback = %#v, want one source row", got)
	}
}

func TestSemanticToolResultCacheIsBoundedAndReturnsCopies(t *testing.T) {
	semanticToolResultCache.Lock()
	clear(semanticToolResultCache.entries)
	semanticToolResultCache.Unlock()
	t.Cleanup(func() {
		semanticToolResultCache.Lock()
		clear(semanticToolResultCache.entries)
		semanticToolResultCache.Unlock()
	})

	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.ResultLanguage = "go"
	for index := 0; index < maxToolResultRenderCache+17; index++ {
		card.renderSemanticResultLines(fmt.Sprintf("package p%d", index), 40)
	}
	semanticToolResultCache.RLock()
	cacheSize := len(semanticToolResultCache.entries)
	semanticToolResultCache.RUnlock()
	if cacheSize > maxToolResultRenderCache {
		t.Fatalf("semantic result cache = %d entries, cap = %d", cacheSize, maxToolResultRenderCache)
	}

	first := card.renderSemanticResultLines("package stable", 40)
	first[0] = "mutated caller copy"
	second := card.renderSemanticResultLines("package stable", 40)
	if len(second) != 1 || second[0] == first[0] {
		t.Fatalf("cache exposed mutable backing storage: first=%#v second=%#v", first, second)
	}
}

func TestSemanticToolResultCacheSeparatesGlyphProfiles(t *testing.T) {
	semanticToolResultCache.Lock()
	clear(semanticToolResultCache.entries)
	semanticToolResultCache.Unlock()
	t.Cleanup(func() {
		semanticToolResultCache.Lock()
		clear(semanticToolResultCache.entries)
		semanticToolResultCache.Unlock()
	})

	unicodeCard := NewToolCard("read_file", ToolCardFile, true, defaultThemeID, GlyphUnicode)
	unicodeCard.PreviewMode = ToolPreviewRead
	asciiCard := NewToolCard("read_file", ToolCardFile, true, defaultThemeID, GlyphASCII)
	asciiCard.PreviewMode = ToolPreviewRead

	const result = "first\nsecond"
	unicodeView := ansi.Strip(strings.Join(
		unicodeCard.renderSemanticResultLines(result, 24),
		"\n",
	))
	asciiView := ansi.Strip(strings.Join(
		asciiCard.renderSemanticResultLines(result, 24),
		"\n",
	))
	if !strings.Contains(unicodeView, "1 │ first") || strings.Contains(unicodeView, " | ") {
		t.Fatalf("Unicode cache projection lost its gutter: %q", unicodeView)
	}
	if !strings.Contains(asciiView, "1 | first") || strings.Contains(asciiView, "│") {
		t.Fatalf("ASCII projection reused Unicode cache content: %q", asciiView)
	}
}

func TestToolCardStatusGlyphsKeepUnknownDistinctAndSingleWidth(t *testing.T) {
	tests := []struct {
		name       string
		state      ToolCardState
		projection ecosystem.ToolProjection
		want       string
	}{
		{name: "running", state: ToolCardRunning, want: "…"},
		{name: "success", state: ToolCardSuccess, want: "✓"},
		{
			name:  "unknown outcome",
			state: ToolCardAttention,
			projection: ecosystem.ToolProjection{
				Transport: ecosystem.TransportSucceeded,
				Domain:    ecosystem.DomainUnknown,
			},
			want: "?",
		},
		{
			name:  "known attention",
			state: ToolCardAttention,
			projection: ecosystem.ToolProjection{
				Transport: ecosystem.TransportSucceeded,
				Domain:    ecosystem.DomainConflict,
			},
			want: "!",
		},
		{name: "failure", state: ToolCardError, want: "✗"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := NewToolCard("remote_tool", ToolCardGeneric, true, defaultThemeID)
			card.State = tt.state
			card.Projection = tt.projection
			if got := card.statusGlyph(); got != tt.want {
				t.Fatalf("status glyph = %q, want %q", got, tt.want)
			} else if width := lipgloss.Width(got); width != 1 {
				t.Fatalf("status glyph %q width = %d, want 1", got, width)
			}
		})
	}
}

func TestToolCardOmitsMeaninglessZeroDuration(t *testing.T) {
	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.SetSummary("internal/ui/toolcard.go")

	withoutDuration := ansi.Strip(card.View(40))
	if strings.Contains(withoutDuration, "(0s)") {
		t.Fatalf("zero-duration receipt kept meaningless timing noise:\n%s", withoutDuration)
	}
	if !strings.Contains(withoutDuration, "Read") || !strings.Contains(withoutDuration, "toolcard.go") {
		t.Fatalf("compact receipt did not spend the recovered width on useful context:\n%s", withoutDuration)
	}

	card.Duration = 42 * time.Millisecond
	// Collapsed success hides duration under the calm-width threshold.
	collapsed := ansi.Strip(card.View(40))
	if strings.Contains(collapsed, "(42ms)") {
		t.Fatalf("collapsed success kept tertiary duration on a calm width:\n%s", collapsed)
	}
	card.Expanded = true
	expanded := ansi.Strip(card.View(40))
	if !strings.Contains(expanded, "(42ms)") {
		t.Fatalf("expanded receipt omitted measured duration:\n%s", expanded)
	}
}

func TestToolCardProjectionStateFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		projection ecosystem.ToolProjection
		want       ToolCardState
	}{
		{
			name: "transport success without domain interpretation",
			projection: ecosystem.ToolProjection{
				Transport: ecosystem.TransportSucceeded,
			},
			want: ToolCardAttention,
		},
		{
			name: "successful domain with stale evidence",
			projection: ecosystem.ToolProjection{
				Transport: ecosystem.TransportSucceeded,
				Domain:    ecosystem.DomainSucceeded,
				Evidence:  ecosystem.EvidenceStale,
			},
			want: ToolCardAttention,
		},
		{
			name: "successful domain with contradicted evidence",
			projection: ecosystem.ToolProjection{
				Transport: ecosystem.TransportSucceeded,
				Domain:    ecosystem.DomainSucceeded,
				Evidence:  ecosystem.EvidenceContradicted,
			},
			want: ToolCardAttention,
		},
		{
			name: "domain failure wins over running transport",
			projection: ecosystem.ToolProjection{
				Transport: ecosystem.TransportRunning,
				Domain:    ecosystem.DomainFailed,
			},
			want: ToolCardError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolCardStateFromProjection(tt.projection); got != tt.want {
				t.Fatalf("state = %v, want %v for %#v", got, tt.want, tt.projection)
			}
		})
	}
}

func TestToolCardAttentionHonorsNoColor(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })

	card := NewToolCard("mcphub__mcphub_call_tool", ToolCardGeneric, true, defaultThemeID)
	card.State = ToolCardAttention
	card.Projection = ecosystem.ToolProjection{
		Operation: "cortex_start_task", Role: ecosystem.RoleCoordination,
		Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainAttention,
		Route: ecosystem.ToolRoute{Gateway: "mcphub", Server: "cortex", CallID: "call-7", Lazy: true},
	}
	rendered := card.View(48)
	if hasANSIColor(rendered) {
		t.Fatalf("NO_COLOR attention receipt emitted ANSI color: %q", rendered)
	}
}

func TestASCIIExpandedRoutedAttentionCardUsesProfileAwareChrome(t *testing.T) {
	projection := ecosystem.ToolProjection{
		Specialist: "cortex",
		Operation:  "cortex_start_task",
		Role:       ecosystem.RoleCoordination,
		Transport:  ecosystem.TransportSucceeded,
		Domain:     ecosystem.DomainAttention,
		Route: ecosystem.ToolRoute{
			Gateway: "mcphub",
			Server:  "cortex",
			Tool:    "cortex_start_task",
			CallID:  "call-ascii-17",
			Lazy:    true,
		},
		Digest: &ecosystem.ReceiptDigest{
			Kind:          ecosystem.DigestMCPHubStored,
			OriginalBytes: 4096,
			BudgetBytes:   1024,
		},
	}
	card := NewToolCard("mcphub__mcphub_call_tool", ToolCardGeneric, true, defaultThemeID, GlyphASCII)
	card.State = ToolCardAttention
	card.Lifecycle = ToolLifecycleAttention
	card.Expanded = true
	card.Duration = 2 * time.Millisecond
	card.Projection = projection

	rendered := ansi.Strip(card.View(42))
	for _, forbidden := range []string{"…", "·"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("ASCII routed attention card retained %q:\n%s", forbidden, rendered)
		}
	}
	for _, want := range []string{
		"v ! Result stored",
		"specialist: Cortex | coordination",
		"route: sonar > MCPHub > Cortex |",
		"result stored | 4096 bytes | fetch",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("ASCII routed attention card omitted %q:\n%s", want, rendered)
		}
	}
	assertToolCardLinesFit(t, rendered, 42)

	asciiDetails := strings.Join(toolProjectionDetails(projection, GlyphASCII), "\n")
	asciiAttention := compactToolAttention(projection, GlyphASCII)
	asciiSummary := boundedToolCardSummary(projection.SummaryText(), GlyphASCII)
	for label, value := range map[string]string{
		"details":   asciiDetails,
		"attention": asciiAttention,
		"summary":   asciiSummary,
	} {
		for _, forbidden := range []string{"…", "·"} {
			if strings.Contains(value, forbidden) {
				t.Fatalf("ASCII %s retained %q: %q", label, forbidden, value)
			}
		}
	}
	if !strings.Contains(asciiDetails, " | lazy") ||
		!strings.Contains(asciiAttention, "result stored | 4096 bytes | fetch call-ascii-17") ||
		!strings.Contains(asciiSummary, "result stored | 4096 bytes | fetch call-ascii-17") {
		t.Fatalf(
			"ASCII semantic projections lost profile-aware separators:\ndetails=%s\nattention=%s\nsummary=%s",
			asciiDetails,
			asciiAttention,
			asciiSummary,
		)
	}

	if unicodeAttention := compactToolAttention(projection, GlyphUnicode); !strings.Contains(unicodeAttention, " · ") {
		t.Fatalf("Unicode attention punctuation changed: %q", unicodeAttention)
	}
	if unicodeDetails := strings.Join(toolProjectionDetails(projection, GlyphUnicode), "\n"); !strings.Contains(unicodeDetails, " · ") {
		t.Fatalf("Unicode projection punctuation changed: %q", unicodeDetails)
	}
}

func assertToolCardLinesFit(t *testing.T, rendered string, width int) {
	t.Helper()
	for lineNumber, line := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("line %d width = %d, want <= %d:\n%s", lineNumber, got, width, rendered)
		}
	}
}
