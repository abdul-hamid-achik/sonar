package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewModelManager(t *testing.T) {
	m := NewModelManager("http://localhost:11434", 4096)

	if m.baseURL != "http://localhost:11434" {
		t.Errorf("baseURL = %q, want %q", m.baseURL, "http://localhost:11434")
	}
	if m.configuredNumCtx() != 4096 {
		t.Errorf("numCtx = %d, want %d", m.configuredNumCtx(), 4096)
	}
	if m.clients == nil {
		t.Error("clients map should be initialized")
	}
}

func TestModelManagerBaseURL(t *testing.T) {
	m := NewModelManager("http://custom:9999", 2048)
	if m.BaseURL() != "http://custom:9999" {
		t.Errorf("BaseURL() = %q, want %q", m.BaseURL(), "http://custom:9999")
	}
}

func TestModelManagerNumCtx(t *testing.T) {
	m := NewModelManager("http://localhost:11434", 8192)
	if m.NumCtx() != 8192 {
		t.Errorf("NumCtx() = %d, want %d", m.NumCtx(), 8192)
	}
}

func TestModelManagerContextPolicyUsesVerifiedInventory(t *testing.T) {
	manager := NewModelManager("http://localhost:11434", 16384)
	manager.ConfigureOllamaInventory([]OllamaModel{
		{Name: "local-small:latest", Location: OllamaModelLocationLocal, ContextLength: 8192},
		{Name: "local-large:latest", Location: OllamaModelLocationLocal, ContextLength: 262144},
		{Name: "cloud-large:latest", Location: OllamaModelLocationCloud, ContextLength: 1048576},
		{Name: "remote:latest", Location: OllamaModelLocationRemote, ContextLength: 131072},
	}, true)

	tests := []struct {
		name   string
		model  string
		policy ModelContextPolicy
		known  bool
	}{
		{
			name: "local native below allocation", model: "local-small",
			policy: ModelContextPolicy{Native: 8192, Request: 8192, Effective: 8192, NativeKnown: true}, known: true,
		},
		{
			name: "local native above allocation", model: "local-large",
			policy: ModelContextPolicy{Native: 262144, Request: 16384, Effective: 16384, NativeKnown: true}, known: true,
		},
		{
			name: "verified cloud omits allocation", model: "cloud-large",
			policy: ModelContextPolicy{Native: 1048576, Effective: 1048576, Cloud: true, NativeKnown: true}, known: true,
		},
		{
			name: "remote is not cloud", model: "remote",
			policy: ModelContextPolicy{Native: 131072, Request: 16384, Effective: 16384, NativeKnown: true}, known: true,
		},
		{
			name: "cloud suffix is not authority", model: "unverified:cloud",
			policy: ModelContextPolicy{Request: 16384, Effective: 16384}, known: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := manager.ContextPolicy(test.model); got != test.policy {
				t.Fatalf("policy = %#v, want %#v", got, test.policy)
			}
			if effective, known := manager.EffectiveContext(test.model); effective != test.policy.Effective || known != test.known {
				t.Fatalf("effective context = %d, %v; want %d, %v", effective, known, test.policy.Effective, test.known)
			}
		})
	}

	manager.ConfigureOllamaInventory(nil, false)
	if got := manager.ContextPolicy("cloud-large"); got.Cloud || got.NativeKnown || got.Request != 16384 || got.Effective != 16384 {
		t.Fatalf("unverified refresh retained stale policy: %#v", got)
	}
}

func TestModelManagerCloudContextRequiresVerifiedNativeMaximum(t *testing.T) {
	manager := NewModelManager("http://localhost:11434", 16384)
	manager.ConfigureOllamaCloudInventory([]string{"cloud:latest"}, true)
	policy := manager.ContextPolicy("cloud")
	if !policy.Cloud || policy.Request != 0 || policy.Effective != 0 || policy.NativeKnown {
		t.Fatalf("cloud policy without native maximum = %#v", policy)
	}
	if effective, known := manager.EffectiveContext("cloud"); effective != 0 || known {
		t.Fatalf("unknown cloud effective context = %d, %v", effective, known)
	}
	if err := manager.SetCurrentModel("cloud"); err == nil {
		t.Fatal("selected a cloud model without a verified context maximum")
	}
}

