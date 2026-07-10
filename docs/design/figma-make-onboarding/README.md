# Figma Make onboarding design — source snapshot

> Note: this is a partial, non-compiling reference snapshot — its components import modules that are
> not included here. Read it for design intent; don't try to build or run it.

Snapshot of the Figma Make project `EXAMPLEMAKEKEY00000000` ("Redesign onboarding process",
published prototype: https://example.figma.site) taken 2026-07-10 at the
migration-ready gate (round-3 sources, all ship-blockers fixed).

This snapshot covers the surfaces ported in the first migration pass (Welcome, funnel shell,
phase rail, Environment matrix picker, Model step, shared primitives, tokens, step data).
The workspace-wizard and later-pass tab designs live in the Make project and are re-pulled
via the Figma MCP (`ReadMcpResourceTool`, uri
`file://figma/make/source/EXAMPLEMAKEKEY00000000/<path>`) when their re-skin passes start —
the MCP is main-conversation-only (subagents cannot fetch these URIs).

Design law carried by these sources: frozen 9 step ids/labels, the fixed status vocabulary,
"Doesn't stop:" residuals always ≤1 interaction away, the constant-note verbatim exactly once,
grants/warnings amber never green, done-states from live probes only.
