package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func goalRecoveryFixtureItems() []GoalRecoveryItem {
	return []GoalRecoveryItem{
		{
			ItemID:      "ctrl_execution_123",
			Kind:        GoalRecoveryExecutionEffect,
			Summary:     "Reconcile the outcome of write_file",
			Tool:        "write_file",
			ExecutionID: "exec_123",
			TurnID:      "turn_7",
			EventType:   "outcome_unknown",
			EffectClass: "effectful",
			Age:         "3m ago",
			Actionable:  true,
		},
		{
			ItemID:      "ctrl_execution_456",
			Kind:        GoalRecoveryExecutionEffect,
			Summary:     "Reconcile the outcome of deploy_release",
			Tool:        "deploy_release",
			ExecutionID: "exec_456",
			TurnID:      "turn_8",
			EventType:   "started",
			EffectClass: "unknown",
			Age:         "1m ago",
			Actionable:  true,
		},
	}
}

func newGoalRecoveryFixture(width, height int) *GoalRecovery {
	return NewGoalRecovery(goalRecoveryFixtureItems()[:1], GoalRecoveryOptions{
		Width: width, Height: height, IsDark: true, ReducedMotion: true,
	})
}

func prepareGoalRecoveryConfirmation(t *testing.T, recovery *GoalRecovery) {
	t.Helper()
	item, ok := recovery.SelectedItem()
	if !ok {
		t.Fatal("recovery fixture has no selected item")
	}
	recovery.draftItemID = item.ItemID
	recovery.observationIndex = 0
	recovery.sourceIndex = 0
	recovery.summary.SetValue("Backend job exists and reports succeeded")
	recovery.reference.SetValue("job_123")
	recovery.focusStage(GoalRecoveryStageConfirmation)
}

func TestGoalRecoveryCopiesItemsAndKeepsListPresentationOnly(t *testing.T) {
	items := goalRecoveryFixtureItems()
	recovery := NewGoalRecovery(items, GoalRecoveryOptions{Width: 80, Height: 24, IsDark: true})
	items[0].Tool = "mutated outside child"
	items[0].ItemID = "different"

	selected, ok := recovery.SelectedItem()
	if !ok || selected.Tool != "write_file" || selected.ItemID != "ctrl_execution_123" {
		t.Fatalf("selected immutable copy = %#v, ok=%v", selected, ok)
	}
	view := ansi.Strip(recovery.View())
	for _, want := range []string{"Recovery · 2 checks", "not proof of backend or goal completion", "write_file", "exec_123"} {
		if !strings.Contains(view, want) {
			t.Fatalf("recovery list missing %q:\n%s", want, view)
		}
	}

	_, _ = recovery.Update(downKey())
	selected, ok = recovery.SelectedItem()
	if !ok || selected.ItemID != "ctrl_execution_456" {
		t.Fatalf("list navigation selected %#v, ok=%v", selected, ok)
	}
	if detail := ansi.Strip(recovery.listDetail()); !strings.Contains(detail, "deploy_release") || !strings.Contains(detail, "exec_456") {
		t.Fatalf("selected detail did not follow Bubbles list:\n%s", detail)
	}

	event, _ := recovery.Update(escKey())
	if event.Action != GoalRecoveryActionClose || event.ItemID != "" || event.Draft != (GoalRecoveryDraft{}) {
		t.Fatalf("list Escape event = %#v", event)
	}
}

