package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type deadlineToolCaller struct {
	started chan struct{}
}

type recordingToolCaller struct {
	calls atomic.Int64
}

func TestRegistryRefreshesServerDrivenToolListChanges(t *testing.T) {
	downstream := sdk.NewServer(&sdk.Implementation{Name: "dynamic", Version: "1"}, nil)
	downstream.AddTool(&sdk.Tool{
		Name: "first", InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return downstream }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	registry := NewRegistry()
	defer registry.Close()
	if _, err := registry.ConnectServer(context.Background(), config.ServerConfig{
		Name: "dynamic", Transport: "streamable-http", URL: server.URL,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.ResolveToolName("first"); !ok {
		t.Fatal("initial tool was not registered")
	}
	initialEpoch := registry.SnapshotTools().Epoch

	downstream.AddTool(&sdk.Tool{
		Name: "second", InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := registry.SnapshotTools()
		if snapshot.Epoch > initialEpoch {
			if name, ok := registry.ResolveToolName("second"); ok && name == "dynamic__second" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dynamic tool did not enter registry: %#v", registry.SnapshotTools())
}

func (c *recordingToolCaller) CallTool(context.Context, string, map[string]any) (*ToolResult, error) {
	c.calls.Add(1)
	return &ToolResult{Content: "ok"}, nil
}

func (c *deadlineToolCaller) CallTool(ctx context.Context, _ string, _ map[string]any) (*ToolResult, error) {
	close(c.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestNewRegistry(t *testing.T) {
	r := NewRegistry()
	if r.version != developmentImplementationVersion {
		t.Fatalf("default registry version = %q, want %q", r.version, developmentImplementationVersion)
	}

	if r.ToolCount() != 0 {
		t.Errorf("ToolCount() = %d, want 0", r.ToolCount())
	}
	if r.ServerCount() != 0 {
		t.Errorf("ServerCount() = %d, want 0", r.ServerCount())
	}
	if tools := r.Tools(); len(tools) != 0 {
		t.Errorf("Tools() = %v, want empty", tools)
	}
}

func TestNewRegistryWithVersion(t *testing.T) {
	r := NewRegistryWithVersion("0.3.0", WithLocalOnly(true))
	if r.version != "0.3.0" {
		t.Fatalf("release registry version = %q, want 0.3.0", r.version)
	}
	if !r.localOnly {
		t.Fatal("local-only registry option was not applied")
	}
}

func TestRegistry_CallTool_Unknown(t *testing.T) {
	r := NewRegistry()

	result, err := r.CallTool(context.Background(), "nonexistent_tool", nil)
	if err != nil {
		t.Fatalf("CallTool() unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("CallTool() IsError = false, want true for unknown tool")
	}
	if !strings.Contains(result.Content, "unknown tool") {
		t.Errorf("CallTool() Content = %q, want to contain 'unknown tool'", result.Content)
	}
}

func TestRegistryCallToolPreservesDispatchDeadline(t *testing.T) {
	r := NewRegistry()
	caller := &deadlineToolCaller{started: make(chan struct{})}
	r.toolMap["srv__mutate"] = toolRoute{client: caller, remoteName: "mutate"}
	r.SetCallTimeout(10 * time.Millisecond)
	_, err := r.CallTool(context.Background(), "srv__mutate", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
	select {
	case <-caller.started:
	default:
		t.Fatal("MCP backend was not dispatched")
	}
}

func TestRegistryCallToolAtEpochRejectsChangedCatalogBeforeDispatch(t *testing.T) {
	r := NewRegistry()
	caller := &recordingToolCaller{}
	r.toolMap["srv__inspect"] = toolRoute{client: caller, remoteName: "inspect"}
	epoch := r.SnapshotTools().Epoch

	r.mu.Lock()
	r.epoch++
	r.mu.Unlock()

	result, err := r.CallToolAtEpoch(context.Background(), epoch, "srv__inspect", nil)
	if !errors.Is(err, ErrRegistryEpochChanged) || result != nil {
		t.Fatalf("stale call result=%#v error=%v", result, err)
	}
	if caller.calls.Load() != 0 {
		t.Fatal("stale catalog call reached the captured backend")
	}
}

func TestRegistryCallToolAtEpochDispatchesCapturedCurrentRoute(t *testing.T) {
	r := NewRegistry()
	caller := &recordingToolCaller{}
	r.toolMap["srv__inspect"] = toolRoute{client: caller, remoteName: "inspect"}
	epoch := r.SnapshotTools().Epoch

	result, err := r.CallToolAtEpoch(context.Background(), epoch, "srv__inspect", map[string]any{"scope": "safe"})
	if err != nil || result == nil || result.Content != "ok" {
		t.Fatalf("current call result=%#v error=%v", result, err)
	}
	if caller.calls.Load() != 1 {
		t.Fatalf("current catalog backend calls = %d, want 1", caller.calls.Load())
	}
}

func TestRegistryCallToolAtEpochRejectsDispatchAfterCloseAdmissionStops(t *testing.T) {
	r := NewRegistry()
	caller := &recordingToolCaller{}
	r.toolMap["srv__inspect"] = toolRoute{client: caller, remoteName: "inspect"}
	epoch := r.SnapshotTools().Epoch

	// Hold one admitted lifecycle operation so Close remains in its intentional
	// wait window after setting closed=true but before clearing routes/epoch.
	_, finish, err := r.beginLifecycleOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan struct{})
	go func() {
		r.Close()
		close(closeDone)
	}()
	deadline := time.After(time.Second)
	for {
		r.mu.RLock()
		closed := r.closed
		r.mu.RUnlock()
		if closed {
			break
		}
		select {
		case <-deadline:
			finish()
			t.Fatal("Close did not stop lifecycle admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	result, callErr := r.CallToolAtEpoch(context.Background(), epoch, "srv__inspect", nil)
	if result != nil || !errors.Is(callErr, ErrRegistryClosed) {
		t.Fatalf("close-in-progress call result=%#v error=%v, want ErrRegistryClosed", result, callErr)
	}
	if caller.calls.Load() != 0 {
		t.Fatal("call admitted after registry shutdown began")
	}
	finish()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after the held lifecycle operation settled")
	}
}

func TestRegistry_HealthCheck_Empty(t *testing.T) {
	r := NewRegistry()

	statuses := r.HealthCheck(context.Background())
	if len(statuses) != 0 {
		t.Errorf("HealthCheck() returned %d statuses, want 0", len(statuses))
	}
}

func TestRegistry_HealthCheck_TracksFailedServers(t *testing.T) {
	r := NewRegistry()

	// Simulate a failed server by directly adding to failedServers
	r.mu.Lock()
	r.failedServers = append(r.failedServers, FailedServer{
		Name:   "failed-server",
		Reason: "connection refused",
	})
	r.mu.Unlock()

	statuses := r.HealthCheck(context.Background())
	if len(statuses) != 1 {
		t.Fatalf("HealthCheck() returned %d statuses, want 1", len(statuses))
	}

	status := statuses[0]
	if status.Name != "failed-server" {
		t.Errorf("status.Name = %q, want 'failed-server'", status.Name)
	}
	if status.Connected {
		t.Error("status.Connected = true, want false")
	}
	if status.LastError != "connection refused" {
		t.Errorf("status.LastError = %q, want 'connection refused'", status.LastError)
	}
}

func TestRegistryFailedServersReturnsSnapshot(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	r.setFailedServer("down", "original")

	first := r.FailedServers()
	if len(first) != 1 {
		t.Fatalf("failed server snapshot = %#v", first)
	}
	first[0].Reason = "caller mutation"
	second := r.FailedServers()
	if len(second) != 1 || second[0].Reason != "original" {
		t.Fatalf("caller mutated registry backing state: %#v", second)
	}
}

func TestRegistry_RegisterConnectedServer_ReplacesExistingState(t *testing.T) {
	r := NewRegistry()

	first := &MCPClient{name: "demo"}
	second := &MCPClient{name: "demo"}
	firstDefs := []llm.ToolDef{{Name: "tool-a"}, {Name: "tool-b"}}
	secondDefs := []llm.ToolDef{{Name: "tool-a"}}

	r.mu.Lock()
	r.failedServers = append(r.failedServers, FailedServer{Name: "demo", Reason: "old error"})
	r.registerConnectedServerLocked("demo", first, firstDefs)
	r.registerConnectedServerLocked("demo", second, secondDefs)
	r.mu.Unlock()

	if r.ServerCount() != 1 {
		t.Fatalf("ServerCount() = %d, want 1", r.ServerCount())
	}
	if r.ToolCount() != 1 {
		t.Fatalf("ToolCount() = %d, want 1", r.ToolCount())
	}
	tools := r.Tools()
	if len(tools) != 1 || tools[0].Name != "demo__tool-a" {
		t.Fatalf("Tools() = %#v, want only demo__tool-a", tools)
	}
	if route := r.toolMap["demo__tool-a"]; route.client != second || route.remoteName != "tool-a" {
		t.Fatalf("demo__tool-a route = %#v, want client %p and remote tool-a", route, second)
	}
	if _, ok := r.toolMap["demo__tool-b"]; ok {
		t.Fatal("tool-b should be removed when the server is replaced")
	}
	if len(r.FailedServers()) != 0 {
		t.Fatalf("failed server entry should be cleared on successful registration, got %#v", r.FailedServers())
	}
}

func TestRegistryServerInstructionsAreSortedBoundedAndReplaced(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	longUnicode := strings.Repeat("é", maxServerInstructionBytes)
	r.mu.Lock()
	r.registerConnectedServerLocked("zeta", &MCPClient{name: "zeta", instructions: "  use zeta_search  "}, nil)
	r.registerConnectedServerLocked("alpha", &MCPClient{name: "alpha", instructions: longUnicode}, nil)
	r.mu.Unlock()

	got := r.ServerInstructions()
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("ServerInstructions() = %#v, want alpha then zeta", got)
	}
	if len(got[0].Text) > maxServerInstructionBytes || !utf8.ValidString(got[0].Text) || !strings.Contains(got[0].Text, "guidance truncated") {
		t.Fatalf("unicode guidance was not bounded safely: bytes=%d tail=%q", len(got[0].Text), got[0].Text[len(got[0].Text)-min(80, len(got[0].Text)):])
	}
	if got[1].Text != "use zeta_search" {
		t.Fatalf("trimmed zeta guidance = %q", got[1].Text)
	}

	got[1].Text = "caller mutation"
	if again := r.ServerInstructions(); len(again) != 2 || again[1].Text != "use zeta_search" {
		t.Fatalf("caller mutated registry guidance: %#v", again)
	}

	r.mu.Lock()
	r.registerConnectedServerLocked("zeta", &MCPClient{name: "zeta", instructions: "replacement"}, nil)
	r.removeServerLocked("alpha")
	r.mu.Unlock()
	if replaced := r.ServerInstructions(); len(replaced) != 1 || replaced[0] != (ServerInstruction{Name: "zeta", Text: "replacement"}) {
		t.Fatalf("replaced guidance = %#v", replaced)
	}
}

func TestMCPClientInstructionsNilSafe(t *testing.T) {
	var client *MCPClient
	if got := client.Instructions(); got != "" {
		t.Fatalf("nil client instructions = %q", got)
	}
}

func TestRegistryServerInstructionsKeepAggregateEntriesAtomic(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	r.mu.Lock()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		r.registerConnectedServerLocked(name, &MCPClient{
			name:         name,
			instructions: strings.Repeat(name, maxServerInstructionBytes+1),
		}, nil)
	}
	r.mu.Unlock()

	got := r.ServerInstructions()
	total := 0
	for _, instruction := range got {
		total += len(instruction.Text)
		if !strings.HasSuffix(instruction.Text, serverInstructionTruncatedMarker) {
			t.Fatalf("partially projected guidance for %s: %q", instruction.Name, instruction.Text[len(instruction.Text)-min(80, len(instruction.Text)):])
		}
	}
	if total > maxAllServerInstructionBytes {
		t.Fatalf("aggregate guidance bytes = %d", total)
	}
	if len(got) != maxAllServerInstructionBytes/maxServerInstructionBytes {
		t.Fatalf("atomic bounded entries = %d, want %d", len(got), maxAllServerInstructionBytes/maxServerInstructionBytes)
	}
}

func TestRegistryNamespacesDuplicateToolNamesByServer(t *testing.T) {
	r := NewRegistry()
	first := &MCPClient{name: "first"}
	second := &MCPClient{name: "second"}

	r.mu.Lock()
	r.registerConnectedServerLocked("first", first, []llm.ToolDef{{Name: "search"}})
	r.registerConnectedServerLocked("second", second, []llm.ToolDef{{Name: "search"}})
	r.mu.Unlock()

	firstRoute, firstOK := r.toolMap["first__search"]
	secondRoute, secondOK := r.toolMap["second__search"]
	if !firstOK || !secondOK {
		t.Fatalf("namespaced routes missing: %#v", r.toolMap)
	}
	if firstRoute.client != first || secondRoute.client != second {
		t.Fatalf("routes point at wrong servers: %#v", r.toolMap)
	}
	if firstRoute.remoteName != "search" || secondRoute.remoteName != "search" {
		t.Fatalf("remote tool names changed: %#v", r.toolMap)
	}
	if _, ok := r.ResolveToolName("search"); ok {
		t.Fatal("ambiguous remote tool resolved to an arbitrary server")
	}
	if got, ok := r.ResolveToolName("first__search"); !ok || got != "first__search" {
		t.Fatalf("exact exposed tool resolution = %q, %v", got, ok)
	}
}

func TestRegistryToolSnapshotIsCoherentDetachedAndVersioned(t *testing.T) {
	r := NewRegistry()
	r.mu.Lock()
	r.registerConnectedServerLocked("demo", &MCPClient{name: "demo"}, []llm.ToolDef{{
		Name: "inspect",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"workspace": map[string]any{"type": "string"}},
			"required":   []any{"workspace"},
		},
	}})
	r.mu.Unlock()

	first := r.SnapshotTools()
	if first.Epoch <= 1 || len(first.Tools) != 1 || first.Tools[0].Name != "demo__inspect" {
		t.Fatalf("first snapshot = %#v", first)
	}
	first.Tools[0].Parameters["type"] = "array"
	first.Tools[0].Parameters["properties"].(map[string]any)["workspace"].(map[string]any)["type"] = "number"
	second := r.SnapshotTools()
	if second.Epoch != first.Epoch || second.Tools[0].Parameters["type"] != "object" ||
		second.Tools[0].Parameters["properties"].(map[string]any)["workspace"].(map[string]any)["type"] != "string" {
		t.Fatalf("caller mutated registry snapshot: %#v", second)
	}

	r.setFailedServer("demo", "connection lost")
	failed := r.SnapshotTools()
	if failed.Epoch <= second.Epoch || len(failed.Tools) != 1 || failed.ServerAvailable("demo") {
		t.Fatalf("failure did not advance epoch: before=%d after=%d", second.Epoch, failed.Epoch)
	}
	if visible := failed.ModelVisibleTools(); len(visible) != 0 {
		t.Fatalf("failed server still advertised to the model: %#v", visible)
	}
	r.mu.Lock()
	r.registerConnectedServerLocked("demo", &MCPClient{name: "demo"}, []llm.ToolDef{{Name: "inspect"}})
	r.mu.Unlock()
	reconnected := r.SnapshotTools()
	if reconnected.Epoch <= failed.Epoch {
		t.Fatalf("reconnect did not advance epoch: before=%d after=%d", failed.Epoch, reconnected.Epoch)
	}
	if visible := reconnected.ModelVisibleTools(); len(visible) != 1 || visible[0].Name != "demo__inspect" {
		t.Fatalf("reconnect did not restore model-visible tools: %#v", visible)
	}
}

func TestRegistryLateConnectionFailureCannotPoisonNewerSuccess(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	newer := &MCPClient{name: "demo"}
	r.testConnector = func(context.Context, config.ServerConfig) (*MCPClient, []llm.ToolDef, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
			return nil, nil, errors.New("stale connection failure")
		}
		return newer, []llm.ToolDef{{Name: "inspect"}}, nil
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := r.ConnectServer(context.Background(), config.ServerConfig{Name: "demo", Command: "demo"})
		firstResult <- err
	}()
	<-firstStarted
	if count, err := r.ConnectServer(context.Background(), config.ServerConfig{Name: "demo", Command: "demo"}); err != nil || count != 1 {
		t.Fatalf("newer connection = count %d error %v", count, err)
	}
	close(releaseFirst)
	if err := <-firstResult; !errors.Is(err, ErrConnectionSuperseded) || !strings.Contains(err.Error(), "stale connection failure") {
		t.Fatalf("older attempt error = %v", err)
	}
	snapshot := r.SnapshotTools()
	if !snapshot.ServerAvailable("demo") || len(snapshot.Tools) != 1 || len(r.FailedServers()) != 0 || r.clients["demo"] != newer {
		t.Fatalf("late failure poisoned newer connection: snapshot=%#v failed=%#v client=%p", snapshot, r.FailedServers(), r.clients["demo"])
	}
}

func TestRegistryLateConnectionSuccessCannotReplaceNewerSuccess(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	oldClosed := make(chan struct{}, 1)
	var calls atomic.Int32
	older := &MCPClient{name: "demo-old"}
	newer := &MCPClient{name: "demo-new"}
	r.testConnector = func(context.Context, config.ServerConfig) (*MCPClient, []llm.ToolDef, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
			return older, []llm.ToolDef{{Name: "old"}}, nil
		}
		return newer, []llm.ToolDef{{Name: "new"}}, nil
	}
	r.testCloseClient = func(client *MCPClient) error {
		if client == older {
			oldClosed <- struct{}{}
		}
		return nil
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := r.ConnectServer(context.Background(), config.ServerConfig{Name: "demo", Command: "demo"})
		firstResult <- err
	}()
	<-firstStarted
	if count, err := r.ConnectServer(context.Background(), config.ServerConfig{Name: "demo", Command: "demo"}); err != nil || count != 1 {
		t.Fatalf("newer connection = count %d error %v", count, err)
	}
	close(releaseFirst)
	if err := <-firstResult; !errors.Is(err, ErrConnectionSuperseded) {
		t.Fatalf("older attempt error = %v", err)
	}
	select {
	case <-oldClosed:
	default:
		t.Fatal("superseded client was not closed")
	}
	snapshot := r.SnapshotTools()
	if len(snapshot.Tools) != 1 || snapshot.Tools[0].Name != "demo__new" || r.clients["demo"] != newer {
		t.Fatalf("late success replaced newer connection: snapshot=%#v client=%p", snapshot, r.clients["demo"])
	}
}

func TestHealthMonitorStopsRetryingWhenReconnectIsSuperseded(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	olderClosed := make(chan struct{}, 1)
	var calls atomic.Int32
	older := &MCPClient{name: "demo-old"}
	newer := &MCPClient{name: "demo-new"}
	r.testConnector = func(context.Context, config.ServerConfig) (*MCPClient, []llm.ToolDef, error) {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return older, []llm.ToolDef{{Name: "old"}}, nil
		case 2:
			return newer, []llm.ToolDef{{Name: "new"}}, nil
		default:
			return nil, nil, errors.New("stale monitor retried after supersession")
		}
	}
	r.testCloseClient = func(client *MCPClient) error {
		if client == older {
			olderClosed <- struct{}{}
		}
		return nil
	}
	r.mu.Lock()
	r.serverConfigs["demo"] = config.ServerConfig{Name: "demo", Command: "demo"}
	r.setFailedServerLocked("demo", "initial failure")
	r.mu.Unlock()

	roundDone := make(chan struct{})
	go func() {
		r.healthCheckRound(context.Background(), MonitorConfig{
			MaxRetries:  2,
			BackoffBase: time.Millisecond,
		}, func(string) {})
		close(roundDone)
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("health monitor reconnect did not start")
	}
	if count, err := r.ConnectServer(context.Background(), config.ServerConfig{Name: "demo", Command: "demo"}); err != nil || count != 1 {
		t.Fatalf("newer connection = count %d error %v", count, err)
	}
	close(releaseFirst)
	select {
	case <-roundDone:
	case <-time.After(time.Second):
		t.Fatal("health monitor did not stop after supersession")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("connector calls = %d, want stale monitor plus newer owner only", got)
	}
	select {
	case <-olderClosed:
	default:
		t.Fatal("superseded monitor client was not closed")
	}
	snapshot := r.SnapshotTools()
	if !snapshot.ServerAvailable("demo") || len(snapshot.Tools) != 1 ||
		snapshot.Tools[0].Name != "demo__new" || len(r.FailedServers()) != 0 || r.clients["demo"] != newer {
		t.Fatalf("monitor supersession damaged newer connection: snapshot=%#v failed=%#v client=%p", snapshot, r.FailedServers(), r.clients["demo"])
	}
}

func TestRegistryRejectsAmbiguousServerNamespace(t *testing.T) {
	r := NewRegistry()
	_, err := r.ConnectServer(context.Background(), config.ServerConfig{
		Name: "first__second", Command: "unused",
	})
	if err == nil || !strings.Contains(err.Error(), "reserved namespace delimiter") {
		t.Fatalf("ConnectServer error = %v, want namespace rejection", err)
	}
	if r.ServerCount() != 0 || r.ToolCount() != 0 {
		t.Fatalf("invalid server mutated registry: servers=%d tools=%d", r.ServerCount(), r.ToolCount())
	}
}

func TestRegistryCloseCancelsAndJoinsHealthMonitor(t *testing.T) {
	r := NewRegistry()
	r.mu.Lock()
	r.failedServers = append(r.failedServers, FailedServer{Name: "down", Reason: "test"})
	r.mu.Unlock()

	enteredLog := make(chan struct{})
	releaseLog := make(chan struct{})
	var enterOnce sync.Once
	r.StartHealthMonitor(context.Background(), MonitorConfig{
		Interval: time.Millisecond, MaxRetries: 0, BackoffBase: time.Millisecond,
	}, func(string) {
		enterOnce.Do(func() { close(enteredLog) })
		<-releaseLog
	})

	select {
	case <-enteredLog:
	case <-time.After(time.Second):
		t.Fatal("health monitor did not enter its owned goroutine")
	}

	closeDone := make(chan struct{})
	go func() {
		r.Close()
		close(closeDone)
	}()

	// The monitor deliberately ignores cancellation while inside logFn. Close
	// must remain blocked until that owned goroutine is released and joined.
	select {
	case <-closeDone:
		close(releaseLog)
		t.Fatal("Registry.Close returned before the health monitor exited")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseLog)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Registry.Close did not finish after the health monitor exited")
	}

	if r.ServerCount() != 0 || len(r.FailedServers()) != 0 {
		t.Fatalf("closed registry retained state: servers=%d failed=%v", r.ServerCount(), r.FailedServers())
	}
}

func TestHealthMonitorPublishesFailureAndRecoverySnapshots(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	r.mu.Lock()
	r.failedServers = append(r.failedServers, FailedServer{Name: "demo", Reason: "connection refused"})
	r.serverConfigs["demo"] = config.ServerConfig{Name: "demo", Command: "unused"}
	r.mu.Unlock()
	r.testConnector = func(context.Context, config.ServerConfig) (*MCPClient, []llm.ToolDef, error) {
		return &MCPClient{name: "demo"}, []llm.ToolDef{{Name: "inspect"}}, nil
	}

	var snapshots [][]ConnectionStatus
	r.healthCheckRound(context.Background(), MonitorConfig{
		MaxRetries: 1, BackoffBase: time.Nanosecond,
		OnSnapshot: func(statuses []ConnectionStatus) {
			snapshots = append(snapshots, append([]ConnectionStatus(nil), statuses...))
		},
	}, func(string) {})
	if len(snapshots) < 2 {
		t.Fatalf("health monitor snapshots=%d, want failure and recovery", len(snapshots))
	}
	first := snapshots[0]
	last := snapshots[len(snapshots)-1]
	if len(first) != 1 || first[0].Connected || first[0].Name != "demo" {
		t.Fatalf("first snapshot = %#v, want unavailable demo", first)
	}
	if len(last) != 1 || !last[0].Connected || last[0].ToolCount != 1 {
		t.Fatalf("last snapshot = %#v, want recovered demo", last)
	}
	first[0].Name = "caller mutation"
	if current := r.ConnectionStatuses(); len(current) != 1 || current[0].Name != "demo" {
		t.Fatalf("snapshot caller mutated registry state: %#v", current)
	}
}

func TestHealthMonitorReconnectsRetainedFailedClientOnlyOncePerRound(t *testing.T) {
	r := NewRegistry()
	t.Cleanup(r.Close)
	r.mu.Lock()
	r.clients["demo"] = &MCPClient{name: "demo"}
	r.serverTools["demo"] = nil
	r.failedServers = append(r.failedServers, FailedServer{Name: "demo", Reason: "connection refused"})
	r.serverConfigs["demo"] = config.ServerConfig{Name: "demo", Command: "unused"}
	r.rebuildToolMapLocked()
	r.mu.Unlock()
	var connectorCalls atomic.Int32
	r.testConnector = func(context.Context, config.ServerConfig) (*MCPClient, []llm.ToolDef, error) {
		connectorCalls.Add(1)
		return &MCPClient{name: "demo"}, []llm.ToolDef{{Name: "inspect"}}, nil
	}

	r.healthCheckRound(context.Background(), MonitorConfig{
		MaxRetries: 1, BackoffBase: time.Nanosecond,
	}, func(string) {})
	if calls := connectorCalls.Load(); calls != 1 {
		t.Fatalf("reconnect calls = %d, want one per unhealthy server per round", calls)
	}
	statuses := r.ConnectionStatuses()
	if len(statuses) != 1 || !statuses[0].Connected || statuses[0].ToolCount != 1 {
		t.Fatalf("recovered statuses = %#v", statuses)
	}
}

func TestRegistryCloseRejectsLateConnectionRegistration(t *testing.T) {
	r := NewRegistry()
	lateClient := &MCPClient{name: "late"}
	connectStarted := make(chan struct{})
	clientClosed := make(chan struct{})
	var connectorCalls atomic.Int32
	var closeCalls atomic.Int32
	var closeOnce sync.Once
	r.testConnector = func(ctx context.Context, _ config.ServerConfig) (*MCPClient, []llm.ToolDef, error) {
		connectorCalls.Add(1)
		close(connectStarted)
		<-ctx.Done()
		// Simulate a transport that races cancellation and still hands back a
		// successfully initialized client. The registry must reject and close it.
		return lateClient, []llm.ToolDef{{Name: "mutate"}}, nil
	}
	r.testCloseClient = func(client *MCPClient) error {
		if client == lateClient {
			closeCalls.Add(1)
			closeOnce.Do(func() { close(clientClosed) })
		}
		return nil
	}

	type connectResult struct {
		count int
		err   error
	}
	connectDone := make(chan connectResult, 1)
	go func() {
		count, err := r.ConnectServer(context.Background(), config.ServerConfig{Name: "late"})
		connectDone <- connectResult{count: count, err: err}
	}()
	select {
	case <-connectStarted:
	case <-time.After(time.Second):
		t.Fatal("connection did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		r.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not join the in-flight connection")
	}
	result := <-connectDone
	if !errors.Is(result.err, ErrRegistryClosed) || result.count != 0 {
		t.Fatalf("late ConnectServer result = (%d, %v), want ErrRegistryClosed", result.count, result.err)
	}
	select {
	case <-clientClosed:
	default:
		t.Fatal("late client was not closed before Registry.Close returned")
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("late client close calls = %d, want 1", got)
	}
	if r.ServerCount() != 0 || r.ToolCount() != 0 || len(r.Tools()) != 0 {
		t.Fatalf("client appeared after Close: servers=%d tools=%d defs=%v", r.ServerCount(), r.ToolCount(), r.Tools())
	}

	if _, err := r.ConnectServer(context.Background(), config.ServerConfig{Name: "after-close"}); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("ConnectServer after Close error = %v", err)
	}
	if _, err := r.ReconnectServer(context.Background(), "late"); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("ReconnectServer after Close error = %v", err)
	}
	if got := connectorCalls.Load(); got != 1 {
		t.Fatalf("connector ran %d times; post-Close calls must be rejected before launch", got)
	}
}
