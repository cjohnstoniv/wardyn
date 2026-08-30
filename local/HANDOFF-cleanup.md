# Cleanup-pass handoff — 0.7 prepped, not released (for the owner's release steps)

**Branch `feat/v0.7-phase0` in `/home/cjohn/wt-v07-phase0`, HEAD `df8d1d71`, 16 commits past the deployments handoff
(`7dca6081`); base unchanged (`origin/main` @ `4f6fc979` = 0.6.6). Committed, NOT pushed, NOT tagged, no version
bump.** Every commit is `-s` by cjohnstoniv. Gates at HEAD: `make release-check` (= `make ci` + CHANGELOG grep +
PG lane, `WARDYN_TEST_PG` set) green · `scripts/run-ui-e2e.sh` 17/17 · `make screenshots` regenerated + committed ·
Playwright collects 190 tests / 34 files (the retired spec's 4 fewer).

Read this, then `local/HANDOFF-deployments.md` §1 for the deployments corrections that still apply.

## 1. Corrections for the next agent

1. **`make lint` + `go test ./...` green is NOT `make ci` green.** At `7dca6081` `make ci` had two latent reds nobody
   had run into: staticcheck SA1019 in `cmd/wardynd/trusted_ca_test.go` (`//nolint` only speaks to golangci-lint; standalone
   staticcheck needs `//lint:ignore` — the test now asserts by verifying the CA against the pool instead) and a gitleaks
   `generic-api-key` false positive on the migration filename in `store_secret_migration_pg_test.go` (fingerprint added to
   `.gitleaksignore`). Run `make release-check` before calling a range done.
2. **`CHANGELOG.md`'s released sections had 513 injected lines.** The rebase-restore commit (`35c38b9f`) pasted the
   0.7 phase-0 bullets INTO this branch's `## [0.6.5]` section; 41 `[Unreleased]` bullets were therefore duplicated
   against "released" text that `origin/main` never had. Released sections are now byte-identical to `origin/main`'s
   (`diff <(sed -n '/^## \[0.6.6\]/,$p' CHANGELOG.md) <(git show 4f6fc979:CHANGELOG.md | sed -n '/^## \[0.6.6\]/,$p')`
   is empty — keep it that way). `[Unreleased]` is ONE Added/Fixed/Changed set (67 bullets), heading unchanged.
3. **The AUDIT-ACTIONS citation window is ±6 lines and an emit site sat at exactly +6:** one inserted line above
   `harness.login.started` tripped `TestAuditActionsDocCitationsAreLive`. Re-point in the same commit (done: `:465`→`:472`).
4. **golangci-lint's cache can hold entries from a deleted worktree** (`../wt-deploy-ca/...` surfaced as a funlen
   "issue"); `golangci-lint cache clean` before trusting a lint verdict that names a path outside the tree.
5. **Subagents cannot write report files** (the harness refuses "FINDINGS/REPORT"-shaped writes) — they return text;
   the orchestrator persists it. Every subagent is also injected with the memory index and the ponytail persona.
6. `internal/api/helpers.go` `sortedKeys` is a deliberate non-nil wrapper (JSON `[]`, never `null`) — named in
   `AGENTS.md` now; the two private copies elsewhere were folded into `slices.Sorted(maps.Keys(m))`.
7. `scripts/record-demo.sh` globs `<NN>-*.spec.ts` — an un-numbered spec can never be recorded; that is how a
   988-line file stayed unreachable for weeks.

## 2. What this pass shipped (commit map, oldest first)

- `46725283` docs: add AGENTS.md, the working standard for agents and humans
- `f7dda2fc` fix(api): revoke the minted run identity when a harness-login run fails to persist
- `c1270fcd` docs(agents): name sortedKeys in the reuse list
- `06efd2db` refactor: apply the re-review's Go cuts — dead helpers and stdlib swaps
- `98740bac` refactor(ui): apply the re-review's UI cuts — canon strings, a stranded comment, eight narrowed exports
- `768d9234` refactor: the trunk-verified cut list — stdlib for map-key sorts, collect[T], newStepRun, one constant
- `c754296b` docs(changelog): fold the three [Unreleased] sets into one Added/Fixed/Changed
- `8b292c05` refactor(ui): one HostList for the allowed/denied host cards
- `8e7326a2` demo: retire the pre-split policies episode spec, keeping its 24 unique captions
- `316f8076` scripts: env_get for the m-prime probe, PASS printed last, and a flag that could never be 0
- `68c0208e` docs(changelog): drop the tool_rules bullet re-pasted into [Unreleased]
- `eb7a5a42` docs(changelog): released sections match origin/main again; [Unreleased] reads as one release
- `75ace344` test(wardynd): assert the trusted CA verifies against the pool, not a deprecated subject count
- `35e02b2e` test(api): license header on the new revoke test
- `150ed8f2` chore(gitleaks): ignore the generic-api-key false positive on a migration filename
- `df8d1d71` docs(img): regenerate screenshots at HEAD

Net: `git diff --shortstat 7dca6081..HEAD` = 53 files changed, 473 insertions(+), 1994 deletions(-).

## 3. Re-review verdicts worth knowing (the finder → skeptic chain, all rows)

- Go range: 8 findings, 5 applied (dead `secretsFor`/`secretsForRun`, duplicated `gatewayHostFromBaseURL`,
  `firstNonEmpty`→`cmp.Or`, two test-helper key sorts); 2 routed into the `sort.Strings` sweep; 1 measure-only
  (`handleCreateRun` reconstructs to 150–151/150 funlen — run `golangci-lint run --enable-only funlen ./internal/api/`
  before touching it). UI range: 5 findings, 4 applied, 1 refuted (`classifyProxyState` — the two ladders differ).
