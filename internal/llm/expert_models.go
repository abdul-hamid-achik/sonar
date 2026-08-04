package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// ExpertModelResource is one live Ollama model fact used by the expert
// admission planner. Inventory weights come from /api/tags and residency comes
// from /api/ps; Active is the manager's process-local usage record.
type ExpertModelResource struct {
	Name          string
	WeightBytes   int64
	ResidentBytes int64
	ContextLength int
	Location      OllamaModelLocation
	Active        bool
	Resident      bool
	Current       bool
	Selected      bool
	ExpertOnly    bool
}

// ExpertModelSnapshot is a point-in-time union of every manager-active,
// Ollama-resident, current, and selected model. InventoryVerified means the
// selected weights were resolved from the live daemon rather than startup
// configuration. The lease field is intentionally opaque and process-local.
type ExpertModelSnapshot struct {
	Models            []ExpertModelResource
	InventoryVerified bool
	lease             *expertModelLease
}

type modelActivity struct {
	expert           bool
	nonExpert        bool
	nonExpertVersion uint64
}

type expertModelLease struct {
	manager   *ModelManager
	selected  map[string]string
	protected map[string]bool
	nonExpert map[string]uint64
	releasing chan struct{}
	done      chan struct{}
	err       error
}

func (m *ModelManager) markExpertActivityLocked(model string) {
	key := modelResourceKey(model)
	activity := m.activity[key]
	activity.expert = true
	m.activity[key] = activity
}

func (m *ModelManager) markNonExpertActivityLocked(model string) {
	key := modelResourceKey(model)
	activity := m.activity[key]
	m.activitySeq++
	activity.nonExpert = true
	activity.nonExpertVersion = m.activitySeq
	m.activity[key] = activity
}

