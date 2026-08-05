package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/log"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
	"github.com/abdul-hamid-achik/sonar/internal/skill"
)

// Model is the BubbleTea model for the chat interface.
type Model struct {
	// UI components
	viewport      viewport.Model
	input         textarea.Model
	spin          spinner.Model
	scramble      ScrambleModel
	styles        Styles
	md            *MarkdownRenderer
	markdownWidth int
	keys          KeyMap

	// State
	state                    State
	overlay                  OverlayKind
	overlayParent            OverlayKind
	entries                  []ChatEntry
	streamBuf                strings.Builder
	lastStreamPaint          time.Time // throttles per-token re-renders during streaming
	turnStartedAt            time.Time
	lastTurnDuration         time.Duration
	now                      func() time.Time
	activityHeartbeatToken   uint64
	activityHeartbeatPending bool
	// chromeSpring drives sticky text reveal + context meter ease (harmonica).
	chromeSpring chromeSpringState
	width        int
	height       int
	ready        bool
	isDark       bool
	// themeID selects the color scheme. Empty means the default; it is passed
	// explicitly to every palette lookup rather than held in a package global,
	// because the ui tests run in parallel and a shared mutable theme would let
	// one test repaint another's assertions.
	themeID string
	// themePickerBase is the committed theme captured when the theme picker
	// opens. Live preview mutates themeID while the user navigates; cancelling
	// restores this value without persisting, Enter persists the previewed one.
	themePickerBase       string
	reducedMotion         bool
	glyphProfile          GlyphProfile
	evalCount             int
	promptTokens          int
	turnEvalTotal         int
	turnPromptTotal       int
	toolsPending          int
	capabilityRoute       *agent.CapabilityRoute
	lastCapabilityRoute   *agent.CapabilityRoute
	continuation          continuationActionState
	bobWorkspaceContext   bobWorkspaceContextState
	inputLines            int
	composerMeasureDigest [32]byte
	composerMeasureW      int
	composerMeasureRows   int
	userScrolledUp        bool

	// Scroll anchor system - prevents jitter during streaming.
	anchorActive bool // true when user wants to stay at bottom
	// transcriptLayout is the last renderer-owned semantic geometry snapshot.
	// It is independent of viewport position and is replaced atomically after
	// each transcript render.
	transcriptLayout TranscriptLayoutSnapshot
	// transcriptPaint owns the logical document offset and bounded Bubbles
	// staging window. The viewport's private YOffset is local to that window
	// and must never be interpreted as a document coordinate.
	transcriptPaint transcriptPaintState

	// Transcript identity reconciliation is semantic work, not paint work.
	// invalidateEntryCache clears this admission cache; ordinary spinner and
	// streaming paints reuse the already-reconciled entries.
	transcriptReconcileValid     bool
	transcriptReconciledCount    int
	transcriptReconciledTurnID   TurnID
	transcriptReconciledBlockIDs map[BlockID]struct{}
	transcriptReconcileEpoch     uint64
	transcriptRenderProbe        *transcriptRenderProbe
	liveTailLayoutID             BlockID
	liveTailLayoutTurnID         TurnID
	liveTailLayoutSessionID      int64
	liveTailLayoutEpoch          uint64
	liveTailLayoutReconcile      uint64
	liveTailLayoutVisible        bool

	// Startup
	initializing bool
	turnReady    bool
	startupItems []startupItem
	initCancel   context.CancelFunc

	// Composer-owned completion popup
	completionState           *CompletionState // nil when no overlay
	completionSuppressedDraft string           // exact unchanged draft dismissed with Escape
	completionGeneration      uint64
	completionSearch          func(context.Context, string, string) []Completion
	completionReader          *completionWorkspaceReader
	// Transcript search owns a bounded, safe projection of the virtualized
	// transcript while its footer surface is open. It never retains raw tool
	// payloads or reasoning content.
	transcriptSearch *TranscriptSearchState

	// Tool display
	toolEntries        []ToolEntry
	outputDetails      *OutputDetailStore
	modalStack         ModalStack
	outputViewers      map[OverlayID]*OutputViewer
	diffViewers        map[OverlayID]*DiffViewer
	toolsCollapsed     bool
	toolHitRegions     []toolHitRegion
	thinkingHitRegions []thinkingHitRegion
	diffGeneration     uint64
	// Receipt inspection is an ephemeral affordance for the just-completed
	// turn. It must never point at a stale tool from an earlier no-tool turn.
	turnToolStartIndex          int
	lastTurnToolIndex           int
	receiptInspectActive        bool
	receiptInspectToolIndex     int
	receiptInspectAnchorPaused  bool
	receiptInspectAnchorYOffset int
	receiptInspectReflowAnchor  transcriptReflowAnchor

	// Incremental rendering cache
	cachedEntriesRender      string
	cachedEntryCount         int
	cachedStableCount        int
	cachedPrefixLayoutCount  int
	cachedPrefixState        entryRenderState
	cachedToolHitRegions     []toolHitRegion
	cachedThinkingHitRegions []thinkingHitRegion
	entryCacheValid          bool

	// Per-entry render memo: a settled entry re-renders only when its
	// composite key changes, so full transcript walks stay cheap. Live
	// (running) tool groups bypass the memo entirely.
	entryMemo map[BlockID]entryRenderMemo

	// Thinking state
	thinkBuf       strings.Builder
	inThinking     bool
	thinkSearchBuf string

	// Single transient status-line slot (see notice.go). Also drives the
	// terminal-title "done" receipt while a success notice is active.
	footerNotice *footerNotice

	// Session persistence
	sessionID          int64
	sessionPublicID    string
	activeSessionTitle string
	// sessionTitleNeedsAI is set when the durable title is still the provisional
	// first-line prompt; a background model job upgrades it after the first turn.
	sessionTitleNeedsAI          bool
	sessionTitleAIDone           bool
	sessionTitleGenToken         uint64
	sessionTitleGen              sessionTitleGen
	executionCursor              int64
	executionLease               *db.ExecutionSessionLease
	sessionStore                 *db.Store
	sessionStateMu               sync.RWMutex
	sessionStateRevision         int64
	sessionStateRevisionKnown    bool
	sessionStatePersistenceDirty bool
	sessionsPickerState          *SessionsPickerState
	sessionLoadToken             uint64
	sessionLoading               bool
	sessionLoadCancel            context.CancelFunc
	pendingSessionSwitch         *pendingSessionSwitch
	sessionListToken             uint64
	sessionListing               bool
	sessionListCancel            context.CancelFunc
	startupResumeSelector        *SessionResumeSelector

	// Paste detection
	pendingPaste *pendingPaste

	// Image attachments are admitted into a private content-addressed store.
	// Provider bytes remain transient; refs are safe to persist and render.
	imageStore          *imageasset.Store
	pendingImages       []pendingImageAttachment
	turnImages          []pendingImageAttachment
	imageAttachToken    uint64
	imageAttachRunning  bool
	imageAttachCancel   context.CancelFunc
	imageAttachFallback string
	imageAttachQueue    []imageFileAttachmentRequest

	// Responsive layout
	forceCompact bool // user-toggled compact mode

	// Mode system
	mode        Mode
	modeConfigs [3]ModeConfig

	// Model management
	modelManager             *llm.ModelManager
	router                   config.ModelRouter
	modelPreferenceStore     ModelPreferenceStore
	themePickerState         *ThemePickerState
	contextDoctorState       *ContextDoctorState
	contextDoctorRequest     uint64
	modelPickerState         *ModelPickerState
	modelInventoryRequest    uint64
	manualOnlyModels         map[string]struct{}
	settingsPickerState      *SettingsPickerState
	permissionsPanelState    *PermissionsPanelState
	agentPickerState         *AgentPickerState
	providerPickerState      *ProviderPickerState
	agentHubState            *AgentHubState
	providerSwitchToken      uint64
	numCtxApplyToken         uint64
	providerSwitchRunning    bool
	providerSwitchName       string
	providerSwitchCancel     context.CancelFunc
	modePickerState          *ModePickerState
	runtimeStatusState       *RuntimeStatusState
	planFormState            *PlanFormState
	goalFormState            *GoalForm
	goalInspectorState       *GoalInspector
	goalPlan                 *goalPlanCard
	goalRecoveryState        *GoalRecovery
	goalRecoveryProjection   goalRecoveryProjection
	goalRecoveryLoadToken    uint64
	goalRecoveryLoadRunning  bool
	goalRecoveryLoadScope    goalRecoveryOperationScope
	goalRecoveryApplyToken   uint64
	goalRecoveryApplyRunning bool
	goalRecoveryApplyItemID  string
	standaloneRecovery       *standaloneRecoveryState
	modelPinned              bool

	// Goal Runtime. The host owns continuation, budget, cancellation and
	// persistence; Cortex is an advisory semantic state machine only.
	goalRuntime           *goal.Runtime
	goalAdvisor           GoalAdvisor
	goalOperation         string
	goalOperationToken    uint64
	goalOperationCancel   context.CancelFunc
	goalOperationRunning  bool
	cortexDecision        *cortexDecisionPresentation
	cortexDecisionOp      *cortexDecisionOperation
	cortexDecisionAttempt *cortexDecisionAttempt
	cortexDecisionGen     uint64
	goalTurnID            string
	goalTurnToolCalls     int
	goalTurnSuccesses     int
	goalNeedsEvaluation   bool
	goalPersistenceDirty  bool

	// Logging
	logger *log.Logger

	// Features
	agent               *agent.Agent
	cmdRegistry         *command.Registry
	skillMgr            *skill.Manager
	completer           *Completer
	loadedFile          string
	agentsDir           *config.AgentsDir
	baseLoadedContext   string
	manualLoadedContext string
	manualSkills        []string
	profileSkills       []string

	// Runtime
	program            *tea.Program
	cancel             context.CancelFunc
	commitCancel       context.CancelFunc
	commitRunner       commitEffectRunner
	commitToken        uint64
	commitRunning      bool
	fileOpToken        uint64
	fileLoading        bool
	readScopeOpToken   uint64
	readScopeOpRunning bool
	readScopeOpLabel   string
	readScopeOpDraft   string
	readScopePrompt    *ReadScopePrompt
	// Input crosses a bounded two-phase resume handshake after an undersized
	// terminal becomes supported. Charm does not expose input provenance across
	// its concurrent senders, so quiet windows mitigate reordering while a
	// consumed explicit gesture keeps authority surfaces fail-closed.
	terminalInputResumePhase terminalInputResumePhase
	terminalInputResumeToken uint64
	terminalInputResumeAt    time.Time
	terminalInputTickPending bool
	exportToken              uint64
	exportRunning            bool
	compactingContext        bool
	shuttingDown             bool

	// Display info
	model                     string
	modelList                 []string
	ollamaModels              []OllamaModelDescriptor
	ollamaVersion             string
	localOnly                 bool
	ollamaOffline             bool
	ollamaInventoryAttempted  bool
	pendingOllamaInventory    *OllamaModelInventoryMsg
	ollamaInventoryCommitting bool
	ollamaInventoryCommitID   uint64
	agentProfile              string
	agentList                 []string
	toolCount                 int
	serverCount               int
	numCtx                    int
	approvalPosture           ApprovalPosture
	expertRuntimeSetupFailed  bool

	failedServers []FailedServer
	mcpServers    []MCPServerStatus

	// ICE
	iceEnabled       bool
	iceConversations int
	iceSessionID     string

	// Session token totals
	sessionEvalTotal   int
	sessionPromptTotal int
	sessionTurnCount   int

	// File change tracking
	fileChanges map[string]int // path → number of modifications

	// Tool approval prompt
	pendingApproval    *ToolApprovalMsg
	approvalState      *ApprovalState
	queuedFollowUp     *queuedFollowUp
	turnMessagesBefore []llm.Message
	turnPromptFloor    agent.ContextPromptFloor
	turnPrompt         string
	turnPromptVisible  bool
	turnEntryIndex     int
	turnCheckpointSet  bool
	turnRunContext     context.Context
	turnRunOptions     agent.TurnOptions
	turnLogicalID      string
	turnSegmentID      string
	turnAuthority      Mode
	autoCheckpoints    autoCheckpointSupervisor

	// Prompt history
	promptHistory      []string // all submitted inputs
	historyIndex       int      // -1 = not browsing, 0 = most recent
	historySaved       string   // saved current input when entering history
	clipboardRead      func() (string, error)
	clipboardWrite     func(string) error
	clipboardImageRead func(context.Context) (string, []byte, error)

	// Help overlay viewport (scrollable)
	helpViewport viewport.Model

	// configSourcePath is the host config file used by /context save. Empty when
	// the process started without a writable resolved config path.
	configSourcePath string
}

