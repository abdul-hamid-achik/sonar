package ui

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// Every setting is reachable by the name it has in the config file.
//
// Not a shorter alias: what you type while tuning and what you would write down
// afterwards should be the same word, or the surface has to be learned twice.
func TestEverySpokenSettingIsReachableByItsConfigName(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	m.setVoiceSetting("speak_when unfocused")
	if m.voice.config.SpeaksWhileFocused() {
		t.Error("speak_when did not take")
	}
	m.setVoiceSetting("speak_when always")
	if !m.voice.config.SpeaksWhileFocused() {
		t.Error("speak_when could not be set back")
	}

	// A value outside the bounds is refused rather than clamped: a rate nobody
	// can follow is not a smaller version of a rate they can.
	before := m.voice.config.Rate
	m.setVoiceSetting("rate 9000")
	if m.voice.config.Rate != before {
		t.Errorf("an out-of-range rate was accepted: %d", m.voice.config.Rate)
	}
	m.setVoiceSetting("rate not-a-number")
	if m.voice.config.Rate != before {
		t.Errorf("a non-numeric rate was accepted: %d", m.voice.config.Rate)
	}

	// An unknown setting names the ones that exist rather than failing silently.
	if cmd := m.setVoiceSetting("velocidad 200"); cmd == nil {
		t.Error("an unknown setting said nothing")
	}
}

// A respelling can be added and removed in one command each, because the whole
// table is guesses that only an ear can correct.
func TestARespellingIsOneCommandToFixAndOneToUndo(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.beginVoiceTurn("Revisá el deploy y contame qué encontraste, por favor.")

	m.setVoiceSetting("pronounce deploy dipló")
	if got := m.voice.pronounce("es", "El deploy quedó."); !strings.Contains(got, "dipló") {
		t.Fatalf("a new respelling did not take: %q", got)
	}

	// Removing it restores the word as written, which is what an empty value
	// means in the config file too.
	m.setVoiceSetting("pronounce deploy")
	if got := m.voice.pronounce("es", "El deploy quedó."); !strings.Contains(got, "deploy") {
		t.Fatalf("removing a respelling did not restore the word: %q", got)
	}
}

// A voice can be set for everything or for one language, and the language form
// is told apart from the name form by shape rather than by a flag.
func TestAVoiceCanBeSetForOneLanguageOrForAll(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	m.setVoiceSetting("voice es Paulina")
	if m.voice.config.Voices["es"] != "Paulina" {
		t.Fatalf("a per-language voice did not take: %+v", m.voice.config.Voices)
	}
	m.setVoiceSetting("voice es")
	if _, present := m.voice.config.Voices["es"]; present {
		t.Fatalf("a per-language voice could not be removed: %+v", m.voice.config.Voices)
	}
	m.setVoiceSetting("voice Samantha")
	if m.voice.config.Voice != "Samantha" {
		t.Fatalf("a single voice did not take: %q", m.voice.config.Voice)
	}
}

// The session prints the config block that would reproduce it.
//
// This stands in for persistence on purpose: tuning by ear produces a state
// nobody should inherit by accident on the next launch, so the session stays a
// session and keeping a result means writing it down.
func TestTheSessionCanBeWrittenDown(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voice.config.SpeakWhen = config.SpeakWhenUnfocused
	m.voice.config.Rate = 195
	m.voice.config.Voices = map[string]string{"es": "Paulina"}
	m.voice.config.Pronounce = map[string]map[string]string{"es": {"deploy": "dipló"}}

	block := m.voiceSettingsYAML()
	for _, want := range []string{
		"voice:", "enabled: true", "speak_when: unfocused", "rate: 195",
		"voices:", "es: Paulina", "pronounce:", `deploy: "dipló"`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the config block is missing %q:\n%s", want, block)
		}
	}
}

// A profile is a named mix, not a hidden fifth channel: it writes exactly the
// toggles /voice status reports, and it persists nothing.
func TestVoiceProfilePresetsSetTheMixWithoutPersisting(t *testing.T) {
	m := newTestModel(t)
	m.voice = &voiceState{config: config.VoiceConfig{Enabled: true}}

	m.setVoiceSetting("profile walkaway")
	cfg := m.voice.config
	if !cfg.Answer || !cfg.Alerts || !cfg.Activity || cfg.Reasoning {
		t.Fatalf("walkaway mix = %+v, want answer+alerts+activity on, reasoning off", cfg)
	}
	if !cfg.SpeaksWhileFocused() {
		t.Fatal("walkaway should speak regardless of focus")
	}

	m.setVoiceSetting("profile desk")
	cfg = m.voice.config
	if cfg.Activity {
		t.Fatal("desk keeps activity off")
	}
	if cfg.SpeaksWhileFocused() {
		t.Fatal("desk holds speech back while focused")
	}

	entriesBefore := len(m.entries)
	m.setVoiceSetting("profile night")
	if m.voice.config.Activity || m.voice.config.SpeaksWhileFocused() {
		t.Fatal("an unknown profile must not change the mix")
	}
	if len(m.entries) == entriesBefore {
		t.Fatal("an unknown profile should answer with usage, not silence")
	}
}
