package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOllamaInventoryPreservesLocalCloudCustomAndUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[
 {"name":"local-cloud-name:latest","model":"local-cloud-name:latest","size":42,"digest":"local-digest","capabilities":["completion","tools"],"details":{"format":"gguf","family":"custom","families":["custom"],"parameter_size":"1.2B","quantization_level":"Q4_K_M"}},
 {"name":"large:cloud","model":"large:cloud","size":0,"remote_model":"provider/large","remote_host":"https://ollama.com","digest":"cloud-digest"},
 {"name":"private:remote","model":"private:remote","size":0,"remote_model":"provider/private","remote_host":"https://models.example.test","digest":"remote-digest"},
 {"name":"metadata-only","size":0}
]}`)
	}))
	defer server.Close()
	client := newInventoryTestClient(t, server.URL)

	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 {
		t.Fatalf("models = %#v", models)
	}
	if models[0].Location != OllamaModelLocationLocal || models[0].Name != "local-cloud-name:latest" || models[0].Family != "custom" || models[0].Quantization != "Q4_K_M" {
		t.Fatalf("local custom model = %#v", models[0])
	}
	if !reflect.DeepEqual(models[0].Capabilities, []string{"completion", "tools"}) {
		t.Fatalf("tag capabilities = %#v", models[0].Capabilities)
	}
	if models[1].Location != OllamaModelLocationCloud || models[1].RemoteModel != "provider/large" {
		t.Fatalf("cloud model = %#v", models[1])
	}
	if models[2].Location != OllamaModelLocationRemote || models[2].RemoteHost != "https://models.example.test" {
		t.Fatalf("remote model = %#v", models[2])
	}
	if models[3].Location != OllamaModelLocationUnknown || models[3].Name != "metadata-only" {
		t.Fatalf("unknown model = %#v", models[3])
	}
}

func TestOllamaShowModelPreservesCapabilitiesAndNativeContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"capabilities":["completion","tools","thinking"],"details":{"family":"qwen3","parameter_size":"4B","quantization_level":"Q4"},"model_info":{"qwen3.context_length":131072,"unrelated.context_length":12},"parameters":"temperature 0.7","template":"template","license":"license"}`)
	}))
	defer server.Close()
	client := newInventoryTestClient(t, server.URL)

	info, err := client.ShowModel(context.Background(), " qwen3:4b ")
	if err != nil {
		t.Fatal(err)
	}
	if info.Model.Name != "qwen3:4b" || info.NativeContext != 131072 || info.Model.ParameterSize != "4B" {
		t.Fatalf("info = %#v", info)
	}
	if !reflect.DeepEqual(info.Capabilities, []string{"completion", "tools", "thinking"}) {
		t.Fatalf("capabilities = %#v", info.Capabilities)
	}
	info.ModelInfo["qwen3.context_length"] = float64(1)
	if info.NativeContext != 131072 {
		t.Fatal("derived context changed after metadata mutation")
	}
}

func TestOllamaShowModelFindsContextWithoutFamily(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"model_info":{"architecture.context_length":32768}}`)
	}))
	defer server.Close()
	info, err := newInventoryTestClient(t, server.URL).ShowModel(context.Background(), "custom")
	if err != nil {
		t.Fatal(err)
	}
	if info.NativeContext != 32768 {
		t.Fatalf("native context = %d", info.NativeContext)
	}
}

func TestOllamaRuntimeAndVersionMetadata(t *testing.T) {
	expires := "2026-07-12T15:00:00Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			_, _ = fmt.Fprintf(w, `{"models":[{"name":"qwen:latest","size":200,"size_vram":150,"context_length":16384,"expires_at":%q,"details":{"family":"qwen"}}]}`, expires)
		case "/api/version":
			_, _ = fmt.Fprint(w, `{"version":"0.12.6"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newInventoryTestClient(t, server.URL)
	running, err := client.ListRunningModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].ContextLength != 16384 || running[0].SizeVRAM != 150 || running[0].Model.Location != OllamaModelLocationLocal {
		t.Fatalf("running = %#v", running)
	}
	if running[0].ExpiresAt.Format(time.RFC3339) != expires {
		t.Fatalf("expiry = %s", running[0].ExpiresAt)
	}
	version, err := client.Version(context.Background())
	if err != nil || version != "0.12.6" {
		t.Fatalf("version = %q, %v", version, err)
	}
}

