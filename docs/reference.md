---
title: Command reference
description: Find Local Agent CLI commands, slash commands, keyboard shortcuts, and automation boundaries in one place.
outline: deep
---

# Command reference

Local Agent has three operator surfaces: the interactive TUI, human-readable headless prompts, and durable-goal lifecycle commands.

## CLI

| Command | Purpose |
|---|---|
| `local-agent` | Open the TUI in the current workspace |
| `local-agent -p "prompt"`, `local-agent --prompt "prompt"` | Run one human-readable NORMAL prompt |
| `local-agent --plan --prompt "prompt"` | Run one read-only PLAN prompt; equivalent to `--mode plan` |
| `local-agent --auto --prompt "prompt"` | Run one proactive AUTO prompt; equivalent to `--mode auto`, with routine confined workspace actions pre-authorized |
| `local-agent --mode <normal\|plan\|auto> --prompt "prompt"` | Select headless authority explicitly |
| `local-agent --tools read,diff --plan --prompt "prompt"` | Narrow one headless turn to the named built-in tools |
| `local-agent --model <name>` | Select and pin the initial Ollama model |
| `local-agent --agent <name>` | Select the initial agent profile |
| `local-agent --resume <a1b2c3d\|latest>` | Open the TUI and restore an exact or newest current-workspace session |
| `local-agent --qwen-router` | Enable the experimental Qwen-specific router |
| `local-agent --skip-approvals` | Skip approval prompts while preserving explicit denies and host/tool boundaries |
| `local-agent --yolo` | Deprecated compatibility alias for `--skip-approvals` |
| `local-agent init [--force]` | Create a project `AGENTS.md` |
| `local-agent logs` | List recent structured logs |
| `local-agent logs -f` | Follow the latest log |
| `local-agent session list [--json] [--limit N]` | List sessions with short handles and titles |
| `local-agent session export [--format jsonl\|md\|both] [--out DIR] <a1b2c3d>` | Export one bounded session audit projection |
| `local-agent session repair [--json] <a1b2c3d>` | Repair one session projection from durable execution records |
| `local-agent --version` | Print the build version |

`-p` and `--prompt` are exact aliases for a human-readable convenience surface,
not a stable JSON event protocol. `--auto` and `--plan` require a non-empty
prompt, are mutually exclusive, and reject a conflicting explicit `--mode`.
AUTO does not imply the broader `--skip-approvals` posture. It pre-authorizes a
bounded catalog of confined workspace actions, while dangerous, external,
dynamic, and unknown effects remain gated. Generic path-qualified workspace
executables remain outside that catalog. The sole exact exception is a bounded
query through the identity-verified `<workspace>/bin/minerva`; the host treats
that query as effectful workspace execution, not pure read. Minerva mutations,
persistent `mcp serve`, and delegated or broad diagnostics remain gated and
should use the exact trusted MCPHub route when available. An explicitly empty
or whitespace-only prompt exits with status 2 before configuration, network,
or provider startup.
In headless mode, requests that need an approval fail closed by default because
there is no approval UI.

`--tools` is headless-only and accepts an exact comma-separated list. Every
name must be a built-in tool already allowed by the selected mode; empty,
duplicate, unknown, and mode-disallowed names fail before configuration or
provider startup. The resulting turn exposes only that built-in subset, with
memory tools and MCP routes disabled. It is a tool-surface restriction, not an
authority grant.

`--resume` is interactive-only and cannot be combined with `-p` or `--prompt`.
Sessions are displayed with a random 7-character hex handle such as `a1b2c3d`;
session, goal, and recovery commands accept that handle. JSON remains numeric.
`latest`
selects the most recently updated session
whose canonical workspace matches the startup workspace. The restore runs after
TUI initialization and does not dispatch provider work by itself.

## Durable goals

Create a bounded headless goal without dispatching provider work, then run one
foreground turn:

```bash
local-agent goal open --objective "Finish the release audit" \
  --criterion "the audit findings are verified" \
  --max-continuation-turns 3 \
  --max-eval-tokens 1200
local-agent goal run <session-id> --prompt "Inspect the release and verify the criterion"
```

`goal open` prints the new session ID. `goal run` restores the goal and its
conversation, records its turn admission before dispatch, uses AUTO authority,
explicitly resumes a paused goal, and durably settles the turn before exiting.
It runs one turn in the foreground; it does not create a background daemon or
schedule another turn automatically.
Add `--skip-approvals` only when you intend to suppress approval prompts.

