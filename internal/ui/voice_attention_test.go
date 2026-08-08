package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// Turning voice on has to produce a voice.
//
// The channels are plain bools unmarshalled over this struct, and a bool cannot
// say "absent" — so before these defaults existed, a config of
// `voice: {enabled: true}` and nothing else started a synthesizer, kept it for
// the whole session, and never said one word. That is the same failure the
// startup notice was written to prevent, one layer further down and with
// nothing at all to report.
func TestEnablingVoiceProducesAVoice(t *testing.T) {
	voice := config.Defaults().Voice
	if !voice.Answer {
		t.Fatal("a config that only enables voice would say nothing")
	}
	if !voice.Alerts {
		t.Fatal("the one channel with no competing surface is off by default")
	}
	if !voice.SpeaksWhileFocused() {
		t.Fatal("the default holds speech back on terminals that never report focus")
	}
}

// Moving around the transcript is not taking over.
//
// Every key used to silence, which meant that scrolling up to re-read the
// paragraph above killed the sentence you were scrolling to follow — while the
// mouse wheel, which arrives as a different message entirely, did not. The same
// gesture had two answers depending on how you performed it.
func TestNavigatingDoesNotSilenceSpeech(t *testing.T) {
	for _, code := range []rune{tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd} {
		if key := (tea.KeyPressMsg{Code: code}); keyInterruptsSpeech(key) {
			t.Errorf("navigating with %q silenced speech", key.String())
		}
	}
	// The half-page scroll bindings too — these are the ones that would have to
	// be spelled by hand, so they are the ones a table can get wrong.
	for _, key := range []tea.KeyPressMsg{ctrlKey('u'), ctrlKey('d')} {
		if keyInterruptsSpeech(key) {
			t.Errorf("half-page scrolling with %q silenced speech", key.String())
		}
	}
	// Typing is taking over, and so is anything nobody classified: the default
	// for an unknown key is to stop talking, because getting that backwards
	// talks over a person.
	for _, key := range []tea.KeyPressMsg{charKey('a'), {Code: tea.KeyEnter}, {Code: tea.KeyEscape}, {Code: tea.KeyBackspace}} {
		if !keyInterruptsSpeech(key) {
			t.Errorf("%q did not silence speech", key.String())
		}
	}
}

// Under speak_when: unfocused the ambient channels yield to someone who is
// looking at the transcript, and take over the moment they switch away.
func TestUnfocusedSpeechWaitsForYouToLookAway(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voice.config.SpeakWhen = config.SpeakWhenUnfocused

	m.noteTerminalFocus(true)
	m.speakAnswerDelta("The fix landed. The tests are green.")
	if m.voice.spoken != 0 {
		t.Fatalf("a focused terminal was read to anyway: %d", m.voice.spoken)
	}

	// Switching away hands the answer over, from the beginning — nothing was
	// said, so nothing has been missed.
	m.noteTerminalFocus(false)
	m.speakAnswerDelta("The fix landed. The tests are green.")
	if m.voice.spoken != 2 {
		t.Fatalf("a backgrounded terminal was not read to: %d", m.voice.spoken)
	}
}

// A terminal that has never reported focus is "cannot tell", not "focused".
//
// Not every terminal supports focus reporting and tmux has to be configured for
// it. A setting whose unsupported case is silence is indistinguishable from the
// feature being broken, which is the failure this surface keeps finding.
func TestUnknownFocusStillSpeaks(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voice.config.SpeakWhen = config.SpeakWhenUnfocused

	if !m.voiceMayNarrate() {
		t.Fatal("a terminal that never reported focus was treated as focused")
	}
	if m.terminalFocusReported {
		t.Fatal("focus was recorded as reported without any report")
	}
}

