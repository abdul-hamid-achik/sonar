package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

type ModelManager struct {
	baseURL string
	// numCtx is atomic rather than mu-guarded. It is written by SetNumCtx and
	// read from paths that hold mu, localMu, or no lock at all; getClient
	// already holds mu when it calls ContextPolicy, which takes localMu, so the
	// ordering is mu -> localMu and guarding numCtx with mu would invert it in
	// contextPolicyLocked. An atomic has no ordering to get wrong.
	numCtx       atomic.Int64
	clients      map[string]*OllamaClient
	active       map[string]bool
	activity     map[string]modelActivity
	activitySeq  uint64
	currentModel string
	mu           sync.RWMutex
	switchMu     sync.Mutex
	inferenceMu  sync.RWMutex
	expertGate   chan struct{}
	admission    *modelAdmission
	localMu      sync.RWMutex
	localOnly    bool
	localKnown   bool
	localModels  map[string]int64
	localChecked time.Time
	cloudKnown   bool
	cloudModels  map[string]struct{}
	cloudGrants  map[string]struct{}
	nativeCtx    map[string]int
	// remote, when non-nil, routes ChatStream/Ping/Model through an
	// OpenAI-compatible provider instead of Ollama. Embeddings still use
	// Ollama when ICE is enabled.
	remote        *OpenAICompatibleClient
	remoteContext int
	remoteLabel   string
	// Provider catalog for multi-profile switching (/provider).
	providerCatalog  config.ProviderConfig
	providerActive   string
	ollamaFallback   string // model restored when switching back to ollama
	privacyLocalOnly bool
}

const localInventoryTTL = 30 * time.Second

// LocalModel records an Ollama model identity and the byte size of its local
// weights. Remote/cloud entries never appear in this inventory.
type LocalModel struct {
	Name string
	Size int64
}

// ModelContextPolicy is the context contract for one exact Ollama model.
// Native is the verified model maximum reported by Ollama. Request is the
// num_ctx value sent on the wire; zero deliberately omits num_ctx. Effective is
// the window host-side budgeting may rely on. Ollama Cloud models use their
// verified native maximum and omit num_ctx so the service keeps its documented
// maximum-by-default behavior.
type ModelContextPolicy struct {
	Native      int
	Request     int
	Effective   int
	Cloud       bool
	NativeKnown bool
}

var _ Client = (*ModelManager)(nil)

func NewModelManager(baseURL string, numCtx int) *ModelManager {
	manager := &ModelManager{
		baseURL:     baseURL,
		clients:     make(map[string]*OllamaClient),
		active:      make(map[string]bool),
		activity:    make(map[string]modelActivity),
		expertGate:  make(chan struct{}, 1),
		admission:   newModelAdmission(),
		cloudModels: make(map[string]struct{}),
		cloudGrants: make(map[string]struct{}),
		nativeCtx:   make(map[string]int),
	}
	manager.numCtx.Store(int64(numCtx))
	return manager
}

// ConfigureProviderCatalog installs the multi-profile provider definitions used
// by /provider. localOnly is the host privacy gate for remote base URLs.
func (m *ModelManager) ConfigureProviderCatalog(catalog config.ProviderConfig, localOnly bool, ollamaModel string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providerCatalog = catalog
	m.privacyLocalOnly = localOnly
	if strings.TrimSpace(ollamaModel) != "" {
		m.ollamaFallback = ollamaModel
	}
	if m.providerActive == "" {
		m.providerActive = catalog.ActiveName()
	}
}

// ConfigureRemoteProvider attaches an OpenAI-compatible chat backend. When set,
// ordinary chat, ping, and model selection use the remote client. Local Ollama
// inventory, admission guards, and embeddings remain available for ICE.
func (m *ModelManager) ConfigureRemoteProvider(client *OpenAICompatibleClient, contextSize int, label string) error {
	if client == nil {
		return errors.New("remote provider client is nil")
	}
	if contextSize <= 0 {
		contextSize = 128000
	}
	if strings.TrimSpace(label) == "" {
		label = "remote"
	}
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attachRemoteLocked(client, contextSize, label)
}

func (m *ModelManager) attachRemoteLocked(client *OpenAICompatibleClient, contextSize int, label string) error {
	m.remote = client
	m.remoteContext = contextSize
	m.remoteLabel = label
	m.providerActive = label
	m.currentModel = client.Model()
	m.localMu.Lock()
	m.cloudKnown = true
	if m.cloudModels == nil {
		m.cloudModels = make(map[string]struct{})
	}
	m.cloudModels[config.CanonicalModelName(client.Model())] = struct{}{}
	if m.nativeCtx == nil {
		m.nativeCtx = make(map[string]int)
	}
	m.nativeCtx[config.CanonicalModelName(client.Model())] = contextSize
	if m.cloudGrants == nil {
		m.cloudGrants = make(map[string]struct{})
	}
	m.cloudGrants[config.CanonicalModelName(client.Model())] = struct{}{}
	m.localMu.Unlock()
	return nil
}

