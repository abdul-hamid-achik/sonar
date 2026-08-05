package ui

import (
	"math/rand"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/lucasb-eyer/go-colorful"
)

// scrambleChars is the character set for the scramble animation. Braille dot
// patterns (single-width) shimmer like a "thinking" indicator instead of
// looking like a random alphanumeric hash.
const scrambleChars = "⠁⠂⠄⠆⠇⠋⠙⠸⠴⠦⠧⠏⠟⡇⡏⡗⣇⣧⣷⣿"

// scrambleWidth is the number of characters in the animation.
const scrambleWidth = 6

// ScrambleTickMsg triggers the next animation frame.
type ScrambleTickMsg struct {
	ID    int
	Frame int
}

// ScrambleModel is a custom BubbleTea component that renders a gradient
// character scramble animation, inspired by Charmbracelet's Crush CLI.
type ScrambleModel struct {
	id        int
	frame     int
	chars     []rune
	visible   int
	colorFrom colorful.Color
	colorTo   colorful.Color
	rng       *rand.Rand
}

// NewScrambleModel creates a new scramble animation with theme-appropriate colors.
func NewScrambleModel(isDark bool, themeID string) ScrambleModel {
	s := ScrambleModel{
		id:    1,
		chars: make([]rune, scrambleWidth),
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	s.SetDark(isDark, themeID)
	s.randomizeChars()
	return s
}

// SetDark repoints the gradient at the active scheme.
//
// The endpoints used to be four literal hexes, and the dark pair was Nord's
// Accent and a purple close to its Accent2 — so the wait animation painted
// Nord on every scheme. Switching to Catppuccin or Gruvbox left this one
// surface behind, which is the whole failure the theme registry exists to
// prevent, and the comment here already claimed to interpolate "the active
// theme's colors".
//
// Accent → Accent2 is the correct pair by meaning, not by resemblance: the
// gradient reads as one signal travelling out and coming back, which is the
// two-accent relationship every scheme already answers.
func (s *ScrambleModel) SetDark(isDark bool, themeID string) {
	palette := newSemanticPalette(isDark, themeID)
	s.colorFrom, _ = colorful.MakeColor(palette.Accent)
	s.colorTo, _ = colorful.MakeColor(palette.Accent2)
}

// Reset resets the animation (new ID + zero visible). Call when agent starts.
func (s *ScrambleModel) Reset() {
	s.id++
	s.frame = 0
	s.visible = 0
	s.randomizeChars()
}

// Tick schedules the next animation frame (~15 FPS = 66ms).
func (s ScrambleModel) Tick() tea.Cmd {
	id := s.id
	frame := s.frame
	return tea.Tick(66*time.Millisecond, func(time.Time) tea.Msg {
		return ScrambleTickMsg{ID: id, Frame: frame}
	})
}

// Update processes tick messages and advances the animation.
func (s ScrambleModel) Update(msg tea.Msg) (ScrambleModel, tea.Cmd) {
	if tick, ok := msg.(ScrambleTickMsg); ok {
		if tick.ID != s.id || tick.Frame != s.frame {
			return s, nil // stale tick, ignore
		}
		s.randomizeChars()
		if s.visible < scrambleWidth {
			s.visible++
		}
		s.frame++
		return s, s.Tick()
	}
	return s, nil
}

// View renders the complete visible animation.
func (s ScrambleModel) View() string {
	return s.ViewN(scrambleWidth)
}

// ViewN renders at most maxCells of the animation. The parent uses a single
// animated cell on narrow terminals and the full six-cell shimmer when space
// permits, keeping the working action visible at every supported width.
func (s ScrambleModel) ViewN(maxCells int) string {
	if s.visible == 0 {
		return ""
	}
	visible := min(s.visible, scrambleWidth)
	visible = min(visible, max(0, maxCells))
	if visible == 0 {
		return ""
	}

	// NO_COLOR fallback
	if noColor {
		dots := ""
		for i := 0; i < visible; i++ {
			dots += "."
		}
		return dots
	}

	result := ""
	for i := 0; i < visible && i < len(s.chars); i++ {
		// Calculate gradient position
		t := float64(i) / float64(scrambleWidth-1)
		c := s.colorFrom.BlendHcl(s.colorTo, t).Clamped()
		hex := c.Hex()

		style := lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
		result += style.Render(string(s.chars[i]))
	}
	return result
}

// randomizeChars fills the chars slice with random characters.
func (s *ScrambleModel) randomizeChars() {
	runes := []rune(scrambleChars)
	for i := range s.chars {
		s.chars[i] = runes[s.rng.Intn(len(runes))]
	}
}
