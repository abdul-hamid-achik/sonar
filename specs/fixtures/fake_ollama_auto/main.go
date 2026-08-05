// Command fake-ollama-auto runs sonar against a deterministic,
// loopback-only Ollama fixture. It proves that consecutive host-catalogued
// development commands execute in AUTO without opening approval prompts.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const fixtureModel = "qwen3.5:0.8b"

type fixtureState struct {
	mu            sync.Mutex
	chatRequests  int
	toolReceipts  int
	protocolError string
}

func (s *fixtureState) nextChat() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatRequests++
	return s.chatRequests
}

func (s *fixtureState) recordToolReceipt() {
	s.mu.Lock()
	s.toolReceipts++
	s.mu.Unlock()
}

func (s *fixtureState) fail(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.protocolError == "" {
		s.protocolError = fmt.Sprintf(format, args...)
	}
}

type durableAutoState struct {
	ApprovalRequests int
	PolicyApprovals  int
	CompletedBash    int
	Unresolved       int
	Err              string
}

func (s durableAutoState) OK() bool {
	return s.Err == "" && s.ApprovalRequests == 0 && s.PolicyApprovals == 3 &&
		s.CompletedBash == 3 && s.Unresolved == 0
}

func (s *fixtureState) writeReceipt(path string, durable durableAutoState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := s.chatRequests == 4 && s.toolReceipts == 3 && s.protocolError == "" && durable.OK()
	content := fmt.Sprintf(
		"protocol_ok=%t\nchat_requests=%d\ntool_receipts=%d\napproval_requested=%d\npolicy_approvals=%d\ncompleted_bash=%d\nunresolved=%d\ndurable_error=%s\nprotocol_error=%s\n",
		ok,
		s.chatRequests,
		s.toolReceipts,
		durable.ApprovalRequests,
		durable.PolicyApprovals,
		durable.CompletedBash,
		durable.Unresolved,
		strings.ReplaceAll(durable.Err, "\n", " "),
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

type chatRequest struct {
	Messages []chatMessage `json:"messages"`
}

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: fake-ollama-auto SONAR_BINARY RECEIPT_PATH")
		return 2
	}
	binary, receiptPath := os.Args[1], os.Args[2]

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake Ollama listen: %v\n", err)
		return 1
	}
	state := &fixtureState{}
	server := &http.Server{Handler: fixtureHandler(state), ReadHeaderTimeout: 2 * time.Second}
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
	childErr := command.Run()

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

	durable := inspectDurableAuto()
	if err := state.writeReceipt(receiptPath, durable); err != nil {
		fmt.Fprintf(os.Stderr, "write fake Ollama receipt: %v\n", err)
		return 1
	}
	if childErr == nil && durable.OK() {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(childErr, &exitErr) {
		return exitErr.ExitCode()
	}
	if childErr != nil {
		fmt.Fprintf(os.Stderr, "wait for sonar: %v\n", childErr)
	}
	return 1
}

func inspectDurableAuto() durableAutoState {
	home, err := os.UserHomeDir()
	if err != nil {
		return durableAutoState{Err: fmt.Sprintf("resolve HOME: %v", err)}
	}
	databasePath := filepath.Join(home, ".config", "sonar", "sonar.db")
	connection, err := sql.Open("sqlite", databasePath+"?_foreign_keys=ON")
	if err != nil {
		return durableAutoState{Err: fmt.Sprintf("open database: %v", err)}
	}
	defer func() { _ = connection.Close() }()

	state := durableAutoState{}
	queries := []struct {
		query string
		args  []any
		dest  *int
	}{
		{
			query: "SELECT COUNT(*) FROM execution_events WHERE event_type = ?",
			args:  []any{"approval_requested"}, dest: &state.ApprovalRequests,
		},
		{
			query: "SELECT COUNT(*) FROM execution_events WHERE event_type = ? AND approval = ? AND tool_name = ?",
			args:  []any{"approved", "policy", "bash"}, dest: &state.PolicyApprovals,
		},
		{
			query: "SELECT COUNT(*) FROM execution_events WHERE event_type = ? AND tool_name = ?",
			args:  []any{"completed", "bash"}, dest: &state.CompletedBash,
		},
		{
			query: `SELECT COUNT(*)
				FROM execution_events e
				WHERE e.event_type = 'started'
				  AND NOT EXISTS (
					SELECT 1 FROM execution_events terminal
					WHERE terminal.execution_id = e.execution_id
					  AND terminal.event_type IN ('completed', 'failed', 'cancelled', 'outcome_unknown')
				  )`,
			dest: &state.Unresolved,
		},
	}
	for _, item := range queries {
		if err := connection.QueryRowContext(context.Background(), item.query, item.args...).Scan(item.dest); err != nil {
			state.Err = fmt.Sprintf("inspect durable AUTO receipts: %v", err)
			return state
		}
	}
	return state
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
			writeBashCall(w, "auto-safe-1", "go test ./...")
		case 2:
			if !hasSuccessfulToolReceipt(request, "auto-safe-1", "bash") {
				state.fail("first follow-up omitted a successful tool receipt")
			} else {
				state.recordToolReceipt()
			}
			writeBashCall(w, "auto-safe-2", "./bin/minerva stack check")
		case 3:
			if !hasSuccessfulToolReceipt(request, "auto-safe-2", "bash") {
				state.fail("second follow-up omitted a successful tool receipt")
			} else {
				state.recordToolReceipt()
			}
			writeBashCall(w, "auto-safe-3", "./bin/minerva skill list 2>&1 | grep test-skill")
		case 4:
			if !hasSuccessfulToolReceipt(request, "auto-safe-3", "bash") {
				state.fail("third follow-up omitted a successful tool receipt")
			} else {
				state.recordToolReceipt()
			}
			writeNDJSON(w, map[string]any{
				"message": map[string]any{
					"role": "assistant", "content": "AUTO completed the safe local check and both exact Minerva queries without interruption.",
				},
				"done": true, "done_reason": "stop", "eval_count": 5, "prompt_eval_count": 8,
				"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
			})
		default:
			state.fail("unexpected chat request %d", call)
			writeNDJSON(w, map[string]any{"error": "unexpected chat request", "done": true})
		}
	})
	return mux
}

func writeBashCall(w http.ResponseWriter, id, command string) {
	writeNDJSON(w, map[string]any{
		"message": map[string]any{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"id": id,
				"function": map[string]any{
					"index": 0, "name": "bash",
					"arguments": map[string]any{"command": command},
				},
			}},
		},
		"done": true, "done_reason": "stop", "eval_count": 5, "prompt_eval_count": 7,
		"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
	})
}

func hasSuccessfulToolReceipt(request chatRequest, expectedID, expectedName string) bool {
	lastToolMessage := -1
	matched := -1
	for _, message := range request.Messages {
		if message.Role != "tool" || message.Content == "" {
			continue
		}
		lastToolMessage++
		if message.ToolCallID != expectedID || message.ToolName != expectedName {
			continue
		}
		lower := strings.ToLower(message.Content)
		if !strings.Contains(lower, "denied") && !strings.Contains(lower, "error") &&
			!strings.Contains(lower, "exit status") {
			matched = lastToolMessage
		}
	}
	// Ollama sends the complete conversation on every request. Requiring the
	// expected identity to be the final tool message prevents an earlier receipt
	// from satisfying a later follow-up.
	return lastToolMessage >= 0 && matched == lastToolMessage
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
