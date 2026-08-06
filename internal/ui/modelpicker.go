package ui

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"charm.land/bubbles/v2/list"
	"github.com/abdul-hamid-achik/sonar/internal/catalog"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const (
	modelPickerCompactWidth = 40
	modelPickerCompactRows  = 14
)

// OllamaModelSource is the execution boundary reported by Ollama. It is kept
// in the UI package so the picker does not depend on a particular wire client.
type OllamaModelSource uint8

const (
	OllamaModelLocal OllamaModelSource = iota
	OllamaModelCloud
	OllamaModelRemote
)

// OllamaModelDescriptor is the UI-facing inventory contract. The Ollama
// adapter owns discovery and enrichment; the picker only projects that state.
type OllamaModelDescriptor struct {
	Name             string
	DisplayName      string
	Source           OllamaModelSource
	SizeBytes        int64
	ParameterSize    string
	Quantization     string
	ContextLength    int
	EffectiveContext int
	AllocatedContext int
	SizeVRAM         int64
	Capabilities     []string
	Current          bool
	Running          bool
	Selectable       bool
	Fit              bool
	AutoRoutable     bool
	ManualOnly       bool
	Reason           string
}

// OllamaModelInventoryMsg replaces the picker's cached inventory. RequestID
// lets the parent discard stale asynchronous refreshes.
type OllamaModelInventoryMsg struct {
	RequestID uint64
	Models    []OllamaModelDescriptor
	Err       error
}

// OllamaModelDetailsRequestedMsg asks the parent to enrich/show one model.
// Cached descriptors may be rendered immediately while /api/show completes.
type OllamaModelDetailsRequestedMsg struct{ Model OllamaModelDescriptor }

type OllamaModelDetailsResultMsg struct {
	Model OllamaModelDescriptor
	Err   error
}

type modelItem struct {
	name       string
	descriptor OllamaModelDescriptor
	size       string // retained for focused compatibility tests
	capability string // retained for focused compatibility tests
	isCurrent  bool
	unsafe     bool
}

func (i modelItem) Title() string {
	name := modelDisplayName(i.descriptor)
	if name == "" {
		name = sanitizeTerminalSingleLine(i.name)
	}
	parts := []string{modelGroupLabel(descriptorGroup(i.descriptor)), name}
	if state := modelRowState(i.descriptor, i.isCurrent, i.unsafe); state != "" {
		parts = append(parts, state)
	}
	return strings.Join(parts, " · ")
}

func (i modelItem) Description() string {
	if i.descriptor.Name == "" { // legacy config projection
		size := sanitizeTerminalSingleLine(i.size)
		capability := sanitizeTerminalSingleLine(i.capability)
		if i.unsafe {
			return fmt.Sprintf("%s · needs >16GB — unavailable", size)
		}
		return fmt.Sprintf("%s · %s", size, capability)
	}
	parts := make([]string, 0, 5)
	if size := humanModelBytes(i.descriptor.SizeBytes); size != "" {
		parts = append(parts, size)
	}
	if i.descriptor.ParameterSize != "" {
		if value := sanitizeTerminalSingleLine(i.descriptor.ParameterSize); value != "" {
			parts = append(parts, value)
		}
	}
	if i.descriptor.Quantization != "" {
		if value := sanitizeTerminalSingleLine(i.descriptor.Quantization); value != "" {
			parts = append(parts, value)
		}
	}
	if capabilities := compactCapabilities(i.descriptor.Capabilities); capabilities != "" {
		parts = append(parts, capabilities)
	}
	if i.descriptor.ContextLength > 0 {
		parts = append(parts, compactTokenCount(i.descriptor.ContextLength)+" max ctx")
	}
	if i.descriptor.EffectiveContext > 0 && i.descriptor.EffectiveContext != i.descriptor.ContextLength {
		parts = append(parts, compactTokenCount(i.descriptor.EffectiveContext)+" effective")
	}
	return strings.Join(parts, " · ")
}