// ClearRemoteProvider returns chat inference to the local Ollama path.
func (m *ModelManager) ClearRemoteProvider() {
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remote = nil
	m.remoteContext = 0
	m.remoteLabel = ""
	if m.providerActive == "" || m.providerActive != "ollama" {
		m.providerActive = "ollama"
	}
}

// SwitchProvider activates a named profile from the installed catalog.
// Remote profiles resolve API keys from the process environment at switch time.
func (m *ModelManager) SwitchProvider(name string) error {
	return m.SwitchProviderContext(context.Background(), name)
}

// SwitchProviderContext activates a named profile while allowing callers to
// cancel admission and bounded local-inventory refresh work. Once the provider
// mutation begins it is committed atomically under the manager locks; a
// cancellation that arrives during the best-effort inventory refresh only
// shortens that refresh and does not report a half-applied switch.
func (m *ModelManager) SwitchProviderContext(ctx context.Context, name string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("provider name is empty")
	}
	if err := m.admission.acquireOrdinary(ctx); err != nil {
		return err
	}
	defer m.admission.releaseOrdinary()

	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	catalog := m.providerCatalog
	fallback := m.ollamaFallback
	current := m.currentModel
	m.mu.Unlock()

	profileName, profile, err := resolveSwitchTarget(catalog, name)
	if err != nil {
		return err
	}
	if err := config.ValidateProviderProfile(profileName, profile); err != nil {
		return err
	}

	if !profile.IsRemote() {
		restore := fallback
		if restore == "" {
			restore = current
		}
		if strings.TrimSpace(profile.Model) != "" {
			// Optional model pin on an ollama profile
			restore = profile.Model
		}
		if restore == "" {
			return errors.New("no Ollama model available to restore; set ollama.model")
		}
		m.mu.Lock()
		m.remote = nil
		m.remoteContext = 0
		m.remoteLabel = ""
		m.providerActive = profileName
		m.mu.Unlock()
		inventoryCtx, cancelInventory := context.WithTimeout(ctx, 2*time.Second)
		_ = m.ensureModelLocalFresh(inventoryCtx, restore)
		cancelInventory()
		m.mu.Lock()
		m.currentModel = restore
		m.mu.Unlock()
		return nil
	}

	apiKey, err := profile.ResolveAPIKey()
	if err != nil {
		return err
	}
	client, err := NewProviderClient(profile.Type, profile.BaseURL, profile.Model, apiKey)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.remote == nil && current != "" {
		m.ollamaFallback = current
	}
	err = m.attachRemoteLocked(client, profile.ContextSize, profileName)
	m.mu.Unlock()
	return err
}

func resolveSwitchTarget(catalog config.ProviderConfig, name string) (string, config.ProviderProfile, error) {
	if catalog.HasProfiles() || catalog.Type != "" || catalog.Active != "" {
		if profileName, profile, err := catalog.ResolveProfile(name); err == nil {
			return profileName, profile, nil
		} else if catalog.HasProfiles() {
			return "", config.ProviderProfile{}, err
		}
	}
	// Flat / ad-hoc: treat name as a provider type.
	profile := config.ProviderProfile{Type: name}.Resolve()
	if config.IsKnownProviderType(profile.Type) {
		return name, profile, nil
	}
	return "", config.ProviderProfile{}, fmt.Errorf("unknown provider %q", name)
}

// RemoteProvider reports whether chat inference uses a non-Ollama adapter.
func (m *ModelManager) RemoteProvider() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.remote != nil
}

// RemoteProviderLabel is a short UI/startup label (for example "xai").
func (m *ModelManager) RemoteProviderLabel() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.remoteLabel != "" {
		return m.remoteLabel
	}
	return m.providerActive
}

// ActiveProviderName is the catalog profile currently selected.
func (m *ModelManager) ActiveProviderName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.providerActive != "" {
		return m.providerActive
	}
	if m.remote != nil {
		return m.remoteLabel
	}
	return "ollama"
}

// ProviderNames lists configured profile names for /provider list.
func (m *ModelManager) ProviderNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := m.providerCatalog.ProfileNames()
	if len(names) == 0 {
		return []string{"ollama"}
	}
	return names
}

// ProviderDescriptor is a UI-safe view of one catalog profile. It never
// includes secret values — only whether the configured env var is currently set.
type ProviderDescriptor struct {
	Name       string
	Type       string
	Model      string
	APIKeyEnv  string
	Remote     bool
	Active     bool
	KeyPresent bool // process env has a non-empty value for APIKeyEnv
}

