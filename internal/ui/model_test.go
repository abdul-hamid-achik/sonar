package ui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestSubmitInput_EmptyReturnsNil(t *testing.T) {
	m := newTestModel(t)
	// Input is empty by default.
	cmd := m.submitInput()
	if cmd != nil {
		t.Error("submitInput with empty input should return nil")
	}
}

func TestContextAdmissionPreflightFailureRestoresDraft(t *testing.T) {
	m := newTestModel(t)
	const prompt = "review the context budget"
	m.input.SetValue(prompt)

	if cmd := m.submitPreparedInput(prompt); cmd == nil {
		t.Fatal("prompt did not start")
	}
	if len(m.agent.Messages()) != 1 {
		t.Fatalf("precondition messages = %d, want 1", len(m.agent.Messages()))
	}

	updated, _ := m.Update(AgentDoneMsg{Err: &agent.TurnContextBudgetError{
		EstimatedPromptTokens: 21_134,
		ContextWindowTokens:   16_384,
	}})
	m = updated.(*Model)

	if got := m.input.Value(); got != prompt {
		t.Fatalf("restored draft = %q, want %q", got, prompt)
	}
	if len(m.agent.Messages()) != 0 {
		t.Fatalf("preflight-rejected prompt remained in agent history: %#v", m.agent.Messages())
	}
	for _, entry := range m.entries {
		if entry.Kind == "user" {
			t.Fatalf("preflight-rejected prompt remained in transcript: %#v", entry)
		}
	}
}

func TestHelpChordOnlyWhenIdleAndEmpty(t *testing.T) {
	for _, shortcut := range []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "f1_primary", key: f1Key()},
		{name: "ctrl_h_fallback", key: ctrlKey('h')},
	} {
		t.Run("idle_empty_opens_help_"+shortcut.name, func(t *testing.T) {
			m := newTestModel(t)
			m.state = StateIdle
			// Input is empty.

			updated, _ := m.Update(shortcut.key)
			m = updated.(*Model)

			if m.overlay != OverlayHelp {
				t.Errorf("%s with idle+empty should open help, got overlay=%d", shortcut.name, m.overlay)
			}
		})

		t.Run("idle_nonempty_no_help_"+shortcut.name, func(t *testing.T) {
			m := newTestModel(t)
			m.state = StateIdle
			m.input.SetValue("hello")

			updated, _ := m.Update(shortcut.key)
			m = updated.(*Model)

			if m.overlay == OverlayHelp {
				t.Errorf("%s with non-empty input should not open help", shortcut.name)
			}
		})

		t.Run("waiting_no_help_"+shortcut.name, func(t *testing.T) {
			m := newTestModel(t)
			m.state = StateWaiting

			updated, _ := m.Update(shortcut.key)
			m = updated.(*Model)

			if m.overlay == OverlayHelp {
				t.Errorf("%s in StateWaiting should not open help", shortcut.name)
			}
		})
	}

	t.Run("legacy_backspace_does_not_open_help", func(t *testing.T) {
		m := newTestModel(t)
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		m = updated.(*Model)
		if m.overlay == OverlayHelp {
			t.Error("legacy Backspace alias opened help")
		}
	})
}

func TestToggleToolsChordOnlyWhenIdleAndEmpty(t *testing.T) {
	t.Run("idle_empty_toggles", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateIdle
		before := m.toolsCollapsed

		updated, _ := m.Update(ctrlKey('b'))
		m = updated.(*Model)

		if m.toolsCollapsed == before {
			t.Error("Ctrl+B with idle+empty should toggle toolsCollapsed")
		}
	})

	t.Run("idle_nonempty_no_toggle", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateIdle
		m.input.SetValue("hello")
		before := m.toolsCollapsed

		updated, _ := m.Update(ctrlKey('b'))
		m = updated.(*Model)

		if m.toolsCollapsed != before {
			t.Error("Ctrl+B with non-empty input should not toggle tools")
		}
	})
}

func TestESC_CancelOnlyWhenStreamingOrWaiting(t *testing.T) {
	t.Run("idle_no_cancel", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateIdle
		cancelCalled := false
		m.cancel = func() { cancelCalled = true }

		updated, _ := m.Update(escKey())
		_ = updated.(*Model)

		if cancelCalled {
			t.Error("ESC in idle should not call cancel")
		}
	})

	t.Run("streaming_cancels", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateStreaming
		cancelCalled := false
		m.cancel = func() { cancelCalled = true }

		updated, _ := m.Update(escKey())
		_ = updated.(*Model)

		if !cancelCalled {
			t.Error("ESC in streaming should call cancel")
		}
	})

	t.Run("waiting_cancels", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateWaiting
		cancelCalled := false
		m.cancel = func() { cancelCalled = true }

		updated, _ := m.Update(escKey())
		_ = updated.(*Model)

		if !cancelCalled {
			t.Error("ESC in waiting should call cancel")
		}
	})
}