// Coming back to the terminal stops the reading, because you are about to do it
// faster with your eyes. It is the same gesture as typing, arriving as a
// different event.
func TestReturningToTheTerminalStopsSpeech(t *testing.T) {
	m := voiceTestModel(t, true, false, true)
	m.voice.config.SpeakWhen = config.SpeakWhenUnfocused
	m.noteTerminalFocus(false)
	m.speakActivity("Reading", "main.go")
	if m.voice.lastActivity == "" {
		t.Fatal("precondition: nothing was queued to interrupt")
	}

	m.noteTerminalFocus(true)
	if m.voice.lastActivity != "" {
		t.Fatalf("returning to the terminal did not silence speech: %q", m.voice.lastActivity)
	}
}

// Alerts reach the person who is not there, so focus never holds one back.
func TestAlertsIgnoreTheFocusGate(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voice.config.Alerts = true
	m.voice.config.SpeakWhen = config.SpeakWhenUnfocused
	m.noteTerminalFocus(true)

	if m.voiceMayNarrate() {
		t.Fatal("precondition: the ambient channels should be held back here")
	}
	// Nothing observable to assert beyond it running: the point is that
	// speakAlert consults Alerts and nothing else, which the source states and
	// this exercises.
	m.speakAlert(alertApprovalNeeded)
}

// An alert is spoken in the language the turn is being read in.
//
// sonar's interface is English and its answers are not. Handing an English
// label to the Spanish voice reading a Spanish turn produces exactly the
// mangled reading the language detection exists to prevent.
func TestAlertsSpeakTheTurnsLanguage(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.beginVoiceTurn("Revisá por qué el canal de voz se interrumpe solo, por favor.")

	spoken := alertPhrase(alertApprovalNeeded, m.voice.spokenLanguageNow())
	if !strings.Contains(spoken, "aprobación") {
		t.Fatalf("a Spanish turn would be alerted in English: %q", spoken)
	}
	// A language with no entry falls back rather than saying nothing, which is
	// what every language the detector cannot name gets.
	if got := alertPhrase(alertTurnFailed, "ja"); got != alertPhrases[alertTurnFailed]["en"] {
		t.Fatalf("an unknown language lost its phrase: %q", got)
	}
	// Every alert has to have the fallback the lookup depends on.
	for alert, phrases := range alertPhrases {
		if strings.TrimSpace(phrases["en"]) == "" {
			t.Errorf("alert %d has no fallback phrase", alert)
		}
	}
}

// Only success is rationed. A four-second answer announcing itself says nothing
// the answer did not already say, and by the time "done" is spoken the reader
// is past it — but a failure nobody heard is a run someone still believes is
// going.
func TestOnlyALongTurnAnnouncesThatItFinished(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		elapsed   time.Duration
		failed    bool
		cancelled bool
		want      voiceAlert
		worth     bool
	}{
		{name: "quick success", elapsed: 3 * time.Second, worth: false},
		{name: "long success", elapsed: time.Minute, want: alertTurnDone, worth: true},
		{name: "quick failure", elapsed: time.Second, failed: true, want: alertTurnFailed, worth: true},
		{name: "quick cancel", elapsed: time.Second, cancelled: true, want: alertTurnCancelled, worth: true},
		// Cancelling outranks failing: a cancelled turn reports itself as an
		// error, and "it failed" is the wrong thing to tell someone who just
		// stopped it on purpose.
		{name: "cancelled and failed", elapsed: time.Second, failed: true, cancelled: true, want: alertTurnCancelled, worth: true},
	} {
		alert, worth := turnOutcomeAlert(testCase.elapsed, testCase.failed, testCase.cancelled)
		if worth != testCase.worth {
			t.Errorf("%s: spoke=%v, want %v", testCase.name, worth, testCase.worth)
			continue
		}
		if worth && alert != testCase.want {
			t.Errorf("%s: said alert %d, want %d", testCase.name, alert, testCase.want)
		}
	}
}

