package mcp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/charmbracelet/log"
)

// FailedServer records an MCP server that failed to connect.
type FailedServer struct {
	Name   string
	Reason string
}

// ServerStatus represents the health status of an MCP server.
type ServerStatus struct {
	Name      string
	Connected bool
	LastError string
	LastPing  time.Time
}

type toolRoute struct {
	client toolCaller
	// server is the connection this tool dispatches to. The exposed name is
	// usually namespaced with it, but not always — mcphub strips a redundant
	// self-prefix when publishing — so deriving it from the name would be
	// wrong for exactly the gateway that makes a trace most worth reading.
	server     string
	remoteName string
}

type toolCaller interface {
	CallTool(context.Context, string, map[string]any) (*ToolResult, error)
}

var (
	// ErrRegistryClosed reports an operation attempted after shutdown began.
	ErrRegistryClosed = errors.New("MCP registry is closed")
	// ErrRegistryEpochChanged reports that a caller's exact catalog snapshot is
	// stale. CallToolAtEpoch checks and acquires the route under one registry
	// lock, preventing a reconnect from redirecting an already validated call.
	ErrRegistryEpochChanged = errors.New("MCP registry catalog changed")
	// ErrConnectionSuperseded reports that a newer connection attempt now owns
	// the server generation. Callers must not retry the stale attempt unchanged.
	ErrConnectionSuperseded = errors.New("MCP connection attempt superseded")
)

// Registry manages multiple MCP server connections and routes tool calls.
type Registry struct {
	mu               sync.RWMutex
	logger           *log.Logger
	clients          map[string]*MCPClient
	toolMap          map[string]toolRoute // exposed tool name -> server and remote name
	serverTools      map[string][]llm.ToolDef
	serverGuidance   map[string]string
	failedServers    []FailedServer
	serverConfigs    map[string]config.ServerConfig // name -> config for reconnection
	callTimeout      time.Duration                  // per tool-call timeout (0 = default)
	version          string                         // advertised MCP client implementation version
	epoch            uint64                         // increments on every connection/catalog state transition
	connectAttempt   map[string]uint64              // latest admitted connection generation per server
	toolRefreshRun   map[string]bool                // server -> list_changed refresh worker owns it
	toolRefreshDirty map[string]bool                // server -> another notification arrived while refreshing
	localOnly        bool                           // enforce per-request local HTTP authority
	closed           bool
	lifecycleCtx     context.Context
	cancel           context.CancelFunc
	lifecycleWG      sync.WaitGroup
	closeOnce        sync.Once

	// Test seams keep shutdown overlap tests deterministic without launching a
	// real child process. Production always uses discoverServer/client.Close.
	// A test connector must honor ctx cancellation, matching that production
	// contract; the real STDIO lifecycle regression proves the production path.
	testConnector   func(context.Context, config.ServerConfig) (*MCPClient, []llm.ToolDef, error)
	testCloseClient func(*MCPClient) error
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return NewRegistryWithVersion(developmentImplementationVersion)
}

// RegistryOption configures a Registry without expanding each constructor as
// new host-owned execution policy is introduced.
type RegistryOption func(*Registry)

// WithLocalOnly enforces local-machine, same-origin authority on every MCP
// SSE and Streamable HTTP request made by the registry.
func WithLocalOnly(required bool) RegistryOption {
	return func(registry *Registry) {
		registry.localOnly = required
	}
}

// NewRegistryWithVersion creates a registry whose MCP client handshakes
// advertise the same build version as the sonar CLI.
func NewRegistryWithVersion(version string, options ...RegistryOption) *Registry {
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	registry := &Registry{
		toolMap:          make(map[string]toolRoute),
		clients:          make(map[string]*MCPClient),
		serverTools:      make(map[string][]llm.ToolDef),
		serverGuidance:   make(map[string]string),
		serverConfigs:    make(map[string]config.ServerConfig),
		callTimeout:      defaultCallTimeout,
		version:          clientImplementation(version).Version,
		epoch:            1,
		connectAttempt:   make(map[string]uint64),
		toolRefreshRun:   make(map[string]bool),
		toolRefreshDirty: make(map[string]bool),
		lifecycleCtx:     lifecycleCtx,
		cancel:           cancel,
	}
	for _, option := range options {
		if option != nil {
			option(registry)
		}
	}
	return registry
}

