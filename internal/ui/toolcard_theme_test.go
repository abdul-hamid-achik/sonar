package ui

import (
	"image/color"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

func TestNewToolCardStylesUsesLightDarkPalette(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	tests := []struct {
		name   string
		isDark bool
	}{
		{name: "light"},
		{name: "dark", isDark: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			styles := NewToolCardStyles(tt.isDark, defaultThemeID)
			palette := newSemanticPalette(tt.isDark, defaultThemeID)
			got := map[string]lipgloss.Style{
				"border running":   styles.BorderRunning,
				"border success":   styles.BorderSuccess,
				"border attention": styles.BorderAttention,
				"border error":     styles.BorderError,
				"title running":    styles.TitleRunning,
				"title success":    styles.TitleSuccess,
				"title attention":  styles.TitleAttention,
				"title error":      styles.TitleError,
				"args":             styles.Args,
				"result":           styles.Result,
				"error":            styles.Error,
				"warning":          styles.Warning,
				"dimmed":           styles.Dimmed,
				"elapsed":          styles.Elapsed,
				"diff added":       styles.DiffAdded,
				"diff removed":     styles.DiffRemoved,
				"diff header":      styles.DiffHeader,
				"search path":      styles.SearchPath,
				"search location":  styles.SearchLocation,
				"search match":     styles.SearchMatch,
			}
			want := map[string]color.Color{
				"border running": palette.Accent2, "border success": palette.Success,
				"border attention": palette.Warning, "border error": palette.Error,
				"title running": palette.Accent, "title success": palette.Success,
				"title attention": palette.Warning, "title error": palette.Error,
				"args": palette.Muted, "result": palette.Muted, "error": palette.Error,
				"warning": palette.Warning, "dimmed": palette.Dim, "elapsed": palette.Accent2,
				"diff added": palette.Success, "diff removed": palette.Error,
				"diff header": palette.Accent, "search path": palette.Accent,
				"search location": palette.Dim, "search match": palette.Special,
			}

			for name, style := range got {
				assertToolCardForeground(t, name, style, want[name])
			}
		})
	}
}

func TestToolCardManagerSetDarkUpdatesStylesAndPreservesCallID(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	mgr := NewToolCardManager(false)
	mgr.AddCardWithID("call-42", "read_file", ToolCardFile, time.Now())

	if len(mgr.Cards) != 1 {
		t.Fatalf("card count = %d, want 1", len(mgr.Cards))
	}
	assertToolCardForeground(t, "light running title", mgr.Cards[0].Styles.TitleRunning,
		newSemanticPalette(false, defaultThemeID).Accent)

	mgr.SetDark(true, defaultThemeID)

	card := mgr.Cards[0]
	if card.ID != "call-42" {
		t.Fatalf("call ID = %q, want %q", card.ID, "call-42")
	}
	if !mgr.IsDark {
		t.Fatal("manager should use the dark theme")
	}
	assertToolCardForeground(t, "dark running title", card.Styles.TitleRunning,
		newSemanticPalette(true, defaultThemeID).Accent)
}

func assertToolCardForeground(t *testing.T, name string, style lipgloss.Style, want color.Color) {
	t.Helper()

	got := style.GetForeground()
	gotR, gotG, gotB, gotA := got.RGBA()
	wantR, wantG, wantB, wantA := want.RGBA()
	if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
		t.Errorf("%s foreground = %#v, want %#v", name, got, want)
	}
}
