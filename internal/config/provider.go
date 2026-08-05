package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
)

// maxProviderContextSize bounds a catalog-sourced context window so a future
// catalog entry cannot overflow host-side budget arithmetic.
const maxProviderContextSize = 1 << 24

// Provider types. sonar is a single-provider harness: DeepSeek is the default
// and the only type that needs no explicit configuration.
const (
	ProviderTypeDeepSeek         = "deepseek"
	ProviderTypeOllama           = "ollama"
	ProviderTypeOpenAICompatible = "openai_compatible"
	ProviderTypeXAI              = "xai"

	// MaxProviderProfileNameBytes and MaxProviderModelNameBytes are the shared
	// configuration/session identity bounds. Configuration must reject values
	// that durable session state could not later encode.
	MaxProviderProfileNameBytes = 128
	MaxProviderModelNameBytes   = 512
)

// ProviderConfig selects the chat inference adapter. Secrets are never stored
// here — only environment variable *names*. Prefer TinyVault injection:
//
//	tvault run -p sonar --only XAI_API_KEY,OPENAI_API_KEY -- sonar
//
// Two shapes are supported:
//
//  1. Flat (single active provider):
//     provider: { type: xai, model: grok-4.5 }
//
//  2. Multi-profile (named catalog + active):
//     provider:
//     active: xai
//     profiles:
//     ollama: { type: ollama }
//     xai:    { type: xai, model: grok-4.5 }
//     openai: { type: openai_compatible, base_url: https://api.openai.com/v1, model: gpt-4.1, api_key_env: OPENAI_API_KEY }
type ProviderConfig struct {
	// Active is the profile name in use when Profiles is non-empty.
	// SONAR_PROVIDER overrides this and also accepts a type for the flat form.
	Active string `yaml:"active,omitempty"`
	// Profiles is a named catalog of provider definitions. When empty, the
	// flat Type/BaseURL/Model/APIKeyEnv fields describe the sole provider.
	Profiles map[string]ProviderProfile `yaml:"profiles,omitempty"`

	// Flat fields (single-provider form, also the resolved surface after ActiveProfile).
	Type        string `yaml:"type,omitempty"`
	BaseURL     string `yaml:"base_url,omitempty"`
	Model       string `yaml:"model,omitempty"`
	APIKeyEnv   string `yaml:"api_key_env,omitempty"`
	ContextSize int    `yaml:"context_size,omitempty"`
}

// ProviderProfile is one named inference backend definition.
type ProviderProfile struct {
	Type        string `yaml:"type,omitempty"`
	BaseURL     string `yaml:"base_url,omitempty"`
	Model       string `yaml:"model,omitempty"`
	APIKeyEnv   string `yaml:"api_key_env,omitempty"`
	ContextSize int    `yaml:"context_size,omitempty"`
}

// NormalizedProviderType returns the effective type after empty → deepseek.
func NormalizedProviderType(typ string) string {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "", ProviderTypeDeepSeek:
		return ProviderTypeDeepSeek
	case ProviderTypeOllama:
		return ProviderTypeOllama
	case ProviderTypeOpenAICompatible:
		return ProviderTypeOpenAICompatible
	case ProviderTypeXAI:
		return ProviderTypeXAI
	default:
		return strings.ToLower(strings.TrimSpace(typ))
	}
}

// IsKnownProviderType reports whether a provider type can be selected. It is
// the single answer to that question: a hard-coded list of type names is how
// the catalog unlock leaks — one allowlist updated, another silently dropping
// a valid provider back to the local runtime.
func IsKnownProviderType(typ string) bool {
	normalized := NormalizedProviderType(typ)
	switch normalized {
	case ProviderTypeOllama, ProviderTypeOpenAICompatible:
		return true
	}
	_, known := catalog.LookupProvider(catalog.ProviderID(normalized))
	return known
}

// NormalizedType returns the effective provider type after empty → deepseek.
func (c ProviderConfig) NormalizedType() string {
	return NormalizedProviderType(c.Type)
}

// IsRemote reports whether chat inference leaves this machine.
func (c ProviderConfig) IsRemote() bool {
	return ProviderProfile(c.asProfile()).IsRemote()
}

