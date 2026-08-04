---
title: Testing and verification
description: Verify Local Agent with Go race tests, linting, vulnerability checks, and deterministic Glyphrun terminal contracts.
outline: deep
---

# Testing and verification

Local Agent combines code-level Go gates with black-box terminal behavior specs.

## Repository verification

```bash
task verify
```

The verification task runs:

- the VitePress production website build and rendered internal-link check;
- `go mod tidy -diff`;
- the portable TinyVault provider-wrapper smoke test using synthetic metadata;
- `golangci-lint`;
- `go vet`;
- the complete Go test suite with the race detector;
- `govulncheck` v1.6.0.

`task verify` covers the local Go and public-site gates. Glyphrun has separate
contract, fast, and full tasks because it exercises the compiled program through
a pseudo-terminal.

## Pinned versions

The repository currently fixes these versions or version ranges:

| Surface | Repository contract |
|---|---|
| Application toolchain | Go 1.25.12 from `go.mod` |
| Website runtime | Node 22 or newer; CI uses Node 24 |
| Website packages | VitePress 1.6.4 and Vue 3.5.39 |
| CI linter | golangci-lint 2.12.2 |
| Vulnerability scanner | govulncheck 1.6.0 in `Taskfile.yml` and CI |
| Terminal contracts | Glyphrun 0.14.3 in CI, installed with Go 1.26.x; local-agent is then rebuilt with the Go 1.25.12 application toolchain |

The local `golangci-lint` and `glyph` tasks use the executables on `PATH`; use
the CI versions above when reproducing CI exactly.

## Continuous integration

The verification workflow has four independent jobs:

- Linux Go verification: module diff, golangci-lint, vet, race tests,
  integration-tag compile-only coverage, the provider-wrapper smoke test, and
  govulncheck;
- VitePress production build plus a test of links in the rendered site;
- all committed Glyphrun contract hashes plus the complete deterministic
  terminal suite from `task glyphrun`;
- a macOS build, `--version` and provider-wrapper smoke tests, and
  Darwin-sensitive package tests.

## Terminal behavior

[Glyphrun](https://glyphrun.dev/) drives the compiled application through a real pseudo-terminal and verifies user-visible outcomes against a deterministic terminal emulator.

```bash
task glyphrun-contracts
task glyphrun-cli
task glyphrun
task test:integration
```

`task glyphrun-contracts` verifies that every committed spec still matches its
reviewed intent and outcomes. `task glyphrun-cli` is the fast local smoke suite
for public flags, skip-approval behavior, and exact external-file reads.
`task glyphrun` prebuilds Local Agent once, then runs every deterministic spec;
this keeps a cold dependency build outside the bounded per-spec terminal
timeouts. The same execution gate runs in CI, while specs under `specs/live/`
remain explicitly opt-in.

`task test:integration` keeps the build-tagged integration package compiling
and runs its host-side checks. Tests whose live dependency is unavailable
self-skip; this task is not a model-quality evaluation.

Committed scenarios cover normal and minimum terminal sizes, authority modes,
public CLI parsing, explicit external-file review, inline goal review and
approvals, goal receipts, the Ollama inventory, composer paste behavior,
session restore, receipt inspection, saved reasoning, transcript follow
behavior, settings, and Help. Their fixtures are Go programs or repository
data, so the suite does not require system `sqlite3`, `rg`, or a `scripts/`
directory.

When a scenario contract intentionally changes, verify and stamp only that
changed spec:

```bash
glyph spec verify specs/<changed>.yml --stamp
glyph spec verify specs/<changed>.yml --format md
glyph run specs/<changed>.yml --format md --progress always --parallel 1
```

The contract hash covers `intent`, `outcomes`, `redaction`, and `coversSymbol`.
Do not restamp a mismatch merely to make verification pass; review the changed
contract first.

Refresh snapshots only for an intentional visual change:

```bash
task glyphrun-snapshots
git diff -- .glyphrun/snapshots
```

Snapshots are evidence of rendering; they are not a substitute for outcome assertions.

## Optional live Ollama smoke

With a running local Ollama and the documented default `qwen3.5:2b` already installed, run the constrained-model tool scenario separately:

```bash
task eval
EVAL_REPEATS=5 task eval
```

The task checks the configured `OLLAMA_HOST` (loopback by default) for an
already-installed `qwen3.5:2b`, then repeats the exact tool-call contract three
times by default and reports Glyphrun's structured stability result. It exits
if the model is unavailable and never runs `ollama pull` or downloads weights.
This is a narrow live smoke—not a repository task-completion benchmark—and is
intentionally outside the deterministic hosted-CI suite.
Any failed repeat makes `task eval` exit nonzero. The structured result and the
corresponding `.glyphrun/runs/` artifact distinguish tool dispatch, final-answer
grounding, and process completion instead of collapsing a model miss into a
generic timeout.

## Related evidence tools

- [Cairntrace](https://cairntrace.dev/) describes and verifies browser behavior.
- [Vidtrace](https://vidtrace.dev/) converts bug recordings into timestamped evidence bundles.
- [file.cheap](https://file.cheap) stores and compares agent workflow artifacts.

These are related tools in the wider ecosystem, not bundled Local Agent dependencies.
