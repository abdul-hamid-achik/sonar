package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// setCurrentModelProjection commits the presentation-side half of an already
// successful ModelManager switch. Context occupancy belongs to one model, so
// it must not be carried across a denominator change.
func (m *Model) setCurrentModelProjection(name string) {
	previous := config.CanonicalModelName(m.model)
	changed := previous != "" && previous != config.CanonicalModelName(name)
	m.model = name
	if changed && m.agent != nil {
		m.agent.CommitModelSwitch()
	}
	m.syncEffectiveContext(changed)
}

func (m *Model) syncEffectiveContext(resetOccupancy bool) {
	if m.modelManager != nil {
		m.numCtx = m.modelManager.NumCtx()
	} else if m.agent != nil {
		if effective := m.agent.NumCtx(); effective > 0 {
			m.numCtx = effective
		}
	}
	if resetOccupancy {
		m.promptTokens = 0
	}
}

func (m *Model) openModelDetails(model OllamaModelDescriptor) {
	copy := model
	m.modelDetailsState = &copy
	m.overlay = OverlayModelDetails
	m.input.Blur()
}

func (m *Model) requestOllamaModelDetails(model OllamaModelDescriptor) tea.Cmd {
	if m.modelManager == nil {
		return nil
	}
	manager := m.modelManager
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		info, err := manager.ShowOllamaModel(ctx, model.Name)
		if err != nil {
			return OllamaModelDetailsResultMsg{Model: model, Err: err}
		}
		model.ParameterSize = info.Model.ParameterSize
		model.Quantization = info.Model.Quantization
		model.Capabilities = append([]string(nil), info.Capabilities...)
		if info.NativeContext > 0 {
			model.ContextLength = boundedContextLength(info.NativeContext)
		}
		return OllamaModelDetailsResultMsg{Model: model}
	}
}

func (m *Model) closeModelDetails() {
	m.modelDetailsState = nil
	m.overlay = OverlayModelPicker
}

func (m *Model) renderModelDetails() string {
	if m.modelDetailsState == nil {
		return ""
	}
	width := pickerListWidth(m.width)
	content := renderOllamaModelDetails(*m.modelDetailsState, width, m.isDark, m.themeID)
	if compactModelPicker(m.width, m.height) {
		content = renderCompactOllamaModelDetails(*m.modelDetailsState, width, m.isDark, m.themeID)
	}
	footer := m.renderKeyHints(width, keyHint{Key: "esc", Action: "models"})
	return m.renderPickerFrame(content, footer)
}

func (m *Model) openModelPull() tea.Cmd {
	m.modelPullState = NewModelPullState(m.isDark, m.reducedMotion, m.themeID)
	m.overlay = OverlayModelPull
	m.input.Blur()
	return m.modelPullState.Input.Focus()
}

func (m *Model) closeModelPull() {
	m.cancelModelPull()
	m.modelPullState = nil
	if m.ollamaInventoryAttempted {
		m.modelPickerState = newOllamaModelPickerState(m.ollamaModels, m.model, m.width, m.height, m.isDark, m.themeID, m.reducedMotion)
		m.restylePickerOverlays()
		if m.ollamaVersion != "" {
			m.modelPickerState.List.Title = ollamaModelPickerTitle(m.ollamaVersion)
		}
	}
	m.overlay = OverlayModelPicker
}

func (m *Model) renderModelPull() (string, *tea.Cursor) {
	if m.modelPullState == nil {
		return "", nil
	}
	width := pickerListWidth(m.width)
	content, inputCursor := m.modelPullState.ViewWithCursor(width, compactModelPicker(m.width, m.height))
	footerAction := "close"
	if m.modelPullState.Phase == ModelPullRunning {
		footerAction = "cancel"
	}
	hints := []keyHint{{Key: "esc", Action: footerAction}}
	switch m.modelPullState.Phase {
	case ModelPullEntry:
		hints = append(hints, keyHint{Key: "enter", Action: "pull"})
	case ModelPullFailed:
		hints = append(hints, keyHint{Key: "enter/r", Action: "retry"}, keyHint{Key: "e", Action: "edit"})
	}
	footer := m.renderKeyHints(width, hints...)
	view := m.renderPickerFrame(content, footer)
	if m.modelPullState.Phase != ModelPullEntry {
		return view, nil
	}
	return view, pickerFrameCursor(inputCursor)
}