// IsRemote reports whether this profile dispatches to a network endpoint.
//
// Every profile does. sonar has no local runtime: `ollama` now means Ollama
// Cloud at https://ollama.com/v1, which is an ordinary hosted OpenAI-compatible
// provider reached with an API key, not a daemon on this machine.
//
// The constant is deliberate rather than an oversight. This predicate used to
// enumerate the one local type, and enumerating provider types is what silently
// demoted a valid catalog provider to the local path and produced "no
// compatible local chat model is installed" for a hosted model that was
// configured correctly. It stays as a named boundary — callers ask a question
// with a meaning instead of assuming — and it is the seam to reopen if a local
// runtime ever comes back.
func (p ProviderProfile) IsRemote() bool {
	return true
}

func (c ProviderConfig) asProfile() ProviderProfile {
	return ProviderProfile{
		Type:        c.Type,
		BaseURL:     c.BaseURL,
		Model:       c.Model,
		APIKeyEnv:   c.APIKeyEnv,
		ContextSize: c.ContextSize,
	}
}

// Resolve applies type-specific defaults without mutating the stored config.
func (c ProviderConfig) Resolve() ProviderConfig {
	return c.asProfile().Resolve().asConfig()
}

// Resolve applies type-specific defaults (xai base URL, key env, model).
func (p ProviderProfile) Resolve() ProviderProfile {
	out := p
	out.Type = NormalizedProviderType(out.Type)
	// A provider the Catwalk snapshot does not carry. Ollama is the only one:
	// the snapshot indexes hosted API vendors and Ollama reached that list late,
	// so its facts live here until a refresh brings them in. The catalog stays
	// the authority for everything it does know.
	if defaults, ok := builtinProviderDefaults[out.Type]; ok {
		if strings.TrimSpace(out.BaseURL) == "" {
			out.BaseURL = defaults.BaseURL
		}
		if strings.TrimSpace(out.APIKeyEnv) == "" {
			out.APIKeyEnv = defaults.APIKeyEnv
		}
		if strings.TrimSpace(out.Model) == "" {
			out.Model = defaults.Model
		}
		if out.ContextSize <= 0 {
			out.ContextSize = defaults.ContextSize
		}
		return out
	}
	// Every other type names a provider in the embedded catalog, which is the
	// authority for its endpoint, credential variable, default model, and
	// context window. A second hand-written copy of those facts is how they
	// drift; the catalog is refreshed as a unit instead.
	if defaults, ok := catalogProviderDefaults(out.Type); ok {
		if strings.TrimSpace(out.BaseURL) == "" {
			out.BaseURL = defaults.BaseURL
		}
		if strings.TrimSpace(out.APIKeyEnv) == "" {
			out.APIKeyEnv = defaults.APIKeyEnv
		}
		if strings.TrimSpace(out.Model) == "" {
			out.Model = defaults.Model
		}
		if out.ContextSize <= 0 {
			out.ContextSize = defaults.ContextSize
		}
	}
	if out.ContextSize <= 0 {
		// A profile the catalog does not know (a bare openai_compatible host,
		// a private gateway). Keep a conservative floor rather than guess large.
		out.ContextSize = 128000
	}
	return out
}

// providerDefaults is the catalog-derived starting point for a profile.
type providerDefaults struct {
	BaseURL     string
	APIKeyEnv   string
	Model       string
	ContextSize int
}

// Documented public endpoints for the catalog providers whose api_endpoint is
// an environment-variable template rather than a URL.
const (
	// AnthropicDefaultAPIEndpoint is the public Anthropic Messages API host.
	// internal/llm/anthropic.go's AnthropicBaseURL aliases this constant so the
	// dialect's own fallback and the config layer's cannot drift apart.
	AnthropicDefaultAPIEndpoint = "https://api.anthropic.com"

	// OpenAIDefaultAPIEndpoint is the public OpenAI chat-completions host, which
	// the generic OpenAI-compatible dialect already speaks.
	OpenAIDefaultAPIEndpoint = "https://api.openai.com/v1"

	// OllamaCloudDefaultAPIEndpoint is Ollama Cloud's OpenAI-compatible surface.
	// Verified rather than assumed: /v1/chat/completions answers 401 without a
	// credential and /v1/models answers 200, so the route exists and requires
	// auth. This is NOT the daemon on localhost:11434 — sonar reaches hosted
	// models only, and `ollama` names ollama.com the same way `groq` names Groq.
	OllamaCloudDefaultAPIEndpoint = "https://ollama.com/v1"
)

