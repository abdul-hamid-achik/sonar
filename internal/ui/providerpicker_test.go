package ui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestProviderPickerResizeThemeAndReducedMotion(t *testing.T) {
	previousNoColor := noColor
	noColor = false
	t.Cleanup(func() { noColor = previousNoColor })

	m := newTestModel(t)
	m.reducedMotion = true
	manager := llm.NewModelManager("http://localhost:11434", 4096)
	t.Cleanup(manager.Close)
	profiles := make(map[string]config.ProviderProfile, 8)
	for index := range 8 {
		name := fmt.Sprintf("local-%d", index)
		profiles[name] = config.ProviderProfile{
			Type:  config.ProviderTypeOllama,
			Model: fmt.Sprintf("model-%d", index),
		}
	}
	manager.ConfigureProviderCatalog(config.ProviderConfig{
		Active:   "local-3",
		Profiles: profiles,
	}, false, "model-3")
	m.modelManager = manager

	m.openProviderPicker()
	if m.providerPickerState == nil {
		t.Fatal("provider picker did not open")
	}
	if m.providerPickerState.List.FilterInput.Styles().Cursor.Blink {
		t.Fatal("reduced motion left the provider filter cursor blinking")
	}
	m.providerPickerState.List.Select(5)
	selected := m.providerPickerState.List.SelectedItem().(providerItem).presentation.ProfileID

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 36, Height: 12})
	m = updated.(*Model)
	if got, want := m.providerPickerState.List.Width(), pickerListWidth(36); got != want {
		t.Fatalf("provider list width after resize = %d, want %d", got, want)
	}
	if got, want := m.providerPickerState.List.Height(), pickerListHeight(12, 8*defaultPickerItemHeight+2, 4); got != want {
		t.Fatalf("provider list height after resize = %d, want %d", got, want)
	}
	if got := m.providerPickerState.List.SelectedItem().(providerItem).presentation.ProfileID; got != selected {
		t.Fatalf("resize changed provider selection from %q to %q", selected, got)
	}

	updated, _ = m.Update(tea.BackgroundColorMsg{Color: color.White})
	m = updated.(*Model)
	if m.providerPickerState.List.FilterInput.Styles().Cursor.Blink {
		t.Fatal("theme change re-enabled the reduced-motion provider cursor")
	}
	assertSameColor(
		t,
		"provider title",
		m.providerPickerState.List.Styles.Title.GetForeground(),
		newSemanticPalette(false, defaultThemeID).Accent,
	)
	assertRenderedLinesFit(t, m.renderProviderPicker(), 36)
	assertRenderedHeightFits(t, m.renderProviderPicker(), 12)
}

