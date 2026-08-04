// Package catalog answers metadata questions about inference providers and
// models: what a model costs, how large its context is, whether it can reason,
// whether it accepts images.
//
// The data is a pinned snapshot of Catwalk (catwalk.charm.land), embedded at
// build time. Nothing here touches the network — startup latency is a tracked
// concern and tests must stay deterministic. Refresh the snapshot explicitly
// with `sonar providers refresh`.
//
// # Catalog is not dialect
//
// This package deliberately says nothing about how to build a request. Catwalk
// classifies DeepSeek as "openai-compat", which is true about the endpoint
// shape and insufficient about the request contract — DeepSeek needs a thinking
// toggle and a reasoning_content round-trip that no generic OpenAI client
// sends. Wire behavior lives in the llm dialects; treating a catalog entry as
// enough to talk to a provider is the mistake this separation exists to
// prevent.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"charm.land/catwalk/pkg/catwalk"
)

//go:embed providers.json
var embeddedProviders []byte

// Provider and Model are Catwalk's types, re-exported so callers need not
// import catwalk directly and so the snapshot format and the in-memory format
// cannot drift apart.
type (
	Provider = catwalk.Provider
	Model    = catwalk.Model
	// ProviderID identifies a provider ("deepseek", "anthropic", "groq", …).
	ProviderID = catwalk.InferenceProvider
	// WireType is the request-family hint ("openai-compat", "anthropic", …).
	// It selects a dialect; it does not define one.
	WireType = catwalk.Type
)

var (
	loadOnce  sync.Once
	loaded    []Provider
	loadErr   error
	byID      map[ProviderID]Provider
	modelsBy  map[ProviderID]map[string]Model
	sortedIDs []ProviderID
)

func load() {
	loadOnce.Do(func() {
		var providers []Provider
		if err := json.Unmarshal(embeddedProviders, &providers); err != nil {
			loadErr = fmt.Errorf("catalog: decode embedded snapshot: %w", err)
			return
		}
		if len(providers) == 0 {
			loadErr = fmt.Errorf("catalog: embedded snapshot has no providers")
			return
		}
		byID = make(map[ProviderID]Provider, len(providers))
		modelsBy = make(map[ProviderID]map[string]Model, len(providers))
		sortedIDs = make([]ProviderID, 0, len(providers))
		for _, provider := range providers {
			if strings.TrimSpace(string(provider.ID)) == "" {
				continue
			}
			byID[provider.ID] = provider
			sortedIDs = append(sortedIDs, provider.ID)
			models := make(map[string]Model, len(provider.Models))
			for _, model := range provider.Models {
				models[model.ID] = model
			}
			modelsBy[provider.ID] = models
		}
		sort.Slice(sortedIDs, func(i, j int) bool { return sortedIDs[i] < sortedIDs[j] })
		loaded = providers
	})
}

// Err reports a malformed embedded snapshot. A non-nil result means the binary
// was built with a broken catalog, so callers should surface it at startup
// rather than degrade silently to an empty list.
func Err() error {
	load()
	return loadErr
}

// Providers returns every known provider, ordered by ID.
func Providers() []Provider {
	load()
	out := make([]Provider, 0, len(sortedIDs))
	for _, id := range sortedIDs {
		out = append(out, byID[id])
	}
	return out
}

// ProviderIDs returns every known provider ID, ordered.
func ProviderIDs() []ProviderID {
	load()
	return append([]ProviderID(nil), sortedIDs...)
}

// LookupProvider returns the provider with this ID.
func LookupProvider(id ProviderID) (Provider, bool) {
	load()
	provider, ok := byID[id]
	return provider, ok
}

// LookupModel returns one model within a provider.
func LookupModel(providerID ProviderID, modelID string) (Model, bool) {
	load()
	models, ok := modelsBy[providerID]
	if !ok {
		return Model{}, false
	}
	model, ok := models[strings.TrimSpace(modelID)]
	return model, ok
}

// Models returns a provider's models in catalog order.
func Models(providerID ProviderID) []Model {
	load()
	provider, ok := byID[providerID]
	if !ok {
		return nil
	}
	return append([]Model(nil), provider.Models...)
}

// APIKeyEnv returns the environment variable NAME a provider's key is read
// from, derived from Catwalk's "$VAR" template. It returns "" for providers
// that authenticate through a cloud credential chain (bedrock, vertexai) rather
// than a single variable.
//
// Only the name is ever returned. The value belongs to the process
// environment and must not pass through configuration or presentation.
func APIKeyEnv(providerID ProviderID) string {
	load()
	provider, ok := byID[providerID]
	if !ok {
		return ""
	}
	template := strings.TrimSpace(provider.APIKey)
	if !strings.HasPrefix(template, "$") {
		return ""
	}
	name := strings.TrimPrefix(template, "$")
	// Reject anything that is not a plain variable reference; a template with
	// shell syntax is not a name we can safely hand to os.Getenv.
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return ""
		}
	}
	return name
}

// EstimateCostUSD returns the estimated cost of one turn against a model.
// cachedInputTokens is the cache-hit portion of inputTokens.
//
// The result is an estimate. It does not know about surge pricing — DeepSeek,
// for one, doubles every billing item during published peak hours — so callers
// must present it as an estimate, never as a settled charge.
func EstimateCostUSD(model Model, inputTokens, cachedInputTokens, outputTokens int) float64 {
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if cachedInputTokens < 0 {
		cachedInputTokens = 0
	}
	if cachedInputTokens > inputTokens {
		cachedInputTokens = inputTokens
	}
	const perMillion = 1_000_000.0
	missTokens := inputTokens - cachedInputTokens
	return float64(cachedInputTokens)/perMillion*model.CostPer1MInCached +
		float64(missTokens)/perMillion*model.CostPer1MIn +
		float64(outputTokens)/perMillion*model.CostPer1MOut
}