Inspect validated goal state for the current workspace with:

```bash
local-agent goal list --limit 20
local-agent goal show <session-id>
local-agent goal pending <session-id>
local-agent goal recover <session-id>
```

Add `--json` for machine-readable inspection projections and `goal open` output.
The default `goal recover` is a dry run. Applying typed recovery evidence
requires the complete explicit form printed by `--help`; there is no force
flag. Successful reconciliation pauses or exhausts the goal and never schedules
provider work by itself.

An ordinary session with an outcome-unknown tool receipt uses a separate exact-execution workflow:

```bash
local-agent execution recover <session-id> <execution-id>
local-agent execution recover <session-id> --all
```

Inspection is read-only. Applying evidence requires the exact revision and event ID printed by inspection plus a typed observation, source, reference, summary, and observation time. It records an immutable reconciliation receipt; it never retries the tool or rewrites its original outcome.

`--all` lists the bounded pending set and an exact set digest. To record the
same typed evidence for the reviewed set, use the complete command it prints:
`--all --apply --set-digest HASH` plus every evidence field. The operation
aborts atomically if the pending set changed. A batch is limited to 100 pending
executions; larger backlogs must be recovered individually first.

`session export` defaults to `both` formats under `./local-agent-audit-N/`,
writing `session-N.jsonl` and `session-N-summary.md`. The Markdown Open Issues
table and JSONL `open_issue` records contain exact recovery or projection-repair
commands. Review exports before sharing because bounded raw session state,
receipt detail, and paths may be present.

`session repair` re-derives a stale execution projection from terminal durable
ledger events. Close the interactive session and reconcile all outcome-unknown
executions first. It never retries tools or rewrites ledger history. Goal-owned
sessions instead use `goal show` and `goal recover`.

## Conversation commands

| Command | Purpose |
|---|---|
| `/help` | Open keyboard and command help |
| `/clear`, `/new` | Start a clean conversation |
| `/plan [task]` | Enter PLAN and open the guided read-only Task/Scope/Focus form, optionally prefilled |
| `/model`, `/models` | Open the live Ollama inventory |
| `/model list` | List currently admitted models |
| `/model <name>` | Switch to and pin a model |
| `/model auto` | Release the pin and resume local automatic routing |
| `/theme` | Open the color-scheme picker |
| `/theme list` | List the built-in color schemes |
| `/theme <name>` | Switch color scheme (remembered for the next start) |
| `/provider` | Open the inference provider picker |
| `/provider list` | List configured provider profiles |
| `/provider <name>` | Switch provider profile (persisted for restart) |
| `/agent [name\|list]` | List or switch agent profiles |
| `/skill`, `/skill list` | List discovered skills and their activation state |
| `/skill activate <name>`, `/skill deactivate <name>` | Add or remove one skill from active prompt context |
| `/load <path>`, `/unload` | Add or remove one bounded Markdown context file |
| `/image <path>`, `/attach <path>` | Validate a PNG, JPEG, or GIF and attach it to the pending ordinary prompt |
| `/image list`, `/image clear` | Inspect or remove images attached to the pending prompt |
| `/image forget-history` | Remove historical image references without changing pending images or cached objects |
| `/scope list` | List process-local external exact-file and directory read grants |
| `/scope add-read <directory>` | Grant read-only access to one directory outside the writable workspace |
| `/scope remove-read <path>`, `/scope clear-read` | Revoke one or every external read-only grant |
| `/servers` | Show connected MCP servers and tool count |
| `/ice` | Show optional ICE status |
| `/sessions`, `/resume` | Open the saved-session picker; neither command accepts an ID argument |
| `/artifacts`, `/artifact` | List bounded file.cheap stash receipts in the current session |
| `/changes` | List files modified in the current TUI session |
| `/stats` | Show in-memory token and context counters |
| `/checkpoint [label]` | Save current agent history |
| `/checkpoints` | List checkpoints |
| `/restore <id>` | Restore a checkpoint from the active session |
| `/recover` | Review the current session's outcome-unknown execution and record typed evidence |
| `/exit` | Quit gracefully; when the session is resumable, print its resume command after restoring the terminal |

Manual selections of verified local models are remembered across process
restarts. `/model auto` clears that preference. An explicit `--model` or
agent-profile model takes precedence, and conversation-scoped Cloud consent is
never restored implicitly.

