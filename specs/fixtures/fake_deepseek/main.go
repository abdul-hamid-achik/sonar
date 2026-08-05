// Command fake-deepseek runs sonar against a deterministic, loopback-only
// OpenAI-compatible provider.
//
// It exists because sonar's interactive terminal specs were inherited from
// local-agent, where a turn succeeds with no credential because Ollama is
// local. sonar exits 1 without one, so those specs died at their second step;
// pointing them at a dead port only moved the failure, because a spec that
// sends a prompt and then tests what the UI does next needs the turn to
// actually complete.
//
// The DeepSeek wire contract is honoured rather than approximated. An assistant
// message that carries tool calls must echo its own reasoning_content or the
// real API answers 400, and the harness sends an empty string for a restored
// turn. This fixture asserts the field is present on any request whose history
// contains an assistant tool call, so a regression in that contract fails here
// instead of against a metered endpoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const fixtureModel = "deepseek-v4-flash"

type fixtureState struct {
	mu            sync.Mutex
	chatRequests  int
	protocolError string
}

func (s *fixtureState) nextChat() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatRequests++
	return s.chatRequests
}

func (s *fixtureState) fail(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.protocolError == "" {
		s.protocolError = fmt.Sprintf(format, args...)
	}
}

func (s *fixtureState) writeReceipt(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := s.chatRequests > 0 && s.protocolError == ""
	content := fmt.Sprintf(
		"protocol_ok=%t\nchat_requests=%d\nprotocol_error=%s\n",
		ok, s.chatRequests, strings.ReplaceAll(s.protocolError, "\n", " "),
	)
	return os.WriteFile(path, []byte(content), 0o600)
}

// wireMessage mirrors only what this fixture inspects. reasoning_content is a
// pointer so an absent field and an empty string stay distinguishable — that
// difference is the entire DeepSeek contract.
type wireMessage struct {
	Role             string  `json:"role"`
	Content          string  `json:"content"`
	ReasoningContent *string `json:"reasoning_content"`
	ToolCalls        []struct {
		ID string `json:"id"`
	} `json:"tool_calls"`
}

type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: fake-deepseek SONAR_BINARY RECEIPT_PATH [ARG...]")
		return 2
	}
	// Anything after the receipt path is forwarded, so a spec that needs
	// `--resume <id>` can still run under the fixture instead of launching the
	// binary directly and inheriting whatever the developer's shell exports.
	binary, receiptPath := os.Args[1], os.Args[2]
	forwarded := os.Args[3:]

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake DeepSeek listen: %v\n", err)
		return 1
	}
	state := &fixtureState{}
	server := &http.Server{Handler: fixtureHandler(state), ReadHeaderTimeout: 2 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	command := exec.Command(binary, forwarded...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	// The provider is selected entirely through the environment so the spec's
	// config fixture stays about the behaviour under test.
	env := hermeticEnv()
	env = replaceEnv(env, "SONAR_PROVIDER", "deepseek")
	env = replaceEnv(env, "SONAR_PROVIDER_BASE_URL", "http://"+listener.Addr().String()+"/v1")
	env = replaceEnv(env, "SONAR_PROVIDER_MODEL", fixtureModel)
	env = replaceEnv(env, "SONAR_PROVIDER_API_KEY_ENV", "FAKE_DEEPSEEK_API_KEY")
	env = replaceEnv(env, "FAKE_DEEPSEEK_API_KEY", "fixture-key-never-leaves-loopback")
	command.Env = env
	childErr := command.Run()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	if serveErr := <-serveDone; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		state.fail("serve fake DeepSeek: %v", serveErr)
	}
	if shutdownErr != nil {
		state.fail("shutdown fake DeepSeek: %v", shutdownErr)
	}

	if err := state.writeReceipt(receiptPath); err != nil {
		fmt.Fprintf(os.Stderr, "write fake DeepSeek receipt: %v\n", err)
		return 1
	}
	if childErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(childErr, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "wait for sonar: %v\n", childErr)
	return 1
}

func fixtureHandler(state *fixtureState) http.Handler {
	mux := http.NewServeMux()
	// Reachability probe. sonar pings before it will call a provider live.
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": fixtureModel, "object": "model"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		handleChat(state, w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		state.fail("unexpected request path %q", r.URL.Path)
		http.NotFound(w, r)
	})
	return mux
}

func handleChat(state *fixtureState, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		state.fail("read chat body: %v", err)
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var request wireRequest
	if err := json.Unmarshal(body, &request); err != nil {
		state.fail("decode chat body: %v", err)
		http.Error(w, "undecodable body", http.StatusBadRequest)
		return
	}
	if request.Model != fixtureModel {
		state.fail("chat model %q, want %q", request.Model, fixtureModel)
	}
	// The contract that costs a real 400: an assistant turn carrying tool calls
	// must send reasoning_content back, even when it is empty.
	for _, message := range request.Messages {
		if message.Role == "assistant" && len(message.ToolCalls) > 0 && message.ReasoningContent == nil {
			state.fail("assistant tool-call message omitted reasoning_content")
		}
	}

	index := state.nextChat()
	answer := fmt.Sprintf("Fixture answer %d.", index)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		state.fail("response writer cannot stream")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	writeChunk(w, flusher, map[string]any{
		"id": "fixture", "object": "chat.completion.chunk", "model": fixtureModel,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": answer}}},
	})
	writeChunk(w, flusher, map[string]any{
		"id": "fixture", "object": "chat.completion.chunk", "model": fixtureModel,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		// A terminal usage receipt is mandatory: sonar refuses a stream that
		// ends without one rather than guessing what the turn cost.
		"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeChunk(w io.Writer, flusher http.Flusher, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "data: "+string(encoded)+"\n\n")
	flusher.Flush()
}

// hermeticEnv strips every provider credential and provider override the
// ambient shell may carry before a fixture declares its own.
//
// Without it a spec is not hermetic: this suite runs on a developer machine
// where DEEPSEEK_API_KEY is routinely exported, os.Environ() passes it
// straight through, and sonar then configures a real hosted provider the spec
// never asked for. Observed consequences, in increasing order of seriousness:
// the top bar reads "DEEPSEEK · remote prompts · qwen3.5:0.8b" because the
// fake server owns the inventory while a different provider dispatches; the
// same spec passes or fails depending on whether a key happens to be
// exported; and a test run can send a prompt to a metered endpoint with the
// developer's real credential.
//
// A deterministic terminal suite must not be able to bill you.
func hermeticEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if strings.HasPrefix(key, "SONAR_PROVIDER") ||
			strings.HasSuffix(key, "_API_KEY") ||
			strings.HasSuffix(key, "_API_TOKEN") {
			// OLLAMA_HOST is deliberately NOT stripped: specs point it at a
			// dead port to keep the local runtime out of the picture, and
			// removing it would let sonar reach a developer's real daemon.

			continue
		}
		out = append(out, entry)
	}
	return out
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}
