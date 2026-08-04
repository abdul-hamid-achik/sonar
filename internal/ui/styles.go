package ui

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// noColor detects NO_COLOR environment variable.
var noColor = os.Getenv("NO_COLOR") != ""

// The Nord hex values that used to live here are now one entry in the theme
// registry (internal/ui/theme.go) alongside every other scheme. Keeping a
// second copy here would be two places to change one color.

// semanticPalette is the single color vocabulary shared by the transcript,
// Bubbles components, overlays, composer, and tool receipts. Components own
// layout, but they should not invent a second meaning for the same state.
type semanticPalette struct {
	Background color.Color
	Dim        color.Color
	Muted      color.Color
	Text       color.Color
	Accent     color.Color
	Accent2    color.Color
	Error      color.Color
	Success    color.Color
	Special    color.Color
	Warning    color.Color
	Border     color.Color
}

// newSemanticPalette resolves the color vocabulary for one appearance.
//
// themeID is required, not variadic. A variadic theme reads nicely and shipped
// eight surfaces that silently painted in the default scheme while the rest of
// the frame used the selected one — the omission is invisible at the call site
// and invisible in review. An empty string still means "default", so a caller
// that genuinely has no theme says so explicitly.
func newSemanticPalette(isDark bool, themeID string) semanticPalette {
	colors := resolveTheme(resolveThemeID(themeID)).themeColorsFor(isDark)
	return semanticPalette{
		Background: lipgloss.Color(colors.Background),
		Dim:        lipgloss.Color(colors.Dim),
		Muted:      lipgloss.Color(colors.Muted),
		Text:       lipgloss.Color(colors.Text),
		Accent:     lipgloss.Color(colors.Accent),
		Accent2:    lipgloss.Color(colors.Accent2),
		Error:      lipgloss.Color(colors.Error),
		Success:    lipgloss.Color(colors.Success),
		Special:    lipgloss.Color(colors.Special),
		Warning:    lipgloss.Color(colors.Warning),
		Border:     lipgloss.Color(colors.Border),
	}
}

// resolveThemeID mirrors resolveGlyphProfile: the last non-empty value wins,
// and no value means the default.
func resolveThemeID(themeIDs ...string) string {
	for index := len(themeIDs) - 1; index >= 0; index-- {
		if id := normalizeThemeID(themeIDs[index]); id != "" {
			return id
		}
	}
	return defaultThemeID
}

func outputSemanticPalette(isDark bool, themeID string) semanticPalette {
	if !noColor {
		return newSemanticPalette(isDark, themeID)
	}
	plain := lipgloss.NoColor{}
	return semanticPalette{
		Background: plain,
		Dim:        plain, Muted: plain, Text: plain, Accent: plain, Accent2: plain,
		Error: plain, Success: plain, Special: plain, Warning: plain, Border: plain,
	}
}

// Styles holds all pre-built lipgloss styles.
type Styles struct {
	// Header

	// Messages
	UserContent lipgloss.Style
	UserGutter  lipgloss.Style

	// Tools
	ToolErrorText   lipgloss.Style
	ToolRunningText lipgloss.Style

	// Footer
	Divider        lipgloss.Style
	StatusDot      lipgloss.Style
	StatusText     lipgloss.Style
	StatusCheck    lipgloss.Style
	StatusError    lipgloss.Style
	StatusWarning  lipgloss.Style
	ApprovalPrompt lipgloss.Style
	StreamHint     lipgloss.Style
	ErrorText      lipgloss.Style
	ErrorChip      lipgloss.Style
	Dimmed         lipgloss.Style

	// System messages
	WelcomeHint lipgloss.Style

	// Completion popup

	// Completion modal
	CompletionFilter   lipgloss.Style
	CompletionCategory lipgloss.Style

	// Startup progress
	StartupDetail lipgloss.Style

	// Mode badges
	ModeAsk   lipgloss.Style
	ModePlan  lipgloss.Style
	ModeBuild lipgloss.Style

	// Context percentage fuel gauge
	ContextPctLow  lipgloss.Style
	ContextPctMid  lipgloss.Style
	ContextPctHigh lipgloss.Style

	// Tool type rendering

	// Diff view
	DiffAdded   lipgloss.Style
	DiffRemoved lipgloss.Style
	DiffContext lipgloss.Style
	DiffHeader  lipgloss.Style

	// Thinking display
	ThinkingHeader  lipgloss.Style
	ThinkingContent lipgloss.Style
	ThinkingBorder  lipgloss.Style

	// Shared overlay styles (used by help, model picker, sessions, plan form, completion)
	OverlayTitle  lipgloss.Style
	OverlayBorder color.Color
	OverlayAccent lipgloss.Style
	OverlayDim    lipgloss.Style

	// Focus indicators
	FocusIndicator lipgloss.Style
}

// NewStyles creates a Styles set based on the background color.
func NewStyles(isDark bool, themeID string) Styles {
	if noColor {
		return plainStyles()
	}
	return adaptiveStyles(isDark, themeID)
}

