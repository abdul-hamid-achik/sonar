package ui

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/speech"
)

// The /voice control surface.
//
// Everything here answers a question the config file cannot: which voice am I
// actually going to get, what does it sound like, and is this thing on. Picking
// a voice by editing YAML means choosing a name out of the 184 `say -v ?` lists
// with no way to hear any of them, and then restarting to find out. The two
// subcommands that matter are the ones that close that loop — `voices` says who
// would be chosen for what, and `test` says it out loud.
//
// The toggles are here for the same reason the slash command exists at all: a
// setting you change by editing a file and restarting is a setting nobody tunes,
// and the whole complaint about spoken output is that the mix is wrong for the
// moment you are in. They last for the session; the config file stays the place
// where a preference is written down.

// voiceChannels maps a name a person would type to the field it switches.
//
// A table rather than a switch so the same list drives the toggles, the status
// report, and the error message that names what is valid.
var voiceChannels = []struct {
	name string
	get  func(*voiceState) bool
	set  func(*voiceState, bool)
	help string
}{
	{
		name: "answer",
		get:  func(v *voiceState) bool { return v.config.Answer },
		set:  func(v *voiceState, on bool) { v.config.Answer = on },
		help: "the reply, sentence by sentence as it streams",
	},
	{
		name: "alerts",
		get:  func(v *voiceState) bool { return v.config.Alerts },
		set:  func(v *voiceState, on bool) { v.config.Alerts = on },
		help: "approval waiting, turn done, turn failed",
	},
	{
		name: "activity",
		get:  func(v *voiceState) bool { return v.config.Activity },
		set:  func(v *voiceState, on bool) { v.config.Activity = on },
		help: "what the harness is doing, never what it is doing it to",
	},
	{
		name: "reasoning",
		get:  func(v *voiceState) bool { return v.config.Reasoning },
		set:  func(v *voiceState, on bool) { v.config.Reasoning = on },
		help: "thinking blocks, once settled",
	},
	{
		name: "context_alert",
		get:  func(v *voiceState) bool { return v.config.ContextAlert },
		set:  func(v *voiceState, on bool) { v.config.ContextAlert = on },
		help: "the context window passing 75%, once per crossing",
	},
}

// voiceChannelReport is the "is this thing on" half of /voice status.
func (m *Model) voiceChannelReport() string {
	var report strings.Builder
	report.WriteString("  Channels\n")
	for _, channel := range voiceChannels {
		state := "off"
		if channel.get(m.voice) {
			state = "on "
		}
		fmt.Fprintf(&report, "    %-3s %-10s %s\n", state, channel.name, channel.help)
	}
	if !m.voice.config.SpeaksWhileFocused() {
		// Reported rather than assumed, because this setting is the one that can
		// leave the feature silent for a reason nobody can see: a terminal that
		// never reports focus, or a tmux that was not told to.
		switch {
		case !m.terminalFocusReported:
			report.WriteString("    speak_when: unfocused — this terminal has not reported focus yet;\n" +
				"      speech is not being held back until it does\n")
		case m.terminalFocused:
			report.WriteString("    speak_when: unfocused — this window has focus, so only alerts speak\n")
		default:
			report.WriteString("    speak_when: unfocused — this window is in the background\n")
		}
	}
	fmt.Fprintf(&report, "    %-3s %-10s %s\n", "", "provider", m.effectiveProvider())
	if dropped := m.voice.speaker.DroppedStale(); dropped > 0 {
		// The drop itself is deliberate policy — speech runs behind the agent,
		// and a sentence that waited half a minute answers a question nobody
		// is still asking — but audio that never arrived with nothing anywhere
		// saying why reads as a bug. This line is the why.
		fmt.Fprintf(&report, "    %d queued %s dropped for going stale (waited over 30s)\n",
			dropped, pluralizeNoun(dropped, "sentence", "sentences"))
	}
	if runtime.GOOS == "darwin" && speech.Available() && !speech.HasHighQualityVoices() {
		// The single largest change available to how this feature sounds, and it
		// is a download rather than a line of code. Nothing else in sonar would
		// ever tell anyone, so it is said where someone came looking. Darwin
		// only: the detection heuristics and the menu path are macOS's, and on
		// an espeak-ng host this advice would send someone to a settings pane
		// that does not exist.
		report.WriteString("    Voices installed here are the compact ones. The downloadable\n" +
			"      variants sound markedly better: System Settings → Accessibility →\n" +
			"      Spoken Content → System Voice → Manage Voices.\n")
	}
	return report.String()
}

