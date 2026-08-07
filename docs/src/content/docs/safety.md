---
title: Safety
description: Read this before --skip-approvals or a long unattended run.
---

- **This is not a sandbox.** Tools run as your user. The workspace boundary, the
  ignore policy and the approval model are the controls; there is no
  kernel-level isolation unless you turn one on.
- **AUTO is scoped, not unrestricted.** A bounded shell subset runs without
  asking; anything else is approval-gated, and every dispatch is recorded in a
  durable ledger before it runs.
- **Approvals are auditable.** Each prompt names the rule the request tripped,
  and grants are scoped — one path, one command prefix, one MCP tool — never
  "allow everything".
- **`privacy.local_only` bounds tool endpoints, not inference.** It refuses
  remote MCP servers. It cannot make sonar private: every request leaves your
  machine by construction.
- **Your prompts, code and tool output go to the provider.** Check their
  retention policy before pointing sonar at anything you cannot send.

## Optional OS confinement

`sandbox.enabled` confines shell subprocesses with the operating system's own
primitives — Seatbelt on macOS, bubblewrap on Linux. It is a second layer under
the scoped-shell catalog, never a replacement: the catalog reads a command line
and can refuse `curl` before it runs, and is structurally blind to what a program
does once started.

It is off by default because it changes which commands **succeed**, not only
which are allowed to start. With the network denied, a build that needs to
download a module fails.

Turning it on also widens what AUTO runs unattended, and that is the point: most
catalog refusals exist to prove containment from a command line alone, and once
the kernel proves it for every command they have nothing left to prove. What
keeps asking is what confinement does not cover — the workspace stays writable,
so `rm -rf .` still destroys uncommitted work.
