package ui

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestOllamaModelPickerGroupsAndProjectsMetadata(t *testing.T) {
	models := []OllamaModelDescriptor{
		{Name: "blocked", Source: OllamaModelLocal, Selectable: false, Fit: true, Reason: "embeddings only"},
		{Name: "cloud-code", Source: OllamaModelCloud, ParameterSize: "120B", ContextLength: 131072, Capabilities: []string{"completion", "tools", "thinking"}, Selectable: true, Fit: true},
		{Name: "local-code", Source: OllamaModelLocal, SizeBytes: 4 * 1024 * 1024 * 1024, ParameterSize: "4B", Quantization: "Q4_K_M", ContextLength: 65536, EffectiveContext: 16384, Capabilities: []string{"completion", "tools"}, Current: true, Running: true, Selectable: true, Fit: true},
	}
	state := newOllamaModelPickerState(models, "local-code", 100, 40, true, defaultThemeID)
	if state.List.Index() != 1 {
		t.Fatalf("current local index = %d, want 1", state.List.Index())
	}
	items := state.List.Items()
	if len(items) != 3 {
		t.Fatalf("items = %d", len(items))
	}
	blocked := items[0].(modelItem)
	local := items[1].(modelItem)
	cloud := items[2].(modelItem)
	if !strings.HasPrefix(local.Title(), "LOCAL ·") || !strings.Contains(local.Title(), "current") || !strings.Contains(local.Description(), "4.0 GB · 4B · Q4_K_M · tools+completion · 65K max ctx · 16K effective") {
		t.Fatalf("local projection = %q / %q", local.Title(), local.Description())
	}
	if !strings.HasPrefix(cloud.Title(), "CLOUD ·") || !strings.Contains(cloud.Title(), "available") {
		t.Fatalf("cloud title = %q", cloud.Title())
	}
	if !strings.HasPrefix(blocked.Title(), "LOCAL ·") || !strings.Contains(blocked.Title(), "unavailable") || blocked.Description() != "" {
		t.Fatalf("blocked projection = %q / %q", blocked.Title(), blocked.Description())
	}
}

func TestOllamaModelDisplayProjectionStripsTerminalControlsWithoutChangingIdentity(t *testing.T) {
	rawName := "qwen\x1b]52;c;NAME_SECRET\x07\n:cloud\u202e"
	rawDisplayName := "Qwen\x1b]0;DISPLAY_SECRET\x07\x1b[2J\nCloud\u2066"
	rawReason := "Review\x1b]8;;https://REASON_SECRET.invalid\x07 link\x1b]8;;\x07\nrequired\u202e"
	descriptor := OllamaModelDescriptor{
		Name: rawName, DisplayName: rawDisplayName, Source: OllamaModelCloud,
		ParameterSize: "120B\x1b]52;c;PARAM_SECRET\x07\nclass", Quantization: "Q4\x1b[31m\u009b2J\nK_M\u2066",
		Capabilities: []string{"tools", "thinking", "completion"},
		Selectable:   true, Fit: true, RequiresConsent: true, Reason: rawReason,
	}
	state := newOllamaModelPickerState([]OllamaModelDescriptor{descriptor}, rawName, 100, 30, true, defaultThemeID)
	state.Notice = "Refresh\x1b]0;NOTICE_SECRET\x07\x1b[2J\nfailed\u2066"
	state.List.Title = ollamaModelPickerTitle("0.31\x1b]0;VERSION_SECRET\x07\n.2\u202e")
	item := state.List.Items()[0].(modelItem)

	for name, value := range map[string]string{
		"title":        item.Title(),
		"description":  item.Description(),
		"filter":       item.FilterValue(),
		"reason":       modelDecisionReason(descriptor),
		"picker title": state.List.Title,
	} {
		if strings.ContainsAny(value, "\x1b\r\n\t") {
			t.Fatalf("%s retained a terminal or row control: %q", name, value)
		}
		for _, character := range value {
			if unicode.IsControl(character) || isBidiControl(character) {
				t.Fatalf("%s retained unsafe rune %U: %q", name, character, value)
			}
		}
		for _, secret := range []string{"DISPLAY_SECRET", "PARAM_SECRET", "REASON_SECRET", "VERSION_SECRET"} {
			if strings.Contains(value, secret) {
				t.Fatalf("%s retained OSC payload %q: %q", name, secret, value)
			}
		}
	}

	m := newTestModel(t)
	m.width, m.height = 100, 30
	m.modelPickerState = state
	m.overlay = OverlayModelPicker
	wideDetails := renderOllamaModelDetails(descriptor, 72, m.isDark, defaultThemeID)
	compactDetails := renderCompactOllamaModelDetails(descriptor, 32, m.isDark, defaultThemeID)
	detail := m.renderModelSelectionDetail(state, 72)
	consent := newCloudConsentState(descriptor, 72, 24, m.isDark, defaultThemeID)
	for name, rendered := range map[string]string{
		"picker":          m.renderModelPicker(),
		"selected detail": detail,
		"wide details":    wideDetails,
		"compact details": compactDetails,
		"cloud consent":   consent.List.View(),
	} {
		plain := ansi.Strip(rendered)
		for _, secret := range []string{"DISPLAY_SECRET", "PARAM_SECRET", "REASON_SECRET", "NOTICE_SECRET"} {
			if strings.Contains(plain, secret) {
				t.Fatalf("%s retained OSC payload %q:\n%s", name, secret, plain)
			}
		}
		if strings.Contains(plain, "Review\n") || strings.Contains(plain, "Qwen\nCloud") {
			t.Fatalf("%s allowed metadata to create a row:\n%s", name, plain)
		}
		for _, character := range plain {
			if character == '\n' {
				continue
			}
			if unicode.IsControl(character) || isBidiControl(character) {
				t.Fatalf("%s retained unsafe rune %U:\n%s", name, character, plain)
			}
		}
	}

	selected, ok := state.SelectedDescriptor()
	if !ok || selected.Name != rawName || selected.DisplayName != rawDisplayName || selected.Reason != rawReason {
		t.Fatalf("display sanitization changed selection identity: %#v", selected)
	}
	if consent.ModelName != rawName {
		t.Fatalf("cloud consent changed network identifier: %q", consent.ModelName)
	}
}

