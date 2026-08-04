package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestGlyphProfilesPreserveSingleCellGeometry(t *testing.T) {
	for _, profile := range []GlyphProfile{GlyphUnicode, GlyphASCII} {
		glyphs := glyphSet(profile)
		for name, glyph := range map[string]string{
			"user rail":    glyphs.UserRail,
			"collapsed":    glyphs.Collapsed,
			"expanded":     glyphs.Expanded,
			"success":      glyphs.Success,
			"error":        glyphs.Error,
			"running":      glyphs.Running,
			"queued":       glyphs.Queued,
			"waiting":      glyphs.Waiting,
			"cancelled":    glyphs.Cancelled,
			"continuation": glyphs.Continuation,
			"selected":     glyphs.Selected,
			"unselected":   glyphs.Unselected,
			"vertical":     glyphs.Vertical,
			"horizontal":   glyphs.Horizontal,
			"left":         glyphs.Left,
			"right":        glyphs.Right,
		} {
			if width := lipgloss.Width(glyph); width != 1 {
				t.Fatalf("profile %d %s glyph %q is %d cells, want 1", profile, name, glyph, width)
			}
		}
	}
}

func TestASCIIProfileMatchesSemanticFallbackContract(t *testing.T) {
	glyphs := glyphSet(GlyphASCII)
	if glyphs.UserRail != "|" || glyphs.Collapsed != ">" || glyphs.Expanded != "v" ||
		glyphs.Success != "+" || glyphs.Error != "x" || glyphs.Running != "*" ||
		glyphs.Continuation != ">" {
		t.Fatalf("ASCII glyph contract changed: %+v", glyphs)
	}
}

func TestRequestedGlyphProfileDoesNotConflateNoColor(t *testing.T) {
	t.Setenv("SONAR_GLYPHS", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	if got := requestedGlyphProfile(); got != GlyphUnicode {
		t.Fatalf("NO_COLOR selected profile %d, want Unicode", got)
	}

	t.Setenv("SONAR_GLYPHS", "ascii")
	if got := requestedGlyphProfile(); got != GlyphASCII {
		t.Fatalf("explicit ASCII profile = %d", got)
	}
}

func TestNewSelectsASCIIComponentsForDumbTerminal(t *testing.T) {
	t.Setenv("SONAR_GLYPHS", "")
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "")

	m := New(nil, nil, nil, nil, nil, nil, nil)
	if m.glyphProfile != GlyphASCII {
		t.Fatalf("Model glyph profile = %d, want ASCII", m.glyphProfile)
	}
	if frames := strings.Join(m.spin.Spinner.Frames, ""); frames != "|/-\\" {
		t.Fatalf("dumb-terminal spinner frames = %q, want ASCII Line spinner", frames)
	}
	if prompt := ansi.Strip(m.input.View()); strings.ContainsAny(prompt, "▌▏│❯") {
		t.Fatalf("dumb-terminal composer retained Unicode prompt glyphs: %q", prompt)
	}
}

