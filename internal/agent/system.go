package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

// Optional prompt budget shares (percentages of the optional budget).
// The shares should sum to ~100% across all optional sections.
const (
	budgetShareLoadedContextDefault     = 50 // loaded context when no skill catalog
	budgetShareLoadedContextWithCatalog = 40 // loaded context when skill catalog present
	budgetShareSkillCatalog             = 10
	budgetShareSkillContent             = 20
	budgetShareICEOrMemory              = 25
	budgetShareIgnoreContent            = 5
)

// gitProbeTimeout bounds filesystem and git probes during prompt assembly so
// a hung network mount or slow repository cannot stall the turn.
const gitProbeTimeout = 750 * time.Millisecond

const systemTemplate = `You are a helpful personal assistant running locally on the user's machine.
You have access to tools via MCP servers. You MUST use tools to accomplish tasks — do not guess or make up answers when a tool can provide the real information.
%s
Current date: %s
%s%s
%s%s%s
## Available Tools
%s
## Guidelines
- **ALWAYS use your tools** when the user asks you to read, explore, search, or modify files. You have filesystem tools — use them.
- When the user says "read this codebase" or similar, use list/read tools starting from the working directory shown above.
- Be concise and direct in your responses.
- When a tool call fails, explain what happened and suggest alternatives.
- For multi-step tasks, explain your plan briefly before executing.
- Format responses in markdown when it improves readability.
- If you're unsure about something, say so rather than guessing.
- Never fabricate tool results — always call the actual tool.
- Do NOT claim you cannot access files or the filesystem. You have tools for that — use them.
%s`

// smallModelTemplate is a more concise template for small models (0.8B, 2B).
const smallModelTemplate = `You are a local AI assistant. Use tools to read/write files and run commands.
%sDate: %s
%s%s
%s%s%s
## Tools
%s
Guidelines:
- Be concise and direct
- Use tools when needed to complete tasks
- If a tool fails, continue with available information
- Don't guess - use tools to verify
- You can complete tasks even if some tools fail
%s`

var modelSizePattern = regexp.MustCompile(`(?:^|[^0-9.])(\d+(?:\.\d+)?)b(?:$|[^a-z0-9])`)

// A network-backed workspace can wedge an otherwise harmless metadata stat in
// the kernel. Bound abandonment globally: one stuck probe may remain, while
// every current and future turn still observes context cancellation.
var projectInfoProbeSlots = make(chan struct{}, 1)

// isSmallModel returns true if the model name indicates a small model (<=2B parameters).
func isSmallModel(modelName string) bool {
	match := modelSizePattern.FindStringSubmatch(strings.ToLower(modelName))
	if len(match) != 2 {
		return false
	}
	size, err := strconv.ParseFloat(match[1], 64)
	return err == nil && size <= 2
}

// buildSystemPrompt generates the system prompt with current tool info,
// systemPromptOptions carries every optional prompt-assembly parameter.
// Zero values are safe: nil slices, empty strings, and 0 budget all produce
// the same output as the legacy wrapper chain's defaults.
type systemPromptOptions struct {
	ModePrefix     string
	Tools          []llm.ToolDef
	SkillContent   string
	SkillCatalog   string
	LoadedContext  string
	MemStore       *memory.Store
	ICEContext     string
	WorkDir        string
	IgnoreContent  string
	ModelName      string
	NumCtx         int
	ReadGrants     []ReadGrant
	WriteGrants    []WriteGrant
	OptionalBudget int // 0 → derive from NumCtx via optionalPromptBudget()
}

// buildSystemPrompt assembles the system prompt from the given options.
// It optimizes for small models if isSmallModel(opts.ModelName) is true.
// OptionalBudget applies only to bounded optional text (skills, loaded
// instructions, ICE, and ignores); it never modifies the tool definitions
// supplied separately to the provider or the host's authority model.
func buildSystemPrompt(ctx context.Context, opts systemPromptOptions) string {
	return formatSystemPrompt(opts.ModelName, resolveBoundedPromptSections(ctx, opts))
}

// boundedPromptSections holds every assembled system-prompt section after
// optional-budget bounding, in template order. buildSystemPrompt formats them
// and ContextBreakdown measures them, so the reported sizes are the exact
// bytes the provider receives.
type boundedPromptSections struct {
	ModePrefixSection string
	EnvSection        string
	IgnoreSection     string
	SkillSection      string
	CtxSection        string
	MemorySection     string
	ToolList          string
	MemoryGuidelines  string
}