// ProviderCatalog returns descriptors for every installed profile.
func (m *ModelManager) ProviderCatalog() []ProviderDescriptor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := m.providerCatalog.ProfileNames()
	if len(names) == 0 {
		names = []string{"ollama"}
	}
	active := m.providerActive
	if active == "" {
		if m.remote != nil {
			active = m.remoteLabel
		} else {
			active = "ollama"
		}
	}
	out := make([]ProviderDescriptor, 0, len(names))
	for _, name := range names {
		profileName, profile, err := resolveSwitchTarget(m.providerCatalog, name)
		if err != nil {
			continue
		}
		keyPresent := false
		if profile.IsRemote() && strings.TrimSpace(profile.APIKeyEnv) != "" {
			keyPresent = strings.TrimSpace(os.Getenv(profile.APIKeyEnv)) != ""
		}
		out = append(out, ProviderDescriptor{
			Name:       profileName,
			Type:       config.NormalizedProviderType(profile.Type),
			Model:      profile.Model,
			APIKeyEnv:  profile.APIKeyEnv,
			Remote:     profile.IsRemote(),
			Active:     profileName == active,
			KeyPresent: keyPresent,
		})
	}
	return out
}

// ConfigureOllamaInventory installs the context and execution-location facts
// from one verified Ollama inventory snapshot. Model names are canonicalized so
// implicit :latest aliases share a single policy. An unverified snapshot clears
// prior facts instead of allowing stale cloud or native-context metadata to
// authorize later requests.
func (m *ModelManager) ConfigureOllamaInventory(models []OllamaModel, verified bool) {
	cloudModels, nativeCtx, _ := ollamaRuntimeFacts(models, verified)
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()
	m.localMu.Lock()
	m.cloudKnown = verified
	m.cloudModels = cloudModels
	m.nativeCtx = nativeCtx
	m.pruneCloudGrantsLocked()
	m.localMu.Unlock()
}

// ConfigureOllamaCloudInventory records exact cloud identities reported by the
// configured Ollama daemon. It never grants access by itself.
func (m *ModelManager) ConfigureOllamaCloudInventory(models []string, verified bool) {
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()
	m.localMu.Lock()
	defer m.localMu.Unlock()
	m.cloudKnown = verified
	m.cloudModels = make(map[string]struct{}, len(models))
	for _, model := range models {
		if canonical := config.CanonicalModelName(model); canonical != "" {
			m.cloudModels[canonical] = struct{}{}
		}
	}
	m.pruneCloudGrantsLocked()
}

// ConfigureOllamaRuntimeInventory atomically installs all privacy-admission and
// context facts from one Ollama snapshot. Keeping local weights, cloud identity,
// grants, and native limits in one inference-serialized commit prevents a
// refresh from reclassifying a model between admission and request creation.
func (m *ModelManager) ConfigureOllamaRuntimeInventory(required bool, models []OllamaModel, verified bool) {
	cloudModels, nativeCtx, localModels := ollamaRuntimeFacts(models, verified)
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()
	m.localMu.Lock()
	defer m.localMu.Unlock()

	m.localOnly = required
	m.localKnown = verified
	if verified {
		m.localChecked = time.Now()
	} else {
		m.localChecked = time.Time{}
	}
	m.localModels = localModels
	m.cloudKnown = verified
	m.cloudModels = cloudModels
	m.nativeCtx = nativeCtx
	m.pruneCloudGrantsLocked()
}

func ollamaRuntimeFacts(models []OllamaModel, verified bool) (map[string]struct{}, map[string]int, map[string]int64) {
	cloudModels := make(map[string]struct{}, len(models))
	nativeCtx := make(map[string]int, len(models))
	localModels := make(map[string]int64, len(models))
	if !verified {
		return cloudModels, nativeCtx, localModels
	}
	for _, model := range models {
		canonical := config.CanonicalModelName(model.Name)
		if canonical == "" {
			continue
		}
		switch model.Location {
		case OllamaModelLocationCloud:
			cloudModels[canonical] = struct{}{}
		case OllamaModelLocationLocal:
			localModels[canonical] = model.SizeBytes
		}
		contextLength := boundedNativeContext(model.ContextLength)
		if contextLength == 0 {
			continue
		}
		// Duplicate implicit/explicit :latest entries should agree. If they do
		// not, retaining the smaller verified maximum avoids overstating the
		// provider budget.
		if previous, exists := nativeCtx[canonical]; !exists || contextLength < previous {
			nativeCtx[canonical] = contextLength
		}
	}
	return cloudModels, nativeCtx, localModels
}

func (m *ModelManager) pruneCloudGrantsLocked() {
	for granted := range m.cloudGrants {
		if _, exists := m.cloudModels[granted]; !exists {
			delete(m.cloudGrants, granted)
		}
	}
}

func boundedNativeContext(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > int64(math.MaxInt) {
		return math.MaxInt
	}
	return int(value)
}

// ContextPolicy returns the current model-specific request and host-budget
// policy. Cloud status is derived only from a verified Ollama inventory; model
// name suffixes never grant cloud semantics.
func (m *ModelManager) ContextPolicy(model string) ModelContextPolicy {
	m.localMu.RLock()
	defer m.localMu.RUnlock()
	return m.contextPolicyLocked(model)
}

