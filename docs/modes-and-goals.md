---
title: Modes and durable goals
description: Use NORMAL, PLAN, and AUTO authority safely, then start bounded durable goals with an explicit command.
outline: deep
---

# Modes and durable goals

Modes describe host-enforced authority. Pressing `shift+tab` cycles the mode; it does not open a form, submit work, or create a goal.

## NORMAL

NORMAL is interactive work. The model can read the workspace and request edits, file operations, shell commands, memory changes, and MCP tools. Mutations remain subject to the approval policy.

Use NORMAL for a request where you want to stay in the loop turn by turn.

## PLAN

PLAN exposes workspace reads, search, listing, diff, existence checks, and memory recall. Mutations are rejected by the host even if the model asks for one.

Use PLAN to understand a repository, compare approaches, or produce a change plan without editing.

Run `/plan [task]` for a guided entrypoint. It switches to PLAN and opens the
inline Task/Scope/Focus form; submitting it requests an implementation-ready
plan with assumptions, affected components, dependencies, risks, acceptance
criteria, and verification checks. The host still exposes read-only authority,
and the form does not create a durable goal.

## AUTO

AUTO sends ordinary prompts directly with proactive access to the NORMAL tool
surface. Validated workspace writes and directory creation, exact trusted local
MCP routes, and a static catalog of direct ordinary toolchain commands can
proceed without interruption. Task runners, package `run` targets, generic
path-qualified workspace executables, raw `find`, `rg`, `grep`, `tree`, `du`,
and `ls` shell commands, and `go generate` remain approval-gated; use the
host-owned read, list, and grep tools for ignore-aware repository inspection.

Minerva has one deliberately narrow exception for local CLI verification. AUTO
may run a bounded query only from the physical `<workspace>/bin/minerva` binary
after verifying its exact Go main-package and module build identities. The host
classifies the call as effectful workspace execution, not a pure read, because
it still runs repository-owned code with Local Agent's process access. Mutation
commands, persistent `mcp serve`, and delegated or broad diagnostics remain
approval-gated. Ordinary Minerva product integration should use its exact
host-trusted MCPHub contract.

Raw Git also remains approval-gated because repository configuration, filters,
and hooks can execute programs even during commands that look read-only; use
`/changes` and `/commit` for the host-owned paths.

AUTO is not blanket shell authority. Deletion, dynamic expansion, file
redirection, external shell paths, explicit network CLIs or endpoints, unknown
commands, memory mutation, human decisions, and uncatalogued MCP effects still
stop for approval. Explicit external paths named in the prompt may receive a
temporary exact typed read/write scope; that scope never becomes shell
authority. AUTO uses a 40-iteration segment by default instead of NORMAL's 10.
Productive work reaching that watchdog emits a bounded continuation checkpoint
and resumes in a fresh segment; repeated/no-progress behavior still stops.
Configure the two segment ceilings independently with `tools.max_iterations`
and `tools.auto_max_iterations`.

AUTO classifies the outer command and visible operands; it is not process
containment. An admitted compiler or test command can still execute
repository-owned code with Local Agent's process access, so use AUTO only in
workspaces whose development commands you trust.

## Typed continuation actions

Trusted Bob and Cortex contracts can return an exact next action. Local Agent
normalizes the action into bounded host-owned fields, validates the tool and
arguments against the current registry, and preserves named missing inputs and
blockers. A downstream command string is never treated as shell authority.

The default `suggest` mode presents a valid action to the model and UI without
dispatching it. Optional `auto_read_only` can follow at most two exact, fully
specified, unblocked, registry-validated read-only actions while AUTO authority
is active. Shell, mutation, secreted execution, unresolved gateway proxy calls,
stale actions, and repeated action fingerprints never qualify. Every eligible
call still passes current route trust, workspace, permission, replay, and ledger
checks. These actions are suggestions or contracts; they do not grant authority
or make Local Agent reproduce Bob's ownership logic or Cortex's task state.