func resolveBoundedPromptSections(ctx context.Context, opts systemPromptOptions) boundedPromptSections {
	budget := opts.OptionalBudget
	if budget == 0 {
		budget = optionalPromptBudget(opts.NumCtx)
	}
	modePrefix := opts.ModePrefix
	tools := opts.Tools
	skillContent := opts.SkillContent
	skillCatalog := opts.SkillCatalog
	loadedContext := opts.LoadedContext
	memStore := opts.MemStore
	iceContext := opts.ICEContext
	workDir := opts.WorkDir
	ignoreContent := opts.IgnoreContent
	readGrants := opts.ReadGrants
	writeGrants := opts.WriteGrants

	if budget > 0 {
		loadedContextShare := budgetShareLoadedContextDefault
		if skillCatalog != "" {
			loadedContextShare = budgetShareLoadedContextWithCatalog
			skillCatalog = boundPromptText(skillCatalog, budget*budgetShareSkillCatalog/100)
		}
		loadedContext = boundPromptText(loadedContext, budget*loadedContextShare/100)
		skillContent = boundPromptText(skillContent, budget*budgetShareSkillContent/100)
		iceContext = boundPromptText(iceContext, budget*budgetShareICEOrMemory/100)
		ignoreContent = boundPromptText(ignoreContent, budget*budgetShareIgnoreContent/100)
	}

	toolList := nativeToolPromptSummary(tools)
	if hasLazyMCPHubTools(tools) {
		toolList = strings.TrimSpace(toolList) + "\n\n" + lazyMCPHubSystemGuidance() + "\n"
	}

	envSection := buildEnvironmentSectionContextWithPathGrants(ctx, workDir, readGrants, writeGrants)

	var skillSection string
	if skillContent != "" {
		skillSection = fmt.Sprintf("\n## Active Skills\n%s\n", skillContent)
	}
	if skillCatalog != "" {
		skillSection += skillCatalog
	}

	var ctxSection string
	if loadedContext != "" {
		ctxSection = fmt.Sprintf("\n## Loaded Context\n%s\n", loadedContext)
	}

	var memorySection string
	if iceContext != "" {
		memorySection = iceContext
	} else if memStore != nil {
		memorySection = buildMemorySection(memStore)
	}
	if budget > 0 && iceContext == "" {
		memorySection = boundPromptText(memorySection, budget*budgetShareICEOrMemory/100)
	}

	// A project memory store may still contribute bounded remembered context when
	// the active mode does not grant memory-tool authority. Keep those two
	// concerns separate: instructions may name only definitions that this exact
	// provider turn advertises.
	memoryGuidelines := buildMemoryGuidelines(tools)

	var ignoreSection string
	if ignoreContent != "" {
		ignoreSection = fmt.Sprintf("\n## Ignored Paths\nThe following paths/patterns should be excluded from file operations:\n%s\n", ignoreContent)
	}

	var modePrefixSection string
	if modePrefix != "" {
		modePrefixSection = "\n" + modePrefix + "\n"
	}

	return boundedPromptSections{
		ModePrefixSection: modePrefixSection,
		EnvSection:        envSection,
		IgnoreSection:     ignoreSection,
		SkillSection:      skillSection,
		CtxSection:        ctxSection,
		MemorySection:     memorySection,
		ToolList:          toolList,
		MemoryGuidelines:  memoryGuidelines,
	}
}

func formatSystemPrompt(modelName string, sections boundedPromptSections) string {
	template := systemTemplate
	if isSmallModel(modelName) {
		template = smallModelTemplate
	}
	dateStr := time.Now().Format("Monday, January 2, 2006")
	return fmt.Sprintf(template,
		sections.ModePrefixSection,
		dateStr,
		sections.EnvSection,
		sections.IgnoreSection,
		sections.SkillSection,
		sections.CtxSection,
		sections.MemorySection,
		sections.ToolList,
		sections.MemoryGuidelines,
	)
}

func hasLazyMCPHubTools(tools []llm.ToolDef) bool {
	for _, tool := range tools {
		name := tool.Name
		if strings.HasSuffix(name, "__mcphub_call_tool") ||
			strings.HasSuffix(name, "__mcphub_resolve_tool") ||
			strings.HasSuffix(name, "__mcphub_describe_tool") ||
			strings.HasSuffix(name, "__mcphub_search_tools") {
			return true
		}
	}
	return false
}

// nativeToolPromptSummary avoids paying for tool names and descriptions twice:
// provider requests already carry the exact native definitions and JSON
// schemas. The system prompt states only the bounded contract for using them.
func nativeToolPromptSummary(tools []llm.ToolDef) string {
	if len(tools) == 0 {
		return "No tools currently available.\n"
	}
	noun := "tools"
	if len(tools) == 1 {
		noun = "tool"
	}
	return fmt.Sprintf(
		"%d %s available through the native tool schemas attached to this request. Use only those advertised names and argument schemas; do not invent tools or parameters.\n",
		len(tools), noun,
	)
}