func TestSystemMessageMsg_AppendsEntry(t *testing.T) {
	m := newTestModel(t)
	before := len(m.entries)

	updated, _ := m.Update(SystemMessageMsg{Msg: "hello system"})
	m = updated.(*Model)

	if len(m.entries) != before+1 {
		t.Fatalf("expected %d entries, got %d", before+1, len(m.entries))
	}
	last := m.entries[len(m.entries)-1]
	if last.Kind != "system" {
		t.Errorf("expected kind 'system', got %q", last.Kind)
	}
	if last.Content != "hello system" {
		t.Errorf("expected content 'hello system', got %q", last.Content)
	}
}

func TestProviderNativeThinkingIsSeparatedFromAnswer(t *testing.T) {
	m := newTestModel(t)
	m.state = StateWaiting
	updated, _ := m.Update(StreamThinkingMsg{Text: "inspect constraints"})
	m = updated.(*Model)
	updated, _ = m.Update(StreamTextMsg{Text: "final answer"})
	m = updated.(*Model)
	updated, _ = m.Update(AgentDoneMsg{})
	m = updated.(*Model)

	last := m.entries[len(m.entries)-1]
	if last.Content != "final answer" || last.ThinkingContent != "inspect constraints" {
		t.Fatalf("thinking/answer were not separated: %#v", last)
	}
}

func TestErrorMsg_AppendsEntry(t *testing.T) {
	m := newTestModel(t)
	before := len(m.entries)

	updated, _ := m.Update(ErrorMsg{Msg: "something broke"})
	m = updated.(*Model)

	if len(m.entries) != before+1 {
		t.Fatalf("expected %d entries, got %d", before+1, len(m.entries))
	}
	last := m.entries[len(m.entries)-1]
	if last.Kind != "error" {
		t.Errorf("expected kind 'error', got %q", last.Kind)
	}
	if last.Content != "something broke" {
		t.Errorf("expected content 'something broke', got %q", last.Content)
	}
}

func TestToolCallResultMsg(t *testing.T) {
	t.Run("updates_tool_entry", func(t *testing.T) {
		m := newTestModel(t)
		m.toolEntries = append(m.toolEntries, ToolEntry{
			Name:   "read_file",
			Status: ToolStatusRunning,
		})
		m.toolsPending = 1

		updated, _ := m.Update(ToolCallResultMsg{
			Name:     "read_file",
			Result:   "file contents",
			IsError:  false,
			Duration: 42 * time.Millisecond,
		})
		m = updated.(*Model)

		if m.toolEntries[0].Status != ToolStatusDone {
			t.Errorf("expected ToolStatusDone, got %d", m.toolEntries[0].Status)
		}
		if m.toolEntries[0].Result != "file contents" {
			t.Errorf("expected 'file contents', got %q", m.toolEntries[0].Result)
		}
		if m.toolsPending != 0 {
			t.Errorf("toolsPending should be 0, got %d", m.toolsPending)
		}
	})

	t.Run("truncates_long_result", func(t *testing.T) {
		m := newTestModel(t)
		m.toolEntries = append(m.toolEntries, ToolEntry{
			Name:   "read_file",
			Status: ToolStatusRunning,
		})

		longResult := strings.Repeat("x", 2500)
		updated, _ := m.Update(ToolCallResultMsg{
			Name:   "read_file",
			Result: longResult,
		})
		m = updated.(*Model)

		if len(m.toolEntries[0].Result) != 2000 {
			t.Errorf("result should be truncated to 2000, got %d", len(m.toolEntries[0].Result))
		}
		if !strings.HasSuffix(m.toolEntries[0].Result, "...") {
			t.Error("truncated result should end with '...'")
		}
	})

	t.Run("error_status", func(t *testing.T) {
		m := newTestModel(t)
		m.toolEntries = append(m.toolEntries, ToolEntry{
			Name:   "exec",
			Status: ToolStatusRunning,
		})

		updated, _ := m.Update(ToolCallResultMsg{
			Name:    "exec",
			Result:  "command failed",
			IsError: true,
		})
		m = updated.(*Model)

		if m.toolEntries[0].Status != ToolStatusError {
			t.Errorf("expected ToolStatusError, got %d", m.toolEntries[0].Status)
		}
		if !m.toolEntries[0].IsError {
			t.Error("IsError should be true")
		}
	})

	t.Run("unmatched_result_does_not_change_pending_count", func(t *testing.T) {
		m := newTestModel(t)
		m.toolEntries = append(m.toolEntries, ToolEntry{
			ID: "active", Name: "read_file", Status: ToolStatusRunning,
		})
		m.toolsPending = 1

		updated, _ := m.Update(ToolCallResultMsg{
			ID: "stale", Name: "read_file", Result: "late duplicate",
		})
		m = updated.(*Model)

		if m.toolsPending != 1 {
			t.Fatalf("unmatched result changed toolsPending to %d", m.toolsPending)
		}
		if m.toolEntries[0].Status != ToolStatusRunning {
			t.Fatalf("unmatched result changed active entry status to %d", m.toolEntries[0].Status)
		}
	})
}

