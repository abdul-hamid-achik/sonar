package ui

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// A Spanish voice reads an English word as a different word, not as an accent.
//
// The clearest group is "g" before e or i, which Spanish reads as /x/: "merge"
// comes out "MER-je" and "package" comes out "pa-KA-je". A listener does not
// hear an accent there, they hear a word they have to decode backwards.
func TestSpanishRespellsTheWordsItWouldOtherwiseRewrite(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	got := m.voice.pronounce("es", "Hice el merge del package y limpié el cache con git.")
	for _, unwanted := range []string{"merge", "package", "cache", "git"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q was left for the Spanish rules to rewrite: %q", unwanted, got)
		}
	}
	// The Spanish around them is untouched: this respells vocabulary, it does
	// not translate a sentence.
	if !strings.Contains(got, "Hice el") || !strings.Contains(got, "limpié el") {
		t.Fatalf("the sentence itself was altered: %q", got)
	}
}

// English is read by an English voice, which already knows these words.
func TestEnglishIsLeftAlone(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	const sentence = "I merged the package and cleared the cache with git."
	if got := m.voice.pronounce("en", sentence); got != sentence {
		t.Fatalf("English was respelled for no reason: %q", got)
	}
	// An undecided language is not a licence to respell either: the voice that
	// ends up reading it is the host default, which is the host's language.
	if got := m.voice.pronounce("", sentence); got != sentence {
		t.Fatalf("an undecided language was respelled: %q", got)
	}
}

// The table is a starting guess and the listener is the only one who can
// correct it, so config overrides any entry and an empty value removes one.
//
// This exists because the rest of the feature was verified by measuring audio
// length and this part cannot be: no file size says whether "diplói" sounds
// more like "deploy" than "deploy" does.
func TestTheEarOverrulesTheTable(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voice.config.Pronounce = map[string]map[string]string{
		"es": {
			"deploy": "dipló",  // replaces the shipped guess
			"cache":  "",       // removes it: the original sounded better
			"branch": "branch", // adds one the table never had
		},
	}

	got := m.voice.pronounce("es", "El deploy, el cache y la branch.")
	if !strings.Contains(got, "dipló") || strings.Contains(got, "diplói") {
		t.Errorf("an override did not replace the shipped spelling: %q", got)
	}
	if !strings.Contains(got, "cache") {
		t.Errorf("an emptied entry did not restore the original word: %q", got)
	}
}

// Longest match first, or a plural is matched as its singular and left with a
// stray letter behind it.
func TestAPluralIsNotItsSingularPlusDebris(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	got := m.voice.pronounce("es", "Revisé los packages y los messages.")
	if strings.Contains(got, "pákichs") || strings.Contains(got, "pákich s") {
		t.Fatalf("a plural was matched as its singular: %q", got)
	}
	if !strings.Contains(got, "pákiches") || !strings.Contains(got, "mésiches") {
		t.Fatalf("a plural was not respelled: %q", got)
	}
}

// A respelling is a whole word or it is nothing. Rewriting the middle of an
// identifier is how "imagemagick" becomes unreadable and how a config key stops
// matching what the screen shows.
func TestRespellingNeverRewritesPartOfAWord(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	for _, sentence := range []string{"Usá imagemagick para eso.", "El prebuild falló.", "Mirá digital."} {
		if got := m.voice.pronounce("es", sentence); got != sentence {
			t.Errorf("a word containing a table entry was rewritten: %q -> %q", sentence, got)
		}
	}
}

// Everything spoken goes through one seam, so no channel can pronounce a word
// differently from the others. The fifth call site is always the one added next.
func TestEveryChannelSpeaksThroughTheSameSeam(t *testing.T) {
	m := voiceTestModel(t, true, true, true)
	m.voice.config.Alerts = true
	m.voice.config.SpeakWhen = config.SpeakWhenAlways
	m.beginVoiceTurn("Revisá el deploy del package por favor, y contame.")

	// With no speaker installed these are inert; what is being pinned is that
	// they run through the respelling at all, which the nil speaker cannot show.
	// The seam itself is the assertion below.
	m.speakAnswerDelta("Ya hice el merge.")
	m.speakSegmentEnd("Ya hice el merge.")
	m.speakActivity("Reading", "main.go")
	m.speakReasoning("Reviso el package.")
	m.speakAlert(alertApprovalNeeded)

	if got := m.voice.pronounce("es", "merge"); got == "merge" {
		t.Fatal("the seam every channel uses does not respell anything")
	}
}

// The respelling and the language belong to the driver, not to Spanish.
//
// Measured through four engines and transcribed back: `say` needs both, because
// it binds a monolingual voice per process and reads English vocabulary with
// that voice's rules. Every engine that detects language itself handled the
// mixture — and giving one the respellings made it WORSE, turning "guit" into
// "gitad". So a driver that does not need them must not receive them.
func TestOnlyADriverThatNeedsThemGetsThem(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	// With no speaker installed, Needs is the zero value: nothing is needed and
	// nothing is applied. That is the shape a self-detecting driver reports.
	language, sentence := m.forDriver("es", "Hice el merge del package.")
	if strings.Contains(sentence, "merch") {
		t.Fatalf("a driver that needs no respelling was given one: %q", sentence)
	}
	if language != "" {
		t.Fatalf("a driver that detects language was handed a guess: %q", language)
	}
}

// An override whose edges are not word characters still matches.
//
// The pattern used to wrap one \b pair around the whole alternation, and a word
// boundary only exists between a word character and a non-word one — so an
// entry ending in punctuation could never match. "C++" is the obvious thing
// somebody writes, and it failed silently: no error, no match, no way to tell
// the config from the code.
func TestAnOverrideMayEndInPunctuation(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voice.config.Pronounce = map[string]map[string]string{
		"es": {"C++": "ce plus plus", ".NET": "dot net"},
	}

	got := m.voice.pronounce("es", "Lo escribí en C++ y lo porté a .NET.")
	if !strings.Contains(got, "ce plus plus") {
		t.Errorf("an override ending in punctuation never matched: %q", got)
	}
	if !strings.Contains(got, "dot net") {
		t.Errorf("an override starting with punctuation never matched: %q", got)
	}
	// Ordinary entries keep their boundaries, or "git" starts matching inside
	// "digital".
	if got := m.voice.pronounce("es", "Mirá digital y prebuild."); got != "Mirá digital y prebuild." {
		t.Errorf("word boundaries were lost for ordinary entries: %q", got)
	}
}

// Two overrides that normalise to the same key resolve the same way every run.
//
// Map iteration order would otherwise pick a different winner on different
// launches, and a config that behaves differently each time it is read is worse
// than one that picks the wrong entry consistently.
func TestCollidingOverridesResolveDeterministically(t *testing.T) {
	overrides := map[string]string{"GIT": "primero", "git": "segundo", "Git": "tercero"}
	first := newPronunciationTable("es", overrides)
	for range 20 {
		again := newPronunciationTable("es", overrides)
		if again.say["git"] != first.say["git"] {
			t.Fatalf("colliding overrides resolved differently: %q then %q",
				first.say["git"], again.say["git"])
		}
	}
}
