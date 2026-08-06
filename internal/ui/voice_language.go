package ui

import (
	"strings"
	"unicode"
)

// Which language to read an answer in.
//
// A voice reads the language it was built for and mangles every other one: an
// English voice saying "encontré el problema" produces something between an
// accent and a different sentence. A bilingual session gets one or the other
// wrong unless the harness looks at the text.
//
// The detector is function words, not a library. Two reasons, and the first is
// the binding constraint: both harnesses ship CGO_ENABLED=0 static binaries, so
// a detection library has to be pure Go, and the pure-Go ones carry model data
// measured in megabytes to answer a question with two plausible outcomes here.
// The second is that function words are what actually separate these languages
// in short text — "de la que en un" is Spanish in a way no amount of vocabulary
// coverage improves on.
//
// It answers "" when the evidence is thin, which is the honest outcome for
// "OK." or "go test ./...", and leaves the choice to the configured default.

// spokenLanguageMinimumScore is how much evidence a verdict needs.
//
// One matched word is not evidence: "no" and "a" are words in both languages,
// and "the" appears in Spanish prose quoting an English identifier. Two
// independent signals is the smallest threshold that stops a three-word
// sentence from flipping the voice mid-answer.
const spokenLanguageMinimumScore = 2

// Function words, weighted equally. Words that exist in both languages ("no",
// "a", "en", "son") are deliberately absent from both lists: a marker that
// matches everything moves no verdict and only adds noise.
var (
	spokenSpanishMarkers = map[string]bool{
		"el": true, "la": true, "los": true, "las": true, "un": true, "una": true,
		"de": true, "del": true, "que": true, "por": true, "para": true, "con": true,
		"pero": true, "porque": true, "cuando": true, "donde": true, "esto": true,
		"eso": true, "esta": true, "este": true, "está": true, "están": true,
		"hay": true, "ser": true, "está_": true, "más": true, "muy": true,
		"también": true, "así": true, "ahora": true, "todo": true, "todos": true,
		"cada": true, "sobre": true, "entre": true, "hasta": true, "desde": true,
		"sin": true, "ya": true, "sí": true, "qué": true, "cómo": true,
	}
	spokenEnglishMarkers = map[string]bool{
		"the": true, "of": true, "and": true, "to": true, "is": true, "it": true,
		"that": true, "this": true, "with": true, "for": true, "was": true,
		"are": true, "be": true, "have": true, "has": true, "from": true,
		"they": true, "you": true, "what": true, "which": true, "there": true,
		"their": true, "would": true, "will": true, "can": true, "but": true,
		"because": true, "when": true, "where": true, "more": true, "also": true,
		"now": true, "each": true, "about": true, "between": true, "until": true,
		"since": true, "without": true, "already": true, "how": true,
	}
)

// spokenLanguage reports "es", "en", or "" when the text does not say.
func spokenLanguage(text string) string {
	spanish, english := 0, 0
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r)
	}) {
		switch {
		case spokenSpanishMarkers[word]:
			spanish++
		case spokenEnglishMarkers[word]:
			english++
		}
	}
	// Orthography, in two tiers, because the two are not equally diagnostic.
	//
	// English has no ñ and no inverted marks at all, so one of those settles it
	// alone — "Año." is Spanish on the strength of a single letter, and short
	// answers usually carry nothing else to go on. Accented vowels only lean
	// that way: English borrows café, née and résumé without becoming Spanish,
	// so they count as one signal and still need a second.
	for _, r := range text {
		switch r {
		case 'ñ', 'Ñ', '¿', '¡':
			spanish += spokenLanguageMinimumScore
		case 'á', 'é', 'í', 'ó', 'ú', 'Á', 'É', 'Í', 'Ó', 'Ú':
			spanish++
		}
	}

	if spanish >= spokenLanguageMinimumScore && spanish > english {
		return "es"
	}
	if english >= spokenLanguageMinimumScore && english > spanish {
		return "en"
	}
	return ""
}