// PrepareExpertModels serializes expert consultations sharing one manager,
// refreshes live weights and residency, and returns an opaque cleanup lease.
// Ordinary current-model and embedding calls share a context-aware admission
// boundary and cannot change residency until the lease is released.
func (m *ModelManager) PrepareExpertModels(ctx context.Context, selected []string) (ExpertModelSnapshot, error) {
	if m == nil {
		return ExpertModelSnapshot{}, errors.New("model manager is unavailable")
	}
	if ctx == nil {
		return ExpertModelSnapshot{}, errors.New("expert model snapshot context is required")
	}
	selectedByKey := make(map[string]string, len(selected))
	for _, name := range selected {
		name = strings.TrimSpace(name)
		key := modelResourceKey(name)
		if key == "" {
			return ExpertModelSnapshot{}, errors.New("selected expert model name is required")
		}
		selectedByKey[key] = name
	}
	if len(selectedByKey) == 0 {
		return ExpertModelSnapshot{}, errors.New("at least one expert model is required")
	}
	m.localMu.RLock()
	localOnly := m.localOnly
	m.localMu.RUnlock()
	if localOnly {
		client, err := NewOllamaClient(m.baseURL, "", m.configuredNumCtx())
		if err != nil {
			return ExpertModelSnapshot{}, fmt.Errorf("local-only expert model snapshot: %w", err)
		}
		if err := validateLocalOllamaHost(client); err != nil {
			return ExpertModelSnapshot{}, fmt.Errorf("local-only expert model snapshot: %w", err)
		}
	}
	select {
	case m.expertGate <- struct{}{}:
	case <-ctx.Done():
		return ExpertModelSnapshot{}, ctx.Err()
	}
	releaseGate := true
	reservationHeld := false
	defer func() {
		if releaseGate {
			if reservationHeld {
				m.admission.finishReservation()
			}
			<-m.expertGate
		}
	}()
	if err := m.admission.reserve(ctx, selectedByKey); err != nil {
		return ExpertModelSnapshot{}, err
	}
	reservationHeld = true

	// Keep model selection and runtime-inventory reconfiguration stable while
	// the two live daemon snapshots are collected. The admission reservation
	// keeps ordinary chat and embedding inference from changing residency until
	// this lease is released; /api/ps remains the authoritative resident set.
	m.inferenceMu.RLock()
	inventory, inventoryErr := m.ListOllamaModels(ctx)
	if inventoryErr == nil {
		if err := ctx.Err(); err != nil {
			inventoryErr = err
		}
	}
	var running []OllamaRunningModel
	if inventoryErr == nil {
		running, inventoryErr = m.ListRunningOllamaModels(ctx)
	}
	m.mu.RLock()
	current := m.currentModel
	active := make(map[string]bool, len(m.active))
	for name, value := range m.active {
		active[name] = value
	}
	activity := make(map[string]modelActivity, len(m.activity))
	for key, value := range m.activity {
		activity[key] = value
	}
	m.mu.RUnlock()
	m.inferenceMu.RUnlock()
	if inventoryErr != nil {
		return ExpertModelSnapshot{}, fmt.Errorf("snapshot live Ollama models: %w", inventoryErr)
	}

	resources := buildExpertModelResources(inventory, running, current, active, activity, selectedByKey)
	cleanupSelected := make(map[string]string, len(selectedByKey))
	protected := make(map[string]bool, len(selectedByKey))
	for _, resource := range resources {
		if !resource.Selected || resource.Location == OllamaModelLocationCloud || resource.Location == OllamaModelLocationRemote {
			continue
		}
		key := modelResourceKey(resource.Name)
		cleanupSelected[key] = selectedByKey[key]
		if resource.Current || (resource.Resident || resource.Active) && !resource.ExpertOnly {
			protected[key] = true
		}
	}
	// A selected model that was already resident or non-expert-active belongs to
	// the pre-existing workload, even after this consultation records expert
	// activity. Persist that ownership so a later lease cannot reinterpret it as
	// expert-only and unload it.
	nonExpert := make(map[string]uint64, len(cleanupSelected))
	m.mu.Lock()
	for key := range protected {
		use := m.activity[key]
		if !use.nonExpert {
			m.activitySeq++
			use.nonExpert = true
			use.nonExpertVersion = m.activitySeq
			m.activity[key] = use
		}
	}
	for key := range cleanupSelected {
		nonExpert[key] = m.activity[key].nonExpertVersion
	}
	m.mu.Unlock()
	lease := &expertModelLease{
		manager:   m,
		selected:  cleanupSelected,
		protected: protected,
		nonExpert: nonExpert,
		releasing: make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	releaseGate = false
	return ExpertModelSnapshot{Models: resources, InventoryVerified: true, lease: lease}, nil
}

// ReleaseExpertModels unloads only selected models that were not protected by
// pre-existing non-expert residency, are still non-current, and have not gained
// a non-expert process-local user since the snapshot. It always releases the
// consultation gate, even when one unload fails.
func (m *ModelManager) ReleaseExpertModels(ctx context.Context, snapshot ExpertModelSnapshot) error {
	if m == nil || snapshot.lease == nil || snapshot.lease.manager != m {
		return errors.New("expert model lease is invalid")
	}
	lease := snapshot.lease
	select {
	case lease.releasing <- struct{}{}:
		lease.err = m.releaseExpertModels(ctx, lease)
		close(lease.done)
		return lease.err
	default:
		if ctx == nil {
			return errors.New("expert model cleanup context is required")
		}
		select {
		case <-lease.done:
			return lease.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *ModelManager) releaseExpertModels(ctx context.Context, lease *expertModelLease) error {
	defer func() { <-m.expertGate }()
	defer m.admission.finishReservation()
	if ctx == nil {
		return errors.New("expert model cleanup context is required")
	}
	if err := m.admission.waitForExpertDrain(ctx); err != nil {
		return err
	}

	type unloadTarget struct {
		key    string
		name   string
		client *OllamaClient
	}
	m.mu.RLock()
	currentKey := modelResourceKey(m.currentModel)
	targets := make([]unloadTarget, 0, len(lease.selected))
	for key, selectedName := range lease.selected {
		if lease.protected[key] || key == currentKey {
			continue
		}
		use := m.activity[key]
		if !use.expert || use.nonExpertVersion > lease.nonExpert[key] {
			continue
		}
		var client *OllamaClient
		active := false
		for name, candidate := range m.clients {
			if modelResourceKey(name) != key {
				continue
			}
			if m.active[name] {
				active = true
			}
			if client == nil {
				client = candidate
				selectedName = name
			}
		}
		if active && client != nil {
			targets = append(targets, unloadTarget{key: key, name: selectedName, client: client})
		}
	}
	m.mu.RUnlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].key < targets[j].key })

	var cleanupErr error
	for _, target := range targets {
		if err := target.client.Unload(ctx); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("unload expert model %q: %w", target.name, err))
			continue
		}
		m.mu.Lock()
		for name := range m.active {
			if modelResourceKey(name) == target.key {
				m.active[name] = false
			}
		}
		delete(m.activity, target.key)
		m.mu.Unlock()
	}
	return cleanupErr
}