func (m *ModelManager) contextPolicyLocked(model string) ModelContextPolicy {
	canonical := config.CanonicalModelName(model)
	native, nativeKnown := m.nativeCtx[canonical]
	_, cloudModel := m.cloudModels[canonical]
	cloud := canonical != "" && m.cloudKnown && cloudModel
	policy := ModelContextPolicy{Native: native, Cloud: cloud, NativeKnown: nativeKnown}
	if cloud {
		// Ollama Cloud uses its maximum context by default. Sending the local
		// KV-cache allocation here would incorrectly reduce that service window.
		if nativeKnown {
			policy.Effective = native
		}
		return policy
	}
	if canonical == "" || m.configuredNumCtx() <= 0 {
		return policy
	}
	policy.Request = m.configuredNumCtx()
	if nativeKnown && native < policy.Request {
		policy.Request = native
	}
	policy.Effective = policy.Request
	return policy
}

// EffectiveContext returns the current host-side context budget and whether it
// is known. Local requests always have an explicit effective allocation. Cloud
// requests are known only when their native maximum came from verified Ollama
// metadata.
func (m *ModelManager) EffectiveContext(model string) (int, bool) {
	policy := m.ContextPolicy(model)
	return policy.Effective, policy.Effective > 0
}

// GrantOllamaCloudModel grants one verified cloud model for the current
// conversation. The grant is exact, ephemeral, and never covers
// remote-host models.
func (m *ModelManager) GrantOllamaCloudModel(model string) error {
	canonical := config.CanonicalModelName(model)
	m.localMu.Lock()
	defer m.localMu.Unlock()
	if canonical == "" || !m.cloudKnown {
		return fmt.Errorf("cloud inventory is unavailable")
	}
	if _, exists := m.cloudModels[canonical]; !exists {
		return fmt.Errorf("model %q is not a verified Ollama Cloud model", model)
	}
	m.cloudGrants[canonical] = struct{}{}
	return nil
}

func (m *ModelManager) RevokeOllamaCloudModel(model string) {
	m.localMu.Lock()
	delete(m.cloudGrants, config.CanonicalModelName(model))
	m.localMu.Unlock()
}

func (m *ModelManager) RevokeOllamaCloudGrants() {
	m.localMu.Lock()
	m.cloudGrants = make(map[string]struct{})
	m.localMu.Unlock()
}

func (m *ModelManager) GetClient(modelName string) (*OllamaClient, error) {
	m.inferenceMu.RLock()
	defer m.inferenceMu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return m.getClient(ctx, modelName)
}

func (m *ModelManager) getClient(ctx context.Context, modelName string) (*OllamaClient, error) {
	if err := m.ensureModelLocal(ctx, modelName); err != nil {
		return nil, err
	}
	policy := m.ContextPolicy(modelName)
	if err := validateRequestContextPolicy(modelName, policy); err != nil {
		return nil, err
	}
	m.mu.RLock()
	client, exists := m.clients[modelName]
	m.mu.RUnlock()

	if exists && client.numCtx == policy.Request {
		return client, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-read policy after taking the client lock so a context refresh that
	// raced the first lookup cannot install a client with the old request value.
	policy = m.ContextPolicy(modelName)
	if err := validateRequestContextPolicy(modelName, policy); err != nil {
		return nil, err
	}
	if client, exists := m.clients[modelName]; exists && client.numCtx == policy.Request {
		return client, nil
	}

	client, err := NewOllamaClient(m.baseURL, modelName, policy.Request)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", modelName, err)
	}

	m.clients[modelName] = client
	return client, nil
}

func validateRequestContextPolicy(model string, policy ModelContextPolicy) error {
	if policy.Cloud && !policy.NativeKnown {
		return fmt.Errorf("cloud model %q has no verified context maximum; refresh Ollama metadata before using it", model)
	}
	return nil
}

