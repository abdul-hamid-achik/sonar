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
//   - One terminal cell per frame, verified by test, so adopting it cannot
//     change layout geometry.
//   - Shape carries the pulse and color only reinforces it, so NO_COLOR and
//     monochrome terminals read the same three beats.
//   - Reduced motion renders nothing and the counter never advances; the
//     ASCII profile keeps its own spinner rather than importing glyphs the
//     profile exists to avoid.
//
// Three beats, not four. A fourth needed a shape the set does not have, so it
// repeated the ring at a dimmer color — which reads as a stutter on any
// monochrome terminal, exactly where shape is supposed to be carrying the
// animation on its own. Every beat below is a distinct glyph.
const sonarPulseFrames = 3

// sonarPulseTickDivider slows the pulse to roughly one ping per second.
//
// The frames ride the activity spinner's clock, and that clock runs at the rate
// a spinner wants — MiniDot is ~12fps, which put a full three-beat cycle at
// four pings a second. Nothing pings four times a second. It read as a
// vibration rather than as sonar, which is the whole thing the glyphs are for.
const sonarPulseTickDivider = 4

// sonarPulseBeats are the emit, expand and fade of one ping.
//
// They are bullets rather than the geometric circles the semantic glyph set
// ships (● U+25CF, ○ U+25CB). Both families measure one cell, so this is not a
// layout question — it is an optical one. Geometric shapes are drawn large and
// centered on their own box, so inline beside text they sit high and read as
// oversized; the bullet family is designed to share a baseline and an x-height
// with the words around it. The first version used the geometric pair and
// looked, accurately, like something pasted into the row.
var sonarPulseBeats = [sonarPulseFrames]string{"•", "◦", "·"}

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
	palette := outputSemanticPalette(m.isDark, m.themeID)

	// Emit, expand, fade. The shape sequence is the animation; the colors ride
	// along so the same four beats read on a monochrome terminal.
	//
	// The fade deliberately does not end in a blank cell. A gap would read as
	// sonar's own silence between pings, which is truer to the metaphor and
	// false to the state: nothing has stopped, and an empty accent cell in a
	// row that is otherwise reporting progress reads as a stall.
	beat := m.sonarPulseFrame % sonarPulseFrames
	color := palette.Dim
	switch beat {
	case 0:
		color = palette.Accent
	case 1:
		color = palette.Accent2
	}
	return lipgloss.NewStyle().Foreground(color).Render(sonarPulseBeats[beat])
}

// advanceSonarPulse moves the pulse and reports whether the BEAT changed. It
// rides the caller's tick and owns no clock of its own.
//
// The answer matters as much as the movement. Dividing the tick means three of
// every four now leave the glyph exactly where it was, and a caller that
// repaints on each one produces three byte-identical frames per ping — which is
// precisely the waste the transcript's own perf contract exists to prevent.
func (m *Model) advanceSonarPulse() bool {
	if m == nil || m.reducedMotion {
		return false
	}
	m.sonarPulseTick++
	if m.sonarPulseTick%sonarPulseTickDivider != 0 {
		return false
	}
	m.sonarPulseFrame++
	return true
}

// sonarPulseOwnsMotion reports whether the pulse, rather than the Bubbles
// spinner, is what the activity surfaces are currently painting. The spinner
// advances on every tick; the pulse does not, and a caller deciding whether to
// repaint has to know which one it is following.
func (m *Model) sonarPulseOwnsMotion() bool {
	return m.sonarPulseGlyph() != ""
}