func TestOllamaPullModelStreamsProgressAndRequiresSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintln(w, `{"status":"pulling manifest"}`)
		_, _ = fmt.Fprintln(w, `{"status":"downloading","digest":"sha256:abc","total":100,"completed":75}`)
		_, _ = fmt.Fprintln(w, `{"status":"success"}`)
	}))
	defer server.Close()
	var got []OllamaPullProgress
	err := newInventoryTestClient(t, server.URL).PullModel(context.Background(), "qwen", func(progress OllamaPullProgress) error { got = append(got, progress); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].Completed != 75 || got[1].Total != 100 {
		t.Fatalf("progress = %#v", got)
	}
}

func TestOllamaPullModelPropagatesCancellationAndCallbackError(t *testing.T) {
	callbackErr := errors.New("stop progress")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, `{"status":"downloading"}`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-time.After(200 * time.Millisecond)
	}))
	defer server.Close()
	client := newInventoryTestClient(t, server.URL)
	if err := client.PullModel(context.Background(), "qwen", func(OllamaPullProgress) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Fatalf("callback error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.PullModel(ctx, "qwen", func(OllamaPullProgress) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestOllamaPullModelRejectsIncompleteAndInvalidInputs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, `{"status":"downloading"}`) }))
	defer server.Close()
	client := newInventoryTestClient(t, server.URL)
	if err := client.PullModel(context.Background(), "qwen", func(OllamaPullProgress) error { return nil }); err == nil || !strings.Contains(err.Error(), "before success") {
		t.Fatalf("incomplete error = %v", err)
	}
	if err := client.PullModel(context.Background(), " ", func(OllamaPullProgress) error { return nil }); err == nil {
		t.Fatal("empty model accepted")
	}
	if err := client.PullModel(context.Background(), "qwen", nil); err == nil {
		t.Fatal("nil callback accepted")
	}
}

func TestOllamaInventoryMethodsAreConcurrentSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[]}`)
		case "/api/version":
			_, _ = fmt.Fprint(w, `{"version":"1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newInventoryTestClient(t, server.URL)
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(2)
		go func() {
			defer group.Done()
			if _, err := client.ListModels(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer group.Done()
			if _, err := client.Version(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
}

func TestModelManagerExposesOllamaInventoryLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"qwen","size":42,"capabilities":["completion","tools"]}]}`)
		case "/api/show":
			_, _ = fmt.Fprint(w, `{"capabilities":["completion","tools"]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[]}`)
		case "/api/version":
			_, _ = fmt.Fprint(w, `{"version":"1.2.3"}`)
		case "/api/pull":
			_, _ = fmt.Fprintln(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager := NewModelManager(server.URL, 4096)
	models, err := manager.ListOllamaModels(context.Background())
	if err != nil || len(models) != 1 || models[0].Capabilities[1] != "tools" {
		t.Fatalf("inventory = %#v, %v", models, err)
	}
	if info, err := manager.ShowOllamaModel(context.Background(), "qwen"); err != nil || len(info.Capabilities) != 2 {
		t.Fatalf("show = %#v, %v", info, err)
	}
	if running, err := manager.ListRunningOllamaModels(context.Background()); err != nil || len(running) != 0 {
		t.Fatalf("running = %#v, %v", running, err)
	}
	if version, err := manager.OllamaVersion(context.Background()); err != nil || version != "1.2.3" {
		t.Fatalf("version = %q, %v", version, err)
	}
	if err := manager.PullOllamaModel(context.Background(), "qwen", func(OllamaPullProgress) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func newInventoryTestClient(t *testing.T, baseURL string) *OllamaClient {
	t.Helper()
	client, err := NewOllamaClient(baseURL, "", 4096)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
