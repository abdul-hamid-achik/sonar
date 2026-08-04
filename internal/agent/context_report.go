package agent

import (
	"context"
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// ContextSection is one bounded slice of the estimated next-turn prompt.
type ContextSection struct {
	Key    string
	Label  string
	Tokens int
	Detail string
}

// ContextBreakdown reports where the next turn's prompt budget would go under
// the current mode policy, before per-turn additions (ICE assembly, capability
// hints, continuation previews) that only exist inside a running turn. Token
// counts use the same admission estimators as the turn budget, so the numbers
// match what admitSystemPrompt would gate against.
type ContextBreakdown struct {
	Model       string
	NumCtx      int
	TotalTokens int
	Sections    []ContextSection
}

// ContextBreakdown measures the prompt the next turn would assemble. It runs
// the same bounded environment probes as prompt assembly, so callers should
// treat it as a short blocking operation and invoke it off the UI thread.
func (a *Agent) ContextBreakdown(ctx context.Context) ContextBreakdown {
	if a == nil {
		return ContextBreakdown{}
	}
	numCtx := a.NumCtx()
	a.mu.RLock()
	client := a.llmClient
	a.mu.RUnlock()
	model := ""
	if client != nil {
		model = client.Model()
	}
	modePrefix, policy := a.modeContext()

	var tools []llm.ToolDef
	mcpToolCount := 0
	if policy.AllowMCP && a.registry != nil {
		snapshot := a.mcpToolSnapshot()
		tools = append(tools, snapshot.Tools...)
		mcpToolCount = len(snapshot.Tools)
	}

	a.mu.RLock()
	memStore := a.memoryStore
	skillContent := a.skillContent
	loadedContext := a.loadedCtx
	workDir := a.workDir
	ignoreContent := a.ignoreContent
	messageCount := len(a.messages)
	messageTokens := estimateMessagesPromptTokens(a.messages)
	a.mu.RUnlock()

	if memStore != nil {
		tools = append(tools, filterToolDefsByName(a.memoryBuiltinToolDefs(), policy.memoryTools)...)
	}
	tools = append(tools, filterToolDefsByName(a.toolsBuiltinToolDefs(), policy.localTools)...)

	sections := resolveBoundedPromptSections(ctx, systemPromptOptions{
		ModePrefix:    modePrefix,
		Tools:         tools,
		SkillContent:  skillContent,
		SkillCatalog:  a.skillCatalogPrompt(),
		LoadedContext: loadedContext,
		MemStore:      memStore,
		WorkDir:       workDir,
		IgnoreContent: ignoreContent,
		ModelName:     model,
		NumCtx:        numCtx,
		ReadGrants:    a.ReadGrants(),
		WriteGrants:   a.WriteGrants(),
	})

	systemTokens := estimateTextPromptTokens(formatSystemPrompt(model, sections))
	toolTokens := estimateToolDefinitionsPromptTokens(tools)

	envTokens := estimateTextPromptTokens(sections.EnvSection)
	skillTokens := estimateTextPromptTokens(sections.SkillSection)
	loadedTokens := estimateTextPromptTokens(sections.CtxSection)
	memoryTokens := estimateTextPromptTokens(sections.MemorySection)
	ignoreTokens := estimateTextPromptTokens(sections.IgnoreSection)
	baseTokens := systemTokens - envTokens - skillTokens - loadedTokens - memoryTokens - ignoreTokens
	if baseTokens < 0 {
		baseTokens = 0
	}

	report := ContextBreakdown{
		Model:  model,
		NumCtx: numCtx,
		// estimateHostPromptTokens charges one framing token beyond text+tools.
		TotalTokens: systemTokens + toolTokens + messageTokens + 1,
	}

	appendSection := func(key, label string, tokens int, detail string, alwaysShown bool) {
		if tokens <= 0 && !alwaysShown {
			return
		}
		report.Sections = append(report.Sections, ContextSection{
			Key:    key,
			Label:  label,
			Tokens: tokens,
			Detail: detail,
		})
	}

	appendSection("system", "System prompt", baseTokens, "", true)
	appendSection("environment", "Environment", envTokens, "", true)
	appendSection("skills", "Skills", skillTokens, "", false)
	appendSection("context", "Loaded context", loadedTokens, "", false)
	appendSection("memory", "Memory", memoryTokens, "", false)
	appendSection("ignore", "Ignored paths", ignoreTokens, "", false)
	appendSection("tools", "Tool schemas", toolTokens,
		fmt.Sprintf("%d MCP · %d local", mcpToolCount, len(tools)-mcpToolCount), true)
	messageNoun := "messages"
	if messageCount == 1 {
		messageNoun = "message"
	}
	appendSection("conversation", "Conversation", messageTokens,
		fmt.Sprintf("%d %s", messageCount, messageNoun), true)
	return report
}
