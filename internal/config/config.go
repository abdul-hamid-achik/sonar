package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/execpath"
	"github.com/abdul-hamid-achik/sonar/internal/netpolicy"
	"github.com/abdul-hamid-achik/sonar/internal/safeio"
	"gopkg.in/yaml.v3"
)

const maxStartupConfigBytes int64 = 1 << 20
const maxRepoMCPExecutableBytes int64 = 256 << 20
const repoMCPTrustEnv = "SONAR_TRUST_REPO_MCP"

var configFileReader = safeio.NewReader()
var repoMCPExecutableReader = safeio.NewReader()
var configFileReadTimeout = safeio.StartupReadTimeout

type Config struct {
	// SourcePath is the host-resolved config selected by repository/XDG
	// precedence. It is runtime metadata only and is never serialized.
	SourcePath string       `yaml:"-" json:"-"`
	Ollama     OllamaConfig `yaml:"ollama"`
	// Provider selects the inference adapter. Empty/ollama keeps the existing
	// Ollama path. Multi-profile catalogs use active + profiles; flat type/
	// base_url/model remains supported. Credentials are env names only.
	// Prefer TinyVault: tvault run --only KEY -- sonar.
	Provider ProviderConfig `yaml:"provider,omitempty"`
	Model    ModelConfig    `yaml:"model,omitempty"`
	Agents   AgentsConfig   `yaml:"agents,omitempty"`
	Servers  []ServerConfig `yaml:"servers,omitempty"`
	// SkillsDir is decoded only to reject the retired split skill root with a
	// clear migration error instead of silently ignoring an old configuration.
	SkillsDir     string              `yaml:"skills_dir,omitempty"`
	ICE           ICEConfig           `yaml:"ice,omitempty"`
	AgentProfile  string              `yaml:"agent_profile,omitempty"`
	Tools         ToolsConfig         `yaml:"tools,omitempty"`
	Continuations ContinuationsConfig `yaml:"continuations,omitempty"`
	Experts       ExpertsConfig       `yaml:"experts,omitempty"`
	Privacy       PrivacyConfig       `yaml:"privacy,omitempty"`
	Sandbox       SandboxConfig       `yaml:"sandbox,omitempty"`
	Credentials   CredentialsConfig   `yaml:"credentials,omitempty"`
	UI            UIConfig            `yaml:"ui,omitempty"`
}

// UIConfig holds presentation-only preferences. Nothing here affects authority,
// tool policy, or what leaves the machine.
type UIConfig struct {
	// Theme names a built-in color scheme. An unknown name falls back to the
	// default rather than failing startup: a config written for a newer build
	// must not leave the terminal colorless. Run /theme to see what is
	// available, or to pick one interactively (that choice is stored separately
	// and takes precedence over this key).
	Theme string `yaml:"theme,omitempty"`
	// ProseWidth caps how wide conversational text is allowed to wrap, in
	// columns. Structural surfaces — code fences, diffs, tables, inspectors —
	// always use the full pane and are unaffected.
	//
	// Zero — the default — means prose follows the terminal, sharing a right
	// edge with every other surface. Set a number to pin a fixed measure
	// instead; long lines really are harder to scan, and some readers prefer a
	// shorter one.
	//
	// This used to default to a fixed 140 columns, which pinned the layout to a
	// number nobody chose and left a dead right margin on any wider terminal.
	// Values below MinProseWidth are refused: a narrower measure wraps ordinary
	// sentences into ribbons.
	ProseWidth int `yaml:"prose_width,omitempty"`
}

// Prose measure bounds. Zero means "follow the terminal"; the named default
// exists only for callers that want the historical fixed measure.
const (
	DefaultProseWidth = 0
	MinProseWidth     = 40
	MaxProseWidth     = 500
)

// EffectiveProseWidth returns the configured prose cap, or the default.
// Validation has already accepted the value.
func (u UIConfig) EffectiveProseWidth() int {
	if u.ProseWidth <= 0 {
		return DefaultProseWidth
	}
	return u.ProseWidth
}

// (DefaultProseWidth is zero, so an unset value reaches SetProseCap as zero
// and selects the dynamic measure.)

// UnmarshalYAML rejects an explicit null continuation policy before it can be
// mistaken for omission. Other top-level decoding remains backward-compatible;
// strictness is intentionally scoped to this authority-sensitive subsection.
func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("config must be a mapping")
	}
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		if key == "continuations" && value.ShortTag() == "!!null" {
			return fmt.Errorf("continuations cannot be null; omit it to use safe defaults")
		}
	}
	type plain Config
	decoded := plain(*c)
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = Config(decoded)
	return nil
}

// CredentialsConfig points at an optional KEY=VALUE file holding provider API
// keys, so they need not be exported by hand on every launch.
type CredentialsConfig struct {
	// EnvFile is read at startup. Only names the catalog recognises as provider
	// credential variables are applied, and never over an existing environment
	// variable — a shared secrets file commonly also defines PATH or EDITOR, and
	// applying those would corrupt the process.
	//
	// Values are read from the file into the process environment and never
	// stored in configuration, logged, or printed.
	EnvFile string `yaml:"env_file,omitempty"`
}

type PrivacyConfig struct {
	// LocalOnly rejects remote MCP endpoints. It is a TOOL-endpoint boundary,
	// not an inference one: sonar always dispatches inference to DeepSeek over
	// the network, so applying this flag to the provider could only reject the
	// harness's single supported configuration. Keeping the two separate lets a
	// workspace insist that its tools stay on this machine while its model does
	// not. Approved subprocesses can still access the network; they are an
	// explicit trust boundary surfaced by the tool permission UI.
	LocalOnly bool `yaml:"local_only"`
}

