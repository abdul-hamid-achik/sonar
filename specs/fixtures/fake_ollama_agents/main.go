// Command fake-ollama-agents runs sonar against a deterministic,
// loopback-only Ollama fixture that performs one real consult_experts cycle.
// The expert response is intentionally delayed so Glyphrun can inspect the
// live Agent Hub before the parent turn settles.
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
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	fixtureModel        = "qwen3.5:0.8b"
	expertCallID        = "agents-consult-1"
	defaultExpertDelay  = 8 * time.Second
	maximumExpertDelay  = 30 * time.Second
	expertContractProbe = "bounded, read-only expert consultation"
)

type requestKind string

const (
	requestParentInitial  requestKind = "parent_initial"
	requestExpert         requestKind = "expert"
	requestParentFollowup requestKind = "parent_followup"
)

type fixtureState struct {
	mu            sync.RWMutex
	requests      map[requestKind]int
	protocolError string
}

func newFixtureState() *fixtureState {
	return &fixtureState{requests: make(map[requestKind]int, 3)}
}

func (s *fixtureState) record(kind requestKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[kind]++
	return s.requests[kind]
}

func (s *fixtureState) fail(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.protocolError == "" {
		s.protocolError = fmt.Sprintf(format, args...)
	}
}

func (s *fixtureState) writeReceipt(path string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	parentInitial := s.requests[requestParentInitial]
	expert := s.requests[requestExpert]
	parentFollowup := s.requests[requestParentFollowup]
	ok := parentInitial == 1 && expert == 1 && parentFollowup == 1 && s.protocolError == ""
	content := fmt.Sprintf(
		"protocol_ok=%t\nparent_initial=%d\nexpert_requests=%d\nparent_followup=%d\nprotocol_error=%s\n",
		ok,
		parentInitial,
		expert,
		parentFollowup,
		strings.ReplaceAll(s.protocolError, "\n", " "),
	)
	return os.WriteFile(path, []byte(content), 0o600)
}

type chatMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolName   string `json:"tool_name"`
	ToolCallID string `json:"tool_call_id"`
}

type chatTool struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools"`
	Think    *bool         `json:"think"`
}

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: fake-ollama-agents SONAR_BINARY RECEIPT_PATH")
		return 2
	}
	binary, receiptPath := os.Args[1], os.Args[2]
	delay, err := configuredExpertDelay(os.Getenv("SONAR_FIXTURE_EXPERT_DELAY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake Ollama expert delay: %v\n", err)
		return 2
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake Ollama listen: %v\n", err)
		return 1
	}
	state := newFixtureState()
	server := &http.Server{
		Handler:           fixtureHandler(state, delay),
		ReadHeaderTimeout: 2 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	command := exec.Command(binary, "-model", fixtureModel)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	// The fixture fakes Ollama, so it says so. Relying on the default
	// provider made the spec depend on whether a hosted credential
	// happened to be exported in the ambient shell.
	commandEnv := replaceEnv(hermeticEnv(), "OLLAMA_HOST", "http://"+listener.Addr().String())
	commandEnv = replaceEnv(commandEnv, "SONAR_PROVIDER", "ollama")
	command.Env = commandEnv
	if err := command.Start(); err != nil {
		_ = listener.Close()
		fmt.Fprintf(os.Stderr, "start sonar: %v\n", err)
		return 1
	}

	childDone := make(chan error, 1)
	go func() { childDone <- command.Wait() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	var childErr error
	select {
	case childErr = <-childDone:
	case signal := <-signals:
		_ = command.Process.Signal(signal)
		childErr = <-childDone
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	serveErr := <-serveDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		state.fail("serve fake Ollama: %v", serveErr)
	}
	if shutdownErr != nil {
		state.fail("shutdown fake Ollama: %v", shutdownErr)
	}
	if err := state.writeReceipt(receiptPath); err != nil {
		fmt.Fprintf(os.Stderr, "write fake Ollama receipt: %v\n", err)
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

func configuredExpertDelay(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return defaultExpertDelay, nil
	}
	delay, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if delay < 0 || delay > maximumExpertDelay {
		return 0, fmt.Errorf("must be between 0s and %s", maximumExpertDelay)
	}
	return delay, nil
}

func requestLooksLikeSessionTitle(raw []byte) bool {
	s := string(raw)
	return strings.Contains(s, "Reply with ONLY a short session title") ||
		(strings.Contains(s, "Session title:") && strings.Contains(s, "User request:"))
}

func writeSessionTitleReply(w http.ResponseWriter) {
	writeNDJSON(w, map[string]any{
		"message": map[string]any{"role": "assistant", "content": "Fixture session"},
		"done":    true, "done_reason": "stop", "eval_count": 2, "prompt_eval_count": 2,
		"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
	})
}

func fixtureHandler(state *fixtureState, expertDelay time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"version": "0.31.2-test"})
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"models": []map[string]any{{
			"name": fixtureModel, "model": fixtureModel, "size": 1 << 20,
			"capabilities": []string{"completion", "tools"},
			"details":      map[string]any{"context_length": 16384},
		}}})
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{
			"capabilities": []string{"completion", "tools"},
			"model_info":   map[string]any{"fixture.context_length": 16384},
		})
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"models": []map[string]any{{
			"name": fixtureModel, "model": fixtureModel,
			"size": 1 << 20, "size_vram": 1 << 20, "context_length": 16384,
		}}})
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"done": true})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var request chatRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			state.fail("read chat request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if requestLooksLikeSessionTitle(body) {
			writeSessionTitleReply(w)
			return
		}
		if err := json.Unmarshal(body, &request); err != nil {
			state.fail("decode chat request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if request.Model != fixtureModel {
			state.fail("unexpected model %q", request.Model)
			writeNDJSON(w, map[string]any{"error": "unexpected model", "done": true})
			return
		}

		kind := classifyRequest(request)
		if kind == "" {
			state.fail("could not classify chat request")
			writeNDJSON(w, map[string]any{"error": "unexpected chat request", "done": true})
			return
		}
		if count := state.record(kind); count != 1 {
			state.fail("duplicate %s request %d", kind, count)
			writeNDJSON(w, map[string]any{"error": "duplicate chat request", "done": true})
			return
		}

		switch kind {
		case requestParentInitial:
			writeExpertToolCall(w)
		case requestExpert:
			if request.Think == nil || *request.Think {
				state.fail("expert request did not disable provider reasoning")
			}
			timer := time.NewTimer(expertDelay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
			writeNDJSON(w, map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "Advisory report: keep the Agent Hub bounded, causal, and honest about unavailable child events.",
				},
				"done": true, "done_reason": "stop", "eval_count": 11, "prompt_eval_count": 17,
				"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
			})
		case requestParentFollowup:
			writeNDJSON(w, map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "The bounded expert consultation completed and remains available in the Agent Hub.",
				},
				"done": true, "done_reason": "stop", "eval_count": 9, "prompt_eval_count": 19,
				"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
			})
		}
	})
	return mux
}

