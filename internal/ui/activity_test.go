package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
	"github.com/charmbracelet/x/ansi"
)

func TestMinimumTerminalWorkingStatesFit(t *testing.T) {
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		set  func(*Model)
		want string
	}{
		{name: "idle", set: func(*Model) {}, want: "Ask · /help"},
		// Width decides between the counted label ("⚠ 1 MCP server unavailable")
		// and the compact fallback ("MCP unavailable"); both must name MCP.
		{name: "failed runtime", set: func(m *Model) {
			m.failedServers = []FailedServer{{Name: "tools", Reason: "offline"}}
		}, want: "MCP"},
		{name: "waiting", set: func(m *Model) {
			m.state = StateWaiting
			m.turnStartedAt = base.Add(-1500 * time.Millisecond)
		}, want: "Running"},
		{name: "reasoning", set: func(m *Model) {
			m.state = StateStreaming
			m.turnStartedAt = base.Add(-2 * time.Second)
			m.thinkBuf.WriteString("checking")
		}, want: "Running"},
		{name: "streaming", set: func(m *Model) {
			m.state = StateStreaming
			m.turnStartedAt = base.Add(-3 * time.Second)
			m.streamBuf.WriteString("partial")
		}, want: "Running"},
		{name: "session loading", set: func(m *Model) { m.sessionLoading = true }, want: "Restoring session"},
		{name: "file operation", set: func(m *Model) { m.fileLoading = true }, want: "Reading local file"},
		{name: "commit", set: func(m *Model) { m.commitRunning = true }, want: "Generating commit"},
		{name: "export", set: func(m *Model) { m.exportRunning = true }, want: "Publishing export"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t)
			m.model = "qwen3.5:2b"
			m.toolCount = 19
			m.reducedMotion = true
			m.now = func() time.Time { return base }
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
			m = updated.(*Model)
			tt.set(m)
			m.recalcViewportHeight()
			m.refreshTranscript()

			view := m.View().Content
			if !strings.Contains(view, tt.want) {
				t.Fatalf("minimum view missing %q:\n%s", tt.want, view)
			}
			if tt.name == "idle" && !strings.Contains(ansi.Strip(view), "Ask") {
				t.Fatalf("minimum composer placeholder is not actionable:\n%s", view)
			}
			assertRenderedLinesFit(t, view, 30)
			if got := lipgloss.Height(strings.TrimSuffix(view, "\n")); got > 12 {
				t.Fatalf("minimum view height = %d, want <= 12:\n%s", got, view)
			}
			if m.composerIsBusy() && !strings.Contains(view, "esc") && tt.name != "export" {
				t.Fatalf("cancellable working state lost Esc affordance:\n%s", view)
			}
			if (tt.name == "waiting" || tt.name == "reasoning" || tt.name == "streaming") &&
				!strings.Contains(view, "queue") {
				t.Fatalf("minimum active-turn view lost the queue affordance:\n%s", view)
			}
		})
	}

}

func TestAutoCheckpointActivityExplainsInvisibleContinuation(t *testing.T) {
	m := newTestModel(t)
	m.state = StateWaiting
	m.autoCheckpoints.segmentsContinued = 2
	activity, ok := m.currentWorkingActivity()
	if !ok || activity.label != "Continuing automatically" ||
		activity.detail != "checkpoint 2/8" || !activity.cancellable {
		t.Fatalf("AUTO checkpoint activity = %#v, ok=%v", activity, ok)
	}
}

func TestMinimumTerminalNoticeKeepsSettingsRecoveryVisible(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = updated.(*Model)
	m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "local runtime unavailable"})
	m.invalidateEntryCache()
	m.recalcViewportHeight()
	m.refreshTranscript()
	m.transcriptGotoBottom()

	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "ctrl+p settings") {
		t.Fatalf("minimum notice state lost the Settings recovery path:\n%s", view)
	}
	assertRenderedHeightFits(t, m.View().Content, m.height)
}

func TestAnimationClocksStopOutsideTheirPhase(t *testing.T) {
	m := newTestModel(t)
	m.initializing = false
	m.state = StateIdle

	spinnerMsg := m.spin.Tick()
	if _, cmd := m.Update(spinnerMsg); cmd != nil {
		t.Fatal("idle spinner tick scheduled another frame")
	}

	m.state = StateStreaming
	spinnerMsg = m.spin.Tick()
	if _, cmd := m.Update(spinnerMsg); cmd == nil {
		t.Fatal("active streaming spinner did not schedule another frame")
	}

	m.state = StateIdle
	scrambleCmd := m.scramble.Tick()
	if _, cmd := m.Update(scrambleCmd()); cmd != nil {
		t.Fatal("idle scramble tick scheduled another frame")
	}

	m.state = StateWaiting
	scrambleCmd = m.scramble.Tick()
	if _, cmd := m.Update(scrambleCmd()); cmd == nil {
		t.Fatal("waiting scramble did not schedule another frame")
	}
}

