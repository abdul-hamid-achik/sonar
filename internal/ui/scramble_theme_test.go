package ui

import (
	"testing"

	colorful "github.com/lucasb-eyer/go-colorful"
)

// The wait animation's gradient endpoints were four literal hexes, and the
// dark pair was Nord's own Accent — so every scheme rendered this one surface
// in Nord. Switching to Catppuccin or Gruvbox changed the whole TUI except the
// thing moving in front of you.
//
// This is the rule the theme registry exists to enforce, and the scramble was
// the last surviving violation of it.
func TestScrambleGradientFollowsTheActiveScheme(t *testing.T) {
	seen := make(map[string][]string)
	for _, themeID := range themeIDs() {
		for _, isDark := range []bool{true, false} {
			scramble := NewScrambleModel(isDark, themeID)
			from := scramble.colorFrom.Hex()
			to := scramble.colorTo.Hex()
			if from == to {
				t.Errorf("%s (dark=%v): gradient endpoints are identical (%s)", themeID, isDark, from)
			}
			key := from + "→" + to
			seen[key] = append(seen[key], themeID)
		}
	}
	// Distinct schemes must not collapse onto one gradient. A single shared
	// pair would mean the lookup ran but the answer never varied.
	if len(seen) < len(themeIDs()) {
		t.Errorf("%d distinct gradients across %d schemes; the palette is not being read",
			len(seen), len(themeIDs()))
	}
}

// SetDark must repoint an existing model, not only a freshly constructed one:
// the theme changes at runtime through /theme while the model is alive.
func TestScrambleRepointsOnThemeChange(t *testing.T) {
	ids := themeIDs()
	if len(ids) < 2 {
		t.Skip("need two schemes to observe a change")
	}
	scramble := NewScrambleModel(true, ids[0])
	before := scramble.colorFrom.Hex()

	scramble.SetDark(true, ids[1])
	if scramble.colorFrom.Hex() == before {
		t.Errorf("switching from %s to %s left the gradient at %s", ids[0], ids[1], before)
	}
}

// The endpoints are the scheme's two accents, by meaning: the gradient reads as
// one signal travelling out and returning, which is the relationship every
// scheme already answers. Pinning it keeps a future edit from reaching for a
// role that means something else.
func TestScrambleUsesTheAccentPair(t *testing.T) {
	for _, isDark := range []bool{true, false} {
		palette := newSemanticPalette(isDark, defaultThemeID)
		scramble := NewScrambleModel(isDark, defaultThemeID)

		wantFrom, _ := colorful.MakeColor(palette.Accent)
		wantTo, _ := colorful.MakeColor(palette.Accent2)
		if got := scramble.colorFrom.Hex(); got != wantFrom.Hex() {
			t.Errorf("dark=%v from = %s, want the scheme Accent %s", isDark, got, wantFrom.Hex())
		}
		if got := scramble.colorTo.Hex(); got != wantTo.Hex() {
			t.Errorf("dark=%v to = %s, want the scheme Accent2 %s", isDark, got, wantTo.Hex())
		}
	}
}
