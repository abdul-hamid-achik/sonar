package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func expertProgressEvent(sequence uint64, phase expertteam.ProgressPhase, index int) expertteam.ProgressEvent {
	event := expertteam.ProgressEvent{
		Sequence: sequence, Phase: phase, Strategy: expertselector.StrategySwarm,
		Total: 2, Parallelism: 1, ExpertIndex: index,
	}
	switch sequence {
	case 1:
		event.ExpertIndex = -1
		event.Queued = 2
	case 2:
		event.Running, event.Queued = 1, 1
		event.Expert, event.Model, event.Location = "critic", "qwen3.5:2b", llm.OllamaModelLocationLocal
	case 3:
		event.Queued, event.Completed = 1, 1
		event.Expert, event.Model, event.Location = "critic", "qwen3.5:2b", llm.OllamaModelLocationLocal
		event.Status, event.EvalTokens = expertteam.ExpertCompleted, 42
	case 4:
		event.Running, event.Completed = 1, 1
		event.Expert, event.Model, event.Location = "verifier", "flash:cloud", llm.OllamaModelLocationCloud
	case 5:
		event.Completed, event.Failed = 1, 1
		event.Expert, event.Model, event.Location = "verifier", "flash:cloud", llm.OllamaModelLocationCloud
		event.Status, event.ErrorCode = expertteam.ExpertFailed, "no_visible_report"
	}
	return event
}

func updateExpertProgressTestModel(t *testing.T, model *Model, msg tea.Msg) *Model {
	t.Helper()
	updated, _ := model.Update(msg)
	result, ok := updated.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", updated)
	}
	return result
}

func TestExpertProgressProducesOneBoundedInspectableSurface(t *testing.T) {
	m := newTestModel(t)
	m = updateExpertProgressTestModel(t, m, ToolCallStartMsg{
		ID: "experts-1", Name: "consult_experts", StartTime: time.Now(),
		Args: map[string]any{"objective": "private objective must not survive", "strategy": "swarm"},
	})
	if entry := m.toolEntries[0]; entry.Args != "" || entry.RawArgs != nil || entry.Collapsed {
		t.Fatalf("expert start retained private args or hid live surface: %#v", entry)
	}

	indices := []int{-1, 0, 0, 1, 1}
	for sequence := uint64(1); sequence <= 5; sequence++ {
		phase := []expertteam.ProgressPhase{
			expertteam.ProgressPlanned, expertteam.ProgressStarted, expertteam.ProgressCompleted,
			expertteam.ProgressStarted, expertteam.ProgressFailed,
		}[sequence-1]
		m = updateExpertProgressTestModel(t, m, ExpertProgressMsg{
			CallID: "experts-1", Event: expertProgressEvent(sequence, phase, indices[sequence-1]),
		})
	}

	progress := m.toolEntries[0].ExpertProgress
	if progress == nil || progress.Sequence != 5 || progress.Completed != 1 || progress.Failed != 1 {
		t.Fatalf("unexpected final progress: %#v", progress)
	}
	cardView := testProjectedToolCard(t, m, 0)
	view := cardView.View(96)
	for _, want := range []string{"critic", "qwen3.5:2b", "42 tok", "verifier", "flash:cloud", "no visible report"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expert surface missing %q:\n%s", want, view)
		}
	}
	activity, ok := m.currentWorkingActivity()
	if !ok || activity.label != "Consulting experts" || strings.Contains(activity.label, "Tool running") {
		t.Fatalf("expert activity = %#v, %v", activity, ok)
	}

	// A valid event for another call, a duplicate, a gap, and an ANSI-bearing
	// identity all fail closed without changing the accepted sequence.
	invalid := []ExpertProgressMsg{
		{CallID: "other", Event: expertProgressEvent(1, expertteam.ProgressPlanned, -1)},
		{CallID: "experts-1", Event: expertProgressEvent(5, expertteam.ProgressFailed, 1)},
		{CallID: "experts-1", Event: expertProgressEvent(7, expertteam.ProgressFailed, 1)},
	}
	malicious := expertProgressEvent(6, expertteam.ProgressStarted, 0)
	malicious.Expert = "critic\x1b[31m"
	invalid = append(invalid, ExpertProgressMsg{CallID: "experts-1", Event: malicious})
	for _, msg := range invalid {
		m = updateExpertProgressTestModel(t, m, msg)
	}
	if got := m.toolEntries[0].ExpertProgress.Sequence; got != 5 {
		t.Fatalf("invalid progress changed sequence to %d", got)
	}

	raw := "private objective must not survive\nprovider reasoning\n\x1b[31mraw report"
	m = updateExpertProgressTestModel(t, m, ToolCallResultMsg{
		ID: "experts-1", Name: "consult_experts", Result: raw, Duration: time.Second,
	})
	entry := m.toolEntries[0]
	if entry.Result != "" || entry.Args != "" || entry.ExpertProgress == nil {
		t.Fatalf("settled expert entry retained raw data or lost projection: %#v", entry)
	}
	if card := testProjectedToolCard(t, m, 0); card.Result != "" || card.State != ToolCardAttention {
		t.Fatalf("settled expert card = %#v", card)
	}
	// Progress arriving after settlement is stale, even with the right call ID.
	m = updateExpertProgressTestModel(t, m, ExpertProgressMsg{
		CallID: "experts-1", Event: expertProgressEvent(6, expertteam.ProgressStarted, 0),
	})
	if got := m.toolEntries[0].ExpertProgress.Sequence; got != 5 {
		t.Fatalf("post-settlement progress changed sequence to %d", got)
	}

	persisted := persistToolEntries(m.toolEntries)
	encoded, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private objective", "provider reasoning", "raw report", "\\u001b"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("persisted expert projection contains %q: %s", forbidden, encoded)
		}
	}
	restored := restoreToolEntries(persisted)
	if len(restored) != 1 || restored[0].ExpertProgress == nil || restored[0].Result != "" || restored[0].Args != "" {
		t.Fatalf("restored expert projection = %#v", restored)
	}
}

