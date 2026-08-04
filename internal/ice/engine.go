package ice

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

// EngineConfig holds the configuration needed to create an Engine.
type EngineConfig struct {
	EmbedModel string
	StorePath  string
	NumCtx     int
	Workspace  string
	// SessionID optionally binds ICE to an existing durable session at
	// construction time. When empty, a transient identifier is generated for
	// backwards compatibility.
	SessionID string
}

// ErrICESessionChanged reports that optional ICE work was cancelled because
// the engine was rebound to a different durable session.
var ErrICESessionChanged = errors.New("ICE session changed")

// Engine is the ICE coordinator. It owns the embedder, conversation store,
// and assembler, and exposes high-level methods for the agent loop.
type Engine struct {
	embedder   *Embedder
	store      *Store
	memStore   *memory.Store
	budgetCfg  BudgetConfig
	context    contextWindowProvider
	sessionID  string
	sessionCtx context.Context
	sessionEnd context.CancelCauseFunc
	sessionGen uint64
	sessionMax map[string]int
	projectID  string
	turnIndex  int
	autoMemory *AutoMemory
	mu         sync.RWMutex
	autoMu     sync.Mutex
	autoCancel context.CancelFunc
	autoWG     sync.WaitGroup
	autoClosed bool
	lifecycle  context.Context
	close      context.CancelFunc
	closeOnce  sync.Once
	closeErr   error
}

type contextWindowProvider interface {
	NumCtx() int
}

// NewEngine creates an ICE engine. Returns an error if the store path
// cannot be determined.
func NewEngine(client llm.Client, memStore *memory.Store, cfg EngineConfig) (*Engine, error) {
	if strings.TrimSpace(cfg.Workspace) == "" {
		return nil, fmt.Errorf("workspace identity is required for scoped ICE retrieval")
	}
	storePath := cfg.StorePath
	if storePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("determine home dir: %w", err)
		}
		storePath = filepath.Join(home, ".config", "sonar", "conversations.json")
	}

	embedModel := cfg.EmbedModel
	if embedModel == "" {
		embedModel = defaultEmbedModel
	}

	sessionID := strings.TrimSpace(cfg.SessionID)
	if sessionID == "" {
		sessionID = fmt.Sprintf("s_%d", time.Now().UnixNano())
	}
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	lifecycle, cancel := context.WithCancel(context.Background())

	store := NewStore(storePath)
	store.SetEmbedModel(embedModel)
	if err := store.Err(); err != nil {
		cancel()
		return nil, err
	}
	sessionCtx, sessionEnd := context.WithCancelCause(lifecycle)
	projectID := workspaceID(cfg.Workspace)
	turnIndex := store.maxTurnIndexScoped(projectID, sessionID)
	engine := &Engine{
		embedder:   NewEmbedder(client, embedModel),
		store:      store,
		memStore:   memStore,
		budgetCfg:  DefaultBudgetConfig(cfg.NumCtx),
		sessionID:  sessionID,
		sessionCtx: sessionCtx,
		sessionEnd: sessionEnd,
		sessionGen: 1,
		sessionMax: map[string]int{sessionID: turnIndex},
		projectID:  projectID,
		turnIndex:  turnIndex,
		autoMemory: &AutoMemory{client: client, memStore: memStore},
		lifecycle:  lifecycle,
		close:      cancel,
	}
	if provider, ok := client.(contextWindowProvider); ok {
		engine.context = provider
	}
	return engine, nil
}

// AssembleContext retrieves relevant past context for the given query.
func (e *Engine) AssembleContext(ctx context.Context, query string) (string, error) {
	return e.assembleContext(ctx, query, nil)
}

// AssembleContextWithPromptTokens retrieves relevant past context using the
// host's authoritative count of all prompt tokens already admitted. It avoids
// adding ICE when the prompt has consumed the safe context window.
func (e *Engine) AssembleContextWithPromptTokens(ctx context.Context, query string, promptTokens int) (string, error) {
	return e.assembleContext(ctx, query, &promptTokens)
}

