package ui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/rivo/uniseg"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type inferenceBoundarySnapshotMsg struct {
	reply chan<- inferenceBoundarySnapshot
}

type inferenceBoundarySnapshot struct {
	entries []ChatEntry
	session string
	err     error
}

type inferenceBoundaryProbe struct {
	model *Model
	ready chan struct{}
}

func (probe *inferenceBoundaryProbe) Init() tea.Cmd {
	close(probe.ready)
	return nil
}

func (probe *inferenceBoundaryProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if request, ok := message.(inferenceBoundarySnapshotMsg); ok {
		session, err := encodeSessionState(probe.model)
		request.reply <- inferenceBoundarySnapshot{
			entries: append([]ChatEntry(nil), probe.model.entries...),
			session: session,
			err:     err,
		}
		return probe, nil
	}
	updated, command := probe.model.Update(message)
	probe.model = updated.(*Model)
	return probe, command
}

func (*inferenceBoundaryProbe) View() tea.View {
	return tea.NewView("")
}

func TestRemoteInferenceErrorsNeverReachTranscriptOrSession(t *testing.T) {
	const (
		remoteSecret = "REMOTE_INFERENCE_SECRET_7d65"
		remotePath   = "/Users/provider/.ssh/id_ed25519"
	)
	providerMessage := remoteSecret + "\n" + remotePath +
		"\x1b]8;;https://attacker.invalid\x07click\x1b]8;;\x07\u202espoof"
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "http body",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"error": map[string]any{"message": providerMessage},
				})
			},
		},
		{
			name: "sse event",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				payload, _ := json.Marshal(map[string]any{
					"error": map[string]any{"message": providerMessage},
				})
				frame := append([]byte("data: "), payload...)
				frame = append(frame, '\n', '\n')
				_, _ = writer.Write(frame)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			client, err := llm.NewOpenAICompatibleClient(llm.OpenAICompatibleOptions{
				BaseURL: server.URL,
				Model:   "grok-test",
				APIKey:  "test-key",
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime := agent.New(client, nil, 32*1024)
			runtime.AddUserMessage("Please answer briefly.")
			model := newTestModel(t)
			model.agent = runtime
			model.state = StateWaiting

			probe := &inferenceBoundaryProbe{model: model, ready: make(chan struct{})}
			program := tea.NewProgram(
				probe,
				tea.WithInput(strings.NewReader("")),
				tea.WithOutput(io.Discard),
				tea.WithoutRenderer(),
				tea.WithoutSignalHandler(),
			)
			done := make(chan error, 1)
			go func() {
				_, runErr := program.Run()
				done <- runErr
			}()
			select {
			case <-probe.ready:
			case <-time.After(time.Second):
				t.Fatal("boundary probe did not start")
			}
			defer func() {
				program.Quit()
				select {
				case runErr := <-done:
					if runErr != nil {
						t.Errorf("boundary probe exit: %v", runErr)
					}
				case <-time.After(time.Second):
					t.Error("boundary probe did not stop")
				}
			}()

			runErr := runtime.Run(t.Context(), NewAdapter(program))
			if runErr == nil || !errors.Is(runErr, agent.ErrRemoteInferenceFailed) {
				t.Fatalf("agent error = %v, want safe remote sentinel", runErr)
			}
			program.Send(AgentDoneMsg{Err: runErr})
			reply := make(chan inferenceBoundarySnapshot, 1)
			program.Send(inferenceBoundarySnapshotMsg{reply: reply})
			var snapshot inferenceBoundarySnapshot
			select {
			case snapshot = <-reply:
			case <-time.After(time.Second):
				t.Fatal("boundary snapshot timed out")
			}
			if snapshot.err != nil {
				t.Fatalf("encode session: %v", snapshot.err)
			}
			entries, err := json.Marshal(snapshot.entries)
			if err != nil {
				t.Fatal(err)
			}

			for surface, content := range map[string]string{
				"entries": string(entries),
				"session": snapshot.session,
				"error":   runErr.Error(),
			} {
				for _, forbidden := range []string{
					remoteSecret, remotePath, "attacker.invalid", "\x1b", "\u202e",
				} {
					if strings.Contains(content, forbidden) {
						t.Fatalf("%s retained remote provider payload %q: %s", surface, forbidden, content)
					}
				}
				if !strings.Contains(content, agent.RemoteInferenceFailureCopy) {
					t.Fatalf("%s omitted fixed remote failure copy: %s", surface, content)
				}
			}
		})
	}
}

