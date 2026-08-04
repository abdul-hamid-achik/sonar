package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

func TestSerializeDeserialize_Roundtrip(t *testing.T) {
	entries := []ChatEntry{
		{Kind: "user", Content: "Hello there"},
		{Kind: "assistant", Content: "Hi! How can I help?"},
		{Kind: "system", Content: "Model switched to qwen3"},
	}

	serialized := serializeEntries(entries)
	deserialized := deserializeEntries(serialized)

	if len(deserialized) != len(entries) {
		t.Fatalf("roundtrip length: got %d, want %d", len(deserialized), len(entries))
	}

	for i, e := range deserialized {
		if e.Kind != entries[i].Kind {
			t.Errorf("entry[%d] kind: got %q, want %q", i, e.Kind, entries[i].Kind)
		}
		if e.Content != entries[i].Content {
			t.Errorf("entry[%d] content: got %q, want %q", i, e.Content, entries[i].Content)
		}
	}
}

func TestSerializeEntries_Empty(t *testing.T) {
	result := serializeEntries(nil)
	if result != "" {
		t.Errorf("nil entries should serialize to empty, got %q", result)
	}
}

func TestDeserializeEntries_Empty(t *testing.T) {
	result := deserializeEntries("")
	if result != nil {
		t.Errorf("empty content should deserialize to nil, got %v", result)
	}
}

func TestDeserializeEntries_UnknownHeader(t *testing.T) {
	content := "## Unknown\n\nSome content\n\n## User\n\nValid content"
	result := deserializeEntries(content)
	if len(result) != 1 {
		t.Fatalf("should skip unknown headers, got %d entries", len(result))
	}
	if result[0].Kind != "user" {
		t.Errorf("should parse valid entry, got kind %q", result[0].Kind)
	}
}

func TestSerializeEntries_ErrorKind(t *testing.T) {
	entries := []ChatEntry{
		{Kind: "error", Content: "Something went wrong"},
	}
	serialized := serializeEntries(entries)
	if serialized == "" {
		t.Error("error entries should serialize")
	}

	deserialized := deserializeEntries(serialized)
	if len(deserialized) != 1 || deserialized[0].Kind != "error" {
		t.Errorf("error entry should roundtrip, got %v", deserialized)
	}
}

func TestSerializeEntries_MultilineContent(t *testing.T) {
	entries := []ChatEntry{
		{Kind: "user", Content: "line1\nline2\nline3"},
	}
	serialized := serializeEntries(entries)
	deserialized := deserializeEntries(serialized)
	if len(deserialized) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(deserialized))
	}
	if deserialized[0].Content != "line1\nline2\nline3" {
		t.Errorf("multiline content should roundtrip, got %q", deserialized[0].Content)
	}
}

func TestSerializeEntriesStripsBidirectionalControlsFromImageNames(t *testing.T) {
	entries := []ChatEntry{{
		Kind: "user", Content: "inspect",
		Attachments: []imageasset.Ref{{
			Digest: strings.Repeat("a", 64), MIMEType: "image/png", Name: "screen\u202egnp.png",
			SizeBytes: 1, Width: 1, Height: 1,
		}},
	}}
	serialized := serializeEntries(entries)
	if strings.ContainsRune(serialized, '\u202e') || !strings.Contains(serialized, "screengnp.png") {
		t.Fatalf("serialized session retained visual-order control: %q", serialized)
	}
}

func TestSessionTitleIsBounded(t *testing.T) {
	got := sessionTitle(strings.Repeat("x", 100))
	if len([]rune(got)) != 72 || !strings.HasSuffix(got, "...") {
		t.Fatalf("session title = %q (%d runes)", got, len([]rune(got)))
	}
}

func TestSessionTitleSanitizesPromptControls(t *testing.T) {
	got := sessionTitle("safe\x1b]8;;https://example.invalid\x07link\x1b]8;;\x07\u202eevil")
	if got != "safelinkevil" {
		t.Fatalf("session title = %q, want %q", got, "safelinkevil")
	}
}

func TestSessionTitleUsesGuidedPlanTask(t *testing.T) {
	prompt := "Plan the following task:\n\nTask: Improve composer wrapping and paste UX\nConstraints: preserve session data"
	if got, want := sessionTitle(prompt), "Improve composer wrapping and paste UX"; got != want {
		t.Fatalf("guided plan session title = %q, want %q", got, want)
	}
}

func TestSessionTitleUsesFirstNonEmptyPromptLine(t *testing.T) {
	if got, want := sessionTitle("\n  \nInvestigate the session footer\nmore context"), "Investigate the session footer"; got != want {
		t.Fatalf("session title = %q, want %q", got, want)
	}
}

func TestSessionDisplayLabelKeepsStableHandle(t *testing.T) {
	if got, want := sessionDisplayLabel("abcdef0", "Composer polish", 72), "abcdef0 · Composer polish"; got != want {
		t.Fatalf("sessionDisplayLabel() = %q, want %q", got, want)
	}
	if got, want := sessionDisplayLabel("abcdef0", "Composer polish", 0), "abcdef0"; got != want {
		t.Fatalf("compact sessionDisplayLabel() = %q, want %q", got, want)
	}
}

