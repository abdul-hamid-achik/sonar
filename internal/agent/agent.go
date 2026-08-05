package agent

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/log"

	"github.com/abdul-hamid-achik/sonar/internal/capabilityadvisor"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/ice"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// Default tool and iteration limits. These are user-visible configuration
// defaults that appear in documentation and error messages.
const (
	defaultMaxIterations     = 10
	defaultAutoMaxIterations = 40
	defaultToolTimeout       = 30 * time.Second
	defaultMaxGrepResults    = 500
)

// Agent orchestrates the LLM and tools in a ReAct loop.
type Agent struct {
	mu                   sync.RWMutex
	llmClient            llm.Client
	registry             *mcp.Registry
	messages             []llm.Message
	skillContent         string
	skillLoader          SkillLoader
	loadedCtx            string
	numCtx               int
	memoryStore          *memory.Store
	iceEngine            *ice.Engine
	iceSessionScope      string
	iceSessionGeneration uint64
	router               config.ModelRouter
	modePrefix           string
	toolPolicy           ToolPolicy
	authorityMode        AuthorityMode
	workDir              string
	ignoreContent        string
	filesystemVersion    uint64
	activeFilesystem     filesystemContext
	filesystemPinned     bool
	permChecker          *permission.Checker
	approvalCallback     func(permission.ApprovalRequest)
	approvalHostVersion  uint64
	// approvalGrants contains host-approved session grants for this Agent
	// process. Keys bind workspace, tool, scope kind, and (for exact-request)
	// canonical arguments. Wider scopes use documented resources. Grants
	// are never persisted as global tool-only policies.
	approvalGrants map[string]struct{}
	// workspaceRules are durable, workspace-scoped bash prefix and exact MCP
	// tool allows loaded from the user config directory. They never reintroduce
	// broad tool_permissions.allow rows.
	workspaceRules      permission.WorkspaceRules
	workspaceRulesStore *permission.WorkspaceRulesStore
	// approvalPosture is process-local approval UX policy (e.g. accept
	// workspace edits without per-call prompts). It never broadens deny
	// policies or skip-approvals host posture.
	approvalPosture     ApprovalPosture
	toolsConfig         config.ToolsConfig
	continuationsConfig config.ContinuationsConfig
	logger              *log.Logger
	turnRunning         atomic.Bool
	turnMu              sync.Mutex
	turnCancel          context.CancelFunc
	turnDone            chan struct{}
	closed              bool
	readOnlySlots       chan struct{}
	// background owns every shell process started with bash(background=true).
	// It is created on first use and terminated by Close, so no backgrounded
	// process can outlive the session that started it.
	background     *backgroundRegistry
	hooks          []ToolHook
	mcpServerScope map[string]struct{}
	mcpScopeSet    bool
	trustedMCP     map[string]trustedMCPServer
	// mcpRouteVersion changes only when exact MCP route trust or scope changes.
	// It intentionally excludes approval-renderer churn so opaque continuation
	// contexts survive the UI installing a per-turn callback.
	mcpRouteVersion uint64
	readRoots       map[string]*additionalReadRoot
	readFiles       map[string]*additionalReadFile
	// writeAuthorities are temporary, host-inspected capabilities for exact
	// external directories or files explicitly named by the user. They are
	// process-local, identity-pinned, and never serialized into session state.
	writeAuthorities  map[string]*additionalWriteAuthority
	capabilityAdvisor capabilityAdviser
	capabilityRetries map[capabilityRetryKey]struct{}
	// capabilityCatalogEpoch tracks the MCP registry ToolSnapshot epoch so a
	// reconnect or catalog change invalidates host capability cache entries.
	capabilityCatalogEpoch uint64
	// capabilityCatalogRevision is the last opaque MCPHub catalog_revision
	// observed from a successful resolve. Process-local only.
	capabilityCatalogRevision string
	// capabilityStatsHint caches a coarse mcphub_stats projection.
	capabilityStatsHint    string
	capabilityStatsExpires time.Time
	// capabilityMetrics is process-local routing telemetry (no prompt text).
	capabilityMetrics CapabilityRoutingMetrics
	expertConsultant  ExpertConsultant
	imageResolver     ImageResolver
	mcphubResults     *ecosystem.MCPHubResultAssembler
	// continuationContracts is a bounded, ephemeral cache of exact downstream
	// input schemas observed through a trusted MCPHub describe call. It is never
	// serialized and never grants authority; the host trust catalog still owns
	// route and effect classification.
	continuationContracts map[string]continuationContract
	continuationSequence  uint64
	continuationHistory   *continuationTurnState
	// autoContinuationHistory is a separate, bounded session-scope reservation
	// ledger. Suggesting an action never writes it; only an LA-3 schedule claim
	// does. It prevents the same source revision/context from auto-running again
	// across model turns.
	autoContinuationHistory *autoContinuationHistory
	continuationFreshness   *continuationFreshnessState
	// bobWorkspaceContext is a bounded, ephemeral session cache populated only
	// by an exact typed bob_context receipt bound to the active workspace. Raw
	// manifest or StructuredContent bytes never enter this state.
	bobWorkspaceContext    *bobWorkspaceContextCache
	bobWorkspaceGeneration uint64
	// bobStoredAdmissions binds a turn-scoped MCPHub stored call ID to the
	// exact workspace/generation admitted for bob_context. It is cleared when
	// the turn ends and never serialized.
	bobStoredAdmissions map[string]bobContextAdmission
	// contextPromptFloor preserves the last exact provider prompt-token receipt
	// as a lower bound across user turns. It is keyed to the provider model and
	// survives session persistence, preventing the model-agnostic estimator from
	// re-admitting an unchanged over-capacity transcript on the next turn.
	contextPromptFloor ContextPromptFloor

	checkpointStore     CheckpointStore
	checkpointSessionID int64

	executionLedger     ExecutionLedger
	executionSessionID  int64
	executionCursor     int64
	executionRunID      string
	executionRunIDErr   error
	requireExecutionLog bool
	unresolvedExecution *UnresolvedExecutionError
}

