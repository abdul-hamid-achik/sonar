package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestZZReproContextMeterDuplication(t *testing.T) {
	for _, pct := range []int{10, 90} {
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		m = updated.(*Model)
		m.model = "ornith:latest"
		m.numCtx = 1000
		m.promptTokens = pct * 10
		m.entries = []ChatEntry{{Kind: "user", Content: "hey"}}
		m.recalcViewportHeight()
		m.refreshTranscript()

		meter := ansi.Strip(m.renderContextStatus())
		view := ansi.Strip(m.View().Content)
		goal := ansi.Strip(m.renderGoalFooterStatus(GoalSummary{
			Objective: "ship it", Phase: GoalPhaseActive, TurnsUsed: 1, TurnBudget: 5,
		}, m.chatPaneWidth()))
		t.Logf("pct=%d meter=%q viewCount=%d owner=%d", pct, meter, strings.Count(view, meter), m.planStatus().ownedBy(factContext))
		t.Logf("  topbar=%q", ansi.Strip(m.renderSessionTopBar(m.chatPaneWidth())))
		t.Logf("  status=%q", ansi.Strip(m.renderStatusLine()))
		t.Logf("  goalfooter=%q (contains meter=%v)", goal, strings.Contains(goal, meter))
	}
}
