package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	permissionpkg "github.com/abdul-hamid-achik/sonar/internal/permission"
)

const (
	maxApprovalDiffInputBytes = 1024 * 1024
	maxApprovalDiffCells      = 2_000_000
	maxApprovalDiffBytes      = 256 * 1024
)

// approvalScopeWorkspace is the exact workspace key session grants are stored
// under. Every reader of approvalGrants must compute it identically — a
// divergent spelling (unresolved symlink, uncleaned join) would make saved
// grants silently never fire.
func (a *Agent) approvalScopeWorkspace() string {
	workspace := a.filesystemContext().workDir
	if workspace == "" {
		workspace, _ = os.Getwd()
	}
	if absolute, err := filepath.Abs(workspace); err == nil {
		workspace = filepath.Clean(absolute)
	}
	return workspace
}

func (a *Agent) newApprovalRequest(ctx context.Context, mode AuthorityMode, tc llm.ToolCall, argumentsHash string) permissionpkg.ApprovalRequest {
	workspace := a.approvalScopeWorkspace()
	request := permissionpkg.ApprovalRequest{
		RequestID:       tc.ID,
		ToolName:        tc.Name,
		Args:            cloneApprovalArguments(tc.Arguments),
		ArgumentsSHA256: argumentsHash,
		Scope: permissionpkg.ApprovalScope{
			Kind:      permissionpkg.ScopeExactRequest,
			Workspace: workspace,
			Resource:  argumentsHash,
		},
	}
	request.Preview = a.buildApprovalPreview(ctx, mode, tc, argumentsHash)
	return request
}

func cloneApprovalArguments(arguments map[string]any) map[string]any {
	if arguments == nil {
		return nil
	}
	cloned := make(map[string]any, len(arguments))
	for key, value := range arguments {
		cloned[key] = cloneApprovalValue(value)
	}
	return cloned
}

func cloneApprovalValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneApprovalArguments(typed)
	case []any:
		cloned := make([]any, len(typed))
		for i := range typed {
			cloned[i] = cloneApprovalValue(typed[i])
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func (a *Agent) buildApprovalPreview(ctx context.Context, mode AuthorityMode, tc llm.ToolCall, argumentsHash string) permissionpkg.ApprovalPreview {
	preview := permissionpkg.ApprovalPreview{
		Kind:            permissionpkg.PreviewGeneric,
		ArgumentsSHA256: argumentsHash,
	}
	if def, ok := a.mcpToolDefinition(tc.Name); ok {
		preview.ActionLabel = boundApprovalLabel(def.DisplayName)
		preview.Consequence = mcpApprovalConsequence(def.Behavior)
	}
	pathArg := func(key string, destructive bool) string {
		raw, _ := tc.Arguments[key].(string)
		if raw == "" {
			return ""
		}
		var (
			resolved string
			err      error
		)
		if destructive {
			resolved, err = a.resolveDestructivePath(raw)
		} else {
			resolved, err = a.resolvePath(raw)
		}
		if err == nil {
			return resolved
		}
		return raw
	}
	readablePathArg := func(key string) string {
		raw, _ := tc.Arguments[key].(string)
		if raw == "" {
			return ""
		}
		resolved, err := a.resolveReadablePath(raw)
		if err == nil {
			defer func() { _ = resolved.close() }()
			return resolved.absolute
		}
		return raw
	}

	switch tc.Name {
	case "write":
		preview.Kind = permissionpkg.PreviewFileWrite
		preview.Consequence = "Creates or replaces the target file with the proposed content."
		preview.Path = pathArg("path", false)
		content, _ := tc.Arguments["content"].(string)
		preview.ByteSize = int64(len(content))
		preview.ContentSHA256 = executionpkg.HashText(content)
		before, exists, reason := a.approvalExistingContent(preview.Path)
		if exists {
			preview.ExistingSHA256 = executionpkg.HashText(before)
		}
		if reason != "" {
			preview.DiffOmittedReason = reason
			break
		}
		preview.Diff, preview.DiffTruncated, preview.DiffOmittedReason = approvalDiff(ctx, before, content)
	case "edit":
		preview.Path = pathArg("path", false)
		// A string-replacement edit carries no patch body to display, so its
		// preview diff is derived from the exact replacement instead. The
		// preview kind, path resolution and hashes are identical to the patch
		// mode: the approval contract does not depend on which mode was used.
		if edit, replacementMode := parseReplacementEdit(tc.Arguments); replacementMode {
			a.replacementEditPreview(ctx, &preview, edit)
			break
		}
		preview.Kind = permissionpkg.PreviewFilePatch
		preview.Consequence = "Changes the target file according to the proposed patch."
		patch, _ := tc.Arguments["patch"].(string)
		preview.Diff = patch
		if len(preview.Diff) > maxApprovalDiffBytes {
			preview.Diff = truncateApprovalUTF8(preview.Diff, maxApprovalDiffBytes)
			preview.DiffTruncated = true
		}
		before, exists, reason := a.approvalExistingContent(preview.Path)
		if exists {
			preview.ExistingSHA256 = executionpkg.HashText(before)
		}
		if reason == "" && exists {
			if after, err := applyPatch(before, patch); err == nil {
				preview.ByteSize = int64(len(after))
				preview.ContentSHA256 = executionpkg.HashText(after)
			} else {
				preview.DiffOmittedReason = fmt.Sprintf("patch could not be applied for preview: %v", err)
			}
		} else if reason != "" {
			preview.DiffOmittedReason = reason
		}
	case "bash":
		preview.Kind = permissionpkg.PreviewCommand
		preview.Command, _ = tc.Arguments["command"].(string)
		preview.Consequence = "Host policy did not pre-authorize this command for the current turn. Shell commands can change files, start processes, or contact external systems; inspect the exact command before allowing it."
		preview.Reason = a.autoCommandApprovalReason(mode, preview.Command)
	case "copy":
		preview.Kind = permissionpkg.PreviewFilesystem
		preview.ActionLabel = "Copy file"
		preview.Consequence = "Creates or replaces the destination file with a copy; the source remains unchanged."
		preview.SourcePath = readablePathArg("source")
		preview.DestinationPath = pathArg("destination", false)
	case "move":
		preview.Kind = permissionpkg.PreviewFilesystem
		preview.ActionLabel = "Move path"
		preview.Consequence = "Moves the source to the destination; the source path will no longer exist."
		preview.SourcePath = pathArg("source", true)
		preview.DestinationPath = pathArg("destination", true)
	case "remove":
		preview.Kind = permissionpkg.PreviewFilesystem
		preview.ActionLabel = "Remove path"
		if a.getArgBool(tc.Arguments, "recursive", false) {
			preview.Consequence = "Deletes the target and its descendants from the workspace."
		} else {
			preview.Consequence = "Deletes the target; non-empty directories are refused without recursive removal."
		}
		preview.Path = pathArg("path", true)
	case "mkdir":
		preview.Kind = permissionpkg.PreviewFilesystem
		preview.ActionLabel = "Create directory"
		preview.Consequence = "Creates the directory path, including missing parent directories."
		preview.Path = pathArg("path", false)
	default:
		for _, key := range []string{"path", "file_path", "filename", "file"} {
			if raw, ok := tc.Arguments[key].(string); ok && raw != "" {
				preview.Path = raw
				break
			}
		}
	}
	return preview
}

// revalidateApprovalPreview verifies the exact presentation contract the user
// approved. It never substitutes a newly built preview: any changed path,
// existence/content hash, diff, command, or bounded metadata fails closed and
// requires a fresh modal.
func (a *Agent) revalidateApprovalPreview(ctx context.Context, mode AuthorityMode, tc llm.ToolCall, request permissionpkg.ApprovalRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current := a.buildApprovalPreview(ctx, mode, tc, request.ArgumentsSHA256)
	if err := ctx.Err(); err != nil {
		return err
	}
	if current != request.Preview {
		return errors.New("approval preview changed while the request was open")
	}
	return nil
}

func mcpApprovalConsequence(behavior llm.ToolBehavior) string {
	if !behavior.Declared {
		return "The MCP server supplied no effect metadata. This call may read data, contact external systems, or change durable state; inspect the exact arguments."
	}
	// MCP destructiveHint is meaningful only for non-read-only tools. Keep the
	// call effect-unknown and approval-gated, but do not present a conventional
	// readOnlyHint as an asserted destructive operation.
	if behavior.ReadOnly {
		if behavior.OpenWorld {
			return "The server declares this read-only, but it may contact external systems. Server annotations are untrusted."
		}
		return "The server declares this read-only. Server annotations are untrusted, so the call remains effect-unknown and approval-gated."
	}
	consequence := "Server metadata indicates this call may create or update durable state."
	if behavior.Destructive {
		consequence = "Server metadata indicates this call may make destructive changes."
	}
	if behavior.OpenWorld {
		consequence = strings.TrimSuffix(consequence, ".") + " and contact external systems."
	}
	return consequence
}

func boundApprovalLabel(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	const maximumBytes = 160
	if len(value) <= maximumBytes {
		return value
	}
	return truncateApprovalUTF8(value, maximumBytes-3) + "..."
}

func (a *Agent) approvalExistingContent(path string) (content string, exists bool, omittedReason string) {
	if strings.TrimSpace(path) == "" {
		return "", false, "target path is unavailable"
	}
	workspace, _, relative, err := a.openWritableRootForPath(path)
	if err != nil {
		return "", false, fmt.Sprintf("existing content unavailable: %v", err)
	}
	defer func() { _ = workspace.Close() }()
	parent, name, err := workspace.openParent(relative, false)
	if os.IsNotExist(err) {
		return "", false, ""
	}
	if err != nil {
		return "", false, fmt.Sprintf("existing content unavailable: %v", err)
	}
	defer func() { _ = parent.Close() }()
	data, _, err := readPinnedRootFile(parent, name, maxApprovalDiffInputBytes)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, ""
	}
	if err != nil {
		return "", false, fmt.Sprintf("existing content unavailable: %v", err)
	}
	return string(data), true, ""
}