// ExpertConsultant is the bounded read-only team runtime surface. Keeping the
// interface on Agent allows deterministic fakes without coupling tests to a
// provider implementation.
type ExpertConsultant interface {
	Consult(context.Context, expertteam.Request) (expertteam.Result, error)
}

// ExpertProgressConsultant is an optional extension. Existing embedders keep
// the stable ExpertConsultant surface; runtimes that implement this interface
// can expose only the bounded host-owned progress projection.
type ExpertProgressConsultant interface {
	ConsultWithProgress(context.Context, expertteam.Request, expertteam.Observer) (expertteam.Result, error)
}

// ExpertRequestValidator is an optional pure preflight extension. Production
// runtimes use it to reject unavailable exact profile names before the one
// consultation dispatch allowed for a parent turn is consumed.
type ExpertRequestValidator interface {
	ValidateRequest(context.Context, expertteam.Request) error
}

// contextWindowProvider is an optional capability implemented by clients that
// can report the context window of their currently selected model.
type contextWindowProvider interface {
	NumCtx() int
}

// ContextPromptFloor is the bounded durable projection of one exact provider
// prompt-token receipt. MessageTokens is the host estimate for the message
// prefix that produced Tokens; later message growth is added conservatively.
// It contains no prompt text, tool payload, or provider transcript.
type ContextPromptFloor struct {
	Tokens        int    `json:"tokens,omitempty"`
	HostTokens    int    `json:"host_tokens,omitempty"`
	MessageTokens int    `json:"message_tokens,omitempty"`
	Model         string `json:"model,omitempty"`
}

// MaxContextPromptFloorTokens is a serialization and arithmetic bound, not a
// configured model limit. It sits far above supported context windows while
// preventing a tampered session from installing a permanent MaxInt denial.
const MaxContextPromptFloorTokens = 16 * 1024 * 1024

// Validate rejects partial or unbounded durable prompt-floor projections.
func (floor ContextPromptFloor) Validate() error {
	if floor.Tokens == 0 && floor.HostTokens == 0 && floor.MessageTokens == 0 && floor.Model == "" {
		return nil
	}
	model := strings.TrimSpace(floor.Model)
	if floor.Tokens <= 0 || floor.Tokens > MaxContextPromptFloorTokens || floor.HostTokens < 0 ||
		floor.HostTokens > MaxContextPromptFloorTokens || floor.MessageTokens < 0 ||
		floor.MessageTokens > MaxContextPromptFloorTokens || model == "" || len(model) > 512 || !utf8.ValidString(model) {
		return fmt.Errorf("invalid context prompt floor")
	}
	return nil
}

// SetLogger sets the structured logger used for observability. Safe to leave
// unset; all logging is nil-guarded.
func (a *Agent) SetLogger(l *log.Logger) {
	a.logger = l
}

// log returns a logger scoped to the given turn correlation ID, or nil.
func (a *Agent) logTurn(turnID string) *log.Logger {
	if a.logger == nil {
		return nil
	}
	return a.logger.With("turn", turnID)
}