type adapterNoticeProbe struct {
	ready    chan struct{}
	messages chan tea.Msg
}

func (probe *adapterNoticeProbe) Init() tea.Cmd {
	close(probe.ready)
	return nil
}

func (probe *adapterNoticeProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message.(type) {
	case ErrorMsg, SystemMessageMsg:
		probe.messages <- message
	}
	return probe, nil
}

func (*adapterNoticeProbe) View() tea.View {
	return tea.NewView("")
}

func TestAdapterBoundsAndSanitizesTranscriptNotices(t *testing.T) {
	probe := &adapterNoticeProbe{
		ready:    make(chan struct{}),
		messages: make(chan tea.Msg, 2),
	}
	program := tea.NewProgram(
		probe,
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(io.Discard),
		tea.WithoutRenderer(),
		tea.WithoutSignalHandler(),
	)
	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()
	select {
	case <-probe.ready:
	case <-time.After(time.Second):
		t.Fatal("adapter notice probe did not start")
	}
	defer func() {
		program.Quit()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("adapter notice probe exit: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("adapter notice probe did not stop")
		}
	}()

	raw := "\x1b]0;owned\x07\u202e" +
		strings.Repeat("e\u0301", maxAdapterSystemNoticeGraphemes*3)
	adapter := NewAdapter(program)
	adapter.Error(raw)
	adapter.SystemMessage(raw)

	notices := make(map[string]string, 2)
	for range 2 {
		select {
		case message := <-probe.messages:
			switch typed := message.(type) {
			case ErrorMsg:
				notices["error"] = typed.Msg
			case SystemMessageMsg:
				notices["system"] = typed.Msg
			}
		case <-time.After(time.Second):
			t.Fatal("adapter notice timed out")
		}
	}
	for kind, limits := range map[string]struct {
		bytes     int
		graphemes int
	}{
		"error":  {bytes: maxAdapterErrorNoticeBytes, graphemes: maxAdapterErrorNoticeGraphemes},
		"system": {bytes: maxAdapterSystemNoticeBytes, graphemes: maxAdapterSystemNoticeGraphemes},
	} {
		notice := notices[kind]
		if !utf8.ValidString(notice) {
			t.Fatalf("%s notice is not valid UTF-8", kind)
		}
		if len(notice) > limits.bytes {
			t.Fatalf("%s notice bytes = %d, want <= %d", kind, len(notice), limits.bytes)
		}
		if count := uniseg.GraphemeClusterCount(notice); count > limits.graphemes {
			t.Fatalf("%s notice graphemes = %d, want <= %d", kind, count, limits.graphemes)
		}
		for _, forbidden := range []string{"\x1b", "\x07", "\u202e", "owned"} {
			if strings.Contains(notice, forbidden) {
				t.Fatalf("%s notice retained terminal control payload %q: %q", kind, forbidden, notice)
			}
		}
		if !strings.HasSuffix(notice, adapterNoticeTruncatedMarker) {
			t.Fatalf("%s notice omitted truncation marker: %q", kind, notice)
		}
		content := strings.TrimSuffix(notice, adapterNoticeTruncatedMarker)
		if content != "" && !strings.HasSuffix(content, "\u0301") {
			t.Fatalf("%s notice split a combining grapheme: %q", kind, content[len(content)-min(len(content), 16):])
		}
	}
}