func TestOllamaModelPickerNarrowAndFilterable(t *testing.T) {
	state := newOllamaModelPickerState([]OllamaModelDescriptor{{Name: "qwen", Source: OllamaModelLocal, Selectable: true, Fit: true}}, "", 32, 12, false, defaultThemeID)
	if got := state.List.Width(); got > pickerListWidth(32) {
		t.Fatalf("width = %d", got)
	}
	if !state.Compact || state.ItemHeight != 1 {
		t.Fatalf("narrow density = compact %v, item height %d", state.Compact, state.ItemHeight)
	}
}

func TestOllamaModelPickerFilterKeepsSourceOnEveryResult(t *testing.T) {
	state := newOllamaModelPickerState([]OllamaModelDescriptor{
		{Name: "local-code", Source: OllamaModelLocal, Selectable: true, Fit: true},
		{Name: "cloud-first", Source: OllamaModelCloud, Selectable: true, Fit: true, RequiresConsent: true},
		{Name: "cloud-target", Source: OllamaModelCloud, Selectable: true, Fit: true, RequiresConsent: true},
	}, "", 100, 30, false, defaultThemeID)
	state.List.SetFilterText("cloud-target")
	visible := state.List.VisibleItems()
	if len(visible) != 1 {
		t.Fatalf("visible items = %d, want 1", len(visible))
	}
	title := visible[0].(modelItem).Title()
	if !strings.HasPrefix(title, "CLOUD · cloud-target") || !strings.Contains(title, "review") {
		t.Fatalf("filtered title lost source/state: %q", title)
	}
}

func TestOllamaModelPickerFitsWideAndMinimumTerminals(t *testing.T) {
	const reason = "conversation confirmation required before prompts leave this machine"
	models := []OllamaModelDescriptor{
		{Name: "qwen3.5:2b", Source: OllamaModelLocal, Current: true, Running: true, Selectable: true, Fit: true},
		{Name: "kimi-code:cloud", Source: OllamaModelCloud, ParameterSize: "120B", ContextLength: 1_048_576, Capabilities: []string{"completion", "tools", "thinking"}, Selectable: true, Fit: true, RequiresConsent: true, Reason: reason},
	}
	for _, size := range []struct {
		width, height int
		compact       bool
	}{{100, 30, false}, {30, 12, true}} {
		m := newTestModel(t)
		m.width, m.height = size.width, size.height
		m.modelPickerState = newOllamaModelPickerState(models, "qwen3.5:2b", size.width, size.height, m.isDark, defaultThemeID)
		m.modelPickerState.List.Select(1)
		m.overlay = OverlayModelPicker

		view := m.renderModelPicker()
		plain := strings.Join(strings.Fields(ansi.Strip(view)), " ")
		detail := strings.Join(strings.Fields(ansi.Strip(m.renderModelSelectionDetail(m.modelPickerState, pickerListWidth(size.width)))), " ")
		if !strings.Contains(detail, reason) || strings.Contains(detail, "…") {
			t.Fatalf("%dx%d decision reason was truncated: %q", size.width, size.height, detail)
		}
		contextLabel := "1048K max ctx"
		if size.compact {
			contextLabel = "1048K ctx"
		}
		for _, want := range []string{"120B", "tools · thinking · completion", contextLabel} {
			if !strings.Contains(detail, want) {
				t.Fatalf("%dx%d selected metadata missing %q: %q", size.width, size.height, want, detail)
			}
		}
		for _, want := range []string{"CLOUD", "review required", "esc", "enter", "/"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%dx%d picker missing %q:\n%s", size.width, size.height, want, ansi.Strip(view))
			}
		}
		if m.modelPickerState.Compact != size.compact {
			t.Fatalf("%dx%d compact = %v, want %v", size.width, size.height, m.modelPickerState.Compact, size.compact)
		}
		assertRenderedLinesFit(t, view, size.width)
		assertRenderedHeightFits(t, view, size.height)
		if size.width == 100 {
			if m.modelPickerState.List.Width() <= 60 {
				t.Fatalf("wide picker did not use available width: %d", m.modelPickerState.List.Width())
			}
			for _, want := range []string{"/ filter", "d details", "a add", "r refresh"} {
				if !strings.Contains(plain, want) {
					t.Fatalf("wide picker missing hint %q: %s", want, plain)
				}
			}
		}
	}
}