// TestRunningToolCardAnimatesWhileFooterOwnsTheClock pins how a running
// receipt and the footer divide one activity clock.
//
// The receipt owns motion and nothing else: it advances a glyph so the card
// reads as working rather than stuck, and it still carries no elapsed time, so
// there remains exactly one place on screen to read how long this has taken.
// Reduced motion keeps the static cue — an ellipsis says unfinished without
// moving — which is also what every terminal spec renders.
func TestRunningToolCardAnimatesWhileFooterOwnsTheClock(t *testing.T) {
	m := newTestModel(t)
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base.Add(1500 * time.Millisecond) }
	m.state = StateStreaming
	m.toolsPending = 1
	m.toolEntries = []ToolEntry{{
		ID: "call-1", Name: "read_file", Args: `{"path":"internal/ui/view.go"}`,
		Summary: "internal/ui/view.go", Status: ToolStatusRunning, StartTime: base, Collapsed: true,
	}}
	m.entries = []ChatEntry{
		{Kind: "user", Content: "inspect the UI"},
		{Kind: "tool_group", ToolIndex: 0},
	}
	m.refreshTranscript()
	before := m.viewport.View()
	footerBefore := m.renderWorkingLine()

	msg := m.spin.Tick()
	updated, _ := m.Update(msg)
	m = updated.(*Model)
	after := m.viewport.View()
	footerAfter := m.renderWorkingLine()
	if before == after {
		t.Fatalf("spinner tick left the running ToolCard on a frozen frame:\n%s", after)
	}
	if footerBefore == footerAfter || !strings.Contains(footerAfter, "Tool running") ||
		!strings.Contains(footerAfter, "1.5s") || strings.Contains(footerAfter, "internal/ui") {
		t.Fatalf("tool footer did not own live activity: before=%q after=%q", footerBefore, footerAfter)
	}
	for _, want := range []string{"Reading", "internal/ui"} {
		if !strings.Contains(after, want) {
			t.Fatalf("running tool receipt missing %q:\n%s", want, after)
		}
	}
	if strings.Contains(after, "1.5s") {
		t.Fatalf("running ToolCard retained live elapsed time:\n%s", after)
	}

	reduced := newTestModel(t)
	reduced.reducedMotion = true
	reduced.now = m.now
	reduced.state = StateStreaming
	reduced.toolsPending = 1
	reduced.toolEntries = append([]ToolEntry(nil), m.toolEntries...)
	reduced.toolEntries[0].Status = ToolStatusRunning
	reduced.entries = []ChatEntry{
		{Kind: "user", Content: "inspect the UI"},
		{Kind: "tool_group", ToolIndex: 0},
	}
	reduced.refreshTranscript()
	quiet := reduced.viewport.View()
	if !strings.Contains(quiet, "…") {
		t.Fatalf("reduced motion dropped the static running cue:\n%s", quiet)
	}
	updated, _ = reduced.Update(reduced.spin.Tick())
	reduced = updated.(*Model)
	if got := reduced.viewport.View(); got != quiet {
		t.Fatalf("reduced motion animated the running receipt:\nbefore:\n%s\nafter:\n%s", quiet, got)
	}

	m.toolsPending = 0
	m.toolEntries[0].Status = ToolStatusDone
	m.toolEntries[0].Duration = 2 * time.Second
	m.invalidateEntryCache()
	m.refreshTranscript()
	stable := m.viewport.View()

	foreign := spinner.TickMsg{Time: base}
	updated, _ = m.Update(foreign)
	m = updated.(*Model)
	if got := m.viewport.View(); got != stable {
		t.Fatalf("completed card changed across an idle tick:\nbefore:\n%s\nafter:\n%s", stable, got)
	}
}

func TestRunningToolActivityIsScopedToCurrentTurn(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	m := newTestModel(t)
	m.now = func() time.Time { return base.Add(10 * time.Second) }
	m.toolsPending = 1
	m.toolEntries = []ToolEntry{
		{
			ID: "old-running", Name: "read_file",
			Status: ToolStatusRunning, StartTime: base.Add(-time.Hour),
		},
		{
			ID: "current-running", Name: "read_file",
			Status: ToolStatusRunning, StartTime: base,
		},
	}
	m.turnToolStartIndex = 1

	if got := m.runningToolElapsed(); got != 10*time.Second {
		t.Fatalf("current-turn elapsed = %s, want 10s", got)
	}
}

func BenchmarkCurrentWorkingActivityWithLargeToolHistory(b *testing.B) {
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	m := newTestModelB(b)
	m.now = func() time.Time { return base.Add(10 * time.Second) }
	m.toolsPending = 1
	m.toolEntries = make([]ToolEntry, 10_001)
	for index := 0; index < 10_000; index++ {
		m.toolEntries[index] = ToolEntry{
			ID:   fmt.Sprintf("history-%05d", index),
			Name: "read_file", Status: ToolStatusDone,
		}
	}
	m.turnToolStartIndex = 10_000
	m.toolEntries[m.turnToolStartIndex] = ToolEntry{
		ID: "current", Name: "read_file",
		Status: ToolStatusRunning, StartTime: base,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = m.currentWorkingActivity()
	}
}

func TestActiveToolFooterSitsAdjacentToFramedComposer(t *testing.T) {
	// Grok-style density: activity rail immediately above the framed draft;
	// the frame top edge is the breathing boundary (no extra blank row).
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updated.(*Model)
	m.state = StateStreaming
	m.toolsPending = 1
	m.reducedMotion = true
	m.recalcViewportHeight()
	m.refreshTranscript()

	view := ansi.Strip(m.View().Content)
	statusAt := strings.Index(view, "Tool running")
	composerAt := strings.Index(view, "Write a follow-up")
	if statusAt < 0 || composerAt < 0 || statusAt >= composerAt {
		t.Fatalf("activity/composer order is invalid: status=%d composer=%d\n%s", statusAt, composerAt, view)
	}
	between := view[statusAt:composerAt]
	// Exactly one newline between activity text and the next chrome row.
	if strings.Count(between, "\n") < 1 {
		t.Fatalf("activity rail not stacked above composer:\n%s", between)
	}
	assertRenderedHeightFits(t, m.View().Content, m.height)
}

