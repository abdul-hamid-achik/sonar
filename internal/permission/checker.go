package permission

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
)

// Policy represents a tool's permission policy.
type Policy string

const (
	// PolicyAllow is retained only for decoding legacy databases and for the
	// Check return value when approval prompts are explicitly skipped. Broad tool-name grants are not
	// valid under the current exact-request approval contract.
	PolicyAllow Policy = "allow"
	PolicyDeny  Policy = "deny" // always deny
	PolicyAsk   Policy = "ask"  // prompt user each time
)

// ErrBroadAllowUnsupported reports an attempt to create the retired global
// tool-name allow policy. Callers must use an exact-request approval or opt
// explicitly into the process-wide skip-approvals posture instead.
var ErrBroadAllowUnsupported = errors.New("broad tool allow policies are unsupported; use an exact-request approval")

// Checker manages tool permissions with an in-memory cache backed by SQLite.
type Checker struct {
	store         *db.Store
	cache         map[string]Policy
	mu            sync.RWMutex
	skipApprovals bool
}

// NewChecker creates a permission checker. If store is nil, all tools default to "ask".
// If skipApprovals is true, approval prompts are bypassed while host, scope,
// and tool validation remain authoritative.
func NewChecker(store *db.Store, skipApprovals bool) *Checker {
	c := &Checker{
		store:         store,
		cache:         make(map[string]Policy),
		skipApprovals: skipApprovals,
	}
	if store != nil {
		c.loadFromDB()
	}
	return c
}

// Check returns the policy for the given tool.
func (c *Checker) Check(toolName string) Policy {
	c.mu.RLock()
	p, ok := c.cache[toolName]
	c.mu.RUnlock()
	// A configured deny is an authority boundary, not an approval prompt. The
	// process-wide skip posture may remove Ask interactions, but it must never
	// turn an explicit in-memory or persisted denial into permission.
	if ok && p == PolicyDeny {
		return p
	}
	if c.skipApprovals {
		return PolicyAllow
	}
	if ok {
		return p
	}
	return PolicyAsk
}

// SetPolicy updates an ask/deny policy for a tool and persists it. Broad
// allows are rejected because they cannot carry an exact-request scope.
func (c *Checker) SetPolicy(toolName string, policy Policy) error {
	if policy == PolicyAllow {
		return ErrBroadAllowUnsupported
	}
	if policy != PolicyAsk && policy != PolicyDeny {
		return fmt.Errorf("invalid permission policy %q", policy)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.store != nil {
		if _, err := c.store.UpsertToolPermission(context.Background(), db.UpsertToolPermissionParams{
			ToolName: toolName,
			Policy:   string(policy),
		}); err != nil {
			return fmt.Errorf("persist permission for %s: %w", toolName, err)
		}
	}
	c.cache[toolName] = policy
	return nil
}

// SkipsApprovals reports the process-wide approval-prompt posture.
func (c *Checker) SkipsApprovals() bool {
	return c != nil && c.skipApprovals
}

// IsYolo is retained for source compatibility.
// Deprecated: use SkipsApprovals.
func (c *Checker) IsYolo() bool {
	return c.SkipsApprovals()
}

// AllPolicies returns all explicitly set policies.
func (c *Checker) AllPolicies() map[string]Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]Policy, len(c.cache))
	for k, v := range c.cache {
		result[k] = v
	}
	return result
}

// Reset clears all stored permissions.
func (c *Checker) Reset() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.store != nil {
		if err := c.store.ResetToolPermissions(context.Background()); err != nil {
			return fmt.Errorf("reset persisted permissions: %w", err)
		}
	}
	c.cache = make(map[string]Policy)
	return nil
}

func (c *Checker) loadFromDB() {
	perms, err := c.store.ListToolPermissions(context.Background())
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range perms {
		switch Policy(p.Policy) {
		case PolicyDeny, PolicyAsk:
			c.cache[p.ToolName] = Policy(p.Policy)
		case PolicyAllow:
			// Databases from before exact-request approval persisted broad
			// per-tool allows. Never rehydrate that obsolete authority. The
			// corresponding migration rewrites the durable row to "ask".
		}
	}
}

// ApprovalDecision is the host-owned resolution of an interactive approval.
// It deliberately distinguishes a human denial from a host refusal so the
// agent cannot report an inspection/rendering failure as a user decision.
type ApprovalDecision string

const (
	DecisionAllowOnce    ApprovalDecision = "allow_once"
	DecisionAllowSession ApprovalDecision = "allow_session"
	DecisionUserDeny     ApprovalDecision = "user_deny"
	DecisionHostRefuse   ApprovalDecision = "host_refuse"
	DecisionCancelled    ApprovalDecision = "cancelled"
)