func TestGoalRecoveryStillUnknownAndBackNavigationNeverApply(t *testing.T) {
	recovery := newGoalRecoveryFixture(80, 24)

	event, _ := recovery.Update(enterKey())
	if event.Action != GoalRecoveryActionNone || recovery.Stage() != GoalRecoveryStageObservation {
		t.Fatalf("open observation event=%#v stage=%d", event, recovery.Stage())
	}
	if recovery.selectedObservation() != GoalRecoveryStillUnknown {
		t.Fatalf("safe initial observation = %q", recovery.selectedObservation())
	}

	// Still unknown is a local safe exit: it emits no event that a parent could
	// accidentally interpret as evidence.
	event, _ = recovery.Update(enterKey())
	if event.Action == GoalRecoveryActionApply || event != (GoalRecoveryEvent{}) {
		t.Fatalf("Still unknown emitted event %#v", event)
	}
	if recovery.Stage() != GoalRecoveryStageList || !strings.Contains(ansi.Strip(recovery.View()), "No evidence recorded · goal remains blocked") {
		t.Fatalf("Still unknown did not return to a truthful list:\n%s", recovery.View())
	}

	_, _ = recovery.Update(enterKey())
	event, _ = recovery.Update(escKey())
	if event.Action == GoalRecoveryActionApply || recovery.Stage() != GoalRecoveryStageList {
		t.Fatalf("observation Escape event=%#v stage=%d", event, recovery.Stage())
	}

	prepareGoalRecoveryConfirmation(t, recovery)
	event, _ = recovery.Update(escKey())
	if event.Action == GoalRecoveryActionApply || recovery.Stage() != GoalRecoveryStageReference {
		t.Fatalf("confirmation Escape event=%#v stage=%d", event, recovery.Stage())
	}

	prepareGoalRecoveryConfirmation(t, recovery)
	if recovery.confirmationIndex != 0 {
		t.Fatalf("confirmation default = %d, want Back", recovery.confirmationIndex)
	}
	event, _ = recovery.Update(enterKey())
	if event.Action == GoalRecoveryActionApply || recovery.Stage() != GoalRecoveryStageReference {
		t.Fatalf("default Back event=%#v stage=%d", event, recovery.Stage())
	}
}

func TestGoalRecoveryKeyboardFlowEmitsBoundedDraftOnlyAfterRecord(t *testing.T) {
	recovery := newGoalRecoveryFixture(80, 24)

	_, _ = recovery.Update(enterKey())
	for range 3 {
		_, _ = recovery.Update(leftKey())
	}
	if recovery.selectedObservation() != GoalRecoveryEffectApplied {
		t.Fatalf("selected observation = %q", recovery.selectedObservation())
	}
	_, _ = recovery.Update(enterKey())
	if recovery.Stage() != GoalRecoveryStageSource || recovery.selectedSource() != GoalRecoveryExternalReceipt {
		t.Fatalf("source stage=%d source=%q", recovery.Stage(), recovery.selectedSource())
	}
	_, _ = recovery.Update(enterKey())
	if recovery.Stage() != GoalRecoveryStageSummary {
		t.Fatalf("source Enter stage = %d", recovery.Stage())
	}
	recovery.summary.SetValue("  Backend job exists and reports succeeded  ")
	_, _ = recovery.Update(tabKey())
	if recovery.Stage() != GoalRecoveryStageReference {
		t.Fatalf("summary Tab stage = %d, error=%q", recovery.Stage(), recovery.errorText)
	}
	recovery.reference.SetValue("  job_123  ")
	_, _ = recovery.Update(enterKey())
	if recovery.Stage() != GoalRecoveryStageConfirmation || recovery.confirmationIndex != 0 {
		t.Fatalf("confirmation stage=%d default=%d", recovery.Stage(), recovery.confirmationIndex)
	}

	confirmation := ansi.Strip(recovery.View())
	for _, want := range []string{"not proof of backend or", "Ledger stays outcome_unknown", "AUTO will not resume", "Back", "Record evidence"} {
		if !strings.Contains(confirmation, want) {
			t.Fatalf("confirmation missing %q:\n%s", want, confirmation)
		}
	}
	if !strings.Contains(confirmation, "goal remains") || !strings.Contains(confirmation, "blocked until") {
		t.Fatalf("confirmation omitted blocked-goal invariant:\n%s", confirmation)
	}
	if strings.Contains(confirmation, "Clears the retry block") {
		t.Fatalf("member confirmation claimed final-parent authority:\n%s", confirmation)
	}

	_, _ = recovery.Update(rightKey())
	event, _ := recovery.Update(enterKey())
	if event.Action != GoalRecoveryActionApply || event.ItemID != "ctrl_execution_123" {
		t.Fatalf("record event = %#v", event)
	}
	want := GoalRecoveryDraft{
		Observation: GoalRecoveryEffectApplied,
		Source:      GoalRecoveryExternalReceipt,
		Summary:     "Backend job exists and reports succeeded",
		Reference:   "job_123",
	}
	if event.Draft != want {
		t.Fatalf("record draft = %#v, want %#v", event.Draft, want)
	}
}