// SandboxConfig confines shell subprocesses with the operating system's own
// primitives — Seatbelt on macOS, bubblewrap on Linux — so that the workspace
// boundary and the secret policy are enforced by the kernel rather than
// inferred from a command line.
//
// It is a second layer, never a replacement. The scoped-shell catalog reads
// argv and can refuse `curl` before it runs; the sandbox cannot read intent but
// binds what a process actually touches, including the workspace code that
// `go test` and `npm test` execute — which argv inspection is structurally
// blind to. Neither one subsumes the other.
//
// Enabled defaults to false. A sandbox changes which commands SUCCEED, not
// only which ones are allowed to start: with the network denied, a build that
// needs to download a module fails, and a security feature that silently
// breaks builds is one an operator turns off wholesale. Turning it on should
// be a decision someone made, not a surprise they diagnose.
type SandboxConfig struct {
	// Enabled confines every shell subprocess this host starts. On a platform
	// with no confinement driver it has no effect and startup says so, rather
	// than reporting a boundary that is not there.
	//
	// It also widens AUTO, which is the reason the sandbox is worth having.
	// Most catalog refusals exist to prove containment from argv; once the
	// kernel proves it for every command, they stop asking. What keeps asking
	// is what confinement does not cover — the workspace is writable, so
	// destructive work inside it and published state like a force push remain
	// human decisions. See internal/agent/auto_confined.go.
	Enabled bool `yaml:"enabled"`

	// AllowNetwork lets confined commands reach the network.
	//
	// Denying it costs inference nothing: sonar's provider calls are made by
	// the sonar process, never by a confined child. What it costs is
	// `npm install` and `go mod download` — both of which already require an
	// interactive approval, so the usual answer is to leave this false and
	// approve those individually.
	AllowNetwork bool `yaml:"allow_network"`
}

type AgentsConfig struct {
	Dir      string `yaml:"dir,omitempty"`
	AutoLoad bool   `yaml:"auto_load"`
}

type ToolsConfig struct {
	Timeout string `yaml:"timeout,omitempty"` // e.g., "30s", "2m"
	// MCPTimeout bounds a single MCP tool call. The default suits interactive
	// tools, but work that builds an index or walks a large repository can
	// legitimately exceed it — and a timeout is indistinguishable from a hung
	// server, so an unanswered effectful call becomes an outcome_unknown that
	// halts the turn for manual reconciliation. Raise this for a stack with
	// slow-but-honest servers. Empty keeps the built-in default.
	MCPTimeout        string `yaml:"mcp_timeout,omitempty"`
	MaxGrepResults    int    `yaml:"max_grep_results,omitempty"`
	MaxIterations     int    `yaml:"max_iterations,omitempty"`
	AutoMaxIterations int    `yaml:"auto_max_iterations,omitempty"`
	// AutoMaxSegments and AutoMaxWallTime bound a complete AUTO turn, not one
	// provider segment. auto_max_iterations is the per-segment watchdog; AUTO
	// chains segments after it fires, so the reachable amount of work is
	// iterations x segments, capped by wall time.
	//
	// These were host constants (8 segments, 90 minutes), which made an
	// unattended job of a few hours impossible to configure at any value of
	// auto_max_iterations. They are settings because the right ceiling depends
	// on what the run costs: a local Ollama model bounded only by patience is
	// a different decision from a metered hosted provider.
	//
	// Zero keeps the built-in default. The stall guard and the goal budgets
	// still apply; raising these does not make a stuck run run longer.
	AutoMaxSegments int    `yaml:"auto_max_segments,omitempty"`
	AutoMaxWallTime string `yaml:"auto_max_wall_time,omitempty"` // e.g. "90m", "6h"
	// ApprovalTimeout bounds how long a tool call waits for a human approval
	// before the host refuses it and the run continues with the next call.
	//
	// Empty or zero waits indefinitely, which is right when someone is
	// watching. Set it for unattended runs: otherwise the first approval
	// consumes the turn's entire remaining wall budget standing in front of a
	// modal nobody will answer, and the run ends having done nothing.
	//
	// A timeout can only ever withhold permission, never grant it.
	ApprovalTimeout string `yaml:"approval_timeout,omitempty"` // e.g. "2m"
}

// MaxApprovalTimeout bounds tools.approval_timeout. A value longer than this
// is indistinguishable from waiting forever, which the empty value already
// expresses more clearly.
const MaxApprovalTimeout = 24 * time.Hour

// ApprovalWaitTimeout returns the configured unattended approval timeout, or
// zero to wait indefinitely. Validation has already accepted the value.
func (t ToolsConfig) ApprovalWaitTimeout() time.Duration {
	parsed, err := time.ParseDuration(strings.TrimSpace(t.ApprovalTimeout))
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// AUTO turn ceilings. The defaults are unchanged from the constants they
// replaced, so an existing configuration behaves exactly as before.
const (
	DefaultAutoMaxSegments = 8
	DefaultAutoMaxWallTime = 90 * time.Minute
	// MaxAutoWallTime is a backstop, not a recommendation. An unattended run
	// that has gone wrong should still end on its own, and a value beyond a
	// day is far more likely to be a typo than an intention.
	MaxAutoWallTime    = 24 * time.Hour
	MaxAutoMaxSegments = 512
)

// AutoTurnCeilings returns the effective whole-turn AUTO budget. Validation has
// already accepted the configured values.
func (t ToolsConfig) AutoTurnCeilings() (segments int, wallTime time.Duration) {
	segments = DefaultAutoMaxSegments
	if t.AutoMaxSegments > 0 {
		segments = t.AutoMaxSegments
	}
	wallTime = DefaultAutoMaxWallTime
	if parsed, err := time.ParseDuration(strings.TrimSpace(t.AutoMaxWallTime)); err == nil && parsed > 0 {
		wallTime = parsed
	}
	return segments, wallTime
}

// MaxMCPCallTimeout bounds tools.mcp_timeout. A hung server must still fail
// eventually; without a ceiling a misconfiguration could hold a turn open
// indefinitely, which is the failure this timeout exists to prevent.
const MaxMCPCallTimeout = 10 * time.Minute

// MCPCallTimeout returns the configured per-call MCP timeout, or zero when the
// built-in default applies. Validation has already accepted the value.
func (c Config) MCPCallTimeout() time.Duration {
	if c.Tools.MCPTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(c.Tools.MCPTimeout)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// MaxAutoContinuationSteps is a host-owned safety ceiling. Configuration may
// lower the budget, but it cannot widen an automatic continuation chain beyond
// this bound.
const MaxAutoContinuationSteps = 2

// ContinuationMode controls how validated, typed continuation actions are
// surfaced. It does not grant tool authority; normal registry, trust, approval,
// and execution-ledger checks still apply to every action.
type ContinuationMode string

const (
	ContinuationOff          ContinuationMode = "off"
	ContinuationSuggest      ContinuationMode = "suggest"
	ContinuationAutoReadOnly ContinuationMode = "auto_read_only"
)

func (m ContinuationMode) Valid() bool {
	switch m {
	case ContinuationOff, ContinuationSuggest, ContinuationAutoReadOnly:
		return true
	default:
		return false
	}
}

// ContinuationsConfig configures host-controlled handling of typed
// continuation contracts. Auto mode is intentionally limited to exact,
// registry-validated, read-only actions; it never converts a downstream
// suggestion into authority.
type ContinuationsConfig struct {
	Mode         ContinuationMode `yaml:"mode" json:"mode"`
	MaxAutoSteps int              `yaml:"max_auto_steps" json:"max_auto_steps"`
}

// UnmarshalYAML makes this authority-sensitive subsection strict. A typo must
// not silently select a more permissive default, and omitted fields retain the
// host defaults established before decoding.
func (c *ContinuationsConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("continuations must be a mapping")
	}
	allowed := map[string]bool{
		"mode": true, "max_auto_steps": true,
	}
	seen := make(map[string]struct{}, len(allowed))
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		if !allowed[key] {
			return fmt.Errorf("unknown continuations field %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate continuations field %q", key)
		}
		if value.ShortTag() == "!!null" {
			return fmt.Errorf("continuations.%s cannot be null; omit it to use the host default", key)
		}
		seen[key] = struct{}{}
	}
	type plain ContinuationsConfig
	decoded := plain(*c)
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = ContinuationsConfig(decoded)
	return nil
}

