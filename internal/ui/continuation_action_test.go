package ui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestContinuationActionMessageIsEphemeralAndRejectsStaleDelivery(t *testing.T) {
	m := newTestModel(t)
	m.beginContinuationTurn("turn-current")
	entriesBefore := len(m.entries)
	toolsBefore := len(m.toolEntries)

	action := ContinuationActionPresentation{
		Tool:       "cortex_plan_private_marker",
		Inputs:     []string{"hypotheses_private_marker", "uncertainty"},
		BlockedBy:  []string{"insufficient_evidence_private_marker"},
		ReasonCode: "evidence_missing",
	}
	updated, _ := m.Update(ContinuationActionMsg{TurnID: "turn-current", Sequence: 2, Action: &action})
	m = updated.(*Model)
	if len(m.entries) != entriesBefore || len(m.toolEntries) != toolsBefore {
		t.Fatalf("continuation mutated transcript state: entries=%d tools=%d", len(m.entries), len(m.toolEntries))
	}
	plain := ansi.Strip(m.renderContinuationAction())
	for _, want := range []string{"next:", "cortex_plan_private_marker", "needs:", "hypotheses_private_marker", "blocked:", "insufficient_evidence_private_marker"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("continuation card missing %q:\n%s", want, plain)
		}
	}

	state, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"cortex_plan_private_marker", "hypotheses_private_marker", "insufficient_evidence_private_marker", "evidence_missing"} {
		if strings.Contains(state, forbidden) {
			t.Fatalf("ephemeral continuation entered session state through %q: %s", forbidden, state)
		}
	}

	stale := ContinuationActionPresentation{Tool: "stale_tool"}
	updated, _ = m.Update(ContinuationActionMsg{TurnID: "turn-current", Sequence: 1, Action: &stale})
	m = updated.(*Model)
	updated, _ = m.Update(ContinuationActionMsg{TurnID: "turn-prior", Sequence: 99, Action: &stale})
	m = updated.(*Model)
	if got := ansi.Strip(m.renderContinuationAction()); strings.Contains(got, "stale_tool") || !strings.Contains(got, "cortex_plan_private_marker") {
		t.Fatalf("stale delivery replaced current suggestion:\n%s", got)
	}

	replacement := ContinuationActionPresentation{Tool: "bob_playbook", Inputs: []string{"environment"}}
	updated, _ = m.Update(ContinuationActionMsg{TurnID: "turn-current", Sequence: 3, Action: &replacement})
	m = updated.(*Model)
	if got := ansi.Strip(m.renderContinuationAction()); !strings.Contains(got, "bob_playbook") || strings.Contains(got, "cortex_plan_private_marker") {
		t.Fatalf("newer suggestion did not replace the old one:\n%s", got)
	}

	updated, _ = m.Update(ContinuationActionMsg{TurnID: "turn-current", Sequence: 4})
	m = updated.(*Model)
	if m.continuation.card != nil || m.renderContinuationAction() != "" {
		t.Fatal("authoritative clear retained a continuation suggestion")
	}
}

func TestContinuationActionSanitizesAndBoundsPresentation(t *testing.T) {
	action, ok := normalizeContinuationActionPresentation(ContinuationActionPresentation{
		Tool:       "cortex_plan\x1b[31m\nspoof",
		Inputs:     append([]string{"hypotheses\x1b]0;private\a", "hypotheses\x1b]0;private\a"}, repeatedContinuationIDs("input", 12)...),
		BlockedBy:  repeatedContinuationIDs("blocker", 12),
		ReasonCode: "reason\u202econtrol",
	})
	if !ok {
		t.Fatal("bounded action was rejected")
	}
	if len(action.Inputs) != maxContinuationItems || len(action.BlockedBy) != maxContinuationItems {
		t.Fatalf("identifier bounds = inputs:%d blockers:%d", len(action.Inputs), len(action.BlockedBy))
	}
	card := newContinuationActionCard(action, true, defaultThemeID)
	for _, width := range []int{30, 47, 48, 80} {
		card.SetWidth(width)
		rendered := card.View()
		assertRenderedLinesFit(t, rendered, width)
		if width < continuationCompactWidth && lipgloss.Height(rendered) > 2 {
			t.Fatalf("compact card height at width %d = %d:\n%s", width, lipgloss.Height(rendered), rendered)
		}
	}
	plain := ansi.Strip(card.View())
	for _, forbidden := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(plain, forbidden) {
			t.Fatalf("control sequence survived presentation: %q", plain)
		}
	}
}

func TestContinuationActionCardHonorsNoColor(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	action, ok := normalizeContinuationActionPresentation(ContinuationActionPresentation{
		Tool: "cortex_plan", Inputs: []string{"hypotheses"}, BlockedBy: []string{"evidence"},
	})
	if !ok {
		t.Fatal("fixture did not normalize")
	}
	card := newContinuationActionCard(action, true, defaultThemeID)
	card.SetWidth(72)
	if rendered := card.View(); hasANSIColor(rendered) {
		t.Fatalf("NO_COLOR continuation emitted ANSI color: %q", rendered)
	}
}