func TestAutoActivityFooterSeparatesAuthorityAndKeyboardActions(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updated.(*Model)
	m.mode = ModeAuto
	m.state = StateStreaming
	m.reducedMotion = true
	m.turnStartedAt = time.Now().Add(-2 * time.Second)

	line := m.renderWorkingLine()
	plain := ansi.Strip(line)
	for _, want := range []string{"Running", "AUTO", "esc stop", "enter queue"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("AUTO activity footer omitted %q: %q", want, plain)
		}
	}
	for _, styled := range []string{
		m.styles.ToolRunningText.Render("Running"),
		m.styles.ModeBuild.Render("AUTO"),
		m.styles.FocusIndicator.Render("esc"),
		m.styles.FocusIndicator.Render("enter"),
	} {
		if !strings.Contains(line, styled) {
			t.Fatalf("AUTO activity footer lost semantic styling %q:\n%s", ansi.Strip(styled), line)
		}
	}
	if width := lipgloss.Width(line); width > m.chatPaneWidth() {
		t.Fatalf("AUTO activity footer width = %d, pane = %d: %q", width, m.chatPaneWidth(), plain)
	}
}

func TestMinimumAutoActivityKeepsAuthorityAndRecoveryKeys(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = updated.(*Model)
	m.mode = ModeAuto
	m.state = StateStreaming
	m.reducedMotion = true

	line := m.renderWorkingLine()
	plain := ansi.Strip(line)
	for _, want := range []string{"Run", "AUTO", "esc", "queue"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("minimum AUTO footer omitted %q: %q", want, plain)
		}
	}
	if width := lipgloss.Width(line); width > m.chatPaneWidth() {
		t.Fatalf("minimum AUTO footer width = %d, pane = %d: %q", width, m.chatPaneWidth(), plain)
	}
}

func TestAutoActivityRetainsAuthorityAcrossResponsiveVariants(t *testing.T) {
	variants := []struct {
		name  string
		setup func(*Model)
	}{
		{name: "queue available", setup: func(*Model) {}},
		{name: "goal owns queue", setup: func(m *Model) { m.goalTurnID = "goal-turn" }},
		{name: "follow paused", setup: func(m *Model) { m.pauseFollow() }},
	}
	for _, width := range []int{30, 40, 58, 80} {
		for _, variant := range variants {
			t.Run(fmt.Sprintf("%s/%d", variant.name, width), func(t *testing.T) {
				m := newTestModel(t)
				m.width = width
				m.mode = ModeAuto
				m.state = StateStreaming
				m.reducedMotion = true
				variant.setup(m)

				line := ansi.Strip(m.renderWorkingLine())
				if !strings.Contains(line, "AUTO") {
					t.Fatalf("width %d AUTO footer lost authority in %s: %q", width, variant.name, line)
				}
				if got := lipgloss.Width(line); got > m.chatPaneWidth() {
					t.Fatalf("width %d AUTO footer is %d cells in %s: %q", width, got, variant.name, line)
				}
			})
		}
	}
}

func TestWorkingControlKeyRecognizesOnlyRealFooterKeys(t *testing.T) {
	for _, test := range []struct {
		segment string
		want    string
	}{
		{segment: "esc stop", want: "esc"},
		{segment: "enter queue", want: "enter"},
		{segment: "end latest", want: "end"},
		{segment: "Route ambiguous", want: ""},
		{segment: "queue", want: ""},
	} {
		if got := workingControlKey(test.segment); got != test.want {
			t.Fatalf("workingControlKey(%q) = %q, want %q", test.segment, got, test.want)
		}
	}
}

func TestCompletedToolFlashAdvertisesInspectableReceipt(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updated.(*Model)
	m.footerNotice = &footerNotice{text: "✓ Done", severity: noticeSuccess}
	m.toolEntries = []ToolEntry{{Name: "hitspec_capture_webpage", Status: ToolStatusDone, Collapsed: true}}
	m.lastTurnToolIndex = 0

	status := ansi.Strip(m.renderStatusLine())
	if !strings.Contains(status, "Done") || !strings.Contains(status, "ctrl+t inspect receipt") {
		t.Fatalf("completed tool footer does not expose receipt inspection: %q", status)
	}
	m.toolEntries[0].Collapsed = false
	if status = ansi.Strip(m.renderStatusLine()); !strings.Contains(status, "ctrl+t hide receipt") {
		t.Fatalf("expanded tool footer does not expose receipt collapse: %q", status)
	}

	m.input.SetValue("draft")
	if status = ansi.Strip(m.renderStatusLine()); strings.Contains(status, "inspect receipt") || strings.Contains(status, "hide receipt") {
		t.Fatalf("receipt shortcut was advertised while the composer owns a draft: %q", status)
	}
}

