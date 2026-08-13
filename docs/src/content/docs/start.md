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
| `/mouse` | turn mouse capture on for wheel-scroll and click-to-expand; `alt+m` is the same chord when the terminal actually sends it |
| `ctrl+y` | copy the last reply when the draft is empty — including over tmux and SSH |

Run `/help` for the rest.

## Select and copy

Drag-select is the default. Mouse reporting stays off so the terminal owns
press and release, the way any other program in a terminal works.

`/mouse` (or `alt+m`) turns capture on when you want the wheel to scroll the
transcript or a click to expand a tool card. `pgup`/`pgdn` always scroll.
While capture is on, most terminals still hand the mouse back with a
modifier: `shift+drag` in Ghostty, kitty, WezTerm, Alacritty and xterm;
`option+drag` in iTerm2. Terminal.app has none — toggle capture off again.

Stock macOS terminals compose Option+M into µ instead of sending `alt+m`.
`/mouse` is the binding that still works. To make `alt+m` itself work:

- Ghostty: `macos-option-as-alt = true`
- iTerm2: Profiles → Keys → Left Option key = Esc+
- Terminal.app: Profiles → Keyboard → Use Option as Meta key

`ctrl+y` copies the last assistant reply without the mouse. It writes the host
clipboard and asks the terminal to do the same, so a tmux or SSH session still
lands the text on the machine you are looking at.

## Long unattended runs

`tools.auto_max_iterations` bounds a single provider segment; AUTO chains
segments after it fires. The ceiling on the whole turn is
`tools.auto_max_segments` and `tools.auto_max_wall_time` — up to 512 segments
and 24 hours.

`tools.approval_timeout` decides what an unanswered approval does when nobody is
watching. It **refuses and continues** rather than cancelling: the model sees
the refusal and takes another route. A timeout can only ever withhold
permission, never grant it.