func buildExpertModelResources(
	inventory []OllamaModel,
	running []OllamaRunningModel,
	current string,
	active map[string]bool,
	activity map[string]modelActivity,
	selected map[string]string,
) []ExpertModelResource {
	inventoryByKey := make(map[string]OllamaModel, len(inventory))
	for _, model := range inventory {
		if key := modelResourceKey(model.Name); key != "" {
			inventoryByKey[key] = model
		}
	}
	resources := make(map[string]ExpertModelResource, len(running)+len(active)+len(selected)+1)
	upsert := func(name string) ExpertModelResource {
		key := modelResourceKey(name)
		resource := resources[key]
		if resource.Name == "" {
			resource.Name = strings.TrimSpace(name)
		}
		if inventoryModel, ok := inventoryByKey[key]; ok {
			resource.Name = inventoryModel.Name
			resource.WeightBytes = maxInt64(resource.WeightBytes, inventoryModel.SizeBytes)
			resource.Location = inventoryModel.Location
		}
		use := activity[key]
		resource.ExpertOnly = use.expert && !use.nonExpert
		return resource
	}
	store := func(resource ExpertModelResource) {
		if key := modelResourceKey(resource.Name); key != "" {
			resources[key] = resource
		}
	}

	for _, runningModel := range running {
		resource := upsert(runningModel.Model.Name)
		resource.Resident = true
		resource.ResidentBytes = maxInt64(resource.ResidentBytes, runningModel.Model.SizeBytes)
		resource.ResidentBytes = maxInt64(resource.ResidentBytes, runningModel.SizeVRAM)
		resource.ContextLength = max(resource.ContextLength, boundedContextLength(runningModel.ContextLength))
		if resource.Location == "" {
			resource.Location = runningModel.Model.Location
		}
		store(resource)
	}
	for name, isActive := range active {
		if !isActive {
			continue
		}
		resource := upsert(name)
		resource.Active = true
		store(resource)
	}
	if strings.TrimSpace(current) != "" {
		resource := upsert(current)
		resource.Current = true
		store(resource)
	}
	for key, name := range selected {
		resource := upsert(name)
		resource.Selected = true
		if resource.Name == "" {
			resource.Name = name
		}
		resources[key] = resource
	}

	result := make([]ExpertModelResource, 0, len(resources))
	for _, resource := range resources {
		result = append(result, resource)
	}
	sort.Slice(result, func(i, j int) bool {
		return modelResourceKey(result[i].Name) < modelResourceKey(result[j].Name)
	})
	return result
}

func modelResourceKey(name string) string {
	return strings.ToLower(config.CanonicalModelName(strings.TrimSpace(name)))
}

func boundedContextLength(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > int64(math.MaxInt) {
		return math.MaxInt
	}
	return int(value)
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