// New creates a new Agent.
func New(llmClient llm.Client, registry *mcp.Registry, numCtx int) *Agent {
	runID, runIDErr := execution.NewRunID()
	agent := &Agent{
		llmClient:               llmClient,
		registry:                registry,
		numCtx:                  numCtx,
		toolPolicy:              DefaultToolPolicy(),
		authorityMode:           AuthorityNormal,
		executionRunID:          runID,
		executionRunIDErr:       runIDErr,
		approvalGrants:          make(map[string]struct{}),
		trustedMCP:              make(map[string]trustedMCPServer),
		readRoots:               make(map[string]*additionalReadRoot),
		readFiles:               make(map[string]*additionalReadFile),
		writeAuthorities:        make(map[string]*additionalWriteAuthority),
		capabilityRetries:       make(map[capabilityRetryKey]struct{}),
		mcphubResults:           ecosystem.NewMCPHubResultAssembler(),
		continuationContracts:   make(map[string]continuationContract),
		continuationHistory:     newContinuationTurnState(0),
		autoContinuationHistory: newAutoContinuationHistory(),
		continuationFreshness:   newContinuationFreshnessState(),
		bobWorkspaceGeneration:  1,
		continuationsConfig: config.ContinuationsConfig{
			Mode:         config.ContinuationSuggest,
			MaxAutoSteps: config.MaxAutoContinuationSteps,
		},
		// Filesystem reads can enter OS syscalls that do not observe context
		// cancellation. Allow at most one abandoned worker for the lifetime of
		// an Agent; later reads wait on this slot and remain cancellable.
		readOnlySlots: make(chan struct{}, 1),
	}
	capabilityRegistry := scopedCapabilityRegistry{agent: agent}
	if registry != nil {
		capabilityRegistry.backend = registry
	}
	agent.capabilityAdvisor = capabilityadvisor.New(capabilityRegistry)
	return agent
}

// SetRouter sets the model router for auto-selection.
func (a *Agent) SetRouter(router config.ModelRouter) {
	a.router = router
}

// SetExpertConsultant installs application-level Team/Swarm/MoE support. A nil
// consultant removes the model-visible tool.
func (a *Agent) SetExpertConsultant(consultant ExpertConsultant) {
	a.mu.Lock()
	a.expertConsultant = consultant
	a.mu.Unlock()
}

// SetImageResolver installs the path-free content-addressed lookup used to
// rehydrate images restored from durable session state. A nil resolver removes
// the lookup. The callback is always invoked without an Agent lock held.
func (a *Agent) SetImageResolver(resolver ImageResolver) {
	a.mu.Lock()
	a.imageResolver = resolver
	a.mu.Unlock()
}

// SetModeContext configures the mode prefix injected into the system prompt
// and the tool policy for the current turn.
func (a *Agent) SetModeContext(prefix string, policy ToolPolicy) {
	a.mu.Lock()
	a.modePrefix = prefix
	a.toolPolicy = policy
	a.mu.Unlock()
}

// modeContext returns one coherent mode snapshot. A turn keeps this snapshot
// for its lifetime so a concurrent host-mode change cannot alter its visible
// tools after the provider has already received the system prompt.
func (a *Agent) modeContext() (string, ToolPolicy) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.modePrefix, a.toolPolicy
}

// AppendLoadedContext appends to the loaded context.
func (a *Agent) AppendLoadedContext(content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loadedCtx == "" {
		a.loadedCtx = content
	} else {
		a.loadedCtx += content
	}
}

// Router returns the model router.
func (a *Agent) Router() config.ModelRouter {
	return a.router
}

// NumCtx returns the active provider context window when available, falling
// back to the value configured when the agent was created.
func (a *Agent) NumCtx() int {
	a.mu.RLock()
	client := a.llmClient
	fallback := a.numCtx
	a.mu.RUnlock()

	if provider, ok := client.(contextWindowProvider); ok {
		if numCtx := provider.NumCtx(); numCtx > 0 {
			return numCtx
		}
	}
	return fallback
}

// SetMemoryStore sets the memory store for cross-session persistence.
func (a *Agent) SetMemoryStore(store *memory.Store) {
	a.mu.Lock()
	a.memoryStore = store
	a.mu.Unlock()
}

// MemoryStore returns the active project-scoped memory store.
func (a *Agent) MemoryStore() *memory.Store {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.memoryStore
}

// AddUserMessage appends a user message to the conversation.
func (a *Agent) AddUserMessage(content string) {
	_ = a.AddUserMessageWithImages(content, nil)
}

// AddUserMessageWithImages appends a user message with transient image bytes
// and/or complete durable references. It validates and defensively copies every
// image before publishing the message to concurrent readers.
func (a *Agent) AddUserMessageWithImages(content string, images []llm.ImageData) error {
	cloned, err := cloneValidatedImages(images)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, llm.Message{
		Role:    "user",
		Content: content,
		Images:  cloned,
	})
	return nil
}

