package ui

import (
	"fmt"
	"testing"
)

func TestFocusedComposerOwnsEveryPrintableFirstCharacter(t *testing.T) {
	characters := make([]rune, 0, 98)
	for character := rune(' '); character <= rune('~'); character++ {
		characters = append(characters, character)
	}
	characters = append(characters, 'é', '你', '🙂')

	for _, character := range characters {
		t.Run(fmt.Sprintf("%U", character), func(t *testing.T) {
			m := newTestModel(t)
			m.toolEntries = []ToolEntry{{
				Name: "write_file", Status: ToolStatusDone, Collapsed: true,
			}}
			m.lastTurnToolIndex = 0
			beforeToolsCollapsed := m.toolsCollapsed

			updated, _ := m.Update(charKey(character))
			m = updated.(*Model)

			if got, want := m.input.Value(), string(character); got != want {
				t.Fatalf("first printable character = %q, want %q", got, want)
			}
			if m.overlay == OverlayHelp {
				t.Fatal("printable composer input opened Help")
			}
			if m.toolsCollapsed != beforeToolsCollapsed {
				t.Fatal("printable composer input toggled all ToolCards")
			}
			if !m.toolEntries[0].Collapsed {
				t.Fatal("printable composer input expanded a ToolCard")
			}
			if m.viewerModalActive() {
				t.Fatal("printable composer input opened a receipt viewer")
			}
		})
	}
}
