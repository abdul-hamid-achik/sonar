---
title: Safety and privacy boundaries
description: Learn exactly what Local Agent checks, what still requires trust, and why local-first is not the same as sandboxed.
outline: deep
---

# Safety and privacy boundaries

Local Agent makes authority visible and defaults to local endpoints. Those controls reduce risk; they do not create an operating-system sandbox.

## Default local-only policy

```yaml
privacy:
  local_only: true
```

With this setting, Local Agent treats `localhost`, loopback IPs, and unspecified bind aliases such as `0.0.0.0` or `::` as local-machine endpoints. It:

- rejects other Ollama URLs;
- rejects other SSE and Streamable HTTP MCP URLs;
- pins every SSE and Streamable HTTP MCP request to the configured local origin, bypasses environment proxies, and rejects redirects, DNS answers, or server-supplied message endpoints that leave loopback;
- excludes Ollama Cloud from automatic routing and requires exact conversation-only consent for manual selection;
- canonicalizes built-in file paths, resolves symlinks, and rejects paths outside the startup workspace unless a temporary read grant covers them;
- permits explicit, process-local external exact-file or directory read grants without widening write authority;
- applies host-owned secret-name exclusions plus the workspace's
  `.agentignore` to built-in file operations;
- removes most inherited environment variables before built-in shell execution;
- refuses to start repository-supplied STDIO MCP processes until their exact executable configuration is trusted;
- starts STDIO MCP servers with a minimal environment and deterministic local executable lookup.

Repository-local STDIO trust is bound to the absolute configuration path, the
server name, command, resolved executable path and content, arguments, and
explicit environment. Local Agent prints the required
`LOCAL_AGENT_TRUST_REPO_MCP=sha256:...` value and exits before starting any MCP
process. A changed executable or moved configuration produces a different
digest. A trusted launch uses the resolved path covered by that digest and
rechecks its content immediately before startup. Protect the executable and its
parent directory from concurrent writes, as the final OS launch remains
path-based. This startup trust does not by itself approve later MCP tool calls.
Separately, an exact trust contract in the trusted local-STDIO configuration
can classify specific routes as read-only or workspace-effectful. Those
contracts are host-owned, bound to the executable identity, and do not trust
MCP annotations; all other routes remain on the normal approval path.

The host policy excludes conventional secret-bearing names even when a
workspace has no `.agentignore`: `.env`, `.env.*`, `*.pem`, `*.key`,
`id_rsa*`, `id_ed25519*`, `.npmrc`, `.netrc`, `credentials`, `.aws/**`,
`.ssh/**`, `*.p12`, and `*.keystore`. A repository may add patterns for other
paths, but its `.agentignore` cannot negate these host exclusions. The host
policy itself does not deny the exact conventional template leaf names
`.env.example`, `.env.sample`, `.env.template`, and `.env.dist` from the broad
`.env.*` rule. A repository rule may still exclude any of those files. These
exceptions only describe filename policy; review template contents before
sharing them.

Built-in workspace reads and approved file mutations execute relative to a
workspace directory handle pinned for that operation. Local Agent rechecks the
workspace identity, rejects symlink components while opening mutation parents,
and creates atomic-write temporary files inside the pinned parent. A path
component changed after validation therefore cannot redirect built-in file I/O
outside the startup workspace. This boundary does not constrain an approved
shell command or MCP process and does not turn Local Agent into an OS sandbox.
It also inherits the operating system's mount namespace: content mounted below
the workspace, including a bind mount to another tree, is visible as content of
that workspace. Keep mounts that expose sensitive data outside approved roots,
or run Local Agent inside a sandbox with the mount layout you intend to grant.

## Approval policy

The following requests require approval in NORMAL by default:

- write, edit, copy, move, remove, and directory creation;
- shell commands;
- memory save, update, and delete;
- every MCP tool call.

Interactive approvals stay process-local. Choices are:

- **once** — this call only;
- **same request again this session** — exact tool and arguments for the rest of
  this process;
- **this tool again this session** — offered only for `write` / `edit` /
  `mkdir`; any arguments for that tool, still revalidated against the workspace;
- **deny**.

`/permissions accept-edits on` is the Claude-style middle ground for coding:
workspace `write` / `edit` / `mkdir` skip prompts in NORMAL without auto-approving
shell, remove, memory mutation, or MCP. Explicit deny policies still win.

