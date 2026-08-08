package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

type permissionsAction int

const (
	permissionsToggleAcceptEdits permissionsAction = iota
	permissionsClearSession
	permissionsExport
	permissionsImportHint
	permissionsClearRules
	permissionsRevokeSession
	permissionsForgetBash
	permissionsForgetMCP
	permissionsForgetMCPServer
	permissionsForgetPath
)

type permissionsItem struct {
	action      permissionsAction
	title       string
	value       string
	description string
	data        string // tool name, pattern, or path for revoke/forget
}

func (i permissionsItem) Title() string {
	title := sanitizeTerminalSingleLine(i.title)
	value := sanitizeTerminalSingleLine(i.value)
	if value == "" {
		return title
	}
	return title + " · " + value
}

func (i permissionsItem) Description() string { return sanitizeTerminalSingleLine(i.description) }
func (i permissionsItem) FilterValue() string {
	return sanitizeTerminalSingleLine(i.title + " " + i.value + " " + i.data)
}

// PermissionsPanelState is a transient list of posture, session grants, and
// durable workspace rules. The parent Model owns every side effect.
type PermissionsPanelState struct {
	List       list.Model
	ItemHeight int
	Compact    bool
}

func newPermissionsPanelState(items []permissionsItem, terminalWidth, terminalHeight int, isDark bool, themeID string, profiles ...GlyphProfile) *PermissionsPanelState {
	profile := resolveGlyphProfile(profiles...)
	listItems := make([]list.Item, len(items))
	for i := range items {
		listItems[i] = items[i]
	}
	compact := compactSettingsRows(terminalWidth, terminalHeight)
	delegate := newPickerDelegate(isDark, compact, themeID, profile)
	itemHeight := delegate.Height()
	width := pickerListWidth(terminalWidth)
	height := pickerListHeight(terminalHeight, len(listItems)*itemHeight+2, 4)
	l := list.New(listItems, delegate, width, height)
	configurePickerList(&l, isDark, themeID)
	configurePickerListGlyphProfile(&l, profile)
	l.Title = "Permissions"
	setSettingsTitleDensity(&l, compact)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()
	return &PermissionsPanelState{List: l, ItemHeight: itemHeight, Compact: compact}
}

func (m *Model) permissionsPanelItems() []permissionsItem {
	items := make([]permissionsItem, 0, 16)
	acceptValue := "Off"
	acceptDesc := "Toggle accept-workspace-edits for write/edit/mkdir"
	if m.acceptWorkspaceEditsEnabled() {
		acceptValue = "On"
		acceptDesc = "Workspace file edits auto-approve · press enter to turn off"
	}
	if m.skipApprovalsEnabled() {
		acceptValue = "Unavailable"
		acceptDesc = "Skip-approvals is active; accept-edits is subordinate"
	}
	items = append(items, permissionsItem{
		action: permissionsToggleAcceptEdits, title: "Accept workspace edits",
		value: acceptValue, description: acceptDesc,
	})

	sessionCount := 0
	if m.agent != nil {
		sessionCount = len(m.agent.ListSessionApprovalSummary())
	}
	items = append(items, permissionsItem{
		action: permissionsClearSession, title: "Clear session grants",
		value:       fmt.Sprintf("%d", sessionCount),
		description: "Drop every process-local session approval grant",
	})

	if m.agent != nil {
		for _, grant := range m.agent.ListSessionApprovalSummary() {
			tool := strings.SplitN(grant, " · ", 2)[0]
			items = append(items, permissionsItem{
				action: permissionsRevokeSession, title: "Revoke session",
				value: grant, description: "Remove this process-local grant", data: tool,
			})
		}
	}

	var rules permission.WorkspaceRules
	if m.agent != nil {
		rules = m.agent.WorkspaceRulesSnapshot()
	}
	items = append(items, permissionsItem{
		action: permissionsExport, title: "Export workspace rules",
		value:       "JSON",
		description: "Write portable rules to " + permission.DefaultExportFileName,
	})
	items = append(items, permissionsItem{
		action: permissionsImportHint, title: "Import workspace rules",
		value:       "slash",
		description: "Use /permissions import [--replace] <path>",
	})
	ruleTotal := len(rules.BashPrefixes) + len(rules.MCPTools) + len(rules.MCPServers) + len(rules.WritePaths)
	items = append(items, permissionsItem{
		action: permissionsClearRules, title: "Clear workspace rules",
		value:       fmt.Sprintf("%d", ruleTotal),
		description: "Remove every durable bash/MCP/path rule for this workspace",
	})

	for _, prefix := range rules.BashPrefixes {
		items = append(items, permissionsItem{
			action: permissionsForgetBash, title: "Bash rule",
			value: prefix, description: "Remove this durable bash pattern", data: prefix,
		})
	}
	for _, tool := range rules.MCPTools {
		items = append(items, permissionsItem{
			action: permissionsForgetMCP, title: "MCP rule",
			value: tool, description: "Remove this durable MCP tool allow", data: tool,
		})
	}
	for _, server := range rules.MCPServers {
		items = append(items, permissionsItem{
			action: permissionsForgetMCPServer, title: "MCP server rule",
			value: server, description: "Remove this whole-server allow (every tool, approval only)", data: server,
		})
	}
	for _, path := range rules.WritePaths {
		items = append(items, permissionsItem{
			action: permissionsForgetPath, title: "Path rule",
			value: path, description: "Remove this durable write/edit/mkdir path", data: path,
		})
	}
	return items
}

