package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpertModelLeaseUsesLiveWeightsSnapshotsAllResidencyAndUnloadsOnlyNewExpert(t *testing.T) {
	var mu sync.Mutex
	unloaded := make([]string, 0, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[
				{"name":"current:2b","model":"current:2b","size":2147483648},
				{"name":"old:4b","model":"old:4b","size":4294967296},
				{"name":"expert:3b","model":"expert:3b","size":3221225472}
			]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"old:4b","model":"old:4b","size":4294967296,"size_vram":3221225472,"context_length":16384}]}`)
		case "/api/chat":
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"report"},"done":true}`)
		case "/api/generate":
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &request)
			mu.Lock()
			unloaded = append(unloaded, request.Model)
			mu.Unlock()
			_, _ = fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	if err := manager.SetCurrentModel("current:2b"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"expert:3b"})
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]ExpertModelResource, len(snapshot.Models))
	for _, model := range snapshot.Models {
		byName[modelResourceKey(model.Name)] = model
	}
	if !snapshot.InventoryVerified || byName["expert:3b"].WeightBytes != 3<<30 || !byName["expert:3b"].Selected {
		t.Fatalf("selected live resource = %#v", byName["expert:3b"])
	}
	if old := byName["old:4b"]; !old.Resident || old.ResidentBytes != 4<<30 || old.ContextLength != 16384 {
		t.Fatalf("old resident resource = %#v", old)
	}
	if current := byName["current:2b"]; !current.Current {
		t.Fatalf("current resource = %#v", current)
	}

	if err := manager.ChatStreamForModel(context.Background(), "expert:3b", ChatOptions{}, func(StreamChunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseExpertModels(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(unloaded) != 1 || unloaded[0] != "expert:3b" {
		t.Fatalf("unloaded models = %#v, want only expert:3b", unloaded)
	}
}

func TestExpertModelSelectionDoesNotGrantOllamaCloudConsent(t *testing.T) {
	var chatRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"expert:cloud","model":"expert:cloud","size":0,"remote_model":"provider/expert","remote_host":"https://ollama.com","details":{"context_length":65536}}]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[]}`)
		case "/api/chat":
			chatRequests.Add(1)
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"report"},"done":true,"eval_count":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	manager.ConfigureOllamaRuntimeInventory(true, []OllamaModel{{
		Name: "expert:cloud", Location: OllamaModelLocationCloud, ContextLength: 65536,
	}}, true)
	snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"expert:cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ChatStreamForModel(context.Background(), "expert:cloud", ChatOptions{}, func(StreamChunk) error { return nil }); err == nil {
		t.Fatal("selected expert model bypassed cloud consent")
	}
	if chatRequests.Load() != 0 {
		t.Fatalf("unconsented cloud expert dispatched %d provider request(s)", chatRequests.Load())
	}
	if err := manager.ReleaseExpertModels(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestExpertModelLeaseRefreshesWeightsForEveryConsultation(t *testing.T) {
	var size atomic.Int64
	size.Store(2 << 30)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprintf(w, `{"models":[{"name":"expert:latest","model":"expert:latest","size":%d}]}`, size.Load())
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	for _, want := range []int64{2 << 30, 5 << 30} {
		size.Store(want)
		snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"expert"})
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Models) != 1 || snapshot.Models[0].WeightBytes != want {
			t.Fatalf("live snapshot weight = %#v, want %d", snapshot.Models, want)
		}
		if err := manager.ReleaseExpertModels(context.Background(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExpertModelLeaseProtectsPreexistingResidentSelectedModel(t *testing.T) {
	var unloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"expert:3b","model":"expert:3b","size":3221225472}]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"expert:3b","model":"expert:3b","size":3221225472,"size_vram":3221225472}]}`)
		case "/api/chat":
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"report"},"done":true}`)
		case "/api/generate":
			unloads.Add(1)
			_, _ = fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	for consultation := 0; consultation < 2; consultation++ {
		snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"expert:3b"})
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.ChatStreamForModel(context.Background(), "expert:3b", ChatOptions{}, func(StreamChunk) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := manager.ReleaseExpertModels(context.Background(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if unloads.Load() != 0 {
		t.Fatalf("preexisting resident model was unloaded across repeated consultations %d time(s)", unloads.Load())
	}
}

func TestExpertModelLeaseNeverUnloadsCurrentSelectedModel(t *testing.T) {
	var unloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"current:2b","model":"current:2b","size":2147483648}]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[]}`)
		case "/api/chat":
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"report"},"done":true}`)
		case "/api/generate":
			unloads.Add(1)
			_, _ = fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	if err := manager.SetCurrentModel("current:2b"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"current:2b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ChatStreamForModel(context.Background(), "current:2b", ChatOptions{}, func(StreamChunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseExpertModels(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if unloads.Load() != 0 {
		t.Fatalf("current model was unloaded %d time(s)", unloads.Load())
	}
}

func TestExpertModelLeaseReclaimsPreviouslyExpertOwnedResidentModel(t *testing.T) {
	var unloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"expert:3b","model":"expert:3b","size":3221225472}]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"expert:3b","model":"expert:3b","size":3221225472,"size_vram":3221225472}]}`)
		case "/api/generate":
			unloads.Add(1)
			_, _ = fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	client, err := NewOllamaClient(server.URL, "expert:3b", 8192)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.clients["expert:3b"] = client
	manager.active["expert:3b"] = true
	manager.markExpertActivityLocked("expert:3b")
	manager.mu.Unlock()

	snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"expert:3b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseExpertModels(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if unloads.Load() != 1 {
		t.Fatalf("previous expert residency unloads = %d, want 1", unloads.Load())
	}
}