func TestOllamaModelPickerRestyleKeepsLocalAndCloudRowsVisible(t *testing.T) {
	models := []OllamaModelDescriptor{
		{Name: "qwen3.5:2b", Source: OllamaModelLocal, Current: true, Running: true, Selectable: true, Fit: true},
		{Name: "kimi-code:cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, RequiresConsent: true},
	}
	m := newTestModel(t)
	m.width, m.height = 90, 28
	m.modelPickerState = newOllamaModelPickerState(models, models[0].Name, m.width, m.height, m.isDark, defaultThemeID)

	// Opening an overlay restyles it for the active glyph profile. Its geometry
	// must already agree with that delegate density.
	m.restylePickerOverlays()
	m.overlay = OverlayModelPicker

	if m.modelPickerState.ItemHeight != 1 {
		t.Fatalf("restyled model row height = %d, want 1", m.modelPickerState.ItemHeight)
	}
	plain := ansi.Strip(m.renderModelPicker())
	for _, want := range []string{"LOCAL · qwen3.5:2b", "CLOUD · kimi-code:cloud"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("restyled picker omitted %q:\n%s", want, plain)
		}
	}
}

func TestOllamaModelPickerEscapeClearsFilterBeforeClosing(t *testing.T) {
	m := newTestModel(t)
	m.modelPickerState = newOllamaModelPickerState([]OllamaModelDescriptor{{Name: "qwen", Source: OllamaModelLocal, Selectable: true, Fit: true}}, "", m.width, m.height, m.isDark, defaultThemeID)
	m.overlay = OverlayModelPicker
	updated, _ := m.Update(charKey('/'))
	m = updated.(*Model)
	updated, _ = m.Update(charKey('q'))
	m = updated.(*Model)
	if m.modelPickerState.List.FilterState() != list.Filtering {
		t.Fatalf("filter state = %v", m.modelPickerState.List.FilterState())
	}
	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayModelPicker || m.modelPickerState.List.FilterState() == list.Filtering {
		t.Fatalf("escape overlay=%v filter=%v", m.overlay, m.modelPickerState.List.FilterState())
	}
}

func TestOllamaModelPickerEnterSelectsVisibleResultWhileFiltering(t *testing.T) {
	m := newTestModel(t)
	descriptor := OllamaModelDescriptor{Name: "qwen", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true}
	m.ollamaModels = []OllamaModelDescriptor{descriptor}
	m.modelPickerState = newOllamaModelPickerState(m.ollamaModels, "", m.width, m.height, m.isDark, defaultThemeID)
	m.overlay = OverlayModelPicker
	updated, _ := m.Update(charKey('/'))
	m = updated.(*Model)
	updated, _ = m.Update(charKey('q'))
	m = updated.(*Model)
	if m.modelPickerState.List.FilterState() != list.Filtering {
		t.Fatalf("filter state = %v", m.modelPickerState.List.FilterState())
	}
	updated, _ = m.Update(enterKey())
	m = updated.(*Model)
	if m.overlay != OverlayNone || m.model != "qwen" {
		t.Fatalf("filtered selection overlay=%d model=%q", m.overlay, m.model)
	}
}

func TestOllamaModelPickerEmptyStateIsActionable(t *testing.T) {
	state := newOllamaModelPickerState(nil, "", 80, 24, false, defaultThemeID)
	if !strings.Contains(state.Notice, "add a model") || !strings.Contains(state.Notice, "refresh") {
		t.Fatalf("empty notice = %q", state.Notice)
	}
}

func TestOllamaModelRefreshShowsImmediateFeedback(t *testing.T) {
	m := newTestModel(t)
	m.modelPickerState = newOllamaModelPickerState(nil, "", m.width, m.height, m.isDark, defaultThemeID)
	m.overlay = OverlayModelPicker

	updated, _ := m.Update(charKey('r'))
	m = updated.(*Model)
	if !strings.Contains(m.modelPickerState.Notice, "Refreshing") {
		t.Fatalf("refresh notice = %q", m.modelPickerState.Notice)
	}
}

func TestOllamaModelPickerOpensDetailsAndPullSurfaces(t *testing.T) {
	m := newTestModel(t)
	descriptor := OllamaModelDescriptor{Name: "local-code", Source: OllamaModelLocal, Selectable: true, Fit: true, Capabilities: []string{"completion", "tools"}}
	m.modelPickerState = newOllamaModelPickerState([]OllamaModelDescriptor{descriptor}, "", m.width, m.height, m.isDark, defaultThemeID)
	m.overlay = OverlayModelPicker

	updated, _ := m.Update(charKey('d'))
	m = updated.(*Model)
	if m.overlay != OverlayModelDetails || m.modelDetailsState == nil || m.modelDetailsState.Name != descriptor.Name {
		t.Fatalf("details state = overlay %d model %#v", m.overlay, m.modelDetailsState)
	}
	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayModelPicker {
		t.Fatalf("details escape returned to overlay %d", m.overlay)
	}
	updated, _ = m.Update(charKey('a'))
	m = updated.(*Model)
	if m.overlay != OverlayModelPull || m.modelPullState == nil || m.modelPullState.Phase != ModelPullEntry {
		t.Fatalf("pull state = overlay %d state %#v", m.overlay, m.modelPullState)
	}
}

