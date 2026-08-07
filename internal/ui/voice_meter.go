package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// The listening meter: proof that the microphone is hearing you.
//
// The animation this replaces was a three-glyph pulse on a timer. It moved
// whether or not anything was being recorded, which meant the screen looked
// identical for an open microphone, a muted one, an input set to the wrong
// device, and a permission never granted. Somebody talking into that has no way
// to tell — and "I talk and talk and nothing happens" is exactly what a timer
// animation is designed to produce.
//
// So this one is driven by the input level ffmpeg reports, and it is a rolling
// history rather than a single bar: one bar says "loud now", a row of them
// shows a sentence being spoken. That shape is legible at a glance in a way a
// bouncing single value is not.
//
// When the level stays on the floor long enough to be sure, the rail stops
// animating and says so in words. A meter that is honestly flat is better than
// a pulse that is dishonestly alive, but neither is as useful as a sentence
// naming the likely cause.

// voiceMeterWidth is how many readings the meter shows.
//
// Roughly two seconds at the activity tick rate: long enough to hold a spoken
// phrase, short enough to fit beside a label without taking the rail over.
const voiceMeterWidth = 16

// voiceMeterRamp maps a level to a glyph, quietest first.
//
// Block elements rather than the pulse's dots, because a level is a QUANTITY
// and these are the one glyph family in the set that reads as one — height
// carries the value with no colour and no legend. The pulse keeps its dots; the
// two animations mean different things and should not share a vocabulary.
var voiceMeterRamp = []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇"}

// voiceMeterASCII is the same shape for terminals that cannot draw the blocks.
// It is coarser on purpose: five levels that certainly render beat seven that
// may arrive as replacement boxes.
var voiceMeterASCII = []string{".", ",", "-", "=", "#"}

// sampleVoiceLevel records one reading and reports whether a microphone is open.
//
// Called from the pulse's tick because that is the clock the rail already runs
// on, and because sampling has to continue at the same rate the meter is drawn:
// a history sampled more slowly than it scrolls shows a shape that never
// happened.
func (m *Model) sampleVoiceLevel() bool {
	if m == nil || m.voiceInput == nil || !m.listeningForVoice() {
		return false
	}
	level := m.voiceInput.listener.Level()
	m.voiceInput.levels = append(m.voiceInput.levels, level)
	if len(m.voiceInput.levels) > voiceMeterWidth {
		m.voiceInput.levels = m.voiceInput.levels[len(m.voiceInput.levels)-voiceMeterWidth:]
	}
	return true
}

// voiceMeter renders the rolling level history, or "" when there is nothing to
// draw.
//
// Empty under reduced motion, which is the honest answer rather than a static
// bar: the value of this surface is entirely in its movement, and a frozen
// meter would claim a reading it is not updating. The words in the rail still
// report an open microphone, and the silence warning still fires.
func (m *Model) voiceMeter(cells int) string {
	if m == nil || !m.listeningForVoice() {
		return ""
	}
	return m.voiceMeterFromHistory(cells)
}

// voiceMeterFromHistory draws whatever history is held, without asking whether
// a microphone is open. Split out so the rendering can be exercised without a
// recorder: the shape of the meter is a property, and a permission grant only a
// human can give is not a reason to leave it unpinned.
func (m *Model) voiceMeterFromHistory(cells int) string {
	if m == nil || m.voiceInput == nil || m.reducedMotion {
		return ""
	}
	if cells <= 0 {
		return ""
	}
	if cells > voiceMeterWidth {
		cells = voiceMeterWidth
	}
	ramp := voiceMeterRamp
	if m.glyphProfile == GlyphASCII {
		ramp = voiceMeterASCII
	}
	palette := outputSemanticPalette(m.isDark, m.themeID)
	quiet := lipgloss.NewStyle().Foreground(palette.Dim)
	// Warning is the role an open microphone already wears in the pulse. Keeping
	// it means the meter and the glyph beside it are one state in two readings,
	// not two states.
	live := lipgloss.NewStyle().Foreground(palette.Warning)

	// The newest readings, right-aligned, so the meter grows leftwards from the
	// present rather than sliding the present around.
	history := m.voiceInput.levels
	if len(history) > cells {
		history = history[len(history)-cells:]
	}
	var meter strings.Builder
	for index := 0; index < cells; index++ {
		level := 0.0
		if offset := index - (cells - len(history)); offset >= 0 {
			level = history[offset]
		}
		step := int(level * float64(len(ramp)))
		if step >= len(ramp) {
			step = len(ramp) - 1
		}
		if step <= 0 {
			meter.WriteString(quiet.Render(ramp[0]))
			continue
		}
		meter.WriteString(live.Render(ramp[step]))
	}
	return meter.String()
}

