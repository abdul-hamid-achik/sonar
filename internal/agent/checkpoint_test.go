package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type checkpointModelClient struct {
	model string
}

func (*checkpointModelClient) ChatStream(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error {
	return nil
}
func (*checkpointModelClient) Ping() error { return nil }
func (client *checkpointModelClient) Model() string {
	if client.model != "" {
		return client.model
	}
	return "checkpoint-model"
}
func (*checkpointModelClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestCheckpointRoundTrip(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	a := New(nil, nil, 8192)
	a.SetCheckpointStore(store, 0)

	// Build an initial conversation and checkpoint it.
	a.AddUserMessage("first question")
	a.AppendMessage(llm.Message{Role: "assistant", Content: "first answer"})
	id, err := a.CreateCheckpoint(context.Background(), "snapshot", db.CheckpointManual)
	if err != nil {
		t.Fatalf("create checkpoint: %v", err)
	}
	if id == 0 {
		t.Fatal("expected a real checkpoint id")
	}

	// Diverge: add more turns.
	a.AddUserMessage("second question")
	a.AppendMessage(llm.Message{Role: "assistant", Content: "second answer"})
	if got := len(a.Messages()); got != 4 {
		t.Fatalf("expected 4 messages before restore, got %d", got)
	}

	// Restore should rewind to the 2-message snapshot.
	n, err := a.RestoreCheckpoint(context.Background(), id)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 messages restored, got %d", n)
	}
	msgs := a.Messages()
	if len(msgs) != 2 || msgs[0].Content != "first question" || msgs[1].Content != "first answer" {
		t.Fatalf("restored history mismatch: %+v", msgs)
	}

	list, err := a.ListCheckpoints(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(list))
	}
}

func TestCheckpointImageReferenceRoundTripStaysPathFreeAndResolvesLazily(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	raw := []byte("checkpoint image bytes")
	image, err := llm.NewReferencedImageData("capture.png", "image/png", 12, 8, raw)
	if err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 8192)
	ag.SetWorkDir(t.TempDir())
	ag.SetCheckpointStore(store, 0)
	if err := ag.AddUserMessageWithImages("inspect", []llm.ImageData{image}); err != nil {
		t.Fatal(err)
	}
	id, err := ag.CreateCheckpoint(context.Background(), "image", db.CheckpointManual)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := store.GetCheckpoint(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cp.Messages, string(raw)) || strings.Contains(cp.Messages, t.TempDir()) {
		t.Fatalf("checkpoint leaked image bytes or a source path: %s", cp.Messages)
	}
	var persisted checkpointSnapshot
	if err := json.Unmarshal([]byte(cp.Messages), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != checkpointSnapshotVersion || len(persisted.Messages) != 1 || len(persisted.Messages[0].Images) != 1 ||
		len(persisted.Messages[0].Images[0].Data) != 0 || persisted.Messages[0].Images[0].SHA256 != image.SHA256 {
		t.Fatalf("checkpoint image projection = %#v", persisted)
	}

	ag.ReplaceMessages(nil)
	if _, err := ag.RestoreCheckpoint(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	ag.SetImageResolver(func(_ context.Context, reference llm.ImageData) ([]byte, error) {
		if reference.SHA256 != image.SHA256 || len(reference.Data) != 0 {
			t.Fatalf("resolver reference = %#v", reference)
		}
		return raw, nil
	})
	resolved, err := ag.resolveProviderImages(context.Background(), ag.Messages())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resolved[0].Images[0].Data); got != string(raw) {
		t.Fatalf("resolved image = %q", got)
	}
	if len(ag.Messages()[0].Images[0].Data) != 0 {
		t.Fatal("lazy resolution mutated restored checkpoint history")
	}
}

