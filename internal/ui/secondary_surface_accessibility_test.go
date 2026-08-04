package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
	"github.com/charmbracelet/x/ansi"
)

func TestASCIIProfilePropagatesAcrossSecondaryControls(t *testing.T) {
	m := newTestModel(t)
	m.glyphProfile = GlyphASCII
	m.agentList = []string{"reviewer"}

	m.openAgentPicker()
	agentPicker := m.renderAgentPicker()
	m.closeAgentPicker()

	m.openModePicker()
	modePicker := m.renderModePicker()
	m.closeModePicker()

	m.openSettingsPicker()
	settingsPicker := m.renderSettingsPicker()
	m.closeSettingsPicker()

	for name, rendered := range map[string]string{
		"agent picker":    agentPicker,
		"mode picker":     modePicker,
		"settings picker": settingsPicker,
	} {
		plain := ansi.Strip(rendered)
		assertNoUnicodeSemanticGlyphs(t, plain)
		if strings.ContainsAny(plain, "╭╮╰╯") || !strings.Contains(plain, "+") {
			t.Fatalf("%s did not use ASCII frame/current markers:\n%s", name, plain)
		}
	}
	if !strings.Contains(ansi.Strip(agentPicker), "+") || !strings.Contains(ansi.Strip(modePicker), "+") {
		t.Fatalf("current picker choices omitted ASCII success marker:\n%s\n%s", agentPicker, modePicker)
	}
}