// ExpertsConfig controls application-level read-only Team/Swarm/MoE
// consultation. Zero concurrency/fan-out values mean machine-adaptive auto;
// explicit values are safety caps and never force the resource planner above
// measured capacity.
type ExpertsConfig struct {
	Enabled                     bool   `yaml:"enabled"`
	MaxConcurrentInference      int    `yaml:"max_concurrent_inference,omitempty"`
	MaxConcurrentDistinctModels int    `yaml:"max_concurrent_distinct_models,omitempty"`
	MaxTeamExperts              int    `yaml:"max_team_experts,omitempty"`
	MaxSwarmWorkers             int    `yaml:"max_swarm_workers,omitempty"`
	MaxMoEExperts               int    `yaml:"max_moe_experts,omitempty"`
	MaxEvalTokens               int    `yaml:"max_eval_tokens,omitempty"`
	Timeout                     string `yaml:"timeout,omitempty"`
}

type ICEConfig struct {
	Enabled    bool   `yaml:"enabled"`
	EmbedModel string `yaml:"embed_model,omitempty"`
	StorePath  string `yaml:"store_path,omitempty"`
}

type OllamaConfig struct {
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	NumCtx  int    `yaml:"num_ctx"`
}

type ServerConfig struct {
	Name             string          `yaml:"name" json:"name"`
	Command          string          `yaml:"command,omitempty" json:"command,omitempty"`
	Args             []string        `yaml:"args,omitempty" json:"args,omitempty"`
	Env              []string        `yaml:"env,omitempty" json:"env,omitempty"`
	Transport        string          `yaml:"transport,omitempty" json:"transport,omitempty"`
	URL              string          `yaml:"url,omitempty" json:"url,omitempty"`
	Trust            *MCPTrustConfig `yaml:"trust,omitempty" json:"trust,omitempty"`
	ExecutableSHA256 string          `yaml:"-" json:"-"`
}

// RepoMCPTrustError reports executable MCP authority supplied by a
// repository-local configuration before any server process is started. The
// digest binds consent to both the selected repository path and the exact
// STDIO command, arguments, and environment.
type RepoMCPTrustError struct {
	SourcePath  string
	Digest      string
	ServerCount int
}

func (e *RepoMCPTrustError) Error() string {
	return fmt.Sprintf(
		"repository-local config %q requests %d STDIO MCP server process(es); refusing to start them without explicit trust (re-run with %s=%s for this exact executable configuration)",
		e.SourcePath,
		e.ServerCount,
		repoMCPTrustEnv,
		e.Digest,
	)
}

// Defaults returns the configuration a run starts from before any file or
// environment override is applied. Exported so callers that mutate a loaded
// config after Validate — and tests that need a valid baseline — do not have
// to reconstruct every required field by hand and drift from the real one.
func Defaults() Config { return defaults() }

func defaults() Config {
	modelCfg := DefaultModelConfig()
	return Config{
		Ollama: OllamaConfig{
			Model:   "qwen3.5:2b",
			BaseURL: "http://localhost:11434",
			// num_ctx is the KV-cache allocation, not the model's max context.
			// On a 16GB unified-memory Mac the cache scales linearly with this
			// value and competes with weights + the embed model for RAM, so the
			// default is kept modest. Raise it per-tier only when you have headroom;
			// never approach the model's 256K ceiling on 16GB. See config.example.yaml.
			NumCtx: 16384,
		},
		Model: modelCfg,
		Agents: AgentsConfig{
			Dir:      "",
			AutoLoad: true,
		},
		Tools: ToolsConfig{
			Timeout:           "30s",
			MaxGrepResults:    500,
			MaxIterations:     10,
			AutoMaxIterations: 40,
		},
		Continuations: ContinuationsConfig{
			Mode:         ContinuationSuggest,
			MaxAutoSteps: MaxAutoContinuationSteps,
		},
		Experts: ExpertsConfig{
			Enabled:       true,
			MaxEvalTokens: 768,
			Timeout:       "90s",
		},
		// ICE needs an embeddings endpoint, and DeepSeek's API does not expose
		// one — its Embed call fails outright. Retrieval therefore ships off by
		// default; enabling it requires pointing ICE at a separate local
		// embedding backend, which reintroduces a non-API dependency.
		ICE: ICEConfig{
			Enabled: false,
		},
		Privacy: PrivacyConfig{LocalOnly: true},
	}
}

