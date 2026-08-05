package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestSessionV3PersistsTranscriptIdentityWithoutPrivateThinking(t *testing.T) {
	source := newTestModel(t)
	source.entries = []ChatEntry{
		{Kind: "user", Content: "inspect", ThinkingContent: "private user-side scratch"},
		{Kind: "assistant", Content: "done", ThinkingContent: "private provider reasoning"},
	}

	raw, err := encodeSessionState(source)
	if err != nil {
		t.Fatalf("encode session: %v", err)
	}
	for _, private := range []string{"private user-side scratch", "private provider reasoning", "thinking_content"} {
		if strings.Contains(raw, private) {
			t.Fatalf("encoded session contains private thinking marker %q: %s", private, raw)
		}
	}

	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if state.Version != 3 || len(state.Entries) != 2 {
		t.Fatalf("decoded version/entries = %d/%d", state.Version, len(state.Entries))
	}
	for index, entry := range state.Entries {
		if !entry.BlockID.Valid() || !entry.TurnID.Valid() || entry.Revision == 0 || !entry.Lifecycle.Valid() {
			t.Fatalf("entry %d has incomplete identity: %#v", index, entry)
		}
		if entry.ThinkingContent != "" {
			t.Fatalf("entry %d restored private thinking %q", index, entry.ThinkingContent)
		}
	}
	if state.Entries[0].TurnID != state.Entries[1].TurnID {
		t.Fatalf("one causal turn split across IDs: %#v", state.Entries)
	}

	target := newTestModel(t)
	if err := target.restoreSessionState(state); err != nil {
		t.Fatalf("restore session: %v", err)
	}
	for index, entry := range target.entries {
		if entry.BlockID != state.Entries[index].BlockID ||
			entry.TurnID != state.Entries[index].TurnID ||
			entry.Revision != state.Entries[index].Revision ||
			entry.Lifecycle != state.Entries[index].Lifecycle {
			t.Fatalf("entry %d identity changed during restore: %#v", index, entry)
		}
		if entry.ThinkingContent != "" {
			t.Fatalf("entry %d restored private thinking %q", index, entry.ThinkingContent)
		}
	}
}

func TestLegacySessionTranscriptIdentityIsDeterministicAndContentIndependent(t *testing.T) {
	decode := func(t *testing.T, version int, userContent, assistantContent string) persistedSessionState {
		t.Helper()
		raw := `{"version":` + string(rune('0'+version)) +
			`,"messages":[],"entries":[{"kind":"user","content":"` + userContent +
			`","thinking_content":"legacy private reasoning"},{"kind":"assistant","content":"` + assistantContent +
			`"}],"mode":2}`
		state, err := decodeSessionState(raw)
		if err != nil {
			t.Fatalf("decode legacy v%d session: %v", version, err)
		}
		return state
	}

	for _, version := range []int{1, 2} {
		first := decode(t, version, "alpha", "one")
		second := decode(t, version, "beta", "two")
		if first.Version != currentPersistedSessionVersion || second.Version != currentPersistedSessionVersion {
			t.Fatalf("legacy v%d did not migrate to v%d", version, currentPersistedSessionVersion)
		}
		for index := range first.Entries {
			if first.Entries[index].BlockID != second.Entries[index].BlockID ||
				first.Entries[index].TurnID != second.Entries[index].TurnID {
				t.Fatalf("legacy v%d identity depends on content at entry %d", version, index)
			}
			if first.Entries[index].Revision != 1 || !first.Entries[index].Lifecycle.Terminal() {
				t.Fatalf("legacy v%d entry %d identity is incomplete: %#v", version, index, first.Entries[index])
			}
			if first.Entries[index].ThinkingContent != "" {
				t.Fatalf("legacy v%d entry %d retained private thinking", version, index)
			}
		}
		if first.Entries[0].TurnID != first.Entries[1].TurnID {
			t.Fatalf("legacy v%d causal turn was split: %#v", version, first.Entries)
		}
	}
}

