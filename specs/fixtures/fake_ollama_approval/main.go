// Command fake-ollama-approval runs sonar against a deterministic,
// loopback-only Ollama fixture. It exists solely for Glyphrun approval UX.
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
	fixtureModel = "qwen3.5:0.8b"
	fixturePath  = "approval-probe.txt"
)

type fixtureState struct {
	mu                  sync.Mutex
	chatRequests        int
	sawDeniedToolResult bool
	protocolError       string
}

func (s *fixtureState) fail(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.protocolError == "" {
		s.protocolError = fmt.Sprintf(format, args...)
	}
}

func (s *fixtureState) nextChat() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatRequests++
	return s.chatRequests
}

func (s *fixtureState) markDeniedToolResult() {
	s.mu.Lock()
	s.sawDeniedToolResult = true
	s.mu.Unlock()
}

func (s *fixtureState) writeReceipt(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := s.chatRequests == 2 && s.sawDeniedToolResult && s.protocolError == ""
	content := fmt.Sprintf(
		"protocol_ok=%t\nchat_requests=%d\nsaw_denied_tool_result=%t\nprotocol_error=%s\n",
		ok,
		s.chatRequests,
		s.sawDeniedToolResult,
		strings.ReplaceAll(s.protocolError, "\n", " "),
	)
	return os.WriteFile(path, []byte(content), 0o600)
}

type chatRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: fake-ollama-approval SONAR_BINARY RECEIPT_PATH")
		return 2
	}
	binary, receiptPath := os.Args[1], os.Args[2]

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake Ollama listen: %v\n", err)
		return 1
	}
	state := &fixtureState{}
	server := &http.Server{
		Handler:           fixtureHandler(state),
		ReadHeaderTimeout: 2 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()

	cmd := exec.Command(binary, "-model", fixtureModel)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The fixture fakes Ollama, so it says so. Relying on the default
	// provider made the spec depend on whether a hosted credential
	// happened to be exported in the ambient shell.
	cmdEnv := replaceEnv(hermeticEnv(), "OLLAMA_HOST", "http://"+listener.Addr().String())
	cmdEnv = replaceEnv(cmdEnv, "SONAR_PROVIDER", "ollama")
	cmd.Env = cmdEnv
	if err := cmd.Start(); err != nil {
		_ = listener.Close()
		fmt.Fprintf(os.Stderr, "start sonar: %v\n", err)
		return 1
	}

	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	var childErr error
	select {
	case childErr = <-childDone:
	case sig := <-signals:
		_ = cmd.Process.Signal(sig)
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

func fixtureHandler(state *fixtureState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"models": []map[string]any{{
			"name": fixtureModel, "model": fixtureModel, "size": 1 << 20,
		}}})
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{})
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

		switch call := state.nextChat(); call {
		case 1:
			// Emit provider-native reasoning before the tool request so the terminal
			// spec covers the live transcript hierarchy independently from the
			// operational cancel/queue footer.
			writeNDJSON(w, map[string]any{
				"message": map[string]any{
					"role": "assistant", "thinking": "Checking workspace policy before the write.",
				},
				"done": false,
			})
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
			writeNDJSON(w, map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "approval-call-1",
						"function": map[string]any{
							"index": 0,
							"name":  "write",
							"arguments": map[string]any{
								"path": fixturePath, "content": "must not be written",
							},
						},
					}},
				},
				"done": true, "done_reason": "stop", "eval_count": 5, "prompt_eval_count": 7,
				"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
			})
		case 2:
			denied := false
			for _, message := range request.Messages {
				if message.Role == "tool" && strings.Contains(message.Content, "denied") {
					denied = true
					break
				}
			}
			if !denied {
				state.fail("second chat request omitted the denied tool result")
				writeNDJSON(w, map[string]any{"error": "denied tool result missing", "done": true})
				return
			}
			state.markDeniedToolResult()
			writeNDJSON(w, map[string]any{
				"message": map[string]any{
					"role": "assistant", "content": "Denied safely. No file was changed.",
				},
				"done": true, "done_reason": "stop", "eval_count": 6, "prompt_eval_count": 8,
				"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
			})
		default:
			state.fail("unexpected chat request %d", call)
			writeNDJSON(w, map[string]any{"error": "unexpected chat request", "done": true})
		}
	})
	return mux
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

func replaceEnv(environ []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environ)+1)
	for _, item := range environ {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}
