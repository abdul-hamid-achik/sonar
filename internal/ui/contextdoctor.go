package ui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/harmonica"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

// Context doctor: /context opens a transient overlay that visualizes where the
// estimated next-turn prompt goes (system sections, tool schemas, conversation)
// inside the active context window. Bars reveal with a harmonica spring
// cascade; reducedMotion snaps to the final frame and never schedules ticks.
const (
	contextDoctorFPS          = 30.0
	contextDoctorFrame        = time.Second / 30
	contextDoctorFetchTimeout = 5 * time.Second
	// contextDoctorStagger gates each bar until its predecessor has mostly
	// arrived, producing a cascade without per-bar clocks.
	contextDoctorStagger = 0.35

	contextDoctorLabelWidth = 15
	contextDoctorValueWidth = 13
	contextDoctorMinBar     = 8
)

// ContextDoctorMsg carries one measured breakdown back to the open overlay.
type ContextDoctorMsg struct {
	RequestID uint64
	Breakdown agent.ContextBreakdown
}

type contextDoctorTickMsg struct {
	Token uint64
}

// ContextDoctorState is the transient context-usage viewer. It holds
// presentation state only; measurements come from the agent snapshot.
type ContextDoctorState struct {
	Breakdown agent.ContextBreakdown
	Loaded    bool

	spring  harmonica.Spring
	pos     []float64
	vel     []float64
	token   uint64
	pending bool
}

func newContextDoctorState() *ContextDoctorState {
	return &ContextDoctorState{
		spring: harmonica.NewSpring(harmonica.FPS(contextDoctorFPS), 7.0, 0.6),
	}
}

func (s *ContextDoctorState) active() bool {
	for i := range s.pos {
		if !springSettled(s.pos[i], s.vel[i], 1) {
			return true
		}
	}
	return false
}

func (s *ContextDoctorState) step() {
	for i := range s.pos {
		if i > 0 && s.pos[i-1] < contextDoctorStagger {
			continue
		}
		s.pos[i], s.vel[i] = s.spring.Update(s.pos[i], s.vel[i], 1)
		if s.pos[i] < 0 {
			s.pos[i] = 0
		}
		if s.pos[i] > 1 {
			s.pos[i] = 1
		}
	}
}

func (s *ContextDoctorState) snap() {
	for i := range s.pos {
		s.pos[i] = 1
		s.vel[i] = 0
	}
}

func (s *ContextDoctorState) startTick() tea.Cmd {
	if s.pending || !s.active() {
		return nil
	}
	s.token++
	token := s.token
	s.pending = true
	return tea.Tick(contextDoctorFrame, func(time.Time) tea.Msg {
		return contextDoctorTickMsg{Token: token}
	})
}

func (s *ContextDoctorState) reveal(index int) float64 {
	if index < 0 || index >= len(s.pos) {
		return 1
	}
	return s.pos[index]
}

func (m *Model) openContextDoctor() tea.Cmd {
	if m == nil {
		return nil
	}
	if m.agent == nil {
		return m.setFooterNotice(noticeError, "Context inspection needs an active agent", 4*time.Second)
	}
	m.contextDoctorState = newContextDoctorState()
	m.overlay = OverlayContextDoctor
	m.input.Blur()
	m.recalcViewportHeight()
	m.contextDoctorRequest++
	requestID := m.contextDoctorRequest
	agentRef := m.agent
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), contextDoctorFetchTimeout)
		defer cancel()
		return ContextDoctorMsg{RequestID: requestID, Breakdown: agentRef.ContextBreakdown(ctx)}
	}
}

func (m *Model) closeContextDoctor() {
	m.contextDoctorState = nil
	m.closeOverlayToParent()
}

func (m *Model) updateContextDoctorMessage(msg tea.Msg) tea.Cmd {
	state := m.contextDoctorState
	if state == nil {
		return nil
	}
	switch msg := msg.(type) {
	case ContextDoctorMsg:
		if msg.RequestID != m.contextDoctorRequest {
			return nil
		}
		state.Breakdown = msg.Breakdown
		state.Loaded = true
		state.pos = make([]float64, len(msg.Breakdown.Sections))
		state.vel = make([]float64, len(msg.Breakdown.Sections))
		if m.reducedMotion {
			state.snap()
			return nil
		}
		return state.startTick()
	case contextDoctorTickMsg:
		if !state.pending || msg.Token != state.token {
			return nil
		}
		state.pending = false
		if m.reducedMotion {
			state.snap()
			return nil
		}
		state.step()
		if state.active() {
			return state.startTick()
		}
		state.snap()
		return nil
	}
	return nil
}