func TestCompletedNoToolTurnDoesNotAdvertiseStaleReceipt(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updated.(*Model)
	m.toolEntries = []ToolEntry{
		{Name: "old_failed_read", Status: ToolStatusError, IsError: true, Collapsed: true},
		{Name: "old_successful_read", Status: ToolStatusDone, Collapsed: true},
	}
	m.turnToolStartIndex = len(m.toolEntries)
	m.state = StateStreaming

	updated, _ = m.Update(AgentDoneMsg{})
	m = updated.(*Model)
	status := ansi.Strip(m.renderStatusLine())
	if !strings.Contains(status, "Done") {
		t.Fatalf("successful no-tool turn lost completion status: %q", status)
	}
	if strings.Contains(status, "inspect receipt") || strings.Contains(status, "hide receipt") {
		t.Fatalf("no-tool turn advertised an earlier tool receipt: %q", status)
	}
}

func TestCurrentTurnToolReceiptOutcomeFailsClosed(t *testing.T) {
	successfulProjection := ecosystem.ToolProjection{
		Transport: ecosystem.TransportSucceeded,
		Domain:    ecosystem.DomainSucceeded,
	}
	tests := []struct {
		name       string
		entry      *ToolEntry
		successful bool
	}{
		{name: "no tools", successful: true},
		{name: "semantic success", entry: &ToolEntry{
			Status: ToolStatusDone, Projection: successfulProjection,
		}, successful: true},
		{name: "running", entry: &ToolEntry{
			Status: ToolStatusRunning, Projection: successfulProjection,
		}},
		{name: "error", entry: &ToolEntry{
			Status: ToolStatusError, IsError: true, Projection: successfulProjection,
		}},
		{name: "cancelled", entry: &ToolEntry{
			Status: ToolStatusCancelled, Projection: successfulProjection,
		}},
		{name: "domain attention", entry: &ToolEntry{
			Status: ToolStatusDone,
			Projection: ecosystem.ToolProjection{
				Transport: ecosystem.TransportSucceeded,
				Domain:    ecosystem.DomainAttention,
			},
		}},
		{name: "domain unknown", entry: &ToolEntry{
			Status: ToolStatusDone,
			Projection: ecosystem.ToolProjection{
				Transport: ecosystem.TransportSucceeded,
				Domain:    ecosystem.DomainUnknown,
			},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.toolEntries = []ToolEntry{{
				Name: "historical_failure", Status: ToolStatusError, IsError: true,
			}}
			m.turnToolStartIndex = len(m.toolEntries)
			if test.entry != nil {
				m.toolEntries = append(m.toolEntries, *test.entry)
			}

			last, successful := m.currentTurnToolReceiptOutcome()
			if successful != test.successful {
				t.Fatalf("successful = %v, want %v", successful, test.successful)
			}
			wantLast := -1
			if test.entry != nil && test.entry.Status != ToolStatusRunning {
				wantLast = 1
			}
			if last != wantLast {
				t.Fatalf("last terminal receipt = %d, want %d", last, wantLast)
			}
		})
	}

	t.Run("invalid turn boundary", func(t *testing.T) {
		m := newTestModel(t)
		m.toolEntries = []ToolEntry{{
			Status: ToolStatusDone, Projection: successfulProjection,
		}}
		m.turnToolStartIndex = len(m.toolEntries) + 1
		last, successful := m.currentTurnToolReceiptOutcome()
		if last != -1 || successful {
			t.Fatalf("invalid boundary outcome = last %d, successful %v", last, successful)
		}
	})
}

func TestDeniedToolFollowedByModelResponseDoesNotFlashTurnSuccess(t *testing.T) {
	base := time.Date(2026, 7, 18, 23, 20, 29, 0, time.UTC)
	m := newTestModel(t)
	m.now = func() time.Time { return base.Add(3300 * time.Millisecond) }
	m.turnStartedAt = base
	m.state = StateStreaming
	m.turnToolStartIndex = len(m.toolEntries)

	updated, _ := m.Update(ToolCallStartMsg{
		ID:        "approval-denied-write",
		Name:      "write",
		Args:      map[string]any{"path": "approval-probe.txt", "content": "must not be written"},
		StartTime: base,
	})
	m = updated.(*Model)
	updated, _ = m.Update(ToolCallResultMsg{
		ID:       "approval-denied-write",
		Name:     "write",
		Result:   "tool call denied: user denied tool execution",
		IsError:  true,
		Duration: 300 * time.Millisecond,
	})
	m = updated.(*Model)
	updated, _ = m.Update(StreamTextMsg{
		Text: "Denied safely. No file was changed.",
	})
	m = updated.(*Model)
	updated, _ = m.Update(StreamDoneMsg{EvalCount: 7, PromptTokens: 19})
	m = updated.(*Model)
	updated, _ = m.Update(AgentDoneMsg{})
	m = updated.(*Model)

	if m.state != StateIdle {
		t.Fatalf("denied-tool turn state = %v, want idle", m.state)
	}
	if len(m.toolEntries) != 1 ||
		m.toolEntries[0].Status != ToolStatusError ||
		!m.toolEntries[0].IsError {
		t.Fatalf("denied tool receipt = %#v", m.toolEntries)
	}
	if transcript := ansi.Strip(m.renderEntries()); !strings.Contains(
		transcript,
		"Denied safely. No file was changed.",
	) {
		t.Fatalf("settled turn omitted the model response:\n%s", transcript)
	}
	status := ansi.Strip(m.renderStatusLine())
	success := glyphSet(m.glyphProfile).Success + " Done"
	if m.footerNotice != nil {
		t.Fatalf("denied-tool turn armed a duplicate notice %#v; footer=%q",
			m.footerNotice, status)
	}
	if strings.Contains(status, success) {
		t.Fatalf("denied-tool turn rendered misleading success footer %q", status)
	}
	if m.sessionTurnCount != 1 {
		t.Fatalf("settled denial turn count = %d, want 1", m.sessionTurnCount)
	}
}

