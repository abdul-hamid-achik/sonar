package speech

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingSynthesizer replaces the host synthesizer with a shell that writes
// what it was told to say into a shared log, bracketed by the voice it was
// started as.
//
// A speaker is not an observable. The ordering this package promises — that a
// voice change waits for the previous voice to finish rather than cutting it —
// can only be read back from the processes themselves, so the fake records the
// three moments that matter: it started, it received a sentence, it exited.
func recordingSynthesizer(t *testing.T, extra string) (Driver, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "spoken.log")
	script := `printf 'start %s\n' "$1" >> "$0"; cat >> "$0"; printf 'end %s\n' "$1" >> "$0"; ` + extra
	return &recordingDriver{log: log, script: script}, log
}

// recordingDriver is a sayDriver pointed at a shell that writes down what it
// was told instead of speaking it. It keeps sayDriver's Needs, because these
// tests are about the queue's ordering contracts and those are the same
// whatever the driver needs.
type recordingDriver struct {
	log    string
	script string
	voices map[string]string
}

func (d *recordingDriver) Needs() Needs { return Needs{Language: true, Respelling: true} }

func (d *recordingDriver) Open(language string) (Voice, error) {
	voice := d.voices[language]
	if voice == "" {
		voice = "default"
	}
	command := exec.Command("/bin/sh", "-c", d.script, d.log, voice)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	run := &sayVoice{command: command, stdin: stdin, exited: make(chan struct{})}
	go func() { _ = command.Wait(); close(run.exited) }()
	return run, nil
}

// withVoices names the per-language voice a recording driver should report, so
// the language-change ordering test can tell one run from the next.
func withVoices(driver Driver, voices map[string]string) Driver {
	if recorder, ok := driver.(*recordingDriver); ok {
		recorder.voices = voices
	}
	return driver
}

