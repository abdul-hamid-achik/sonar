package ice

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type sessionTestClient struct {
	embedCalls atomic.Int64
}

func (*sessionTestClient) ChatStream(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error {
	return nil
}

func (*sessionTestClient) Ping() error   { return nil }
func (*sessionTestClient) Model() string { return "session-test" }

func (c *sessionTestClient) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	c.embedCalls.Add(1)
	embeddings := make([][]float32, len(texts))
	for i := range texts {
		embeddings[i] = []float32{1, 0}
	}
	return embeddings, nil
}

func newSessionTestEngine(t *testing.T, client llm.Client, sessionID string) *Engine {
	t.Helper()
	workspace := t.TempDir()
	engine, err := NewEngine(client, nil, EngineConfig{
		StorePath: filepath.Join(workspace, "conversations.json"),
		Workspace: workspace,
		NumCtx:    16_384,
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close ICE engine: %v", err)
		}
	})
	return engine
}

func TestAssembleContextWithPromptTokensSkipsRetrievalWhenBudgetExhausted(t *testing.T) {
	client := &sessionTestClient{}
	engine := newSessionTestEngine(t, client, "durable:current")
	if _, err := engine.store.AddScoped(
		engine.projectID,
		"durable:other",
		"user",
		"relevant context that must not be retrieved under context pressure",
		[]float32{1, 0},
		1,
	); err != nil {
		t.Fatal(err)
	}

	assembled, err := engine.AssembleContextWithPromptTokens(context.Background(), "relevant context", 12_288)
	if err != nil {
		t.Fatal(err)
	}
	if assembled != "" {
		t.Fatalf("assembled context = %q, want empty string", assembled)
	}
	if calls := client.embedCalls.Load(); calls != 0 {
		t.Fatalf("embedding calls = %d, want zero when optional budget is exhausted", calls)
	}

	assembled, err = engine.AssembleContext(context.Background(), "relevant context")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(assembled, "must not be retrieved") {
		t.Fatalf("compatibility assembly did not retrieve available context: %q", assembled)
	}
	if calls := client.embedCalls.Load(); calls != 1 {
		t.Fatalf("compatibility embedding calls = %d, want one", calls)
	}
}

func TestEngineDurableSessionResumeSwitchAndExclusion(t *testing.T) {
	client := &sessionTestClient{}
	engine := newSessionTestEngine(t, client, "db:42")
	if got := engine.SessionID(); got != "db:42" {
		t.Fatalf("initial session = %q, want db:42", got)
	}

	entries := []struct {
		session string
		content string
		turn    int
	}{
		{session: "db:42", content: "alpha current-session context", turn: 7},
		{session: "db:43", content: "alpha other-session context", turn: 4},
	}
	for _, entry := range entries {
		if _, err := engine.store.AddScoped(
			engine.projectID,
			entry.session,
			"user",
			entry.content,
			[]float32{1, 0},
			entry.turn,
		); err != nil {
			t.Fatal(err)
		}
	}

	assembled, err := engine.AssembleContextWithPromptTokens(context.Background(), "alpha", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(assembled, "current-session") || !strings.Contains(assembled, "other-session") {
		t.Fatalf("db:42 assembled context did not exclude itself: %q", assembled)
	}

	if err := engine.SetSessionID(context.Background(), "db:43"); err != nil {
		t.Fatal(err)
	}
	assembled, err = engine.AssembleContextWithPromptTokens(context.Background(), "alpha", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(assembled, "current-session") || strings.Contains(assembled, "other-session") {
		t.Fatalf("db:43 assembled context did not exclude itself: %q", assembled)
	}

	if err := engine.SetSessionID(context.Background(), "db:42"); err != nil {
		t.Fatal(err)
	}
	if err := engine.IndexMessage(context.Background(), "assistant", "resumed session answer"); err != nil {
		t.Fatal(err)
	}
	results := engine.store.SearchScoped([]float32{1, 0}, engine.projectID, "", 20)
	foundResumed := false
	for _, result := range results {
		if result.Entry.Content != "resumed session answer" {
			continue
		}
		foundResumed = true
		if result.Entry.SessionID != "db:42" || result.Entry.TurnIndex != 8 {
			t.Fatalf("resumed entry = %#v, want db:42 turn 8", result.Entry)
		}
	}
	if !foundResumed {
		t.Fatal("resumed session entry was not stored")
	}
}

func TestSetSessionIDHonorsCancelledContext(t *testing.T) {
	engine := newSessionTestEngine(t, &sessionTestClient{}, "db:before")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := engine.SetSessionID(ctx, "db:after"); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetSessionID error = %v, want context cancellation", err)
	}
	if got := engine.SessionID(); got != "db:before" {
		t.Fatalf("session changed to %q after cancelled bind", got)
	}
}

type blockingSessionClient struct {
	started     chan struct{}
	startedOnce sync.Once
}

func (*blockingSessionClient) ChatStream(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error {
	return nil
}

func (*blockingSessionClient) Ping() error   { return nil }
func (*blockingSessionClient) Model() string { return "blocking-session-test" }

func (c *blockingSessionClient) Embed(ctx context.Context, _ string, _ []string) ([][]float32, error) {
	c.startedOnce.Do(func() { close(c.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSessionSwitchCancelsInFlightIndexingWithoutCrossSessionWrite(t *testing.T) {
	client := &blockingSessionClient{started: make(chan struct{})}
	engine := newSessionTestEngine(t, client, "db:old")
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.IndexMessage(context.Background(), "user", "must not cross the session boundary")
	}()
	<-client.started

	if err := engine.SetSessionID(context.Background(), "db:new"); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; !errors.Is(err, ErrICESessionChanged) {
		t.Fatalf("in-flight index error = %v, want ErrICESessionChanged", err)
	}
	if got := engine.store.CountScoped(engine.projectID); got != 0 {
		t.Fatalf("stored entries = %d, want zero after cancelled indexing", got)
	}
}

func TestEngineSessionBindingIsConcurrentSafe(t *testing.T) {
	engine := newSessionTestEngine(t, &sessionTestClient{}, "db:0")
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 10; iteration++ {
				sessionID := fmt.Sprintf("db:%d:%d", worker, iteration)
				if err := engine.SetSessionID(context.Background(), sessionID); err != nil {
					t.Errorf("SetSessionID(%q): %v", sessionID, err)
					return
				}
				if got := engine.SessionID(); got == "" {
					t.Error("SessionID returned an empty identifier")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestEngineCloseAndSessionBindingAreConcurrentSafe(t *testing.T) {
	engine := newSessionTestEngine(t, &sessionTestClient{}, "db:open")
	start := make(chan struct{})
	errCh := make(chan error, 8)
	var wg sync.WaitGroup

	for worker := 0; worker < 4; worker++ {
		worker := worker
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errCh <- engine.SetSessionID(context.Background(), fmt.Sprintf("db:close-race:%d", worker))
		}()
		go func() {
			defer wg.Done()
			<-start
			errCh <- engine.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("concurrent close/session error = %v", err)
		}
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	if err := engine.SetSessionID(context.Background(), "db:after-close"); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetSessionID after Close error = %v, want context.Canceled", err)
	}
}
