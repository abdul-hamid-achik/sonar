package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// renderDecisionPrompt keeps every safety action labelled. Wide terminals get
// one compact row; narrow terminals keep identity on the first row and greedily
// wrap complete key/action pairs below it instead of truncating the decisions.
// Horizontal alignment uses the transcript content grid so decision rows share
// OriginX=3 with tools and approval surfaces.
func (m *Model) renderDecisionPrompt(subject, detail string, choices ...keyHint) string {
	grid := m.contentGrid()
	inner := grid.ContentWidth()
	lineWidth := grid.LineWidth()
	lead := grid.Prefix(" ")
	choiceRows := m.renderDecisionChoiceRows(inner, choices...)

	subjectView := m.styles.ApprovalPrompt.Render(strings.TrimSpace(subject))
	detail = strings.TrimSpace(detail)
	choiceLine := ""
	if len(choiceRows) == 1 {
		choiceLine = choiceRows[0]
	}

	// Give semantic detail every remaining cell while preserving the complete
	// choice grammar at the right edge.
	if choiceLine != "" {
		prefix := lead + subjectView
		if detail != "" {
			prefix += " "
		}
		suffix := m.styles.StatusText.Render(" · ") + choiceLine
		detailBudget := lineWidth - lipgloss.Width(prefix) - lipgloss.Width(suffix)
		if detail == "" || detailBudget >= 6 {
			if detail != "" {
				prefix += m.styles.StatusText.Render(truncateDisplay(detail, detailBudget))
			}
			return prefix + suffix
		}
	}

	header := lead + subjectView
	if detail != "" {
		budget := max(1, lineWidth-lipgloss.Width(header)-1)
		header += " " + m.styles.StatusText.Render(truncateDisplay(detail, budget))
	}
	rows := []string{truncateDisplay(header, lineWidth)}
	for _, row := range choiceRows {
		rows = append(rows, lead+row)
	}
	return strings.Join(rows, "\n")
}

func (m *Model) renderDecisionChoiceRows(width int, choices ...keyHint) []string {
	if width <= 0 {
		return nil
	}
	separator := m.styles.StatusText.Render(" · ")
	rows := make([]string, 0, len(choices))
	current := ""
	for _, choice := range choices {
		keyLabel := strings.ToLower(strings.TrimSpace(choice.Key))
		action := strings.ToLower(strings.TrimSpace(choice.Action))
		if keyLabel == "" && action == "" {
			continue
		}
		part := m.styles.FocusIndicator.Render(keyLabel)
		if action != "" {
			if keyLabel != "" {
				part += " "
			}
			part += m.styles.StatusText.Render(action)
		}
		part = truncateDisplay(part, width)
		candidate := part
		if current != "" {
			candidate = current + separator + part
		}
		if current != "" && lipgloss.Width(candidate) > width {
			rows = append(rows, current)
			current = part
			continue
		}
		current = candidate
	}
	if current != "" {
		rows = append(rows, current)
	}
	return rows
}