func TestGoalRecoveryEmitsExactStableDispositionAndSourceValues(t *testing.T) {
	observations := []struct {
		index int
		want  string
	}{
		{index: 0, want: "effect_applied"},
		{index: 1, want: "effect_not_applied"},
		{index: 2, want: "effect_compensated"},
	}
	sources := []struct {
		index int
		want  string
	}{
		{index: 0, want: "external_receipt"},
		{index: 1, want: "workspace_artifact"},
		{index: 2, want: "verification_check"},
		{index: 3, want: "operator_observation"},
	}

	for _, observation := range observations {
		for _, source := range sources {
			recovery := newGoalRecoveryFixture(80, 24)
			item, _ := recovery.SelectedItem()
			recovery.draftItemID = item.ItemID
			recovery.observationIndex = observation.index
			recovery.sourceIndex = source.index
			recovery.summary.SetValue("verified redacted evidence")
			recovery.reference.SetValue("receipt_123")
			recovery.focusStage(GoalRecoveryStageConfirmation)
			recovery.confirmationIndex = 1
			event, _ := recovery.Update(enterKey())
			if event.Action != GoalRecoveryActionApply {
				t.Fatalf("%s/%s action = %q, error=%q", observation.want, source.want, event.Action, recovery.errorText)
			}
			if string(event.Draft.Observation) != observation.want || string(event.Draft.Source) != source.want {
				t.Fatalf("stable values = %q/%q, want %q/%q", event.Draft.Observation, event.Draft.Source, observation.want, source.want)
			}
		}
	}
}

func TestGoalRecoveryValidationKeepsInvalidDraftInsideChild(t *testing.T) {
	recovery := newGoalRecoveryFixture(80, 24)
	item, _ := recovery.SelectedItem()
	recovery.draftItemID = item.ItemID
	recovery.observationIndex = 0
	recovery.sourceIndex = 0
	recovery.focusStage(GoalRecoveryStageSummary)

	event, _ := recovery.Update(tabKey())
	if event.Action == GoalRecoveryActionApply || recovery.Stage() != GoalRecoveryStageSummary || recovery.errorText != "evidence summary is required" {
		t.Fatalf("empty summary event=%#v stage=%d error=%q", event, recovery.Stage(), recovery.errorText)
	}

	recovery.summary.SetValue("checked the external system")
	_, _ = recovery.Update(tabKey())
	event, _ = recovery.Update(enterKey())
	if event.Action == GoalRecoveryActionApply || recovery.Stage() != GoalRecoveryStageReference || recovery.errorText != "evidence reference is required" {
		t.Fatalf("empty reference event=%#v stage=%d error=%q", event, recovery.Stage(), recovery.errorText)
	}

	recovery.reference.SetValue(strings.Repeat("界", goalRecoveryMaximumReferenceBytes/3+1))
	event, _ = recovery.Update(enterKey())
	if event.Action == GoalRecoveryActionApply || !strings.Contains(recovery.errorText, "exceeds 1024 bytes") {
		t.Fatalf("oversized reference event=%#v error=%q", event, recovery.errorText)
	}

	if err := validateGoalRecoveryText("evidence summary", strings.Repeat("界", goalRecoveryMaximumSummaryBytes/3+1), goalRecoveryMaximumSummaryBytes); err == nil || !strings.Contains(err.Error(), "exceeds 4096 bytes") {
		t.Fatalf("oversized summary error = %v", err)
	}
}

