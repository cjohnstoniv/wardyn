# Working in this repository — for coding agents and humans alike

Wardyn is a governed-sandbox control plane. The security invariants, the gates and the size caps already
exist and are enforced elsewhere; this file tells you where they are and how to write code that survives
them. It restates nothing that would drift.

## 1. Simplicity

- No interface with one implementation, no factory for one product, no config knob for a value nothing
  sets, no wrapper that only delegates, no scaffolding "for later".
- Reuse before you write. The shared homes, so you can find them before re-implementing them:
  - `internal/api/helpers.go` — `parseIDParam`, `notFoundIf`, `decodeStrict`, `readCappedBody`, `refreshRun`,
    `getWorkspaceOr404`, `ownsWorkspaceOrAdmin`, `unionAllowedDomains`.
  - `internal/store/pagination.go` — `collect[T]` for every `rows.Next()` loop; `store.Pager`.
  - `internal/cliutil`, `internal/sidecar`, `internal/dockerutil` — CLI, sidecar and Docker-client helpers
    shared across `cmd/` and `internal/`.
  - `scripts/lib/*.sh` — shell logging, colour and cleanup; new scripts source it.
  - `ui/e2e/demo/{stage,overlay,narrator,funnel,task,sweep}.ts` and `ui/e2e/fixtures.ts` — the demo and e2e
    primitives (a spec must never import another spec).
  - `ui/src/app/components/wardyn/primitives.tsx` — the console's shared components; strings live in
    `ui/src/app/components/wardyn/copy.ts`, `ui/src/app/lib/workspace-copy.ts` and
    `ui/src/app/lib/permissions-copy.ts`.
- Prefer the standard library: `slices.Sorted(maps.Keys(m))` over a collect-then-sort loop,
  `slices.ContainsFunc` over a `found := false` loop, `cmp.Or` over an if-chain of defaults.
- Deletion beats addition. Boring beats clever. The shortest diff that is correct in the right place wins;
  the smallest change in the wrong place is a second bug.
- A bug fix fixes the root cause where every caller routes through it, not the symptom the report named.

## 2. Comments

Comment the *why*, never the *what*. The test for keeping one:

- A doc comment on an **exported** identifier stays (godoc convention).
- A comment that restates the signature or the next line goes — no `// gets the X value`.
- A comment naming a non-obvious constraint, a known ceiling, a gotcha, or a deliberate simplification
  stays. The repository's `ponytail:` notes are the model: name the ceiling and the upgrade path in one
  line (`// ponytail: global lock; per-account locks if throughput matters`).
- Never cite code by `file.go:NNN` in a Go comment or in `threatmodel/*.md` — a guard refuses it because
  line numbers rot. Cite symbols.

`CONTRIBUTING.md` already says it: "sparse comments on constraints only".

## 3. Point at, never restate

- **Gates:** `make ci` is the merge gate; `scripts/run-ui-e2e.sh` (Playwright) is NOT in it and must run
  before a console change is called done. Guards in `cmd/wardynd/*_guard_test.go` run on every full
  `go test ./...` — run the full tree, not `./internal/...`.
- **Invariants:** `ARCHITECTURE.md` (the numbered invariants) and `threatmodel/THREAT-MODEL.md` (residuals) are
  authoritative. `CONTRIBUTING.md`: "do not restate a count here, it is what drifts" — the same law applies
  to this file.
- **Size and complexity caps:** `scripts/check-file-size.sh` (split by seam, never allowlist) and
  `.golangci.yml` (`funlen`, `gocyclo`, `gocognit`).
- **Console:** `docs/design/CONSOLE-RULES.md` is binding; a "simplification" that reintroduces an ad-hoc size,
  a fourth elevation, a hex literal or a second copy of a rule is a regression, not a cleanup. Visual
  changes go through a mock round first.
- **Docs that cite code:** `docs/AUDIT-ACTIONS.md` and `docs/MEMBERS.md` are guarded — re-point a citation,
  never delete it.

## 4. Before you delete

Grep-dead is not purposeless. Run `git log -S <symbol>` and read why it was written; check `local/` too —
working notes invoke tools by prompt text, not by Makefile. Record the provenance in the commit body.
