package permission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	workspaceRulesVersion = 2
	maxBashPrefixes       = 64
	maxMCPTools           = 64
	maxWritePaths         = 128
	maxPrefixBytes        = 256
	maxMCPToolBytes       = 256
	maxWritePathBytes     = 1024
)

// WorkspaceRules is the durable, workspace-scoped allow list for bash patterns,
// exact MCP tool names, and exact write paths. It never grants remove/move
// globally and is never loaded from a repository path (user config only).
type WorkspaceRules struct {
	Version      int      `json:"version"`
	Workspace    string   `json:"workspace"`
	BashPrefixes []string `json:"bash_prefixes,omitempty"` // literal prefixes or trailing globs
	MCPTools     []string `json:"mcp_tools,omitempty"`
	WritePaths   []string `json:"write_paths,omitempty"` // workspace-relative paths for write/edit/mkdir
	UpdatedAt    string   `json:"updated_at,omitempty"`
}

// WorkspaceRulesStore loads and saves rules under
// ~/.config/sonar/workspace-rules/<hash>.json.
type WorkspaceRulesStore struct {
	mu      sync.RWMutex
	rootDir string
}

// NewWorkspaceRulesStore uses the default XDG-style config root when rootDir is empty.
func NewWorkspaceRulesStore(rootDir string) (*WorkspaceRulesStore, error) {
	if strings.TrimSpace(rootDir) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("workspace rules home: %w", err)
		}
		rootDir = filepath.Join(home, ".config", "sonar", "workspace-rules")
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("workspace rules dir: %w", err)
	}
	if err := os.Chmod(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure workspace rules dir: %w", err)
	}
	return &WorkspaceRulesStore{rootDir: rootDir}, nil
}

func (s *WorkspaceRulesStore) pathFor(workspace string) (string, error) {
	workspace, err := canonicalizeWorkspace(workspace)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(workspace))
	return filepath.Join(s.rootDir, hex.EncodeToString(sum[:])+".json"), nil
}

// Load returns rules for workspace. Missing files yield empty rules.
func (s *WorkspaceRulesStore) Load(workspace string) (WorkspaceRules, error) {
	if s == nil {
		return WorkspaceRules{}, fmt.Errorf("workspace rules store is nil")
	}
	workspace, err := canonicalizeWorkspace(workspace)
	if err != nil {
		return WorkspaceRules{}, err
	}
	path, err := s.pathFor(workspace)
	if err != nil {
		return WorkspaceRules{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return WorkspaceRules{Version: workspaceRulesVersion, Workspace: workspace}, nil
		}
		return WorkspaceRules{}, fmt.Errorf("read workspace rules: %w", err)
	}
	var rules WorkspaceRules
	if err := json.Unmarshal(data, &rules); err != nil {
		return WorkspaceRules{}, fmt.Errorf("decode workspace rules: %w", err)
	}
	rules = sanitizeWorkspaceRules(rules, workspace)
	return rules, nil
}

// Save writes rules for the workspace with owner-only permissions.
func (s *WorkspaceRulesStore) Save(rules WorkspaceRules) error {
	if s == nil {
		return fmt.Errorf("workspace rules store is nil")
	}
	workspace, err := canonicalizeWorkspace(rules.Workspace)
	if err != nil {
		return err
	}
	rules = sanitizeWorkspaceRules(rules, workspace)
	rules.Version = workspaceRulesVersion
	rules.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	path, err := s.pathFor(workspace)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workspace rules: %w", err)
	}
	payload = append(payload, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("write workspace rules temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish workspace rules: %w", err)
	}
	return nil
}

// AddBashPrefix appends a normalized bash pattern (literal or trailing glob).
func (s *WorkspaceRulesStore) AddBashPrefix(workspace, prefix string) (WorkspaceRules, error) {
	prefix, ok := NormalizeBashPattern(prefix)
	if !ok {
		return WorkspaceRules{}, fmt.Errorf("invalid bash pattern %q (use a prefix or trailing glob like \"git status *\")", prefix)
	}
	if len(prefix) > maxPrefixBytes {
		return WorkspaceRules{}, fmt.Errorf("bash pattern exceeds %d bytes", maxPrefixBytes)
	}
	rules, err := s.Load(workspace)
	if err != nil {
		return WorkspaceRules{}, err
	}
	for _, existing := range rules.BashPrefixes {
		if existing == prefix {
			return rules, nil
		}
	}
	if len(rules.BashPrefixes) >= maxBashPrefixes {
		return WorkspaceRules{}, fmt.Errorf("bash pattern limit (%d) reached", maxBashPrefixes)
	}
	rules.BashPrefixes = append(rules.BashPrefixes, prefix)
	sort.Strings(rules.BashPrefixes)
	if err := s.Save(rules); err != nil {
		return WorkspaceRules{}, err
	}
	return rules, nil
}