// SetCallTimeout overrides the per tool-call timeout. A non-positive value
// resets it to the default.
func (r *Registry) SetCallTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d <= 0 {
		d = defaultCallTimeout
	}
	r.callTimeout = d
}

// connectTimeout is the per-server connection timeout.
const connectTimeout = 15 * time.Second

// defaultCallTimeout bounds a single MCP tool call so a hung or slow server
// cannot block the agent loop (and freeze the UI) indefinitely.
const defaultCallTimeout = 60 * time.Second

// beginLifecycleOperation prevents WaitGroup Add/Wait races by admitting new
// connections and tool calls under the same mutex that Close uses to mark the
// registry closed.
func (r *Registry) beginLifecycleOperation(ctx context.Context) (context.Context, func(), error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, ErrRegistryClosed
	}
	opCtx, cancel := context.WithCancel(ctx)
	stopLifecycleCancel := context.AfterFunc(r.lifecycleCtx, cancel)
	r.lifecycleWG.Add(1)
	r.mu.Unlock()

	finish := func() {
		stopLifecycleCancel()
		cancel()
		r.lifecycleWG.Done()
	}
	return opCtx, finish, nil
}

func (r *Registry) closeMCPClient(client *MCPClient) error {
	if client == nil {
		return nil
	}
	if r.testCloseClient != nil {
		return r.testCloseClient(client)
	}
	return client.Close()
}

// discoverServer is the only production connector. Every blocking SDK call is
// given ctx. STDIO Connect also links ctx to an owned process-group cancel, so
// Registry.Close's lifecycleWG wait cannot be stranded by a hanging child.
func (r *Registry) discoverServer(ctx context.Context, srv config.ServerConfig) (*MCPClient, []llm.ToolDef, error) {
	initialCatalogListed := make(chan struct{})
	client, err := connectWithVersionTrustAndToolChanges(
		ctx, r.version, srv.Name, srv.Command, srv.Args, srv.Env, srv.Transport, srv.URL,
		r.localOnly, srv.ExecutableSHA256,
		func() {
			// AddTool calls made before/during initialize can produce a queued
			// notification even though the first ListTools already observes them.
			// Only changes after that authoritative list require another round trip.
			select {
			case <-initialCatalogListed:
				r.scheduleToolRefresh(srv.Name)
			default:
			}
		},
	)
	if err != nil {
		return nil, nil, err
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		if closeErr := r.closeMCPClient(client); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close failed MCP connection: %w", closeErr))
		}
		return nil, nil, fmt.Errorf("%s tools: %w", srv.Name, err)
	}
	close(initialCatalogListed)

	serverDefs := make([]llm.ToolDef, 0, len(tools))
	for _, tool := range tools {
		serverDefs = append(serverDefs, ToLLMToolDefFromMCP(tool))
	}
	return client, serverDefs, nil
}

