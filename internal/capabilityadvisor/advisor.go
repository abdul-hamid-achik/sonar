// Package capabilityadvisor asks MCPHub for a bounded capability route without
// granting the recommendation any execution authority.
package capabilityadvisor

import (
	"context"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

const (
	resolverToolName = "mcphub_resolve_tool"
	resolverMaxHits  = 5
	maxCacheEntries  = 256
	// Positive routes are deliberately short-lived when the host has no
	// proactive catalog-revision feed. MCP servers can reconnect or change
	// their advertised tools while sonar remains open, so an unbounded
	// positive cache would keep recommending a stale route indefinitely.
	resolvedCacheTTL  = 5 * time.Minute
	ambiguousCacheTTL = 30 * time.Second
	// A no-match can mean that a downstream was temporarily disconnected.
	// Keep negative caching brief even when the host cannot yet supply a
	// catalog revision.
	noMatchCacheTTL = 30 * time.Second
)

// Registry is the narrow MCP surface used by the host-owned advisor. The
// advisor only calls mcphub_resolve_tool; recommended tools remain model-owned
// calls through the normal authority and execution paths.
type Registry interface {
	ResolveToolName(remoteName string) (string, bool)
	CallTool(ctx context.Context, exposedName string, args map[string]any) (*mcp.ToolResult, error)
}

// Activity is the bounded, host-authored summary sent to MCPHub. These fields
// must describe the activity, never contain raw files, tool output, secrets,
// credentials, or actual input values. AvailableInputKinds contains labels
// such as "url", "workspace", "database", or "artifact_id" only.
type Activity struct {
	Objective           string
	Phase               string
	CurrentActivity     string
	DesiredOutcome      string
	AvailableInputKinds []string
	// IntentTags are locally classified, allowlisted semantic labels such as
	// "symbols", "observability", or "repository". They preserve useful
	// routing signal without sending the user's prompt or arbitrary wording.
	IntentTags []string
}

// Request carries host control metadata separately from the activity sent to
// MCPHub. GoalID and CacheDiscriminator participate only in an in-memory hashed
// cache key and are never included in the resolver query or durable state.
// NonTrivial fails closed: a false value skips resolution. Reconsider bypasses
// a previous successful recommendation or no-match result after a downstream
// failure or an explicit user request. CatalogRevision is optional, opaque
// host-owned metadata. When available, it separates cache generations without
// ever entering the resolver query.
type Request struct {
	GoalID             string
	NonTrivial         bool
	Reconsider         bool
	CatalogRevision    string
	CacheDiscriminator [32]byte
	Activity           Activity
}

// Status is a bounded host conclusion. It intentionally carries no remote
// error text or arbitrary resolver output.
type Status string

const (
	StatusSkipped     Status = "skipped"
	StatusResolved    Status = "resolved"
	StatusAmbiguous   Status = "ambiguous"
	StatusNoMatch     Status = "no_match"
	StatusUnavailable Status = "unavailable"
	StatusInvalid     Status = "invalid"
)

// Hint is the complete allowlisted projection of a resolver recommendation.
// Argument values, schemas, descriptions, scores, matched terms, and resolver
// prose are intentionally absent. An ambiguous hint is still useful model
// context, but must not be treated as a selected or executed tool.
type Hint struct {
	Namespaced                string
	Server                    string
	Tool                      string
	RequiredFields            []string
	Alternatives              []string
	Ambiguous                 bool
	MetadataTruncated         bool
	ArgumentTemplateTruncated bool
	AlternativesTruncated     bool
}

// NeedsDescription reports whether MCPHub omitted part of the argument shape
// and mcphub_describe_tool should precede any model-authored downstream call.
func (h Hint) NeedsDescription() bool { return h.ArgumentTemplateTruncated }

// Truncated reports whether any resolver list or recommendation metadata was
// bounded. The individual flags remain available so a caller can distinguish
// an incomplete argument shape from merely compact presentation metadata.
func (h Hint) Truncated() bool {
	return h.MetadataTruncated || h.ArgumentTemplateTruncated || h.AlternativesTruncated
}

// Result is deliberately non-error-bearing. Resolver failures must not fail a
// user turn; callers continue without Hint when Status is unavailable or
// invalid. Attempted means this invocation dispatched an MCP resolver call.
// Cached means no MCP call was needed because this phase/activity was already
// resolved successfully (including a valid ambiguous or no-match result).
// CatalogRevision is an optional bounded opaque revision returned by MCPHub.
type Result struct {
	Status          Status
	Hint            *Hint
	CatalogRevision string
	Attempted       bool
	Cached          bool
}

type cacheKey [32]byte

type cacheEntry struct {
	status          Status
	hint            *Hint
	catalogRevision string
	expiresAt       time.Time
}

type flight struct {
	done   chan struct{}
	result Result
}

// Advisor owns only ephemeral deduplication state. It does not persist raw
// inputs or outputs, reconnect servers, modify MCP scope, or execute a
// recommended downstream tool.
type Advisor struct {
	registry Registry

	mu       sync.RWMutex
	cache    map[cacheKey]cacheEntry
	order    []cacheKey
	inflight map[cacheKey]*flight
	now      func() time.Time
}

// New returns a capability advisor backed by registry. A nil registry is
// allowed and degrades every eligible request to StatusUnavailable.
func New(registry Registry) *Advisor {
	return &Advisor{
		registry: registry,
		cache:    make(map[cacheKey]cacheEntry),
		inflight: make(map[cacheKey]*flight),
		now:      time.Now,
	}
}

// InvalidateAll drops every in-memory recommendation. Call when the host
// observes a catalog generation change (MCP reconnect, pin change, or an
// MCPHub catalog_revision advance) so the next Advise cannot reuse a stale
// route. Inflight calls are left alone; they still complete for their waiters.
func (a *Advisor) InvalidateAll() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.cache = make(map[cacheKey]cacheEntry)
	a.order = a.order[:0]
	a.mu.Unlock()
}