func TestInlineApprovalPausesActivityClock(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	responses := make(chan permission.ApprovalResponse, 1)
	m = openApprovalForTest(t, m, ToolApprovalMsg{
		ToolName: "write_file",
		Args:     map[string]any{"path": "internal/ui/view.go"},
		Response: responses,
	})

	inline := ansi.Strip(m.renderApproval())
	for _, want := range []string{"Permission · write_file", "once", "session", "deny"} {
		if !strings.Contains(inline, want) {
			t.Fatalf("inline approval omitted %q:\n%s", want, inline)
		}
	}
	if m.needsSpinner() || m.needsScramble() || m.renderWorkingLine() != "" {
		t.Fatal("inline approval left a hidden activity clock or footer line active")
	}

	updated, cmd := m.Update(charKey('y'))
	m = updated.(*Model)
	if cmd == nil || m.pendingApproval != nil || !m.needsSpinner() {
		t.Fatal("approval response did not resume the streaming activity clock")
	}
	if response := <-responses; !response.Allowed {
		t.Fatal("approval response was not delivered")
	}
}

func TestShutdownTransfersWaitingAnimationOwnership(t *testing.T) {
	m := newTestModel(t)
	m.state = StateWaiting
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	stale := ScrambleTickMsg{ID: m.scramble.id, Frame: m.scramble.frame}

	cmd := m.beginShutdown()
	if cmd == nil || m.needsScramble() || !m.needsSpinner() {
		t.Fatal("shutdown did not transfer from scramble to spinner ownership")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("shutdown did not cancel the waiting turn")
	}
	frame := m.scramble.frame
	updated, _ := m.Update(stale)
	m = updated.(*Model)
	if m.scramble.frame != frame {
		t.Fatal("a hidden waiting tick advanced during shutdown")
	}
	if got := strings.Count(m.View().Content, "Stopping safely"); got != 1 {
		t.Fatalf("shutdown activity rendered %d competing surfaces", got)
	}
}

func TestOwnedOperationsNeverRenderReady(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(*Model)
	}{
		{name: "file", set: func(m *Model) { m.fileLoading = true }},
		{name: "commit", set: func(m *Model) { m.commitRunning = true }},
		{name: "export", set: func(m *Model) { m.exportRunning = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t)
			tt.set(m)
			if got := m.renderStatusLine(); strings.Contains(strings.ToLower(got), "ready") {
				t.Fatalf("owned operation rendered misleading ready status: %q", got)
			}
			if line := m.renderWorkingLine(); line == "" {
				t.Fatal("owned operation has no visible working line")
			}
		})
	}
}

func TestPausedFollowRecoveryFitsTheSingleWorkingRow(t *testing.T) {
	for _, width := range []int{30, 40, 80} {
		m := newTestModel(t)
		m.width = width
		m.state = StateStreaming
		m.turnStartedAt = m.nowTime().Add(-2 * time.Second)
		m.pauseFollow()

		line := ansi.Strip(m.renderWorkingLine())
		if !strings.Contains(line, "end") {
			t.Fatalf("width %d paused working line lost End recovery: %q", width, line)
		}
		if !strings.Contains(line, "esc") {
			t.Fatalf("width %d paused working line lost cancellation: %q", width, line)
		}
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("width %d paused working line is %d cells: %q", width, got, line)
		}
	}
}

func TestCompletedTurnShowsShortUsefulReceipt(t *testing.T) {
	m := newTestModel(t)
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base.Add(2400 * time.Millisecond) }
	m.turnStartedAt = base
	m.state = StateStreaming

	updated, _ := m.Update(AgentDoneMsg{})
	m = updated.(*Model)
	status := m.renderStatusLine()
	for _, want := range []string{"Done", "2.4s"} {
		if !strings.Contains(status, want) {
			t.Fatalf("completion receipt missing %q: %q", want, status)
		}
	}

	if m.footerNotice == nil {
		t.Fatal("completed turn did not arm the footer notice")
	}
	updated, _ = m.Update(footerNoticeExpiredMsg{deadline: m.footerNotice.expiresAt})
	m = updated.(*Model)
	if strings.Contains(m.renderStatusLine(), "Done") {
		t.Fatal("completion receipt did not settle back to idle status")
	}
}

func TestReducedMotionUsesStaticWorkingGlyph(t *testing.T) {
	t.Setenv("SONAR_REDUCED_MOTION", "1")
	m := newTestModel(t)
	if !m.reducedMotion {
		t.Fatal("reduced-motion environment was not applied")
	}
	m.state = StateStreaming
	line := m.renderWorkingLine()
	if !strings.Contains(line, "…") || !strings.Contains(line, "Running") {
		t.Fatalf("reduced-motion working line is not clear and static: %q", line)
	}
	if strings.Contains(line, "•") {
		t.Fatalf("reduced-motion working line used an ambiguous settled dot: %q", line)
	}
	if cmd := m.startSpinnerCmd(); cmd != nil {
		t.Fatal("reduced motion scheduled a spinner clock")
	}
	if m.needsSpinner() || m.needsScramble() {
		t.Fatal("reduced motion admitted a decorative activity clock")
	}
	if cmd := m.startActivityCmd(); cmd == nil || !m.activityHeartbeatPending {
		t.Fatal("reduced motion omitted its low-frequency informational heartbeat")
	}
}

