package ui

import "testing"

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

	m.speakTurnEnd("Encontré el problema en el canal.")
	if m.voice.languageKnown || m.voice.language != "" {
		t.Fatalf("the turn boundary kept a stale language: %q", m.voice.language)
	}
}