// Messages returns a copy of the conversation history. A copy (not the live
// slice) is required because callers read it from other goroutines (e.g. the
// TUI's checkpoint-restore path) while Run may be appending concurrently.
func (a *Agent) Messages() []llm.Message {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneMessagesWithImages(a.messages)
}

// ClearHistory resets the conversation history.
func (a *Agent) ClearHistory() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = nil
	a.contextPromptFloor = ContextPromptFloor{}
	a.continuationHistory = newContinuationTurnState(0)
	a.resetAutoContinuationHistoryLocked()
	a.invalidateBobWorkspaceContextLocked()
}

// AppendMessage appends a message to the conversation history.
func (a *Agent) AppendMessage(msg llm.Message) {
	msg.Images = cloneImages(msg.Images)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, msg)
}

// AppendDurableRecoveryContext installs one already validated, host-authored
// reconciliation receipt. Exact content is idempotent; a persisted copy is
// re-marked HostOwned after restore instead of being duplicated. Callers must
// derive content from durable typed reconciliation rather than user text.
func (a *Agent) AppendDurableRecoveryContext(content string) error {
	if !strings.HasPrefix(content, DurableRecoveryContextPrefix) {
		return fmt.Errorf("durable recovery context has an invalid prefix")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next := make([]llm.Message, 0, len(a.messages)+1)
	found := false
	seenHostContent := make(map[string]struct{})
	for _, message := range a.messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, DurableRecoveryContextPrefix) {
			if message.Content == content {
				message.HostOwned = true
				found = true
			}
			// A prefix never grants authority. Persisted or otherwise injected
			// prefixed messages are removed unless the host marker is already
			// present or this exact content is the newly validated projection.
			if !message.HostOwned {
				continue
			}
			if _, duplicate := seenHostContent[message.Content]; duplicate {
				continue
			}
			seenHostContent[message.Content] = struct{}{}
		}
		next = append(next, message)
	}
	if !found {
		next = append(next, llm.Message{Role: "system", Content: content, HostOwned: true})
	}
	if _, err := collectDurableRecoveryContexts(next); err != nil {
		return err
	}
	if !reflect.DeepEqual(a.messages, next) {
		a.contextPromptFloor = ContextPromptFloor{}
	}
	a.messages = next
	return nil
}

// InstallDurableRecoveryContexts replaces every prefixed system message with
// the complete set derived from the current durable DB projection. This is the
// restore boundary: persisted JSON carries no HostOwned marker, so no saved
// prefix text is allowed to survive unless the DB independently re-authorizes
// its exact content.
func (a *Agent) InstallDurableRecoveryContexts(contents []string) error {
	nextContexts := make([]llm.Message, 0, len(contents))
	seen := make(map[string]struct{}, len(contents))
	for _, content := range contents {
		if !strings.HasPrefix(content, DurableRecoveryContextPrefix) {
			return fmt.Errorf("durable recovery context has an invalid prefix")
		}
		if _, duplicate := seen[content]; duplicate {
			continue
		}
		seen[content] = struct{}{}
		nextContexts = append(nextContexts, llm.Message{Role: "system", Content: content, HostOwned: true})
	}
	if _, err := collectDurableRecoveryContexts(nextContexts); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	next := make([]llm.Message, 0, len(a.messages)+len(nextContexts))
	for _, message := range a.messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, DurableRecoveryContextPrefix) {
			continue
		}
		next = append(next, message)
	}
	next = append(next, nextContexts...)
	if !reflect.DeepEqual(a.messages, next) {
		a.contextPromptFloor = ContextPromptFloor{}
	}
	a.messages = next
	return nil
}

// ReplaceMessages replaces the entire conversation history.
func (a *Agent) ReplaceMessages(msgs []llm.Message) {
	a.replaceMessages(msgs, true)
}

// ReplaceMessagesWithinSession atomically rewrites the transcript for
// compaction or transactional rollback without erasing LA-2/LA-3 replay
// history. Callers must use ReplaceMessages for a genuine restore, import, or
// conversation boundary.
func (a *Agent) ReplaceMessagesWithinSession(msgs []llm.Message) {
	a.replaceMessages(msgs, false)
}

// RestoreMessagesWithinSession atomically rolls back an exact transcript and
// the bounded provider receipt that described it. It is intentionally distinct
// from ReplaceMessagesWithinSession, whose arbitrary rewrites invalidate that
// receipt. A model change clears the non-portable floor while still restoring
// the transcript.
func (a *Agent) RestoreMessagesWithinSession(msgs []llm.Message, floor ContextPromptFloor) error {
	if err := floor.Validate(); err != nil {
		return err
	}
	currentModel := ""
	if a.llmClient != nil {
		currentModel = a.llmClient.Model()
	}
	if floor.Tokens > 0 && config.CanonicalModelName(floor.Model) != config.CanonicalModelName(currentModel) {
		floor = ContextPromptFloor{}
	}
	msgs = cloneMessagesWithImages(msgs)
	a.mu.Lock()
	a.messages = msgs
	a.contextPromptFloor = floor
	a.mu.Unlock()
	return nil
}