func (d ApprovalDecision) Valid() bool {
	switch d {
	case DecisionAllowOnce, DecisionAllowSession, DecisionUserDeny,
		DecisionHostRefuse, DecisionCancelled:
		return true
	default:
		return false
	}
}

// ApprovalPreviewKind lets a host choose a purpose-built renderer without
// reparsing arbitrary tool arguments.
type ApprovalPreviewKind string

const (
	PreviewGeneric    ApprovalPreviewKind = "generic"
	PreviewFileWrite  ApprovalPreviewKind = "file_write"
	PreviewFilePatch  ApprovalPreviewKind = "file_patch"
	PreviewFilesystem ApprovalPreviewKind = "filesystem"
	PreviewCommand    ApprovalPreviewKind = "command"
)

// ApprovalPreview is bounded presentation metadata. The complete arguments
// remain in ApprovalRequest.Args and are cryptographically bound by
// ArgumentsSHA256; hosts may render them in a viewport and must not impose an
// arbitrary whole-request display limit.
type ApprovalPreview struct {
	Kind        ApprovalPreviewKind
	ActionLabel string
	Consequence string
	// Reason is a host-authored, deterministic explanation of why this exact
	// request still needs a human decision (for example, which scoped-shell
	// rule a bash command tripped). It is static policy text, never raw model
	// or MCP content, and must be stable across rebuilds so preview
	// revalidation can compare it safely.
	Reason string
	// CommandPrefix is the bash grant prefix that would actually cure THIS
	// refusal: derived by the host from the segment it refused, not from the
	// command's first non-trivial segment. Empty whenever the host cannot name
	// a curable segment, and hosts then fall back to DeriveBashPrefix — the
	// field narrows an offer, it never creates one. Deterministic and bounded
	// like Reason, so preview revalidation still compares it by value.
	CommandPrefix     string
	Path              string
	SourcePath        string
	DestinationPath   string
	Command           string
	ByteSize          int64
	ArgumentsSHA256   string
	ContentSHA256     string
	ExistingSHA256    string
	Diff              string
	DiffTruncated     bool
	DiffOmittedReason string
}

// ApprovalScope is the maximum authority represented by an AllowSession
// decision. ScopeExactRequest intentionally starts narrower than path or
// command-prefix grants: it binds the grant to workspace, tool and canonical
// arguments while leaving room for audited scope kinds later.
// ScopeSessionTool is a process-local grant for the same workspace+tool for
// the rest of the Agent session (write/edit/mkdir only).
type ApprovalScope struct {
	Kind      string
	Workspace string
	Resource  string
}

const (
	ScopeExactRequest = "exact_request"
	// ScopeSessionTool grants the named tool for the remainder of the Agent
	// session within the same workspace. Resource is empty; argument changes
	// do not re-prompt. It is process-local and never persisted.
	ScopeSessionTool = "session_tool"
	// ScopeSessionPath grants write/edit/mkdir for one canonical path for the
	// rest of the process. Resource is the absolute path. The grant is shared
	// across the write-family tools (write, edit, mkdir).
	ScopeSessionPath = "session_path"
	// ScopeSessionBashPrefix grants bash commands that share a safe prefix for
	// the rest of the process. Resource is the prefix string. Compound shell
	// commands never match. Process-local only.
	ScopeSessionBashPrefix = "session_bash_prefix"
	// ScopeSessionMCPTool grants one exact namespaced MCP tool (any args) for
	// the rest of the process. Resource is empty. Process-local only.
	ScopeSessionMCPTool = "session_mcp_tool"

	// SessionPathFamily is the grant-key tool slot for path-scoped session
	// grants so write, edit, and mkdir share one path approval.
	SessionPathFamily = "write|edit|mkdir"

	// Durable scope kinds carried by the UI for workspace-persisted rules.
	// They are not stored as grant keys; the host saves then remembers the
	// corresponding process-local session scope.
	DurableBashPrefix = "workspace_bash_prefix"
	DurableMCPTool    = "workspace_mcp_tool"
	DurableWritePath  = "workspace_write_path"
)

// KnownSessionScopeKind reports whether kind is a supported AllowSession scope.
func KnownSessionScopeKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "", ScopeExactRequest, ScopeSessionTool, ScopeSessionPath,
		ScopeSessionBashPrefix, ScopeSessionMCPTool:
		return true
	default:
		return false
	}
}

