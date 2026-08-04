---
title: Local Agent ecosystem
description: Explore the focused local-first tools that connect to, verify, or complement Local Agent without blurring their ownership boundaries.
outline: deep
---

# Local Agent ecosystem

Local Agent is the conversation, model, permission, and foreground-goal host. The surrounding projects remain focused tools with explicit interfaces. They are not all bundled integrations.

The shared transcript vocabulary keeps those ownership boundaries legible:

| Tool | Product role | Evidence meaning |
| --- | --- | --- |
| MCPHub | Gateway and lazy catalog | Routing only |
| Cortex | Coordination, criteria, and evidence closure | Preserves claims and proof relationships |
| Vecgrep | Semantic discovery | Candidate |
| Codemap | Code structure and impact | Supported, or stale when its index reports drift |
| Glyphrun | CLI and TUI verification | Verified or contradicted from a versioned run |
| Cairntrace | Browser verification | Verified or contradicted from a versioned run |
| Vidtrace | Video evidence | Artifact-backed support |
| file.cheap | Artifact storage | Preserves evidence; does not create truth by itself |
| TinyVault | Secret boundary | Security state, never evidence content |
| Monitor | Ambient and requested observability | Operational support |
| Bob | Repository contract and convergence | No evidence upgrade; clean, drift, conflict, blocked, or failed domain state |
| VecLite | Vector storage | Internal capability, not an independent claim |

Local Agent shows these as semantic tool receipts in one chronological transcript. A green transport receipt is never substituted for a domain result or verification outcome.

## Connected runtime

### [Cortex](https://cortexai.tools/)

An evidence-guided engineering kernel for durable task state, bounded changes, and verification. Local Agent can use Cortex as the optional semantic advisor for durable goals, directly or through MCPHub. Local acceptance IDs and statements cross that boundary as typed, immutable `acceptanceCriteria`; they are never flattened into goal prose.

Exact Cortex actions can become bounded next-step suggestions. Local Agent
still owns tool resolution, schemas, effect classification, approval, budgets,
loop prevention, scheduling, and cancellation; it does not reproduce Cortex's
task lifecycle or evidence ledger.

### [MCPHub](https://mcphubcli.dev/)

A single configuration and gateway for discovering MCP servers and synchronizing them to agent harnesses. MCPHub is the recommended gateway for Local Agent; it does not replace Local Agent's final approval policy.

