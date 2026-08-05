package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

type RuntimeStatusState struct {
	Viewport viewport.Model
}

func (m *Model) openRuntimeStatus() {
	m.refreshRuntimeStatus(false)
	m.overlay = OverlayRuntimeStatus
	m.input.Blur()
}

func (m *Model) closeRuntimeStatus() {
	m.runtimeStatusState = nil
	m.closeOverlayToParent()
}

func (m *Model) refreshRuntimeStatus(preserveOffset bool) {
	offset := 0
	if preserveOffset && m.runtimeStatusState != nil {
		offset = m.runtimeStatusState.Viewport.YOffset()
	}
	width := pickerListWidth(m.width)
	content := m.buildRuntimeStatusContent(width)
	// Reserve one terminal row beyond the frame's calculated content so a
	// full-height centered overlay never clips its bottom border in the final
	// Bubble Tea view (notably at the supported 40x20 narrow test tier).
	height := min(max(1, lipgloss.Height(content)), max(1, m.height-6))
	vp := viewport.New(
		viewport.WithWidth(width),
		viewport.WithHeight(height),
	)
	vp.KeyMap.Up.SetEnabled(false)
	vp.KeyMap.Down.SetEnabled(false)
	vp.KeyMap.PageUp.SetEnabled(false)
	vp.KeyMap.PageDown.SetEnabled(false)
	vp.KeyMap.HalfPageUp.SetEnabled(false)
	vp.KeyMap.HalfPageDown.SetEnabled(false)
	vp.SetContent(content)
	vp.SetYOffset(offset)
	m.runtimeStatusState = &RuntimeStatusState{Viewport: vp}
}