func (m *Model) openPermissionsPanel() {
	// openSettingsChild may set overlayParent=Settings before calling this.
	m.permissionsPanelState = newPermissionsPanelState(m.permissionsPanelItems(), m.width, m.height, m.isDark, m.themeID, m.glyphProfile)
	m.overlay = OverlayPermissions
	m.input.Blur()
}

func (m *Model) refreshPermissionsPanel() {
	if m.permissionsPanelState == nil {
		return
	}
	selected := m.permissionsPanelState.List.Index()
	m.permissionsPanelState = newPermissionsPanelState(m.permissionsPanelItems(), m.width, m.height, m.isDark, m.themeID, m.glyphProfile)
	if selected >= 0 && selected < len(m.permissionsPanelState.List.Items()) {
		m.permissionsPanelState.List.Select(selected)
	}
}

func (m *Model) closePermissionsPanel() {
	m.permissionsPanelState = nil
	m.closeOverlayToParent()
}

func (m *Model) activatePermissionsItem(item permissionsItem) tea.Cmd {
	switch item.action {
	case permissionsToggleAcceptEdits:
		if m.skipApprovalsEnabled() {
			m.entries = append(m.entries, ChatEntry{
				Kind: "system", Content: "Accept-edits is unavailable while approval prompts are skipped.",
			})
			m.refreshTranscript()
			return nil
		}
		if m.acceptWorkspaceEditsEnabled() {
			m.SetApprovalPosture(ApprovalPosturePrompted)
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Accept workspace edits disabled."})
		} else {
			m.SetApprovalPosture(ApprovalPostureAcceptWorkspaceEdits)
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: "Accept workspace edits enabled. write/edit/mkdir inside the workspace no longer prompt.",
			})
		}
		m.refreshTranscript()
		m.refreshPermissionsPanel()
	case permissionsClearSession:
		count := 0
		if m.agent != nil {
			count = m.agent.RevokeSessionApprovals("")
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: fmt.Sprintf("Cleared %d process-local session approval grant%s.", count, pluralSuffix(count)),
		})
		m.refreshTranscript()
		m.refreshPermissionsPanel()
	case permissionsRevokeSession:
		count := 0
		if m.agent != nil {
			count = m.agent.RevokeSessionApprovals(item.data)
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: fmt.Sprintf("Revoked %d session grant%s for %q.", count, pluralSuffix(count), item.data),
		})
		m.refreshTranscript()
		m.refreshPermissionsPanel()
	case permissionsExport:
		return m.exportWorkspaceRules("")
	case permissionsImportHint:
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: "Import with: /permissions import [--replace] <path>",
		})
		m.refreshTranscript()
	case permissionsClearRules:
		if m.agent == nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
			m.refreshTranscript()
			return nil
		}
		if _, err := m.agent.ClearWorkspaceRules(); err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Cleared all durable workspace rules."})
		}
		m.refreshTranscript()
		m.refreshPermissionsPanel()
	case permissionsForgetBash:
		if m.agent == nil {
			return nil
		}
		if _, removed, err := m.agent.RemoveWorkspaceBashPrefix(item.data); err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else if !removed {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No matching bash rule."})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Removed durable bash rule."})
		}
		m.refreshTranscript()
		m.refreshPermissionsPanel()
	case permissionsForgetMCP:
		if m.agent == nil {
			return nil
		}
		if _, removed, err := m.agent.RemoveWorkspaceMCPTool(item.data); err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else if !removed {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No matching MCP rule."})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Removed durable MCP rule."})
		}
		m.refreshTranscript()
		m.refreshPermissionsPanel()
	case permissionsForgetMCPServer:
		if m.agent == nil {
			return nil
		}
		if _, removed, err := m.agent.RemoveWorkspaceMCPServer(item.data); err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else if !removed {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No matching MCP server rule."})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Removed whole-server MCP rule."})
		}
		m.refreshTranscript()
		m.refreshPermissionsPanel()
	case permissionsForgetPath:
		if m.agent == nil {
			return nil
		}
		if _, removed, err := m.agent.RemoveWorkspaceWritePath(item.data); err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		} else if !removed {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No matching path rule."})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Removed durable path rule."})
		}
		m.refreshTranscript()
		m.refreshPermissionsPanel()
	}
	return nil
}