// ConnectServer connects a single MCP server and registers its tools.
// Returns the number of tools discovered, or an error.
func (r *Registry) ConnectServer(ctx context.Context, srv config.ServerConfig) (int, error) {
	opCtx, finish, err := r.beginLifecycleOperation(ctx)
	if err != nil {
		return 0, err
	}
	defer finish()

	if strings.Contains(srv.Name, "__") {
		return 0, fmt.Errorf("server %q contains reserved namespace delimiter __", srv.Name)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, ErrRegistryClosed
	}
	r.serverConfigs[srv.Name] = srv
	r.connectAttempt[srv.Name]++
	attempt := r.connectAttempt[srv.Name]
	r.mu.Unlock()

	connCtx, cancel := context.WithTimeout(opCtx, connectTimeout)
	defer cancel()

	connector := r.testConnector
	productionConnector := connector == nil
	if productionConnector {
		connector = r.discoverServer
	}
	client, serverDefs, err := connector(connCtx, srv)
	if err != nil {
		r.mu.Lock()
		superseded := r.connectAttempt[srv.Name] != attempt
		if !r.closed && !superseded {
			r.setFailedServerLocked(srv.Name, err.Error())
		}
		r.mu.Unlock()
		if superseded {
			return 0, fmt.Errorf(
				"connect to %s failed after a newer attempt took ownership: %w",
				srv.Name, errors.Join(ErrConnectionSuperseded, err),
			)
		}
		return 0, fmt.Errorf("connect to %s: %w", srv.Name, err)
	}
	if ctxErr := connCtx.Err(); ctxErr != nil {
		r.mu.RLock()
		registryClosed := r.closed
		r.mu.RUnlock()
		if registryClosed {
			ctxErr = ErrRegistryClosed
		}
		closeErr := r.closeMCPClient(client)
		if closeErr != nil {
			ctxErr = errors.Join(ctxErr, fmt.Errorf("close cancelled MCP connection: %w", closeErr))
		}
		return 0, fmt.Errorf("connect to %s cancelled before registration: %w", srv.Name, ctxErr)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		closeErr := r.closeMCPClient(client)
		if closeErr != nil {
			return 0, errors.Join(ErrRegistryClosed, fmt.Errorf("close late MCP connection: %w", closeErr))
		}
		return 0, ErrRegistryClosed
	}
	if r.connectAttempt[srv.Name] != attempt {
		r.mu.Unlock()
		closeErr := r.closeMCPClient(client)
		err := fmt.Errorf("connect to %s: %w", srv.Name, ErrConnectionSuperseded)
		if closeErr != nil {
			return 0, errors.Join(err, fmt.Errorf("close superseded MCP connection: %w", closeErr))
		}
		return 0, err
	}
	existing := r.clients[srv.Name]
	if existing != nil {
		r.removeServerLocked(srv.Name)
	}
	r.registerConnectedServerLocked(srv.Name, client, serverDefs)
	refreshAfterRegister := productionConnector && r.toolRefreshDirty[srv.Name]
	r.mu.Unlock()
	if refreshAfterRegister {
		// Close the narrow race where list_changed arrives after the initial
		// ListTools but before this client becomes registry-visible.
		r.scheduleToolRefresh(srv.Name)
	}
	if existing != nil {
		_ = r.closeMCPClient(existing)
	}

	return len(serverDefs), nil
}

// scheduleToolRefresh coalesces server-driven tools/list_changed
// notifications. The worker participates in Registry lifecycle ownership, so
// Close cancels and joins it before tearing down the client it is listing.
func (r *Registry) scheduleToolRefresh(name string) {
	opCtx, finish, err := r.beginLifecycleOperation(r.lifecycleCtx)
	if err != nil {
		return
	}

	r.mu.Lock()
	if r.clients[name] == nil {
		// A notification can arrive after discoverServer's initial ListTools but
		// before ConnectServer publishes the client. Preserve that edge; the
		// registrar schedules exactly one refresh after the atomic install.
		r.toolRefreshDirty[name] = true
		r.mu.Unlock()
		finish()
		return
	}
	if r.toolRefreshRun[name] {
		r.toolRefreshDirty[name] = true
		r.mu.Unlock()
		finish()
		return
	}
	r.toolRefreshRun[name] = true
	r.toolRefreshDirty[name] = false
	r.mu.Unlock()

	go func() {
		defer finish()
		for {
			refreshCtx, cancel := context.WithTimeout(opCtx, connectTimeout)
			r.refreshServerTools(refreshCtx, name)
			cancel()

			r.mu.Lock()
			if r.toolRefreshDirty[name] && !r.closed {
				r.toolRefreshDirty[name] = false
				r.mu.Unlock()
				continue
			}
			delete(r.toolRefreshRun, name)
			delete(r.toolRefreshDirty, name)
			r.mu.Unlock()
			return
		}
	}()
}

