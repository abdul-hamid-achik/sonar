package ui

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func installTranscriptSearchPreemptionFixture(
	t *testing.T,
	m *Model,
) color.Color {
	t.Helper()
	marker := lipgloss.Color("#d75fff")
	m.viewport.StyleLineFunc = func(int) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(marker).Italic(true)
	}
	if command := m.openTranscriptSearch(); command == nil || m.transcriptSearch == nil ||
		m.overlay != OverlayTranscriptSearch {
		t.Fatalf("search did not open: cmd=%v state=%v overlay=%v",
			command != nil, m.transcriptSearch != nil, m.overlay)
	}
	return marker
}

func assertTranscriptSearchPreempted(
	t *testing.T,
	m *Model,
	marker color.Color,
) {
	t.Helper()
	if m.transcriptSearch != nil {
		t.Fatal("incoming authority retained hidden transcript search state")
	}
	if m.viewport.StyleLineFunc == nil {
		t.Fatal("incoming authority dropped the viewport line-style chain")
	}
	style := m.viewport.StyleLineFunc(0)
	assertSameColor(t, "restored transcript line style", style.GetForeground(), marker)
	if !style.GetItalic() {
		t.Fatal("restored transcript line style lost its prior attributes")
	}
}

func TestTranscriptSearchIsPreemptedByStreamingToolApproval(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	marker := installTranscriptSearchPreemptionFixture(t, m)
	responses := make(chan permission.ApprovalResponse, 1)

	updated, _ := m.Update(ToolApprovalMsg{
		ToolName: "bash",
		Args:     map[string]any{"command": "go test ./internal/ui"},
		Response: responses,
	})
	m = updated.(*Model)

	assertTranscriptSearchPreempted(t, m, marker)
	if m.pendingApproval == nil || m.approvalState == nil ||
		m.overlay != OverlayNone || m.input.Focused() {
		t.Fatalf(
			"approval did not become the sole owner: pending=%v state=%v overlay=%v focused=%v",
			m.pendingApproval != nil,
			m.approvalState != nil,
			m.overlay,
			m.input.Focused(),
		)
	}
}

func TestTranscriptSearchIsPreemptedByCortexDecision(t *testing.T) {
	m := newTestModel(t)
	marker := installTranscriptSearchPreemptionFixture(t, m)
	presentation, err := newCortexDecisionPresentation(
		"task_search_preemption",
		*cortexDecisionFixture("search-preemption"),
		m.width,
		m.height,
		m.isDark,
		m.themeID,
		true,
		m.glyphProfile,
	)
	if err != nil {
		t.Fatal(err)
	}

	m.cortexDecision = presentation
	m.activateCortexDecision()

	assertTranscriptSearchPreempted(t, m, marker)
	if m.overlay != OverlayCortexDecision || m.cortexDecision != presentation ||
		m.input.Focused() {
		t.Fatalf(
			"Cortex did not become the sole owner: overlay=%v decision=%v focused=%v",
			m.overlay,
			m.cortexDecision == presentation,
			m.input.Focused(),
		)
	}
}

func TestTranscriptSearchIsPreemptedByGoalRecovery(t *testing.T) {
	fixture := newGoalRecoveryCoordinatorFixture(t, 80, 24, 0)
	loadGoalRecoveryProjection(t, fixture, true)
	m := fixture.m
	if m.goalInspectorState == nil {
		t.Fatal("goal inspector fixture did not install its parent state")
	}
	// Simulate the inspector yielding its visible surface while retaining the
	// validated parent/projection; a delayed recovery activation must still
	// preempt a search opened in that interval.
	m.overlay = OverlayNone
	m.overlayParent = OverlayNone
	m.input.Focus()
	marker := installTranscriptSearchPreemptionFixture(t, m)

	m.openGoalRecovery()

	assertTranscriptSearchPreempted(t, m, marker)
	if m.overlay != OverlayGoalRecovery || m.goalRecoveryState == nil ||
		m.overlayParent != OverlayGoalInspector || m.input.Focused() {
		t.Fatalf(
			"Recovery did not become the sole nested owner: overlay=%v state=%v parent=%v focused=%v",
			m.overlay,
			m.goalRecoveryState != nil,
			m.overlayParent,
			m.input.Focused(),
		)
	}
}

func TestTranscriptSearchIsPreemptedByReadScopePreview(t *testing.T) {
	m := newTestModel(t)
	m.readScopeOpRunning = true
	m.readScopeOpToken = 17
	m.readScopeOpDraft = "inspect /external"
	marker := installTranscriptSearchPreemptionFixture(t, m)

	m.handleReadScopePreviewResult(ReadScopePreviewResultMsg{
		Token:     17,
		Requested: "/external",
		Canonical: "/external",
		Workspace: "/workspace",
		Draft:     "inspect /external",
		Grant:     agent.ReadGrant{},
	})

	assertTranscriptSearchPreempted(t, m, marker)
	if m.readScopePrompt == nil || m.readScopePrompt.Operation != "add-read" ||
		m.input.Focused() {
		t.Fatalf(
			"read-scope prompt did not become the sole owner: prompt=%v operation=%q focused=%v",
			m.readScopePrompt != nil,
			func() string {
				if m.readScopePrompt == nil {
					return ""
				}
				return m.readScopePrompt.Operation
			}(),
			m.input.Focused(),
		)
	}
}

func TestImageAttachmentFallbackPreemptsSearchAndSurvivesPasteReview(t *testing.T) {
	m := newTestModel(t)
	m.imageAttachToken = 23
	fallback := strings.Repeat("fallback line\n", 40)
	marker := installTranscriptSearchPreemptionFixture(t, m)

	_ = m.handleImageAttachmentResult(ImageAttachmentResultMsg{
		Token:     23,
		Preflight: true,
		Name:      "clipboard.png",
		Fallback:  fallback,
		Err:       errImageAttachmentBusy,
	})

	assertTranscriptSearchPreempted(t, m, marker)
	if m.pendingPaste == nil || m.pendingPaste.Content != fallback ||
		!m.pendingPaste.NeedsReview || m.overlay != OverlayNone {
		t.Fatalf(
			"fallback paste was not retained for review: pending=%v content=%v review=%v overlay=%v",
			m.pendingPaste != nil,
			m.pendingPaste != nil && m.pendingPaste.Content == fallback,
			m.pendingPaste != nil && m.pendingPaste.NeedsReview,
			m.overlay,
		)
	}
}
