---
title: Configuration
description: Configure Local Agent per repository or through XDG paths, then apply environment overrides deliberately.
outline: deep
---

# Configuration

Local Agent supports both repository-local and user-wide configuration. Files are not merged: the first matching path wins, then environment overrides are applied.

## Search order

1. `./local-agent.yaml`
2. `./local-agent.yml`
3. `$XDG_CONFIG_HOME/local-agent/config.yaml`
4. `$XDG_CONFIG_HOME/local-agent/config.yml`
5. `$HOME/.config/local-agent/config.yaml`
6. `$HOME/.config/local-agent/config.yml`

`XDG_CONFIG_HOME` is used only when it is absolute. Duplicate paths are checked once.

## Repository-local STDIO trust

A repository configuration is data from the repository, not pre-approved
process authority. When `./local-agent.yaml` or `./local-agent.yml` supplies a
STDIO MCP server directly—or selects an `agents.dir` that supplies one—Local
Agent stops before spawning the process and prints a trust digest. To approve
that exact configuration for the launch, pass the digest back through the
process environment:

```bash
LOCAL_AGENT_TRUST_REPO_MCP=sha256:<digest-from-the-error> local-agent
```

The digest covers the absolute repository configuration path plus each STDIO
server name, command, resolved absolute executable path, executable content,
argument list, explicit environment, and canonical effective MCP trust
contracts. A trusted launch is pinned to that
resolved executable path and rechecks its content immediately before process
startup. Moving the repository, replacing the executable, or changing any of
those values requires a new decision. User-wide
configuration under `$XDG_CONFIG_HOME` or `$HOME/.config`, the default
`~/.agents` root, and an agents root selected through
`LOCAL_AGENT_AGENTS_DIR` remain user-controlled startup authority and do not
require this repository trust step.

This approval permits the configured server process to start and binds consent
to the effective trust contracts. Individual calls still follow the normal
approval policy unless an exact local-STDIO route is declared in that trust
configuration; explicit permission denies always win, and workspace-effectful
AUTO authority still requires an explicit in-workspace `workspace` argument.

## Minimal configuration

```yaml
ollama:
  model: qwen3.5:2b
  base_url: http://localhost:11434
  num_ctx: 16384

privacy:
  local_only: true

ui:
  theme: nord

model:
  default_model: qwen3.5:2b
  fallback_chain:
    - qwen3.5:2b
    - qwen3.5:0.8b
    - qwen3.5:4b
  auto_select: true
  embed_model: nomic-embed-text

tools:
  timeout: 30s
  max_grep_results: 500
  max_iterations: 10
  auto_max_iterations: 40

continuations:
  mode: suggest # off | suggest | auto_read_only
  max_auto_steps: 2

experts:
  enabled: true
  max_concurrent_inference: 0
  max_concurrent_distinct_models: 0
  max_team_experts: 0
  max_swarm_workers: 0
  max_moe_experts: 0
  max_eval_tokens: 768
  timeout: 90s

ice:
  enabled: false

servers: []
```

`continuations.mode` controls exact typed next actions from trusted Cortex and
Bob contracts. `suggest` is the default and never dispatches the action itself.
`auto_read_only` can follow only fully specified, unblocked,
registry-and-schema-validated read-only actions under current route trust,
workspace, deny-policy, replay, and ledger checks. Shell commands, mutations,
secreted execution, and unresolved generic gateway calls never qualify. It can
dispatch only while AUTO authority is active. The hard automatic ceiling is two
steps; configuration values above two are rejected.

Local Agent conservatively estimates text, structured tool schemas, and vision
patches before provider dispatch, and targets the final 25% of the active
`num_ctx` window as a generation reserve. If compaction cannot bring that
estimate below the admission threshold—before the first request or after tool
results—the turn stops with a recovery message instead of knowingly sending an
overfilled request. Provider tokenization remains model-specific, so this is an
admission guard rather than an exact tokenizer.

For effective context windows of 16K or less, Local Agent applies a stricter
character budget to optional prompt material: loaded context, active skill
content and catalog entries, ICE retrieval, and ignore patterns. Bounded
content can be truncated with a marker before request assembly. Native tool
schemas and host authorization are governed separately, so this budget neither
removes a tool from the registry nor grants a call.