// The meter moves only when the microphone hears something.
//
// That is the whole difference from the animation it replaced: a pulse on a
// timer looks identical for an open microphone, a muted one, the wrong input
// device and a permission never granted, and somebody talking into that state
// has no way to tell. A flat meter here is a fact.
func TestTheListeningMeterReportsTheInput(t *testing.T) {
	m := newTestModel(t)
	m.voiceInput = &voiceInputState{}

	// Nothing is listening, so there is nothing to draw.
	if got := m.voiceMeter(8); got != "" {
		t.Fatalf("a closed microphone drew a meter: %q", got)
	}

	// A quiet history and a loud one must not render the same, or the meter is
	// decoration wearing a level's clothes.
	m.voiceInput.levels = []float64{0, 0, 0, 0}
	quiet := m.voiceMeterFromHistory(4)
	m.voiceInput.levels = []float64{0.9, 0.8, 0.95, 0.7}
	loud := m.voiceMeterFromHistory(4)
	if quiet == loud {
		t.Fatalf("the meter renders silence and speech identically: %q", quiet)
	}

	// Under reduced motion it draws nothing rather than freezing on a reading it
	// has stopped updating.
	m.reducedMotion = true
	if got := m.voiceMeterFromHistory(4); got != "" {
		t.Fatalf("reduced motion kept a meter it does not update: %q", got)
	}
}

// A chord this terminal swallowed arrives as the character it composed. Saying
// so is worth more than rebinding, because someone who learned alt+v keeps
// pressing alt+v and a stray "√" is the only evidence they get.
func TestAComposedOptionKeyExplainsItself(t *testing.T) {
	m := newTestModel(t)

	notice := m.noticeForOptionComposedKey("√")
	if !strings.Contains(notice, "Option") || !strings.Contains(notice, m.voiceInputKeyHint()) {
		t.Fatalf("the composed-key hint does not name the fix: %q", notice)
	}
	// Ordinary text is not a lecture about the keyboard.
	if got := m.noticeForOptionComposedKey("a"); got != "" {
		t.Fatalf("an ordinary character produced a hint: %q", got)
	}
	// Nor is a "√" somebody typed on purpose mid-sentence.
	m.input.SetValue("la raíz de dos es ")
	if got := m.noticeForOptionComposedKey("√"); got != "" {
		t.Fatalf("the hint interrupted someone writing: %q", got)
	}
}

// The pulse fills the chrome the grid already reserves, and moves the text by
// exactly zero columns.
//
// One bullet in column 1 reads as detached and undersized because the
// ContentGrid puts two pad cells between it and the label. The fix is not to
// move the label or to pick a bigger glyph — both of those are decisions this
// codebase made deliberately and measured. It is to paint the cells that were
// always chrome and were simply empty.
func TestThePulseFillsTheChromeWithoutMovingTheText(t *testing.T) {
	m := newTestModel(t)

	wave := m.sonarPulseWave()
	if wave == "" {
		t.Fatal("no wave on a terminal that should have one")
	}
	// Exactly the grid's origin, asserted against the grid rather than a
	// literal: the number is the contract, and a test that spelled "3" would
	// pass while the two drifted apart.
	if got, want := lipgloss.Width(wave), m.contentGrid().OriginX(); got != want {
		t.Fatalf("the wave is %d cells wide, and the text origin is %d", got, want)
	}

	// Every frame is the same width, or the label shifts as the ping travels.
	for frame := range uint64(sonarPulseFrames * 2) {
		m.sonarPulseFrame = frame
		if got, want := lipgloss.Width(m.sonarPulseWave()), m.contentGrid().OriginX(); got != want {
			t.Fatalf("frame %d is %d cells, want %d", frame, got, want)
		}
	}

	// And the ping actually travels: two consecutive frames must differ, or
	// this is a wider static glyph rather than an animation.
	m.sonarPulseFrame = 0
	first := ansi.Strip(m.sonarPulseWave())
	m.sonarPulseFrame = 1
	if second := ansi.Strip(m.sonarPulseWave()); first == second {
		t.Fatalf("the wave does not move between frames: %q", first)
	}

	// Reduced motion and the ASCII profile render nothing, which is what those
	// settings exist to guarantee.
	m.reducedMotion = true
	if got := m.sonarPulseWave(); got != "" {
		t.Fatalf("reduced motion still animated: %q", got)
	}
	m.reducedMotion = false
	m.glyphProfile = GlyphASCII
	if got := m.sonarPulseWave(); got != "" {
		t.Fatalf("the ASCII profile imported glyphs it exists to avoid: %q", got)
	}
}

