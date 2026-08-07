# AGENTS.md

Guidance for coding agents working **on** sonar. Humans evaluating or using
sonar want [README.md](README.md) instead.

This is the single source of truth. `CLAUDE.md` imports this file rather than
restating it: two prose files describing one repository drift, and these two
already did — an earlier copy called sonar "a private fork of sonar" (a fork of
itself, left by a rename) and described a `docs/` website that does not exist
here. Nothing compares two prose files, so there is only one.

## What sonar is

A terminal coding agent for hosted models, reached over an API with an API key.
No local runtime. Go 1.25+, Charm v2 for the TUI, MCP servers for the extended
tool surface.

Forked from [`local-agent`](https://github.com/abdul-hamid-achik/local-agent)
and cut down: the agent loop, tool dispatch, permission model, durable goals,
session store, and MCP surface came across intact; the local-first inference
machinery did not.

`docs/` is an Astro Starlight site, written for sonar and deployed to Vercel.
It returned under the condition the earlier one failed: a `docs/` inherited from
upstream lived here until it was removed, because every page described a
different product and told the reader to install it.

So it ships with the thing that makes "written for sonar" checkable. Nothing
compares two prose files — the same reason `internal/drift` exists — so
`internal/docs` reads every page and fails the Go suite when the site names a
slash command the registry does not have or a setting `config.Config` does not
define. It cannot check whether prose is TRUE; it checks the class of error that
actually happened, which is a page naming a thing that is simply not there. The
site's own scaffold shipped an `AGENTS.md`, a `CLAUDE.md` and a `README.md`
describing the template; all three were deleted on arrival for the same reason.

`README.md` is still the front door for someone evaluating sonar, and
`config.example.yaml` is still the reference for settings — it is checked in
beside the code it describes, so a page here points at it rather than copying it.

Keep tracked files free of maintainer-specific paths, usernames, host names,
and private tool inventories. Examples use neutral defaults such as
`~/.config/sonar/env`. Product notes, planning, and ADRs live outside this
repository, in `~/notes/projects/sonar`.

## Build and development

This project uses [Task](https://taskfile.dev/).

```bash
task build          # compile to bin/sonar
task run            # build + run
task dev            # go run ./cmd/sonar
task install        # install onto this machine's PATH (GOBIN, or GOPATH/bin)
task uninstall      # remove it again
task test           # go test ./...
task lint           # golangci-lint run ./...
task verify         # tidy, lint, vet, race tests, govulncheck
task glyphrun       # every deterministic terminal behaviour spec
task glyphrun-contracts  # verify every committed spec contract hash
task clean          # remove bin/
```

One test:

```bash
go test ./internal/agent/ -run TestFunctionName
```

Before claiming a change is done: `task verify` and `task glyphrun` both pass.
`golangci-lint run ./...` reports zero issues today; keep it there.

### Terminal specs

`specs/` holds Glyphrun specs that drive the real binary in a PTY and compare
full-screen snapshots. They catch what unit tests cannot: layout, truncation,
and what a user actually reads.

Two things about them are easy to get wrong.

**Never bisect a spec failure in a `git worktree`.** The status bar paints
`<branch> · <repo-dirname>`, and snapshots capture the whole screen, so a
worktree fails every header-bearing spec for a reason unrelated to your change.
Clone into a directory named `sonar`, stay on `main`, and restore the old tree
with `git checkout <sha> -- .`.

**Fixtures must not inherit your environment.** Every fixture in
`specs/fixtures/` scrubs provider credentials through `hermeticEnv()` and
declares its own provider. Before that existed, a developer machine exporting
`DEEPSEEK_API_KEY` made the suite configure a real hosted provider no spec asked
for — specs passed or failed on ambient state, and **a test run could reach a
metered endpoint with a real credential**. `OLLAMA_HOST` is deliberately
exempt: specs point it at a dead port on purpose.

Re-baseline with `--update-snapshots` only after reading the diff and
confirming the new render is the intended one.

`specs/pending/` is outside the glob and is not run. It holds specs that
describe behaviour sonar does not have for a decided reason rather than a bug;
each is listed in that directory's README with what would move it back. Prefer
it to deleting a spec, and prefer either to leaving CI red.

## Provider boundary

**DeepSeek V4 Flash is the default, not the boundary.** Provider metadata comes
from an embedded [Catwalk](https://github.com/charmbracelet/catwalk) snapshot in
`internal/catalog` — 40 providers, 1403 models — so adding a provider is data,
plus a dialect only if its family is new.

Never enumerate provider types in a new allowlist. Ask
`config.IsKnownProviderType`. Three separate enumerations once demoted a valid
hosted provider to the local runtime path; that is why the predicate is
single-sourced.

Dialect is chosen from **provider identity**, not endpoint shape, in
`llm.NewProviderClient`. Both startup and the runtime `/provider` switch go
through it, so the two cannot disagree.

| Dialect | Coverage |
| --- | --- |
| DeepSeek | `deepseek` |
| Anthropic Messages | the four anthropic-family providers |
| OpenAI-compatible | 27 of the catalog's 40, plus `ollama` |
| — | google and the cloud-credential families still need dialects |

`ollama` means **Ollama Cloud**, not a local daemon. It is not in the Catwalk
snapshot, so its endpoint (`https://ollama.com/v1`), credential
(`OLLAMA_API_KEY`) and default model come from `builtinProviderDefaults` in
`internal/config/provider.go`. It needs no adapter: the native `/api` protocol
that would need one is the local daemon's.

`ProviderProfile.IsRemote()` is therefore constant `true`. It stays as a named
boundary rather than being deleted — it is the seam to reopen if a local
runtime ever returns — and the branches it made unreachable are gone: the
switch-back-to-local path in `manager.go`, the credential exemption in
`ResolveAPIKey`, and the expert-consultation wiring in `cmd/sonar`. The RAM-fit
model guard is inert for the same reason and stays, because it is reached
through `Validate()` rather than a branch.

What has *not* been untangled is `internal/ui`: the model picker, inventory
views and Agent Hub still reference the local runtime and `expertteam`. None of
it can be reached, but no file is Ollama-only, so removing it is a rewrite of
those surfaces rather than a deletion.

### The DeepSeek contract

Three parts diverge from the generic OpenAI shape and all three are
load-bearing. See `internal/llm/deepseek.go`:

- Chain-of-thought is toggled by `{"thinking": {"type": ...}}`, not by
  `reasoning_effort`. Effort grades depth once thinking is on; it cannot switch
  it off.
- Thinking defaults to **enabled**. Never assume a request is cheap.
- An assistant message carrying tool calls must echo its own
  `reasoning_content` back on every later request, or the API returns **400**.
  `turnRuntime.lastReasoning` carries it through the loop; it is host-only and
  must never cross a session, transcript, or checkpoint boundary. The field is
  a `*string` so that absent and empty stay distinguishable.

`internal/llm/deepseek_test.go` pins each behaviour. Do not weaken those tests
to make a refactor pass.

`privacy.local_only` bounds **tool** endpoints (MCP), not inference. Every
sonar request leaves the machine by construction.

It is really enforced — `internal/mcp/http_policy.go` resolves the target
itself and verifies every address it gets back — so the setting is not a
leftover, even though the name reads like one. What *was* a leftover is an
inference gate: the model picker refused an Ollama Cloud model under
`local_only`, copied from `local-agent`, where it is correct because Ollama
runs models on the machine and there is a local alternative to fall back to.
Here it could only ever refuse every model the harness supports, and it
contradicted `TestSwitchProviderIgnoresLocalOnly` two packages away. It is
gone, and `internal/ui`'s `TestNoLocalOnlyInferenceGateReturns` fails if it
comes back — a source scan, because nothing else fails at the moment the copy
lands.

That split is the general shape of these two repositories: sonar is hosted
APIs, `local-agent` is local models through Ollama. A rule about locality
usually belongs to exactly one of them.

## Architecture

### Package layout (`internal/`)

- **agent/** — ReAct loop, prompt construction, mode policy, ordered tool
  dispatch, compaction, hooks, checkpoints, and lifecycle tracing. Built-in,
  memory, and MCP effects execute deterministically in model order.
- **llm/** — DeepSeek and Anthropic adapters over a shared OpenAI-compatible
  transport, per-request expectations, and runtime switching. Inherited Ollama
  client and inventory still live here.
- **mcp/** — STDIO, SSE, and Streamable HTTP connections, namespaced tools,
  health checks, bounded results, reconnects, and dispatch tracing.
- **ecosystem/** — Exact companion-tool receipt parsers and bounded
  transport/domain/evidence projections. Raw MCP structured output must not
  cross into persisted UI state.
- **config/** — YAML/XDG loading, environment overrides, model preferences,
  routing, privacy policy, agent profiles, credentials, and ignore rules.
- **ice/** — Off by default: DeepSeek exposes no embeddings endpoint, so
  retrieval needs a separate local backend.
- **memory/** — Workspace-scoped JSON memory under
  `~/.config/sonar/memory/<workspace-hash>.json`, owner-only, locked.
- **db/** — SQLite sessions, permissions, checkpoints, usage, execution events,
  control-plane records, durable goal projections.
- **goal/** — Durable goal lifecycle, immutable criteria, budgets, permits,
  receipts, evidence-backed recovery values.
- **goaladvisor/** — Bounded optional semantic adapter. It does not own
  scheduling or approvals.
- **controlplane/** — Append-only exception values and validation.
- **supervisor/**, **workunit/** — Tested scheduling/admission contracts. Not
  wired headless or multi-process execution engines.
- **skill/** — Skill discovery and activation from sonar and shared `~/.agents`
  directories.
- **command/** — Canonical slash-command registry and hidden aliases.
- **speech/** — Host synthesizer and microphone as subprocesses. Receives
  finished sentences; it does not decide what to say.
- **drift/** — Compares this repository against `local-agent` on a pinned list
  of packages that must stay identical.
- **docs/** — Test-only. Compares the published site against the command
  registry and the config type, so a page cannot name what does not exist.
- **ui/** — Bubble Tea v2 smart parent, Bubbles children, transient overlays,
  Glamour rendering.

### Request flow

1. The TUI or headless controller submits input under NORMAL, PLAN, or AUTO
   authority.
2. Project instructions, active skills, loaded context, workspace memory, and
   optional retrieval form bounded prompt context.
3. `ModelManager` resolves the provider model and freezes the expected context
   policy for that request.
4. The response streams through the Agent loop.
5. Tool calls pass host mode policy, workspace validation, permission checks,
   and durable execution recording before dispatch.
6. Receipts return in model order; the loop continues within iteration, token,
   and context limits.
7. Session state is persisted. Background auto-memory yields to foreground
   inference and joins at shutdown.

An explicit `/goal` adds a durable Goal Runtime around this flow. Ordinary AUTO
prompts stay direct and approval-gated. sonar owns budgets, permits,
cancellation, approvals, persistence, and recovery.

**MCP transport success, domain success, and verified evidence are
independent.** Conflating them is the standard way to misread an MCP session: a
transport failure has no domain outcome, and reporting one asserts the server
was reached. Parse known structured contracts inside `internal/ecosystem`;
unknown versions stay attention/unknown, and raw `StructuredContent` is
discarded before the UI or persistence boundary.

### Unattended runs

`tools.auto_max_iterations` bounds one provider segment. AUTO chains segments
after it fires, and the ceiling on top is `tools.auto_max_segments` and
`tools.auto_max_wall_time` (bounded at 512 and 24h). Those two were host
constants until they were not — raising the visible knob did nothing past 90
minutes and nothing explained why.

`tools.approval_timeout` refuses rather than cancels. Dispatch ends a turn on
cancellation but continues past a host refusal, so the model sees the refusal
and takes another route. A timeout can only ever withhold permission, never
grant it.

### Voice

Four things here are load-bearing and each was learned by getting it wrong.

**A provider segment is not a turn.** `StreamDone` fires at every model
response — once per tool round, once per AUTO continuation, and once more when
a capped request charges an unaccounted reservation. Anything reset there is
reset several times per turn: the language verdict was, so a turn with tool
calls re-decided its language at every round and the short segments that carry
no function words fell back to the host default, reading Spanish answers in an
English voice. The spoken position is per stream buffer and resets with it in
`resetTranscriptStreamText`; the language is per turn and resets in
`beginVoiceTurn`, which an AUTO continuation deliberately does not reach.

**The speaker queues; only `Stop` cuts.** A voice belongs to a process and is
chosen when that process starts, so two languages mean two processes. Starting
the second by signalling the first cuts a sentence in half — and since a
segment end closes the synthesizer's input, every tool call took that path.
`internal/speech` now serializes utterances through one worker that waits for a
voice to finish before starting the next. Writing to a synthesizer also blocks
once its pipe fills, and the caller is Bubble Tea's `Update`, so queuing keeps
the frame loop off the audio device.

**`say` obeys `[[…]]` from stdin, and every word spoken came from a model.**
Measured: the same sentence renders 136,996 bytes with `[[slnc 2000]]` in it
and 49,956 without, so the command is executed rather than read. `[[volm 0]]`
is a mute that looks like a bug in this package. `escapeSynthesizerCommands`
breaks the delimiter before anything is written, and the deliberate prosody
pause is added *after* it — reversing the two hands back the channel that
escaping just claimed.

**Alerts are the channel that justifies the feature.** Reading an answer aloud
competes with reading it off the screen and loses. An approval waiting on
somebody who is not looking has no competing channel at all, so `alerts` is on
by default and ignores `speak_when`, which every other channel obeys. Alerts
name what is waiting, never the command — the projection discipline matters
more here than anywhere, because the sentence has to survive being heard from
another room.

macOS ships compact voices; the downloadable variants are a different feature
entirely. `VoiceForLanguage` prefers one when it exists, and the parenthesised-
name heuristic that demotes the novelty set had to learn not to demote them.

**`/voice` tunes the session; the config file records the decision.** Every
setting is reachable by its config-file name — provider, speak_when, rate,
voice, pronounce — because these are settings only an ear can judge and a loop
that requires editing a file and restarting is one nobody closes. Nothing is
persisted even though `runtimepref` would make it easy: the session is where you
find out what sounds right, and a mid-experiment state inherited on the next
launch is a setting nobody chose. `voiceSettingsYAML` prints the block to paste
instead. Changing the provider, rate or voice reopens the speaker — a driver
binds those at construction — and the turn's language, position and digest
survive it, because they belong to the conversation rather than to the device.

**The harness never opens the microphone; you do.** An approval can be answered
by speaking, but only in the seconds after somebody pressed the dictation key
while a prompt was already waiting — the alternative puts the harness in charge
of when the room is recorded, and this codebase already states that a
microphone opened by anything other than a deliberate act is a privacy problem
rather than a convenience. That choice pays twice: the person is at the
keyboard so they can SEE what they are approving, and the keyboard is one reach
away so refusing the dangerous cases costs a keypress exactly where a keypress
is cheap. `voice_approval.go` can only ever produce AllowOnce or Deny — the type
has no session member to reach — and anything carrying a
`DestructiveCommandWarning` is refused rather than downgraded. Approving needs a
distinctive word and denying does not, because the local `base` Whisper reports
no confidence and the two directions cost different things: a wrong deny is a
refusal the model routes around, a wrong allow is a command nobody asked for.
"sí" and "no" are in neither list.

**Voice steering is read-only, and that is a safety boundary rather than a
scope decision.** "Show me the diff" and "approve it" go through the same
microphone, the same `base` Whisper model and the same far-field audio; a
mis-transcription costs a screen in one case and a command in the other. So
`voice_command.go` matches a closed vocabulary against the WHOLE utterance —
containing a phrase is not being one, because the rest of the sentence is
usually a request — and nothing it can reach sends a prompt, answers an
approval, cancels a turn, or changes a setting that outlives the session.
Cancelling is absent deliberately: a mis-heard "stop" that kills a two-hour
AUTO run is expensive, and Escape already stops without a microphone. A test
pins the reachable set, so a new phrase cannot quietly widen it.

**Order matters in the digest path, and it bought silence once.** `speakDigest`
dropped the pending narration before projecting the digest — and the projection
is lossy by design, so a digest that reduces to nothing discarded a queue that
would have been fine and said nothing in its place. Project first, drop second.
The same line carries its own language, because "again" three turns later would
otherwise read a Spanish sentence in whatever voice the current turn is using.

**"Back" means one step out of wherever you are.** The stage yields to viewers,
so once a detour is open the stage is already inactive — and a `back` that only
knew how to leave the stage silently did nothing, which is the word most likely
to be said right after opening a detour. It closes the viewer, then the overlay,
then the stage.

**A panel may lose its prose, never its exits.** The listening stage trimmed its
tail on short terminals, which is where `esc`, the dictation key and `/voice
off` live. It trims from the middle now. This is the same rule as the one below
about hiding actions, applied to the way out.

**The listening stage is a router, not a viewer.** The first design for it was
a denser transcript — assistant turns collapsed to their digest, tool cards
folded to a count. That is still a log, and a log is a thing you read; somebody
listening wants the present tense and a way to reach one detail, not a
compressed history of what they already heard. So `voice_stage.go` is one
centred panel, and every detail surface it names already exists — `alt+d`,
`alt+o`, `ctrl+f`, `ctrl+t`. It adds no bindings, because the composer stays
focused on it and a single-letter shortcut would fight typing. The rule it must
keep: it may hide prose, never an action and never an error.

**There are two drivers, and the hosted one was justified by measurement
before it was built.** `internal/speech/hosted.go` reaches OpenAI's speech
endpoint and plays the result through one `ffplay` per run — MP3 frames
concatenate, measured at 5.904s + 5.808s rendering as exactly 11.712s, so a run
stays continuous the way `say`'s stdin does. `afplay` cannot: it takes a path
and has no pipe mode. The driver reports `Needs{}` empty, which is the finding
rather than an omission. It is off by default and selected with
`voice.provider: openai`; an unknown provider is an error rather than a silent
fallback, because somebody who asked for the hosted engine and quietly got the
local one would hear the exact mispronunciation they were trying to fix.

**A driver declares what it needs; the caller asks rather than assumes.** The
same sentence was generated through four engines and transcribed back with the
local Whisper to see which words survived. `say` with Paulina produced "el
merch … el caché … confit"; with the respelling table, every technical term
survived. xAI told `language=es-MX` failed the same way — "Merch", "Catch",
"Diploi", "Geet" — and xAI given `auto` got them all right, as did OpenAI's
`gpt-4o-mini-tts`.

The pattern is not that hosted is better. It is that **every engine told the
language applied that language's letter-to-sound rules to the English
vocabulary, and every engine left to detect it handled the mixture.** So the
language detector, the per-turn verdict, the per-language voice map and the
phonetic table are not requirements of reading Spanish aloud — they are
compensation for one property of `say`, which binds a monolingual voice when
the process starts. `speech.Needs` says which of them a driver wants, and
`Model.forDriver` applies only those: passing the respellings to an engine that
did not need them made it worse, turning "guit" into "gitad".

**Speech is slower than the agent works, and two mechanisms bound that.**
`internal/speech` drops a queued utterance that waited longer than
`staleUtteranceAfter` at the moment it would be spoken — alerts are `sticky` and
finish markers are never dropped, because `Speaking()` answers from the queue.
And while the answer channel is on, `voiceAnswerHint` asks the model to close
with one to three sentences written to be heard, carried in an HTML comment so
the transcript keeps the raw record and renders nothing. `speakDigest` then
`DropPending()`s the backlog — dropping, never cutting; cutting stays Stop's
alone — and reads that instead. An extra summarising request was considered and
rejected: it costs a round trip and a second provider failure surface at exactly
the moment the listener is waiting for the outcome, while a model that ignores
an inline hint costs nothing at all.

**The approval alert names its action, and no other alert does.** The rule that
alerts withhold their subject exists because "go to the screen" is the only safe
instruction — and that assumes the listener will come. This channel exists for
the person who will not, for whom "something is waiting" makes the trip
mandatory just to learn whether it was worth it. It says the host's own bounded
label plus `spokenPath`, never the command.

**Speech is measurable, so measure it.** `say -o out.aiff` writes uncompressed
audio, so file size is proportional to duration and a shell loop answers
questions that otherwise get guessed. It is how the useful facts here were
found: an emoji nearly doubles an utterance (29,376 bytes for "Listo", 56,512
for "Listo ✅"), `say` already spells most initialisms correctly — MCP, CPU,
TLS, XML, LLM and SQL render within 10% of their spaced form, several
byte-identical — and only API, CLI, TUI, IDE and URL come out 30–50% short,
which is the signature of being read as a word. The same loop is what stopped a
list of file extensions being added for nothing: `auto_command.go` reads at 84%
of "auto command dot go", so the reducer was never needed there.

What the size cannot answer is whether something sounds *right*, which is
exactly the problem `voice_pronunciation.go` addresses and the reason it is
built the way it is. A Spanish voice applies Spanish letter-to-sound rules to
everything, so English vocabulary becomes different words rather than an accent:
"g" before e is /x/, making "merge" into "MER-je" and "package" into "pa-KA-je";
"h" is silent; "git" is "jit". The respelling table fixes that, and every entry
in it is a guess no measurement can confirm — so `voice.pronounce` in the config
overrides any entry, an empty value removes one, and the Spanish `/voice test`
line is loaded with the words the table covers. Ship a guess, hand the listener
the correction.

All four channels reach the synthesizer through `Model.say` and
`Model.sayNext`, and that is the only reason the respelling is consistent. A
transformation applied at four call sites out of five is a harness that
pronounces one word two ways in a turn, and the fifth call site is always the
one added next.

### Key interfaces

- `llm.Client` — inference adapter contract (`ChatStream`, `Ping`, `Embed`).
- `agent.Output` — streaming and tool-state callbacks.
- `command.Registry` — slash-command dispatch and completion metadata.
- Goal stores and execution repositories — durable state/evidence boundaries,
  independent from presentation.

### Concurrency

Preserve each package's lock ownership and ordering. Inventory commits may wait
on active inference and therefore run in a Bubble Tea command goroutine, never
inside `Update`. MCP connections and health checks may run concurrently;
unknown tool effects may not. Auto-memory is single-flight, cancelled before
foreground inference, and joined at shutdown.

### Configuration

First matching file wins; files are not merged:

1. `./sonar.yaml`
2. `./sonar.yml`
3. `$XDG_CONFIG_HOME/sonar/config.yaml`
4. `$XDG_CONFIG_HOME/sonar/config.yml`
5. `$HOME/.config/sonar/config.yaml`
6. `$HOME/.config/sonar/config.yml`

Environment overrides apply afterward. Shared profiles live under
`~/.agents/agents/<name>/agent.yaml`; shared skills under
`~/.agents/skills/<name>/SKILL.md`. Read `config.example.yaml` before changing
precedence or paths.

## TUI rules

- **Always use Charm libraries**: [Bubble Tea v2](https://charm.land/bubbletea/v2),
  [Bubbles v2](https://charm.land/bubbles/v2),
  [Lip Gloss v2](https://charm.land/lipgloss/v2),
  [Glamour](https://github.com/charmbracelet/glamour).
- Prefer existing Bubbles components (spinner, viewport, textarea, textinput,
  list, table, paginator, progress, stopwatch, timer, key) over custom ones.
- Follow "smart parent, dumb child": the main `Model` processes all messages;
  children expose methods returning `tea.Cmd`.
- Colours come from `internal/ui/theme.go` through `semanticPalette`. **Never
  hardcode a hex or ANSI colour**, and never add a colour meaning outside that
  vocabulary — a new scheme answers the existing ten roles.
  `theme_discipline_test.go` fails on any literal outside the registry, because
  three shipped that way and each was found by accident rather than by looking.
- Pass the active `themeID` explicitly to every palette lookup. It is
  deliberately not a package global: `ui` tests run in parallel, and
  non-variadic helper signatures make the compiler catch a surface that would
  otherwise paint in the default scheme.
- Ambient state (model, remote boundary, context meter, mode) has exactly one
  owner per frame, assigned by `planStatus` in `status_facts.go`. A surface asks
  before painting rather than guessing what another already showed.
- All content starts at the `ContentGrid` origin; column 1 is reserved for
  accent chrome. Do not hand-write indents.
- Modals share one width scale and one anchor and own the rows they cover. Do
  not add a per-overlay maximum width.
- Render cached content where possible.

## Working conventions

**Every fix goes to both repositories.** sonar and `local-agent` share most of
their code. `go test ./internal/drift/` reports what did not cross. Five fixes
were sonar-only until it caught them, including two security fixes — that
package exists because of this.

**Verify before asserting.** Measure the claim you are about to write down. A
"TUI freeze" measured 4.5 ms against a 16 ms frame; a "missing" diff viewer
already existed; a width overflow turned out to be a bad probe setting `m.width`
directly instead of sending `tea.WindowSizeMsg`.

**Look for the second copy.** When a value is duplicated, the tighter or later
copy is the one the user sees. Making a prose cap configurable changed nothing
because a smaller cap sat in the markdown renderer.

**A test asserting a literal can encode the bug.** An inline-code test asserted
one scheme's hexes, so it passed while nine schemes rendered wrong. Assert the
property, not the value.

**A green suite is a claim with an expiry date.** Re-measure at the moment you
write the number down, not from the last run you remember.

**A sweep ends when `grep -rn` returns zero**, not at "I found three".
