package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/capabilityadvisor"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const (
	// Under pressure, native tool schemas may consume at most one quarter of
	// the effective context window. The ordinary 75% prompt-admission boundary
	// then still preserves at least one quarter of the window for generation.
	maxToolSchemaContextShare = 4

	// Rebuilding the system prompt around an admitted set can add a small amount
	// of tool-dependent guidance (notably memory guidelines). Keep that outside
	// the schema budget so a greedy exact-schema fit cannot cross the boundary.
	toolAdmissionPromptOverheadReserve = 256
)

var essentialLocalToolRank = map[string]int{
	"read":       0,
	"grep":       1,
	"glob":       2,
	"ls":         3,
	"find":       4,
	"bash":       5,
	"exists":     6,
	"write":      7,
	"edit":       8,
	"load_skill": 9,
}

var localToolNames = map[string]struct{}{
	"grep": {}, "read": {}, "write": {}, "glob": {}, "bash": {}, "bash_output": {},
	"ls": {}, "find": {}, "diff": {}, "edit": {}, "mkdir": {}, "remove": {},
	"copy": {}, "move": {}, "exists": {}, "load_skill": {},
	"agent": {}, "agent_output": {},
}

var memoryToolNames = map[string]struct{}{
	"memory_save": {}, "memory_recall": {}, "memory_delete": {},
	"memory_update": {}, "memory_list": {},
}

var essentialMCPHubMetaToolRank = map[string]int{
	"mcphub_resolve_tool":  0,
	"mcphub_call_tool":     1,
	"mcphub_describe_tool": 2,
	"mcphub_search_tools":  3,
	"mcphub_get_result":    4,
	"mcphub_poll_result":   5,
}

var otherMCPHubMetaTools = map[string]struct{}{
	"mcphub_list_servers": {},
	"mcphub_stats":        {},
}

var lazyGatewayBootstrapLocalTools = []string{"read", "grep"}

var lazyGatewayBootstrapOptionalOperations = []string{
	"mcphub_describe_tool",
	"mcphub_search_tools",
}

// toolAdmissionBreakdown is a bounded, host-authored diagnostic. It describes
// only schema counts and token estimates; no MCP result or StructuredContent
// crosses the parser boundary.
type toolAdmissionBreakdown struct {
	AvailableTools     int
	AdmittedTools      int
	OmittedTools       int
	PromptTargetTokens int
	BasePromptTokens   int
	PromptTokensBefore int
	PromptTokensAfter  int
	SchemaBudgetTokens int
	SchemaTokensBefore int
	SchemaTokensAfter  int
	Applied            bool
}