When tool schemas are the pressure source, Local Agent can reduce only the
turn-local provider projection. It keeps each retained definition complete and
does not change the connected MCP registry, call authority, or saved session.
For a lazy MCPHub gateway it favors a complete resolve-and-call path rather
than exposing only half of it. Configure the gateway's per-agent `pin` and
`tool_schema_budget` first; see [MCP tools and MCPHub](/mcp#small-model-gateway-profile)
for a compact profile.

Start from the annotated [`config.example.yaml`](https://github.com/abdul-hamid-achik/local-agent/blob/main/config.example.yaml) when you need the complete model and MCP examples.

## Expert runtime

The `experts` block configures the read-only application-level [expert
runtime](/experts). Zero values select machine-adaptive auto limits. Non-zero
concurrency and fan-out values are caps: they can make a run smaller, but they
cannot force the resource planner above its CPU, RAM, or built-in safety limit.
`max_concurrent_distinct_models` separately protects the more expensive case
where selected profiles use different local model weights.

Experts are enabled by default. `max_eval_tokens` is the ceiling for each
expert, while the remaining evaluation allowance of a bounded parent turn is
the aggregate consultation cap. The runtime can reduce fan-out and distributes
that remainder without exceeding the per-expert ceiling. Charged child usage is
added to the parent and therefore to an active Goal's accumulated evaluation
budget. `timeout` is also per-expert; the parent turn's cancellation and
deadline still stop the whole consultation. Disabling the block removes
`consult_experts` from the model tool catalog.

The automatic resource snapshot honors process-visible Linux cgroup v1/v2 CPU
and memory limits. A sequential consultation still reserves the full accepted
set of local model weights; if that set does not fit, deterministic fan-out is
reduced. Verified Cloud or remote-only selections do not consume local model
weight budget and remain serial because provider-side capacity is unknown.

## Environment overrides

| Variable | Purpose |
|---|---|
| `OLLAMA_HOST` | Override `ollama.base_url` |
| `LOCAL_AGENT_MODEL` | Override the initial model (also remote provider model when remote is active) |
| `LOCAL_AGENT_PROVIDER` | Profile name when `provider.profiles` is defined; adapter type in the flat form: `ollama`, `xai`, `openai_compatible` |
| `LOCAL_AGENT_PROVIDER_BASE_URL` | Override remote provider base URL |
| `LOCAL_AGENT_PROVIDER_MODEL` | Override remote provider model id |
| `LOCAL_AGENT_PROVIDER_API_KEY_ENV` | Env var **name** that holds the API key (never the secret value) |
| `LOCAL_AGENT_PROVIDER_CONTEXT_SIZE` | Host-side context budget for remote models |
| `LOCAL_AGENT_AGENTS_DIR` | Override the agents directory |
| `LOCAL_AGENT_TOOLS_TIMEOUT` | Override the built-in tool timeout |
| `LOCAL_AGENT_TOOLS_MAX_GREP` | Override the maximum grep results |
| `LOCAL_AGENT_TOOLS_MAX_ITER` | Override NORMAL/PLAN provider iterations |
| `LOCAL_AGENT_TOOLS_AUTO_MAX_ITER` | Override AUTO provider iterations |
| `LOCAL_AGENT_CONTINUATIONS_MODE` | Set typed continuation handling to `off`, `suggest`, or `auto_read_only` |
| `LOCAL_AGENT_CONTINUATIONS_MAX_AUTO_STEPS` | Set the bounded read-only auto-follow budget, from 0 to 2 (`auto_read_only` requires at least 1) |
| `LOCAL_AGENT_ICE_EMBED_MODEL` | Override the ICE embedding model |
| `LOCAL_AGENT_LOCAL_ONLY` | Toggle local-machine endpoint enforcement |
| `LOCAL_AGENT_TRUST_REPO_MCP` | Trust the exact digest printed for repository-local STDIO MCP authority |
| `LOCAL_AGENT_ALLOW_LARGE_MODELS` | Bypass the 16 GB-oriented admission guard |
| `LOCAL_AGENT_REDUCED_MOTION` | Replace animated TUI activity with static glyphs |
| `LOCAL_AGENT_GLYPHS` | Set to `ascii` to replace Unicode UI glyphs independently of color and reduced motion |

An unparseable value is a startup error rather than a silent fallback: a typo
must not be indistinguishable from a deliberate setting. Boolean flags accept
`1/true/yes/y/on` and `0/false/no/n/off`, case-insensitively.

Remote providers (`LOCAL_AGENT_PROVIDER`, API key env names, optional TinyVault
PATH wrapper variables such as `LOCAL_AGENT_NO_VAULT` and
`LOCAL_AGENT_VAULT_PROJECT`) are documented in [Remote providers](./providers.md).

## Appearance

The terminal palette is chosen with `/theme`, which opens a picker, or set
directly with `/theme <name>`. Ten schemes are built in:

| Name | Notes |
|---|---|
| `nord` | Arctic blues; the default |
| `catppuccin` | Mocha on dark terminals, Latte on light |
| `dracula` | High-contrast neon on dark |
| `everforest` | Soft forest greens, easy on the eyes |
| `gruvbox` | Warm retro contrast |
| `kanagawa` | Wave on dark terminals, Lotus on light |
| `one` | Atom's One Dark and One Light |
| `rose-pine` | Muted natural tones; Dawn on light |
| `solarized` | Ethan Schoonover's precision palette |
| `tokyo-night` | Storm on dark terminals, Day on light |

Each scheme ships a light and a dark variant, and every foreground is checked
against WCAG AA for normal text in both, measured against that scheme's own
darkest surface. Local Agent does not paint a terminal background, so the
scheme changes foreground colors only and your terminal's own background is
preserved.

A theme can also be set declaratively:

```yaml
ui:
  theme: catppuccin
```

An interactive `/theme` choice is stored in the owner-private
`~/.config/local-agent/runtime-preferences.json` and takes precedence over this
key, because it is the more recent and more deliberate of the two. A name this
build does not recognise falls back to the default rather than failing startup.

Themes are presentation only. They do not affect authority, tool policy,
approvals, or what leaves the machine.

## Runtime model preference

An explicit local model selected through `/model <name>` or the model picker is
stored separately from repository configuration in the owner-private
`~/.config/local-agent/runtime-preferences.json`. It is restored on the next
process start only when Ollama still advertises the model and current policy
admits it. `/model auto` clears the saved pin. A CLI `--model` selection and
agent-profile models take precedence, and conversation-scoped Cloud consent is
never saved.

## Repository instructions

At startup, Local Agent reads `./AGENTS.md`. If that file does not exist, it falls back to the legacy `./AGENT.md` name.

Create a starter file with:

```bash
local-agent init
```

Instructions are model context, not a security boundary. Mode policy, tool admission, workspace path checks, and approval checks remain host-owned.

## Profiles and skills

Global profiles and skills use the shared agent directory:

```text
~/.agents/
  agents.md
  mcp.json
  agents/
    reviewer/
      agent.yaml
  skills/
    go-review/
      SKILL.md
```

The selected shared agents directory is the only global skill root; it defaults
to `~/.agents` and may be changed with `agents.dir` or
`LOCAL_AGENT_AGENTS_DIR`. The private `~/.config/local-agent` directory is
reserved for configuration and runtime data and is not searched for skills.
The retired top-level `skills_dir` setting is rejected with migration guidance.
Give each Agent Skill an explicit identity at the start of `SKILL.md`:

```markdown
---
name: go-review
description: Review Go changes for correctness and concurrency
---
```

Local Agent uses the declared name for catalog lookup and profile activation.
Skill names must be unique across search paths; invalid YAML frontmatter, files
over 1 MiB, and symlinked files fail closed during startup. Switch profiles
with `/agent`, and activate skills with `/skill`.

Inactive skills contribute only bounded name and description metadata to the
model. For a clearly matching task, the model can request the already-discovered
body by exact name through a read-only built-in tool. This on-demand path does
not activate the skill or expose its filesystem path and auxiliary directory
assets.