// InvalidateCatalog drops cache entries that were stored under a different
// catalog generation than revision. When revision is empty, this is a no-op so
// hosts without a revision feed do not thrash the cache.
func (a *Advisor) InvalidateCatalog(revision string) {
	if a == nil || revision == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.order[:0]
	for _, key := range a.order {
		entry, ok := a.cache[key]
		if !ok {
			continue
		}
		if entry.catalogRevision != "" && entry.catalogRevision != revision {
			delete(a.cache, key)
			continue
		}
		kept = append(kept, key)
	}
	a.order = kept
}

// Advise resolves the current non-trivial activity at most once for its cache
// key. It never returns an error and never calls the recommended tool.
func (a *Advisor) Advise(ctx context.Context, request Request) Result {
	if !request.NonTrivial {
		return Result{Status: StatusSkipped}
	}
	if a == nil || a.registry == nil || ctx == nil {
		return Result{Status: StatusUnavailable}
	}
	if ctx.Err() != nil {
		return Result{Status: StatusUnavailable}
	}

	prepared, err := prepareRequest(request)
	if err != nil {
		return Result{Status: StatusInvalid}
	}

	var active *flight
	for {
		// A reconsider request may have waited for an older flight to finish.
		// Re-check cancellation before it claims a fresh flight; otherwise a
		// deadline that expired while waiting could be reported as a resolver
		// attempt even though no refresh should have been dispatched.
		if ctx.Err() != nil {
			return Result{Status: StatusUnavailable}
		}
		a.mu.Lock()
		if request.Reconsider {
			a.deleteCacheLocked(prepared.key)
		}
		if cached, ok := a.cache[prepared.key]; ok {
			if cached.expired(a.now()) {
				a.deleteCacheLocked(prepared.key)
			} else {
				a.mu.Unlock()
				return resultFromCache(cached)
			}
		}
		if existing, ok := a.inflight[prepared.key]; ok {
			done := existing.done
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return Result{Status: StatusUnavailable}
			case <-done:
				if request.Reconsider {
					// A forced refresh must not inherit a recommendation that
					// started before the request. Loop, discard its cache entry,
					// and own a fresh resolver call.
					continue
				}
				return resultFromFlight(existing.result)
			}
		}
		active = &flight{done: make(chan struct{})}
		a.inflight[prepared.key] = active
		a.mu.Unlock()
		break
	}

	result := a.resolve(ctx, prepared.query)
	if prepared.catalogRevision != "" && result.CatalogRevision != "" &&
		prepared.catalogRevision != result.CatalogRevision {
		// The host and resolver observed different catalog generations. Do not
		// cache or expose a recommendation whose availability may already have
		// changed; a later call can retry against a coherent snapshot.
		result = Result{Status: StatusInvalid, Attempted: result.Attempted}
	}

	a.mu.Lock()
	if cacheable(result.Status) {
		a.storeCacheLocked(prepared.key, cacheEntryFromResult(result, prepared.catalogRevision, a.now()))
	}
	active.result = cloneResult(result)
	delete(a.inflight, prepared.key)
	close(active.done)
	a.mu.Unlock()
	return result
}