func (i modelItem) FilterValue() string {
	return strings.Join([]string{
		sanitizeTerminalSingleLine(i.name),
		modelDisplayName(i.descriptor),
		modelGroupLabel(descriptorGroup(i.descriptor)),
		modelRowState(i.descriptor, i.isCurrent, i.unsafe),
		sanitizeTerminalSingleLine(strings.Join(i.descriptor.Capabilities, " ")),
		sanitizeTerminalSingleLine(i.descriptor.Reason),
	}, " ")
}

// modelDisplayName returns a terminal-safe presentation projection while the
// descriptor itself retains the exact Ollama identifier used for selection
// and network requests.
func modelDisplayName(model OllamaModelDescriptor) string {
	name := sanitizeTerminalSingleLine(model.DisplayName)
	if name == "" {
		name = sanitizeTerminalSingleLine(model.Name)
	}
	return name
}

// ollamaModelPickerTitle is a constant heading. The daemon version it used to
// carry is a runtime fact, and now lives in the Runtime panel with the others.
func ollamaModelPickerTitle(string) string {
	return "Ollama models"
}

type ModelPickerState struct {
	List      list.Model
	Models    []config.Model // compatibility projection for existing callers
	Inventory []OllamaModelDescriptor
	// CatalogModels is set when the picker was built from the provider catalog
	// rather than a local inventory. The two are mutually exclusive.
	CatalogModels []catalog.Model
	CurrentModel  string
	RequestID     uint64
	Notice        string
	Compact       bool
	ItemHeight    int
}

// SelectedCatalogModel returns the highlighted catalog model, if the picker is
// catalog-backed and something is highlighted.
func (s *ModelPickerState) SelectedCatalogModel() (catalog.Model, bool) {
	if s == nil || len(s.CatalogModels) == 0 {
		return catalog.Model{}, false
	}
	item, ok := s.List.SelectedItem().(catalogModelItem)
	if !ok {
		return catalog.Model{}, false
	}
	return item.model, true
}

func (s *ModelPickerState) SelectedDescriptor() (OllamaModelDescriptor, bool) {
	if s == nil {
		return OllamaModelDescriptor{}, false
	}
	item, ok := s.List.SelectedItem().(modelItem)
	if !ok {
		return OllamaModelDescriptor{}, false
	}
	return item.descriptor, item.descriptor.Name != ""
}

func (s *ModelPickerState) SelectedReason() string {
	descriptor, ok := s.SelectedDescriptor()
	if !ok {
		return ""
	}
	return modelDecisionReason(descriptor)
}

func newModelPickerState(models []config.Model, currentModel string, terminalWidth, terminalHeight int, isDark bool, themeID string, reducedMotion ...bool) *ModelPickerState {
	descriptors := make([]OllamaModelDescriptor, 0, len(models))
	for _, model := range models {
		descriptors = append(descriptors, OllamaModelDescriptor{
			Name: model.Name, DisplayName: model.Name, Source: OllamaModelLocal,
			ParameterSize: model.Size, ContextLength: model.ContextSize,
			Capabilities: []string{model.Capability.String()}, Current: model.Name == currentModel,
			Selectable: true, Fit: config.CheckModelMemorySafe(model.Name) == nil, AutoRoutable: true,
		})
	}
	state := newOllamaModelPickerState(descriptors, currentModel, terminalWidth, terminalHeight, isDark, themeID, reducedMotion...)
	state.Models = models
	return state
}