func TestModelManagerCurrentCloudFailsClosedWhenRefreshedContextBecomesUnknown(t *testing.T) {
	chatCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			chatCalls++
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"unexpected"},"done":true}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 16_384)
	manager.ConfigureOllamaRuntimeInventory(false, []OllamaModel{{
		Name: "cloud:latest", Location: OllamaModelLocationCloud, ContextLength: 1_048_576,
	}}, true)
	if err := manager.SetCurrentModel("cloud"); err != nil {
		t.Fatal(err)
	}
	manager.ConfigureOllamaRuntimeInventory(false, []OllamaModel{{
		Name: "cloud:latest", Location: OllamaModelLocationCloud,
	}}, true)
	if got := manager.NumCtx(); got != 0 {
		t.Fatalf("unknown cloud NumCtx = %d, want 0", got)
	}
	err := manager.ChatStream(context.Background(), ChatOptions{ExpectedContext: 16_384}, func(StreamChunk) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "no verified context maximum") {
		t.Fatalf("unknown cloud request error = %v", err)
	}
	if chatCalls != 0 {
		t.Fatalf("provider received %d request(s), want none", chatCalls)
	}
}

func TestModelManagerRejectsTurnWhenExpectedContextChanged(t *testing.T) {
	chatCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			chatCalls++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 16_384)
	manager.ConfigureOllamaRuntimeInventory(false, []OllamaModel{{
		Name: "local:latest", Location: OllamaModelLocationLocal, SizeBytes: 1 << 30, ContextLength: 262_144,
	}}, true)
	if err := manager.SetCurrentModel("local"); err != nil {
		t.Fatal(err)
	}
	err := manager.ChatStream(context.Background(), ChatOptions{ExpectedContext: 1_048_576}, func(StreamChunk) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "context changed before inference") {
		t.Fatalf("context mismatch error = %v", err)
	}
	if chatCalls != 0 {
		t.Fatalf("provider received %d request(s), want none", chatCalls)
	}
}

func TestModelManagerRejectsTurnWhenExpectedModelChangedAtSameContext(t *testing.T) {
	chatCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			chatCalls++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 16_384)
	manager.ConfigureOllamaRuntimeInventory(false, []OllamaModel{
		{Name: "first:latest", Location: OllamaModelLocationLocal, SizeBytes: 1 << 30, ContextLength: 262_144},
		{Name: "second:latest", Location: OllamaModelLocationLocal, SizeBytes: 1 << 30, ContextLength: 262_144},
	}, true)
	if err := manager.SetCurrentModel("second"); err != nil {
		t.Fatal(err)
	}
	err := manager.ChatStream(context.Background(), ChatOptions{
		ExpectedModel: "first", ExpectedContext: 16_384,
	}, func(StreamChunk) error { return nil })
	if !errors.Is(err, ErrInferenceNotStarted) || !strings.Contains(err.Error(), "model changed before inference") {
		t.Fatalf("model mismatch error = %v", err)
	}
	if chatCalls != 0 {
		t.Fatalf("provider received %d request(s), want none", chatCalls)
	}
}