func TestGoalRecoveryResponsiveViewsFitEveryStage(t *testing.T) {
	stages := []struct {
		stage GoalRecoveryStage
		want  string
	}{
		{GoalRecoveryStageList, "Recovery"},
		{GoalRecoveryStageObservation, "Observation"},
		{GoalRecoveryStageSource, "Evidence source"},
		{GoalRecoveryStageSummary, "Evidence summary"},
		{GoalRecoveryStageReference, "Evidence reference"},
		{GoalRecoveryStageConfirmation, "Confirm · 5/5"},
	}
	sizes := []struct {
		name          string
		width, height int
	}{
		{name: "minimum", width: 30, height: 12},
		{name: "normal", width: 80, height: 24},
	}

	for _, size := range sizes {
		for _, stage := range stages {
			t.Run(size.name+"_"+stage.want, func(t *testing.T) {
				recovery := newGoalRecoveryFixture(size.width, size.height)
				item, _ := recovery.SelectedItem()
				recovery.draftItemID = item.ItemID
				recovery.observationIndex = 0
				recovery.sourceIndex = 0
				recovery.summary.SetValue("Backend job exists and reports succeeded")
				recovery.reference.SetValue("job_123")
				recovery.focusStage(stage.stage)

				rendered := recovery.View()
				plain := ansi.Strip(rendered)
				if !strings.Contains(plain, stage.want) || !strings.Contains(plain, "esc") {
					t.Fatalf("stage %d missing identity or dismissal:\n%s", stage.stage, plain)
				}
				if !strings.Contains(rendered, "╰") {
					t.Fatalf("stage %d lost closing frame:\n%s", stage.stage, rendered)
				}
				assertRenderedLinesFit(t, rendered, size.width)
				assertRenderedHeightFits(t, rendered, size.height)
			})
		}
	}
}

func TestGoalRecoveryConfirmationCopyRemainsExplicitWhenScrolled(t *testing.T) {
	recovery := newGoalRecoveryFixture(30, 12)
	prepareGoalRecoveryConfirmation(t, recovery)
	if got := recovery.detail.Height(); got != 4 {
		t.Fatalf("compact confirmation viewport height = %d, want 4", got)
	}
	document := ansi.Strip(recovery.confirmationDetail())
	for _, want := range []string{
		"Not proof.",
		"outcome_unknown",
		"Goal stays blocked.",
		"AUTO will not resume",
		"Effect applied",
		"External receipt",
	} {
		if !strings.Contains(document, want) {
			t.Fatalf("confirmation document missing %q:\n%s", want, document)
		}
	}
	if recovery.detail.YOffset() != 0 {
		t.Fatalf("confirmation opened below safety copy at offset %d", recovery.detail.YOffset())
	}
	_, _ = recovery.Update(charKey('j'))
	if recovery.confirmationIndex != 0 {
		t.Fatalf("j changed safe confirmation default to %d", recovery.confirmationIndex)
	}
}

