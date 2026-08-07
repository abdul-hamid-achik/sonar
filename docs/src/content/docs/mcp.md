---
title: MCP servers and tools
description: The extended tool surface, and the three things about it that are not the same.
---

sonar speaks MCP over STDIO, SSE and Streamable HTTP. Tools from a server are
namespaced by it, results are bounded, connections are health-checked and
reconnect on their own, and every dispatch is traced.

```
/servers            # connection status for each one
/mcp reconnect <name>
/tools [server]     # browse what was discovered
```

Servers are declared in your configuration; see `config.example.yaml` for the
shape.

## Three things that are not the same

This is the one idea worth carrying away, because conflating them is the
standard way to misread an MCP session:

1. **Transport success** — the server was reached.
2. **Domain success** — the thing it was asked to do worked.
3. **Verified evidence** — sonar parsed a contract it actually understands.

A transport failure has no domain outcome at all, and reporting one asserts the
server was reached. sonar keeps them apart: known structured contracts are
parsed into bounded projections, an unrecognised version stays *unknown* rather
than being guessed at, and raw structured output is discarded before it can
reach the UI or the session store.

## local_only bounds these, not inference

`privacy.local_only` refuses remote MCP endpoints, and it is really enforced —
the host resolves the target itself and checks every address it gets back. It
says nothing about the model: every inference request leaves your machine by
construction.