func TestExpertProgressRejectsUnsafeOrTamperedSnapshots(t *testing.T) {
	formatOnly := expertProgressEvent(2, expertteam.ProgressStarted, 0)
	formatOnly.Expert = "\u202e"
	if _, ok := normalizeExpertProgressEvent(formatOnly); ok {
		t.Fatal("format-control-only expert identity survived normalization")
	}
	overSequence := expertProgressEvent(6, expertteam.ProgressStarted, 0)
	overSequence.Expert, overSequence.Model = "critic", "qwen3.5:2b"
	if _, ok := normalizeExpertProgressEvent(overSequence); ok {
		t.Fatal("impossible progress sequence survived normalization")
	}

	state := newExpertProgressState(expertProgressEvent(1, expertteam.ProgressPlanned, -1))
	if state == nil {
		t.Fatal("planned state rejected")
	}
	started := expertProgressEvent(2, expertteam.ProgressStarted, 0)
	if !state.apply(started) {
		t.Fatal("valid start rejected")
	}
	tampered := cloneExpertProgressState(state)
	tampered.Experts[0].Model = "qwen\x1b[31m"
	if got := sanitizeExpertProgressState(tampered, false); got != nil {
		t.Fatalf("ANSI-bearing snapshot survived: %#v", got)
	}
	tampered = cloneExpertProgressState(state)
	tampered.Running, tampered.Queued = 0, 2
	if got := sanitizeExpertProgressState(tampered, false); got != nil {
		t.Fatalf("inconsistent counts survived: %#v", got)
	}
	maliciousPersisted := sanitizePersistedToolEntryArgs([]persistedToolEntry{{
		ID: "experts", Name: "consult_experts", Status: ToolStatusDone,
		Args: "objective=secret", Result: "raw report", ExpertProgress: tampered,
	}})
	if maliciousPersisted[0].Args != "" || maliciousPersisted[0].Result != "" || maliciousPersisted[0].ExpertProgress != nil {
		t.Fatalf("unsafe expert snapshot survived sanitization: %#v", maliciousPersisted[0])
	}
}

func TestExpertResultDropsIncompleteProgressProjection(t *testing.T) {
	m := newTestModel(t)
	m = updateExpertProgressTestModel(t, m, ToolCallStartMsg{
		ID: "experts-incomplete", Name: "consult_experts", StartTime: time.Now(),
		Args: map[string]any{"objective": "private", "strategy": "swarm"},
	})
	m = updateExpertProgressTestModel(t, m, ExpertProgressMsg{
		CallID: "experts-incomplete", Event: expertProgressEvent(1, expertteam.ProgressPlanned, -1),
	})
	m = updateExpertProgressTestModel(t, m, ToolCallResultMsg{
		ID: "experts-incomplete", Name: "consult_experts", Result: "untrusted aggregate report",
	})
	entry := m.toolEntries[0]
	if entry.ExpertProgress != nil || entry.Summary != "expert progress unavailable" || entry.Result != "" {
		t.Fatalf("incomplete progress was painted as settled: %#v", entry)
	}
	card := testProjectedToolCard(t, m, 0)
	if card.ExpertProgress != nil || card.Result != "" {
		t.Fatalf("incomplete card retained progress or raw result: %#v", card)
	}
}

