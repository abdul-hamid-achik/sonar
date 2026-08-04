package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

const maxBobWorkspaceRecipeCells = 96

type bobWorkspaceContextStyles struct {
	label lipgloss.Style
	value lipgloss.Style
}

type bobWorkspaceContextPresentation struct {
	recipeID      string
	recipeVersion int64
	state         string
	conflicts     int64
}

type bobWorkspaceContextState struct {
	generation uint64
	card       *bobWorkspaceContextCard
}

func newBobWorkspaceContextStyles(isDark bool, themeID string) bobWorkspaceContextStyles {
	palette := outputSemanticPalette(isDark, themeID)
	return bobWorkspaceContextStyles{
		// Bob describes repository contract state. Accent identifies the source;
		// the value deliberately stays neutral instead of borrowing success or
		// verified-evidence colors.
		label: lipgloss.NewStyle().Foreground(palette.Accent2).Bold(true),
		value: lipgloss.NewStyle().Foreground(palette.Muted),
	}
}

// bobWorkspaceContextCard is a cached, non-interactive Charm child. The smart
// parent owns event freshness, visibility, layout, and transcript anchoring.
type bobWorkspaceContextCard struct {
	context bobWorkspaceContextPresentation
	width   int
	isDark  bool
	themeID string
	styles  bobWorkspaceContextStyles

	cachedKey   string
	cachedWidth int
	cachedView  string
}

func newBobWorkspaceContextCard(context bobWorkspaceContextPresentation, isDark bool, themeID string) *bobWorkspaceContextCard {
	return &bobWorkspaceContextCard{
		context: context,
		isDark:  isDark,
		styles:  newBobWorkspaceContextStyles(isDark, themeID),
	}
}

func (c *bobWorkspaceContextCard) SetWidth(width int) {
	width = max(1, width)
	if c.width == width {
		return
	}
	c.width = width
	c.invalidate()
}

func (c *bobWorkspaceContextCard) SetTheme(isDark bool, themeID string) {
	if c.isDark == isDark {
		return
	}
	c.isDark = isDark
	c.styles = newBobWorkspaceContextStyles(isDark, c.themeID)
	c.invalidate()
}

func (c *bobWorkspaceContextCard) invalidate() {
	c.cachedKey = ""
	c.cachedWidth = 0
	c.cachedView = ""
}

func (c *bobWorkspaceContextCard) View() string {
	if c == nil {
		return ""
	}
	width := max(1, c.width)
	key := fmt.Sprintf("%s\x1e%d\x1e%s\x1e%d", c.context.recipeID, c.context.recipeVersion, c.context.state, c.context.conflicts)
	if c.cachedView != "" && c.cachedKey == key && c.cachedWidth == width {
		return c.cachedView
	}

	const label = "bob:"
	available := max(0, width-lipgloss.Width(label))
	recipe := fmt.Sprintf("%s@%d", c.context.recipeID, c.context.recipeVersion)
	conflicts := bobConflictLabel(c.context.conflicts)
	candidates := []string{
		fmt.Sprintf(" ◇ repo %s · %s · %s", c.context.state, recipe, conflicts),
		fmt.Sprintf(" ◇ %s · %s", c.context.state, recipe),
		fmt.Sprintf(" ◇ %s · %s", c.context.state, conflicts),
		" ◇ " + c.context.state,
		" ◇",
	}
	body := candidates[len(candidates)-1]
	for _, candidate := range candidates {
		if lipgloss.Width(candidate) <= available {
			body = candidate
			break
		}
	}
	body = truncateDisplay(body, available)
	view := c.styles.label.Render(truncateDisplay(label, width))
	if available > 0 {
		view += c.styles.value.Render(body)
	}
	view = strings.TrimRight(view, "\n")
	c.cachedKey = key
	c.cachedWidth = width
	c.cachedView = view
	return view
}

func bobConflictLabel(count int64) string {
	if count == 1 {
		return "1 conflict"
	}
	return fmt.Sprintf("%d conflicts", count)
}

func normalizeBobWorkspaceContext(digest *ecosystem.ReceiptDigest) (bobWorkspaceContextPresentation, bool) {
	if digest == nil || digest.Kind != ecosystem.DigestBobContext || digest.RecipeVersion <= 0 || digest.ConflictCount < 0 {
		return bobWorkspaceContextPresentation{}, false
	}
	recipeID := strings.TrimSpace(truncateDisplay(
		safeToolIdentifier(sanitizeTerminalSingleLine(digest.RecipeID)), maxBobWorkspaceRecipeCells,
	))
	if recipeID == "" {
		return bobWorkspaceContextPresentation{}, false
	}
	state := sanitizeTerminalSingleLine(digest.State)
	switch state {
	case "clean", "drifted":
		if digest.ConflictCount != 0 {
			return bobWorkspaceContextPresentation{}, false
		}
	case "conflicted":
		if digest.ConflictCount == 0 {
			return bobWorkspaceContextPresentation{}, false
		}
	default:
		return bobWorkspaceContextPresentation{}, false
	}
	return bobWorkspaceContextPresentation{
		recipeID: recipeID, recipeVersion: digest.RecipeVersion,
		state: state, conflicts: digest.ConflictCount,
	}, true
}

func (m *Model) handleBobWorkspaceContext(message BobWorkspaceContextMsg) {
	if message.Generation == 0 || message.Generation <= m.bobWorkspaceContext.generation {
		return
	}
	followWasPaused := m.followPaused()
	followYOffset := m.transcriptYOffset()
	m.bobWorkspaceContext = bobWorkspaceContextState{generation: message.Generation}
	if context, ok := normalizeBobWorkspaceContext(message.Digest); ok {
		m.bobWorkspaceContext.card = newBobWorkspaceContextCard(context, m.isDark, m.themeID)
		m.bobWorkspaceContext.card.SetWidth(m.chatPaneWidth())
	}
	if m.ready {
		m.recalcViewportHeight()
		m.refreshTranscript()
		m.restoreFollowPosition(followWasPaused, followYOffset)
	}
}

func (m *Model) clearBobWorkspaceContext() {
	wasVisible := m.bobWorkspaceContextVisible()
	// Keep the generation fence while clearing the ephemeral card. Adapter
	// messages are asynchronous, so an event queued before a session reset can
	// arrive afterward. Resetting the fence to zero would let that stale event
	// repopulate the new/restored session.
	m.bobWorkspaceContext = bobWorkspaceContextState{generation: m.bobWorkspaceContext.generation}
	if wasVisible && m.ready {
		m.recalcViewportHeight()
	}
}

func (m *Model) bobWorkspaceContextVisible() bool {
	if m == nil || m.bobWorkspaceContext.card == nil || m.state != StateIdle || m.initializing || m.shuttingDown ||
		m.overlay != OverlayNone || m.pendingApproval != nil || m.pendingPaste != nil ||
		m.readScopePrompt != nil || m.queuedFollowUp != nil || m.composerIsBusy() {
		return false
	}
	return true
}

func (m *Model) renderBobWorkspaceContext() string {
	if !m.bobWorkspaceContextVisible() {
		return ""
	}
	m.bobWorkspaceContext.card.SetWidth(m.chatPaneWidth())
	return m.bobWorkspaceContext.card.View()
}