func (a *Agent) replaceMessages(msgs []llm.Message, resetContinuationHistory bool) {
	msgs = cloneMessagesWithImages(msgs)
	if resetContinuationHistory {
		msgs = restoreConversationSummaryOwnership(msgs)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = msgs
	// Any transcript rewrite invalidates the exact message prefix attached to a
	// provider receipt. Restore paths explicitly reinstall a validated durable
	// floor after replacing messages.
	a.contextPromptFloor = ContextPromptFloor{}
	if resetContinuationHistory {
		a.continuationHistory = newContinuationTurnState(0)
		a.resetAutoContinuationHistoryLocked()
		a.invalidateBobWorkspaceContextLocked()
	}
}

// ContextPromptFloor returns the current bounded provider-receipt projection
// for session persistence.
func (a *Agent) ContextPromptFloor() ContextPromptFloor {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.contextPromptFloor
}

// RestoreContextPromptFloor installs a validated persisted receipt only when it
// belongs to the currently selected provider model. A model mismatch clears it:
// token counts are not portable across tokenizers.
func (a *Agent) RestoreContextPromptFloor(floor ContextPromptFloor) error {
	if err := floor.Validate(); err != nil {
		return err
	}
	currentModel := ""
	if a.llmClient != nil {
		currentModel = a.llmClient.Model()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if floor.Tokens == 0 || config.CanonicalModelName(floor.Model) != config.CanonicalModelName(currentModel) {
		a.contextPromptFloor = ContextPromptFloor{}
		return nil
	}
	floor.Model = strings.TrimSpace(floor.Model)
	a.contextPromptFloor = floor
	return nil
}

func (a *Agent) recordContextPromptFloor(tokens, hostTokens, messageTokens int, model string) {
	floor := ContextPromptFloor{Tokens: tokens, HostTokens: hostTokens, MessageTokens: messageTokens, Model: strings.TrimSpace(model)}
	if tokens <= 0 || floor.Validate() != nil {
		return
	}
	a.mu.Lock()
	a.contextPromptFloor = floor
	a.mu.Unlock()
}

func (a *Agent) clearContextPromptFloor() {
	a.mu.Lock()
	a.contextPromptFloor = ContextPromptFloor{}
	a.mu.Unlock()
}

func (a *Agent) contextPromptFloorEstimate(model string, currentHostTokens int) int {
	a.mu.RLock()
	floor := a.contextPromptFloor
	currentMessageTokens := estimateMessagesPromptTokens(a.messages)
	a.mu.RUnlock()
	if floor.Tokens <= 0 || config.CanonicalModelName(floor.Model) != config.CanonicalModelName(model) {
		return 0
	}
	estimate := floor.Tokens
	if currentHostTokens > floor.HostTokens {
		estimate = saturatingPromptTokenAdd(estimate, currentHostTokens-floor.HostTokens)
	}
	if currentMessageTokens > floor.MessageTokens {
		estimate = saturatingPromptTokenAdd(estimate, currentMessageTokens-floor.MessageTokens)
	}
	return estimate
}

func saturatingPromptTokenAdd(value, delta int) int {
	maxInt := int(^uint(0) >> 1)
	if delta > maxInt-value {
		return maxInt
	}
	return value + delta
}

// restoreConversationSummaryOwnership re-marks the single bounded recap that
// compaction persists across a JSON/session/checkpoint boundary. Unlike durable
// recovery receipts, this text grants no execution or reconciliation authority;
// HostOwned only allows a later compaction to carry the recap forward. Malformed,
// empty, oversized, and duplicate projections are removed rather than promoted.
func restoreConversationSummaryOwnership(messages []llm.Message) []llm.Message {
	next := make([]llm.Message, 0, len(messages))
	seen := false
	for _, message := range messages {
		if message.Role != "system" || !strings.HasPrefix(message.Content, conversationSummaryPrefix) {
			next = append(next, message)
			continue
		}
		recap := strings.TrimSpace(strings.TrimPrefix(message.Content, conversationSummaryPrefix))
		if seen || recap == "" || !utf8.ValidString(recap) || estimateTextPromptTokens(recap) > maxConversationSummaryTokens {
			continue
		}
		seen = true
		message.Content = conversationSummaryPrefix + recap
		message.HostOwned = true
		next = append(next, message)
	}
	return next
}

// SetSkillContent sets the combined content of active skills.
func (a *Agent) SetSkillContent(content string) {
	a.mu.Lock()
	a.skillContent = content
	a.mu.Unlock()
}

// SetLoadedContext sets the loaded context file content.
func (a *Agent) SetLoadedContext(content string) {
	a.mu.Lock()
	a.loadedCtx = content
	a.mu.Unlock()
}

// LoadedContext returns the currently assembled loaded context.
func (a *Agent) LoadedContext() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.loadedCtx
}

// SkillContent returns the currently active skill prompt content.
func (a *Agent) SkillContent() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.skillContent
}

