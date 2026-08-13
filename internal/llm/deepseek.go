package llm

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// DeepSeek is sonar's only inference backend. Every fact below is taken from
// the official API contract (api-docs.deepseek.com) rather than inferred from
// the generic OpenAI shape, because three of them differ in ways that break an
// agent loop:
//
//   - Thinking is toggled by `{"thinking": {"type": ...}}`, not by
//     reasoning_effort. reasoning_effort only grades depth once thinking is on.
//   - Thinking defaults to ENABLED. A harness that never sends the toggle pays
//     for chain-of-thought on every turn.
//   - An assistant message that carries tool calls must echo its own
//     reasoning_content back on all later requests, or the API answers 400.
//
// See OpenAICompatibleClient's DialectDeepSeek handling for the wire details.
const (
	// DeepSeekBaseURL is the documented OpenAI-format endpoint. DeepSeek also
	// serves an Anthropic-format surface at /anthropic, which sonar does not use.
	DeepSeekBaseURL = "https://api.deepseek.com"

	// DeepSeekAPIKeyEnv names the environment variable holding the key. Only
	// the NAME is ever configured; the value is read from the process
	// environment so a secret can never land in YAML.
	DeepSeekAPIKeyEnv = "DEEPSEEK_API_KEY"

	// DeepSeekFlashModel is the default cheap tier. Its current published
	// build is DeepSeek-V4-Flash-0731 (284B total / 13B active).
	DeepSeekFlashModel = "deepseek-v4-flash"

	// DeepSeekProModel is the catalog's large DeepSeek tier. Same 1M context
	// and thinking contract as Flash; pick it explicitly via /model or config.
	DeepSeekProModel = "deepseek-v4-pro"

	// DeepSeekContextWindow is the 1M-token context DeepSeek serves by default.
	DeepSeekContextWindow = 1_000_000

	// DeepSeekMaxOutputTokens is the published generation ceiling.
	DeepSeekMaxOutputTokens = 384_000

	// DeepSeekDefaultEffort matches the API default for ordinary requests.
	// "low" and "medium" are mapped to "high" server-side; "xhigh" maps to "max".
	DeepSeekDefaultEffort = "high"

	// DeepSeekMaxEffort is the deepest setting, which DeepSeek selects
	// automatically for some agent clients.
	DeepSeekMaxEffort = "max"
)

// DeepSeekPricing is the published per-million-token rate card, in USD.
// Cache hits are ~50x cheaper than misses, so prompt-prefix stability is the
// single biggest cost lever in a long agent session.
//
// DeepSeek has announced peak/off-peak pricing that doubles every billing item
// during 09:00–12:00 and 14:00–18:00 Beijing time (UTC+8). These constants are
// the off-peak/regular rates; any cost surface must present them as an
// estimate, never as a settled charge.
const (
	DeepSeekInputCacheHitUSDPerMTok  = 0.0028
	DeepSeekInputCacheMissUSDPerMTok = 0.14
	DeepSeekOutputUSDPerMTok         = 0.28
)

// DeepSeekOptions configures the pinned DeepSeek client.
type DeepSeekOptions struct {
	// APIKey is the resolved secret value, read from the environment by the
	// caller. Required: sonar has no unauthenticated mode.
	APIKey string
	// Model defaults to DeepSeekFlashModel. Other catalog ids (including
	// DeepSeekProModel) are accepted; unlisted ids pass through too.
	Model string
	// BaseURL overrides the endpoint for a gateway or proxy that speaks the
	// same dialect. Defaults to DeepSeekBaseURL.
	BaseURL string
	// Thinking enables chain-of-thought. Defaults to true via NewDeepSeekClient,
	// matching the API default.
	Thinking bool
	// ReasoningEffort grades thinking depth. Defaults to DeepSeekDefaultEffort.
	ReasoningEffort string
}

// ResolveProviderModel fills in a provider's default model when the caller
// left it empty, and otherwise passes the requested model through.
//
// sonar runs many models; DeepSeek Flash is only the default. An id the
// catalog does not list is accepted rather than rejected: the catalog is a
// pinned snapshot, so refusing unlisted models would make the harness unusable
// the day a provider ships a new one. The cost is that such a model has no
// catalog-derived context window or pricing, and surfaces that depend on those
// must degrade rather than assume — see catalog.FindModel's miss path.
func ResolveProviderModel(providerType, model string) (string, error) {
	id := catalog.ProviderID(config.NormalizedProviderType(providerType))
	provider, known := catalog.LookupProvider(id)
	if !known {
		// A private endpoint the catalog does not describe. Nothing to validate
		// against, so the caller's model stands.
		return strings.TrimSpace(model), nil
	}
	model = strings.TrimSpace(model)
	if model == "" {
		if fallback := strings.TrimSpace(provider.DefaultSmallModelID); fallback != "" {
			return fallback, nil
		}
		return strings.TrimSpace(provider.DefaultLargeModelID), nil
	}
	return model, nil
}

