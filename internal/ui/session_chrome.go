package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
)

// sessionHeaderProjection is the Grok-style top chrome: identity bar and an
// optional sticky last-user strip. projectFrame TakeTops this from SafeScreen
// so the transcript viewport shrinks automatically.
type sessionHeaderProjection struct {
	content        string
	reservedHeight int
}

// sessionHeaderActive is true when the Grok-style top chrome claims a row.
// Welcome and idle footer use this to avoid re-printing model/context.
func (m *Model) sessionHeaderActive() bool {
	if m == nil || !m.ready || m.height < 14 || m.width < 36 {
		return false
	}
	return m.chatPaneWidth() >= 30
}

// projectSessionHeader returns top chrome when the frame is tall enough. On
// minimum terminals (30x12) it stays empty so welcome+composer still fit.
func (m *Model) projectSessionHeader() sessionHeaderProjection {
	if !m.sessionHeaderActive() {
		return sessionHeaderProjection{}
	}
	paneW := m.chatPaneWidth()

	var lines []string
	if bar := m.renderSessionTopBar(paneW); bar != "" {
		lines = append(lines, bar)
	}
	// Sticky user: real conversation + room for vertical padding (Grok band).
	if m.stickyUserActive() {
		if sticky := m.renderStickyUserStrip(paneW); sticky != "" {
			// Outer blanks breathe around a single-row sticky. On tall frames
			// the elevated 3-row band already pads itself — stacking outer
			// blanks produced blank-on-blank voids (measured: 4 empty of 6).
			// Keep the outer blanks through ordinary heights (incl. Glyphrun
			// 22-row readers) so one PgUp still leaves latest-follow; only
			// reclaim them once the frame is clearly roomy.
			elevated := stickyUsesElevatedBand(m.height, paneW)
			omitOuter := elevated && m.height >= 28
			if !omitOuter && m.height >= 18 && len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, sticky)
			if !omitOuter && m.height >= 18 {
				lines = append(lines, "")
			}
		}
	}
	if len(lines) == 0 {
		return sessionHeaderProjection{}
	}
	content := strings.Join(lines, "\n")
	return sessionHeaderProjection{
		content: content,
		// Sticky may be multi-line (padded band); measure real paint height.
		reservedHeight: lipgloss.Height(content),
	}
}

// renderSessionTopBar paints: branch · path ........ context meter
// Mode and model live on the bottom shortcuts row — not here.
// Content starts at OriginX so it lines up with welcome, transcript, and status.
func (m *Model) renderSessionTopBar(paneW int) string {
	lead := m.contentGrid().Prefix(" ")
	innerW := max(1, paneW-lipgloss.Width(lead))
	left := m.sessionIdentityLeft(innerW)
	right := m.sessionIdentityRight(innerW)
	// Pack left and right with flexible spaces between.
	gap := innerW - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		// Prefer right-side instrumentation on narrow widths.
		if right != "" {
			return lead + truncateDisplayWithGlyphProfile(right, innerW, m.glyphProfile)
		}
		return lead + truncateDisplayWithGlyphProfile(left, innerW, m.glyphProfile)
	}
	return lead + left + strings.Repeat(" ", gap) + right
}

func (m *Model) sessionIdentityLeft(paneW int) string {
	branch := sessionGitBranchCached(m.workspaceDir())
	path := m.sessionWorkspaceLabel(paneW)
	parts := make([]string, 0, 2)
	if branch != "" && paneW >= 48 {
		parts = append(parts, m.styles.StatusText.Render(branch))
	}
	if path != "" {
		parts = append(parts, m.styles.Dimmed.Render(path))
	}
	if len(parts) == 0 {
		return m.styles.StatusText.Render("sonar")
	}
	return strings.Join(parts, m.styles.Dimmed.Render(glyphSeparator(m.glyphProfile)))
}