func (m *Model) buildRuntimeStatusContent(width int) string {
	profile := m.agentProfile
	if profile == "" {
		profile = "Default"
	}
	routing := "Pinned"
	if !m.modelPinned {
		routing = "Auto"
	}
	lines := make([]string, 0, 18)
	toolSummary := fmt.Sprintf("%d visible", m.toolCount)
	var readGrants []agent.ReadGrant
	var writeGrants []agent.WriteGrant
	serverNames, failedServers, serverToolCounts := m.mcpRuntimeProjection()
	if m.agent != nil {
		toolSummary = runtimeToolAvailabilityLabel(m.agent.ToolAvailability())
		readGrants = m.agent.ReadGrants()
		writeGrants = m.agent.WriteGrants()
	}
	readScope := "workspace only"
	if len(readGrants) > 0 {
		grantLabel := "grants"
		if len(readGrants) == 1 {
			grantLabel = "grant"
		}
		readScope = fmt.Sprintf("%d temporary external %s", len(readGrants), grantLabel)
	}
	model := m.currentModelSurfaceLabel(false)
	modelRuntime := routing
	if model != "" {
		modelRuntime += " · " + model
	}
	mode := m.modeConfigs[m.presentedMode()].Label
	if m.goalRuntime != nil {
		mode += " · Goal Runtime"
	}
	connections := projectEcosystemConnections(serverNames, failedServers)
	if m.agent != nil {
		if workspace := compactWorkspacePath(m.agent.WorkDir(), runtimeStatusValueWidth(width)); workspace != "" {
			lines = append(lines, m.runtimeStatusRow("Workspace", workspace, width))
		}
	}
	lines = append(lines, m.runtimeStatusRow("Model", modelRuntime, width))
	if ping := m.chromeSpring.ping; ping.samples > 0 {
		// The static receipt behind the waiting-phase sonar trace
		// (sonar_ping.go): the model's first-response latency, readable with
		// no motion at all. This is the reduced-motion parity surface for the
		// fact the trace animates.
		lines = append(lines, m.runtimeStatusRow("Echo",
			fmt.Sprintf("first response · last %s · typical %s",
				formatWorkingElapsed(ping.last), formatWorkingElapsed(ping.baseline)),
			width,
		))
	}
	// The daemon version used to be spliced into the model picker's title
	// ("Ollama 0.31.2-test · models"), which is a heading, not a place to read
	// diagnostics. It belongs with the rest of the runtime facts.
	if version := sanitizeTerminalSingleLine(m.ollamaVersion); version != "" {
		lines = append(lines, m.runtimeStatusRow("Ollama", version, width))
	}
	if m.ollamaOffline {
		// Ping failure can mean either host transport or model selection/setup.
		// Keep the recovery conditional rather than presenting transport success
		// or failure as verified model availability.
		lines = append(lines, m.runtimeStatusRow(
			"Ollama",
			"setup needed · ctrl+o select or install a model; run ollama serve if the host is unavailable",
			width,
		))
	}
	lines = append(lines,
		m.runtimeStatusRow("Profile", profile, width),
		m.runtimeStatusRow("Mode", mode, width),
		m.runtimeStatusRow("Approval", m.approvalPostureRuntimeLabel(), width),
		m.runtimeStatusRow("Read scope", readScope, width),
		m.runtimeStatusRow("Tools", toolSummary, width),
		m.runtimeStatusRow("MCP", summarizeConnectionHealth(connections), width),
	)
	if label := sessionDisplayLabel(m.sessionPublicID, m.activeSessionTitle, 72); label != "" {
		lines = append(lines, m.runtimeStatusRow("Session", label, width))
	}
	contextRouting := contextRoutingRuntimeLabel(agent.CapabilityRoutingHostUnavailable)
	if m.agent != nil {
		contextRouting = contextRoutingRuntimeLabel(m.agent.CapabilityRoutingState())
	}
	lines = append(lines, m.runtimeStatusRow("Context route", contextRouting, width))
	if m.lastCapabilityRoute != nil {
		route := sanitizeCapabilityRoute(*m.lastCapabilityRoute)
		if capabilityRouteRenderable(route) {
			value := capabilityRouteLabel(route)
			if detail := capabilityRouteDetail(route); detail != "" {
				value += " · " + detail
			}
			lines = append(lines, m.runtimeStatusRow("Last MCP route", value+" · advisory only", width))
		}
	}
	if m.promptTokens > 0 && m.numCtx > 0 {
		percent := min(100, max(0, m.promptTokens*100/m.numCtx))
		lines = append(lines, m.runtimeStatusRow("Context",
			fmt.Sprintf("~%s / %s · %d%%", formatTokens(m.promptTokens), formatTokens(m.numCtx), percent),
			width,
		))
	}
	if m.sessionTurnCount > 0 {
		lines = append(lines, m.runtimeStatusRow("Usage",
			fmt.Sprintf("%d %s · %s output", m.sessionTurnCount, pluralizeNoun(m.sessionTurnCount, "turn", "turns"), formatTokens(m.sessionEvalTotal)),
			width,
		))
	}
	if m.iceEnabled {
		lines = append(lines, m.runtimeStatusRow("ICE",
			fmt.Sprintf("enabled · %d %s", m.iceConversations, pluralizeNoun(m.iceConversations, "conversation", "conversations")),
			width,
		))
	} else {
		lines = append(lines, m.runtimeStatusRow("ICE", "disabled", width))
	}
	if len(connections) > 0 {
		lines = append(lines, "", m.styles.OverlayAccent.Render("Tool connections"))
		for _, connection := range connections {
			value := connection.Health.String() + " · " + connection.Role
			if count := serverToolCounts[strings.ToLower(connection.Name)]; connection.Health == capabilityConnected && count > 0 {
				value += fmt.Sprintf(" · %d %s", count, pluralizeNoun(count, "tool", "tools"))
			}
			lines = append(lines, m.runtimeStatusRow(connection.Label, value, width))
			if connection.Health == capabilityUnavailable {
				if connection.Detail != "" {
					lines = append(lines, m.runtimeStatusRow("", connection.Detail, width))
				}
				if connection.Recovery != "" {
					lines = append(lines, m.runtimeStatusRow("", connection.Recovery, width))
				}
			}
		}
	} else {
		lines = append(lines, "", m.styles.OverlayDim.Render(
			wrapText("MCP is optional. Add a server in sonar.yaml or the XDG config when you need ecosystem tools.", max(1, width)),
		))
	}
	if len(readGrants) > 0 {
		lines = append(lines, "", m.styles.OverlayAccent.Render("External read access"))
		for _, grant := range readGrants {
			label := "Directory"
			if grant.Kind == agent.ReadGrantExactFile {
				label = "Exact file"
			}
			// Runtime is scrollable, so show the complete escaped authority instead
			// of independently compacting paths into potentially identical tails.
			lines = append(lines, m.runtimeStatusRow(label, displayWorkspacePath(grant.Path), width))
		}
		lines = append(lines, m.styles.OverlayDim.Render(
			wrapText("Temporary and not saved with sessions · /scope clear-read revokes all · shell writes remain workspace-only; typed-write grants expire with the turn", max(1, width)),
		))
	}
	if len(writeGrants) > 0 {
		lines = append(lines, "", m.styles.OverlayAccent.Render("Turn-bound typed-write access"))
		for _, grant := range writeGrants {
			label := "Directory"
			if grant.Kind == agent.WriteGrantExactFile {
				label = "Exact file"
			}
			lines = append(lines, m.runtimeStatusRow(label, displayWorkspacePath(grant.Path), width))
		}
		lines = append(lines, m.styles.OverlayDim.Render(
			wrapText("Built-in typed operations only · expires when this turn settles · raw shell remains confined to the startup workspace", max(1, width)),
		))
	}
	return strings.Join(lines, "\n")
}

