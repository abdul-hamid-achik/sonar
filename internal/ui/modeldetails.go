package ui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// renderCompactOllamaModelDetails keeps the minimum terminal actionable. It
// projects one bounded line per decision-relevant group instead of letting the
// complete metadata table push the footer and closing border off-screen.
func renderCompactOllamaModelDetails(model OllamaModelDescriptor, width int, isDark bool, themeID string) string {
	width = max(1, width)
	palette := outputSemanticPalette(isDark, themeID)
	titleStyle := lipgloss.NewStyle().Foreground(palette.Accent).Bold(true)
	textStyle := lipgloss.NewStyle().Foreground(palette.Text)
	dimStyle := lipgloss.NewStyle().Foreground(palette.Dim)
	warningStyle := lipgloss.NewStyle().Foreground(palette.Warning)

	appendLine := func(lines []string, style lipgloss.Style, value string) []string {
		value = sanitizeTerminalSingleLine(value)
		if value == "" {
			return lines
		}
		return append(lines, style.Render(truncateDisplay(value, width)))
	}

	title := modelDisplayName(model)
	lines := appendLine(nil, titleStyle, title)
	lines = appendLine(lines, textStyle, modelSelectionState(model))

	modelParts := make([]string, 0, 4)
	if value := sanitizeTerminalSingleLine(model.ParameterSize); value != "" {
		modelParts = append(modelParts, value)
	}
	if value := sanitizeTerminalSingleLine(model.Quantization); value != "" {
		modelParts = append(modelParts, value)
	}
	if size := humanModelBytes(model.SizeBytes); size != "" {
		modelParts = append(modelParts, size+" weights")
	}
	if size := humanModelBytes(model.SizeVRAM); size != "" {
		modelParts = append(modelParts, size+" VRAM")
	}
	if len(modelParts) > 0 {
		lines = appendLine(lines, dimStyle, "Model · "+strings.Join(modelParts, " · "))
	}

	contextParts := make([]string, 0, 3)
	if model.AllocatedContext > 0 {
		contextParts = append(contextParts, compactTokenCount(model.AllocatedContext)+" active")
	}
	if model.EffectiveContext > 0 && model.EffectiveContext != model.AllocatedContext {
		contextParts = append(contextParts, compactTokenCount(model.EffectiveContext)+" effective")
	}
	if model.ContextLength > 0 && model.ContextLength != model.EffectiveContext {
		contextParts = append(contextParts, compactTokenCount(model.ContextLength)+" max")
	}
	if len(contextParts) > 0 {
		lines = appendLine(lines, dimStyle, "Context · "+strings.Join(contextParts, " · "))
	}
	if capabilities := compactCapabilities(model.Capabilities); capabilities != "" {
		lines = appendLine(lines, dimStyle, "Can · "+strings.ReplaceAll(capabilities, "+", " · "))
	}
	if reason := sanitizeTerminalSingleLine(model.Reason); reason != "" {
		lines = appendLine(lines, warningStyle, "Note · "+reason)
	}
	if model.Source != OllamaModelLocal {
		lines = appendLine(lines, dimStyle, "Cloud · remote prompts")
	}
	return strings.Join(lines, "\n")
}

// renderOllamaModelDetails is a transport-independent details projection for a
// modal owned by the parent model. It remains useful while enrichment loads:
// unknown metadata is omitted instead of rendered as misleading zero values.
func renderOllamaModelDetails(model OllamaModelDescriptor, width int, isDark bool, themeID string) string {
	width = max(24, width)
	palette := outputSemanticPalette(isDark, themeID)
	label := lipgloss.NewStyle().Foreground(palette.Dim).Width(min(15, width/3))
	value := lipgloss.NewStyle().Foreground(palette.Text)
	accent := lipgloss.NewStyle().Foreground(palette.Accent).Bold(true)
	status := "Available"
	statusColor := palette.Success
	if !model.Selectable {
		status, statusColor = "Unavailable", palette.Warning
	} else if !model.Fit {
		status, statusColor = "Outside default profile", palette.Warning
	} else if model.Running {
		status = "Running"
	}
	source := strings.ToLower(modelGroupLabel(descriptorGroup(OllamaModelDescriptor{Source: model.Source, Selectable: true, Fit: true})))
	rows := [][2]string{{"Source", source}, {"Status", status}}
	if parameterSize := sanitizeTerminalSingleLine(model.ParameterSize); parameterSize != "" {
		rows = append(rows, [2]string{"Parameters", parameterSize})
	}
	if quantization := sanitizeTerminalSingleLine(model.Quantization); quantization != "" {
		rows = append(rows, [2]string{"Quantization", quantization})
	}
	if size := humanModelBytes(model.SizeBytes); size != "" {
		rows = append(rows, [2]string{"Weights", size})
	}
	if model.ContextLength > 0 {
		rows = append(rows, [2]string{"Max context", compactTokenCount(model.ContextLength) + " tokens"})
	}
	// "Effective" only means something when policy lowered the window below the
	// model's maximum. Printing the same number twice under two labels invited
	// the reader to look for a difference that is not there.
	if model.EffectiveContext > 0 && model.EffectiveContext != model.ContextLength {
		rows = append(rows, [2]string{"Effective", compactTokenCount(model.EffectiveContext) + " tokens"})
	}
	if model.AllocatedContext > 0 {
		rows = append(rows, [2]string{"Active context", compactTokenCount(model.AllocatedContext) + " tokens"})
	}
	if size := humanModelBytes(model.SizeVRAM); size != "" {
		rows = append(rows, [2]string{"VRAM", size})
	}
	if caps := compactCapabilities(model.Capabilities); caps != "" {
		rows = append(rows, [2]string{"Capabilities", caps})
	}

	var b strings.Builder
	title := modelDisplayName(model)
	b.WriteString(accent.Render(title))
	if model.Current {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(palette.Success).Render("✓ current"))
	}
	for _, row := range rows {
		b.WriteString("\n")
		b.WriteString(label.Render(row[0]))
		b.WriteString(" ")
		rowStyle := value
		if row[0] == "Status" {
			rowStyle = lipgloss.NewStyle().Foreground(statusColor)
		}
		b.WriteString(rowStyle.Render(row[1]))
	}
	if reason := sanitizeTerminalSingleLine(model.Reason); reason != "" {
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Foreground(palette.Warning).Render(reason))
	}
	if model.Source != OllamaModelLocal {
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Foreground(palette.Dim).Render("Prompts leave this machine through Ollama."))
	}
	return lipgloss.NewStyle().Width(width).Render(b.String())
}

// handleOllamaModelDetailsResult applies a details receipt to the open
// model-details overlay.
func (m *Model) handleOllamaModelDetailsResult(msg OllamaModelDetailsResultMsg) {
	if m.modelDetailsState != nil && config.CanonicalModelName(m.modelDetailsState.Name) == config.CanonicalModelName(msg.Model.Name) {
		if msg.Err != nil {
			m.modelDetailsState.Reason = "Details unavailable: " + msg.Err.Error()
		} else {
			copy := msg.Model
			m.modelDetailsState = &copy
		}
	}
}
