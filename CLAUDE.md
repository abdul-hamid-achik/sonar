# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This repository is private. There is no public website and no published
package; `docs/` is internal reference prose, still largely inherited from the
`local-agent` upstream and not yet rewritten for sonar.

## Provider Boundary

sonar runs hosted models over the API. **DeepSeek V4 Flash is the default, not
the boundary** — provider metadata comes from the embedded Catwalk snapshot in
`internal/catalog`, so adding a provider is data plus, only if its family is
new, a dialect.

Never enumerate provider types in a new allowlist. Ask
`config.IsKnownProviderType`. Three separate enumerations once demoted a valid
hosted provider to the local runtime path; that is why the predicate is
single-sourced. Three parts of DeepSeek's contract diverge from the generic OpenAI shape,
and all three are load-bearing — see `internal/llm/deepseek.go`:

- Chain-of-thought is toggled by `{"thinking": {"type": ...}}`, not by
  `reasoning_effort`. Effort only grades depth once thinking is on.
- Thinking defaults to **enabled**. Never assume a request is cheap.
- An assistant message carrying tool calls must echo its own
  `reasoning_content` back on every later request, or the API returns 400.
  `turnRuntime.lastReasoning` carries it through the loop; it is host-only and
  must never cross a session, transcript, or checkpoint boundary.

`internal/llm/deepseek_test.go` pins each behavior. Do not weaken those tests to
make a refactor pass.

`privacy.local_only` bounds **tool** endpoints (MCP), not inference. Every sonar
request leaves the machine by construction.

## Build & Development Commands