func Load() (*Config, error) {
	cfg, _, err := loadConfigAndAgents()
	return cfg, err
}

func loadConfigAndAgents() (*Config, *AgentsDir, error) {
	cfg := defaults()

	localPath, data, err := findAndReadConfigFile()
	if err != nil {
		return nil, nil, err
	}
	repoConfig := isRepositoryLocalConfigPath(localPath)
	var repoConfiguredServers []ServerConfig
	repoSelectedAgentsDir := false
	if localPath != "" {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, nil, fmt.Errorf("parse config %s: %w", localPath, err)
		}
		if repoConfig {
			repoConfiguredServers = append([]ServerConfig(nil), cfg.Servers...)
			repoSelectedAgentsDir = strings.TrimSpace(cfg.Agents.Dir) != ""
		}
		cfg.SourcePath = localPath
		if absolute, absErr := filepath.Abs(localPath); absErr == nil {
			cfg.SourcePath = filepath.Clean(absolute)
		}
	}

	// Environment selection must be applied before loading the shared agents
	// root. Otherwise SONAR_AGENTS_DIR changes the returned config but
	// silently preloads metadata from a different directory.
	if err := applyEnvOverrides(&cfg); err != nil {
		return nil, nil, err
	}
	if os.Getenv("SONAR_AGENTS_DIR") != "" {
		// Process environment is user-controlled startup authority. Do not
		// attribute an environment-selected agents root to repository config.
		repoSelectedAgentsDir = false
	}

	var agentsData *AgentsDir
	if cfg.Agents.AutoLoad {
		agentsDir, resolveErr := resolveAgentsDir(cfg.Agents.Dir)
		if resolveErr != nil {
			return nil, nil, resolveErr
		}
		agentsData, err = LoadAgentsDir(agentsDir)
		if err != nil {
			return nil, nil, fmt.Errorf("load agents directory %s: %w", agentsDir, err)
		}
		if agentsData != nil && len(cfg.Servers) == 0 && agentsData.HasMCP() {
			cfg.Servers = agentsData.GetMCPServers()
		}
	}

	// The model fallback has nothing to do with the agents directory. Nested
	// inside the auto_load block it meant an explicitly empty ollama.model was
	// silently backfilled with the default auto_load: true, and hard-rejected
	// by Validate with auto_load: false — the same configuration accepted or
	// refused depending on an unrelated setting.
	if cfg.Ollama.Model == "" {
		cfg.Ollama.Model = cfg.Model.DefaultModel
	}

	clampNumCtxForMemory(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	if repoConfig {
		var repoAuthorities []ServerConfig
		if len(repoConfiguredServers) > 0 {
			repoAuthorities = cfg.Servers
		}
		if len(repoConfiguredServers) == 0 && repoSelectedAgentsDir && agentsData != nil {
			repoAuthorities = cfg.Servers
		}
		if err := requireRepositoryMCPTrust(cfg.SourcePath, repoAuthorities); err != nil {
			return nil, nil, err
		}
	}
	return &cfg, agentsData, nil
}

func isRepositoryLocalConfigPath(path string) bool {
	return path == "sonar.yaml" || path == "sonar.yml"
}

type repoMCPExecutableAuthority struct {
	Name             string          `json:"name"`
	Command          string          `json:"command"`
	ResolvedCommand  string          `json:"resolved_command"`
	ExecutablePath   string          `json:"executable_path"`
	ExecutableSHA256 string          `json:"executable_sha256"`
	Args             []string        `json:"args,omitempty"`
	Env              []string        `json:"env,omitempty"`
	Trust            *MCPTrustConfig `json:"trust,omitempty"`
}

type repoMCPTrustMaterial struct {
	Version    int                          `json:"version"`
	SourcePath string                       `json:"source_path"`
	Servers    []repoMCPExecutableAuthority `json:"servers"`
}