// builtinProviderDefaults carries the providers the embedded Catwalk snapshot
// does not list. It is a defaults table, not an allowlist: IsKnownProviderType
// remains the only answer to whether a provider is selectable.
//
// Ollama Cloud speaks the plain OpenAI-compatible dialect, so it needs no
// adapter of its own — only an endpoint, a credential variable, and a default
// model. Its native /api protocol, the one that needs a bespoke client, is the
// local daemon's, and sonar does not talk to local daemons.
var builtinProviderDefaults = map[string]providerDefaults{
	ProviderTypeOllama: {
		BaseURL:   OllamaCloudDefaultAPIEndpoint,
		APIKeyEnv: "OLLAMA_API_KEY",
		// One of the 18 models ollama.com serves, and the same family sonar
		// defaults to elsewhere, so a provider switch does not also change the
		// class of model answering.
		Model:       "deepseek-v4-flash",
		ContextSize: 128000,
	},
}

// providerEndpointFallbacks answers one question: what URL does a provider use
// when its catalog endpoint template resolves to nothing. It is a defaults
// table, not an allowlist — a provider missing from it stays fully selectable
// and simply has to be given a base_url. IsKnownProviderType remains the only
// answer to whether a provider can be selected at all.
//
// Only providers with a single documented public endpoint appear here.
//
// Azure OpenAI is deliberately absent: its host is per-resource
// (https://<resource>.openai.azure.com) and no default could be right for
// anyone. Gemini is deliberately absent too — its native API is not
// OpenAI-shaped and sonar has no Google dialect yet, so inventing an endpoint
// would only move the failure from config load to the first request, which is
// the exact failure mode this resolution step exists to remove.
var providerEndpointFallbacks = map[string]string{
	"anthropic": AnthropicDefaultAPIEndpoint,
	"openai":    OpenAIDefaultAPIEndpoint,
}

// catalogEndpointEnvName reports the environment variable a catalog
// api_endpoint template names ("$OPENAI_API_ENDPOINT" -> "OPENAI_API_ENDPOINT").
//
// isTemplate is true whenever the value is a template at all, including a
// template this code cannot safely resolve (shell syntax, an empty name). In
// that case name is empty and the caller must still treat the value as a
// template rather than pass it through as a URL.
func catalogEndpointEnvName(endpoint string) (name string, isTemplate bool) {
	if !strings.HasPrefix(endpoint, "$") {
		return "", false
	}
	name = strings.TrimPrefix(endpoint, "$")
	if validateEnvVarName(name) != nil {
		return "", true
	}
	return name, true
}

// resolveCatalogEndpoint turns a catalog api_endpoint into a usable base URL.
//
// Catwalk records four providers' endpoints as an environment-variable
// template rather than a URL — "$ANTHROPIC_API_ENDPOINT",
// "$OPENAI_API_ENDPOINT", "$GEMINI_API_ENDPOINT", "$AZURE_OPENAI_API_ENDPOINT"
// — because the endpoint is either an operator override or, for Azure,
// per-resource and known only to the operator. Copying that template verbatim
// into a profile is not a harmless placeholder: "$" is a valid sub-delims host
// character, so url.Parse("https://$OPENAI_API_ENDPOINT") succeeds and the
// template clears both validateProviderProfile and parseProviderBaseURL, then
// fails as a DNS lookup on the first real request. That reads as a network
// outage, not as the configuration bug it is.
//
// Resolution happens here, once, so every provider benefits — a per-dialect
// guard only ever rescues the dialects someone remembered to write one for.
// A template resolves to its environment variable's value when that is set,
// then to the provider's documented public endpoint, and otherwise to "".
// Empty means absent: validateProviderProfile reports a missing base_url and
// names the variable, at load time and out loud.
//
// The resolved value is never privileged. It lands in ProviderProfile.BaseURL
// and passes through exactly the same validateProviderProfile URL checks as a
// base_url typed into YAML, so an environment variable cannot become a way to
// inject an unvalidated endpoint.
func resolveCatalogEndpoint(providerType, endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	envName, isTemplate := catalogEndpointEnvName(trimmed)
	if !isTemplate {
		return trimmed
	}
	if envName != "" {
		if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
			return value
		}
	}
	return providerEndpointFallbacks[providerType]
}

