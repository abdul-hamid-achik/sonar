package ui

import "charm.land/lipgloss/v2"

// Sonar pulse: the emit half of the harness's own metaphor.
//
// sonar_ping.go already owns the listening half. While a request is
// outstanding, StateWaiting is literally a ping awaiting its echo, and the
// trace there encodes a fact — how far this wait has travelled against this
// model's own typical first response. Every other active phase (a tool
// running, a stream arriving, a session restoring) showed a generic braille
// dot instead: correct, and belonging to no particular program.
//
// This is the one place in the activity vocabulary that is identity rather
// than information, and saying so is the point. The pulse carries no fact the
// row does not already state one cell away — elapsed time is right there, and
// the informative trace still owns the waiting phase where a fact exists to
// show. What it does carry is that this is sonar working, which is worth a
// cell in the same way a name is worth a word.
//
// Discipline, unchanged from the ping:
//
//   - No new clock. The frame advances on the spinner tick the activity phase
//     already owns, so nothing here can drift from or outlive that phase.
//   - One terminal cell per frame, drawn from the glyph vocabulary the rest of
//     the UI already ships, so adopting it cannot change layout geometry.
//   - Shape carries the pulse and color only reinforces it, so NO_COLOR and
//     monochrome terminals read the same three beats.
//   - Reduced motion renders nothing and the counter never advances; the
//     ASCII profile keeps its own spinner rather than importing glyphs the
//     profile exists to avoid.
//
// Three beats, not four. A fourth needed a shape the vocabulary does not have,
// so it repeated the ring at a dimmer color — which reads as a stutter on any
// monochrome terminal, exactly where shape is supposed to be carrying the
// animation on its own. Every beat below is a distinct glyph.
const sonarPulseFrames = 3

// sonarPulseGlyph is the current frame, or "" when this terminal or this
// moment should keep the existing motion.
//
// The empty answer matters: callers substitute it for the Bubbles spinner, and
// returning a frame under reduced motion or the ASCII profile would reintroduce
// exactly what those settings exist to remove.
func (m *Model) sonarPulseGlyph() string {
	if m == nil || m.reducedMotion || m.glyphProfile == GlyphASCII {
		return ""
	}
	glyphs := glyphSet(m.glyphProfile)
	palette := outputSemanticPalette(m.isDark, m.themeID)

	// Emit, expand, fade. The shape sequence is the animation; the colors ride
	// along so the same four beats read on a monochrome terminal.
	//
	// The fade deliberately does not end in a blank cell. A gap would read as
	// sonar's own silence between pings, which is truer to the metaphor and
	// false to the state: nothing has stopped, and an empty accent cell in a
	// row that is otherwise reporting progress reads as a stall.
	switch m.sonarPulseFrame % sonarPulseFrames {
	case 0:
		return lipgloss.NewStyle().Foreground(palette.Accent).Render(glyphs.Selected)
	case 1:
		return lipgloss.NewStyle().Foreground(palette.Accent2).Render(glyphs.Unselected)
	default:
		return lipgloss.NewStyle().Foreground(palette.Dim).Render(glyphSeparatorDot(m.glyphProfile))
	}
}

// advanceSonarPulse moves the pulse one beat. It rides the caller's tick and
// owns no clock of its own.
func (m *Model) advanceSonarPulse() {
	if m == nil || m.reducedMotion {
		return
	}
	m.sonarPulseFrame++
}

// glyphSeparatorDot is the middle dot without its surrounding spaces. The rail
// already uses it between segments, so the faded beat is a glyph this terminal
// is known to render at one cell.
func glyphSeparatorDot(profile GlyphProfile) string {
	if resolveGlyphProfile(profile) == GlyphASCII {
		return "."
	}
	return "·"
}
