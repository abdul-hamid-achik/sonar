package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ice"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

type inventoryAutoMemoryClient struct {
	started chan struct{}
	stopped chan struct{}
}

func (c *inventoryAutoMemoryClient) ChatStream(ctx context.Context, _ llm.ChatOptions, _ func(llm.StreamChunk) error) error {
	close(c.started)
	<-ctx.Done()
	close(c.stopped)
	return ctx.Err()
}

func (*inventoryAutoMemoryClient) Ping() error   { return nil }
func (*inventoryAutoMemoryClient) Model() string { return "inventory-auto-memory" }
func (*inventoryAutoMemoryClient) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}

func TestBuildOllamaModelDescriptorsAppliesCapabilitiesAndPrivacy(t *testing.T) {
	inventory := []llm.OllamaModel{
		{Name: "local-code", Location: llm.OllamaModelLocationLocal, SizeBytes: 2 << 30, ContextLength: 65536, Capabilities: []string{"completion", "tools"}},
		{Name: "cloud-code", Location: llm.OllamaModelLocationCloud, ContextLength: 262144, Capabilities: []string{"completion", "tools"}},
		{Name: "embed", Location: llm.OllamaModelLocationLocal, SizeBytes: 1 << 20, Capabilities: []string{"embedding"}},
	}
	models := BuildOllamaModelDescriptors(inventory, []llm.OllamaRunningModel{{Model: inventory[0], ContextLength: 32768}}, "local-code", true)
	if len(models) != 2 {
		t.Fatalf("chat inventory size = %d, want 2", len(models))
	}
	if !models[0].Current || !models[0].Running || models[0].ContextLength != 65536 || models[0].AllocatedContext != 32768 || !models[0].Selectable {
		t.Fatalf("local descriptor = %#v", models[0])
	}
	if !models[1].Selectable || models[1].Source != OllamaModelCloud || !models[1].RequiresConsent || models[1].AutoRoutable {
		t.Fatalf("local-only cloud descriptor = %#v", models[1])
	}
}