func (m *Model) startModelPull(name string) tea.Cmd {
	if m.modelManager == nil || strings.TrimSpace(name) == "" {
		return nil
	}
	m.cancelModelPull()
	m.modelPullRequest++
	requestID := m.modelPullRequest
	ctx, cancel := context.WithCancel(context.Background())
	m.modelPullCancel = cancel
	m.modelPullRunning = true
	progress := make(chan OllamaModelPullProgressMsg, 8)
	m.modelPullProgress = progress
	go func() {
		err := m.modelManager.PullOllamaModel(ctx, name, func(update llm.OllamaPullProgress) error {
			message := OllamaModelPullProgressMsg{RequestID: requestID, Name: name, Status: update.Status, Completed: update.Completed, Total: update.Total}
			select {
			case progress <- message:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if errors.Is(err, context.Canceled) {
			err = errors.New("model download cancelled")
		} else if err != nil {
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "401") || strings.Contains(lower, "403") || strings.Contains(lower, "unauthorized") {
				err = errors.New("ollama authentication required; run `ollama signin` for the local daemon or set OLLAMA_API_KEY for ollama.com")
			}
		}
		// Terminal state supersedes queued progress. Drain coalesced frames so
		// the owned receipt remains deliverable even after the modal is closed.
		for {
			select {
			case <-progress:
				continue
			default:
				progress <- OllamaModelPullProgressMsg{RequestID: requestID, Name: name, Done: err == nil, Err: err}
				close(progress)
				return
			}
		}
	}()
	return waitModelPullProgress(progress)
}

func waitModelPullProgress(progress <-chan OllamaModelPullProgressMsg) tea.Cmd {
	return func() tea.Msg {
		message, ok := <-progress
		if !ok {
			return nil
		}
		return message
	}
}

func (m *Model) cancelModelPull() {
	if m.modelPullCancel != nil {
		m.modelPullCancel()
	}
	m.modelPullCancel = nil
}

func (m *Model) refreshOllamaInventory() tea.Cmd {
	if m.modelManager == nil {
		return nil
	}
	m.modelInventoryRequest++
	requestID := m.modelInventoryRequest
	manager := m.modelManager
	current, localOnly := m.model, m.localOnly
	previous := append([]OllamaModelDescriptor(nil), m.ollamaModels...)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		inventory, err := manager.ListOllamaModels(ctx)
		if err != nil {
			return OllamaModelInventoryMsg{RequestID: requestID, Err: err}
		}
		inventory = enrichRefreshedOllamaCapabilities(ctx, manager, inventory, previous)
		running, _ := manager.ListRunningOllamaModels(ctx)
		return OllamaModelInventoryMsg{RequestID: requestID, Models: BuildOllamaModelDescriptors(inventory, running, current, localOnly)}
	}
}

func enrichRefreshedOllamaCapabilities(ctx context.Context, manager *llm.ModelManager, inventory []llm.OllamaModel, previous []OllamaModelDescriptor) []llm.OllamaModel {
	known := make(map[string][]string, len(previous))
	for _, model := range previous {
		if len(model.Capabilities) > 0 {
			known[config.CanonicalModelName(model.Name)] = append([]string(nil), model.Capabilities...)
		}
	}
	result := append([]llm.OllamaModel(nil), inventory...)
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for index := range result {
		if len(result[index].Capabilities) > 0 && result[index].ContextLength > 0 {
			continue
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				return
			}
			if info, err := manager.ShowOllamaModel(ctx, result[index].Name); err == nil {
				if len(result[index].Capabilities) == 0 && len(info.Capabilities) > 0 {
					result[index].Capabilities = append([]string(nil), info.Capabilities...)
				}
				if info.NativeContext > 0 {
					result[index].ContextLength = info.NativeContext
				}
				if len(result[index].Capabilities) > 0 {
					return
				}
			}
			result[index].Capabilities = append([]string(nil), known[config.CanonicalModelName(result[index].Name)]...)
		}(index)
	}
	wg.Wait()
	return result
}