func requireRepositoryMCPTrust(sourcePath string, servers []ServerConfig) error {
	authorities := make([]repoMCPExecutableAuthority, 0, len(servers))
	serverIndexes := make([]int, 0, len(servers))
	for index, server := range servers {
		if server.Transport != "" && server.Transport != "stdio" {
			continue
		}
		authority, err := repositoryMCPExecutableAuthority(server)
		if err != nil {
			return fmt.Errorf("resolve repository MCP executable %q: %w", server.Name, err)
		}
		authorities = append(authorities, authority)
		serverIndexes = append(serverIndexes, index)
	}
	if len(authorities) == 0 {
		return nil
	}

	// Server ordering does not change process authority. Sort canonical copies
	// while retaining the original indexes used to pin runtime launch paths.
	sortedAuthorities := append([]repoMCPExecutableAuthority(nil), authorities...)
	sort.Slice(sortedAuthorities, func(i, j int) bool {
		left, _ := json.Marshal(sortedAuthorities[i])
		right, _ := json.Marshal(sortedAuthorities[j])
		return string(left) < string(right)
	})
	material, err := json.Marshal(repoMCPTrustMaterial{
		Version:    3,
		SourcePath: filepath.Clean(sourcePath),
		Servers:    sortedAuthorities,
	})
	if err != nil {
		return fmt.Errorf("encode repository MCP trust material: %w", err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(material))
	if strings.TrimSpace(os.Getenv(repoMCPTrustEnv)) == digest {
		for authorityIndex, serverIndex := range serverIndexes {
			// Pin startup to the exact symlink-resolved target covered by the
			// digest so neither PATH nor a retargeted launcher symlink can select a
			// different executable.
			servers[serverIndex].Command = authorities[authorityIndex].ExecutablePath
			servers[serverIndex].ExecutableSHA256 = authorities[authorityIndex].ExecutableSHA256
		}
		return nil
	}
	return &RepoMCPTrustError{
		SourcePath:  sourcePath,
		Digest:      digest,
		ServerCount: len(authorities),
	}
}

func repositoryMCPExecutableAuthority(server ServerConfig) (repoMCPExecutableAuthority, error) {
	trust, err := ResolveMCPTrust(server)
	if err != nil {
		return repoMCPExecutableAuthority{}, fmt.Errorf("resolve MCP trust: %w", err)
	}
	resolved, err := execpath.Resolve(server.Command)
	if err != nil {
		return repoMCPExecutableAuthority{}, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return repoMCPExecutableAuthority{}, fmt.Errorf("make executable path absolute: %w", err)
	}
	resolved = filepath.Clean(resolved)
	realPath, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return repoMCPExecutableAuthority{}, fmt.Errorf("resolve executable symlinks: %w", err)
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return repoMCPExecutableAuthority{}, fmt.Errorf("make executable target absolute: %w", err)
	}
	realPath = filepath.Clean(realPath)

	info, err := os.Stat(realPath)
	if err != nil {
		return repoMCPExecutableAuthority{}, fmt.Errorf("inspect executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return repoMCPExecutableAuthority{}, fmt.Errorf("resolved executable path %q is not a regular file", realPath)
	}
	contents, err := repoMCPExecutableReader.ReadRegularFileNoFollow(
		realPath, maxRepoMCPExecutableBytes, safeio.StartupReadTimeout,
	)
	if err != nil {
		return repoMCPExecutableAuthority{}, fmt.Errorf("read executable for trust digest: %w", err)
	}
	contentHash := sha256.Sum256(contents)

	return repoMCPExecutableAuthority{
		Name:             server.Name,
		Command:          server.Command,
		ResolvedCommand:  resolved,
		ExecutablePath:   realPath,
		ExecutableSHA256: fmt.Sprintf("sha256:%x", contentHash),
		Args:             append([]string(nil), server.Args...),
		Env:              append([]string(nil), server.Env...),
		Trust:            trust,
	}, nil
}

// resolveAgentsDir returns an explicit root whenever shared agent metadata is
// enabled. FindAgentsDir preserves compatibility with an existing selected
// root, while a fresh install consistently targets ~/.agents without creating
// it merely by reading configuration.
func resolveAgentsDir(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	if discovered := FindAgentsDir(); discovered != "" {
		return discovered, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve default agents directory: %w", err)
	}
	return filepath.Join(home, ".agents"), nil
}

// safeMaxNumCtx bounds the KV-cache allocation on a 16GB Mac. A larger context
// (e.g. a stale config's 262144) allocates a multi-GB cache that, with the
// model weights and embed model, can exhaust memory and crash the machine.
const safeMaxNumCtx = 32768

// clampNumCtxForMemory lowers an unsafe num_ctx to safeMaxNumCtx (overridable
// via SONAR_ALLOW_LARGE_MODELS) and warns. Runs at load so even an old
// config file with a huge num_ctx can't OOM the machine.
func clampNumCtxForMemory(cfg *Config) {
	if largeModelsAllowed() || cfg.Ollama.NumCtx <= safeMaxNumCtx {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: num_ctx %d is unsafe on a 16GB Mac; clamping to %d. Lower it in your config, or set SONAR_ALLOW_LARGE_MODELS=1 to keep your value.\n", cfg.Ollama.NumCtx, safeMaxNumCtx)
	cfg.Ollama.NumCtx = safeMaxNumCtx
}

// Validate checks the loaded configuration for problems that would otherwise
// surface as confusing runtime failures, and fails fast with a clear message.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.SkillsDir) != "" {
		return errors.New("config: skills_dir is no longer supported; move skills under the selected agents directory at skills/<name>/SKILL.md")
	}
	if err := c.validateProvider(); err != nil {
		return err
	}
	_, activeProfile, activeErr := c.Provider.ActiveProfile()
	activeRemote := activeErr == nil && activeProfile.IsRemote()
	if c.Ollama.Model == "" && !activeRemote {
		return fmt.Errorf("config: ollama.model is empty (set a model, e.g. qwen3.5:2b)")
	}
	if c.Privacy.LocalOnly && !activeRemote {
		if err := CheckLocalModelNameMemorySafe(c.Ollama.Model); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	if c.Ollama.BaseURL != "" {
		// Accept the lenient forms Ollama allows: bare host, host:port, or a
		// full URL. Only a scheme is normalized in for parsing.
		raw := c.Ollama.BaseURL
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return fmt.Errorf("config: invalid ollama.base_url %q: must be like http://localhost:11434 or localhost:11434", c.Ollama.BaseURL)
		}
		if c.Privacy.LocalOnly && !isLocalHost(u.Hostname()) {
			return fmt.Errorf("config: privacy.local_only rejects non-local ollama.base_url %q", c.Ollama.BaseURL)
		}
	}
	if c.Ollama.NumCtx <= 0 {
		return fmt.Errorf("config: ollama.num_ctx must be positive, got %d", c.Ollama.NumCtx)
	}
	if c.Tools.Timeout != "" {
		if d, err := time.ParseDuration(c.Tools.Timeout); err != nil {
			return fmt.Errorf("config: invalid tools.timeout %q: %w", c.Tools.Timeout, err)
		} else if d <= 0 {
			return fmt.Errorf("config: tools.timeout must be positive, got %s", c.Tools.Timeout)
		}
	}
	if c.Tools.MCPTimeout != "" {
		if d, err := time.ParseDuration(c.Tools.MCPTimeout); err != nil {
			return fmt.Errorf("config: invalid tools.mcp_timeout %q: %w", c.Tools.MCPTimeout, err)
		} else if d <= 0 {
			return fmt.Errorf("config: tools.mcp_timeout must be positive, got %s", c.Tools.MCPTimeout)
		} else if d > MaxMCPCallTimeout {
			return fmt.Errorf("config: tools.mcp_timeout %s exceeds the %s ceiling", c.Tools.MCPTimeout, MaxMCPCallTimeout)
		}
	}
	if c.Tools.AutoMaxSegments < 0 {
		return fmt.Errorf("config: tools.auto_max_segments must not be negative, got %d", c.Tools.AutoMaxSegments)
	}
	if c.Tools.AutoMaxSegments > MaxAutoMaxSegments {
		return fmt.Errorf("config: tools.auto_max_segments %d exceeds the %d ceiling", c.Tools.AutoMaxSegments, MaxAutoMaxSegments)
	}
	if c.UI.ProseWidth != 0 {
		if c.UI.ProseWidth < MinProseWidth {
			return fmt.Errorf("config: ui.prose_width %d is below the %d-column minimum; a narrower measure wraps sentences into ribbons", c.UI.ProseWidth, MinProseWidth)
		}
		if c.UI.ProseWidth > MaxProseWidth {
			return fmt.Errorf("config: ui.prose_width %d exceeds the %d-column ceiling", c.UI.ProseWidth, MaxProseWidth)
		}
	}
	if raw := strings.TrimSpace(c.Tools.ApprovalTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			return fmt.Errorf("config: invalid tools.approval_timeout %q: %w", raw, err)
		case d <= 0:
			return fmt.Errorf("config: tools.approval_timeout must be positive, got %s (omit it to wait indefinitely)", raw)
		case d > MaxApprovalTimeout:
			return fmt.Errorf("config: tools.approval_timeout %s exceeds the %s ceiling", raw, MaxApprovalTimeout)
		}
	}
	if raw := strings.TrimSpace(c.Tools.AutoMaxWallTime); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			return fmt.Errorf("config: invalid tools.auto_max_wall_time %q: %w", raw, err)
		case d <= 0:
			return fmt.Errorf("config: tools.auto_max_wall_time must be positive, got %s", raw)
		case d > MaxAutoWallTime:
			// An unattended run that has gone wrong must still end by itself.
			return fmt.Errorf("config: tools.auto_max_wall_time %s exceeds the %s ceiling", raw, MaxAutoWallTime)
		}
	}
	if !c.Continuations.Mode.Valid() {
		return fmt.Errorf(
			"config: continuations.mode must be one of %q, %q, or %q, got %q",
			ContinuationOff,
			ContinuationSuggest,
			ContinuationAutoReadOnly,
			c.Continuations.Mode,
		)
	}
	if c.Continuations.MaxAutoSteps < 0 || c.Continuations.MaxAutoSteps > MaxAutoContinuationSteps {
		return fmt.Errorf(
			"config: continuations.max_auto_steps must be 0..%d, got %d",
			MaxAutoContinuationSteps,
			c.Continuations.MaxAutoSteps,
		)
	}
	if c.Continuations.Mode == ContinuationAutoReadOnly && c.Continuations.MaxAutoSteps == 0 {
		return fmt.Errorf("config: continuations.max_auto_steps must be at least 1 in %q mode", ContinuationAutoReadOnly)
	}
	for name, value := range map[string]int{
		"max_concurrent_inference":       c.Experts.MaxConcurrentInference,
		"max_concurrent_distinct_models": c.Experts.MaxConcurrentDistinctModels,
		"max_team_experts":               c.Experts.MaxTeamExperts,
		"max_swarm_workers":              c.Experts.MaxSwarmWorkers,
		"max_moe_experts":                c.Experts.MaxMoEExperts,
	} {
		if value < 0 || value > 16 {
			return fmt.Errorf("config: experts.%s must be 0..16, got %d", name, value)
		}
	}
	if c.Experts.MaxEvalTokens < 1 || c.Experts.MaxEvalTokens > 8192 {
		return fmt.Errorf("config: experts.max_eval_tokens must be 1..8192, got %d", c.Experts.MaxEvalTokens)
	}
	if d, err := time.ParseDuration(c.Experts.Timeout); err != nil {
		return fmt.Errorf("config: invalid experts.timeout %q: %w", c.Experts.Timeout, err)
	} else if d < time.Second || d > 10*time.Minute {
		return fmt.Errorf("config: experts.timeout must be between 1s and 10m, got %s", c.Experts.Timeout)
	}
	serverNames := make(map[string]struct{}, len(c.Servers))
	for i, s := range c.Servers {
		if s.Name == "" {
			return fmt.Errorf("config: servers[%d] has an empty name", i)
		}
		if strings.Contains(s.Name, "__") {
			return fmt.Errorf("config: server %q contains reserved namespace delimiter __", s.Name)
		}
		if _, duplicate := serverNames[s.Name]; duplicate {
			return fmt.Errorf("config: duplicate server name %q", s.Name)
		}
		serverNames[s.Name] = struct{}{}
		switch s.Transport {
		case "sse", "streamable-http":
			if s.URL == "" {
				return fmt.Errorf("config: server %q uses %s transport but has no url", s.Name, s.Transport)
			}
			u, err := url.Parse(s.URL)
			if err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("config: server %q has invalid url %q", s.Name, s.URL)
			}
			if c.Privacy.LocalOnly && !isLocalHost(u.Hostname()) {
				return fmt.Errorf("config: privacy.local_only rejects non-local MCP server %q (%s)", s.Name, s.URL)
			}
		case "", "stdio":
			if s.Command == "" {
				return fmt.Errorf("config: server %q uses stdio transport but has no command", s.Name)
			}
		default:
			return fmt.Errorf("config: server %q has unknown transport %q (want stdio, sse, or streamable-http)", s.Name, s.Transport)
		}
		if _, err := ResolveMCPTrust(s); err != nil {
			return fmt.Errorf("config: server %q trust: %w", s.Name, err)
		}
	}
	return nil
}

