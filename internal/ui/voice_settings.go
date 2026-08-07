package ui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// Tuning spoken output from the session rather than from a file.
//
// The panel already said why the channel toggles exist: a setting you change by
// editing a file and restarting is a setting nobody tunes. Everything else about
// voice had exactly that problem, and it is worse for these than for the
// channels — the voice, the rate, the respellings and the synthesizer itself are
// all settings you can only judge BY EAR, so the loop has to be short enough to
// close in one sitting. `/voice test`, listen, adjust, listen again.
//
// # Nothing here is persisted, and that is deliberate
//
// runtimepref already stores the model, the provider and the theme, so the
// precedent exists. It is not used, because these two things are different: the
// session is where you find out what sounds right, and the config file is where
// a decision is written down. Persisting a mid-experiment state would make the
// next launch inherit a setting nobody chose.
//
// What closes that loop instead is `/voice status`, which prints the current
// values as a YAML block you can paste. The experiment stays an experiment
// until you decide it is a preference.
//
// # Some of these rebuild the synthesizer
//
// A driver reads the voice, the rate and the provider when it is constructed —
// `say` binds them into a subprocess's arguments — so changing one means opening
// a new speaker. The turn's spoken position and language survive it: they belong
// to the conversation, not to the device.

// setVoiceSetting applies one setting, named by its config key.
//
// The names are the config keys rather than shorter aliases, so that what you
// type here and what you would write in the file are the same word. Anything
// else means learning the surface twice.
func (m *Model) setVoiceSetting(request string) tea.Cmd {
	name, value, _ := strings.Cut(strings.TrimSpace(request), " ")
	value = strings.TrimSpace(value)
	if !m.voiceActive() {
		return m.setFooterNotice(noticeInfo,
			"Spoken output is off. Run /voice on first.", 4*time.Second)
	}
	switch strings.ToLower(name) {
	case "provider":
		return m.setVoiceProvider(value)
	case "speak_when", "speakwhen":
		return m.setVoiceSpeakWhen(value)
	case "rate":
		return m.setVoiceRate(value)
	case "voice":
		return m.setVoiceName(value)
	case "pronounce":
		return m.setVoicePronunciation(value)
	default:
		return m.setFooterNotice(noticeWarning,
			"Unknown voice setting "+name+". Try: provider, speak_when, rate, voice, pronounce.", 6*time.Second)
	}
}

func (m *Model) setVoiceProvider(value string) tea.Cmd {
	if value == "" {
		return m.appendVoiceReport("Usage: /voice provider say|openai")
	}
	previous := m.voice.config.Provider
	m.voice.config.Provider = value
	if notice, ok := m.reopenVoice(); !ok {
		// Put it back. A provider that could not be built is not the session's
		// provider, and leaving the name set would make /voice status describe a
		// synthesizer that is not the one speaking.
		m.voice.config.Provider = previous
		return m.setFooterNotice(noticeWarning, notice, 8*time.Second)
	}
	return m.setFooterNotice(noticeInfo, "Voice provider: "+m.effectiveProvider()+".", 4*time.Second)
}

func (m *Model) setVoiceSpeakWhen(value string) tea.Cmd {
	switch strings.ToLower(value) {
	case config.SpeakWhenAlways, config.SpeakWhenUnfocused:
		m.voice.config.SpeakWhen = strings.ToLower(value)
	default:
		return m.appendVoiceReport("Usage: /voice speak_when always|unfocused")
	}
	if m.voice.config.SpeaksWhileFocused() {
		return m.setFooterNotice(noticeInfo, "Speaking whether or not this window has focus.", 4*time.Second)
	}
	if !m.terminalFocusReported {
		return m.setFooterNotice(noticeWarning,
			"Holding speech back while focused — but this terminal has not reported focus, so nothing is held back yet.", 8*time.Second)
	}
	return m.setFooterNotice(noticeInfo, "Speaking only while this window is in the background.", 4*time.Second)
}

