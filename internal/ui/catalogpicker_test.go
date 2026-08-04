package ui

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
)

func deepSeekCatalogModels(t *testing.T) []catalog.Model {
	t.Helper()
	models := catalog.Models("deepseek")
	if len(models) == 0 {
		t.Fatal("catalog has no deepseek models")
	}
	return models
}

// The row must carry the three facts that change a decision when you pay per
// token. A picker that lists only names makes the cheap and expensive tiers
// indistinguishable.
func TestCatalogPickerRowShowsPriceContextAndReasoning(t *testing.T) {
	model, ok := catalog.LookupModel("deepseek", "deepseek-v4-flash")
	if !ok {
		t.Fatal("deepseek-v4-flash missing from catalog")
	}
	item := catalogModelItem{model: model}

	description := item.Description()
	for _, want := range []string{"$0.14/$0.28 per 1M", "1M ctx", "reasons high"} {
		if !strings.Contains(description, want) {
			t.Errorf("description %q is missing %q", description, want)
		}
	}
	// DeepSeek V4 takes no images; the row must say so, because silently
	// offering attachments builds a request the provider rejects.
	if !strings.Contains(description, "no images") {
		t.Errorf("description %q does not flag missing attachment support", description)
	}
}

func TestCatalogPickerMarksTheCurrentModel(t *testing.T) {
	models := deepSeekCatalogModels(t)
	state := newCatalogModelPickerState("deepseek", models, "deepseek-v4-flash", 120, 40, true, "nord", false)

	selected, ok := state.SelectedCatalogModel()
	if !ok {
		t.Fatal("catalog picker has no selection")
	}
	if selected.ID != "deepseek-v4-flash" {
		t.Errorf("selection = %q, want the current model preselected", selected.ID)
	}
	if title := (catalogModelItem{model: selected, isCurrent: true}).Title(); !strings.Contains(title, "current") {
		t.Errorf("current row title %q does not mark it", title)
	}
}

// A picker built from a local inventory must not answer catalog queries, and
// vice versa: the two sources are mutually exclusive and confusing them would
// select a model against the wrong provider.
func TestCatalogSelectionIsExclusiveOfInventorySelection(t *testing.T) {
	catalogState := newCatalogModelPickerState("deepseek", deepSeekCatalogModels(t), "deepseek-v4-flash", 120, 40, true, "nord", false)
	if _, ok := catalogState.SelectedDescriptor(); ok {
		t.Error("catalog-backed picker returned an Ollama descriptor")
	}

	inventoryState := newOllamaModelPickerState(
		[]OllamaModelDescriptor{{Name: "qwen3.5:2b", Source: OllamaModelLocal, Selectable: true, Fit: true}},
		"qwen3.5:2b", 120, 40, true, "nord", false,
	)
	if _, ok := inventoryState.SelectedCatalogModel(); ok {
		t.Error("inventory-backed picker returned a catalog model")
	}
}

func TestCatalogPickerTitleNamesProviderAndCount(t *testing.T) {
	models := deepSeekCatalogModels(t)
	state := newCatalogModelPickerState("deepseek", models, "", 120, 40, true, "nord", false)
	title := state.List.Title
	if !strings.Contains(strings.ToLower(title), "deepseek") {
		t.Errorf("title %q does not name the provider", title)
	}
	if !strings.Contains(title, "model") {
		t.Errorf("title %q does not report a model count", title)
	}
}

// A provider the catalog does not describe (a private gateway) must not
// produce an empty picker that looks like the provider has no models.
func TestCatalogModelsAbsentForUnknownProvider(t *testing.T) {
	if models := catalog.Models("not-a-provider"); len(models) != 0 {
		t.Errorf("unknown provider returned %d models", len(models))
	}
	state := newCatalogModelPickerState("not-a-provider", nil, "", 120, 40, true, "nord", false)
	if state.Notice == "" {
		t.Error("empty catalog picker gives the user no explanation")
	}
}

func TestFormatTokenCount(t *testing.T) {
	for _, test := range []struct {
		tokens int64
		want   string
	}{
		{1_000_000, "1M"},
		{1_048_576, "1.0M"},
		{262_144, "262K"},
		{131_072, "131K"},
		{500, "500"},
		{0, ""},
		{-5, ""},
	} {
		if got := formatTokenCount(test.tokens); got != test.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", test.tokens, got, test.want)
		}
	}
}

func TestFormatModelPriceLabelsFreeTiers(t *testing.T) {
	if got := formatModelPrice(catalog.Model{}); got != "free" {
		t.Errorf("zero-cost model priced as %q, want free", got)
	}
	// Sub-cent rates must not round away to $0.00 — cache-hit pricing is the
	// whole reason prompt-prefix stability matters.
	got := formatModelPrice(catalog.Model{CostPer1MIn: 0.0028, CostPer1MOut: 0.28})
	if strings.Contains(got, "0.00 ") || !strings.Contains(got, "0.0028") {
		t.Errorf("sub-cent price rendered as %q", got)
	}
}