func (m *Model) applyOllamaInventory(message OllamaModelInventoryMsg) {
	if message.RequestID != m.modelInventoryRequest {
		return
	}
	if message.Err != nil {
		m.ollamaOffline = true
		if m.modelPickerState != nil {
			m.modelPickerState.List.StopSpinner()
			m.modelPickerState.List.Title = "Ollama models · offline"
			m.modelPickerState.Notice = "Refresh failed · " + message.Err.Error()
		}
		if m.modelPullState != nil && m.modelPullState.Phase == ModelPullComplete {
			m.modelPullState.Status = "Inventory refresh failed · close and press r to retry"
		}
		return
	}
	m.ollamaOffline = false
	m.ollamaInventoryAttempted = true
	models := m.applyModelRoutingPolicy(message.Models)
	granted := make(map[string]bool, len(m.ollamaModels))
	for _, previous := range m.ollamaModels {
		if previous.ConsentGranted {
			granted[config.CanonicalModelName(previous.Name)] = true
		}
	}
	for index := range models {
		models[index].Current = config.CanonicalModelName(models[index].Name) == config.CanonicalModelName(m.model)
		switch models[index].Source {
		case OllamaModelLocal:
		case OllamaModelCloud:
			if m.localOnly && granted[config.CanonicalModelName(models[index].Name)] {
				models[index].RequiresConsent = false
				models[index].ConsentGranted = true
				models[index].Reason = "Ollama Cloud · conversation consent"
			}
		}
	}
	if m.modelManager != nil {
		for index := range models {
			models[index].EffectiveContext = m.modelManager.ContextPolicy(models[index].Name).Effective
		}
		m.syncEffectiveContext(false)
	}
	m.ollamaModels = models
	if m.modelPullState != nil && m.modelPullState.Phase == ModelPullComplete {
		// Pull completion means the daemon accepted the download. Only surface
		// the stable refresh receipt after the exact inventory has also crossed
		// the parent-owned authority commit boundary.
		m.modelPullState.Status = "Inventory refreshed"
	}
	m.modelList = manuallySelectableOllamaModels(models)
	autoModels := make([]string, 0, len(models))
	for _, model := range models {
		// Source is the authority boundary. AutoRoutable is still useful
		// policy metadata for local models, but a stale or malformed remote
		// descriptor must never cross into automatic selection.
		if model.Source == OllamaModelLocal && model.Selectable && model.Fit && model.AutoRoutable {
			autoModels = append(autoModels, model.Name)
		}
	}
	if aware, ok := m.router.(interface{ SetAvailableModels([]string) }); ok {
		aware.SetAvailableModels(autoModels)
	}
	if m.completer != nil {
		m.completer.UpdateModels(m.modelList)
	}
	if m.overlay == OverlayModelPicker {
		m.modelPickerState = newOllamaModelPickerState(models, m.model, m.width, m.height, m.isDark, m.themeID, m.reducedMotion)
		m.restylePickerOverlays()
		if m.ollamaVersion != "" {
			m.modelPickerState.List.Title = ollamaModelPickerTitle(m.ollamaVersion)
		}
	}
	if m.settingsPickerState != nil {
		m.refreshSettingsPicker()
	}
}

func (m *Model) applyModelRoutingPolicy(models []OllamaModelDescriptor) []OllamaModelDescriptor {
	result := append([]OllamaModelDescriptor(nil), models...)
	for index := range result {
		if result[index].Source != OllamaModelLocal {
			continue
		}
		if _, manualOnly := m.manualOnlyModels[config.CanonicalModelName(result[index].Name)]; manualOnly {
			result[index].AutoRoutable = false
			result[index].ManualOnly = true
		}
	}
	return result
}

