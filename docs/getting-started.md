---
title: Getting started
description: Install local-agent, connect Ollama, and complete a first approval-gated coding session.
outline: deep
---

# Getting started

Install `local-agent`, connect it to Ollama, and open a repository. The basic local model workflow does not require a configuration file.

::: warning Alpha software
Run Local Agent in a clean Git worktree, read approval requests, and review the resulting diff. The current safety layer is useful, but it is not an operating-system sandbox.
:::

## Requirements

- macOS or Linux
- [Go 1.25.12 or newer](https://go.dev/dl/) when installing from source
- [Ollama](https://ollama.com/) running on the same machine
- A Git worktree for work you want the agent to inspect or change

Windows release binaries are not published yet.

## Install

On macOS, install the release cask:

```bash
brew install --cask abdul-hamid-achik/tap/local-agent
```

Or install the latest tagged Go release:

```bash
go install github.com/abdul-hamid-achik/local-agent/cmd/local-agent@latest
```

Or run a checkout directly:

```bash
git clone https://github.com/abdul-hamid-achik/local-agent.git
cd local-agent
go run ./cmd/local-agent
```

## Prepare Ollama

Start Ollama and pull a compact model:

```bash
ollama serve
ollama pull qwen3.5:2b
```

The 4B tier is optional and is the preferred installed tier for coding, debugging, review, and multi-step tool use:

```bash
ollama pull qwen3.5:4b
ollama list
```

Local Agent reads Ollama's live inventory. You do not need to duplicate every installed model in configuration.

## Start a first session

Open the repository you want to work in, then launch the TUI:

```bash
cd /path/to/your/repository
local-agent
```

Try a read-only request first:

```text
Explain the request flow in this repository and identify the tests that cover it.
```

Local Agent begins in **NORMAL** mode. Reads can proceed inside the workspace.
Mutating tools such as edits, writes, shell commands, and MCP calls require
approval by default. Switch to **AUTO** when you want validated workspace writes
and catalogued direct build, test, lint, formatting, and inspection commands to
proceed without repeated prompts. Task runners, package `run` targets, raw
`find`, `rg`, and `grep` shell commands, `go generate`, Git, and dangerous,
external, dynamic, or unknown effects remain gated.

To reopen saved work directly, pass a positive session ID or select the newest
session in the current canonical workspace:

```bash
local-agent --resume a1b2c3d
local-agent --resume latest
```

The TUI shows each session as a random 7-character hex handle such as
`a1b2c3d · title`. Commands accept that handle; `latest` selects the newest
current-workspace session.

Startup resume is available only in the interactive TUI, so it cannot be
combined with `-p` or `--prompt`. It restores state without sending a prompt or automatically
continuing a durable goal.

After a clean interactive exit, Local Agent prints the exact
`local-agent --resume <handle>` command for the active saved session. An unsaved
conversation and a failed TUI run do not print a resume command.

## Essential controls

| Key | Action |
|---|---|
| `enter` | Send the prompt, or queue one follow-up while a turn is running |
| `shift+enter`, `ctrl+j`, `alt+enter` | Insert a newline; use `ctrl+j` when the terminal cannot distinguish `shift+enter` |
| `shift+tab` | Cycle NORMAL, PLAN, and AUTO |
| `ctrl+o` | Open the live Ollama model picker |
| `ctrl+p` | Open session settings |
| `tab` | Complete commands, paths, and skills |
| `esc` | Close an overlay or inline form, cancel an approval, or cancel active work |
| `ctrl+c` | Quit |

The composer grows with wrapped text up to a terminal-height-aware limit, then
scrolls internally while keeping the cursor visible. When earlier or later
draft rows are outside the visible composer, a cue names the corresponding
`ctrl+home` or `ctrl+end` jump. Typed and pasted text follow the same layout.
The mouse wheel scrolls the conversation without moving the draft.

Selecting a verified local model with `/model <name>` or the model picker saves
the pin for the next process start. `/model auto` clears it. A CLI `--model`
selection and agent-profile selections take precedence, and conversation-only
Cloud consent is never saved.

Inside the inline approval surface:

| Key | Decision |
|---|---|
| `n` | Deny (default when you press Enter) |
| `y` | Allow once |
| `s` | Allow the same request again this process (exact arguments) |
| `a` | Context-sensitive widen: tool again (`write`/`edit`/`mkdir`), bash command prefix, or this MCP tool again (process-local) |
| `p` | For `write`/`edit`/`mkdir`: this path again this process |
| `w` | For bash/MCP: save a durable workspace rule (survives restarts for this workspace) |
| `d` | Inspect exact arguments |
| `esc` | Cancel the approval and the active turn |

Use `/permissions accept-edits on` when you want workspace file edits to stop
prompting for the rest of this process without enabling shell or MCP bypass.
`/permissions` lists posture, session grants, and durable workspace rules.

Durable workspace rules (Claude-style “don’t ask again” for this repo):

```text
/permissions allow-bash go test
/permissions allow-bash "git status *"
/permissions allow-mcp mcphub__mcphub_list_servers
/permissions allow-path src/app.go
/permissions forget-bash "git status *"
/permissions forget-mcp mcphub__mcphub_list_servers
/permissions forget-path src/app.go
```

On an approval prompt, `w` saves a durable rule for the current action:

- bash → prefix or `prefix *` when there are extra args
- MCP → exact `server__tool` name
- write/edit/mkdir → that workspace-relative path (covers write, edit, and mkdir)

Rules live under `~/.config/local-agent/workspace-rules/` (not in the repo).
They never reintroduce a broad permanent tool-name allow. Bash globs only allow
a trailing `*`; compound commands with `&&`, pipes, or `$` still prompt.

Manage interactively via **Settings → Permissions**, or:

```text
/permissions panel
/permissions export                         # → ./local-agent-permissions.json
/permissions export ~/Desktop/rules.json
/permissions import ~/Desktop/rules.json
/permissions import --replace ~/Desktop/rules.json
/permissions clear-rules
```

Export files are portable JSON (bash patterns, MCP tools, relative write paths).
Import merges by default; `--replace` swaps the workspace rule set.

## Attach an image

Add a PNG, JPEG, or GIF to the next ordinary prompt with `/image` (or the
`/attach` alias), then type the question you want to ask:

```text
/image ./screenshots/failing-layout.png
What is causing the alignment problem?
```

You can also drag or paste one supported image path, or a complete
space/newline-separated list of paths. Local Agent validates the complete list,
keeps attachment order, and de-duplicates identical image content; mixed prose
remains draft text. Each prompt accepts up to four images. Use `/image list` to
inspect pending attachments and `/image clear` to remove them. An unpinned
session selects an admitted, auto-routable vision-capable Ollama model without
implicitly selecting a manual-only Cloud model; a pinned non-vision model fails
locally before a provider request starts.
If an older stored image is unavailable, the draft is restored; use
`/image forget-history` to remove active historical image context before
retrying. Existing checkpoints remain unchanged and can restore their refs.

On macOS, press `Ctrl+V` to read a clipboard image from the system pasteboard
and attach its PNG representation.
Bracketed terminal paste and other platforms can attach a saved image by
dragging it or pasting/copying its path. See
[Image attachments](./reference.md#image-attachments) for formats, limits, and
persistence details.

## Optional configuration

Repository-local configuration takes precedence over XDG and home configuration:

```bash
cp config.example.yaml local-agent.yaml
```

For a user-wide configuration:

```bash
mkdir -p ~/.config/local-agent
cp config.example.yaml ~/.config/local-agent/config.yaml
```

Continue with [Ollama models](./ollama-models.md), [remote providers](./providers.md) (xAI Grok / OpenAI-compatible + TinyVault), [authority modes and goals](./modes-and-goals.md), or the complete [configuration reference](./configuration.md).
