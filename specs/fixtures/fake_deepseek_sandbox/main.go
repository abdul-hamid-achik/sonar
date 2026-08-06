// Command fake-deepseek-sandbox proves the OS confinement boundary through the
// real binary.
//
// internal/sandbox already tests the boundary against the kernel, and
// internal/agent tests it through the bash tool. Both build their own
// exec.Cmd. What neither can show is that a command dispatched by a real AUTO
// turn — through config loading, the authority snapshot, the approval path and
// the tool loop — still arrives at the kernel confined. That is a lot of
// machinery between a setting and a syscall, and the failure it protects
// against is the quiet one: a sandbox configured, reported active, and not
// applied.
//
// So the fixture scripts three commands whose OUTPUT says whether confinement
// held, and asserts on what the model was told rather than on what the host
// intended:
//
//  1. read a workspace file — must succeed, or the sandbox is unusable;
//  2. read the workspace .env — must not yield its contents;
//  3. write outside the workspace — must not create the file.
//
// Each returns a marker the fixture checks in the following turn's tool
// receipt, so a confinement that silently stopped applying fails here rather
// than in a session.
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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/specs/fixtures/openaiwire"
)

const fixtureModel = "deepseek-v4-flash"

// escapeTarget is written outside the workspace on purpose. It sits under the
// home directory the spec creates, which is outside every root the default
// policy makes writable — unlike a temp directory, which the policy allows so
// `go build` can create its work directory.
const escapeTarget = "sandbox-escape-probe.txt"

type fixtureState struct {
	mu            sync.Mutex
	chatRequests  int
	toolReceipts  int
	protocolError string

	workspaceReadable bool
	secretWithheld    bool
	escapeRefused     bool
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

func (s *fixtureState) observe(set *bool, value bool) {
	s.mu.Lock()
	*set = value
	s.mu.Unlock()
}

func (s *fixtureState) writeReceipt(path string, escapeExists bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := s.chatRequests == 4 && s.toolReceipts == 3 && s.protocolError == "" &&
		s.workspaceReadable && s.secretWithheld && s.escapeRefused && !escapeExists
	content := fmt.Sprintf(
		"protocol_ok=%t\nchat_requests=%d\ntool_receipts=%d\n"+
			"workspace_readable=%t\nsecret_withheld=%t\nescape_refused=%t\nescape_file_exists=%t\n"+
			"protocol_error=%s\n",
		ok, s.chatRequests, s.toolReceipts,
		s.workspaceReadable, s.secretWithheld, s.escapeRefused, escapeExists,
		s.protocolError,
	)
	return os.WriteFile(path, []byte(content), 0o644)
}

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: fake-deepseek-sandbox <sonar-binary> <receipt-path>")
		return 2
	}
	binary, receiptPath := os.Args[1], os.Args[2]

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve HOME: %v\n", err)
		return 1
	}
	escapePath := filepath.Join(home, escapeTarget)
	_ = os.Remove(escapePath)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture listen: %v\n", err)
		return 1
	}
	state := &fixtureState{}
	server := &http.Server{Handler: fixtureHandler(state, escapePath), ReadHeaderTimeout: 2 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	command := exec.Command(binary, "-model", fixtureModel)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
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
		state.fail("serve fixture: %v", serveErr)
	}
	if shutdownErr != nil {
		state.fail("shutdown fixture: %v", shutdownErr)
	}

	_, statErr := os.Stat(escapePath)
	escapeExists := statErr == nil
	_ = os.Remove(escapePath)

	if err := state.writeReceipt(receiptPath, escapeExists); err != nil {
		fmt.Fprintf(os.Stderr, "write receipt: %v\n", err)
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

func fixtureHandler(state *fixtureState, escapePath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		openaiwire.WriteModels(w, fixtureModel)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			state.fail("read chat request: %v", err)
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if requestLooksLikeSessionTitle(body) {
			openaiwire.WriteSessionTitle(w)
			return
		}
		var request openaiwire.ChatRequest
		if err := json.Unmarshal(body, &request); err != nil {
			state.fail("decode chat request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		// Each command prints a marker so the NEXT turn's receipt carries the
		// answer. Asserting on the model's view is the whole point: the host
		// believing it confined something is exactly the thing under test.
		switch call := state.nextChat(); call {
		case 1:
			openaiwire.WriteBashCall(w, "sandbox-read", "cat app.txt")
		case 2:
			receipt := openaiwire.ToolReceiptText(request, "sandbox-read")
			state.observe(&state.workspaceReadable, strings.Contains(receipt, "WORKSPACE-CONTENT"))
			state.recordToolReceipt()
			// A workspace SCRIPT, not `cat .env`. The catalog refuses the
			// direct read on path authority before confinement is consulted at
			// all, so asserting on it would test the wrong layer. A script is
			// opaque to argv — which is exactly the gap the sandbox exists to
			// cover — and `sh` is uncatalogued, so reaching it also proves the
			// confinement widening admitted it.
			openaiwire.WriteBashCall(w, "sandbox-secret", "sh leak.sh")
		case 3:
			receipt := openaiwire.ToolReceiptText(request, "sandbox-secret")
			// The secret's value must not appear. The command may fail loudly
			// or return nothing; either is a withheld secret, and pinning which
			// one would pin a message rather than the boundary.
			state.observe(&state.secretWithheld, !strings.Contains(receipt, "SECRET-VALUE"))
			state.recordToolReceipt()
			openaiwire.WriteBashCall(w, "sandbox-escape", "sh escape.sh")
		case 4:
			receipt := openaiwire.ToolReceiptText(request, "sandbox-escape")
			_ = receipt
			_, statErr := os.Stat(escapePath)
			state.observe(&state.escapeRefused, statErr != nil)
			state.recordToolReceipt()
			openaiwire.WriteText(w, "Confinement held for all three probes.")
		default:
			state.fail("unexpected chat request %d", call)
			openaiwire.WriteError(w, "unexpected chat request")
		}
	})
	return mux
}

func requestLooksLikeSessionTitle(raw []byte) bool {
	s := string(raw)
	return strings.Contains(s, "Reply with ONLY a short session title") ||
		(strings.Contains(s, "Session title:") && strings.Contains(s, "User request:"))
}

// hermeticEnv strips every provider credential and provider override the
// ambient shell may carry before this fixture declares its own. Without it a
// spec is not hermetic: this suite runs on machines where DEEPSEEK_API_KEY is
// routinely exported, and a test run must not be able to bill anyone.
func hermeticEnv() []string {
	environment := os.Environ()
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "SONAR_PROVIDER") ||
			strings.HasSuffix(upper, "_API_KEY") ||
			upper == "OPENAI_API_KEY" {
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
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, prefix+value)
}