Additional process-local scopes:

- **path session** — further `write`/`edit`/`mkdir` on the same path (one
  approval covers the whole write family for that path);
- **bash prefix session** — further simple bash commands sharing a safe prefix
  (compound shell commands with `&&`, pipes, or `$` never match);
- **MCP tool session** — further calls to the same namespaced MCP tool with any
  arguments (preflight and route trust still apply).

Durable workspace rules (user config only, not repository files) can save:

- bash patterns — literal prefixes or Claude-style trailing globs
  (`git status *`, `go test *`);
- exact namespaced MCP tools (`server__tool`);
- exact workspace-relative write paths for `write` / `edit` / `mkdir` only.

Use `/permissions allow-bash|allow-mcp|allow-path`, the approval `w` key, or
**Settings → Permissions**. Export/import (`/permissions export|import`) moves
rules between machines as portable JSON; write paths stay workspace-relative.
They are never rewritten into `tool_permissions.policy = allow`. Path grants
reject targets outside the workspace. Bash patterns never match compound shell
commands. Session grants and accept-edits remain process-local unless you
explicitly save a workspace rule.

AUTO narrows those prompts for routine work: validated workspace writes and
directory creation, host-catalogued local MCP routes, and a static catalog of
ordinary development commands are pre-authorized. Destructive commands,
dynamic shell expansion, output redirection to files, raw shell access to
external paths, explicit network CLIs or endpoints, unknown commands, memory
mutation, and uncatalogued MCP effects remain gated. Temporary external
read/write scopes are consumed only by the exact typed host capability shown in
the UI; they never widen raw shell authority. Generic path-qualified workspace
executables are also gated. The one narrow AUTO exception is an exact Minerva
query launched from the physical `<workspace>/bin/minerva` binary after the
host verifies its Go main-package and module build identities. It is classified
as effectful workspace execution, not as a pure read.

That exception covers only the bounded query grammar. Minerva mutations,
persistent `mcp serve`, and delegated or broad diagnostic surfaces remain
approval-gated. Prefer Minerva's exact host-trusted MCPHub route for ordinary
product integration. This classification reduces interruptions; it does not
turn the built-in shell into an operating-system sandbox.

The classifier validates the outer command and its visible operands. Admitted
commands such as `go test`, `go build`, `npm test`, or `bun test` can still
execute code and hooks supplied by the repository, which inherit the host
process's filesystem and network access. Use AUTO only with a workspace whose
build logic you trust. Task runners such as `make`, `task`, and `just`,
package-script delegation such as `npm run`, `go generate`, and raw shell
`find`, `grep`, or `rg` searches remain approval-gated; use Local Agent's
built-in ignore-aware list, read, and grep tools for routine inspection. Raw Git
is also outside the automatic catalog:
repository configuration, clean or text-conversion filters, filesystem
monitors, signing, and hooks can execute programs even when the outer Git
command appears read-only. `/changes` and `/commit` provide the corresponding
host-owned inspection and commit paths.

The inline approval surface replaces the composer while leaving the transcript
visible. It shows the action, scope, target or command, and a bounded diff when
one is available. Press `y` to allow once, `n` to deny, `s` to allow the
identical canonical request again during the current Agent process, `d` to
inspect exact arguments, or `esc` to cancel the approval and active turn. The
session grant is not a broad tool-name policy and is not persisted across
process restarts.

At the supported 30×12 minimum, a long file target is projected by its
identifying tail rather than an indistinguishable path prefix. `pgdn` exposes
the remaining preview and `d` switches to the exact arguments before a
decision. Below that minimum, Local Agent replaces interactive surfaces with a
resize notice and pauses keyboard, mouse, and paste input except for `ctrl+c`.
Restoring a supported size first waits for a quiet input boundary and requires
an explicit `enter` re-arm gesture. That gesture is consumed, a second bounded
quiet guard runs, and only then does the unchanged pending decision and draft
return.

Databases created by older releases may contain broad per-tool `allow` rows.
On upgrade, Local Agent retires those rows to `ask`; they never authorize a
new request. Persisted `deny` policies remain effective.

## External files and projects

The startup workspace is the default typed writable root. In the interactive
TUI, an ordinary prompt that explicitly names an existing absolute or `~/` path
outside that workspace is inspected before the model sees the prompt.

The mode determines the scope that may be prepared:

- PLAN can prepare an exact read scope only.
- NORMAL shows the exact scope for review. When the prompt explicitly asks to
  mutate that path, the review can combine read access with a typed write scope.
- AUTO commits the same safely inspected read or typed-write scope without an
  extra decision modal and records the scope receipt in the transcript.

If Local Agent cannot establish a requested mutation scope safely, it restores
the draft, sends nothing to the model, and does not fall back to shell access.
Allowing a NORMAL or PLAN review resumes the original draft once; denying or
cancelling restores it unchanged.

Single-, double-, or backtick-quoted path literals and macOS drag-and-drop paths
with backslash-escaped whitespace are recognized without invoking a shell or
evaluating substitutions. The scanner has a hard limit of 32 distinct explicit
path candidates. After canonicalization, deduplication, and collapsing paths
already covered by a requested directory, one prompt may add at most four new
read scopes and, independently, at most four matching typed write scopes.
Workspace paths and paths already covered by active read authority do not
consume the read budget. Exceeding a limit restores the draft, grants nothing,
sends nothing to the model, and asks you to split the request.

For example, this asks for one exact-file read scope rather than access to all
of Downloads:

```text
Analyze ~/Downloads/bug.mp4 with Vidtrace and explain the failure sequence.
```

The approval surface shows the kind, requested access, and canonical identity
of every proposed scope. Terminal control characters are escaped for display
without changing the authority value. If two paths become visually
indistinguishable in a narrow terminal, confirmation is disabled until the
terminal is wide enough to show distinct identities.

Sibling files remain unavailable. Local Agent verifies that the object has not
changed between inspection and scope commit. Exact-file reads recheck the
authorized file identity, so replacing the file does not transfer authority to
its replacement. Directory scopes use a pinned `os.Root` boundary so relative
operations and symlinks cannot escape the selected directory.

A prompt-derived read scope is process-local and lasts until
`/scope clear-read` or session exit. A prompt-derived typed write scope expires
when that turn settles. It is limited to Local Agent's built-in write, edit, and
mkdir operations plus exact trusted workspace MCP tools. It does not authorize
Bash, remove, move, or a broader parent directory.

When a task needs a whole source tree for reading, grant that directory
explicitly for the current process:

```text
/scope add-read ~/src/another-project
```

Built-in list, read, grep, and copy-source operations can then use that root.
Manual `/scope add-read` grants are read-only; they do not create typed write
scope or shell authority. Directory read scopes combine the external root's
`.agentignore` with the same host-owned secret exclusions used in the startup
workspace; that root cannot negate the host defaults either. Exact-file and
directory read scopes are not saved in the session and can be revoked with
`/scope remove-read` or `/scope clear-read`.

An external scope does not silently authorize an MCP call. The exact route,
trusted identity, advertised tool contract, and approval policy still apply.
Approved shell commands and trusted STDIO servers remain independent trust
boundaries.

## What local-only cannot guarantee

An approved shell command can use absolute paths, leave the workspace, start subprocesses, or contact the network. A trusted STDIO MCP server is a separate process and can act according to its own configuration.

Local Agent currently does not provide:

- an OS-level filesystem, process, or network sandbox;
- a kernel-enforced egress firewall;
- detection or isolation of mount points created below an approved filesystem root;
- argument-scoped persistent approvals;
- automatic proof that an external side effect completed.

Do not describe the current alpha as “fully private,” “offline,” or “sandboxed.”

## PLAN and headless execution

PLAN removes mutation tools and rejects model-generated mutations in the host.

In non-interactive `-p`/`--prompt` mode, requests that need an approval fail
closed by default because there is no approval UI. `--skip-approvals` skips
approval prompts, but explicit deny policies, host validation, workspace/scope
limits, privacy checks, tool preflight, and the execution ledger still apply.
Limit it to disposable or strongly versioned workspaces. `--yolo` is a
deprecated compatibility alias for `--skip-approvals`.

## Practical operating checklist

1. Start from a clean Git worktree.
2. Use PLAN for unfamiliar repositories; PLAN does not itself authorize repository-supplied MCP processes.
3. Keep `privacy.local_only` enabled unless you understand the endpoint change.
4. Read every approval request, especially shell and MCP arguments.
5. Inspect `git diff` before committing.
6. Treat Cloud consent and STDIO servers as explicit trust decisions; never reuse a repository MCP digest after its configuration changes.
7. Keep valuable work backed up outside the agent process.
