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

	"github.com/abdul-hamid-achik/sonar/specs/fixtures/openaiwire"
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
		// `ollama` is a hosted OpenAI-compatible provider now, so the fixture
		// serves that wire shape on loopback and points the provider profile at
		// itself. It used to fake the native /api protocol against OLLAMA_HOST,
		// which no longer reaches anything. Declaring the provider explicitly also
		// keeps the spec off whatever credential the ambient shell exports.
		commandEnv := replaceEnv(hermeticEnv(), "SONAR_PROVIDER_BASE_URL", "http://"+listener.Addr().String()+"/v1")
		commandEnv = replaceEnv(commandEnv, "SONAR_PROVIDER_MODEL", fixtureModel)
		commandEnv = replaceEnv(commandEnv, "SONAR_PROVIDER_API_KEY_ENV", "FAKE_OLLAMA_API_KEY")
		commandEnv = replaceEnv(commandEnv, "FAKE_OLLAMA_API_KEY", "fixture-key-never-leaves-loopback")
		commandEnv = replaceEnv(commandEnv, "SONAR_PROVIDER", "ollama")
		command.Env = commandEnv
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

func fixtureHandler(state *fixtureState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		openaiwire.WriteModels(w, fixtureModel)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var request openaiwire.ChatRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			state.fail("read chat request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if requestLooksLikeSessionTitle(body) {
			openaiwire.WriteSessionTitle(w)
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
			if !openaiwire.HasAnySuccessfulToolReceipt(request) {
				state.fail("first follow-up omitted a successful tool receipt")
			}
			state.recordToolReceipt()
			openaiwire.WriteText(w, "Session approval recorded.")
		case 3:
			writeToolCall(w, "session-reuse", "session-grant.txt", "session scoped approval")
		case 4:
			if !openaiwire.HasAnySuccessfulToolReceipt(request) {
				state.fail("in-process reuse follow-up omitted a successful tool receipt")
			}
			state.recordToolReceipt()
			openaiwire.WriteText(w, "Session approval reused without another prompt.")
		case 5:
			writeToolCall(w, "restart-once", "session-grant.txt", "session scoped approval")
		case 6:
			if !openaiwire.HasAnySuccessfulToolReceipt(request) {
				state.fail("restart follow-up omitted a successful tool receipt")
			}
			state.recordToolReceipt()
			openaiwire.WriteText(w, "Restart required a fresh approval.")
		default:
			state.fail("unexpected chat request %d", call)
			openaiwire.WriteError(w, "unexpected chat request")
		}
	})
	return mux
}

func writeToolCall(w http.ResponseWriter, id, path, content string) {
	openaiwire.WriteToolCall(w, id, "write", map[string]any{"path": path, "content": content})
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
