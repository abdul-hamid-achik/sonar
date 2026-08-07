package ui

import (
	"strings"
	"testing"
	"unicode"
)

// The detector is measured, not asserted. These are answers this project
// actually produces, in both languages a bilingual session mixes.
//
// "" is a verdict too, and the right one for text with nothing to go on: the
// caller falls back to the configured voice rather than flipping languages on
// the strength of one word.
func TestSpokenLanguageReadsWhatIsThere(t *testing.T) {
	for _, testCase := range []struct{ text, want string }{
		{"Encontré el problema en el canal de respuesta.", "es"},
		{"Revisá la configuración y corré los tests.", "es"},
		{"Listo, ya quedó todo verde.", "es"},
		{"El bug estaba en spokenText, que proyectaba mal.", "es"},
		{"Corré go test y verificá que pase.", "es"},
		{"Se arregló, pero hay que medirlo.", "es"},
		{"I found the problem in the answer channel.", "en"},
		{"The fix landed and the tests are green.", "en"},
		{"This is a streaming answer with code in it.", "en"},
		{"It works, but we should measure it.", "en"},
		// Nothing to go on. Silence beats a coin flip, because a wrong verdict
		// here reads a whole answer in the wrong language.
		{"Done.", ""},
		{"OK.", ""},
		{"go test ./...", ""},
		{"", ""},
	} {
		if got := spokenLanguage(testCase.text); got != testCase.want {
			t.Errorf("spokenLanguage(%q) = %q, want %q", testCase.text, got, testCase.want)
		}
	}
}

// A single marker is not evidence. "no" and "a" are words in both languages,
// and Spanish prose quotes English identifiers constantly — a threshold of one
// would flip the voice mid-answer on a variable name.
func TestSpokenLanguageNeedsMoreThanOneWord(t *testing.T) {
	if got := spokenLanguage("the"); got != "" {
		t.Fatalf("one English marker decided a language: %q", got)
	}
	if got := spokenLanguage("Usá the flag."); got != "" {
		t.Fatalf("a quoted English word outvoted a Spanish sentence: %q", got)
	}
}

// An accent or an inverted mark is worth a function word, because short answers
// often carry nothing else. "Sí." has one letter of evidence and it is decisive.
func TestSpokenLanguageCountsSpanishCharacters(t *testing.T) {
	for _, text := range []string{"Sí.", "¿Qué?", "Año."} {
		if got := spokenLanguage(text); got != "es" {
			t.Errorf("spokenLanguage(%q) = %q, want es", text, got)
		}
	}
}

// Technical prose is thin in function words, so the common short ones are where
// the evidence has to come from.
//
// Each of these came back undecided against the real detector — one marker
// short of a verdict — which meant the voice fell through to whatever the
// previous turn had seeded rather than to what the sentence plainly says.
func TestTheDetectorReadsRealTechnicalProse(t *testing.T) {
	for _, testCase := range []struct{ text, want string }{
		{"Hay 3 tests fallando en TestSpokenLanguage, TestVoiceAnswer y TestAlerts.", "es"},
		{"Check your DEEPSEEK_API_KEY env var, e.g. in env.", "en"},
		{"Lo mismo se puede hacer otra vez al final.", "es"},
		{"These should not be in the list if they do not appear.", "en"},
	} {
		if got := spokenLanguage(testCase.text); got != testCase.want {
			t.Errorf("spokenLanguage(%q) = %q, want %q", testCase.text, got, testCase.want)
		}
	}
	// A marker only earns its place by not existing in the other language, and a
	// marker with a character no tokenizer can produce earns nothing at all —
	// "está_" sat in the Spanish list unreachable, because the tokenizer splits
	// on everything that is not a letter.
	for word := range spokenSpanishMarkers {
		if strings.ContainsFunc(word, func(r rune) bool { return !unicode.IsLetter(r) }) {
			t.Errorf("Spanish marker %q can never be produced by the tokenizer", word)
		}
		if spokenEnglishMarkers[word] {
			t.Errorf("marker %q is in both lists, so it moves no verdict", word)
		}
	}
	for word := range spokenEnglishMarkers {
		if strings.ContainsFunc(word, func(r rune) bool { return !unicode.IsLetter(r) }) {
			t.Errorf("English marker %q can never be produced by the tokenizer", word)
		}
	}
}

// The turn decides once. The synthesizer only reads the language when it starts
// a process, so re-deciding per sentence would rescan the whole answer to change
// nothing — but a verdict of "" must NOT latch, or an answer opening with
// "Listo." would pin the rest of the turn to the default voice.
func TestTurnLanguageLatchesOnlyOnceDecided(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	if got := m.turnLanguage("Done."); got != "" {
		t.Fatalf("thin evidence produced a verdict: %q", got)
	}
	if m.voice.languageKnown {
		t.Fatal("an undecided language latched")
	}

	if got := m.turnLanguage("Encontré el problema en el canal."); got != "es" {
		t.Fatalf("a decidable answer was not decided: %q", got)
	}
	// Later English text must not move it: the process is already running with
	// a Spanish voice and switching would cut a sentence in half.
	if got := m.turnLanguage("The fix landed and the tests are green."); got != "es" {
		t.Fatalf("the turn's language changed mid-flight: %q", got)
	}

	// A provider segment ending must NOT clear it. The loop ends one at every
	// tool round, and the segments that follow are usually short and carry no
	// function words to decide on — which is exactly how a Spanish answer ended
	// up being read out in English halfway through.
	m.speakSegmentEnd("Encontré el problema en el canal.")
	if !m.voice.languageKnown || m.voice.language != "es" {
		t.Fatalf("a segment boundary dropped the turn's language: %q", m.voice.language)
	}

	// The user's next message is the boundary that decides again.
	m.beginVoiceTurn("Now check whether the tests are green for that change.")
	if m.voice.languageKnown {
		t.Fatal("a new turn kept the previous turn's verdict")
	}
	if got := m.turnLanguage("Done."); got != "en" {
		t.Fatalf("a turn with no evidence of its own ignored the prompt: %q", got)
	}
}

// The user's own message decides what the opening sentences are read in.
//
// Waiting for the answer to prove its language means the first sentences are
// read in the host default, and they are exactly the sentences with nothing to
// go on: "Listo.", "Ya está.", "Voy a revisar." decide nothing and are most of
// what a coding agent opens with. The prompt is already written by then.
func TestThePromptSeedsTheVoiceBeforeTheAnswerArrives(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	m.beginVoiceTurn("Revisá por qué el canal de voz se interrumpe a sí mismo, por favor.")
	if got := m.turnLanguage("Listo."); got != "es" {
		t.Fatalf("an undecidable opening ignored the prompt: %q", got)
	}
	if m.voice.languageKnown {
		t.Fatal("the seed latched as a verdict; only the answer may decide")
	}

	// The answer still overrules it, because the seed is a prior and not a
	// decision. Queued speech makes that switch safe: the new voice starts when
	// the current sentence ends rather than on top of it.
	if got := m.turnLanguage("The fix landed and all of the tests are green."); got != "en" {
		t.Fatalf("the answer could not overrule the seed: %q", got)
	}

	// A message with no evidence of its own keeps the session's language rather
	// than falling back to the host default: "sí", "dale" and "ok" are the
	// shortest things people send and the least diagnostic.
	m.beginVoiceTurn("dale")
	if got := m.turnLanguage("Listo."); got != "es" {
		t.Fatalf("a one-word prompt reset the session's language: %q", got)
	}
}