func approvalDiff(ctx context.Context, before, after string) (diff string, truncated bool, omittedReason string) {
	if before == after {
		return "", false, ""
	}
	if len(before) > maxApprovalDiffInputBytes || len(after) > maxApprovalDiffInputBytes {
		return "", false, fmt.Sprintf("diff omitted: input exceeds %d bytes; exact content remains bound by SHA-256", maxApprovalDiffInputBytes)
	}
	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")
	if len(beforeLines) > 0 && len(afterLines) > maxApprovalDiffCells/len(beforeLines) {
		return "", false, fmt.Sprintf("diff omitted: %d x %d lines; exact content remains bound by SHA-256", len(beforeLines), len(afterLines))
	}
	diff, err := computeDiff(ctx, beforeLines, afterLines)
	if err != nil {
		return "", false, fmt.Sprintf("diff unavailable: %v", err)
	}
	if len(diff) <= maxApprovalDiffBytes {
		return diff, false, ""
	}
	return truncateApprovalUTF8(diff, maxApprovalDiffBytes), true, ""
}

func truncateApprovalUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func approvalGrantKey(request permissionpkg.ApprovalRequest) string {
	return request.Scope.Workspace + "\x00" + request.ToolName + "\x00" + request.Scope.Kind + "\x00" + request.Scope.Resource
}

func sessionToolGrantKey(workspace, toolName string) string {
	return workspace + "\x00" + toolName + "\x00" + permissionpkg.ScopeSessionTool + "\x00"
}

func sessionPathGrantKey(workspace, path string) string {
	// Shared across write/edit/mkdir so approving a path once covers the
	// whole file-mutation family for that path (matches durable write paths).
	return workspace + "\x00" + permissionpkg.SessionPathFamily + "\x00" + permissionpkg.ScopeSessionPath + "\x00" + path
}

func sessionBashPrefixGrantKey(workspace, prefix string) string {
	return workspace + "\x00" + "bash" + "\x00" + permissionpkg.ScopeSessionBashPrefix + "\x00" + prefix
}

func sessionMCPToolGrantKey(workspace, toolName string) string {
	return workspace + "\x00" + toolName + "\x00" + permissionpkg.ScopeSessionMCPTool + "\x00"
}