func isLocalHost(host string) bool {
	// Ollama commonly exports OLLAMA_HOST=0.0.0.0 (or ::) to describe its
	// local listen address. Connecting to an unspecified address still targets
	// this host, so it is safe for local-only client routing. Actual LAN/WAN
	// addresses remain rejected.
	return netpolicy.IsLocalHost(host)
}

// CheckModelMemorySafe rejects cloud and clearly oversized local tiers. The
// 9B Qwen/Ornith and Gemma E2B profiles are allowed as explicit exclusive
// profiles; the router never auto-selects them and ModelManager unloads the
// previous chat model before switching. Override the remaining guard only for
// measured hardware profiles with SONAR_ALLOW_LARGE_MODELS=1.
func CheckModelMemorySafe(model string) error {
	// A hardware override may relax RAM limits, but it must never turn a cloud
	// alias into an allowed model for this local-only harness.
	if isRemoteModelAlias(model) {
		return fmt.Errorf("model %q is a cloud/remote alias and is not allowed by the local-only model policy", model)
	}
	if largeModelsAllowed() || !isMemoryRiskyModel(model) {
		return nil
	}
	return fmt.Errorf("model %q is not enabled for this local profile — cloud models and local tiers >=10B (including Gemma E4B+) can exhaust a 16GB machine. Use Qwen 0.8B/2B/4B, Phi-4 Mini, or an explicit exclusive Qwen/Ornith 9B or Gemma E2B profile. To override after measuring headroom, set SONAR_ALLOW_LARGE_MODELS=1", model)
}

