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

## `tui_ollama_inventory.yml`

Asserts that the model picker badges an Ollama Cloud model `CLOUD · kimi-code:cloud`.
sonar renders `LOCAL · kimi-code:cloud`, because `internal/ui/ollama_inventory.go` —
the file that sets `descriptor.Source = OllamaModelCloud` — does not exist here.
It was removed along with the rest of the local-first inference machinery. Both
sites that build a descriptor now hardcode `OllamaModelLocal`.

So the spec is not failing; it is describing `local-agent`, where it still runs
and still passes. Making it pass in sonar would mean restoring cloud-model
classification to a harness whose own documentation calls the Ollama path
inherited dead weight and not supported.

**Moves back when:** `provider: {type: ollama}` is confirmed as a supported
path and the inventory UI is rewired. **Delete it when:** that provider type is
removed instead.