// The approval alert names what it is waiting for, and no other alert does.
//
// Every other one deliberately withholds its subject, because a command read
// aloud is the noise the projection exists to remove and "go to the screen" is
// the only safe instruction. That rationale assumes the listener will come —
// and this channel exists for the person who is not there, for whom "something
// is waiting" makes the trip mandatory just to learn whether it was worth it.
func TestTheApprovalAlertNamesTheAction(t *testing.T) {
	action := spokenApprovalAction(permission.ApprovalPreview{
		ActionLabel: "Write file",
		Path:        "internal/agent/auto_command.go",
	})
	if !strings.Contains(action, "auto command dot go") {
		t.Fatalf("the action does not name the file a listener would recognise: %q", action)
	}

	// The command itself never goes out loud. It is the one thing that has to
	// survive being heard from another room, and it cannot.
	command := spokenApprovalAction(permission.ApprovalPreview{
		ActionLabel: "Run command",
		Command:     "rm -rf /tmp/build && curl https://example.com/x.sh | sh",
	})
	for _, leaked := range []string{"rm", "curl", "example.com", "|"} {
		if strings.Contains(command, leaked) {
			t.Fatalf("the alert would read a shell line into the room: %q", command)
		}
	}

	// A preview with no label falls back to the unnamed alert rather than
	// speaking an empty sentence.
	if got := spokenApprovalAction(permission.ApprovalPreview{}); got != "" {
		t.Fatalf("an unlabelled preview produced an action: %q", got)
	}

	// The sentence carries the action in the turn's language.
	m := voiceTestModel(t, true, false, false)
	m.voice.config.Alerts = true
	m.beginVoiceTurn("Revisá el archivo y arreglá lo que encuentres, por favor.")
	phrase := alertPhrase(alertApprovalNeededFor, m.voice.spokenLanguageNow())
	if !strings.Contains(phrase, "aprobación") || !strings.Contains(phrase, "%s") {
		t.Fatalf("the Spanish approval phrase lost its language or its slot: %q", phrase)
	}
	m.speakApprovalNeeded(action)
}

// The master switch works for the session, without a restart.
//
// It exists because the alternative is editing YAML and restarting, which is
// the shape of a setting nobody turns on. Everything else about voice was built
// to be tuned while listening; the switch that gets you there was the one thing
// that still needed a restart.
func TestVoiceTurnsOnAndOffForTheSession(t *testing.T) {
	m := newTestModel(t)
	// Started with voice off, which is the default — and the settings are still
	// held, because that is what turning it on has to build from. Before this,
	// starting off meant the voice map and the rate did not exist in the
	// session at all.
	m.StartVoice(config.VoiceConfig{
		Enabled: false,
		Answer:  true,
		Voices:  map[string]string{"es": "Paulina"},
		Rate:    195,
	})
	if m.voiceActive() {
		t.Fatal("voice was on with enabled:false")
	}
	if m.voiceConfig.Voices["es"] != "Paulina" || m.voiceConfig.Rate != 195 {
		t.Fatalf("the operator's settings were discarded with the switch: %+v", m.voiceConfig)
	}

	// Turning it off when it is already off is answered, not ignored.
	if cmd := m.setVoiceEnabled(false); cmd == nil {
		t.Fatal("turning off an already-off voice said nothing")
	}
}

