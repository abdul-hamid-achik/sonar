---
title: Sessions, memory, and ICE
description: Understand lossless session restore, checkpoints, workspace-scoped memory, and optional Ollama-powered ICE retrieval.
outline: deep
---

# Sessions, memory, and ICE

Local Agent separates resumable conversation state from optional cross-session retrieval.

## Sessions

`/sessions` and its `/resume` alias open SQLite-backed sessions for the current
workspace. A restored session includes both visible and model-facing state:

- transcript and tool-call identities;
- mode, model pin, and profile;
- token counters and tool receipts;
- path-free image attachment metadata for resumable vision turns;
- bounded file.cheap artifact receipts;
- durable goal state when present.

Loading a session replaces the active conversation. It does not merge two
transcripts. If the current composer contains text or pending images, the TUI
first presents one atomic choice: keep both with the restored session, discard
both, or cancel. A failed restore leaves the original draft, cursor position,
and image order unchanged. A separately held recovery follow-up must first be
resolved with Up or Escape, so unsent work cannot cross session boundaries.

The same lossless restore path is available at TUI startup:

```bash
local-agent --resume a1b2c3d
local-agent --resume latest
```

Each durable session has a random 7-character hex public handle (for example
`a1b2c3d`). The Runtime view and session picker show the handle and generated
title after the first submitted turn; the TUI footer includes that identity when
the current responsive status layout has room. The title is derived from the
first meaningful task text, including the reviewed Task field in guided PLAN
mode. Commands accept the hex handle; bare integers and legacy `S`-prefixed ids
are rejected. JSON and export filenames retain numeric IDs.

An exact session must belong to the current canonical workspace. `latest` selects
that workspace's most recently updated session. The database title is restored
with the session, and an active session lease prevents two Local Agent processes
from resuming it concurrently. Startup restore retains the normal cloud-consent
and recovery checks but does not submit a prompt or automatically resume a
durable goal. `--resume` cannot be combined with headless `-p` or `--prompt`.

After a clean interactive exit, Local Agent restores the terminal and prints a
copyable resume command when the conversation has a durable session:

```text
Session a1b2c3d · Polish transcript UX
Resume this session with:
  local-agent --resume a1b2c3d
```

The message is omitted when no resumable session exists or the TUI exits with
an error.

List or export sessions outside the TUI with:

```bash
local-agent session list
local-agent session export --format both a1b2c3d
```

The default export directory is `./local-agent-audit-<public-id>/`, containing
`session-<id>.jsonl` and `session-<id>-summary.md` (numeric internal IDs remain
in export filenames). The Markdown Open Issues table and JSONL `open_issue`
records identify unresolved executions and give the exact
`execution recover` or `session repair` command. Exports are bounded debugging
artifacts and can include raw session content, receipt detail, and paths; review
them before sharing.

For an ordinary session with uncertain tool effects, inspect either one receipt
or the bounded pending set without retrying anything:

```bash
local-agent execution recover a1b2c3d EXECUTION_ID
local-agent execution recover a1b2c3d --all
```

The batch listing prints an exact pending-set digest. Batch apply requires the
complete `--all --apply --set-digest HASH` command and typed evidence printed by
the inspection; it aborts atomically if the set changed. If terminal ledger
effects are newer than the saved transcript, first reconcile every uncertain
execution, close the TUI, then run `local-agent session repair a1b2c3d`. Repair
re-derives the projection under an exclusive lease; it never retries a tool or
rewrites the immutable ledger. Goal-owned sessions use `goal show` and
`goal recover` instead.

`/artifacts` (or `/artifact`) lists completed file.cheap save receipts from the
active or restored transcript. The durable projection contains a host-derived
stash URI, file and byte counts, creation time, full content SHA-256, and static
secret-scan or indexing flags. Raw manifests, source paths, findings, and
provider error prose remain outside persisted UI state.

## Checkpoints

Checkpoints snapshot the current agent message history inside the active session:

```text
/checkpoint before-refactor
/checkpoints
/restore 42
```

Restore validates that the checkpoint belongs to the active session.

## Saved model preference

An explicit local-model selection made with `/model`, the model picker, or the
equivalent settings surface is remembered across process restarts. The
preference is user-scoped rather than workspace- or session-scoped and is
stored in the owner-private file:

```text
~/.config/local-agent/runtime-preferences.json
```

At startup, Local Agent restores the preference only when the current Ollama
inventory verifies that the model is local and manually selectable. An explicit
`--model` flag or agent-profile model takes precedence. A previous Ollama Cloud
selection never restores its conversation-only consent in a new process.
`/model auto` clears the saved manual preference before returning to automatic
local routing.

## Structured memory

The memory store is workspace-scoped and available even when ICE is disabled. NORMAL and AUTO can request explicit save, update, and delete tools; PLAN can recall.

The store uses owner-only files, interprocess locking, coherent reloads, and fail-closed corruption handling.

## Optional ICE retrieval

ICE embeds prior conversations with Ollama and retrieves relevant history for the same canonical workspace. It is enabled by default; set `enabled: false` to turn off background embedding and auto-memory entirely:

```yaml
ice:
  enabled: true
  embed_model: nomic-embed-text
```

Pull the embedding model before enabling it:

```bash
ollama pull nomic-embed-text
```

When enabled, ICE can retrieve similar prior messages and run bounded background extraction after completed turns. Background extraction is single-flight, yields to foreground inference, and writes only to the current workspace's memory store.

## Storage

```text
~/.config/local-agent/conversations.json
~/.config/local-agent/memory/<workspace-hash>.json
~/.config/local-agent/local-agent.db
~/.config/local-agent/runtime-preferences.json
~/.config/local-agent/images/
~/.config/local-agent/logs/
```

The image directory is an owner-private, content-addressed object store. Session
and checkpoint JSON keep the sanitized name, MIME type, byte size, dimensions,
and full SHA-256 reference needed to rehydrate an attachment; they do not store
the original path or raw bytes. `/image forget-history` removes references from
conversation history but deliberately does not delete cached objects or pending
attachments. Existing checkpoints remain immutable and can reintroduce their
historical references if restored.

SQLite migrations are applied transactionally and recorded with source
checksums. Startup refuses a migration whose recorded checksum no longer
matches the embedded source.

ICE currently uses a bounded flat JSON scan rather than an approximate-nearest-neighbor index.

## Legacy data

Historical global entries without trustworthy project provenance remain quarantined. They are not loaded into repository-scoped memory, do not appear in the command surface, and do not produce routine startup notices.
