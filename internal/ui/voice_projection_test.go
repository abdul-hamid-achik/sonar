package ui

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/speech"
)

// TestSpokenTextDropsWhatCannotBeHeard is the measured case. The raw form of
// this sentence takes 12.9 seconds through `say`; the projected form takes 6.1.
// The whole difference is a URL and a path being spelled character by
// character, and no amount of better phrasing recovers it — the answer is
// saying less.
func TestSpokenTextDropsWhatCannotBeHeard(t *testing.T) {
	raw := "The fix lands in `internal/agent/auto_command.go:852`. " +
		"See https://github.com/abdul-hamid-achik/sonar/pull/12 — " +
		"run `go test ./internal/agent/` first."
	got := spokenText(raw)

	for _, unwanted := range []string{"https://", "github.com", "internal/agent", ":852", "`"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("projection kept %q, which a listener cannot use: %q", unwanted, got)
		}
	}
	// What survives is what identifies the thing.
	if !strings.Contains(got, "auto command dot go") {
		t.Fatalf("projection lost the file's identity: %q", got)
	}
	if !strings.Contains(got, "a link") {
		t.Fatalf("projection dropped the link instead of naming it: %q", got)
	}
	if len(got) >= len(raw) {
		t.Fatalf("projection did not shorten anything: %d -> %d", len(raw), len(got))
	}
}

// A fence is code, and code is never spoken. Removing it must also not fuse the
// sentences on either side into one.
func TestSpokenTextNeverSpeaksAFence(t *testing.T) {
	raw := "Run the suite.\n\n```go\nfunc main() { println(\"hello\") }\n```\n\nThen commit."
	got := spokenText(raw)
	for _, unwanted := range []string{"func main", "println", "```"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("code was spoken: %q", got)
		}
	}
	if !strings.Contains(got, "Run the suite") || !strings.Contains(got, "Then commit") {
		t.Fatalf("prose around the fence was lost: %q", got)
	}
	sentences, _ := spokenSentences(got)
	if len(sentences) < 2 {
		t.Fatalf("removing the fence fused two sentences into one: %q", sentences)
	}
}