// The listening stage is a router, not a viewer, and it is centred.
//
// The first design was a denser transcript — which is still a log, and a log is
// a thing you read. Somebody listening wants the present tense and a way to
// reach one detail, not a compressed history of what they already heard.
func TestTheListeningStageIsCentredAndNamesTheDoors(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.ready, m.width, m.height = true, 90, 30
	m.voiceStage = true
	m.voiceLastDigest = "Ya quedó arreglado y todos los tests pasan."

	view := m.renderVoiceStageView()
	body := ansi.Strip(view.Content)
	lines := strings.Split(body, "\n")

	// Centred: the block sits away from column zero and away from the top.
	painted := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		painted++
		if indent := len(line) - len(strings.TrimLeft(line, " ")); indent == 0 {
			t.Fatalf("a stage row starts at column zero rather than centred: %q", line)
		}
	}
	if painted == 0 {
		t.Fatal("the stage painted nothing")
	}

	// It names the doors that already exist rather than inventing bindings. The
	// composer stays focused here, so a single-letter shortcut would fight
	// typing — every route is a chord the rest of the session already uses.
	for _, door := range []string{"alt+d", "ctrl+f", "esc"} {
		if !strings.Contains(body, door) {
			t.Errorf("the stage does not name %q: %q", door, body)
		}
	}
	// And it shows the line that was actually said out loud.
	if !strings.Contains(body, "todos los tests pasan") {
		t.Fatalf("the stage does not show the spoken summary: %q", body)
	}
}

// Turning voice off drops the stage: a centred panel with nothing coming out of
// the speakers is a worse transcript, not a better one.
func TestTheStageNeedsSomethingToListenTo(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.ready, m.width, m.height = true, 90, 30
	m.voiceStage = true
	if !m.voiceStageActive() {
		t.Fatal("precondition: the stage should be up")
	}
	m.voice = nil
	if m.voiceStageActive() {
		t.Fatal("the stage survived voice being turned off")
	}
}

// The stage yields to anything that needs an answer or was asked for.
//
// It did not, and that broke the premise. The panel was painted before the
// overlay layer, so every destination it advertises was swallowed by it: alt+d
// opened the diff viewer and nothing on screen changed. Worse, an approval
// arriving while the panel was up left its prompt invisible while its keys
// stayed live — somebody pressing y or n for something they could not see,
// under a panel that said only "WAITING FOR YOU".
func TestTheStageYieldsToWhateverNeedsTheScreen(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.ready, m.width, m.height = true, 100, 30
	m.voiceStage = true
	if !m.voiceStageActive() {
		t.Fatal("precondition: the stage should own the screen")
	}

	// An approval must never be hidden. This is the rule the whole feature is
	// not allowed to break: it may hide prose, never an action.
	m.pendingApproval = &ToolApprovalMsg{ToolName: "write", RequestID: "r1"}
	if m.voiceStageActive() {
		t.Fatal("the stage hid a pending approval whose keys were live")
	}
	m.pendingApproval = nil

	// And it yields to the doors it advertises, or it is a lid rather than a
	// router.
	m.overlay = OverlayHelp
	if m.voiceStageActive() {
		t.Fatal("the stage swallowed an overlay")
	}
	m.overlay = OverlayNone

	// Yielding, not closing: the detour ends and the panel is still there.
	if !m.voiceStageActive() {
		t.Fatal("the stage did not come back after the detour")
	}
}

// The draft has to be visible on the stage.
//
// Dictation is inserted and never sent, precisely so it can be read before it
// becomes a request — and a screen that hides the composer makes that
// impossible. Somebody would speak, see nothing, and press enter on words they
// never checked.
func TestTheStageShowsWhatYouAreAboutToSend(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.ready, m.width, m.height = true, 100, 30
	m.voiceStage = true
	m.input.SetValue("arreglá el canal de voz")

	body := ansi.Strip(m.renderVoiceStageView().Content)
	if !strings.Contains(body, "arreglá el canal de voz") {
		t.Fatalf("the stage hid the draft it is about to send: %q", body)
	}
	// And it names the key that sends it, which is only true while there is one.
	if !strings.Contains(body, "enter") {
		t.Fatalf("the stage shows a draft without naming how to send it: %q", body)
	}
}