// RemoveBashPrefix deletes a pattern when present.
func (s *WorkspaceRulesStore) RemoveBashPrefix(workspace, prefix string) (WorkspaceRules, bool, error) {
	prefix, ok := NormalizeBashPattern(prefix)
	if !ok {
		// Also try literal prefix-only remove for older stored values.
		if p, pok := NormalizeBashPrefix(prefix); pok {
			prefix = p
		} else {
			return WorkspaceRules{}, false, fmt.Errorf("invalid bash pattern %q", prefix)
		}
	}
	rules, err := s.Load(workspace)
	if err != nil {
		return WorkspaceRules{}, false, err
	}
	next := rules.BashPrefixes[:0]
	removed := false
	for _, existing := range rules.BashPrefixes {
		if existing == prefix {
			removed = true
			continue
		}
		next = append(next, existing)
	}
	rules.BashPrefixes = next
	if !removed {
		return rules, false, nil
	}
	if err := s.Save(rules); err != nil {
		return WorkspaceRules{}, false, err
	}
	return rules, true, nil
}

// AddMCPTool appends an exact namespaced MCP tool and saves.
func (s *WorkspaceRulesStore) AddMCPTool(workspace, tool string) (WorkspaceRules, error) {
	tool, ok := NormalizeMCPToolName(tool)
	if !ok {
		return WorkspaceRules{}, fmt.Errorf("invalid MCP tool %q (use server__tool)", tool)
	}
	if len(tool) > maxMCPToolBytes {
		return WorkspaceRules{}, fmt.Errorf("MCP tool name exceeds %d bytes", maxMCPToolBytes)
	}
	rules, err := s.Load(workspace)
	if err != nil {
		return WorkspaceRules{}, err
	}
	for _, existing := range rules.MCPTools {
		if existing == tool {
			return rules, nil
		}
	}
	if len(rules.MCPTools) >= maxMCPTools {
		return WorkspaceRules{}, fmt.Errorf("MCP tool limit (%d) reached", maxMCPTools)
	}
	rules.MCPTools = append(rules.MCPTools, tool)
	sort.Strings(rules.MCPTools)
	if err := s.Save(rules); err != nil {
		return WorkspaceRules{}, err
	}
	return rules, nil
}

// RemoveMCPTool deletes a tool allow when present.
func (s *WorkspaceRulesStore) RemoveMCPTool(workspace, tool string) (WorkspaceRules, bool, error) {
	tool, ok := NormalizeMCPToolName(tool)
	if !ok {
		return WorkspaceRules{}, false, fmt.Errorf("invalid MCP tool %q", tool)
	}
	rules, err := s.Load(workspace)
	if err != nil {
		return WorkspaceRules{}, false, err
	}
	next := rules.MCPTools[:0]
	removed := false
	for _, existing := range rules.MCPTools {
		if existing == tool {
			removed = true
			continue
		}
		next = append(next, existing)
	}
	rules.MCPTools = next
	if !removed {
		return rules, false, nil
	}
	if err := s.Save(rules); err != nil {
		return WorkspaceRules{}, false, err
	}
	return rules, true, nil
}

// AddWritePath appends a workspace-relative path grant for write/edit/mkdir.
func (s *WorkspaceRulesStore) AddWritePath(workspace, path string) (WorkspaceRules, error) {
	workspace, err := canonicalizeWorkspace(workspace)
	if err != nil {
		return WorkspaceRules{}, err
	}
	rel, ok := NormalizeWritePath(workspace, path)
	if !ok {
		return WorkspaceRules{}, fmt.Errorf("write path must resolve inside the workspace")
	}
	if len(rel) > maxWritePathBytes {
		return WorkspaceRules{}, fmt.Errorf("write path exceeds %d bytes", maxWritePathBytes)
	}
	rules, err := s.Load(workspace)
	if err != nil {
		return WorkspaceRules{}, err
	}
	for _, existing := range rules.WritePaths {
		if existing == rel {
			return rules, nil
		}
	}
	if len(rules.WritePaths) >= maxWritePaths {
		return WorkspaceRules{}, fmt.Errorf("write path limit (%d) reached", maxWritePaths)
	}
	rules.WritePaths = append(rules.WritePaths, rel)
	sort.Strings(rules.WritePaths)
	if err := s.Save(rules); err != nil {
		return WorkspaceRules{}, err
	}
	return rules, nil
}