func TestSpokenPathKeepsOnlyWhatIdentifies(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"internal/agent/auto_command.go", "auto command dot go"},
		{"internal/agent/auto_command.go:852", "auto command dot go"},
		{"./bin/sonar", "sonar"},
		{"README.md", "README dot md"},
		{"some-file_name.test.go", "some file name dot test dot go"},
		{"/", "a path"},
	} {
		if got := spokenPath(tc.in); got != tc.want {
			t.Fatalf("spokenPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSpokenSentencesHoldsBackTheIncompleteTail is what makes streaming speech
// possible. The text after the last boundary may still be growing; speaking it
// would say half a clause now and the other half later as if it were new.
func TestSpokenSentencesHoldsBackTheIncompleteTail(t *testing.T) {
	sentences, remainder := spokenSentences("First one. Second one. And a third that is still")
	if len(sentences) != 2 {
		t.Fatalf("expected two complete sentences, got %q", sentences)
	}
	if remainder != "And a third that is still" {
		t.Fatalf("incomplete tail was not held back: %q", remainder)
	}

	// The tail completes when more arrives, and nothing is spoken twice.
	more, tail := spokenSentences(remainder + " arriving. ")
	if len(more) != 1 || !strings.HasPrefix(more[0], "And a third") {
		t.Fatalf("completed tail was not spoken as one sentence: %q", more)
	}
	if tail != "" {
		t.Fatalf("nothing should remain: %q", tail)
	}
}

// A decimal is not a sentence boundary. The projection turns file extensions
// into " dot " before this runs, which is what keeps "v0.4.2" from splitting.
func TestSpokenSentencesDoesNotSplitOnDecimals(t *testing.T) {
	sentences, remainder := spokenSentences("It took 1.5 seconds on v0.4.2 to finish. Then it stopped.")
	if len(sentences) != 2 {
		t.Fatalf("a decimal split a sentence: %q remainder=%q", sentences, remainder)
	}
	if !strings.Contains(sentences[0], "1.5") || !strings.Contains(sentences[0], "v0.4.2") {
		t.Fatalf("numbers were mangled: %q", sentences[0])
	}
}

// Activity reuses the labels the transcript already paints, so the spoken and
// the visible surfaces cannot describe the same work differently.
func TestSpokenActivityNamesTheWorkNotThePath(t *testing.T) {
	got := spokenActivity("Reading", "internal/agent/auto_command.go")
	if !strings.Contains(got, "Reading") {
		t.Fatalf("activity lost its verb: %q", got)
	}
	if strings.Contains(got, "internal/agent") {
		t.Fatalf("activity spoke a path: %q", got)
	}
	if got := spokenActivity("", "whatever"); got != "" {
		t.Fatalf("an unlabelled activity produced speech: %q", got)
	}
}

// voiceTestModel enables the channels a case cares about. It never starts a
// real synthesizer: the channel policy is what these tests are about, and
// spawning `say` in a unit test would make the suite depend on an audio device.
func voiceTestModel(t *testing.T, answer, reasoning, activity bool) *Model {
	t.Helper()
	m := newTestModel(t)
	m.voice = &voiceState{config: config.VoiceConfig{
		Enabled: true, Answer: answer, Reasoning: reasoning, Activity: activity,
	}}
	return m
}

// TestVoiceChannelsAreIndependent is the reason there are three switches rather
// than one. An AUTO turn produces eight tool receipts and several reasoning
// blocks; spoken together they bury the sentence that was worth waiting for.
func TestVoiceChannelsAreIndependent(t *testing.T) {
	// With no speaker installed every channel is inert, which is what a host
	// without a synthesizer gets. Nothing here may panic.
	silent := newTestModel(t)
	silent.speakAnswerDelta("Anything at all.")
	silent.speakReasoning("Thinking.")
	silent.speakActivity("Reading", "main.go")
	silent.speakTurnEnd("Done.")
	silent.silenceVoice()
	if silent.voiceActive() {
		t.Fatal("a model with no speaker reported voice as active")
	}

	// A channel that is off consumes nothing, so the answer channel's position
	// stays at zero when only activity is on.
	activityOnly := voiceTestModel(t, false, false, true)
	activityOnly.speakAnswerDelta("A complete sentence. And another.")
	if activityOnly.voice.spoken != 0 {
		t.Fatalf("a disabled answer channel consumed text: %d", activityOnly.voice.spoken)
	}
}

// TestVoiceAnswerSpeaksEachSentenceOnce pins the streaming contract: the
// position advances by whole sentences and never revisits one.
func TestVoiceAnswerSpeaksEachSentenceOnce(t *testing.T) {
	m := voiceTestModel(t, true, false, false)

	m.speakAnswerDelta("The fix landed.")
	if m.voice.spoken != 1 {
		t.Fatalf("a complete sentence was not consumed: %d", m.voice.spoken)
	}

	// The same prefix arriving again must add nothing.
	m.speakAnswerDelta("The fix landed.")
	if m.voice.spoken != 1 {
		t.Fatalf("a sentence was consumed twice: %d", m.voice.spoken)
	}

	// A growing but incomplete tail is held back.
	m.speakAnswerDelta("The fix landed. Now running")
	if m.voice.spoken != 1 {
		t.Fatalf("an incomplete tail was spoken: %d", m.voice.spoken)
	}

	// Completing it consumes exactly that sentence.
	m.speakAnswerDelta("The fix landed. Now running the tests.")
	if m.voice.spoken != 2 {
		t.Fatalf("the completed tail was not consumed: %d", m.voice.spoken)
	}

	// Ending the turn is the only boundary that rewinds, because the next turn
	// is a different answer.
	m.speakTurnEnd("The fix landed. Now running the tests.")
	if m.voice.spoken != 0 {
		t.Fatalf("the turn boundary did not reset the position: %d", m.voice.spoken)
	}
}

// The holdback applies to text that is still arriving, and to nothing else.
//
// Waiting for a closing delimiter is right while more is coming and destructive
// once it is not: a finished answer carrying one unmatched backtick lost every
// word after it — permanently, since the turn boundary is the last chance to
// say anything. A delimiter that will never close is a character, not an
// unfinished span.
func TestSettledTextIsNeverHeldBack(t *testing.T) {
	const answer = "El símbolo ` sirve para código. Eso era todo lo que faltaba."

	if got := spokenStreamingText(answer); strings.Contains(got, "Eso era todo") {
		t.Fatalf("streaming did not hold back an unclosed span: %q", got)
	}
	settled := spokenText(answer)
	if !strings.Contains(settled, "Eso era todo lo que faltaba") {
		t.Fatalf("a settled answer lost everything after a lone backtick: %q", settled)
	}

	// End to end: the turn boundary has to say what streaming withheld.
	m := voiceTestModel(t, true, false, false)
	m.speakAnswerDelta(answer)
	held := m.voice.spoken
	m.speakTurnEnd(answer)
	if held != 0 {
		t.Fatalf("precondition: streaming should have withheld everything, said %d", held)
	}
	if sentences, _ := spokenSentences(settled); len(sentences) < 2 {
		t.Fatalf("the settled projection has nothing to flush: %q", sentences)
	}
}

// A slash is not a path.
//
// The reducer keeps only a token's last segment, which is what makes
// "internal/agent/auto_command.go" worth hearing and what silently rewrote
// every other slash people type. Each case below is what the projection
// actually said before this rule existed.
func TestSpokenProjectionOnlyReducesRealPaths(t *testing.T) {
	for _, testCase := range []struct{ raw, want string }{
		// Left alone: a date reduced to its year, two fractions reduced to
		// their denominators, and a conjunction reduced to its second half —
		// which reversed the sentence.
		{"Lo hicimos el 5/8/2026 y salió bien.", "Lo hicimos el 5/8/2026 y salió bien."},
		{"Cubrimos 1/2 de los casos.", "Cubrimos 1/2 de los casos."},
		{"El ratio es 3/4 aproximadamente.", "El ratio es 3/4 aproximadamente."},
		{"Aplica a usuarios y/o administradores.", "Aplica a usuarios y/o administradores."},
		// Still reduced, because these are paths.
		{"Mirá internal/agent/auto_command.go ahí.", "Mirá auto command dot go ahí."},
		{"El paquete internal/ui tiene el bug.", "El paquete ui tiene el bug."},
		{"Está en /usr/local/bin ahora.", "Está en bin ahora."},
		{"El reporte 2024/01/informe.pdf sirve.", "El reporte informe dot pdf sirve."},
		// A wildcard is punctuation with no name in it. "go test dot dot dot"
		// is a second of noise; whoever hears "go test" knows which tree.
		{"Corré go test ./... y verificá.", "Corré go test y verificá."},
	} {
		if got := spokenText(testCase.raw); got != testCase.want {
			t.Errorf("spokenText(%q)\n got: %q\nwant: %q", testCase.raw, got, testCase.want)
		}
	}
}

// A code fence is held back until it closes.
//
// While one streams it is indistinguishable from prose, so every line of the
// block would be read aloud; and when the closing fence lands the projection
// collapses to something SHORTER than what was already spoken. Both failures
// are the same missing rule.
func TestVoiceHoldsBackAnUnfinishedFence(t *testing.T) {
	const opening = "Found it. Look:\n\n```go\nfunc main() {\n\t// Do the thing. Then this.\n"
	projected := spokenStreamingText(opening)
	if strings.Contains(projected, "func main") || strings.Contains(projected, "Then this") {
		t.Fatalf("an unfinished fence was projected as prose: %q", projected)
	}
	if !strings.Contains(projected, "Found it.") {
		t.Fatalf("prose before the fence was dropped with it: %q", projected)
	}

	// The same rule covers an inline span whose closing backtick has not
	// arrived, which is the streaming case that slid the old byte offset.
	if got := spokenStreamingText("Mirá el archivo `internal"); strings.Contains(got, "internal") {
		t.Fatalf("an unfinished inline span was projected: %q", got)
	}

	// Streaming the whole thing must never step backwards.
	m := voiceTestModel(t, true, false, false)
	m.speakAnswerDelta(opening)
	afterFence := m.voice.spoken
	m.speakAnswerDelta(opening + "}\n```\n\nThat fixes it.")
	if m.voice.spoken < afterFence {
		t.Fatalf("the closing fence rewound the position: %d -> %d", afterFence, m.voice.spoken)
	}
}

// The activity channel says "reading" once for a run of reads, because the
// transcript's own collapsed run already decided four reads are one event.
func TestVoiceActivityDoesNotRepeatItself(t *testing.T) {
	m := voiceTestModel(t, false, false, true)
	m.speakActivity("Reading", "internal/agent/one.go")
	first := m.voice.lastActivity
	if first == "" {
		t.Fatal("the first activity was not recorded")
	}
	m.speakActivity("Reading", "internal/agent/one.go")
	if m.voice.lastActivity != first {
		t.Fatalf("a repeated activity changed state: %q", m.voice.lastActivity)
	}
}

// Interruption cancels and never queues. This is the gap every existing
// implementation of spoken agent output leaves open.
//
// What it must NOT do is forget where the answer had reached. Silencing runs on
// every key press, so an interrupted turn that then keeps streaming would
// re-speak itself from its first sentence — and because the Enter that sends a
// message is also a key press, a turn-scoped mute would silence every answer.
// The position survives; only the audio and the activity de-duplicator reset.
func TestVoiceSilenceCancelsAudioAndKeepsItsPlace(t *testing.T) {
	m := voiceTestModel(t, true, false, true)
	m.speakAnswerDelta("One sentence. Two sentences.")
	m.speakActivity("Reading", "main.go")
	if m.voice.spoken == 0 || m.voice.lastActivity == "" {
		t.Fatal("precondition: nothing was queued to discard")
	}
	position := m.voice.spoken

	m.silenceVoice()
	if m.voice.lastActivity != "" {
		t.Fatalf("silence kept the activity de-duplicator: %q", m.voice.lastActivity)
	}
	if m.voice.spoken != position {
		t.Fatalf("silence lost the answer position: %d -> %d", position, m.voice.spoken)
	}

	// The rest of the same answer arriving must add only what is new.
	m.speakAnswerDelta("One sentence. Two sentences. Three sentences.")
	if m.voice.spoken != position+1 {
		t.Fatalf("an interrupted answer repeated itself: %d -> %d", position, m.voice.spoken)
	}
}

// TestListeningTakesTheRailFromEverything pins the one state that outranks a
// running turn. An open microphone is the only moment the harness captures the
// room rather than reporting on itself, and a person has to be able to notice
// it without going looking.
func TestListeningTakesTheRailFromEverything(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.toolsPending = 1
	busy, ok := m.currentWorkingActivity()
	if !ok || busy.label == "Listening" {
		t.Fatalf("precondition: a busy turn already claims to listen: %#v", busy)
	}

	m.voiceInput = &voiceInputState{transcribing: true}
	transcribing, ok := m.currentWorkingActivity()
	if !ok || transcribing.label != "Transcribing" {
		t.Fatalf("transcribing did not take the rail from a running tool: %#v", transcribing)
	}
	// Transcription is a one-shot wait with nothing to animate, so it is static
	// rather than adding a second clock.
	if !transcribing.static {
		t.Fatal("transcribing animates, which would add a clock for a fixed wait")
	}
}

// The listening animation is the pulse read backwards: an echo converging
// rather than a ping going out. Distinguishing them by SHAPE is what makes it
// survive a monochrome terminal, where two colors of the same dot would not.
func TestListeningAnimationInvertsTheEmitOne(t *testing.T) {
	emit := sonarPulseBeats
	listen := sonarListenBeats
	if len(emit) != len(listen) {
		t.Fatalf("the two animations do not share a vocabulary: %v vs %v", emit, listen)
	}
	for index := range emit {
		if emit[index] != listen[len(listen)-1-index] {
			t.Fatalf("listening is not the emit sequence reversed: %v vs %v", emit, listen)
		}
	}
}

// The two halves of voice fail for different reasons and are fixed by different
// commands, so the message names which one rather than saying "unavailable".
func TestVoiceUnavailableNoticeNamesTheFix(t *testing.T) {
	if got := voiceUnavailableNotice(speech.ErrNoCapture); !strings.Contains(got, "ffmpeg") {
		t.Fatalf("a missing recorder did not name its install: %q", got)
	}
	if got := voiceUnavailableNotice(speech.ErrNoTranscriber); !strings.Contains(got, "whisper") {
		t.Fatalf("a missing transcriber did not name its install: %q", got)
	}
	if got := voiceUnavailableNotice(nil); got != "" {
		t.Fatalf("no error produced a notice: %q", got)
	}
}