// admitToolSchemasForContext keeps the complete provider surface whenever it
// fits. Under pressure it replaces only the turn-local provider projection;
// the registry snapshot, execution authority, and durable session state remain
// unchanged.
func (t *turnRuntime) admitToolSchemasForContext(ctx context.Context) toolAdmissionBreakdown {
	if t.availableTools == nil {
		// Tests and narrowly constructed runtimes may not pass through the
		// ordinary turn constructor. Snapshot once so subsequent admissions
		// still have the same re-expansion semantics as production turns.
		t.availableTools = append([]llm.ToolDef(nil), t.tools...)
	}
	candidates := append([]llm.ToolDef(nil), t.availableTools...)
	// A tool that cannot be approved this turn can only be refused at dispatch.
	// Offering it costs the model a call and the turn its tokens to learn that.
	candidates = t.a.dropUnapprovableTools(candidates)
	if !sameToolCatalog(t.tools, candidates) {
		t.tools = candidates
		t.rebuildSystem(ctx)
	}

	breakdown := toolAdmissionBreakdown{
		AvailableTools:     len(candidates),
		AdmittedTools:      len(candidates),
		PromptTokensBefore: t.estimatedPromptTokens(),
		SchemaTokensBefore: estimateToolDefinitionsPromptTokens(candidates),
	}
	if t.turnNumCtx <= 0 || !shouldCompactForContext(breakdown.PromptTokensBefore, t.turnNumCtx) {
		breakdown.PromptTokensAfter = breakdown.PromptTokensBefore
		breakdown.SchemaTokensAfter = breakdown.SchemaTokensBefore
		return breakdown
	}

	t.tools = nil
	t.rebuildSystem(ctx)

	breakdown.Applied = true
	breakdown.PromptTargetTokens = contextPromptTargetTokens(t.turnNumCtx)
	breakdown.BasePromptTokens = t.estimatedPromptTokens()
	windowBudget := breakdown.PromptTargetTokens - breakdown.BasePromptTokens - toolAdmissionPromptOverheadReserve
	if windowBudget < 0 {
		windowBudget = 0
	}
	breakdown.SchemaBudgetTokens = min(t.turnNumCtx/maxToolSchemaContextShare, windowBudget)

	t.tools = admitToolDefsForSchemaBudget(candidates, t.capabilityHint, breakdown.SchemaBudgetTokens, t.trustedMCPHubNamespaces)
	t.rebuildSystem(ctx)
	breakdown.AdmittedTools = len(t.tools)
	breakdown.OmittedTools = breakdown.AvailableTools - breakdown.AdmittedTools
	breakdown.SchemaTokensAfter = estimateToolDefinitionsPromptTokens(t.tools)
	breakdown.PromptTokensAfter = t.estimatedPromptTokens()

	if t.lg != nil {
		t.lg.Info("tool schema admission",
			"available_tools", breakdown.AvailableTools,
			"admitted_tools", breakdown.AdmittedTools,
			"omitted_tools", breakdown.OmittedTools,
			"prompt_target_tokens", breakdown.PromptTargetTokens,
			"base_prompt_tokens", breakdown.BasePromptTokens,
			"prompt_tokens_before", breakdown.PromptTokensBefore,
			"prompt_tokens_after", breakdown.PromptTokensAfter,
			"schema_budget_tokens", breakdown.SchemaBudgetTokens,
			"schema_tokens_before", breakdown.SchemaTokensBefore,
			"schema_tokens_after", breakdown.SchemaTokensAfter,
		)
	}
	if breakdown.OmittedTools > 0 && t.out != nil {
		t.out.SystemMessage(fmt.Sprintf("Context pressure · %d of %d tools admitted this turn · %d omitted to fit context",
			breakdown.AdmittedTools, breakdown.AvailableTools, breakdown.OmittedTools))
	}
	return breakdown
}

func sameToolCatalog(left, right []llm.ToolDef) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Name != right[index].Name {
			return false
		}
	}
	return true
}

// estimatedPromptTokens returns the host prompt-token estimate clamped to both
// durable floors: the provider's last reported prompt count (authoritative for
// an unchanged prompt) and the durable receipt floor. Every compaction and
// budget decision should route through this single helper so the floors are
// applied consistently.
func (t *turnRuntime) estimatedPromptTokens() int {
	estimated := t.a.estimatePromptTokens(t.system, t.tools)
	if estimated < t.lastPromptTokens {
		estimated = t.lastPromptTokens
	}
	hostTokens := estimateHostPromptTokens(t.system, t.tools)
	if receiptFloor := t.a.contextPromptFloorEstimate(t.turnModel, hostTokens); estimated < receiptFloor {
		return receiptFloor
	}
	return estimated
}

func contextPromptTargetTokens(numCtx int) int {
	if numCtx <= 0 {
		return 0
	}
	return int(float64(numCtx) * compactThreshold)
}

type rankedToolDef struct {
	def         llm.ToolDef
	measurement toolSchemaMeasurement
	priority    int
	rank        int
	index       int
}

// toolSchemaMeasurement retains just enough information to reproduce
// estimateToolDefinitionsPromptTokens for any selected JSON array without
// re-marshalling every growing candidate set. JSON punctuation is ASCII, so
// the aggregate can preserve the host estimator's separate ASCII/non-ASCII
// accounting exactly.
type toolSchemaMeasurement struct {
	asciiBytes    int
	nonASCIIBytes int
}

type measuredToolDef struct {
	def         llm.ToolDef
	measurement toolSchemaMeasurement
}

func measureToolDefinitions(defs []llm.ToolDef) ([]measuredToolDef, bool) {
	measured := make([]measuredToolDef, len(defs))
	for index, def := range defs {
		encoded, err := json.Marshal(def)
		if err != nil {
			return nil, false
		}
		measurement := toolSchemaMeasurement{}
		for len(encoded) > 0 {
			r, size := utf8.DecodeRune(encoded)
			if r < utf8.RuneSelf {
				measurement.asciiBytes++
			} else {
				measurement.nonASCIIBytes += size
			}
			encoded = encoded[size:]
		}
		measured[index] = measuredToolDef{def: def, measurement: measurement}
	}
	return measured, true
}