func (a *Advisor) resolve(ctx context.Context, query string) Result {
	if ctx.Err() != nil {
		return Result{Status: StatusUnavailable}
	}
	exposedName, ok := a.registry.ResolveToolName(resolverToolName)
	if !ok {
		return Result{Status: StatusUnavailable}
	}
	result, err := a.registry.CallTool(ctx, exposedName, map[string]any{
		"query":    query,
		"max_hits": resolverMaxHits,
	})
	if err != nil || result == nil || result.IsError || hasErrorMetadata(result.ErrorMeta) {
		return Result{Status: StatusUnavailable, Attempted: true}
	}

	hint, matched, catalogRevision, err := parseResolverResult(result)
	if err != nil {
		return Result{Status: StatusInvalid, Attempted: true}
	}
	if !matched {
		return Result{Status: StatusNoMatch, CatalogRevision: catalogRevision, Attempted: true}
	}
	status := StatusResolved
	if hint.Ambiguous {
		status = StatusAmbiguous
	}
	return Result{Status: status, Hint: hint, CatalogRevision: catalogRevision, Attempted: true}
}

func cacheable(status Status) bool {
	return status == StatusResolved || status == StatusAmbiguous || status == StatusNoMatch
}

func resultFromCache(entry cacheEntry) Result {
	return Result{
		Status: entry.status, Hint: cloneHint(entry.hint),
		CatalogRevision: entry.catalogRevision, Cached: true,
	}
}

func resultFromFlight(result Result) Result {
	result = cloneResult(result)
	result.Attempted = false
	result.Cached = cacheable(result.Status)
	return result
}

func cloneResult(result Result) Result {
	result.Hint = cloneHint(result.Hint)
	return result
}

func cacheEntryFromResult(result Result, requestedRevision string, now time.Time) cacheEntry {
	revision := result.CatalogRevision
	if revision == "" {
		revision = requestedRevision
	}
	entry := cacheEntry{
		status: result.Status, hint: cloneHint(result.Hint), catalogRevision: revision,
	}
	switch result.Status {
	case StatusResolved:
		entry.expiresAt = now.Add(resolvedCacheTTL)
	case StatusAmbiguous:
		entry.expiresAt = now.Add(ambiguousCacheTTL)
	case StatusNoMatch:
		entry.expiresAt = now.Add(noMatchCacheTTL)
	}
	return entry
}

func (entry cacheEntry) expired(now time.Time) bool {
	return !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt)
}

func cloneHint(hint *Hint) *Hint {
	if hint == nil {
		return nil
	}
	cloned := *hint
	cloned.RequiredFields = append([]string(nil), hint.RequiredFields...)
	cloned.Alternatives = append([]string(nil), hint.Alternatives...)
	return &cloned
}

func (a *Advisor) storeCacheLocked(key cacheKey, entry cacheEntry) {
	if _, exists := a.cache[key]; exists {
		a.cache[key] = entry
		return
	}
	if len(a.cache) >= maxCacheEntries && len(a.order) > 0 {
		delete(a.cache, a.order[0])
		a.order = a.order[1:]
	}
	a.cache[key] = entry
	a.order = append(a.order, key)
}

func (a *Advisor) deleteCacheLocked(key cacheKey) {
	if _, exists := a.cache[key]; !exists {
		return
	}
	delete(a.cache, key)
	for index, candidate := range a.order {
		if candidate != key {
			continue
		}
		copy(a.order[index:], a.order[index+1:])
		a.order = a.order[:len(a.order)-1]
		return
	}
}