func newOllamaModelPickerState(models []OllamaModelDescriptor, currentModel string, terminalWidth, terminalHeight int, isDark bool, themeID string, reducedMotion ...bool) *ModelPickerState {
	models = append([]OllamaModelDescriptor(nil), models...)
	sort.SliceStable(models, func(i, j int) bool {
		gi, gj := descriptorGroup(models[i]), descriptorGroup(models[j])
		return gi < gj
	})

	items := make([]list.Item, 0, len(models))
	selectedIdx := 0
	for _, model := range models {
		if model.Name == currentModel || model.Current {
			selectedIdx = len(items)
		}
		items = append(items, modelItem{name: model.Name, descriptor: model})
	}

	compact := compactModelPicker(terminalWidth, terminalHeight)
	// Model identity and decision state belong to the navigable row; the
	// selected-detail strip below owns metadata. Model rows intentionally stay
	// single-line at every terminal size so metadata is not repeated and mixed
	// local/cloud inventories remain simultaneously scannable.
	delegate := newPickerDelegate(isDark, true, themeID)
	pickerW := pickerListWidth(terminalWidth)
	pickerH := modelPickerListHeight(terminalHeight, len(items), delegate.Height())
	l := list.New(items, delegate, pickerW, pickerH)
	configurePickerList(&l, isDark, themeID, reducedMotion...)
	setSettingsTitleDensity(&l, compact)
	l.Title = "Ollama models"
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(!compact && len(items)*delegate.Height() > pickerH-2)
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()
	if len(items) > 0 {
		l.Select(selectedIdx)
	}

	state := &ModelPickerState{
		List:         l,
		Inventory:    models,
		CurrentModel: currentModel,
		Compact:      compact,
		ItemHeight:   delegate.Height(),
	}
	if len(models) == 0 {
		state.Notice = "No models found · add a model or refresh Ollama"
	}
	return state
}

func descriptorGroup(model OllamaModelDescriptor) int {
	switch model.Source {
	case OllamaModelLocal:
		return 0
	case OllamaModelCloud:
		return 1
	default:
		return 2
	}
}

func modelGroupLabel(group int) string {
	switch group {
	case 0:
		return "LOCAL"
	case 1:
		return "CLOUD"
	case 2:
		return "REMOTE"
	default:
		return "UNAVAILABLE"
	}
}

func modelRowState(model OllamaModelDescriptor, legacyCurrent, legacyUnsafe bool) string {
	switch {
	case !model.Selectable || !model.Fit || legacyUnsafe:
		return "unavailable"
	case model.Current || legacyCurrent:
		return "current"
	case model.Running:
		return "running"
	case model.Source == OllamaModelLocal && model.ManualOnly:
		return "manual"
	default:
		return "available"
	}
}

func modelDecisionReason(model OllamaModelDescriptor) string {
	reason := sanitizeTerminalSingleLine(model.Reason)
	switch {
	case !model.Selectable || !model.Fit:
		if reason != "" {
			return reason
		}
		return "model is unavailable under the current Ollama policy"
	case model.Source == OllamaModelLocal && model.ManualOnly:
		if reason != "" {
			return reason
		}
		return "manual-only profile; automatic routing will not select it"
	default:
		return ""
	}
}

func compactModelPicker(terminalWidth, terminalHeight int) bool {
	return terminalWidth <= modelPickerCompactWidth || terminalHeight <= modelPickerCompactRows
}

func modelPickerListHeight(terminalHeight, itemCount, itemHeight int) int {
	// Reserve room for the frame, primary key hints, and the selected-model
	// detail. Compact terminals may need one extra wrapped decision line.
	if terminalHeight <= modelPickerCompactRows {
		// At the supported 30x12 minimum, keep one navigable result below the
		// title and give the selected-detail strip the remaining rows. Bubbles
		// scrolls the result window as selection moves.
		return 2
	}
	return pickerListHeight(terminalHeight, itemCount*itemHeight+2, 7)
}

func modelSelectionState(model OllamaModelDescriptor) string {
	parts := []string{modelGroupLabel(descriptorGroup(model))}
	switch {
	case !model.Selectable || !model.Fit:
		parts = append(parts, "unavailable")
	case model.Current:
		parts = append(parts, "current")
	case model.Running:
		parts = append(parts, "running")
	case model.Source == OllamaModelLocal && model.ManualOnly:
		parts = append(parts, "manual-only")
	default:
		parts = append(parts, "available")
	}
	if model.Current && model.Running {
		parts = append(parts, "running")
	}
	return strings.Join(parts, " · ")
}