func TestSessionResumeInfoRequiresDurableSession(t *testing.T) {
	m := newTestModel(t)
	m.sessionID = 42
	if info, ok := m.SessionResumeInfo(); ok || info.Handle != "" {
		t.Fatalf("session without durable store returned resume info %#v", info)
	}

	store, err := db.OpenPath(filepath.Join(t.TempDir(), "resume-info.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m.sessionStore = store
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "resume info", WorkspaceID: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	m.sessionID = session.ID
	m.sessionPublicID = session.PublicID
	m.activeSessionTitle = "Resume\ninfo\x1b]0;owned\x07"
	if info, ok := m.SessionResumeInfo(); !ok || info.Handle != sessionref.Format(session.PublicID) || info.Title != "Resume info" {
		t.Fatalf("durable session resume info = %#v, ok=%v", info, ok)
	}

	m.sessionID = 0
	if info, ok := m.SessionResumeInfo(); ok || info.Handle != "" {
		t.Fatalf("zero session returned resume info %#v", info)
	}
}

func TestLosslessSessionStateRestoresAgentHistory(t *testing.T) {
	source := newTestModel(t)
	source.modelPinned = true
	source.entries = []ChatEntry{{Kind: "user", Content: "inspect"}, {Kind: "assistant", Content: "done"}}
	source.agent.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "inspect"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: "tool", Content: "contents", ToolName: "read", ToolCallID: "call-1"},
		{Role: "assistant", Content: "done"},
	})

	raw, err := encodeSessionState(source)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	target := newTestModel(t)
	if err := target.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	got := target.agent.Messages()
	if len(got) != 4 || got[1].ToolCalls[0].ID != got[2].ToolCallID {
		t.Fatalf("restored agent history is incomplete: %#v", got)
	}
	if !target.modelPinned {
		t.Fatal("saved model pin state was not restored")
	}
}

func TestContextPromptFloorInteractivePersistenceRoundTrip(t *testing.T) {
	floor := agent.ContextPromptFloor{
		Tokens:        2_950,
		HostTokens:    725,
		MessageTokens: 2_025,
		Model:         "test-model",
	}
	messages := []llm.Message{
		{Role: "user", Content: "inspect the release"},
		{Role: "assistant", Content: "the release is ready"},
	}

	source := newTestModel(t)
	source.agent = agent.New(&importCaptureClient{}, nil, 4_096)
	t.Cleanup(source.agent.Close)
	source.model = floor.Model
	source.agent.ReplaceMessages(messages)
	if err := source.agent.RestoreContextPromptFloor(floor); err != nil {
		t.Fatalf("install source context prompt floor: %v", err)
	}
	source.entries = []ChatEntry{
		{Kind: "user", Content: messages[0].Content},
		{Kind: "assistant", Content: messages[1].Content},
	}

	raw, err := encodeSessionState(source)
	if err != nil {
		t.Fatalf("encode session: %v", err)
	}
	decoded, err := decodeSessionState(raw)
	if err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if decoded.ContextPromptFloor != floor {
		t.Fatalf("decoded context prompt floor = %#v, want %#v", decoded.ContextPromptFloor, floor)
	}

	target := newTestModel(t)
	target.agent = agent.New(&importCaptureClient{}, nil, 4_096)
	t.Cleanup(target.agent.Close)
	target.model = floor.Model
	if err := target.restoreSessionState(decoded); err != nil {
		t.Fatalf("restore session: %v", err)
	}
	if got := target.agent.ContextPromptFloor(); got != floor {
		t.Fatalf("restored context prompt floor = %#v, want %#v", got, floor)
	}
	if got := target.agent.Messages(); len(got) != len(messages) || got[1].Content != messages[1].Content {
		t.Fatalf("restored messages = %#v", got)
	}
}

func TestContextPromptFloorLegacySnapshotWithoutFieldRemainsResumable(t *testing.T) {
	raw := `{"version":1,"messages":[{"role":"user","content":"continue"}],"entries":[{"kind":"user","content":"continue"}],"mode":2,"model":"test-model"}`
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatalf("decode legacy session: %v", err)
	}
	if state.ContextPromptFloor != (agent.ContextPromptFloor{}) {
		t.Fatalf("legacy session synthesized context prompt floor %#v", state.ContextPromptFloor)
	}

	target := newTestModel(t)
	target.agent = agent.New(&importCaptureClient{}, nil, 4_096)
	t.Cleanup(target.agent.Close)
	target.model = "test-model"
	if err := target.restoreSessionState(state); err != nil {
		t.Fatalf("restore legacy session: %v", err)
	}
	if got := target.agent.ContextPromptFloor(); got != (agent.ContextPromptFloor{}) {
		t.Fatalf("restored legacy context prompt floor = %#v", got)
	}
}