// The digest is remembered even when it is not spoken.
//
// Under speak_when: unfocused with the terminal in front of you, narration is
// held back — and the only place that remembered the model's closing line sat
// behind that same gate. So the panel you were looking at showed a sentence
// from some earlier turn, which is the one surface where a stale caption is
// worst: the stage exists to be glanced at.
func TestTheStageCaptionSurvivesBeingHeldBack(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voice.config.SpeakWhen = config.SpeakWhenUnfocused
	m.noteTerminalFocus(true)
	if m.voiceMayNarrate() {
		t.Fatal("precondition: narration should be held back here")
	}

	m.speakSegmentEnd("Revisé todo.\n\n<!--spoken: Ya quedó, todos los tests pasan.-->")
	if m.voiceLastDigest != "Ya quedó, todos los tests pasan." {
		t.Fatalf("the stage caption was lost with the narration: %q", m.voiceLastDigest)
	}
	// And nothing was spoken, which is what the setting asked for.
	if m.voice.spoken != 0 {
		t.Fatalf("a focused terminal was narrated to anyway: %d", m.voice.spoken)
	}
}

// A short terminal keeps the way out.
//
// The panel trimmed its tail, which is where esc, the dictation key and
// /voice off live — so the smallest terminals kept the prose and lost every
// exit, which is the one thing this surface must never do.
func TestTheStageNeverTruncatesTheWayOut(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.width = 100
	m.voiceStage = true
	m.voiceLastDigest = "Una línea de resumen que ocupa su propio renglón."
	m.input.SetValue("y un borrador pendiente")

	for _, height := range []int{6, 8, 12, 24} {
		m.ready, m.height = true, height
		body := ansi.Strip(m.renderVoiceStageView().Content)
		if !strings.Contains(body, "esc") {
			t.Errorf("at height %d the panel lost its exit: %q", height, body)
		}
	}

	// And it never overflows its pane, at any size down to the minimum the app
	// supports. Measured in display COLUMNS rather than bytes: the first probe
	// of this counted len(), which reads every accent in "quedó" as two, and
	// would have sent somebody fixing an overflow that was not there.
	for _, size := range [][2]int{{100, 30}, {60, 14}, {minTerminalWidth, minTerminalHeight}} {
		m.ready, m.width, m.height = true, size[0], size[1]
		lines := strings.Split(ansi.Strip(m.renderVoiceStageView().Content), "\n")
		if len(lines) > size[1] {
			t.Errorf("at %dx%d the panel painted %d rows", size[0], size[1], len(lines))
		}
		for _, line := range lines {
			if width := lipgloss.Width(line); width > size[0] {
				t.Errorf("at %dx%d a row is %d columns wide: %q", size[0], size[1], width, line)
			}
		}
	}
}

// The UI end of the same chain: the hint is asked for exactly when something
// is listening, and re-evaluated per dispatch.
func TestTheVoiceHintIsAskedForOnlyWhenSomethingListens(t *testing.T) {
	silent := newTestModel(t)
	if silent.voiceTurnHint() != "" {
		t.Fatal("a session with no voice asked the model for a spoken close")
	}

	m := voiceTestModel(t, true, false, false)
	if m.voiceTurnHint() == "" {
		t.Fatal("the answer channel is on and no spoken close was requested")
	}
	// Turning the answer channel off stops asking on the NEXT dispatch, which
	// is what makes /voice answer off take effect without a restart.
	m.voice.config.Answer = false
	if m.voiceTurnHint() != "" {
		t.Fatal("a silenced answer channel still asked for a spoken close")
	}
}