// estimateMeasuredToolDefinitionsPromptTokens is equivalent to estimating the
// JSON encoding of a []llm.ToolDef. It accounts for the opening/closing array
// delimiters and commas that json.Marshal adds around independently measured
// definitions. Keeping this exact matters because admission is a safety
// boundary, not a lossy optimization.
func estimateMeasuredToolDefinitionsPromptTokens(defs []measuredToolDef) int {
	if len(defs) == 0 {
		return 0
	}
	asciiBytes := len(defs) + 1 // '[' + ']' + one comma between each pair
	nonASCIIBytes := 0
	for _, def := range defs {
		asciiBytes += def.measurement.asciiBytes
		nonASCIIBytes += def.measurement.nonASCIIBytes
	}
	return (asciiBytes+3)/4 + nonASCIIBytes
}

// admitToolDefsForSchemaBudget is deterministic for a given catalog and hint.
// It greedily admits exact encoded schemas in priority order and never edits a
// definition. Provider-visible names therefore retain their registry identity.
func admitToolDefsForSchemaBudget(defs []llm.ToolDef, hint *capabilityadvisor.Hint, budget int, trustedMCPHubNamespaces map[string]struct{}) []llm.ToolDef {
	if len(defs) == 0 || budget <= 0 {
		return nil
	}
	measured, ok := measureToolDefinitions(defs)
	if !ok {
		// Preserve the existing fail-open estimator behavior for an invalid tool
		// definition. The legacy path uses the slice-level marshal and therefore
		// retains its historical choice when that marshal cannot be measured.
		return admitToolDefsForSchemaBudgetLegacy(defs, hint, budget, trustedMCPHubNamespaces)
	}
	if estimateMeasuredToolDefinitionsPromptTokens(measured) <= budget {
		return append([]llm.ToolDef(nil), defs...)
	}

	ranked := make([]rankedToolDef, 0, len(defs))
	for index, measuredDef := range measured {
		priority, rank := toolAdmissionPriority(measuredDef.def.Name, hint, trustedMCPHubNamespaces)
		ranked = append(ranked, rankedToolDef{
			def:         measuredDef.def,
			measurement: measuredDef.measurement,
			priority:    priority,
			rank:        rank,
			index:       index,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].priority != ranked[j].priority {
			return ranked[i].priority < ranked[j].priority
		}
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank < ranked[j].rank
		}
		if ranked[i].def.Name != ranked[j].def.Name {
			return ranked[i].def.Name < ranked[j].def.Name
		}
		return ranked[i].index < ranked[j].index
	})

	// Keep the lazy gateway usable as a complete path rather than admitting a
	// large local prefix and stranding the model with only one half of
	// resolve -> call. The bootstrap is atomic: it is seeded only when the
	// exact schemas for local inspection plus both gateway operations fit.
	// Definitions are selected verbatim from the authorized turn catalog.
	admittedMeasured := lazyGatewayBootstrapForMeasuredBudget(measured, budget, trustedMCPHubNamespaces)
	admitted := make([]llm.ToolDef, 0, len(admittedMeasured))
	admittedNames := make(map[string]struct{}, len(admittedMeasured))
	for _, measuredDef := range admittedMeasured {
		admitted = append(admitted, measuredDef.def)
		admittedNames[measuredDef.def.Name] = struct{}{}
	}
	for _, candidate := range ranked {
		if _, alreadyAdmitted := admittedNames[candidate.def.Name]; alreadyAdmitted {
			continue
		}
		nextMeasured := append(admittedMeasured, measuredToolDef{def: candidate.def, measurement: candidate.measurement})
		if estimateMeasuredToolDefinitionsPromptTokens(nextMeasured) <= budget {
			admitted = append(admitted, candidate.def)
			admittedMeasured = nextMeasured
			admittedNames[candidate.def.Name] = struct{}{}
		}
	}
	return admitted
}