func TestReducedMotionActivityHeartbeatHasOnePendingChain(t *testing.T) {
	m := newTestModel(t)
	m.reducedMotion = true
	m.state = StateStreaming

	first := m.startActivityCmd()
	if first == nil || !m.activityHeartbeatPending {
		t.Fatal("first active state did not schedule a heartbeat")
	}
	firstToken := m.activityHeartbeatToken
	if duplicate := m.startActivityCmd(); duplicate != nil {
		t.Fatal("active state scheduled a duplicate heartbeat")
	}
	if m.activityHeartbeatToken != firstToken {
		t.Fatalf("duplicate start advanced token from %d to %d", firstToken, m.activityHeartbeatToken)
	}

	updated, cmd := m.Update(activityHeartbeatMsg{Token: firstToken - 1})
	m = updated.(*Model)
	if cmd != nil || !m.activityHeartbeatPending || m.activityHeartbeatToken != firstToken {
		t.Fatal("stale heartbeat disturbed the live chain")
	}
}

func TestReducedMotionActivityHeartbeatStopsWhenIdle(t *testing.T) {
	m := newTestModel(t)
	m.reducedMotion = true
	m.state = StateStreaming
	if cmd := m.startActivityCmd(); cmd == nil {
		t.Fatal("active state did not schedule a heartbeat")
	}
	token := m.activityHeartbeatToken
	m.state = StateIdle

	updated, cmd := m.Update(activityHeartbeatMsg{Token: token})
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("idle heartbeat scheduled another receipt")
	}
	if m.activityHeartbeatPending {
		t.Fatal("idle heartbeat remained pending")
	}
}

func TestReducedMotionActivityHeartbeatRefreshesElapsedInformation(t *testing.T) {
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	t.Run("provider footer", func(t *testing.T) {
		now := base.Add(500 * time.Millisecond)
		m := newTestModel(t)
		m.reducedMotion = true
		m.state = StateWaiting
		m.turnStartedAt = base
		m.now = func() time.Time { return now }

		if line := ansi.Strip(m.renderWorkingLine()); strings.Contains(line, "0.5s") {
			t.Fatalf("sub-second provider footer exposed a noisy timer: %q", line)
		}
		if cmd := m.startActivityCmd(); cmd == nil {
			t.Fatal("provider activity did not schedule a heartbeat")
		}
		token := m.activityHeartbeatToken
		now = base.Add(1500 * time.Millisecond)

		updated, next := m.Update(activityHeartbeatMsg{Token: token})
		m = updated.(*Model)
		if next == nil || !m.activityHeartbeatPending {
			t.Fatal("active provider heartbeat did not continue")
		}
		if line := ansi.Strip(m.renderWorkingLine()); !strings.Contains(line, "1.5s") {
			t.Fatalf("provider elapsed time did not advance: %q", line)
		}
		if m.needsSpinner() || m.needsScramble() {
			t.Fatal("provider heartbeat enabled decorative motion")
		}
	})

	t.Run("running tool card", func(t *testing.T) {
		now := base.Add(1200 * time.Millisecond)
		m := newTestModel(t)
		m.reducedMotion = true
		m.state = StateStreaming
		m.turnStartedAt = base
		m.now = func() time.Time { return now }
		m.toolsPending = 1
		m.toolEntries = []ToolEntry{{
			ID: "call-heartbeat", Name: "read_file", Summary: "internal/ui/activity.go",
			Status: ToolStatusRunning, StartTime: base, Collapsed: true,
		}}
		m.entries = []ChatEntry{
			{Kind: "user", Content: "inspect the activity timer"},
			testToolChatEntry(0),
		}
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.transcriptGotoBottom()
		beforeTranscript := m.viewport.View()
		if strings.Contains(ansi.Strip(beforeTranscript), "1.2s") {
			t.Fatalf("running ToolCard exposed live elapsed time:\n%s", beforeTranscript)
		}
		if beforeFooter := ansi.Strip(m.renderWorkingLine()); !strings.Contains(beforeFooter, "1.2s") {
			t.Fatalf("tool footer omitted initial elapsed time: %q", beforeFooter)
		}
		if cmd := m.startActivityCmd(); cmd == nil {
			t.Fatal("running tool did not schedule a heartbeat")
		}
		token := m.activityHeartbeatToken
		now = base.Add(3200 * time.Millisecond)

		updated, next := m.Update(activityHeartbeatMsg{Token: token})
		m = updated.(*Model)
		if next == nil {
			t.Fatal("running tool heartbeat did not continue")
		}
		afterTranscript := m.viewport.View()
		if afterTranscript != beforeTranscript {
			t.Fatalf("running tool heartbeat repainted the transcript:\nbefore:\n%s\nafter:\n%s", beforeTranscript, afterTranscript)
		}
		afterFooter := ansi.Strip(m.renderWorkingLine())
		if !strings.Contains(afterFooter, "3.2s") || strings.Contains(afterFooter, "1.2s") {
			t.Fatalf("tool footer elapsed time did not advance: %q", afterFooter)
		}
		if after := ansi.Strip(afterTranscript); strings.Contains(after, "•") || !strings.Contains(after, "…") {
			t.Fatalf("running tool heartbeat changed the static receipt glyph:\n%s", after)
		}
	})
}