// sessionToolScopeEligible reports whether a tool may receive a process-local
// session-wide tool grant. Only workspace edit built-ins qualify.
func sessionToolScopeEligible(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "write", "edit", "mkdir":
		return true
	default:
		return false
	}
}

func sessionPathScopeEligible(toolName string) bool {
	return sessionToolScopeEligible(toolName)
}

func sessionMCPToolScopeEligible(toolName string) bool {
	_, ok := permissionpkg.NormalizeMCPToolName(toolName)
	return ok
}

func (a *Agent) hasSessionApproval(request permissionpkg.ApprovalRequest) bool {
	exactKey := approvalGrantKey(request)
	a.mu.RLock()
	defer a.mu.RUnlock()
	if _, exact := a.approvalGrants[exactKey]; exact {
		return true
	}
	workspace := request.Scope.Workspace
	tool := request.ToolName

	// session_tool: any args for write/edit/mkdir
	if sessionToolScopeEligible(tool) {
		if _, ok := a.approvalGrants[sessionToolGrantKey(workspace, tool)]; ok {
			return true
		}
	}

	// session_path: write/edit/mkdir on the same canonical path (tool-family).
	if sessionPathScopeEligible(tool) {
		if path := approvalPathResource(request); path != "" {
			if _, ok := a.approvalGrants[sessionPathGrantKey(workspace, path)]; ok {
				return true
			}
		}
	}

	// session_bash_prefix (literal or trailing glob patterns)
	if tool == "bash" {
		command, _ := request.Args["command"].(string)
		for key := range a.approvalGrants {
			parts := strings.Split(key, "\x00")
			if len(parts) < 4 || parts[0] != workspace || parts[1] != "bash" || parts[2] != permissionpkg.ScopeSessionBashPrefix {
				continue
			}
			if permissionpkg.BashPatternMatches(command, parts[3]) {
				return true
			}
		}
	}

	// session_mcp_tool: any args for this exact MCP tool name
	if sessionMCPToolScopeEligible(tool) {
		if _, ok := a.approvalGrants[sessionMCPToolGrantKey(workspace, tool)]; ok {
			return true
		}
	}
	return false
}

func approvalPathResource(request permissionpkg.ApprovalRequest) string {
	if path := strings.TrimSpace(request.Preview.Path); path != "" {
		return filepath.Clean(path)
	}
	if raw, ok := request.Args["path"].(string); ok {
		return filepath.Clean(strings.TrimSpace(raw))
	}
	return ""
}

func (a *Agent) rememberSessionApproval(request permissionpkg.ApprovalRequest) {
	key := approvalGrantKey(request)
	// Path grants are shared across write/edit/mkdir for the same path.
	if request.Scope.Kind == permissionpkg.ScopeSessionPath && request.Scope.Resource != "" {
		key = sessionPathGrantKey(request.Scope.Workspace, request.Scope.Resource)
	}
	if request.Scope.Kind == permissionpkg.ScopeSessionBashPrefix {
		key = sessionBashPrefixGrantKey(request.Scope.Workspace, request.Scope.Resource)
	}
	a.mu.Lock()
	if a.approvalGrants == nil {
		a.approvalGrants = make(map[string]struct{})
	}
	a.approvalGrants[key] = struct{}{}
	a.mu.Unlock()
}

// applySessionScope widens request.Scope from a typed AllowSession response.
// Ineligible widen attempts fall back to exact_request (caller's default).
func applySessionScope(request *permissionpkg.ApprovalRequest, scopeKind string) {
	if request == nil {
		return
	}
	switch scopeKind {
	case permissionpkg.ScopeSessionTool:
		if !sessionToolScopeEligible(request.ToolName) {
			return
		}
		request.Scope.Kind = permissionpkg.ScopeSessionTool
		request.Scope.Resource = ""
	case permissionpkg.ScopeSessionPath:
		if !sessionPathScopeEligible(request.ToolName) {
			return
		}
		path := approvalPathResource(*request)
		if path == "" {
			return
		}
		request.Scope.Kind = permissionpkg.ScopeSessionPath
		request.Scope.Resource = path
	case permissionpkg.ScopeSessionBashPrefix:
		if request.ToolName != "bash" {
			return
		}
		command, _ := request.Args["command"].(string)
		prefix, ok := permissionpkg.DeriveBashPrefix(command)
		if !ok {
			return
		}
		request.Scope.Kind = permissionpkg.ScopeSessionBashPrefix
		request.Scope.Resource = prefix
	case permissionpkg.ScopeSessionMCPTool:
		if !sessionMCPToolScopeEligible(request.ToolName) {
			return
		}
		request.Scope.Kind = permissionpkg.ScopeSessionMCPTool
		request.Scope.Resource = ""
	}
}