func TestNewKeepsUnicodeGlyphsIndependentFromNoColor(t *testing.T) {
	t.Setenv("SONAR_GLYPHS", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")

	m := New(nil, nil, nil, nil, nil, nil, nil)
	if m.glyphProfile != GlyphUnicode {
		t.Fatalf("NO_COLOR selected profile %d, want Unicode", m.glyphProfile)
	}
}

func TestASCIIProfilePropagatesAcrossPrimarySemanticSurfaces(t *testing.T) {
	t.Setenv("SONAR_GLYPHS", "ascii")
	t.Setenv("TERM", "xterm-256color")
	m := New(nil, nil, nil, nil, nil, nil, nil)
	m.initializing = false
	m.handleWindowSize(tea.WindowSizeMsg{Width: 80, Height: 24}, nil)

	var user strings.Builder
	m.renderUserMsg(&user, "inspect the interface", nil, 72)

	var failure strings.Builder
	m.renderEntryError(&failure, "operation failed", 72)

	m.input.SetValue("first line\nsecond line")
	surfaces := []string{
		ansi.Strip(m.View().Content),
		user.String(),
		failure.String(),
		m.renderThinkingBox("inspect\nverify", false),
		ansi.Strip(m.input.View()),
	}
	m.openProviderPicker()
	surfaces = append(surfaces, ansi.Strip(m.renderProviderPicker()))
	m.closeProviderPicker()

	for _, state := range []struct {
		cardState ToolCardState
		lifecycle ToolLifecycle
	}{
		{ToolCardRunning, ToolLifecycleRunning},
		{ToolCardSuccess, ToolLifecycleSucceeded},
		{ToolCardAttention, ToolLifecycleCancelled},
		{ToolCardError, ToolLifecycleFailed},
	} {
		card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID, GlyphASCII)
		card.State = state.cardState
		card.Lifecycle = state.lifecycle
		card.Expanded = state.cardState != ToolCardRunning
		surfaces = append(surfaces, card.View(48))
	}

	projection := ecosystem.ToolProjection{
		Transport: ecosystem.TransportSucceeded,
		Domain:    ecosystem.DomainSucceeded,
		Evidence:  ecosystem.EvidenceVerified,
		Route: ecosystem.ToolRoute{
			Gateway: "mcphub",
			Server:  "cortex",
		},
	}
	surfaces = append(surfaces, strings.Join(toolProjectionDetails(projection, GlyphASCII), "\n"))

	diff := renderUnifiedDiffAtWidth(
		"main.go",
		[]DiffLine{{Kind: DiffAdded, Content: strings.Repeat("content ", 8), NewLine: 1}},
		NewStyles(true, defaultThemeID),
		0,
		18,
		GlyphASCII,
	)
	surfaces = append(surfaces, diff)

	nodeStyles := NewToolCardStyles(true, defaultThemeID)
	nodes := []WorkNode{
		{Status: WorkNodeRunning, Label: "runner", Model: "m", Location: WorkNodeLocationLocal},
		{Status: WorkNodeWaiting, Label: "waiter", Model: "m", Location: WorkNodeLocationLocal},
		{Status: WorkNodeCompleted, Label: "done", Model: "m", Location: WorkNodeLocationLocal},
		{Status: WorkNodeFailed, Label: "failed", Model: "m", Location: WorkNodeLocationLocal},
		{Status: WorkNodeCancelled, Label: "cancelled", Model: "m", Location: WorkNodeLocationLocal},
	}
	for _, node := range nodes {
		surfaces = append(
			surfaces,
			strings.Join(renderExpertProgressNode(node, 72, nodeStyles, GlyphASCII), "\n"),
		)
	}
	narrowNode := WorkNode{
		ID:       "expert-00",
		Index:    0,
		Kind:     WorkNodeKindExpert,
		Label:    "accessibility-reviewer",
		Model:    "model-with-a-long-name",
		Location: WorkNodeLocationLocal,
		Status:   WorkNodeRunning,
		Activity: WorkNodeActivityRunning,
		Elapsed:  1500 * time.Millisecond,
		Revision: 2,
	}
	if !narrowNode.valid(1) {
		t.Fatalf("narrow ASCII fixture is not a valid work node: %#v", narrowNode)
	}
	narrowAgent := strings.Join(
		renderExpertProgressNode(narrowNode, 38, nodeStyles, GlyphASCII),
		"\n",
	)
	for _, forbidden := range []string{"…", "·"} {
		if strings.Contains(narrowAgent, forbidden) {
			t.Fatalf("narrow ASCII agent retained %q:\n%s", forbidden, narrowAgent)
		}
	}
	for _, want := range []string{"* accessibility-reviewer | running", "consulting | 1.5s | model-with"} {
		if !strings.Contains(narrowAgent, want) {
			t.Fatalf("narrow ASCII agent omitted %q:\n%s", want, narrowAgent)
		}
	}
	narrowQueue := (&ExpertProgressState{
		Sequence:    1,
		Strategy:    "team",
		Total:       2,
		Parallelism: 1,
		Queued:      2,
		Experts:     make([]ExpertProgressItem, 2),
	}).renderDetails(24, nodeStyles, GlyphASCII)
	for _, forbidden := range []string{"…", "·"} {
		if strings.Contains(narrowQueue, forbidden) {
			t.Fatalf("narrow ASCII queue retained %q:\n%s", forbidden, narrowQueue)
		}
	}
	if !strings.Contains(narrowQueue, "2 experts queued |") || !strings.Contains(narrowQueue, "~") {
		t.Fatalf("narrow ASCII queue omitted its separator or truncation receipt:\n%s", narrowQueue)
	}
	surfaces = append(surfaces, narrowAgent, narrowQueue)
	surfaces = append(surfaces, renderAgentViewerBody(AgentGroupProjection{
		ID:                "ascii_group",
		Revision:          1,
		Lifecycle:         BlockLive,
		ProgressAvailable: true,
		Nodes:             nodes,
	}, 72, true, defaultThemeID, GlyphASCII))

	rendered := ansi.Strip(strings.Join(surfaces, "\n"))
	assertNoUnicodeSemanticGlyphs(t, rendered)
	for _, want := range []string{
		// User rail is the content-grid accent (1 cell) + pad (2 spaces).
		"|  inspect the interface",
		"x error",
		"| v Thought",
		"|> first line",
		"v + Read",
		"v x Read failed",
		"sonar > MCPHub > Cortex",
		"| > content",
		"* runner",
		"+ done",
		"x failed",
		"- cancelled",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("ASCII surfaces omitted %q:\n%s", want, rendered)
		}
	}
}