func TestContextPromptFloorPersistenceRejectsMismatchedModel(t *testing.T) {
	floor := agent.ContextPromptFloor{
		Tokens: 2_950, HostTokens: 725, MessageTokens: 2_025, Model: "other-model",
	}
	if _, err := marshalPersistedSessionState(persistedSessionState{
		Version:            currentPersistedSessionVersion,
		Mode:               ModeNormal,
		Model:              "test-model",
		ContextPromptFloor: floor,
	}); err == nil || !strings.Contains(err.Error(), "model does not match") {
		t.Fatalf("mismatched model encode error = %v", err)
	}

	raw := `{"version":2,"messages":[],"entries":[],"mode":2,"model":"test-model","context_prompt_floor":{"tokens":2950,"host_tokens":725,"message_tokens":2025,"model":"other-model"}}`
	if _, err := decodeSessionState(raw); err == nil || !strings.Contains(err.Error(), "model does not match") {
		t.Fatalf("mismatched model decode error = %v", err)
	}
}

func TestContextPromptFloorPersistenceRejectsPartialAndUnboundedNumbers(t *testing.T) {
	tooLarge := agent.MaxContextPromptFloorTokens + 1
	tests := []struct {
		name  string
		floor string
	}{
		{name: "tokens without model", floor: `{"tokens":2950,"host_tokens":725,"message_tokens":2025}`},
		{name: "host tokens without total", floor: `{"host_tokens":725,"model":"test-model"}`},
		{name: "message tokens without total", floor: `{"message_tokens":2025,"model":"test-model"}`},
		{name: "negative host tokens", floor: `{"tokens":2950,"host_tokens":-1,"message_tokens":2025,"model":"test-model"}`},
		{name: "oversized total", floor: fmt.Sprintf(`{"tokens":%d,"host_tokens":1,"message_tokens":1,"model":"test-model"}`, tooLarge)},
		{name: "oversized host", floor: fmt.Sprintf(`{"tokens":2950,"host_tokens":%d,"message_tokens":1,"model":"test-model"}`, tooLarge)},
		{name: "oversized message", floor: fmt.Sprintf(`{"tokens":2950,"host_tokens":1,"message_tokens":%d,"model":"test-model"}`, tooLarge)},
		{name: "fractional total", floor: `{"tokens":1.5,"host_tokens":1,"message_tokens":1,"model":"test-model"}`},
		{name: "integer overflow", floor: `{"tokens":999999999999999999999999999999,"host_tokens":1,"message_tokens":1,"model":"test-model"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"version":2,"messages":[],"entries":[],"mode":2,"model":"test-model","context_prompt_floor":` + test.floor + `}`
			if _, err := decodeSessionState(raw); err == nil {
				t.Fatalf("decode accepted invalid context prompt floor %s", test.floor)
			}
		})
	}
}

func TestHeadlessSessionTerminalAndErrorSnapshotsPreserveContextPromptFloor(t *testing.T) {
	floor := agent.ContextPromptFloor{
		Tokens: 2_950, HostTokens: 725, MessageTokens: 2_025, Model: "test-model",
	}
	tests := []struct {
		name     string
		messages []llm.Message
	}{
		{
			name: "terminal answer",
			messages: []llm.Message{
				{Role: "user", Content: "inspect the release"},
				{Role: "assistant", Content: "the release is ready"},
			},
		},
		{
			name:     "provider error after dispatch",
			messages: []llm.Message{{Role: "user", Content: "inspect the release"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := EncodeHeadlessSessionStateWithContextFloor(test.messages, floor.Model, "", true, 0, floor)
			if err != nil {
				t.Fatalf("encode headless session: %v", err)
			}
			state, err := decodeSessionState(raw)
			if err != nil {
				t.Fatalf("decode headless session: %v", err)
			}
			if state.ContextPromptFloor != floor {
				t.Fatalf("saved context prompt floor = %#v, want %#v", state.ContextPromptFloor, floor)
			}
			if len(state.Messages) != len(test.messages) {
				t.Fatalf("saved messages = %#v, want %d", state.Messages, len(test.messages))
			}
		})
	}
}

func TestRestoreSessionClearsPreviousTurnDiagnostics(t *testing.T) {
	m := newTestModel(t)
	route := agent.CapabilityRoute{
		Phase: "research", Status: agent.CapabilityRouteResolved,
		Server: "hitspec", Tool: "hitspec_capture_webpage",
	}
	m.capabilityRoute = &route
	m.lastCapabilityRoute = &route
	m.promptTokens = 4_096
	m.evalCount = 256
	m.turnPromptTotal = 4_096
	m.turnEvalTotal = 256
	m.footerNotice = &footerNotice{text: "✓ Done", severity: noticeSuccess}
	m.lastTurnDuration = 3 * time.Second

	if err := m.restoreSessionState(persistedSessionState{
		Version: currentPersistedSessionVersion,
		Mode:    ModeNormal,
	}); err != nil {
		t.Fatal(err)
	}

	if m.capabilityRoute != nil || m.lastCapabilityRoute != nil {
		t.Fatalf("restore retained contextual route: active=%#v last=%#v", m.capabilityRoute, m.lastCapabilityRoute)
	}
	if m.promptTokens != 0 || m.evalCount != 0 || m.turnPromptTotal != 0 || m.turnEvalTotal != 0 {
		t.Fatalf("restore retained token diagnostics: prompt=%d eval=%d turn_prompt=%d turn_eval=%d",
			m.promptTokens, m.evalCount, m.turnPromptTotal, m.turnEvalTotal)
	}
	if m.footerNotice != nil || m.lastTurnDuration != 0 {
		t.Fatalf("restore retained completion receipt: notice=%#v duration=%s", m.footerNotice, m.lastTurnDuration)
	}
}

func TestEncodeHeadlessSessionStateIsResumable(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: "user", Content: "inspect the tree"},
		{Role: "assistant", Content: "I will inspect it", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "ls"}}},
		{Role: "tool", Content: "README.md", ToolName: "ls", ToolCallID: "call-1"},
		{Role: "assistant", Content: "The repository has a README."},
	}
	raw, err := EncodeHeadlessSessionState(messages, "qwen3.5:4b", "reviewer", true, 42)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != currentPersistedSessionVersion || state.Mode != ModeNormal || state.Model != "qwen3.5:4b" || !state.ModelPinned || state.AgentProfile != "reviewer" || state.ExecutionCursor != 42 {
		t.Fatalf("headless metadata = mode %v model %q pinned %v profile %q cursor %d", state.Mode, state.Model, state.ModelPinned, state.AgentProfile, state.ExecutionCursor)
	}
	if len(state.Messages) != len(messages) || state.Messages[2].Role != "tool" {
		t.Fatalf("headless messages = %#v", state.Messages)
	}
	if len(state.Entries) != 3 {
		t.Fatalf("visible headless entries = %#v, want user and assistant text only", state.Entries)
	}
	for _, entry := range state.Entries {
		if entry.Kind == "tool" {
			t.Fatalf("tool message leaked into visible transcript: %#v", state.Entries)
		}
	}
}

