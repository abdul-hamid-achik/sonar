package ui

import (
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// GlyphProfile selects terminal-safe symbols independently from color and
// motion preferences. NO_COLOR must never silently imply ASCII: a terminal may
// support Unicode while intentionally disabling color.
type GlyphProfile uint8

const (
	GlyphUnicode GlyphProfile = iota
	GlyphASCII
)

func (profile GlyphProfile) Valid() bool {
	return profile == GlyphUnicode || profile == GlyphASCII
}

// resolveGlyphProfile keeps the zero value backwards compatible while making
// optional component constructors fail safe when handed an unknown profile.
func resolveGlyphProfile(profiles ...GlyphProfile) GlyphProfile {
	if len(profiles) > 0 && profiles[0].Valid() {
		return profiles[0]
	}
	return GlyphUnicode
}

// GlyphSet is the semantic vocabulary shared by transcript, tools, agents,
// and controls. Every token below is one terminal cell in both profiles so
// swapping profiles cannot change layout geometry.
type GlyphSet struct {
	UserRail     string
	Collapsed    string
	Expanded     string
	Success      string
	Error        string
	Running      string
	Queued       string
	Waiting      string
	Cancelled    string
	Continuation string
	Selected     string
	Unselected   string
	Vertical     string
	Horizontal   string
	Left         string
	Right        string
}

func glyphSet(profile GlyphProfile) GlyphSet {
	if profile == GlyphASCII {
		return GlyphSet{
			UserRail:     "|",
			Collapsed:    ">",
			Expanded:     "v",
			Success:      "+",
			Error:        "x",
			Running:      "*",
			Queued:       "o",
			Waiting:      "o",
			Cancelled:    "-",
			Continuation: ">",
			Selected:     "*",
			Unselected:   "o",
			Vertical:     "|",
			Horizontal:   "-",
			Left:         "<",
			Right:        ">",
		}
	}
	return GlyphSet{
		UserRail:     "▌",
		Collapsed:    "▸",
		Expanded:     "▾",
		Success:      "✓",
		Error:        "✗",
		Running:      "◉",
		Queued:       "○",
		Waiting:      "○",
		Cancelled:    "–",
		Continuation: "↳",
		Selected:     "●",
		Unselected:   "○",
		Vertical:     "│",
		Horizontal:   "─",
		Left:         "←",
		Right:        "→",
	}
}

func glyphSeparator(profile GlyphProfile) string {
	if resolveGlyphProfile(profile) == GlyphASCII {
		return " | "
	}
	return " · "
}

func glyphEllipsis(profile GlyphProfile) string {
	if resolveGlyphProfile(profile) == GlyphASCII {
		return "~"
	}
	return "…"
}

func borderForGlyphProfile(profile GlyphProfile) lipgloss.Border {
	if resolveGlyphProfile(profile) != GlyphASCII {
		return lipgloss.RoundedBorder()
	}
	return lipgloss.Border{
		Top:          "-",
		Bottom:       "-",
		Left:         "|",
		Right:        "|",
		TopLeft:      "+",
		TopRight:     "+",
		BottomLeft:   "+",
		BottomRight:  "+",
		MiddleLeft:   "+",
		MiddleRight:  "+",
		Middle:       "+",
		MiddleTop:    "+",
		MiddleBottom: "+",
	}
}

// requestedGlyphProfile reads an explicit user preference. TERM=dumb is the
// sole automatic fallback because it explicitly declares minimal terminal
// capabilities; NO_COLOR remains a separate presentation axis.
func requestedGlyphProfile() GlyphProfile {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SONAR_GLYPHS")), "ascii") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return GlyphASCII
	}
	return GlyphUnicode
}
