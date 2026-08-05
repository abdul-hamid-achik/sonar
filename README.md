# sonar

A terminal coding agent for hosted models, over the API, with an API key. No local runtime.

MIT licensed. Built on [Charm](https://charm.land) and the [Catwalk](https://github.com/charmbracelet/catwalk) provider catalog.

**DeepSeek V4 Flash is the default, not the boundary.** Provider metadata comes from an embedded [Catwalk](https://github.com/charmbracelet/catwalk) snapshot — 40 providers, 1403 models — so selecting `groq`, `cerebras`, or `moonshot` needs a provider name and a key, not code.

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

## Known gaps

- **Dialect coverage.** DeepSeek, the four anthropic-family providers, and the
  27 catalog providers on the plain OpenAI-compatible dialect work. **Google
  and the cloud-credential families (azure, bedrock, vertex) do not**, and need
  their own dialects.
- **Inherited Ollama code.** The adapter and inventory are still compiled into
  `internal/llm` and reachable via an explicit `provider: {type: ollama}`. 23
  non-test UI files still reference it — no file exists *only* for Ollama, so
  removing it is an untangling rather than a deletion. Undecided whether it
  goes or stays supported.
- **The catalog is a pinned snapshot.** `sonar providers refresh` does not
  exist yet.
- **No published binaries yet.** The release pipeline is wired
  (`.goreleaser.yaml`, triggered by a `v*` tag, Homebrew upload skipped when no
  tap token is present) but nothing has been tagged. Build it yourself with
  `task install`.
