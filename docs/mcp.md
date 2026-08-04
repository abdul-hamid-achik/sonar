---
title: MCP tools and MCPHub
description: Connect Local Agent to MCP servers directly or through MCPHub while preserving approval and privacy boundaries.
outline: deep
---

# MCP tools and MCPHub

Local Agent is an MCP client for tools. It supports STDIO, SSE, and Streamable HTTP transports. Every exposed tool is namespaced as `<server>__<tool>` and remains subject to the Local Agent approval policy.

## Recommended gateway

[MCPHub](https://mcphubcli.dev/) provides one local gateway for discovery, authentication, and downstream tool policy:

```yaml
servers:
  - name: mcphub
    command: mcphub
    args: [mcp, serve, --agent, local-agent]
```

Read the [MCPHub documentation](https://mcphubcli.dev/) or inspect its [source repository](https://github.com/abdul-hamid-achik/mcphub) for gateway setup and operational details.

With a gateway:

- MCPHub owns lazy discovery and synchronization of downstream servers.
- Local Agent owns the final user approval and transcript.
- Cortex and other tools appear as namespaced MCP calls.

MCPHub's lazy mode intentionally advertises its eight management tools plus any
chosen pins. Unpinned tools are not missing: the normal flow is
`resolve (or search) → describe when needed → call`. Local Agent keeps the
outer MCPHub namespace for safe routing, but the transcript presents the
downstream specialist and action. It does not flatten the entire downstream
catalog into every model request.

Large lazy results may return a stored-result receipt. Retrieve its bounded pages explicitly with `mcphub_get_result`; Local Agent does not inject every page into the conversation automatically. Treat stored TinyVault results as sensitive because the gateway stores the exact downstream result for its configured retention period.

### Small-model gateway profile

For a small local model, keep the local-agent connection above and configure
the corresponding MCPHub agent in `mcphub.yaml` as a gateway with no directly
advertised downstream schemas:

```yaml
expose: lazy

agents:
  local-agent:
    type: local-agent
    path: ~/.config/local-agent/config.yaml
    mode: gateway
    pin: []
    tool_schema_budget: "0"
```

`pin: []` prevents the agent from inheriting global pins. A schema budget of
`"0"` leaves the eight MCPHub management tools available and keeps every allowed
downstream tool discoverable and callable through the lazy workflow. This
changes advertisement, not authorization: use `servers` and `tools` in the
same agent entry when you also need to limit which downstream capabilities may
be called. Run `mcphub sync` to preview the harness entry, apply with
`mcphub sync --write`, then restart Local Agent.

Use a small nonzero `tool_schema_budget` only when a directly mounted tool is
worth its recurring schema cost. MCPHub admits complete definitions that fit;
it never truncates a schema. See the [MCPHub routing guide](https://mcphubcli.dev/guide/routing)
for the exact scope and budget rules.

### Local schema admission

MCPHub controls the catalog it advertises. Local Agent adds a second, turn-local
guard before it sends a provider request. If the estimated prompt is already
near the active context limit, it keeps complete native definitions that fit a
bounded schema budget and rebuilds the prompt. The registry, execution
authority, and saved session state do not change.

For a lazy gateway, Local Agent gives priority to a usable path: local `read`
and `grep`, plus the matching `mcphub_resolve_tool` and `mcphub_call_tool`
definitions. It does not expose a partial resolve/call pair. If even that
complete set cannot fit, the provider request is refused with a recovery
message rather than sent knowingly over budget. Treat this as a safety valve,
not a replacement for a lean MCPHub policy or an appropriately sized
`ollama.num_ctx`.

## Contextual MCP selection

For a non-trivial ordinary turn or durable-goal phase, Local Agent can ask an
exact host-trusted local MCPHub for one bounded capability recommendation before
the provider turn. This is host assistance, not automatic execution:

```text
private user task
  → host-owned activity projection
  → mcphub_resolve_tool
  → advisory server__tool route
  → host may pre-fetch mcphub_describe_tool (confident routes)
  → host specialist playbook + optional coarse mcphub_stats snapshot
  → model-authored mcphub_call_tool (with workspace soft-fill hints)
  → normal scope, privacy, approval, and ledger checks
```

The resolver receives only generic activity fields such as phase, desired
outcome, available input kinds, and locally classified allowlisted intent
facets such as `symbols`, `observability`, `browser`, or `repository`. These
facets preserve useful `use_when` matching without copying arbitrary task
wording. It does not receive raw prompt text, file contents, paths, URLs,
credentials, or previous tool output. Equivalent activities are
cached only in memory. Clear recommendations expire after five minutes;
ambiguous and no-match conclusions expire after 30 seconds. The host also
invalidates that cache when the MCP registry catalog epoch advances (reconnect
or tool-list change) and partitions entries by MCPHub's `catalog_revision` when
present. A failed exact downstream route or an explicit request to reconsider
capabilities bypasses the cached choice immediately and re-runs resolve with
`Reconsider` set.

The TUI labels the result as a suggested MCP route rather than a tool run.
Runtime can show the most recent route after the turn settles, but this bounded
projection remains process-local and is not saved with the session. Process-local
routing counters (resolved / ambiguous / no-match / host pre-fetch) are available
to the host for diagnostics; they never store prompt text.

Local Agent accepts only MCPHub's exact contextual-resolver contract version 1:
the response must include a consistent status, a non-empty valid catalog
revision, and the bounded recommendation control fields. Missing, older, newer,
or internally inconsistent envelopes fail closed as an invalid advisory; they
are never guessed into a route.

An ambiguous recommendation is never selected or executed. The model can use
`mcphub_search_tools` to compare bounded candidate metadata. For a clear route,
the host may pre-fetch `mcphub_describe_tool` and attach a sanitized schema to
the active turn so small models skip a discovery hop. When pre-fetch is
unavailable, the advisory still directs the model to describe before argument
construction. Required-field summaries remain incomplete for mutually exclusive
inputs. The host may soft-suggest only workspace-shaped defaults such as
`workspace="."`; URLs, secrets, and free text are never filled.

When resolve returns no usable route, the host still attaches a short phase
playbook (research / planning / implementation / verification / web / code) so
the model knows which specialist family to discover through the lazy gateway.

Search descriptions, `use_when` hints, the selected tool description, and its
sanitized JSON Schema are available only to the active model turn. They are
labeled as untrusted contract metadata, cannot expand authority, and are
replaced by a bounded semantic receipt before session persistence. Raw MCP
`StructuredContent` never leaves the parser boundary.

See MCPHub's [contextual routing guide](https://mcphubcli.dev/guide/contextual-routing)
for catalog-side `use_when`, ambiguity, and ranking configuration.

### Hitspec compatibility

Hitspec 2.17 exposes `hitspec_fetch`, `hitspec_list_requests`, and
`hitspec_validate`. `hitspec_fetch` returns content inline and does not create a
file. A durable 2.17 result therefore remains an explicit composition: review
the inline response, write the accepted content through a separately authorized
host file operation, then call `fcheap_save` separately.

Hitspec 2.18 additionally supports optional `hitspec_search_web` and
`hitspec_capture_webpage` surfaces when their server dependencies are configured.
Local Agent interprets the bounded `hitspec_search_web` envelope as a completed
discovery operation with candidate—not verified—evidence. Candidate URLs,
titles, and snippets are available only to the active model turn; durable
session state keeps a bounded receipt with the candidate count and source
domains instead of retaining the private query or raw snippets.

Local Agent recognizes the compact `hitspec_capture_webpage` structured receipt
as a file.cheap artifact outcome while keeping webpage content, URLs, titles,
tags, and downstream failure prose outside durable session state. Transport,
storage, indexing, and evidence remain separate states. The typed capture
contract is accepted only when its one-file stash metrics match the rendered
Markdown byte count and any successful indexing was explicitly requested.

See the versioned [Hitspec MCP reference](https://hitspec.dev/reference/mcp) for
the exact tool schemas and operator-owned startup requirements. MCPHub discovers
only the tools the running Hitspec server actually advertises, so a 2.17 server
does not gain the optional 2.18 surfaces through Local Agent configuration.

When web discovery needs a secret but retrieval does not, MCPHub can register
separate core and protected Hitspec processes. A locked vault then removes only
the protected extension instead of the non-secret core. This is an
operator-owned capability split, not a Local Agent permission bypass; see
[MCPHub's Hitspec guide](https://mcphubcli.dev/guide/hitspec#split-core-capabilities-from-protected-web-discovery)
for the exact current flags and boundary.

## Direct servers

You can also connect a server directly:

```yaml
servers:
  - name: local-tools
    command: /absolute/path/to/mcp-server
    args: [serve]

  - name: local-http-tools
    transport: streamable-http
    url: http://127.0.0.1:8812/mcp
```

Servers connect concurrently at startup. One failed server does not prevent the TUI from opening, and a background health monitor attempts reconnection.

## Cortex and durable goals

[Cortex](https://cortexai.tools/) is an optional evidence-guided kernel. When reachable directly or through MCPHub, the Goal Runtime can link one stable Cortex case and read its semantic status between productive turns.

Cortex does not own Local Agent's budgets, approval prompts, scheduling, or
cancellation. Local Agent consumes Cortex task state only through its exact
versioned envelope; it does not reproduce Cortex phases, leases, evidence
ledgers, or completion assessment.

## Bob repository contracts

[Bob](https://bobcli.dev/) reports repository contract and convergence state.
Local Agent has exact structured parsers for these read-only Bob operations:

| Operation | Host interpretation |
| --- | --- |
| `bob_context` | Recipe, bounded capability and extension summaries, and clean, drifted, or conflicted repository state |
| `bob_path` | Path classification, ownership effect, and related extension or playbook IDs |
| `bob_playbook` | Available or blocked playbooks, named required inputs, scope, risk, and the first bounded step |

Parser authority comes from the exact trusted Bob route and supported schema,
not a similar tool name or prose. A malformed result, unsupported schema
version, route mismatch, unknown enum, or oversized result fails closed instead
of becoming domain success.

Bob results always carry `EvidenceNone`: no evidence assessment. `clean` means
that the repository matches Bob's contract; it does not mean the application
is tested or the current task is verified. Behavioral evidence must still come
from an exact verifier contract, and Cortex remains responsible for task proof
assessment when a durable goal uses it.

After validation, durable session state keeps only a bounded semantic digest.
Raw structured output, absolute workspace paths, arbitrary reasons and
commands, user values, manifests, previews, and file contents are not saved.
Local Agent may give the active model a smaller validated Bob projection for
the current turn, under a separate byte and item cap; that transient content is
discarded after the turn.

The same parser handles direct Bob results and a complete MCPHub stored result.
For a stored result, Local Agent first binds the exact call ID, downstream
server, and tool from the original dispatch. It then accepts only a bounded,
contiguous page chain for that call ID. Partial pages remain incomplete;
mismatched, reordered, oversized, or malformed pages fail closed. Only the
complete serialized result reaches the Bob parser, and the assembled bytes are
discarded afterward.

### Bob workspace bootstrap

A regular, non-symlink `bob.yaml` at the workspace root marks the repository as
a Bob candidate; the filename alone is not a valid contract. If one
unambiguous eligible trusted `bob_context` read is registered, Local Agent
suggests a compact context read. A unique direct route is preferred; otherwise
exactly one pinned MCPHub route is required. In
`auto_read_only` mode, AUTO may perform that same read only after all automatic
continuation checks pass.

The host caches the validated bounded digest for the active agent and
invalidates it when the workspace marker or filesystem generation changes.
Prompts receive the compact cached projection rather than a full Bob document.
Non-Bob repositories and Bob repositories without an eligible tool continue
normally.

## Typed continuation actions

Exact Cortex and Bob results can include machine-readable next actions. Local
Agent converts supported actions into bounded host-owned fields, then validates
the target against the current tool registry and advertised input schema. It
also preserves named missing inputs, blockers, source revision or context
digest, and workspace identity. Downstream command text is display data, not
shell authority, and downstream effect labels never override the host's effect
classification.

The default `suggest` mode shows the first valid action to the model and TUI
without dispatching it. The host can therefore ask only for named missing
inputs and expose blockers without making the model reconstruct state from
prose. `off` disables this projection.

Optional `auto_read_only` applies only during AUTO turns. It can follow at most
two actions in one automatic chain, and only when each action is:

- produced by an exact trusted Cortex or Bob contract with a successful domain
  result;
- fully specified, unblocked, current, and bound to the active workspace;
- resolved to one exact registered direct or pinned tool whose schema still
  matches;
- host-classified as read-only and declared non-destructive, idempotent, and
  closed-world; and
- allowed by current route trust, MCP scope, approval policy, replay checks,
  and the execution ledger.

Shell commands, Bob apply, other mutations, secreted execution, unresolved
generic MCPHub proxy calls, prose-derived actions, stale actions, and repeated
action fingerprints never auto-run. An explicit deny still wins. If any check
changes before dispatch, Local Agent stops the automatic path and retains the
bounded suggestion when it is still valid.

Continuation actions are contracts for a possible next call, not authority
grants. Local Agent does not merge Bob's ownership engine with Cortex's task
state machine. See [Configuration](./configuration.md) for `off`, `suggest`, and
`auto_read_only` settings.

## Security boundary

`privacy.local_only: true` accepts only local-machine HTTP MCP endpoints (`localhost`, loopback IPs, and unspecified bind aliases). It cannot constrain what an approved STDIO server does after it starts.

Treat each MCP server as a separate trusted process. It may read files, contact services, or create effects according to its own configuration. Review each approval request and keep server catalogs narrow.

Local Agent treats MCP safety annotations as untrusted presentation hints, not
authority. They may improve the wording of an approval preview, but they never
change authorization or durable effect classification. By default, MCP calls
remain effect-unknown and follow the normal approval-policy path. An exact
local-STDIO trust contract may explicitly classify named routes as `read_only`
or `workspace_effectful`: read-only routes can use the host's reduced-friction
read path, while workspace-effectful routes can receive AUTO authority only
when they provide an explicit workspace inside the active workspace. Unknown,
remote, wrapped, or uncatalogued routes remain gated. `--skip-approvals`
removes an `ask` prompt, but an explicit `deny` remains effective and the
normal audit receipt is still recorded. `--yolo` is only a deprecated
compatibility alias for that flag.

## Structured result boundary

Local Agent preserves MCP `StructuredContent` separately from display text. It prefers an exact structured contract for semantic interpretation and accepts text only when the known tool returns one complete JSON document. It never parses prose or Markdown with status heuristics.

Structured payloads are short-lived host input. After an exact tool-specific parser derives transport, domain, evidence, and routing state, the structured copy is discarded rather than copied into the saved visual tool card. The post-hook text required by model history and the execution ledger keeps its existing bounded persistence policy. This avoids duplicate `text + structured` output and keeps a second secret-bearing or very large structured copy out of the transcript projection.

Versioned verifier contracts can produce verified or contradicted evidence. Unrecognized versions, partial results, stale indexes, and stored-result receipts remain neutral or attention states; they do not become green merely because MCP returned without a protocol error.

## Current protocol boundary

The current MCP implementation is tool-focused. Prompts, roots, subscriptions, sampling, and direct multimodal rendering are not yet exposed.