func TestASCIIActivitySpinnerRemainsFooterOnly(t *testing.T) {
	t.Setenv("SONAR_GLYPHS", "ascii")
	t.Setenv("TERM", "xterm-256color")
	m := New(nil, nil, nil, nil, nil, nil, nil)
	m.initializing = false
	m.state = StateStreaming
	m.handleWindowSize(tea.WindowSizeMsg{Width: 80, Height: 24}, nil)

	const historyEntries = 256
	m.entries = make([]ChatEntry, 0, historyEntries+1)
	for index := 0; index < historyEntries; index++ {
		m.entries = append(m.entries, ChatEntry{
			BlockID:   BlockID(fmt.Sprintf("ascii_history_%03d", index)),
			TurnID:    TurnID(fmt.Sprintf("ascii_turn_%03d", index)),
			Revision:  1,
			Lifecycle: BlockSettled,
			Kind:      "user",
			Content:   "history",
		})
	}
	m.toolEntries = []ToolEntry{{
		ID:        "ascii-active-tool",
		Name:      "read_file",
		Status:    ToolStatusRunning,
		StartTime: time.Now(),
		Collapsed: true,
	}}
	m.entries = append(m.entries, ChatEntry{
		BlockID:   "ascii_active_tool",
		TurnID:    "ascii_active_turn",
		Revision:  1,
		Lifecycle: BlockLive,
		Kind:      "tool_group",
		ToolIndex: 0,
	})
	m.toolsPending = 1
	m.invalidateEntryCache()
	m.refreshTranscript()
	before := m.viewport.GetContent()
	assertNoUnicodeSemanticGlyphs(t, ansi.Strip(before))

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	updated, next := m.Update(m.spin.Tick())
	m = updated.(*Model)
	if next == nil {
		t.Fatal("ASCII activity spinner did not continue")
	}
	assertNoTranscriptPaint(t, probe, "ASCII spinner tick")
	if after := m.viewport.GetContent(); after != before {
		t.Fatal("ASCII spinner tick changed transcript content")
	}
}

func assertNoUnicodeSemanticGlyphs(t *testing.T, rendered string) {
	t.Helper()
	glyphs := glyphSet(GlyphUnicode)
	for name, glyph := range map[string]string{
		"user rail":    glyphs.UserRail,
		"collapsed":    glyphs.Collapsed,
		"expanded":     glyphs.Expanded,
		"success":      glyphs.Success,
		"error":        glyphs.Error,
		"running":      glyphs.Running,
		"queued":       glyphs.Queued,
		"waiting":      glyphs.Waiting,
		"cancelled":    glyphs.Cancelled,
		"continuation": glyphs.Continuation,
		"selected":     glyphs.Selected,
		"vertical":     glyphs.Vertical,
		"horizontal":   glyphs.Horizontal,
		"left":         glyphs.Left,
		"right":        glyphs.Right,
	} {
		if strings.Contains(rendered, glyph) {
			t.Fatalf("ASCII surface retained Unicode %s glyph %q:\n%s", name, glyph, rendered)
		}
	}
}