func TestSessionV3TranscriptIdentityValidationFailsClosed(t *testing.T) {
	valid := persistedChatEntry{
		Kind: "user", BlockID: "block-a", TurnID: "turn-a", Revision: 1, Lifecycle: BlockSettled,
	}
	tests := []struct {
		name    string
		entries []persistedChatEntry
		want    string
	}{
		{
			name:    "mixed identified and zero",
			entries: []persistedChatEntry{valid, {Kind: "assistant"}},
			want:    "mixed or incomplete",
		},
		{
			name: "invalid block id",
			entries: []persistedChatEntry{{
				Kind: "user", BlockID: " block-a", TurnID: "turn-a", Revision: 1, Lifecycle: BlockSettled,
			}},
			want: "invalid block ID",
		},
		{
			name: "duplicate block id",
			entries: []persistedChatEntry{
				valid,
				{Kind: "assistant", BlockID: "block-a", TurnID: "turn-a", Revision: 1, Lifecycle: BlockSettled},
			},
			want: "repeats block ID",
		},
		{
			name: "missing turn",
			entries: []persistedChatEntry{{
				Kind: "assistant", BlockID: "block-a", Revision: 1, Lifecycle: BlockSettled,
			}},
			want: "missing a turn ID",
		},
		{
			name: "invalid lifecycle",
			entries: []persistedChatEntry{{
				Kind: "user", BlockID: "block-a", TurnID: "turn-a", Revision: 1, Lifecycle: BlockLifecycle(99),
			}},
			want: "invalid lifecycle",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := marshalPersistedSessionState(persistedSessionState{
				Version: currentPersistedSessionVersion,
				Mode:    ModeNormal,
				Entries: test.entries,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}

	raw, err := marshalPersistedSessionState(persistedSessionState{
		Version: currentPersistedSessionVersion,
		Mode:    ModeNormal,
		Entries: []persistedChatEntry{{Kind: "user"}, {Kind: "assistant"}},
	})
	if err != nil {
		t.Fatalf("internal all-zero DTO was not completed before encode: %v", err)
	}
	completed, err := decodeSessionState(raw)
	if err != nil || len(completed.Entries) != 2 ||
		!completed.Entries[0].BlockID.Valid() || !completed.Entries[1].BlockID.Valid() {
		t.Fatalf("encoded DTO lacks completed identity: state=%#v err=%v", completed.Entries, err)
	}

	if _, err := decodeSessionState(
		`{"version":3,"messages":[],"entries":[{"kind":"user","content":"missing"}],"mode":2}`,
	); err == nil || !strings.Contains(err.Error(), "identity is missing") {
		t.Fatalf("raw v3 without transcript identity error = %v", err)
	}
}

func TestSessionV3ToolLifecycleMustMatchPersistedStatus(t *testing.T) {
	base := persistedSessionState{
		Version: currentPersistedSessionVersion,
		Mode:    ModeNormal,
		Entries: []persistedChatEntry{{
			Kind: "tool_group", ToolIndex: 0,
			BlockID: "block-tool", TurnID: "turn-tool", Revision: 1,
			Lifecycle: BlockSettled,
		}},
		ToolEntries: []persistedToolEntry{{
			ID: "call-1", Name: "read_file", Status: ToolStatusDone,
		}},
	}
	if _, err := marshalPersistedSessionState(base); err != nil {
		t.Fatalf("valid tool transcript rejected: %v", err)
	}
	cancelled := base
	cancelled.Entries = append([]persistedChatEntry(nil), base.Entries...)
	cancelled.ToolEntries = append([]persistedToolEntry(nil), base.ToolEntries...)
	cancelled.Entries[0].Lifecycle = BlockCancelled
	cancelled.ToolEntries[0].Status = ToolStatusCancelled
	cancelled.ToolEntries[0].Result = cancelledToolResult
	cancelled.ToolEntries[0].Projection = canonicalCancelledProjectionForTest("read_file")
	if _, err := marshalPersistedSessionState(cancelled); err != nil {
		t.Fatalf("valid cancelled tool transcript rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*persistedSessionState)
		want string
	}{
		{
			name: "missing tool index",
			edit: func(state *persistedSessionState) { state.Entries[0].ToolIndex = 1 },
			want: "missing tool index",
		},
		{
			name: "live block with done tool",
			edit: func(state *persistedSessionState) { state.Entries[0].Lifecycle = BlockLive },
			want: "does not match status",
		},
		{
			name: "settled block with failed tool",
			edit: func(state *persistedSessionState) { state.ToolEntries[0].Status = ToolStatusError },
			want: "does not match status",
		},
		{
			name: "invalid tool status",
			edit: func(state *persistedSessionState) { state.ToolEntries[0].Status = ToolStatus(99) },
			want: "invalid status",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := base
			state.Entries = append([]persistedChatEntry(nil), base.Entries...)
			state.ToolEntries = append([]persistedToolEntry(nil), base.ToolEntries...)
			test.edit(&state)
			if _, err := marshalPersistedSessionState(state); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("marshal error = %v, want %q", err, test.want)
			}

			target := newTestModel(t)
			target.entries = []ChatEntry{{Kind: "system", Content: "keep"}}
			if err := target.restoreSessionState(state); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("restore error = %v, want %q", err, test.want)
			}
			if len(target.entries) != 1 || target.entries[0].Content != "keep" {
				t.Fatalf("invalid tool state mutated target transcript: %#v", target.entries)
			}
		})
	}
}

func TestSessionV3RejectsAdversarialCancelledToolState(t *testing.T) {
	validState := func() persistedSessionState {
		return persistedSessionState{
			Version: currentPersistedSessionVersion,
			Mode:    ModeNormal,
			Entries: []persistedChatEntry{{
				Kind: "tool_group", ToolIndex: 0,
				BlockID: "block-cancelled", TurnID: "turn-cancelled", Revision: 1,
				Lifecycle: BlockCancelled,
			}},
			ToolEntries: []persistedToolEntry{{
				ID:         "call-cancelled",
				Name:       "read_file",
				Result:     cancelledToolResult,
				Status:     ToolStatusCancelled,
				Projection: canonicalCancelledProjectionForTest("read_file"),
			}},
		}
	}
	if raw, err := json.Marshal(validState()); err != nil {
		t.Fatal(err)
	} else if _, err := decodeSessionState(string(raw)); err != nil {
		t.Fatalf("canonical cancelled JSON rejected: %v", err)
	}

	validOutput := OutputDetailDigest{
		TotalRows: 1, RetainedRows: 1, TotalBytes: 1, RetainedBytes: 1,
	}
	tests := []struct {
		name string
		want string
		edit func(*persistedToolEntry)
	}{
		{
			name: "successful verified projection",
			want: "must be transport failed",
			edit: func(tool *persistedToolEntry) {
				tool.Projection.Transport = ecosystem.TransportSucceeded
				tool.Projection.Domain = ecosystem.DomainSucceeded
				tool.Projection.DomainTyped = true
				tool.Projection.Evidence = ecosystem.EvidenceVerified
			},
		},
		{
			name: "typed unknown domain",
			want: "must be transport failed",
			edit: func(tool *persistedToolEntry) {
				tool.Projection.DomainTyped = true
			},
		},
		{
			name: "receipt digest",
			want: "digest or artifact",
			edit: func(tool *persistedToolEntry) {
				tool.Projection.Digest = &ecosystem.ReceiptDigest{}
			},
		},
		{
			name: "artifact digest",
			want: "digest or artifact",
			edit: func(tool *persistedToolEntry) {
				tool.Projection.Artifact = &ecosystem.ArtifactDigest{}
			},
		},
		{
			name: "output detail",
			want: "output detail",
			edit: func(tool *persistedToolEntry) {
				digest := validOutput
				tool.OutputDetail = &digest
			},
		},
		{
			name: "arbitrary result output",
			want: "result output",
			edit: func(tool *persistedToolEntry) {
				tool.Result = "late success"
			},
		},
		{
			name: "result language",
			want: "result language",
			edit: func(tool *persistedToolEntry) {
				tool.ResultLanguage = "go"
			},
		},
		{
			name: "diff output",
			want: "diff output",
			edit: func(tool *persistedToolEntry) {
				tool.DiffLines = []DiffLine{{Content: "+late success"}}
			},
		},
		{
			name: "error flag",
			want: "marked as an error",
			edit: func(tool *persistedToolEntry) {
				tool.IsError = true
			},
		},
		{
			name: "unnormalized projection",
			want: "not normalized",
			edit: func(tool *persistedToolEntry) {
				tool.Projection.Transport = ecosystem.TransportState("invented")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := validState()
			test.edit(&state.ToolEntries[0])
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeSessionState(string(raw)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decode error = %v, want %q; JSON=%s", err, test.want, raw)
			}
		})
	}
}

func canonicalCancelledProjectionForTest(name string) ecosystem.ToolProjection {
	projection := ecosystem.ProjectToolCall(name, nil).Normalize()
	projection.Transport = ecosystem.TransportFailed
	projection.Domain = ecosystem.DomainUnknown
	projection.DomainTyped = false
	projection.Evidence = ecosystem.EvidenceNone
	projection.Digest = nil
	projection.Artifact = nil
	return projection.Normalize()
}

func TestSessionProviderReferenceIsSafeAndRestoreMismatchIsNonMutating(t *testing.T) {
	t.Setenv("XAI_API_KEY", "super-secret-test-key")
	manager := providerSwitchTestManager(t)
	if err := manager.SwitchProvider("xai"); err != nil {
		t.Fatalf("switch test provider: %v", err)
	}
	source := newTestModel(t)
	source.modelManager = manager
	source.model = manager.Model()
	source.entries = []ChatEntry{{Kind: "user", Content: "provider-bound"}}

	raw, err := encodeSessionState(source)
	if err != nil {
		t.Fatalf("encode provider-bound session: %v", err)
	}
	for _, forbidden := range []string{"super-secret-test-key", "XAI_API_KEY", "api.x.ai", "base_url", "api_key"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("provider reference leaked %q: %s", forbidden, raw)
		}
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatalf("decode provider-bound session: %v", err)
	}
	if state.Provider == nil || state.Provider.Profile != "xai" ||
		state.Provider.Model != "grok-4" || state.Provider.Locality != persistedProviderRemote {
		t.Fatalf("safe provider reference = %#v", state.Provider)
	}

	target := newTestModel(t)
	target.model = "keep-model"
	target.entries = []ChatEntry{{Kind: "system", Content: "keep transcript"}}
	err = target.restoreSessionState(state)
	if err == nil || !strings.Contains(err.Error(), "session requires remote provider \"xai\"") {
		t.Fatalf("provider mismatch restore error = %v", err)
	}
	if target.model != "keep-model" || len(target.entries) != 1 || target.entries[0].Content != "keep transcript" {
		t.Fatalf("provider mismatch mutated target: model=%q entries=%#v", target.model, target.entries)
	}
}