The [MCPHub website](https://mcphubcli.dev/) and [source repository](https://github.com/abdul-hamid-achik/mcphub) document the gateway, synchronization, and deployment workflow.

## Discovery and local data

### [Codemap](https://github.com/abdul-hamid-achik/codemap)

Structural code maps, references, impact, and index freshness. Local Agent treats fresh structural results as support, while missing or stale indexes remain visible instead of being interpreted as negative evidence.

### [Vecgrep](https://vecgrep.dev/)

Local-first semantic code search with Ollama embeddings, hybrid search, and an MCP surface. It is MCP-capable and related to Local Agent; it is not a native embedded search engine inside Local Agent.

### [TinyVault](https://www.tinyvault.dev/)

A local-first secrets manager and MCP server in a Go binary. It can be configured through MCP; it is not bundled.

### [file.cheap](https://file.cheap)

A local-first CLI and MCP file vault for storing, finding, compressing, and comparing workflow artifacts. It can be configured through MCP; it is not Local Agent's session store.

### [VecLite](https://veclite.dev)

An embeddable Go vector database with single-file storage, HNSW, BM25, and hybrid search. It is related infrastructure; Local Agent's current ICE store does not embed VecLite.

## Evidence and verification

### [Glyphrun](https://glyphrun.dev)

Black-box testing for CLI and TUI workflows over a real pseudo-terminal and deterministic emulator. Local Agent uses Glyphrun as test infrastructure for its terminal contracts.

### [Cairntrace](https://cairntrace.dev/)

Local-first behavioral browser specs for coding agents, with repairable steps and evidence-backed outcomes. It is a related verifier, not bundled into Local Agent.

### [Vidtrace](https://vidtrace.dev)

A local-first Go CLI that turns bug recordings into frames, OCR, transcripts, and timestamped evidence timelines.

## Operations and building

### [Monitor](https://monitorcli.dev)

A terminal system monitor for macOS and Linux with a TUI, JSON CLI, and MCP surface. It can be configured through MCP; it is not a built-in resource scheduler.

### [Bob](https://bobcli.dev/)

A deterministic, model-free repository factory that turns a `bob.yaml` contract into owned infrastructure and detects drift in CI. It complements Local Agent's coding workflow but is not embedded.

Bob is built to be driven by agents. Run `bob learn --json` once at the start of a session for a machine-readable onboarding brief: every command, the exit-code contract (0 success, 2 conflicts, 3 drift without conflicts, 4 invalid input; `plan` always exits 0), the stable `data.error.code` values, and the recipe catalog. Every `--json` failure envelope carries copy-pasteable corrective commands in `next_actions`, and every plan action carries a stable `code` field for branching without parsing prose.

Bob's `files` recipe lets an agent declare an arbitrary file tree in `bob.yaml` — paths, modes, contents, and literal `${vars.key}` substitution — and scaffold or reconcile any repository through Bob's plan/apply/lock conflict safety; `bob recipe show files --json` returns the schema with an example.

When Bob is reached through an exact trusted MCP route, Local Agent parses
`bob_context`, `bob_path`, and `bob_playbook` through their supported versioned
schemas. Context reports bounded repository and capability state; path reports
ownership and human-edit effects; playbook reports availability, blockers,
required inputs, scope, risk, and bounded steps. A lookalike route, malformed
document, future schema, or unknown closed value fails closed.

Every Bob result carries `EvidenceNone`: no evidence assessment. Bob can prove
whether repository files converge on its contract, but not whether application
behavior is correct. A Bob `clean` state is therefore visually and semantically
distinct from verified evidence or Cortex task completion.

Raw Bob structured output stays inside the parser boundary. Saved session state
contains only a bounded semantic digest; absolute workspace paths, arbitrary
reasons or commands, user values, manifests, previews, and file contents are
not persisted. A smaller validated projection may be supplied transiently to
the active model turn. Direct Bob calls and complete MCPHub stored results use
the same parser after the gateway pages have been bounded and matched to the
exact original call ID and route.

At session work time, a regular root `bob.yaml` is only a cheap candidate
signal. If one unambiguous eligible registered `bob_context` read is available,
Local Agent can suggest the compact read and cache its bounded digest. A unique
direct route is preferred; otherwise exactly one pinned MCPHub route is
required. With explicitly configured `auto_read_only` and an
AUTO turn, it may perform the read only if the current host policy proves the
target is read-only, non-destructive, idempotent, closed-world, workspace-bound,
and not denied. The automatic continuation budget never exceeds two calls.

Bob and Cortex actions share a bounded host projection, not product authority.
In the default suggestion mode, Local Agent exposes a valid next tool, named
missing inputs, and blockers without executing it. Optional read-only
auto-follow revalidates the route, current tool schema, workspace, effect,
approval policy, staleness, and replay fingerprint before each dispatch. Shell,
mutation, secreted, unresolved proxy, and prose-derived actions remain gated or
unsupported.

The [Bob website](https://bobcli.dev/) and [source repository](https://github.com/abdul-hamid-achik/bob) document its repository contracts and build workflow; the [agent guide](https://bobcli.dev/agents) is the onboarding reference for coding agents.

## Relationship rule

“MCP-capable” means a tool can be exposed through a configured MCP server. It does not mean Local Agent ships, authenticates, or silently trusts that tool. Every configured MCP call still crosses the Local Agent approval boundary.
