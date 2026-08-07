---
title: Durable goals
description: An objective with its own budget, criteria and receipts, that survives a restart.
---

An ordinary prompt is a turn: it runs, it ends, and what it did lives in the
transcript. `/goal` wraps that loop in a **Goal Runtime** — an objective that
owns a budget, immutable criteria, permits, and receipts, and that is written
down durably enough to survive the process exiting.

```
/goal 2h ship the release notes
/goal show
/goal pause
/goal resume
/goal budget
/goal drop
```

`/goal` with no arguments opens a reviewed form instead of guessing. A duration
in front of the objective sets its time budget.

## What the runtime owns

Budgets, permits, cancellation, approvals, persistence and recovery. Ordinary
AUTO prompts stay direct and approval-gated — attaching a goal is the deliberate
act that adds the rest.

The criteria are **immutable** once set. A goal whose definition of done can be
edited while it runs is a goal that always succeeds.

## It survives a restart

The lifecycle is projected into the session store as it happens, so a goal that
was running when sonar exited comes back knowing what it had already done. That
is the difference between a long prompt and a durable objective, and it is why
receipts are recorded before effects run rather than after.

## From the command line

The `goal` subcommand reaches the same runtime without the TUI:

```bash
sonar goal --help
```

## When an effect has no outcome

If sonar is killed between dispatching a tool and recording what happened, that
execution has no outcome — and guessing one would be worse than admitting it.
`/recover` reviews those and records typed evidence for what actually happened,
which is what lets the next turn proceed from a true state rather than an
assumed one.
