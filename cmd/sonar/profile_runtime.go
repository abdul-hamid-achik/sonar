package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/safeio"
	"github.com/abdul-hamid-achik/sonar/internal/skill"
)

const maxProjectInstructionsBytes int64 = 1 << 20

const (
	maxHostProjectionPathRunes = 1024
	maxHostProjectionNameRunes = 128
	maxHostProjectionServers   = 32
)

var projectInstructionsReader = safeio.NewReader()
var projectInstructionsReadTimeout = safeio.StartupReadTimeout

func buildBaseLoadedContext(agentsDir *config.AgentsDir) (string, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve project instructions directory: %w", err)
	}
	return buildBaseLoadedContextAt(agentsDir, workDir)
}

func buildBaseLoadedContextAt(agentsDir *config.AgentsDir, workDir string) (string, error) {
	var parts []string
	if agentsDir != nil && agentsDir.GetGlobalInstructions() != "" {
		parts = append(parts, agentsDir.GetGlobalInstructions())
	}
	// AGENTS.md is the cross-harness convention. Keep AGENT.md as a legacy
	// fallback so existing sonar projects continue to work.
	for _, name := range []string{"AGENTS.md", "AGENT.md"} {
		path := filepath.Join(workDir, name)
		data, err := projectInstructionsReader.ReadRegularFileNoFollow(path, maxProjectInstructionsBytes, projectInstructionsReadTimeout)
		if err == nil {
			parts = append(parts, string(data))
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read project instructions %s: %w", path, err)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// buildHostConfigProjection gives the model the small set of configuration
// facts the host already validated. It intentionally excludes server args,
// environment values, remote URLs, model credentials, and file contents. The
// filesystem tools remain workspace-confined; models should not probe HOME to
// rediscover these facts.
func buildHostConfigProjection(cfg *config.Config, agentsDir *config.AgentsDir, servers []config.ServerConfig) string {
	if cfg == nil {
		return ""
	}
	configSource := cfg.SourcePath
	if configSource == "" {
		configSource = "built-in defaults"
	} else {
		configSource = strconv.Quote(boundHostProjectionField(configSource, maxHostProjectionPathRunes))
	}

	agentsSource := "not loaded"
	profiles, skills := 0, 0
	globalInstructions := false
	if agentsDir != nil {
		agentsSource = agentsDir.Path
		if agentsSource == "" {
			agentsSource = "loaded without an on-disk directory"
		} else {
			agentsSource = strconv.Quote(boundHostProjectionField(agentsSource, maxHostProjectionPathRunes))
		}
		profiles = len(agentsDir.Agents)
		skills = len(agentsDir.Skills)
		globalInstructions = agentsDir.GlobalInstructions != ""
	}

	serverSummaries := make([]string, 0, len(servers))
	for _, server := range servers {
		name := strings.TrimSpace(server.Name)
		if name == "" {
			continue
		}
		transport := strings.TrimSpace(server.Transport)
		if transport == "" {
			transport = "stdio"
		}
		details := []string{transport}
		if filepath.Base(server.Command) == "mcphub" && len(server.Args) >= 2 && server.Args[0] == "mcp" && server.Args[1] == "serve" {
			details = append(details, "gateway")
			for i := 0; i+1 < len(server.Args); i++ {
				if server.Args[i] == "--agent" && strings.TrimSpace(server.Args[i+1]) != "" {
					details = append(details, "scoped agent route")
					break
				}
			}
		}
		serverSummaries = append(serverSummaries, fmt.Sprintf("%s (%s)", strconv.Quote(boundHostProjectionField(name, maxHostProjectionNameRunes)), strings.Join(details, ", ")))
	}
	sort.Strings(serverSummaries)
	if len(serverSummaries) > maxHostProjectionServers {
		omitted := len(serverSummaries) - maxHostProjectionServers
		serverSummaries = append(serverSummaries[:maxHostProjectionServers], fmt.Sprintf("... (%d more configured endpoints)", omitted))
	}
	serverText := "none"
	if len(serverSummaries) > 0 {
		serverText = strings.Join(serverSummaries, ", ")
	}

	return fmt.Sprintf(`## Host Configuration Projection
These facts were loaded and validated by the host. Do not use filesystem tools to inspect the config or agent-metadata directories; they intentionally remain outside the workspace boundary. This projection contains no server arguments, environment values, secret values, remote URLs, or config file contents.
- Active config: %s
- Config precedence: repository local, then XDG_CONFIG_HOME, then ~/.config
- Local-only privacy: %t
- Agent metadata: %s (profiles: %d, skills: %d, global instructions: %t)
- Configured MCP endpoints: %s
- MCP discovery: a gateway may intentionally advertise only management tools and pins; follow its MCP server guidance to discover lazy tools.`,
		configSource,
		cfg.Privacy.LocalOnly,
		agentsSource,
		profiles,
		skills,
		globalInstructions,
		serverText,
	)
}

func boundHostProjectionField(value string, maxRunes int) string {
	runes := []rune(strings.ToValidUTF8(value, "�"))
	if maxRunes <= 0 {
		return ""
	}
	if len(runes) <= maxRunes {
		return string(runes)
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

func appendLoadedContext(base, projection string) string {
	base = strings.TrimSpace(base)
	projection = strings.TrimSpace(projection)
	switch {
	case base == "":
		return projection
	case projection == "":
		return base
	default:
		return base + "\n\n" + projection
	}
}

func applyInitialAgentProfile(ag *agent.Agent, skillMgr *skill.Manager, modelManager *llm.ModelManager, agentsDir *config.AgentsDir, baseLoadedContext, profileName string) error {
	if profileName == "" {
		if ag != nil {
			ag.SetLoadedContext(baseLoadedContext)
			if skillMgr != nil {
				ag.SetSkillContent(skillMgr.ActiveContent())
			}
		}
		return nil
	}
	if agentsDir == nil {
		return fmt.Errorf("agent profile %q requested but no profile directory is available", profileName)
	}

	profile := agentsDir.GetAgent(profileName)
	if profile == nil {
		return fmt.Errorf("unknown agent profile: %s", profileName)
	}
	if skillMgr != nil {
		for _, skillName := range profile.Skills {
			if !skillMgr.Has(skillName) {
				return fmt.Errorf("profile skill %q is not available", skillName)
			}
		}
	}

	if profile.Model != "" && modelManager != nil {
		if err := config.CheckModelMemorySafe(profile.Model); err != nil {
			return fmt.Errorf("profile model: %w", err)
		}
		if err := modelManager.SetCurrentModel(profile.Model); err != nil {
			return fmt.Errorf("set profile model: %w", err)
		}
	}
	if skillMgr != nil {
		for _, skillName := range profile.Skills {
			if err := skillMgr.Activate(skillName); err != nil {
				return fmt.Errorf("activate profile skill %q: %w", skillName, err)
			}
		}
	}

	loadedContext := baseLoadedContext
	if profile.SystemPrompt != "" {
		if loadedContext != "" {
			loadedContext += "\n\n"
		}
		loadedContext += profile.SystemPrompt
	}

	if ag != nil {
		ag.SetMCPServerScope(profile.MCPServers)
		ag.SetLoadedContext(loadedContext)
		if skillMgr != nil {
			ag.SetSkillContent(skillMgr.ActiveContent())
		}
	}

	return nil
}
