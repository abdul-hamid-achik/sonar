// Command fake-ollama-json-receipt hosts a deterministic Ollama endpoint,
// runs one headless `sonar --json -p` turn against it, and validates the
// emitted sonar.turn-receipt.v1 document exactly: schema identity,
// closed status/stop reason, provider usage, aggregated timings, and the
// model digest bound from the live inventory. The Glyphrun spec asserts the
// written verdict file, keeping the receipt contract black-box tested.
package main

import (
	"bytes"
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
	"time"
)

const (
	fixtureModel  = "qwen3.5:2b"
	fixtureDigest = "sha256:1f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988"
	fixtureAnswer = "durable receipt turn complete"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: fake-ollama-json-receipt SONAR_BINARY VERDICT_PATH")
		return 2
	}
	binary, verdictPath := os.Args[1], os.Args[2]

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake Ollama listen: %v\n", err)
		return 1
	}
	var chatCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, fixtureModel)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		chatCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		if chatCalls > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":{"message":"unexpected extra chat request"}}`)
			return
		}
		// Flush the headers, then pause before the first token. Both halves
		// matter: the client starts its clock when response headers arrive, so
		// sleeping before the flush would delay the clock too and leave
		// time-to-first-token at zero. This models a real provider — headers
		// immediately, first token later — and on loopback it is the only way
		// the assertion can tell a real measurement from an unset field.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(15 * time.Millisecond)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", fixtureAnswer)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	var childStdout bytes.Buffer
	command := exec.Command(binary, "--json", "-p", "emit one durable receipt", "-model", fixtureModel)
	command.Stdin = os.Stdin
	command.Stdout = io.MultiWriter(&childStdout, os.Stdout)
	command.Stderr = os.Stderr
	// `ollama` is a hosted OpenAI-compatible provider now, so the fixture
	// serves that wire shape on loopback and points the provider profile at
	// itself. It used to fake the native /api protocol against OLLAMA_HOST,
	// which no longer reaches anything.
	commandEnv := replaceEnv(hermeticEnv(), "SONAR_PROVIDER", "ollama")
	commandEnv = replaceEnv(commandEnv, "SONAR_PROVIDER_BASE_URL", "http://"+listener.Addr().String()+"/v1")
	commandEnv = replaceEnv(commandEnv, "SONAR_PROVIDER_MODEL", fixtureModel)
	commandEnv = replaceEnv(commandEnv, "SONAR_PROVIDER_API_KEY_ENV", "FAKE_OLLAMA_API_KEY")
	commandEnv = replaceEnv(commandEnv, "FAKE_OLLAMA_API_KEY", "fixture-key-never-leaves-loopback")
	command.Env = commandEnv
	childErr := command.Run()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = server.Shutdown(shutdownCtx)
	cancel()
	if serveErr := <-serveDone; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "serve fake Ollama: %v\n", serveErr)
	}

	failures := validateReceipt(childStdout.String())
	if childErr != nil {
		failures = append(failures, fmt.Sprintf("sonar exited with error: %v", childErr))
	}
	verdict := "ok\n"
	if len(failures) > 0 {
		verdict = "fail\n" + strings.Join(failures, "\n") + "\n"
	}
	if err := os.WriteFile(verdictPath, []byte(verdict), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write verdict: %v\n", err)
		return 1
	}
	if len(failures) > 0 {
		for _, failure := range failures {
			fmt.Fprintln(os.Stderr, "receipt check failed: "+failure)
		}
		return 1
	}
	return 0
}

// validateReceipt enforces the sonar.turn-receipt.v1 contract on the
// child's stdout: exactly one newline-terminated JSON document with the exact
// facts this fixture's provider reported.
func validateReceipt(stdout string) []string {
	var failures []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}
	trimmed := strings.TrimSpace(stdout)
	check(trimmed != "", "stdout carried no receipt document")
	check(strings.Count(trimmed, "\n") == 0, "stdout must carry exactly one document, got %q", trimmed)
	var receipt struct {
		Schema     string `json:"schema"`
		RunID      string `json:"run_id"`
		TurnID     string `json:"turn_id"`
		Status     string `json:"status"`
		StopReason string `json:"stop_reason"`
		Truncated  bool   `json:"truncated"`
		Text       string `json:"text"`
		Usage      struct {
			PromptTokens int64 `json:"prompt_tokens"`
			EvalTokens   int64 `json:"eval_tokens"`
		} `json:"usage"`
		Timing *struct {
			TTFTMS  int64 `json:"ttft_ms"`
			EvalMS  int64 `json:"eval_ms"`
			TotalMS int64 `json:"total_ms"`
		} `json:"timing"`
		Model struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
			NumCtx int    `json:"num_ctx"`
		} `json:"model"`
		Session *struct {
			Workspace string `json:"workspace"`
		} `json:"session"`
		ToolCalls []json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(trimmed), &receipt); err != nil {
		return append(failures, fmt.Sprintf("stdout is not one valid JSON document: %v", err))
	}
	check(receipt.Schema == "sonar.turn-receipt.v1", "schema = %q", receipt.Schema)
	check(strings.HasPrefix(receipt.RunID, "run_"), "run_id = %q", receipt.RunID)
	check(strings.HasPrefix(receipt.TurnID, "turn_"), "turn_id = %q", receipt.TurnID)
	check(receipt.Status == "settled", "status = %q", receipt.Status)
	check(receipt.StopReason == "completed", "stop_reason = %q", receipt.StopReason)
	check(!receipt.Truncated, "truncated must be false for a stop finish")
	check(receipt.Text == fixtureAnswer, "text = %q", receipt.Text)
	check(receipt.Usage.EvalTokens == 5 && receipt.Usage.PromptTokens == 7,
		"usage = %d/%d, want 5/7", receipt.Usage.EvalTokens, receipt.Usage.PromptTokens)
	// A hosted provider reports no per-phase durations, so eval_ms stays zero
	// by contract ("not reported", never "instant"). Time-to-first-token and
	// total are client-measured and must be present and positive — they were
	// absent from every hosted turn until the OpenAI-compatible dialect started
	// measuring them, which made this the one field the local-daemon fixture
	// alone could satisfy.
	if receipt.Timing == nil {
		failures = append(failures, "timing block is missing")
	} else {
		check(receipt.Timing.TTFTMS > 0, "ttft_ms = %d, want a client-measured value", receipt.Timing.TTFTMS)
		check(receipt.Timing.TotalMS > 0, "total_ms = %d, want a client-measured value", receipt.Timing.TotalMS)
		check(receipt.Timing.TotalMS >= receipt.Timing.TTFTMS,
			"total_ms %d is shorter than ttft_ms %d", receipt.Timing.TotalMS, receipt.Timing.TTFTMS)
	}
	check(receipt.Model.Name == fixtureModel, "model.name = %q", receipt.Model.Name)
	// No model.digest: a weight digest is a fact about locally installed
	// weights, and sonar reaches hosted models only.
	check(receipt.Model.NumCtx > 0, "model.num_ctx = %d", receipt.Model.NumCtx)
	check(receipt.Session != nil && receipt.Session.Workspace != "", "session workspace is missing")
	check(receipt.ToolCalls != nil, "tool_calls must serialize as an array")
	return failures
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
	result := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}