func TestToolCallResultsCorrelateDuplicateNamesByID(t *testing.T) {
	m := newTestModel(t)
	for _, id := range []string{"call-1", "call-2"} {
		updated, _ := m.Update(ToolCallStartMsg{ID: id, Name: "read", StartTime: time.Now()})
		m = updated.(*Model)
	}

	updated, _ := m.Update(ToolCallResultMsg{ID: "call-1", Name: "read", Result: "first"})
	m = updated.(*Model)
	updated, _ = m.Update(ToolCallResultMsg{ID: "call-2", Name: "read", Result: "second"})
	m = updated.(*Model)

	if m.toolEntries[0].Result != "first" || m.toolEntries[1].Result != "second" {
		t.Fatalf("tool results were swapped: %#v", m.toolEntries)
	}
	first := testProjectedToolCard(t, m, 0)
	second := testProjectedToolCard(t, m, 1)
	if first.Result != "first" || second.Result != "second" ||
		first.ID != "call-1" || second.ID != "call-2" {
		t.Fatalf("tool projections were swapped: %#v %#v", first, second)
	}
}

func TestAgentDoneMsg(t *testing.T) {
	m := newTestModel(t)
	setScrollableTranscript(m)
	m.transcriptGotoTop()
	m.state = StateStreaming
	m.pauseFollow()

	updated, _ := m.Update(AgentDoneMsg{})
	m = updated.(*Model)

	if m.state != StateIdle {
		t.Errorf("state should be StateIdle, got %d", m.state)
	}
	if !m.userScrolledUp || m.anchorActive {
		t.Error("completion should preserve an explicit paused-follow intent")
	}
}

func TestAgentDoneCancellationSettlesRunningToolsAsCancelled(t *testing.T) {
	m := newTestModel(t)
	started := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return started.Add(3 * time.Second) }
	m.state = StateStreaming
	m.turnToolStartIndex = 0
	m.toolsPending = 1
	outputDetail, err := m.outputDetails.Admit("stale output")
	if err != nil {
		t.Fatal(err)
	}
	m.toolEntries = []ToolEntry{{
		ID:                      "call-cancelled",
		Name:                    "read_file",
		Args:                    "path=README.md",
		RawArgs:                 map[string]any{"path": "README.md"},
		ResultLanguage:          "text",
		OutputDetail:            outputDetail,
		Status:                  ToolStatusRunning,
		StartTime:               started,
		BeforeContent:           "ephemeral",
		BeforeSnapshotAvailable: true,
		DiffLines:               []DiffLine{{Content: "+stale"}},
		Projection:              ecosystem.ProjectToolCall("read_file", nil),
	}}
	m.entries = append(m.entries, ChatEntry{Kind: "tool_group", ToolIndex: 0})

	updated, _ := m.Update(AgentDoneMsg{Err: context.Canceled})
	m = updated.(*Model)

	entry := m.toolEntries[0]
	if entry.Status != ToolStatusCancelled || entry.IsError {
		t.Fatalf("cancelled tool lifecycle = status %d error=%t", entry.Status, entry.IsError)
	}
	if entry.Duration != 3*time.Second || entry.Result != "Cancelled by user before completion" {
		t.Fatalf("cancelled tool receipt = duration %s result %q", entry.Duration, entry.Result)
	}
	if entry.RawArgs != nil || entry.BeforeContent != "" || entry.BeforeSnapshotAvailable {
		t.Fatalf("cancelled tool retained ephemeral input: %#v", entry)
	}
	if entry.ResultLanguage != "" || entry.OutputDetail != (OutputDetailReceipt{}) ||
		len(entry.DiffLines) != 0 {
		t.Fatalf("cancelled tool retained incompatible output state: %#v", entry)
	}
	if m.outputDetails.Available(outputDetail.Ref) {
		t.Fatal("cancelled tool retained its stale output capability")
	}
	projection := entry.Projection.Normalize()
	if projection.Transport != ecosystem.TransportFailed ||
		projection.Domain != ecosystem.DomainUnknown ||
		projection.Evidence != ecosystem.EvidenceNone {
		t.Fatalf("cancelled semantic projection = %#v", projection)
	}
	if m.toolsPending != 0 {
		t.Fatalf("cancelled turn retained %d pending tools", m.toolsPending)
	}
	card := testProjectedToolCard(t, m, 0)
	if card.Lifecycle != ToolLifecycleCancelled || card.State != ToolCardAttention {
		t.Fatalf("cancelled render model = lifecycle %d state %d", card.Lifecycle, card.State)
	}
	if rendered := ansi.Strip(card.View(80)); !strings.Contains(rendered, "Cancelled before completion") ||
		strings.Contains(rendered, "✗") {
		t.Fatalf("cancelled receipt was painted as a failure:\n%s", rendered)
	}

	updated, _ = m.Update(ToolCallResultMsg{
		ID: "call-cancelled", Name: "read_file", Result: "late success",
	})
	m = updated.(*Model)
	if m.toolEntries[0].Status != ToolStatusCancelled ||
		m.toolEntries[0].Result != "Cancelled by user before completion" {
		t.Fatalf("late result overwrote cancelled receipt: %#v", m.toolEntries[0])
	}
}