func waitForSilence(t *testing.T, speaker *Speaker) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for speaker.Speaking() {
		if time.Now().After(deadline) {
			t.Fatal("the speaker never went quiet")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Nothing has been said yet: the first synthesizer creates the file.
		return nil
	}
	if err != nil {
		t.Fatalf("read what was spoken: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

// A language change waits for the previous voice; it never cuts it.
//
// This is the contract the whole queue exists for. A synthesizer voice is
// chosen when its process STARTS, so reading two languages means two processes
// — and the version before this one started the second by sending SIGTERM to
// the first, mid-sentence. Every segment boundary of a turn with tool calls hit
// that path, which is what "it interrupts itself and never finishes a sentence"
// was.
func TestALanguageChangeWaitsForTheVoiceItReplaces(t *testing.T) {
	synthesizer, log := recordingSynthesizer(t, "")
	speaker := newSpeaker(withVoices(synthesizer, map[string]string{"es": "vozES", "en": "vozEN"}))
	defer speaker.Close()

	speaker.SayIn("es", "Encontré el problema.")
	speaker.SayIn("es", "Ya está arreglado.")
	speaker.SayIn("en", "The tests are green.")
	speaker.Finish()
	waitForSilence(t, speaker)

	// The pause is ours: every sentence carries one so a listener can hear where
	// one thought ended. It is part of what reaches the process, so the expected
	// sequence spells it rather than pretending the text arrives bare.
	pause := sentencePause()
	want := []string{
		"start vozES",
		pause + "Encontré el problema.",
		pause + "Ya está arreglado.",
		"end vozES",
		"start vozEN",
		pause + "The tests are green.",
		"end vozEN",
	}
	got := readLog(t, log)
	if len(got) != len(want) {
		t.Fatalf("spoken sequence\n got: %q\nwant: %q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("spoken sequence differs at %d\n got: %q\nwant: %q", index, got, want)
		}
	}
}

// Two sentences in one language share one process, so speech stays continuous
// rather than gapped by a fork between every sentence.
func TestOneLanguageIsOneProcess(t *testing.T) {
	synthesizer, log := recordingSynthesizer(t, "")
	speaker := newSpeaker(withVoices(synthesizer, map[string]string{"es": "vozES"}))
	defer speaker.Close()

	for _, sentence := range []string{"Uno.", "Dos.", "Tres."} {
		speaker.SayIn("es", sentence)
	}
	speaker.Finish()
	waitForSilence(t, speaker)

	if starts := strings.Count(strings.Join(readLog(t, log), "\n"), "start "); starts != 1 {
		t.Fatalf("one language started %d synthesizers", starts)
	}
}

// Interruption cancels and never queues, which means the backlog is dropped
// rather than read out after the key press that asked for silence.
func TestStopDropsTheBacklog(t *testing.T) {
	// The fake holds the audio device for a while after its input closes, which
	// is what a real synthesizer does with a sentence it has been given: the
	// backlog behind it is what Stop has to discard.
	synthesizer, log := recordingSynthesizer(t, "sleep 30")
	speaker := newSpeaker(withVoices(synthesizer, map[string]string{"es": "vozES", "en": "vozEN"}))
	defer speaker.Close()

	speaker.SayIn("es", "La primera.")
	// Wait until the first sentence has actually reached a process, so what
	// follows is a cancel of live audio rather than of an empty queue.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(strings.Join(readLog(t, log), "\n"), "La primera.") {
		if time.Now().After(deadline) {
			t.Fatal("the first sentence never reached a synthesizer")
		}
		time.Sleep(5 * time.Millisecond)
	}
	speaker.SayIn("es", "La segunda.")
	speaker.SayIn("en", "The third.")
	speaker.Stop()

	if speaker.Speaking() {
		t.Fatal("a stopped speaker still reported audio on its way")
	}
	// Which same-voice sentences got through before the cut is timing, not
	// contract: one CI run caught the worker writing the second sentence into
	// the live Spanish process in the instant before Stop landed, and nothing
	// synchronizes that race. The English utterance is the deterministic
	// marker instead: a new language needs a new process, the worker starts
	// it only after the Spanish voice finishes, and the fake holds that voice
	// for 30 seconds — so vozEN speaking at all can only mean the backlog
	// survived Stop. Give the worker room to do exactly that if it were going
	// to.
	time.Sleep(200 * time.Millisecond)
	if spoken := strings.Join(readLog(t, log), "\n"); strings.Contains(spoken, "vozEN") || strings.Contains(spoken, "third") {
		t.Fatalf("a cancelled backlog was spoken anyway: %q", spoken)
	}
}

// The model cannot drive the synthesizer.
//
// `say` obeys commands delimited by [[ and ]] anywhere in its input, stdin
// included — measured, "Uno. [[slnc 2000]] Dos." renders 136,996 bytes against
// 49,956 without it. Every word this package speaks came from a model, so those
// brackets are model-controlled: [[slnc 60000]] is a minute of silence,
// [[volm 0]] is a mute, and both look like a bug in here rather than a sentence
// somebody generated.
func TestTheModelCannotDriveTheSynthesizer(t *testing.T) {
	// The control channel this pins is say's. Until a Linux synthesizer is
	// wired, escapeSynthesizerCommands is a no-op there and there is nothing to
	// prove — asserting against a no-op would pin the absence of a driver
	// rather than the presence of a boundary.
	if escapeSynthesizerCommands("[[x]]") == "[[x]]" {
		t.Skip("no synthesizer control channel on this platform")
	}
	synthesizer, log := recordingSynthesizer(t, "")
	speaker := newSpeaker(withVoices(synthesizer, map[string]string{"es": "vozES"}))
	defer speaker.Close()

	speaker.SayIn("es", "Listo. [[slnc 60000]] [[volm 0]] Seguimos.")
	speaker.Finish()
	waitForSilence(t, speaker)

	spoken := strings.Join(readLog(t, log), "\n")
	// The words survive; only the delimiter is broken, because a lone bracket is
	// not a command and no synthesizer pronounces either form.
	if !strings.Contains(spoken, "Seguimos.") {
		t.Fatalf("escaping ate the sentence: %q", spoken)
	}
	if strings.Contains(spoken, "[[slnc 60000]]") || strings.Contains(spoken, "[[volm 0]]") {
		t.Fatalf("a model-written command reached the synthesizer: %q", spoken)
	}
	// Ours still has to work, or the escaping took the prosody with it.
	if pause := sentencePause(); pause != "" && !strings.Contains(spoken, pause) {
		t.Fatalf("the sentence pause did not survive escaping: %q", spoken)
	}
}

// An alert goes ahead of the backlog, and never ahead of the sentence being
// read.
//
// Speech is slower than the model streams, so an answer channel left running
// builds a queue — and an alert behind it announces a blocked run half a minute
// after it blocked. Jumping the queue fixes that; jumping the sentence would
// cost the listener the words they were following, which is the cut this whole
// package is built to avoid.
func TestAnAlertJumpsTheQueueButNotTheSentence(t *testing.T) {
	synthesizer, log := recordingSynthesizer(t, "")
	speaker := newSpeaker(withVoices(synthesizer, map[string]string{"es": "vozES", "en": "vozEN"}))
	defer speaker.Close()

	// The first sentence has to be in the synthesizer before the rest are
	// queued, or every one of them is still in the queue and jumping it proves
	// nothing about what jumping it costs.
	//
	// The backlog deliberately switches language. A same-voice backlog races
	// this test: the worker may stream a queued same-voice sentence into the
	// live process's stdin in the instant before SayNext lands, and anything
	// already written is beyond the jump — one CI run caught the alert
	// legitimately behind the second sentence for exactly that reason. A
	// different voice needs a different process, which cannot start while the
	// Spanish one is still open, so the English backlog is provably still in
	// the queue when the alert jumps it, no matter how the goroutines
	// interleave.
	speaker.SayIn("es", "Primera del backlog.")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(strings.Join(readLog(t, log), "\n"), "Primera") {
		if time.Now().After(deadline) {
			t.Fatal("the first sentence never reached a synthesizer")
		}
		time.Sleep(5 * time.Millisecond)
	}
	speaker.SayIn("en", "Second in the backlog.")
	speaker.SayIn("en", "Third in the backlog.")
	speaker.SayNext("es", "Espero tu aprobación.")
	speaker.Finish()
	waitForSilence(t, speaker)

	spoken := readLog(t, log)
	positionOf := func(want string) int {
		t.Helper()
		for index, line := range spoken {
			if strings.Contains(line, want) {
				return index
			}
		}
		t.Fatalf("%q was never spoken: %q", want, spoken)
		return -1
	}
	alert := positionOf("aprobación")
	if second := positionOf("Second"); alert > second {
		t.Fatalf("the alert waited behind the backlog: %q", spoken)
	}
	// Already handed over, so it stays where it was. Anything a synthesizer has
	// been given is beyond recall by design — that is the same property that
	// makes a language change wait rather than cut.
	if first := positionOf("Primera"); alert < first {
		t.Fatalf("the alert displaced a sentence already being read: %q", spoken)
	}
}

// Queuing a sentence never waits on the audio device.
//
// Writing to a synthesizer's stdin blocks once its pipe fills, and the caller
// is Bubble Tea's Update. Before the queue, a synthesizer that stopped reading
// stalled the frame loop; now it stalls one worker goroutine and nothing else.
func TestSayingNeverBlocksTheCaller(t *testing.T) {
	// A synthesizer that never reads its input, so the worker's write blocks as
	// soon as the pipe is full.
	deaf := &recordingDriver{log: "/dev/null", script: "sleep 30"}

	speaker := newSpeaker(deaf)
	defer speaker.Close()

	speaker.SayIn("es", strings.Repeat("Una frase muy larga. ", 20_000))
	started := time.Now()
	for index := 0; index < 100; index++ {
		speaker.SayIn("es", "Otra frase.")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("queuing waited on the synthesizer: %s", elapsed)
	}
}

// A sentence that waited too long stops being worth saying.
//
// Speech is slower than an agent works, so on a long run the queue drifts
// behind real time and the listener hears narration of work that finished
// minutes ago — a present that is not the present, which is worse than silence.
// The utterance cap does not help: it is a runaway guard at twenty minutes of
// audio, not a policy about staleness.
func TestAStaleSentenceIsDroppedRatherThanSaidLate(t *testing.T) {
	synthesizer, log := recordingSynthesizer(t, "")
	speaker := newSpeaker(withVoices(synthesizer, map[string]string{"es": "vozES"}))
	defer speaker.Close()

	// The clock is injected, because the property is not observable in real
	// time and a test that waited thirty seconds for it would never be run.
	base := time.Now()
	speaker.mu.Lock()
	speaker.now = func() time.Time { return base }
	speaker.mu.Unlock()

	speaker.SayIn("es", "Esto lo dije hace mucho.")
	speaker.SayNext("es", "Espero tu aprobación.")

	// Age everything already queued well past the bound, then queue something
	// fresh behind it.
	speaker.mu.Lock()
	speaker.now = func() time.Time { return base.Add(staleUtteranceAfter + time.Minute) }
	speaker.mu.Unlock()
	speaker.SayIn("es", "Esto es lo que está pasando ahora.")
	speaker.Finish()
	waitForSilence(t, speaker)

	spoken := strings.Join(readLog(t, log), "\n")
	if strings.Contains(spoken, "hace mucho") {
		t.Errorf("a stale sentence was read out late: %q", spoken)
	}
	// An alert is sticky: it stays true however late it plays, and it jumped the
	// queue anyway. Dropping one would lose the only channel that reaches
	// somebody who is not at the screen.
	if !strings.Contains(spoken, "aprobación") {
		t.Errorf("a late alert was dropped: %q", spoken)
	}
	// And nothing fresh is ever skipped.
	if !strings.Contains(spoken, "está pasando ahora") {
		t.Errorf("a fresh sentence was dropped with the stale ones: %q", spoken)
	}
}

// A finish marker never goes stale. Speaking answers from the queue, so
// dropping one would leave the rail reporting audio that is not coming.
func TestAFinishMarkerIsNeverStale(t *testing.T) {
	synthesizer, _ := recordingSynthesizer(t, "")
	speaker := newSpeaker(synthesizer)
	defer speaker.Close()

	base := time.Now()
	speaker.mu.Lock()
	speaker.now = func() time.Time { return base }
	speaker.mu.Unlock()
	speaker.SayIn("es", "Una frase.")
	speaker.Finish()
	speaker.mu.Lock()
	speaker.now = func() time.Time { return base.Add(time.Hour) }
	speaker.mu.Unlock()

	waitForSilence(t, speaker)
	if speaker.Speaking() {
		t.Fatal("the speaker never settled, so a finish marker was dropped")
	}
}

// Everything at once, under the race detector.
//
// The queue is the part of this package where a bug would be worst and hardest
// to see by reading: a worker goroutine, a condition variable, a generation
// counter, and a subprocess whose lifetime crosses all three. Reasoning about
// that is worth something; running it is worth more.
//
// What this asserts is deliberately not "the right thing was spoken" — under
// concurrent Stops nothing is deterministic. It asserts the three properties
// that must hold whatever the interleaving: it does not deadlock, it does not
// race, and it settles.
func TestTheQueueSurvivesEverythingAtOnce(t *testing.T) {
	synthesizer, _ := recordingSynthesizer(t, "")
	speaker := newSpeaker(withVoices(synthesizer, map[string]string{"es": "vozES", "en": "vozEN"}))

	const goroutines = 8
	var wait sync.WaitGroup
	for worker := range goroutines {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			language := "es"
			if worker%2 == 0 {
				language = "en"
			}
			for round := range 40 {
				switch round % 7 {
				case 0, 1, 2:
					speaker.SayIn(language, "Una frase cualquiera.")
				case 3:
					speaker.SayNext(language, "Espero tu aprobación.")
				case 4:
					speaker.Finish()
				case 5:
					speaker.DropPending()
				case 6:
					// The one operation allowed to cut. Interleaving it with
					// everything else is the whole point: a generation check
					// missed here is a sentence spoken after silence was asked
					// for, or a worker parked forever on a run nobody will end.
					speaker.Stop()
				}
				_ = speaker.Speaking()
				_ = speaker.SpeakingLanguage()
			}
		}(worker)
	}

	settled := make(chan struct{})
	go func() { wait.Wait(); close(settled) }()
	select {
	case <-settled:
	case <-time.After(60 * time.Second):
		t.Fatal("the queue deadlocked under concurrent use")
	}

	// Close has to return even with a run live and a worker mid-flight, or
	// shutdown hangs on an audio device.
	done := make(chan struct{})
	go func() { speaker.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Close blocked on a live run")
	}

	// And a closed speaker refuses further work rather than reopening one.
	speaker.SayIn("es", "Después de cerrar.")
	if speaker.Speaking() {
		t.Fatal("a closed speaker accepted new work")
	}
}