func (e *Engine) assembleContext(ctx context.Context, query string, promptTokens *int) (string, error) {
	snapshot, opCtx, release, err := e.beginSessionOperation(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	e.mu.RLock()
	memStore := e.memStore
	budgetCfg := e.activeBudgetConfigLocked()
	e.mu.RUnlock()
	a := &Assembler{
		embedder:  e.embedder,
		convStore: e.store,
		memStore:  memStore,
		budgetCfg: budgetCfg,
		sessionID: snapshot.id,
		projectID: e.projectID,
	}
	var assembled string
	if promptTokens == nil {
		assembled, err = a.Assemble(opCtx, query)
	} else {
		assembled, err = a.AssembleWithPromptTokens(opCtx, query, *promptTokens)
	}
	if cause := context.Cause(opCtx); cause != nil {
		return "", cause
	}
	if err != nil {
		return "", err
	}
	e.mu.RLock()
	current := e.sessionGen == snapshot.generation
	e.mu.RUnlock()
	if !current {
		return "", ErrICESessionChanged
	}
	return assembled, nil
}

func (e *Engine) activeBudgetConfigLocked() BudgetConfig {
	if e.context != nil {
		if numCtx := e.context.NumCtx(); numCtx > 0 {
			return DefaultBudgetConfig(numCtx)
		}
	}
	return e.budgetCfg
}

// SetMemoryStore swaps the project-scoped store after an explicit legacy
// claim. Background auto-memory is joined first so it cannot retain and later
// persist the pre-claim empty store.
func (e *Engine) SetMemoryStore(store *memory.Store) {
	if e == nil {
		return
	}
	e.StopAutoMemory()
	e.mu.Lock()
	e.memStore = store
	if e.autoMemory != nil {
		e.autoMemory.memStore = store
	}
	e.mu.Unlock()
}

// ScopedEntryCount excludes quarantined provenance-free entries.
func (e *Engine) ScopedEntryCount() int {
	if e == nil || e.store == nil {
		return 0
	}
	return e.store.CountScoped(e.projectID)
}

// IndexMessage embeds and stores a conversation message.
// Content is truncated to 2000 characters before embedding.
func (e *Engine) IndexMessage(ctx context.Context, role, content string) error {
	if content == "" {
		return nil
	}
	snapshot, opCtx, release, err := e.beginSessionOperation(ctx)
	if err != nil {
		return err
	}
	defer release()

	text := content
	if len(text) > 2000 {
		text = text[:2000]
	}

	emb, err := e.embedder.Embed(opCtx, text)
	if cause := context.Cause(opCtx); cause != nil {
		return cause
	}
	if err != nil {
		return fmt.Errorf("embed message: %w", err)
	}

	e.mu.Lock()
	if e.sessionGen != snapshot.generation {
		e.mu.Unlock()
		return ErrICESessionChanged
	}
	e.turnIndex++
	turnIndex := e.turnIndex
	if e.sessionMax == nil {
		e.sessionMax = make(map[string]int)
	}
	e.sessionMax[snapshot.id] = turnIndex
	e.mu.Unlock()
	_, err = e.store.AddScoped(e.projectID, snapshot.id, role, text, emb, turnIndex)
	return err
}

// IndexSummary embeds and stores a compaction summary.
func (e *Engine) IndexSummary(ctx context.Context, summary string) error {
	if summary == "" {
		return nil
	}
	snapshot, opCtx, release, err := e.beginSessionOperation(ctx)
	if err != nil {
		return err
	}
	defer release()

	emb, err := e.embedder.Embed(opCtx, summary)
	if cause := context.Cause(opCtx); cause != nil {
		return cause
	}
	if err != nil {
		return fmt.Errorf("embed summary: %w", err)
	}

	e.mu.Lock()
	if e.sessionGen != snapshot.generation {
		e.mu.Unlock()
		return ErrICESessionChanged
	}
	turnIndex := e.turnIndex
	e.mu.Unlock()
	_, err = e.store.AddScoped(e.projectID, snapshot.id, "summary", summary, emb, turnIndex)
	return err
}

// DetectAutoMemory runs auto-memory detection in a background goroutine.
//
// It uses the engine lifecycle with its own timeout so normal turn completion
// does not kill it immediately. A new foreground turn cancels the job to yield
// local inference resources, and Close joins it before shutdown.
func (e *Engine) DetectAutoMemory(ctx context.Context, userMsg, assistantMsg string) {
	if e == nil || e.autoMemory == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	default:
	}
	e.autoMu.Lock()
	if e.autoClosed || e.lifecycle.Err() != nil {
		e.autoMu.Unlock()
		return
	}
	if e.autoCancel != nil {
		e.autoCancel()
		e.autoCancel = nil
	}
	jobCtx, cancel := context.WithTimeout(e.lifecycle, 30*time.Second)
	e.autoCancel = cancel
	e.autoWG.Add(1)
	e.autoMu.Unlock()
	go func() {
		defer e.autoWG.Done()
		defer cancel()
		_ = e.autoMemory.Detect(jobCtx, userMsg, assistantMsg)
	}()
}

// CancelAutoMemory yields inference resources to a new foreground turn.
func (e *Engine) CancelAutoMemory() {
	e.autoMu.Lock()
	defer e.autoMu.Unlock()
	if e.autoCancel != nil {
		e.autoCancel()
		e.autoCancel = nil
	}
}

// StopAutoMemory cancels and joins background inference before a model switch.
func (e *Engine) StopAutoMemory() {
	if e == nil {
		return
	}
	e.autoMu.Lock()
	if e.autoCancel != nil {
		e.autoCancel()
		e.autoCancel = nil
	}
	e.autoWG.Wait()
	e.autoMu.Unlock()
}

