package ui

import (
	"sort"
	"strings"
)

// A Theme is a named pair of semantic color vocabularies, one for a light
// terminal and one for a dark terminal.
//
// Themes do not introduce new color meanings. semanticPalette remains the only
// vocabulary the components speak, so adding a theme is a matter of answering
// the same eleven questions in a different palette — never of teaching a component
// about a specific scheme.
//
// Every value here is checked by TestThemeForegroundsMeetContrastInBothModes,
// which measures each foreground against that theme's own background.
// Where an upstream scheme publishes only a dark variant, or publishes a light
// variant whose accents fall below 4.5:1 on its light surface, the values are
// deliberately darkened versions of the same hues rather than the upstream hex.
// That adaptation is noted per theme; it is the same treatment the built-in
// Nord palette has always had.
type Theme struct {
	ID          string
	Label       string
	Description string
	Light       themeColors
	Dark        themeColors
}

// themeColors answers the semantic vocabulary in hex. Field-for-field it
// mirrors semanticPalette.
type themeColors struct {
	Background string
	Dim        string
	Muted      string
	Text       string
	Accent     string
	Accent2    string
	Error      string
	Success    string
	Special    string
	Warning    string
	Border     string
}

// themes is the registry, keyed by ID. Order for display comes from themeIDs.
var themes = map[string]Theme{
	"nord": {
		ID:          "nord",
		Label:       "Nord",
		Description: "Arctic blues — the sonar default",
		Dark: themeColors{
			Background: "#2E3440",
			Dim:        "#96A2B8", Muted: "#D8DEE9", Text: "#E5E9F0",
			Accent: "#88C0D0", Accent2: "#81A1C1", Error: "#ED7A84",
			Success: "#A3BE8C", Special: "#C18CB9", Warning: "#EBCB8B",
			Border: "#4C566A",
		},
		Light: themeColors{
			Background: "#ECEFF4",
			Dim:        "#5B6779", Muted: "#4C566A", Text: "#3B4252",
			Accent: "#3D7070", Accent2: "#48698F", Error: "#AF4141",
			Success: "#40732E", Special: "#7B5A83", Warning: "#8A6500",
			Border: "#D8DEE9",
		},
	},
	"catppuccin": {
		ID:          "catppuccin",
		Label:       "Catppuccin",
		Description: "Mocha on dark terminals, Latte on light",
		Dark: themeColors{
			Background: "#1E1E2E",
			Dim:        "#9399B2", Muted: "#BAC2DE", Text: "#CDD6F4",
			Accent: "#94E2D5", Accent2: "#89B4FA", Error: "#F38BA8",
			Success: "#A6E3A1", Special: "#CBA6F7", Warning: "#F9E2AF",
			Border: "#45475A",
		},
		// Latte's own accents sit near 3:1 on its surface, so the darker end of each
		// Latte hue is used for foregrounds.
		Light: themeColors{
			Background: "#EFF1F5",
			Dim:        "#65687E", Muted: "#5C5F77", Text: "#4C4F69",
			Accent: "#136B70", Accent2: "#1B5CDE", Error: "#D20F39",
			Success: "#2F7A20", Special: "#8839EF", Warning: "#8A5A0B",
			Border: "#BCC0CC",
		},
	},
	"gruvbox": {
		ID:          "gruvbox",
		Label:       "Gruvbox",
		Description: "Warm retro contrast",
		Dark: themeColors{
			Background: "#282828",
			Dim:        "#A89984", Muted: "#D5C4A1", Text: "#EBDBB2",
			Accent: "#8EC07C", Accent2: "#83A598", Error: "#FB5643",
			Success: "#B8BB26", Special: "#D3869B", Warning: "#FABD2F",
			Border: "#504945",
		},
		Light: themeColors{
			Background: "#FBF1C7",
			Dim:        "#74675D", Muted: "#665C54", Text: "#3C3836",
			Accent: "#3D7352", Accent2: "#076678", Error: "#9D0006",
			Success: "#5A6600", Special: "#8F3F71", Warning: "#8A5A00",
			Border: "#D5C4A1",
		},
	},
	"tokyo-night": {
		ID:          "tokyo-night",
		Label:       "Tokyo Night",
		Description: "Storm on dark terminals, Day on light",
		Dark: themeColors{
			Background: "#24283B",
			Dim:        "#8891B5", Muted: "#A9B1D6", Text: "#C0CAF5",
			Accent: "#7DCFFF", Accent2: "#7AA2F7", Error: "#F7768E",
			Success: "#9ECE6A", Special: "#BB9AF7", Warning: "#E0AF68",
			Border: "#3B4261",
		},
		Light: themeColors{
			Background: "#E1E2E7",
			Dim:        "#586086", Muted: "#4C5AA0", Text: "#343B58",
			Accent: "#00647F", Accent2: "#175FBF", Error: "#B12046",
			Success: "#49622F", Special: "#7A3FD0", Warning: "#755B34",
			Border: "#B4B5B9",
		},
	},
	"solarized": {
		ID:          "solarized",
		Label:       "Solarized",
		Description: "Ethan Schoonover's precision palette",
		Dark: themeColors{
			Background: "#002B36",
			Dim:        "#839496", Muted: "#93A1A1", Text: "#EEE8D5",
			Accent: "#2AA198", Accent2: "#3295DA", Error: "#E56663",
			Success: "#859900", Special: "#8488CD", Warning: "#B58900",
			Border: "#073642",
		},
		// Solarized shares one accent set across modes; only the bases swap.
		// Blue and violet clear 4.5:1 on the light surface; warmer accents do not, so
		// those are taken one step darker.
		Light: themeColors{
			Background: "#FDF6E3",
			Dim:        "#5A6D74", Muted: "#586E75", Text: "#073642",
			Accent: "#1D7A73", Accent2: "#1E6EA6", Error: "#C32B28",
			Success: "#5F6F00", Special: "#5E62AF", Warning: "#845F00",
			Border: "#93A1A1",
		},
	},
	"rose-pine": {
		ID:          "rose-pine",
		Label:       "Rosé Pine",
		Description: "Muted natural tones; Dawn on light terminals",
		// Rosé Pine has no green. Success maps to foam, the palette's own
		// affirmative token, rather than importing a hue the scheme rejects.
		Dark: themeColors{
			Background: "#191724",
			Dim:        "#908CAA", Muted: "#C9C6DC", Text: "#E0DEF4",
			Accent: "#EBBCBA", Accent2: "#9CCFD8", Error: "#EB6F92",
			Success: "#9CCFD8", Special: "#C4A7E7", Warning: "#F6C177",
			Border: "#403D52",
		},
		Light: themeColors{
			Background: "#FAF4ED",
			Dim:        "#6A6682", Muted: "#575279", Text: "#464261",
			Accent: "#9E5168", Accent2: "#286983", Error: "#9E5168",
			Success: "#286983", Special: "#765F8F", Warning: "#8A5A0B",
			Border: "#DFDAD9",
		},
	},
	"everforest": {
		ID:          "everforest",
		Label:       "Everforest",
		Description: "Soft forest greens, easy on the eyes",
		Dark: themeColors{
			Background: "#2D353B",
			Dim:        "#949F97", Muted: "#C0B6A0", Text: "#D3C6AA",
			Accent: "#83C092", Accent2: "#7FBBB3", Error: "#E67E80",
			Success: "#A7C080", Special: "#D699B6", Warning: "#DBBC7F",
			Border: "#4F585E",
		},
		Light: themeColors{
			Background: "#FDF6E3",
			Dim:        "#657464", Muted: "#5C6A72", Text: "#4A555B",
			Accent: "#267759", Accent2: "#2D7095", Error: "#CF2B29",
			Success: "#637100", Special: "#B63A72", Warning: "#886500",
			Border: "#E0DCC7",
		},
	},
	"one": {
		ID:          "one",
		Label:       "One",
		Description: "Atom's One Dark and One Light",
		Dark: themeColors{
			Background: "#282C34",
			Dim:        "#8C93A0", Muted: "#ABB2BF", Text: "#D7DAE0",
			Accent: "#56B6C2", Accent2: "#61AFEF", Error: "#E96D77",
			Success: "#98C379", Special: "#C678DD", Warning: "#E5C07B",
			Border: "#3E4451",
		},
		Light: themeColors{
			Background: "#FAFAFA",
			Dim:        "#696C77", Muted: "#4F525E", Text: "#383A42",
			Accent: "#0175A5", Accent2: "#2B63DA", Error: "#CA3431",
			Success: "#367735", Special: "#A626A4", Warning: "#986801",
			Border: "#D3D3D6",
		},
	},
	"kanagawa": {
		ID:          "kanagawa",
		Label:       "Kanagawa",
		Description: "Wave on dark terminals, Lotus on light",
		Dark: themeColors{
			Background: "#1F1F28",
			Dim:        "#9A968C", Muted: "#C8C3AC", Text: "#DCD7BA",
			Accent: "#7AA89F", Accent2: "#7E9CD8", Error: "#E46876",
			Success: "#98BB6C", Special: "#957FB8", Warning: "#E6C384",
			Border: "#363646",
		},
		Light: themeColors{
			Background: "#F2ECBC",
			Dim:        "#625F54", Muted: "#5F5F70", Text: "#545464",
			Accent: "#4B6863", Accent2: "#4D699B", Error: "#AA3647",
			Success: "#516539", Special: "#624C83", Warning: "#656036",
			Border: "#E4D794",
		},
	},
	"dracula": {
		ID:          "dracula",
		Label:       "Dracula",
		Description: "High-contrast neon on dark; darkened hues on light",
		Dark: themeColors{
			Background: "#282A36",
			Dim:        "#8991B6", Muted: "#DDDDD5", Text: "#F8F8F2",
			Accent: "#8BE9FD", Accent2: "#BD93F9", Error: "#FF5555",
			Success: "#50FA7B", Special: "#FF79C6", Warning: "#F1FA8C",
			Border: "#44475A",
		},
		// Dracula publishes no official light variant. Rather than ship an
		// invented "Dracula Light", these keep each Dracula hue and lower its
		// lightness until it clears 4.5:1 on the adapted light surface.
		Light: themeColors{
			Background: "#F8F8F2",
			Dim:        "#5A6392", Muted: "#4A5178", Text: "#282A36",
			Accent: "#0E7490", Accent2: "#6D3FC0", Error: "#C02C2C",
			Success: "#2E7D3E", Special: "#B03A7A", Warning: "#8A6D00",
			Border: "#B9BCCB",
		},
	},
}

// defaultThemeID is the palette sonar has always shipped.
const defaultThemeID = "nord"

// themeIDs returns every registered theme ID with the default first and the
// rest alphabetical, so pickers and docs agree on ordering.
func themeIDs() []string {
	ids := make([]string, 0, len(themes))
	for id := range themes {
		if id == defaultThemeID {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return append([]string{defaultThemeID}, ids...)
}

// resolveTheme maps a configured ID to a theme, falling back to the default. An
// unknown ID is not an error at the render boundary: a config file naming a
// theme this build does not have must not leave the UI colorless.
func resolveTheme(id string) Theme {
	if theme, ok := themes[normalizeThemeID(id)]; ok {
		return theme
	}
	return themes[defaultThemeID]
}

// knownThemeID reports whether id names a registered theme. Command handling
// uses this to reject a typo instead of silently selecting the default.
func knownThemeID(id string) bool {
	_, ok := themes[normalizeThemeID(id)]
	return ok
}

func normalizeThemeID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// themeColorsFor returns the vocabulary for one appearance.
func (t Theme) themeColorsFor(isDark bool) themeColors {
	if isDark {
		return t.Dark
	}
	return t.Light
}