func TestReducedMotionUsesStaticRunningToolGlyph(t *testing.T) {
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	newRunningModel := func(t *testing.T) *Model {
		t.Helper()
		m := newTestModel(t)
		m.reducedMotion = true
		m.now = func() time.Time { return base.Add(1500 * time.Millisecond) }
		m.toolEntries = []ToolEntry{{
			ID: "call-1", Name: "read_file", Status: ToolStatusRunning, StartTime: base,
		}}
		return m
	}

	t.Run("tool card", func(t *testing.T) {
		m := newRunningModel(t)
		var rendered strings.Builder
		m.renderToolGroup(&rendered, testToolChatEntry(0))
		view := ansi.Strip(rendered.String())
		if !strings.Contains(view, "…") || strings.Contains(view, "•") {
			t.Fatalf("reduced-motion tool card used an ambiguous activity glyph: %q", view)
		}
	})

	t.Run("strict projection", func(t *testing.T) {
		m := newRunningModel(t)
		var rendered strings.Builder
		m.renderToolGroup(&rendered, testToolChatEntry(0))
		view := ansi.Strip(rendered.String())
		if !strings.Contains(view, "…") || strings.Contains(view, "•") {
			t.Fatalf("reduced-motion fallback receipt used an ambiguous activity glyph: %q", view)
		}
	})
}

func TestRunningFooterOwnsControlsNotAssistantState(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.model = "private-model-name"
	m.thinkBuf.WriteString("working through the request")

	line := ansi.Strip(m.renderWorkingLine())
	for _, want := range []string{"Running", "esc stop", "enter queue"} {
		if !strings.Contains(line, want) {
			t.Fatalf("operational footer omitted %q: %q", want, line)
		}
	}
	for _, forbidden := range []string{"Thinking", "Reasoning", "Responding", "private-model-name"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("operational footer duplicated assistant/model state %q: %q", forbidden, line)
		}
	}
}

func TestIdleRecoveryUsesOneCompactFooterAction(t *testing.T) {
	m := newTestModel(t)
	m.standaloneRecovery = &standaloneRecoveryState{}

	status := ansi.Strip(m.renderStatusLine())
	for _, want := range []string{"Recovery paused", "/recover", "inspect"} {
		if !strings.Contains(status, want) {
			t.Fatalf("recovery footer omitted %q: %q", want, status)
		}
	}
	if strings.Contains(status, "\n") {
		t.Fatalf("wide recovery footer used more than one row: %q", status)
	}
}

func TestFormatWorkingElapsed(t *testing.T) {
	tests := map[time.Duration]string{
		0:                       "0.0s",
		1500 * time.Millisecond: "1.5s",
		12 * time.Second:        "12s",
		64 * time.Second:        "1m04s",
		-500 * time.Millisecond: "0.0s",
	}
	for input, want := range tests {
		if got := formatWorkingElapsed(input); got != want {
			t.Errorf("formatWorkingElapsed(%s) = %q, want %q", input, got, want)
		}
	}
}

func TestWorkingLineKeepsActiveSessionHandleAtOrdinaryWidth(t *testing.T) {
	for _, width := range []int{30, 40, 80} {
		m := newTestModel(t)
		m.width = width
		m.state = StateWaiting
		m.turnStartedAt = time.Now().Add(-2 * time.Second)
		m.sessionID = 7
		m.sessionPublicID = "aaaaaa7"
		m.activeSessionTitle = "Composer polish"

		line := ansi.Strip(m.renderWorkingLine())
		if !strings.Contains(line, "aaaaaa7") {
			t.Fatalf("width %d working line omitted active session handle: %q", width, line)
		}
	}
}

func TestSpecialFootersKeepActiveSessionHandle(t *testing.T) {
	for _, width := range []int{30, 40, 80} {
		m := newTestModel(t)
		m.width = width
		m.sessionID = 42
		m.sessionPublicID = "aaaaa2a"
		m.activeSessionTitle = "TUI polish"

		m.pauseFollow()
		if line := ansi.Strip(m.renderFollowPausedStatus(width)); !strings.Contains(line, "aaaaa2a") || !strings.Contains(line, "end") {
			t.Fatalf("width %d follow-paused footer = %q", width, line)
		}

		m.resumeFollow()
		m.standaloneRecovery = &standaloneRecoveryState{}
		if line := ansi.Strip(m.renderStatusLine()); !strings.Contains(line, "aaaaa2a") || !strings.Contains(line, "/recover") {
			t.Fatalf("width %d recovery footer = %q", width, line)
		}

		m.standaloneRecovery = nil
		m.pendingImages = []pendingImageAttachment{{Ref: imageasset.Ref{Name: "screen.png"}}}
		if line := ansi.Strip(m.renderStatusLine()); !strings.Contains(line, "aaaaa2a") || !strings.Contains(line, "Images ready") {
			t.Fatalf("width %d image footer = %q", width, line)
		}
	}
}