Slash commands use a small argument parser rather than a shell. Single or
double quotes and backslash-escaped whitespace group arguments; environment
variables and command substitutions remain literal. `/load`, `/image`, `/scope`,
`/import`, and `/export` separately expand a leading `~/`. An unterminated quote is rejected
before command dispatch. Documented arity is enforced: commands with no
arguments and `/goal show`, `/goal pause`, `/goal resume`, `/goal budget`, or
`/goal drop` reject trailing fields, while `/restore` accepts exactly one
canonical positive decimal ID. `/load` accepts a regular, non-symlink Markdown
file up to 32 KB.

## Goal commands

| Command | Purpose |
|---|---|
| `/goal <duration> <prompt>` | Infer bounded criteria and start a concrete goal; ambiguous prompts ask one follow-up |
| `/goal [objective]` | Open the inline review, optionally prefilled; bare `/goal` shows the active goal when one exists |
| `/goal new [objective]` | Open an inline, editable goal review below the transcript |
| `/goal show` | Inspect objective, criteria, proof, state, and budgets |
| `/goal pause` | Stop host-initiated continuation |
| `/goal resume` | Explicitly request one user-directed continuation |
| `/goal budget` | Change limits without editing the immutable goal |
| `/goal drop` | Abandon work without claiming completion |

Use Go duration syntax such as `30m` or `1h30m`. Duration-shaped but invalid input is rejected explicitly.

## Import, export, and Git

| Command | Purpose |
|---|---|
| `/export [--force] <path>` | Atomically export an owner-private Markdown transcript envelope |
| `/import <path>` | Import a supported transcript into a new session |
| `/commit [context]` | Generate a message from staged changes and run a constrained commit |

`/commit` intentionally disables Git hooks, signing, fsmonitor helpers, pagers, and automatic maintenance for its subprocess. Run `git commit` yourself when hooks or signing are required.

## Keyboard

| Key | Action |
|---|---|
| `enter` | Send the draft, or queue one follow-up while a turn is active |
| `shift+enter`, `ctrl+j`, `alt+enter` | Insert a newline without sending |
| `shift+tab` | Cycle NORMAL, PLAN, and AUTO without opening a form |
| `ctrl+p` | Open session settings |
| `ctrl+o` | Open the Ollama model picker |
| `ctrl+g` | Open the Agent Hub |
| `ctrl+f` | Search the safe transcript projection |
| `f1`, `ctrl+h` | Open Help when the composer is empty (`f1` is unambiguous on legacy terminals) |
| `tab` | Complete commands, files, and skills |
| `up`, `down` | Browse input history |
| `pgup`, `pgdown` | Scroll the conversation without editing the draft |
| `ctrl+u`, `ctrl+d` | Edit the draft; with an empty or unavailable composer, scroll by half a page |
| `end` | Jump to the latest conversation output when the composer is empty or unavailable |
| `ctrl+b`, `ctrl+r` | Toggle all tool details or the focused/latest tool |
| `alt+o`, `alt+d` | Open retained output or a diff for the inspected tool when available |
| `ctrl+t` | Toggle model thinking display |
| `ctrl+y` | Copy the latest response |
| `ctrl+e` | Edit input with `$VISUAL`, then `$EDITOR` |
| `ctrl+k` | Toggle compact transcript layout |
| `esc` | Close an overlay or inline form, cancel an approval, or cancel active generation |
| `ctrl+n`, `ctrl+l` | New conversation or clear the view |
| `ctrl+c` | Quit |

The supported minimum terminal is 30 columns by 12 rows. Compact status rows
retain skipped-approval, unavailable-MCP, and Cloud/Remote boundaries instead
of truncating the rightmost state. At that minimum, file approvals preserve an
identifying target tail and show explicit paging plus exact-argument controls.
Below the minimum, input is paused except for `ctrl+c`. After resizing, Local
Agent waits for terminal input to become quiet and asks you to press `enter` to
re-arm it; that gesture is consumed before the unchanged composer, overlay, or
pending authority decision returns.

