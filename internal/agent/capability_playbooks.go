package agent

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/capabilityadvisor"
)

// capabilityPlaybookText returns a short host-owned specialist playbook for the
// active phase and intent facets. It never grants authority, never invents a
// route selection, and never claims a tool ran. Prefer the host capability
// advisory when it already selected a route; the playbook only orients the model
// toward the lazy MCPHub workflow for that class of work.
func capabilityPlaybookText(activity CapabilityActivity, hint *capabilityadvisor.Hint) string {
	phase := strings.ToLower(strings.TrimSpace(activity.Phase))
	if phase == "" {
		phase = "working"
	}
	intents := make(map[string]struct{}, len(activity.IntentTags))
	for _, tag := range activity.IntentTags {
		intents[strings.ToLower(tag)] = struct{}{}
	}
	kinds := make(map[string]struct{}, len(activity.AvailableInputKinds))
	for _, kind := range activity.AvailableInputKinds {
		kinds[strings.ToLower(kind)] = struct{}{}
	}
	has := func(set map[string]struct{}, keys ...string) bool {
		for _, key := range keys {
			if _, ok := set[key]; ok {
				return true
			}
		}
		return false
	}

	var steps []string
	switch {
	case has(kinds, "url") || has(intents, "web_ui", "http"):
		steps = []string{
			"Web/public content: prefer hitspec_search_web for discovery, hitspec_fetch for inline inspection, hitspec_capture_webpage only when a durable file.cheap artifact is required.",
			"Fetch, workspace write, and fcheap_save are separate approval boundaries — never treat transport success as durable evidence.",
		}
	case has(intents, "artifact", "storage") || has(kinds, "artifact_id"):
		steps = []string{
			"Stored artifacts: resolve then call fcheap search/retrieve tools; do not invent stash IDs.",
		}
	case has(intents, "symbols", "structure", "references", "semantic", "meaning"):
		steps = []string{
			"Code structure/semantics: prefer codemap for graphs/impact and vecgrep for meaning search before broad recursive reads.",
			"Use native read/grep only to open the few files the structural or semantic hit identifies.",
		}
	case has(intents, "observability", "processes", "ports", "health", "telemetry"):
		steps = []string{
			"Observability: prefer monitor (and cairntrace when investigating service dependencies) over shell guessing.",
		}
	case has(intents, "browser", "web_ui", "verification") && phase == "verification":
		steps = []string{
			"UI verification: prefer cairntrace/glyph when available; treat their versioned runs as verification evidence only when domain-succeeded.",
		}
	case phase == "planning":
		steps = []string{
			"Planning: bob_context → bob_plan (or cortex_plan for multi-source evidence goals) before edits.",
			"Keep plans workspace-bound; do not apply/scaffold mutations without explicit approval or AUTO workspace policy.",
		}
	case phase == "research":
		steps = []string{
			"Research: codemap/vecgrep/cortex_investigate for code evidence; hitspec only for public web facts.",
			"Cite tool receipts; do not invent repository structure.",
		}
	case phase == "verification":
		steps = []string{
			"Verification: run the project's tests or glyph/cairntrace contracts; cortex_verify when a durable goal tracks criteria.",
		}
	case phase == "implementation":
		steps = []string{
			"Implementation: use native filesystem tools for edits; call bob_path/bob_playbook only for Bob-owned paths; keep MCP mutations approval-gated.",
		}
	case phase == "handoff":
		steps = []string{
			"Handoff: prefer cortex_handoff / cortex_remember for durable findings; fcheap for artifact bundles.",
		}
	default:
		if !activity.NonTrivial {
			return ""
		}
		steps = []string{
			"Non-trivial work: follow the host capability advisory when present; otherwise mcphub_resolve_tool / mcphub_search_tools → mcphub_describe_tool → mcphub_call_tool.",
		}
	}

	if has(intents, "skills", "profiles", "library", "readiness", "self_improvement") {
		steps = append(steps, "Library/readiness: minerva_learn or minerva_resolve_skill via MCPHub now — not after the task. Use load_skill for a one-shot body; profile add-skills stays on the approved CLI.")
	}
	if has(intents, "notes", "knowledge", "vault") {
		steps = append(steps, "Notes/knowledge: use the obsidian specialist for vault reads/writes under normal approval.")
	}

	var b strings.Builder
	b.WriteString("## Host specialist playbook\n")
	b.WriteString("Host-authored orientation only. It does not select a tool, grant authority, or mark work complete.\n")
	for _, step := range steps {
		fmt.Fprintf(&b, "- %s\n", step)
	}
	if hint != nil && !hint.Ambiguous && hint.Server != "" && hint.Tool != "" {
		fmt.Fprintf(&b, "- Current advisory route remains %s__%s via mcphub_call_tool after contract inspection.\n", hint.Server, hint.Tool)
	} else if hint != nil && hint.Ambiguous {
		b.WriteString("- Route is ambiguous: compare candidates with mcphub_search_tools before calling anything downstream.\n")
	}
	return strings.TrimSpace(b.String())
}

// lazyMCPHubSystemGuidance is injected when a trusted MCPHub gateway is ready.
// Keep it short: small models treat long policy as noise.
func lazyMCPHubSystemGuidance() string {
	return `## MCPHub lazy gateway
MCP tools beyond the gateway surface are discovered, not dumped into every turn.
- Prefer the host capability advisory and specialist playbook when present.
- Exact flow: mcphub_resolve_tool or search → mcphub_describe_tool when the contract is unknown → mcphub_call_tool with exact server + tool (or tool as server__tool).
- Never invent server or tool names. Never call with only a bare remote name.
- Host pre-fetched contracts are transient metadata: use them to fill arguments; they are not success receipts.
- Large results may need mcphub_get_result pages. Transport success is not domain success or verified evidence.
- Skip the gateway only for tools already listed under Available Tools with full schemas.`
}