func TestRefreshEnrichesMissingCapabilitiesWithShow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"capabilities":["completion","tools"],"model_info":{"qwen.context_length":65536}}`)
	}))
	defer server.Close()
	manager := llm.NewModelManager(server.URL, 4096)
	models := enrichRefreshedOllamaCapabilities(context.Background(), manager, []llm.OllamaModel{{Name: "qwen", Location: llm.OllamaModelLocationLocal, SizeBytes: 1 << 30}}, nil)
	if len(models) != 1 || !hasOllamaCapability(models[0].Capabilities, "tools") || models[0].ContextLength != 65536 {
		t.Fatalf("enriched models = %#v", models)
	}
}

func TestApplyInventoryRecomputesCurrentModelAtReceiptTime(t *testing.T) {
	m := newTestModel(t)
	m.model = "model-b"
	m.modelPullState = NewModelPullState(false, true, defaultThemeID)
	m.modelPullState.Name = "model-b"
	m.modelPullState.Phase = ModelPullComplete
	m.modelInventoryRequest = 3
	m.applyOllamaInventory(OllamaModelInventoryMsg{RequestID: 3, Models: []OllamaModelDescriptor{
		{Name: "model-a", Current: true, Selectable: true, Fit: true},
		{Name: "model-b", Selectable: true, Fit: true},
	}})
	if m.ollamaModels[0].Current || !m.ollamaModels[1].Current {
		t.Fatalf("current projection = %#v", m.ollamaModels)
	}
	if got := m.modelPullState.Status; got != "Inventory refreshed" {
		t.Fatalf("pull inventory receipt = %q", got)
	}
	if got := ansi.Strip(m.modelPullState.View(58)); !strings.Contains(got, "is available · Inventory refreshed") {
		t.Fatalf("pull inventory receipt view = %q", got)
	}
	m.modelPullState.Name = "a-very-long-model-name"
	regular, cursor := m.modelPullState.ViewWithCursor(58, false)
	if cursor != nil {
		t.Fatal("completed pull receipt retained an input cursor")
	}
	if got := ansi.Strip(regular); !strings.Contains(got, "a-very") || !strings.Contains(got, "is available · Inventory refreshed") {
		t.Fatalf("regular pull inventory receipt view = %q", got)
	}
	assertRenderedLinesFit(t, regular, 58)

	compact, cursor := m.modelPullState.ViewWithCursor(24, true)
	if cursor != nil {
		t.Fatal("completed pull receipt retained an input cursor")
	}
	if got := ansi.Strip(compact); !strings.Contains(got, "a-very") || !strings.Contains(got, "refreshed") {
		t.Fatalf("compact pull inventory receipt view = %q", got)
	}
	assertRenderedLinesFit(t, compact, 24)
}

func TestInventoryCommitRunsOutsideUpdateDuringActiveTurn(t *testing.T) {
	m := newTestModel(t)
	m.modelManager = llm.NewModelManager("http://localhost:11434", 16384)
	m.state = StateWaiting
	m.modelInventoryRequest = 7
	message := OllamaModelInventoryMsg{RequestID: 7, Models: []OllamaModelDescriptor{{
		Name: "cloud-code", Source: OllamaModelCloud, ContextLength: 262144, Selectable: true, Fit: true,
	}}}

	updated, cmd := m.Update(message)
	m = updated.(*Model)
	if cmd == nil || !m.ollamaInventoryCommitting || len(m.ollamaModels) != 0 {
		t.Fatalf("inventory commit did not leave Update asynchronously: committing=%v models=%#v", m.ollamaInventoryCommitting, m.ollamaModels)
	}

	receipt := cmd()
	updated, _ = m.Update(receipt)
	m = updated.(*Model)
	if m.ollamaInventoryCommitting || len(m.ollamaModels) != 1 || m.ollamaModels[0].Name != "cloud-code" {
		t.Fatalf("committed receipt was not applied: committing=%v models=%#v", m.ollamaInventoryCommitting, m.ollamaModels)
	}
}

func TestInventoryCommitCancelsAndJoinsOptionalAutoMemory(t *testing.T) {
	dir := t.TempDir()
	client := &inventoryAutoMemoryClient{started: make(chan struct{}), stopped: make(chan struct{})}
	engine, err := ice.NewEngine(client, memory.NewStore(filepath.Join(dir, "memories.json")), ice.EngineConfig{
		StorePath: filepath.Join(dir, "conversations.json"), Workspace: dir, NumCtx: 16_384,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	m := newTestModel(t)
	m.agent.SetICEEngine(engine)
	m.modelManager = llm.NewModelManager("http://localhost:11434", 16_384)
	engine.DetectAutoMemory(context.Background(), strings.Repeat("user context ", 4), strings.Repeat("assistant context ", 4))
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("auto-memory did not start")
	}

	receipt := make(chan tea.Msg, 1)
	go func() {
		receipt <- m.commitOllamaInventory(OllamaModelInventoryMsg{RequestID: 6})()
	}()
	select {
	case <-client.stopped:
	case <-time.After(time.Second):
		t.Fatal("inventory commit did not cancel auto-memory")
	}
	select {
	case message := <-receipt:
		if _, ok := message.(ollamaModelInventoryCommittedMsg); !ok {
			t.Fatalf("inventory commit receipt = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("inventory commit did not join and return after cancellation")
	}
}

func TestInventoryRefreshRecoversOfflineCurrentLocalModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[{"name":"local-code:latest","model":"local-code:latest","size":1073741824}]}`)
	}))
	defer server.Close()

	manager := llm.NewModelManager(server.URL, 16_384)
	manager.ConfigureOllamaRuntimeInventory(true, nil, false)
	m := newTestModel(t)
	m.modelManager = manager
	m.model = "local-code"
	m.localOnly = true
	m.ollamaOffline = true
	m.modelInventoryRequest = 9

	updated, cmd := m.Update(OllamaModelInventoryMsg{RequestID: 9, Models: []OllamaModelDescriptor{{
		Name: "local-code:latest", Source: OllamaModelLocal, SizeBytes: 1 << 30,
		ContextLength: 65_536, Selectable: true, Fit: true, AutoRoutable: true,
	}}})
	m = updated.(*Model)
	if cmd == nil || !m.ollamaInventoryCommitting {
		t.Fatalf("offline recovery did not start an owned commit: cmd=%v committing=%v", cmd != nil, m.ollamaInventoryCommitting)
	}

	updated, _ = m.Update(cmd())
	m = updated.(*Model)
	if got := manager.CurrentModel(); config.CanonicalModelName(got) != config.CanonicalModelName("local-code") {
		t.Fatalf("manager model after recovery = %q", got)
	}
	if m.ollamaOffline || m.ollamaInventoryCommitting {
		t.Fatalf("recovery state offline=%v committing=%v", m.ollamaOffline, m.ollamaInventoryCommitting)
	}
	if len(m.entries) == 0 || m.entries[len(m.entries)-1].Kind != "system" || !strings.Contains(m.entries[len(m.entries)-1].Content, "Ollama reconnected") {
		t.Fatalf("recovery receipt = %#v", m.entries)
	}
}

