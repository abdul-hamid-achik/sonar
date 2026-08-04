package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type commitTestClient struct {
	chat func(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error
}

func (c commitTestClient) ChatStream(ctx context.Context, opts llm.ChatOptions, fn func(llm.StreamChunk) error) error {
	return c.chat(ctx, opts, fn)
}

func (commitTestClient) Ping() error { return nil }
func (commitTestClient) Model() string {
	return "test-model"
}
func (commitTestClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type commitTestGit struct {
	diff   func(context.Context) (string, error)
	commit func(context.Context, string) error
}

func (g commitTestGit) StagedDiff(ctx context.Context) (string, error) {
	return g.diff(ctx)
}

func (g commitTestGit) Commit(ctx context.Context, msg string) error {
	return g.commit(ctx, msg)
}

func TestRunCommitBoundsEveryStage(t *testing.T) {
	assertDeadline := func(t *testing.T, ctx context.Context) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("commit stage has no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > time.Second {
			t.Fatalf("unexpected stage deadline: %s", remaining)
		}
	}

	client := commitTestClient{chat: func(ctx context.Context, _ llm.ChatOptions, fn func(llm.StreamChunk) error) error {
		assertDeadline(t, ctx)
		return fn(llm.StreamChunk{Text: "fix: bounded commit"})
	}}
	git := commitTestGit{
		diff: func(ctx context.Context) (string, error) {
			assertDeadline(t, ctx)
			return "file.go | 1 +\n\ndiff --git a/file.go b/file.go", nil
		},
		commit: func(ctx context.Context, msg string) error {
			assertDeadline(t, ctx)
			if !strings.Contains(msg, "Assisted-by: sonar (qwen)") {
				t.Fatalf("commit message missing attribution: %q", msg)
			}
			return nil
		},
	}

	msg := runCommitWithGit(
		context.Background(), client, "qwen", "", 42, git,
		commitTimeouts{diff: time.Second, message: time.Second, commit: time.Second},
	)().(CommitResultMsg)
	if msg.Err != nil || msg.Token != 42 {
		t.Fatalf("commit result = %#v", msg)
	}
}

func TestRunCommitCancellationBeforeGitMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	messageStarted := make(chan struct{})
	client := commitTestClient{chat: func(ctx context.Context, _ llm.ChatOptions, _ func(llm.StreamChunk) error) error {
		close(messageStarted)
		<-ctx.Done()
		return ctx.Err()
	}}
	var commitCalls atomic.Int32
	git := commitTestGit{
		diff: func(context.Context) (string, error) { return "staged diff", nil },
		commit: func(context.Context, string) error {
			commitCalls.Add(1)
			return nil
		},
	}

	result := make(chan CommitResultMsg, 1)
	go func() {
		result <- runCommitWithGit(
			ctx, client, "qwen", "", 7, git,
			commitTimeouts{diff: time.Second, message: time.Second, commit: time.Second},
		)().(CommitResultMsg)
	}()
	<-messageStarted
	cancel()
	msg := <-result
	if msg.Err == nil || !errors.Is(msg.Err, context.Canceled) {
		t.Fatalf("cancel result = %#v", msg)
	}
	if got := commitCalls.Load(); got != 0 {
		t.Fatalf("git commit dispatched %d times after cancellation", got)
	}
}

func TestRunCommitDoesNotMutateWhenClientSwallowsDeadline(t *testing.T) {
	client := commitTestClient{chat: func(ctx context.Context, _ llm.ChatOptions, _ func(llm.StreamChunk) error) error {
		<-ctx.Done()
		return nil // Simulate a provider that fails to propagate ctx.Err().
	}}
	var commitCalls atomic.Int32
	git := commitTestGit{
		diff: func(context.Context) (string, error) { return "staged diff", nil },
		commit: func(context.Context, string) error {
			commitCalls.Add(1)
			return nil
		},
	}
	msg := runCommitWithGit(
		context.Background(), client, "qwen", "", 8, git,
		commitTimeouts{diff: time.Second, message: 10 * time.Millisecond, commit: time.Second},
	)().(CommitResultMsg)
	if msg.Err == nil || !errors.Is(msg.Err, context.DeadlineExceeded) {
		t.Fatalf("deadline result = %#v", msg)
	}
	if got := commitCalls.Load(); got != 0 {
		t.Fatalf("git commit dispatched %d times after LLM deadline", got)
	}
}