func TestWorkingAuthorityAndSessionIdentityCoexistAcrossWidths(t *testing.T) {
	for _, mode := range []struct {
		value Mode
		label string
	}{
		{value: ModeAuto, label: "AUTO"},
		{value: ModePlan, label: "PLAN"},
	} {
		for _, width := range []int{30, 40, 80} {
			t.Run(fmt.Sprintf("%s/%d", mode.label, width), func(t *testing.T) {
				m := newTestModel(t)
				m.width = width
				m.mode = mode.value
				m.state = StateWaiting
				m.reducedMotion = true
				m.sessionID = 7
				m.sessionPublicID = "aaaaaa7"
				m.activeSessionTitle = "Composer polish"

				line := ansi.Strip(m.renderWorkingLine())
				for _, want := range []string{mode.label, "aaaaaa7"} {
					if !strings.Contains(line, want) {
						t.Fatalf("width %d footer omitted %q: %q", width, want, line)
					}
				}
				if got := lipgloss.Width(line); got > m.chatPaneWidth() {
					t.Fatalf("width %d footer is %d cells, pane %d: %q", width, got, m.chatPaneWidth(), line)
				}
			})
		}
	}
}

func TestSessionRestoreActivityUsesPendingTargetIdentity(t *testing.T) {
	for _, width := range []int{30, 40, 80} {
		m := newTestModel(t)
		m.width = width
		m.reducedMotion = true
		m.sessionID = 7
		m.sessionPublicID = "aaaaaa7"
		m.activeSessionTitle = "Source work"
		m.sessionLoading = true
		m.sessionLoadToken = 11
		m.pendingSessionSwitch = &pendingSessionSwitch{
			TargetSessionID: 42,
			TargetPublicID:  "aaaaa2a",
			TargetTitle:     "Target work",
			Choice:          sessionSwitchKeep,
			LoadToken:       11,
		}

		line := ansi.Strip(m.renderWorkingLine())
		if !strings.Contains(line, "aaaaa2a") || strings.Contains(line, "aaaaaa7") {
			t.Fatalf("width %d restore activity used source identity: %q", width, line)
		}
		if width >= 72 && !strings.Contains(line, "Target work") {
			t.Fatalf("width %d restore activity hid target title: %q", width, line)
		}
		if got := lipgloss.Width(line); got > m.chatPaneWidth() {
			t.Fatalf("width %d restore activity is %d cells, pane %d: %q", width, got, m.chatPaneWidth(), line)
		}
	}
}

func TestWorkingFooterShowsUnqueuedDraftImages(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.state = StateWaiting
	m.pendingImages = []pendingImageAttachment{{Ref: imageasset.Ref{Name: "screen.png"}}}
	line := ansi.Strip(m.renderWorkingLine())
	if !strings.Contains(line, "+ 1 image") {
		t.Fatalf("working footer hid pending draft image: %q", line)
	}
}

// TestActivityRailMarksWhatItDropped covers a small lie the rail used to tell.
// The same turn reads "Running · AUTO · 2m42s · ↑ 2.2k" on a wide pane and
// "Running · AUTO" on a narrow one, with nothing to distinguish a turn that has
// no elapsed time from a pane too narrow to show it.
func TestActivityRailMarksWhatItDropped(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	build := func(width int) string {
		m := newTestModel(t)
		m.width = width
		m.now = func() time.Time { return base.Add(162 * time.Second) }
		m.state = StateStreaming
		m.turnStartedAt = base
		m.evalCount = 2200
		return ansi.Strip(m.renderWorkingLine())
	}

	wide := build(120)
	if !strings.Contains(wide, "2m42s") || !strings.Contains(wide, "2.2k") {
		t.Fatalf("a wide rail dropped content it had room for: %q", wide)
	}
	if strings.HasSuffix(strings.TrimSpace(wide), "…") {
		t.Fatalf("a rail showing everything still claimed it had dropped something: %q", wide)
	}

	// Find the width where the rail actually starts dropping rather than
	// hard-coding one: a layout change should move this test, not break it.
	degradedAt, degraded := 0, ""
	for width := 110; width >= 44; width-- {
		if line := build(width); !strings.Contains(line, "2.2k") {
			degradedAt, degraded = width, line
			break
		}
	}
	if degradedAt == 0 {
		t.Fatal("no width in range dropped the token count; this test no longer covers degradation")
	}
	if !strings.HasSuffix(strings.TrimSpace(degraded), "…") {
		t.Fatalf("a rail degraded at width %d did not say so: %q", degradedAt, degraded)
	}
}

// The marker never costs content. Budgeting it before the candidate was chosen
// made the 30-column tier drop its route identity to make room for a mark
// saying something had been dropped.
func TestActivityRailMarkerNeverCostsATier(t *testing.T) {
	for _, width := range []int{30, 40, 56, 72, 90, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := newTestModel(t)
			m.width = width
			m.state = StateStreaming
			line := m.renderWorkingLine()
			if got := lipgloss.Width(line); got > m.chatPaneWidth() {
				t.Fatalf("rail overflowed: width=%d pane=%d line=%q", got, m.chatPaneWidth(), line)
			}
			if plain := ansi.Strip(line); !strings.Contains(plain, "Run") {
				t.Fatalf("rail lost its label at width %d: %q", width, plain)
			}
		})
	}
}