func (m *ModelManager) SetCurrentModel(model string) error {
	if err := m.admission.acquireOrdinary(context.Background()); err != nil {
		return err
	}
	defer m.admission.releaseOrdinary()
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()

	if m.remote != nil {
		if err := m.remote.SetModel(model); err != nil {
			return err
		}
		m.mu.Lock()
		previous := m.currentModel
		m.currentModel = model
		if previous != "" && previous != model {
			m.active[previous] = false
			delete(m.activity, modelResourceKey(previous))
		}
		m.mu.Unlock()
		m.localMu.Lock()
		if m.cloudModels == nil {
			m.cloudModels = make(map[string]struct{})
		}
		m.cloudModels[config.CanonicalModelName(model)] = struct{}{}
		if m.nativeCtx == nil {
			m.nativeCtx = make(map[string]int)
		}
		if m.remoteContext > 0 {
			m.nativeCtx[config.CanonicalModelName(model)] = m.remoteContext
		}
		if m.cloudGrants == nil {
			m.cloudGrants = make(map[string]struct{})
		}
		m.cloudGrants[config.CanonicalModelName(model)] = struct{}{}
		m.localMu.Unlock()
		return nil
	}

	// Admission and policy selection must share the same inference snapshot.
	// Otherwise a verified refresh could reclassify local weights as cloud after
	// the local check but before the client is created.
	inventoryCtx, cancelInventory := context.WithTimeout(context.Background(), 2*time.Second)
	err := m.ensureModelLocalFresh(inventoryCtx, model)
	cancelInventory()
	if err != nil {
		return err
	}

	policy := m.ContextPolicy(model)
	if policy.Cloud && !policy.NativeKnown {
		return fmt.Errorf("cloud model %q has no verified context maximum; refresh Ollama metadata before selecting it", model)
	}
	client, err := NewOllamaClient(m.baseURL, model, policy.Request)
	if err != nil {
		return fmt.Errorf("create client for %s: %w", model, err)
	}

	m.mu.RLock()
	previousName := m.currentModel
	previousClient := m.clients[previousName]
	previousActive := m.active[previousName]
	m.mu.RUnlock()
	if previousName != "" && previousName != model && previousClient != nil && previousActive {
		unloadCtx, cancelUnload := context.WithTimeout(context.Background(), 5*time.Second)
		err := previousClient.Unload(unloadCtx)
		cancelUnload()
		if err != nil {
			return fmt.Errorf("unload previous model %q before switching to %q: %w", previousName, model, err)
		}
	}

	m.mu.Lock()
	m.clients[model] = client
	m.currentModel = model
	m.markNonExpertActivityLocked(model)
	if previousName != "" && previousName != model {
		m.active[previousName] = false
		delete(m.activity, modelResourceKey(previousName))
	}
	m.mu.Unlock()
	return nil
}

// ClearCurrentModel removes inference authority from the current selection.
// It is deliberately fail-closed: the selection is cleared before a best-
// effort unload, so an Ollama inventory reclassification cannot leave a
// remote model usable merely because releasing its resident runner failed.
func (m *ModelManager) ClearCurrentModel() error {
	if err := m.admission.acquireOrdinary(context.Background()); err != nil {
		return err
	}
	defer m.admission.releaseOrdinary()
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()

	m.mu.Lock()
	previousName := m.currentModel
	previousClient := m.clients[previousName]
	previousActive := m.active[previousName]
	m.currentModel = ""
	if !previousActive {
		delete(m.activity, modelResourceKey(previousName))
	}
	m.mu.Unlock()

	if previousName == "" || previousClient == nil || !previousActive {
		return nil
	}
	unloadCtx, cancelUnload := context.WithTimeout(context.Background(), 5*time.Second)
	err := previousClient.Unload(unloadCtx)
	cancelUnload()
	if err != nil {
		return fmt.Errorf("unload cleared model %q: %w", previousName, err)
	}
	m.mu.Lock()
	m.active[previousName] = false
	delete(m.activity, modelResourceKey(previousName))
	m.mu.Unlock()
	return nil
}

func (m *ModelManager) CurrentModel() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentModel
}

func (m *ModelManager) ChatStream(ctx context.Context, opts ChatOptions, fn func(StreamChunk) error) error {
	if err := m.admission.acquireOrdinary(ctx); err != nil {
		return inferenceNotStarted(err)
	}
	defer m.admission.releaseOrdinary()
	m.inferenceMu.RLock()
	defer m.inferenceMu.RUnlock()
	m.mu.RLock()
	model := m.currentModel
	remote := m.remote
	m.mu.RUnlock()

	if model == "" {
		return inferenceNotStarted(ErrNoModelSelected)
	}
	if err := validateExpectedModel(model, opts.ExpectedModel); err != nil {
		return inferenceNotStarted(err)
	}
	if err := m.validateExpectedContext(model, opts.ExpectedContext); err != nil {
		return inferenceNotStarted(err)
	}

	if remote != nil {
		m.mu.Lock()
		m.active[model] = true
		m.markNonExpertActivityLocked(model)
		m.mu.Unlock()
		return remote.ChatStream(ctx, opts, fn)
	}

	client, err := m.getClient(ctx, model)
	if err != nil {
		return inferenceNotStarted(err)
	}
	m.mu.Lock()
	m.active[model] = true
	m.markNonExpertActivityLocked(model)
	m.mu.Unlock()
	return client.ChatStream(ctx, opts, fn)
}

func validateExpectedModel(model, expected string) error {
	if strings.TrimSpace(expected) == "" {
		return nil
	}
	if config.CanonicalModelName(model) != config.CanonicalModelName(expected) {
		return fmt.Errorf(
			"model changed before inference: turn expected %q, current selection is %q; retry the turn",
			expected, model,
		)
	}
	return nil
}

