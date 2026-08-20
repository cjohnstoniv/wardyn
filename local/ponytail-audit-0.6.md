# Ponytail audit — Wardyn 0.6 (whole repo, fresh)

Stage F4.1 of the 0.6 plan. Audits `main` content as of `50848161` in worktree
`/home/cjohn/wt-v06-audit`. Supersedes `local/ponytail-review-0.4.5.md` (stale — most of
its list has since been applied or overtaken).

**F4.1 recorded no code changes.** F4.2 (applied, this file's `Status` column) took the obvious
wins and deferred the rest with a reason. Every applied deletion had its provenance researched
first — `git log -S` on the symbol — and two Tier-1 items did not survive that check.

## Method

- Exported-symbol reachability sweep, Go (`internal/ pkg/ cmd/`) and TS (`ui/src`), counting
  references outside the declaring file — run twice for Go, once counting tests and once
  excluding them, so "alive only via its own tests" separates from "alive".
- Duplicate-name sweep across packages (helpers reimplemented per package).
- Dependency reachability: every `go.mod` direct require and every `ui/package.json` dep.
- Per-package/per-file mass ranking; then read the heads of everything above ~700 lines.
- Env-knob inventory (`WARDYN_*` in code vs `docs/ENV.md`).
- Grepped the existing `ponytail:` markers first so already-accepted debt is not re-litigated.

Prior passes did their job: **zero** unreferenced exported Go functions, **zero** unused Go
modules, and only one unused npm dependency exist repo-wide. The remaining wins are
architectural or test-side, not dead-code sweeping.

## Ranked register

Ranked by (confidence x lines removed x defect-risk removed) / risk-of-removal.
"Δ" is estimated lines removed, negative = deletion.

### Tier 1 — take these (high confidence, low risk)

| # | Status | Location | Cut | Replaced by | Δ |
|---|---|---|---|---|---|
| 1 | **APPLIED** `2bf842bf` | `ui/src/app/components/wardyn/tier-matrix.tsx` (+ `tier-matrix.test.tsx`) | The whole component pair — nothing renders `TierMatrix`; only its own test imports it | `setup/environment-step.tsx:396-417` already renders `CC_MATRIX_ROWS`/`CC_MATRIX_WHERE` with its own `MatrixCell` (:482). Keep `cc-meta.ts`; move the drift assertions the deleted test held into `environment-step.test.tsx` if not already covered | −223 |
| 2 | **DEFERRED** | `internal/api/runs_lifecycle.go:219 SweepTerminalSandboxes` | The unwired state — its own doc says "none are wired up here; this is the primitive itself", so an orphaned live sandbox behind a failed finalize is never reclaimed | Call it from the existing periodic reconcile/lifecycle ticker (~5 lines, `cmd/wardynd`), which also retires the `THREAT-MODEL.md:1040` residual. If nobody will wire it: delete the func + `sandbox_sweep_test.go` (−180) and keep the residual honest | +5 / −180 |
| 3 | **DEFERRED** (doc landed `358f7ad8`) | `internal/secretmask/secretmask.go:108 Evict` | Either the method or the leak — no production caller evicts, so every run's minted-token corpus stays in the process-global registry for the daemon's lifetime | Call `Evict(runID)` from the run finalize path (`finalizeRunTail`), or delete the method and say in the type doc that masking corpora are process-lifetime | +2 / −15 |
| 4 | **DEFERRED** | 40 `*_pg_test.go` files, 93 `WARDYN_TEST_PG` reads — e.g. `internal/db/migrate_pg_test.go:34`, `internal/recording/pgstore_pg_test.go:34`, `internal/broker/concurrency_pg_test.go:40`, `cmd/wardynd/gt_rotator_pg_test.go:43` | The per-file copy of skip-if-unset + Connect + Migrate + Cleanup (`pgPool`, `runsPGPool`, `newPGStore`), plus `migrateTolerant` duplicated in `internal/db` and `internal/secretstore/pg` | One `internal/db/dbtest` package with `Pool(t)` (stdlib `httptest` convention); the recording copy's own comment already admits it "mirrors" the others | −200 |
| 5 | **APPLIED** `cb351ba9` | `ui/src/app/components/ui/switch.tsx` + `@radix-ui/react-switch` in `ui/package.json:29` | Both — the primitive has no importer anywhere in `src/` or `e2e/` | Nothing; it is the only unused dependency in the tree | −31, −1 dep |
| 6 | **PART-APPLIED** `83883777` | `ui/src/app/components/wardyn/form-primitives.tsx:95 DomainPillList`; `new-run/wizard-types.ts:775 WorkspaceProfileOption` + `:776 workspaceProfileOptions`; `wardyn/copy.ts:69 SETUP_RESIDENCY_NOTE`, `:109 RISK_ATTRIBUTION` | Five dead exports — declaration is the only occurrence in `src/` and `e2e/` | Nothing. `RISK_ATTRIBUTION`/`SETUP_RESIDENCY_NOTE` are canon strings with no render site; confirm against the mock before deleting (mock-first UI law) | −60 |
| 7 | **REJECTED** | `internal/identity/embedded/revocation.go:30 NewMemRevocationStore` | A test fixture shipped in a production file (no non-test caller) | Move `MemRevocationStore` into `embedded_test.go`, or into `identitytest` if the conformance suite needs it | −25 |
| 8 | **APPLIED** `358f7ad8` | `internal/component/registry.go:59 Lookup` | The `export` — only `Resolve` (same file) calls it | Unexport to `lookup` | 0 |
| 9 | **APPLIED** `358f7ad8` | `internal/store/pagination.go:120-137` (`ApprovalsByRunCreatorPager` doc) | The "PRODUCTION WIRING NOTE … that wiring is out of this lane's scope … until it lands" paragraph — stale: `cmd/wardynd/adapters.go:139` delegates it and `:143` asserts it | Delete the paragraph; the next reader currently believes a shipped path is unwired | −12 |
| 10 | **APPLIED** `83883777` | `ui/.../corp-network-egress.tsx:46`, `demos/demo-screen.tsx:497`, `run-detail-ssh.tsx:199`, `run-detail/widget-registry.ts:188`, `new-run/run-warnings.ts:28,43`, `settings/harness-login-pane.tsx:202`, `setup/corp-network-proxy.tsx:53,68` | The `export` keyword on nine symbols used only inside their own file (no test imports them either) | Module-local `function`/`const`; shrinks each file's public surface with zero behavior change | 0 |
| 11 | **APPLIED** `83883777` | `ui/src/app/components/screens/demos/demo-screen.tsx:252`, `screens/app-shell.tsx:540` | Two `TODO(stage-4): /settings` breadcrumbs | Nothing — `/settings` shipped (`App.tsx:327`); the TODOs are stale | −2 |

### Tier 2 — worth doing, needs a judgement call

| # | Location | Cut | Replaced by | Δ |
|---|---|---|---|---|
| 12 | `internal/workspacescan/ai.go` (+ `ai_test.go`, `api/scanresult.go:105-124`, `api/server.go:376-386`, `cmd/wardynd/boot_deps.go:387`, `boot_flags.go:184`, `docs/ENV.md:83`) | The whole `WARDYN_SCAN_AI_ADVISOR` lane: off by default, needs a resident `claude` CLI on the host PATH, advisory-only, fails open, cannot override a deterministic fact, and always forces `needs_review` | The deterministic marker table (`detect.go`) that already runs — the advisor's best case is "an operator reviews it anyway". A previous pass demoted it to opt-in instead of deleting; a knob nobody sets is the same YAGNI one rung later | −650 |
| 13 | `internal/identity/registry.go`, `internal/secretstore/registry.go` (+ their share of `internal/component`) | The plugin seam for two of four seams: `identity` has exactly one impl (`embedded`), `secretstore` exactly one (`pg`) — no second implementation exists or is scheduled | Direct construction in `cmd/wardynd/boot_deps.go`. **Contested:** `docs/PLUGGABILITY.md` sells these as extension points, so this is a product decision, not a code one. `recording` (fs+pg) and `substrate` (docker+k8s) genuinely have two impls — keep `component` for them | −180 |
| 14 | `internal/identity/identitytest/conformance.go`, `internal/secretstore/secretstoretest/conformance.go` | Two shared conformance suites each run against exactly one implementation | Fold each into the single impl's own `_test.go`. Only worth it alongside #13 — if the seams stay public, the suites are the contract | −190 |
| 15 | `internal/broker/broker.go:183-217` (`Querier`, `Row`, `TxBeginner`, `Tx`) + `internal/broker/pgx.go` | Four interfaces mirroring pgx's own shapes so unit tests can fake Postgres | `pgxpool.Pool`/`pgx.Tx` directly, with the mint tests moved to the `WARDYN_TEST_PG` lane the repo already runs. **Contested:** costs the broker its daemon-free coverage, and `make ci` enforces a 65% cover floor without a database | −150 |
| 16 | `internal/store/pagination.go:75-90` (`Pager` type-assert) + its call sites in `api/runs.go`, `api/approvals.go`, `api/policies.go`, `api/workspaces.go`, `api/audit.go` | The "fall back to unbounded `List*` + window in Go" branch — production is always PG, which implements `Pager`; the fallback exists only so test fakes keep working | Make the fakes implement it (or embed `store.PG`), then the handler calls the method directly. The sibling `RunsByCreatorPager` already fails closed rather than falling back, so the two halves of this file disagree about what an absent impl means | −120 |
| 17 | `internal/setup/detect_proxy.go:582-663` (maven XML, apt.conf, npm/pip/cargo ini greps) | The per-tool config detectors — five bespoke parsers feeding a wizard *suggestion* that the operator confirms by hand anyway | Keep env + shell-profile + git + OS lanes (the ones with real precedence semantics); an unlisted tool's proxy is one paste into the field | −150 |
| 18 | `internal/api/http.go:152`, `:185`, `internal/api/sshgateway_channels.go:94`, `cmd/wardynd/main.go:707`, `cmd/wardyn/main.go:115` | Five inline copies of "parse host, is it loopback" — already self-flagged at `cmd/wardyn/main.go:103` as "fourth inline loopback predicate in the tree" (now five) | One exported `IsLoopbackHost(hostport string) bool` in a leaf package both `cmd/` and `internal/api` already import (`internal/cliutil` or a new 10-line `internal/netutil`) | −30 |
| 19 | `internal/gitremote/gitremote.go:87,131` vs `internal/workspacescan/scan.go:285,338` | `depthUnder` + `readCapped` duplicated between the two tree-walkers; the workspacescan copy's own comment says "duplicated rather than exported, it's three lines" — but it is now two functions, not one | Export from `gitremote` (already a leaf that `workspacescan` imports) — the "it's only three lines" defence stops holding at the second function | −25 |
| 20 | `docs/ENV.md` vs code: 163 `WARDYN_*` names in `internal/ cmd/ pkg/`, 140 documented rows | Both directions of drift: `WARDYN_COMPOSER_CONFIG`, `WARDYN_ENVBUILD_TEST_NETWORK`, `WARDYN_KATA_VERSION`, `WARDYN_UPDATE_GOLDEN` are undocumented; and 163 knobs is itself the finding | Document the four (or mark the test-only ones as such), then cull: any knob with exactly one sane value in every deployment is a constant. Add the both-directions grep to `make ci` so this cannot drift again | −? |

### Tier 3 — noted, do not act without owner input

| # | Location | Observation | Note |
|---|---|---|---|
| 21 | `scripts/record-demo.sh`, `verify-demo-take.sh`, `demo-typist.sh`, `narrate-*.py`, `demo-ffwd.py`, `scripts/demo-beats/`, `ui/e2e/demo/*.spec.ts`, `docs/DEMO-SCRIPT.md` | ~11.3k lines (≈7% of the repo) of marketing-video harness, all landed in the last week, coupled to every console surface — a UI change breaks ten specs at once | Not a delete: it is the shipped `/demos` deliverable. But 0.6 changes console surfaces (WS-D), so budget the re-take, or narrow the specs to the beats that actually appear on camera |
| 22 | `internal/api/server.go` `Config` — 47 exported fields, several optional-hook funcs (`ScanAIAdvisor`, …) | A 47-field constructor argument is the accumulation point every optional feature lands in | Splitting it is a bigger diff than it is worth today; each Tier-2 deletion above removes a field. Re-measure after F4.2 |
| 23 | `App.tsx:325,334` | Three `/integrations*` → `/settings` redirects for a screen deleted in 0.5 | Bookmark compatibility is a real reason to keep them; 0.6 is the natural place to drop them if the owner is willing |
| 24 | 16 of 40 `scripts/*.sh` do not source `scripts/lib/common.sh` (`check-*.sh`, `test-podman.sh`, `test-report.sh`, `demo-typist.sh`, `stage-*.sh`, …) | Two conventions for logging/colour/cleanup in one script tree | Low value alone; fold in whenever one of those scripts is edited for another reason |
| 25 | `internal/sidecar/sidecar.go:5` | Doc says "binaries" plural, names only `wardyn-scan`; actual importers are `cmd/wardyn-scan` and `cmd/wardyn-aws-sso` | One-word doc fix, not a cut — recorded so the next reader does not "discover" a dead package |
| 26 | `internal/lifecycle/lifecycle.go:228 Tick` | Exported only so the same-package tests can drive one scan; `Run` (:201) is the only production caller | Unexport if `lifecycle_test.go` is in-package (it is). Trivial, bundle with #8 |

### F4.2 outcomes — what the provenance check changed

Every Tier-1 item below was researched with `git log -S <symbol>` before any deletion. Three
came back different from how the audit read them.

**#2 SweepTerminalSandboxes — DEFERRED, a design decision, not a cut.** Deleting is off the table:
`git log` puts it in `26ae235b` (leased watchers / leader-elected rotator), written deliberately
for the gap `THREAT-MODEL.md:1040` names — grep-dead is not purposeless. Wiring it is not the
5 lines the audit estimated either. It calls `Store.ListRuns` (the WHOLE table, unpaged) and
probes `Runner.Status` for every terminal run carrying a `SandboxRef`, so on a ticker its cost
grows with run history forever, and — like the reaper beside it — it needs `reapTickLock`-style
leader election or every replica sweeps the same runs at once. Owner call: boot-only (bounded,
misses post-boot orphans), locked ticker with a bounded/paged query, or an admin route.

**#3 secretmask.Evict — DEFERRED; the proposed fix was wrong and is now documented as such.**
Calling `Evict` from `finalizeRunTail` would have created a leak, not closed one. Masking sites
take their `Snapshot` lazily, at use time, and several read AFTER the run is terminal — most
sharply `cmd/wardynd`'s `maskingRecorder`, which masks each audit event's `Data`/`Target` as it
is recorded, finalize's own audit and any `teardown_error` included. An empty snapshot masks
nothing (this layer fails OPEN by design), so evicting at the terminal transition unmasks
exactly the events most likely to quote a credential. `358f7ad8` writes that ordering
constraint into `Evict`'s doc so the next reader doesn't rediscover it in production. The
underlying bound — a run's corpus lives as long as the process — is real and still open; it
needs an eviction point after the last reader, which nothing currently identifies.

**#4 pg test bootstrap — DEFERRED, and the audit's own sizing was wrong.** "40 files, 93 reads"
counts every file that reads `WARDYN_TEST_PG`; the actual duplication is SIX bootstrap helpers
(`internal/broker`, `cmd/wardynd`, `internal/store`, `internal/recording`, `internal/db`,
`internal/secretstore/pg`), each with a different return shape (`*pgxpool.Pool`, `(*Store,
*pgxpool.Pool, age.Identity)`, `(*Server, *pgxpool.Pool)`), so one `Pool(t)` does not absorb
them. `internal/db`'s copy is in-package (`package db`), so a `db/dbtest` helper cycles unless
that file moves to `package db_test` first. Real, worth doing, needs the live PG lane to verify
— not an obvious win.

**#6 — PART-APPLIED.** `DomainPillList`, `workspaceProfileOptions`/`WorkspaceProfileOption` and
`SETUP_RESIDENCY_NOTE` are gone. Provenance says all three lost their callers to `ef49039d`
(the 5-step wizard's retirement), and `SETUP_RESIDENCY_NOTE` is additionally superseded:
`lib/integrations.ts`'s `RESIDENCY_META` covers all three of its keys plus three more, and it
is what the Settings cards actually render.

`RISK_ATTRIBUTION` stays — **DEFERRED, mock-first law.** It is not orphaned copy: `RiskBadge`
renders in `profile-review.tsx`, so there IS a live risk-grade surface, and it simply never
shows the "Graded by Wardyn's rules, not the model." attribution D8 asks for. Deleting the
string would quietly settle an honesty question in the wrong direction. Owner call: render it
next to the badge, or drop the requirement.

The same commit found two neighbours the audit missed, both collateral of `ef49039d` and both
dead in-file (not merely over-exported): `parseMissingSecret` and `useAddSecretFix`
(`run-warnings.ts`), ~90 lines of the launch-error "add the missing secret and retry" wiring,
whose own docs name the deleted `wizard.tsx` as a caller. Deleted. **Owner note:** that is a UX
regression that already happened — the one-page New Run screen never grew the affordance back.
Recorded here rather than rebuilt in an audit lane.

Also cleared while in there: `demo-screen.tsx` and `runs-first-run.tsx` (x2) still linked to
`/integrations`, so each click bounced through `App.tsx`'s compatibility redirect. They name
`/settings` directly now — which makes **#23** purely a question about external bookmarks.

**#7 NewMemRevocationStore — REJECTED, the finding is wrong.** It has no *production* caller,
but `test/apie2e/harness_test.go:327` calls `embedded.NewMemRevocationStore()` from a DIFFERENT
package. Symbols declared in `_test.go` files are visible only to that directory's own test
binary, so moving it into `embedded_test.go` breaks the apie2e suite. It stays where it is.

**#26 lifecycle.Tick — REJECTED, same class of error.** `lifecycle_test.go` is
`package lifecycle_test` (external), so `Tick` cannot be unexported without breaking it. The
audit assumed in-package and said so explicitly; it was wrong.

### Tier 2 and 3 — all DEFERRED

Untouched by F4.2, by design. Every Tier-2 entry is either a product decision (#12 AI advisor,
#13 pluggability seams, #14 their conformance suites, #15 the broker's daemon-free coverage) or
a multi-file refactor that wants its own lane with the live PG/e2e suites (#16 the `Pager`
fallback, #17 proxy detectors, #18 the loopback predicate, #19 the tree-walk helpers, #20 the
ENV.md both-directions drift + a `make ci` gate for it). Tier 3 is observation, not worklist —
except #23, whose in-app half is now done (see #6 above).


## Checked and judged sound — do not re-audit

Recorded so the next pass spends its budget elsewhere. Each of these looks like a candidate
and is not:

- `internal/api/metrics.go` — hand-rolled Prometheus text exposition instead of
  `client_golang`. Correct call for a security product with SBOM/licence/vuln gates; the
  file already carries the ceiling comment.
- `internal/api/compose_setup.go SetupItem` vs `internal/api/setup.go SetupCheck` — look
  like duplicate checklist models; are not (prose `Fix` vs structured UI-drivable `Fix`),
  and the distinction is documented at the type.
- `internal/audit/sinks/{file,syslog,webhook}.go` — three sinks, all reachable via
  `WARDYN_AUDIT_SINKS`, hand-rolled rotation has no stdlib equivalent and no dep is worth it.
- `pkg/client` — SDK surface; unused exports there are the product, not dead code.
- `internal/api/{site_config_probe,integrations*}.go` — survived the 0.5 integrations
  collapse on purpose; both still route (`routes.go:280-310`) and both back live console UI.
- `internal/dockerutil` (40 lines, one func) — genuinely shared by `runner/docker` and
  `envbuild`, which keep separate narrow Docker-client interfaces. Correct as-is.
- `internal/component` for `recording` + `substrate` — two real implementations each.
- npm dependency tree — every dep except `@radix-ui/react-switch` (#5) is reachable.

## Net

**F4.2 as landed: 22 files, +64 / −474 (−443 excluding the lockfile), one npm dependency,
across four commits**
(`2bf842bf`, `cb351ba9`, `83883777`, `358f7ad8`). Six Tier-1 items applied, one part-applied,
two rejected as wrong findings, three deferred with a named decision behind each.

The two "additions, not deletions" both survived contact with their own code and neither is an
addition any more: #2 is a sizing question (unpaged `ListRuns` per tick + leader election), and
#3's proposed call site turned out to be a live footgun that is now documented at the method.

Tier 2 still holds roughly **−1,500 lines**, but three of its seven entries are product
decisions (pluggability seams, AI advisor, broker test strategy), not code decisions.
