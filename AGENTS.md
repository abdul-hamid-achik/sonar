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

sonar has no website and no `docs/` tree. `README.md` and
`config.example.yaml` are the documentation. A `docs/` inherited from upstream
lived here until it was removed — every page described a different product and
told the reader to install it. Wrong documentation is worse than none, so if a
doc tree returns, it is written for sonar or it does not ship.

Keep tracked files free of maintainer-specific paths, usernames, host names,
and private tool inventories. Examples use neutral defaults such as
`~/.config/sonar/env`. ADRs live outside this repository.

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
| OpenAI-compatible | 27 of the catalog's 40 |
| — | google and the cloud-credential families still need dialects |

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
- **drift/** — Compares this repository against `local-agent` on a pinned list
  of packages that must stay identical.
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