// ApprovalRequest represents a tool requesting user approval.
type ApprovalRequest struct {
	RequestID       string
	ToolName        string
	Args            map[string]any
	ArgumentsSHA256 string
	Preview         ApprovalPreview
	Scope           ApprovalScope
	Response        chan ApprovalResponse
}

// ApprovalResponse represents the user's decision on a tool approval.
type ApprovalResponse struct {
	Decision ApprovalDecision
	Code     string
	Message  string

	// ScopeKind narrows an AllowSession decision. Empty means ScopeExactRequest
	// (the historical default). ScopeSessionTool is only honored for eligible
	// tools by the agent authorization path; it is never persisted globally.
	ScopeKind string

	// Allowed and Always are a temporary source-compatibility bridge for older
	// embedding callbacks. New code must set Decision. Legacy Always maps to a
	// session-scoped exact-request grant and is never persisted globally.
	Allowed bool
	Always  bool
}

func AllowOnce() ApprovalResponse {
	return ApprovalResponse{Decision: DecisionAllowOnce, Allowed: true}
}

func AllowSession() ApprovalResponse {
	return ApprovalResponse{Decision: DecisionAllowSession, Allowed: true, Always: true}
}

// AllowSessionTool returns a session-scoped tool grant response. The agent
// only remembers this wider grant for write/edit/mkdir; other tools fall back
// to an exact-request grant.
func AllowSessionTool() ApprovalResponse {
	return ApprovalResponse{
		Decision:  DecisionAllowSession,
		Allowed:   true,
		Always:    true,
		ScopeKind: ScopeSessionTool,
	}
}

// AllowSessionPath returns a path-scoped session grant for write/edit/mkdir.
func AllowSessionPath() ApprovalResponse {
	return ApprovalResponse{
		Decision:  DecisionAllowSession,
		Allowed:   true,
		Always:    true,
		ScopeKind: ScopeSessionPath,
	}
}

// AllowSessionBashPrefix returns a bash-prefix session grant response.
func AllowSessionBashPrefix() ApprovalResponse {
	return ApprovalResponse{
		Decision:  DecisionAllowSession,
		Allowed:   true,
		Always:    true,
		ScopeKind: ScopeSessionBashPrefix,
	}
}

// AllowSessionMCPTool returns a session grant for one MCP tool (any args).
func AllowSessionMCPTool() ApprovalResponse {
	return ApprovalResponse{
		Decision:  DecisionAllowSession,
		Allowed:   true,
		Always:    true,
		ScopeKind: ScopeSessionMCPTool,
	}
}

func Deny() ApprovalResponse {
	return ApprovalResponse{Decision: DecisionUserDeny}
}

func Refuse(code, message string) ApprovalResponse {
	return ApprovalResponse{Decision: DecisionHostRefuse, Code: strings.TrimSpace(code), Message: strings.TrimSpace(message)}
}

func Cancelled(message string) ApprovalResponse {
	return ApprovalResponse{Decision: DecisionCancelled, Code: "cancelled", Message: strings.TrimSpace(message)}
}

// Normalize resolves the deprecated bool bridge and validates host output.
// Invalid or incomplete responses fail closed as a host refusal, never as a
// user denial.
func (r ApprovalResponse) Normalize() ApprovalResponse {
	if r.Decision == "" {
		switch {
		case r.Allowed && r.Always:
			r.Decision = DecisionAllowSession
		case r.Allowed:
			r.Decision = DecisionAllowOnce
		default:
			// A zero-value legacy response historically meant deny. Preserve
			// that narrow behavior while typed callers use Deny explicitly.
			r.Decision = DecisionUserDeny
		}
	}
	if !r.Decision.Valid() {
		return Refuse("invalid_approval_decision", fmt.Sprintf("approval host returned invalid decision %q", r.Decision))
	}
	switch r.Decision {
	case DecisionAllowOnce:
		r.Allowed, r.Always = true, false
		r.ScopeKind = ""
	case DecisionAllowSession:
		r.Allowed, r.Always = true, true
		// Empty ScopeKind means exact_request. Unknown kinds fail closed.
		switch r.ScopeKind {
		case ScopeSessionTool, ScopeSessionPath, ScopeSessionBashPrefix, ScopeSessionMCPTool:
			// keep
		default:
			r.ScopeKind = ""
		}
	default:
		r.Allowed, r.Always = false, false
		r.ScopeKind = ""
	}
	if r.Decision == DecisionHostRefuse && strings.TrimSpace(r.Code) == "" {
		r.Code = "host_refused"
	}
	return r
}

