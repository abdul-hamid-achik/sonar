package ui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// remapANSI16Line regenerates one untrusted tool-output line for display.
// Only basic SGR state (reset, bold toggles, and the 16 basic foreground
// slots) is interpreted, and it selects adaptive palette styles rather than
// echoing source bytes. Every other escape sequence is dropped whole, so the
// output is always sanitized plain segments re-rendered through lipgloss and
// never contains a byte copied from an input escape sequence.

// ansiRemapDefaultFg marks the default foreground slot, which renders with
// the same muted style as plain tool-result lines.
const ansiRemapDefaultFg = -1

type ansiRemapSegment struct {
	text string
	bold bool
	fg   int
}

func remapANSI16Line(line string, palette semanticPalette, width int) string {
	return remapANSI16LineWithGlyphProfile(line, palette, width, GlyphUnicode)
}

func remapANSI16LineWithGlyphProfile(line string, palette semanticPalette, width int, profile GlyphProfile) string {
	if width <= 0 {
		return ""
	}
	parsed := parseANSI16Segments(line)
	segments := parsed[:0]
	for _, segment := range parsed {
		// The same character rules as the sanitized plain path: control and
		// bidi stripping, then deterministic tab expansion.
		text := strings.ReplaceAll(sanitizeTerminalLine(segment.text), "\t", "    ")
		if text == "" {
			continue
		}
		segment.text = text
		segments = append(segments, segment)
	}
	return renderANSI16SegmentsWithGlyphProfile(segments, palette, width, profile)
}

// renderANSI16Segments renders only adapter-sanitized plain text and
// allowlisted style tokens. Unlike remapANSI16Line it never receives or parses
// terminal escape bytes.
func renderANSI16SegmentsWithGlyphProfile(
	segments []ansiRemapSegment,
	palette semanticPalette,
	width int,
	profile GlyphProfile,
) string {
	if width <= 0 {
		return ""
	}
	totalWidth := 0
	for _, segment := range segments {
		totalWidth += lipgloss.Width(segment.text)
	}
	var b strings.Builder
	if totalWidth <= width {
		for _, segment := range segments {
			b.WriteString(ansiRemapStyle(segment.bold, segment.fg, palette).Render(segment.text))
		}
		return b.String()
	}

	ellipsis := "…"
	if resolveGlyphProfile(profile) == GlyphASCII {
		ellipsis = "~"
	}
	defaultStyle := ansiRemapStyle(false, ansiRemapDefaultFg, palette)
	if width <= lipgloss.Width(ellipsis) {
		return defaultStyle.Render(ellipsis)
	}
	budget := width - lipgloss.Width(ellipsis)
	used := 0
	for _, segment := range segments {
		if used >= budget {
			break
		}
		text := segment.text
		if used+lipgloss.Width(text) > budget {
			text = clipDisplayWidth(text, budget-used)
		}
		if text == "" {
			continue
		}
		b.WriteString(ansiRemapStyle(segment.bold, segment.fg, palette).Render(text))
		used += lipgloss.Width(text)
	}
	b.WriteString(defaultStyle.Render(ellipsis))
	return b.String()
}

// parseANSI16Segments walks the line byte-safe (escape bytes are ASCII, so
// multi-byte UTF-8 runes pass through untouched) and produces plain-text
// segments tagged with the bold/foreground state active where they appeared.
func parseANSI16Segments(line string) []ansiRemapSegment {
	var segments []ansiRemapSegment
	var current strings.Builder
	bold := false
	fg := ansiRemapDefaultFg
	flush := func() {
		if current.Len() == 0 {
			return
		}
		segments = append(segments, ansiRemapSegment{text: current.String(), bold: bold, fg: fg})
		current.Reset()
	}
	for i := 0; i < len(line); {
		if line[i] != 0x1b {
			current.WriteByte(line[i])
			i++
			continue
		}
		sequence, next := consumeEscapeSequence(line, i)
		i = next
		if nextBold, nextFg, ok := applySGRSubset(sequence, bold, fg); ok && (nextBold != bold || nextFg != fg) {
			flush()
			bold, fg = nextBold, nextFg
		}
	}
	flush()
	return segments
}

