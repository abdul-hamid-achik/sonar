package speech

import "testing"

// The listing is parsed by finding the field that looks like a locale, not by
// counting columns, because voice names contain spaces and parentheses:
// "Grandma (Spanish (Spain))" is one name.
func TestVoiceListingSurvivesNamesWithSpaces(t *testing.T) {
	for _, testCase := range []struct {
		field string
		want  bool
	}{
		{"es_MX", true},
		{"en_US", true},
		{"zh_CN", true},
		{"(Spanish", false},
		{"Grandma", false},
		{"", false},
		{"es_", false},
		{"ES_MX", false},
	} {
		if got := looksLikeLocale(testCase.field); got != testCase.want {
			t.Errorf("looksLikeLocale(%q) = %v, want %v", testCase.field, got, testCase.want)
		}
	}
}

// A configured voice wins outright. Someone who named one chose it, and a
// language detector second-guessing that is the harness overriding a preference
// it was handed explicitly.
func TestConfiguredVoiceOutranksDetection(t *testing.T) {
	if got := VoiceForLanguage("es", "Paulina"); got != "Paulina" {
		t.Fatalf("configured voice was overridden: %q", got)
	}
	// Even for a language the host has nothing for.
	if got := VoiceForLanguage("zz", "Paulina"); got != "Paulina" {
		t.Fatalf("configured voice was dropped for an unknown language: %q", got)
	}
}

// A language the host cannot speak gets no name at all, so the caller lets the
// synthesizer use its default. Passing a voice that is not installed makes
// `say` fail on every sentence instead of falling back.
func TestUnknownLanguageNamesNoVoice(t *testing.T) {
	if got := VoiceForLanguage("zz", ""); got != "" {
		t.Fatalf("an unavailable language named a voice: %q", got)
	}
	if got := VoiceForLanguage("", ""); got != "" {
		t.Fatalf("an empty language named a voice: %q", got)
	}
}

// The host's own language is left to the system default, which is a choice the
// user already made in settings. Picking from the list instead would override it
// with an alphabetical accident — on a US machine that accident is "Albert", a
// novelty voice nobody would choose to be read prose by.
func TestHostLanguageDefersToTheSystemDefault(t *testing.T) {
	if language := hostLanguage(); language != "" {
		if got := VoiceForLanguage(language, ""); got != "" {
			t.Fatalf("the host's own language named %q instead of deferring", got)
		}
	}
}