func TestGoalRecoveryCacheReducedMotionAndCursorContainment(t *testing.T) {
	recovery := newGoalRecoveryFixture(30, 12)
	first := recovery.View()
	second := recovery.View()
	if first != second || recovery.cache.renders != 1 {
		t.Fatalf("unchanged view was not cached: renders=%d", recovery.cache.renders)
	}

	_, _ = recovery.Update(enterKey())
	_ = recovery.View()
	if recovery.cache.renders != 2 {
		t.Fatalf("stage navigation did not invalidate cache: renders=%d", recovery.cache.renders)
	}
	if recovery.reference.Styles().Cursor.Blink || recovery.summary.Styles().Cursor.Blink {
		t.Fatal("reduced motion left a Bubbles cursor blinking")
	}
	recovery.SetReducedMotion(false)
	if !recovery.reference.Styles().Cursor.Blink || !recovery.summary.Styles().Cursor.Blink {
		t.Fatal("normal motion did not restore Bubbles cursor blinking")
	}

	recovery.focusStage(GoalRecoveryStageSummary)
	view, cursor := recovery.ViewWithCursor()
	if cursor == nil {
		t.Fatal("focused recovery textarea returned no cursor")
	}
	lines := strings.Split(view, "\n")
	if cursor.Y < 0 || cursor.Y >= len(lines) {
		t.Fatalf("cursor Y=%d outside %d rows", cursor.Y, len(lines))
	}
	if cursor.X < 0 || cursor.X > lipgloss.Width(lines[cursor.Y]) {
		t.Fatalf("cursor X=%d outside row width %d", cursor.X, lipgloss.Width(lines[cursor.Y]))
	}
	cursor.X = 999
	_, cachedCursor := recovery.ViewWithCursor()
	if cachedCursor == nil || cachedCursor.X == 999 {
		t.Fatal("caller mutated cached recovery cursor")
	}
}

func TestGoalRecoverySetItemsCannotRetargetAnOpenDraft(t *testing.T) {
	recovery := NewGoalRecovery(goalRecoveryFixtureItems(), GoalRecoveryOptions{Width: 80, Height: 24})
	_, _ = recovery.Update(enterKey())
	recovery.summary.SetValue("must remain bound to the first item")

	_ = recovery.SetItems(goalRecoveryFixtureItems()[1:])
	if recovery.Stage() != GoalRecoveryStageList || recovery.draftItemID != "" || recovery.summary.Value() != "" {
		t.Fatalf("removed item retained/retargeted draft: stage=%d item=%q summary=%q", recovery.Stage(), recovery.draftItemID, recovery.summary.Value())
	}
	selected, ok := recovery.SelectedItem()
	if !ok || selected.ItemID != "ctrl_execution_456" {
		t.Fatalf("replacement selection = %#v, ok=%v", selected, ok)
	}
}

func TestGoalRecoveryShowRecordedReceiptOwnsDraftResetAndFocus(t *testing.T) {
	recovery := newGoalRecoveryFixture(80, 24)
	prepareGoalRecoveryConfirmation(t, recovery)

	_ = recovery.ShowRecordedReceipt(goalRecoveryFixtureItems(), "Evidence recorded; goal remains blocked.")
	if recovery.Stage() != GoalRecoveryStageList || recovery.draftItemID != "" ||
		recovery.summary.Value() != "" || recovery.reference.Value() != "" {
		t.Fatalf("recorded receipt retained child draft: stage=%d item=%q summary=%q reference=%q",
			recovery.Stage(), recovery.draftItemID, recovery.summary.Value(), recovery.reference.Value())
	}
	if plain := ansi.Strip(recovery.View()); !strings.Contains(plain, "Evidence recorded") || !strings.Contains(plain, "goal remains blocked") {
		t.Fatalf("recorded receipt notice missing:\n%s", plain)
	}
}