func TestInventoryRecoveryNeverImplicitlySelectsCloudOrUnsafeModel(t *testing.T) {
	models := []OllamaModelDescriptor{
		{Name: "cloud-code", Source: OllamaModelCloud, Selectable: true, Fit: true},
		{Name: "too-large", Source: OllamaModelLocal, Selectable: true, Fit: false},
	}
	if got := recoverableLocalModel(models, "cloud-code"); got != "" {
		t.Fatalf("cloud recovery target = %q", got)
	}
	if got := recoverableLocalModel(models, "too-large"); got != "" {
		t.Fatalf("unsafe recovery target = %q", got)
	}
}

func TestShutdownWaitsForOllamaInventoryCommitReceipt(t *testing.T) {
	m := newTestModel(t)
	m.ollamaInventoryCommitting = true
	m.ollamaInventoryCommitID = 12
	m.modelInventoryRequest = 12

	if cmd := m.beginShutdown(); cmd == nil || m.shutdownReady() {
		t.Fatalf("shutdown did not retain inventory ownership: cmd=%v ready=%v", cmd != nil, m.shutdownReady())
	}
	updated, quit := m.Update(ollamaModelInventoryCommittedMsg{
		Inventory: OllamaModelInventoryMsg{RequestID: 12},
	})
	m = updated.(*Model)
	if quit == nil || !m.shutdownReady() || m.ollamaInventoryCommitting {
		t.Fatalf("inventory receipt did not release shutdown: quit=%v ready=%v committing=%v", quit != nil, m.shutdownReady(), m.ollamaInventoryCommitting)
	}
}

func TestBuildOllamaModelDescriptorsKeepsCloudManualWhenLocalOnlyDisabled(t *testing.T) {
	models := BuildOllamaModelDescriptors([]llm.OllamaModel{{
		Name: "qwen:cloud", Location: llm.OllamaModelLocationCloud, ContextLength: 262144, Capabilities: []string{"completion", "tools"},
	}}, nil, "", false)
	if len(models) != 1 || !models[0].Selectable || !models[0].Fit || models[0].AutoRoutable || models[0].RequiresConsent {
		t.Fatalf("cloud descriptor = %#v", models)
	}
}

func TestBuildOllamaModelDescriptorsRejectsCloudWithoutContextMaximum(t *testing.T) {
	models := BuildOllamaModelDescriptors([]llm.OllamaModel{{
		Name: "unknown:cloud", Location: llm.OllamaModelLocationCloud, Capabilities: []string{"completion", "tools"},
	}}, nil, "", false)
	if len(models) != 1 || models[0].Selectable || models[0].AutoRoutable || !strings.Contains(models[0].Reason, "context maximum unavailable") {
		t.Fatalf("unknown-context cloud descriptor = %#v", models)
	}
}

type inventoryAvailabilityRouter struct {
	stubRouter
	available []string
}