// voiceListeningDetail is what the rail says under an open microphone.
//
// It names the stop that works everywhere first. And when the recorder has been
// running long enough to be sure it has heard nothing, it stops naming keys and
// names the problem instead: at that point the useful sentence is not how to
// close the microphone, it is why talking into it did nothing.
func (m *Model) voiceListeningDetail() string {
	if m == nil || m.voiceInput == nil {
		return ""
	}
	if heard, sure := m.voiceInput.listener.Hearing(); sure && !heard {
		return "hearing nothing — check the input device or mute switch"
	}
	return "esc cancels · " + m.voiceInputKeyHint() + " stops"
}

// optionComposedKeys maps what a stock macOS terminal INSERTS to the chord the
// user was reaching for.
//
// Option is not Meta unless the terminal is told so. Until it is, Option+V does
// not send alt+v — it composes "√" and types it into the composer, which is why
// dictation looked broken rather than unbound: the key produced a character
// nobody wanted and no other effect at all.
//
// Naming it is worth more than rebinding it. Someone who learned alt+v will
// keep pressing alt+v, and a stray "√" in the draft is the only evidence they
// get. This turns that evidence into the sentence that fixes it.
var optionComposedKeys = map[string]string{
	"√": "alt+v", "ç": "alt+c", "∂": "alt+d", "µ": "alt+m",
	"ø": "alt+o", "®": "alt+r", "†": "alt+t",
}

// noticeForOptionComposedKey returns the hint for a character that is really a
// swallowed chord, or "" for ordinary text.
//
// Only for a composer that was empty. Someone mid-sentence typing "√" means the
// symbol, and interrupting them to explain their own keyboard would be the
// harness talking over the user — the same mistake speech makes when it does
// not stop for a keypress.
func (m *Model) noticeForOptionComposedKey(typed string) string {
	if m == nil || strings.TrimSpace(m.input.Value()) != "" {
		return ""
	}
	chord, composed := optionComposedKeys[typed]
	if !composed {
		return ""
	}
	if chord == "alt+v" {
		return "That is Option+V — this terminal composes it instead of sending alt+v. " +
			"Press " + m.voiceInputKeyHint() + " to dictate, or run /voice."
	}
	return "That is Option — this terminal composes a character instead of sending " +
		chord + ". Set it to use Option as Meta, or use the slash command."
}

// voiceInputKeyHint names the binding as this build actually has it.
//
// Read from the keymap rather than written out, because the help text and the
// binding drifted apart once already: a literal in a rendered string cannot be
// caught by anything, and the string that named the wrong key was the only
// instruction a listener had.
func (m *Model) voiceInputKeyHint() string {
	if m == nil {
		return "/voice"
	}
	if keys := m.keys.VoiceInput.Keys(); len(keys) > 0 {
		return keys[0]
	}
	return "/voice"
}

// voiceMeterCells is how much of the rail the meter may take.
//
// It follows the same width rule the waiting trace uses, for the same reason:
// below it the row has to spend its cells on the words. One cell still reports
// a level, which is the part that cannot be replaced by text.
func (m *Model) voiceMeterCells() int {
	if m == nil {
		return 1
	}
	// Wider panes get a longer window, and the reason is legibility rather than
	// space. The history scrolls at the tick rate, so eight cells hold about
	// half a second — which reads as flicker, not as a voice. Sixteen holds long
	// enough to see a phrase rise and fall, which is the shape that answers "is
	// it hearing me" at a glance.
	switch width := m.chatPaneWidth(); {
	case width >= 100:
		return 16
	case width >= 58:
		return 10
	default:
		return 1
	}
}
