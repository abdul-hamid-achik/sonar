package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestParseGeneratedSessionTitle(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain", raw: "Polish composer layout", want: "Polish composer layout"},
		{name: "quoted", raw: `"Fix sticky user chrome"`, want: "Fix sticky user chrome"},
		{name: "prefixed", raw: "Title: Tighten transcript spacing", want: "Tighten transcript spacing"},
		{name: "trailing period", raw: "Rename sessions with AI.", want: "Rename sessions with AI"},
		{name: "multiline takes first", raw: "\nFix auth redirects\nextra junk", want: "Fix auth redirects"},
		{name: "none rejected", raw: "NONE", want: ""},
		{name: "symbols rejected", raw: "***", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGeneratedSessionTitle(tt.raw); got != tt.want {
				t.Fatalf("parseGeneratedSessionTitle(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestProvisionalSessionTitleStillUsesFirstPromptLine(t *testing.T) {
	got := sessionTitle("\nPolish the composer\nwith more detail")
	if got != "Polish the composer" {
		t.Fatalf("sessionTitle() = %q, want provisional first line", got)
	}
}

func TestScheduleSessionTitleGenRequiresProvisionalFlag(t *testing.T) {
	m := newTestModel(t)
	m.sessionID = 7
	m.sessionTitleNeedsAI = false
	if cmd := m.scheduleSessionTitleGen(); cmd != nil {
		t.Fatal("schedule should no-op when title is already final")
	}
	m.sessionTitleNeedsAI = true
	m.sessionTitleAIDone = true
	if cmd := m.scheduleSessionTitleGen(); cmd != nil {
		t.Fatal("schedule should no-op when AI title already applied")
	}
}

func TestHandleSessionTitleJobUpdatesActiveTitle(t *testing.T) {
	store, err := db.OpenPath(t.TempDir() + "/title.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "hey chat!", Model: "test", Mode: "NORMAL", WorkspaceID: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.sessionStore = store
	m.sessionID = session.ID
	m.activeSessionTitle = "hey chat!"
	m.sessionTitleNeedsAI = true
	m.sessionTitleGenToken = 3

	m.handleSessionTitleJob(sessionTitleJobMsg{
		Token: 3, SessionID: session.ID, Title: "Greet and orient session",
	})
	if m.activeSessionTitle != "Greet and orient session" {
		t.Fatalf("active title = %q", m.activeSessionTitle)
	}
	if m.sessionTitleNeedsAI || !m.sessionTitleAIDone {
		t.Fatalf("flags needsAI=%v aiDone=%v", m.sessionTitleNeedsAI, m.sessionTitleAIDone)
	}
	durable, err := store.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Title != "Greet and orient session" {
		t.Fatalf("durable title = %q", durable.Title)
	}
}

func TestHandleSessionTitleJobIgnoresStaleToken(t *testing.T) {
	m := newTestModel(t)
	m.sessionID = 9
	m.activeSessionTitle = "provisional"
	m.sessionTitleNeedsAI = true
	m.sessionTitleGenToken = 2
	m.handleSessionTitleJob(sessionTitleJobMsg{
		Token: 1, SessionID: 9, Title: "Should not apply",
	})
	if m.activeSessionTitle != "provisional" {
		t.Fatalf("stale job renamed session to %q", m.activeSessionTitle)
	}
}

type titleStreamClient struct {
	response string
	started  chan struct{}
	block    chan struct{}
}

func (c *titleStreamClient) ChatStream(ctx context.Context, _ llm.ChatOptions, fn func(llm.StreamChunk) error) error {
	if c.started != nil {
		close(c.started)
	}
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.response != "" && fn != nil {
		return fn(llm.StreamChunk{Text: c.response})
	}
	return nil
}

func (*titleStreamClient) Ping() error   { return nil }
func (*titleStreamClient) Model() string { return "title-test" }
func (*titleStreamClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestGenerateSessionTitleParsesModelOutput(t *testing.T) {
	client := &titleStreamClient{response: "Title: \"Fix session naming\"\n"}
	got, err := generateSessionTitle(context.Background(), client, sessionTitleRequest{
		Workspace: "sonar",
		Mode:      "NORMAL",
		User:      "hey chat! can you rename sessions with AI?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Fix session naming" {
		t.Fatalf("title = %q", got)
	}
}

func TestBuildSessionTitleUserPromptIncludesContext(t *testing.T) {
	prompt := buildSessionTitleUserPrompt(sessionTitleRequest{
		Workspace: "sonar",
		Mode:      "PLAN",
		Profile:   "ornith",
		User:      "tighten sticky chrome",
		Assistant: "I'll densify separators.",
	})
	for _, want := range []string{
		"Workspace: sonar",
		"Mode: PLAN",
		"Profile: ornith",
		"tighten sticky chrome",
		"I'll densify separators.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestEnsureExecutionSessionMarksProvisionalTitleForAI(t *testing.T) {
	store, err := db.OpenPath(t.TempDir() + "/provisional.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	m := newTestModel(t)
	m.sessionStore = store
	created, err := m.ensureExecutionSession("hey chat! what is this project?", "NORMAL")
	if err != nil || !created {
		t.Fatalf("ensureExecutionSession created=%v err=%v", created, err)
	}
	if !m.sessionTitleNeedsAI || m.sessionTitleAIDone {
		t.Fatalf("needsAI=%v aiDone=%v", m.sessionTitleNeedsAI, m.sessionTitleAIDone)
	}
	if m.activeSessionTitle != "hey chat! what is this project?" {
		t.Fatalf("provisional title = %q", m.activeSessionTitle)
	}
}

func TestCancelSessionTitleGenAbortsInflight(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	client := &titleStreamClient{started: started, block: block, response: "Should not win"}

	// Direct generate with cancel.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := generateSessionTitle(ctx, client, sessionTitleRequest{User: "hello"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("title gen did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("title gen did not observe cancel")
	}
	close(block)
}