func (m *Model) renderModelSelectionDetail(state *ModelPickerState, width int) string {
	if state == nil {
		return ""
	}
	descriptor, ok := state.SelectedDescriptor()
	if !ok {
		return m.styles.OverlayDim.Render(wrapText(sanitizeTerminalSingleLine(state.Notice), width))
	}

	lines := []string{m.styles.OverlayAccent.Render(wrapText(modelSelectionState(descriptor), width))}
	if metadata := (modelItem{descriptor: descriptor}).Description(); metadata != "" {
		// Capability joins are compact inside list rows, but the selected-detail
		// strip should wrap at semantic boundaries instead of splitting a word.
		metadata = strings.ReplaceAll(metadata, "+", " · ")
		if state.Compact {
			metadata = strings.ReplaceAll(metadata, " max ctx", " ctx")
		}
		lines = append(lines, m.styles.OverlayDim.Render(wrapText(metadata, width)))
	}
	if reason := modelDecisionReason(descriptor); reason != "" {
		lines = append(lines, m.styles.OverlayDim.Render(wrapText("Unavailable · "+reason, width)))
	}
	if state.Notice != "" {
		if notice := sanitizeTerminalSingleLine(state.Notice); notice != "" {
			lines = append(lines, m.styles.OverlayDim.Render(wrapText(notice, width)))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderModelPicker() string {
	ps := m.modelPickerState
	if ps == nil {
		return ""
	}
	width := pickerListWidth(m.width)
	footer := m.renderKeyHints(width,
		keyHint{Key: m.keys.Cancel.Help().Key, Action: m.overlayCloseLabel()},
		keyHint{Key: m.keys.CompleteSelect.Help().Key, Action: "select"},
		keyHint{Key: "/", Action: "filter"},
	)
	content := strings.TrimRight(ps.List.View(), "\n")
	if detail := m.renderModelSelectionDetail(ps, width); detail != "" {
		content += "\n" + detail
	}
	return m.renderPickerFrame(content, footer)
}

func humanModelBytes(size int64) string {
	if size <= 0 {
		return ""
	}
	const gib = int64(1024 * 1024 * 1024)
	const mib = int64(1024 * 1024)
	if size >= gib {
		return fmt.Sprintf("%.1f GB", float64(size)/float64(gib))
	}
	return fmt.Sprintf("%d MB", max(int64(1), size/mib))
}

func compactTokenCount(value int) string {
	if value >= 1000 {
		return fmt.Sprintf("%dK", value/1000)
	}
	return fmt.Sprintf("%d", value)
}

func compactCapabilities(values []string) string {
	wanted := []string{"tools", "thinking", "vision", "completion", "embedding"}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "tool" || value == "tool_calling" {
			value = "tools"
		}
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for _, value := range wanted {
		if seen[value] {
			result = append(result, value)
		}
	}
	return strings.Join(result, "+")
}

// openModelPicker shows the model picker overlay.
func (m *Model) openModelPicker() {
	// The catalog is the authority for any provider it knows. The local
	// inventory below is the fallback for a runtime it cannot describe.
	if providerID, models, ok := m.catalogModelsForActiveProvider(); ok {
		m.modelPickerState = newCatalogModelPickerState(
			providerID, models, m.model, m.width, m.height, m.isDark, m.themeID, m.reducedMotion,
		)
		m.restylePickerOverlays()
		m.overlay = OverlayModelPicker
		m.input.Blur()
		return
	}
	// Past this point the catalog does not describe the active provider, and in
	// this fork there is exactly one such provider: `ollama`, which means
	// Ollama Cloud and is absent from the Catwalk snapshot.
	//
	// The local daemon's inventory used to answer here, and it is the wrong
	// answer twice over. Those models run on this machine, and sonar has no
	// local runtime to run them with — ProviderProfile.IsRemote() is constant
	// true — so every row offered a switch that could not happen. It also
	// described a DIFFERENT service than the one in use: Ollama Cloud is a
	// hosted OpenAI-compatible endpoint that shares a name with the daemon and
	// nothing else. A picker listing one while connected to the other is not a
	// fallback, it is a wrong answer delivered confidently.
	//
	// Saying so is the honest option. sonar cannot enumerate models for a
	// provider outside the catalog, and the configured model still works — it
	// simply cannot be chosen from a list nobody can build.
	// Config-declared models remain a legitimate fallback: they name what the
	// operator chose, not what some daemon happens to have pulled.
	if m.router == nil {
		return
	}
	catalog := m.router.ListModels()
	byName := make(map[string]config.Model, len(catalog))
	for _, model := range catalog {
		byName[model.Name] = model
	}
	models := catalog
	if len(m.modelList) > 0 {
		models = make([]config.Model, 0, len(m.modelList))
		for _, name := range m.modelList {
			if model, ok := byName[name]; ok {
				models = append(models, model)
			} else {
				models = append(models, config.Model{
					Name: name, DisplayName: name, Size: "local", Capability: config.CapabilityMedium,
				})
			}
		}
	}
	if len(models) == 0 {
		return
	}

	m.modelPickerState = newModelPickerState(models, m.model, m.width, m.height, m.isDark, m.themeID, m.reducedMotion)
	m.restylePickerOverlays()
	m.overlay = OverlayModelPicker
	m.input.Blur()
}

// selectModel switches to the given model and closes the picker.
func (m *Model) selectModel(name string) {
	if descriptor, ok := m.ollamaModelDescriptor(name); ok {
		if !descriptor.Selectable || !descriptor.Fit {
			reason := descriptor.Reason
			if reason == "" {
				reason = "model is not admitted by the current Ollama policy"
			}
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: reason})
			m.closeModelPicker()
			m.refreshTranscript()
			m.resumeFollow()
			return
		}
	} else if _, _, hosted := catalog.FindModel(name); hosted {
		// A hosted model runs on the provider's hardware. The local memory
		// guard below describes this machine's RAM and would reject a perfectly
		// valid remote model on the strength of its name.
	} else if err := config.CheckModelMemorySafe(name); err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		m.closeModelPicker()
		m.refreshTranscript()
		m.resumeFollow()
		return
	}
	m.switchSelectedModel(name)
}

