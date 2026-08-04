package ui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// GoalPhase is the lifecycle state displayed by RenderGoalStatusLine.
type GoalPhase string

const (
	GoalPhaseActive    GoalPhase = "active"
	GoalPhasePaused    GoalPhase = "paused"
	GoalPhaseExhausted GoalPhase = "exhausted"
	GoalPhaseCompleted GoalPhase = "completed"
	GoalPhaseDropped   GoalPhase = "dropped"
	GoalPhaseBlocked   GoalPhase = "blocked"
)

// GoalSummary contains only the presentation data needed by the compact goal
// line. Zero budgets are omitted rather than presented as misleading limits.
type GoalSummary struct {
	Objective   string
	Phase       GoalPhase
	TurnsUsed   int64
	TurnBudget  int64
	TokensUsed  int64
	TokenBudget int64
	Elapsed     time.Duration
	TimeBudget  time.Duration
}

type goalStatusMetric struct {
	text  string
	alert bool
}

// RenderGoalStatusLine renders a single adaptive, width-safe status row for
// headers, composer chrome, or transient receipts. State is always conveyed by
// a glyph and text, never color alone.
func RenderGoalStatusLine(summary GoalSummary, width int, isDark bool, themeID string, profiles ...GlyphProfile) string {
	if width <= 0 {
		return ""
	}

	styles := NewStyles(isDark, themeID)
	palette := outputSemanticPalette(isDark, themeID)
	profile := resolveGlyphProfile(profiles...)
	glyph, label, phaseColor := goalPhasePresentation(summary.Phase, palette, profile)
	phase := lipgloss.NewStyle().
		Foreground(phaseColor).
		Bold(summary.Phase == GoalPhaseActive || summary.Phase == GoalPhaseExhausted || summary.Phase == GoalPhaseBlocked).
		Render(glyph + " " + label)
	if lipgloss.Width(phase) >= width {
		return truncateDisplay(phase, width)
	}

	objective := sanitizeTerminalSingleLine(summary.Objective)
	if objective == "" {
		objective = "untitled goal"
	}
	metrics := goalStatusMetrics(summary)
	separatorText := goalStatusSeparator(profile)
	separator := styles.StatusText.Render(separatorText)

	// Preserve a useful objective before progressively adding budget detail.
	const minimumObjectiveWidth = 7
	visibleMetrics := make([]goalStatusMetric, 0, len(metrics))
	for _, metric := range metrics {
		candidate := append(append([]goalStatusMetric(nil), visibleMetrics...), metric)
		metricWidth := goalMetricsWidth(candidate, profile)
		if lipgloss.Width(phase)+lipgloss.Width(separator)*2+minimumObjectiveWidth+metricWidth <= width {
			visibleMetrics = candidate
		}
	}

	right := renderGoalMetrics(visibleMetrics, palette, profile)
	objectiveWidth := width - lipgloss.Width(phase) - lipgloss.Width(separator)
	if right != "" {
		objectiveWidth -= lipgloss.Width(separator) + lipgloss.Width(right)
	}
	if objectiveWidth < 1 {
		return truncateDisplay(phase, width)
	}

	line := phase + separator + styles.StatusText.Render(truncateDisplay(objective, objectiveWidth))
	if right != "" {
		line += separator + right
	}
	return truncateDisplay(line, width)
}

func goalPhasePresentation(phase GoalPhase, palette semanticPalette, profiles ...GlyphProfile) (glyph, label string, foreground color.Color) {
	profile := resolveGlyphProfile(profiles...)
	glyphs := glyphSet(profile)
	switch phase {
	case GoalPhaseActive:
		return glyphs.Selected, "active", palette.Accent
	case GoalPhasePaused:
		if profile == GlyphASCII {
			return "=", "paused", palette.Warning
		}
		return "Ⅱ", "paused", palette.Warning
	case GoalPhaseExhausted:
		return "!", "exhausted", palette.Warning
	case GoalPhaseCompleted:
		return glyphs.Success, "completed", palette.Success
	case GoalPhaseDropped:
		if profile == GlyphASCII {
			return "x", "dropped", palette.Muted
		}
		return "×", "dropped", palette.Muted
	case GoalPhaseBlocked:
		return "!", "blocked", palette.Error
	default:
		return glyphs.Unselected, "goal", palette.Muted
	}
}