func (r ApprovalResponse) IsAllowed() bool {
	switch r.Normalize().Decision {
	case DecisionAllowOnce, DecisionAllowSession:
		return true
	default:
		return false
	}
}

// RequestApproval sends an approval request through the callback and blocks for a response.
// Returns (allowed, alwaysAllow).
func RequestApproval(toolName string, args map[string]any, callback func(ApprovalRequest)) (bool, bool) {
	return RequestApprovalContext(context.Background(), toolName, args, callback)
}

// RequestApprovalContext is the cancellable form used by an active agent
// turn. Missing approval UI fails closed: callers must opt into the explicit
// skip-approvals posture or
// provide an explicit response before a risky tool may execute.
func RequestApprovalContext(ctx context.Context, toolName string, args map[string]any, callback func(ApprovalRequest)) (bool, bool) {
	response := ResolveApprovalContext(ctx, ApprovalRequest{ToolName: toolName, Args: args}, callback)
	return response.Allowed, response.Decision == DecisionAllowSession
}

// ResolveApprovalContext is the typed, cancellable approval boundary used by
// the agent runtime. Missing UI is a host refusal and context cancellation is
// a cancellation; neither is mislabeled as a user denial.
func ResolveApprovalContext(ctx context.Context, request ApprovalRequest, callback func(ApprovalRequest)) ApprovalResponse {
	return ResolveApprovalContextWithTimeout(ctx, request, callback, 0)
}

// ResolveApprovalContextWithTimeout is the same boundary with an unattended
// answer.
//
// An AUTO run left alone hits its first approval and stops there. The prompt
// waits on ctx.Done(), so nothing is executed without permission — but nothing
// else happens either, and the turn's whole remaining wall budget is spent
// standing in front of a modal nobody is going to answer. Coming back to a
// cancelled turn that did no work is the outcome that makes unattended AUTO
// useless in practice.
//
// After timeout with no answer the request is refused, not cancelled, and that
// distinction is the entire point. A cancellation ends the turn; a host
// refusal records a terminal event for this one call and lets dispatch carry
// on with the rest, so the model sees the refusal and can take another route.
// Authority is unchanged: the call did not run, and a timeout can only ever
// withhold permission, never grant it.
//
// A model that answers a refusal by re-sending the identical call is already
// bounded — maxIdenticalHostRefusals ends the turn — so this cannot become a
// loop that burns the budget it was added to protect.
//
// A zero or negative timeout waits indefinitely, which is the right default
// for an interactive session where someone is watching.
func ResolveApprovalContextWithTimeout(
	ctx context.Context,
	request ApprovalRequest,
	callback func(ApprovalRequest),
	timeout time.Duration,
) ApprovalResponse {
	if callback == nil {
		return Refuse("approval_ui_unavailable", "interactive approval is unavailable")
	}
	ch := make(chan ApprovalResponse, 1)
	request.Response = ch
	// Dispatch cannot sit in front of the cancellation select: UI adapters or
	// embedding callbacks may block while delivering the prompt. The buffered
	// response channel lets a late answer finish after cancellation as well.
	go callback(request)

	var unanswered <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		unanswered = timer.C
	}
	select {
	case resp := <-ch:
		return resp.Normalize()
	case <-unanswered:
		return Refuse("approval_timeout", fmt.Sprintf(
			"no approval decision within %s, so the request was refused and the run continued. "+
				"Do not retry it unchanged; take an approach that does not need this permission, "+
				"or leave it for a human to authorize.", timeout))
	case <-ctx.Done():
		return Cancelled(ctx.Err().Error())
	}
}

// CheckResult represents the decision from checking a tool's permission.
type CheckResult int

const (
	CheckAllow CheckResult = iota // proceed
	CheckDeny                     // blocked
	CheckAsk                      // needs user approval
)

// ToCheckResult checks the tool and returns the result, simplifying the caller logic.
func (c *Checker) ToCheckResult(toolName string) CheckResult {
	if c == nil {
		return CheckAllow
	}
	switch c.Check(toolName) {
	case PolicyAllow:
		return CheckAllow
	case PolicyDeny:
		return CheckDeny
	default:
		return CheckAsk
	}
}

// AlwaysAllow is an explicit auto-approval callback for trusted callers.
var AlwaysAllow = func(req ApprovalRequest) {
	req.Response <- AllowOnce()
}

// ErrDenied is returned when a tool call is denied by permissions.
type ErrDenied struct {
	ToolName string
}

func (e *ErrDenied) Error() string {
	return "tool call denied by permission policy: " + e.ToolName
}