func TestProviderPickerWheelMovesVisibleCursorWithoutScrollingTranscript(t *testing.T) {
	m := newTestModel(t)
	descriptors := make([]llm.ProviderDescriptor, 8)
	for index := range descriptors {
		descriptors[index] = llm.ProviderDescriptor{
			Name:  fmt.Sprintf("provider-%d", index),
			Type:  "ollama",
			Model: fmt.Sprintf("model-%d", index),
		}
	}
	m.providerPickerState = newProviderPickerState(descriptors, "", m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlayProviderPicker
	m.setTestTranscriptContent("one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten")
	m.transcriptGotoTop()

	x, y := providerPickerItemPoint(t, m, 0)
	updated, _ := m.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	m = updated.(*Model)
	if got := m.providerPickerState.List.Index(); got != 1 {
		t.Fatalf("provider cursor after wheel = %d, want 1", got)
	}
	if got := m.transcriptYOffset(); got != 0 {
		t.Fatalf("provider wheel moved hidden transcript to offset %d", got)
	}

	projection, ok := m.projectProviderPickerPointer()
	if !ok {
		t.Fatal("provider picker has no pointer projection")
	}
	localY := y - projection.startY
	outsideX := projection.rowRect(localY).MaxX
	updated, _ = m.Update(tea.MouseWheelMsg{X: outsideX, Y: y, Button: tea.MouseWheelDown})
	m = updated.(*Model)
	if got := m.providerPickerState.List.Index(); got != 1 {
		t.Fatalf("wheel outside overlay changed provider cursor to %d", got)
	}
	if got := m.transcriptYOffset(); got != 0 {
		t.Fatalf("outside provider wheel moved hidden transcript to offset %d", got)
	}
}

func TestProviderPickerClickActivatesExactFilteredUnicodeItem(t *testing.T) {
	t.Setenv("XAI_API_KEY", "test-key")
	m := newTestModel(t)
	manager := llm.NewModelManager("http://127.0.0.1:9", 4096)
	t.Cleanup(manager.Close)

	targetName := "xai-日本"
	profiles := map[string]config.ProviderProfile{
		"ollama": {Type: config.ProviderTypeOllama, Model: "qwen3.5:2b"},
		targetName: {
			Type:  config.ProviderTypeXAI,
			Model: "grok-4",
		},
	}
	for index := range 6 {
		profiles[fmt.Sprintf("local-%d", index)] = config.ProviderProfile{
			Type:  config.ProviderTypeOllama,
			Model: fmt.Sprintf("model-%d", index),
		}
	}
	manager.ConfigureProviderCatalog(config.ProviderConfig{
		Active:   "ollama",
		Profiles: profiles,
	}, false, "qwen3.5:2b")
	m.modelManager = manager
	m.model = "qwen3.5:2b"
	m.openProviderPicker()
	m.providerPickerState.List.SetFilterText("日本")

	visible := m.providerPickerState.List.VisibleItems()
	if len(visible) != 1 {
		t.Fatalf("filtered providers = %d, want 1", len(visible))
	}
	item := visible[0].(providerItem)
	if item.presentation.ProfileID != ProviderProfileID(targetName) {
		t.Fatalf("filtered provider identity = %q, want %q", item.presentation.ProfileID, targetName)
	}

	x, y := providerPickerItemPoint(t, m, 0)
	updated, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = updated.(*Model)
	if cmd == nil || !m.providerSwitchRunning || m.overlay != OverlayNone || m.providerPickerState != nil {
		t.Fatalf(
			"filtered pointer selection did not start switch: cmd=%v running=%v overlay=%v state=%v",
			cmd != nil,
			m.providerSwitchRunning,
			m.overlay,
			m.providerPickerState != nil,
		)
	}
	result := awaitCommandMessage[providerSwitchResultMsg](t, commandMessages(cmd), 2*time.Second)
	if result.Name != targetName || result.Err != nil {
		t.Fatalf("pointer switch receipt = name %q err %v", result.Name, result.Err)
	}
	updated, _ = m.Update(result)
	m = updated.(*Model)
	if got := m.activeProviderName(); got != targetName {
		t.Fatalf("pointer activated provider %q, want %q", got, targetName)
	}
}

func TestProviderPickerPointerFailsClosedOutsideItemsAndEmptyFilter(t *testing.T) {
	m := newTestModel(t)
	descriptors := make([]llm.ProviderDescriptor, 7)
	for index := range descriptors {
		descriptors[index] = llm.ProviderDescriptor{Name: fmt.Sprintf("provider-%d", index), Type: "ollama"}
	}
	m.providerPickerState = newProviderPickerState(descriptors, "", m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlayProviderPicker
	x, y := providerPickerItemPoint(t, m, 0)

	updated, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})
	m = updated.(*Model)
	if cmd != nil || m.providerPickerState == nil || m.providerSwitchRunning {
		t.Fatal("non-left provider click escaped the picker boundary")
	}

	projection, ok := m.projectProviderPickerPointer()
	if !ok {
		t.Fatal("provider picker has no pointer projection")
	}
	outsideX := projection.rowRect(y - projection.startY).MaxX
	updated, cmd = m.Update(tea.MouseClickMsg{X: outsideX, Y: y, Button: tea.MouseLeft})
	m = updated.(*Model)
	if cmd != nil || m.providerPickerState == nil || m.providerSwitchRunning {
		t.Fatal("provider click outside overlay selected an item")
	}

	m.providerPickerState.List.SetFilterText("no-provider-can-match-this")
	updated, cmd = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = updated.(*Model)
	if cmd != nil || m.providerPickerState == nil || m.providerSwitchRunning {
		t.Fatal("provider click selected a stale row after an empty filter")
	}
}