func classifyRequest(request chatRequest) requestKind {
	for _, message := range request.Messages {
		if message.Role == "system" && strings.Contains(message.Content, expertContractProbe) {
			return requestExpert
		}
	}
	for _, message := range request.Messages {
		if message.Role == "tool" && message.ToolCallID == expertCallID && message.ToolName == "consult_experts" {
			return requestParentFollowup
		}
	}
	for _, tool := range request.Tools {
		if tool.Function.Name == "consult_experts" {
			return requestParentInitial
		}
	}
	return ""
}

func writeExpertToolCall(w http.ResponseWriter) {
	writeNDJSON(w, map[string]any{
		"message": map[string]any{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"id": expertCallID,
				"function": map[string]any{
					"index": 0,
					"name":  "consult_experts",
					"arguments": map[string]any{
						"strategy":  "team",
						"objective": "Review the Agent Hub interaction contract.",
						"experts":   []string{"generalist"},
					},
				},
			}},
		},
		"done": true, "done_reason": "stop", "eval_count": 5, "prompt_eval_count": 7,
		"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeNDJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	_ = json.NewEncoder(w).Encode(value)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// hermeticEnv strips every provider credential and provider override the
// ambient shell may carry before this fixture declares its own.
//
// Without it a spec is not hermetic. This suite runs on machines where
// DEEPSEEK_API_KEY is routinely exported, os.Environ() passes it straight
// through, and sonar then configures a real hosted provider the spec never
// asked for — so the fake server owns the model inventory while a different
// provider dispatches. Observed: a top bar reading "DEEPSEEK · remote prompts
// · qwen3.5:0.8b", the same spec passing or failing depending on whether a key
// happened to be exported, and a test run reaching a metered endpoint with a
// real credential.
//
// A deterministic terminal suite must not be able to bill you.
func hermeticEnv() []string {
	environment := os.Environ()
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if strings.HasPrefix(key, "SONAR_PROVIDER") ||
			strings.HasSuffix(key, "_API_KEY") ||
			strings.HasSuffix(key, "_API_TOKEN") {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func replaceEnv(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}