// sessionIdentityRight carries the ambient identity this frame assigned to the
// top bar: model (or its remote boundary) and the context meter. Mode stays on
// the bottom row next to the key that changes it.
func (m *Model) sessionIdentityRight(paneW int) string {
	plan := m.planStatus()
	parts := make([]string, 0, 2)

	if m.currentModelIsNonLocal() {
		if plan.owns(factRemoteBoundary, surfaceTopBar) {
			// Compact keeps the boundary token and drops the model name; the
			// full label would crowd out the meter on ordinary widths.
			parts = append(parts, m.styles.StatusWarning.Render(
				m.currentModelReachabilityLabel(paneW < 72),
			))
		}
	} else if plan.owns(factModel, surfaceTopBar) {
		if model := m.currentModelReachabilityLabel(paneW < 58); model != "" {
			parts = append(parts, m.styles.Dimmed.Render(
				truncateDisplayWithGlyphProfile(model, min(24, max(8, paneW/3)), m.glyphProfile),
			))
		}
	}
	if plan.owns(factContext, surfaceTopBar) {
		if ctx := m.renderContextStatus(); ctx != "" {
			parts = append(parts, ctx)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, m.styles.Dimmed.Render(glyphSeparator(m.glyphProfile)))
}

func (m *Model) workspaceDir() string {
	if m != nil && m.agent != nil {
		if dir := strings.TrimSpace(m.agent.WorkDir()); dir != "" {
			return filepath.Clean(dir)
		}
	}
	if dir, err := os.Getwd(); err == nil {
		return filepath.Clean(dir)
	}
	return ""
}

func (m *Model) sessionWorkspaceLabel(paneW int) string {
	dir := m.workspaceDir()
	if dir == "" {
		return ""
	}
	// Top bar stays scannable and host-portable: basename only. Full paths vary
	// by machine (macOS home vs GitHub runner) and post-render normalizers
	// cannot fix padding computed from the longer original path.
	display := filepath.Base(dir)
	// Glyphrun / temp workdirs encode scenario names that still truncate
	// differently across hosts — keep a stable short label for those.
	if strings.Contains(display, "glyphrun") {
		display = "workspace"
	}
	limit := 18
	switch {
	case paneW >= 96:
		limit = 28
	case paneW >= 72:
		limit = 24
	case paneW >= 56:
		limit = 20
	}
	return truncateDisplayWithGlyphProfile(display, limit, m.glyphProfile)
}

// stickyUserActive reports when the sticky last-user strip is painted. The
// transcript omits that same user entry so the prompt is not printed twice.
func (m *Model) stickyUserActive() bool {
	if !m.sessionHeaderActive() || m.height < 16 {
		return false
	}
	return m.latestUserPromptText() != ""
}

// renderStickyUserStrip keeps the latest user prompt visible while the body
// scrolls — Grok Build's "last message stays" band with vertical padding.
//
// Paint as ONE full-width style. Pre-coloring the rail/body and then applying
// Background only to the composite produces a partial "chip" highlight that
// looks broken against the terminal surface.
func (m *Model) renderStickyUserStrip(paneW int) string {
	text := m.latestUserPromptText()
	if text == "" || paneW < 8 {
		return ""
	}
	text = sanitizeTerminalSingleLine(text)
	// Soft single-line summary; multi-line drafts collapse to one sticky row.
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i]) + "…"
	}

	// Plain cells only — styling is applied once to the whole bar.
	rail := glyphSet(m.glyphProfile).UserRail
	if rail == "" {
		rail = "│"
	}
	// Share the content grid's prefix so the rail sits in the same accent
	// column as tool receipts and the transcript's own user gutter. This row
	// previously inset the rail by one, which read as a wobble against the
	// identity bar directly above it.
	// Always paint the full prompt: the transcript omits this entry whenever
	// sticky is active, so progressive reveal would hide the only copy.
	prefix := m.contentGrid().Prefix(rail)
	budget := max(4, paneW-lipgloss.Width(prefix))
	body := truncateDisplayWithGlyphProfile(text, budget, m.glyphProfile)
	plain := prefix + body
	// Pad with spaces so the elevated surface truly spans the pane.
	if gap := paneW - lipgloss.Width(plain); gap > 0 {
		plain += strings.Repeat(" ", gap)
	}

	palette := newSemanticPalette(m.isDark, m.themeID)
	// Subtle elevated band — a surface lifted off the page without a harsh
	// chip. Border is the scheme's own value for exactly that, and it is what
	// inline code uses for the same reason.
	//
	// These were two literal hexes whose own comments named them: "nord snow
	// storm 2" and "nord polar night 1". The palette was already computed on
	// the line above and used for the foreground, so the band painted Nord on
	// nine schemes while the text over it followed the theme correctly.
	elevated := palette.Border
	barStyle := lipgloss.NewStyle().
		Width(paneW).
		MaxWidth(paneW).
		Background(elevated).
		Foreground(palette.Text)

	// Vertical padding: on roomy frames paint a 3-row elevated band
	// (empty / prompt / empty) so the sticky isn't crushed against the
	// identity bar above or the transcript below. Horizontal layout was fine.
	content := barStyle.Render(plain)
	if stickyUsesElevatedBand(m.height, paneW) {
		blank := barStyle.Render(strings.Repeat(" ", paneW))
		content = blank + "\n" + content + "\n" + blank
	}
	return content
}

