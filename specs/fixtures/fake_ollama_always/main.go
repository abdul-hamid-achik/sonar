// Command fake-ollama-always runs two consecutive sonar TUI processes
// against one deterministic Ollama fixture. The shared HOME proves that an
// exact-request session grant is reused in-process but not after restart.
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

func (s *fixtureState) writeReceipt(path string, durable durableApprovalState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := s.chatRequests == 6 && s.toolReceipts == 3 && s.protocolError == "" && durable.OK()
	return os.WriteFile(path, []byte(fmt.Sprintf(
		"protocol_ok=%t\nchat_requests=%d\ntool_receipts=%d\napproval_session_receipts=%d\napproval_once_receipts=%d\napproval_requested_receipts=%d\napproval_policy_receipts=%d\npersisted_allow_rows=%d\ndurable_error=%s\nprotocol_error=%s\n",
		ok, s.chatRequests, s.toolReceipts, durable.SessionReceipts, durable.OnceReceipts,
		durable.RequestedReceipts, durable.PolicyReceipts, durable.AllowRows,
		strings.ReplaceAll(durable.Err, "\n", " "), strings.ReplaceAll(s.protocolError, "\n", " "),
	)), 0o600)
}

type durableApprovalState struct {
	SessionReceipts   int
	OnceReceipts      int
	RequestedReceipts int
	PolicyReceipts    int
	AllowRows         int
	Err               string
}

func (s durableApprovalState) OK() bool {
	return s.Err == "" && s.SessionReceipts == 2 && s.OnceReceipts == 1 &&
		s.RequestedReceipts == 2 && s.PolicyReceipts == 0 && s.AllowRows == 0
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
		fmt.Fprintln(os.Stderr, "usage: fake-ollama-always SONAR_BINARY RECEIPT_PATH")
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

	exitCode := 0
	for process := 1; process <= 2; process++ {
		command := exec.Command(binary, "-model", fixtureModel)
		command.Stdin = os.Stdin
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		command.Env = replaceEnv(os.Environ(), "OLLAMA_HOST", "http://"+listener.Addr().String())
		if err := command.Run(); err != nil {
			state.fail("sonar process %d: %v", process, err)
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
			break
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	serveErr := <-serveDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		state.fail("serve fake Ollama: %v", serveErr)
		exitCode = 1
	}
	if shutdownErr != nil {
		state.fail("shutdown fake Ollama: %v", shutdownErr)
		exitCode = 1
	}
	durable := inspectDurableApproval()
	if !durable.OK() {
		exitCode = 1
	}
	if err := state.writeReceipt(receiptPath, durable); err != nil {
		fmt.Fprintf(os.Stderr, "write fixture receipt: %v\n", err)
		return 1
	}
	return exitCode
}

func inspectDurableApproval() durableApprovalState {
	home, err := os.UserHomeDir()
	if err != nil {
		return durableApprovalState{Err: fmt.Sprintf("resolve HOME: %v", err)}
	}
	databasePath := filepath.Join(home, ".config", "sonar", "sonar.db")
	connection, err := sql.Open("sqlite", databasePath+"?_foreign_keys=ON")
	if err != nil {
		return durableApprovalState{Err: fmt.Sprintf("open database: %v", err)}
	}
	defer func() { _ = connection.Close() }()

	state := durableApprovalState{}
	queries := []struct {
		query string
		args  []any
		dest  *int
	}{
		{
			query: "SELECT COUNT(*) FROM execution_events WHERE event_type = ? AND approval = ?",
			// ApprovalSession retains the historical "always" wire value for
			// append-only compatibility; it no longer represents global policy.
			args: []any{"approved", "always"}, dest: &state.SessionReceipts,
		},
		{
			query: "SELECT COUNT(*) FROM execution_events WHERE event_type = ? AND approval = ?",
			args:  []any{"approved", "once"}, dest: &state.OnceReceipts,
		},
		{
			query: "SELECT COUNT(*) FROM execution_events WHERE event_type = ?",
			args:  []any{"approval_requested"}, dest: &state.RequestedReceipts,
		},
		{
			query: "SELECT COUNT(*) FROM execution_events WHERE event_type = ? AND approval = ?",
			args:  []any{"approved", "policy"}, dest: &state.PolicyReceipts,
		},
		{
			query: "SELECT COUNT(*) FROM tool_permissions WHERE tool_name = ? AND policy = ?",
			args:  []any{"write", "allow"}, dest: &state.AllowRows,
		},
	}
	for _, item := range queries {
		if err := connection.QueryRowContext(context.Background(), item.query, item.args...).Scan(item.dest); err != nil {
			state.Err = fmt.Sprintf("inspect durable approval: %v", err)
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
			writeToolCall(w, "session-first", "session-grant.txt", "session scoped approval")
		case 2:
			if !hasSuccessfulToolReceipt(request) {
				state.fail("first follow-up omitted a successful tool receipt")
			}
			state.recordToolReceipt()
			writeNDJSON(w, map[string]any{
				"message": map[string]any{"role": "assistant", "content": "Session approval recorded."},
				"done":    true, "done_reason": "stop", "eval_count": 4, "prompt_eval_count": 6,
				"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
			})
		case 3:
			writeToolCall(w, "session-reuse", "session-grant.txt", "session scoped approval")
		case 4:
			if !hasSuccessfulToolReceipt(request) {
				state.fail("in-process reuse follow-up omitted a successful tool receipt")
			}
			state.recordToolReceipt()
			writeNDJSON(w, map[string]any{
				"message": map[string]any{"role": "assistant", "content": "Session approval reused without another prompt."},
				"done":    true, "done_reason": "stop", "eval_count": 4, "prompt_eval_count": 6,
				"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
			})
		case 5:
			writeToolCall(w, "restart-once", "session-grant.txt", "session scoped approval")
		case 6:
			if !hasSuccessfulToolReceipt(request) {
				state.fail("restart follow-up omitted a successful tool receipt")
			}
			state.recordToolReceipt()
			writeNDJSON(w, map[string]any{
				"message": map[string]any{"role": "assistant", "content": "Restart required a fresh approval."},
				"done":    true, "done_reason": "stop", "eval_count": 4, "prompt_eval_count": 6,
				"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
			})
		default:
			state.fail("unexpected chat request %d", call)
			writeNDJSON(w, map[string]any{"error": "unexpected chat request", "done": true})
		}
	})
	return mux
}

func writeToolCall(w http.ResponseWriter, id, path, content string) {
	writeNDJSON(w, map[string]any{
		"message": map[string]any{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"id": id,
				"function": map[string]any{
					"index": 0, "name": "write",
					"arguments": map[string]any{"path": path, "content": content},
				},
			}},
		},
		"done": true, "done_reason": "stop", "eval_count": 5, "prompt_eval_count": 7,
		"total_duration": 200000000, "load_duration": 10000000, "prompt_eval_duration": 40000000, "eval_duration": 150000000,
	})
}

func hasSuccessfulToolReceipt(request chatRequest) bool {
	for _, message := range request.Messages {
		if message.Role == "tool" && message.Content != "" && !strings.Contains(strings.ToLower(message.Content), "denied") && !strings.Contains(strings.ToLower(message.Content), "error") {
			return true
		}
	}
	return false
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