// switchSelectedModel commits a model switch after admission checks succeed.
func (m *Model) switchSelectedModel(name string) bool {
	old := m.model
	if config.CanonicalModelName(old) == config.CanonicalModelName(name) && strings.TrimSpace(old) != "" {
		// Selecting the active model is idempotent. This also absorbs duplicate
		// Enter/delivery events without re-preparing the provider or stacking
		// identical `Model` receipts in the transcript.
		m.modelPinned = true
		m.saveManualModelPreference(name)
		for index := range m.ollamaModels {
			m.ollamaModels[index].Current = config.CanonicalModelName(m.ollamaModels[index].Name) == config.CanonicalModelName(name)
		}
		m.closeModelPicker()
		m.refreshTranscript()
		m.resumeFollow()
		return true
	}
	if m.modelManager != nil {
		m.prepareModelSwitch()
		if err := m.modelManager.SetCurrentModel(name); err != nil {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: fmt.Sprintf("Failed to switch model: %v", err),
			})
			m.closeModelPicker()
			return false
		}
	}
	m.setCurrentModelProjection(name)
	m.ollamaOffline = false
	m.modelPinned = true
	m.saveManualModelPreference(name)
	for index := range m.ollamaModels {
		m.ollamaModels[index].Current = config.CanonicalModelName(m.ollamaModels[index].Name) == config.CanonicalModelName(name)
	}
	if m.logger != nil {
		m.logger.Info("model switched", "from", old, "to", name)
	}
	// Empty state and the fixed status line already own the current model. Once
	// a conversation exists, retain one compact transition receipt.
	if m.conversationStarted() {
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Model · " + m.currentModelSurfaceLabel(false)})
	}
	m.closeModelPicker()
	m.refreshTranscript()
	m.resumeFollow()
	return true
}