// consumeEscapeSequence returns the whole escape sequence starting at start
// (which must index an ESC byte) and the index of the first byte after it.
// Unterminated sequences extend to the end of the input so a truncated escape
// can never leak its tail as visible text.
func consumeEscapeSequence(line string, start int) (string, int) {
	if start+1 >= len(line) {
		return line[start:], len(line)
	}
	switch line[start+1] {
	case '[':
		// CSI: parameter and intermediate bytes, then one final byte.
		i := start + 2
		for i < len(line) && line[i] >= 0x20 && line[i] <= 0x3f {
			i++
		}
		if i < len(line) && line[i] >= 0x40 && line[i] <= 0x7e {
			return line[start : i+1], i + 1
		}
		return line[start:], len(line)
	case ']', 'P', 'X', '^', '_':
		// String sequences (OSC, DCS, SOS, PM, APC) end at BEL or ST (ESC \).
		for i := start + 2; i < len(line); i++ {
			if line[i] == 0x07 {
				return line[start : i+1], i + 1
			}
			if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '\\' {
				return line[start : i+2], i + 2
			}
		}
		return line[start:], len(line)
	default:
		return line[start : start+2], start + 2
	}
}

// applySGRSubset interprets a complete CSI SGR sequence whose parameters are
// all within {0, 1, 22, 30..37, 39, 90..97}. Any other sequence — including
// 256-color and truecolor SGR — reports ok=false and is dropped whole without
// touching state, so a rejected prefix can never smuggle in a partial effect.
func applySGRSubset(sequence string, bold bool, fg int) (nextBold bool, nextFg int, ok bool) {
	if len(sequence) < 3 || sequence[0] != 0x1b || sequence[1] != '[' || sequence[len(sequence)-1] != 'm' {
		return bold, fg, false
	}
	body := sequence[2 : len(sequence)-1]
	for i := 0; i < len(body); i++ {
		if body[i] != ';' && (body[i] < '0' || body[i] > '9') {
			return bold, fg, false
		}
	}
	params := strings.Split(body, ";")
	values := make([]int, 0, len(params))
	for _, param := range params {
		value := 0
		if param != "" {
			parsed, err := strconv.Atoi(param)
			if err != nil {
				return bold, fg, false
			}
			value = parsed
		}
		if !allowedSGRParam(value) {
			return bold, fg, false
		}
		values = append(values, value)
	}
	for _, value := range values {
		switch {
		case value == 0:
			bold, fg = false, ansiRemapDefaultFg
		case value == 1:
			bold = true
		case value == 22:
			bold = false
		case value == 39:
			fg = ansiRemapDefaultFg
		case value >= 30 && value <= 37:
			fg = value - 30
		case value >= 90 && value <= 97:
			fg = value - 90
		}
	}
	return bold, fg, true
}

func allowedSGRParam(value int) bool {
	switch {
	case value == 0 || value == 1 || value == 22 || value == 39:
		return true
	case value >= 30 && value <= 37:
		return true
	case value >= 90 && value <= 97:
		return true
	default:
		return false
	}
}

// ansiRemapStyle maps one basic ANSI foreground slot onto the shared adaptive
// vocabulary. Bright variants land on the same slots, and the default slot
// matches the plain tool-result style so uncolored spans stay consistent.
func ansiRemapStyle(bold bool, fg int, palette semanticPalette) lipgloss.Style {
	foreground := palette.Muted
	switch fg {
	case 0:
		foreground = palette.Dim
	case 1:
		foreground = palette.Error
	case 2:
		foreground = palette.Success
	case 3:
		foreground = palette.Warning
	case 4:
		foreground = palette.Accent2
	case 5:
		foreground = palette.Special
	case 6:
		foreground = palette.Accent
	case 7:
		foreground = palette.Text
	}
	style := lipgloss.NewStyle().Foreground(foreground)
	if bold {
		style = style.Bold(true)
	}
	return style
}

// clipDisplayWidth truncates plain text to a display-cell budget without an
// ellipsis; remapANSI16Line appends one shared ellipsis after the last
// clipped segment.
func clipDisplayWidth(s string, budget int) string {
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > budget {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String()
}