// buildMemoryGuidelines describes only the exact built-in memory definitions
// advertised to the current provider turn. A store can exist while Ask, Plan,
// or a narrowed headless policy withholds some or all memory tools; naming a
// withheld tool encourages invalid calls and makes the prompt contradict its
// own tool schema.
func buildMemoryGuidelines(tools []llm.ToolDef) string {
	available := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if memory.IsBuiltinTool(tool.Name) {
			available[tool.Name] = struct{}{}
		}
	}
	if len(available) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n## Memory Guidelines\n")
	if _, ok := available["memory_save"]; ok {
		b.WriteString("- Use memory_save for important durable user preferences, project facts, and key decisions; do not save trivial or session-specific information.\n")
	}
	if _, ok := available["memory_recall"]; ok {
		b.WriteString("- Use memory_recall to look up previously saved information when relevant.\n")
	}
	if _, ok := available["memory_list"]; ok {
		b.WriteString("- Use memory_list to inspect stored memories and their IDs.\n")
	}
	if _, ok := available["memory_update"]; ok {
		b.WriteString("- Use memory_update to correct an existing memory by ID.\n")
	}
	if _, ok := available["memory_delete"]; ok {
		b.WriteString("- Use memory_delete to remove an existing memory by ID.\n")
	}
	return b.String()
}

// optionalPromptBudget bounds context that is useful but non-authoritative:
// loaded instructions, active skills, the on-demand skill catalog, ICE, and
// ignored paths. Tool schemas remain complete provider definitions and are
// budgeted separately by turn admission. On constrained local windows (16K or
// below), keeping this optional material to roughly 3/16 of the window leaves
// room for complete schemas, conversation, and generation on first turn.
func optionalPromptBudget(numCtx int) int {
	if numCtx <= 0 {
		return 0
	}
	if numCtx <= 16*1024 {
		budget := numCtx * 3 / 4
		if budget < 2048 {
			return 2048
		}
		return budget
	}
	budget := numCtx * 4 / 3
	if budget < 4096 {
		return 4096
	}
	if budget > 64*1024 {
		return 64 * 1024
	}
	return budget
}

func boundPromptText(text string, maxRunes int) string {
	if text == "" || maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	marker := []rune(fmt.Sprintf("\n... [%d context characters omitted] ...\n", len(runes)-maxRunes))
	if len(marker) >= maxRunes {
		return string(runes[:maxRunes])
	}
	available := maxRunes - len(marker)
	head := available * 3 / 4
	tail := available - head
	return string(runes[:head]) + string(marker) + string(runes[len(runes)-tail:])
}

func buildEnvironmentSectionContextWithReadGrants(ctx context.Context, workDir string, readGrants []ReadGrant) string {
	return buildEnvironmentSectionContextWithPathGrants(ctx, workDir, readGrants, nil)
}

func buildEnvironmentSectionContextWithPathGrants(ctx context.Context, workDir string, readGrants []ReadGrant, writeGrants []WriteGrant) string {
	if workDir == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n## Environment\n")
	fmt.Fprintf(&b, "Working directory: %s\n", strconv.QuoteToGraphic(workDir))
	b.WriteString("Filesystem authority: the working directory is the writable workspace.\n")
	if len(readGrants) == 0 {
		b.WriteString("Additional temporary read grants: none. Unlisted external paths are unavailable.\n")
	} else {
		b.WriteString("Additional temporary read grants (read-only unless the same path is separately listed for typed write):\n")
		for _, grant := range readGrants {
			kind := "directory"
			if grant.Kind == ReadGrantExactFile {
				kind = "exact file only; siblings remain unavailable"
			}
			fmt.Fprintf(&b, "- %s: %s\n", kind, strconv.QuoteToGraphic(grant.Path))
		}
	}
	if len(writeGrants) == 0 {
		b.WriteString("Additional temporary write grants: none.\n")
	} else {
		b.WriteString("Additional temporary typed-write grants (expire when this turn settles):\n")
		for _, grant := range writeGrants {
			kind := "directory; built-in write/edit/mkdir and exact trusted workspace tools only"
			if grant.Kind == WriteGrantExactFile {
				kind = "exact file only; built-in write/edit only; siblings remain unavailable"
			}
			fmt.Fprintf(&b, "- %s: %s\n", kind, strconv.QuoteToGraphic(grant.Path))
		}
		b.WriteString("Shell never receives these paths. Do not retry denied typed access through bash, cat/sed/cp, redirection, temp files, remove, or move; prefer an exact product CLI/MCP operation.\n")
	}

	// Auto-detect project type from marker files.
	if info := detectProjectInfoContext(ctx, workDir); info != "" {
		b.WriteString(info)
	}

	// Git context
	if gitInfo := detectGitInfoContext(ctx, workDir); gitInfo != "" {
		b.WriteString(gitInfo)
	}

	return b.String()
}