// unresolvedEndpointHint names the environment variable behind a catalog
// provider's endpoint template so a missing base_url reads as the unset
// override it usually is, rather than as an unexplained requirement.
func unresolvedEndpointHint(providerType string) string {
	provider, ok := catalog.LookupProvider(catalog.ProviderID(providerType))
	if !ok {
		return ""
	}
	name, isTemplate := catalogEndpointEnvName(strings.TrimSpace(provider.APIEndpoint))
	if !isTemplate || name == "" {
		return ""
	}
	return fmt.Sprintf(
		" (the catalog names its endpoint as the environment template $%s, which is unset; export it or set base_url)",
		name,
	)
}

// catalogProviderDefaults reads a provider's defaults from the embedded
// catalog. It prefers the provider's small/fast default model: an interactive
// coding agent pays per token, so the cheap tier is the better default and the
// user can always pick the large one explicitly.
//
// The endpoint goes through resolveCatalogEndpoint rather than being copied
// verbatim: some catalog entries record an environment-variable template where
// a URL belongs, and a template must never reach a network client.
func catalogProviderDefaults(providerType string) (providerDefaults, bool) {
	id := catalog.ProviderID(providerType)
	provider, ok := catalog.LookupProvider(id)
	if !ok {
		return providerDefaults{}, false
	}
	defaults := providerDefaults{
		BaseURL:   resolveCatalogEndpoint(providerType, provider.APIEndpoint),
		APIKeyEnv: catalog.APIKeyEnv(id),
		Model:     strings.TrimSpace(provider.DefaultSmallModelID),
	}
	if defaults.Model == "" {
		defaults.Model = strings.TrimSpace(provider.DefaultLargeModelID)
	}
	if model, found := catalog.LookupModel(id, defaults.Model); found {
		if model.ContextWindow > 0 && model.ContextWindow <= int64(maxProviderContextSize) {
			defaults.ContextSize = int(model.ContextWindow)
		}
	}
	return defaults, true
}

func (p ProviderProfile) asConfig() ProviderConfig {
	return ProviderConfig{
		Type:        p.Type,
		BaseURL:     p.BaseURL,
		Model:       p.Model,
		APIKeyEnv:   p.APIKeyEnv,
		ContextSize: p.ContextSize,
	}
}

// HasProfiles reports whether a multi-profile catalog is configured.
func (c ProviderConfig) HasProfiles() bool {
	return len(c.Profiles) > 0
}

// ProfileNames returns sorted profile names. When no catalog is configured,
// returns a single synthetic name for the flat form ("ollama", "xai", …).
func (c ProviderConfig) ProfileNames() []string {
	if !c.HasProfiles() {
		name := c.flatProfileName()
		return []string{name}
	}
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (c ProviderConfig) flatProfileName() string {
	typ := c.NormalizedType()
	if typ == "" {
		return ProviderTypeDeepSeek
	}
	return typ
}

// ActiveName returns the selected profile name after defaults.
func (c ProviderConfig) ActiveName() string {
	if active := strings.TrimSpace(c.Active); active != "" {
		return active
	}
	if c.HasProfiles() {
		// Prefer an explicit deepseek profile, else the first sorted name.
		if _, ok := c.Profiles[ProviderTypeDeepSeek]; ok {
			return ProviderTypeDeepSeek
		}
		names := c.ProfileNames()
		if len(names) > 0 {
			return names[0]
		}
	}
	return c.flatProfileName()
}

// LookupProfile returns the named profile (unresolved defaults).
func (c ProviderConfig) LookupProfile(name string) (ProviderProfile, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ProviderProfile{}, false
	}
	if c.HasProfiles() {
		profile, ok := c.Profiles[name]
		return profile, ok
	}
	if name == c.flatProfileName() || name == c.NormalizedType() || (name == ProviderTypeOllama && c.NormalizedType() == ProviderTypeOllama) {
		return c.asProfile(), true
	}
	return ProviderProfile{}, false
}

// ActiveProfile returns the resolved active profile and its catalog name.
func (c ProviderConfig) ActiveProfile() (name string, profile ProviderProfile, err error) {
	return c.ResolveProfile(c.ActiveName())
}