func TestModelManagerRuntimeInventoryCommitWaitsForInferenceAndRevokesLocalAdmission(t *testing.T) {
	chatStarted := make(chan struct{}, 1)
	releaseChat := make(chan struct{})
	var cloud atomic.Bool
	var chatCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if cloud.Load() {
				_, _ = fmt.Fprintln(w, `{"models":[{"name":"same:latest","model":"same:latest","remote_model":"same:latest","remote_host":"https://ollama.com","size":0}]}`)
			} else {
				_, _ = fmt.Fprintln(w, `{"models":[{"name":"same:latest","model":"same:latest","size":1073741824}]}`)
			}
		case "/api/chat":
			chatCalls.Add(1)
			chatStarted <- struct{}{}
			<-releaseChat
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"done"},"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 16_384)
	manager.ConfigureOllamaRuntimeInventory(true, []OllamaModel{{
		Name: "same:latest", Location: OllamaModelLocationLocal, SizeBytes: 1 << 30, ContextLength: 262_144,
	}}, true)
	if err := manager.SetCurrentModel("same"); err != nil {
		t.Fatal(err)
	}
	chatDone := make(chan error, 1)
	go func() {
		chatDone <- manager.ChatStream(context.Background(), ChatOptions{ExpectedContext: 16_384}, func(StreamChunk) error { return nil })
	}()
	select {
	case <-chatStarted:
	case <-time.After(time.Second):
		t.Fatal("chat did not start")
	}

	cloud.Store(true)
	refreshDone := make(chan struct{})
	go func() {
		manager.ConfigureOllamaRuntimeInventory(true, []OllamaModel{{
			Name: "same:latest", Location: OllamaModelLocationCloud, ContextLength: 1_048_576,
		}}, true)
		close(refreshDone)
	}()
	select {
	case <-refreshDone:
		t.Fatal("runtime inventory changed during active inference")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseChat)
	if err := <-chatDone; err != nil {
		t.Fatalf("active chat: %v", err)
	}
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("runtime inventory did not commit after inference")
	}
	err := manager.ChatStream(context.Background(), ChatOptions{}, func(StreamChunk) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not installed with local Ollama weights") {
		t.Fatalf("reclassified cloud admission error = %v", err)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("provider received %d chat request(s), want one pre-refresh request", got)
	}
}

func TestModelManagerSwitchAdmissionAndPolicyUseOneInventorySnapshot(t *testing.T) {
	tagsStarted := make(chan struct{}, 1)
	releaseTags := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		tagsStarted <- struct{}{}
		<-releaseTags
		_, _ = fmt.Fprintln(w, `{"models":[{"name":"same:latest","model":"same:latest","size":1073741824}]}`)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 16_384)
	manager.ConfigureOllamaRuntimeInventory(true, []OllamaModel{{
		Name: "same:latest", Location: OllamaModelLocationLocal, SizeBytes: 1 << 30, ContextLength: 262_144,
	}}, true)
	switchDone := make(chan error, 1)
	go func() { switchDone <- manager.SetCurrentModel("same") }()
	select {
	case <-tagsStarted:
	case <-time.After(time.Second):
		t.Fatal("model admission did not start")
	}

	refreshDone := make(chan struct{})
	go func() {
		manager.ConfigureOllamaRuntimeInventory(true, []OllamaModel{{
			Name: "same:latest", Location: OllamaModelLocationCloud, ContextLength: 1_048_576,
		}}, true)
		close(refreshDone)
	}()
	select {
	case <-refreshDone:
		t.Fatal("inventory changed between model admission and client policy")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseTags)
	if err := <-switchDone; err != nil {
		t.Fatalf("local switch: %v", err)
	}
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("inventory refresh did not resume after switch")
	}
	if policy := manager.ContextPolicy("same"); !policy.Cloud || policy.Effective != 1_048_576 {
		t.Fatalf("post-switch policy = %#v", policy)
	}
}

func TestModelManagerNumCtxTracksCurrentModelPolicy(t *testing.T) {
	manager := NewModelManager("http://localhost:11434", 16_384)
	manager.ConfigureOllamaInventory([]OllamaModel{
		{Name: "local:latest", Location: OllamaModelLocationLocal, ContextLength: 262_144},
		{Name: "cloud:latest", Location: OllamaModelLocationCloud, ContextLength: 1_048_576},
	}, true)
	if err := manager.SetCurrentModel("local"); err != nil {
		t.Fatal(err)
	}
	if got := manager.NumCtx(); got != 16_384 {
		t.Fatalf("local NumCtx = %d, want 16384", got)
	}
	if err := manager.SetCurrentModel("cloud"); err != nil {
		t.Fatal(err)
	}
	if got := manager.NumCtx(); got != 1_048_576 {
		t.Fatalf("cloud NumCtx = %d, want 1048576", got)
	}
}