func TestEncodeHeadlessGoalSessionStateIsResumable(t *testing.T) {
	t.Parallel()

	runtime := newUIGoalRuntime(t, 73, goal.BudgetLimits{MaxContinuationTurns: 3, MaxEvalTokens: 1200})
	snapshot := snapshotUIGoal(t, runtime)
	messages := []llm.Message{{Role: "user", Content: "finish the goal"}, {Role: "assistant", Content: "working"}}
	raw, err := EncodeHeadlessGoalSessionState(messages, "qwen3.5:4b", "builder", true, 19, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != currentPersistedSessionVersion || state.Mode != ModeAuto || state.ExecutionCursor != 19 {
		t.Fatalf("goal metadata = version %d mode %v cursor %d", state.Version, state.Mode, state.ExecutionCursor)
	}
	if state.Goal == nil || state.Goal.SessionID != 73 || state.Goal.ID != snapshot.ID {
		t.Fatalf("goal snapshot = %#v", state.Goal)
	}
	if len(state.Messages) != 2 || state.Messages[1].Content != "working" {
		t.Fatalf("goal messages = %#v", state.Messages)
	}
}

func TestPersistedModeMigrationSeparatesLegacyBuildFromAuto(t *testing.T) {
	tests := []struct {
		name string
		mode Mode
		goal bool
		want Mode
	}{
		{name: "ask becomes normal", mode: ModeAsk, want: ModeNormal},
		{name: "plan stays plan", mode: ModePlan, want: ModePlan},
		{name: "interactive build becomes normal", mode: ModeBuild, want: ModeNormal},
		{name: "build carrying durable goal becomes auto", mode: ModeBuild, goal: true, want: ModeAuto},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := persistedSessionState{Version: 1, Mode: test.mode}
			if test.goal {
				runtime := newUIGoalRuntime(t, 42, goal.BudgetLimits{})
				snapshot := snapshotUIGoal(t, runtime)
				state.Goal = &snapshot
			}
			migrated, err := migratePersistedSessionState(state)
			if err != nil {
				t.Fatal(err)
			}
			if migrated.Version != currentPersistedSessionVersion || migrated.Mode != test.want {
				t.Fatalf("migrated state = version %d mode %d, want version %d mode %d", migrated.Version, migrated.Mode, currentPersistedSessionVersion, test.want)
			}
		})
	}

	if _, err := migratePersistedSessionState(persistedSessionState{Version: 99}); err == nil {
		t.Fatal("unsupported session version was accepted")
	}
}

func TestUnresolvedExecutionWarningOnlyBlocksStartedEffects(t *testing.T) {
	t.Parallel()

	states := []execution.State{
		{
			Identity: execution.Identity{ToolName: "read", EffectClass: execution.EffectReadOnly},
			Latest:   execution.Event{Type: execution.EventStarted},
		},
		{
			Identity: execution.Identity{ToolName: "bash", EffectClass: execution.EffectUnknown},
			Latest:   execution.Event{Type: execution.EventStarted},
		},
	}
	warning := unresolvedExecutionWarning(states, false)
	if !strings.Contains(warning, "bash") || !strings.Contains(warning, "outcome is unknown") || !strings.Contains(warning, "/recover") {
		t.Fatalf("unresolvedExecutionWarning() = %q", warning)
	}
	states[1].Latest.Type = execution.EventOutcomeUnknown
	warning = unresolvedExecutionWarning(states, false)
	if !strings.Contains(warning, "bash") || !strings.Contains(warning, "outcome-unknown receipt") || !strings.Contains(warning, "/recover") {
		t.Fatalf("outcome-unknown warning = %q", warning)
	}
	states[1].Latest.Type = execution.EventCompleted
	states[1].Identity.EffectClass = execution.EffectReadOnly
	if warning := unresolvedExecutionWarning(states, false); warning != "" {
		t.Fatalf("terminal/read-only warning = %q, want empty", warning)
	}
}