// Model returns the LLM model name.
func (a *Agent) Model() string {
	return a.llmClient.Model()
}

// LLMClient returns the underlying LLM client.
func (a *Agent) LLMClient() llm.Client {
	return a.llmClient
}

// ToolAvailability separates local authority from currently connected MCP
// transport. MCPRetained includes scoped definitions kept during reconnect so
// diagnostics can explain why a tool remains known without calling it ready.
type ToolAvailability struct {
	Local        int
	MCPConnected int
	MCPRetained  int
}

// Ready returns tool definitions backed by local authority or a currently
// connected MCP namespace.
func (availability ToolAvailability) Ready() int {
	return availability.Local + availability.MCPConnected
}

// ToolAvailability returns a non-blocking host snapshot. It does not ping or
// reconnect MCP servers and says nothing about downstream domain success.
func (a *Agent) ToolAvailability() ToolAvailability {
	if a == nil {
		return ToolAvailability{}
	}
	a.mu.RLock()
	policy := a.toolPolicy
	hasMemory := a.memoryStore != nil
	a.mu.RUnlock()

	availability := ToolAvailability{
		Local: len(filterToolDefsByName(a.toolsBuiltinToolDefs(), policy.localTools)),
	}
	if hasMemory {
		availability.Local += len(filterToolDefsByName(a.memoryBuiltinToolDefs(), policy.memoryTools))
	}
	if !policy.AllowMCP || a.registry == nil {
		return availability
	}

	mcpTools := a.mcpTools()
	availability.MCPRetained = len(mcpTools)
	connected := make(map[string]struct{})
	for _, status := range a.registry.ConnectionStatuses() {
		if status.Connected {
			connected[status.Name] = struct{}{}
		}
	}
	for _, tool := range mcpTools {
		server, _, namespaced := strings.Cut(tool.Name, "__")
		if !namespaced {
			continue
		}
		if _, ready := connected[server]; ready {
			availability.MCPConnected++
		}
	}
	return availability
}

// ToolCount returns the model-visible tool definition count. During an MCP
// reconnect this intentionally includes retained definitions; presentation
// surfaces should use ToolAvailability when claiming readiness.
func (a *Agent) ToolCount() int {
	availability := a.ToolAvailability()
	return availability.Local + availability.MCPRetained
}

// SetMCPServerScope restricts model-visible and executable MCP tools to the
// named servers. An empty scope keeps the default of all configured servers.
func (a *Agent) SetMCPServerScope(serverNames []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.approvalHostVersion++
	a.mcpRouteVersion++
	if len(serverNames) == 0 {
		a.mcpServerScope = nil
		a.mcpScopeSet = false
		return
	}
	a.mcpScopeSet = true
	a.mcpServerScope = make(map[string]struct{}, len(serverNames))
	for _, name := range serverNames {
		if name = strings.TrimSpace(name); name != "" {
			a.mcpServerScope[name] = struct{}{}
		}
	}
}

// DenyAllMCPTools installs an explicit empty scope. It is used when an
// explicitly requested profile fails to load, preventing fallback to the
// default all-server authority.
func (a *Agent) DenyAllMCPTools() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.approvalHostVersion++
	a.mcpRouteVersion++
	a.mcpScopeSet = true
	a.mcpServerScope = make(map[string]struct{})
}

