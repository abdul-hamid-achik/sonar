package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/charmbracelet/log"
)

type localInferenceFailureClient struct {
	err error
}

func (client *localInferenceFailureClient) ChatStream(
	context.Context,
	llm.ChatOptions,
	func(llm.StreamChunk) error,
) error {
	return client.err
}

func (*localInferenceFailureClient) Ping() error { return nil }
func (*localInferenceFailureClient) Model() string {
	return "local-test"
}
func (*localInferenceFailureClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type inferenceFailureOutput struct {
	errors  []string
	systems []string
}

func (*inferenceFailureOutput) StreamText(string)                            {}
func (*inferenceFailureOutput) StreamReasoning(string)                       {}
func (*inferenceFailureOutput) StreamDone(int, int)                          {}
func (*inferenceFailureOutput) ToolCallStart(string, string, map[string]any) {}
func (*inferenceFailureOutput) ToolCallResult(string, string, string, bool, time.Duration) {
}
func (output *inferenceFailureOutput) SystemMessage(message string) {
	output.systems = append(output.systems, message)
}
func (output *inferenceFailureOutput) Error(message string) {
	output.errors = append(output.errors, message)
}

func TestLocalInferenceFailureRetainsActionableOllamaDiagnostic(t *testing.T) {
	localErr := errors.New("ollama local socket /tmp/ollama.sock: connection refused")
	runtime := New(&localInferenceFailureClient{err: localErr}, nil, 32*1024)
	runtime.AddUserMessage("Answer briefly.")
	output := &inferenceFailureOutput{}

	runErr := runtime.Run(t.Context(), output)
	if runErr == nil {
		t.Fatal("local inference failure unexpectedly succeeded")
	}
	visible := strings.Join(append(append([]string(nil), output.errors...), output.systems...), "\n")
	for _, want := range []string{"ollama local socket", "/tmp/ollama.sock", "connection refused"} {
		if !strings.Contains(visible, want) || !strings.Contains(runErr.Error(), want) {
			t.Fatalf("local diagnostic lost %q: output=%q err=%v", want, visible, runErr)
		}
	}
	if strings.Contains(visible, RemoteInferenceFailureCopy) ||
		errors.Is(runErr, ErrRemoteInferenceFailed) {
		t.Fatalf("local error was misclassified as remote: output=%q err=%v", visible, runErr)
	}
}

func TestRemoteCompactionFailureUsesHostAuthoredCopy(t *testing.T) {
	const providerPayload = "COMPACTION_REMOTE_SECRET /Users/provider/private.key \x1b]0;owned\x07"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"error": map[string]any{"message": providerPayload},
		})
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
	runtime := New(client, nil, 32*1024)
	for index := range keepMessages + 2 {
		runtime.AppendMessage(llm.Message{
			Role:    "user",
			Content: "historical user message " + strings.Repeat("context ", index+1),
		})
		runtime.AppendMessage(llm.Message{
			Role:    "assistant",
			Content: "historical assistant answer",
		})
	}
	output := &inferenceFailureOutput{}

	if runtime.compactForContextAndModel(t.Context(), output, 32*1024, "grok-test") {
		t.Fatal("failed remote compaction unexpectedly replaced history")
	}
	visible := strings.Join(output.errors, "\n")
	if !strings.Contains(visible, RemoteInferenceFailureCopy) {
		t.Fatalf("compaction error omitted host-authored copy: %q", visible)
	}
	for _, forbidden := range []string{"COMPACTION_REMOTE_SECRET", "/Users/provider", "\x1b", "owned"} {
		if strings.Contains(visible, forbidden) {
			t.Fatalf("compaction error retained provider payload %q: %q", forbidden, visible)
		}
	}
}

func TestRemoteInferenceFailureCannotEnterPersistentLogger(t *testing.T) {
	const providerPayload = "REMOTE_LOG_SECRET /Users/provider/private.key"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"error": map[string]any{"message": providerPayload},
		})
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
	runtime := New(client, nil, 32*1024)
	runtime.AddUserMessage("Answer briefly.")
	var logs bytes.Buffer
	runtime.SetLogger(log.NewWithOptions(&logs, log.Options{
		ReportTimestamp: false,
		Level:           log.DebugLevel,
	}))

	runErr := runtime.Run(t.Context(), &inferenceFailureOutput{})
	if runErr == nil || !errors.Is(runErr, ErrRemoteInferenceFailed) {
		t.Fatalf("Run error = %v, want remote inference boundary", runErr)
	}
	logged := logs.String()
	if !strings.Contains(logged, RemoteInferenceFailureCopy) {
		t.Fatalf("persistent log omitted host-authored failure identity: %q", logged)
	}
	for _, forbidden := range []string{"REMOTE_LOG_SECRET", "/Users/provider", "private.key"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("persistent log retained remote payload %q: %q", forbidden, logged)
		}
	}
}