func (m *ModelManager) validateExpectedContext(model string, expected int) error {
	if expected <= 0 {
		return nil
	}
	policy := m.ContextPolicy(model)
	if err := validateRequestContextPolicy(model, policy); err != nil {
		return err
	}
	if policy.Effective != expected {
		return fmt.Errorf(
			"model context changed before inference for %q: turn expected %d tokens, current policy provides %d; retry the turn",
			model, expected, policy.Effective,
		)
	}
	return nil
}

func (m *ModelManager) ChatStreamForModel(ctx context.Context, model string, opts ChatOptions, fn func(StreamChunk) error) error {
	if err := m.admission.acquireExpert(ctx, model); err != nil {
		return inferenceNotStarted(err)
	}
	defer m.admission.releaseExpert()
	m.inferenceMu.RLock()
	defer m.inferenceMu.RUnlock()
	if err := validateExpectedModel(model, opts.ExpectedModel); err != nil {
		return inferenceNotStarted(err)
	}
	if err := m.validateExpectedContext(model, opts.ExpectedContext); err != nil {
		return inferenceNotStarted(err)
	}
	m.mu.RLock()
	remote := m.remote
	m.mu.RUnlock()
	if remote != nil {
		// Experts on a single remote profile share the configured client model.
		if config.CanonicalModelName(model) != config.CanonicalModelName(remote.Model()) {
			return inferenceNotStarted(fmt.Errorf("remote provider only serves model %q", remote.Model()))
		}
		return remote.ChatStream(ctx, opts, fn)
	}
	client, err := m.getClient(ctx, model)
	if err != nil {
		return inferenceNotStarted(err)
	}
	m.mu.Lock()
	m.active[model] = true
	m.markExpertActivityLocked(model)
	m.mu.Unlock()
	return client.ChatStream(ctx, opts, fn)
}

func (m *ModelManager) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return m.PingContext(ctx)
}

// PingContext checks the currently selected provider/model while preserving
// the caller's cancellation and deadline across the actual network request.
func (m *ModelManager) PingContext(ctx context.Context) error {
	m.inferenceMu.RLock()
	defer m.inferenceMu.RUnlock()
	m.mu.RLock()
	model := m.currentModel
	remote := m.remote
	m.mu.RUnlock()

	if model == "" {
		return ErrNoModelSelected
	}
	if remote != nil {
		return remote.PingContext(ctx)
	}

	client, err := m.getClient(ctx, model)
	if err != nil {
		return err
	}
	return client.PingContext(ctx)
}

func (m *ModelManager) PingModel(ctx context.Context, model string) error {
	m.inferenceMu.RLock()
	defer m.inferenceMu.RUnlock()
	m.mu.RLock()
	remote := m.remote
	m.mu.RUnlock()
	if remote != nil {
		if config.CanonicalModelName(model) != config.CanonicalModelName(remote.Model()) {
			return fmt.Errorf("remote provider only serves model %q", remote.Model())
		}
		return remote.PingContext(ctx)
	}
	client, err := m.getClient(ctx, model)
	if err != nil {
		return err
	}
	return client.PingContext(ctx)
}

// ListLocalModels returns only models with local weights. Ollama cloud entries
// are deliberately excluded so a "local-only" routing decision cannot
// silently cross the network.
func (m *ModelManager) ListLocalModels(ctx context.Context) ([]string, error) {
	inventory, err := m.ListLocalModelInventory(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]string, len(inventory))
	for i, model := range inventory {
		models[i] = model.Name
	}
	return models, nil
}

// ListOllamaModels returns the configured Ollama host's unfiltered inventory.
// Privacy and routing policy must be applied by the caller using Location and
// Capabilities; unlike ListLocalModels, cloud and remote-host entries remain.
func (m *ModelManager) ListOllamaModels(ctx context.Context) ([]OllamaModel, error) {
	client, err := NewOllamaClient(m.baseURL, "", m.configuredNumCtx())
	if err != nil {
		return nil, err
	}
	return client.ListModels(ctx)
}

func (m *ModelManager) ShowOllamaModel(ctx context.Context, model string) (OllamaModelInfo, error) {
	client, err := NewOllamaClient(m.baseURL, "", m.configuredNumCtx())
	if err != nil {
		return OllamaModelInfo{}, err
	}
	return client.ShowModel(ctx, model)
}

func (m *ModelManager) ListRunningOllamaModels(ctx context.Context) ([]OllamaRunningModel, error) {
	client, err := NewOllamaClient(m.baseURL, "", m.configuredNumCtx())
	if err != nil {
		return nil, err
	}
	return client.ListRunningModels(ctx)
}

func (m *ModelManager) OllamaVersion(ctx context.Context) (string, error) {
	client, err := NewOllamaClient(m.baseURL, "", m.configuredNumCtx())
	if err != nil {
		return "", err
	}
	return client.Version(ctx)
}

func (m *ModelManager) PullOllamaModel(ctx context.Context, model string, fn func(OllamaPullProgress) error) error {
	client, err := NewOllamaClient(m.baseURL, "", m.configuredNumCtx())
	if err != nil {
		return err
	}
	return client.PullModel(ctx, model, fn)
}