func TestProviderItemSanitizesTerminalControlsWithoutChangingIdentity(t *testing.T) {
	rawName := "xai-日本\x1b]52;c;NAME_SECRET\x07\nspoof\u202e"
	rawModel := "grok-4\x1b]0;MODEL_SECRET\x07\tfast\u2066"
	rawType := "xai\x1b[2J\nTYPE_SPOOF\u202d"
	rawAPIKeyEnv := "XAI_API_KEY\x1b]8;;https://ENV_SECRET.invalid\x07link\x1b]8;;\x07\nENV_SPOOF\u2069"
	descriptor := llm.ProviderDescriptor{
		Name: rawName, Model: rawModel, Type: rawType, APIKeyEnv: rawAPIKeyEnv, Remote: true,
	}
	presentations := providerOptionPresentations([]llm.ProviderDescriptor{descriptor})
	if len(presentations) != 1 {
		t.Fatalf("provider presentations = %d, want 1", len(presentations))
	}
	item := providerItem{presentation: presentations[0]}

	for label, value := range map[string]string{
		"title": item.Title(), "description": item.Description(), "filter": item.FilterValue(),
	} {
		for _, secret := range []string{"NAME_SECRET", "MODEL_SECRET", "ENV_SECRET"} {
			if strings.Contains(value, secret) {
				t.Fatalf("%s retained OSC payload %q: %q", label, secret, value)
			}
		}
		if strings.ContainsAny(value, "\x1b\r\n\t") {
			t.Fatalf("%s retained a terminal or row control: %q", label, value)
		}
		for _, character := range value {
			if unicode.IsControl(character) || isBidiControl(character) {
				t.Fatalf("%s retained unsafe rune %U: %q", label, character, value)
			}
		}
	}
	if item.presentation.ProfileID != ProviderProfileID(rawName) {
		t.Fatal("display sanitization changed opaque provider selection identity")
	}
	if item.presentation.CredentialHint != "" {
		t.Fatalf("unsafe credential hint crossed UI boundary: %q", item.presentation.CredentialHint)
	}

	m := newTestModel(t)
	m.providerPickerState = newProviderPickerState([]llm.ProviderDescriptor{descriptor}, "", m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlayProviderPicker
	plain := ansi.Strip(m.renderProviderPicker())
	for _, secret := range []string{"NAME_SECRET", "MODEL_SECRET", "ENV_SECRET"} {
		if strings.Contains(plain, secret) {
			t.Fatalf("rendered provider picker retained OSC payload %q:\n%s", secret, plain)
		}
	}
}

func TestProviderPickerUnicodeHitTestingAtMinimumTerminal(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	descriptors := []llm.ProviderDescriptor{
		{Name: "日本語🤖", Type: "xai", Model: "grok-4"},
		{Name: "ollama", Type: "ollama", Model: "qwen"},
	}
	m.providerPickerState = newProviderPickerState(descriptors, "", m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlayProviderPicker

	rendered := m.renderProviderPicker()
	assertRenderedLinesFit(t, rendered, minTerminalWidth)
	assertRenderedHeightFits(t, rendered, minTerminalHeight)
	x, y := providerPickerItemPoint(t, m, 0)
	item, index, ok := m.providerPickerItemAt(x, y)
	if !ok || index != 0 || item.presentation.ProfileID != "日本語🤖" {
		t.Fatalf("minimum-terminal hit = item %#v index %d ok %v", item, index, ok)
	}
	projection, _ := m.projectProviderPickerPointer()
	row := projection.rowRect(y - projection.startY)
	if _, _, ok := m.providerPickerItemAt(row.MaxX, y); ok {
		t.Fatal("exclusive provider row edge was accepted")
	}
}

func TestProviderPickerHitTestingUsesVisiblePageOffset(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: minTerminalHeight})
	m = updated.(*Model)
	descriptors := make([]llm.ProviderDescriptor, 12)
	for index := range descriptors {
		descriptors[index] = llm.ProviderDescriptor{
			Name:  fmt.Sprintf("provider-%02d", index),
			Type:  "ollama",
			Model: fmt.Sprintf("model-%02d", index),
		}
	}
	m.providerPickerState = newProviderPickerState(descriptors, "", m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlayProviderPicker
	perPage := m.providerPickerState.List.Paginator.PerPage
	if perPage < 1 || perPage >= len(descriptors) {
		t.Fatalf("test requires provider pagination, per-page = %d", perPage)
	}
	m.providerPickerState.List.Select(perPage)

	x, y := providerPickerItemPoint(t, m, 1)
	item, index, ok := m.providerPickerItemAt(x, y)
	wantIndex := perPage + 1
	if !ok || index != wantIndex || item.presentation.ProfileID != ProviderProfileID(descriptors[wantIndex].Name) {
		t.Fatalf(
			"page-offset hit = item %q index %d ok %v, want item %q index %d",
			item.presentation.ProfileID,
			index,
			ok,
			descriptors[wantIndex].Name,
			wantIndex,
		)
	}
}

func TestProviderPickerPreservesPausedTranscriptAnchor(t *testing.T) {
	t.Setenv("XAI_API_KEY", "test-key")
	m := newTestModel(t)
	manager := llm.NewModelManager("http://localhost:11434", 4096)
	t.Cleanup(manager.Close)
	manager.ConfigureProviderCatalog(config.ProviderConfig{
		Active: "ollama",
		Profiles: map[string]config.ProviderProfile{
			"ollama": {
				Type:  config.ProviderTypeOllama,
				Model: "qwen3.5:2b",
			},
			"xai": {
				Type:    config.ProviderTypeXAI,
				BaseURL: "https://api.x.ai/v1",
				Model:   "grok-4",
			},
		},
	}, false, "qwen3.5:2b")
	m.modelManager = manager
	m.model = "qwen3.5:2b"
	for index := range 80 {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: fmt.Sprintf("history line %03d", index),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.setTranscriptYOffset(7)
	m.pauseFollow()

	assertAnchor := func(stage string) {
		t.Helper()
		if got := m.transcriptYOffset(); got != 7 || !m.followPaused() {
			t.Fatalf("%s moved paused transcript: offset=%d paused=%v", stage, got, m.followPaused())
		}
	}

	m.openProviderPicker()
	assertAnchor("open")
	m.closeProviderPicker()
	assertAnchor("close")
	m.openProviderPicker()
	cmd := m.selectProviderProfile("xai")
	if cmd == nil || !m.providerSwitchRunning {
		t.Fatalf("provider selection did not start an asynchronous switch: cmd=%v running=%v", cmd != nil, m.providerSwitchRunning)
	}
	assertAnchor("confirm start")
	result := awaitCommandMessage[providerSwitchResultMsg](t, commandMessages(cmd), 2*time.Second)
	updated, _ := m.Update(result)
	m = updated.(*Model)
	assertAnchor("confirm receipt")
	if got := m.activeProviderName(); got != "xai" {
		t.Fatalf("active provider after confirm = %q, want xai", got)
	}
}

func providerPickerItemPoint(t *testing.T, m *Model, rowOnPage int) (int, int) {
	t.Helper()
	projection, ok := m.projectProviderPickerPointer()
	if !ok {
		t.Fatal("provider picker has no pointer projection")
	}
	state := m.providerPickerState
	localY := 1 + providerPickerTitleRows(state) + rowOnPage*(state.ItemHeight+max(0, state.ItemSpacing))
	rect := Inset(projection.rowRect(localY), Insets{Left: pickerFrameCursorX, Right: pickerFrameCursorX})
	if rect.Empty() {
		t.Fatalf("provider row %d has empty pointer rectangle: %#v", rowOnPage, rect)
	}
	return rect.MinX, rect.MinY
}