func TestRunCommitPanicStillReturnsOwnedReceipt(t *testing.T) {
	client := commitTestClient{chat: func(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error {
		panic("provider exploded")
	}}
	git := commitTestGit{
		diff:   func(context.Context) (string, error) { return "staged diff", nil },
		commit: func(context.Context, string) error { return nil },
	}
	msg := runCommitWithGit(
		context.Background(), client, "qwen", "", 11, git,
		commitTimeouts{diff: time.Second, message: time.Second, commit: time.Second},
	)().(CommitResultMsg)
	if msg.Token != 11 || msg.Err == nil || !strings.Contains(msg.Err.Error(), "provider exploded") {
		t.Fatalf("panic receipt = %#v", msg)
	}
}

func TestRunCommitRemoteProviderFailureCannotEnterTranscriptOrSession(t *testing.T) {
	const providerSecret = "REMOTE_COMMIT_SECRET /Users/provider/private.key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, providerSecret, http.StatusUnauthorized)
	}))
	defer server.Close()

	client, err := llm.NewOpenAICompatibleClient(llm.OpenAICompatibleOptions{
		BaseURL: server.URL,
		Model:   "grok-test",
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	var commitCalls atomic.Int32
	git := commitTestGit{
		diff: func(context.Context) (string, error) {
			return "file.go | 1 +\n\ndiff --git a/file.go b/file.go", nil
		},
		commit: func(context.Context, string) error {
			commitCalls.Add(1)
			return nil
		},
	}
	msg := runCommitWithGit(
		context.Background(),
		client,
		"grok-test",
		"",
		17,
		git,
		commitTimeouts{diff: time.Second, message: time.Second, commit: time.Second},
	)().(CommitResultMsg)
	if msg.Err == nil || !strings.Contains(msg.Err.Error(), ProviderFailureCopy) {
		t.Fatalf("remote commit receipt = %#v, want host-owned provider copy", msg)
	}
	if strings.Contains(msg.Err.Error(), providerSecret) {
		t.Fatalf("remote provider body crossed CommitResultMsg: %v", msg.Err)
	}
	if got := commitCalls.Load(); got != 0 {
		t.Fatalf("git commit dispatched %d times after provider failure", got)
	}

	m := newTestModel(t)
	m.commitRunning = true
	m.commitToken = msg.Token
	m.handleCommitResult(msg, nil)
	if len(m.entries) != 1 || !strings.Contains(m.entries[0].Content, ProviderFailureCopy) {
		t.Fatalf("commit transcript entry = %#v", m.entries)
	}
	state, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state, providerSecret) {
		t.Fatalf("remote provider body crossed durable session boundary: %s", state)
	}
	if !strings.Contains(state, ProviderFailureCopy) {
		t.Fatalf("durable session omitted host-owned provider failure: %s", state)
	}
}

func TestShutdownCancelsAndJoinsCommitAndAgentEffects(t *testing.T) {
	m := newTestModel(t)
	commitStarted := make(chan context.Context, 1)
	m.commitRunner = func(ctx context.Context, _ llm.Client, _, _, _ string, token uint64) tea.Cmd {
		return func() tea.Msg {
			commitStarted <- ctx
			<-ctx.Done()
			return CommitResultMsg{Token: token, Err: ctx.Err()}
		}
	}
	commitCmd := m.handleCommandAction(command.Result{Action: command.ActionCommit})
	if commitCmd == nil || !m.commitRunning {
		t.Fatal("commit effect was not registered before dispatch")
	}
	commitMessages := commandMessages(commitCmd)
	commitCtx := <-commitStarted

	agentCtx, cancelAgent := context.WithCancel(context.Background())
	m.cancel = cancelAgent
	m.state = StateStreaming
	updated, quitCmd := m.Update(ShutdownMsg{})
	m = updated.(*Model)
	if quitCmd == nil || m.shutdownReady() {
		t.Fatal("shutdown did not remain active with liveness while owned effects join")
	}
	select {
	case <-commitCtx.Done():
	default:
		t.Fatal("shutdown did not cancel commit effect")
	}
	select {
	case <-agentCtx.Done():
	default:
		t.Fatal("shutdown did not cancel agent effect")
	}

	commitResult := awaitCommandMessage[CommitResultMsg](t, commitMessages, 2*time.Second)
	updated, quitCmd = m.Update(commitResult)
	m = updated.(*Model)
	if quitCmd != nil {
		t.Fatal("shutdown quit before agent joined")
	}
	updated, quitCmd = m.Update(AgentDoneMsg{Err: context.Canceled})
	m = updated.(*Model)
	if quitCmd == nil || m.commitRunning || m.cancel != nil {
		t.Fatalf("shutdown did not quit after all effects joined: commit=%v cancel=%v", m.commitRunning, m.cancel)
	}
}

func TestCommitResultWithStaleTokenCannotReleaseShutdown(t *testing.T) {
	m := newTestModel(t)
	m.shuttingDown = true
	m.commitRunning = true
	m.commitToken = 9
	updated, cmd := m.Update(CommitResultMsg{Token: 8})
	m = updated.(*Model)
	if cmd != nil || !m.commitRunning {
		t.Fatal("stale commit receipt released shutdown ownership")
	}
}

func TestCommitOwnsInputAndEscapeCancelsUntilReceipt(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.commitRunning = true
	m.commitCancel = cancel
	m.input.Focus()
	updated, _ := m.Update(charKey('x'))
	m = updated.(*Model)
	if m.input.Value() != "" {
		t.Fatalf("input changed during owned commit: %q", m.input.Value())
	}
	updated, cmd := m.Update(escKey())
	m = updated.(*Model)
	if cmd != nil || !m.commitRunning {
		t.Fatal("Escape released commit ownership before receipt")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Escape did not cancel commit context")
	}
}
