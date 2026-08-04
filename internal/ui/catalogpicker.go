package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// catalogModelItem is one selectable model from the provider catalog.
//
// The row carries identity and the current marker; the description carries the
// three facts that actually change a decision when you are paying per token —
// price, context, and whether the model reasons. Attachment support appears
// only when absent, because that is the case that silently breaks a turn.
type catalogModelItem struct {
	model     catalog.Model
	isCurrent bool
}

func (i catalogModelItem) Title() string {
	name := sanitizeTerminalSingleLine(i.model.Name)
	if name == "" {
		name = sanitizeTerminalSingleLine(i.model.ID)
	}
	if i.isCurrent {
		return name + " · current"
	}
	return name
}

func (i catalogModelItem) Description() string {
	parts := make([]string, 0, 4)
	if price := formatModelPrice(i.model); price != "" {
		parts = append(parts, price)
	}
	if window := formatTokenCount(i.model.ContextWindow); window != "" {
		parts = append(parts, window+" ctx")
	}
	if i.model.CanReason {
		effort := strings.TrimSpace(i.model.DefaultReasoningEffort)
		if effort == "" {
			parts = append(parts, "reasons")
		} else {
			parts = append(parts, "reasons "+sanitizeTerminalSingleLine(effort))
		}
	}
	// Stated only when false: offering an attachment a model cannot take is
	// what builds a request the provider rejects.
	if !i.model.SupportsImages {
		parts = append(parts, "no images")
	}
	if len(parts) == 0 {
		return sanitizeTerminalSingleLine(i.model.ID)
	}
	return strings.Join(parts, " · ")
}

func (i catalogModelItem) FilterValue() string {
	return strings.Join([]string{
		sanitizeTerminalSingleLine(i.model.ID),
		sanitizeTerminalSingleLine(i.model.Name),
	}, " ")
}

// formatModelPrice renders input/output cost per million tokens. Free tiers
// report zero on both sides and are labeled rather than shown as "$0.00".
func formatModelPrice(model catalog.Model) string {
	if model.CostPer1MIn <= 0 && model.CostPer1MOut <= 0 {
		return "free"
	}
	return fmt.Sprintf("$%s/$%s per 1M", trimPrice(model.CostPer1MIn), trimPrice(model.CostPer1MOut))
}

func trimPrice(value float64) string {
	switch {
	case value <= 0:
		return "0"
	case value < 0.01:
		return fmt.Sprintf("%.4f", value)
	case value < 1:
		return fmt.Sprintf("%.2f", value)
	default:
		return fmt.Sprintf("%.2f", value)
	}
}

// formatTokenCount abbreviates a token count for a single-line row.
func formatTokenCount(tokens int64) string {
	switch {
	case tokens <= 0:
		return ""
	case tokens >= 1_000_000:
		if tokens%1_000_000 == 0 {
			return fmt.Sprintf("%dM", tokens/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%dK", tokens/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

// newCatalogModelPickerState builds the model picker from a provider's catalog
// entry. Models keep catalog order, which is the provider's own preference
// ordering rather than an alphabetical reshuffle.
func newCatalogModelPickerState(
	providerID string,
	models []catalog.Model,
	currentModel string,
	terminalWidth, terminalHeight int,
	isDark bool,
	themeID string,
	reducedMotion ...bool,
) *ModelPickerState {
	items := make([]list.Item, 0, len(models))
	selectedIdx := 0
	canonicalCurrent := config.CanonicalModelName(currentModel)
	for _, model := range models {
		isCurrent := config.CanonicalModelName(model.ID) == canonicalCurrent
		if isCurrent {
			selectedIdx = len(items)
		}
		items = append(items, catalogModelItem{model: model, isCurrent: isCurrent})
	}

	compact := compactModelPicker(terminalWidth, terminalHeight)
	delegate := newPickerDelegate(isDark, true, themeID)
	pickerW := pickerListWidth(terminalWidth)
	pickerH := modelPickerListHeight(terminalHeight, len(items), delegate.Height())
	l := list.New(items, delegate, pickerW, pickerH)
	configurePickerList(&l, isDark, themeID, reducedMotion...)
	setSettingsTitleDensity(&l, compact)
	l.Title = catalogModelPickerTitle(providerID, len(models))
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(!compact && len(items)*delegate.Height() > pickerH-2)
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()
	if len(items) > 0 {
		l.Select(selectedIdx)
	}

	state := &ModelPickerState{
		List:          l,
		CatalogModels: append([]catalog.Model(nil), models...),
		CurrentModel:  currentModel,
		Compact:       compact,
		ItemHeight:    delegate.Height(),
	}
	if len(models) == 0 {
		state.Notice = "No catalog models for this provider · refresh the catalog or set a model explicitly"
	}
	return state
}

func catalogModelPickerTitle(providerID string, count int) string {
	name := sanitizeTerminalSingleLine(providerID)
	if provider, ok := catalog.LookupProvider(catalog.ProviderID(providerID)); ok {
		if trimmed := strings.TrimSpace(provider.Name); trimmed != "" {
			name = sanitizeTerminalSingleLine(trimmed)
		}
	}
	if count == 1 {
		return name + " · 1 model"
	}
	return fmt.Sprintf("%s · %d models", name, count)
}

// catalogModelsForActiveProvider returns the catalog models for the provider
// currently in use, and whether the catalog knows that provider at all. A
// private openai_compatible endpoint has no entry, which is not an error — it
// just means the picker has nothing to offer and the model stays as configured.
func (m *Model) catalogModelsForActiveProvider() (string, []catalog.Model, bool) {
	providerID := config.NormalizedProviderType(m.activeProviderName())
	models := catalog.Models(catalog.ProviderID(providerID))
	if len(models) == 0 {
		return providerID, nil, false
	}
	return providerID, models, true
}
