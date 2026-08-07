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
//
// This is the bound a user meets, and it is the shorter of two: the recorder
// carries speech.MaxUtterance as its own stop time, for the case this timer
// cannot cover — a harness killed outright leaves nothing running to fire it.
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
	// levels is the rolling input-level history the meter draws. See
	// voice_meter.go: it is the only evidence on screen that the microphone is
	// hearing a voice rather than recording a muted input.
	levels []float64
	// approvalAtStart is the request that was on screen when recording began.
	//
	// Transcription takes seconds, and an approval can be answered from the
	// keyboard and replaced by the next one while it runs. Without this, words
	// spoken about request A resolved request B — a grant for something the
	// speaker never saw. Empty when nothing was waiting.
	approvalAtStart string
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
// and transcribe what was said. It also gives the rail it lights up a clock.
//
// The activity animation only advances while a tick is already in flight, and
// pressing this from an idle composer is exactly the case where none is — so
// without the spinner the listening beats would sit frozen on whichever frame
// they stopped at, which reads as a hung microphone rather than an open one.
func (m *Model) toggleVoiceInput() tea.Cmd {
	if m == nil {
		return nil
	}
	// Sequenced rather than nested in one Batch call: the spinner is only owed
	// once the toggle has changed the state it reads, and argument evaluation
	// order is not a thing to make a UI depend on.
	toggled := m.toggleVoiceInputOnly()
	return tea.Batch(toggled, m.startSpinnerCmd())
}

func (m *Model) toggleVoiceInputOnly() tea.Cmd {
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
	if err := m.voiceTranscriber().Ready(); err != nil {
		// Refused before the microphone opens, not after. Recording someone's
		// voice and then telling them it cannot be transcribed is the worst
		// ordering available.
		//
		// Asked of the transcriber this session will actually use, not of the
		// package default: a configured model path is part of whether it can
		// run, and the earlier version checked only that the binary existed.
		return m.setFooterNotice(noticeWarning, voiceUnavailableNotice(err), 8*time.Second)
	}
	// Speech output and speech input must not overlap: a synthesizer talking
	// into an open microphone transcribes the harness. This has to happen
	// BEFORE the microphone opens — it used to run after, which is the overlap
	// it exists to prevent. Nobody heard it because the key handler silences
	// speech before routing any key, so the call below was already redundant by
	// the time it ran; a path that reaches here without a key press would have
	// recorded the tail of a sentence.
	m.silenceVoice()
	if err := m.voiceInput.listener.Start(); err != nil {
		return m.setFooterNotice(noticeWarning, voiceUnavailableNotice(err), 6*time.Second)
	}
	// The request this recording is about, captured before a word is spoken.
	if m.pendingApproval != nil {
		m.voiceInput.approvalAtStart = m.pendingApproval.RequestID
	} else {
		m.voiceInput.approvalAtStart = ""
	}
	// The meter starts empty. Without this, a new recording opened with the
	// PREVIOUS one's bars still on screen and rendered them as if they were
	// current — a meter showing a phrase nobody had said yet, which is the one
	// thing this surface exists not to do.
	m.voiceInput.levels = nil
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
	// A closed vocabulary of read-only steering is checked first, and only
	// against the WHOLE utterance. "Mostrame el diff" opens the diff; "mostrame
	// el diff y arreglá el bug" is dictation, because the second half is a
	// request and swallowing it would drop what somebody asked for.
	//
	// Nothing here can send a prompt, answer an approval or cancel a turn. See
	// voice_command.go: a mis-transcription costs a screen nobody wanted, which
	// is the only class of mistake this microphone is allowed to make.
	// An approval waiting on screen is the only thing that widens what a spoken
	// utterance can do, and it widens it only because YOU opened the microphone
	// with a key while it was there. The harness never opens one on its own; see
	// voice_approval.go for why that decision shapes the rest.
	if m.pendingApproval != nil &&
		m.pendingApproval.RequestID == m.voiceInput.approvalAtStart {
		if answered := m.answerApprovalByVoice(text); answered != nil {
			return answered
		}
	}
	if spoken := voiceCommandFor(text); spoken != voiceCommandNone {
		return m.runVoiceCommand(spoken)
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
	case strings.Contains(err.Error(), speech.ErrNoModel.Error()):
		// Named separately from the transcriber because Homebrew installs
		// whisper-cli and no usable model, so this is what a fresh install
		// looks like — and "brew install whisper-cpp" is then advice to
		// reinstall something that is already there.
		return "Voice input needs a model: " + speech.ModelDownloadHint()
	default:
		return "Voice input failed: " + sanitizeTerminalSingleLine(err.Error())
	}
}

// reportVoiceStatus prints every part of the voice pipeline and what is missing.
//
// Voice can fail in four independent places — recorder, transcriber, model,
// synthesizer — and three of them fail silently from the user's side: the key
// does nothing, or the microphone opens and produces no text. Without this,
// diagnosing a missing model meant listing audio devices, recording a sample by
// hand and reading the source, which is what it actually took once.
func (m *Model) reportVoiceStatus() tea.Cmd {
	var voices map[string]string
	if m.voiceActive() {
		voices = m.voice.config.Voices
	}
	var report strings.Builder
	report.WriteString("Voice pipeline\n\n")
	broken := 0
	for _, entry := range speech.Diagnose(m.voiceInputModel, voices) {
		mark := "ok  "
		if !entry.OK {
			mark, broken = "MISSING", broken+1
		}
		detail := entry.Detail
		if detail == "" {
			detail = "—"
		}
		fmt.Fprintf(&report, "  %-8s %-12s %s\n", mark, entry.Component, detail)
		if entry.Fix != "" {
			fmt.Fprintf(&report, "           %-12s %s\n", "", entry.Fix)
		}
	}
	// Whether speech is switched on is a separate question from whether it
	// could work, and answering only the second is how a fully-installed
	// pipeline stays silent with nothing explaining it.
	switch {
	case !m.voiceActive():
		report.WriteString("\n  Spoken output is off. Run /voice on to turn it on for this session,\n  or set voice.enabled: true to have it on from the start.")
	default:
		report.WriteString("\n" + m.voiceChannelReport())
	}
	if broken == 0 {
		report.WriteString("\n  Dictation is ready: " + m.voiceInputKeyHint() + " or /voice opens the microphone.")
	}
	m.entries = append(m.entries, ChatEntry{
		Kind: "system", Content: sanitizeTerminalMultiline(report.String()),
	})
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
	return nil
}