// detectProjectInfo looks for common project marker files and returns a brief description.
func detectProjectInfo(workDir string) string {
	markers := []struct {
		file string
		desc string
	}{
		{"go.mod", "Go module"},
		{"package.json", "Node.js/JavaScript"},
		{"Cargo.toml", "Rust"},
		{"pyproject.toml", "Python"},
		{"setup.py", "Python"},
		{"Makefile", ""},
		{"Taskfile.yml", ""},
	}

	var found []string
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(workDir, m.file)); err == nil {
			if m.desc != "" {
				found = append(found, fmt.Sprintf("%s (%s)", m.file, m.desc))
			} else {
				found = append(found, m.file)
			}
		}
	}

	if len(found) == 0 {
		return ""
	}

	return fmt.Sprintf("Project markers: %s\n", strings.Join(found, ", "))
}

func detectProjectInfoContext(ctx context.Context, workDir string) string {
	select {
	case projectInfoProbeSlots <- struct{}{}:
	case <-ctx.Done():
		return ""
	default:
		// Metadata is optional. If a previous network-filesystem syscall is
		// abandoned, do not make every later turn wait for its own cancellation.
		return ""
	}
	done := make(chan string, 1)
	go func() {
		defer func() { <-projectInfoProbeSlots }()
		done <- detectProjectInfo(workDir)
	}()
	select {
	case info := <-done:
		return info
	case <-ctx.Done():
		return ""
	}
}

// detectGitInfoContext returns bounded git branch and status information.
func detectGitInfoContext(ctx context.Context, workDir string) string {
	probeCtx, cancel := context.WithTimeout(ctx, gitProbeTimeout)
	defer cancel()
	if runGitCommandContext(probeCtx, workDir, "rev-parse", "--is-inside-work-tree") != "true" {
		return ""
	}

	var b strings.Builder

	// Get current branch
	branch := runGitCommandContext(probeCtx, workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if branch != "" {
		fmt.Fprintf(&b, "Git branch: %s\n", branch)
	}

	// Get status (short format)
	status := runGitCommandContext(probeCtx, workDir, "status", "--porcelain")
	if status != "" {
		lines := strings.Split(strings.TrimSpace(status), "\n")
		var modified, added, deleted int
		for _, line := range lines {
			if len(line) >= 2 {
				switch line[0] {
				case 'M', 'm':
					modified++
				case 'A':
					added++
				case 'D':
					deleted++
				}
			}
		}
		if modified > 0 || added > 0 || deleted > 0 {
			statusParts := []string{}
			if modified > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d modified", modified))
			}
			if added > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d added", added))
			}
			if deleted > 0 {
				statusParts = append(statusParts, fmt.Sprintf("%d deleted", deleted))
			}
			fmt.Fprintf(&b, "Git status: %s\n", strings.Join(statusParts, ", "))
		}
	}

	// Get recent commits (last 3)
	recentLog := runGitCommandContext(probeCtx, workDir, "log", "-3", "--oneline")
	if recentLog != "" {
		b.WriteString("Recent commits:\n")
		for _, line := range strings.Split(strings.TrimSpace(recentLog), "\n") {
			fmt.Fprintf(&b, "  - %s\n", line)
		}
	}

	if b.Len() == 0 {
		return ""
	}

	return b.String()
}

// runGitCommandContext runs a bounded git command and returns trimmed output.
func runGitCommandContext(ctx context.Context, dir string, args ...string) string {
	if err := ctx.Err(); err != nil {
		return ""
	}
	commandCtx, cancel := context.WithTimeout(ctx, gitProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "git", args...)
	configureCommandProcessGroup(cmd)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// buildMemorySection creates the remembered facts section from recent memories.
func buildMemorySection(store *memory.Store) string {
	if store.Count() == 0 {
		return ""
	}

	recent, err := store.Recent(10)
	if err != nil || len(recent) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n## Remembered Facts\n")
	for _, mem := range recent {
		fmt.Fprintf(&b, "- %s", mem.Content)
		if len(mem.Tags) > 0 {
			fmt.Fprintf(&b, " [tags: %s]", strings.Join(mem.Tags, ", "))
		}
		b.WriteString("\n")
	}

	return b.String()
}
