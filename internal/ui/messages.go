package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// Message types for the BubbleTea update loop.

// StreamTextMsg delivers incremental text from the LLM.
type StreamTextMsg struct {
	Text string
}

// StreamThinkingMsg carries provider-native reasoning separately from answer text.
type StreamThinkingMsg struct{ Text string }

// StreamDoneMsg signals the LLM has finished responding.
type StreamDoneMsg struct {
	EvalCount    int
	PromptTokens int
}

// interruptionStatsLoadedMsg delivers loaded interruption stats for the permissions panel.
type interruptionStatsLoadedMsg struct {
	Stats []interruptionStat
}

// ToolCallStartMsg signals a tool invocation has begun.
type ToolCallStartMsg struct {
	ID                      string
	Name                    string
	Args                    map[string]any
	StartTime               time.Time
	BeforeContent           string
	BeforeSnapshotAvailable bool
}

// ToolCallResultMsg delivers the result of a tool call.
type ToolCallResultMsg struct {
	ID           string
	Name         string
	Result       string
	IsError      bool
	Duration     time.Duration
	Projection   ecosystem.ToolProjection
	OutputDetail OutputDetailReceipt
}

// ErrorMsg reports an error.
type ErrorMsg struct {
	Msg string
}

// SystemMessageMsg displays a system-level message.
type SystemMessageMsg struct {
	Msg string
}

// ClipboardImagePasteMsg carries one explicitly requested clipboard image only
// until the parent can admit it to the private content-addressed store. Raw
// bytes never enter Model state, transcript entries, or session persistence.
type ClipboardImagePasteMsg struct {
	Name string
	Data []byte
	Err  error
}

// CapabilityRouteMsg is an ephemeral host advisory. It must never be appended
// to ChatEntry, ToolEntry, session state, or evidence receipts.
type CapabilityRouteMsg struct {
	Route agent.CapabilityRoute
}

// ContinuationActionPresentation is the UI-facing projection of one validated
// continuation suggestion. It deliberately excludes arguments, command text,
// workspace references, and arbitrary downstream prose. The agent adapter may
// populate it only after the exact continuation interpreter has accepted the
// source contract.
type ContinuationActionPresentation struct {
	Tool       string
	Inputs     []string
	BlockedBy  []string
	ReasonCode string
}

// ContinuationActionMsg atomically replaces the ephemeral suggestion for one
// active turn. Sequence is monotonic within TurnID; a nil Action is an
// authoritative clear. Neither the message nor its presentation is persisted.
type ContinuationActionMsg struct {
	TurnID   string
	Sequence uint64
	Action   *ContinuationActionPresentation
}

// BobWorkspaceContextMsg replaces the ephemeral bounded Bob workspace status.
// Generation is monotonic for one Agent lifetime; a nil digest is an
// authoritative clear. Raw Bob output never crosses into Bubble Tea state.
type BobWorkspaceContextMsg struct {
	Generation uint64
	Digest     *ecosystem.ReceiptDigest
}

// ContextCompactedMsg invalidates the previous provider occupancy snapshot.
// The retained history is smaller, but its next exact prompt size is not known
// until Ollama reports the following request.
type ContextCompactedMsg struct{}

// ContextCompactionStartedMsg and ContextCompactionFinishedMsg expose the
// hidden summarization request as one explicit UI phase. They carry no model
// text or transcript data.
type ContextCompactionStartedMsg struct{}
type ContextCompactionFinishedMsg struct{}

// AgentDoneMsg signals the agent loop has completed.
type AgentDoneMsg struct {
	// TurnID is the logical user/Goal turn identity. SegmentTurnID is the
	// execution-ledger identity used by the just-finished AUTO segment. They are
	// equal for ordinary turns; a productive AUTO checkpoint keeps TurnID stable
	// while continuing under a fresh SegmentTurnID.
	TurnID        string
	SegmentTurnID string
	Err           error
}

// ShutdownMsg requests a graceful stop. Active turns are cancelled and joined
// before BubbleTea exits so dispatched effects receive a final receipt.
type ShutdownMsg struct{}

// FailedServer records an MCP server that failed to connect.
type FailedServer struct {
	Name   string
	Reason string
}

// MCPServerStatus is a bounded presentation snapshot. Detail is transient UI
// context only and is sanitized again when it enters Model state; it must never
// be copied into persisted transcript or session state.
type MCPServerStatus struct {
	Name      string
	Connected bool
	ToolCount int
	Detail    string
}

// MCPStatusSnapshotMsg replaces the previous cached MCP presentation state.
// The registry health monitor emits it after connection-state transitions.
type MCPStatusSnapshotMsg struct {
	Servers []MCPServerStatus
}

// CoreReadyMsg signals that the host has committed the startup model and
// agent profile, so ordinary turns may run even while optional MCP/ICE work
// continues in the background.
type CoreReadyMsg struct {
	Model                    string
	ModelList                []string
	OllamaModels             []OllamaModelDescriptor
	OllamaInventoryAttempted bool
	AgentProfile             string
	NumCtx                   int
}

