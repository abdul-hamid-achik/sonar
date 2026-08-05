package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func TestCloudModelBoundaryIsVisibleAcrossCoreSurfaces(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen-cloud:latest"
	m.modelPinned = true
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "qwen-cloud:latest", DisplayName: "Qwen Cloud", Source: OllamaModelCloud,
		Current: true, Selectable: true, Fit: true,
	}}

	settings := m.settingsItems()[int(settingsModel)].Title()
	for _, want := range []string{"CLOUD", "remote prompts", "Pinned"} {
		if !strings.Contains(settings, want) {
			t.Fatalf("Settings hid %q in %q", want, settings)
		}
	}

	// The safety contract is that a reader sees the boundary, not that any
	// particular renderer prints it. planStatus decides which surface owns it
	// per frame, so this asserts against the composed view: it stays true if
	// ownership moves again, and it still fails if the boundary is dropped.
	for _, size := range []struct {
		width, height int
	}{{30, 12}, {80, 24}} {
		for _, started := range []bool{false, true} {
			m.width, m.height = size.width, size.height
			if started {
				m.entries = []ChatEntry{{Kind: "user", Content: "hello"}}
			} else {
				m.entries = nil
			}
			m.recalcViewportHeight()
			m.refreshTranscript()

			view := ansi.Strip(m.View().Content)
			if !strings.Contains(view, "CLOUD") {
				t.Fatalf("view at %dx%d (started=%v) hid the Cloud boundary:\n%s",
					size.width, size.height, started, view)
			}
		}
	}
	m.entries = nil
}

// The Cloud boundary must appear exactly once. Printing it on two surfaces was
// the old way of guaranteeing it appeared at all.
func TestCloudModelBoundaryIsNotRepeated(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen-cloud:latest"
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "qwen-cloud:latest", Source: OllamaModelCloud,
		Current: true, Selectable: true, Fit: true,
	}}
	m.entries = []ChatEntry{{Kind: "user", Content: "hello"}}
	m.recalcViewportHeight()
	m.refreshTranscript()

	view := ansi.Strip(m.View().Content)
	if got := strings.Count(view, "remote prompts"); got != 1 {
		t.Fatalf("Cloud boundary appeared %d times, want exactly 1:\n%s", got, view)
	}
}

func TestGoalFooterPreservesCloudAndSkippedApprovalBoundaries(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen-cloud:latest"
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: m.model, Source: OllamaModelCloud, Current: true,
	}}
	m.SetApprovalPosture(ApprovalPostureSkipApprovals)
	summary := GoalSummary{Objective: "ship safely", Phase: GoalPhaseActive}

	// Which surface carries the Cloud boundary is planStatus's decision, so the
	// contract is checked on the composed frame: the reader sees it exactly
	// once. The goal row is still checked for the facts it does own.
	m.goalRuntime = newUIGoalRuntime(t, 77, goal.BudgetLimits{MaxContinuationTurns: 4})
	m.entries = []ChatEntry{{Kind: "user", Content: "working"}}
	for _, width := range []int{30, 80} {
		m.width, m.height = width, 24
		m.recalcViewportHeight()
		m.refreshTranscript()
		frame := ansi.Strip(m.View().Content)
		if got := strings.Count(frame, "CLOUD"); got != 1 {
			t.Fatalf("frame at width %d printed the Cloud boundary %d times:\n%s", width, got, frame)
		}
		plain := ansi.Strip(m.renderGoalFooterStatus(summary, width))
		for _, want := range []string{"AUTO", "active"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("goal row at width %d hid %q: %q", width, want, plain)
			}
		}
		if !strings.Contains(plain, "no prompts") && !strings.Contains(plain, "approval prompts skipped") {
			t.Fatalf("goal footer at width %d hid skipped approvals: %q", width, plain)
		}
		for _, line := range strings.Split(plain, "\n") {
			if len(line) > 0 && ansi.StringWidth(line) > width {
				t.Fatalf("goal footer at width %d overflowed: %q", width, line)
			}
		}
	}
}

func TestGoalFooterKeepsSessionHandleBesideGoalProgress(t *testing.T) {
	m := newTestModel(t)
	m.sessionID = 7
	m.sessionPublicID = "aaaaaa7"
	m.activeSessionTitle = "Ship a polished goal UI"
	summary := GoalSummary{Objective: "Ship a polished goal UI", Phase: GoalPhaseActive}

	ordinary := ansi.Strip(m.renderGoalFooterStatus(summary, 80))
	if !strings.Contains(ordinary, "aaaaaa7") || !strings.Contains(ordinary, "Ship a polished") {
		t.Fatalf("ordinary goal footer lost progress or session identity: %q", ordinary)
	}
	roomy := ansi.Strip(m.renderGoalFooterStatus(summary, 120))
	if !strings.Contains(roomy, "aaaaaa7") {
		t.Fatalf("roomy goal footer omitted compact session handle: %q", roomy)
	}
}

func TestCurrentModelSurfaceLabelKeepsLegacyAndRemoteModelsTruthful(t *testing.T) {
	m := newTestModel(t)
	m.model = "legacy:latest"
	if got := m.currentModelSurfaceLabel(false); got != "legacy:latest" {
		t.Fatalf("legacy model label = %q", got)
	}
	m.ollamaModels = []OllamaModelDescriptor{{Name: m.model, Source: OllamaModelRemote}}
	if got := m.currentModelSurfaceLabel(true); got != "REMOTE · remote prompts" {
		t.Fatalf("remote compact label = %q", got)
	}
}
