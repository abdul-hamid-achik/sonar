# Pending specs

Specs in this directory are **not run**. `task glyphrun` and
`task glyphrun-contracts` glob `specs/*.yml`, which does not descend here.

A spec belongs here when it describes behaviour sonar does not currently have,
and the reason is a decision rather than a bug. Deleting it would lose the
coverage if the decision goes the other way; leaving it in the suite would make
CI permanently red for something nobody intends to fix this week. Each one is
listed below with what would have to be true for it to move back.

Their committed snapshots stay in `.glyphrun/snapshots/<spec-name>/`, keyed by
spec name rather than path, so a spec moves back with a single `git mv`.

## `tui_agents.yml`

Drives an expert consultation — several models reviewed in parallel and
reported in the Agent Hub. The feature consulted a **multi-model local
inventory**: it picked distinct models off `/api/tags` and ran them side by
side. A hosted provider serves one model per profile and exposes no inventory
to choose from, so the feature as built had nothing to stand on once every
provider became remote.

This parking is now **permanent, not deferred**: the expert machinery itself
has been deleted — the Agent Hub and expert UI, the agent-side
`consult_experts` dispatch, and the `internal/expertteam` /
`internal/expertselector` packages are gone, and `internal/tools` (drift-
synced, so it still defines `consult_experts`) is filtered so the definition
never reaches the model. There is no longer a gate to flip.

This is the part of the Ollama decision that cost something real. Moving to
cloud-only Ollama did not just delete a daemon; it removed the only surface
that could satisfy the expert team.

The spec stays as the behavioural record of what was removed. **Moves back
when:** expert consultation is rebuilt from history on multiple hosted
provider profiles instead of a local inventory — a rebuild, not a revert.