// reportVoiceVoices says who would read what, and who else is available.
//
// The first half is the part that cannot be worked out by reading the config:
// resolution runs through a per-language setting, then a single configured
// voice, then a downloaded variant, then a standard name, then the host's own
// region — and the answer to "so which one is it" was previously "start it and
// listen".
func (m *Model) reportVoiceVoices() tea.Cmd {
	if !speech.Available() {
		return m.appendVoiceReport("This host has no synthesizer, so no voice would be chosen.")
	}
	var configured map[string]string
	single := ""
	if m.voiceActive() {
		configured, single = m.voice.config.Voices, m.voice.config.Voice
	}
	var report strings.Builder
	report.WriteString("Voices\n\n  Chosen for\n")
	for _, language := range m.voiceLanguages(configured) {
		name := strings.TrimSpace(configured[language])
		source := "config voices"
		if name == "" {
			name, source = speech.VoiceForLanguage(language, single), "host"
			if strings.TrimSpace(single) != "" {
				source = "config voice"
			}
		}
		if name == "" {
			name, source = "(system default)", "system settings"
		}
		fmt.Fprintf(&report, "    %-4s %-28s %s\n", language, name, source)
	}
	installed := speech.HostVoiceNames()
	fmt.Fprintf(&report, "\n  %d voices installed. `say -v '?'` lists them all.\n", len(installed))
	report.WriteString("  Set voice.voices in your config to override: {es: Paulina, en: Samantha}.\n")
	report.WriteString("  /voice test speaks a line in each of the languages above.")
	return m.appendVoiceReport(report.String())
}

// voiceLanguages is which languages to report on: the ones configured, plus the
// ones the detector can actually produce, so the list is never empty and never
// pretends to cover languages nothing would ever choose.
func (m *Model) voiceLanguages(configured map[string]string) []string {
	seen := map[string]bool{}
	var languages []string
	for _, language := range spokenDetectableLanguages() {
		seen[language] = true
		languages = append(languages, language)
	}
	var extra []string
	for language := range configured {
		if language = strings.TrimSpace(language); language != "" && !seen[language] {
			extra = append(extra, language)
		}
	}
	sort.Strings(extra)
	return append(languages, extra...)
}

// reportVoiceTest reads one line aloud in each language that would be used.
//
// The point of the whole panel. A voice name is not a sound, and every other
// way to find out what one sounds like involves leaving sonar. The sentence is
// in the language being demonstrated, because a Spanish voice reading English
// demonstrates the wrong thing — and because hearing that mismatch is exactly
// how someone discovers their voices map needs an entry.
func (m *Model) reportVoiceTest() tea.Cmd {
	if !m.voiceActive() {
		return m.appendVoiceReport("Spoken output is off. Run /voice on to turn it on for this session.")
	}
	var configured map[string]string
	if m.voiceActive() {
		configured = m.voice.config.Voices
	}
	languages := m.voiceLanguages(configured)
	for _, language := range languages {
		m.say(language, voiceTestPhrase(language))
	}
	m.voice.speaker.Finish()
	return m.appendVoiceReport("Speaking a line in: " + strings.Join(languages, ", ") +
		".\n  Any key stops. /voice voices shows which voice each one uses.")
}