// ListLocalModelInventory returns local identities with their actual weight
// sizes so memory admission never has to infer safety from a tag string.
func (m *ModelManager) ListLocalModelInventory(ctx context.Context) ([]LocalModel, error) {
	client, err := NewOllamaClient(m.baseURL, "", m.configuredNumCtx())
	if err != nil {
		return nil, err
	}
	// A model installed on a LAN or Internet Ollama daemon is not on-device.
	// Reject the host before issuing /api/tags so local-only admission cannot
	// reinterpret a remote daemon's positive size field as local weights.
	if err := validateLocalOllamaHost(client); err != nil {
		return nil, err
	}
	response, err := client.listModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Ollama models: %w", err)
	}

	models := make([]LocalModel, 0, len(response))
	seen := make(map[string]struct{}, len(response))
	for _, model := range response {
		if model.RemoteModel != "" || model.RemoteHost != "" || model.Size <= 0 {
			continue
		}
		name := model.Model
		if name == "" {
			name = model.Name
		}
		if name == "" {
			continue
		}
		canonical := config.CanonicalModelName(name)
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		models = append(models, LocalModel{Name: name, Size: model.Size})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}

func validateLocalOllamaHost(client *OllamaClient) error {
	if client == nil || client.base == nil {
		return fmt.Errorf("configured Ollama host is unavailable")
	}
	if !isLocalOllamaHost(client.base.Hostname()) {
		return fmt.Errorf(
			"configured Ollama host %q is not a local-machine address; cannot verify on-device model weights",
			client.base.Hostname(),
		)
	}
	return nil
}

// ConfigureLocalOnly requires all inference model names to be proven by an
// Ollama inventory entry with local weights. An unverified inventory keeps the
// UI available for diagnostics, but no client can be selected until a later
// operation successfully refreshes the inventory.
func (m *ModelManager) ConfigureLocalOnly(required bool, models []string, verified bool) {
	inventory := make([]LocalModel, len(models))
	for i, model := range models {
		inventory[i] = LocalModel{Name: model, Size: -1}
	}
	m.ConfigureLocalInventory(required, inventory, verified)
}

// ConfigureLocalInventory installs a verified local-weight inventory. The
// legacy name-only method intentionally records unknown sizes and therefore
// cannot admit inference in local-only mode.
func (m *ModelManager) ConfigureLocalInventory(required bool, models []LocalModel, verified bool) {
	m.localMu.Lock()
	defer m.localMu.Unlock()
	m.localOnly = required
	m.localKnown = verified
	if verified {
		m.localChecked = time.Now()
	} else {
		m.localChecked = time.Time{}
	}
	m.localModels = make(map[string]int64, len(models))
	for _, model := range models {
		if canonical := config.CanonicalModelName(model.Name); canonical != "" {
			m.localModels[canonical] = model.Size
		}
	}
}

func (m *ModelManager) ensureModelLocal(ctx context.Context, model string) error {
	return m.ensureModelLocalWithRefresh(ctx, model, false)
}

func (m *ModelManager) ensureModelLocalFresh(ctx context.Context, model string) error {
	return m.ensureModelLocalWithRefresh(ctx, model, true)
}

func (m *ModelManager) ensureModelLocalWithRefresh(ctx context.Context, model string, forceRefresh bool) error {
	canonical := config.CanonicalModelName(model)
	if canonical == "" {
		return fmt.Errorf("model name is required")
	}
	m.localMu.RLock()
	required := m.localOnly
	known := m.localKnown
	checked := m.localChecked
	size, allowed := m.localModels[canonical]
	_, cloudGranted := m.cloudGrants[canonical]
	_, cloudVerified := m.cloudModels[canonical]
	cloudKnown := m.cloudKnown
	m.localMu.RUnlock()
	if !required {
		return nil
	}
	// Cached inventory cannot turn a LAN/Internet Ollama daemon into an
	// on-device runtime. Enforce the host boundary on every local-only admission,
	// including explicitly granted cloud aliases and callers that installed
	// inventory programmatically. Cloud consent crosses the model-execution
	// boundary; it never grants access to a remote Ollama control plane.
	client, err := NewOllamaClient(m.baseURL, "", m.configuredNumCtx())
	if err != nil {
		return fmt.Errorf("local-only model admission: %w", err)
	}
	if err := validateLocalOllamaHost(client); err != nil {
		return fmt.Errorf("local-only model admission: %w", err)
	}
	if cloudKnown && cloudVerified && cloudGranted {
		return nil
	}
	if !known || !allowed || forceRefresh || time.Since(checked) >= localInventoryTTL {
		models, err := m.ListLocalModelInventory(ctx)
		if err != nil {
			return fmt.Errorf("local-only model inventory is unavailable: %w", err)
		}
		m.ConfigureLocalInventory(true, models, true)
		allowed = false
		for _, candidate := range models {
			if config.CanonicalModelName(candidate.Name) == canonical {
				allowed = true
				size = candidate.Size
				break
			}
		}
	}
	if !allowed {
		return fmt.Errorf("model %q is not installed with local Ollama weights", model)
	}
	if err := config.CheckLocalModelSizeSafe(model, size); err != nil {
		return fmt.Errorf("local-only model admission: %w", err)
	}
	return nil
}

