---
title: Approvals and permissions
description: What asks, what does not, and how a grant is scoped.
---

Every tool call passes host mode policy, workspace validation and a permission
check before it runs, and the dispatch is recorded in a durable ledger **before**
the effect happens rather than after.

## A prompt names the rule it tripped

An approval is not "sonar wants to do something". It names which rule refused,
what the consequence is, and — for a compound shell command — which segment of
it needs the decision, as a line the host wrote rather than one the UI re-split.

## Grants are scoped

There is no "allow everything". Each choice binds exactly one thing:

| | |
| --- | --- |
| once | this request only |
| session | this same request again, for this session |
| tool / path / prefix | one tool, one path, or one command prefix, for this session |
| workspace | the same, saved durably for this workspace |

`/permissions` shows the current posture, the session grants and the workspace
rules, and can revoke or clear them. It also exports and imports rules, so a
workspace policy can be reviewed as a file.

## Modes

`shift+tab` cycles the authority of the whole session:

- **NORMAL** — tools ask.
- **PLAN** — read-only. It may inspect and reason and cannot write.
- **AUTO** — a bounded shell subset runs without asking; anything outside it is
  still approval-gated.

## Timeouts refuse, they never grant

`tools.approval_timeout` decides what an unanswered prompt does. It **refuses
and continues**: the model sees the refusal and takes another route, and the run
keeps going. A timeout can only ever withhold permission — there is no
configuration in which waiting produces a grant.

## Skipping approvals

`--skip-approvals` exists and is a real decision, not a convenience. Host, scope
and tool boundaries still apply; what stops is the asking. Read
[Safety](/safety/) before using it on anything you cannot lose.
