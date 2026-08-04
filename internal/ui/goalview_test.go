package ui

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestRenderGoalStatusLineIsAdaptiveAndUseful(t *testing.T) {
	summary := GoalSummary{
		Objective:   "Polish Unicode 模型 goal session experience",
		Phase:       GoalPhaseActive,
		TurnsUsed:   3,
		TurnBudget:  12,
		TokensUsed:  8_000,
		TokenBudget: 32_000,
		Elapsed:     14 * time.Minute,
		TimeBudget:  time.Hour,
	}

	for _, width := range []int{12, 30, 48, 80} {
		t.Run(formatGoalBudget(width), func(t *testing.T) {
			line := RenderGoalStatusLine(summary, width, true, defaultThemeID)
			if strings.Contains(line, "\n") {
				t.Fatalf("status line wrapped at width %d: %q", width, line)
			}
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("status width = %d, terminal width = %d: %q", got, width, line)
			}
			if !strings.Contains(line, "active") {
				t.Fatalf("status line lost textual state at width %d: %q", width, line)
			}
		})
	}

	wide := RenderGoalStatusLine(summary, 80, true, defaultThemeID)
	for _, want := range []string{"3/12 auto turns", "8k/32k tok", "14m/1h"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide status missing %q: %q", want, wide)
		}
	}
}

func TestRenderGoalStatusLineSanitizesUserObjective(t *testing.T) {
	raw := "ship\x1b]52;c;GOAL_SECRET\x07\x1b[2J\nrelease\u202e\u2066"
	plain := ansi.Strip(RenderGoalStatusLine(GoalSummary{Objective: raw, Phase: GoalPhaseActive}, 100, true, defaultThemeID))
	if strings.Contains(plain, "GOAL_SECRET") || strings.Contains(plain, "\n") {
		t.Fatalf("goal objective escaped its fixed row: %q", plain)
	}
	for _, character := range plain {
		if unicode.IsControl(character) || isBidiControl(character) {
			t.Fatalf("goal objective retained unsafe rune %U: %q", character, plain)
		}
	}
	for _, visible := range []string{"ship", "release"} {
		if !strings.Contains(plain, visible) {
			t.Fatalf("goal objective lost visible text %q: %q", visible, plain)
		}
	}
}

func TestRenderGoalStatusLineNamesEveryPhaseWithoutColor(t *testing.T) {
	tests := []struct {
		phase GoalPhase
		glyph string
		label string
	}{
		{phase: GoalPhaseActive, glyph: "●", label: "active"},
		{phase: GoalPhasePaused, glyph: "Ⅱ", label: "paused"},
		{phase: GoalPhaseExhausted, glyph: "!", label: "exhausted"},
		{phase: GoalPhaseCompleted, glyph: "✓", label: "completed"},
		{phase: GoalPhaseDropped, glyph: "×", label: "dropped"},
		{phase: GoalPhaseBlocked, glyph: "!", label: "blocked"},
		{phase: GoalPhase("unknown"), glyph: "○", label: "goal"},
	}

	for _, test := range tests {
		t.Run(test.label, func(t *testing.T) {
			line := RenderGoalStatusLine(GoalSummary{Objective: "ship", Phase: test.phase}, 40, false, defaultThemeID)
			if !strings.Contains(line, test.glyph) || !strings.Contains(line, test.label) {
				t.Fatalf("phase %q was not conveyed with glyph and text: %q", test.phase, line)
			}
		})
	}
}

func TestRenderGoalStatusLineOmitsUnlimitedBudgets(t *testing.T) {
	line := RenderGoalStatusLine(GoalSummary{
		Objective:  "keep the compact surface clear",
		Phase:      GoalPhasePaused,
		TurnsUsed:  5,
		TokensUsed: 9000,
		Elapsed:    time.Hour,
	}, 80, true, defaultThemeID)

	if strings.Contains(line, "turn") || strings.Contains(line, "tok") || strings.Contains(line, "/") {
		t.Fatalf("unlimited budgets should not create progress fractions: %q", line)
	}
	if !strings.Contains(line, "keep the compact surface clear") {
		t.Fatalf("available space was not given to the objective: %q", line)
	}
}

func TestGoalStatusTerminalPhasesStopLiveTimeAndWarningAccrual(t *testing.T) {
	for _, phase := range []GoalPhase{GoalPhaseCompleted, GoalPhaseDropped} {
		t.Run(string(phase), func(t *testing.T) {
			summary := GoalSummary{
				Objective:   "finished goal",
				Phase:       phase,
				TurnsUsed:   8,
				TurnBudget:  8,
				TokensUsed:  1200,
				TokenBudget: 1000,
				Elapsed:     3 * time.Hour,
				TimeBudget:  time.Hour,
			}
			line := RenderGoalStatusLine(summary, 100, true, defaultThemeID)
			if strings.Contains(line, "3h/1h") {
				t.Fatalf("terminal status kept a live wall clock: %q", line)
			}
			metrics := goalStatusMetrics(summary)
			if len(metrics) != 2 {
				t.Fatalf("terminal metrics = %#v, want stable turn/token receipts", metrics)
			}
			for _, metric := range metrics {
				if metric.alert {
					t.Fatalf("terminal success/drop retained active budget warning: %#v", metrics)
				}
			}
		})
	}
}

func TestGoalStatusFormatting(t *testing.T) {
	tokenTests := map[int64]string{
		0:         "0",
		999:       "999",
		1_000:     "1k",
		12_500:    "12.5k",
		1_000_000: "1m",
	}
	for input, want := range tokenTests {
		if got := formatGoalTokens(input); got != want {
			t.Errorf("formatGoalTokens(%d) = %q, want %q", input, got, want)
		}
	}

	durationTests := map[time.Duration]string{
		45 * time.Second:              "45s",
		14 * time.Minute:              "14m",
		time.Hour:                     "1h",
		time.Hour + 30*time.Minute:    "1h30m",
		25*time.Hour + 30*time.Minute: "1d1h",
	}
	for input, want := range durationTests {
		if got := formatGoalDuration(input); got != want {
			t.Errorf("formatGoalDuration(%s) = %q, want %q", input, got, want)
		}
	}
}
