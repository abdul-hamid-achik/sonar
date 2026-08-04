---
title: Expert teams, swarms, and MoE
description: Use bounded read-only expert consultations that adapt concurrency to the host machine.
outline: deep
---

# Expert teams, swarms, and MoE

Local Agent can ask several ordinary agent profiles for independent advice and
return their reports to the parent agent for synthesis. This is an
**application-level** mixture of experts, not a token-level MoE architecture
inside one model.

Ask naturally for a team, swarm, or mixture of experts. When the
`consult_experts` tool is enabled, the parent model can select it with a bounded
objective and optional exact profile names. The host permits at most one expert
consultation in each parent turn.

## Strategies

| Strategy | Selection behavior |
| --- | --- |
| `team` | Uses exact requested profiles, or a stable bounded group when no names are supplied. |
| `swarm` | Combines task relevance with profile diversity and avoids equivalent profile contracts. |
| `moe` | Routes locally from the objective to matching `use_cases` and descriptions; a built-in generalist is the no-match fallback. |

Selection is deterministic and local. The selector reduces the objective to a
bounded lexical signal set; it does not call another model, tool, registry,
network service, or filesystem. Selection receipts contain host-authored
reasons rather than copied prompt fragments or paths.

With automatic fan-out, the built-in logical caps are three Team experts,
seven Swarm workers, and four MoE experts. Configuration and the remaining
parent budget may reduce those counts; the resource plan independently admits,
rejects, or serializes the resulting inference work.

Profiles from the selected agents directory participate alongside the
built-in architect, critic, explorer, generalist, and verifier profiles. A
profile's `model`, `description`, `use_cases`, and `system_prompt` shape its
consultation. Skills and `mcp_servers` do not: expert calls deliberately receive
no tools. The directory defaults to `~/.agents`; `agents.dir` and
`LOCAL_AGENT_AGENTS_DIR` can select another shared root.

## Exact model assignment

An expert request can choose one model for the whole consultation with `model`
and override individual selected profiles with `model_overrides`. Resolution is
deterministic: a per-profile override wins, then the request-wide model, then
the model configured on the profile, and finally the current parent model.

```json
{
  "strategy": "team",
  "objective": "Review the release boundary",
  "experts": ["critic", "verifier"],
  "model": "qwen3.5:2b",
  "model_overrides": [
    { "expert": "critic", "model": "qwen3.5:0.8b" }
  ]
}
```

Overrides must name known profiles selected in the same request. Unknown or
unselected profile names fail closed. A requested model is routing intent, not
an authority grant: it must still pass the current model inventory,
`local_only` and remote-execution rules, Cloud consent, context admission, and
resource admission. A natural-language request affects routing only when the
parent model emits a valid exact `consult_experts` request.

## Resource adaptation

Before each consultation, the host refreshes Ollama's live model inventory and
residency, then takes one current CPU/RAM snapshot. The planner combines the
selected models' live local weight sizes and effective `num_ctx` with every
local model already resident in Ollama. The complete accepted local weight set
must fit that one snapshot, even when the calls will run sequentially. The plan
keeps these limits separate:

- simultaneous calls that share one model's weights;
- simultaneous calls using distinct model weights;
- logical Team, Swarm, or MoE fan-out.

On a constrained or unknown-memory host, simultaneous concurrency falls to one
when the accepted models fit. If the whole selected local set does not fit, the
runtime reduces fan-out deterministically in selector order; it does not assume
that serial calls make unbudgeted resident weights free. If the measured policy
cannot admit even one inference, the consultation fails before an expert model
call. A larger host may run several reports in parallel. Explicit configuration
values are caps only and cannot force concurrency above the measured plan;
impossible explicit CPU reserve or thread settings fail closed.

On Linux, the probe intersects host telemetry with the process-visible cgroup
v1 or v2 CPU quota, effective cpuset, memory limit, and current memory usage,
including inherited hierarchy limits. Linux uses `MemAvailable` when present.
macOS uses physical memory with a conservative total-memory estimate because
there is no equivalent stable `MemAvailable` sysctl. Other or unavailable
probes fail safe to serial inference.