// New creates a new TUI Model.
func New(ag *agent.Agent, cmdReg *command.Registry, skillMgr *skill.Manager, completer *Completer, modelManager *llm.ModelManager, router config.ModelRouter, logger *log.Logger) *Model {
	reducedMotion := reducedMotionRequested()
	glyphProfile := requestedGlyphProfile()
	ta := textarea.New()
	ta.Placeholder = "Ask"
	ta.Focus()
	ta.CharLimit = 32 * 1024
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = maxComposerVisibleRows
	// Keep the visible viewport cap separate from admission. Bubbles measures
	// MaxContentHeight in wrapped visual rows, while sonar's paste guard
	// owns the logical-line and character limits. A reachable visual-row cap can
	// therefore truncate an otherwise accepted, heavily wrapped paste.
	ta.MaxContentHeight = math.MaxInt
	// Clipboard reads are parent-owned so Ctrl+V produces the same public
	// tea.PasteMsg path as terminal bracketed paste and cannot bypass review.
	ta.KeyMap.Paste.SetEnabled(false)
	ta.SetHeight(1)
	ta.ShowLineNumbers = false
	// A single send marker followed by continuation rails makes multiline
	// drafts read as one composer instead of several submitted messages.
	configureComposerModeWithGlyphProfile(&ta, true, defaultThemeID, ModeNormal, reducedMotion, glyphProfile)

	initialStyles := NewStyles(true, defaultThemeID)
	mainSpinner := spinner.MiniDot
	if glyphProfile == GlyphASCII {
		mainSpinner = spinner.Line
	}
	// Spinner sits in the content-grid accent cell on the activity rail; omit
	// StatusDot's left pad so Prefix owns OriginX exclusively.
	s := spinner.New(
		spinner.WithSpinner(mainSpinner),
		spinner.WithStyle(initialStyles.StatusDot.UnsetPaddingLeft()),
	)

	initialModel := ""
	initialNumCtx := 0
	if modelManager != nil {
		initialModel = modelManager.CurrentModel()
		initialNumCtx = modelManager.NumCtx()
	}

	return &Model{
		input:                   ta,
		clipboardRead:           clipboard.ReadAll,
		clipboardWrite:          clipboard.WriteAll,
		clipboardImageRead:      readClipboardImage,
		spin:                    s,
		scramble:                NewScrambleModel(true),
		styles:                  initialStyles,
		keys:                    DefaultKeyMap(),
		state:                   StateIdle,
		isDark:                  true,
		themeID:                 defaultThemeID,
		reducedMotion:           reducedMotion,
		glyphProfile:            glyphProfile,
		chromeSpring:            newChromeSpringState(),
		now:                     time.Now,
		inputLines:              1,
		outputDetails:           NewOutputDetailStore(),
		outputViewers:           make(map[OverlayID]*OutputViewer),
		diffViewers:             make(map[OverlayID]*DiffViewer),
		toolsCollapsed:          true,
		initializing:            true,
		model:                   initialModel,
		numCtx:                  initialNumCtx,
		approvalPosture:         ApprovalPosturePrompted,
		mode:                    ModeNormal,
		modeConfigs:             DefaultModeConfigs(),
		modelManager:            modelManager,
		router:                  router,
		logger:                  logger,
		agent:                   ag,
		cmdRegistry:             cmdReg,
		skillMgr:                skillMgr,
		completer:               completer,
		completionReader:        newCompletionWorkspaceReader(),
		historyIndex:            -1,
		lastTurnToolIndex:       -1,
		receiptInspectToolIndex: -1,
		turnEntryIndex:          -1,
		commitRunner:            runCommit,
	}
}

