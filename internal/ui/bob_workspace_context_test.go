package ui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestBobWorkspaceContextIsBoundedEphemeralAndGenerationFenced(t *testing.T) {
	m := newTestModel(t)
	entriesBefore := len(m.entries)
	toolsBefore := len(m.toolEntries)
	digest := testBobWorkspaceDigest("go-agent-tool-private-marker", "clean", 0)

	updated, _ := m.Update(BobWorkspaceContextMsg{Generation: 2, Digest: digest})
	m = updated.(*Model)
	if len(m.entries) != entriesBefore || len(m.toolEntries) != toolsBefore {
		t.Fatalf("Bob context mutated transcript state: entries=%d tools=%d", len(m.entries), len(m.toolEntries))
	}
	plain := ansi.Strip(m.renderBobWorkspaceContext())
	if want := "bob: ◇ repo clean · go-agent-tool-private-marker@4 · 0 conflicts"; plain != want {
		t.Fatalf("Bob context row = %q, want %q", plain, want)
	}
	for _, forbidden := range []string{"verified", "✓", "✔"} {
		if strings.Contains(strings.ToLower(plain), forbidden) {
			t.Fatalf("Bob convergence implied verification through %q: %q", forbidden, plain)
		}
	}

	raw, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "go-agent-tool-private-marker") {
		t.Fatalf("ephemeral Bob context entered session state: %s", raw)
	}

	stale := testBobWorkspaceDigest("stale-recipe", "conflicted", 2)
	updated, _ = m.Update(BobWorkspaceContextMsg{Generation: 1, Digest: stale})
	m = updated.(*Model)
	if got := ansi.Strip(m.renderBobWorkspaceContext()); strings.Contains(got, "stale-recipe") || !strings.Contains(got, "private-marker") {
		t.Fatalf("stale Bob context replaced current state: %q", got)
	}

	// A newer malformed projection fails closed and acts as an authoritative
	// replacement rather than leaving a stale clean repository status visible.
	invalid := *digest
	invalid.State = "future-state"
	updated, _ = m.Update(BobWorkspaceContextMsg{Generation: 3, Digest: &invalid})
	m = updated.(*Model)
	if m.bobWorkspaceContext.card != nil || m.renderBobWorkspaceContext() != "" || m.bobWorkspaceContext.generation != 3 {
		t.Fatal("new malformed Bob context did not clear the stale status")
	}
}

func TestBobWorkspaceContextCardIsOneRowResponsiveCachedAndNoColor(t *testing.T) {
	context, ok := normalizeBobWorkspaceContext(testBobWorkspaceDigest(strings.Repeat("recipe-", 30), "conflicted", 12))
	if !ok {
		t.Fatal("valid Bob context did not normalize")
	}
	card := newBobWorkspaceContextCard(context, true, defaultThemeID)
	for _, width := range []int{1, 8, 20, 30, 48, 80} {
		card.SetWidth(width)
		rendered := card.View()
		if got := lipgloss.Height(rendered); got != 1 {
			t.Fatalf("width %d Bob context used %d rows: %q", width, got, rendered)
		}
		if got := lipgloss.Width(rendered); got > width {
			t.Fatalf("width %d Bob context overflowed to %d cells: %q", width, got, rendered)
		}
	}
	unsafe := testBobWorkspaceDigest("go-agent\x1b]0;spoof\a\n\u202erecipe", "clean", 0)
	safeContext, ok := normalizeBobWorkspaceContext(unsafe)
	if !ok {
		t.Fatal("bounded unsafe recipe identifier was rejected")
	}
	for _, forbidden := range []string{"\x1b", "\a", "\n", "\u202e"} {
		if strings.Contains(safeContext.recipeID, forbidden) {
			t.Fatalf("control sequence survived Bob context normalization: %q", safeContext.recipeID)
		}
	}

	card.SetWidth(72)
	first := card.View()
	if first == "" || card.cachedView == "" {
		t.Fatal("first render did not populate the Bob context cache")
	}
	card.SetWidth(72)
	if second := card.View(); second != first || card.cachedView == "" {
		t.Fatal("same-width Bob context did not reuse its cached render")
	}
	card.SetWidth(48)
	if card.cachedView != "" {
		t.Fatal("width change retained stale Bob context render")
	}
	_ = card.View()
	card.SetTheme(false, defaultThemeID)
	if card.cachedView != "" {
		t.Fatal("theme change retained stale Bob context render")
	}

	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	plainCard := newBobWorkspaceContextCard(context, true, defaultThemeID)
	plainCard.SetWidth(72)
	if rendered := plainCard.View(); hasANSIColor(rendered) {
		t.Fatalf("NO_COLOR Bob context emitted ANSI color: %q", rendered)
	}
}

