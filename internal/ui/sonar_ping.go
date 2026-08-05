package ui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Sonar wait trace: the harness is named sonar, and StateWaiting is literally
// a ping awaiting its echo — the request left the machine (send_turn) and
// nothing has come back yet (the first stream chunk or tool call flips the
// state to streaming, exactly once per segment).
//
// While waiting, the activity rail's motion cells used to show random braille
// shimmer. The trace replaces that noise with a fact the user otherwise has to
// compute from the elapsed counter and memory: how far the current wait has
// progressed relative to this model's typical first-response latency. A ping
// head travels a fixed track; the expected-echo marker sits at the midpoint
// (= the typical latency); a head parked at the right edge means the echo is
// late, and it adopts the Warning role at twice the typical wait.
//
// Discipline: this file adds no clocks. Observation rides the per-update
// maybeKickChromeSpring tail hook and never schedules ticks; rendering rides
// the waiting phase's existing scramble tick, which remains that phase's one
// clock owner. Under reducedMotion the trace renders nothing, leaving the
// static ellipsis frame exactly as it is today, and the same fact remains
// readable without motion in the /runtime Echo row.
const (
	// sonarTraceCells is the fixed trace width. It matches the six motion
	// cells the waiting scramble already owns at the wide tier, so adopting
	// the trace can never change layout geometry.
	sonarTraceCells = 6
	// sonarPingBaselineFloor bounds the render timeline. A sub-250ms baseline
	// would saturate the track instantly and flag Warning on ordinary jitter;
	// the floor only widens the displayed timeline — the recorded EMA keeps
	// the true value.
	sonarPingBaselineFloor = 250 * time.Millisecond
)

// sonarPingState tracks one in-flight ping and the per-model first-response
// baseline. It lives inside chromeSpringState because Model already owns that
// struct and its values are presentation-only; no Model field or Update case
// is added for it.
type sonarPingState struct {
	// inWait/waitStart describe the current outstanding ping, stamped when
	// the model enters StateWaiting.
	inWait    bool
	waitStart time.Time

	// baseline is an EMA (newest quarter weight) of completed waits; last is
	// the most recent sample. Both reset when the model identity changes —
	// a different model answers with a different latency profile.
	baseline  time.Duration
	last      time.Duration
	samples   int
	lastModel string
}

// observeSonarPing watches state transitions from the per-update chrome
// spring hook. It only records; it never schedules ticks, in any mode.
func (m *Model) observeSonarPing() {
	if m == nil {
		return
	}
	s := &m.chromeSpring.ping
	if m.model != s.lastModel {
		// A stale baseline from another model would misplace the
		// expected-echo marker for every wait until it converged.
		s.lastModel = m.model
		s.baseline = 0
		s.last = 0
		s.samples = 0
	}
	waiting := m.state == StateWaiting
	switch {
	case waiting && !s.inWait:
		s.inWait = true
		s.waitStart = m.nowTime()
	case !waiting && s.inWait:
		s.inWait = false
		if m.state != StateStreaming || s.waitStart.IsZero() {
			// A wait that ended without an echo (cancel, error, shutdown)
			// is not a latency sample; recording it would poison the
			// baseline with the user's patience instead of the provider's.
			return
		}
		sample := m.nowTime().Sub(s.waitStart)
		if sample <= 0 {
			return
		}
		s.last = sample
		if s.samples == 0 {
			s.baseline = sample
		} else {
			s.baseline = (s.baseline*3 + sample) / 4
		}
		s.samples++
	}
}

// sonarTraceHead maps an elapsed wait onto the trace. The expected-echo
// marker sits at cells/2, so elapsed==baseline puts the head exactly on the
// marker; the head pins at the last cell once the echo is late and overdue
// reports the ≥2× threshold where the head adopts the Warning role.
func sonarTraceHead(elapsed, baseline time.Duration, cells int) (head int, overdue bool) {
	if baseline < sonarPingBaselineFloor {
		baseline = sonarPingBaselineFloor
	}
	if cells < 2 || elapsed <= 0 {
		return 0, false
	}
	marker := cells / 2
	head = int(float64(elapsed) / float64(baseline) * float64(marker))
	if head >= cells {
		head = cells - 1
	}
	return head, elapsed >= 2*baseline
}

// sonarWaitTrace renders the waiting-phase trace, or "" whenever it has
// nothing honest to say: no completed echo yet this model (first wait keeps
// the existing shimmer), the narrow one-cell tier, the ASCII profile (whose
// waiting phase owns the spinner), or reducedMotion (whose correct static
// frame is the existing ellipsis — a frozen mid-track head would read as a
// live measurement that never updates).
func (m *Model) sonarWaitTrace(cells int) string {
	if m == nil || m.reducedMotion || m.glyphProfile == GlyphASCII ||
		cells < sonarTraceCells || m.state != StateWaiting {
		return ""
	}
	s := m.chromeSpring.ping
	if s.samples == 0 || !s.inWait || s.waitStart.IsZero() {
		return ""
	}
	head, overdue := sonarTraceHead(m.nowTime().Sub(s.waitStart), s.baseline, sonarTraceCells)

	palette := outputSemanticPalette(m.isDark, m.themeID)
	trackStyle := lipgloss.NewStyle().Foreground(palette.Dim)
	markerStyle := lipgloss.NewStyle().Foreground(palette.Border)
	headStyle := lipgloss.NewStyle().Foreground(palette.Accent)
	if overdue {
		headStyle = lipgloss.NewStyle().Foreground(palette.Warning)
	}

	// Position carries the fact; color only reinforces it, so NO_COLOR and
	// monochrome terminals still read the trace correctly. Every glyph is one
	// cell wide: the head and marker reuse the shared vocabulary, and the
	// track reuses the middle dot the rail already uses as its separator.
	glyphs := glyphSet(m.glyphProfile)
	marker := sonarTraceCells / 2
	var b strings.Builder
	for cell := 0; cell < sonarTraceCells; cell++ {
		switch {
		case cell == head:
			b.WriteString(headStyle.Render(glyphs.Selected))
		case cell == marker:
			b.WriteString(markerStyle.Render(glyphs.Vertical))
		default:
			b.WriteString(trackStyle.Render("·"))
		}
	}
	return b.String()
}
