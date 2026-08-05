package command

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// /provider rejected "deepseek" — sonar's own default provider — as unknown,
// because the fallback for an unconfigured type was a hand-written list of
// three names written before the catalog existed. The switch layer already
// resolved a bare type through config.IsKnownProviderType, so the slash command
// was the only thing refusing.
//
// This asserts the property rather than a list: every type the configuration
// layer considers selectable must be reachable from /provider. A new catalog
// provider therefore cannot become selectable in one layer and unknown in the
// other without failing here.
func TestProviderCommandAcceptsEverySelectableType(t *testing.T) {
	r := newTestRegistry()
	// The configured profile is named after none of the types under test, which
	// is the whole point: the user is naming a provider type, not a profile, so
	// every case here reaches the type fallback rather than the name loop above
	// it. A ProviderList containing "deepseek" would have hidden the headline
	// case behind a name match.
	ctx := &Context{Provider: "work-gateway", ProviderList: []string{"work-gateway"}}

	types := selectableProviderTypes(t)
	for _, providerType := range types {
		t.Run(providerType, func(t *testing.T) {
			result := r.Execute(ctx, "provider", []string{providerType})
			if result.Error != "" {
				t.Fatalf("/provider %s = %q, want a switch", providerType, result.Error)
			}
			if result.Action != ActionSwitchProvider {
				t.Fatalf("/provider %s action = %d, want ActionSwitchProvider", providerType, result.Action)
			}
			if result.Data != providerType {
				t.Errorf("switch target = %q, want %q", result.Data, providerType)
			}
		})
	}
}

// The named-profile path still wins over type resolution, and a genuinely
// unknown name is still refused — widening the fallback must not turn the
// command into one that accepts anything.
func TestProviderCommandStillRefusesUnknownNames(t *testing.T) {
	r := newTestRegistry()
	ctx := &Context{Provider: "deepseek", ProviderList: []string{"work-gateway"}}

	if result := r.Execute(ctx, "provider", []string{"work-gateway"}); result.Data != "work-gateway" {
		t.Fatalf("a configured profile name = %#v, want it selected", result)
	}
	for _, target := range []string{"nope", "deep seek", "deepseek-v4", "$OPENAI_API_ENDPOINT"} {
		if result := r.Execute(ctx, "provider", []string{target}); result.Error == "" {
			t.Errorf("/provider %q = %#v, want an error", target, result)
		}
	}
}

// A provider type is a type whatever its casing; the profile-name loop above it
// already matches case-insensitively.
func TestProviderCommandTypeMatchIsCaseInsensitive(t *testing.T) {
	r := newTestRegistry()
	ctx := &Context{Provider: "deepseek"}

	for _, target := range []string{"DeepSeek", "OLLAMA", "OpenAI_Compatible"} {
		result := r.Execute(ctx, "provider", []string{target})
		if result.Action != ActionSwitchProvider {
			t.Errorf("/provider %q = %#v, want a switch", target, result)
		}
	}
}

// selectableProviderTypes enumerates what the configuration layer will accept,
// so this test cannot fall out of date with the catalog the way the list it
// replaced did.
func selectableProviderTypes(t *testing.T) []string {
	t.Helper()
	candidates := []string{
		config.ProviderTypeOllama,
		config.ProviderTypeOpenAICompatible,
		config.ProviderTypeDeepSeek,
		"anthropic", "openai", "groq", "xai", "cerebras", "moonshot", "opencode-zen",
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !config.IsKnownProviderType(candidate) {
			t.Fatalf("%q is not selectable; the candidate list is stale", candidate)
		}
		out = append(out, candidate)
	}
	if len(out) < 8 {
		t.Fatalf("only %d selectable types; the sweep looks broken", len(out))
	}
	return out
}

// The command must not silently accept an empty target: IsKnownProviderType
// normalizes "" to the default provider, which would make `/provider ""` read
// as a successful switch and then fail asynchronously as "provider name is
// empty" from the manager.
func TestProviderCommandRefusesAnEmptyTarget(t *testing.T) {
	r := newTestRegistry()
	ctx := &Context{Provider: "deepseek"}

	for _, target := range []string{"", "   "} {
		result := r.Execute(ctx, "provider", []string{target})
		if result.Action == ActionSwitchProvider {
			t.Errorf("/provider %q switched to nothing", target)
		}
		if result.Error == "" || !strings.Contains(result.Error, "provider") {
			t.Errorf("/provider %q = %#v, want an error naming the problem", target, result)
		}
	}
}
