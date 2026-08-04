# sonar

A terminal coding agent pinned to a single model: **DeepSeek V4 Flash**, over the API, with an API key. No local runtime, no model picker, no provider shopping.

Forked from `local-agent` and cut down. The agent loop, tool dispatch, permission model, durable goals, session store, and MCP surface came across intact; the local-first inference machinery did not.

```
$ sonar

  ◜◝ sonar
  deepseek-v4-flash · 1M ctx · thinking
  $0.14/$0.28 per 1M · key: DEEPSEEK_API_KEY

› refactor internal/llm to drop the inventory layer
```

## Quick start

```bash
export DEEPSEEK_API_KEY=sk-...
task build
./bin/sonar
```

Without a key, sonar exits `1` and names the missing variable. There is no unauthenticated mode.

Configuration is optional — the defaults already resolve to DeepSeek. See `config.example.yaml` for the full surface.

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

- **One provider.** `deepseek` is the default and needs no configuration. `deepseek-v4-flash` is the only accepted model — context policy, cost display, and thinking defaults are all calibrated to one published set of numbers, so silently running another model would invalidate all three.
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
task dev       # go run ./cmd/sonar
```

## Known gaps

- The Ollama adapter, model inventory, model picker, and pull/cloud-consent UI are still compiled in and reachable via an explicit `provider: {type: ollama}`. They are dead weight for an API-only harness and are the next thing to remove — roughly 49 files in `internal/ui` still reference them.
- `docs/` still carries prose inherited from local-agent and has not been rewritten for sonar.