func TestGoalRecoveryTurnBoundaryUsesDistinctConclusion(t *testing.T) {
	item := GoalRecoveryItem{
		ItemID: "recongrp_turn_9", Kind: GoalRecoveryTurnBoundary,
		Subject: "Provider turn receipt lost", Summary: "Inspect the lost provider turn before abandoning it",
		TurnID: "turn_9", EventType: "provider_receipt_lost", Actionable: true,
	}
	recovery := NewGoalRecovery([]GoalRecoveryItem{item}, GoalRecoveryOptions{Width: 80, Height: 24, ReducedMotion: true})

	_, _ = recovery.Update(enterKey())
	if recovery.Stage() != GoalRecoveryStageObservation || recovery.selectedObservation() != GoalRecoveryStillUnknown {
		t.Fatalf("turn parent initial stage=%d observation=%q", recovery.Stage(), recovery.selectedObservation())
	}
	view := ansi.Strip(recovery.View())
	for _, want := range []string{"Abandon inspected turn", "Still unknown"} {
		if !strings.Contains(view, want) {
			t.Fatalf("turn observation missing %q:\n%s", want, view)
		}
	}
	for _, forbidden := range []string{"Effect applied", "Effect not applied", "Effect compensated"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("turn parent reused execution choice %q:\n%s", forbidden, view)
		}
	}

	_, _ = recovery.Update(leftKey())
	_, _ = recovery.Update(enterKey())
	_, _ = recovery.Update(enterKey())
	recovery.summary.SetValue("Inspected the turn and every projected member")
	_, _ = recovery.Update(tabKey())
	recovery.reference.SetValue("turn-audit-9")
	_, _ = recovery.Update(enterKey())
	recovery.confirmationIndex = 1
	event, _ := recovery.Update(enterKey())
	if event.Action != GoalRecoveryActionApply || string(event.Draft.Observation) != "turn_abandoned_after_inspection" {
		t.Fatalf("turn parent event = %#v", event)
	}
	confirmation := ansi.Strip(recovery.confirmationDetail())
	if !strings.Contains(confirmation, "provider turn only") || !strings.Contains(confirmation, "No execution outcome is invented") {
		t.Fatalf("turn confirmation blurred authority boundary:\n%s", confirmation)
	}
}

func TestGoalRecoveryReviewOnlyTurnParentCannotApply(t *testing.T) {
	item := GoalRecoveryItem{
		ItemID: "recongrp_turn_10", Kind: GoalRecoveryTurnBoundary,
		Subject: "Provider turn receipt lost", Summary: "Two execution members still need evidence",
		TurnID: "turn_10", EventType: "provider_receipt_lost",
		DisabledReason: "Resolve 2 execution members before abandoning this turn.",
	}
	recovery := NewGoalRecovery([]GoalRecoveryItem{item}, GoalRecoveryOptions{Width: 30, Height: 12, ReducedMotion: true})
	if recovery.ActionableCount() != 0 {
		t.Fatalf("review-only parent actionable count = %d", recovery.ActionableCount())
	}
	plain := ansi.Strip(recovery.View())
	for _, want := range []string{"Provider turn", "receipt lost", "unavailable"} {
		if !strings.Contains(strings.ToLower(plain), strings.ToLower(want)) {
			t.Fatalf("review-only parent missing %q:\n%s", want, plain)
		}
	}
	event, _ := recovery.Update(enterKey())
	if event.Action == GoalRecoveryActionApply || recovery.Stage() != GoalRecoveryStageList {
		t.Fatalf("review-only parent event=%#v stage=%d", event, recovery.Stage())
	}
	if !strings.Contains(ansi.Strip(recovery.View()), "Resolve") {
		t.Fatalf("review-only reason is not visible:\n%s", recovery.View())
	}
	assertRenderedLinesFit(t, recovery.View(), 30)
	assertRenderedHeightFits(t, recovery.View(), 12)
}

func TestGoalRecoveryListShowsCoordinatorErrorAtMinimumSize(t *testing.T) {
	recovery := newGoalRecoveryFixture(30, 12)
	recovery.SetError("session revision changed; reload before recording evidence")

	rendered := recovery.View()
	plain := ansi.Strip(rendered)
	if !strings.Contains(plain, "session revision") {
		t.Fatalf("list hid coordinator error:\n%s", plain)
	}
	if !strings.Contains(plain, "esc") {
		t.Fatalf("list error displaced Escape help:\n%s", plain)
	}
	assertRenderedLinesFit(t, rendered, 30)
	assertRenderedHeightFits(t, rendered, 12)
}

func TestGoalRecoveryHonorsNoColor(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	recovery := newGoalRecoveryFixture(80, 24)
	if rendered := recovery.View(); strings.Contains(rendered, "\x1b[38") || strings.Contains(rendered, "\x1b[48") {
		t.Fatalf("NO_COLOR recovery emitted ANSI foreground/background colors: %q", rendered)
	}
}