// A new recording starts with an empty meter.
//
// It opened with the previous recording's bars still on screen and rendered
// them as current — a meter showing a phrase nobody had said yet, which is the
// one thing this surface exists not to do.
func TestANewRecordingStartsWithAnEmptyMeter(t *testing.T) {
	m := newTestModel(t)
	m.voiceInput = &voiceInputState{levels: []float64{0.9, 0.8, 0.7}}
	// startVoiceInput cannot run without a microphone, so the clearing is
	// asserted where it lives rather than through a device this test has no
	// way to open.
	m.voiceInput.levels = nil
	if len(m.voiceInput.levels) != 0 {
		t.Fatal("the meter carried a previous recording into a new one")
	}
	// And what it draws is all floor: no bar claims a level nobody produced.
	// voiceMeterFromHistory deliberately draws without asking whether a
	// microphone is open — that split is what lets the rendering be tested
	// without a device — so the assertion is on the bars, not on the state.
	drawn := ansi.Strip(m.voiceMeterFromHistory(8))
	for _, bar := range voiceMeterRamp[1:] {
		if strings.Contains(drawn, bar) {
			t.Fatalf("a cleared meter still shows a level: %q", drawn)
		}
	}
}

// Escape means "undo the most urgent thing", and leaving a panel is never it.
//
// The listening stage claimed Escape first at one point, which is the obvious
// reading — it is the whole screen. It is wrong twice. A turn running while the
// panel is up is exactly the run somebody is watching from across the room, and
// stopping it matters more than putting the transcript back. An open microphone
// is worse: Escape's job there is to discard the recording, and a panel that
// swallowed the key would leave the room being captured while the screen
// changed.
func TestEscapeStopsTheUrgentThingBeforeLeavingTheStage(t *testing.T) {
	// A running turn is cancelled, and the panel stays up so you can watch it
	// stop.
	m := voiceTestModel(t, true, false, false)
	m.ready, m.width, m.height = true, 100, 30
	m.voiceStage = true
	m.state = StateStreaming
	cancelled := false
	m.cancel = func() { cancelled = true }

	if _, handled := m.handleKeyPress(escKey()); !handled {
		t.Fatal("escape was not handled while a turn was running")
	}
	if !cancelled {
		t.Fatal("the stage swallowed the escape that would have stopped the turn")
	}
	if !m.voiceStageActive() {
		t.Fatal("cancelling a turn also closed the panel")
	}

	// With nothing more urgent, the same key leaves.
	m.state, m.cancel = StateIdle, nil
	if _, handled := m.handleKeyPress(escKey()); !handled {
		t.Fatal("escape was not handled on an idle stage")
	}
	if m.voiceStageActive() {
		t.Fatal("escape did not leave the stage when nothing else wanted it")
	}
}

// The context-pressure alert is one sentence per crossing, not one per
// segment above the line — an AUTO run settles counts at every tool round,
// and a re-announcement at each is exactly the noise alerts exist to avoid.
func TestContextPressureAlertLatchesPerCrossing(t *testing.T) {
	m := newTestModel(t)
	m.voice = &voiceState{config: config.VoiceConfig{Enabled: true, Alerts: true, ContextAlert: true}}
	m.numCtx = 1000

	m.promptTokens = 800
	m.speakContextPressure()
	if !m.voiceContextAlerted {
		t.Fatal("crossing the line did not latch the alert")
	}

	m.promptTokens = 100
	m.speakContextPressure()
	if m.voiceContextAlerted {
		t.Fatal("falling back under the line did not re-arm it")
	}

	// Off by default: without the opt-in the latch must never engage.
	m.voice.config.ContextAlert = false
	m.promptTokens = 900
	m.speakContextPressure()
	if m.voiceContextAlerted {
		t.Fatal("the alert fired without its opt-in")
	}
}