func TestModelManagerCachedClientFollowsContextPolicy(t *testing.T) {
	manager := NewModelManager("http://localhost:11434", 16384)

	localClient, err := manager.GetClient("model")
	if err != nil {
		t.Fatal(err)
	}
	if localClient.numCtx != 16384 {
		t.Fatalf("initial client num_ctx = %d", localClient.numCtx)
	}

	manager.ConfigureOllamaInventory([]OllamaModel{{
		Name: "model:latest", Location: OllamaModelLocationCloud, ContextLength: 1048576,
	}}, true)
	cloudClient, err := manager.GetClient("model")
	if err != nil {
		t.Fatal(err)
	}
	if cloudClient == localClient || cloudClient.numCtx != 0 {
		t.Fatalf("cached local client survived cloud policy: same=%v num_ctx=%d", cloudClient == localClient, cloudClient.numCtx)
	}

	manager.ConfigureOllamaInventory([]OllamaModel{{
		Name: "model:latest", Location: OllamaModelLocationLocal, ContextLength: 8192,
	}}, true)
	reducedLocalClient, err := manager.GetClient("model")
	if err != nil {
		t.Fatal(err)
	}
	if reducedLocalClient == cloudClient || reducedLocalClient.numCtx != 8192 {
		t.Fatalf("cached cloud client survived local policy: same=%v num_ctx=%d", reducedLocalClient == cloudClient, reducedLocalClient.numCtx)
	}
}

func TestModelManagerCurrentModel(t *testing.T) {
	m := NewModelManager("http://localhost:11434", 4096)

	// Should return empty when no model set
	if m.CurrentModel() != "" {
		t.Errorf("CurrentModel() = %q, want %q", m.CurrentModel(), "")
	}

	// Set a model
	if err := m.SetCurrentModel("llama3"); err != nil {
		t.Fatalf("SetCurrentModel: %v", err)
	}

	if m.CurrentModel() != "llama3" {
		t.Errorf("CurrentModel() = %q, want %q", m.CurrentModel(), "llama3")
	}
}

func TestModelManagerChatStreamNoModel(t *testing.T) {
	m := NewModelManager("http://localhost:11434", 4096)

	err := m.ChatStream(context.Background(), ChatOptions{}, func(chunk StreamChunk) error {
		return nil
	})

	if err == nil {
		t.Error("ChatStream should fail when no model is set")
	}
	if !errors.Is(err, ErrNoModelSelected) || !errors.Is(err, ErrInferenceNotStarted) {
		t.Fatalf("ChatStream error = %v, want pre-dispatch and no-model sentinels", err)
	}
}

func TestModelManagerChatStreamForModelMarksOnlyPreDispatchErrors(t *testing.T) {
	t.Run("context policy changed", func(t *testing.T) {
		manager := NewModelManager("http://localhost:11434", 8192)
		manager.ConfigureOllamaInventory([]OllamaModel{{
			Name:          "expert:3b",
			Location:      OllamaModelLocationLocal,
			ContextLength: 16_384,
		}}, true)
		err := manager.ChatStreamForModel(
			context.Background(),
			"expert:3b",
			ChatOptions{ExpectedContext: 4096},
			func(StreamChunk) error { return nil },
		)
		if !errors.Is(err, ErrInferenceNotStarted) || !strings.Contains(err.Error(), "context changed before inference") {
			t.Fatalf("context pre-dispatch error = %v", err)
		}
	})

	t.Run("client creation rejected", func(t *testing.T) {
		manager := NewModelManager("http://localhost:11434", 8192)
		err := manager.ChatStreamForModel(context.Background(), "", ChatOptions{}, func(StreamChunk) error { return nil })
		if !errors.Is(err, ErrInferenceNotStarted) || !strings.Contains(err.Error(), "model name is required") {
			t.Fatalf("client pre-dispatch error = %v", err)
		}
	})

	t.Run("provider client error is dispatch unknown", func(t *testing.T) {
		manager := NewModelManager("http://localhost:11434", 8192)
		err := manager.ChatStreamForModel(context.Background(), "expert:3b", ChatOptions{}, nil)
		if err == nil || errors.Is(err, ErrInferenceNotStarted) {
			t.Fatalf("provider client error = %v, want unmarked dispatch status", err)
		}
	})
}

func TestModelManagerPingNoModel(t *testing.T) {
	m := NewModelManager("http://localhost:11434", 4096)

	err := m.Ping()

	if err == nil {
		t.Error("Ping should fail when no model is set")
	}
}

func TestModelManagerPingContextHonorsCallerDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	client, err := NewOllamaClient(server.URL, "slow-model", 4096)
	if err != nil {
		t.Fatal(err)
	}
	m := NewModelManager(server.URL, 4096)
	m.currentModel = "slow-model"
	m.clients["slow-model"] = client

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = m.PingContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PingContext error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("PingContext ignored caller deadline: %v", elapsed)
	}
}

func TestModelManagerEmbedWithCurrentModelNoModel(t *testing.T) {
	m := NewModelManager("http://localhost:11434", 4096)

	_, err := m.EmbedWithCurrentModel(context.Background(), []string{"test"})

	if err == nil {
		t.Error("EmbedWithCurrentModel should fail when no model is set")
	}
}

func TestModelManagerClose(t *testing.T) {
	m := NewModelManager("http://localhost:11434", 4096)

	// Should not panic
	m.Close()

	if len(m.clients) != 0 {
		t.Errorf("after Close, clients map should be empty, got %d", len(m.clients))
	}
}

func TestModelManagerListLocalModelsExcludesCloud(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[
			{"name":"qwen3.5:2b","model":"qwen3.5:2b","size":123},
			{"name":"gemma-cloud","model":"gemma-cloud","remote_model":"gemma:cloud","size":0},
			{"name":"remote-host-only","model":"remote-host-only","remote_host":"https://cloud.invalid","size":999},
			{"name":"qwen3.5:4b","model":"qwen3.5:4b","size":456}
		]}`)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	got, err := manager.ListLocalModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"qwen3.5:2b", "qwen3.5:4b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %#v, want %#v", got, want)
	}
}

func TestModelManagerLocalOnlyAdmissionRefreshesAndFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[
			{"name":"qwen3.5:2b","model":"qwen3.5:2b","size":123},
			{"name":"remote:latest","model":"remote:latest","remote_model":"provider/model","size":0}
		]}`)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	manager.ConfigureLocalOnly(true, nil, false)
	if err := manager.SetCurrentModel("remote:latest"); err == nil || !strings.Contains(err.Error(), "not installed with local Ollama weights") {
		t.Fatalf("remote alias admission error = %v", err)
	}
	if err := manager.SetCurrentModel("qwen3.5:2b"); err != nil {
		t.Fatalf("local model rejected after inventory refresh: %v", err)
	}
	if _, err := manager.GetClient("remote:latest"); err == nil {
		t.Fatal("GetClient bypassed local-only admission")
	}
}

func TestModelManagerLocalOnlyRejectsUnavailableInventory(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	manager := NewModelManager(url, 4096)
	manager.ConfigureLocalOnly(true, nil, false)
	if err := manager.SetCurrentModel("innocent-alias:latest"); err == nil || !strings.Contains(err.Error(), "inventory is unavailable") {
		t.Fatalf("unverified inventory admission error = %v", err)
	}
}