Verified Ollama Cloud and configured remote models do not reserve local model
weights. An all-external consultation stays serial because Local Agent cannot
infer provider capacity from host telemetry. Mixed consultations still budget
every accepted local model. Each verified external expert receipt carries a
visible `CLOUD` or `REMOTE` execution-boundary notice; an unknown inventory
location never implies local execution. Existing local-only policy, remote-host
configuration, and exact conversation-only Cloud consent remain authoritative.

The planner measures system or unified RAM, model weights, and a conservative
KV-cache estimate. It does not yet have portable discrete-VRAM telemetry or
model-specific KV allocation data, so the plan is an admission guard rather
than a throughput benchmark or a guarantee against every runtime allocation.

After the consultation, Local Agent asks Ollama to unload only selected local
models that became active solely for expert work. It protects the current model,
models that were already resident for non-expert use, and a selected model that
gains a non-expert user while the consultation is running. Cleanup failure is a
bounded warning and does not erase completed reports. Ordinary model switches,
chat, embeddings, and shutdown wait behind the expert residency lease; cleanup
and cancellation remain deadline-aware rather than waiting indefinitely.

## Evaluation budgets

`max_eval_tokens` is a ceiling for each child, not a second pool outside the
parent. When a parent turn has an evaluation-token limit—including a bounded
Goal turn—the remaining parent budget becomes the aggregate cap for the whole
consultation. If that remainder cannot give every selected expert at least one
token, the deterministic fan-out is reduced. The remaining allowance is then
distributed across the admitted children, with no child exceeding
`max_eval_tokens`.

Charged child usage is added to the parent turn before synthesis, so the parent
model receives only the remaining allowance. If an expert entered provider
streaming but its terminal usage is missing or cannot be trusted, Local Agent
charges that child's reserved limit conservatively. Invalid aggregate usage
fails closed and can consume the parent's remaining reservation rather than
allowing the Goal budget to be bypassed. A known local rejection before any
provider callback does not consume the child reservation.

## Authority and evidence

Every child call is read-only and receives no built-in or MCP tools. It cannot
inspect a repository, open an external media file, execute Vidtrace, or mutate
state by itself. The parent agent remains responsible for tool calls,
approvals, file authority, and verification.

The Runtime overlay reports expert availability, profile count, and the
read-only boundary explicitly. That status describes host capability only; it
does not mean a consultation ran or produced verified evidence.

An active consultation occupies one expandable transcript card. Its summary
shows finished, active, and queued work; expanded details show each profile,
exact model, host-verified execution location, status, and charged evaluation
tokens. This progress projection is bounded: it does not contain the objective,
report body, provider prose, or private reasoning. If the host cannot verify an
execution location, the UI reports it as unknown.

Reports are advisory tool output, not evidence. A completed expert inference
does not prove that a claim is correct or that an external action succeeded.
When some experts succeed and others fail, the bounded result retains the
completed reports alongside typed failure codes; partial success never becomes
verified evidence and is projected as attention rather than semantic success.
Raw provider errors are projected to bounded host codes, and every report plus
the aggregate result has an independent byte limit.

Cancellation is shared across the consultation, with an additional timeout and
generation-token cap for each expert. Queued experts do not start after
cancellation, and data received after an expert's terminal usage receipt is
rejected. Any bounded partial receipt returned with cancellation remains an
error/advisory result. Model admission, local-only policy, and explicit Ollama
Cloud consent remain enforced by the existing model manager.

## Configuration

```yaml
experts:
  enabled: true                      # default
  max_concurrent_inference: 0       # 0 = auto
  max_concurrent_distinct_models: 0 # 0 = auto
  max_team_experts: 0               # 0 = built-in policy cap
  max_swarm_workers: 0
  max_moe_experts: 0
  max_eval_tokens: 768
  timeout: 90s
```

Experts are enabled when the block is omitted because `enabled` defaults to
`true`. Set it to `false` to remove `consult_experts` from the model tool
catalog.

See [Configuration](/configuration) for validation rules and the annotated
repository example.