// ResolveProfile looks up name and applies type defaults.
func (c ProviderConfig) ResolveProfile(name string) (string, ProviderProfile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = c.ActiveName()
	}
	// Allow selecting by type when profiles use type-as-name convention.
	profile, ok := c.LookupProfile(name)
	if !ok && c.HasProfiles() {
		// Match profile whose resolved type equals name (e.g. active: xai
		// when the profile is registered under another key is not supported;
		// try case-insensitive name match).
		for catalogName, candidate := range c.Profiles {
			if strings.EqualFold(catalogName, name) {
				profile, ok = candidate, true
				name = catalogName
				break
			}
		}
	}
	if !ok {
		return "", ProviderProfile{}, fmt.Errorf("provider profile %q is not defined (known: %s)", name, strings.Join(c.ProfileNames(), ", "))
	}
	return name, profile.Resolve(), nil
}

// ResolvedActive is the flat ProviderConfig surface for the active profile
// (compat for call sites that still use Resolve()).
func (c ProviderConfig) ResolvedActive() ProviderConfig {
	name, profile, err := c.ActiveProfile()
	if err != nil {
		// Fall back to flat resolve for partial configs; Validate catches errors.
		out := c.Resolve()
		out.Active = c.ActiveName()
		return out
	}
	out := profile.asConfig()
	out.Active = name
	out.Profiles = c.Profiles
	return out
}

// AllAPIKeyEnvs returns unique non-empty api_key_env names across the catalog
// (for tvault --only hints).
func (c ProviderConfig) AllAPIKeyEnvs() []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(env string) {
		env = strings.TrimSpace(env)
		if env == "" {
			return
		}
		if _, ok := seen[env]; ok {
			return
		}
		seen[env] = struct{}{}
		out = append(out, env)
	}
	if c.HasProfiles() {
		for _, name := range c.ProfileNames() {
			_, profile, err := c.ResolveProfile(name)
			if err != nil {
				continue
			}
			if profile.IsRemote() {
				add(profile.APIKeyEnv)
			}
		}
		return out
	}
	resolved := c.Resolve()
	if resolved.IsRemote() {
		add(resolved.APIKeyEnv)
	}
	return out
}

// ResolveAPIKey reads the active profile API key from the process environment.
func (c ProviderConfig) ResolveAPIKey() (string, error) {
	_, profile, err := c.ActiveProfile()
	if err != nil {
		return "", err
	}
	return profile.ResolveAPIKey()
}

// ResolveAPIKey reads this profile's key from the environment.
func (p ProviderProfile) ResolveAPIKey() (string, error) {
	resolved := p.Resolve()
	if !resolved.IsRemote() {
		return "", nil
	}
	envName := strings.TrimSpace(resolved.APIKeyEnv)
	if envName == "" {
		return "", errors.New("provider.api_key_env is empty")
	}
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		return "", fmt.Errorf(
			"%s is unset or empty; export it, or inject it at launch (for example: tvault run -p sonar --only %s -- sonar)",
			envName,
			envName,
		)
	}
	return value, nil
}

func (c *Config) validateProvider() error {
	if c.Provider.Active != "" {
		if err := validateProviderIdentityField(c.Provider.Active, MaxProviderProfileNameBytes, false); err != nil {
			return fmt.Errorf("config: provider.active: %w", err)
		}
	}
	if c.Provider.HasProfiles() {
		if len(c.Provider.Profiles) == 0 {
			return fmt.Errorf("config: provider.profiles is empty")
		}
		for name, profile := range c.Provider.Profiles {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("config: provider.profiles contains an empty name")
			}
			// Shape-check every profile in the catalog, not just the active one,
			// so a bad definition surfaces at load instead of at /provider time.
			if err := validateProviderProfile(name, profile.Resolve()); err != nil {
				return err
			}
		}
		active := c.Provider.ActiveName()
		if _, ok := c.Provider.LookupProfile(active); !ok {
			if strings.TrimSpace(c.Provider.Active) != "" {
				return fmt.Errorf(
					"config: provider.active %q is not a defined profile (known: %s)",
					c.Provider.Active,
					strings.Join(c.Provider.ProfileNames(), ", "),
				)
			}
		}
		_, activeProfile, err := c.Provider.ActiveProfile()
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if err := validateProviderProfile(active, activeProfile); err != nil {
			return err
		}
		return nil
	}
	return validateProviderProfile(c.Provider.ActiveName(), c.Provider.Resolve().asProfile())
}

// ValidateProviderProfile checks one profile against endpoint and type rules.
// Used at startup and when switching providers at runtime.
//
// It deliberately takes no local-only flag. sonar dispatches every inference
// request to a network endpoint, so gating the provider on privacy.local_only
// could only ever reject the harness's single supported configuration. That
// setting survives as a tool-endpoint control (MCP), which is where a
// local-only boundary still means something — see PrivacyConfig.LocalOnly.
func ValidateProviderProfile(name string, profile ProviderProfile) error {
	return validateProviderProfile(name, profile.Resolve())
}