// hasWorkspaceRuleApproval reports a durable workspace rule match. Deny and
// skip-approvals are handled by the caller before this check.
func (a *Agent) hasWorkspaceRuleApproval(tc llm.ToolCall) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	rules := a.workspaceRules
	a.mu.RUnlock()
	switch {
	case tc.Name == "bash":
		command, _ := tc.Arguments["command"].(string)
		return rules.AllowsBash(command)
	case sessionMCPToolScopeEligible(tc.Name):
		return rules.AllowsMCPTool(tc.Name)
	case sessionPathScopeEligible(tc.Name):
		path, _ := tc.Arguments["path"].(string)
		if strings.TrimSpace(path) == "" {
			return false
		}
		// Prefer host-resolved workspace path so relative args still match.
		resolved, err := a.resolveWorkspacePath(path)
		if err != nil {
			// Fall back to preview-style absolute join if resolve fails closed
			// for non-workspace targets (rule must never authorize outside).
			return false
		}
		workspace, err := a.checkpointWorkspaceID()
		if err != nil {
			return false
		}
		return rules.AllowsWritePath(workspace, resolved)
	default:
		return false
	}
}

// ReloadWorkspaceRules loads durable rules for the current workDir.
func (a *Agent) ReloadWorkspaceRules() error {
	if a == nil {
		return nil
	}
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return err
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return err
	}
	rules, err := store.Load(workspace)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	// Loading rules for a workdir change does not bump approvalHostVersion:
	// a pinned turn keeps its filesystem/host snapshot, and SetWorkDir may
	// legitimately prepare the next turn while an approval is open. Explicit
	// rule mutations (Add/Remove) bump the host version instead.
	a.mu.Unlock()
	return nil
}

func (a *Agent) ensureWorkspaceRulesStore() (*permissionpkg.WorkspaceRulesStore, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.workspaceRulesStore != nil {
		return a.workspaceRulesStore, nil
	}
	store, err := permissionpkg.NewWorkspaceRulesStore("")
	if err != nil {
		return nil, err
	}
	a.workspaceRulesStore = store
	return store, nil
}

// SetWorkspaceRulesStore installs a rules store (tests and custom roots).
func (a *Agent) SetWorkspaceRulesStore(store *permissionpkg.WorkspaceRulesStore) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.workspaceRulesStore = store
	a.mu.Unlock()
}