func (r *Registry) refreshServerTools(ctx context.Context, name string) {
	r.mu.RLock()
	client := r.clients[name]
	r.mu.RUnlock()
	if client == nil {
		return
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		return
	}
	defs := make([]llm.ToolDef, 0, len(tools))
	for _, tool := range tools {
		defs = append(defs, ToLLMToolDefFromMCP(tool))
	}
	for i := range defs {
		defs[i].Name = namespacedToolName(name, defs[i].Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.clients[name] != client {
		return
	}
	if reflect.DeepEqual(r.serverTools[name], defs) {
		return
	}
	r.serverTools[name] = defs
	r.rebuildToolMapLocked()
	r.epoch++
}

// ConnectAll spawns and connects to all configured MCP servers.
// Servers that fail to connect are logged but don't prevent others.
func (r *Registry) ConnectAll(ctx context.Context, servers []config.ServerConfig, logFn func(string)) {
	for _, srv := range servers {
		toolCount, err := r.ConnectServer(ctx, srv)
		if err != nil {
			logFn(fmt.Sprintf("skip %s: %v", srv.Name, err))
			continue
		}
		logFn(fmt.Sprintf("connected %s (%d tools)", srv.Name, toolCount))
	}
}

// Tools returns all discovered tool definitions.
func (r *Registry) Tools() []llm.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var serverNames []string
	for name := range r.serverTools {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)

	var toolDefs []llm.ToolDef
	for _, name := range serverNames {
		toolDefs = append(toolDefs, r.serverTools[name]...)
	}
	return toolDefs
}

// ToolSnapshot is one coherent, immutable view of the connected MCP catalog.
// Epoch changes whenever connection or advertised-tool state changes, allowing
// consumers to invalidate ephemeral schema-derived suggestions safely.
type ToolSnapshot struct {
	Epoch              uint64
	Tools              []llm.ToolDef
	AvailableServers   []string
	UnavailableServers []string
}

// ServerAvailable reports whether a server namespace was connected in this
// exact snapshot. Definitions remain retained for truthful UI/catalog metrics,
// but unavailable routes must not authorize a continuation dispatch.
func (snapshot ToolSnapshot) ServerAvailable(name string) bool {
	index := sort.SearchStrings(snapshot.AvailableServers, name)
	return index < len(snapshot.AvailableServers) && snapshot.AvailableServers[index] == name
}

// SnapshotTools returns the current exposed definitions and epoch under one
// registry lock. Tool parameter maps are detached so callers cannot mutate the
// registry-owned catalog.
func (r *Registry) SnapshotTools() ToolSnapshot {
	if r == nil {
		return ToolSnapshot{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	failed := make(map[string]struct{}, len(r.failedServers))
	for _, receipt := range r.failedServers {
		failed[receipt.Name] = struct{}{}
	}
	serverNames := make([]string, 0, len(r.serverTools))
	for name := range r.serverTools {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)
	snapshot := ToolSnapshot{
		Epoch: r.epoch, AvailableServers: make([]string, 0, len(serverNames)),
		UnavailableServers: make([]string, 0, len(failed)),
	}
	for name := range failed {
		snapshot.UnavailableServers = append(snapshot.UnavailableServers, name)
	}
	for _, name := range serverNames {
		if _, unavailable := failed[name]; !unavailable {
			snapshot.AvailableServers = append(snapshot.AvailableServers, name)
		}
	}
	sort.Strings(snapshot.UnavailableServers)
	for _, name := range serverNames {
		for _, definition := range r.serverTools[name] {
			snapshot.Tools = append(snapshot.Tools, cloneToolDefinition(definition))
		}
	}
	return snapshot
}

// ToolCount returns the total number of registered tools.
func (r *Registry) ToolCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, defs := range r.serverTools {
		count += len(defs)
	}
	return count
}

// ResolveToolName returns a unique exposed name for a remote MCP tool. It is
// intended for host-side integrations that know a capability by its protocol
// name while the model-facing registry remains fully namespaced.
func (r *Registry) ResolveToolName(remoteName string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, exact := r.toolMap[remoteName]; exact {
		return remoteName, true
	}
	suffix := "__" + remoteName
	match := ""
	for exposed, route := range r.toolMap {
		if route.remoteName != remoteName && !strings.HasSuffix(exposed, suffix) {
			continue
		}
		if match != "" {
			return "", false
		}
		match = exposed
	}
	return match, match != ""
}

// ServerCount returns the number of connected servers.
func (r *Registry) ServerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

// ServerNames returns the names of all connected servers.
func (r *Registry) ServerNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.clients))
	for name := range r.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

const maxAllServerInstructionBytes = 16 * 1024

// ServerInstructions returns a deterministic, bounded snapshot of usage
// guidance supplied by connected MCP servers during initialization. Guidance
// remains server-authored data; consumers must not treat it as host policy.
func (r *Registry) ServerInstructions() []ServerInstruction {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.serverGuidance))
	for name, guidance := range r.serverGuidance {
		if strings.TrimSpace(guidance) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	remaining := maxAllServerInstructionBytes
	instructions := make([]ServerInstruction, 0, len(names))
	for _, name := range names {
		if remaining <= 0 {
			break
		}
		guidance := boundServerInstruction(r.serverGuidance[name], maxServerInstructionBytes)
		if guidance == "" {
			continue
		}
		// Keep each server's guidance atomic. A silently cut calling convention
		// is less useful than omitting that entry from this bounded snapshot.
		if len(guidance) > remaining {
			continue
		}
		instructions = append(instructions, ServerInstruction{Name: name, Text: guidance})
		remaining -= len(guidance)
	}
	return instructions
}