func (m *Model) setVoiceRate(value string) tea.Cmd {
	rate, err := strconv.Atoi(value)
	if err != nil || rate < voiceMinimumRate || rate > voiceMaximumRate {
		return m.appendVoiceReport(fmt.Sprintf(
			"Usage: /voice rate <%d-%d>  (words per minute; the default is 210, faster than the system's)",
			voiceMinimumRate, voiceMaximumRate))
	}
	previous := m.voice.config.Rate
	m.voice.config.Rate = rate
	if notice, ok := m.reopenVoice(); !ok {
		m.voice.config.Rate = previous
		return m.setFooterNotice(noticeWarning, notice, 8*time.Second)
	}
	return m.setFooterNotice(noticeInfo,
		fmt.Sprintf("Speaking at %d words per minute. /voice test to hear it.", rate), 5*time.Second)
}

// voiceMinimumRate and voiceMaximumRate bound what is worth trying rather than
// what `say` accepts. Below the floor a sentence outlasts the work it describes;
// above the ceiling the projection's whole argument — that a listener can follow
// this — stops being true.
const (
	voiceMinimumRate = 80
	voiceMaximumRate = 400
)

// setVoiceName sets the voice for everything, or for one language.
//
//	/voice voice Paulina        — this voice for every language
//	/voice voice es Paulina     — this voice for Spanish only
//	/voice voice es             — forget the Spanish entry
func (m *Model) setVoiceName(value string) tea.Cmd {
	if value == "" {
		return m.appendVoiceReport(
			"Usage: /voice voice <name>  |  /voice voice <language> <name>  |  /voice voice <language>\n" +
				"  /voice voices lists what each language would use; /voice test speaks them.")
	}
	first, rest, hasRest := strings.Cut(value, " ")
	previousVoice, previousVoices := m.voice.config.Voice, m.voice.config.Voices
	switch {
	case isVoiceLanguage(first):
		// Copied rather than mutated: the map came from configuration, and a
		// command that edited it in place would change what a failed rebuild
		// has to restore.
		voices := map[string]string{}
		for language, name := range m.voice.config.Voices {
			voices[language] = name
		}
		if hasRest && strings.TrimSpace(rest) != "" {
			voices[first] = strings.TrimSpace(rest)
		} else {
			delete(voices, first)
		}
		m.voice.config.Voices = voices
	default:
		m.voice.config.Voice = value
	}
	if notice, ok := m.reopenVoice(); !ok {
		m.voice.config.Voice, m.voice.config.Voices = previousVoice, previousVoices
		return m.setFooterNotice(noticeWarning, notice, 8*time.Second)
	}
	// The tables are compiled per language and the voice does not change what
	// they say, but a language whose voice changed deserves a fresh look at
	// which respellings apply to it.
	m.voice.tables = nil
	return m.reportVoiceVoices()
}

