package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WorkspaceRulesExportFormat is the portable document version for import/export.
const WorkspaceRulesExportFormat = 1

// WorkspaceRulesExport is a portable snapshot of durable workspace rules.
// Write paths are workspace-relative; bash patterns and MCP tools are exact
// host strings. The source workspace is informational only.
type WorkspaceRulesExport struct {
	FormatVersion   int      `json:"format_version"`
	ExportedAt      string   `json:"exported_at,omitempty"`
	SourceWorkspace string   `json:"source_workspace,omitempty"`
	BashPrefixes    []string `json:"bash_prefixes,omitempty"`
	MCPTools        []string `json:"mcp_tools,omitempty"`
	MCPServers      []string `json:"mcp_servers,omitempty"`
	WritePaths      []string `json:"write_paths,omitempty"`
}

// ExportDocument builds a portable document from in-memory rules.
func (r WorkspaceRules) ExportDocument() WorkspaceRulesExport {
	return WorkspaceRulesExport{
		FormatVersion:   WorkspaceRulesExportFormat,
		ExportedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		SourceWorkspace: r.Workspace,
		BashPrefixes:    append([]string(nil), r.BashPrefixes...),
		MCPTools:        append([]string(nil), r.MCPTools...),
		MCPServers:      append([]string(nil), r.MCPServers...),
		WritePaths:      append([]string(nil), r.WritePaths...),
	}
}

// RuleCount returns the number of durable entries in the export.
func (e WorkspaceRulesExport) RuleCount() int {
	return len(e.BashPrefixes) + len(e.MCPTools) + len(e.MCPServers) + len(e.WritePaths)
}

// Validate checks export shape before import.
func (e WorkspaceRulesExport) Validate() error {
	if e.FormatVersion != 0 && e.FormatVersion != WorkspaceRulesExportFormat {
		return fmt.Errorf("unsupported permissions export format %d (want %d)", e.FormatVersion, WorkspaceRulesExportFormat)
	}
	for _, prefix := range e.BashPrefixes {
		if _, ok := NormalizeBashPattern(prefix); !ok {
			if _, ok := NormalizeBashPrefix(prefix); !ok {
				return fmt.Errorf("invalid bash pattern in export: %q", prefix)
			}
		}
	}
	for _, tool := range e.MCPTools {
		if _, ok := NormalizeMCPToolName(tool); !ok {
			return fmt.Errorf("invalid MCP tool in export: %q", tool)
		}
	}
	for _, server := range e.MCPServers {
		if _, ok := NormalizeMCPServerName(server); !ok {
			return fmt.Errorf("invalid MCP server in export: %q", server)
		}
	}
	for _, path := range e.WritePaths {
		rel := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
		if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
			return fmt.Errorf("invalid write path in export: %q", path)
		}
	}
	return nil
}

// WriteExportFile writes the portable document with owner-only permissions.
func WriteExportFile(path string, doc WorkspaceRulesExport) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("export path is required")
	}
	if doc.FormatVersion == 0 {
		doc.FormatVersion = WorkspaceRulesExportFormat
	}
	if doc.ExportedAt == "" {
		doc.ExportedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := doc.Validate(); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode permissions export: %w", err)
	}
	payload = append(payload, '\n')
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create export directory: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("write export temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish export: %w", err)
	}
	return nil
}

// ReadExportFile loads a portable permissions document.
func ReadExportFile(path string) (WorkspaceRulesExport, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return WorkspaceRulesExport{}, fmt.Errorf("import path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return WorkspaceRulesExport{}, fmt.Errorf("read permissions export: %w", err)
	}
	var doc WorkspaceRulesExport
	if err := json.Unmarshal(data, &doc); err != nil {
		return WorkspaceRulesExport{}, fmt.Errorf("decode permissions export: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return WorkspaceRulesExport{}, err
	}
	if doc.FormatVersion == 0 {
		doc.FormatVersion = WorkspaceRulesExportFormat
	}
	return doc, nil
}

// Import merges or replaces durable rules from a portable document into the
// target workspace. Write paths that do not normalize under the workspace are
// skipped (not fatal) so machines with different trees can still import bash/MCP.
func (s *WorkspaceRulesStore) Import(workspace string, doc WorkspaceRulesExport, replace bool) (WorkspaceRules, int, error) {
	if s == nil {
		return WorkspaceRules{}, 0, fmt.Errorf("workspace rules store is nil")
	}
	if err := doc.Validate(); err != nil {
		return WorkspaceRules{}, 0, err
	}
	workspace, err := canonicalizeWorkspace(workspace)
	if err != nil {
		return WorkspaceRules{}, 0, err
	}
	var rules WorkspaceRules
	if replace {
		rules = WorkspaceRules{Version: workspaceRulesVersion, Workspace: workspace}
	} else {
		rules, err = s.Load(workspace)
		if err != nil {
			return WorkspaceRules{}, 0, err
		}
	}
	added := 0
	for _, prefix := range doc.BashPrefixes {
		normalized, ok := NormalizeBashPattern(prefix)
		if !ok {
			if lit, lok := NormalizeBashPrefix(prefix); lok {
				normalized = lit
			} else {
				continue
			}
		}
		if !containsString(rules.BashPrefixes, normalized) {
			if len(rules.BashPrefixes) >= maxBashPrefixes {
				break
			}
			rules.BashPrefixes = append(rules.BashPrefixes, normalized)
			added++
		}
	}
	for _, tool := range doc.MCPTools {
		normalized, ok := NormalizeMCPToolName(tool)
		if !ok {
			continue
		}
		if !containsString(rules.MCPTools, normalized) {
			if len(rules.MCPTools) >= maxMCPTools {
				break
			}
			rules.MCPTools = append(rules.MCPTools, normalized)
			added++
		}
	}
	for _, server := range doc.MCPServers {
		normalized, ok := NormalizeMCPServerName(server)
		if !ok {
			continue
		}
		if !containsString(rules.MCPServers, normalized) {
			if len(rules.MCPServers) >= maxMCPServers {
				break
			}
			rules.MCPServers = append(rules.MCPServers, normalized)
			added++
		}
	}
	for _, path := range doc.WritePaths {
		// Paths in exports are already relative; re-check against target workspace.
		rel := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
		if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
			continue
		}
		// Ensure the relative path is not an escape via further cleaning.
		if strings.Contains(rel, "..") {
			continue
		}
		if !containsString(rules.WritePaths, rel) {
			if len(rules.WritePaths) >= maxWritePaths {
				break
			}
			rules.WritePaths = append(rules.WritePaths, rel)
			added++
		}
	}
	rules = sanitizeWorkspaceRules(rules, workspace)
	if err := s.Save(rules); err != nil {
		return WorkspaceRules{}, 0, err
	}
	return rules, added, nil
}

// ClearAll removes every durable rule for the workspace.
func (s *WorkspaceRulesStore) ClearAll(workspace string) (WorkspaceRules, error) {
	workspace, err := canonicalizeWorkspace(workspace)
	if err != nil {
		return WorkspaceRules{}, err
	}
	rules := WorkspaceRules{Version: workspaceRulesVersion, Workspace: workspace}
	if err := s.Save(rules); err != nil {
		return WorkspaceRules{}, err
	}
	return rules, nil
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// DefaultExportFileName is the suggested filename for a permissions export.
const DefaultExportFileName = "sonar-permissions.json"
