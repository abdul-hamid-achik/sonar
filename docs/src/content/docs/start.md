---
title: Getting started
description: Install sonar, give it a key, and run it.
---

## Install

sonar builds with [Task](https://taskfile.dev/):

```bash
task install     # compiles and puts `sonar` on your PATH
```

Or without installing it:

```bash
task build && ./bin/sonar
```

## Give it a key

```bash
export DEEPSEEK_API_KEY=sk-...
sonar
```

Without a key sonar exits `1` and names the missing variable. There is no
unauthenticated mode.

You can keep keys in a file instead:

```yaml
# sonar.yaml
credentials:
  env_file: ~/.config/sonar/env
```

Only names the provider catalog recognises as credentials are read from that
file, and never over an already-exported variable — a shared secrets file
usually also defines `PATH` and `EDITOR`, and applying those would corrupt the
process.

## The keys worth knowing first

| | |
| --- | --- |
| `shift+tab` | cycle NORMAL → PLAN → AUTO. PLAN is read-only; AUTO runs tools under a scoped-shell policy and still asks before anything outside it |
| `enter` while running | slash commands run immediately; other drafts queue for after the turn |
| `alt+d` | full diff of what the agent changed |
| `ctrl+f` | search the transcript |
| `ctrl+g` | dictate into the composer |
| `f1` | every key and command |
| `/mouse` | hand the mouse to the terminal for native select; `alt+m` is the same chord when the terminal actually sends it |
| `ctrl+y` | copy the selection, or the last reply when the draft is empty — including over tmux and SSH |

Run `/help` for the rest.

## Select and copy

Drag across the transcript to select; release copies. The wheel still scrolls.
Double-click a word, triple-click a line. `esc` clears the highlight. `ctrl+y`
copies the selection, or the last assistant reply when nothing is selected
and the draft is empty.

Mouse reporting is on so the wheel can scroll the chat. That is also what
stops the terminal from doing its own drag-select. `/mouse` (or `alt+m`)
hands the mouse back when you want the emulator's select instead; `pgup`/`pgdn`
still scroll.

Most terminals also hand the mouse back with a modifier while capture is on:
`shift+drag` in Ghostty, kitty, WezTerm, Alacritty and xterm; `option+drag`
in iTerm2. Terminal.app has none.

Stock macOS terminals compose Option+M into µ instead of sending `alt+m`.
`/mouse` is the binding that still works. To make `alt+m` itself work:

- Ghostty: `macos-option-as-alt = true`
- iTerm2: Profiles → Keys → Left Option key = Esc+
- Terminal.app: Profiles → Keyboard → Use Option as Meta key

`ctrl+y` writes the host clipboard and asks the terminal to do the same, so a
tmux or SSH session still lands the text on the machine you are looking at.

## Long unattended runs

`tools.auto_max_iterations` bounds a single provider segment; AUTO chains
segments after it fires. The ceiling on the whole turn is
`tools.auto_max_segments` and `tools.auto_max_wall_time` — up to 512 segments
and 24 hours.

`tools.approval_timeout` decides what an unanswered approval does when nobody is
watching. It **refuses and continues** rather than cancelling: the model sees
the refusal and takes another route. A timeout can only ever withhold
permission, never grant it.
