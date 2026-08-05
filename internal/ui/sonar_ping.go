package ui

import (
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/harmonica"
)

// Sonar wait trace: the harness is named sonar, and StateWaiting is literally
// a ping awaiting its echo — the request left the machine and nothing has come
// back yet (the first stream chunk or tool call flips the state to streaming,
// exactly once per segment).
//
// While waiting, the activity rail's motion cells used to show random braille
// shimmer. The trace replaces that noise with a fact the user otherwise has to
// compute from the elapsed counter and memory: how far the current wait has
// progressed relative to this model's typical first-response latency. A ping
// head travels a fixed track; the expected-echo marker sits at the midpoint.
//
// The comparison is per model and deliberately so. A fast small model and a
// slow large one are both healthy; only a model measured against itself says
// anything.
//
// Discipline: this file adds no clocks. Observation rides the per-update
// maybeKickChromeSpring tail hook; the spring rides the waiting phase's
// existing scramble tick, which remains that phase's one clock owner. Under
// reducedMotion nothing renders and the spring never advances, leaving the
// static ellipsis frame exactly as it is, and the same fact stays readable
// without motion in the /runtime Echo row.
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
	// sonarPingFPS is the rate of the clock that advances the spring: the
	// waiting phase's scramble tick. harmonica precomputes its coefficients
	// from this, so a value that does not match the real tick makes the
	// animation resolve too fast or too slow.
	sonarPingFPS = 15
)

// sonarPingState tracks one in-flight wait and the per-model first-response
// baseline. It lives inside chromeSpringState because Model already owns that
// struct and its values are presentation-only; no Model field or Update case
// is added for it.
type sonarPingState struct {
	// inWait/waitStart describe the current outstanding request, stamped when
	// the model enters StateWaiting.
	inWait    bool
	waitStart time.Time

	// baseline is an EMA (newest quarter weight) of completed waits; last is
	// the most recent sample. Both reset when the model identity changes —
	// a different model answers with a different latency profile, and on a
	// local host a different weight class entirely.
	baseline  time.Duration
	last      time.Duration
	samples   int
	lastModel string

	// headPos/headVel are the animated head position in cells, driven by a
	// harmonica spring toward the position elapsed time says it should hold.
	//
	// The arithmetic alone would place the head correctly; it would also make
	// it teleport between six integer cells, which reads as a counter rather
	// than as something moving. The spring gives it momentum: it eases in,
	// glides, and settles onto the expected-echo marker with a small
	// overshoot. That overshoot is the point — it is the frame where a wait
	// crosses from "normal" to "late", and a hard stop hides the moment.
	headPos, headVel float64
	spring           harmonica.Spring
	springReady      bool
}