// CheckLocalModelNameMemorySafe applies only the pre-discovery memory-tier
// heuristic. Execution location must come from Ollama inventory rather than
// words such as "cloud" or "remote" in a custom local tag.
func CheckLocalModelNameMemorySafe(model string) error {
	if largeModelsAllowed() || !isLocalMemoryRiskyModel(model) {
		return nil
	}
	return fmt.Errorf("model %q is outside the default local memory profile", model)
}

const maxDefaultLocalModelBytes int64 = 8 << 30

// CheckLocalModelSizeSafe enforces the 16GB profile from Ollama's actual
// on-disk weight size. Names are only hints (for example 8x7b and custom tags
// are ambiguous), so local-only admission must call this after discovery.
func CheckLocalModelSizeSafe(model string, size int64) error {
	if !largeModelsAllowed() && isLocalMemoryRiskyModel(model) {
		return fmt.Errorf("model %q is outside the default local memory profile", model)
	}
	if size <= 0 {
		return fmt.Errorf("model %q has no verified local weight size", model)
	}
	if largeModelsAllowed() || size <= maxDefaultLocalModelBytes {
		return nil
	}
	return fmt.Errorf("model %q uses %.1f GiB of local weights, above the %.0f GiB default budget for this 16GB profile; set SONAR_ALLOW_LARGE_MODELS=1 only after measuring memory headroom", model, float64(size)/(1<<30), float64(maxDefaultLocalModelBytes)/(1<<30))
}

func isLocalMemoryRiskyModel(model string) bool {
	m := strings.ToLower(model)
	if strings.Contains(m, "gemma") && !strings.Contains(m, ":e2b") {
		return true
	}
	if mt := paramBPattern.FindStringSubmatch(m); mt != nil {
		if b, err := strconv.ParseFloat(mt[1], 64); err == nil && b >= 10.0 {
			return true
		}
	}
	return false
}

func isRemoteModelAlias(model string) bool {
	name := strings.ToLower(model)
	return strings.Contains(name, "cloud") || strings.Contains(name, "remote")
}

// largeModelsAllowed reports whether the memory-safety guard is disabled.
func largeModelsAllowed() bool {
	allowed, err := parseEnvBool("SONAR_ALLOW_LARGE_MODELS", os.Getenv("SONAR_ALLOW_LARGE_MODELS"))
	return err == nil && allowed
}

