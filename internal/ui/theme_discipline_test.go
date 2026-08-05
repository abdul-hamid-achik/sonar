package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A hex literal outside the theme registry is a colour that cannot follow
// /theme. Three shipped that way and each was found by accident rather than by
// looking: the wait animation's gradient, markdown's inline-code background,
// and the session header band — the last two literally commented "nord snow
// storm 2" and "nord polar night 1" while the text drawn over them followed
// the scheme correctly.
//
// theme.go is the registry and styles.go projects it; everywhere else must ask
// the palette. This is cheaper to enforce than to rediscover.
func TestNoHardcodedColoursOutsideTheThemeRegistry(t *testing.T) {
	hexColor := regexp.MustCompile(`(?:lipgloss\.)?Color\("#[0-9A-Fa-f]{3,8}"\)`)
	owners := map[string]bool{"theme.go": true, "styles.go": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || owners[name] {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for lineNumber, line := range strings.Split(string(body), "\n") {
			if match := hexColor.FindString(line); match != "" {
				t.Errorf("%s:%d hardcodes %s; ask newSemanticPalette instead so it follows /theme",
					name, lineNumber+1, match)
			}
		}
	}
}

// The session header band and inline code both mean "a surface lifted off the
// page", and both must resolve from the scheme rather than resembling one.
func TestElevatedSurfacesComeFromTheScheme(t *testing.T) {
	for _, themeID := range themeIDs() {
		for _, isDark := range []bool{true, false} {
			palette := newSemanticPalette(isDark, themeID)
			background := colorHex(palette.Background)

			if got := colorHex(palette.Border); got == background {
				t.Errorf("%s dark=%v: Border matches Background, so an elevated band would vanish",
					themeID, isDark)
			}
			style := markdownStyleConfig(isDark, themeID)
			if style.Code.BackgroundColor == nil || *style.Code.BackgroundColor != colorHex(palette.Border) {
				t.Errorf("%s dark=%v: inline code background drifted from the scheme border",
					themeID, isDark)
			}
		}
	}
}