func (m *ModelManager) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if err := m.admission.acquireOrdinary(ctx); err != nil {
		return nil, err
	}
	defer m.admission.releaseOrdinary()
	m.inferenceMu.RLock()
	defer m.inferenceMu.RUnlock()
	m.localMu.RLock()
	_, cloudModel := m.cloudModels[config.CanonicalModelName(model)]
	localOnly := m.localOnly
	m.localMu.RUnlock()
	if localOnly && cloudModel {
		return nil, fmt.Errorf("cloud conversation consent does not cover embeddings")
	}
	client, err := m.getClient(ctx, model)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	// Embedding weights are resident too. Chat switches only evict the
	// previous current chat model, while Close unloads every resident client.
	m.active[model] = true
	m.markNonExpertActivityLocked(model)
	m.mu.Unlock()
	return client.Embed(ctx, model, texts)
}

func (m *ModelManager) EmbedWithCurrentModel(ctx context.Context, texts []string) ([][]float32, error) {
	m.mu.RLock()
	model := m.currentModel
	m.mu.RUnlock()

	if model == "" {
		return nil, ErrNoModelSelected
	}
	return m.Embed(ctx, model, texts)
}

func (m *ModelManager) Close() {
	m.RevokeOllamaCloudGrants()
	if err := m.admission.acquireOrdinary(context.Background()); err != nil {
		return
	}
	defer m.admission.releaseOrdinary()
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()

	m.mu.Lock()
	activeClients := make([]*OllamaClient, 0, len(m.active))
	for name, active := range m.active {
		if active && m.clients[name] != nil {
			activeClients = append(activeClients, m.clients[name])
		}
	}
	m.clients = make(map[string]*OllamaClient)
	m.active = make(map[string]bool)
	m.activity = make(map[string]modelActivity)
	m.currentModel = ""
	m.mu.Unlock()

	for _, client := range activeClients {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.Unload(ctx)
		cancel()
	}
}

func (m *ModelManager) BaseURL() string {
	return m.baseURL
}

func (m *ModelManager) NumCtx() int {
	m.mu.RLock()
	model := m.currentModel
	m.mu.RUnlock()
	if model != "" {
		policy := m.ContextPolicy(model)
		if policy.Cloud && !policy.NativeKnown {
			return 0
		}
		if policy.Effective > 0 {
			return policy.Effective
		}
	}
	return m.configuredNumCtx()
}

// ConfiguredNumCtx returns the host-configured local KV allocation (the value
// sent as options.num_ctx for local models), independent of cloud effective
// windows.
func (m *ModelManager) ConfiguredNumCtx() int { return m.configuredNumCtx() }

// configuredNumCtx is the single read path for the atomic allocation.
func (m *ModelManager) configuredNumCtx() int { return int(m.numCtx.Load()) }

// SetNumCtx updates the local KV-cache allocation used for subsequent local
// Ollama requests. Cloud profiles keep their native maximum. The current local
// client is rebuilt so the next turn picks up the new window; in-flight turns
// keep their frozen snapshot.
func (m *ModelManager) SetNumCtx(numCtx int) error {
	normalized, err := config.NormalizeNumCtx(numCtx)
	if err != nil {
		return err
	}
	if err := m.admission.acquireOrdinary(context.Background()); err != nil {
		return err
	}
	defer m.admission.releaseOrdinary()
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.inferenceMu.Lock()
	defer m.inferenceMu.Unlock()

	m.mu.Lock()
	m.numCtx.Store(int64(normalized))
	model := m.currentModel
	m.mu.Unlock()

	if m.remote != nil || model == "" {
		return nil
	}
	policy := m.ContextPolicy(model)
	if policy.Cloud {
		return nil
	}
	client, err := NewOllamaClient(m.baseURL, model, policy.Request)
	if err != nil {
		return fmt.Errorf("rebuild client for num_ctx %d: %w", normalized, err)
	}
	m.mu.Lock()
	m.clients[model] = client
	m.mu.Unlock()
	return nil
}

// LocalModelWeightBytes returns the cached local weight size for model when
// known from inventory.
func (m *ModelManager) LocalModelWeightBytes(model string) int64 {
	canonical := config.CanonicalModelName(model)
	m.localMu.RLock()
	defer m.localMu.RUnlock()
	if m.localModels == nil {
		return 0
	}
	return m.localModels[canonical]
}

func (m *ModelManager) Model() string {
	return m.CurrentModel()
}