func TestStaleSessionLoadCannotReplaceCurrentState(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "user", Content: "current"}}
	m.sessionLoading = true
	m.sessionLoadToken = 2

	updated, _ := m.Update(SessionLoadedMsg{
		LoadToken: 1,
		SessionID: 99,
		State:     persistedSessionState{Version: 1, Mode: ModeBuild},
		RecoveryContexts: []db.StandaloneReconciliationContext{{
			ResolutionID: "ctrlres_stale", EvidenceSHA256: strings.Repeat("a", 64),
			Disposition: reconciliation.DispositionEffectApplied, SourceKind: reconciliation.SourceVerificationCheck,
		}},
	})
	m = updated.(*Model)
	if len(m.entries) != 1 || m.entries[0].Content != "current" || m.sessionID != 0 {
		t.Fatalf("stale load replaced current state: entries=%#v session=%d", m.entries, m.sessionID)
	}
	if !m.sessionLoading {
		t.Fatal("stale result cancelled the newer in-flight session load")
	}
	if got := standaloneRecoveryHostMessages(m.agent.Messages()); len(got) != 0 {
		t.Fatalf("stale load injected recovery context: %#v", got)
	}
}

func TestSessionLoadAdoptsExactStateRevision(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "session-revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "revisioned session", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := persistedSessionState{Version: currentPersistedSessionVersion, Mode: ModeBuild}
	raw, err := marshalPersistedSessionState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, raw); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.SetSessionStore(store)
	m.standaloneRecovery = &standaloneRecoveryState{}
	if err := m.initializeSessionStateRevision(record.Revision + 99); err != nil {
		t.Fatal(err)
	}
	m.sessionLoading = true
	m.sessionLoadToken = 8
	updated, _ := m.Update(SessionLoadedMsg{
		LoadToken: 8, SessionID: session.ID, State: state, StateRecord: record, Title: session.Title,
	})
	m = updated.(*Model)
	m.sessionStateMu.RLock()
	gotRevision, known, dirty := m.sessionStateRevision, m.sessionStateRevisionKnown, m.sessionStatePersistenceDirty
	m.sessionStateMu.RUnlock()
	if m.sessionID != session.ID || gotRevision != record.Revision || !known || dirty {
		t.Fatalf("loaded revision state = session %d revision %d known %v dirty %v", m.sessionID, gotRevision, known, dirty)
	}
	if m.standaloneRecovery != nil {
		t.Fatalf("clean session retained a prior recovery target: %#v", m.standaloneRecovery)
	}
}

func TestEscapeInvalidatesSessionLoad(t *testing.T) {
	m := newTestModel(t)
	m.sessionLoading = true
	m.sessionLoadToken = 4
	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	if m.sessionLoading || m.sessionLoadToken != 5 {
		t.Fatalf("session load was not invalidated: loading=%v token=%d", m.sessionLoading, m.sessionLoadToken)
	}
}

func TestCancelSessionListCancelsOwnedLookup(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.sessionListing = true
	m.sessionListToken = 4
	m.sessionListCancel = cancel

	m.cancelSessionList()

	if m.sessionListing || m.sessionListToken != 5 || m.sessionListCancel != nil {
		t.Fatalf("session list was not invalidated: listing=%v token=%d cancel=%v", m.sessionListing, m.sessionListToken, m.sessionListCancel != nil)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("session list cancellation did not reach the owned lookup")
	}
}

func TestSessionListReceiptReleasesOwnedLookup(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.sessionListing = true
	m.sessionListToken = 7
	m.sessionListCancel = cancel

	updated, _ := m.Update(SessionListMsg{ListToken: 7})
	m = updated.(*Model)

	if m.sessionListing || m.sessionListCancel != nil {
		t.Fatalf("session list receipt retained lookup ownership: listing=%v cancel=%v", m.sessionListing, m.sessionListCancel != nil)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("session list receipt did not release the owned lookup")
	}
}

func TestShutdownWaitsForSessionLoadCancellationReceipt(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.sessionLoading = true
	m.sessionLoadToken = 11
	m.sessionLoadCancel = cancel

	cmd := m.beginShutdown()
	if cmd == nil || m.shutdownReady() || !m.sessionLoading {
		t.Fatalf("shutdown did not retain load ownership: cmd=%v ready=%v loading=%v", cmd != nil, m.shutdownReady(), m.sessionLoading)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("shutdown did not cancel session load context")
	}
	updated, quit := m.Update(SessionLoadedMsg{LoadToken: 11, Err: context.Canceled})
	m = updated.(*Model)
	if quit == nil || !m.shutdownReady() || m.sessionLoading {
		t.Fatalf("cancellation receipt did not release shutdown: quit=%v ready=%v loading=%v", quit != nil, m.shutdownReady(), m.sessionLoading)
	}
}