// voiceTestPhrase is what a test says, in the language it is testing.
//
// The Spanish line is not a greeting. It is loaded with the English words a
// Spanish voice reads as different words — merge, package, cache, deploy, git —
// because the respelling table behind them is the one part of this feature
// nobody could verify by measurement, and this sentence is how you check it.
// Hearing it wrong is the instruction: voice.pronounce overrides any entry.
func voiceTestPhrase(language string) string {
	if phrase := map[string]string{
		"en": "This is how sonar will read an answer to you.",
		"es": "Hice el merge del package, limpié el cache y el deploy con git ya quedó.",
	}[language]; phrase != "" {
		return phrase
	}
	return "This is how sonar will read an answer to you."
}

// setVoiceChannel switches one channel for this session.
func (m *Model) setVoiceChannel(name string, on bool) tea.Cmd {
	if !m.voiceActive() {
		return m.appendVoiceReport("Spoken output is off. Run /voice on to turn it on for this session.")
	}
	for _, channel := range voiceChannels {
		if channel.name != name {
			continue
		}
		channel.set(m.voice, on)
		if !on {
			// Switching a channel off has to stop what it already queued, or the
			// answer keeps arriving for another twenty seconds after being told
			// to stop — which reads as the setting not working.
			m.silenceVoice()
		}
		state := "off"
		if on {
			state = "on"
		}
		return m.setFooterNotice(noticeInfo, "Voice "+name+" "+state+" for this session.", 4*time.Second)
	}
	return m.appendVoiceReport("Unknown channel " + name + ". Try: " + voiceChannelNames() + ".")
}

func voiceChannelNames() string {
	names := make([]string, 0, len(voiceChannels))
	for _, channel := range voiceChannels {
		names = append(names, channel.name)
	}
	return strings.Join(names, ", ")
}

func (m *Model) appendVoiceReport(body string) tea.Cmd {
	m.entries = append(m.entries, ChatEntry{
		Kind: "system", Content: sanitizeTerminalMultiline(body),
	})
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
	return nil
}

// setVoiceEnabled is the master switch, for the session.
//
// It exists because the alternative is editing YAML and restarting, which is
// the shape of a setting nobody turns on. Everything else about voice was built
// to be tuned while listening — channels, the test line, the pronunciation
// overrides — and the switch that gets you there was the one thing that still
// needed a restart.
//
// Turning it on speaks nothing retroactively: the answer channel reads what
// arrives after this, and the request for a spoken close is evaluated per
// dispatch, so the very next turn is written to be heard. Turning it off stops
// the audio immediately rather than letting the backlog drain, because somebody
// who just asked for silence means now.
//
// Dictation is deliberately unaffected in both directions. Hearing and speaking
// are independent capabilities with independent drivers, and ctrl+g works on a
// host that has a microphone and no synthesizer at all.
func (m *Model) setVoiceEnabled(on bool) tea.Cmd {
	if !on {
		if !m.voiceActive() {
			return m.setFooterNotice(noticeInfo, "Spoken output is already off.", 3*time.Second)
		}
		m.closeVoice()
		return m.setFooterNotice(noticeInfo, "Spoken output off. /voice on turns it back on.", 4*time.Second)
	}
	if m.voiceActive() {
		return m.setFooterNotice(noticeInfo, "Spoken output is already on. /voice status shows the channels.", 4*time.Second)
	}
	if notice, ok := m.openVoice(); !ok {
		return m.setFooterNotice(noticeWarning, notice, 8*time.Second)
	}
	// Seeded from whatever is already in the composer, or from the last thing
	// sent: turning voice on mid-session should not read the next answer in the
	// host's language just because this turn began before the switch did.
	m.beginVoiceTurn(m.input.Value() + " " + m.turnPrompt)
	return m.setFooterNotice(noticeInfo,
		"Spoken output on for this session. /voice status shows the channels.", 5*time.Second)
}
