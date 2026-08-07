---
title: Configuration
description: Where sonar reads its settings, and what the file can say.
---

Configuration is optional — the defaults resolve to DeepSeek. The complete,
commented surface is `config.example.yaml` in the repository, and that file is
the reference: it is checked into the same commit as the code it describes, so
it cannot drift from it the way a copy here would.

## Where it looks

The first matching file wins. Files are **not** merged:

1. `./sonar.yaml`
2. `./sonar.yml`
3. `$XDG_CONFIG_HOME/sonar/config.yaml`
4. `$XDG_CONFIG_HOME/sonar/config.yml`
5. `$HOME/.config/sonar/config.yaml`
6. `$HOME/.config/sonar/config.yml`

Environment overrides apply afterward.

## Secrets are never in the file

Only environment variable **names** are configured. The value is read from the
process environment at launch:

```bash
export DEEPSEEK_API_KEY=...
# or, with a secret manager:
tvault run -p sonar --only DEEPSEEK_API_KEY -- sonar
```

## Shared profiles and skills

Agent profiles live under `~/.agents/agents/<name>/agent.yaml`, and shared
skills under `~/.agents/skills/<name>/SKILL.md`. Both are read from outside the
repository so they can be used across projects.

## privacy.local_only bounds tools, not inference

It refuses remote MCP servers, and it is really enforced — the host resolves the
target itself and checks every address it gets back. It cannot make sonar
private: every inference request leaves your machine by construction, because
that is what a hosted model is.