Switching modes never creates durable work. To start a bounded foreground goal, use `/goal <duration> <prompt>` or `/goal new`. Every durable definition must have:

- one objective;
- at least one independently checkable acceptance criterion; and
- at least one finite wall-time, evaluation-token, or automatic-turn limit.

`/goal <duration> <prompt>` is already an explicit creation instruction. A
concrete prompt starts directly; an ambiguous prompt asks one contextual
follow-up before anything runs. `/goal new` opens the inline manual review.
Free-form input such as `/goal improve the narrow model picker` opens the same
review prefilled. Bare `/goal` opens it when no goal exists and shows the active
goal otherwise.

## Headless goal turns

Create durable goal state without dispatching provider work:

```bash
local-agent goal open --objective "Finish the release audit" \
  --criterion "the audit findings are verified" \
  --max-continuation-turns 3 \
  --max-eval-tokens 1200
```

Use the returned session ID to run one explicit foreground turn:

```bash
local-agent goal run <session-id> --prompt "Inspect the release and verify the criterion"
```

The run uses AUTO authority under the configured approval policy. Local Agent
persists the turn admission before provider dispatch and stores the settled goal
receipt with the conversation afterward. The explicit command resumes a paused
goal, but refuses blocked or exhausted state. A command invocation runs one
turn; it does not detach or automatically start another turn. Use `goal show`
to inspect the resulting state before issuing another `goal run`.

## Compact goal command

You can start with a duration and prompt:

```text
/goal 30m fix the flaky session restore test and prove the fix
```

Local Agent deterministically infers bounded, prompt-specific acceptance criteria and starts the goal when the prompt names a concrete target. Obvious ambiguity such as `fix it` opens a focused follow-up instead. The duration must be a valid Go-style value such as `15m`, `1h`, or `1h30m`; invalid duration-like input is rejected rather than silently becoming part of the objective.

For an empty or partial draft:

```text
/goal new
/goal new improve the model picker on narrow terminals
```

## Continuation and completion

Automatic continuation is conservative:

- the previous turn must produce a successful tool receipt;
- the goal must still have budget;
- persistence must be healthy;
- a linked Cortex case must advance semantically before another host-initiated turn.

Budget exhaustion, cancellation, a failed turn, unavailable Cortex status, or an unproductive yield pauses the run. Stopping never means success.

When Cortex is unavailable, the bounded goal remains useful but each later turn requires explicit `/goal resume`, and Local Agent does not declare its own completion.

If Cortex asks a typed human question, the goal pauses and an inline Cortex
decision surface replaces the composer while the transcript stays visible.
Use `up`/`down` or `j`/`k` to inspect the options and `enter` to confirm one.
`esc` hides the presentation without answering or unblocking the goal. Recording
an answer updates Cortex; it does not resume Local Agent work. The first
`/goal resume` after an answer refreshes Cortex and clears the durable decision
blocker without dispatching provider work. If the fresh Cortex state leaves the
goal actionable and paused, the TUI asks for a second explicit `/goal resume`.
That command checks fresh state again and starts one user-directed turn only
when the goal still permits it; a blocked, complete, or abandoned case does not
start another turn.

## Goal controls

```text
/goal show     inspect criteria, proof, state, blockers, and budgets
/goal pause    stop automatic continuation
/goal resume   explicitly request one user-directed continuation
/goal budget   adjust limits without changing the immutable goal
/goal drop     abandon the goal without claiming completion
```

Goals run in the foreground. Closing the TUI cancels active work; it does not create a detached agent daemon.

## Recovery after uncertain effects

If a process stops after a side effect crossed the durable dispatch boundary but before its outcome was recorded, the goal becomes outcome-unknown. Local Agent will not retry that effect automatically.

Read-only CLI projections and an explicit evidence workflow are available for inspection and reconciliation. A successful reconciliation ends in PAUSED or EXHAUSTED and still requires a later explicit resume. See the [architecture](./architecture.md) for the durable execution contracts.