// Flush persists any pending changes to the conversation store.
func (e *Engine) Flush() error {
	return e.store.Flush()
}

// Close cancels and joins background memory extraction before flushing.
func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		e.close()
		e.autoMu.Lock()
		e.autoClosed = true
		if e.autoCancel != nil {
			e.autoCancel()
			e.autoCancel = nil
		}
		e.autoWG.Wait()
		e.autoMu.Unlock()
		e.closeErr = e.Flush()
	})
	return e.closeErr
}

// Store returns the underlying conversation store.
func (e *Engine) Store() *Store {
	return e.store
}

// SessionID returns the current session identifier.
func (e *Engine) SessionID() string {
	if e == nil {
		return ""
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.sessionID
}

// SetSessionID binds ICE to a durable session. In-flight retrieval or
// embedding work from the previous session is cancelled, and completed work
// remains scoped to the session snapshot it started with.
func (e *Engine) SetSessionID(ctx context.Context, sessionID string) error {
	if e == nil {
		return fmt.Errorf("ICE engine is unavailable")
	}
	if ctx == nil {
		return fmt.Errorf("context is required to set ICE session")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	if cause := context.Cause(e.lifecycle); cause != nil {
		return cause
	}

	e.mu.RLock()
	unchanged := e.sessionID == sessionID
	e.mu.RUnlock()
	if unchanged {
		return nil
	}

	turnIndex := e.store.maxTurnIndexScoped(e.projectID, sessionID)
	if err := ctx.Err(); err != nil {
		return err
	}
	newSessionCtx, newSessionEnd := context.WithCancelCause(e.lifecycle)

	e.mu.Lock()
	if err := ctx.Err(); err != nil {
		e.mu.Unlock()
		newSessionEnd(err)
		return err
	}
	if cause := context.Cause(e.lifecycle); cause != nil {
		e.mu.Unlock()
		newSessionEnd(cause)
		return cause
	}
	if e.sessionID == sessionID {
		e.mu.Unlock()
		newSessionEnd(context.Canceled)
		return nil
	}
	if e.sessionMax == nil {
		e.sessionMax = make(map[string]int)
	}
	if remembered := e.sessionMax[sessionID]; remembered > turnIndex {
		turnIndex = remembered
	}
	oldSessionEnd := e.sessionEnd
	e.sessionID = sessionID
	e.sessionCtx = newSessionCtx
	e.sessionEnd = newSessionEnd
	e.sessionGen++
	e.turnIndex = turnIndex
	e.sessionMax[sessionID] = turnIndex
	e.mu.Unlock()

	if oldSessionEnd != nil {
		oldSessionEnd(ErrICESessionChanged)
	}
	return nil
}

type engineSessionSnapshot struct {
	id         string
	generation uint64
	ctx        context.Context
}

func (e *Engine) beginSessionOperation(ctx context.Context) (engineSessionSnapshot, context.Context, func(), error) {
	if e == nil {
		return engineSessionSnapshot{}, nil, nil, fmt.Errorf("ICE engine is unavailable")
	}
	if ctx == nil {
		return engineSessionSnapshot{}, nil, nil, fmt.Errorf("context is required for ICE operation")
	}
	if err := ctx.Err(); err != nil {
		return engineSessionSnapshot{}, nil, nil, err
	}

	e.mu.RLock()
	snapshot := engineSessionSnapshot{
		id:         e.sessionID,
		generation: e.sessionGen,
		ctx:        e.sessionCtx,
	}
	e.mu.RUnlock()
	if snapshot.ctx == nil {
		return engineSessionSnapshot{}, nil, nil, fmt.Errorf("ICE session is unavailable")
	}
	if cause := context.Cause(snapshot.ctx); cause != nil {
		return engineSessionSnapshot{}, nil, nil, cause
	}

	opCtx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(snapshot.ctx, func() {
		cause := context.Cause(snapshot.ctx)
		if cause == nil {
			cause = context.Canceled
		}
		cancel(cause)
	})
	release := func() {
		stop()
		cancel(context.Canceled)
	}
	if cause := context.Cause(opCtx); cause != nil {
		release()
		return engineSessionSnapshot{}, nil, nil, cause
	}
	return snapshot, opCtx, release, nil
}

func validateSessionID(sessionID string) error {
	switch {
	case sessionID == "":
		return fmt.Errorf("ICE session identity is required")
	case len(sessionID) > 256:
		return fmt.Errorf("ICE session identity exceeds 256 bytes")
	default:
		return nil
	}
}

func workspaceID(workspace string) string {
	if workspace == "" {
		return ""
	}
	canonical, err := filepath.Abs(workspace)
	if err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
			canonical = resolved
		}
	}
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("workspace:%x", sum[:8])
}