// SetTheme selects the color scheme and rebuilds every cached style. It
// returns false for an unregistered id so a caller can report the typo instead
// of silently repainting in the default.
func (m *Model) SetTheme(id string) bool {
	if m == nil || !knownThemeID(id) {
		return false
	}
	resolved := normalizeThemeID(id)
	if resolved == m.themeID {
		return true
	}
	m.themeID = resolved
	m.rebuildThemedSurfaces()
	return true
}

// ThemeID reports the active color scheme.
func (m *Model) ThemeID() string {
	if m == nil || m.themeID == "" {
		return defaultThemeID
	}
	return m.themeID
}

// SetConfigSourcePath records the resolved host config path so /context save
// can rewrite ollama.num_ctx without scanning for config files again.
func (m *Model) SetConfigSourcePath(path string) {
	m.configSourcePath = strings.TrimSpace(path)
}

// SetProgram sets the tea.Program reference (must be called before Run).
func (m *Model) SetProgram(p *tea.Program) {
	m.program = p
}

// SetModelPinned preserves an explicit CLI or agent-profile model selection.
// Automatic routing remains available after the user runs /model auto.
func (m *Model) SetModelPinned(pinned bool) {
	m.modelPinned = pinned
}

// SetModelRoutingCatalog installs the host-owned manual-only model policy used
// by later asynchronous Ollama inventory refreshes. Availability still comes
// exclusively from Ollama; this catalog can only remove automatic-routing
// authority from a discovered local model.
func (m *Model) SetModelRoutingCatalog(models []config.Model) {
	manualOnly := make(map[string]struct{})
	for _, model := range models {
		if !model.Exclusive {
			continue
		}
		if canonical := config.CanonicalModelName(model.Name); canonical != "" {
			manualOnly[canonical] = struct{}{}
		}
	}
	m.manualOnlyModels = manualOnly
}

// SetSessionStore enables private SQLite-backed, lossless session resume.
func (m *Model) SetSessionStore(store *db.Store) {
	m.sessionStore = store
}

// SetStartupSessionResume schedules one validated restore after InitComplete.
// It never starts provider work and shares the interactive picker's tokened
// restore authority.
func (m *Model) SetStartupSessionResume(selector SessionResumeSelector) error {
	if !selector.valid() {
		return fmt.Errorf("invalid startup session resume selector")
	}
	copy := selector
	m.startupResumeSelector = &copy
	return nil
}

// SetInitCancel stores the cancel function for the background init goroutine.
func (m *Model) SetInitCancel(cancel context.CancelFunc) {
	m.initCancel = cancel
}

func (m *Model) beginShutdown() tea.Cmd {
	m.shuttingDown = true
	m.cancelTerminalInputResume()
	m.cancelSessionTitleGen()
	m.pendingOllamaInventory = nil
	if m.providerSwitchCancel != nil {
		m.providerSwitchCancel()
	}
	if m.goalOperationCancel != nil {
		m.goalOperationCancel()
	}
	m.fileOpToken++
	m.fileLoading = false
	if m.imageAttachCancel != nil {
		m.imageAttachCancel()
	}
	m.clearImageAttachmentQueue()
	if m.initCancel != nil {
		m.initCancel()
	}
	if m.pendingApproval != nil {
		m.resolvePendingApproval(permission.Cancelled("application is shutting down"))
	}
	m.pendingPaste = nil
	m.clearPendingSessionSwitchSnapshot()
	if m.readScopePrompt != nil {
		releaseReadGrants(m.readScopePrompt.Grants)
		releaseWriteGrants(m.readScopePrompt.WriteGrants)
	}
	m.readScopePrompt = nil
	if m.sessionLoading {
		m.cancelSessionLoadForShutdown()
	}
	if m.sessionListing {
		m.cancelSessionListForShutdown()
	}
	if m.commitCancel != nil {
		m.commitCancel()
	}
	if m.cancel != nil {
		m.cancel()
	}
	if !m.shutdownReady() {
		return m.startActivityCmd()
	}
	return tea.Quit
}

func (m *Model) shutdownReady() bool {
	return m.cancel == nil && !m.commitRunning && !m.exportRunning && !m.goalOperationRunning &&
		!m.sessionLoading && !m.sessionListing && !m.imageAttachRunning && !m.ollamaInventoryCommitting &&
		!m.readScopeOpRunning && !m.providerSwitchRunning
}

