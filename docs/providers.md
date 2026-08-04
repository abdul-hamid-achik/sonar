---
title: Remote providers
description: Configure one or more OpenAI-compatible remote inference profiles (xAI Grok, OpenAI, OpenRouter) with API keys from TinyVault.
outline: deep
---

# Remote providers

Local Agent defaults to **Ollama on the local machine**. Remote chat is opt-in through OpenAI-compatible **provider profiles**. Secrets stay out of YAML: only environment variable **names** are configured; values are injected at launch (recommended: [TinyVault](https://www.tinyvault.dev/) / `tvault`).

## Supported types

| `type` | Meaning |
| --- | --- |
| `ollama` (default) | Existing local Ollama path |
| `xai` | Grok via `https://api.x.ai/v1`, key env `XAI_API_KEY` |
| `openai_compatible` | Any OpenAI-style chat API (OpenAI, OpenRouter, local vLLM, …) |

Remote profiles require **`privacy.local_only: false`** (or `LOCAL_AGENT_LOCAL_ONLY=false`).
Their `base_url` must use HTTPS; plain HTTP is accepted only for
`localhost` or literal loopback/unspecified addresses used by local servers.

SuperGrok / X Premium chat subscriptions are **not** API credentials. Create a key at [console.x.ai](https://console.x.ai). No X Platform developer app is required.

## Multi-profile config

Install several providers and pick one with `active`, `/provider`, or env:

```yaml
privacy:
  local_only: false

provider:
  active: ollama
  profiles:
    ollama:
      type: ollama
    xai:
      type: xai
      model: grok-4.5
    openai:
      type: openai_compatible
      base_url: https://api.openai.com/v1
      model: gpt-4.1
      api_key_env: OPENAI_API_KEY
    openrouter:
      type: openai_compatible
      base_url: https://openrouter.ai/api/v1
      model: anthropic/claude-sonnet-4
      api_key_env: OPENROUTER_API_KEY
```

In the TUI:

```text
/provider              # opens the provider picker
/provider list
/provider xai
/provider ollama
```

Also: **Settings (ctrl+p) → Provider**, or complete `/provider ` with profile names.

The last `/provider` selection is saved in `~/.config/local-agent/runtime-preferences.json` and restored on the next launch unless you set `LOCAL_AGENT_PROVIDER` for that process.

`LOCAL_AGENT_PROVIDER=xai` selects the profile named `xai` when a catalog exists; with the flat form it still sets `provider.type`.

## Flat single-provider form

Still supported for one remote backend:

```yaml
privacy:
  local_only: false

provider:
  type: xai
  model: grok-4.5
```

## TinyVault setup

```bash
tvault set -p local-agent XAI_API_KEY          # from console.x.ai
tvault set -p local-agent OPENAI_API_KEY       # optional
tvault set -p local-agent OPENROUTER_API_KEY   # optional

tvault list -p local-agent --names-only
```

### Launch with injected keys

Inject only the provider key the active profile needs, and open the remote
boundary for that run:

```bash
tvault run -p local-agent --only XAI_API_KEY -- env \
  LOCAL_AGENT_LOCAL_ONLY=false \
  local-agent
```

Inject the narrowest set of names you need — `--only` keeps unrelated project
secrets (Obsidian, Tavily, …) out of the agent process. Pick the profile with
`/provider xai` inside the TUI, or set `LOCAL_AGENT_PROVIDER` before launch.

Any secret manager works: Local Agent only reads API key **values** from the
process environment, so `tvault run`, a shell export, or your own launcher are
equivalent as far as the binary is concerned.

## Environment variables

### Local Agent process (built-in)

| Variable | Purpose |
| --- | --- |
| `LOCAL_AGENT_PROVIDER` | Profile name (multi) or type (flat): `ollama`, `xai`, `openai_compatible`, … |
| `LOCAL_AGENT_PROVIDER_BASE_URL` | Override active profile `base_url` |
| `LOCAL_AGENT_PROVIDER_MODEL` | Override active profile `model` |
| `LOCAL_AGENT_PROVIDER_API_KEY_ENV` | Env var **name** that holds the API key (never the secret value) |
| `LOCAL_AGENT_PROVIDER_CONTEXT_SIZE` | Host-side context budget for remote models |
| `LOCAL_AGENT_LOCAL_ONLY` | Toggle local-machine endpoint enforcement (`false` required for remote Grok) |
| `LOCAL_AGENT_MODEL` | Also sets the remote model when a remote profile is active |
| `OLLAMA_HOST` | Override `ollama.base_url` |

API key **values** are read only from `os.Getenv(api_key_env)` (for example
`XAI_API_KEY`). Never put the value in YAML.

### Provider secret env names (values from TinyVault)

| Variable | Typical use |
| --- | --- |
| `XAI_API_KEY` | xAI / Grok (`provider.type: xai`) |
| `OPENAI_API_KEY` | OpenAI-compatible OpenAI host |
| `OPENROUTER_API_KEY` | OpenRouter |
| `ANTHROPIC_API_KEY` | Reserved for a future Anthropic adapter / shared vault convention |

### Related TinyVault process env (optional)

| Variable | Purpose |
| --- | --- |
| `TVAULT_PASSPHRASE` | Non-interactive unlock (prefer `tvault agent` for daily use) |
| `TVAULT_DIR` | Vault directory (default `~/.tvault`) |
| `TVAULT_NO_AGENT` | Bypass a running `tvault agent` |

## What stays local

- Default remains Ollama with `privacy.local_only: true` until you open remote profiles.
- Without a remote profile selected, no prompt leaves the machine.
- ICE embeddings still use Ollama when ICE is enabled.
- Expert Team/Swarm/MoE stays local-Ollama multi-model and is disabled while a **remote** profile is active.
- TinyVault MCP is optional for secret *tools*; credentials for providers should use inject (`tvault run`), not `vault_get_secret` into the model context.

## Safety notes

- Prefer least-privilege inject (`--only` / known provider key names) over dumping the whole vault into the shell.
- Do not put API keys in YAML, git, or shell history.
- Remote inference sends prompts and tool receipts to the selected provider once `local_only` is false.
- SuperGrok chat subscriptions are separate from API billing; Local Agent uses the API-key path today.
