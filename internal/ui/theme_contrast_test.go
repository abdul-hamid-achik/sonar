package ui

import (
	"fmt"
	"image/color"
	"math"
	"reflect"
	"strings"
	"testing"
)

// Every registered theme must be legible in both modes. A theme is a set of
// answers to one semantic vocabulary, so a new scheme cannot be added without
// its foregrounds clearing WCAG AA for normal text — measured against the
// exact surface sonar paints for that appearance.
func TestThemeForegroundsMeetContrastInBothModes(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	for _, id := range themeIDs() {
		theme := resolveTheme(id)
		t.Run(theme.ID, func(t *testing.T) {
			modes := []struct {
				name       string
				isDark     bool
				background color.Color
			}{
				{name: "light", background: hexColor(t, theme.Light.Background)},
				{name: "dark", isDark: true, background: hexColor(t, theme.Dark.Background)},
			}
			for _, mode := range modes {
				t.Run(mode.name, func(t *testing.T) {
					palette := newSemanticPalette(mode.isDark, theme.ID)
					foregrounds := []struct {
						name  string
						color color.Color
					}{
						{name: "dim", color: palette.Dim},
						{name: "muted", color: palette.Muted},
						{name: "text", color: palette.Text},
						{name: "accent", color: palette.Accent},
						{name: "accent2", color: palette.Accent2},
						{name: "error", color: palette.Error},
						{name: "success", color: palette.Success},
						{name: "special", color: palette.Special},
						{name: "warning", color: palette.Warning},
					}
					for _, foreground := range foregrounds {
						t.Run(foreground.name, func(t *testing.T) {
							const minimumContrast = 4.5
							ratio := contrastRatio(foreground.color, mode.background)
							if ratio < minimumContrast {
								t.Fatalf("%s/%s %s contrast = %.2f:1, want >= %.1f:1",
									theme.ID, mode.name, foreground.name, ratio, minimumContrast)
							}
						})
					}
				})
			}
		})
	}
}

// The default theme must stay the one the product has always shipped, and an
// unknown ID must never leave the UI colorless.
func TestThemeResolutionFallsBackToTheDefault(t *testing.T) {
	if got := resolveTheme("no-such-theme").ID; got != defaultThemeID {
		t.Fatalf("unknown theme resolved to %q, want %q", got, defaultThemeID)
	}
	if got := resolveTheme("").ID; got != defaultThemeID {
		t.Fatalf("empty theme resolved to %q, want %q", got, defaultThemeID)
	}
	if got := resolveTheme("  CATPPUCCIN  ").ID; got != "catppuccin" {
		t.Fatalf("theme id is not normalized: %q", got)
	}
	if knownThemeID("no-such-theme") {
		t.Fatal("knownThemeID accepted an unregistered theme")
	}
	if ids := themeIDs(); len(ids) == 0 || ids[0] != defaultThemeID {
		t.Fatalf("theme listing must lead with the default: %v", ids)
	}
}

func hexColor(t *testing.T, value string) color.Color {
	t.Helper()
	if len(value) != 7 || value[0] != '#' {
		t.Fatalf("invalid test color %q", value)
	}
	var red, green, blue uint8
	if _, err := fmt.Sscanf(value, "#%02x%02x%02x", &red, &green, &blue); err != nil {
		t.Fatalf("parse test color %q: %v", value, err)
	}
	return color.RGBA{R: red, G: green, B: blue, A: 0xff}
}

func contrastRatio(a, b color.Color) float64 {
	aLuminance := relativeLuminance(a)
	bLuminance := relativeLuminance(b)
	light, dark := math.Max(aLuminance, bLuminance), math.Min(aLuminance, bLuminance)
	return (light + 0.05) / (dark + 0.05)
}

func relativeLuminance(value color.Color) float64 {
	red, green, blue, _ := value.RGBA()
	linear := func(component uint32) float64 {
		srgb := float64(component) / 65535.0
		if srgb <= 0.04045 {
			return srgb / 12.92
		}
		return math.Pow((srgb+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(red) + 0.7152*linear(green) + 0.0722*linear(blue)
}

// Themes must be visually distinct. A registry entry whose palette duplicates
// another is a menu item that does nothing, and copy-paste is exactly how that
// happens when adding a scheme.
func TestRegisteredThemesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, id := range themeIDs() {
		theme := resolveTheme(id)
		for _, mode := range []struct {
			name   string
			colors themeColors
		}{{"dark", theme.Dark}, {"light", theme.Light}} {
			key := fmt.Sprintf("%s|%v", mode.name, mode.colors)
			if previous, duplicate := seen[key]; duplicate {
				t.Errorf("%s/%s has the same palette as %s", theme.ID, mode.name, previous)
			}
			seen[key] = theme.ID + "/" + mode.name
		}
	}
}

// Every registered theme must be fully specified. A blank role silently falls
// back to the terminal default and breaks the semantic vocabulary.
func TestRegisteredThemesAreComplete(t *testing.T) {
	for _, id := range themeIDs() {
		theme := resolveTheme(id)
		if strings.TrimSpace(theme.Label) == "" || strings.TrimSpace(theme.Description) == "" {
			t.Errorf("%s is missing a label or description", theme.ID)
		}
		for _, mode := range []struct {
			name   string
			colors themeColors
		}{{"dark", theme.Dark}, {"light", theme.Light}} {
			value := reflect.ValueOf(mode.colors)
			for i := 0; i < value.NumField(); i++ {
				hex := value.Field(i).String()
				if !strings.HasPrefix(hex, "#") || len(hex) != 7 {
					t.Errorf("%s/%s %s = %q is not a 6-digit hex color",
						theme.ID, mode.name, value.Type().Field(i).Name, hex)
				}
			}
		}
	}
}