func (m *Model) appendShutdownQuit(commands *[]tea.Cmd) {
	if m.shuttingDown && m.shutdownReady() {
		*commands = append(*commands, tea.Quit)
	}
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		textarea.Blink,
		tea.RequestBackgroundColor,
	}
	if cmd := m.startActivityCmd(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (retModel tea.Model, retCmd tea.Cmd) {
	// Never let a single message handler panic take down the whole UI and
	// leave the terminal in a broken state. Recover, log the stack, surface
	// the error in the chat, and keep running.
	defer func() {
		if r := recover(); r != nil {
			if m.logger != nil {
				m.logger.Error("panic recovered in Update", "panic", r, "stack", string(debug.Stack()))
			}
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: fmt.Sprintf("internal error (recovered): %v", r),
			})
			m.refreshTranscript()
			retModel = m
			retCmd = nil
		}
	}()

	var cmds []tea.Cmd
	// Resize and terminal input are produced by independent Bubble Tea sources.
	// Gate every terminal-originated event in one place while the size fallback
	// or explicit resume handshake owns the screen. Charm exposes no physical
	// input timestamp, so the consumed re-arm gesture—not inferred event order—
	// keeps authority surfaces closed. Ctrl+C alone retains its owner-specific
	// graceful-shutdown path below.
	if paused, cmd := m.pauseTerminalOriginatedInput(msg); paused {
		return m, cmd
	}

	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		m.handleThemeChange(msg)

	case tea.WindowSizeMsg:
		cmds = m.handleWindowSize(msg, cmds)

	case goalRecoveryLoadResultMsg:
		if cmd := m.handleGoalRecoveryLoadResult(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case goalRecoveryApplyResultMsg:
		if cmd := m.handleGoalRecoveryApplyResult(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case standaloneRecoveryInspectResultMsg:
		if cmd := m.handleStandaloneRecoveryInspect(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case standaloneRecoveryApplyResultMsg:
		if cmd := m.handleStandaloneRecoveryApply(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case ShutdownMsg:
		return m, m.beginShutdown()

	case terminalInputResumeMsg:
		if cmd := m.finishTerminalInputResume(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case activityHeartbeatMsg:
		return m, m.handleActivityHeartbeat(msg)

	case tea.KeyPressMsg:
		if cmd, handled := m.handleKeyPress(msg); handled {
			return m, cmd
		}

	case StreamTextMsg:
		cmds = m.handleStreamText(msg, cmds)

	case StreamThinkingMsg:
		cmds = m.handleStreamThinking(msg, cmds)

	case StreamDoneMsg:
		m.handleStreamDone(msg)

	case ContextCompactedMsg:
		m.handleContextCompacted(msg)

	case ContextCompactionStartedMsg:
		cmds = m.handleContextCompactionStarted(msg, cmds)

	case ContextCompactionFinishedMsg:
		m.handleContextCompactionFinished(msg)

	case ToolCallStartMsg:
		cmds = m.handleToolCallStart(msg, cmds)

	case ExpertProgressMsg:
		if cmd := m.handleExpertProgress(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case PlanFormCompletedMsg:
		return m, m.submitPlanFormPrompt(msg.Prompt)

	case goalOpenResultMsg:
		return m, m.handleGoalOpenResult(msg)

	case goalStatusResultMsg:
		return m, m.handleGoalStatusResult(msg)

	case cortexDecisionAnswerResultMsg:
		return m, m.handleCortexDecisionAnswerResult(msg)

	case ToolCallResultMsg:
		cmds = m.handleToolCallResult(msg, cmds)

	case outputViewerPageResultMsg:
		m.handleOutputViewerPageResult(msg)

	case viewerClipboardResultMsg:
		if cmd := m.handleViewerClipboardResult(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case clipboardResultMsg:
		if msg.Err != nil {
			cmds = append(cmds, m.setFooterNotice(
				noticeWarning, "Clipboard is unavailable.", 2*time.Second,
			))
		} else {
			cmds = append(cmds, m.setFooterNotice(
				noticeSuccess, "Copied to clipboard.", 2*time.Second,
			))
		}

	case diffBuildResultMsg:
		m.handleDiffBuildResult(msg)

	case SystemMessageMsg:
		m.handleSystemMessage(msg)

	case CapabilityRouteMsg:
		m.handleCapabilityRoute(msg)

	case ContinuationActionMsg:
		m.handleContinuationAction(msg)

	case BobWorkspaceContextMsg:
		m.handleBobWorkspaceContext(msg)

	case ErrorMsg:
		m.handleErrorMsg(msg)

	case AgentDoneMsg:
		cmds = m.handleAgentDone(msg, cmds)

	case sessionTitleJobMsg:
		if cmd := m.handleSessionTitleJob(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case systemNoticeMsg:
		kind := "system"
		if msg.IsError {
			kind = "error"
		}
		m.entries = append(m.entries, ChatEntry{Kind: kind, Content: msg.Text})
		m.refreshTranscript()
		m.resumeFollow()

	case providerSwitchResultMsg:
		cmds = m.handleProviderSwitchResult(msg, cmds)

	case numCtxAppliedMsg:
		cmds = append(cmds, m.handleNumCtxApplied(msg))

	case StartupStatusMsg:
		m.handleStartupStatus(msg)

	case CoreReadyMsg:
		m.handleCoreReady(msg)

	case InitCompleteMsg:
		cmds = m.handleInitComplete(msg, cmds)

	case MCPStatusSnapshotMsg:
		m.applyMCPStatusSnapshot(msg.Servers)

	case ReadScopePreviewResultMsg:
		m.handleReadScopePreviewResult(msg)
		m.appendShutdownQuit(&cmds)

	case PromptPathPreflightResultMsg:
		if cmd := m.handlePromptPathPreflightResult(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.appendShutdownQuit(&cmds)

	case CommandResultMsg:
		m.handleCommandResult(msg)

	case ImageAttachmentResultMsg:
		if cmd := m.handleImageAttachmentResult(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.appendShutdownQuit(&cmds)

	case ReadScopeResultMsg:
		if cmd := m.handleReadScopeResult(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.appendShutdownQuit(&cmds)

	case ContextLoadResultMsg:
		m.handleContextLoadResult(msg)

	case ImportResultMsg:
		m.handleImportResult(msg)

	case ExportResultMsg:
		cmds = m.handleExportResult(msg, cmds)

	case CompletionDebounceTickMsg:
		if cmd, handled := m.handleCompletionDebounceTick(msg); handled {
			return m, cmd
		}

	case CompletionSearchResultMsg:
		cmds = m.handleCompletionSearchResult(msg, cmds)

	case completionPreviewResultMsg:
		m.handleCompletionPreviewResult(msg)

	case ToolApprovalMsg:
		m.handleToolApprovalRequest(msg)

	case CommitResultMsg:
		cmds = m.handleCommitResult(msg, cmds)

	case editorReturnMsg:
		m.handleEditorReturn(msg)

	case footerNoticeExpiredMsg:
		m.handleFooterNoticeExpired(msg)

	case SessionListMsg:
		m.handleSessionList(msg)
		m.appendShutdownQuit(&cmds)

	case SessionLoadedMsg:
		cmds = append(cmds, m.handleSessionLoadedReceipt(msg))
		m.appendShutdownQuit(&cmds)

	case tea.MouseWheelMsg:
		return m, m.handleMouseWheel(msg)

	case tea.MouseClickMsg:
		if cmd, handled := m.handleMouseClickMsg(msg); handled {
			return m, cmd
		}

	case tea.PasteMsg:
		if cmd, handled := m.handlePasteMsg(msg); handled {
			return m, cmd
		}

	case ClipboardImagePasteMsg:
		if cmd, handled := m.handleClipboardImagePaste(msg); handled {
			return m, cmd
		}
	}

	// Waiting owns the scramble clock. Once streaming starts (or the turn
	// finishes), the next queued tick is ignored and the chain terminates.
	if _, ok := msg.(ScrambleTickMsg); ok && m.needsScramble() {
		var cmd tea.Cmd
		m.scramble, cmd = m.scramble.Update(msg)
		cmds = append(cmds, cmd)
	}

	// The parent owns one Bubbles spinner clock for startup, streaming, tools,
	// and owned operations. It advances the footer only: transcript receipts
	// are stable until a real stream, tool, or expert-progress event arrives.
	if _, ok := msg.(spinner.TickMsg); ok && m.needsSpinner() {
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
	}

	// Sticky reveal + context meter springs (harmonica). Independent of the
	// activity spinner; no-ops under reducedMotion.
	if tick, ok := msg.(chromeSpringTickMsg); ok {
		if cmd := m.handleChromeSpringTick(tick); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	// Visible Charm children own their non-key lifecycle messages (cursor
	// blinks, list filter results, spinner ticks, and progress frames). Key
	// presses stay in the explicit parent branches above so authority-changing
	// actions cannot be hidden inside presentation components.
	if _, isKey := msg.(tea.KeyPressMsg); !isKey && m.overlay != OverlayNone {
		if cmd := m.updateActiveOverlayMessage(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if _, isKey := msg.(tea.KeyPressMsg); !isKey && m.viewerModalActive() {
		if cmd := m.updateViewerMessage(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	// Update sub-components.
	if m.composerEditable() {
		hadOverflowCue := m.renderComposerOverflowCue() != ""
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)

		// Auto-grow textarea based on content.
		m.syncInputHeight()
		if hadOverflowCue != (m.renderComposerOverflowCue() != "") {
			m.recalcViewportHeight()
		}

		// Auto-trigger completion when user types /, @, or #
		newInput := m.input.Value()
		suppressed := m.completionSuppressedDraft != "" && newInput == m.completionSuppressedDraft
		if m.completionSuppressedDraft != "" && !suppressed {
			// Suppression is tied to one exact draft. The first edit restores normal
			// automatic discovery, including reopening for the edited prefix.
			m.completionSuppressedDraft = ""
		}
		if m.completer != nil && newInput != "" && !m.isCompletionActive() && !suppressed {
			cmds = append(cmds, m.triggerCompletion(newInput))
		}
	}

	// A transcript key that reaches this point belongs to the composer (the
	// non-empty Ctrl+U/Ctrl+D case). Do not let the transcript consume it too.
	keyMsg, isKey := msg.(tea.KeyPressMsg)
	composerOwnedScrollKey := isKey && m.transcriptScrollKey(keyMsg)
	if !composerOwnedScrollKey {
		cmds = append(cmds, m.updateTranscriptViewport(msg))
	}
	// A resize can make the entire document fit and therefore make its only
	// logical top also "bottom". That geometric coincidence is not user intent:
	// keep a manually paused reader paused until explicit navigation resumes
	// follow.
	if _, resized := msg.(tea.WindowSizeMsg); !resized {
		m.checkAutoScroll()
	}

	// Kick sticky/context springs after state may have changed this frame.
	if cmd := m.maybeKickChromeSpring(); cmd != nil {
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) updateActiveOverlayMessage(msg tea.Msg) tea.Cmd {
	switch m.overlay {
	case OverlayCompletion:
		if m.completionState != nil {
			var cmd tea.Cmd
			m.completionState.Filter, cmd = m.completionState.Filter.Update(msg)
			return cmd
		}
	case OverlayTranscriptSearch:
		return m.updateTranscriptSearchMessage(msg)
	case OverlayContextDoctor:
		return m.updateContextDoctorMessage(msg)
	case OverlaySettings:
		if m.settingsPickerState != nil {
			var cmd tea.Cmd
			m.settingsPickerState.List, cmd = m.settingsPickerState.List.Update(msg)
			return cmd
		}
	case OverlayPermissions:
		if m.permissionsPanelState != nil {
			var cmd tea.Cmd
			m.permissionsPanelState.List, cmd = m.permissionsPanelState.List.Update(msg)
			return cmd
		}
	case OverlayAgentPicker:
		if m.agentPickerState != nil {
			var cmd tea.Cmd
			m.agentPickerState.List, cmd = m.agentPickerState.List.Update(msg)
			return cmd
		}
	case OverlayProviderPicker:
		if m.providerPickerState != nil {
			var cmd tea.Cmd
			m.providerPickerState.List, cmd = m.providerPickerState.List.Update(msg)
			return cmd
		}
	case OverlayAgents:
		return m.updateAgentHubMessage(msg)
	case OverlayModePicker:
		if m.modePickerState != nil {
			var cmd tea.Cmd
			m.modePickerState.List, cmd = m.modePickerState.List.Update(msg)
			return cmd
		}
	case OverlayModelPicker:
		if m.modelPickerState != nil {
			var cmd tea.Cmd
			m.modelPickerState.List, cmd = m.modelPickerState.List.Update(msg)
			return cmd
		}
	case OverlaySessionsPicker:
		if m.sessionsPickerState != nil && m.sessionsPickerState.ready() {
			var cmd tea.Cmd
			m.sessionsPickerState.List, cmd = m.sessionsPickerState.List.Update(msg)
			return cmd
		}
	case OverlayGoalForm:
		if m.goalFormState != nil {
			_, cmd := m.goalFormState.Update(msg)
			return cmd
		}
	case OverlayGoalRecovery:
		if m.goalRecoveryState != nil {
			_, cmd := m.goalRecoveryState.Update(msg)
			return cmd
		}
	case OverlayPlanForm:
		if m.planFormState != nil && m.planFormState.ActiveField >= 0 && m.planFormState.ActiveField < len(m.planFormState.Fields) {
			field := &m.planFormState.Fields[m.planFormState.ActiveField]
			if field.Kind == "text" {
				var cmd tea.Cmd
				field.Input, cmd = field.Input.Update(msg)
				return cmd
			}
		}
	}
	return nil
}

func unresolvedExecutionWarning(states []execution.State, goalOwned bool) string {
	for _, state := range states {
		toolName := state.Identity.ToolName
		if toolName == "" {
			toolName = "unknown tool"
		}
		switch {
		case state.Latest.Type == execution.EventOutcomeUnknown:
			return fmt.Sprintf(
				"Recovery blocked: %s has a durable outcome-unknown receipt. Use /recover to inspect and record exact evidence; automatic continuation is disabled.",
				toolName,
			)
		case state.Latest.Type == execution.EventStarted && state.Identity.EffectClass != execution.EffectReadOnly:
			return fmt.Sprintf(
				"Recovery blocked: %s has a durable dispatch marker but no terminal receipt. Its outcome is unknown; use /recover to inspect and record exact evidence.",
				toolName,
			)
		case (state.Latest.Type == execution.EventCompleted || state.Latest.Type == execution.EventFailed) &&
			state.Identity.EffectClass != execution.EffectReadOnly:
			if goalOwned {
				return fmt.Sprintf(
					"Recovery blocked: %s %s after the last saved transcript. Continuation is disabled so the effect cannot be repeated; use the goal recovery inspector to reconcile this goal-owned session.",
					toolName, state.Latest.Type,
				)
			}
			return fmt.Sprintf(
				"Recovery blocked: %s %s after the last saved transcript. Continuation is disabled so the effect cannot be repeated; inspect the workspace, then close this session and run `sonar session repair %d`.",
				toolName, state.Latest.Type, state.Identity.SessionID,
			)
		}
	}
	return ""
}

func standaloneRecoveryTarget(states []execution.State, snapshotCursor int64, sessionPublicID string) *agent.UnresolvedExecutionError {
	for _, state := range states {
		if state.Latest.Type != execution.EventOutcomeUnknown &&
			(state.Latest.Type != execution.EventStarted || state.Identity.EffectClass == execution.EffectReadOnly) {
			continue
		}
		return &agent.UnresolvedExecutionError{
			SessionID: state.Identity.SessionID, SessionPublicID: sessionPublicID,
			WorkspaceID:    state.Identity.WorkspaceID,
			SnapshotCursor: snapshotCursor, TurnID: state.Identity.TurnID,
			ExecutionID: state.Identity.ExecutionID, ToolName: state.Identity.ToolName,
			EventType: state.Latest.Type,
			Cause:     errors.New("durable execution outcome requires explicit reconciliation"),
		}
	}
	return nil
}

func agentTextareaStyles(isDark bool, themeID string) textarea.Styles {
	return agentTextareaStylesForMode(isDark, themeID, ModeNormal)
}

// agentTextareaStylesForMode keeps the composer's focus treatment semantic:
// NORMAL is a quiet neutral rail, PLAN uses Nord purple, and AUTO uses the
// success green already shared by completed work. outputSemanticPalette also
// preserves NO_COLOR behavior.
func agentTextareaStylesForMode(isDark bool, themeID string, mode Mode) textarea.Styles {
	styles := textarea.DefaultStyles(isDark)
	palette := outputSemanticPalette(isDark, themeID)
	// Dim is the quiet neutral token that still meets text contrast. Border is
	// deliberately softer and made the NORMAL send marker hard to read on light
	// terminals.
	promptColor := palette.Dim
	cursorColor := palette.Accent
	switch mode {
	case ModePlan:
		promptColor = palette.Special
		cursorColor = palette.Special
	case ModeAuto:
		promptColor = palette.Success
		cursorColor = palette.Success
	}
	styles.Focused = textarea.StyleState{
		Base:        lipgloss.NewStyle(),
		Text:        lipgloss.NewStyle().Foreground(palette.Text),
		CursorLine:  lipgloss.NewStyle(),
		Placeholder: lipgloss.NewStyle().Foreground(palette.Dim),
		Prompt:      lipgloss.NewStyle().Foreground(promptColor).Bold(mode != ModeNormal),
	}
	styles.Blurred = textarea.StyleState{
		Base:        lipgloss.NewStyle(),
		Text:        lipgloss.NewStyle().Foreground(palette.Muted),
		CursorLine:  lipgloss.NewStyle(),
		Placeholder: lipgloss.NewStyle().Foreground(palette.Dim),
		Prompt:      lipgloss.NewStyle().Foreground(palette.Dim),
	}
	styles.Cursor.Color = cursorColor
	return styles
}

func configureComposerMode(input *textarea.Model, isDark bool, themeID string, mode Mode, reducedMotion ...bool) {
	staticCursor := len(reducedMotion) > 0 && reducedMotion[0]
	configureComposerModeWithGlyphProfile(input, isDark, themeID, mode, staticCursor, GlyphUnicode)
}

func configureComposerModeWithGlyphProfile(
	input *textarea.Model,
	isDark bool,
	themeID string,
	mode Mode,
	reducedMotion bool,
	profile GlyphProfile,
) {
	styles := agentTextareaStylesForMode(isDark, themeID, mode)
	styles.Cursor.Blink = !reducedMotion
	input.SetStyles(styles)
	glyphs := glyphSet(resolveGlyphProfile(profile))
	input.SetPromptFunc(3, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			if profile == GlyphASCII {
				return glyphs.UserRail + "> "
			}
			if mode == ModeNormal {
				return "▏❯ "
			}
			return glyphs.UserRail + "❯ "
		}
		return " " + glyphs.Vertical + " "
	})
}

// resetTurnDiagnostics clears presentation derived from the previous turn.
// These values are never part of a saved session, so carrying them across a
// new conversation or a session restore would mislabel the active transcript.
func (m *Model) resetTurnDiagnostics() {
	m.lastTurnDuration = 0
	m.footerNotice = nil
	m.evalCount = 0
	m.promptTokens = 0
	m.turnEvalTotal = 0
	m.turnPromptTotal = 0
	m.capabilityRoute = nil
	m.lastCapabilityRoute = nil
	m.clearContinuationAction()
	m.lastTurnToolIndex = -1
	m.receiptInspectActive = false
	m.receiptInspectToolIndex = -1
}

// flushStream moves accumulated stream text into a chat entry with cached rendering.
func (m *Model) flushStream() {
	content := sanitizeTerminalMultiline(m.streamBuf.String())
	thinking := strings.Trim(sanitizeTerminalMultiline(m.thinkBuf.String()), "\r\n")
	hasSettledContent := strings.TrimSpace(content) != "" ||
		strings.TrimSpace(thinking) != ""
	var settledBlockID BlockID
	var settledTurnID TurnID
	if hasSettledContent && m.liveTailLayoutVisible {
		// Preserve the transient block identity across raw-stream → settled
		// Markdown projection. Semantic reflow can then keep a paused reader on
		// the same logical marker even when Glamour changes the row geometry.
		settledBlockID, settledTurnID = m.liveTailLayoutIdentity()
	}
	if hasSettledContent {
		// The paint cache ends immediately before the live tail, so promoting
		// that tail to one settled append does not invalidate any stable block or
		// reconciliation identity. The reference renderer still owns a monolithic
		// string cache and is invalidated independently.
		m.invalidateLegacyEntryRenderCache()
		var rendered string
		if m.md != nil && strings.TrimSpace(content) != "" {
			rendered = m.md.RenderFull(content)
		}
		entry := ChatEntry{
			BlockID:         settledBlockID,
			TurnID:          settledTurnID,
			Revision:        1,
			Lifecycle:       BlockSettled,
			Kind:            "assistant",
			Content:         content,
			RenderedContent: rendered,
		}
		// Attach thinking content if present.
		if strings.TrimSpace(thinking) != "" {
			entry.ThinkingContent = thinking
			entry.ThinkingCollapsed = true
		}
		m.entries = append(m.entries, entry)
	}
	// A flush is also a semantic segment boundary (tool call or completed turn).
	// Clear whitespace-only buffers and partial tag search state even when there
	// was nothing worth presenting, otherwise a later segment can inherit a
	// phantom live/assistant block.
	m.resetTranscriptStreamText()
	m.resetTranscriptThinkingText()
	m.inThinking = false
	m.thinkSearchBuf = ""
}

// invalidateRenderedCache clears cached renders (e.g. on terminal resize).
func (m *Model) invalidateRenderedCache() {
	for i := range m.entries {
		if m.entries[i].Kind == "assistant" && m.entries[i].RenderedContent != "" {
			if m.md != nil {
				m.entries[i].RenderedContent = m.md.RenderFull(sanitizeTerminalMultiline(m.entries[i].Content))
			}
		}
	}
	m.invalidateEntryCache()
}

// syncInputHeight mirrors Bubbles' visual-row-aware dynamic height into the
// parent layout and recalculates the transcript allocation when it changes.
func (m *Model) syncInputHeight() {
	lines := max(1, m.input.Height())
	if lines != m.inputLines {
		m.inputLines = lines
		m.recalcViewportHeight()
	}
}

const maxComposerVisibleRows = 8

// composerVisibleRowLimit lets roomy terminals show more of a draft while
// preserving the established five-row minimum-terminal contract.
func composerVisibleRowLimit(terminalHeight int) int {
	if terminalHeight <= 0 {
		return maxComposerVisibleRows
	}
	return min(maxComposerVisibleRows, max(5, terminalHeight/3))
}

// reflowInputViewport lets Bubbles populate its internal viewport with content
// inserted directly by the parent before it repositions around the preserved
// cursor. Without this no-op child update, a large accepted paste can clamp its
// five-row viewport against stale pre-paste content and hide the closing rows.
func (m *Model) reflowInputViewport() tea.Cmd {
	hadOverflowCue := m.renderComposerOverflowCue() != ""
	wasFocused := m.input.Focused()
	if !wasFocused {
		// Textarea.Update intentionally ignores messages while blurred. Parent-
		// owned completion and modal flows still need the same content/viewport
		// reconciliation before focus returns, so focus only for this synchronous
		// no-op update and restore the original presentation state immediately.
		_ = m.input.Focus()
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(inputViewportReflowMsg{})
	if !wasFocused {
		m.input.Blur()
	}
	m.syncInputHeight()
	if hadOverflowCue != (m.renderComposerOverflowCue() != "") {
		m.recalcViewportHeight()
	}
	return cmd
}

type inputViewportReflowMsg struct{}

// invalidateEntryCache marks the incremental entry render cache as stale,
// forcing a full re-render on the next renderEntries() call.
func (m *Model) invalidateEntryCache() {
	m.invalidateLegacyEntryRenderCache()
	m.transcriptPaint.cache.valid = false
	m.transcriptPaint.liveCache.valid = false
	m.transcriptReconcileValid = false
	m.transcriptReconciledCount = 0
	m.transcriptReconciledTurnID = ""
	m.transcriptReconciledBlockIDs = nil
}

// invalidateLegacyEntryRenderCache clears only renderEntries' complete-string
// snapshot. The virtualized paint cache and transcript reconciliation have
// stricter append-aware contracts and may remain valid when a live tail is
// promoted to a settled entry.
func (m *Model) invalidateLegacyEntryRenderCache() {
	m.entryCacheValid = false
	m.cachedEntriesRender = ""
	m.cachedEntryCount = 0
	m.cachedStableCount = 0
	m.cachedPrefixLayoutCount = 0
	m.cachedPrefixState = entryRenderState{}
	m.cachedToolHitRegions = nil
	m.cachedThinkingHitRegions = nil
}

// resetEntryMemo drops every per-entry memoized chunk. It runs only when the
// entries slice is replaced wholesale (new conversation, import, session
// restore) or shrinks; ordinary invalidation keeps the memo because each key
// self-validates against the entry it was rendered from.
func (m *Model) resetEntryMemo() {
	clear(m.entryMemo)
	// Every caller replaces, clears, or rejects the semantic entry set. Keep
	// the render-prefix and reconciliation caches under the same ownership
	// boundary so a same-length session restore cannot reuse an old snapshot.
	m.invalidateEntryCache()
}

// checkAutoScroll resets scroll anchor when the viewport is at the bottom,
// allowing auto-scroll to resume during streaming.
func (m *Model) checkAutoScroll() {
	if m.transcriptAtBottom() {
		m.markFollowingLatest()
	}
}

// recalcViewportHeight updates the viewport height based on current footer size.
func (m *Model) recalcViewportHeight() {
	if !m.ready || m.height == 0 {
		return
	}
	anchor := m.captureTranscriptReflowAnchor()
	oldHeight := m.viewport.Height()
	newHeight := m.viewportHeight()
	m.viewport.SetHeight(newHeight)
	if oldHeight != newHeight && m.transcriptGeometryDependsOnHeight(oldHeight, newHeight) {
		// The welcome block is vertically centered, and an expanded inline diff
		// is explicitly budgeted from transcript rows. Footer/composer/inline
		// form reflow therefore needs the same semantic repaint contract as a
		// terminal-height resize when either projection is visible.
		m.invalidateEntryCache()
		m.refreshTranscript()
	}
	// Footer reflow must not silently change transcript ownership. Following
	// stays pinned to the newest row; a paused reader keeps the same semantic
	// block coordinate and screen row when the document geometry permits it.
	m.restoreTranscriptReflowAnchor(anchor)
}

func (m *Model) transcriptGeometryDependsOnHeight(oldHeight, newHeight int) bool {
	if !m.transcriptVirtualized() {
		return false
	}
	if !m.transcriptHasConversation() && !m.hasVisibleLiveTurn() {
		return true
	}
	if inlineDiffPreviewRowsForHeight(oldHeight) == inlineDiffPreviewRowsForHeight(newHeight) {
		return false
	}
	for _, entry := range m.entries {
		if entry.Kind != "tool_group" ||
			entry.ToolIndex < 0 ||
			entry.ToolIndex >= len(m.toolEntries) {
			continue
		}
		tool := m.toolEntries[entry.ToolIndex]
		if !tool.Collapsed && tool.Status != ToolStatusRunning && len(tool.DiffLines) > 0 {
			return true
		}
	}
	return false
}

// viewportHeight is the single vertical-layout authority shared by terminal
// resize and multiline-composer reflow. The one extra row accounts for the
// newline separating the viewport from the footer.
func (m *Model) viewportHeight() int {
	return max(1, m.projectFrame().Transcript.Rect.Height())
}

// isCompletionActive returns true when the completion modal is open.
func (m *Model) isCompletionActive() bool {
	return m.completionState != nil
}

// newCompletionState creates a CompletionState with the filter textinput initialized.
func newCompletionState(kind string, items []Completion, multiSelect bool, themeID string, presentation ...bool) *CompletionState {
	isDark := true
	reducedMotion := false
	if len(presentation) > 0 {
		isDark = presentation[0]
	}
	if len(presentation) > 1 {
		reducedMotion = presentation[1]
	}
	ti := textinput.New()
	ti.SetStyles(semanticTextInputStyles(isDark, themeID, reducedMotion))
	ti.Placeholder = "type to narrow"
	ti.Prompt = ""
	ti.Focus()
	ti.CharLimit = 128

	var sel map[int]bool
	if multiSelect {
		sel = make(map[int]bool)
	}

	return &CompletionState{
		Kind:          kind,
		Filter:        ti,
		BaseItems:     append([]Completion(nil), items...),
		AllItems:      items,
		FilteredItems: items,
		Index:         0,
		Selected:      sel,
	}
}

func (m *Model) triggerCompletion(input string) tea.Cmd {
	if m.completer == nil {
		return nil
	}
	cursorRune := utf8.RuneCountInString(input)
	if input == m.input.Value() {
		cursorRune = textareaCursorRuneOffset(input, m.input.Line(), m.input.Column())
	}
	token, ok := completionTokenAtCursor(input, cursorRune)
	if !ok {
		return nil
	}

	source := token.Source
	if source == "" {
		source = string(token.Anchor.Trigger)
	}
	baseItems := m.completer.CompleteStatic(source)
	filteredItems := FilterCompletions(baseItems, token.Query)
	if token.CommandPrefix != "" {
		filteredItems = append([]Completion(nil), baseItems...)
	}
	if token.Kind != "attachments" && len(filteredItems) == 0 {
		return nil
	}

	anchor := m.captureCompletionTranscriptAnchor()
	m.completionGeneration++
	m.completionState = newCompletionState(token.Kind, baseItems, token.Kind != "command", m.themeID, m.isDark, m.reducedMotion)
	m.completionState.Anchor = token.Anchor
	m.completionState.CommandPrefix = token.CommandPrefix
	m.completionState.Generation = m.completionGeneration
	m.completionState.Filter.SetValue(token.Query)
	m.completionState.Filter.CursorEnd()
	m.completionState.FilteredItems = filteredItems
	m.completionState.Filter.SetWidth(completionFilterInputWidth(m.width))
	m.completionSuppressedDraft = ""
	m.overlay = OverlayCompletion
	m.input.Blur()
	m.recalcViewportHeight()
	m.restoreCompletionTranscriptAnchor(anchor)

	previewCmd := m.refreshCompletionPreview()
	if token.Kind == "attachments" {
		return tea.Batch(previewCmd, m.scheduleCompletionSearch(token.Query, "", false))
	}
	return previewCmd
}

func (m *Model) acceptCompletion() {
	cs := m.completionState
	if cs == nil {
		return
	}
	anchorSnapshot := m.captureCompletionTranscriptAnchor()
	anchor := normalizedCompletionAnchor(cs, m.input.Value())
	insertion := completionInsertion(cs, completionAnchorSuffixStartsWithSpace(anchor))
	if insertion == "" {
		return
	}
	draft, cursorRune := replaceCompletionAnchor(anchor, insertion)
	m.setComposerDraftAtRune(draft, cursorRune)
	m.closeCompletion()
	m.restoreCompletionTranscriptAnchor(anchorSnapshot)
}

func (m *Model) toggleCompletionSelection() {
	cs := m.completionState
	if cs == nil || cs.Selected == nil || len(cs.FilteredItems) == 0 {
		return
	}

	// Map the filtered index back to AllItems index
	filteredItem := cs.FilteredItems[cs.Index]
	for i, item := range cs.AllItems {
		if item.Label == filteredItem.Label && item.Insert == filteredItem.Insert {
			if cs.Selected[i] {
				delete(cs.Selected, i)
			} else {
				cs.Selected[i] = true
			}
			break
		}
	}
}

// drillIntoFolder navigates into a subfolder in the @ completion modal.
func (m *Model) drillIntoFolder() tea.Cmd {
	cs := m.completionState
	if cs == nil || cs.Index >= len(cs.FilteredItems) {
		return nil
	}

	anchor := m.captureCompletionTranscriptAnchor()
	item := cs.FilteredItems[cs.Index]
	cs.CurrentPath = strings.Trim(strings.TrimPrefix(completionItemPath(item), "@"), "/")
	cs.BaseItems = nil
	cs.Filter.SetValue("")
	replaceCompletionItems(cs, nil)
	cs.Index = 0
	m.recalcViewportHeight()
	m.restoreCompletionTranscriptAnchor(anchor)
	return tea.Batch(m.refreshCompletionPreview(), m.scheduleCompletionSearch("", cs.CurrentPath, false))
}

// drillUpFolder navigates to the parent folder in the @ completion modal.
func (m *Model) drillUpFolder() tea.Cmd {
	cs := m.completionState
	if cs == nil || cs.CurrentPath == "" {
		return nil
	}

	anchor := m.captureCompletionTranscriptAnchor()
	// Pop last segment
	if idx := strings.LastIndex(cs.CurrentPath, "/"); idx >= 0 {
		cs.CurrentPath = cs.CurrentPath[:idx]
	} else {
		cs.CurrentPath = ""
	}

	if cs.CurrentPath == "" {
		cs.BaseItems = m.completer.CompleteStatic("@")
	} else {
		cs.BaseItems = nil
	}
	cs.Filter.SetValue("")
	replaceCompletionItems(cs, append([]Completion(nil), cs.BaseItems...))
	cs.Index = 0
	m.recalcViewportHeight()
	m.restoreCompletionTranscriptAnchor(anchor)
	return tea.Batch(m.refreshCompletionPreview(), m.scheduleCompletionSearch("", cs.CurrentPath, false))
}

func (m *Model) closeCompletion() {
	anchor := m.captureCompletionTranscriptAnchor()
	if m.completionState != nil {
		if m.completionState.SearchCancel != nil {
			m.completionState.SearchCancel()
		}
		if m.completionState.PreviewCancel != nil {
			m.completionState.PreviewCancel()
		}
	}
	m.completionGeneration++
	m.completionState = nil
	m.clearCompletionSuppression()
	m.overlay = OverlayNone
	if m.composerEditable() {
		m.input.Focus()
	}
	m.recalcViewportHeight()
	m.restoreCompletionTranscriptAnchor(anchor)
}

func (m *Model) clearCompletionSuppression() {
	m.completionSuppressedDraft = ""
}

// dismissCompletion returns every character typed through the completion
// filter to the composer. Automatic discovery remains dismissed only while
// that exact draft is unchanged; editing it or pressing Tab can reopen the
// completion surface.
func (m *Model) dismissCompletion() {
	anchorSnapshot := m.captureCompletionTranscriptAnchor()
	draft, cursorRune := m.completionDraftAndCursor()
	m.closeCompletion()
	m.setComposerDraftAtRune(draft, cursorRune)
	m.completionSuppressedDraft = draft
	m.restoreCompletionTranscriptAnchor(anchorSnapshot)
}

func (m *Model) completionDraftAndCursor() (string, int) {
	cs := m.completionState
	if cs == nil {
		draft := m.input.Value()
		return draft, textareaCursorRuneOffset(draft, m.input.Line(), m.input.Column())
	}

	anchor := normalizedCompletionAnchor(cs, m.input.Value())
	prefix := string(anchor.Trigger)
	if cs.CommandPrefix != "" {
		prefix = cs.CommandPrefix
	}
	query := prefix + cs.Filter.Value()
	queryCursorRune := utf8.RuneCountInString(prefix) + cs.Filter.Position()
	switch cs.Kind {
	case "attachments":
		if cs.CurrentPath != "" {
			prefix := "@" + strings.Trim(cs.CurrentPath, "/") + "/"
			query = prefix + cs.Filter.Value()
			queryCursorRune = utf8.RuneCountInString(prefix) + cs.Filter.Position()
		}
	}
	return replaceCompletionAnchorAt(anchor, query, queryCursorRune)
}

// lastAssistantContent scans entries backwards for the last assistant message.
// While streaming, prefer the live buffer so Ctrl+Y can grab in-flight text.
func (m *Model) lastAssistantContent() string {
	if m != nil && (m.state == StateStreaming || m.state == StateWaiting) {
		if live := strings.TrimSpace(m.streamBuf.String()); live != "" {
			return live
		}
	}
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].Kind == "assistant" && strings.TrimSpace(m.entries[i].Content) != "" {
			return m.entries[i].Content
		}
	}
	return ""
}

type clipboardResultMsg struct {
	Err error
}

// copyToClipboard copies text to the system clipboard. Its receipt is a
// transient footer notice; copy affordances must never mutate or persist chat.
func (m *Model) copyToClipboard(text string) tea.Cmd {
	write := m.clipboardWrite
	return func() tea.Msg {
		if write == nil {
			return clipboardResultMsg{Err: context.Canceled}
		}
		return clipboardResultMsg{Err: write(text)}
	}
}

// readClipboardPaste keeps explicit Ctrl+V on the same inspected path as a
// terminal bracketed-paste event. The textarea's private clipboard command is
// disabled at construction because its internal message type would otherwise
// insert directly and bypass the parent paste receipt.
func (m *Model) readClipboardPaste() tea.Cmd {
	read := m.clipboardRead
	readImage := m.clipboardImageRead
	return func() tea.Msg {
		var content string
		var textErr error
		if read != nil {
			content, textErr = read()
		}
		if strings.TrimSpace(content) != "" {
			return tea.PasteMsg{Content: content}
		}
		if readImage != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			name, data, err := readImage(ctx)
			if err == nil && len(data) > 0 {
				return ClipboardImagePasteMsg{Name: name, Data: data}
			}
		}
		if textErr != nil {
			return SystemMessageMsg{Msg: "Clipboard text is unavailable and no supported image was found."}
		}
		return SystemMessageMsg{Msg: "Clipboard is empty or has no supported text, PNG, JPEG, or GIF image."}
	}
}

// handleMouseClick hit-tests completed transcript disclosures. Live reasoning
// intentionally has no region and remains non-interactive until it settles.
func (m *Model) handleMouseClick(x, y int) tea.Cmd {
	if x < 0 || x >= m.viewport.Width() || y < 0 || y >= m.viewport.Height() {
		return nil
	}
	// The viewport starts at terminal row zero in the sidebar-free layout.
	vpY := y + m.transcriptYOffset()
	for _, region := range m.toolHitRegions {
		if region.contains(x, vpY) {
			if region.ToolIndex >= 0 && region.ToolIndex < len(m.toolEntries) {
				if target, ok := m.toolActionTarget(region.ToolIndex); ok {
					return m.dispatchUIAction(UIActionRequest{
						ActionID: toolToggleActionID,
						Target:   target,
						Source:   UIActionSourceMouse,
					})
				}
				// Geometry-only fixtures and a pre-reconciliation startup frame
				// may not yet have a canonical BlockID. Production transcript
				// actions always take the typed path above; preserve the legacy
				// disclosure locally until identity is available.
				m.toggleToolReceipt(region.ToolIndex, true)
			}
			return nil
		}
	}
	for _, region := range m.thinkingHitRegions {
		if !region.contains(x, vpY) {
			continue
		}
		if region.EntryIndex < 0 || region.EntryIndex >= len(m.entries) {
			return nil
		}
		entry := m.entries[region.EntryIndex]
		if entry.Kind != "assistant" || strings.TrimSpace(entry.ThinkingContent) == "" ||
			reasoningReceiptDigest(entry.ThinkingContent) != region.Digest {
			return nil
		}
		m.toggleThinkingReceipt(region.EntryIndex)
		return nil
	}
	return nil
}

func (m *Model) toggleThinkingReceipt(entryIndex int) bool {
	if entryIndex < 0 || entryIndex >= len(m.entries) {
		return false
	}
	entry := &m.entries[entryIndex]
	if entry.Kind != "assistant" || strings.TrimSpace(entry.ThinkingContent) == "" {
		return false
	}
	anchor := m.captureTranscriptReflowAnchor()
	entry.ThinkingCollapsed = !entry.ThinkingCollapsed
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.restoreTranscriptReflowAnchor(anchor)
	return true
}

func (m *Model) toggleAllThinkingReceipts() bool {
	targetCollapsed := false
	found := false
	for i := len(m.entries) - 1; i >= 0; i-- {
		entry := m.entries[i]
		if entry.Kind == "assistant" && strings.TrimSpace(entry.ThinkingContent) != "" {
			targetCollapsed = !entry.ThinkingCollapsed
			found = true
			break
		}
	}
	if !found {
		return false
	}
	anchor := m.captureTranscriptReflowAnchor()
	for i := range m.entries {
		if m.entries[i].Kind == "assistant" && strings.TrimSpace(m.entries[i].ThinkingContent) != "" {
			m.entries[i].ThinkingCollapsed = targetCollapsed
		}
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.restoreTranscriptReflowAnchor(anchor)
	return true
}

// toggleToolReceipt keeps the disclosure and transcript anchor as one parent-
// owned interaction. Keyboard inspection reveals the exact ToolCard header;
// hiding it restores the user's previous follow intent and offset.
func (m *Model) toggleToolReceipt(toolIndex int, reveal bool) {
	if toolIndex < 0 || toolIndex >= len(m.toolEntries) {
		return
	}
	if m.receiptInspectActive && m.receiptInspectToolIndex != toolIndex {
		m.cancelReceiptInspection(false)
	}
	anchor := m.captureTranscriptReflowAnchor()
	expanding := m.toolEntries[toolIndex].Collapsed
	restoreInspectionAnchor := !expanding && m.receiptInspectActive && m.receiptInspectToolIndex == toolIndex
	if expanding && reveal {
		m.receiptInspectActive = true
		m.receiptInspectToolIndex = toolIndex
		m.receiptInspectAnchorPaused = m.followPaused()
		m.receiptInspectAnchorYOffset = m.transcriptYOffset()
		m.receiptInspectReflowAnchor = anchor
	}
	m.toolEntries[toolIndex].Collapsed = !m.toolEntries[toolIndex].Collapsed
	m.invalidateEntryCache()
	m.refreshTranscript()
	if expanding && reveal {
		m.revealToolReceipt(toolIndex)
		return
	}
	if restoreInspectionAnchor {
		m.restoreTranscriptReflowAnchor(m.receiptInspectReflowAnchor)
		m.receiptInspectActive = false
		m.receiptInspectToolIndex = -1
		m.receiptInspectReflowAnchor = transcriptReflowAnchor{}
		return
	}
	m.restoreTranscriptReflowAnchor(anchor)
}

func (m *Model) cancelReceiptInspection(collapse bool) {
	if !m.receiptInspectActive {
		return
	}
	toolIndex := m.receiptInspectToolIndex
	if collapse && toolIndex >= 0 && toolIndex < len(m.toolEntries) && !m.toolEntries[toolIndex].Collapsed {
		m.toolEntries[toolIndex].Collapsed = true
		m.invalidateEntryCache()
		m.refreshTranscript()
	}
	if m.receiptInspectReflowAnchor.Valid {
		m.restoreTranscriptReflowAnchor(m.receiptInspectReflowAnchor)
	} else {
		m.restoreFollowPosition(m.receiptInspectAnchorPaused, m.receiptInspectAnchorYOffset)
	}
	m.receiptInspectActive = false
	m.receiptInspectToolIndex = -1
	m.receiptInspectReflowAnchor = transcriptReflowAnchor{}
}

func (m *Model) revealToolReceipt(toolIndex int) {
	for _, region := range m.toolHitRegions {
		if region.ToolIndex == toolIndex {
			m.setTranscriptYOffset(region.Row)
			m.pauseFollow()
			return
		}
	}
}

func approvalDetail(toolName string, args map[string]any) (string, bool) {
	encoded, err := json.MarshalIndent(args, "", "  ")
	if err != nil {
		return fmt.Sprintf("Refused `%s`: its exact arguments could not be encoded for inspection.", toolName), false
	}
	return fmt.Sprintf("Permission required for `%s`:\n```json\n%s\n```", toolName, encoded), true
}

// Ready returns true if the TUI is fully initialized.
// Exported for testing.
func (m *Model) Ready() bool {
	return m.ready
}

// AnchorActive returns true if scroll anchor is active.
// Exported for testing.
func (m *Model) AnchorActive() bool {
	return m.anchorActive
}