func (r *inventoryAvailabilityRouter) SetAvailableModels(models []string) {
	r.available = append([]string(nil), models...)
}

func TestCloudStaysManuallySelectableWhileRouterReceivesOnlyLocals(t *testing.T) {
	for _, localOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("local_only_%t", localOnly), func(t *testing.T) {
			m := newTestModel(t)
			router := &inventoryAvailabilityRouter{}
			m.router = router
			m.localOnly = localOnly
			m.modelInventoryRequest = 4
			m.applyOllamaInventory(OllamaModelInventoryMsg{RequestID: 4, Models: []OllamaModelDescriptor{
				{Name: "local-code", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true},
				// Deliberately malformed routing metadata proves Source remains
				// the final fail-closed boundary at inventory application.
				{Name: "cloud-code", Source: OllamaModelCloud, Selectable: true, Fit: true, AutoRoutable: true, RequiresConsent: localOnly},
				{Name: "remote-code", Source: OllamaModelRemote, Selectable: false, Fit: true, AutoRoutable: true},
			}})
			if got := strings.Join(m.modelList, ","); got != "local-code,cloud-code" {
				t.Fatalf("manual model list = %q, want local and cloud", got)
			}
			result := m.cmdRegistry.Execute(m.buildCommandContext(), "model", []string{"cloud-code"})
			if result.Action != command.ActionSwitchModel || result.Data != "cloud-code" {
				t.Fatalf("manual cloud command = %#v", result)
			}
			if got := strings.Join(router.available, ","); got != "local-code" {
				t.Fatalf("router inventory = %q, want local-code only", got)
			}
		})
	}
}

func TestInventoryRefreshKeepsExclusiveLocalModelsManualOnly(t *testing.T) {
	m := newTestModel(t)
	router := &inventoryAvailabilityRouter{}
	m.router = router
	m.SetModelRoutingCatalog([]config.Model{
		{Name: "qwen3.5:2b"},
		{Name: "phi4-mini:latest", Exclusive: true},
		{Name: "ornith:latest", Exclusive: true},
		{Name: "gemma4:e2b", Exclusive: true},
	})
	m.modelInventoryRequest = 9
	m.applyOllamaInventory(OllamaModelInventoryMsg{RequestID: 9, Models: []OllamaModelDescriptor{
		{Name: "qwen3.5:2b", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true},
		{Name: "phi4-mini:latest", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true},
		{Name: "ornith:latest", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true},
		{Name: "gemma4:e2b", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true},
	}})

	if got := strings.Join(m.modelList, ","); got != "qwen3.5:2b,phi4-mini:latest,ornith:latest,gemma4:e2b" {
		t.Fatalf("manual model list = %q, want all locally selectable profiles", got)
	}
	if got := strings.Join(router.available, ","); got != "qwen3.5:2b" {
		t.Fatalf("automatic router inventory = %q, want only non-exclusive Qwen", got)
	}
	for _, model := range m.ollamaModels {
		wantAuto := model.Name == "qwen3.5:2b"
		if model.AutoRoutable != wantAuto {
			t.Errorf("%s AutoRoutable = %v, want %v", model.Name, model.AutoRoutable, wantAuto)
		}
		if model.ManualOnly == wantAuto {
			t.Errorf("%s ManualOnly = %v, want %v", model.Name, model.ManualOnly, !wantAuto)
		}
	}
}