func (m *Model) exportWorkspaceRules(path string) tea.Cmd {
	if m.agent == nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
		m.refreshTranscript()
		return nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = m.agent.DefaultWorkspaceRulesExportPath()
	}
	doc, err := m.agent.ExportWorkspaceRulesToFile(path)
	if err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
	} else {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: fmt.Sprintf("Exported %d workspace rule%s to %s", doc.RuleCount(), pluralSuffix(doc.RuleCount()), path),
		})
	}
	m.refreshTranscript()
	m.refreshPermissionsPanel()
	return nil
}

func (m *Model) importWorkspaceRules(data string) tea.Cmd {
	if m.agent == nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Agent is unavailable."})
		m.refreshTranscript()
		return nil
	}
	replace := false
	path := strings.TrimSpace(data)
	if strings.HasPrefix(path, "replace|") {
		replace = true
		path = strings.TrimPrefix(path, "replace|")
	}
	rules, added, err := m.agent.ImportWorkspaceRulesFromFile(path, replace)
	if err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
	} else {
		mode := "merged"
		if replace {
			mode = "replaced"
		}
		total := len(rules.BashPrefixes) + len(rules.MCPTools) + len(rules.WritePaths)
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: fmt.Sprintf("Import %s: +%d new · %d total durable rules.", mode, added, total),
		})
	}
	m.refreshTranscript()
	m.refreshPermissionsPanel()
	return nil
}

// auditWorkspaceApprovals reports the workspace's most frequent approval
// prompts from the durable execution ledger, most-interrupting first. Each row
// is the host's own bounded projection (rule tripped, derivable grant prefix)
// — never raw arguments — so the operator can turn the top interruptions into
// durable rules instead of re-living them.
func (m *Model) auditWorkspaceApprovals() tea.Cmd {
	if m.sessionStore == nil || m.agent == nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Session persistence is unavailable."})
		m.refreshTranscript()
		return nil
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		m.refreshTranscript()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stats, err := m.sessionStore.WorkspaceApprovalPromptStats(ctx, workspaceID, 15)
	if err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
		m.refreshTranscript()
		return nil
	}
	if len(stats) == 0 {
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "No approval prompts recorded for this workspace."})
		m.refreshTranscript()
		m.resumeFollow()
		return nil
	}
	var b strings.Builder
	b.WriteString("Most frequent approval prompts in this workspace:\n")
	for _, stat := range stats {
		detail := strings.TrimSpace(strings.TrimPrefix(stat.Detail, "interactive approval requested"))
		detail = strings.TrimSpace(strings.TrimPrefix(detail, ":"))
		line := stat.ToolName
		if detail != "" {
			line += " — " + detail
		}
		when := stat.LastAt
		if len(when) > 10 {
			when = when[:10]
		}
		fmt.Fprintf(&b, "  %3d× %s  (last %s)\n", stat.Count, line, when)
	}
	b.WriteString("Durable cures: /permissions allow-bash <prefix> · allow-mcp <server__tool> · allow-path <path>")
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: b.String()})
	m.refreshTranscript()
	m.resumeFollow()
	return nil
}

func (m *Model) renderPermissionsPanel() string {
	if m.permissionsPanelState == nil {
		return ""
	}
	content := m.permissionsPanelState.List.View()
	if m.permissionsPanelState.Compact && m.width >= 36 {
		if item, ok := m.permissionsPanelState.List.SelectedItem().(permissionsItem); ok && strings.TrimSpace(item.description) != "" {
			detail := truncateDisplay(strings.TrimSpace(item.description), settingsDetailWidth(m.width))
			content = strings.TrimRight(content, "\n") + "\n" +
				m.styles.OverlayDim.Render("  "+detail)
		}
	}
	return m.renderPickerFrame(content, m.pickerNavigationFooter(false))
}
