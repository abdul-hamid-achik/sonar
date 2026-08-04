package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
	"gopkg.in/yaml.v3"
)

type AgentsDir struct {
	Path               string
	Agents             map[string]AgentProfile
	MCPServers         []ServerConfig
	GlobalInstructions string
	Skills             []SkillDef
}

type AgentProfile struct {
	Name         string   `yaml:"name" json:"name"`
	Description  string   `yaml:"description" json:"description"`
	Model        string   `yaml:"model" json:"model"`
	Skills       []string `yaml:"skills" json:"skills"`
	MCPServers   []string `yaml:"mcp_servers" json:"mcp_servers"`
	SystemPrompt string   `yaml:"system_prompt" json:"system_prompt"`
	UseCases     []string `yaml:"use_cases" json:"use_cases"`
}

type SkillDef struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Path        string `yaml:"path" json:"path"`
}

type MCPConfig struct {
	Servers    []ServerConfig `json:"servers,omitempty"`
	MCPServers []ServerConfig `json:"mcp_servers,omitempty"`
}

func (c MCPConfig) resolvedServers() []ServerConfig {
	if len(c.Servers) > 0 {
		return c.Servers
	}
	return c.MCPServers
}

type ModelsConfig struct {
	Models        []Model  `yaml:"models,omitempty"`
	DefaultModel  string   `yaml:"default_model,omitempty"`
	FallbackChain []string `yaml:"fallback_chain,omitempty"`
	AutoSelect    bool     `yaml:"auto_select,omitempty"`
	EmbedModel    string   `yaml:"embed_model,omitempty"`
}

var agentsMetadataReader = safeio.NewReader()

func FindAgentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	candidates := []string{
		filepath.Join(home, ".agents"),
		filepath.Join(home, ".config", "agents"),
	}

	for _, dir := range candidates {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}

	return ""
}

func FindAgentsDirWithCreate() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}

	dirs := []string{
		filepath.Join(home, ".agents"),
		filepath.Join(home, ".config", "agents"),
	}

	for _, dir := range dirs {
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
	}

	if err := os.MkdirAll(dirs[0], 0755); err != nil {
		return "", fmt.Errorf("create agents dir: %w", err)
	}

	return dirs[0], nil
}

func LoadAgentsDir(path string) (*AgentsDir, error) {
	if path == "" {
		path = FindAgentsDir()
		if path == "" {
			return &AgentsDir{
				Path:   "",
				Agents: make(map[string]AgentProfile),
			}, nil
		}
	}

	dir := &AgentsDir{
		Path:   path,
		Agents: make(map[string]AgentProfile),
	}

	if err := dir.loadAgents(path); err != nil {
		return nil, fmt.Errorf("load agents: %w", err)
	}

	if err := dir.loadMCP(path); err != nil {
		return nil, fmt.Errorf("load MCP: %w", err)
	}

	if err := dir.loadGlobalInstructions(path); err != nil {
		return nil, fmt.Errorf("load instructions: %w", err)
	}

	if err := dir.loadSkills(path); err != nil {
		return nil, fmt.Errorf("load skills: %w", err)
	}

	return dir, nil
}

func (d *AgentsDir) loadAgents(path string) error {
	agentsDir := filepath.Join(path, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			var agentPath string
			var data []byte
			for _, name := range []string{"agent.yaml", "agent.md"} {
				candidate := filepath.Join(agentsDir, entry.Name(), name)
				candidateData, readErr := agentsMetadataReader.ReadRegularFileNoFollow(candidate, maxStartupConfigBytes, safeio.StartupReadTimeout)
				if readErr == nil {
					agentPath = candidate
					data = candidateData
					break
				}
				if !errors.Is(readErr, os.ErrNotExist) {
					return fmt.Errorf("read agent profile %s: %w", candidate, readErr)
				}
			}
			if agentPath == "" {
				continue
			}

			var profile AgentProfile
			if err := yaml.Unmarshal(data, &profile); err != nil {
				continue
			}

			if profile.Name == "" {
				profile.Name = entry.Name()
			}

			d.Agents[profile.Name] = profile
		}
	}

	return nil
}