func TestBobWorkspaceContextFooterRowsThemeAndPausedFollowAnchor(t *testing.T) {
	m := newTestModel(t)
	setScrollableTranscript(m)
	m.recalcViewportHeight()
	initialHeight := m.viewport.Height()
	m.setTranscriptYOffset(5)
	m.pauseFollow()

	updated, _ := m.Update(BobWorkspaceContextMsg{Generation: 1, Digest: testBobWorkspaceDigest("go-agent-tool", "clean", 0)})
	m = updated.(*Model)
	if got, want := m.viewport.Height(), initialHeight-1; got != want {
		t.Fatalf("viewport height with Bob row = %d, want %d", got, want)
	}
	if got := m.transcriptYOffset(); got != 5 || !m.followPaused() {
		t.Fatalf("Bob row moved paused transcript: offset=%d paused=%v", got, m.followPaused())
	}

	m.beginContinuationTurn("turn-bob-order")
	action := ContinuationActionPresentation{Tool: "bob_playbook"}
	updated, _ = m.Update(ContinuationActionMsg{TurnID: "turn-bob-order", Sequence: 1, Action: &action})
	m = updated.(*Model)
	wantHeight := initialHeight - 1 - lipgloss.Height(m.renderContinuationAction())
	if got := m.viewport.Height(); got != wantHeight {
		t.Fatalf("combined Bob/action footer height = %d, want %d", got, wantHeight)
	}
	view := ansi.Strip(m.View().Content)
	if bobAt, nextAt := strings.Index(view, "bob: ◇"), strings.Index(view, "next: bob_playbook"); bobAt < 0 || nextAt < 0 || bobAt >= nextAt {
		t.Fatalf("Bob row was not immediately before continuation card:\n%s", view)
	}

	card := m.bobWorkspaceContext.card
	_ = card.View()
	var target color.Color = color.Black
	wantDark := true
	if m.isDark {
		target = color.White
		wantDark = false
	}
	updated, _ = m.Update(tea.BackgroundColorMsg{Color: target})
	m = updated.(*Model)
	if m.bobWorkspaceContext.card != card || card.isDark != wantDark {
		t.Fatalf("live theme change did not restyle Bob context child: dark=%v want=%v", card.isDark, wantDark)
	}
}

func TestBobWorkspaceContextClearsAcrossConversationAndSessionRestore(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(BobWorkspaceContextMsg{Generation: 1, Digest: testBobWorkspaceDigest("reset-marker", "clean", 0)})
	m = updated.(*Model)
	withBob := m.viewport.Height()
	m.clearBobWorkspaceContext()
	if got, want := m.viewport.Height(), m.viewportHeight(); got != want || got <= withBob {
		t.Fatalf("authoritative clear retained Bob footer allocation: height=%d want=%d withBob=%d", got, want, withBob)
	}
	updated, _ = m.Update(BobWorkspaceContextMsg{Generation: 2, Digest: testBobWorkspaceDigest("reset-marker", "clean", 0)})
	m = updated.(*Model)
	m.resetConversationSession()
	if m.bobWorkspaceContext.card != nil || m.bobWorkspaceContext.generation != 2 {
		t.Fatal("new conversation retained ephemeral Bob workspace context")
	}
	updated, _ = m.Update(BobWorkspaceContextMsg{Generation: 2, Digest: testBobWorkspaceDigest("stale-reset-marker", "conflicted", 2)})
	m = updated.(*Model)
	if m.bobWorkspaceContext.card != nil {
		t.Fatal("queued pre-reset Bob event repopulated the new conversation")
	}

	raw, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ = m.Update(BobWorkspaceContextMsg{Generation: 3, Digest: testBobWorkspaceDigest("restore-marker", "drifted", 0)})
	m = updated.(*Model)
	if err := m.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	if m.bobWorkspaceContext.card != nil || m.bobWorkspaceContext.generation != 3 {
		t.Fatal("session restore retained ephemeral Bob workspace context")
	}
	updated, _ = m.Update(BobWorkspaceContextMsg{Generation: 3, Digest: testBobWorkspaceDigest("stale-restore-marker", "clean", 0)})
	m = updated.(*Model)
	if m.bobWorkspaceContext.card != nil {
		t.Fatal("queued pre-restore Bob event repopulated the restored session")
	}
}

func testBobWorkspaceDigest(recipeID, state string, conflicts int64) *ecosystem.ReceiptDigest {
	return &ecosystem.ReceiptDigest{
		Kind: ecosystem.DigestBobContext, RecipeID: recipeID, RecipeVersion: 4,
		State: state, ConflictCount: conflicts,
	}
}
