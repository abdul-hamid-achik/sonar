package llm

import (
	"errors"
	"fmt"
	"strings"

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

	// DeepSeekFlashModel is the model sonar pins to. Its current published
	// build is DeepSeek-V4-Flash-0731 (284B total / 13B active).
	DeepSeekFlashModel = "deepseek-v4-flash"

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

// ErrUnsupportedModel rejects any model other than the pinned Flash build.
var ErrUnsupportedModel = errors.New("unsupported model")

// DeepSeekOptions configures the pinned DeepSeek client.
type DeepSeekOptions struct {
	// APIKey is the resolved secret value, read from the environment by the
	// caller. Required: sonar has no unauthenticated mode.
	APIKey string
	// Model defaults to DeepSeekFlashModel. Any other value is rejected.
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

// NormalizeDeepSeekModel accepts the pinned Flash model and rejects the rest.
// sonar is deliberately single-model: the whole harness — context policy, cost
// display, and thinking defaults — is calibrated to one set of published
// numbers, and silently running another model would invalidate all three.
func NormalizeDeepSeekModel(model string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return DeepSeekFlashModel, nil
	}
	if normalized == DeepSeekFlashModel {
		return DeepSeekFlashModel, nil
	}
	return "", fmt.Errorf("%w %q: sonar runs %s only", ErrUnsupportedModel, model, DeepSeekFlashModel)
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
	model, err := NormalizeDeepSeekModel(opts.Model)
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
// Both the startup path and the runtime /provider switch go through here so a
// DeepSeek profile can never be attached with the plain OpenAI dialect — which
// would compile and connect, then fail at the second tool-call iteration.
func NewProviderClient(providerType, baseURL, model, apiKey string) (*OpenAICompatibleClient, error) {
	if config.NormalizedProviderType(providerType) == config.ProviderTypeDeepSeek {
		return NewDeepSeekClient(DeepSeekOptions{
			APIKey:  apiKey,
			Model:   model,
			BaseURL: baseURL,
			// Thinking matches the API default. Individual requests still opt
			// out through ChatOptions.DisableReasoning.
			Thinking:        true,
			ReasoningEffort: DeepSeekDefaultEffort,
		})
	}
	return NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: baseURL,
		Model:   model,
		APIKey:  apiKey,
	})
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