// WorkspaceRulesSnapshot returns a copy of the loaded durable rules.
func (a *Agent) WorkspaceRulesSnapshot() permissionpkg.WorkspaceRules {
	if a == nil {
		return permissionpkg.WorkspaceRules{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	rules := a.workspaceRules
	rules.BashPrefixes = append([]string(nil), rules.BashPrefixes...)
	rules.MCPTools = append([]string(nil), rules.MCPTools...)
	rules.WritePaths = append([]string(nil), rules.WritePaths...)
	return rules
}

// AddWorkspaceBashPrefix persists a durable bash prefix for this workspace.
func (a *Agent) AddWorkspaceBashPrefix(prefix string) (permissionpkg.WorkspaceRules, error) {
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	rules, err := store.AddBashPrefix(workspace, prefix)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	a.approvalHostVersion++
	a.mu.Unlock()
	return rules, nil
}

// RemoveWorkspaceBashPrefix removes a durable bash prefix.
func (a *Agent) RemoveWorkspaceBashPrefix(prefix string) (permissionpkg.WorkspaceRules, bool, error) {
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	rules, removed, err := store.RemoveBashPrefix(workspace, prefix)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	a.approvalHostVersion++
	a.mu.Unlock()
	return rules, removed, nil
}

// AddWorkspaceMCPTool persists a durable exact MCP tool allow.
func (a *Agent) AddWorkspaceMCPTool(tool string) (permissionpkg.WorkspaceRules, error) {
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	rules, err := store.AddMCPTool(workspace, tool)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	a.approvalHostVersion++
	a.mu.Unlock()
	return rules, nil
}

// RemoveWorkspaceMCPTool removes a durable MCP tool allow.
func (a *Agent) RemoveWorkspaceMCPTool(tool string) (permissionpkg.WorkspaceRules, bool, error) {
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	rules, removed, err := store.RemoveMCPTool(workspace, tool)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	a.approvalHostVersion++
	a.mu.Unlock()
	return rules, removed, nil
}

// AddWorkspaceWritePath persists a durable write/edit/mkdir path for this workspace.
func (a *Agent) AddWorkspaceWritePath(path string) (permissionpkg.WorkspaceRules, error) {
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	// Resolve through the host workspace boundary when possible so relative
	// paths from approvals become portable relative grants.
	if resolved, resolveErr := a.resolveWorkspacePath(path); resolveErr == nil {
		path = resolved
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	rules, err := store.AddWritePath(workspace, path)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	a.approvalHostVersion++
	a.mu.Unlock()
	return rules, nil
}

// RemoveWorkspaceWritePath removes a durable write path allow.
func (a *Agent) RemoveWorkspaceWritePath(path string) (permissionpkg.WorkspaceRules, bool, error) {
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	if resolved, resolveErr := a.resolveWorkspacePath(path); resolveErr == nil {
		path = resolved
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	rules, removed, err := store.RemoveWritePath(workspace, path)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, false, err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	a.approvalHostVersion++
	a.mu.Unlock()
	return rules, removed, nil
}

// ExportWorkspaceRules returns a portable rules document for the current workspace.
func (a *Agent) ExportWorkspaceRules() (permissionpkg.WorkspaceRulesExport, error) {
	if a == nil {
		return permissionpkg.WorkspaceRulesExport{}, fmt.Errorf("agent is unavailable")
	}
	if err := a.ReloadWorkspaceRules(); err != nil {
		return permissionpkg.WorkspaceRulesExport{}, err
	}
	return a.WorkspaceRulesSnapshot().ExportDocument(), nil
}

// ExportWorkspaceRulesToFile writes the portable document to path.
func (a *Agent) ExportWorkspaceRulesToFile(path string) (permissionpkg.WorkspaceRulesExport, error) {
	doc, err := a.ExportWorkspaceRules()
	if err != nil {
		return permissionpkg.WorkspaceRulesExport{}, err
	}
	if err := permissionpkg.WriteExportFile(path, doc); err != nil {
		return permissionpkg.WorkspaceRulesExport{}, err
	}
	return doc, nil
}

// ImportWorkspaceRules merges or replaces durable rules from a portable document.
func (a *Agent) ImportWorkspaceRules(doc permissionpkg.WorkspaceRulesExport, replace bool) (permissionpkg.WorkspaceRules, int, error) {
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, 0, err
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, 0, err
	}
	rules, added, err := store.Import(workspace, doc, replace)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, 0, err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	a.approvalHostVersion++
	a.mu.Unlock()
	return rules, added, nil
}

// ImportWorkspaceRulesFromFile loads a portable export and imports it.
func (a *Agent) ImportWorkspaceRulesFromFile(path string, replace bool) (permissionpkg.WorkspaceRules, int, error) {
	doc, err := permissionpkg.ReadExportFile(path)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, 0, err
	}
	return a.ImportWorkspaceRules(doc, replace)
}

// ClearWorkspaceRules removes every durable rule for the current workspace.
func (a *Agent) ClearWorkspaceRules() (permissionpkg.WorkspaceRules, error) {
	workspace, err := a.checkpointWorkspaceID()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	store, err := a.ensureWorkspaceRulesStore()
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	rules, err := store.ClearAll(workspace)
	if err != nil {
		return permissionpkg.WorkspaceRules{}, err
	}
	a.mu.Lock()
	a.workspaceRules = rules
	a.approvalHostVersion++
	a.mu.Unlock()
	return rules, nil
}

// DefaultWorkspaceRulesExportPath returns workdir/sonar-permissions.json.
func (a *Agent) DefaultWorkspaceRulesExportPath() string {
	if a == nil {
		return permissionpkg.DefaultExportFileName
	}
	workDir := strings.TrimSpace(a.WorkDir())
	if workDir == "" {
		return permissionpkg.DefaultExportFileName
	}
	return filepath.Join(workDir, permissionpkg.DefaultExportFileName)
}

// ListSessionApprovalSummary returns process-local session grants as stable
// labels for host status surfaces. Path and bash grants include a short resource.
func (a *Agent) ListSessionApprovalSummary() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.approvalGrants) == 0 {
		return nil
	}
	summaries := make([]string, 0, len(a.approvalGrants))
	for key := range a.approvalGrants {
		if label := formatSessionGrantSummary(key); label != "" {
			summaries = append(summaries, label)
		}
	}
	sort.Strings(summaries)
	return summaries
}

