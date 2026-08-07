package speech

// What a synthesizer is, from the queue's point of view.
//
// This seam exists because of a measurement, not a prediction. The same
// sentence — "Hice el merge del package, limpié el cache y el deploy con git ya
// quedó." — was generated through four engines and transcribed back with the
// local Whisper to see which words survived:
//
//	say, Paulina, es_MX            "el merch ... el caché ... confit"
//	say, Paulina, es_MX + table    every technical term intact
//	xAI, told language=es-MX       "el Merch ... Catch ... Diploi ... Geet"
//	xAI, language=auto             every technical term intact
//	OpenAI gpt-4o-mini-tts         every technical term intact
//
// The pattern is not "hosted is better". It is that **every engine that was
// TOLD the language applied that language's letter-to-sound rules to the
// English vocabulary, and every engine left to detect it handled the mixture**.
// xAI failed the same way `say` does when given es-MX, and succeeded when given
// nothing.
//
// That reframes most of the voice feature. The language detector, the per-turn
// verdict seeded from the user's prompt, the per-language voice map and the
// phonetic respelling table are not requirements of reading Spanish aloud —
// they are compensation for one property of `say`: it binds a monolingual voice
// when the process starts, so somebody has to decide which. A driver that takes
// mixed text and sorts it out itself needs none of them, and giving it the
// language ANYWAY makes it worse.
//
// So a Driver does not merely produce audio. It declares what the caller must
// do before handing text over, and the caller asks rather than assumes.

// Needs is what a driver requires from the layer above it.
//
// Both fields are true for `say` and expected to be false for any engine that
// detects language itself. They are separate because they are separate
// concessions: one is about choosing a voice, the other about spelling words so
// a chosen voice pronounces them.
type Needs struct {
	// Language means the driver binds one language per run and must be told
	// which. A caller that leaves this unset for such a driver gets whatever
	// the host defaults to, which is how Spanish answers were read in English.
	Language bool
	// Respelling means the driver applies one language's letter-to-sound rules
	// to everything it is given, so foreign vocabulary has to be respelled
	// before it arrives. Measured: passing the respellings to an engine that
	// did NOT need them made it worse — "guit" came back as "gitad".
	Respelling bool
}

// Voice is one live output run: sentences go in, audio comes out, and it ends.
//
// A run rather than a call, because that distinction is what the whole queue is
// built on. `say` reads stdin incrementally, so consecutive sentences flow into
// one process with no gap between them — and a voice change means ending one
// run and starting another, which is why Close and Cancel are different
// operations. Close lets what was already given finish playing; Cancel throws
// it away. Only a person may ask for Cancel.
type Voice interface {
	// Say hands over one finished sentence. It may block if the driver's
	// consumer is slow; the Speaker calls it from its own worker for exactly
	// that reason.
	Say(sentence string) error
	// Close ends the run and lets whatever was already given play out. Done
	// closes when that has finished.
	Close() error
	// Cancel ends the run now and discards anything not yet heard.
	Cancel()
	// Done closes when this run has finished producing sound.
	Done() <-chan struct{}
}

// Driver opens voice runs and says what it needs to open them well.
type Driver interface {
	// Open starts a run. The language is meaningful only when Needs().Language
	// is set; a driver that detects language itself must ignore it rather than
	// let a caller's guess override its own detection.
	Open(language string) (Voice, error)
	// Needs reports what the caller must do before handing text over.
	Needs() Needs
}