func TestExpertModelLeasePreservesNonExpertOwnershipAcrossRepeatedConsultations(t *testing.T) {
	var unloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"shared:2b","model":"shared:2b","size":2147483648}]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[]}`)
		case "/api/chat":
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"report"},"done":true}`)
		case "/api/embed":
			_, _ = fmt.Fprintln(w, `{"model":"shared:2b","embeddings":[[0.1,0.2]]}`)
		case "/api/generate":
			unloads.Add(1)
			_, _ = fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	if _, err := manager.Embed(context.Background(), "shared:2b", []string{"keep this model"}); err != nil {
		t.Fatal(err)
	}
	for consultation := 0; consultation < 2; consultation++ {
		snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"shared:2b"})
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.ChatStreamForModel(context.Background(), "shared:2b", ChatOptions{}, func(StreamChunk) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := manager.ReleaseExpertModels(context.Background(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if unloads.Load() != 0 {
		t.Fatalf("pre-existing non-expert model user was evicted %d time(s)", unloads.Load())
	}
}

func TestExpertModelLeaseBlocksOrdinaryInferenceUntilRelease(t *testing.T) {
	var chats atomic.Int32
	var embeds atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[
				{"name":"current:2b","model":"current:2b","size":2147483648},
				{"name":"expert:3b","model":"expert:3b","size":3221225472},
				{"name":"other:2b","model":"other:2b","size":2147483648},
				{"name":"embed:1b","model":"embed:1b","size":1073741824}
			]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[]}`)
		case "/api/chat":
			chats.Add(1)
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"ordinary"},"done":true}`)
		case "/api/embed":
			embeds.Add(1)
			_, _ = fmt.Fprintln(w, `{"model":"embed:1b","embeddings":[[0.1,0.2]]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	if err := manager.SetCurrentModel("current:2b"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"expert:3b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ChatStreamForModel(context.Background(), "other:2b", ChatOptions{}, func(StreamChunk) error { return nil }); !errors.Is(err, ErrInferenceNotStarted) {
		t.Fatalf("unreserved expert dispatch error = %v, want pre-dispatch rejection", err)
	}

	chatCtx, cancelChat := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = manager.ChatStream(chatCtx, ChatOptions{}, func(StreamChunk) error { return nil })
	cancelChat()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrInferenceNotStarted) {
		t.Fatalf("ordinary chat during reservation error = %v", err)
	}
	embedCtx, cancelEmbed := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err = manager.Embed(embedCtx, "embed:1b", []string{"blocked"})
	cancelEmbed()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ordinary embed during reservation error = %v", err)
	}
	if chats.Load() != 0 || embeds.Load() != 0 {
		t.Fatalf("ordinary inference reached Ollama during reservation: chats=%d embeds=%d", chats.Load(), embeds.Load())
	}

	if err := manager.ReleaseExpertModels(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Embed(context.Background(), "embed:1b", []string{"allowed"}); err != nil {
		t.Fatal(err)
	}
	if embeds.Load() != 1 {
		t.Fatalf("embeddings after reservation = %d, want 1", embeds.Load())
	}
}

func TestExpertModelCleanupDeadlineDoesNotWaitForInferenceLock(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	var finishOnce sync.Once
	closeFinish := func() { finishOnce.Do(func() { close(finish) }) }
	t.Cleanup(closeFinish)
	var embeds atomic.Int32
	var unloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[
				{"name":"expert:3b","model":"expert:3b","size":3221225472},
				{"name":"embed:1b","model":"embed:1b","size":1073741824}
			]}`)
		case "/api/ps":
			_, _ = fmt.Fprint(w, `{"models":[]}`)
		case "/api/chat":
			close(started)
			<-finish
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"expert"},"done":true}`)
		case "/api/embed":
			embeds.Add(1)
			_, _ = fmt.Fprintln(w, `{"model":"embed:1b","embeddings":[[0.1,0.2]]}`)
		case "/api/generate":
			unloads.Add(1)
			_, _ = fmt.Fprintln(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(server.URL, 8192)
	snapshot, err := manager.PrepareExpertModels(context.Background(), []string{"expert:3b"})
	if err != nil {
		t.Fatal(err)
	}
	chatDone := make(chan error, 1)
	go func() {
		chatDone <- manager.ChatStreamForModel(context.Background(), "expert:3b", ChatOptions{}, func(StreamChunk) error { return nil })
	}()
	<-started

	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 40*time.Millisecond)
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- manager.ReleaseExpertModels(cleanupCtx, snapshot) }()
	select {
	case err := <-cleanupDone:
		cancelCleanup()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cleanup deadline error = %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		cancelCleanup()
		closeFinish()
		<-cleanupDone
		t.Fatal("expert cleanup blocked past its context deadline")
	}
	if unloads.Load() != 0 {
		t.Fatalf("cleanup unloaded an in-flight expert %d time(s)", unloads.Load())
	}

	embedCtx, cancelEmbed := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err = manager.Embed(embedCtx, "embed:1b", []string{"still blocked"})
	cancelEmbed()
	if !errors.Is(err, context.DeadlineExceeded) || embeds.Load() != 0 {
		t.Fatalf("ordinary embed while timed-out expert remains active: error=%v requests=%d", err, embeds.Load())
	}

	closeFinish()
	if err := <-chatDone; err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Embed(context.Background(), "embed:1b", []string{"released"}); err != nil {
		t.Fatal(err)
	}
	if embeds.Load() != 1 {
		t.Fatalf("embeddings after expert drain = %d, want 1", embeds.Load())
	}
}
