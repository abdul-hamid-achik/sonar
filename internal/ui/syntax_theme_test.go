package ui

import "testing"

// Glamour ships fixed syntax colours — #00AAFF keywords, #676767 comments,
// #C69669 strings — so a fenced code block rendered identically on all ten
// schemes. Switching to Gruvbox or Catppuccin recoloured the whole TUI except
// the inside of a code block, which is where a reader spends most of their
// attention.
func TestSyntaxHighlightingFollowsTheActiveScheme(t *testing.T) {
	seen := make(map[string][]string)
	for _, themeID := range themeIDs() {
		style := markdownStyleConfig(true, themeID)
		chroma := style.CodeBlock.Chroma
		if chroma == nil {
			t.Fatalf("%s: code blocks have no syntax colours", themeID)
		}
		palette := newSemanticPalette(true, themeID)

		for _, check := range []struct {
			name string
			got  *string
			want string
		}{
			{"comment", chroma.Comment.Color, colorHex(palette.Dim)},
			{"keyword", chroma.Keyword.Color, colorHex(palette.Special)},
			{"type", chroma.KeywordType.Color, colorHex(palette.Accent2)},
			{"function", chroma.NameFunction.Color, colorHex(palette.Accent)},
			{"string", chroma.LiteralString.Color, colorHex(palette.Success)},
			{"number", chroma.LiteralNumber.Color, colorHex(palette.Warning)},
			{"error", chroma.Error.Color, colorHex(palette.Error)},
		} {
			if check.got == nil {
				t.Errorf("%s: %s has no colour", themeID, check.name)
				continue
			}
			if *check.got != check.want {
				t.Errorf("%s: %s = %s, want %s", themeID, check.name, *check.got, check.want)
			}
		}
		key := *chroma.Keyword.Color + *chroma.LiteralString.Color + *chroma.Comment.Color
		seen[key] = append(seen[key], themeID)
	}

	// Distinct schemes must not collapse onto one set of syntax colours. A
	// single shared triple would mean the lookup ran but the answer never
	// varied — which is the state this replaces.
	if len(seen) < 2 {
		t.Errorf("all %d schemes share one syntax palette", len(themeIDs()))
	}
}

// None of Glamour's fixed values may survive anywhere in the mapping. They are
// the fingerprints of the stock theme, and finding one means a class was
// missed rather than deliberately left alone.
func TestNoStockSyntaxColoursRemain(t *testing.T) {
	stock := map[string]string{
		"#00AAFF": "keyword blue",
		"#676767": "comment grey",
		"#C69669": "string tan",
		"#00D787": "function green",
	}
	for _, themeID := range themeIDs() {
		chroma := markdownStyleConfig(true, themeID).CodeBlock.Chroma
		for name, value := range map[string]*string{
			"comment":  chroma.Comment.Color,
			"keyword":  chroma.Keyword.Color,
			"string":   chroma.LiteralString.Color,
			"function": chroma.NameFunction.Color,
			"number":   chroma.LiteralNumber.Color,
			"text":     chroma.Text.Color,
		} {
			if value == nil {
				continue
			}
			if label, isStock := stock[*value]; isStock {
				t.Errorf("%s: %s kept Glamour's %s (%s)", themeID, name, label, *value)
			}
		}
	}
}

// Comments must stay readable. Dim is the correct role for them — secondary,
// not competing — but a scheme whose Dim disappears into its own background
// would make code unreadable rather than calm.
func TestCommentsStayLegibleInEveryScheme(t *testing.T) {
	for _, themeID := range themeIDs() {
		for _, isDark := range []bool{true, false} {
			palette := newSemanticPalette(isDark, themeID)
			chroma := markdownStyleConfig(isDark, themeID).CodeBlock.Chroma
			if chroma.Comment.Color == nil {
				t.Fatalf("%s dark=%v: comments have no colour", themeID, isDark)
			}
			if *chroma.Comment.Color == colorHex(palette.Background) {
				t.Errorf("%s dark=%v: comments match the page background", themeID, isDark)
			}
		}
	}
}