// NewDeepSeekClient builds the pinned DeepSeek chat client.
func NewDeepSeekClient(opts DeepSeekOptions) (*OpenAICompatibleClient, error) {
	key := strings.TrimSpace(opts.APIKey)
	if key == "" {
		return nil, fmt.Errorf(
			"%s is unset or empty; sonar requires a DeepSeek API key (export %s, or inject it at launch)",
			DeepSeekAPIKeyEnv,
			DeepSeekAPIKeyEnv,
		)
	}
	model, err := ResolveProviderModel(config.ProviderTypeDeepSeek, opts.Model)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = DeepSeekBaseURL
	}
	effort := strings.ToLower(strings.TrimSpace(opts.ReasoningEffort))
	if effort == "" {
		effort = DeepSeekDefaultEffort
	}
	return NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL:         baseURL,
		Model:           model,
		APIKey:          key,
		Dialect:         DialectDeepSeek,
		Thinking:        opts.Thinking,
		ReasoningEffort: effort,
	})
}

// NewProviderClient builds the chat client for a resolved provider profile.
//
// The dialect is chosen from the provider's identity, not from its endpoint
// shape. The catalog calls DeepSeek "openai-compat", which is true about the
// URL and wrong about the request contract: it needs a thinking toggle and a
// reasoning_content round-trip no generic OpenAI client sends. Selecting on
// wire type alone would produce a client that connects, answers once, and then
// fails every tool-call turn with a 400.
//
// The anthropic-family branch below is the one place selecting by wire type
// would actually have been safe — Catwalk's "anthropic" type genuinely means
// "speaks the real Anthropic Messages API" for all four providers that carry
// it — but identity is still used, for the same reason as DeepSeek: it keeps
// dialect selection independent of how Catwalk chooses to classify a provider
// upstream, and it is one line longer than a wire-type switch would have been.
//
// Both the startup path and the runtime /provider switch go through here so
// the two can never disagree about which dialect a provider gets.
func NewProviderClient(providerType, baseURL, model, apiKey string) (RemoteChatClient, error) {
	normalized := config.NormalizedProviderType(providerType)
	resolved, err := ResolveProviderModel(normalized, model)
	if err != nil {
		return nil, err
	}
	switch {
	case normalized == config.ProviderTypeDeepSeek:
		client, err := NewDeepSeekClient(DeepSeekOptions{
			APIKey:  apiKey,
			Model:   resolved,
			BaseURL: baseURL,
			// Thinking matches the API default. Individual requests still opt
			// out through ChatOptions.DisableReasoning.
			Thinking:        true,
			ReasoningEffort: DeepSeekDefaultEffort,
		})
		if err != nil {
			return nil, err
		}
		return client, nil
	case IsAnthropicFamilyProvider(normalized):
		client, err := NewAnthropicProviderClient(normalized, baseURL, resolved, apiKey)
		if err != nil {
			return nil, err
		}
		return client, nil
	default:
		// Everything else currently rides the plain OpenAI-compatible dialect.
		// That covers 27 of the catalog's 40 providers; google and the
		// cloud-credential families still need their own dialects before they
		// will work.
		client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
			BaseURL: baseURL,
			Model:   resolved,
			APIKey:  apiKey,
		})
		if err != nil {
			return nil, err
		}
		return client, nil
	}
}

// EstimateDeepSeekCostUSD returns the estimated turn cost from a token receipt.
// cachedPromptTokens is the cache-hit portion of promptTokens; pass 0 when the
// provider did not report one. The result is an estimate: it does not know
// whether the request landed in a peak-pricing window.
func EstimateDeepSeekCostUSD(promptTokens, cachedPromptTokens, completionTokens int) float64 {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	if cachedPromptTokens < 0 {
		cachedPromptTokens = 0
	}
	if cachedPromptTokens > promptTokens {
		cachedPromptTokens = promptTokens
	}
	missTokens := promptTokens - cachedPromptTokens
	const perMillion = 1_000_000.0
	return float64(cachedPromptTokens)/perMillion*DeepSeekInputCacheHitUSDPerMTok +
		float64(missTokens)/perMillion*DeepSeekInputCacheMissUSDPerMTok +
		float64(completionTokens)/perMillion*DeepSeekOutputUSDPerMTok
}