func goalStatusSeparator(profile GlyphProfile) string {
	if resolveGlyphProfile(profile) == GlyphASCII {
		return " - "
	}
	return " · "
}

func goalStatusMetrics(summary GoalSummary) []goalStatusMetric {
	metrics := make([]goalStatusMetric, 0, 3)
	terminal := summary.Phase == GoalPhaseCompleted || summary.Phase == GoalPhaseDropped
	if summary.TurnBudget > 0 {
		metrics = append(metrics, goalStatusMetric{
			text:  fmt.Sprintf("%d/%d auto turns", max(0, summary.TurnsUsed), summary.TurnBudget),
			alert: !terminal && summary.TurnsUsed >= summary.TurnBudget,
		})
	}
	if summary.TokenBudget > 0 {
		metrics = append(metrics, goalStatusMetric{
			text:  fmt.Sprintf("%s/%s tok", formatGoalTokens(summary.TokensUsed), formatGoalTokens(summary.TokenBudget)),
			alert: !terminal && summary.TokensUsed >= summary.TokenBudget,
		})
	}
	// The runtime does not retain a terminal wall-clock receipt. Omitting the
	// live elapsed clock after completion/drop prevents a finished goal from
	// appearing to keep consuming its time budget.
	if summary.TimeBudget > 0 && !terminal {
		metrics = append(metrics, goalStatusMetric{
			text:  fmt.Sprintf("%s/%s", formatGoalDuration(summary.Elapsed), formatGoalDuration(summary.TimeBudget)),
			alert: summary.Elapsed >= summary.TimeBudget,
		})
	}
	return metrics
}

func goalMetricsWidth(metrics []goalStatusMetric, profiles ...GlyphProfile) int {
	if len(metrics) == 0 {
		return 0
	}
	parts := make([]string, len(metrics))
	for index := range metrics {
		parts[index] = metrics[index].text
	}
	return lipgloss.Width(strings.Join(parts, goalStatusSeparator(resolveGlyphProfile(profiles...))))
}

func renderGoalMetrics(metrics []goalStatusMetric, palette semanticPalette, profiles ...GlyphProfile) string {
	parts := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		color := palette.Dim
		if metric.alert {
			color = palette.Warning
		}
		parts = append(parts, lipgloss.NewStyle().Foreground(color).Render(metric.text))
	}
	return strings.Join(parts, lipgloss.NewStyle().Foreground(palette.Border).Render(
		goalStatusSeparator(resolveGlyphProfile(profiles...)),
	))
}

func formatGoalTokens(tokens int64) string {
	tokens = max(int64(0), tokens)
	switch {
	case tokens >= 1_000_000:
		if tokens%1_000_000 == 0 {
			return fmt.Sprintf("%dm", tokens/1_000_000)
		}
		return fmt.Sprintf("%.1fm", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		if tokens%1_000 == 0 {
			return fmt.Sprintf("%dk", tokens/1_000)
		}
		return fmt.Sprintf("%.1fk", float64(tokens)/1_000)
	default:
		return strconvFormatInt(tokens)
	}
}

func strconvFormatInt(value int64) string {
	return fmt.Sprintf("%d", value)
}

func formatGoalDuration(duration time.Duration) string {
	duration = max(time.Duration(0), duration)
	if duration < time.Minute {
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	}
	if duration < 24*time.Hour {
		hours := int(duration.Hours())
		minutes := int(duration.Minutes()) % 60
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	days := int(duration.Hours()) / 24
	hours := int(duration.Hours()) % 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}