func TestShutdownWaitsForSessionListCancellationReceipt(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.sessionListing = true
	m.sessionListToken = 13
	m.sessionListCancel = cancel

	cmd := m.beginShutdown()
	if cmd == nil || m.shutdownReady() || !m.sessionListing {
		t.Fatalf("shutdown did not retain list ownership: cmd=%v ready=%v listing=%v", cmd != nil, m.shutdownReady(), m.sessionListing)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("shutdown did not cancel session list context")
	}
	updated, quit := m.Update(SessionListMsg{ListToken: 13, Err: context.Canceled})
	m = updated.(*Model)
	if quit == nil || !m.shutdownReady() || m.sessionListing {
		t.Fatalf("cancellation receipt did not release shutdown: quit=%v ready=%v listing=%v", quit != nil, m.shutdownReady(), m.sessionListing)
	}
	if m.overlay == OverlaySessionsPicker || m.sessionsPickerState != nil {
		t.Fatal("shutdown receipt reopened the sessions picker")
	}
}

func TestLateSessionListCannotOpenDuringActiveTurn(t *testing.T) {
	m := newTestModel(t)
	m.sessionListing = true
	m.sessionListToken = 7
	m.state = StateStreaming
	updated, _ := m.Update(SessionListMsg{
		ListToken: 7,
		Sessions:  []SessionListItem{{ID: 1, Title: "foreign"}},
	})
	m = updated.(*Model)
	if m.overlay == OverlaySessionsPicker || m.sessionsPickerState != nil {
		t.Fatal("late session list opened a picker during an active turn")
	}
	if m.sessionListing {
		t.Fatal("late session list left input permanently locked")
	}
}

func TestSessionLoadCannotRestoreDuringActiveTurn(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "user", Content: "active"}}
	m.sessionLoading = true
	m.sessionLoadToken = 3
	m.state = StateWaiting
	updated, _ := m.Update(SessionLoadedMsg{
		LoadToken: 3,
		SessionID: 9,
		State: persistedSessionState{
			Version: 1,
			Mode:    ModeBuild,
			Entries: []persistedChatEntry{{Kind: "user", Content: "stale"}},
		},
	})
	m = updated.(*Model)
	if m.sessionID != 0 || len(m.entries) != 1 || m.entries[0].Content != "active" {
		t.Fatalf("active turn was replaced by late session load: session=%d entries=%#v", m.sessionID, m.entries)
	}
}

func TestSessionToolPersistenceExcludesEphemeralDataAndBoundsCards(t *testing.T) {
	m := newTestModel(t)
	m.toolEntries = []ToolEntry{{
		ID:            "tool-1",
		Name:          "write",
		Summary:       strings.Repeat("summary ", maxToolCardSummaryBytes),
		Args:          strings.Repeat("a", maxPersistedToolArgsBytes*2),
		RawArgs:       map[string]any{"token": "RAW_SECRET_DO_NOT_PERSIST"},
		Result:        strings.Repeat("r", maxPersistedToolResultBytes*2),
		BeforeContent: "BEFORE_SECRET_DO_NOT_PERSIST",
		Status:        ToolStatusDone,
		DiffLines:     make([]DiffLine, maxPersistedDiffLines*2),
		Projection: ecosystem.ToolProjection{
			Specialist: "bob", Operation: "bob_check", Role: ecosystem.RoleBuild,
			Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainConflict,
			Route: ecosystem.ToolRoute{Gateway: "mcphub", Server: "bob", Tool: "bob_check", Lazy: true},
		},
	}}
	raw, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "RAW_SECRET_DO_NOT_PERSIST") || strings.Contains(raw, "BEFORE_SECRET_DO_NOT_PERSIST") {
		t.Fatalf("ephemeral tool data leaked into session JSON: %s", raw)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ToolEntries) != 1 {
		t.Fatalf("tool entries = %d", len(state.ToolEntries))
	}
	entry := state.ToolEntries[0]
	if len(entry.Summary) > maxToolCardSummaryBytes || len(entry.Args) > maxPersistedToolArgsBytes || len(entry.Result) > maxPersistedToolResultBytes {
		t.Fatalf("persisted tool card exceeded bounds: summary=%d args=%d result=%d", len(entry.Summary), len(entry.Args), len(entry.Result))
	}
	if len(entry.DiffLines) > maxPersistedDiffLines {
		t.Fatalf("persisted diff lines = %d", len(entry.DiffLines))
	}
	restored := restoreToolEntries(state.ToolEntries)
	if restored[0].RawArgs != nil || restored[0].BeforeContent != "" {
		t.Fatalf("ephemeral fields restored: %#v", restored[0])
	}
	if restored[0].Projection.Domain != ecosystem.DomainConflict || restored[0].Projection.Route.Gateway != "mcphub" {
		t.Fatalf("semantic projection did not round-trip: %#v", restored[0].Projection)
	}
}