// stickyUsesElevatedBand is the single gate for the 3-row sticky surface.
// projectSessionHeader consults it so outer blank rows never stack on the
// band's own pad rows.
func stickyUsesElevatedBand(height, paneW int) bool {
	return height >= 20 && paneW >= 40
}

func (m *Model) latestUserPromptText() string {
	if m == nil {
		return ""
	}
	if i := m.latestUserEntryIndex(); i >= 0 {
		return strings.TrimSpace(m.entries[i].Content)
	}
	return ""
}

func (m *Model) latestUserEntryIndex() int {
	if m == nil {
		return -1
	}
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].Kind == "user" {
			return i
		}
	}
	return -1
}

// omitUserEntryFromTranscript skips the latest user block when the sticky
// strip already owns that prompt — one surface, one truth. Multi-line bodies
// and image attachments stay in the transcript because the sticky strip is a
// single-line summary only. Omit as soon as sticky is active: the strip always
// paints the full single-line prompt (no reveal gate that dual-prints).
func (m *Model) omitUserEntryFromTranscript(entryIndex int) bool {
	if !m.stickyUserActive() || entryIndex < 0 || entryIndex >= len(m.entries) {
		return false
	}
	if entryIndex != m.latestUserEntryIndex() {
		return false
	}
	entry := m.entries[entryIndex]
	if len(entry.Attachments) > 0 {
		return false
	}
	if strings.Contains(entry.Content, "\n") {
		return false
	}
	return true
}

// renderShortcutsBar is the fixed bottom product chrome (Grok-style):
//
//	enter send · shift+tab mode · …          ornith:latest · PLAN
//
// Left: key hints. Right: model · mode. One row, no second meta under the
// composer. While a turn is live the activity rail already owns esc stop /
// enter queue, so left keeps only mode.
// shortcutsBarActive reports whether the fixed bottom row is painted this
// frame. planStatus needs this before any surface renders, so the visibility
// rules live here rather than inline in renderShortcutsBar.
func (m *Model) shortcutsBarActive() bool {
	if m == nil || m.chatPaneWidth() < 24 || m.height < 12 {
		return false
	}
	// While a decision surface owns the footer, its own key hints are enough.
	if m.pendingApproval != nil || m.readScopePrompt != nil || m.pendingPaste != nil {
		return false
	}
	if m.overlay == OverlayCompletion || m.overlay == OverlayTranscriptSearch ||
		m.overlay == OverlayPlanForm || m.overlay == OverlayGoalForm ||
		m.cortexDecisionActive() {
		return false
	}
	return true
}

// shortcutsIdentityRightBudget is the width the shortcuts row reserves for its
// right-hand identity, or zero when the row is too narrow to carry any. Keeping
// it beside shortcutsBarActive lets planStatus ask the same question the
// renderer will answer.
func (m *Model) shortcutsIdentityRightBudget() int {
	paneW := m.chatPaneWidth()
	if !m.shortcutsBarActive() || paneW < 48 {
		return 0
	}
	// Enough for "model · PLAN" on 60-column terminals: paneW/4 alone can drop
	// authority once the budget falls under 16.
	return min(40, max(18, paneW/3))
}

// shortcutsIdentityActive reports whether the shortcuts row will paint ambient
// identity this frame.
func (m *Model) shortcutsIdentityActive() bool {
	return m.shortcutsIdentityRightBudget() > 0
}