// FailedServers returns the list of servers that failed to connect.
func (r *Registry) FailedServers() []FailedServer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	failed := make([]FailedServer, len(r.failedServers))
	copy(failed, r.failedServers)
	return failed
}

// CallTool routes a tool call to the correct MCP server.
func (r *Registry) CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error) {
	return r.callTool(ctx, 0, false, name, args)
}

// CallToolAtEpoch routes a tool only when the exact validated catalog remains
// current. Lifecycle admission, epoch validation, and route acquisition happen
// before dispatch; shutdown or a later reconnect cannot redirect the call to a
// different client, schema, or remote tool.
func (r *Registry) CallToolAtEpoch(ctx context.Context, expectedEpoch uint64, name string, args map[string]any) (*ToolResult, error) {
	if expectedEpoch == 0 {
		return nil, fmt.Errorf("%w: expected epoch must be non-zero", ErrRegistryEpochChanged)
	}
	return r.callTool(ctx, expectedEpoch, true, name, args)
}

func (r *Registry) callTool(ctx context.Context, expectedEpoch uint64, requireEpoch bool, name string, args map[string]any) (*ToolResult, error) {
	opCtx, finish, err := r.beginLifecycleOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()

	r.mu.RLock()
	if requireEpoch && r.epoch != expectedEpoch {
		actualEpoch := r.epoch
		r.mu.RUnlock()
		return nil, fmt.Errorf("%w: expected epoch %d, current epoch %d", ErrRegistryEpochChanged, expectedEpoch, actualEpoch)
	}
	route, ok := r.toolMap[name]
	timeout := r.callTimeout
	r.mu.RUnlock()

	if !ok {
		return &ToolResult{
			Content: fmt.Sprintf("unknown tool: %s", name),
			IsError: true,
		}, nil
	}

	if timeout <= 0 {
		timeout = defaultCallTimeout
	}

	callCtx, cancel := context.WithTimeout(opCtx, timeout)
	defer cancel()

	started := time.Now()
	result, err := route.client.CallTool(callCtx, route.remoteName, args)
	traceCallOutcome(
		r.traceLogger(), callCorrelation(ctx),
		name, route.server, route.remoteName, time.Since(started), result, err,
	)
	if err != nil && callCtx.Err() != nil {
		// Preserve the local deadline/cancellation in the error chain. Callers
		// must treat a dispatched MCP mutation as outcome-unknown, not retry it
		// as though it definitely never ran.
		return nil, fmt.Errorf("MCP tool %s ended without a receipt: %w", name, callCtx.Err())
	}
	return result, err
}

// Close shuts down all MCP server connections.
func (r *Registry) Close() {
	r.closeOnce.Do(func() {
		// Closing admission and cancelling the lifecycle happen under one lock,
		// so no goroutine can Add to lifecycleWG after Wait begins.
		r.mu.Lock()
		r.closed = true
		r.cancel()
		r.mu.Unlock()

		// Includes every health monitor, connection attempt, and in-flight tool
		// call. Lifecycle cancellation bounds each join.
		r.lifecycleWG.Wait()

		r.mu.Lock()
		clients := make([]*MCPClient, 0, len(r.clients))
		for _, client := range r.clients {
			clients = append(clients, client)
		}
		r.clients = make(map[string]*MCPClient)
		r.toolMap = make(map[string]toolRoute)
		r.serverTools = make(map[string][]llm.ToolDef)
		r.serverGuidance = make(map[string]string)
		r.failedServers = nil
		r.serverConfigs = make(map[string]config.ServerConfig)
		r.connectAttempt = make(map[string]uint64)
		r.toolRefreshRun = make(map[string]bool)
		r.toolRefreshDirty = make(map[string]bool)
		r.epoch++
		r.mu.Unlock()

		for _, client := range clients {
			_ = r.closeMCPClient(client)
		}
	})
}