func (m *Model) ollamaModelDescriptor(name string) (OllamaModelDescriptor, bool) {
	wanted := config.CanonicalModelName(name)
	for _, descriptor := range m.ollamaModels {
		if config.CanonicalModelName(descriptor.Name) == wanted {
			return descriptor, true
		}
	}
	return OllamaModelDescriptor{}, false
}

func (m *Model) validateModelAdmission(name string) error {
	if descriptor, ok := m.ollamaModelDescriptor(name); ok {
		// No local-only gate on inference here, and its absence is the point.
		//
		// privacy.local_only bounds TOOL endpoints. In sonar it can never bound
		// inference, because sonar has no local inference to fall back to:
		// ProviderProfile.IsRemote() is constant true, so a rule refusing a
		// hosted model could only ever refuse every model the harness supports.
		// This surface used to carry that rule anyway — a copy of local-agent's,
		// where it is correct because Ollama really does run models on the
		// machine — and it contradicted both AGENTS.md and
		// TestSwitchProviderIgnoresLocalOnly, which pins the opposite.
		//
		// See TestLocalOnlyNeverGatesInferenceInSonar. If a local runtime ever
		// returns, the rule comes back with it.
		if descriptor.Selectable && descriptor.Fit {
			return nil
		}
		if descriptor.Reason != "" {
			return errors.New(descriptor.Reason)
		}
		return fmt.Errorf("model %q is not admitted by the current Ollama policy", name)
	}
	if m.ollamaInventoryAttempted {
		return fmt.Errorf("model %q is absent from the current Ollama inventory", name)
	}
	return config.CheckModelMemorySafe(name)
}

// closeModelPicker dismisses the model picker overlay.
func (m *Model) closeModelPicker() {
	m.modelPickerState = nil
	m.closeOverlayToParent()
}

// hasOllamaCapability reports whether a locally discovered model advertises a
// capability. Transitional: models reachable over an API answer this from the
// provider catalog instead, and this helper retires with the Ollama surface.
func hasOllamaCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if strings.EqualFold(strings.TrimSpace(capability), want) {
			return true
		}
	}
	return false
}

// BuildOllamaModelDescriptors projects a locally discovered inventory into
// picker descriptors.
//
// Transitional. The admission logic this replaced encoded local-runtime
// concerns — memory fit, VRAM residency, automatic-routing eligibility, and
// Ollama Cloud consent — none of which describe a model reached over an API.
// Every discovered model is now simply selectable, and the catalog-driven
// picker will supersede this projection entirely.
func BuildOllamaModelDescriptors(
	models []llm.OllamaModel,
	running []llm.OllamaRunningModel,
	currentModel string,
	_ bool,
) []OllamaModelDescriptor {
	runningByName := make(map[string]llm.OllamaRunningModel, len(running))
	for _, entry := range running {
		runningByName[config.CanonicalModelName(entry.Model.Name)] = entry
	}
	out := make([]OllamaModelDescriptor, 0, len(models))
	for _, model := range models {
		canonical := config.CanonicalModelName(model.Name)
		descriptor := OllamaModelDescriptor{
			Name:          model.Name,
			DisplayName:   model.Name,
			Source:        OllamaModelLocal,
			SizeBytes:     model.SizeBytes,
			ParameterSize: model.ParameterSize,
			Quantization:  model.Quantization,
			ContextLength: boundedContextLength(model.ContextLength),
			Capabilities:  append([]string(nil), model.Capabilities...),
			Current:       canonical == config.CanonicalModelName(currentModel),
			Selectable:    true,
			Fit:           true,
			AutoRoutable:  true,
		}
		if entry, ok := runningByName[canonical]; ok {
			descriptor.Running = true
			descriptor.AllocatedContext = boundedContextLength(int64(entry.ContextLength))
			descriptor.SizeVRAM = entry.SizeVRAM
		}
		descriptor.EffectiveContext = descriptor.ContextLength
		out = append(out, descriptor)
	}
	return out
}

// boundedContextLength clamps a reported context length into int range.
func boundedContextLength(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > int64(math.MaxInt32) {
		return math.MaxInt32
	}
	return int(value)
}