// admitToolDefsForSchemaBudgetLegacy is retained only for definitions that
// cannot be encoded individually. That is already outside the ordinary MCP
// contract, but preserving the old behavior avoids changing admission safety
// for malformed test doubles or future provider extensions.
func admitToolDefsForSchemaBudgetLegacy(defs []llm.ToolDef, hint *capabilityadvisor.Hint, budget int, trustedMCPHubNamespaces map[string]struct{}) []llm.ToolDef {
	if estimateToolDefinitionsPromptTokens(defs) <= budget {
		return append([]llm.ToolDef(nil), defs...)
	}
	ranked := make([]rankedToolDef, 0, len(defs))
	for index, def := range defs {
		priority, rank := toolAdmissionPriority(def.Name, hint, trustedMCPHubNamespaces)
		ranked = append(ranked, rankedToolDef{def: def, priority: priority, rank: rank, index: index})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].priority != ranked[j].priority {
			return ranked[i].priority < ranked[j].priority
		}
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank < ranked[j].rank
		}
		if ranked[i].def.Name != ranked[j].def.Name {
			return ranked[i].def.Name < ranked[j].def.Name
		}
		return ranked[i].index < ranked[j].index
	})

	admitted := lazyGatewayBootstrapForBudgetLegacy(defs, budget, trustedMCPHubNamespaces)
	admittedNames := make(map[string]struct{}, len(admitted))
	for _, def := range admitted {
		admittedNames[def.Name] = struct{}{}
	}
	for _, candidate := range ranked {
		if _, alreadyAdmitted := admittedNames[candidate.def.Name]; alreadyAdmitted {
			continue
		}
		next := append(admitted, candidate.def)
		if estimateToolDefinitionsPromptTokens(next) <= budget {
			admitted = next
			admittedNames[candidate.def.Name] = struct{}{}
		}
	}
	return admitted
}

type lazyGatewayToolSet struct {
	resolve  measuredToolDef
	call     measuredToolDef
	optional map[string]measuredToolDef
}

// lazyGatewayBootstrapForBudget returns an exact-schema bootstrap for one
// gateway or nil when the complete required bundle cannot fit. It never emits
// a partial resolve/call pair. Optional discovery companions are appended in
// declared order only while the exact combined projection stays within budget.
func lazyGatewayBootstrapForBudget(defs []llm.ToolDef, budget int, trustedMCPHubNamespaces map[string]struct{}) []llm.ToolDef {
	measured, ok := measureToolDefinitions(defs)
	if !ok {
		return lazyGatewayBootstrapForBudgetLegacy(defs, budget, trustedMCPHubNamespaces)
	}
	bootstrap := lazyGatewayBootstrapForMeasuredBudget(measured, budget, trustedMCPHubNamespaces)
	result := make([]llm.ToolDef, 0, len(bootstrap))
	for _, def := range bootstrap {
		result = append(result, def.def)
	}
	return result
}

func lazyGatewayBootstrapForMeasuredBudget(defs []measuredToolDef, budget int, trustedMCPHubNamespaces map[string]struct{}) []measuredToolDef {
	if budget <= 0 {
		return nil
	}

	defsByName := make(map[string]measuredToolDef, len(defs))
	gateways := make(map[string]lazyGatewayToolSet)
	for _, def := range defs {
		if _, exists := defsByName[def.def.Name]; !exists {
			defsByName[def.def.Name] = def
		}
		gateway, operation, ok := mcpHubMetaIdentity(def.def.Name)
		if !ok {
			continue
		}
		set := gateways[gateway]
		if set.optional == nil {
			set.optional = make(map[string]measuredToolDef)
		}
		switch operation {
		case "mcphub_resolve_tool":
			set.resolve = def
		case "mcphub_call_tool":
			set.call = def
		default:
			set.optional[operation] = def
		}
		gateways[gateway] = set
	}

	gatewayNames := make([]string, 0, len(gateways))
	for gateway, set := range gateways {
		if set.resolve.def.Name != "" && set.call.def.Name != "" {
			gatewayNames = append(gatewayNames, gateway)
		}
	}
	sort.Strings(gatewayNames)

	for _, gateway := range gatewayNames {
		if _, trusted := trustedMCPHubNamespaces[gateway]; !trusted {
			continue
		}
		set := gateways[gateway]
		bootstrap := make([]measuredToolDef, 0, len(lazyGatewayBootstrapLocalTools)+2+len(lazyGatewayBootstrapOptionalOperations))
		for _, name := range lazyGatewayBootstrapLocalTools {
			if def, ok := defsByName[name]; ok {
				bootstrap = append(bootstrap, def)
			}
		}
		bootstrap = append(bootstrap, set.resolve, set.call)
		if estimateMeasuredToolDefinitionsPromptTokens(bootstrap) > budget {
			continue
		}
		for _, operation := range lazyGatewayBootstrapOptionalOperations {
			def, ok := set.optional[operation]
			if !ok {
				continue
			}
			next := append(bootstrap, def)
			if estimateMeasuredToolDefinitionsPromptTokens(next) <= budget {
				bootstrap = next
			}
		}
		return bootstrap
	}
	return nil
}