// InitCompleteMsg signals startup is done.
type InitCompleteMsg struct {
	Model                    string
	ModelList                []string
	OllamaModels             []OllamaModelDescriptor
	OllamaVersion            string
	OllamaInventoryAttempted bool
	AgentProfile             string
	AgentList                []string
	ToolCount                int
	ServerCount              int
	NumCtx                   int
	FailedServers            []FailedServer
	MCPServers               []MCPServerStatus
	ICEEnabled               bool
	ICEConversations         int
	ICESessionID             string
}

// CommandResultMsg carries the result of a slash command for display.
type CommandResultMsg struct {
	Text string
}

// StartupStatusMsg reports progress of a single startup task (Ollama, MCP server, ICE).
type StartupStatusMsg struct {
	ID     string // unique key: "ollama", "mcp:<name>", "ice"
	Label  string // display name: "Ollama (qwen3:8b)", "docker-gateway"
	Status string // "connecting", "connected", "failed"
	Detail string // e.g. "110 tools", error message
}

// CompletionSearchResultMsg delivers async vecgrep search results.
type CompletionSearchResultMsg struct {
	Generation uint64
	Tag        int
	Results    []Completion
}

// CompletionDebounceTickMsg fires after the debounce interval to trigger a search.
type CompletionDebounceTickMsg struct {
	Generation uint64
	Tag        int
	Query      string
	Path       string
}

// PlanFormCompletedMsg signals the plan form has been submitted with a structured prompt.
type PlanFormCompletedMsg struct {
	Prompt string
}

// SessionListMsg delivers the list of saved SQLite sessions.
type SessionListMsg struct {
	ListToken uint64
	Sessions  []SessionListItem
	Err       error
}

// SessionLoadedMsg delivers a persisted session and its execution lease.
type SessionLoadedMsg struct {
	LoadToken        uint64
	SessionID        int64
	SessionPublicID  string
	State            persistedSessionState
	StateRecord      db.SessionStateRecord
	Title            string
	RecoveryWarning  string
	RecoveryTarget   *agent.UnresolvedExecutionError
	RecoveryContexts []db.StandaloneReconciliationContext
	ExecutionLease   *db.ExecutionSessionLease
	Err              error
}

// ToolApprovalMsg asks the user to approve a tool call.
type ToolApprovalMsg struct {
	RequestID       string
	ToolName        string
	Args            map[string]any
	ArgumentsSHA256 string
	Preview         permission.ApprovalPreview
	Scope           permission.ApprovalScope
	Response        chan<- permission.ApprovalResponse
}

// CommitResultMsg carries the result of an async /commit operation.
type CommitResultMsg struct {
	Token   uint64
	Message string // commit message used
	Err     error
}

// ContextLoadResultMsg completes a bounded asynchronous /load operation.
type ContextLoadResultMsg struct {
	Token uint64
	Path  string
	Data  string
	Err   error
}

// ReadScopeResultMsg completes one process-local external read-root change.
// The Agent remains the authority for canonicalization and overlap checks.
type ReadScopeResultMsg struct {
	Token       uint64
	Operation   string
	Path        string
	Kind        string
	Count       int
	Grants      []agent.ReadGrant
	WriteGrants []agent.WriteGrant
	AutoResume  bool
	RolledBack  int
	RollbackErr error
	Err         error
	// Rollback and Finalize are process-local transaction handles. They never
	// enter transcript/session state; Update consumes exactly one of them.
	Rollback func() (int, error)
	Finalize func()
}

// ReadScopePreviewResultMsg completes canonicalization and read-only boundary
// checks before the user is asked to authorize an external root.
type ReadScopePreviewResultMsg struct {
	Token     uint64
	Requested string
	Canonical string
	Workspace string
	Draft     string
	Grant     agent.ReadGrant
	Err       error
}

// PromptPathPreflightResultMsg carries canonical host projections plus opaque
// preview identities owned by Agent. Missing, workspace-local, non-regular and
// already-authorized candidates are omitted by the background inspector.
type PromptPathPreflightResultMsg struct {
	Token                  uint64
	Draft                  string
	Grants                 []agent.ReadGrant
	WriteGrants            []agent.WriteGrant
	UnavailableWrites      []string
	Authority              Mode
	MoreCandidates         bool
	CandidateLimitExceeded bool
}

// ImportResultMsg completes a bounded asynchronous /import operation. Parsing
// is done off the BubbleTea update loop so a large valid transcript cannot
// freeze rendering.
type ImportResultMsg struct {
	Token          uint64
	Path           string
	Entries        []ChatEntry
	Messages       []llm.Message
	UIOnlySections int
	ToolSections   int
	Err            error
}

// ExportResultMsg reports the outcome of an atomic asynchronous /export.
type ExportResultMsg struct {
	Token uint64
	Path  string
	Err   error
}

// sendMsg is a helper to send a tea.Msg to the program.
func sendMsg(p *tea.Program, msg tea.Msg) {
	if p != nil {
		p.Send(msg)
	}
}

// systemNoticeMsg appends a system or error entry to the transcript from
// an async operation (e.g. MCP reconnect).
type systemNoticeMsg struct {
	Text    string
	IsError bool
}