func (d *AgentsDir) loadMCP(path string) error {
	mcpPath := filepath.Join(path, "mcp.json")
	data, err := agentsMetadataReader.ReadRegularFileNoFollow(mcpPath, maxStartupConfigBytes, safeio.StartupReadTimeout)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var mcpCfg MCPConfig
	if err := json.Unmarshal(data, &mcpCfg); err != nil {
		return fmt.Errorf("parse mcp.json: %w", err)
	}

	d.MCPServers = mcpCfg.resolvedServers()
	return nil
}

func (d *AgentsDir) loadGlobalInstructions(path string) error {
	paths := []string{
		filepath.Join(path, "agents.md"),
		filepath.Join(path, "instructions.md"),
	}

	for _, p := range paths {
		data, err := agentsMetadataReader.ReadRegularFileNoFollow(p, maxStartupConfigBytes, safeio.StartupReadTimeout)
		if err == nil {
			d.GlobalInstructions = string(data)
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read global instructions %s: %w", p, err)
		}
	}

	return nil
}

func (d *AgentsDir) loadSkills(path string) error {
	skillsDir := filepath.Join(path, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillDir := filepath.Join(skillsDir, entry.Name())

		// Try both SKILL.md and skill.md (case insensitive check)
		skillPath := ""
		var skillData []byte
		for _, name := range []string{"SKILL.md", "skill.md"} {
			candidate := filepath.Join(skillDir, name)
			data, readErr := agentsMetadataReader.ReadRegularFileNoFollow(candidate, maxStartupConfigBytes, safeio.StartupReadTimeout)
			if readErr == nil {
				skillPath = candidate
				skillData = data
				break
			}
			if !errors.Is(readErr, os.ErrNotExist) {
				return fmt.Errorf("read skill metadata %s: %w", candidate, readErr)
			}
		}

		if skillPath == "" {
			continue
		}

		d.Skills = append(d.Skills, SkillDef{
			Name:        entry.Name(),
			Description: extractDescription(string(skillData)),
			Path:        skillPath,
		})
	}

	return nil
}

func extractDescription(content string) string {
	for _, line := range splitLines(content) {
		line = trimWhitespace(line)
		if line == "" || startsWith(line, "#") {
			continue
		}
		return line
	}
	return ""
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, r := range s {
		if r == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}

func trimWhitespace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func (d *AgentsDir) GetAgent(name string) *AgentProfile {
	if agent, ok := d.Agents[name]; ok {
		return &agent
	}
	return nil
}

func (d *AgentsDir) ListAgents() []AgentProfile {
	agents := make([]AgentProfile, 0, len(d.Agents))
	for _, agent := range d.Agents {
		agents = append(agents, agent)
	}
	return agents
}

func (d *AgentsDir) GetSkills() []SkillDef {
	return d.Skills
}

func (d *AgentsDir) HasMCP() bool {
	return len(d.MCPServers) > 0
}

func (d *AgentsDir) GetMCPServers() []ServerConfig {
	return d.MCPServers
}

func (d *AgentsDir) GetGlobalInstructions() string {
	return d.GlobalInstructions
}

func CreateDefaultAgentsDir() error {
	dir, err := FindAgentsDirWithCreate()
	if err != nil {
		return err
	}

	subdirs := []string{"agents", "skills", "tasks", "memories"}
	for _, sub := range subdirs {
		path := filepath.Join(dir, sub)
		if _, err := os.Stat(path); err != nil {
			if err := os.MkdirAll(path, 0755); err != nil {
				return fmt.Errorf("create %s: %w", sub, err)
			}
		}
	}

	mcpPath := filepath.Join(dir, "mcp.json")
	if _, err := os.Stat(mcpPath); err != nil {
		defaultMCP := MCPConfig{
			Servers: []ServerConfig{},
		}
		data, _ := json.MarshalIndent(defaultMCP, "", "  ")
		if err := os.WriteFile(mcpPath, data, 0644); err != nil {
			return fmt.Errorf("write mcp.json: %w", err)
		}
	}

	agentsPath := filepath.Join(dir, "agents.md")
	if _, err := os.Stat(agentsPath); err != nil {
		defaultContent := `# Global Agent Instructions

You are a helpful local AI coding assistant.

## Guidelines
- Be concise and direct
- Explain your reasoning
- Ask for clarification when needed
- Never fabricate information
`
		if err := os.WriteFile(agentsPath, []byte(defaultContent), 0644); err != nil {
			return fmt.Errorf("write agents.md: %w", err)
		}
	}

	return nil
}