// isVoiceLanguage reports whether a token is a language code rather than a
// voice name. Voice names are words; language codes are the two-letter tags the
// detector produces plus whatever the operator configured.
func isVoiceLanguage(token string) bool {
	if len(token) != 2 {
		return false
	}
	for _, r := range token {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// setVoicePronunciation adds, replaces or removes one respelling.
//
//	/voice pronounce deploy diplói   — say it this way
//	/voice pronounce deploy          — forget it, use the word as written
//
// This is the setting the whole table was built to make correctable. Every
// entry in it is a guess nobody could verify by measurement — whether "diplói"
// sounds more like "deploy" than "deploy" does is a judgement no file size
// reports — so the loop from hearing one to fixing it has to be one command.
func (m *Model) setVoicePronunciation(value string) tea.Cmd {
	word, spelling, _ := strings.Cut(strings.TrimSpace(value), " ")
	if word == "" {
		return m.appendVoiceReport(
			"Usage: /voice pronounce <word> <how to say it>  |  /voice pronounce <word>\n" +
				"  The second form removes an entry. /voice test speaks a line built from them.")
	}
	language := m.voice.spokenLanguageNow()
	if language == "" {
		language = "es"
	}
	if m.voice.config.Pronounce == nil {
		m.voice.config.Pronounce = map[string]map[string]string{}
	}
	entries := map[string]string{}
	for existing, existingSpelling := range m.voice.config.Pronounce[language] {
		entries[existing] = existingSpelling
	}
	// An empty value is how the table already spells "remove this", so the
	// command and the config file mean the same thing by the same shape.
	entries[strings.ToLower(word)] = strings.TrimSpace(spelling)
	m.voice.config.Pronounce[language] = entries
	// Compiled on next use, so the change is audible on the next sentence.
	m.voice.tables = nil

	if strings.TrimSpace(spelling) == "" {
		return m.setFooterNotice(noticeInfo,
			"\""+word+"\" will be said as written in "+language+".", 5*time.Second)
	}
	return m.setFooterNotice(noticeInfo,
		"\""+word+"\" will be said as \""+spelling+"\" in "+language+". /voice test to hear it.", 6*time.Second)
}

// reopenVoice rebuilds the speaker under the session's current settings.
//
// The conversation's state survives: the language verdict, the spoken position
// and the last digest belong to the turn rather than to the device. Only the
// synthesizer is replaced, and the old one is stopped rather than left draining
// — somebody who just changed the voice does not want to hear the previous one
// finish the sentence.
func (m *Model) reopenVoice() (string, bool) {
	if !m.voiceActive() {
		return "", false
	}
	previous := m.voice
	// Policy-only state: tests (and any host that installed channel policy
	// without a process) have no synthesizer to rebuild. The config mutation
	// already took; rolling it back because Available() is false would make
	// every setting command fail on a machine without say, and would make the
	// nil-speaker fixture contradict the claim that the policy can be
	// exercised without claiming an audio device.
	if previous.speaker == nil {
		return "", true
	}
	previous.speaker.Close()
	m.voice = nil
	m.voiceConfig = previous.config
	notice, ok := m.openVoice()
	if !ok {
		// Restore the state so the session keeps speaking with what it had.
		m.voice = previous
		if _, reopened := m.openVoice(); !reopened {
			m.voice = previous
		}
		return notice, false
	}
	m.voice.spoken, m.voice.answerLen = previous.spoken, previous.answerLen
	m.voice.language, m.voice.languageKnown = previous.language, previous.languageKnown
	m.voice.seed, m.voice.lastActivity = previous.seed, previous.lastActivity
	m.voice.digestSpoken = previous.digestSpoken
	return "", true
}

// effectiveProvider names the synthesizer actually in use.
func (m *Model) effectiveProvider() string {
	if !m.voiceActive() {
		return "none"
	}
	if provider := strings.TrimSpace(m.voice.config.Provider); provider != "" {
		return strings.ToLower(provider)
	}
	return "say"
}

// voiceSettingsYAML renders the session's settings as the config block that
// would reproduce them.
//
// This is what stands in for persistence. Tuning by ear produces a state nobody
// should inherit by accident on the next launch — so the session stays a
// session, and the way to keep a result is to paste the decision into the file
// where decisions live.
func (m *Model) voiceSettingsYAML() string {
	if !m.voiceActive() {
		return ""
	}
	cfg := m.voice.config
	var block strings.Builder
	block.WriteString("  To keep this, put it in your config:\n\n")
	block.WriteString("    voice:\n      enabled: true\n")
	for _, channel := range voiceChannels {
		fmt.Fprintf(&block, "      %s: %t\n", channel.name, channel.get(m.voice))
	}
	if provider := m.effectiveProvider(); provider != "say" {
		fmt.Fprintf(&block, "      provider: %s\n", provider)
	}
	if !cfg.SpeaksWhileFocused() {
		fmt.Fprintf(&block, "      speak_when: %s\n", config.SpeakWhenUnfocused)
	}
	if cfg.Rate > 0 {
		fmt.Fprintf(&block, "      rate: %d\n", cfg.Rate)
	}
	if voice := strings.TrimSpace(cfg.Voice); voice != "" {
		fmt.Fprintf(&block, "      voice: %s\n", voice)
	}
	if len(cfg.Voices) > 0 {
		block.WriteString("      voices:\n")
		for _, language := range sortedKeysOf(cfg.Voices) {
			fmt.Fprintf(&block, "        %s: %s\n", language, cfg.Voices[language])
		}
	}
	for _, language := range sortedLanguagesOf(cfg.Pronounce) {
		entries := cfg.Pronounce[language]
		if len(entries) == 0 {
			continue
		}
		if !strings.Contains(block.String(), "pronounce:") {
			block.WriteString("      pronounce:\n")
		}
		fmt.Fprintf(&block, "        %s:\n", language)
		for _, word := range sortedKeysOf(entries) {
			fmt.Fprintf(&block, "          %s: %q\n", word, entries[word])
		}
	}
	return block.String()
}

func sortedKeysOf(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedLanguagesOf(values map[string]map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