The composer grows for soft-wrapped typing and text paste until its adaptive
visible-row cap, then scrolls internally. A visible cue reports whether earlier
or later draft rows are hidden and names the corresponding `ctrl+home` or
`ctrl+end` jump. The mouse wheel scrolls the transcript without moving the
composer cursor or its internal viewport; a visible document overlay owns the
wheel while it is open. Press `end` to resume following the latest output only
when the composer is empty or unavailable. Local Agent enables terminal mouse
reporting so wheel events reach the transcript. Use the terminal's selection
override, commonly `shift+drag`, to select transcript text; `ctrl+y` remains the
application-level copy shortcut for the latest response. Use `pgup`/`pgdown`,
`ctrl+b` and `ctrl+r` for transcript and tool navigation. With an empty composer,
`ctrl+u`/`ctrl+d` also scroll half a page; while drafting they retain their
standard editing behavior.

`ctrl+f` opens a bounded, case-insensitive transcript search without changing
the current draft. Use `enter`, `down`, or `ctrl+n` for the next match and
`shift+enter`, `up`, or `ctrl+p` for the previous match. `esc` closes search and
restores the prior composer focus, follow mode, and semantic reading position.
The index is built only from the safe UI projection: private model reasoning,
raw tool arguments/results, and raw MCP structured content are not searchable.

Click a completed `Thought` header to expand or collapse only that reasoning
entry. `ctrl+t` toggles all completed model thinking. Expanding an entry
preserves a paused transcript position.

`ctrl+e` resolves `$VISUAL` before `$EDITOR`, accepts a quoted executable path
and arguments without invoking a shell, and treats an empty saved file as an
intentional draft clear. Invalid UTF-8 or output beyond the composer character
limit is rejected without replacing the existing draft.

## Image attachments

Attach an image to the pending ordinary prompt with either command:

```text
/image ./screenshots/design.png
/attach "/path/with spaces/capture.jpg"
```

`/image list` shows pending attachments; `/image clear` removes them. Local
Agent also recognizes an ordered list of PNG, JPEG, or GIF paths delivered in
one terminal paste. Paths may be separated by whitespace or newlines and may be
quoted, shell-escaped, or expressed as local `file://` URLs, so dragging one or
several files works when the terminal inserts their paths as text. The complete
paste must contain image paths only: a path mixed with prose remains ordinary
draft text. Duplicate paths are removed before admission, and images with the
same validated content are attached only once. At most the first four available
slots are queued; Local Agent reports any additional files it skips.

When a turn is already running, sending a follow-up moves its text and pending
images into one visible queue item. Editing or an ordinary failed turn restores
both to the composer in their original order; a successful dispatch consumes
them together. If the active prompt is rejected before inference begins, Local
Agent keeps that retry and the queued follow-up as two separate visible owners.
Press Up to swap which one is editable or Escape to clear the held queue; it is
never merged or auto-dispatched. Resolve that held queue before opening another
saved session or starting a new conversation.

Admission is bounded before provider dispatch:

- at most four images per ordinary prompt;
- at most 12 referenced images, 40 MiB, and 48 million decoded pixels across the active conversation;
- PNG, JPEG, and GIF files only;
- at most 20 MiB per image;
- at most 16,384 pixels on either side and 24 million pixels total.

Local Agent decodes the complete file, not only its header. It then copies the
validated bytes into the owner-private, content-addressed store at
`~/.config/local-agent/images/`. Session and checkpoint JSON contain only the
sanitized basename, MIME type, byte size, dimensions, and complete SHA-256
reference. They do not contain the source path or raw bytes. A restored turn
loads the referenced object from the private store and verifies its size and
digest before sending it.

On macOS, explicit `Ctrl+V` reads a clipboard image from the system pasteboard,
normalizes it to PNG, and admits it through the same limits and private store.
A terminal bracketed-paste event contains text only; on other platforms, drag a
saved image or paste its file path.

If a referenced private object is unavailable, provider inference does not
start and the draft is restored. Run `/image forget-history` to remove every
historical image reference and its visible badge, then retry. The repair is
saved to an active durable session; it leaves pending attachments and cached
objects unchanged. Existing checkpoints remain immutable snapshots and can
reintroduce their image references when restored.

For an unpinned model, Local Agent chooses an admitted, auto-routable
vision-capable Ollama model before the turn. Manual-only Cloud models are never
selected implicitly. If the current model is pinned and does not advertise
vision, the turn fails locally before provider dispatch; select a vision model
or run `/model auto`. Image attachments are not accepted while the host-owned
Goal Runtime is active.

Set `LOCAL_AGENT_REDUCED_MOTION=1` to replace active animations with static
state indicators.