func TestAgentDoneFailureIsNotRenderedAsCompletedTurn(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.sessionTurnCount = 3
	m.footerNotice = &footerNotice{text: "✓ Done", severity: noticeSuccess}

	updated, _ := m.Update(AgentDoneMsg{Err: &agent.UnresolvedExecutionError{
		SessionID:   7,
		ExecutionID: "exec_test",
		ToolName:    "bash",
		EventType:   execution.EventOutcomeUnknown,
	}})
	m = updated.(*Model)

	if m.sessionTurnCount != 3 {
		t.Fatalf("failed turn count = %d, want 3", m.sessionTurnCount)
	}
	if m.footerNotice != nil {
		t.Fatal("failed turn retained the success notice")
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "Recovery paused · bash") ||
		!strings.Contains(m.entries[len(m.entries)-1].Content, "/recover") {
		t.Fatalf("failed turn receipt = %#v", m.entries)
	}
}

func TestAgentDoneCancelledSnapshotAdvancesCompletedEffectCursor(t *testing.T) {
	m, store, terminal := modelWithCompletedExecution(t)

	updated, _ := m.Update(AgentDoneMsg{Err: context.Canceled})
	m = updated.(*Model)
	if m.executionCursor != terminal.ID {
		t.Fatalf("cancelled settled cursor = %d, want %d", m.executionCursor, terminal.ID)
	}
	raw, err := store.GetSessionState(context.Background(), m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecutionCursor != terminal.ID {
		t.Fatalf("saved cursor = %d, want %d", state.ExecutionCursor, terminal.ID)
	}
}

func TestAgentDoneLaterErrorAdvancesProjectedCompletedEffectCursor(t *testing.T) {
	m, _, terminal := modelWithCompletedExecution(t)

	updated, _ := m.Update(AgentDoneMsg{Err: errors.New("later model failure")})
	m = updated.(*Model)
	if m.executionCursor != terminal.ID {
		t.Fatalf("settled error cursor = %d, want projected terminal %d", m.executionCursor, terminal.ID)
	}
}

func TestAgentDoneCompletedRecoveryHazardDoesNotAdvanceCursor(t *testing.T) {
	m, store, terminal := modelWithCompletedExecution(t)
	m.agent.ReplaceMessages(nil)
	m.toolEntries = []ToolEntry{{ID: terminal.Identity.CanonicalCallID, Name: terminal.Identity.ToolName, Status: ToolStatusDone}}

	updated, _ := m.Update(AgentDoneMsg{Err: &agent.UnresolvedExecutionError{
		ExecutionID: terminal.Identity.ExecutionID,
		ToolName:    terminal.Identity.ToolName,
		EventType:   execution.EventCompleted,
		Cause:       context.Canceled,
	}})
	m = updated.(*Model)
	if m.executionCursor != 0 {
		t.Fatalf("unprojected completed hazard advanced cursor to %d", m.executionCursor)
	}
	raw, err := store.GetSessionState(context.Background(), m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecutionCursor != 0 {
		t.Fatalf("saved unprojected cursor = %d", state.ExecutionCursor)
	}
	for _, entry := range m.entries {
		if strings.Contains(entry.Content, "Save session:") {
			t.Fatalf("expected recovery boundary was duplicated as a save failure: %#v", m.entries)
		}
	}
}

func TestAgentDoneSameCallIDWrongResultDoesNotAdvanceCursor(t *testing.T) {
	m, store, terminal := modelWithCompletedExecution(t)
	m.agent.ReplaceMessages([]llm.Message{{
		Role: "tool", ToolCallID: terminal.Identity.CanonicalCallID,
		ToolName: terminal.Identity.ToolName, Content: "wrong result",
	}})

	updated, _ := m.Update(AgentDoneMsg{Err: context.Canceled})
	m = updated.(*Model)
	if m.executionCursor != 0 {
		t.Fatalf("same-call wrong result advanced cursor to %d", m.executionCursor)
	}
	record, err := store.GetSessionStateRecord(context.Background(), m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(record.StateJSON)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecutionCursor != 0 {
		t.Fatalf("saved wrong-result cursor = %d", state.ExecutionCursor)
	}
}

func TestInteractiveSessionCASRejectsStaleWriterWithoutRetry(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "interactive-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "interactive CAS", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	client := &standaloneRecoveryClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.model = client.Model()
	m.agent.SetWorkDir(workspace)
	m.SetSessionStore(store)
	m.sessionID = session.ID
	m.executionLease = lease
	if err := m.initializeSessionStateRevision(0); err != nil {
		t.Fatal(err)
	}
	m.agent.AddUserMessage("already durable")
	if err := m.persistSessionState(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.sessionStateMu.RLock()
	staleRevision := m.sessionStateRevision
	m.sessionStateMu.RUnlock()
	external, err := store.SaveSessionStateCAS(
		context.Background(), session.ID, staleRevision, `{"writer":"external coordinator"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantPromptFloor := agent.ContextPromptFloor{
		Tokens: 12_000, HostTokens: 1_000, MessageTokens: 200, Model: client.Model(),
	}
	if err := m.agent.RestoreContextPromptFloor(wantPromptFloor); err != nil {
		t.Fatal(err)
	}

	if cmd := m.sendToAgent("do not dispatch"); cmd != nil {
		t.Fatal("stale interactive writer returned a provider command")
	}
	m.sessionStateMu.RLock()
	known, dirty, retainedRevision := m.sessionStateRevisionKnown, m.sessionStatePersistenceDirty, m.sessionStateRevision
	m.sessionStateMu.RUnlock()
	if known || !dirty || retainedRevision != staleRevision {
		t.Fatalf("stale CAS state = known %v dirty %v revision %d", known, dirty, retainedRevision)
	}
	messages := m.agent.Messages()
	if len(messages) != 1 || messages[0].Content != "already durable" {
		t.Fatalf("stale save changed agent history: %#v", messages)
	}
	if got := m.agent.ContextPromptFloor(); got != wantPromptFloor {
		t.Fatalf("stale save rollback lost context prompt floor: got %#v, want %#v", got, wantPromptFloor)
	}
	if m.input.Value() != "do not dispatch" || m.state != StateIdle {
		t.Fatalf("stale save lost retry draft: state=%v draft=%q", m.state, m.input.Value())
	}
	if err := m.persistSessionState(context.Background()); !errors.Is(err, ErrSessionStateRevisionUnknown) {
		t.Fatalf("second stale save error = %v", err)
	}
	stored, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != external.Revision || stored.StateJSON != `{"writer":"external coordinator"}` {
		t.Fatalf("stale interactive save overwrote external state: revision=%d state=%s", stored.Revision, stored.StateJSON)
	}
}

func TestExecutionSessionProjectsAndClearsGeneratedIdentity(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "session-identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	m.SetSessionStore(store)

	created, err := m.ensureExecutionSession("\nPolish the composer\nwith more detail", "AUTO")
	if err != nil || !created {
		t.Fatalf("ensureExecutionSession() = created %v, error %v", created, err)
	}
	if m.sessionID <= 0 || m.activeSessionTitle != "Polish the composer" {
		t.Fatalf("active session identity = id %d title %q", m.sessionID, m.activeSessionTitle)
	}
	durableID := m.sessionID
	durable, err := store.GetSession(context.Background(), durableID)
	if err != nil || durable.Title != m.activeSessionTitle {
		t.Fatalf("durable identity = %#v, error %v", durable, err)
	}

	m.resetConversationSession()
	if m.sessionID != 0 || m.activeSessionTitle != "" {
		t.Fatalf("reset retained active identity = id %d title %q", m.sessionID, m.activeSessionTitle)
	}
	if _, err := store.GetSession(context.Background(), durableID); err != nil {
		t.Fatalf("conversation reset deleted saved session: %v", err)
	}
}

func modelWithCompletedExecution(t *testing.T) (*Model, *db.Store, execution.Event) {
	t.Helper()
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "execution.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace, err := canonicalWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{Title: "execution", WorkspaceID: workspace})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	m.agent.SetExecutionLedger(store)
	m.agent.SetExecutionSessionID(session.ID, "")
	m.SetSessionStore(store)
	m.sessionID = session.ID
	m.executionLease = lease
	if err := m.initializeSessionStateRevision(0); err != nil {
		t.Fatal(err)
	}
	identity := execution.Identity{
		SessionID: session.ID, WorkspaceID: workspace, RunID: "run_ui", TurnID: "turn_ui",
		ExecutionID: "exec_ui", IdempotencyKey: "idem_ui", CanonicalCallID: "call_ui",
		ToolName: "write", Iteration: 1, Ordinal: 1, Kind: execution.KindBuiltin, EffectClass: execution.Effectful,
	}
	base := execution.Event{
		Identity: identity, Type: execution.EventRequested, Approval: execution.ApprovalNotApplicable,
		ArgumentsSHA256: execution.HashText("arguments"),
	}
	for _, event := range []execution.Event{
		base,
		func() execution.Event {
			e := base
			e.Type = execution.EventApproved
			e.Approval = execution.ApprovalEmbedding
			return e
		}(),
		func() execution.Event { e := base; e.Type = execution.EventStarted; return e }(),
	} {
		if _, _, err := store.AppendExecutionEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	terminal := base
	terminal.Type = execution.EventCompleted
	terminal.ResultSHA256 = execution.HashText("done")
	stored, _, err := store.AppendExecutionEvent(context.Background(), terminal)
	if err != nil {
		t.Fatal(err)
	}
	m.agent.AppendMessage(llm.Message{Role: "tool", ToolCallID: identity.CanonicalCallID, ToolName: identity.ToolName, Content: "done"})
	return m, store, stored
}

func TestSendToAgentFailsClosedWhenSessionCannotBeCreated(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.SetSessionStore(store)

	if cmd := m.sendToAgent("keep my draft"); cmd != nil {
		t.Fatal("session creation failure still returned an agent command")
	}
	if m.state != StateIdle || m.sessionID != 0 || m.input.Value() != "keep my draft" {
		t.Fatalf("failed send state = state %v session %d draft %q", m.state, m.sessionID, m.input.Value())
	}
	if len(m.agent.Messages()) != 0 {
		t.Fatalf("failed send reached agent history: %#v", m.agent.Messages())
	}
}

func TestSendToAgentRevertsHistoryWhenPreturnSnapshotFails(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{Title: "existing", WorkspaceID: workspace})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, session.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.agent.SetWorkDir(workspace)
	m.SetSessionStore(store)
	m.sessionID = session.ID
	m.executionLease = lease
	if err := m.initializeSessionStateRevision(0); err != nil {
		t.Fatal(err)
	}
	m.agent.AddUserMessage("already durable")

	if cmd := m.sendToAgent("do not dispatch"); cmd != nil {
		t.Fatal("snapshot failure still returned an agent command")
	}
	messages := m.agent.Messages()
	if len(messages) != 1 || messages[0].Content != "already durable" {
		t.Fatalf("snapshot failure changed agent history: %#v", messages)
	}
	if m.input.Value() != "do not dispatch" || m.state != StateIdle {
		t.Fatalf("snapshot failure lost retry draft: state %v draft %q", m.state, m.input.Value())
	}
}

func TestResetConversationReleasesExecutionSessionLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.db")
	first, err := db.OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := db.OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	workspace, err := canonicalWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := first.CreateSession(context.Background(), db.CreateSessionParams{Title: "lease", WorkspaceID: workspace})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := first.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.sessionID = session.ID
	m.executionLease = lease
	if err := m.initializeSessionStateRevision(7); err != nil {
		t.Fatal(err)
	}
	m.sessionStateMu.Lock()
	m.sessionStatePersistenceDirty = true
	m.sessionStateMu.Unlock()

	m.resetConversationSession()
	if m.executionLease != nil || m.sessionID != 0 {
		t.Fatalf("reset retained session ownership: session=%d lease=%v", m.sessionID, m.executionLease)
	}
	m.sessionStateMu.RLock()
	revision, known, dirty := m.sessionStateRevision, m.sessionStateRevisionKnown, m.sessionStatePersistenceDirty
	m.sessionStateMu.RUnlock()
	if revision != 0 || known || dirty {
		t.Fatalf("reset retained revision authority: revision=%d known=%v dirty=%v", revision, known, dirty)
	}
	reacquired, err := second.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
	if err != nil {
		t.Fatalf("reset did not release cross-process lease: %v", err)
	}
	_ = reacquired.Close()
}

func TestShutdownWaitsForActiveTurnBeforeQuit(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.state = StateStreaming
	updated, cmd := m.Update(ShutdownMsg{})
	m = updated.(*Model)
	if cmd == nil || m.shutdownReady() {
		t.Fatal("shutdown did not remain active with liveness while the turn joins")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("shutdown did not cancel active turn")
	}
	updated, cmd = m.Update(AgentDoneMsg{Err: context.Canceled})
	m = updated.(*Model)
	if cmd == nil || !m.shuttingDown {
		t.Fatal("shutdown did not quit after active turn completed")
	}
}

func TestInitCompleteMsg(t *testing.T) {
	t.Run("reflows_startup_footer", func(t *testing.T) {
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m = updated.(*Model)
		m.initializing = true
		m.recalcViewportHeight()

		updated, _ = m.Update(InitCompleteMsg{Model: "llama3", NumCtx: 8192})
		m = updated.(*Model)
		if got, want := m.viewport.Height(), m.viewportHeight(); got != want {
			t.Fatalf("settled viewport height = %d, want recalculated %d", got, want)
		}
	})

	t.Run("basic_fields", func(t *testing.T) {
		m := newTestModel(t)

		updated, _ := m.Update(InitCompleteMsg{
			Model:        "llama3",
			ModelList:    []string{"llama3", "qwen3"},
			AgentProfile: "default",
			AgentList:    []string{"default", "coder"},
			ToolCount:    5,
			ServerCount:  2,
			NumCtx:       8192,
		})
		m = updated.(*Model)

		if m.model != "llama3" {
			t.Errorf("model should be 'llama3', got %q", m.model)
		}
		if len(m.modelList) != 2 {
			t.Errorf("modelList should have 2 items, got %d", len(m.modelList))
		}
		if m.toolCount != 5 {
			t.Errorf("toolCount should be 5, got %d", m.toolCount)
		}
		if m.serverCount != 2 {
			t.Errorf("serverCount should be 2, got %d", m.serverCount)
		}
	})

	t.Run("with_failed_servers", func(t *testing.T) {
		m := newTestModel(t)
		before := len(m.entries)

		updated, _ := m.Update(InitCompleteMsg{
			Model: "llama3",
			FailedServers: []FailedServer{
				{Name: "server1", Reason: "timeout"},
			},
		})
		m = updated.(*Model)

		if len(m.entries) != before+1 {
			t.Fatalf("should append system entry for failed servers, got %d entries", len(m.entries))
		}
		last := m.entries[len(m.entries)-1]
		if last.Kind != "system" {
			t.Errorf("expected kind 'system', got %q", last.Kind)
		}
		if !strings.Contains(last.Content, "server1") {
			t.Errorf("should contain server name, got %q", last.Content)
		}
	})
}

func TestHandleCommandAction(t *testing.T) {
	tests := []struct {
		name   string
		result command.Result
		check  func(t *testing.T, m *Model, cmd tea.Cmd)
	}{
		{
			name:   "ActionShowHelp",
			result: command.Result{Action: command.ActionShowHelp},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				if m.overlay != OverlayHelp {
					t.Errorf("expected OverlayHelp, got %d", m.overlay)
				}
			},
		},
		{
			name:   "ActionClear_with_text",
			result: command.Result{Action: command.ActionClear, Text: "Cleared."},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				// entries should be cleared except for the new system message
				if len(m.entries) != 1 {
					t.Errorf("expected 1 entry, got %d", len(m.entries))
				}
				if m.entries[0].Kind != "system" {
					t.Errorf("expected system entry, got %q", m.entries[0].Kind)
				}
				if m.capabilityRoute != nil || m.lastCapabilityRoute != nil || m.promptTokens != 0 || m.footerNotice != nil {
					t.Fatalf("clear retained prior-turn diagnostics: active=%#v last=%#v prompt=%d notice=%#v",
						m.capabilityRoute, m.lastCapabilityRoute, m.promptTokens, m.footerNotice)
				}
			},
		},
		{
			name:   "ActionQuit",
			result: command.Result{Action: command.ActionQuit},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				if cmd == nil {
					t.Error("ActionQuit should return a cmd (tea.Quit)")
				}
			},
		},
		{
			name:   "ActionLoadContext",
			result: command.Result{Action: command.ActionLoadContext, Data: "test.md", Text: "Loading."},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				if cmd == nil || !m.fileLoading {
					t.Error("load context should start a tokened asynchronous read")
				}
				if m.loadedFile != "" {
					t.Errorf("context changed before async read completed: %q", m.loadedFile)
				}
			},
		},
		{
			name:   "ActionUnloadContext",
			result: command.Result{Action: command.ActionUnloadContext, Text: "Unloaded."},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				if m.loadedFile != "" {
					t.Errorf("expected empty loadedFile, got %q", m.loadedFile)
				}
			},
		},
		{
			name:   "ActionSwitchModel",
			result: command.Result{Action: command.ActionSwitchModel, Data: "gpt-4", Text: "Switched."},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				if m.model != "gpt-4" {
					t.Errorf("expected model='gpt-4', got %q", m.model)
				}
			},
		},
		{
			name:   "ActionSwitchAgent",
			result: command.Result{Action: command.ActionSwitchAgent, Data: "coder", Text: "Switched."},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				if m.agentProfile != "coder" {
					t.Errorf("expected agentProfile='coder', got %q", m.agentProfile)
				}
			},
		},
		{
			name:   "ActionNone_with_text",
			result: command.Result{Action: command.ActionNone, Text: "Info message"},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				if len(m.entries) == 0 {
					t.Fatal("expected at least one entry")
				}
				last := m.entries[len(m.entries)-1]
				if last.Content != "Info message" {
					t.Errorf("expected 'Info message', got %q", last.Content)
				}
			},
		},
		{
			name:   "ActionNone_empty_text",
			result: command.Result{Action: command.ActionNone, Text: ""},
			check: func(t *testing.T, m *Model, cmd tea.Cmd) {
				// Should not add any entry.
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t)
			if tt.result.Action == command.ActionClear {
				route := agent.CapabilityRoute{Phase: "research", Status: agent.CapabilityRouteResolved, Server: "hitspec", Tool: "hitspec_capture_webpage"}
				m.capabilityRoute = &route
				m.lastCapabilityRoute = &route
				m.promptTokens = 4_096
				m.footerNotice = &footerNotice{text: "✓ Done", severity: noticeSuccess}
			}
			// Pre-populate loadedFile for unload test.
			if tt.result.Action == command.ActionUnloadContext {
				m.loadedFile = "old.md"
			}
			if tt.result.Action == command.ActionSwitchAgent {
				m.SetAgentProfileSource(&config.AgentsDir{
					Agents: map[string]config.AgentProfile{
						"coder": {Name: "coder"},
					},
				}, "", "")
			}
			cmd := m.handleCommandAction(tt.result)
			tt.check(t, m, cmd)
		})
	}
}

func TestCommandResultMsg(t *testing.T) {
	t.Run("with_text", func(t *testing.T) {
		m := newTestModel(t)
		before := len(m.entries)

		updated, _ := m.Update(CommandResultMsg{Text: "Result info"})
		m = updated.(*Model)

		if len(m.entries) != before+1 {
			t.Fatalf("expected %d entries, got %d", before+1, len(m.entries))
		}
		if m.entries[len(m.entries)-1].Content != "Result info" {
			t.Errorf("expected 'Result info', got %q", m.entries[len(m.entries)-1].Content)
		}
	})

	t.Run("empty_text_no_entry", func(t *testing.T) {
		m := newTestModel(t)
		before := len(m.entries)

		updated, _ := m.Update(CommandResultMsg{Text: ""})
		m = updated.(*Model)

		if len(m.entries) != before {
			t.Errorf("expected %d entries (no change), got %d", before, len(m.entries))
		}
	})
}

func TestHandleCommandActionRejectsUnknownAction(t *testing.T) {
	m := newTestModel(t)
	m.handleCommandAction(command.Result{Action: command.Action(10_000), Text: "must not be shown"})
	if len(m.entries) == 0 {
		t.Fatal("unknown action was silently ignored")
	}
	last := m.entries[len(m.entries)-1]
	if last.Kind != "error" || !strings.Contains(last.Content, "unsupported command action") {
		t.Fatalf("unknown action entry = %#v", last)
	}
	if strings.Contains(last.Content, "must not be shown") {
		t.Fatalf("unknown action rendered success text: %#v", last)
	}
}
