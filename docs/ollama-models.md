---
title: Ollama models
description: Understand Local Agent's live Ollama inventory, automatic local routing, Cloud consent, context windows, and model downloads.
outline: deep
---

# Ollama models

Ollama is Local Agent's only implemented inference runtime. The model picker is driven by Ollama's live inventory, including installed local weights and available Ollama Cloud aliases.

## Open the inventory

Press `ctrl+o` or run:

```text
/model
```

The picker groups entries by execution location:

- **Local** — model weights available on this Ollama instance.
- **Cloud** — aliases reserved for manual selection; under local-only policy they require exact conversation consent.
- **Remote** — models exposed by a non-Cloud remote host; these are visible but not selectable.

Any row can also be unavailable when blocked by runtime capability, context metadata, privacy policy, or the local memory guard.

Use `d` for runtime and capability details, `a` to open the cancellable pull form, and `r` to refresh the inventory.

## Automatic routing and pins

The TUI starts with availability-aware automatic routing. It considers admitted local models only.

```text
/model qwen3.5:4b   switch to and pin one model
/model auto         release the pin and resume automatic routing
/model list         print the current admitted inventory
```

Choosing a verified local model from the picker also pins it and remembers the choice across process restarts. `/model auto` clears that saved choice. An explicit startup `--model` flag or agent-profile model takes precedence, while Cloud consent remains limited to the current conversation and is never restored implicitly.

## Local and Cloud are different boundaries

Cloud models can remain visible when `privacy.local_only: true`. Selecting one is a deliberate exception for the current conversation:

1. Choose the Cloud entry.
2. Review the destination and privacy boundary.
3. Confirm the exact model for this conversation.

Cloud models are never chosen by automatic routing. Consent does not change the saved local-only setting or authorize a different Cloud model.

## Context windows

Local Agent reconciles the live model's native context with the configured local request limit:

- Local requests use the lower of the configured `ollama.num_ctx` and the model's reported native maximum.
- Cloud requests omit the local `num_ctx` option and display the Cloud model's reported native maximum.
- Unknown Cloud context limits fail closed instead of displaying a guessed number.

This keeps session statistics aligned with the request that was actually sent.

`num_ctx` is a request and KV-cache allocation, not a promise that the whole
native model maximum is practical on the machine. For example, a model can
report a much larger native limit while Local Agent correctly sends the smaller
configured value. Start at `16384` for the documented compact 2B/4B tiers and
raise it only after accounting for model weights, the embedding model, and
other local processes. Pair small models with a lazy MCPHub profile so the
first provider request does not include every downstream tool schema.

## Suggested local tiers

Artifact sizes vary by Ollama build and quantization. The shipped guard is tuned for a 16 GB Apple-silicon machine, not measured free memory.

| Model | Intended use | Auto routing |
|---|---|---|
| `qwen3.5:0.8b` | Short answers and lightweight classification | Eligible |
| `qwen3.5:2b` | Compact interactive work and modest tool chains | Eligible |
| `phi4-mini:latest` | Alternative compact reasoning profile | Exclusive |
| `qwen3.5:4b` | Coding, debugging, review, and multi-step tools | Eligible |
| `qwen3.5:9b` | Deeper manual profile | Exclusive |
| `gemma4:e2b` | Multimodal compact profile | Exclusive |
| `ornith:latest` | Manual agentic coding and verification profile | Exclusive |

The shipped Phi profile is manual-only because its advertised Ollama metadata has not been backed by behavioral tool-use verification in this harness. Its size is not the reason for the restriction.

Large-model admission can be overridden with `LOCAL_AGENT_ALLOW_LARGE_MODELS=1`, but the override does not add memory isolation or make an oversized model safe for the machine.

## Troubleshooting

### The first request already fills the context window

Check the active `ollama.num_ctx` and model with `/model list`. A large native
context advertised by Ollama does not override the configured request limit.
Then reduce the recurring MCP schema surface: use MCPHub gateway mode with
`expose: lazy`, set the Local Agent MCPHub entry to `pin: []` and
`tool_schema_budget: "0"`, and restart the agent after syncing the change. The
[MCP guide](/mcp#small-model-gateway-profile) shows the complete public
configuration.

Local Agent estimates prompts before dispatch and may compact tool definitions
for one pressured turn. If it still cannot keep the prompt below the admission
threshold, it stops instead of sending an overfilled request. Lower the amount
of loaded context or use a model and `num_ctx` combination that fits available
memory; raising a model's advertised maximum does not solve an oversized first
prompt.

If the picker is empty or stale:

```bash
ollama list
curl http://localhost:11434/api/tags
```

Then press `r` in the picker. An offline refresh keeps only a previously verified safe local model; it never reclassifies a Cloud alias as local.