func TestSessionPersistenceRedactsMCPToolCallArgumentsAndLegacyCardText(t *testing.T) {
	secret := "SESSION_MCP_SECRET_DO_NOT_PERSIST"
	state := persistedSessionState{
		Version: currentPersistedSessionVersion,
		Mode:    ModeNormal,
		Messages: []llm.Message{{
			Role: "assistant",
			ToolCalls: []llm.ToolCall{{
				ID: "call-1", Name: "mcphub__mcphub_call_tool",
				Arguments: map[string]any{
					"server": "cortex", "tool": "cortex__investigate",
					"arguments": map[string]any{"query": secret, "manifest_yaml": secret},
				},
			}},
		}},
		ToolEntries: []persistedToolEntry{{
			ID: "call-1", Name: "mcphub__mcphub_call_tool", Args: "query=" + secret,
			Projection: ecosystem.ToolProjection{
				Specialist: "cortex", Operation: "investigate", Role: ecosystem.RoleCoordination,
				Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainUnknown,
				Route: ecosystem.ToolRoute{Gateway: "mcphub", Server: "cortex", Tool: "investigate", Lazy: true},
			},
		}},
	}

	raw, err := marshalPersistedSessionState(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, "manifest_yaml") {
		t.Fatalf("session JSON leaked MCP payload: %s", raw)
	}
	for _, route := range []string{"cortex", "investigate"} {
		if !strings.Contains(raw, route) {
			t.Fatalf("session JSON = %s, missing safe route %q", raw, route)
		}
	}

	decoded, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	encodedAgain, err := marshalPersistedSessionState(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encodedAgain, secret) {
		t.Fatalf("restored session reintroduced MCP secret: %s", encodedAgain)
	}

	model := newTestModel(t)
	if err := model.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	live, err := marshalPersistedSessionState(persistedSessionState{
		Version:     currentPersistedSessionVersion,
		Mode:        ModeNormal,
		Messages:    model.agent.Messages(),
		ToolEntries: persistToolEntries(model.toolEntries),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(live, secret) || len(model.toolEntries) != 1 || strings.Contains(model.toolEntries[0].Args, secret) {
		t.Fatalf("in-memory restore admitted MCP secret: %s entries=%#v", live, model.toolEntries)
	}
}

func TestSessionToolSummaryRoundTripAndLegacyFallback(t *testing.T) {
	t.Run("current snapshot", func(t *testing.T) {
		persisted := persistToolEntries([]ToolEntry{{
			ID: "read-1", Name: "read_file", Summary: "internal/ui/session.go", Args: "path=internal/ui/session.go",
		}})
		if got, want := persisted[0].Summary, "internal/ui/session.go"; got != want {
			t.Fatalf("persisted summary = %q, want %q", got, want)
		}
		restored := restoreToolEntries(persisted)
		if got, want := restored[0].Summary, persisted[0].Summary; got != want {
			t.Fatalf("restored summary = %q, want %q", got, want)
		}
	})

	t.Run("legacy snapshot without summary", func(t *testing.T) {
		state := persistedSessionState{
			Version: 1,
			Mode:    ModeAsk,
			Entries: []persistedChatEntry{{Kind: "tool_group", ToolIndex: 0}},
			ToolEntries: []persistedToolEntry{{
				ID: "run-1", Name: "bash", Args: "command=go test ./internal/ui", Status: ToolStatusDone, Collapsed: true,
			}},
		}
		m := newTestModel(t)
		if err := m.restoreSessionState(state); err != nil {
			t.Fatal(err)
		}
		if got, want := m.toolEntries[0].Summary, "command=go test ./internal/ui"; got != want {
			t.Fatalf("legacy entry summary = %q, want args fallback %q", got, want)
		}
		card := testProjectedToolCard(t, m, 0)
		if got, want := card.Summary, m.toolEntries[0].Summary; got != want {
			t.Fatalf("restored card summary = %q, want %q", got, want)
		}
		if view := card.View(64); !strings.Contains(view, "go test ./internal/ui") {
			t.Fatalf("collapsed legacy receipt omitted recovered context:\n%s", view)
		}
	})
}

func TestSessionToolResultLanguageRoundTripsOnlyAllowlistedAlias(t *testing.T) {
	persisted := persistToolEntries([]ToolEntry{{
		ID: "read-1", Name: "read_file", ResultLanguage: "go",
	}})
	if got := persisted[0].ResultLanguage; got != "go" {
		t.Fatalf("persisted language = %q", got)
	}
	if got := restoreToolEntries(persisted)[0].ResultLanguage; got != "go" {
		t.Fatalf("restored language = %q", got)
	}
	malicious := restoreToolEntries([]persistedToolEntry{{
		ID: "read-2", Name: "read_file", ResultLanguage: "../../bash",
	}})
	if got := malicious[0].ResultLanguage; got != "" {
		t.Fatalf("untrusted persisted language survived: %q", got)
	}
}

func TestSessionToolResultStripsTerminalControlsBeforePersistenceAndRestore(t *testing.T) {
	persisted := persistToolEntries([]ToolEntry{{
		ID: "read-1", Name: "read_file",
		Result: "safe\x1b]0;owned\x07\nnext\u202esecret",
	}})
	if got, want := persisted[0].Result, "safe\nnextsecret"; got != want {
		t.Fatalf("persisted result = %q, want %q", got, want)
	}

	restored := restoreToolEntries([]persistedToolEntry{{
		ID: "legacy-1", Name: "read_file", Status: ToolStatusDone,
		Result: "before\x1b[31mred\x1b[0m\nafter\u2066",
	}})
	if got, want := restored[0].Result, "beforered\nafter"; got != want {
		t.Fatalf("restored legacy result = %q, want %q", got, want)
	}
}

func TestInterruptedToolRestoreSettlesSemanticProjectionIdempotently(t *testing.T) {
	running := ecosystem.ProjectToolCall("mcphub__bob__bob_check", nil)
	persisted := []persistedToolEntry{{
		ID: "tool-interrupted", Name: "mcphub__bob__bob_check", Status: ToolStatusRunning,
		Projection: running,
	}}

	assertSettled := func(label string, entry ToolEntry) {
		t.Helper()
		if entry.Status != ToolStatusError || !entry.IsError || entry.Result != "Interrupted before session was saved" {
			t.Fatalf("%s display state did not settle: %#v", label, entry)
		}
		projection := entry.Projection.Normalize()
		if projection.Transport != ecosystem.TransportFailed || projection.Domain != ecosystem.DomainUnknown || projection.Evidence != ecosystem.EvidenceNone {
			t.Fatalf("%s semantic projection did not settle: %#v", label, projection)
		}
	}

	first := restoreToolEntries(persisted)
	if len(first) != 1 {
		t.Fatalf("first restore entries = %d", len(first))
	}
	assertSettled("first restore", first[0])

	second := restoreToolEntries(persistToolEntries(first))
	if len(second) != 1 {
		t.Fatalf("second restore entries = %d", len(second))
	}
	assertSettled("second restore", second[0])

	m := newTestModel(t)
	m.toolsPending = 9
	state := persistedSessionState{
		Version:     currentPersistedSessionVersion,
		Mode:        ModeNormal,
		Entries:     []persistedChatEntry{{Kind: "tool_group", ToolIndex: 0}},
		ToolEntries: persistToolEntries(second),
	}
	if err := m.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	if card := testProjectedToolCard(t, m, 0); card.State != ToolCardError {
		t.Fatalf("double-restored card revived as non-error: %#v", card)
	}
	if m.toolsPending != 0 {
		t.Fatalf("restore retained %d pending tools without resumable work", m.toolsPending)
	}
	assertSettled("model restore", m.toolEntries[0])
}

func TestCancelledToolRoundTripPreservesDistinctLifecycle(t *testing.T) {
	projection := ecosystem.ProjectToolCall("read_file", nil)
	projection.Transport = ecosystem.TransportFailed
	projection.Domain = ecosystem.DomainUnknown
	projection.Evidence = ecosystem.EvidenceNone
	projection = projection.Normalize()

	persisted := persistToolEntries([]ToolEntry{{
		ID:         "tool-cancelled",
		Name:       "read_file",
		Summary:    "README.md",
		Result:     "Cancelled by user before completion",
		Status:     ToolStatusCancelled,
		Duration:   2 * time.Second,
		Projection: projection,
	}})
	restored := restoreToolEntries(persisted)
	if len(restored) != 1 {
		t.Fatalf("restored tools = %d, want 1", len(restored))
	}
	entry := restored[0]
	if entry.Status != ToolStatusCancelled || entry.IsError ||
		entry.Result != "Cancelled by user before completion" {
		t.Fatalf("cancelled lifecycle changed across restore: %#v", entry)
	}
	view, err := ToolViewModelFromToolEntry(
		ChatEntry{BlockID: "block-cancelled", Revision: 1, Lifecycle: BlockCancelled, Kind: "tool_group"},
		entry,
	)
	if err != nil {
		t.Fatalf("project cancelled restore: %v", err)
	}
	if view.Lifecycle != ToolLifecycleCancelled {
		t.Fatalf("restored lifecycle = %d, want cancelled", view.Lifecycle)
	}
}

func TestLoadPersistedSessionRejectsDifferentCanonicalWorkspace(t *testing.T) {
	workspaceA, err := canonicalWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceB, err := canonicalWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if workspaceA == workspaceB {
		t.Fatalf("test workspaces unexpectedly canonicalized to the same path: %q", workspaceA)
	}

	store, err := db.OpenPath(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	ctx := context.Background()
	session, err := store.CreateSession(ctx, db.CreateSessionParams{
		Title:       "workspace A",
		Model:       "qwen3.5:2b",
		Mode:        "BUILD",
		WorkspaceID: workspaceA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(ctx, session.ID, `{"version":1,"messages":[],"entries":[],"mode":2}`); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := loadPersistedSession(ctx, store, session.ID, workspaceB); err == nil || !strings.Contains(err.Error(), "different workspace") {
		t.Fatalf("cross-workspace load error = %v, want ownership rejection", err)
	}
	loaded, _, _, err := loadPersistedSession(ctx, store, session.ID, workspaceA)
	if err != nil {
		t.Fatalf("same-workspace load failed: %v", err)
	}
	if loaded.ID != session.ID {
		t.Fatalf("loaded session id = %d, want %d", loaded.ID, session.ID)
	}
}
