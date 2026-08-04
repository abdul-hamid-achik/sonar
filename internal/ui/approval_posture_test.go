package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSkippedApprovalPostureIsVisibleAcrossSafetySurfaces(t *testing.T) {
	m := newTestModel(t)
	m.model = "local-model"
	m.SetApprovalPosture(ApprovalPostureSkipApprovals)

	// Roomy frames omit the mid-canvas welcome; min frames still surface posture.
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	var welcome strings.Builder
	m.renderWelcome(&welcome)
	if plain := ansi.Strip(welcome.String()); strings.Contains(plain, "YOLO") || !strings.Contains(plain, "prompts skipped") {
		t.Fatalf("minimum welcome hid skipped-approval posture:\n%s", plain)
	}

	// Footer/status is the durable roomy-frame safety surface.
	m.width, m.height = 80, 24
	m.entries = append(m.entries, ChatEntry{Kind: "user", Content: "hello"})
	if footer := ansi.Strip(m.renderStatusLine()); strings.Contains(footer, "YOLO") || !strings.Contains(footer, "prompts skipped") {
		t.Fatalf("footer hid skipped-approval posture: %q", footer)
	}

	items := m.settingsItems()
	if value := items[settingsRuntime].value; strings.Contains(value, "YOLO") || !strings.Contains(value, "no approval prompts") {
		t.Fatalf("Settings runtime value hid skipped-approval posture: %q", value)
	}
	if runtime := ansi.Strip(m.buildRuntimeStatusContent(56)); !strings.Contains(runtime, "Approval") || !strings.Contains(runtime, "prompts skipped") || !strings.Contains(runtime, "boundaries apply") {
		t.Fatalf("Runtime hid yolo posture:\n%s", runtime)
	}
}

func TestPromptedPostureDoesNotClaimEveryEffectAlwaysAsks(t *testing.T) {
	m := newTestModel(t)
	m.model = "local-model"
	// Posture copy lives on the minimum welcome; roomy frames stay quiet.
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	var welcome strings.Builder
	m.renderWelcome(&welcome)
	plain := ansi.Strip(welcome.String())
	if !strings.Contains(plain, "prompts on") && !strings.Contains(plain, "approval prompts") {
		t.Fatalf("prompted posture wording missing:\n%s", plain)
	}
	if strings.Contains(plain, "tool effects ask first") {
		t.Fatalf("prompted posture overclaimed:\n%s", plain)
	}
}

func TestSkippedApprovalSafetyTextIsResponsiveAndNoColorSafe(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	m := newTestModel(t)
	m.SetApprovalPosture(ApprovalPostureSkipApprovals)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m.entries = append(m.entries, ChatEntry{Kind: "user", Content: "hello"})

	var welcome strings.Builder
	m.renderWelcome(&welcome)
	footer := m.renderStatusLine()
	runtime := m.buildRuntimeStatusContent(pickerListWidth(m.width))
	for name, rendered := range map[string]string{"welcome": welcome.String(), "footer": footer, "runtime": runtime} {
		if hasANSIColor(rendered) {
			t.Fatalf("NO_COLOR %s emitted ANSI color: %q", name, rendered)
		}
		for _, line := range strings.Split(rendered, "\n") {
			if lipgloss.Width(line) > m.chatPaneWidth() {
				t.Fatalf("%s overflowed narrow terminal: width=%d pane=%d line=%q", name, lipgloss.Width(line), m.chatPaneWidth(), line)
			}
		}
	}
}

func TestMinimumWelcomeUsesCompleteSafetyMicrocopy(t *testing.T) {
	for _, test := range []struct {
		name    string
		posture ApprovalPosture
		want    string
	}{
		{name: "prompted", posture: ApprovalPosturePrompted, want: "Local-first · prompts on"},
		{name: "skipped", posture: ApprovalPostureSkipApprovals, want: "Local-first · prompts skipped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.width = minTerminalWidth
			m.height = minTerminalHeight
			m.SetApprovalPosture(test.posture)
			var welcome strings.Builder
			m.renderWelcome(&welcome)
			plain := ansi.Strip(welcome.String())
			if !strings.Contains(plain, test.want) || strings.Contains(plain, "…") {
				t.Fatalf("minimum welcome safety copy = %q, want complete %q", plain, test.want)
			}
		})
	}
}

func TestCompactFooterPreservesCombinedSafetyBoundaries(t *testing.T) {
	m := newTestModel(t)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m.mode = ModePlan
	m.SetApprovalPosture(ApprovalPostureSkipApprovals)
	m.failedServers = []FailedServer{{Name: "mcphub", Reason: "offline"}}
	m.model = "remote-model"
	m.ollamaModels = []OllamaModelDescriptor{{Name: m.model, Source: OllamaModelRemote}}
	m.entries = []ChatEntry{{Kind: "user", Content: "continue"}}

	footer := ansi.Strip(m.renderStatusLine())
	for _, want := range []string{"PLAN", "no prompts", "MCP unavailable", "REMOTE"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("compact footer hid %q: %q", want, footer)
		}
	}
	for _, line := range strings.Split(footer, "\n") {
		if lipgloss.Width(line) > minTerminalWidth {
			t.Fatalf("compact safety row overflowed: %q", line)
		}
	}
}