func formatSessionGrantSummary(key string) string {
	parts := strings.Split(key, "\x00")
	if len(parts) < 3 {
		return ""
	}
	tool := parts[1]
	kind := parts[2]
	resource := ""
	if len(parts) >= 4 {
		resource = parts[3]
	}
	if tool == "" {
		tool = "(unknown tool)"
	}
	if kind == "" {
		kind = permissionpkg.ScopeExactRequest
	}
	label := tool + " · " + kind
	switch kind {
	case permissionpkg.ScopeSessionPath:
		if resource != "" {
			label += " · " + compactGrantResource(resource, 48)
		}
	case permissionpkg.ScopeSessionBashPrefix:
		if resource != "" {
			label += " · " + compactGrantResource(resource, 40)
		}
	case permissionpkg.ScopeExactRequest:
		if resource != "" {
			// Arguments hash: show short digest only.
			label += " · " + compactGrantResource(resource, 12)
		}
	}
	return label
}

func compactGrantResource(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || limit <= 0 {
		return value
	}
	// Prefer basename for absolute paths in summaries.
	if strings.Contains(value, string(filepath.Separator)) || strings.Contains(value, "/") {
		base := filepath.Base(value)
		if base != "" && base != "." && base != string(filepath.Separator) {
			// Keep a short parent when space allows.
			parent := filepath.Base(filepath.Dir(value))
			if parent != "" && parent != "." && len(parent)+1+len(base) <= limit {
				value = parent + "/" + base
			} else {
				value = base
			}
		}
	}
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

// RevokeSessionApprovals removes process-local session grants. When tool is
// empty every grant is cleared; otherwise grants for that tool name are
// removed. write/edit/mkdir also clear shared path-family grants.
func (a *Agent) RevokeSessionApprovals(tool string) int {
	if a == nil {
		return 0
	}
	tool = strings.TrimSpace(tool)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.approvalGrants) == 0 {
		return 0
	}
	if tool == "" {
		n := len(a.approvalGrants)
		a.approvalGrants = make(map[string]struct{})
		return n
	}
	removed := 0
	clearPathFamily := tool == "write" || tool == "edit" || tool == "mkdir" || tool == permissionpkg.SessionPathFamily
	for key := range a.approvalGrants {
		parts := strings.Split(key, "\x00")
		if len(parts) < 2 {
			continue
		}
		grantTool := parts[1]
		grantKind := ""
		if len(parts) >= 3 {
			grantKind = parts[2]
		}
		match := grantTool == tool
		if clearPathFamily && grantKind == permissionpkg.ScopeSessionPath {
			match = true
		}
		if !match {
			continue
		}
		delete(a.approvalGrants, key)
		removed++
	}
	return removed
}

// ClearSessionApprovals drops every process-local session approval grant.
func (a *Agent) ClearSessionApprovals() {
	_ = a.RevokeSessionApprovals("")
}