func TestOllamaModelPickerDoesNotSelectPolicyBlockedModel(t *testing.T) {
	m := newTestModel(t)
	m.model = "current"
	m.modelPickerState = newOllamaModelPickerState([]OllamaModelDescriptor{{
		Name: "cloud", Source: OllamaModelCloud, Selectable: false, Fit: true, Reason: "disabled by local-only privacy",
	}}, m.model, m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlayModelPicker
	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	if m.model != "current" || m.overlay != OverlayModelPicker {
		t.Fatalf("blocked model changed selection: model=%q overlay=%d", m.model, m.overlay)
	}
}

func TestOllamaCloudSelectionRequiresExplicitSessionConsent(t *testing.T) {
	m := newTestModel(t)
	m.localOnly = true
	m.model = "local-code"
	m.modelManager = llm.NewModelManager("http://localhost:11434", 4096)
	m.modelManager.ConfigureLocalInventory(true, nil, true)
	m.modelManager.ConfigureOllamaInventory([]llm.OllamaModel{{Name: "qwen:cloud", Location: llm.OllamaModelLocationCloud, ContextLength: 262_144}}, true)
	m.modelManager.ConfigureOllamaCloudInventory([]string{"qwen:cloud"}, true)
	descriptor := OllamaModelDescriptor{
		Name: "qwen:cloud", Source: OllamaModelCloud, Selectable: true, Fit: true,
		RequiresConsent: true, Reason: "conversation confirmation required",
	}
	m.ollamaModels = []OllamaModelDescriptor{descriptor}
	m.modelPickerState = newOllamaModelPickerState(m.ollamaModels, m.model, m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlayModelPicker

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	if m.overlay != OverlayCloudConsent || m.model != "local-code" || m.cloudConsentState == nil || m.cloudConsentState.List.Index() != 0 {
		t.Fatalf("consent entry = overlay %d model %q state %#v", m.overlay, m.model, m.cloudConsentState)
	}
	updated, _ = m.Update(downKey())
	m = updated.(*Model)
	updated, _ = m.Update(enterKey())
	m = updated.(*Model)
	if m.model != "qwen:cloud" || m.overlay != OverlayNone || !m.ollamaModels[0].ConsentGranted || m.ollamaModels[0].AutoRoutable {
		t.Fatalf("confirmed selection = model %q overlay %d descriptor %#v", m.model, m.overlay, m.ollamaModels[0])
	}
	if _, err := m.modelManager.GetClient("qwen:cloud"); err != nil {
		t.Fatalf("confirmed model not admitted: %v", err)
	}
}

func TestOllamaCloudConsentCancelReturnsToModelsWithoutGrant(t *testing.T) {
	m := newTestModel(t)
	m.localOnly = true
	m.model = "local-code"
	m.modelManager = llm.NewModelManager("http://localhost:11434", 4096)
	m.modelManager.ConfigureLocalInventory(true, nil, true)
	m.modelManager.ConfigureOllamaInventory([]llm.OllamaModel{{Name: "qwen:cloud", Location: llm.OllamaModelLocationCloud, ContextLength: 262_144}}, true)
	m.modelManager.ConfigureOllamaCloudInventory([]string{"qwen:cloud"}, true)
	descriptor := OllamaModelDescriptor{Name: "qwen:cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, RequiresConsent: true}
	m.ollamaModels = []OllamaModelDescriptor{descriptor}
	m.modelPickerState = newOllamaModelPickerState(m.ollamaModels, m.model, m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlayModelPicker

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	updated, _ = m.Update(enterKey())
	m = updated.(*Model)
	if m.overlay != OverlayModelPicker || m.model != "local-code" || m.ollamaModels[0].ConsentGranted {
		t.Fatalf("cancel result = overlay %d model %q descriptor %#v", m.overlay, m.model, m.ollamaModels[0])
	}
}

func TestOllamaCloudConsentIsRevokedOnNewConversation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[{"name":"local-code","model":"local-code","size":1073741824}]}`)
	}))
	defer server.Close()
	m := newTestModel(t)
	m.localOnly = true
	m.model = "qwen:cloud"
	m.modelPinned = true
	m.modelManager = llm.NewModelManager(server.URL, 4096)
	m.modelManager.ConfigureLocalInventory(true, []llm.LocalModel{{Name: "local-code", Size: 1 << 30}}, true)
	m.modelManager.ConfigureOllamaInventory([]llm.OllamaModel{{Name: "qwen:cloud", Location: llm.OllamaModelLocationCloud, ContextLength: 262_144}}, true)
	m.modelManager.ConfigureOllamaCloudInventory([]string{"qwen:cloud"}, true)
	if err := m.modelManager.GrantOllamaCloudModel("qwen:cloud"); err != nil {
		t.Fatal(err)
	}
	if err := m.modelManager.SetCurrentModel("qwen:cloud"); err != nil {
		t.Fatal(err)
	}
	m.ollamaModels = []OllamaModelDescriptor{
		{Name: "local-code", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true},
		{Name: "qwen:cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, ConsentGranted: true, Current: true},
	}
	m.resetConversationSession()
	if m.model != "local-code" || m.modelPinned || !m.ollamaModels[0].Current || m.ollamaModels[1].ConsentGranted || !m.ollamaModels[1].RequiresConsent {
		t.Fatalf("consent reset model=%q pinned=%v descriptors=%#v", m.model, m.modelPinned, m.ollamaModels)
	}
	if _, err := m.modelManager.GetClient("qwen:cloud"); err == nil {
		t.Fatal("revoked cloud model remained admitted")
	}
}

func TestAutomaticRoutingLeavesCloudOnlyAfterLocalSwitch(t *testing.T) {
	m := newTestModel(t)
	m.localOnly = true
	m.model = "qwen:cloud"
	m.modelPinned = true
	m.ollamaModels = []OllamaModelDescriptor{
		{Name: "local-code", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true},
		{Name: "qwen:cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, ConsentGranted: true, Current: true},
	}
	if err := m.enableAutomaticModelRouting(); err != nil {
		t.Fatal(err)
	}
	if m.model != "local-code" || m.modelPinned || !m.ollamaModels[0].Current || m.ollamaModels[1].ConsentGranted {
		t.Fatalf("automatic routing model=%q pinned=%v descriptors=%#v", m.model, m.modelPinned, m.ollamaModels)
	}
}

func TestAutomaticRoutingFailsClosedWithoutLocalModel(t *testing.T) {
	m := newTestModel(t)
	m.localOnly = true
	m.model = "qwen:cloud"
	m.modelPinned = true
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "qwen:cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, ConsentGranted: true, Current: true,
	}}
	if err := m.enableAutomaticModelRouting(); err == nil {
		t.Fatal("automatic routing accepted a cloud-only inventory")
	}
	if m.model != "qwen:cloud" || !m.modelPinned || !m.ollamaModels[0].ConsentGranted {
		t.Fatalf("failed automatic transition mutated authority: model=%q pinned=%v descriptor=%#v", m.model, m.modelPinned, m.ollamaModels[0])
	}
}

func TestOllamaCloudConsentModalFitsSupportedTerminals(t *testing.T) {
	for _, size := range []struct{ width, height int }{{30, 12}, {40, 20}, {90, 28}} {
		m := newTestModel(t)
		m.width, m.height = size.width, size.height
		m.cloudConsentState = newCloudConsentState(OllamaModelDescriptor{Name: "kimi-code:cloud"}, size.width, size.height, m.isDark, defaultThemeID)
		m.overlay = OverlayCloudConsent
		view := m.renderCloudConsent()
		for _, want := range []string{"Use Ollama Cloud?", "results", "Ollama Cloud.", "Cancel", "Use kimi-code", "esc", "enter", "cancel"} {
			if !strings.Contains(view, want) {
				t.Fatalf("%dx%d consent missing %q:\n%s", size.width, size.height, want, view)
			}
		}
		if size.width >= 90 && !strings.Contains(ansi.Strip(view), "↑/↓ move") {
			t.Fatalf("%dx%d consent omitted movement affordance:\n%s", size.width, size.height, view)
		}
		m.cloudConsentState.List.Select(1)
		if selected := ansi.Strip(m.renderCloudConsent()); !strings.Contains(selected, "enter use") {
			t.Fatalf("%dx%d consent did not describe the focused allow action:\n%s", size.width, size.height, selected)
		}
		assertRenderedLinesFit(t, view, size.width)
		assertRenderedHeightFits(t, view, size.height)
	}
}

func TestOllamaNestedSurfacesFitMinimumTerminal(t *testing.T) {
	newMinimumModel := func(t *testing.T) *Model {
		t.Helper()
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
		return updated.(*Model)
	}

	t.Run("details keep actions and privacy boundary visible", func(t *testing.T) {
		m := newMinimumModel(t)
		m.openModelDetails(OllamaModelDescriptor{
			Name: "kimi-code:cloud", Source: OllamaModelCloud, Current: true, Running: true,
			Selectable: true, Fit: true, ParameterSize: "120B", Quantization: "Q8_0",
			SizeBytes: 20 << 30, SizeVRAM: 10 << 30, ContextLength: 262_144,
			EffectiveContext: 16_384, AllocatedContext: 8_192,
			Capabilities: []string{"tools", "thinking", "vision", "completion"},
			Reason:       "Ollama Cloud · conversation consent",
		})

		overlay := m.renderModelDetails()
		plain := ansi.Strip(overlay)
		for _, want := range []string{"kimi-code:cloud", "CLOUD", "Context", "Can", "Cloud · remote prompts", "esc models", "╰"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("minimum details missing %q:\n%s", want, plain)
			}
		}
		assertRenderedLinesFit(t, overlay, 30)
		assertRenderedHeightFits(t, overlay, 12)

		full := ansi.Strip(m.View().Content)
		if !strings.Contains(full, "esc models") || !strings.Contains(full, "╰") || strings.Contains(full, "❯") {
			t.Fatalf("composited minimum details lost actions or exposed composer:\n%s", full)
		}
	})

	t.Run("pull entry owns the translated cursor", func(t *testing.T) {
		m := newMinimumModel(t)
		_ = m.openModelPull()
		m.modelPullState.Input.SetValue("qwen3:4b")
		m.modelPullState.Input.CursorEnd()

		overlay, cursor := m.renderModelPull()
		if cursor == nil {
			t.Fatal("minimum pull entry did not expose its focused Bubbles cursor")
		}
		assertRenderedLinesFit(t, overlay, 30)
		assertRenderedHeightFits(t, overlay, 12)
		assertViewCursorAfter(t, m.View(), "Model › qwen3:4b")
	})

	t.Run("pull receipts bound daemon text", func(t *testing.T) {
		for _, phase := range []ModelPullPhase{ModelPullRunning, ModelPullComplete, ModelPullFailed} {
			m := newMinimumModel(t)
			m.modelPullState = NewModelPullState(m.isDark, true, defaultThemeID)
			m.modelPullState.Name = "a-very-long-ollama-model-name-that-must-not-displace-actions"
			m.modelPullState.Phase = phase
			m.modelPullState.Completed, m.modelPullState.Total = 25, 100
			m.modelPullState.Status = strings.Repeat("daemon status remains bounded ", 8)
			if phase == ModelPullFailed {
				m.modelPullState.Err = errors.New(strings.Repeat("authentication failure with recovery context ", 8))
			}
			m.overlay = OverlayModelPull

			overlay, cursor := m.renderModelPull()
			if cursor != nil {
				t.Fatalf("phase %d unexpectedly retained a text cursor", phase)
			}
			plain := ansi.Strip(overlay)
			if !strings.Contains(plain, "esc") || !strings.Contains(plain, "╰") {
				t.Fatalf("phase %d lost its footer or closing border:\n%s", phase, plain)
			}
			assertRenderedLinesFit(t, overlay, 30)
			assertRenderedHeightFits(t, overlay, 12)

			full := ansi.Strip(m.View().Content)
			if !strings.Contains(full, "esc") || !strings.Contains(full, "╰") || strings.Contains(full, "❯") {
				t.Fatalf("phase %d composited view is not recoverable:\n%s", phase, full)
			}
		}
	})
}

func TestCloudSessionRestoreRequiresFreshConsent(t *testing.T) {
	setup := func(t *testing.T) (*Model, SessionLoadedMsg) {
		t.Helper()
		m := newTestModel(t)
		m.localOnly = true
		m.model = "local-code"
		m.modelManager = llm.NewModelManager("http://localhost:11434", 4096)
		m.modelManager.ConfigureLocalInventory(true, []llm.LocalModel{{Name: "local-code", Size: 1 << 30}}, true)
		m.modelManager.ConfigureOllamaInventory([]llm.OllamaModel{{Name: "qwen:cloud", Location: llm.OllamaModelLocationCloud, ContextLength: 262_144}}, true)
		m.modelManager.ConfigureOllamaCloudInventory([]string{"qwen:cloud"}, true)
		m.ollamaModels = []OllamaModelDescriptor{
			{Name: "local-code", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true, Current: true},
			{Name: "qwen:cloud", Source: OllamaModelCloud, Selectable: true, Fit: true, RequiresConsent: true},
		}
		m.sessionLoading = true
		m.sessionLoadToken = 7
		message := SessionLoadedMsg{
			LoadToken: 7, SessionID: 42, Title: "cloud work",
			State:       persistedSessionState{Version: currentPersistedSessionVersion, Mode: ModeNormal, Model: "qwen:cloud", ModelPinned: true},
			StateRecord: db.SessionStateRecord{SessionID: 42, Revision: 3},
		}
		return m, message
	}

	t.Run("cancel keeps current conversation unchanged", func(t *testing.T) {
		m, message := setup(t)
		m.handleSessionLoadedReceipt(message)
		if m.overlay != OverlayCloudConsent || m.cloudConsentState == nil || m.cloudConsentState.PendingLoad == nil || m.sessionID != 0 {
			t.Fatalf("pending restore overlay=%d state=%#v session=%d", m.overlay, m.cloudConsentState, m.sessionID)
		}
		m.closeCloudConsent()
		if m.overlay != OverlayNone || m.sessionID != 0 || m.model != "local-code" {
			t.Fatalf("cancelled restore overlay=%d session=%d model=%q", m.overlay, m.sessionID, m.model)
		}
	})

	t.Run("allow restores exact cloud model", func(t *testing.T) {
		m, message := setup(t)
		m.handleSessionLoadedReceipt(message)
		m.cloudConsentState.List.Select(1)
		_ = m.confirmCloudModel("qwen:cloud")
		if m.overlay != OverlayNone || m.sessionID != 42 || m.model != "qwen:cloud" || !m.modelPinned || !m.ollamaModels[1].ConsentGranted || !m.ollamaModels[1].Current {
			t.Fatalf("restored session overlay=%d session=%d model=%q pinned=%v descriptors=%#v", m.overlay, m.sessionID, m.model, m.modelPinned, m.ollamaModels)
		}
	})
}

func TestInitializedInventoryRejectsAbsentRestoredModel(t *testing.T) {
	m := newTestModel(t)
	m.ollamaInventoryAttempted = true
	m.ollamaModels = []OllamaModelDescriptor{{Name: "known", Source: OllamaModelLocal, Selectable: true, Fit: true}}
	if err := m.validateModelAdmission("missing"); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("absent model admission error = %v", err)
	}
}

func TestModelSwitchProjectsEffectiveLocalAndCloudContexts(t *testing.T) {
	manager := llm.NewModelManager("http://localhost:11434", 16_384)
	manager.ConfigureOllamaInventory([]llm.OllamaModel{
		{Name: "local-code", Location: llm.OllamaModelLocationLocal, ContextLength: 262_144},
		{Name: "cloud-code", Location: llm.OllamaModelLocationCloud, ContextLength: 1_048_576},
	}, true)
	if err := manager.SetCurrentModel("local-code"); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.modelManager = manager
	m.model = "local-code"
	m.numCtx = manager.NumCtx()
	m.promptTokens = 12_000
	m.ollamaModels = []OllamaModelDescriptor{
		{Name: "local-code", Source: OllamaModelLocal, ContextLength: 262_144, Selectable: true, Fit: true, Current: true},
		{Name: "cloud-code", Source: OllamaModelCloud, ContextLength: 1_048_576, Selectable: true, Fit: true},
	}

	if !m.switchSelectedModel("cloud-code") {
		t.Fatal("cloud model switch failed")
	}
	if m.numCtx != 1_048_576 || m.promptTokens != 0 {
		t.Fatalf("cloud projection = context %d prompt %d, want 1048576/0", m.numCtx, m.promptTokens)
	}
	if !m.switchSelectedModel("local-code") {
		t.Fatal("local model switch failed")
	}
	if m.numCtx != 16_384 {
		t.Fatalf("local projection context = %d, want 16384", m.numCtx)
	}
}

func TestModelPullStateRequestProgressAndFailure(t *testing.T) {
	state := NewModelPullState(true, true, defaultThemeID)
	state.Input.SetValue("  gpt-oss:120b-cloud  ")
	cmd := state.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("missing pull request command")
	}
	request, ok := cmd().(OllamaModelPullRequestedMsg)
	if !ok || request.Name != "gpt-oss:120b-cloud" {
		t.Fatalf("request = %#v", request)
	}
	if state.Phase != ModelPullRunning {
		t.Fatalf("phase = %v", state.Phase)
	}

	if cmd := state.Apply(OllamaModelPullProgressMsg{Name: request.Name, Status: "pulling manifest", Completed: 25, Total: 100}); cmd != nil {
		t.Fatal("reduced motion scheduled progress animation")
	}
	if state.ProgressText() != "25%" || !strings.Contains(state.View(50), "25%") || !strings.Contains(state.View(50), "25 B / 100 B") {
		t.Fatalf("progress view = %q", state.View(50))
	}
	state.Apply(OllamaModelPullProgressMsg{Name: request.Name, Err: errors.New("Ollama sign-in required")})
	if state.Phase != ModelPullFailed || !strings.Contains(state.View(50), "sign-in required") {
		t.Fatalf("failure view = %q", state.View(50))
	}
}

func TestModelPullSanitizesAndBoundsUntrustedProgress(t *testing.T) {
	state := NewModelPullState(true, true, defaultThemeID)
	state.Name, state.Phase = "qwen", ModelPullRunning
	status := "\x1b]0;forged-title\x07pulling\r\nmanifest\u202e" + strings.Repeat("x", 2_000)
	failure := errors.New("\x1b[2Jregistry failed\u2066\nsecond row")

	state.Apply(OllamaModelPullProgressMsg{Name: "qwen", Status: status, Err: failure})

	for label, value := range map[string]string{
		"status": state.Status,
		"error":  state.Err.Error(),
	} {
		if strings.ContainsAny(value, "\r\n\t\x1b") {
			t.Fatalf("%s retained terminal control characters: %q", label, value)
		}
		for _, r := range value {
			if unicode.IsControl(r) || isBidiControl(r) {
				t.Fatalf("%s retained unsafe rune %U: %q", label, r, value)
			}
		}
	}
	if got := ansi.StringWidth(state.Status); got > maxModelPullStatusCells {
		t.Fatalf("status width = %d, max %d", got, maxModelPullStatusCells)
	}
	if got := ansi.StringWidth(state.Err.Error()); got > maxModelPullErrorCells {
		t.Fatalf("error width = %d, max %d", got, maxModelPullErrorCells)
	}
	plain := ansi.Strip(state.View(80))
	if strings.Contains(plain, "forged-title") || strings.Contains(plain, "\x1b") {
		t.Fatalf("rendered pull state retained terminal injection: %q", plain)
	}
}

func TestModelPullReducedMotionUsesUnfinishedStaticGlyph(t *testing.T) {
	state := NewModelPullState(true, true, defaultThemeID)
	state.Name = "qwen"
	state.Phase = ModelPullRunning
	plain := ansi.Strip(state.View(50))
	if !strings.Contains(plain, "… Connecting to Ollama") || strings.Contains(plain, "•") {
		t.Fatalf("reduced-motion connection state used an ambiguous glyph: %q", plain)
	}
}

func TestModelPullIgnoresStaleModelProgress(t *testing.T) {
	state := NewModelPullState(false, true, defaultThemeID)
	state.Name, state.Phase = "wanted", ModelPullRunning
	state.Apply(OllamaModelPullProgressMsg{Name: "stale", Done: true})
	if state.Phase != ModelPullRunning {
		t.Fatalf("stale update changed phase to %v", state.Phase)
	}
}

func TestModelPullEmitsCancelWhileRunning(t *testing.T) {
	state := NewModelPullState(false, true, defaultThemeID)
	state.Name, state.Phase = "qwen", ModelPullRunning
	cmd := state.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("missing cancel command")
	}
	msg, ok := cmd().(OllamaModelPullCancelRequestedMsg)
	if !ok || msg.Name != "qwen" {
		t.Fatalf("cancel = %#v", msg)
	}
}

func TestModelPullFailureCanRetryOrEdit(t *testing.T) {
	state := NewModelPullState(false, true, defaultThemeID)
	state.Name, state.Phase, state.Err = "qwen3:4b", ModelPullFailed, errors.New("network unavailable")

	retry := state.Update(tea.KeyPressMsg{Code: 'r'})
	if retry == nil {
		t.Fatal("retry did not emit a command")
	}
	request, ok := retry().(OllamaModelPullRequestedMsg)
	if !ok || request.Name != "qwen3:4b" || state.Phase != ModelPullRunning || state.Err != nil {
		t.Fatalf("retry = %#v phase=%v err=%v", request, state.Phase, state.Err)
	}

	state.Phase, state.Err = ModelPullFailed, errors.New("model not found")
	state.Update(tea.KeyPressMsg{Code: 'e'})
	if state.Phase != ModelPullEntry || state.Input.Value() != "qwen3:4b" {
		t.Fatalf("edit phase=%v value=%q", state.Phase, state.Input.Value())
	}
}

func TestModelPullThemeChangePreservesOperationState(t *testing.T) {
	state := NewModelPullState(true, false, defaultThemeID)
	state.Name, state.Phase, state.Completed, state.Total = "qwen", ModelPullRunning, 25, 100
	state.SetTheme(false, defaultThemeID)
	if state.isDark || state.Name != "qwen" || state.Phase != ModelPullRunning || state.Completed != 25 || state.Total != 100 {
		t.Fatalf("theme change lost state: %#v", state)
	}
}

func TestModelPullIgnoresStaleOperationReceipt(t *testing.T) {
	m := newTestModel(t)
	m.modelPullRequest = 2
	m.modelPullRunning = true
	m.modelPullState = NewModelPullState(false, true, defaultThemeID)
	m.modelPullState.Name, m.modelPullState.Phase = "qwen", ModelPullRunning

	updated, _ := m.Update(OllamaModelPullProgressMsg{RequestID: 1, Name: "qwen", Err: errors.New("old cancellation")})
	m = updated.(*Model)
	if m.modelPullState.Phase != ModelPullRunning || !m.modelPullRunning {
		t.Fatalf("stale receipt changed phase=%v running=%v", m.modelPullState.Phase, m.modelPullRunning)
	}
	updated, _ = m.Update(OllamaModelPullProgressMsg{RequestID: 2, Name: "qwen", Done: true})
	m = updated.(*Model)
	if m.modelPullState.Phase != ModelPullComplete || m.modelPullRunning {
		t.Fatalf("current receipt phase=%v running=%v", m.modelPullState.Phase, m.modelPullRunning)
	}
}

func TestShutdownWaitsForModelPullTerminalReceipt(t *testing.T) {
	m := newTestModel(t)
	m.modelPullRequest = 7
	m.modelPullRunning = true
	m.shuttingDown = true
	if m.shutdownReady() {
		t.Fatal("shutdown ignored active model pull")
	}
	updated, cmd := m.Update(OllamaModelPullProgressMsg{RequestID: 7, Name: "qwen", Err: errors.New("model download cancelled")})
	m = updated.(*Model)
	if !m.shutdownReady() || cmd == nil {
		t.Fatalf("terminal receipt ready=%v cmd=%v", m.shutdownReady(), cmd != nil)
	}
}

func TestOllamaModelDetailsDisclosesRemoteBoundary(t *testing.T) {
	view := renderOllamaModelDetails(OllamaModelDescriptor{
		Name: "coder:cloud", Source: OllamaModelCloud, ParameterSize: "120B",
		ContextLength: 131072, Capabilities: []string{"tools", "thinking"}, Selectable: true, Fit: true,
	}, 48, false, defaultThemeID)
	for _, want := range []string{"coder:cloud", "cloud", "120B", "131K tokens", "tools+thinking", "Prompts leave this machine"} {
		if !strings.Contains(view, want) {
			t.Fatalf("details missing %q:\n%s", want, view)
		}
	}
}

func TestOllamaModelDetailsDoesNotClaimMachineSpecificFit(t *testing.T) {
	view := renderOllamaModelDetails(OllamaModelDescriptor{
		Name: "large-local", Source: OllamaModelLocal, Selectable: true, Fit: false,
		Reason: "outside local memory profile",
	}, 48, false, defaultThemeID)
	if !strings.Contains(view, "Outside default profile") || strings.Contains(view, "Status         Unavailable") {
		t.Fatalf("profile status = %q", view)
	}
}