func runtimeToolAvailabilityLabel(availability agent.ToolAvailability) string {
	label := fmt.Sprintf("%d ready · %d local · %d MCP", availability.Ready(), availability.Local, availability.MCPConnected)
	if unavailable := availability.MCPRetained - availability.MCPConnected; unavailable > 0 {
		label += fmt.Sprintf(" · %d MCP unavailable", unavailable)
	}
	return label
}

func contextRoutingRuntimeLabel(state agent.CapabilityRoutingHostState) string {
	switch state {
	case agent.CapabilityRoutingHostReady:
		return "ready · MCPHub advisory"
	case agent.CapabilityRoutingHostServerUnavailable:
		return "not ready · MCPHub server unavailable"
	default:
		return "not exposed · policy/catalog"
	}
}

func (m *Model) runtimeStatusRow(label, value string, width int) string {
	label = sanitizeTerminalSingleLine(label)
	value = sanitizeTerminalSingleLine(value)
	valueWidth := runtimeStatusValueWidth(width)
	labelWidth := max(1, width-valueWidth)
	wrapped := strings.Split(wrapText(strings.TrimSpace(value), valueWidth), "\n")
	if len(wrapped) == 0 {
		wrapped = []string{""}
	}

	var b strings.Builder
	b.WriteString(m.styles.OverlayAccent.Width(labelWidth).Render(truncateDisplay(label, labelWidth-1)))
	b.WriteString(m.styles.OverlayDim.Render(wrapped[0]))
	for _, line := range wrapped[1:] {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(" ", labelWidth))
		b.WriteString(m.styles.OverlayDim.Render(line))
	}
	return b.String()
}

func runtimeStatusValueWidth(width int) int {
	// The longest core labels (Context route and Last MCP route) remain whole at
	// the normal 58-column overlay width. Narrow overlays still divide space
	// proportionally and truncate only when the terminal makes that unavoidable.
	const normalLabelWidth = 16
	labelWidth := min(normalLabelWidth, max(1, width/3))
	return max(1, width-labelWidth)
}

func displayWorkspacePath(path string) string {
	display := workspaceDisplayPath(path)
	if display == "" {
		return ""
	}
	return terminalSafePathLiteral(display)
}

func workspaceDisplayPath(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	if home, err := os.UserHomeDir(); err == nil {
		home = filepath.Clean(home)
		if clean == home {
			return "~"
		}
		if strings.HasPrefix(clean, home+string(filepath.Separator)) {
			return "~" + strings.TrimPrefix(clean, home)
		}
	}
	return clean
}

// terminalSafePathLiteral keeps ordinary paths unchanged, but quotes any path
// whose bytes or runes could alter terminal state or make an escaped identity
// ambiguous. strconv's graphic form exposes controls instead of silently
// deleting them, so the visual projection remains faithful to the authority.
func terminalSafePathLiteral(path string) string {
	quote := !utf8.ValidString(path) || strings.TrimSpace(path) != path
	if !quote {
		for _, character := range path {
			if !unicode.IsGraphic(character) || character == '\\' || character == '"' {
				quote = true
				break
			}
		}
	}
	if quote {
		return strconv.QuoteToGraphic(path)
	}
	return path
}

// compactWorkspacePath keeps the identifying end of a workspace visible at
// narrow widths. A leading path fragment is less useful than the repository
// name and its immediate parent.
func compactWorkspacePath(path string, width int) string {
	display := workspaceDisplayPath(path)
	safeDisplay := terminalSafePathLiteral(display)
	if display == "" || lipgloss.Width(safeDisplay) <= width {
		return safeDisplay
	}

	base := filepath.Base(display)
	parent := filepath.Base(filepath.Dir(display))
	if parent != "." && parent != string(filepath.Separator) {
		candidate := terminalSafePathLiteral("…" + string(filepath.Separator) + filepath.Join(parent, base))
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	candidate := terminalSafePathLiteral("…" + string(filepath.Separator) + base)
	if lipgloss.Width(candidate) <= width {
		return candidate
	}
	return truncateDisplay(terminalSafePathLiteral(base), width)
}

func (m *Model) renderRuntimeStatus() string {
	if m.runtimeStatusState == nil {
		return ""
	}
	vp := &m.runtimeStatusState.Viewport
	content := m.styles.OverlayTitle.Render("Runtime") + "\n\n" + vp.View()
	hints := []keyHint{{Key: "esc/q", Action: m.overlayCloseLabel()}}
	if !vp.AtBottom() {
		hints = append(hints,
			keyHint{Key: "j/k", Action: "scroll"},
			keyHint{Key: "↓", Action: "more"},
		)
	} else if vp.YOffset() > 0 {
		hints = append(hints,
			keyHint{Key: "j/k", Action: "scroll"},
			keyHint{Key: fmt.Sprintf("%.0f%%", vp.ScrollPercent()*100)},
		)
	}
	return m.renderPickerFrame(content, m.renderKeyHints(pickerListWidth(m.width), hints...))
}