This project uses [Task](https://taskfile.dev/) as its build tool.

```bash
task build       # Compile to bin/sonar
task run         # Build + run
task dev         # Quick run via go run ./cmd/sonar
task test        # Run all tests: go test ./...
task lint        # Run golangci-lint run ./...
task verify      # tidy, lint, vet, race tests, govulncheck
task clean       # Remove bin/ directory
```

To run a single test:
```bash
go test ./internal/agent/ -run TestFunctionName
```

## Architecture

Go 1.25+ project implementing a terminal coding agent against a single remote
model. The TUI uses Charm v2; MCP servers extend the tool surface.

The Ollama adapter, model inventory, model picker, and pull/cloud-consent UI are
still compiled in and reachable through an explicit `provider: {type: ollama}`.
They are inherited dead weight, not a supported path, and are the next removal —
roughly 49 files in `internal/ui` still reference them.

### Package Layout (`internal/`)

- **agent/** — ReAct loop, prompt construction, mode policy, ordered tool dispatch, compaction, hooks, and checkpoints. Built-in, memory, and MCP effects execute deterministically in model order.
- **llm/** — DeepSeek adapter (`deepseek.go`) over a shared OpenAI-compatible transport, per-request expectations, and runtime switching. Inherited Ollama client and inventory still live here pending removal.
- **mcp/** — STDIO, SSE, and Streamable HTTP connections, namespaced tools, health checks, bounded results, and reconnects.
- **ecosystem/** — Exact companion-tool receipt parsers and bounded transport/domain/evidence projections. Raw MCP structured output must not cross into persisted UI state.
- **config/** — YAML/XDG loading, environment overrides, model preferences, routing, privacy policy, agent profiles, and ignore rules.
- **ice/** — Off by default: DeepSeek exposes no embeddings endpoint, so retrieval needs a separate local backend. Bounded JSON retrieval and single-flight background auto-memory.
- **memory/** — Workspace-scoped structured JSON memory under `~/.config/sonar/memory/<workspace-hash>.json`, with owner-only files, locking, and coherent reloads.
- **db/** — SQLite sessions, permissions, checkpoints, usage, execution events, control-plane records, and durable goal projections.
- **goal/** — Durable goal lifecycle, immutable criteria, budgets, permits, receipts, and evidence-backed recovery values.
- **goaladvisor/** — Bounded optional Cortex/MCPHub semantic adapter; it does not own scheduling or approvals.
- **controlplane/** — Append-only exception values and validation.
- **supervisor/** and **workunit/** — Tested scheduling/admission contracts; they are not wired headless or multi-process execution engines.
- **skill/** — Skill discovery and activation from sonar and shared `~/.agents` directories.
- **command/** — Canonical slash-command registry and hidden compatibility aliases.
- **ui/** — Bubble Tea v2 smart parent, Bubbles child components, transient overlays, model/goal/session flows, and Glamour rendering. `status_facts.go` assigns each ambient fact to one surface per frame; `theme.go` holds the contrast-checked color schemes; `content_grid.go` owns the single left-edge geometry.

### Request Flow

1. The TUI or headless controller submits user input under NORMAL, PLAN, or AUTO authority.
2. Project instructions, active skills, loaded context, workspace memory, and optional ICE retrieval form bounded prompt context.
3. `ModelManager` resolves the pinned DeepSeek model and freezes the expected context policy for that request.
4. The model response streams through the Agent loop.
5. Tool calls pass host mode policy, workspace validation, permission checks, and durable execution recording before dispatch.
6. Tool receipts return in model order; the loop continues within iteration, token, and context limits.
7. Completed session state is persisted. Background auto-memory yields to foreground inference and joins at shutdown.

An explicit `/goal` adds a durable Goal Runtime around this flow. Ordinary AUTO prompts remain direct and approval-gated. sonar owns budgets, permits, cancellation, approvals, persistence, and recovery. Cortex is optional bounded semantic input.

MCP transport success, domain success, and verified evidence are independent. Parse known structured contracts inside `internal/ecosystem`; unknown versions remain attention/unknown, and raw `StructuredContent` is discarded before the UI or session persistence boundary.

### Key Interfaces

- `llm.Client` — inference adapter contract (`ChatStream`, `Ping`, `Embed`).
- `agent.Output` — streaming and tool-state callbacks consumed by the UI/controller.
- `command.Registry` — canonical slash-command dispatch and completion metadata.
- Goal stores and execution repositories — durable state/evidence boundaries independent from presentation.

### Concurrency

Preserve each package's lock ownership and ordering. Inventory commits may wait for active inference and therefore run in a Bubble Tea command goroutine, never inside `Update`. MCP connections and health checks may run concurrently, but unknown tool effects do not. Auto-memory is single-flight, cancelled before foreground inference, and joined during shutdown.

### Configuration

The first matching file wins; files are not merged:

1. `./sonar.yaml`
2. `./sonar.yml`
3. `$XDG_CONFIG_HOME/sonar/config.yaml`
4. `$XDG_CONFIG_HOME/sonar/config.yml`
5. `$HOME/.config/sonar/config.yaml`
6. `$HOME/.config/sonar/config.yml`

Environment overrides apply afterward. Shared profiles live under `~/.agents/agents/<name>/agent.yaml`; shared skills live under `~/.agents/skills/<name>/SKILL.md`. See `config.example.yaml` and `docs/configuration.md` before changing precedence or paths.

## TUI Development Rules

- **Always use Charm libraries** for all TUI components: [BubbleTea v2](https://charm.land/bubbletea/v2), [Bubbles v2](https://charm.land/bubbles/v2), [Lip Gloss v2](https://charm.land/lipgloss/v2), [Glamour](https://github.com/charmbracelet/glamour).
- Prefer existing Bubbles components (spinner, viewport, textarea, textinput, list, table, paginator, progress, stopwatch, timer, key) over custom implementations.
- Follow the Charm "smart parent, dumb child" pattern: the main `Model` processes all messages; child components expose methods returning `tea.Cmd`.
- Colors come from `internal/ui/theme.go` through `semanticPalette`. Never hardcode ANSI colors, and never add a color meaning outside that vocabulary — a new scheme answers the existing ten roles.
- Pass the active `themeID` explicitly to every palette lookup. It is deliberately not a package global: the `ui` tests run in parallel, and helper signatures are non-variadic so the compiler catches a surface that would otherwise paint in the default scheme.
- Ambient state (model, remote boundary, context meter, mode) has exactly one owner per frame, assigned by `planStatus` in `internal/ui/status_facts.go`. A surface must ask before painting rather than guessing what another surface already showed.
- All content starts at the `ContentGrid` origin; column 1 is reserved for accent/chrome. Do not hand-write indents.
- Modals share one width scale and one anchor, and own the rows they cover. Do not add a per-overlay maximum width.
- Render cached content where possible to avoid per-frame re-rendering overhead.