// commitOllamaInventory serializes the manager's privacy/context snapshot on a
// Bubble Tea command goroutine. ConfigureOllamaRuntimeInventory may wait for an
// active provider stream, so it must never run inside Update where it would
// freeze painting and keyboard cancellation.
func (m *Model) commitOllamaInventory(message OllamaModelInventoryMsg) tea.Cmd {
	m.ollamaInventoryCommitting = true
	m.ollamaInventoryCommitID = message.RequestID
	manager := m.modelManager
	agent := m.agent
	localOnly := m.localOnly
	runtimeInventory := runtimeInventoryFromDescriptors(message.Models)
	recoveryTarget := recoverableLocalModel(message.Models, m.model)
	selection := reconcileRefreshedCurrentModel(m.ollamaModels, message.Models, m.model, m.modelPinned, localOnly)
	return func() tea.Msg {
		// Optional ICE auto-memory shares the same manager inference lease. Cancel
		// and join it before committing inventory or recovering a model so a
		// background 30-second extraction cannot stall refresh or shutdown.
		if agent != nil {
			agent.PrepareModelSwitch()
		}
		manager.ConfigureOllamaRuntimeInventory(localOnly, runtimeInventory, true)
		receipt := ollamaModelInventoryCommittedMsg{Inventory: message}
		if selection.Change {
			receipt.SelectionChanged = true
			receipt.PreviousModel = selection.PreviousModel
			receipt.SelectionReason = selection.Reason
			if selection.FallbackModel != "" {
				if err := manager.SetCurrentModel(selection.FallbackModel); err == nil {
					receipt.SelectedModel = selection.FallbackModel
				} else {
					receipt.SelectionErr = err
				}
			}
			if receipt.SelectedModel == "" {
				receipt.SelectionErr = errors.Join(receipt.SelectionErr, manager.ClearCurrentModel())
			}
		} else if manager.CurrentModel() == "" && recoveryTarget != "" {
			if err := manager.SetCurrentModel(recoveryTarget); err != nil {
				receipt.RecoveryErr = err
			} else {
				receipt.RecoveredModel = recoveryTarget
			}
		}
		return receipt
	}
}

type refreshedCurrentModelDecision struct {
	Change        bool
	PreviousModel string
	FallbackModel string
	Reason        string
}

// reconcileRefreshedCurrentModel decides whether a verified inventory may
// retain the current selection. A local model stays valid while it remains
// runnable. Ollama Cloud is retained only when it was already a deliberate,
// pinned Cloud choice under the same conversation authority. A local or
// automatic selection that changes execution location is never grandfathered
// into network access by refresh alone.
func reconcileRefreshedCurrentModel(
	previous, refreshed []OllamaModelDescriptor,
	current string,
	pinned, localOnly bool,
) refreshedCurrentModelDecision {
	wanted := config.CanonicalModelName(current)
	if wanted == "" {
		return refreshedCurrentModelDecision{}
	}
	prior, priorFound := descriptorByCanonicalName(previous, wanted)
	next, nextFound := descriptorByCanonicalName(refreshed, wanted)

	retain := false
	if nextFound && next.Selectable && next.Fit {
		switch next.Source {
		case OllamaModelLocal:
			retain = true
		case OllamaModelCloud:
			retain = pinned && priorFound && prior.Source == OllamaModelCloud && (!localOnly || prior.ConsentGranted)
		}
	}
	if retain {
		return refreshedCurrentModelDecision{}
	}

	decision := refreshedCurrentModelDecision{
		Change:        true,
		PreviousModel: current,
		Reason:        refreshedCurrentModelRejection(next, nextFound),
	}
	// Automatic selection may move only to another verified local routing
	// candidate. An invalid explicit pin is cleared so the user's preference is
	// not silently replaced with a different model.
	if !pinned {
		for _, candidate := range refreshed {
			if candidate.Source == OllamaModelLocal && candidate.Selectable && candidate.Fit && candidate.AutoRoutable {
				decision.FallbackModel = candidate.Name
				break
			}
		}
	}
	return decision
}

func descriptorByCanonicalName(models []OllamaModelDescriptor, canonical string) (OllamaModelDescriptor, bool) {
	for _, model := range models {
		if config.CanonicalModelName(model.Name) == canonical {
			return model, true
		}
	}
	return OllamaModelDescriptor{}, false
}

func refreshedCurrentModelRejection(model OllamaModelDescriptor, found bool) string {
	if !found {
		return "the current model is absent from the refreshed Ollama inventory"
	}
	switch model.Source {
	case OllamaModelCloud:
		return "the current model is now Ollama Cloud and requires a new manual selection"
	case OllamaModelRemote:
		return "the current model is now served by an unsupported remote Ollama host"
	case OllamaModelLocal:
		if strings.TrimSpace(model.Reason) != "" {
			return "the current local model is no longer runnable: " + strings.TrimSpace(model.Reason)
		}
		return "the current local model is no longer runnable"
	default:
		return "the current model execution location is unknown"
	}
}