func TestRefreshedCurrentModelReconciliationKeepsCloudManualOnly(t *testing.T) {
	localCurrent := OllamaModelDescriptor{
		Name: "same", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true,
	}
	cloudCurrent := OllamaModelDescriptor{
		Name: "same", Source: OllamaModelCloud, Selectable: true, Fit: true,
	}
	fallback := OllamaModelDescriptor{
		Name: "local-fallback", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true,
	}

	t.Run("automatic local reclassified as cloud falls back locally", func(t *testing.T) {
		decision := reconcileRefreshedCurrentModel(
			[]OllamaModelDescriptor{localCurrent},
			[]OllamaModelDescriptor{cloudCurrent, fallback},
			"same", false, false,
		)
		if !decision.Change || decision.FallbackModel != fallback.Name || !strings.Contains(decision.Reason, "manual selection") {
			t.Fatalf("decision = %#v", decision)
		}
	})

	t.Run("pinned local reclassified as cloud is cleared", func(t *testing.T) {
		decision := reconcileRefreshedCurrentModel(
			[]OllamaModelDescriptor{localCurrent},
			[]OllamaModelDescriptor{cloudCurrent, fallback},
			"same", true, false,
		)
		if !decision.Change || decision.FallbackModel != "" {
			t.Fatalf("decision = %#v", decision)
		}
	})

	t.Run("existing pinned cloud remains selected", func(t *testing.T) {
		decision := reconcileRefreshedCurrentModel(
			[]OllamaModelDescriptor{cloudCurrent},
			[]OllamaModelDescriptor{cloudCurrent, fallback},
			"same", true, false,
		)
		if decision.Change {
			t.Fatalf("manual cloud selection was not preserved: %#v", decision)
		}
	})

	t.Run("existing consented cloud remains selected in local-only", func(t *testing.T) {
		consented := cloudCurrent
		consented.ConsentGranted = true
		decision := reconcileRefreshedCurrentModel(
			[]OllamaModelDescriptor{consented},
			[]OllamaModelDescriptor{cloudCurrent, fallback},
			"same", true, true,
		)
		if decision.Change {
			t.Fatalf("consented cloud selection was not preserved: %#v", decision)
		}
	})

	t.Run("unpinned cloud cannot survive refresh", func(t *testing.T) {
		decision := reconcileRefreshedCurrentModel(
			[]OllamaModelDescriptor{cloudCurrent},
			[]OllamaModelDescriptor{cloudCurrent, fallback},
			"same", false, false,
		)
		if !decision.Change || decision.FallbackModel != fallback.Name {
			t.Fatalf("decision = %#v", decision)
		}
	})

	t.Run("verified local current remains selected", func(t *testing.T) {
		decision := reconcileRefreshedCurrentModel(
			[]OllamaModelDescriptor{localCurrent},
			[]OllamaModelDescriptor{localCurrent, fallback},
			"same", false, false,
		)
		if decision.Change {
			t.Fatalf("local current was rejected: %#v", decision)
		}
	})
}

func TestInventoryCommitFallsBackBeforeReclassifiedCloudCanRun(t *testing.T) {
	manager := llm.NewModelManager("http://localhost:11434", 16_384)
	manager.ConfigureOllamaRuntimeInventory(false, []llm.OllamaModel{{
		Name: "same", Location: llm.OllamaModelLocationLocal, SizeBytes: 1 << 30, ContextLength: 65_536,
	}}, true)
	if err := manager.SetCurrentModel("same"); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.modelManager = manager
	m.model = "same"
	m.modelPinned = false
	m.localOnly = false
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "same", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true,
	}}
	m.modelInventoryRequest = 21
	message := OllamaModelInventoryMsg{RequestID: 21, Models: []OllamaModelDescriptor{
		{Name: "same", Source: OllamaModelCloud, Selectable: true, Fit: true, ContextLength: 262_144},
		{Name: "local-fallback", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true, SizeBytes: 1 << 30, ContextLength: 65_536},
	}}

	updated, cmd := m.Update(message)
	m = updated.(*Model)
	if cmd == nil || !m.ollamaInventoryCommitting {
		t.Fatalf("inventory commit command=%v committing=%v", cmd != nil, m.ollamaInventoryCommitting)
	}
	updated, _ = m.Update(cmd())
	m = updated.(*Model)
	if manager.CurrentModel() != "local-fallback" || m.model != "local-fallback" || m.modelPinned {
		t.Fatalf("fallback manager=%q ui=%q pinned=%v", manager.CurrentModel(), m.model, m.modelPinned)
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "resumed automatic routing") {
		t.Fatalf("fallback receipt = %#v", m.entries)
	}
}