func (m *Model) renderShortcutsBar(paneW int) string {
	if !m.shortcutsBarActive() {
		return ""
	}

	lead := m.contentGrid().Prefix(" ")
	inner := max(1, paneW-lipgloss.Width(lead))

	// Reserve room for right identity; pack hints into the remainder.
	rightBudget := m.shortcutsIdentityRightBudget()
	leftBudget := max(8, inner-rightBudget-1)

	var hints []keyHint
	if m.state == StateIdle && !m.composerIsBusy() {
		// Idle has nothing to cancel unless voice/queue/mic already owns a
		// gesture. Advertising "esc cancel" here taught a dead verb.
		hints = []keyHint{
			{Key: "enter", Action: "send"},
			{Key: "shift+tab", Action: "mode"},
			{Key: m.keys.Help.Help().Key, Action: "help"},
		}
		if m.idleCancelAffordanceActive() {
			hints = []keyHint{
				{Key: "enter", Action: "send"},
				{Key: "shift+tab", Action: "mode"},
				{Key: "esc", Action: "cancel"},
				{Key: m.keys.Help.Help().Key, Action: "help"},
			}
		}
		// Select mode is sticky chrome, not a 6s footer blink: once capture is
		// off the bar must keep naming the way out.
		if m.mouseCaptureOff {
			hints = append([]keyHint{{Key: "alt+m", Action: "exit select"}}, hints...)
		} else if strings.TrimSpace(m.input.Value()) == "" && m.lastAssistantContent() != "" {
			// Prefer structured copy over the voice invite when there is
			// something to yank and the draft is empty (ctrl+y's precondition).
			withCopy := append(append([]keyHint{}, hints...),
				keyHint{Key: "ctrl+y", Action: "copy"})
			if lipgloss.Width(m.renderKeyHintSet(mergeKeyHintAliases(withCopy), len(withCopy))) <= leftBudget {
				hints = withCopy
			}
		}
		// The voice invite joins only when every hint keeps its label. This
		// row is where first-run discovery happens — the welcome deliberately
		// paints nothing on roomy frames — but renderKeyHints compacts by
		// stripping action words, and a bar that trades "shift+tab mode" for
		// an unlabeled "ctrl+g" taught one thing by unteaching another.
		if !m.mouseCaptureOff {
			withVoice := append(append([]keyHint{}, hints...),
				keyHint{Key: m.keys.VoiceInput.Help().Key, Action: "voice"})
			if lipgloss.Width(m.renderKeyHintSet(mergeKeyHintAliases(withVoice), len(withVoice))) <= leftBudget {
				hints = withVoice
			}
		}
	} else {
		// Live activity rail already surfaces esc stop · enter queue.
		hints = []keyHint{
			{Key: "shift+tab", Action: "mode"},
		}
	}
	left := m.renderKeyHints(leftBudget, hints...)
	right := ""
	if rightBudget > 0 {
		right = m.renderFooterIdentityRight(rightBudget)
	}
	if left == "" && right == "" {
		return ""
	}
	if right == "" {
		return lead + left
	}
	if left == "" {
		gap := max(0, inner-lipgloss.Width(right))
		return lead + strings.Repeat(" ", gap) + right
	}
	gap := inner - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		// Prefer keys when tight.
		return lead + left
	}
	return lead + left + strings.Repeat(" ", gap) + right
}

// idleCancelAffordanceActive reports whether Escape currently cancels something
// while the session is idle. Without that, the shortcuts row must not advertise
// "esc cancel" — a verb with no effect.
func (m *Model) idleCancelAffordanceActive() bool {
	if m == nil {
		return false
	}
	return m.listeningForVoice() || m.queuedFollowUp != nil || m.voiceStageActive()
}

// git branch cache — avoid spawning git every frame.
type gitBranchCache struct {
	mu     sync.Mutex
	dir    string
	branch string
	at     time.Time
}

var sessionGitBranch gitBranchCache

func sessionGitBranchCached(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	sessionGitBranch.mu.Lock()
	defer sessionGitBranch.mu.Unlock()
	if sessionGitBranch.dir == dir && time.Since(sessionGitBranch.at) < 5*time.Second {
		return sessionGitBranch.branch
	}
	branch := ""
	cmd := exec.Command("git", "-C", dir, "branch", "--show-current")
	if out, err := cmd.Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	if branch == "" {
		// Detached HEAD fallback.
		cmd = exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
		if out, err := cmd.Output(); err == nil {
			branch = strings.TrimSpace(string(out))
			if branch == "HEAD" {
				branch = "detached"
			}
		}
	}
	sessionGitBranch.dir = dir
	sessionGitBranch.branch = branch
	sessionGitBranch.at = time.Now()
	return branch
}