// recoverableLocalModel returns only the exact presentation model that a
// verified refresh has now proven safe and runnable locally. Refresh never
// crosses into Cloud implicitly and never substitutes a different model.
func recoverableLocalModel(models []OllamaModelDescriptor, current string) string {
	wanted := config.CanonicalModelName(current)
	if wanted == "" {
		return ""
	}
	for _, model := range models {
		if config.CanonicalModelName(model.Name) == wanted &&
			model.Source == OllamaModelLocal && model.Selectable && model.Fit {
			return model.Name
		}
	}
	return ""
}

func runtimeInventoryFromDescriptors(models []OllamaModelDescriptor) []llm.OllamaModel {
	result := make([]llm.OllamaModel, 0, len(models))
	for _, model := range models {
		location := llm.OllamaModelLocationRemote
		switch model.Source {
		case OllamaModelLocal:
			location = llm.OllamaModelLocationLocal
		case OllamaModelCloud:
			location = llm.OllamaModelLocationCloud
		}
		result = append(result, llm.OllamaModel{
			Name: model.Name, Location: location, SizeBytes: model.SizeBytes, ContextLength: int64(model.ContextLength),
		})
	}
	return result
}

func manuallySelectableOllamaModels(models []OllamaModelDescriptor) []string {
	result := make([]string, 0, len(models))
	for _, model := range models {
		if model.Selectable && model.Fit {
			result = append(result, model.Name)
		}
	}
	return result
}

// handleModelPullRequested starts a model download and its spinner clock.
func (m *Model) handleModelPullRequested(msg OllamaModelPullRequestedMsg, cmds []tea.Cmd) []tea.Cmd {
	cmds = append(cmds, m.startModelPull(msg.Name))
	if m.modelPullState != nil && !m.reducedMotion {
		cmds = append(cmds, m.modelPullState.Spinner.Tick)
	}
	return cmds
}

// handleModelPullCancelRequested cancels an in-flight model download.
func (m *Model) handleModelPullCancelRequested(msg OllamaModelPullCancelRequestedMsg) {
	m.cancelModelPull()
	if m.modelPullState != nil {
		m.modelPullState.Apply(OllamaModelPullProgressMsg{Name: msg.Name, Err: errors.New("model download cancelled")})
	}
}

// handleModelPullProgress applies a tokened model download progress receipt.
func (m *Model) handleModelPullProgress(msg OllamaModelPullProgressMsg, cmds []tea.Cmd) []tea.Cmd {
	if msg.RequestID != m.modelPullRequest {
		return cmds
	}
	if m.modelPullState == nil {
		if msg.Done || msg.Err != nil {
			m.modelPullRunning = false
			m.cancelModelPull()
			m.appendShutdownQuit(&cmds)
		}
		return cmds
	}
	cmds = append(cmds, m.modelPullState.Apply(msg))
	if msg.Done && msg.Err == nil {
		m.modelPullRunning = false
		m.cancelModelPull()
		cmds = append(cmds, m.refreshOllamaInventory())
		m.appendShutdownQuit(&cmds)
	} else if msg.Err != nil {
		m.modelPullRunning = false
		m.cancelModelPull()
		m.appendShutdownQuit(&cmds)
	} else if m.modelPullProgress != nil {
		cmds = append(cmds, waitModelPullProgress(m.modelPullProgress))
	}
	return cmds
}

// handleOllamaModelInventory routes a tokened inventory snapshot either
// directly into projections or through the serialized commit path.
func (m *Model) handleOllamaModelInventory(msg OllamaModelInventoryMsg, cmds []tea.Cmd) []tea.Cmd {
	if msg.RequestID != m.modelInventoryRequest {
		return cmds
	}
	msg.Models = m.applyModelRoutingPolicy(msg.Models)
	if msg.Err != nil || m.modelManager == nil {
		m.applyOllamaInventory(msg)
		if m.ollamaOffline && m.modelManager != nil {
			cmds = append(cmds, m.scheduleOllamaReconnect())
		}
		return cmds
	}
	if m.ollamaInventoryCommitting {
		copy := msg
		m.pendingOllamaInventory = &copy
		return cmds
	}
	cmds = append(cmds, m.commitOllamaInventory(msg))
	return cmds
}