func TestContinuationActionCardCachesAndInvalidatesOnLayoutOrTheme(t *testing.T) {
	action, ok := normalizeContinuationActionPresentation(ContinuationActionPresentation{Tool: "cortex_plan", Inputs: []string{"hypotheses"}})
	if !ok {
		t.Fatal("fixture did not normalize")
	}
	card := newContinuationActionCard(action, true, defaultThemeID)
	card.SetWidth(72)
	first := card.View()
	if first == "" || card.cachedView == "" {
		t.Fatal("first render did not populate the cache")
	}
	card.SetWidth(72)
	if second := card.View(); second != first || card.cachedView == "" {
		t.Fatal("same-size render did not reuse stable content")
	}
	card.SetWidth(48)
	if card.cachedView != "" {
		t.Fatal("width change retained stale cached view")
	}
	_ = card.View()
	card.SetTheme(false, defaultThemeID)
	if card.cachedView != "" {
		t.Fatal("theme change retained stale cached view")
	}
}

func TestContinuationActionExpandedCardStaysWithinThreeRows(t *testing.T) {
	action, ok := normalizeContinuationActionPresentation(ContinuationActionPresentation{
		Tool: strings.Repeat("cortex_plan_", 12), Inputs: repeatedContinuationIDs("input", 12),
		BlockedBy: repeatedContinuationIDs("blocker", 12), ReasonCode: "source_blocked",
	})
	if !ok {
		t.Fatal("fixture did not normalize")
	}
	card := newContinuationActionCard(action, true, defaultThemeID)
	card.SetWidth(48)
	if rendered := card.View(); lipgloss.Height(rendered) > 3 {
		t.Fatalf("48-column action card consumed %d rows:\n%s", lipgloss.Height(rendered), rendered)
	}
}

func TestContinuationActionClearsWhenTurnFails(t *testing.T) {
	m := newTestModel(t)
	m.beginContinuationTurn("turn-failed")
	action := ContinuationActionPresentation{Tool: "cortex_plan"}
	updated, _ := m.Update(ContinuationActionMsg{TurnID: "turn-failed", Sequence: 1, Action: &action})
	m = updated.(*Model)
	if m.continuation.card == nil {
		t.Fatal("fixture did not install continuation")
	}
	updated, _ = m.Update(AgentDoneMsg{TurnID: "turn-failed", Err: context.Canceled})
	m = updated.(*Model)
	if m.continuation.card != nil || m.continuation.turnID != "" {
		t.Fatal("failed turn retained stale continuation")
	}
}

func TestContinuationActionFooterOwnsExactRowsAndPreservesPausedTranscript(t *testing.T) {
	m := newTestModel(t)
	setScrollableTranscript(m)
	m.recalcViewportHeight()
	initialHeight := m.viewport.Height()
	m.setTranscriptYOffset(5)
	m.pauseFollow()
	m.beginContinuationTurn("turn-layout")
	action := ContinuationActionPresentation{
		Tool: "cortex_plan", Inputs: []string{"hypotheses", "uncertainty"}, BlockedBy: []string{"insufficient_evidence"},
	}
	updated, _ := m.Update(ContinuationActionMsg{TurnID: "turn-layout", Sequence: 1, Action: &action})
	m = updated.(*Model)
	cardHeight := lipgloss.Height(m.renderContinuationAction())
	if got, want := m.viewport.Height(), initialHeight-cardHeight; got != want {
		t.Fatalf("viewport height with action = %d, want %d", got, want)
	}
	if got := m.transcriptYOffset(); got != 5 || !m.followPaused() {
		t.Fatalf("action replacement moved paused transcript: offset=%d paused=%v", got, m.followPaused())
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	assertRenderedLinesFit(t, m.View().Content, minTerminalWidth)
	assertRenderedHeightFits(t, m.View().Content, minTerminalHeight)
}

func TestContinuationActionClearsAcrossConversationAndSessionRestore(t *testing.T) {
	m := newTestModel(t)
	m.beginContinuationTurn("turn-reset")
	action := ContinuationActionPresentation{Tool: "cortex_plan"}
	updated, _ := m.Update(ContinuationActionMsg{TurnID: "turn-reset", Sequence: 1, Action: &action})
	m = updated.(*Model)
	updated, _ = m.Update(ctrlKey('n'))
	m = updated.(*Model)
	if m.continuation.card != nil || m.continuation.turnID != "" {
		t.Fatal("new conversation retained continuation state")
	}

	m.beginContinuationTurn("turn-restore")
	updated, _ = m.Update(ContinuationActionMsg{TurnID: "turn-restore", Sequence: 1, Action: &action})
	m = updated.(*Model)
	raw, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	if m.continuation.card != nil || m.continuation.turnID != "" || m.continuation.sequence != 0 {
		t.Fatal("session restore retained ephemeral continuation state")
	}
}

func repeatedContinuationIDs(prefix string, count int) []string {
	result := make([]string, count)
	for index := range result {
		result[index] = prefix + "_" + strings.Repeat("x", index+1)
	}
	return result
}