// RemoveWritePath deletes a path grant when present.
func (s *WorkspaceRulesStore) RemoveWritePath(workspace, path string) (WorkspaceRules, bool, error) {
	workspace, err := canonicalizeWorkspace(workspace)
	if err != nil {
		return WorkspaceRules{}, false, err
	}
	rel, ok := NormalizeWritePath(workspace, path)
	if !ok {
		// Allow removing by already-relative stored form.
		rel = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
		if rel == "" || rel == "." {
			return WorkspaceRules{}, false, fmt.Errorf("invalid write path %q", path)
		}
	}
	rules, err := s.Load(workspace)
	if err != nil {
		return WorkspaceRules{}, false, err
	}
	next := rules.WritePaths[:0]
	removed := false
	for _, existing := range rules.WritePaths {
		if existing == rel {
			removed = true
			continue
		}
		next = append(next, existing)
	}
	rules.WritePaths = next
	if !removed {
		return rules, false, nil
	}
	if err := s.Save(rules); err != nil {
		return WorkspaceRules{}, false, err
	}
	return rules, true, nil
}

// AllowsBash reports whether any durable pattern matches the command.
func (r WorkspaceRules) AllowsBash(command string) bool {
	for _, pattern := range r.BashPrefixes {
		if BashPatternMatches(command, pattern) {
			return true
		}
	}
	return false
}

// AllowsMCPTool reports an exact namespaced tool allow.
func (r WorkspaceRules) AllowsMCPTool(tool string) bool {
	tool = strings.TrimSpace(tool)
	for _, allowed := range r.MCPTools {
		if allowed == tool {
			return true
		}
	}
	return false
}

// AllowsWritePath reports a durable path grant for write/edit/mkdir.
func (r WorkspaceRules) AllowsWritePath(workspace, absolutePath string) bool {
	for _, granted := range r.WritePaths {
		if WritePathMatches(workspace, absolutePath, granted) {
			return true
		}
	}
	return false
}

func sanitizeWorkspaceRules(rules WorkspaceRules, workspace string) WorkspaceRules {
	rules.Version = workspaceRulesVersion
	rules.Workspace = workspace
	prefixes := make([]string, 0, len(rules.BashPrefixes))
	seenPrefix := make(map[string]struct{}, len(rules.BashPrefixes))
	for _, prefix := range rules.BashPrefixes {
		normalized, ok := NormalizeBashPattern(prefix)
		if !ok || len(normalized) > maxPrefixBytes {
			// Migrate older literal prefixes that NormalizeBashPattern also accepts.
			if lit, lok := NormalizeBashPrefix(prefix); lok && len(lit) <= maxPrefixBytes {
				normalized = lit
			} else {
				continue
			}
		}
		if _, dup := seenPrefix[normalized]; dup {
			continue
		}
		seenPrefix[normalized] = struct{}{}
		prefixes = append(prefixes, normalized)
		if len(prefixes) >= maxBashPrefixes {
			break
		}
	}
	sort.Strings(prefixes)
	rules.BashPrefixes = prefixes

	tools := make([]string, 0, len(rules.MCPTools))
	seenTool := make(map[string]struct{}, len(rules.MCPTools))
	for _, tool := range rules.MCPTools {
		normalized, ok := NormalizeMCPToolName(tool)
		if !ok || len(normalized) > maxMCPToolBytes {
			continue
		}
		if _, dup := seenTool[normalized]; dup {
			continue
		}
		seenTool[normalized] = struct{}{}
		tools = append(tools, normalized)
		if len(tools) >= maxMCPTools {
			break
		}
	}
	sort.Strings(tools)
	rules.MCPTools = tools

	paths := make([]string, 0, len(rules.WritePaths))
	seenPath := make(map[string]struct{}, len(rules.WritePaths))
	for _, path := range rules.WritePaths {
		// Stored values are already workspace-relative slash paths.
		rel := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
		if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
			// Re-normalize absolute leftovers against workspace when possible.
			if normalized, ok := NormalizeWritePath(workspace, path); ok {
				rel = normalized
			} else {
				continue
			}
		}
		if len(rel) > maxWritePathBytes {
			continue
		}
		if _, dup := seenPath[rel]; dup {
			continue
		}
		seenPath[rel] = struct{}{}
		paths = append(paths, rel)
		if len(paths) >= maxWritePaths {
			break
		}
	}
	sort.Strings(paths)
	rules.WritePaths = paths
	return rules
}

func canonicalizeWorkspace(workspace string) (string, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return "", fmt.Errorf("workspace is required")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}
