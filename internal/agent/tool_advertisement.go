package agent

import (
	"strings"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// dropUnapprovableTools removes tools that cannot possibly execute this turn
// because they need interactive approval and no approval surface exists — a
// headless run without --skip-approvals.
//
// Advertising them is not free. The model learns the constraint only by calling
// one and being refused, and it reasonably tries neighbouring tools next. One
// measured AUTO run spent five refused calls (vecgrep_ensure three times, then
// vecgrep_index, then vecgrep_clean) and 117K prompt tokens rediscovering that
// the whole write half of a server was closed to it. The refusal text already
// says "do not retry unchanged"; the fix is to never offer the tool.
//
// The filter is deliberately narrow. It drops a tool only when the answer is
// certain for the tool ALONE, independent of its arguments:
//
//   - Built-ins are never dropped. Their authorization is argument-dependent
//     (bash on an auto-scoped command, writes inside the workspace), so a
//     def-level verdict would be a guess.
//   - Gateway call-through tools are never dropped. mcphub_call_tool carries
//     its real target in its arguments, so the same def is approvable for one
//     downstream route and not another.
//   - An MCP tool with an auto-granting trust contract is kept: that contract
//     is exactly what lets it run without a prompt.
//
// Everything it removes would have been refused at dispatch with
// approval_ui_unavailable, so nothing that could have succeeded is hidden.
func (a *Agent) dropUnapprovableTools(defs []llm.ToolDef) []llm.ToolDef {
	if len(defs) == 0 || !a.approvalSurfaceMissing() {
		return defs
	}
	kept := make([]llm.ToolDef, 0, len(defs))
	for _, def := range defs {
		if a.toolIsUnapprovable(def.Name) {
			continue
		}
		kept = append(kept, def)
	}
	if len(kept) == len(defs) {
		return defs
	}
	return kept
}

// approvalSurfaceMissing reports that an approval prompt cannot be answered.
// skipApprovals lives inside the checker, so a checker that would return
// CheckAsk under a skip-approvals posture already returns CheckAllow instead —
// the posture needs no separate term here beyond guarding a nil checker.
func (a *Agent) approvalSurfaceMissing() bool {
	if a == nil {
		return false
	}
	state := a.approvalStateSnapshot()
	if state.callback != nil {
		return false
	}
	if state.checker == nil {
		// Without a checker there is no approval gate to fail; leave the
		// catalog alone rather than guessing.
		return false
	}
	return !state.checker.SkipsApprovals()
}

// toolIsUnapprovable reports whether this tool name alone — with no argument
// context — is guaranteed to need an approval that cannot be granted.
func (a *Agent) toolIsUnapprovable(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	// Built-ins and memory tools authorize on their arguments.
	if a.isToolsTool(name) || a.isMemoryTool(name) {
		return false
	}
	if !strings.Contains(name, "__") {
		return false
	}
	// A gateway call-through names its real target in its arguments, so the
	// def is approvable for some routes and not others.
	if strings.HasSuffix(name, "__mcphub_call_tool") {
		return false
	}
	kind, _ := a.executionKind(name)
	if kind != executionpkg.KindMCP {
		return false
	}
	if contract, ok := a.trustedMCPContract(llm.ToolCall{Name: name}); ok && contract.auto {
		return false
	}
	return mcpToolRequiresApproval()
}