- e2e/docs range: the retired spec (24 unique captions lifted to `local/episode-06-firstrun-proposal.md`);
  `check_video_08_policies` KEPT (it is the content arm for 05/06's floor checks); `03c:159/161` is deliberate
  descending anaphora, NOT a cut; the CHANGELOG 0.6.5 brevity cut was REFUTED (released text) and replaced by the
  real defect (the re-paste + the injected section).
- KEEP additions from this pass: `helpers.go sortedKeys`; `check_video_08_policies`; `corp-network-proxy.tsx`'s
  "deliberately identical" verdict brief vs `corpNetworkGate`; `MemberSetupNotice` stays deleted.

## 4. Owner-gated / open (do not decide these yourself)

- **Two narration cuts are ready but unapplied — each needs its own owner-timed rehearse on the live `:8080` series
  stack** (`scripts/record-demo.sh --no-record --video NN`, `/healthz` answering, never `--reset`, unbatchable):
  `06-your-first-run.spec.ts:582` "It's not the open internet." (third consecutive negation) and
  `07-interactive-runs.spec.ts:369` (restates `:367` behind a false "That means"). Fold into the next re-record round.
- `09-record-a-run.spec.ts:422` "So don't guess. Watch." — trim or deliberate emphasis?
- `ui/src/app/lib/workspace-copy.ts` `RD`, `V2C`, `RD2` are dead canon (only the test imports `RD`/`V2C`; nothing
  imports `RD2`; the ROADMAP line that justified keeping them is gone): delete, or render via a mock round.
- `internal/broker.MintOnApproval`: no production caller; provenance `031d1729`; the finder recommends keeping it as
  the documented seam mirroring `MintForGrant` — a keep-vs-delete call for you, not a lane.
- The People step's token lede also catches `auth.mode: "disabled"` (unreachable per `setup.go`) — copy nit.
- `docs/img/runs-board.png` re-renders with pixel churn on every `make screenshots` — a stability candidate.
- From the deployments handoff, still open: real-engine playback of episode 01 (Chrome + Firefox on the e2e daemon);
  the People step's untitled cards; `processors=24` in `.wslconfig`; episode 02 should film `install.sh`; the eight
  `tag: null` episodes stay stubs.
- Six `keysOf`/`keys` test-helper copies remain across out-of-range test files (only the two in range were cut).

## 5. Release state — prepped, deliberately NOT released

No bump (0.6.6 in `internal/version/version.go`, `deploy/helm/wardyn/Chart.yaml`, `ui/package.json` + their tests),
no tag, no push. `[Unreleased]` is release-ready. Your steps, per `RELEASING.md`:
1. Rename `## [Unreleased]` → `## [0.7.0] — <date>` and bump the three version strings (and the tests that pin them).
2. Commit (`-s`), cut `release/0.7`, tag `v0.7.0`, push branch + tag.
3. After the workflow: `gh api repos/cjohnstoniv/wardyn/releases/tags/v0.7.0 --jq '.draft, (.assets|length)'` must
   print `false` and ≥ 11 (F.1a — the net for the 0.6.2/0.6.3 no-Release defect); then run `docs/VERIFY.md`'s
   blocks against the tag (F.7).
4. Re-shoot per `RELEASING.md` step 7 when the video campaign is ready (F.4 stays undone on purpose).

## 6. The build-vs-contribute decision

The memo and its evidence pack live untracked at `local/build-vs-contribute-2026-08/` (`DECISION.md` is the verdict;
`RULES.md` the pre-registered rules; `EVIDENCE.md`, `MATRIX.md`, `census.txt`, the two Terraform sketches, and the
two blind lanes' files). **Verdict (fresh Fable judge, pre-registered rules, two blind Opus lanes): A — stand alone.** Judge-scored
"expressed" count 2 of 9 (3 at the most generous reading; the fold threshold was 5); external governance PRs
merged into the AGPL core in 12 months: 1 (threshold 3). The complement option as sketched (B1) validates but fails
on a surviving failure mode — a member-scoped launch token resident in every workspace shell lets the agent decide
its own holds; the revision that would make it viable (launch control-plane to control-plane, no token in the
workspace, a launch scope excluded from the approve verb) is named in `DECISION.md` §6 and is the natural next
integration seam. The one fact that would flip A→C is also named there (a merged mid-flight hold verdict plus a
host-side non-TCP drop in the upstream firewall). A durable copy sits at `~/.claude/plans/build-vs-contribute-2026-08/`.

## 7. Working rules that cost time this pass

- One heavy gate at a time, and `set -o pipefail` + print `PIPESTATUS` — a `| tail` hid a red twice before I added it.
- Terraform loads a directory as ONE module and takes no file argument — one sketch per directory.
- `grep -c` returning 0 breaks a `&&` chain (it exits 1): an append after it silently never ran.
- Adding a line above a guarded audit emit site is a doc change too — re-point `docs/AUDIT-ACTIONS.md` in the same commit.
- `git add` on a tracked file under a gitignored directory still needs `-f`.
- **`terraform init` inside `local/` breaks `make lint`**: the module cache pulls a whole upstream registry whose Dockerfiles
  have unpinned `FROM` lines, and `scripts/check-image-pins.sh` walks `./` including gitignored `local/`. Keep
  `.terraform/` out of the worktree (run `init` in a `$HOME` scratch dir) — same class as the compete-clones gotcha.