func TestLegacySessionWithoutProviderIdentityCannotRestoreUnderRemoteProvider(t *testing.T) {
	t.Setenv("XAI_API_KEY", "test-key")
	manager := providerSwitchTestManager(t)
	if err := manager.SwitchProvider("xai"); err != nil {
		t.Fatalf("switch test provider: %v", err)
	}
	state := persistedSessionState{
		Version: currentPersistedSessionVersion,
		Mode:    ModeNormal,
		Model:   manager.Model(),
		Entries: []persistedChatEntry{{
			Kind: "user", Content: "local-only legacy history",
			BlockID: "legacy-block", TurnID: "legacy-turn", Revision: 1,
			Lifecycle: BlockSettled,
		}},
	}
	target := newTestModel(t)
	target.modelManager = manager
	target.model = manager.Model()
	target.entries = []ChatEntry{{Kind: "system", Content: "keep transcript"}}

	err := target.restoreSessionState(state)
	if err == nil {
		t.Fatal("a legacy session restored under a remote provider")
	}
	// The refusal must name the risk it is preventing, not prescribe a remedy.
	// "switch to a local provider" was advice a hosted-model harness cannot
	// act on, which makes a correct refusal read as a broken one.
	for _, want := range []string{"predates provider identity", "leave the machine"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("legacy remote restore error omits %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "switch to a local provider") {
		t.Errorf("the refusal still prescribes an action this harness cannot take: %v", err)
	}
	if len(target.entries) != 1 || target.entries[0].Content != "keep transcript" {
		t.Fatalf("rejected legacy restore mutated transcript: %#v", target.entries)
	}
}

func TestHeadlessRemoteSessionPersistsExactProviderIdentity(t *testing.T) {
	raw, err := EncodeHeadlessSessionStateWithProvider(
		[]llm.Message{{Role: "user", Content: "remote turn"}},
		"grok-test",
		"",
		true,
		7,
		agent.ContextPromptFloor{},
		SessionProviderIdentity{Profile: "xai", Remote: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if state.Provider == nil ||
		state.Provider.Profile != "xai" ||
		state.Provider.Model != "grok-test" ||
		state.Provider.Locality != persistedProviderRemote {
		t.Fatalf("headless provider identity = %#v", state.Provider)
	}
}
