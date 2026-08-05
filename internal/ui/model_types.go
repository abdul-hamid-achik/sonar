package ui

import (
	"context"
	"time"

	"charm.land/bubbles/v2/textinput"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
)

// State represents the TUI's two possible states.
type State int

const (
	StateIdle      State = iota // waiting for user input
	StateWaiting                // sent to LLM, waiting for first token
	StateStreaming              // LLM is generating a response
)

// OverlayKind represents what overlay (if any) is currently shown.
type OverlayKind int

const (
	OverlayNone OverlayKind = iota
	OverlayHelp
	OverlayCompletion
	OverlayModelPicker
	OverlayPlanForm
	OverlayCortexDecision
	OverlaySessionsPicker
	OverlaySettings
	OverlayAgentPicker
	OverlayProviderPicker
	OverlayModePicker
	OverlayGoalForm
	OverlayRuntimeStatus
	OverlayGoalInspector
	OverlayGoalRecovery
	OverlayModelDetails
	OverlayModelPull
	OverlayAgents
	OverlayTranscriptSearch
	OverlayPermissions
	OverlayThemePicker
	OverlayContextDoctor
)

// CompletionState holds all state for the composer-owned completion popup.
type CompletionState struct {
	Kind          string          // "command", "attachments", "skills"
	Filter        textinput.Model // inline filter field
	Anchor        completionAnchor
	CommandPrefix string       // exact `/command ` prefix for registry action completion
	BaseItems     []Completion // non-workspace items retained across async searches
	AllItems      []Completion // full unfiltered list
	FilteredItems []Completion // items matching current filter
	Index         int          // cursor in FilteredItems
	Selected      map[int]bool // multi-select (keys = AllItems indices)
	CurrentPath   string       // for @ file browsing: relative dir path
	Searching     bool         // true while vecgrep is in flight
	Generation    uint64       // guards results across close/reopen cycles
	DebounceTag   int          // cancel stale searches
	SearchCancel  context.CancelFunc
	Preview       completionPreview
	PreviewToken  uint64
	PreviewCancel context.CancelFunc
}

// ToolStatus represents the state of a tool execution.
type ToolStatus int

const (
	ToolStatusRunning ToolStatus = iota
	ToolStatusDone
	ToolStatusError
	ToolStatusCancelled
)

const cancelledToolResult = "Cancelled by user before completion"

// ToolEntry tracks the lifecycle of a single tool call.
type ToolEntry struct {
	ID                      string
	Name                    string
	Summary                 string         // bounded semantic context for compact/restored receipts
	Args                    string         // formatted args string
	RawArgs                 map[string]any `json:"-"` // ephemeral original args
	Result                  string
	ResultDisplay           string `json:"-"` // transient raw-ANSI display variant for render-time remap; never persisted or restored
	ResultLanguage          string // bounded lexer alias derived from trusted call metadata
	OutputDetail            OutputDetailReceipt
	IsError                 bool
	Status                  ToolStatus
	StartTime               time.Time
	Duration                time.Duration
	Collapsed               bool                     // per-entry collapse state
	BeforeContent           string                   `json:"-"` // ephemeral snapshot before file write
	BeforeSnapshotAvailable bool                     `json:"-"` // false when the bounded pre-write read was unavailable
	DiffLines               []DiffLine               // computed diff (nil = not a file write)
	DiffPending             bool                     `json:"-"` // post-write read/LCS is running outside Update
	DiffGeneration          uint64                   `json:"-"` // accepts exactly one matching asynchronous result
	Projection              ecosystem.ToolProjection // bounded semantic role, route, and outcome
	ExpertProgress          *ExpertProgressState     // bounded consult_experts lifecycle projection
}

// ChatEntry is a single item in the chat log.
type ChatEntry struct {
	BlockID           BlockID          // stable semantic identity across reflow and restore
	TurnID            TurnID           // causal turn identity; independent of slice position
	Revision          uint64           // semantic/lifecycle revision, never a layout revision
	Lifecycle         BlockLifecycle   // monotonic semantic lifecycle
	Kind              string           // "user", "assistant", "tool_group", "error", "system"
	Content           string           // raw content
	RenderedContent   string           // cached Glamour output (set once on completion)
	Name              string           // tool name for tool entries
	IsError           bool             // for tool_result
	ToolIndex         int              // index into toolEntries for "tool_group" kind
	ThinkingContent   string           // extracted <think> content
	ThinkingCollapsed bool             // default: true
	Attachments       []imageasset.Ref // validated, path-free image metadata
	semanticDigest    [32]byte         // transient detector for semantic revision changes
}

// toolHitRegion is an exact, ordered transcript row target for one ToolCard
// header. Dense receipts can be adjacent, so hit testing must never infer a
// fixed card height or iterate an unordered map.
type toolHitRegion struct {
	ToolIndex int
	Row       int
	StartCol  int
	EndCol    int
}

func (region toolHitRegion) contains(x, y int) bool {
	return NewCellRect(region.StartCol, region.Row, region.EndCol, region.Row+1).Contains(x, y)
}

// thinkingHitRegion identifies one completed assistant reasoning disclosure.
// The digest prevents an old cached row from toggling a replacement entry that
// happens to occupy the same slice index.
type thinkingHitRegion struct {
	EntryIndex int
	Row        int
	StartCol   int
	EndCol     int
	Digest     [32]byte
}

func (region thinkingHitRegion) contains(x, y int) bool {
	return NewCellRect(region.StartCol, region.Row, region.EndCol, region.Row+1).Contains(x, y)
}

// startupItem tracks the progress of a single startup task.
type startupItem struct {
	ID     string
	Label  string
	Status string // "connecting", "connected", "failed"
	Detail string
}
