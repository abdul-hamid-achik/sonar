// Command fake-ollama-inventory runs sonar against a deterministic
// mixed local/cloud Ollama inventory for terminal UX verification.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fake-ollama-inventory SONAR_BINARY")
		return 2
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var pulled atomic.Bool
	server := &http.Server{Handler: handler(&pulled), ReadHeaderTimeout: 2 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	cmd := exec.Command(os.Args[1])
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = replaceEnv(os.Environ(), "OLLAMA_HOST", "http://"+listener.Addr().String())
	if err := cmd.Start(); err != nil {
		return 1
	}
	child := make(chan error, 1)
	go func() { child <- cmd.Wait() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	select {
	case err = <-child:
	case signal := <-signals:
		_ = cmd.Process.Signal(signal)
		err = <-child
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = server.Shutdown(ctx)
	cancel()
	if serveErr := <-done; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return 1
	}
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return 1
}

func handler(pulled *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{"version": "0.31.2-test"}) })
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		models := []map[string]any{
			model("qwen3.5:2b", 2<<30, []string{"completion", "tools", "thinking"}, map[string]any{"parameter_size": "2.3B", "quantization_level": "Q8_0", "context_length": 262144}),
			{"name": "kimi-code:cloud", "model": "kimi-code:cloud", "size": 340, "remote_model": "kimi-code", "remote_host": "https://ollama.com:443", "capabilities": []string{"completion", "tools", "thinking", "vision"}, "details": map[string]any{"context_length": 262144}},
			model("embed-only", 64<<20, []string{"embedding"}, map[string]any{"parameter_size": "64M"}),
		}
		if pulled.Load() {
			models = append(models, model("tiny-code:latest", 512<<20, []string{"completion", "tools"}, map[string]any{"parameter_size": "0.5B", "quantization_level": "Q4_K_M", "context_length": 65536}))
		}
		writeJSON(w, map[string]any{"models": models})
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"capabilities": []string{"completion", "tools", "thinking"}, "details": map[string]any{"parameter_size": "2.3B", "quantization_level": "Q8_0"}, "model_info": map[string]any{"qwen.context_length": 262144}})
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"models": []map[string]any{{"name": "qwen3.5:2b", "model": "qwen3.5:2b", "size": 2 << 30, "size_vram": 1 << 30, "context_length": 16384}}})
	})
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "application/x-ndjson")
		encoder := json.NewEncoder(w)
		_ = encoder.Encode(map[string]any{"status": "downloading", "total": 100, "completed": 25})
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		if request.Model == "slow-code" {
			// A flushed streaming response may not expose the client disconnect to
			// a test server immediately on every platform. Keep the fixture bounded
			// while still allowing the production cancellation path to win.
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
			return
		}
		pulled.Store(true)
		_ = encoder.Encode(map[string]any{"status": "success", "total": 100, "completed": 100})
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{"done": true}) })
	return mux
}

func model(name string, size int64, capabilities []string, details map[string]any) map[string]any {
	return map[string]any{"name": name, "model": name, "size": size, "capabilities": capabilities, "details": details}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func replaceEnv(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			continue
		}
		result = append(result, entry)
	}
	return append(result, prefix+value)
}
