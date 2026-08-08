package speech

import "testing"

// The predicate is what lets the UI gate ask "which probe does this provider
// need" instead of probing the host unconditionally — the unconditional probe
// is how `voice.provider: openai` was refused on every non-Darwin host while
// NewWithProvider two layers down would have accepted it.
func TestProviderNeedsHostSeparatesHostedFromHostDrivers(t *testing.T) {
	for provider, wantsHost := range map[string]bool{
		"": true, "say": true, "host": true, "local": true, " SAY ": true,
		"openai": false, "OpenAI": false,
		// Unknown names answer false so they reach NewWithProvider's
		// informative error rather than a generic "no synthesizer".
		"mystery": false,
	} {
		if got := ProviderNeedsHost(provider); got != wantsHost {
			t.Errorf("ProviderNeedsHost(%q) = %v, want %v", provider, got, wantsHost)
		}
	}
}