// MCPServerScope reports the explicit MCP allowlist. restricted=false means
// the default all-server scope; restricted=true with no names means deny all.
func (a *Agent) MCPServerScope() (names []string, restricted bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.mcpScopeSet {
		return nil, false
	}
	names = make([]string, 0, len(a.mcpServerScope))
	for name := range a.mcpServerScope {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
}

func (a *Agent) mcpTools() []llm.ToolDef {
	return a.mcpToolSnapshot().Tools
}

func (a *Agent) mcpToolSnapshot() mcp.ToolSnapshot {
	if a.registry == nil {
		return mcp.ToolSnapshot{}
	}
	snapshot := a.registry.SnapshotTools()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.mcpScopeSet {
		return snapshot
	}
	filtered := make([]llm.ToolDef, 0, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		server, _, namespaced := strings.Cut(tool.Name, "__")
		if namespaced {
			if _, allowed := a.mcpServerScope[server]; allowed {
				filtered = append(filtered, tool)
			}
		}
	}
	snapshot.Tools = filtered
	return snapshot
}

func (a *Agent) allowsMCPTool(toolName string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.mcpScopeSet {
		return true
	}
	server, _, namespaced := strings.Cut(toolName, "__")
	if !namespaced {
		return false
	}
	_, allowed := a.mcpServerScope[server]
	return allowed
}

// ServerCount returns the number of connected MCP servers.
func (a *Agent) ServerCount() int {
	if a.registry == nil {
		return 0
	}
	return a.registry.ServerCount()
}

// ServerNames returns the names of connected MCP servers.
func (a *Agent) ServerNames() []string {
	if a.registry == nil {
		return nil
	}
	return a.registry.ServerNames()
}

// ReconnectMCPServer attempts to reconnect a named MCP server and returns
// the number of tools discovered on success.
func (a *Agent) ReconnectMCPServer(ctx context.Context, name string) (int, error) {
	if a.registry == nil {
		return 0, fmt.Errorf("no MCP registry configured")
	}
	return a.registry.ReconnectServer(ctx, name)
}

// MCPToolSummary is a bounded, read-only view of one MCP tool for /tools.
type MCPToolSummary struct {
	Name        string
	Description string
	Server      string
}

// MCPToolSummaries returns a bounded list of discovered MCP tools with
// their server origin parsed from the namespaced name.
func (a *Agent) MCPToolSummaries() []MCPToolSummary {
	tools := a.mcpTools()
	summaries := make([]MCPToolSummary, 0, len(tools))
	for _, td := range tools {
		server := ""
		if idx := strings.Index(td.Name, "__"); idx > 0 {
			server = td.Name[:idx]
		}
		desc := td.Description
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		summaries = append(summaries, MCPToolSummary{
			Name:        td.Name,
			Description: desc,
			Server:      server,
		})
	}
	return summaries
}

// SetWorkspacePolicy atomically updates the workspace boundary and its ignore
// policy for the next turn. Embeddings that reload both values should prefer
// this method so a turn cannot snapshot a mixed pair between two setter calls.
func (a *Agent) SetWorkspacePolicy(dir, ignoreContent string) {
	a.mu.Lock()
	workspaceChanged := a.workDir != dir
	if a.workDir != dir || a.ignoreContent != ignoreContent {
		a.workDir = dir
		a.ignoreContent = ignoreContent
		a.filesystemVersion++
	}
	if workspaceChanged {
		a.continuationHistory = newContinuationTurnState(0)
		a.resetAutoContinuationHistoryLocked()
		a.invalidateBobWorkspaceContextLocked()
	}
	a.mu.Unlock()
}

// SetWorkDir sets only the configured working directory. Use
// SetWorkspacePolicy when the corresponding ignore policy changes too.
func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	changed := a.workDir != dir
	if changed {
		a.workDir = dir
		a.filesystemVersion++
		a.continuationHistory = newContinuationTurnState(0)
		a.resetAutoContinuationHistoryLocked()
		a.invalidateBobWorkspaceContextLocked()
	}
	a.mu.Unlock()
	if changed {
		_ = a.ReloadWorkspaceRules()
	}
}

// WorkDir returns the configured workspace boundary. A running turn keeps the
// snapshot it admitted with; a value configured mid-turn applies next turn.
func (a *Agent) WorkDir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.workDir
}

// SetIgnoreContent sets only the configured .agentignore content. Use
// SetWorkspacePolicy when changing the workspace and ignore policy together.
func (a *Agent) SetIgnoreContent(content string) {
	a.mu.Lock()
	if a.ignoreContent != content {
		a.ignoreContent = content
		a.filesystemVersion++
	}
	a.mu.Unlock()
}

// SetPermissionChecker sets the permission checker for tool approval.
func (a *Agent) SetPermissionChecker(checker *permission.Checker) {
	a.mu.Lock()
	if a.permChecker != checker {
		a.permChecker = checker
		a.approvalHostVersion++
	}
	a.mu.Unlock()
}

// SetApprovalCallback sets the callback for requesting user approval.
func (a *Agent) SetApprovalCallback(cb func(permission.ApprovalRequest)) {
	a.mu.Lock()
	a.approvalCallback = cb
	a.approvalHostVersion++
	a.mu.Unlock()
}