func TestInventoryCommitClearsPinnedLocalReclassifiedAsCloud(t *testing.T) {
	manager := llm.NewModelManager("http://localhost:11434", 16_384)
	manager.ConfigureOllamaRuntimeInventory(false, []llm.OllamaModel{{
		Name: "same", Location: llm.OllamaModelLocationLocal, SizeBytes: 1 << 30, ContextLength: 65_536,
	}}, true)
	if err := manager.SetCurrentModel("same"); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.modelManager = manager
	m.model = "same"
	m.modelPinned = true
	m.localOnly = false
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "same", Source: OllamaModelLocal, Selectable: true, Fit: true, AutoRoutable: true,
	}}
	m.modelInventoryRequest = 22
	message := OllamaModelInventoryMsg{RequestID: 22, Models: []OllamaModelDescriptor{{
		Name: "same", Source: OllamaModelCloud, Selectable: true, Fit: true, ContextLength: 262_144,
	}}}

	updated, cmd := m.Update(message)
	m = updated.(*Model)
	updated, _ = m.Update(cmd())
	m = updated.(*Model)
	if manager.CurrentModel() != "" || m.model != "" || !m.modelPinned {
		t.Fatalf("clear manager=%q ui=%q pinned=%v", manager.CurrentModel(), m.model, m.modelPinned)
	}
	if err := manager.ChatStream(context.Background(), llm.ChatOptions{}, func(llm.StreamChunk) error { return nil }); !errors.Is(err, llm.ErrNoModelSelected) {
		t.Fatalf("chat after reclassification error = %v", err)
	}
}

func TestInventoryCommitOwnsInputUntilSelectionIsReconciled(t *testing.T) {
	m := newTestModel(t)
	m.ollamaInventoryCommitting = true
	m.input.SetValue("must not dispatch under stale authority")

	updated, cmd := m.Update(enterKey())
	m = updated.(*Model)
	if cmd != nil || m.state != StateIdle || m.input.Value() != "must not dispatch under stale authority" {
		t.Fatalf("commit raced input: cmd=%v state=%v draft=%q", cmd != nil, m.state, m.input.Value())
	}
	if working := m.renderWorkingLine(); !strings.Contains(working, "Updating Ollama inventory") {
		t.Fatalf("inventory ownership is not visible: %q", working)
	}
}

func TestBuildOllamaModelDescriptorsKeepsUnknownCapabilitiesDisabled(t *testing.T) {
	models := BuildOllamaModelDescriptors([]llm.OllamaModel{{
		Name: "legacy-unknown", Location: llm.OllamaModelLocationLocal, SizeBytes: 1 << 30,
	}}, nil, "", false)
	if len(models) != 1 || models[0].Selectable || !strings.Contains(models[0].Reason, "capabilities unknown") {
		t.Fatalf("unknown capability descriptor = %#v", models)
	}
}

func TestBuildOllamaModelDescriptorsNeverAdmitsArbitraryRemoteHost(t *testing.T) {
	models := BuildOllamaModelDescriptors([]llm.OllamaModel{{
		Name: "private-remote", Location: llm.OllamaModelLocationRemote, Capabilities: []string{"completion", "tools"},
	}}, nil, "", false)
	if len(models) != 1 || models[0].Selectable || models[0].AutoRoutable || !strings.Contains(models[0].Reason, "not Ollama Cloud") {
		t.Fatalf("remote-host descriptor = %#v", models)
	}
}

func TestOllamaInventorySummaryKeepsRemoteBoundaryVisible(t *testing.T) {
	models := []OllamaModelDescriptor{
		{Source: OllamaModelLocal, Selectable: true, Fit: true},
		{Source: OllamaModelCloud, Selectable: true, Fit: true},
		{Source: OllamaModelRemote, Selectable: true, Fit: true},
		{Source: OllamaModelCloud, Selectable: false, Fit: true},
	}
	want := "1 local · 1 cloud · 1 remote · 1 unavailable"
	if got := ollamaInventorySummary(models); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}
