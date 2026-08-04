---
title: Architecture
description: Understand Local Agent's Go and Charm runtime, model manager, MCP policy, durable goal contracts, and local persistence.
outline: deep
---

# Architecture

Local Agent keeps the conversation surface, inference runtime, tool authority, and durable goal state separate.

```text
Charm TUI or headless output
          |
          v
Agent loop --------> prompt builder + mode policy
    |                         |
    |                         +-- AGENTS.md, skills, context, memory
    |
    +-- tool policy -> approval -> built-ins / MCP registry
    |                                    |
    |                                    +-- typed result -> semantic receipt
    |
    +-- expert selector -> resource plan -> tool-free advisory inference
    |
    +-- ModelManager -> Ollama inventory -> local or consented Cloud model

Goal Runtime -> budgets + permits + receipts -> optional Cortex advisor

Persistence -> scoped memory/ICE + SQLite sessions/ledger + logs
```

## Runtime boundaries

- `internal/agent` owns the ReAct loop, prompts, policies, and tool dispatch.
- `internal/llm` owns the Ollama client, live inventory, model admission, and per-request context policy.
- `internal/mcp` owns server connections, health checks, reconnection, and tool namespacing.
- `internal/ecosystem` turns exact, versioned companion-tool results into bounded semantic receipts.
- `internal/ui` owns the Charm interface and renders state from the host runtime.
- `internal/goal` owns durable goal state, budgets, receipts, and recovery values.
- `internal/goaladvisor` bounds the optional Cortex/MCPHub semantic adapter.
- `internal/expertselector` chooses Team, Swarm, and application-level MoE profiles locally and deterministically.
- `internal/resource` converts one process-visible CPU/RAM snapshot, including Linux cgroup v1/v2 limits, into conservative concurrency and fan-out limits.
- `internal/expertteam` runs bounded, tool-free advisory inference and returns host-bounded receipts to the parent agent.
- `internal/db` persists sessions, permissions, checkpoints, and execution evidence through sqlc-backed schema models and a transactional checksum ledger.

The model does not enforce its own mode. The host decides which tools are visible and rejects out-of-policy requests.

## Harness contract

The harness owns the turn loop, context construction, tool orchestration, approvals, persistence, recovery, and progress projection. Companion tools keep their own domain authority. For example, MCPHub decides how a downstream tool is routed, Bob decides whether a repository is clean or drifting, and Glyphrun or Cairntrace decides whether its verification run passed.

Local Agent therefore projects every tool receipt across three independent axes:

- **Transport** — running, succeeded, or failed to return a receipt.
- **Domain** — succeeded, failed, blocked, drifted, conflicted, needs attention, or is not safely understood.
- **Evidence** — candidate, supported, verified, contradicted, or stale.

A successful MCP exchange is not proof that the requested operation succeeded. Exact structured output is interpreted inside the agent, then discarded; the TUI and saved session receive only the bounded semantic projection. Unknown schemas remain visible as attention states instead of being painted as successful verification. MCP tool-call arguments follow the same boundary: saved sessions retain only safe route identifiers, never arbitrary downstream payloads.

The transcript is the chronological source of truth. Composer-owned inline surfaces handle active authority decisions; overlays are reserved for temporary global selection or deep inspection. Local Agent does not duplicate every companion product into a persistent dashboard.

## Effect ordering

Top-level built-in, memory, and MCP calls execute deterministically in model order. Unknown MCP effects are not parallelized. The bounded exception is inside one read-only `consult_experts` call, where tool-free inference reports may run concurrently under the host resource plan. The append-only execution ledger records durable dispatch intent before an external effect begins, which makes uncertain outcomes visible after interruption instead of guessing that nothing happened.

## Goal supervisor boundary

The foreground Goal Runtime is implemented. The `internal/supervisor` and `internal/workunit` packages define safety-tested scheduling and specialist-admission contracts, but they are not wired as a headless queue or multi-process specialist runner.

Expert consultations are a separate runtime boundary. They fan out ordinary
read-only inference calls inside one foreground turn, do not create durable
work units, and give child experts no tool authority. The parent serializes
top-level consultations and keeps responsibility for effects and verification.

## Current limitations

- Ollama is the default inference adapter. Optional OpenAI-compatible remote providers (including xAI) are config-selected and credentialed via process environment / TinyVault injection.
- General model routing uses heuristics and a 16 GB-oriented admission guard. Expert concurrency separately uses live host telemetry where available and falls back to serial inference when memory confidence is insufficient.
- MCP support is currently tool-focused.
- ICE uses a flat workspace-scoped JSON store.
- In-flight effects do not continue automatically after a crash.
- The current product has no OS-level sandbox.