// newSonarPingSpring is under-damped on purpose. A damping ratio below 1
// overshoots and oscillates as the amplitude decays; at 0.62 the head passes
// the marker by a fraction of a cell and settles back, which is what makes
// arrival legible. Critical damping (1.0) would be smoother and say less.
//
// The frame rate must match the clock that actually advances it — the waiting
// phase's scramble tick at ~15 FPS — or the motion resolves at the wrong speed.
func newSonarPingSpring() harmonica.Spring {
	return harmonica.NewSpring(harmonica.FPS(sonarPingFPS), 7.0, 0.62)
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
		// expected-echo marker for every wait until it converged, and
		// between two local models of different sizes it would not converge
		// to anything meaningful at all.
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
		// Each ping starts at the left edge with no momentum. Carrying the
		// previous wait's velocity would fling the head off on the next turn.
		s.headPos, s.headVel = 0, 0
	case !waiting && s.inWait:
		s.inWait = false
		if m.state != StateStreaming || s.waitStart.IsZero() {
			// A wait that ended without a reply (cancel, error, shutdown)
			// is not a latency sample; recording it would poison the
			// baseline with the user's patience instead of the model's
			// response time.
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

// advanceSonarPing steps the spring one frame toward where elapsed time says
// the head belongs. It is called from the waiting phase's scramble tick, which
// is that phase's one clock owner — this adds no clock of its own.
//
// Under reducedMotion it does nothing and the head stays where it is, because
// nothing renders it there anyway.
func (m *Model) advanceSonarPing() {
	if m == nil || m.reducedMotion {
		return
	}
	s := &m.chromeSpring.ping
	if !s.inWait || s.samples == 0 || s.waitStart.IsZero() {
		return
	}
	if !s.springReady {
		s.spring = newSonarPingSpring()
		s.springReady = true
	}
	target := sonarTraceTarget(m.nowTime().Sub(s.waitStart), s.baseline, sonarTraceCells)
	s.headPos, s.headVel = s.spring.Update(s.headPos, s.headVel, target)
}

// sonarTraceTarget is where elapsed time says the head belongs, in fractional
// cells. The expected-echo marker sits at cells/2, so elapsed == baseline
// targets the marker exactly. The spring chases this; it is not rendered
// directly.
func sonarTraceTarget(elapsed, baseline time.Duration, cells int) float64 {
	if baseline < sonarPingBaselineFloor {
		baseline = sonarPingBaselineFloor
	}
	if cells < 2 || elapsed <= 0 {
		return 0
	}
	marker := float64(cells / 2)
	target := float64(elapsed) / float64(baseline) * marker
	if limit := float64(cells - 1); target > limit {
		// Pinned at the last cell. The spring still approaches it, so a very
		// late reply parks rather than accelerating off the end.
		return limit
	}
	return target
}

// sonarTraceTrail reports the cell the head is leaving and how strongly to
// paint it, from the head's fractional position.
//
// weight is 0 when the head sits on a cell centre and rises toward 1 as it
// crosses between cells. Six cells is too coarse to show travel by moving a
// glyph alone; lighting the vacated cell in proportion turns two dots into one
// object in motion. It returns ok=false when there is nothing to trail —
// on-centre, or already at an edge.
func sonarTraceTrail(pos float64, head int) (cell int, weight float64) {
	offset := pos - float64(head)
	if offset > -0.15 && offset < 0.15 {
		return -1, 0
	}
	// The fraction says which pair of cells the head sits between, which is
	// direction-agnostic: a negative offset means it is short of the rendered
	// cell, so the smear belongs on the neighbour to the left, and vice versa.
	if offset < 0 {
		cell = head - 1
	} else {
		cell = head + 1
	}
	if cell < 0 || cell >= sonarTraceCells {
		return -1, 0
	}
	weight = math.Abs(offset)
	if weight > 1 {
		weight = 1
	}
	return cell, weight
}

// sonarTraceHead maps an elapsed wait onto the trace. The expected-reply
// marker sits at cells/2, so elapsed==baseline puts the head exactly on the
// marker; the head pins at the last cell once the reply is late and overdue
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
// nothing honest to say: no completed reply yet for this model (the first
// wait keeps the existing shimmer), the narrow one-cell tier, the ASCII
// profile (whose waiting phase owns the spinner), or reducedMotion (whose
// correct static frame is the existing ellipsis — a frozen mid-track head
// would read as a live measurement that never updates).
func (m *Model) sonarWaitTrace(cells int) string {
	if m == nil || m.reducedMotion || m.glyphProfile == GlyphASCII ||
		cells < sonarTraceCells || m.state != StateWaiting {
		return ""
	}
	s := m.chromeSpring.ping
	if s.samples == 0 || !s.inWait || s.waitStart.IsZero() {
		return ""
	}
	_, overdue := sonarTraceHead(m.nowTime().Sub(s.waitStart), s.baseline, sonarTraceCells)
	// The spring's position, not the arithmetic one. They agree at rest and
	// differ exactly while the head is moving, which is the animation.
	head := int(math.Round(s.headPos))
	head = max(0, min(head, sonarTraceCells-1))
	// Sub-cell position drives a trailing echo. Six cells cannot show smooth
	// travel by glyph alone, so the fraction between cells is rendered as
	// brightness on the cell the head is leaving: the eye reads the pair as
	// one thing in motion rather than a dot that jumped.
	trail, trailWeight := sonarTraceTrail(s.headPos, head)

	palette := outputSemanticPalette(m.isDark, m.themeID)
	trackStyle := lipgloss.NewStyle().Foreground(palette.Dim)
	markerStyle := lipgloss.NewStyle().Foreground(palette.Border)
	headStyle := lipgloss.NewStyle().Foreground(palette.Accent)
	trailStyle := lipgloss.NewStyle().Foreground(palette.Muted)
	if overdue {
		headStyle = lipgloss.NewStyle().Foreground(palette.Warning)
		trailStyle = lipgloss.NewStyle().Foreground(palette.Accent2)
	}

	// Position carries the fact; color only reinforces it, so NO_COLOR and
	// monochrome terminals still read the trace correctly. Every glyph is one
	// cell wide: the head and marker reuse the shared vocabulary, and the
	// track reuses the middle dot the rail already uses as its separator.
	glyphs := glyphSet(m.glyphProfile)
	marker := sonarTraceCells / 2
	var b strings.Builder
	for cell := range sonarTraceCells {
		switch {
		case cell == head:
			b.WriteString(headStyle.Render(glyphs.Selected))
		case cell == trail && trailWeight > 0:
			// The vacated cell, held briefly. Position still carries the fact
			// on its own, so a NO_COLOR terminal loses only the smoothing.
			b.WriteString(trailStyle.Render(glyphs.Selected))
		case cell == marker:
			b.WriteString(markerStyle.Render(glyphs.Vertical))
		default:
			b.WriteString(trackStyle.Render("·"))
		}
	}
	return b.String()
}