func validateProviderProfile(name string, profile ProviderProfile) error {
	label := name
	if label == "" {
		label = profile.Type
	}
	if err := validateProviderIdentityField(label, MaxProviderProfileNameBytes, false); err != nil {
		return fmt.Errorf("config: provider profile name: %w", err)
	}
	if err := validateProviderIdentityField(profile.Model, MaxProviderModelNameBytes, true); err != nil {
		return fmt.Errorf("config: provider profile %q model: %w", label, err)
	}
	if profile.ContextSize < 0 {
		return fmt.Errorf("config: provider profile %q context_size cannot be negative", label)
	}
	providerType := NormalizedProviderType(profile.Type)
	switch providerType {
	case ProviderTypeOllama:
		// Was `return nil` — no validation at all, because an unauthenticated
		// daemon on localhost had nothing to check. Ollama Cloud is a hosted
		// provider with a credential, so it validates like every other one and
		// falls through to the shared checks below.
	case ProviderTypeOpenAICompatible:
		// A private gateway the catalog does not list. Its base_url, model, and
		// api_key_env must all be spelled out below.
	default:
		// Any provider the catalog knows is selectable by name.
		if _, known := catalog.LookupProvider(catalog.ProviderID(providerType)); !known {
			return fmt.Errorf(
				"config: provider profile %q names an unknown provider %q; use %q for a private endpoint, or a provider in the catalog",
				label,
				providerType,
				ProviderTypeOpenAICompatible,
			)
		}
	}
	if strings.TrimSpace(profile.BaseURL) == "" {
		return fmt.Errorf(
			"config: provider profile %q requires base_url for type %q%s",
			label,
			profile.Type,
			unresolvedEndpointHint(providerType),
		)
	}
	raw := profile.BaseURL
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("config: provider profile %q has an invalid base_url", label)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return fmt.Errorf(
			"config: provider profile %q base_url must not contain user information, a query, or a fragment",
			label,
		)
	}
	// "$" is a legal sub-delims character in a URL host, so an unresolved
	// "$OPENAI_API_ENDPOINT" parses cleanly and would only fail later, as a DNS
	// error that reads like a network outage. No real host contains "$", so
	// rejecting it here costs nothing and turns any template that escapes
	// resolveCatalogEndpoint — or that a user wrote into YAML by hand — into a
	// config error at load.
	if strings.Contains(u.Host, "$") {
		return fmt.Errorf(
			"config: provider profile %q base_url looks like an unresolved environment template%s",
			label,
			unresolvedEndpointHint(providerType),
		)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf(
			"config: provider profile %q base_url must use http or https",
			label,
		)
	}
	if scheme == "http" && !isLocalHost(u.Hostname()) {
		return fmt.Errorf(
			"config: provider profile %q requires https for a non-local base_url",
			label,
		)
	}
	if strings.TrimSpace(profile.Model) == "" {
		return fmt.Errorf("config: provider profile %q requires model for type %q", label, profile.Type)
	}
	if strings.TrimSpace(profile.APIKeyEnv) == "" {
		return fmt.Errorf("config: provider profile %q requires api_key_env for type %q (env var name only; never put the secret in YAML)", label, profile.Type)
	}
	if err := validateEnvVarName(profile.APIKeyEnv); err != nil {
		return fmt.Errorf("config: provider profile %q api_key_env: %w", label, err)
	}
	return nil
}

func validateProviderIdentityField(value string, limit int, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("is empty")
	}
	if !utf8.ValidString(value) {
		return errors.New("is not valid UTF-8")
	}
	if len(value) > limit {
		return fmt.Errorf("exceeds %d bytes", limit)
	}
	if strings.TrimSpace(value) != value || strings.Join(strings.Fields(value), " ") != value {
		return errors.New("contains non-canonical whitespace")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Bidi_Control) {
			return errors.New("contains control characters")
		}
	}
	return nil
}

func validateEnvVarName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
			continue
		case r >= '0' && r <= '9':
			if i == 0 {
				return errors.New("must not start with a digit")
			}
		default:
			return errors.New("contains an invalid environment variable character")
		}
	}
	return nil
}
