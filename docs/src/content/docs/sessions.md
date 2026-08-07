---
title: Sessions, checkpoints and export
description: Going back, picking up where you left off, and taking the transcript with you.
---

Sessions are stored in SQLite alongside permissions, usage, execution events and
goal projections.

```
/sessions            # browse and restore
/checkpoint [label]  # save a point to come back to
/checkpoints         # list them
/restore <id>        # rewind the conversation to one
```

From the command line, `--resume latest` reopens the most recent session and
`--resume <id>` a specific one. The `session` subcommand lists, exports and
repairs saved sessions without the TUI.

## Export and import

```
/export <path>       # readable Markdown plus a typed v2 transcript
/import [path]       # bring one back into a fresh session
```

The export is two things at once on purpose: Markdown a person can read, and a
typed transcript a machine can reload without inferring structure from prose.

## Recovering an execution with no outcome

If sonar is killed between dispatching a tool and recording what it did, that
execution has no recorded outcome — and inventing one would be worse than
leaving the gap. `/recover` reviews those and records typed evidence for what
actually happened.

The `execution` subcommand does the same from the command line, which is what a
supervisor or a cron job would use.

## Context

`/context` shows how much of the window this session is using and can save a
preference for it. When a turn approaches the limit, sonar compacts earlier
turns rather than truncating them, and reconciles the image references it still
has to keep visible.