func TestExpertProgressCardIsNarrowAndCached(t *testing.T) {
	state := newExpertProgressState(expertProgressEvent(1, expertteam.ProgressPlanned, -1))
	for sequence, phase := range []expertteam.ProgressPhase{
		expertteam.ProgressStarted, expertteam.ProgressCompleted,
		expertteam.ProgressStarted, expertteam.ProgressFailed,
	} {
		if !state.apply(expertProgressEvent(uint64(sequence+2), phase, sequence/2)) {
			t.Fatalf("event %d rejected", sequence+2)
		}
	}
	card := NewToolCard("consult_experts", ToolCardGeneric, true, defaultThemeID)
	card.State = ToolCardAttention
	card.Expanded = true
	card.SetSummary(state.summary())
	card.setExpertProgress(state, 24)
	first := card.View(26)
	second := card.View(26)
	if first != second || card.expertProgressCache == "" || card.expertProgressCacheSequence != state.Sequence {
		t.Fatal("expert detail rendering was not stable and cached")
	}
	for _, line := range strings.Split(first, "\n") {
		if got := lipgloss.Width(line); got > 26 {
			t.Fatalf("narrow line width = %d: %q", got, line)
		}
	}
}

func TestExpertProgressDetailsCapVisualRowsAndReportHiddenNodes(t *testing.T) {
	state := &ExpertProgressState{
		Sequence:    17,
		Strategy:    expertselector.StrategyTeam,
		Total:       8,
		Parallelism: 2,
		Completed:   8,
		Experts:     make([]ExpertProgressItem, 8),
	}
	for index := range state.Experts {
		state.Experts[index] = ExpertProgressItem{
			Index: index, Expert: "expert-" + string(rune('a'+index)), Model: "model-" + string(rune('a'+index)),
			Location: llm.OllamaModelLocationLocal, Phase: expertteam.ProgressCompleted,
			Status: expertteam.ExpertCompleted,
		}
	}

	wide := state.renderDetails(80, NewToolCardStyles(true, defaultThemeID))
	wideLines := strings.Split(wide, "\n")
	if len(wideLines) != maxExpertProgressDetailRows+1 {
		t.Fatalf("wide detail rows = %d, want %d:\n%s", len(wideLines), maxExpertProgressDetailRows+1, wide)
	}
	for _, want := range []string{"expert-a", "expert-f", "+2 more · Ctrl+G Agents"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("bounded wide detail missing %q:\n%s", want, wide)
		}
	}
	for _, hidden := range []string{"expert-g", "expert-h"} {
		if strings.Contains(wide, hidden) {
			t.Fatalf("bounded wide detail exposed hidden node %q:\n%s", hidden, wide)
		}
	}

	narrow := state.renderDetails(30, NewToolCardStyles(true, defaultThemeID))
	narrowLines := strings.Split(narrow, "\n")
	if len(narrowLines) != maxExpertProgressDetailRows+1 {
		t.Fatalf("narrow detail rows = %d, want %d:\n%s", len(narrowLines), maxExpertProgressDetailRows+1, narrow)
	}
	for _, want := range []string{"expert-a", "expert-c", "+5 more · Ctrl+G Agents"} {
		if !strings.Contains(narrow, want) {
			t.Fatalf("bounded narrow detail missing %q:\n%s", want, narrow)
		}
	}
	if strings.Contains(narrow, "expert-d") {
		t.Fatalf("narrow detail exceeded its visual row budget:\n%s", narrow)
	}
	for _, line := range narrowLines {
		if got := lipgloss.Width(line); got > 30 {
			t.Fatalf("narrow detail line width = %d: %q", got, line)
		}
	}
}