// lazyGatewayBootstrapForBudgetLegacy keeps the original slice-level marshal
// behavior for malformed definitions that cannot be measured independently.
func lazyGatewayBootstrapForBudgetLegacy(defs []llm.ToolDef, budget int, trustedMCPHubNamespaces map[string]struct{}) []llm.ToolDef {
	if budget <= 0 {
		return nil
	}

	defsByName := make(map[string]llm.ToolDef, len(defs))
	gateways := make(map[string]struct {
		resolve  llm.ToolDef
		call     llm.ToolDef
		optional map[string]llm.ToolDef
	})
	for _, def := range defs {
		if _, exists := defsByName[def.Name]; !exists {
			defsByName[def.Name] = def
		}
		gateway, operation, ok := mcpHubMetaIdentity(def.Name)
		if !ok {
			continue
		}
		set := gateways[gateway]
		if set.optional == nil {
			set.optional = make(map[string]llm.ToolDef)
		}
		switch operation {
		case "mcphub_resolve_tool":
			set.resolve = def
		case "mcphub_call_tool":
			set.call = def
		default:
			set.optional[operation] = def
		}
		gateways[gateway] = set
	}

	gatewayNames := make([]string, 0, len(gateways))
	for gateway, set := range gateways {
		if set.resolve.Name != "" && set.call.Name != "" {
			gatewayNames = append(gatewayNames, gateway)
		}
	}
	sort.Strings(gatewayNames)
	for _, gateway := range gatewayNames {
		if _, trusted := trustedMCPHubNamespaces[gateway]; !trusted {
			continue
		}
		set := gateways[gateway]
		bootstrap := make([]llm.ToolDef, 0, len(lazyGatewayBootstrapLocalTools)+2+len(lazyGatewayBootstrapOptionalOperations))
		for _, name := range lazyGatewayBootstrapLocalTools {
			if def, ok := defsByName[name]; ok {
				bootstrap = append(bootstrap, def)
			}
		}
		bootstrap = append(bootstrap, set.resolve, set.call)
		if estimateToolDefinitionsPromptTokens(bootstrap) > budget {
			continue
		}
		for _, operation := range lazyGatewayBootstrapOptionalOperations {
			def, ok := set.optional[operation]
			if !ok {
				continue
			}
			next := append(bootstrap, def)
			if estimateToolDefinitionsPromptTokens(next) <= budget {
				bootstrap = next
			}
		}
		return bootstrap
	}
	return nil
}

func toolAdmissionPriority(name string, hint *capabilityadvisor.Hint, trustedMCPHubNamespaces map[string]struct{}) (priority, rank int) {
	if localRank, ok := essentialLocalToolRank[name]; ok {
		return 0, localRank
	}
	if gateway, operation, ok := mcpHubMetaIdentity(name); ok && trustedMCPHubNamespace(trustedMCPHubNamespaces, gateway) {
		if metaRank, essential := essentialMCPHubMetaToolRank[operation]; essential {
			return 1, metaRank
		}
	}
	if matchesCapabilityRecommendation(name, hint) {
		return 2, 0
	}
	if _, ok := localToolNames[name]; ok {
		return 3, 0
	}
	if _, ok := memoryToolNames[name]; ok {
		return 4, 0
	}
	if gateway, operation, ok := mcpHubMetaIdentity(name); ok && trustedMCPHubNamespace(trustedMCPHubNamespaces, gateway) {
		if _, secondary := otherMCPHubMetaTools[operation]; secondary {
			return 5, 0
		}
	}
	return 6, 0
}

func trustedMCPHubNamespace(trustedMCPHubNamespaces map[string]struct{}, namespace string) bool {
	_, trusted := trustedMCPHubNamespaces[namespace]
	return trusted
}

func mcpHubMetaIdentity(name string) (gateway, operation string, ok bool) {
	parts := strings.Split(name, "__")
	if len(parts) != 2 || parts[0] == "" {
		return "", "", false
	}
	gateway, operation = parts[0], parts[1]
	if _, ok := essentialMCPHubMetaToolRank[operation]; ok {
		return gateway, operation, true
	}
	_, ok = otherMCPHubMetaTools[operation]
	return gateway, operation, ok
}

func matchesCapabilityRecommendation(name string, hint *capabilityadvisor.Hint) bool {
	if hint == nil || hint.Ambiguous || hint.Namespaced == "" {
		return false
	}
	if name == hint.Namespaced {
		return true
	}
	parts := strings.Split(name, "__")
	return len(parts) == 3 && parts[0] != "" && parts[1] == hint.Server && parts[2] == hint.Tool
}