// SetICEEngine sets the ICE engine for cross-session context retrieval.
func (a *Agent) SetICEEngine(engine *ice.Engine) {
	a.mu.Lock()
	a.iceEngine = engine
	a.mu.Unlock()
}

// ICEEngine returns the ICE engine, or nil if not enabled.
func (a *Agent) ICEEngine() *ice.Engine {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.iceEngine
}

// ICESessionID returns the session scope selected for the next turn. Before a
// durable session exists it falls back to the engine's generated transient
// scope. This is a bounded status projection; RunTurn remains the sole owner of
// applying the desired scope to the mutable engine.
func (a *Agent) ICESessionID() string {
	a.mu.RLock()
	scope := a.iceSessionScope
	engine := a.iceEngine
	a.mu.RUnlock()
	if scope != "" {
		return scope
	}
	if engine != nil {
		return engine.SessionID()
	}
	return ""
}

// PrepareModelSwitch cancels and joins background ICE inference so the model
// manager can unload/switch without racing a stream on the previous model.
func (a *Agent) PrepareModelSwitch() {
	if engine := a.ICEEngine(); engine != nil {
		engine.StopAutoMemory()
	}
}

// CommitModelSwitch invalidates tokenizer-specific admission receipts only
// after the host has successfully committed a different model. Failed switch
// attempts leave the old model and its exact floor intact.
func (a *Agent) CommitModelSwitch() {
	a.clearContextPromptFloor()
}

// SetToolsConfig sets the tools configuration.
func (a *Agent) SetToolsConfig(cfg config.ToolsConfig) {
	a.toolsConfig = cfg
}

// SetContinuationsConfig installs the host policy used by future turns.
// Invalid embedded-caller input fails closed to off; command/config callers
// normally validate before reaching this boundary.
func (a *Agent) SetContinuationsConfig(cfg config.ContinuationsConfig) {
	if !cfg.Mode.Valid() || cfg.MaxAutoSteps < 0 || cfg.MaxAutoSteps > config.MaxAutoContinuationSteps ||
		(cfg.Mode == config.ContinuationAutoReadOnly && cfg.MaxAutoSteps == 0) {
		cfg = config.ContinuationsConfig{Mode: config.ContinuationOff}
	}
	a.mu.Lock()
	a.continuationsConfig = cfg
	a.mu.Unlock()
}

func (a *Agent) continuationsConfigSnapshot() config.ContinuationsConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	cfg := a.continuationsConfig
	if !cfg.Mode.Valid() || cfg.MaxAutoSteps < 0 || cfg.MaxAutoSteps > config.MaxAutoContinuationSteps ||
		(cfg.Mode == config.ContinuationAutoReadOnly && cfg.MaxAutoSteps == 0) {
		return config.ContinuationsConfig{Mode: config.ContinuationOff}
	}
	return cfg
}

// MaxIterations returns the configured max iterations, or default if not set.
func (a *Agent) MaxIterations() int {
	if a.toolsConfig.MaxIterations > 0 {
		return a.toolsConfig.MaxIterations
	}
	return defaultMaxIterations
}

// MaxIterationsForAuthority keeps interactive turns concise while giving AUTO
// enough room for normal inspect/edit/verify loops. The limit remains bounded
// so a provider that never settles cannot run forever.
func (a *Agent) MaxIterationsForAuthority(mode AuthorityMode) int {
	if mode == AuthorityAutoScoped {
		if a.toolsConfig.AutoMaxIterations > 0 {
			return a.toolsConfig.AutoMaxIterations
		}
		return defaultAutoMaxIterations
	}
	return a.MaxIterations()
}

// ToolTimeout returns the configured tool timeout, or default if not set.
func (a *Agent) ToolTimeout() time.Duration {
	if a.toolsConfig.Timeout != "" {
		if d, err := time.ParseDuration(a.toolsConfig.Timeout); err == nil {
			return d
		}
	}
	return defaultToolTimeout
}

// MaxGrepResults returns the configured max grep results, or default if not set.
func (a *Agent) MaxGrepResults() int {
	if a.toolsConfig.MaxGrepResults > 0 {
		return a.toolsConfig.MaxGrepResults
	}
	return defaultMaxGrepResults
}

// Close cleans up resources.
func (a *Agent) Close() {
	a.turnMu.Lock()
	if a.closed {
		done := a.turnDone
		a.turnMu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	a.closed = true
	cancel := a.turnCancel
	done := a.turnDone
	a.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	a.closeBackgroundShells()
	a.mcphubResults.Reset()
	a.clearContinuationContracts()
	if engine := a.ICEEngine(); engine != nil {
		_ = engine.Close()
	}
	if a.registry != nil {
		a.registry.Close()
	}
	a.closeReadRoots()
	a.closeWriteGrants()
}