func TestExpertProgressDetailsKeepLateActiveNodeInsideInlineCap(t *testing.T) {
	state := &ExpertProgressState{
		Sequence:    9,
		Strategy:    expertselector.StrategyTeam,
		Total:       8,
		Parallelism: 1,
		Running:     1,
		Queued:      1,
		Completed:   6,
		Experts:     make([]ExpertProgressItem, 8),
	}
	for index := 0; index < 6; index++ {
		state.Experts[index] = ExpertProgressItem{
			Index: index, Expert: fmt.Sprintf("settled-%d", index), Model: "safe-model",
			Location: llm.OllamaModelLocationLocal, Phase: expertteam.ProgressCompleted,
			Status: expertteam.ExpertCompleted,
		}
	}
	state.Experts[6] = ExpertProgressItem{
		Index: 6, Expert: "active-last", Model: "safe-model",
		Location: llm.OllamaModelLocationCloud, Phase: expertteam.ProgressStarted,
	}

	for _, width := range []int{30, 80} {
		view := state.renderDetails(width, NewToolCardStyles(true, defaultThemeID))
		activeAt := strings.Index(view, "active-last")
		queueAt := strings.Index(view, "1 more queued")
		settledAt := strings.Index(view, "settled-0")
		if activeAt < 0 || queueAt < 0 || settledAt < 0 ||
			activeAt >= queueAt || queueAt >= settledAt {
			t.Fatalf("width %d let settled history crowd out live work:\n%s", width, view)
		}
		if !strings.Contains(view, "more · Ctrl+G Agents") {
			t.Fatalf("width %d omitted the honest hidden-node receipt:\n%s", width, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d rendered %d cells: %q", width, got, line)
			}
		}
	}
}

func TestExpertProgressDetailsPreserveQueuedAggregateWithinCap(t *testing.T) {
	state := &ExpertProgressState{
		Sequence:    17,
		Strategy:    expertselector.StrategySwarm,
		Total:       10,
		Parallelism: 2,
		Queued:      2,
		Completed:   8,
		Experts:     make([]ExpertProgressItem, 10),
	}
	for index := range state.Experts {
		if index == 1 || index == 7 {
			continue
		}
		state.Experts[index] = ExpertProgressItem{
			Index: index, Expert: "expert-" + string(rune('a'+index)), Model: "model-" + string(rune('a'+index)),
			Location: llm.OllamaModelLocationLocal, Phase: expertteam.ProgressCompleted,
			Status: expertteam.ExpertCompleted,
		}
	}

	view := state.renderDetails(80, NewToolCardStyles(true, defaultThemeID))
	firstSettledAt := strings.Index(view, "expert-a")
	queuedAt := strings.Index(view, "2 more queued")
	nextAt := strings.Index(view, "expert-c")
	if firstSettledAt < 0 || queuedAt < 0 || nextAt < 0 ||
		queuedAt >= firstSettledAt || firstSettledAt >= nextAt {
		t.Fatalf("queued aggregate was crowded out by settled history:\n%s", view)
	}
	if !strings.Contains(view, "+3 more · Ctrl+G Agents") {
		t.Fatalf("hidden-node receipt is not honest:\n%s", view)
	}
	if got := len(strings.Split(view, "\n")); got != maxExpertProgressDetailRows+1 {
		t.Fatalf("queued detail rows = %d, want %d:\n%s", got, maxExpertProgressDetailRows+1, view)
	}
}

type expertProgressProbe struct {
	ready    chan struct{}
	messages chan ExpertProgressMsg
}

func (probe *expertProgressProbe) Init() tea.Cmd {
	close(probe.ready)
	return nil
}

func (probe *expertProgressProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if progress, ok := msg.(ExpertProgressMsg); ok {
		probe.messages <- progress
	}
	return probe, nil
}

func (probe *expertProgressProbe) View() tea.View { return tea.NewView("") }

func TestAdapterForwardsExpertProgressWithCallID(t *testing.T) {
	probe := &expertProgressProbe{ready: make(chan struct{}), messages: make(chan ExpertProgressMsg, 1)}
	program := tea.NewProgram(
		probe, tea.WithInput(strings.NewReader("")), tea.WithOutput(io.Discard),
		tea.WithoutRenderer(), tea.WithoutSignalHandler(),
	)
	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()
	select {
	case <-probe.ready:
	case <-time.After(time.Second):
		t.Fatal("probe program did not start")
	}

	event := expertProgressEvent(1, expertteam.ProgressPlanned, -1)
	NewAdapter(program).ExpertProgress("experts-call", event)
	select {
	case got := <-probe.messages:
		if got.CallID != "experts-call" || got.Event != event {
			t.Fatalf("forwarded progress = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("adapter did not forward expert progress")
	}
	program.Quit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe program did not stop")
	}
}