func (m *Model) renderContextDoctor() string {
	state := m.contextDoctorState
	if state == nil {
		return ""
	}
	width := pickerContentWidth(m.width)
	palette := outputSemanticPalette(m.isDark, m.themeID)
	titleStyle := lipgloss.NewStyle().Foreground(palette.Text).Bold(true)
	mutedStyle := lipgloss.NewStyle().Foreground(palette.Muted)

	var b strings.Builder
	title := "Context"
	if state.Loaded && state.Breakdown.Model != "" {
		title += " · " + state.Breakdown.Model
	}
	b.WriteString(titleStyle.Render(truncateDisplay(sanitizeTerminalSingleLine(title), width)))
	b.WriteString("\n")

	if !state.Loaded {
		b.WriteString(mutedStyle.Render("Measuring the next-turn prompt…"))
		return m.renderPickerFrame(b.String(), m.contextDoctorFooter())
	}

	breakdown := state.Breakdown
	b.WriteString(mutedStyle.Render(truncateDisplay(fmt.Sprintf(
		"window %s tokens · estimated next-turn prompt", formatTokens(breakdown.NumCtx)), width)))
	b.WriteString("\n\n")
	b.WriteString(m.renderContextDoctorBars(state, width, palette))
	b.WriteString("\n")
	b.WriteString(m.renderContextDoctorSummary(breakdown, width, palette))
	return m.renderPickerFrame(b.String(), m.contextDoctorFooter())
}

func (m *Model) renderContextDoctorBars(state *ContextDoctorState, width int, palette semanticPalette) string {
	breakdown := state.Breakdown
	barWidth := width - contextDoctorLabelWidth - contextDoctorValueWidth
	if barWidth < contextDoctorMinBar {
		barWidth = contextDoctorMinBar
	}
	fullGlyph, emptyGlyph := "█", "░"
	if resolveGlyphProfile(m.glyphProfile) == GlyphASCII {
		fullGlyph, emptyGlyph = "#", "."
	}

	maxTokens := 1
	for _, section := range breakdown.Sections {
		if section.Tokens > maxTokens {
			maxTokens = section.Tokens
		}
	}

	labelStyle := lipgloss.NewStyle().Foreground(palette.Muted)
	valueStyle := lipgloss.NewStyle().Foreground(palette.Text)
	emptyStyle := lipgloss.NewStyle().Foreground(palette.Dim)

	var b strings.Builder
	var details []string
	for i, section := range breakdown.Sections {
		fillStyle := lipgloss.NewStyle().Foreground(palette.Accent)
		if section.Key == "conversation" {
			fillStyle = lipgloss.NewStyle().Foreground(palette.Accent2)
		}
		fraction := float64(section.Tokens) / float64(maxTokens)
		filled := int(math.Round(fraction * state.reveal(i) * float64(barWidth)))
		if section.Tokens > 0 && filled < 1 {
			filled = 1
		}
		if filled > barWidth {
			filled = barWidth
		}
		percent := 0
		if breakdown.NumCtx > 0 {
			percent = section.Tokens * 100 / breakdown.NumCtx
		}
		label := truncateDisplay(section.Label, contextDoctorLabelWidth-1)
		b.WriteString(labelStyle.Render(fmt.Sprintf("%-*s", contextDoctorLabelWidth, label)))
		b.WriteString(fillStyle.Render(strings.Repeat(fullGlyph, filled)))
		b.WriteString(emptyStyle.Render(strings.Repeat(emptyGlyph, barWidth-filled)))
		b.WriteString(valueStyle.Render(fmt.Sprintf(" %6s · %2d%%", formatTokens(section.Tokens), percent)))
		b.WriteString("\n")
		if section.Detail != "" {
			details = append(details, section.Label+": "+section.Detail)
		}
	}
	if len(details) > 0 {
		b.WriteString(labelStyle.Render(truncateDisplay(strings.Join(details, " — "), width)))
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Model) renderContextDoctorSummary(breakdown agent.ContextBreakdown, width int, palette semanticPalette) string {
	dividerGlyph := "─"
	if resolveGlyphProfile(m.glyphProfile) == GlyphASCII {
		dividerGlyph = "-"
	}
	divider := lipgloss.NewStyle().Foreground(palette.Border).Render(strings.Repeat(dividerGlyph, width))

	percent := 0
	if breakdown.NumCtx > 0 {
		percent = breakdown.TotalTokens * 100 / breakdown.NumCtx
		if percent > 100 {
			percent = 100
		}
	}
	// Reuse the footer meter's occupancy thresholds so both surfaces agree on
	// when the window is getting tight.
	totalStyle := m.styles.ContextPctLow
	if percent >= 85 {
		totalStyle = m.styles.ContextPctHigh
	} else if percent >= 65 {
		totalStyle = m.styles.ContextPctMid
	}
	total := totalStyle.Render(fmt.Sprintf(
		"total %s/%s · %d%%", formatTokens(breakdown.TotalTokens), formatTokens(breakdown.NumCtx), percent))

	free := breakdown.NumCtx - breakdown.TotalTokens
	if free < 0 {
		free = 0
	}
	freeNote := fmt.Sprintf("free %s · compaction at 75%%", formatTokens(free))
	freeStyle := lipgloss.NewStyle().Foreground(palette.Muted)
	if percent >= 75 {
		freeNote = "over the 75% threshold · the next turn compacts first"
		freeStyle = lipgloss.NewStyle().Foreground(palette.Warning)
	}
	return divider + "\n" + total + "\n" + freeStyle.Render(truncateDisplay(freeNote, width))
}

func (m *Model) contextDoctorFooter() string {
	width := pickerListWidth(m.width)
	return m.renderKeyHints(width,
		keyHint{Key: m.keys.Cancel.Help().Key, Action: m.overlayCloseLabel()},
	)
}
