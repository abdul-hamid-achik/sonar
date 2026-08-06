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
	if preferred := preferredVoice(language); preferred != "" {
		return preferred
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

// standardVoices are the speech voices macOS ships per language, in preference
// order, ahead of the joke voices it ships beside them.
//
// A list of names is the last thing this file wanted, and it is here because
// the host offers nothing else to go on. `say -v ?` reports a name and a locale
// and no notion of quality, so "Albert", "Zarvox", "Bahh" and "Bubbles" are
// indistinguishable from "Samantha" — and they sort first, so the alphabetical
// fallback reads every English answer in a novelty voice.
//
// The obvious data signal was tested and does not exist: the example sentence
// beside each voice looked like it might separate the two families, and macOS
// normalised all of them to "Hello! My name is X." years ago. Zarvox and
// Samantha introduce themselves identically.
//
// So this is a preference, not a requirement: every entry is checked against
// what the host actually has, an absent name is skipped, and a language that
// matches nothing falls through to the generic rule below. Adding a language
// here improves a default; leaving one out costs nothing but a worse guess.
var standardVoices = map[string][]string{
	"en": {"Samantha", "Alex", "Ava", "Allison", "Daniel", "Karen", "Moira"},
	"es": {"Paulina", "Mónica", "Jorge", "Marisol", "Juan"},
	"fr": {"Thomas", "Amélie", "Audrey"},
	"de": {"Anna", "Markus", "Petra"},
	"it": {"Alice", "Luca"},
	"pt": {"Luciana", "Joana"},
	"nl": {"Xander", "Ellen"},
	"ja": {"Kyoko", "Otoya"},
	"ko": {"Yuna"},
	"zh": {"Ting-Ting", "Sin-ji", "Mei-Jia"},
	"ru": {"Milena", "Yuri"},
}

// preferredVoice returns the first standard voice for a language that this host
// actually has installed, or "" to fall through to the generic rule.
func preferredVoice(language string) string {
	candidates := standardVoices[language]
	if len(candidates) == 0 {
		return ""
	}
	installed := hostVoices()
	region := hostRegion()
	// Two passes so a region match wins over list order: on a Mexican machine
	// Paulina should beat Mónica even if the list were the other way round.
	for _, matchRegion := range []bool{true, false} {
		if matchRegion && region == "" {
			continue
		}
		for _, candidate := range candidates {
			for _, voice := range installed {
				if voice.name != candidate {
					continue
				}
				if matchRegion && !strings.HasSuffix(voice.locale, "_"+region) {
					continue
				}
				return voice.name
			}
		}
	}
	return ""
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