func TestCheckpointRestoresPromptFloorWithExactTranscript(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ag := New(&checkpointModelClient{}, nil, 8192)
	ag.SetWorkDir(t.TempDir())
	ag.SetCheckpointStore(store, 0)
	ag.AddUserMessage("checkpointed prompt")
	wantFloor := ContextPromptFloor{Tokens: 6_100, HostTokens: 800, MessageTokens: 200, Model: "checkpoint-model"}
	if err := ag.RestoreContextPromptFloor(wantFloor); err != nil {
		t.Fatal(err)
	}
	id, err := ag.CreateCheckpoint(context.Background(), "bounded", db.CheckpointPreCompaction)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := store.GetCheckpoint(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, legacy, err := decodeCheckpointSnapshot(cp.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if legacy || snapshot.Version != checkpointSnapshotVersion || snapshot.ContextPromptFloor != wantFloor {
		t.Fatalf("stored checkpoint envelope = %#v, legacy=%v", snapshot, legacy)
	}

	ag.AddUserMessage("later prompt")
	if err := ag.RestoreContextPromptFloor(ContextPromptFloor{Tokens: 7_000, HostTokens: 900, MessageTokens: 300, Model: "checkpoint-model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.RestoreCheckpoint(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := ag.ContextPromptFloor(); got != wantFloor {
		t.Fatalf("restored prompt floor = %#v, want %#v", got, wantFloor)
	}
	if got := ag.Messages(); len(got) != 1 || got[0].Content != "checkpointed prompt" {
		t.Fatalf("restored transcript = %#v", got)
	}
}

func TestLegacyCheckpointCannotReplaceLivePromptFloor(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ag := New(&checkpointModelClient{}, nil, 8192)
	ag.SetWorkDir(t.TempDir())
	ag.SetCheckpointStore(store, 0)
	ag.AddUserMessage("current prompt")
	wantFloor := ContextPromptFloor{Tokens: 6_500, HostTokens: 700, MessageTokens: 250, Model: "checkpoint-model"}
	if err := ag.RestoreContextPromptFloor(wantFloor); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := ag.checkpointWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON, err := json.Marshal([]llm.Message{{Role: "user", Content: "legacy prompt"}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateCheckpointForWorkspace(context.Background(), 0, workspaceID, "legacy", db.CheckpointManual, string(legacyJSON), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.RestoreCheckpoint(context.Background(), id); err == nil {
		t.Fatal("legacy checkpoint replaced a transcript with an exact live receipt")
	}
	if got := ag.ContextPromptFloor(); got != wantFloor {
		t.Fatalf("rejected legacy restore changed floor: got %#v, want %#v", got, wantFloor)
	}
	if got := ag.Messages(); len(got) != 1 || got[0].Content != "current prompt" {
		t.Fatalf("rejected legacy restore changed transcript: %#v", got)
	}

	// A fresh session has no exact provider receipt to discard, so the legacy
	// message-only format remains backward compatible.
	fresh := New(&checkpointModelClient{}, nil, 8192)
	fresh.SetWorkDir(ag.WorkDir())
	fresh.SetCheckpointStore(store, 0)
	if _, err := fresh.RestoreCheckpoint(context.Background(), id); err != nil {
		t.Fatalf("restore legacy checkpoint in fresh session: %v", err)
	}
	if got := fresh.Messages(); len(got) != 1 || got[0].Content != "legacy prompt" || fresh.ContextPromptFloor().Tokens != 0 {
		t.Fatalf("fresh legacy restore = messages %#v floor %#v", got, fresh.ContextPromptFloor())
	}
}

func TestCheckpointModelMismatchDoesNotMutateLiveState(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ag := New(&checkpointModelClient{}, nil, 8192)
	ag.SetWorkDir(t.TempDir())
	ag.SetCheckpointStore(store, 0)
	ag.AddUserMessage("checkpoint model prompt")
	checkpointFloor := ContextPromptFloor{Tokens: 6_000, HostTokens: 700, MessageTokens: 200, Model: "checkpoint-model"}
	if err := ag.RestoreContextPromptFloor(checkpointFloor); err != nil {
		t.Fatal(err)
	}
	id, err := ag.CreateCheckpoint(context.Background(), "model-bound", db.CheckpointManual)
	if err != nil {
		t.Fatal(err)
	}

	ag.llmClient = &checkpointModelClient{model: "different-model"}
	ag.ReplaceMessages([]llm.Message{{Role: "user", Content: "keep different-model prompt"}})
	liveFloor := ContextPromptFloor{Tokens: 5_500, HostTokens: 600, MessageTokens: 180, Model: "different-model"}
	if err := ag.RestoreContextPromptFloor(liveFloor); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.RestoreCheckpoint(context.Background(), id); err == nil {
		t.Fatal("restored a model-bound checkpoint under another model")
	}
	if got := ag.ContextPromptFloor(); got != liveFloor {
		t.Fatalf("model-mismatch restore changed floor: %#v", got)
	}
	if got := ag.Messages(); len(got) != 1 || got[0].Content != "keep different-model prompt" {
		t.Fatalf("model-mismatch restore changed transcript: %#v", got)
	}
}

func TestInvalidCheckpointEnvelopeDoesNotMutateLiveState(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ag := New(&checkpointModelClient{}, nil, 8192)
	ag.SetWorkDir(t.TempDir())
	ag.SetCheckpointStore(store, 0)
	ag.AddUserMessage("keep live prompt")
	wantFloor := ContextPromptFloor{Tokens: 6_500, HostTokens: 700, MessageTokens: 250, Model: "checkpoint-model"}
	if err := ag.RestoreContextPromptFloor(wantFloor); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := ag.checkpointWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	invalid := `{"version":1,"messages":[{"role":"user","content":"forged"}],"context_prompt_floor":{"tokens":1}}`
	id, err := store.CreateCheckpointForWorkspace(context.Background(), 0, workspaceID, "invalid", db.CheckpointManual, invalid, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.RestoreCheckpoint(context.Background(), id); err == nil {
		t.Fatal("restored invalid checkpoint envelope")
	}
	if got := ag.ContextPromptFloor(); got != wantFloor {
		t.Fatalf("invalid restore changed prompt floor: %#v", got)
	}
	if got := ag.Messages(); len(got) != 1 || got[0].Content != "keep live prompt" {
		t.Fatalf("invalid restore changed transcript: %#v", got)
	}
}

func TestExactWithinSessionRollbackRestoresPromptFloor(t *testing.T) {
	ag := New(&checkpointModelClient{}, nil, 8192)
	ag.AddUserMessage("before turn")
	wantMessages := ag.Messages()
	wantFloor := ContextPromptFloor{Tokens: 6_000, HostTokens: 700, MessageTokens: 200, Model: "checkpoint-model"}
	if err := ag.RestoreContextPromptFloor(wantFloor); err != nil {
		t.Fatal(err)
	}
	ag.AddUserMessage("rejected turn")
	if err := ag.RestoreMessagesWithinSession(wantMessages, wantFloor); err != nil {
		t.Fatal(err)
	}
	if got := ag.ContextPromptFloor(); got != wantFloor {
		t.Fatalf("exact rollback floor = %#v, want %#v", got, wantFloor)
	}
	if got := ag.Messages(); len(got) != 1 || got[0].Content != "before turn" {
		t.Fatalf("exact rollback transcript = %#v", got)
	}
}

func TestDurableRecoveryRewriteInvalidatesPromptFloorButExactNoopPreservesIt(t *testing.T) {
	ag := New(&checkpointModelClient{}, nil, 8192)
	ag.AddUserMessage("ordinary history")
	floor := ContextPromptFloor{Tokens: 6_000, HostTokens: 700, MessageTokens: 200, Model: "checkpoint-model"}
	if err := ag.RestoreContextPromptFloor(floor); err != nil {
		t.Fatal(err)
	}
	first := DurableRecoveryContextPrefix + "\nresolution=first"
	if err := ag.AppendDurableRecoveryContext(first); err != nil {
		t.Fatal(err)
	}
	if got := ag.ContextPromptFloor(); got.Tokens != 0 {
		t.Fatalf("non-idempotent append retained stale floor: %#v", got)
	}

	if err := ag.RestoreContextPromptFloor(floor); err != nil {
		t.Fatal(err)
	}
	if err := ag.AppendDurableRecoveryContext(first); err != nil {
		t.Fatal(err)
	}
	if got := ag.ContextPromptFloor(); got != floor {
		t.Fatalf("exact idempotent append cleared floor: %#v", got)
	}
	if err := ag.InstallDurableRecoveryContexts([]string{first}); err != nil {
		t.Fatal(err)
	}
	if got := ag.ContextPromptFloor(); got != floor {
		t.Fatalf("exact idempotent install cleared floor: %#v", got)
	}
	second := DurableRecoveryContextPrefix + "\nresolution=second"
	if err := ag.InstallDurableRecoveryContexts([]string{second}); err != nil {
		t.Fatal(err)
	}
	if got := ag.ContextPromptFloor(); got.Tokens != 0 {
		t.Fatalf("non-idempotent install retained stale floor: %#v", got)
	}
}

func TestCheckpointDisabledWithoutStore(t *testing.T) {
	a := New(nil, nil, 8192)
	a.AddUserMessage("hi")

	id, err := a.CreateCheckpoint(context.Background(), "x", db.CheckpointManual)
	if err != nil {
		t.Fatalf("create without store should be a no-op, got: %v", err)
	}
	if id != 0 {
		t.Fatalf("expected id 0 when disabled, got %d", id)
	}
	if _, err := a.RestoreCheckpoint(context.Background(), 1); err == nil {
		t.Fatal("restore without store should error")
	}
}

func TestRestoreCheckpointRejectsAnotherSession(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id, err := store.CreateCheckpoint(context.Background(), 22, "foreign", db.CheckpointManual, `[]`, 0)
	if err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 8192)
	ag.SetCheckpointStore(store, 11)
	if _, err := ag.RestoreCheckpoint(context.Background(), id); err == nil {
		t.Fatal("restored checkpoint from another session")
	}
}

func TestRestoreCheckpointRejectsAnotherWorkspaceWithoutSession(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	workspaceA := t.TempDir()
	creator := New(nil, nil, 8192)
	creator.SetWorkDir(workspaceA)
	creator.SetCheckpointStore(store, 0)
	creator.AddUserMessage("workspace A secret")
	id, err := creator.CreateCheckpoint(context.Background(), "foreign", db.CheckpointManual)
	if err != nil {
		t.Fatal(err)
	}

	consumer := New(nil, nil, 8192)
	consumer.SetWorkDir(t.TempDir())
	consumer.SetCheckpointStore(store, 0)
	consumer.AddUserMessage("workspace B remains")
	if _, err := consumer.RestoreCheckpoint(context.Background(), id); err == nil {
		t.Fatal("fresh workspace restored a foreign checkpoint")
	}
	msgs := consumer.Messages()
	if len(msgs) != 1 || msgs[0].Content != "workspace B remains" {
		t.Fatalf("foreign restore changed transcript: %#v", msgs)
	}
}
