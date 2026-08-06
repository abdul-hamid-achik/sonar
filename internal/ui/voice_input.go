package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/speech"
)

// Voice input: one key opens the microphone, the same key closes it, and the
// transcript lands in the composer as ordinary text.
//
// Toggle rather than hold-to-talk. Terminals report key RELEASE only under the
// Kitty keyboard protocol, so a hold binding would work in Ghostty and do
// nothing in Terminal.app — and a control that silently does nothing on the
// default terminal is worse than one that takes two presses everywhere.
//
// The transcript is placed in the composer rather than sent. Dictation
// mis-hears, and a harness that acted on an unreviewed transcription would act
// on something nobody said. What arrives is a draft the speaker reads.

// voiceListenTimeout bounds one utterance. A microphone left open by a
// forgotten keypress is a privacy problem, not just an idle process, so it
// closes itself.
const voiceListenTimeout = 2 * time.Minute

// voiceTranscribeTimeout bounds the transcriber. Local Whisper on a laptop
// measures well under real time for base-sized models, so a minute is generous
// enough that only a wedged process reaches it.
const voiceTranscribeTimeout = time.Minute

type voiceInputState struct {
	listener *speech.Listener
	// token discards the receipt of a recording the user already cancelled.
	token uint64
	// transcribing is true between stopping the microphone and the text
	// arriving, which is a distinct thing for the rail to say.
	transcribing bool
}

// VoiceTranscriptMsg carries a finished transcription back to the parent.
type VoiceTranscriptMsg struct {
	Token uint64
	Text  string
	Err   error
}

// listeningForVoice reports whether the microphone is open. The activity rail
// reads this to invert its own animation.
func (m *Model) listeningForVoice() bool {
	return m != nil && m.voiceInput != nil && m.voiceInput.listener.Recording()
}

func (m *Model) transcribingVoice() bool {
	return m != nil && m.voiceInput != nil && m.voiceInput.transcribing
}

// toggleVoiceInput is the whole interaction: open the microphone, or close it
// and transcribe what was said.
func (m *Model) toggleVoiceInput() tea.Cmd {
	if m == nil {
		return nil
	}
	if m.voiceInput != nil && m.voiceInput.transcribing {
		// A second press while the transcriber is working means "stop waiting",
		// not "start again". Recording over an unfinished transcription would
		// discard words already spoken.
		m.voiceInput.token++
		m.voiceInput.transcribing = false
		return m.setFooterNotice(noticeInfo, "Voice input discarded.", 2*time.Second)
	}
	if m.listeningForVoice() {
		return m.stopVoiceInput()
	}
	return m.startVoiceInput()
}

func (m *Model) startVoiceInput() tea.Cmd {
	if m.voiceInput == nil {
		listener, err := speech.NewListener(m.voiceTranscriber())
		if err != nil {
			return m.setFooterNotice(noticeWarning, voiceUnavailableNotice(err), 6*time.Second)
		}
		m.voiceInput = &voiceInputState{listener: listener}
	}
	if !speech.TranscriberAvailable() {
		// Refused before the microphone opens, not after. Recording someone's
		// voice and then telling them it cannot be transcribed is the worst
		// ordering available.
		return m.setFooterNotice(noticeWarning, voiceUnavailableNotice(speech.ErrNoTranscriber), 6*time.Second)
	}
	if err := m.voiceInput.listener.Start(); err != nil {
		return m.setFooterNotice(noticeWarning, voiceUnavailableNotice(err), 6*time.Second)
	}
	// Speech output and speech input must not overlap: a synthesizer talking
	// into an open microphone transcribes the harness.
	m.silenceVoice()
	m.voiceInput.token++
	token := m.voiceInput.token
	return tea.Tick(voiceListenTimeout, func(time.Time) tea.Msg {
		return VoiceTranscriptMsg{Token: token, Err: errVoiceListenTimeout}
	})
}

func (m *Model) stopVoiceInput() tea.Cmd {
	if m.voiceInput == nil {
		return nil
	}
	m.voiceInput.transcribing = true
	m.voiceInput.token++
	token := m.voiceInput.token
	listener := m.voiceInput.listener
	// Transcription runs in a command goroutine. It is a subprocess that can
	// take seconds, and Update must never be the thing waiting for it.
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), voiceTranscribeTimeout)
		defer cancel()
		text, err := listener.Stop(ctx)
		return VoiceTranscriptMsg{Token: token, Text: text, Err: err}
	}
}

// handleVoiceTranscript places a finished transcription in the composer.
func (m *Model) handleVoiceTranscript(msg VoiceTranscriptMsg) tea.Cmd {
	if m.voiceInput == nil || msg.Token != m.voiceInput.token {
		// A receipt from a recording the user already discarded.
		return nil
	}
	m.voiceInput.transcribing = false
	if msg.Err == errVoiceListenTimeout {
		m.voiceInput.listener.Cancel()
		return m.setFooterNotice(noticeWarning,
			"Voice input closed after two minutes.", 4*time.Second)
	}
	if msg.Err != nil {
		return m.setFooterNotice(noticeError, voiceUnavailableNotice(msg.Err), 6*time.Second)
	}
	text := strings.TrimSpace(sanitizeTerminalSingleLine(msg.Text))
	if text == "" {
		return m.setFooterNotice(noticeInfo, "Heard nothing.", 2*time.Second)
	}
	// Inserted, never sent. Dictation mis-hears, and the speaker reads what
	// arrived before it becomes a request.
	m.clearCompletionSuppression()
	if existing := m.input.Value(); existing != "" && !strings.HasSuffix(existing, " ") {
		m.input.InsertString(" ")
	}
	m.input.InsertString(text)
	m.syncInputHeight()
	return m.reflowInputViewport()
}

// SetVoiceInput installs dictation settings. Dictation needs no enable flag of
// its own: the key does nothing until pressed, and pressing it on a host
// without the tools says which one to install rather than failing quietly.
func (m *Model) SetVoiceInput(cfg config.VoiceInputConfig) {
	if m == nil {
		return
	}
	m.voiceInputModel = strings.TrimSpace(cfg.Model)
	m.voiceInputLanguage = strings.TrimSpace(cfg.Language)
}

func (m *Model) voiceTranscriber() speech.Transcriber {
	return speech.LocalTranscriber{Model: m.voiceInputModel, Language: m.voiceInputLanguage}
}

func (m *Model) closeVoiceInput() {
	if m == nil || m.voiceInput == nil {
		return
	}
	m.voiceInput.listener.Cancel()
	m.voiceInput = nil
}

var errVoiceListenTimeout = fmt.Errorf("speech: listening timed out")

// voiceUnavailableNotice turns a pipeline error into an instruction.
//
// "Voice input unavailable" sends someone looking in the wrong place; the two
// halves fail for different reasons and are fixed by different commands, so the
// message names which one and what to run.
func voiceUnavailableNotice(err error) string {
	switch {
	case err == nil:
		return ""
	case strings.Contains(err.Error(), speech.ErrNoCapture.Error()):
		return "Voice input needs a recorder: brew install ffmpeg"
	case strings.Contains(err.Error(), speech.ErrNoTranscriber.Error()):
		return "Voice input needs a transcriber: brew install whisper-cpp"
	default:
		return "Voice input failed: " + sanitizeTerminalSingleLine(err.Error())
	}
}
