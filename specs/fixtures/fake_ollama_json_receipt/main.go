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
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"models":[{"name":%q,"size":42,"digest":%q,"details":{"family":"qwen3"}}]}`,
			fixtureModel, fixtureDigest)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"capabilities":["completion","tools"],"details":{"family":"qwen3"},"model_info":{"qwen3.context_length":32768}}`)
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"models":[{"name":%q,"model":%q,"size":42,"digest":%q,"size_vram":42,"context_length":32768}]}`,
			fixtureModel, fixtureModel, fixtureDigest)
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, _ *http.Request) {
		chatCalls++
		if chatCalls > 1 {
			_, _ = fmt.Fprintln(w, `{"error":"unexpected extra chat request","done":true}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"message":{"role":"assistant","content":%q},"done":true,"done_reason":"stop","eval_count":5,"prompt_eval_count":7,"total_duration":3810000000,"load_duration":50000000,"prompt_eval_duration":220000000,"eval_duration":3180000000}`+"\n",
			fixtureAnswer)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	var childStdout bytes.Buffer
	command := exec.Command(binary, "--json", "-p", "emit one durable receipt", "-model", fixtureModel)
	command.Stdin = os.Stdin
	command.Stdout = io.MultiWriter(&childStdout, os.Stdout)
	command.Stderr = os.Stderr
	command.Env = replaceEnv(os.Environ(), "OLLAMA_HOST", "http://"+listener.Addr().String())
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
	if receipt.Timing == nil {
		failures = append(failures, "timing block is missing")
	} else {
		check(receipt.Timing.EvalMS == 3180, "eval_ms = %d", receipt.Timing.EvalMS)
		check(receipt.Timing.TotalMS == 3810, "total_ms = %d", receipt.Timing.TotalMS)
	}
	check(receipt.Model.Name == fixtureModel, "model.name = %q", receipt.Model.Name)
	check(receipt.Model.Digest == fixtureDigest, "model.digest = %q", receipt.Model.Digest)
	check(receipt.Model.NumCtx > 0, "model.num_ctx = %d", receipt.Model.NumCtx)
	check(receipt.Session != nil && receipt.Session.Workspace != "", "session workspace is missing")
	check(receipt.ToolCalls != nil, "tool_calls must serialize as an array")
	return failures
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