func adaptiveStyles(isDark bool, themeID string) Styles {
	// Body-muted colors must remain readable; border colors can be subtler.
	// LightDark keeps every semantic token adaptive without hardcoded ANSI.
	palette := newSemanticPalette(isDark, themeID)
	colorDim := palette.Dim
	colorMuted := palette.Muted
	colorText := palette.Text
	colorAccent := palette.Accent
	colorAccent2 := palette.Accent2
	colorError := palette.Error
	colorSuccess := palette.Success
	colorSpecial := palette.Special
	colorWarning := palette.Warning
	colorBorder := palette.Border
	// Inverse-chip text sits on a saturated state background, so it needs a
	// near-paper foreground in both themes rather than the ordinary text color.
	colorChipText := lipgloss.LightDark(isDark)(lipgloss.Color("#FBF7F5"), lipgloss.Color("#ECEFF4"))

	return Styles{

		UserContent: lipgloss.NewStyle().
			Foreground(colorText).
			PaddingLeft(2),
		UserGutter: lipgloss.NewStyle().
			Foreground(colorAccent2),

		ToolErrorText: lipgloss.NewStyle().
			Foreground(colorError),
		ToolRunningText: lipgloss.NewStyle().
			Foreground(colorAccent),

		Divider: lipgloss.NewStyle().
			Foreground(colorBorder),
		StatusDot: lipgloss.NewStyle().
			Foreground(colorAccent).
			PaddingLeft(1),
		StatusText: lipgloss.NewStyle().
			Foreground(colorDim),
		StatusCheck: lipgloss.NewStyle().
			Foreground(colorSuccess).
			PaddingLeft(1),
		StatusError: lipgloss.NewStyle().
			Foreground(colorError).
			PaddingLeft(1),
		// An expected operational posture (for example AUTO's skipped approval
		// prompts) is not a failure; red is reserved for errors and blockers.
		StatusWarning: lipgloss.NewStyle().
			Foreground(colorWarning),
		ApprovalPrompt: lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true),
		StreamHint: lipgloss.NewStyle().
			Foreground(colorDim).
			Italic(true),
		ErrorText: lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true).
			PaddingLeft(2),
		// An inverse chip marks failed operations without relying on red text
		// alone, which long tool output can visually bury.
		ErrorChip: lipgloss.NewStyle().
			Foreground(colorChipText).
			Background(colorError).
			Bold(true).
			Padding(0, 1),
		Dimmed: lipgloss.NewStyle().
			Foreground(colorDim),

		WelcomeHint: lipgloss.NewStyle().
			Foreground(colorAccent2).
			Bold(true),

		CompletionFilter: lipgloss.NewStyle().
			Foreground(colorText),
		CompletionCategory: lipgloss.NewStyle().
			Foreground(colorDim),

		StartupDetail: lipgloss.NewStyle().
			Foreground(colorDim),

		ModeAsk: lipgloss.NewStyle().
			Bold(true).
			Foreground(colorMuted),
		ModePlan: lipgloss.NewStyle().
			Bold(true).
			Foreground(colorSpecial),
		ModeBuild: lipgloss.NewStyle().
			Bold(true).
			Foreground(colorSuccess),

		ContextPctLow: lipgloss.NewStyle().
			Foreground(colorSuccess),
		ContextPctMid: lipgloss.NewStyle().
			Foreground(colorWarning),
		ContextPctHigh: lipgloss.NewStyle().
			Foreground(colorError),

		DiffAdded: lipgloss.NewStyle().
			Foreground(colorSuccess).
			PaddingLeft(6),
		DiffRemoved: lipgloss.NewStyle().
			Foreground(colorError).
			PaddingLeft(6),
		DiffContext: lipgloss.NewStyle().
			Foreground(colorDim).
			PaddingLeft(6),
		DiffHeader: lipgloss.NewStyle().
			Foreground(colorAccent).
			PaddingLeft(6),

		ThinkingHeader: lipgloss.NewStyle().
			Foreground(colorSpecial).
			Italic(true),
		ThinkingContent: lipgloss.NewStyle().
			Foreground(colorDim).
			PaddingLeft(4),
		ThinkingBorder: lipgloss.NewStyle().
			Foreground(colorBorder),

		OverlayTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAccent),
		OverlayBorder: colorBorder,
		OverlayAccent: lipgloss.NewStyle().
			Foreground(colorAccent2).
			Bold(true),
		OverlayDim: lipgloss.NewStyle().
			Foreground(colorDim),

		FocusIndicator: lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true),
	}
}

func plainStyles() Styles {
	p := lipgloss.NewStyle()
	b := lipgloss.NewStyle().Bold(true)
	pl2 := lipgloss.NewStyle().PaddingLeft(2)
	pl4 := lipgloss.NewStyle().PaddingLeft(4)
	return Styles{

		UserContent: pl2,
		UserGutter:  p,

		ToolErrorText:   b,
		ToolRunningText: p,

		Divider:        p,
		StatusDot:      p.PaddingLeft(1),
		StatusText:     p,
		StatusCheck:    p.PaddingLeft(1),
		StatusError:    p.PaddingLeft(1),
		StatusWarning:  p,
		ApprovalPrompt: b,
		StreamHint:     p.Italic(true),
		ErrorText:      b.PaddingLeft(2),
		ErrorChip:      b.Padding(0, 1),
		Dimmed:         p,

		WelcomeHint: b,

		CompletionFilter:   p,
		CompletionCategory: p,

		StartupDetail: p,

		ModeAsk:   b,
		ModePlan:  b,
		ModeBuild: b,

		ContextPctLow:  p,
		ContextPctMid:  p,
		ContextPctHigh: p,

		DiffAdded:   pl4,
		DiffRemoved: pl4,
		DiffContext: pl4,
		DiffHeader:  pl4,

		ThinkingHeader:  p.Italic(true),
		ThinkingContent: pl4,
		ThinkingBorder:  p,

		OverlayTitle:  b,
		OverlayBorder: lipgloss.NoColor{},
		OverlayAccent: b,
		OverlayDim:    p,

		FocusIndicator: b,
	}
}

func ruleWithGlyphProfile(width int, profile GlyphProfile) string {
	if width < 1 {
		return ""
	}
	return strings.Repeat(glyphSet(resolveGlyphProfile(profile)).Horizontal, width)
}
