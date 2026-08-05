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
reported in the Agent Hub. `cmd/sonar/main.go` gates that feature on
`!provider.IsRemote()`, and every provider is remote now, so it is off in every
configuration sonar supports.

That is not an oversight in the gate. The runtime consults a **multi-model
local inventory**: it picks distinct models off `/api/tags` and runs them side
by side. A hosted provider serves one model per profile and exposes no
inventory to choose from, so the feature as built has nothing to stand on.

This is the part of the Ollama decision that cost something real. Moving to
cloud-only Ollama did not just delete a daemon; it removed the only surface
that could satisfy the expert team.

**Moves back when:** expert consultation is rebuilt on multiple hosted provider
profiles instead of a local inventory. **Delete it when:** the expert-team
runtime is removed with the rest of the local machinery.