// handleOllamaInventoryCommitted reconciles a committed inventory snapshot
// with the current model selection and any pending refresh.
func (m *Model) handleOllamaInventoryCommitted(msg ollamaModelInventoryCommittedMsg, cmds []tea.Cmd) []tea.Cmd {
	if msg.Inventory.RequestID != m.ollamaInventoryCommitID {
		return cmds
	}
	m.ollamaInventoryCommitting = false
	if !m.shuttingDown && msg.Inventory.RequestID == m.modelInventoryRequest {
		m.applyOllamaInventory(msg.Inventory)
		switch {
		case msg.SelectionChanged:
			m.setCurrentModelProjection(msg.SelectedModel)
			if msg.SelectedModel != "" {
				m.modelPinned = false
			}
			for index := range m.ollamaModels {
				m.ollamaModels[index].Current = config.CanonicalModelName(m.ollamaModels[index].Name) == config.CanonicalModelName(msg.SelectedModel) && msg.SelectedModel != ""
			}
			detail := msg.SelectionReason
			if msg.SelectionErr != nil {
				detail += fmt.Sprintf("; reconciliation warning: %v", msg.SelectionErr)
			}
			if msg.SelectedModel != "" {
				m.appendGoalSystem(fmt.Sprintf("Ollama inventory changed · %s · resumed automatic routing on local model %s", detail, msg.SelectedModel))
			} else {
				m.appendGoalError(fmt.Sprintf("Ollama inventory changed · %s. Model %q was cleared; select a verified model before the next turn.", detail, msg.PreviousModel))
			}
		case msg.RecoveryErr != nil:
			detail := fmt.Sprintf("Ollama inventory recovered, but model %q could not be activated: %v", m.model, msg.RecoveryErr)
			m.appendGoalError(detail)
			if m.modelPickerState != nil {
				m.modelPickerState.Notice = detail
			}
		case msg.RecoveredModel != "":
			m.setCurrentModelProjection(msg.RecoveredModel)
			m.appendGoalSystem(fmt.Sprintf("Ollama reconnected · %s ready", msg.RecoveredModel))
		}
	}
	if !m.shuttingDown && m.pendingOllamaInventory != nil {
		pending := *m.pendingOllamaInventory
		m.pendingOllamaInventory = nil
		if pending.RequestID == m.modelInventoryRequest {
			cmds = append(cmds, m.commitOllamaInventory(pending))
		}
	}
	m.appendShutdownQuit(&cmds)
	return cmds
}

// scheduleOllamaReconnect returns a tick that retries Ollama inventory after a
// delay when the daemon is unreachable. The tick self-schedules until the
// inventory succeeds or the model shuts down.
func (m *Model) scheduleOllamaReconnect() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg {
		return ollamaReconnectTickMsg{}
	})
}

// handleOllamaReconnectTick retries the Ollama inventory when the daemon was
// previously unreachable.
func (m *Model) handleOllamaReconnectTick() tea.Cmd {
	if !m.ollamaOffline || m.modelManager == nil || m.shuttingDown {
		return nil
	}
	return m.refreshOllamaInventory()
}

// scheduleModelLoadCheck returns a tick that checks whether the active model
// has finished loading into memory. Used for cold-load progress feedback.
// Skips if the model is already reported as running in the current inventory.
func (m *Model) scheduleModelLoadCheck() tea.Cmd {
	if m.modelManager == nil || m.model == "" {
		return nil
	}
	// Skip if the model is already running per the latest inventory.
	canonical := config.CanonicalModelName(m.model)
	for _, desc := range m.ollamaModels {
		if config.CanonicalModelName(desc.Name) == canonical && desc.Running {
			return nil
		}
	}
	manager := m.modelManager
	model := m.model
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		running, err := manager.ListRunningOllamaModels(ctx)
		if err != nil {
			return modelLoadCheckMsg{}
		}
		for _, r := range running {
			if config.CanonicalModelName(r.Model.Name) == config.CanonicalModelName(model) {
				detail := fmt.Sprintf("%s loaded", model)
				if r.SizeVRAM > 0 {
					detail = fmt.Sprintf("%s · %.1f GB VRAM", model, float64(r.SizeVRAM)/1e9)
				}
				return modelLoadCheckMsg{Running: true, Detail: detail}
			}
		}
		return modelLoadCheckMsg{Running: false, Detail: fmt.Sprintf("Loading %s into memory…", model)}
	})
}