func TestASCIIProfileCoversCortexAndGoalSurfaces(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cortex, err := newCortexDecisionPresentation(
		"task_ascii",
		*cortexDecisionFixture("ascii"),
		80,
		24,
		true,
		defaultThemeID,
		true,
		GlyphASCII,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !cortex.move(1) {
		t.Fatal("Cortex fixture could not select an option")
	}

	snapshot := goalInspectorFixture(now)
	inspector := NewGoalInspector(snapshot, nil, GoalInspectorOptions{
		Width: 80, Height: 24, IsDark: true, ReducedMotion: true,
		GlyphProfile: GlyphASCII, Now: now,
	})

	plan, ok := newGoalPlanCard(goalPlanFixture(now), true, defaultThemeID, GlyphASCII)
	if !ok {
		t.Fatal("ASCII goal plan fixture rejected")
	}
	plan.SetSize(80, 24)

	recovery := NewGoalRecovery(goalRecoveryFixtureItems()[:1], GoalRecoveryOptions{
		Width: 80, Height: 24, IsDark: true, ReducedMotion: true, GlyphProfile: GlyphASCII,
	})
	_, _ = recovery.Update(enterKey())

	surfaces := map[string]string{
		"Cortex decision": cortex.View(""),
		"Goal Inspector":  inspector.View(),
		"Goal Plan":       plan.View(),
		"Goal Recovery":   recovery.View(),
	}
	for name, rendered := range surfaces {
		plain := ansi.Strip(rendered)
		assertNoUnicodeSemanticGlyphs(t, plain)
		if strings.ContainsAny(plain, "╭╮╰╯") {
			t.Fatalf("%s retained a rounded Unicode frame:\n%s", name, plain)
		}
	}
	if plain := ansi.Strip(cortex.View("")); !strings.Contains(plain, "* two_step -") {
		t.Fatalf("Cortex did not use ASCII selection/separator chrome:\n%s", plain)
	}
	if plain := ansi.Strip(inspector.buildDocument()); !strings.Contains(plain, "o pending") {
		t.Fatalf("Goal Inspector did not use ASCII acceptance markers:\n%s", plain)
	}
	if plain := ansi.Strip(plan.View()); !strings.Contains(plain, "o pending") {
		t.Fatalf("Goal Plan did not use ASCII acceptance markers:\n%s", plain)
	}
}

func TestASCIIProfileCoversInlineFormControls(t *testing.T) {
	m := newTestModel(t)
	m.glyphProfile = GlyphASCII

	m.planFormState = NewPlanFormState("inspect UI", m.themeID, m.isDark, true)
	m.planFormState.ActiveField = 1
	plan := m.renderPlanForm()

	goalForm := NewGoalForm(defaultGoalFormValues("ship UI"), GoalFormOptions{
		Width: 80, Height: 24, IsDark: m.isDark, ReducedMotion: true, GlyphProfile: GlyphASCII,
	})
	goalForm.focusField(GoalFieldActions)
	goal := goalForm.View()

	for name, rendered := range map[string]string{"Plan form": plan, "Goal form": goal} {
		plain := ansi.Strip(rendered)
		assertNoUnicodeSemanticGlyphs(t, plain)
		if strings.ContainsAny(plain, "╭╮╰╯") {
			t.Fatalf("%s retained a rounded Unicode frame:\n%s", name, plain)
		}
	}
	if plain := ansi.Strip(plan); !strings.Contains(plain, "> single file") ||
		!strings.Contains(plain, "left/right choose") {
		t.Fatalf("Plan form did not use ASCII navigation markers:\n%s", plain)
	}
	if plain := ansi.Strip(goal); !strings.Contains(plain, "> Save goal") {
		t.Fatalf("Goal form did not use ASCII action marker:\n%s", plain)
	}
}

func TestASCIIListFilterChromeIsStaticAndReducedMotionSafe(t *testing.T) {
	state := newSessionsPickerState([]SessionListItem{{
		ID: 1, Title: "ASCII session", CreatedAt: "now",
	}}, 80, 24, true, defaultThemeID, true)
	configurePickerListGlyphProfile(&state.List, GlyphASCII)

	if state.List.FilterInput.Prompt != "Filter > " {
		t.Fatalf("ASCII filter prompt = %q", state.List.FilterInput.Prompt)
	}
	if state.List.FilterInput.Styles().Cursor.Blink {
		t.Fatal("reduced motion left the ASCII filter cursor blinking")
	}
	if prompt := completionFilterPromptForGlyphProfile(GlyphASCII); prompt != "Filter > " {
		t.Fatalf("ASCII completion filter prompt = %q", prompt)
	}
}

func TestASCIIProfileCoversApprovalActivityAndCompletionChrome(t *testing.T) {
	approvalModel := newTestModel(t)
	approvalModel.glyphProfile = GlyphASCII
	approvalModel = openApprovalForTest(t, approvalModel, ToolApprovalMsg{
		ToolName: "bash",
		Preview: permission.ApprovalPreview{
			Kind:        permission.PreviewCommand,
			ActionLabel: "Run tests",
			Consequence: "Executes the proposed command.",
		},
		Response: make(chan permission.ApprovalResponse, 1),
	})

	activityModel := newTestModel(t)
	activityModel.glyphProfile = GlyphASCII
	activityModel.reducedMotion = true
	activityModel.state = StateStreaming
	activityModel.turnStartedAt = activityModel.nowTime().Add(-1500 * time.Millisecond)
	activityModel.promptTokens = 50
	activityModel.numCtx = 100

	statusModel := newTestModel(t)
	statusModel.glyphProfile = GlyphASCII
	statusModel.entries = []ChatEntry{{Kind: "user", Content: "started"}}
	statusModel.promptTokens = 50
	statusModel.numCtx = 100
	statusModel.sessionID = 7
	statusModel.sessionPublicID = "aaaaaa7"
	completionModel := newTestModel(t)
	completionModel.glyphProfile = GlyphASCII
	completionModel.completionState = newCompletionState(
		"attachments",
		[]Completion{{Label: "notes.txt", Insert: "notes.txt"}},
		true,
		completionModel.themeID,
		completionModel.isDark,
	)
	completionModel.completionState.Preview = completionPreview{
		State:   completionPreviewReady,
		Path:    "notes.txt",
		Size:    256,
		Content: strings.Repeat("long preview content ", 8),
	}

	surfaces := map[string]string{
		"approval":       approvalModel.renderApproval(),
		"activity":       activityModel.renderWorkingLine(),
		"context status": activityModel.renderContextStatus(),
		"idle status":    statusModel.renderStatusLine(),
		"completion":     completionModel.renderCompletionPreview(48, 3),
	}
	for name, rendered := range surfaces {
		plain := ansi.Strip(rendered)
		assertNoUnicodeSemanticGlyphs(t, plain)
		for _, forbidden := range []string{"…", "◇", "›", "↑", "↓", "▮", "▯"} {
			// Context status uses the profile separator (ASCII "|") between
			// used/limit and percent; middle-dot is Unicode-only chrome.
			if name == "context status" && forbidden == "·" {
				continue
			}
			if strings.Contains(plain, forbidden) {
				t.Fatalf("%s retained Unicode chrome %q:\n%s", name, forbidden, plain)
			}
		}
		if name != "context status" && !strings.Contains(plain, "|") {
			t.Fatalf("%s omitted ASCII separator chrome:\n%s", name, plain)
		}
	}
	// Absolute used/limit · percent (ASCII separator is " | " via glyphSeparator).
	if context := ansi.Strip(activityModel.renderContextStatus()); !strings.Contains(context, "50%") ||
		!strings.Contains(context, "50/100") {
		t.Fatalf("context status omitted absolute occupancy:\n%s", context)
	}
}