// paramBPattern extracts a parameter count hint like ":9b" or ":0.8b" from a model tag.
var paramBPattern = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*b\b`)

// isMemoryRiskyModel reports whether a model should remain blocked even from
// ordinary manual selection on a 16GB profile. Qwen/Ornith 9B and Gemma E2B
// are handled as exclusive profiles; larger Gemma tiers and >=10B tags remain
// guarded, and cloud entries are always rejected in local-only mode.
func isMemoryRiskyModel(model string) bool {
	m := strings.ToLower(model)
	if strings.Contains(m, "cloud") {
		return true
	}
	if strings.Contains(m, "gemma") && !strings.Contains(m, ":e2b") {
		return true
	}
	if mt := paramBPattern.FindStringSubmatch(m); mt != nil {
		if b, err := strconv.ParseFloat(mt[1], 64); err == nil && b >= 10.0 {
			return true
		}
	}
	return false
}

func LoadWithAgentsDir() (*Config, *AgentsDir, error) {
	return loadConfigAndAgents()
}

func findAndReadConfigFile() (string, []byte, error) {
	for _, path := range configFileCandidates() {
		data, err := configFileReader.ReadRegularFileNoFollow(path, maxStartupConfigBytes, configFileReadTimeout)
		if err == nil {
			return path, data, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return "", nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return "", nil, nil
}

// configFileCandidates returns config locations in precedence order. A
// repository-local file always wins. XDG_CONFIG_HOME is honored when it is an
// absolute path, followed by the historical ~/.config fallback. Cleaned paths
// are de-duplicated so the common XDG_CONFIG_HOME=$HOME/.config setup is read
// only once.
func configFileCandidates() []string {
	candidates := make([]string, 0, 6)
	seen := make(map[string]struct{}, 6)
	appendCandidate := func(path string) {
		path = filepath.Clean(path)
		if _, exists := seen[path]; exists {
			return
		}
		seen[path] = struct{}{}
		candidates = append(candidates, path)
	}
	appendConfigDir := func(root string) {
		if root == "" {
			return
		}
		dir := filepath.Join(root, "sonar")
		appendCandidate(filepath.Join(dir, "config.yaml"))
		appendCandidate(filepath.Join(dir, "config.yml"))
	}

	appendCandidate("sonar.yaml")
	appendCandidate("sonar.yml")

	if xdgConfigHome := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdgConfigHome) {
		appendConfigDir(xdgConfigHome)
	}
	if home, err := os.UserHomeDir(); err == nil {
		appendConfigDir(filepath.Join(home, ".config"))
	}

	return candidates
}

func applyEnvOverrides(cfg *Config) error {
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		cfg.Ollama.BaseURL = v
	}
	if v := os.Getenv("SONAR_ENV_FILE"); v != "" {
		cfg.Credentials.EnvFile = v
	}
	// Provider selection first so model overrides can target the remote profile.
	if v := os.Getenv("SONAR_PROVIDER"); v != "" {
		// Prefer multi-profile active name when a catalog exists; otherwise treat
		// the value as a flat type (xai / openai_compatible / ollama).
		if cfg.Provider.HasProfiles() {
			// Both branches assigned the same value: a name that is not in the
			// catalog is still set so Validate reports it by name rather than
			// silently ignoring the override.
			cfg.Provider.Active = v
		} else {
			cfg.Provider.Type = v
			cfg.Provider.Active = v
		}
	}
	if v := os.Getenv("SONAR_PROVIDER_BASE_URL"); v != "" {
		cfg.Provider.BaseURL = v
		applyActiveProfileField(cfg, func(p *ProviderProfile) { p.BaseURL = v })
	}
	if v := os.Getenv("SONAR_PROVIDER_MODEL"); v != "" {
		cfg.Provider.Model = v
		applyActiveProfileField(cfg, func(p *ProviderProfile) { p.Model = v })
	}
	if v := os.Getenv("SONAR_PROVIDER_API_KEY_ENV"); v != "" {
		cfg.Provider.APIKeyEnv = v
		applyActiveProfileField(cfg, func(p *ProviderProfile) { p.APIKeyEnv = v })
	}
	if v := os.Getenv("SONAR_PROVIDER_CONTEXT_SIZE"); v != "" {
		size, err := parseEnvInt("SONAR_PROVIDER_CONTEXT_SIZE", v)
		if err != nil {
			return err
		}
		cfg.Provider.ContextSize = size
		applyActiveProfileField(cfg, func(p *ProviderProfile) { p.ContextSize = size })
	}
	if v := os.Getenv("SONAR_MODEL"); v != "" {
		cfg.Ollama.Model = v
		if active, profile, err := cfg.Provider.ActiveProfile(); err == nil && profile.IsRemote() {
			cfg.Provider.Model = v
			if cfg.Provider.HasProfiles() {
				p := cfg.Provider.Profiles[active]
				p.Model = v
				cfg.Provider.Profiles[active] = p
			}
		} else if cfg.Provider.IsRemote() {
			cfg.Provider.Model = v
		}
	}
	if v := os.Getenv("SONAR_AGENTS_DIR"); v != "" {
		cfg.Agents.Dir = v
	}
	if v := os.Getenv("SONAR_TOOLS_TIMEOUT"); v != "" {
		cfg.Tools.Timeout = v
	}
	if v := os.Getenv("SONAR_TOOLS_MAX_GREP"); v != "" {
		parsed, err := parseEnvInt("SONAR_TOOLS_MAX_GREP", v)
		if err != nil {
			return err
		}
		cfg.Tools.MaxGrepResults = parsed
	}
	if v := os.Getenv("SONAR_TOOLS_MAX_ITER"); v != "" {
		parsed, err := parseEnvInt("SONAR_TOOLS_MAX_ITER", v)
		if err != nil {
			return err
		}
		cfg.Tools.MaxIterations = parsed
	}
	if v := os.Getenv("SONAR_TOOLS_AUTO_MAX_ITER"); v != "" {
		parsed, err := parseEnvInt("SONAR_AUTO_MAX_ITER", v)
		if err != nil {
			return err
		}
		cfg.Tools.AutoMaxIterations = parsed
	}
	if v := os.Getenv("SONAR_CONTINUATIONS_MODE"); v != "" {
		cfg.Continuations.Mode = ContinuationMode(v)
	}
	if v := os.Getenv("SONAR_CONTINUATIONS_MAX_AUTO_STEPS"); v != "" {
		steps, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: invalid SONAR_CONTINUATIONS_MAX_AUTO_STEPS %q: %w", v, err)
		}
		cfg.Continuations.MaxAutoSteps = steps
	}
	if v := os.Getenv("SONAR_ICE_EMBED_MODEL"); v != "" {
		cfg.ICE.EmbedModel = v
	}
	if v := os.Getenv("SONAR_LOCAL_ONLY"); v != "" {
		localOnly, err := parseEnvBool("SONAR_LOCAL_ONLY", v)
		if err != nil {
			return err
		}
		cfg.Privacy.LocalOnly = localOnly
	}
	return nil
}

// applyActiveProfileField mutates the active multi-profile entry when a catalog
// is present so env overrides land on the profile that will actually run.
func applyActiveProfileField(cfg *Config, mutate func(*ProviderProfile)) {
	if cfg == nil || !cfg.Provider.HasProfiles() || mutate == nil {
		return
	}
	name := cfg.Provider.ActiveName()
	profile, ok := cfg.Provider.LookupProfile(name)
	if !ok {
		return
	}
	mutate(&profile)
	if cfg.Provider.Profiles == nil {
		cfg.Provider.Profiles = make(map[string]ProviderProfile)
	}
	cfg.Provider.Profiles[name] = profile
}

// parseEnvInt reports an unparseable override instead of silently substituting
// the previous value. Six variables documented in one table used to disagree —
// one exited, four fell back silently, and one was a no-op — so a typo was
// indistinguishable from a deliberate setting, and Validate range-checks none
// of them.
func parseEnvInt(name, v string) (int, error) {
	i, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: invalid %s %q: %w", name, v, err)
	}
	return i, nil
}

// envBoolValues is the boolean vocabulary accepted by every sonar
// environment flag. strconv.ParseBool rejects "yes"/"no"/"on"/"off", which
// largeModelsAllowed has always accepted — so SONAR_LOCAL_ONLY=yes was
// discarded while the operator believed the run had been hardened.
var envBoolValues = map[string]bool{
	"1": true, "t": true, "true": true, "yes": true, "y": true, "on": true,
	"0": false, "f": false, "false": false, "no": false, "n": false, "off": false,
}

func parseEnvBool(name, v string) (bool, error) {
	if parsed, ok := envBoolValues[strings.ToLower(strings.TrimSpace(v))]; ok {
		return parsed, nil
	}
	return false, fmt.Errorf("config: invalid %s %q: want true or false", name, v)
}