// HealthCheck pings all servers and returns their status.
//
// Connections are snapshotted under the lock, then pinged with the lock
// released: Ping performs blocking I/O, so holding the read lock across it
// would stall every writer (e.g. ConnectServer) and risk a re-entrant RLock
// deadlock when a writer is queued.
func (r *Registry) HealthCheck(ctx context.Context) []ServerStatus {
	type entry struct {
		name   string
		client *MCPClient
	}

	r.mu.RLock()
	names := make([]string, 0, len(r.clients))
	for name := range r.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	snapshot := make([]entry, 0, len(names))
	for _, name := range names {
		snapshot = append(snapshot, entry{name: name, client: r.clients[name]})
	}
	failed := make([]FailedServer, len(r.failedServers))
	copy(failed, r.failedServers)
	r.mu.RUnlock()

	var results []ServerStatus
	for _, e := range snapshot {
		status := ServerStatus{Name: e.name}

		if e.client.IsConnected() {
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := e.client.Ping(pingCtx)
			cancel()

			status.Connected = err == nil
			if err != nil {
				status.LastError = err.Error()
			}
			status.LastPing = time.Now()
		}

		results = append(results, status)
	}

	// Include failed servers
	for _, f := range failed {
		results = append(results, ServerStatus{
			Name:      f.Name,
			Connected: false,
			LastError: f.Reason,
		})
	}

	return results
}

// ReconnectServer attempts to reconnect to a previously failed server.
// Returns the number of tools reconnected, or an error.
func (r *Registry) ReconnectServer(ctx context.Context, name string) (int, error) {
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return 0, ErrRegistryClosed
	}
	srv, ok := r.serverConfigs[name]
	r.mu.RUnlock()

	if !ok {
		return 0, fmt.Errorf("no config found for server: %s", name)
	}
	return r.ConnectServer(ctx, srv)
}

func (r *Registry) setFailedServer(name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.setFailedServerLocked(name, reason)
}

func (r *Registry) setFailedServerLocked(name, reason string) {
	for i := range r.failedServers {
		if r.failedServers[i].Name == name {
			if r.failedServers[i].Reason != reason {
				r.epoch++
			}
			r.failedServers[i].Reason = reason
			return
		}
	}
	r.failedServers = append(r.failedServers, FailedServer{Name: name, Reason: reason})
	r.epoch++
}

func (r *Registry) clearFailedServerLocked(name string) {
	var remaining []FailedServer
	removed := false
	for _, failed := range r.failedServers {
		if failed.Name != name {
			remaining = append(remaining, failed)
		} else {
			removed = true
		}
	}
	r.failedServers = remaining
	if removed {
		r.epoch++
	}
}

func (r *Registry) removeServerLocked(name string) {
	_, hadClient := r.clients[name]
	_, hadTools := r.serverTools[name]
	delete(r.clients, name)
	delete(r.serverTools, name)
	delete(r.serverGuidance, name)
	r.rebuildToolMapLocked()
	if hadClient || hadTools {
		r.epoch++
	}
}

func (r *Registry) registerConnectedServerLocked(name string, client *MCPClient, defs []llm.ToolDef) bool {
	if r.closed {
		return false
	}
	for i := range defs {
		defs[i].Name = namespacedToolName(name, defs[i].Name)
	}
	r.clients[name] = client
	r.serverTools[name] = defs
	if guidance := boundServerInstruction(client.Instructions(), maxServerInstructionBytes); guidance != "" {
		r.serverGuidance[name] = guidance
	} else {
		delete(r.serverGuidance, name)
	}
	r.clearFailedServerLocked(name)
	r.rebuildToolMapLocked()
	r.epoch++
	return true
}

func (r *Registry) rebuildToolMapLocked() {
	toolMap := make(map[string]toolRoute)
	serverNames := make([]string, 0, len(r.clients))
	for name := range r.clients {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)

	for _, name := range serverNames {
		client := r.clients[name]
		for _, def := range r.serverTools[name] {
			remoteName := strings.TrimPrefix(def.Name, name+"__")
			toolMap[def.Name] = toolRoute{client: client, server: name, remoteName: remoteName}
		}
	}
	r.toolMap = toolMap
}

func namespacedToolName(server, tool string) string {
	return server + "__" + tool
}

func cloneToolDefinition(definition llm.ToolDef) llm.ToolDef {
	copy := definition
	copy.Parameters = cloneJSONMap(definition.Parameters)
	return copy
}

func cloneJSONMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	copy := make(map[string]any, len(value))
	for key, item := range value {
		copy[key] = cloneJSONValue(item)
	}
	return copy
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONMap(typed)
	case []any:
		items := make([]any, len(typed))
		for index, child := range typed {
			items[index] = cloneJSONValue(child)
		}
		return items
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

// MonitorConfig holds configuration for the health monitor.
type MonitorConfig struct {
	Interval    time.Duration
	MaxRetries  int
	BackoffBase time.Duration
	// OnSnapshot receives immutable connection-state snapshots after monitor
	// transitions. Callers must return promptly; Registry.Close joins the
	// monitor and therefore also waits for an active callback to return.
	OnSnapshot func([]ConnectionStatus)
}

var defaultMonitorConfig = MonitorConfig{
	Interval:    30 * time.Second,
	MaxRetries:  3,
	BackoffBase: 5 * time.Second,
}

// StartHealthMonitor begins registry-owned background health checking. The
// returned function cancels this monitor; Registry.Close cancels and joins it.
func (r *Registry) StartHealthMonitor(ctx context.Context, cfg MonitorConfig, logFn func(string)) context.CancelFunc {
	if cfg.Interval <= 0 {
		onSnapshot := cfg.OnSnapshot
		cfg = defaultMonitorConfig
		cfg.OnSnapshot = onSnapshot
	}
	if logFn == nil {
		logFn = func(string) {}
	}

	monitorCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return cancel
	}
	stopLifecycleCancel := context.AfterFunc(r.lifecycleCtx, cancel)
	r.lifecycleWG.Add(1)
	r.mu.Unlock()
	r.emitConnectionSnapshot(cfg.OnSnapshot)

	go func() {
		defer r.lifecycleWG.Done()
		defer stopLifecycleCancel()
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
				r.healthCheckRound(monitorCtx, cfg, logFn)
			}
		}
	}()

	return cancel
}

func (r *Registry) healthCheckRound(ctx context.Context, cfg MonitorConfig, logFn func(string)) {
	statuses := r.HealthCheck(ctx)
	handledUnhealthy := make(map[string]struct{}, len(statuses))

	for _, status := range statuses {
		if status.Connected {
			continue
		}
		// HealthCheck can contain both the retained client row and the failure
		// receipt for the same server. One round owns at most one reconnect per
		// server; otherwise a successful first reconnect is immediately replaced
		// by a redundant second connection from the stale duplicate row.
		if _, handled := handledUnhealthy[status.Name]; handled {
			continue
		}
		handledUnhealthy[status.Name] = struct{}{}
		// Publish the failed ping before waiting through reconnect backoff. The
		// retained client may still exist, so the explicit failure receipt must
		// win in ConnectionStatuses until a successful reconnect clears it.
		r.setFailedServer(status.Name, status.LastError)
		r.emitConnectionSnapshot(cfg.OnSnapshot)

		// Server is down, try to reconnect
		logFn(fmt.Sprintf("server %s unhealthy, attempting reconnect...", status.Name))

		for attempt := 1; attempt <= cfg.MaxRetries; attempt++ {
			backoff := cfg.BackoffBase * time.Duration(attempt)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			_, err := r.ReconnectServer(ctx, status.Name)
			if err == nil {
				logFn(fmt.Sprintf("server %s reconnected", status.Name))
				r.emitConnectionSnapshot(cfg.OnSnapshot)
				break
			}
			r.emitConnectionSnapshot(cfg.OnSnapshot)
			if errors.Is(err, ErrConnectionSuperseded) {
				// A newer attempt owns recovery now. Retrying from this stale loop
				// would steal ownership back and could replace or poison the newer
				// connection, so leave any further recovery to that attempt (or the
				// next monitor round if it ultimately fails).
				logFn(fmt.Sprintf("server %s reconnect superseded by newer attempt", status.Name))
				break
			}

			if attempt == cfg.MaxRetries {
				logFn(fmt.Sprintf("server %s reconnection failed after %d attempts: %v", status.Name, cfg.MaxRetries, err))
			}
		}
	}
}

func (r *Registry) emitConnectionSnapshot(callback func([]ConnectionStatus)) {
	if callback == nil {
		return
	}
	callback(r.ConnectionStatuses())
}
