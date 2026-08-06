package speech

import (
	"os"
	"strings"
	"sync"
)

// Choosing a voice for a language.
//
// A synthesizer voice is built for one language and mangles every other one. An
// English voice reading "encontré el problema" lands somewhere between an accent
// and a different sentence, and a bilingual session hits that on every second
// answer. So the caller says which language a sentence is in, and this file
// answers with a voice the host actually has.
//
// The host is asked rather than assumed. `say -v '?'` lists what is installed,
// which varies with the macOS version and with whatever the user downloaded —
// hard-coding "Mónica" ships a name that is missing on a machine that never
// installed Spanish, and `say` then fails for every sentence rather than
// falling back.

// hostVoices caches the answer. The list costs a subprocess and does not change
// while the process runs.
var hostVoices = sync.OnceValue(listHostVoices)

type hostVoice struct {
	name   string
	locale string // "es_MX"
}

// VoiceForLanguage returns a voice name for an ISO language code, or "" when the
// host has none — in which case the caller must let `say` use its default rather
// than passing a name that does not exist.
//
// configured wins outright. Someone who named a voice chose it, and a language
// detector second-guessing that choice is the harness overriding a preference it
// was given explicitly.
func VoiceForLanguage(language, configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	language = strings.TrimSpace(strings.ToLower(language))
	if language == "" {
		return ""
	}
	if language == hostLanguage() {
		// The host's own language already has an answer: whatever the user
		// picked in system settings. Naming one from the list would override a
		// deliberate choice with an alphabetical accident — on this machine that
		// accident is "Albert", a novelty voice nobody would choose.
		return ""
	}
	var fallback string
	for _, voice := range hostVoices() {
		if !strings.HasPrefix(strings.ToLower(voice.locale), language) {
			continue
		}
		// Parenthesised names are the multi-locale novelty set macOS ships
		// ("Grandma (Spanish (Spain))", "Rocko", "Flo"). They sort first and are
		// nobody's choice for reading prose, so they are taken only if the
		// language has nothing else.
		if strings.ContainsRune(voice.name, '(') {
			if fallback == "" {
				fallback = voice.name
			}
			continue
		}
		// Prefer the region the host is set to. A Mexican machine reading
		// Spanish in a Castilian voice is understood and still wrong, and the
		// region is the one part of this the host already knows.
		if region := hostRegion(); region != "" && strings.HasSuffix(voice.locale, "_"+region) {
			return voice.name
		}
		if fallback == "" || strings.ContainsRune(fallback, '(') {
			fallback = voice.name
		}
	}
	return fallback
}

// hostLanguage is the language half of the host locale, e.g. "en".
var hostLanguage = sync.OnceValue(func() string {
	language, _, _ := strings.Cut(hostLocale(), "_")
	return strings.ToLower(language)
})

// hostRegion is the territory from the host locale, e.g. "MX". Empty when the
// environment says nothing, which is normal in a CI container.
var hostRegion = sync.OnceValue(func() string {
	_, region, _ := strings.Cut(hostLocale(), "_")
	return strings.ToUpper(region)
})

// hostLocale is the first locale the environment names, stripped of its
// encoding and modifier suffixes.
var hostLocale = sync.OnceValue(func() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		if cut := strings.IndexAny(value, ".@"); cut >= 0 {
			value = value[:cut]
		}
		if strings.ContainsRune(value, '_') {
			return value
		}
	}
	return ""
})

// looksLikeLocale matches "es_MX" and rejects the words around it.
func looksLikeLocale(field string) bool {
	language, region, found := strings.Cut(field, "_")
	if !found || len(language) < 2 || len(language) > 3 || len(region) < 2 {
		return false
	}
	for _, r := range language {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	for _, r := range region {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
