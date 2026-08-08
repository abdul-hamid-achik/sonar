package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// The local-runtime bootstrap surface is deleted, not gated. sonar's opening
// frame once said, on a correctly configured session:
//
//	main · sonar   DEEPSEEK · remote prompts · deepseek-v4-flash · 0/1.0M · 0%
//	No local model installed
//	press p to pull qwen3.5:2b (~2.7 GB)
//
// — an instruction to download 2.7 GB nobody needed, under a top bar saying
// the model was already resolved. The predicate was first guarded for remote
// providers and then removed outright, because RemoteProvider() is
// constant-true here: the guarded branch could only ever run in a fixture.
// This scan is what fails if the block rides back in on a merge from
// local-agent, where the same file legitimately keeps it.
func TestWelcomeNeverOffersALocalModelPull(t *testing.T) {
	m := newTestModel(t)
	m.ollamaModels = nil
	m.ollamaInventoryAttempted = true
	withRemoteProvider(t, m)

	var b strings.Builder
	m.renderWelcome(&b)
	frame := b.String()
	for _, forbidden := range []string{"No local model installed", "press p to pull"} {
		if strings.Contains(frame, forbidden) {
			t.Errorf("welcome frame resurrected %q:\n%s", forbidden, frame)
		}
	}
}

// stubRemoteClient is the smallest thing ModelManager will accept as an
// attached remote provider. Nothing here is exercised: the test only needs
// RemoteProvider() to answer true, which is what a configured harness looks
// like from the welcome frame's point of view.
type stubRemoteClient struct{ model string }

func (s *stubRemoteClient) ChatStream(context.Context, llm.ChatOptions, func(llm.StreamChunk) error) error {
	return errors.New("stub remote client does not chat")
}
func (s *stubRemoteClient) Ping() error                       { return nil }
func (s *stubRemoteClient) PingContext(context.Context) error { return nil }
func (s *stubRemoteClient) Model() string                     { return s.model }
func (s *stubRemoteClient) SetModel(model string) error       { s.model = model; return nil }
func (s *stubRemoteClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, errors.New("stub remote client does not embed")
}

func withRemoteProvider(t *testing.T, m *Model) {
	t.Helper()
	manager := llm.NewModelManager("http://127.0.0.1:1", 8192)
	if err := manager.ConfigureRemoteProvider(&stubRemoteClient{model: "deepseek-v4-flash"}, 1_000_000, "deepseek"); err != nil {
		t.Fatalf("attach remote provider: %v", err)
	}
	if !manager.RemoteProvider() {
		t.Fatal("the manager did not report the attached provider as remote")
	}
	m.modelManager = manager
}
