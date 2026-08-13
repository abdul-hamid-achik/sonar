# sonar

A terminal coding agent for hosted models, over the API, with an API key. No local runtime.

MIT licensed. Built on [Charm](https://charm.land) and the [Catwalk](https://github.com/charmbracelet/catwalk) provider catalog.

Full documentation lives in [`docs/`](docs/) — an Astro Starlight site. This
README stays the front door; the site is where the longer explanations go.

**DeepSeek V4 Flash is the default, not the boundary.** Provider metadata comes from an embedded [Catwalk](https://github.com/charmbracelet/catwalk) snapshot — 40 providers, 1403 models — so selecting `groq`, `cerebras`, or `moonshot` needs a provider name and a key, not code. DeepSeek also ships `deepseek-v4-pro` in that catalog — same dialect, switch with `/model deepseek-v4-pro` or pin it in config. `ollama` is in that list too and means **Ollama Cloud** (`https://ollama.com/v1`, `OLLAMA_API_KEY`), not a daemon on your machine.

Forked from [`local-agent`](https://github.com/abdul-hamid-achik/local-agent) and cut down. The agent loop, tool dispatch, permission model, durable goals, session store, and MCP surface came across intact; the local-first inference machinery did not.

```
$ sonar

  ◜◝ sonar
  deepseek-v4-flash · 1M ctx · thinking
  $0.14/$0.28 per 1M · key: DEEPSEEK_API_KEY

› refactor internal/llm to drop the inventory layer
```

> **Alpha.** This is a working harness the author uses daily, not a finished
> product. It is not a sandbox: tools run with your user's permissions, gated by
> an approval model you can loosen. Read [Safety](#safety) before running it
> unattended.

## Quick start

```bash
export DEEPSEEK_API_KEY=sk-...
task install     # builds and puts `sonar` on your PATH
sonar
```

Or without installing:

```bash
task build && ./bin/sonar
```

Or keep keys in a file and point sonar at it:

```yaml
# sonar.yaml
credentials:
  env_file: ~/.config/sonar/env
```

Only names the catalog recognises as provider credentials are read from that
file, and never over an already-exported variable — a shared secrets file
usually also defines `PATH` and `EDITOR`, and applying those would corrupt the
process.

Without a key, sonar exits `1` and names the missing variable. There is no unauthenticated mode.

Configuration is optional — the defaults already resolve to DeepSeek. See `config.example.yaml` for the full surface.

## Using it

Type to talk. `/help` lists every key and command. The parts worth knowing up front:

| | |
| --- | --- |
| `shift+tab` | cycle **NORMAL → PLAN → AUTO**. PLAN is read-only; AUTO runs tools under a scoped-shell policy and still asks before anything outside it |
| `enter` while running | slash commands run immediately; other drafts queue for after the turn |
| `alt+d` | full diff of what the agent changed |
| `ctrl+f` | search the transcript |
| `/goal` | a durable objective with its own budget, criteria, and receipts — it survives a restart |
| `/recover` | inspect an execution whose outcome was never recorded, and log what actually happened |
| `/runtime` | provider, model, approval posture, MCP health, and this model's typical first-response time |
| `-p "…"` | headless: one prompt, `--json` for a machine-readable turn receipt |

The waiting indicator is a measurement, not decoration: a head travels a fixed
track against this model's own typical first response, so a wait that is slow
*for this model* looks different from one that is normal. Position carries the
fact, so it reads the same with `NO_COLOR`.

### Long unattended runs

`tools.auto_max_iterations` bounds a single provider segment; AUTO chains
segments after it fires. The ceiling on the whole turn is
`tools.auto_max_segments` and `tools.auto_max_wall_time` — up to 512 segments
and 24 hours. Both were fixed host constants until recently, which meant no
setting could take a run past 90 minutes and nothing said why.

`tools.approval_timeout` decides what an unanswered approval prompt does when
nobody is watching. It **refuses and continues** rather than cancelling: the
model sees the refusal and takes another route, and the run keeps going. A
timeout can only ever withhold permission, never grant it.

### Hearing it from the next room

macOS only for now, off by default, `voice.enabled: true` to turn on. Four
channels, and the interesting one is **alerts**: a tool waiting for approval, a
long turn that finished, a turn that failed. Those are the things a person
cannot get any other way — reading an answer aloud competes with reading it off
the screen and loses, but an approval nobody is looking at stops the run until
somebody happens to glance over.

Set `voice.speak_when: unfocused` and the answer, reasoning and activity
channels hold back while you are looking at the transcript, then take over the
moment you switch windows. Coming back stops the reading, because you are about
to do it faster. Alerts ignore the setting on purpose.

What you hear is a projection, not the transcript: paths collapse to their
filename, links become "a link", code fences are never spoken. The same
sentence measures 12.9 seconds raw and 6.1 projected. Each answer is read in
its own language, seeded from the language you wrote in so the opening sentence
is right too.

`/voice on` turns spoken output on for the session — no restart, and off by
default. `/voice view` opens the listening stage: one centred panel with the
state, the last line said out loud, and what happened, for a screen you glance
at rather than read. It is a router, not a viewer — every detail surface it
names already exists (`alt+d` diffs, `alt+o` output, `ctrl+f` search), `esc`
returns to the transcript, and it never hides an action or an error.

While the microphone is open you can also steer with a closed set of phrases —
"otra vez" repeats the last line, "callate" stops it, "mostrame el diff" opens
the diff, "volver" returns to the transcript. It matches whole utterances only,
so "mostrame el diff y arreglá el bug" is dictation, and nothing it reaches can
send a prompt, answer an approval or cancel a turn. A mis-hearing costs you a
screen, never a command.

If an approval is waiting and you open the microphone yourself, "aprobalo" or
"denegalo" answers it. sonar never opens the microphone on its own — that stays
a deliberate act — and voice can only ever allow once or deny: it cannot widen a
scope, and anything destructive is refused rather than downgraded, because the
keyboard is one reach away exactly when a mistake is not recoverable.

Everything is tunable from the session, by the same name it has in the config
file: `/voice provider say|openai`, `/voice speak_when always|unfocused`,
`/voice rate 195`, `/voice voice es Paulina`, `/voice pronounce deploy dipló`.
Nothing is persisted — tuning by ear produces a state nobody should inherit by
accident — so `/voice status` prints the config block that would reproduce it.

`/voice status` says what is on and whether this terminal reports focus,
`/voice voices` says which voice each language would use, and `/voice test`
speaks a line in each so you can hear one before choosing it. `/voice <channel>
on|off` retunes the mix for the session. `ctrl+g` or `/voice` dictates into the
composer instead, through a local transcriber — audio carries who else is in
the room, which is a different decision from sending text.

A bilingual session is Spanish grammar around English nouns, and a Spanish voice
reads those nouns with Spanish rules — "merge" becomes "MER-je", "package"
becomes "pa-KA-je", "git" becomes "jit". Those are different words, not an
accent, so sonar respells them before speaking. The table is a set of guesses
that no measurement can settle, which is why `/voice test` speaks a line built
out of them and `voice.pronounce` overrides any entry that sounds worse.

If that still is not enough, `voice.provider: openai` swaps the local `say` for
a hosted engine that handles the mixture natively — measured, it returned every
technical term intact where `say` returned "el merch … el caché … confit". It
costs money per turn, needs `OPENAI_API_KEY`, and puts about two seconds
between a sentence and its audio, so it is off by default. Under it no language
is sent and no respelling is applied: telling an engine the language is what
makes it read English words with Spanish rules.

One thing worth doing once: macOS ships the **compact** version of every voice,
and the compact ones are the robotic ones. Downloading the better variant
(System Settings → Accessibility → Spoken Content → Manage Voices) changes how
this sounds more than any setting. `/voice status` tells you when you have none.

## Safety

Read this before `--skip-approvals` or a long AUTO run.

- **This is not a sandbox.** Tools run as your user. The workspace boundary,
  the ignore policy, and the approval model are the controls; there is no
  kernel-level isolation.
- **AUTO is scoped, not unrestricted.** A bounded shell subset runs without
  asking; anything else is approval-gated, and every dispatch is recorded in a
  durable ledger before it runs.
- **Approvals are auditable.** Each prompt names the rule the request tripped,
  and grants are scoped — one path, one command prefix, one MCP tool — never
  "allow everything".
- **`privacy.local_only` bounds tool endpoints, not inference.** It refuses
  remote MCP servers. It cannot make sonar private: every request leaves your
  machine by construction, because that is what a hosted model is.
- **Your prompts, code, and tool output go to the provider.** Check their
  retention policy before pointing sonar at anything you cannot send.

## Why this is a fork and not a config profile

DeepSeek advertises an OpenAI-compatible endpoint, so pointing a generic client at `api.deepseek.com` looks like it should just work. It does — for exactly one turn. Three parts of the contract differ in ways that break an agent loop:

| | Generic OpenAI assumption | DeepSeek |
| --- | --- | --- |
| Turning reasoning off | `reasoning_effort: "none"` | `{"thinking": {"type": "disabled"}}` — `none` is not a valid effort, and effort cannot switch thinking off |
| Reasoning default | off | **on** |
| Tool-call turns | assistant message carries `content` + `tool_calls` | must also echo its own `reasoning_content` back, or the API returns **400** |

That last one is the killer. An agent is mostly tool calls, so a harness that streams reasoning to the screen and discards it — the ordinary thing to do — dies on the second iteration of every turn. sonar carries `reasoning_content` on the assistant message through the loop and replays it on the wire, while keeping it out of session, transcript, and checkpoint state.

`internal/llm/deepseek.go` holds the contract; `internal/llm/deepseek_test.go` pins each of the three behaviors so a refactor cannot quietly undo them.

## What changed from local-agent

- **Provider metadata is data, not code.** Endpoint, credential variable, default model, and context window all come from the embedded catalog. `deepseek` is the default and needs no configuration; six providers were verified to resolve complete profiles from their name alone.
- **Dialect is chosen from provider identity, not endpoint shape.** The catalog calls DeepSeek `openai-compat` — true about the URL, wrong about the contract. Dispatching on wire type alone builds a client that connects, answers once, then 400s on every tool-call turn.
- **`privacy.local_only` now bounds tool endpoints, not inference.** Every sonar request leaves the machine by construction, so gating the provider on it could only ever reject the one supported configuration. It still refuses remote MCP servers, which is a boundary that survives the change.
- **ICE retrieval ships off.** It needs an embeddings endpoint and DeepSeek's API has none. Enabling it means running a separate local embedding backend.
- **No public website.** The VitePress site, its CI job, and the link checker are gone.

## Cost

Per 1M tokens, regular rate: **$0.14** input on a cache miss, **$0.0028** on a cache hit, **$0.28** output.

A cache hit is ~50x cheaper than a miss, which makes prompt-prefix stability the single biggest cost lever in a long session. DeepSeek has announced peak-hour pricing at **2x every billing item** during 09:00–12:00 and 14:00–18:00 Beijing time (UTC+8), so treat any figure the harness shows as an estimate rather than a settled charge.

## Development

```bash
task build     # -> bin/sonar
task test      # go test ./...
task verify    # tidy, lint, vet, race tests, govulncheck
task glyphrun  # deterministic terminal specs, in a real PTY
task dev       # go run ./cmd/sonar
```

`specs/` drives the real binary in a pseudo-terminal and compares full-screen
snapshots, which is how layout and truncation regressions get caught. Its
fixtures scrub provider credentials from the environment, so a test run cannot
reach a metered endpoint with your real key.

Contributor guidance, including the parts that are easy to get wrong, is in
[AGENTS.md](AGENTS.md).

## Verified

Two providers complete real tool-call turns end to end, not just unit tests:

| Provider | Model | Result |
| --- | --- | --- |
| `deepseek` | `deepseek-v4-flash` | tool call → second iteration with `reasoning_content` echoed → receipt settled |
| `opencode-zen` | `deepseek-v4-flash-free` | tool call → receipt settled |
| `ollama` | `deepseek-v4-flash` | live turn against Ollama Cloud → receipt settled |

Ollama Cloud reports **no token usage**, even when the request asks for it with
`stream_options.include_usage`. Text, tool calls, and stop reasons all work;
the context meter and cost figures read zero, and a budget denominated in eval
tokens cannot bound a run there. Turn and wall-time budgets still can.

## Known gaps

- **Dialect coverage.** DeepSeek, the four anthropic-family providers, and the
  27 catalog providers on the plain OpenAI-compatible dialect work. **Google
  and the cloud-credential families (azure, bedrock, vertex) do not**, and need
  their own dialects.
- **No expert consultation.** It consulted several models from a local Ollama
  inventory, and there is no local inventory. Rebuilding it on multiple hosted
  profiles is possible but not done; `specs/pending/tui_agents.yml` holds the
  coverage.
- **Local-runtime code is still compiled in.** The native Ollama client and
  inventory remain in `internal/llm`, and UI files still reference them, but
  nothing reaches them: every provider is hosted. Removing them is an
  untangling rather than a deletion, because no file exists *only* for Ollama.
- **The catalog is a pinned snapshot.** `sonar providers refresh` does not
  exist yet.
- **No published binaries yet.** The release pipeline is wired
  (`.goreleaser.yaml`, triggered by a `v*` tag, Homebrew upload skipped when no
  tap token is present) but nothing has been tagged. Build it yourself with
  `task install`.