func TestModelManagerLocalOnlyRejectsRemoteOllamaHostBeforeInventoryRequest(t *testing.T) {
	manager := NewModelManager("http://192.0.2.10:11434", 4096)
	manager.ConfigureLocalInventory(true, []LocalModel{{Name: "qwen3.5:2b", Size: 1 << 30}}, true)

	started := time.Now()
	_, err := manager.GetClient("qwen3.5:2b")
	if err == nil || !strings.Contains(err.Error(), "not a local-machine address") {
		t.Fatalf("remote Ollama local-only admission error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("remote host rejection waited for network I/O: %s", elapsed)
	}
}

func TestModelManagerLocalOnlyCloudGrantNeverAuthorizesRemoteOllamaHost(t *testing.T) {
	manager := NewModelManager("http://192.0.2.10:11434", 4096)
	manager.ConfigureLocalInventory(true, nil, true)
	manager.ConfigureOllamaCloudInventory([]string{"qwen:cloud"}, true)
	if err := manager.GrantOllamaCloudModel("qwen:cloud"); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	err := manager.ensureModelLocal(context.Background(), "qwen:cloud")
	if err == nil || !strings.Contains(err.Error(), "not a local-machine address") {
		t.Fatalf("remote Ollama cloud-grant admission error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("remote cloud-grant rejection waited for network I/O: %s", elapsed)
	}
	started = time.Now()
	if _, err := manager.PrepareExpertModels(context.Background(), []string{"qwen:cloud"}); err == nil || !strings.Contains(err.Error(), "not a local-machine address") {
		t.Fatalf("remote Ollama expert snapshot error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("remote expert snapshot rejection waited for network I/O: %s", elapsed)
	}
}

func TestModelManagerClearCurrentModelFailsClosed(t *testing.T) {
	manager := NewModelManager("http://localhost:11434", 4096)
	if err := manager.SetCurrentModel("qwen3.5:2b"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ClearCurrentModel(); err != nil {
		t.Fatal(err)
	}
	if manager.CurrentModel() != "" {
		t.Fatalf("cleared model = %q", manager.CurrentModel())
	}
	if err := manager.ChatStream(context.Background(), ChatOptions{}, func(StreamChunk) error { return nil }); !errors.Is(err, ErrNoModelSelected) {
		t.Fatalf("chat after clear error = %v, want ErrNoModelSelected", err)
	}
}

func TestModelManagerOllamaCloudGrantIsExactAndRevocable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[]}`)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	manager.ConfigureLocalInventory(true, nil, true)
	manager.ConfigureOllamaCloudInventory([]string{"qwen:cloud"}, true)
	if err := manager.GrantOllamaCloudModel("qwen:cloud"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ensureModelLocal(context.Background(), "qwen:cloud"); err != nil {
		t.Fatalf("exact cloud grant rejected: %v", err)
	}
	if err := manager.GrantOllamaCloudModel("other:cloud"); err == nil {
		t.Fatal("unverified cloud identity received a grant")
	}
	if err := manager.ensureModelLocal(context.Background(), "other:cloud"); err == nil {
		t.Fatal("exact cloud grant covered a different model")
	}
	manager.RevokeOllamaCloudModel("qwen:cloud")
	if err := manager.ensureModelLocal(context.Background(), "qwen:cloud"); err == nil {
		t.Fatal("revoked cloud grant remained usable")
	}
}

func TestModelManagerLocalOnlyMatchesImplicitLatestTag(t *testing.T) {
	manager := NewModelManager("http://localhost:11434", 4096)
	manager.ConfigureLocalInventory(true, []LocalModel{{Name: "nomic-embed-text:latest", Size: 1 << 30}}, true)
	if err := manager.ensureModelLocal(context.Background(), "nomic-embed-text"); err != nil {
		t.Fatalf("implicit :latest model was rejected: %v", err)
	}
	if err := manager.ensureModelLocal(context.Background(), "nomic-embed-text:v1"); err == nil {
		t.Fatal("a different explicit tag bypassed local-only admission")
	}
}

func TestModelManagerSwitchRefreshesLocalInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[]}`)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	manager.ConfigureLocalInventory(true, []LocalModel{{Name: "qwen3.5:2b", Size: 2 << 30}}, true)
	if err := manager.SetCurrentModel("qwen3.5:2b"); err == nil {
		t.Fatal("model switch trusted a stale startup inventory")
	}
}

func TestModelManagerRejectsOversizedInventoryWeights(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[
			{"name":"mixtral:8x7b","model":"mixtral:8x7b","size":10737418240},
			{"name":"deepseek-r1:latest","model":"deepseek-r1:latest","size":10737418240}
		]}`)
	}))
	defer server.Close()
	manager := NewModelManager(server.URL, 4096)
	manager.ConfigureLocalOnly(true, nil, false)
	for _, model := range []string{"mixtral:8x7b", "deepseek-r1:latest"} {
		if err := manager.SetCurrentModel(model); err == nil || !strings.Contains(err.Error(), "above the 8 GiB") {
			t.Fatalf("oversized model %q admission error = %v", model, err)
		}
	}
}

func TestModelManagerUnloadsPreviousModelOnSwitch(t *testing.T) {
	unloaded := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		unloaded <- string(body)
		_, _ = fmt.Fprintln(w, `{"done":true}`)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	if err := manager.SetCurrentModel("qwen3.5:2b"); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.active["qwen3.5:2b"] = true
	manager.mu.Unlock()
	if err := manager.SetCurrentModel("gemma4:e2b"); err != nil {
		t.Fatal(err)
	}

	select {
	case body := <-unloaded:
		if !strings.Contains(body, `"model":"qwen3.5:2b"`) || !strings.Contains(body, `"keep_alive":"0s"`) {
			t.Fatalf("unload request = %s", body)
		}
	case <-time.After(time.Second):
		t.Fatal("model switch did not unload the previous model")
	}
}

func TestModelManagerSwitchFailsIfPreviousModelCannotUnload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" {
			http.Error(w, "cannot unload", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	if err := manager.SetCurrentModel("model-a"); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.active["model-a"] = true
	manager.mu.Unlock()
	if err := manager.SetCurrentModel("model-b"); err == nil {
		t.Fatal("model switch succeeded after unload failure")
	}
	if got := manager.CurrentModel(); got != "model-a" {
		t.Fatalf("current model changed after failed unload: %q", got)
	}
}

func TestModelManagerCloseUnloadsActiveModels(t *testing.T) {
	unloaded := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		unloaded <- struct{}{}
		_, _ = fmt.Fprintln(w, `{"done":true}`)
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	if err := manager.SetCurrentModel("qwen3.5:2b"); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.active["qwen3.5:2b"] = true
	manager.mu.Unlock()
	manager.Close()

	select {
	case <-unloaded:
	case <-time.After(time.Second):
		t.Fatal("Close did not unload active model")
	}
	if manager.CurrentModel() != "" {
		t.Fatalf("current model survived close: %q", manager.CurrentModel())
	}
}

func TestModelManagerCloseUnloadsEmbeddingModel(t *testing.T) {
	unloaded := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embed":
			_, _ = fmt.Fprintln(w, `{"model":"nomic-embed-text","embeddings":[[0.1,0.2]]}`)
		case "/api/generate":
			body, _ := io.ReadAll(r.Body)
			unloaded <- string(body)
			_, _ = fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	if _, err := manager.Embed(context.Background(), "nomic-embed-text", []string{"hello"}); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	select {
	case body := <-unloaded:
		if !strings.Contains(body, `"model":"nomic-embed-text"`) {
			t.Fatalf("unloaded wrong model: %s", body)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unload resident embedding model")
	}
}

func TestModelManagerSwitchWaitsForActiveInference(t *testing.T) {
	chatStarted := make(chan struct{}, 1)
	releaseChat := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			chatStarted <- struct{}{}
			<-releaseChat
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"done"},"done":true}`)
		case "/api/generate":
			_, _ = fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 4096)
	if err := manager.SetCurrentModel("model-a"); err != nil {
		t.Fatal(err)
	}
	chatDone := make(chan error, 1)
	go func() {
		chatDone <- manager.ChatStream(context.Background(), ChatOptions{}, func(StreamChunk) error { return nil })
	}()
	select {
	case <-chatStarted:
	case <-time.After(time.Second):
		t.Fatal("chat did not start")
	}

	switchDone := make(chan error, 1)
	go func() { switchDone <- manager.SetCurrentModel("model-b") }()
	select {
	case err := <-switchDone:
		t.Fatalf("model switched during active inference: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseChat)
	select {
	case err := <-chatDone:
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("chat did not finish")
	}
	select {
	case err := <-switchDone:
		if err != nil {
			t.Fatalf("SetCurrentModel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("model switch stayed blocked after inference completed")
	}
}

func TestModelManagerSetNumCtxRebuildsLocalPolicy(t *testing.T) {
	manager := NewModelManager("http://127.0.0.1:9", 16_384)
	if err := manager.SetNumCtx(98_304); err != nil {
		t.Fatal(err)
	}
	if got := manager.ConfiguredNumCtx(); got != 98_304 {
		t.Fatalf("ConfiguredNumCtx = %d", got)
	}
	if got := manager.NumCtx(); got != 98_304 {
		t.Fatalf("NumCtx = %d", got)
	}
}
