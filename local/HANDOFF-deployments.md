# Deployments campaign handoff — for the UI campaign's integration, then `~/.claude/plans/ultra-velvety-bee.md`

Branch `feat/v0.7-deployments` in `/home/cjohn/wt-v07-deploy`, branched from `feat/v0.7-phase0` @ `c9fe30ae`
(which has NOT moved since — a fast-forward integrates it). Code head `86a298ee` = 76 commits over the base; this file's commit sits on top.
Plan: `~/.claude/plans/handoff-prompt-starry-hopcroft.md` (v9 + execution log). Unpushed, untagged, no
version bump — release finalization stays with the cleanup pass.

## 1. Corrections for the next agent — things the cleanup plan and the UI handoff state as facts that moved

1. **Files split at the 1000-line gate** (each lane passed alone; the merge crossed): `internal/api/server.go` →
   `internal/api/security_headers.go` (CSP + headers); `cmd/wardynd/main.go` → `cmd/wardynd/keypem.go`;
   `internal/egress/proxy/local_routes.go` → `internal/egress/proxy/llm_routes.go` (the `/wardyn/llm/*`
   handlers, classifiers, `isLLMHost`) and `internal/egress/proxy/egress_target.go` (`egressTarget`,
   `gatewayTarget`, `vetTrustedHost`, the 6d lift helpers). Any line-number cite into those four files is stale.
2. **Test counts:** UI `1319` (files `94`); Playwright lists `194 tests / 35 files`; `go test ./...` all packages
   ok; the PG-backed tests (`*_pg_test.go`, incl. the new `internal/api/injection_owner_pg_test.go`) run only
   with `WARDYN_TEST_PG` set — `make ci` runs them EMPTY (Makefile), so run them explicitly against
   `wardyn-test-pg` :55432 before calling a store/secrets change done.
3. **Deleted / moved surfaces:** `MemberSetupNotice` is gone (`screens/onboarding/onboarding-screen.tsx`
   renders `member-getting-started.tsx` for members); the account-menu Demos entry is hidden for members;
   `SectionCard.title` is now optional (`wardyn/primitives.tsx`) — the People step's cards are untitled because
   the page heading already carries "Who can sign in"; `ShellMeta` gained `resolved` (the `/me` fetch settled)
   and `RoleProvider` exposes `useRoleResolved()`; `FirstRunLanding` (`App.tsx`) waits for BOTH status and role.
4. **e2e fixtures:** `mockMemberRole` lives in `ui/e2e/fixtures.ts`. A spec must never import another spec —
   Playwright then collects ZERO tests suite-wide and the run looks green. On `/setup` there are TWO
   `<header>`s (shell + funnel); scope to `page.locator("header").first()` before `.last()`-ing a button.
5. **Secrets are per-principal** (migration `0050`, PK `(owned_by, name)`): `secretstore.Store.For(owner)` is the
   chokepoint; `secretOwnerFromRequest(r)` is the ONE owner helper (`""` for operators);
   `presentSecretNamesFor(ctx, owner)` feeds every request-scoped verdict; `resolveIntegrationRef(ctx, owner, ref)`
   takes the owner explicitly. `GET /secrets` returns `{names, mine}` (`lib/api/secrets.ts` `listSecretsMine`).
   `bedrock-api-key` + the SigV4 trio are refused for non-operators. `resolveRunUpstreamProxy` stays operator.
6. **Egress:** `egressTarget(host, port)` returns `(target, ruleSource, err)` and has NO gateway branch —
   `gatewayTarget` is called only by the brokered LLM route. `rule_source: site-config:internal-host` marks a
   6d lift. `SiteConfig.InternalHosts` is validated against `ipguard.Liftable` (RFC1918, fc00::/7, 100.64/10).
7. **Wire fields with no UI reader yet** (Go + tests only; TS mirrors are hand-maintained, add when rendered):
   `SetupStatus.trusted_ca_certs`, `/healthz.network_policy`.
8. **Videos:** `ui/src/app/lib/demo-videos.ts` is the ONE manifest (13 shipped on literal `"v0.6.0"` tags, 8
   `tag: null`); `cmd/wardynd/demo_videos_guard_test.go` ties it to README; the CSP `media-src` lists
   `https://github.com https://release-assets.githubusercontent.com` (the live 302 target — `RELEASING.md`
   step 7 re-checks it); `security_headers_test.go` asserts the segment exactly.
9. `MintOnApproval` (`internal/broker`) has no production caller — it now carries the run's `CreatedBy` as `Sub`,
   fixed as dead code, not a live leak. `routes.go`'s operator-only count comment was stale; recounted.

## 2. What this campaign shipped (commit map, oldest first)

**Phase 1 — install paths by audience (docs lane; reviewed, then audited):** `778a9f37` README Install split by who runs the box · `a82e8863` `docs/MEMBERS.md` · `a1123d15` install.sh closing summary names the mode · `3b295bad` setup.sh/Makefile team-mode copy · `581f907c` compose README · `da7d3ae3` installer pin honesty · `5c43af5c` install.sh is token mode; ARCHITECTURE CSP wording · `c828998d` DESKTOP/Helm one-liners.

**Phase 3 — videos (videos lane):** `48c1cd07` `demo-videos.ts` manifest · `51783b82` README↔manifest guard · `8dcc74bb` CSP `media-src` · `96569d07` RELEASING re-shoot step · `196ab8de` ARCHITECTURE one-external-resource paragraph · `a420f695` exact-segment header test.

**Phase 4 — corporate CA (CA lane):** `25fb2933` boot knob · `51f660de` proxy trusts the forwarded bundle · `2e9e7f81` bundle in `WARDYN_PROXY_CONFIG_JSON` · `d63d1335` sandbox trust append · `c0505c2f` compose/desktop/Helm delivery · `92191232` docs + BYOI ceiling · `9825400f` threat-model #28 · `7ee86af2` `trusted_ca_certs` on `/setup/status` · `4b737ed5` published `agent-base` stub installs the bundle (review fix) · `aca18da7` certificates-only bundle + sidecar control-plane client trusts the pool (audit fix).

**6c commits 1–4 — per-principal secrets (secrets lane):** `c7d58c0e` migration 0050 · `c4080d9f` `Store.For(owner)` · `2a303aaf` self-service `/secrets` · `b97952a6` resolve/mint from the run owner · `c2c9bb19` `PUT ?owner=` + Bedrock names on DELETE (review fix) · `6c2424ff` two-file split at the 1000-line gate (merge-only).

**6b + 6a (lane C):** `046102b8` `k8sNetpolVerdict` + `/healthz.network_policy` · `0d78339a` boot audit `k8s.netpol_unenforced` · `99b5bb78` `ruleSourceLabel` + `RuleSourceChip` · `e878aae6` chip label families + `/approvals?tab=decided` (review fix).

**Phase 5 (a) — member Getting Started (lane a):** `201967b8` `DoneChip` · `b05dd8cb` episode rows/list · `824bbed2` `useWorkspaceList.error` · `df457e2a` seen flag · `c9237012` "Your model key" · `a68a0020` member screen · `7da5b2f7` wired in, Demos hidden · `cb04a8b5` e2e spec · `34c62ca6` `mockMemberRole` in `fixtures.ts` (review fix) · `6f4177d0` one `listSecretsMine` fetch (review fix) · `1e2b7a7e` TopBar Demos test · `f5923f51` copy.ts blocks.

**Phase 5 (b) — People step + landing (lane b):** `0214bcaf` `deploymentMode` · `2692988b` People step · `c7522b5b` episode rows on every step · `fc5fb69c` landing waits for the real role · `3a96ad9c` `resolved` = fetch settled (review fix) · `8f6d8ffe` one "Who can sign in" heading (review fix) · `39cd2a0e` `.catch` on the meta fetch · `b379352c` admin e2e negative control scoped to the shell header (daemon run fix).

**Phase 6 — 6d/6e/6c-5 (lane A):** `529470c2` internal hosts · `50ef6894` gateway knobs + `llmProviderFor` seam · `490015f4` `llm_routes.go` split (pure move) · `1f6ede7f` gateway dial on the api-key lane · `7c64d9ec` artifact-redirect veto · `9aec9f1a` docs · `26ec4e67` member own-key grant arm + owner threading + `MintOnApproval` Sub · review fixes `bcc5f3b8` (gateway vet scoped to the brokered route — the blocker) `f254c3ab` `d883435c` `42b04bb5` `f7809f77` `65d6b7ad` `4764d663` `78f94257` (pg-backed invariant-1 test) `e14396cc` `ba37cca8` · `ff1e6b18` workspace requirements + `GET /integrations` on the caller's presence map · `86a298ee` selected-integration lane folds the member's own key; literal-IP + k8s doc ceilings (audit fix).

**Phase 7:** `e730b125` CHANGELOG + ROADMAP · `a11bd9d7` `docs/img/getting-started.png` · `c18e4177` CHANGELOG folds + MEMBERS.md integration-credential rule · this file.

## 3. Owner-gated / decided

- **Playback proof in real engines** (plan Verification §6): Playwright's Chromium lacks H.264, so the e2e proves
  request counting only. Owner: on the e2e daemon (`scripts/run-ui-e2e.sh`, :8088) press Watch on episode 01 in
  Chrome AND Firefox — `loadedmetadata` fires, zero CSP console errors across the
  `github.com → release-assets.githubusercontent.com` redirect.
- **People step layout:** the two cards are untitled (the page `h2` carries the canon string; a titled
  `SectionCard` doubled the same `h2` for assistive tech). The mock cannot confirm this from the tree — a
  one-line owner yes/no. The token-mode lede was tightened to the plan's variant ("the token the installer
  printed"); the shared body still hedges for both modes.
- **`.wslconfig`:** add `processors=24` (owner's file) — three concurrent full gate runs starved the host once.
- **Episode 02 re-shoot** should film `install.sh` (README now calls `make setup` a contributor path); the
  audience episodes `02b/02c/04b/04c/11/12/12b/13` stay `tag: null` until recorded.
- **Decided (mine, routine):** `GET /secrets` gains additive `mine`; ceiling-pairing filter on member-visible
  operator names ships; Bedrock stays operator-namespace and out of 6e; 6e = api-key lane only; upstream-first
  for the gateway (a direct-dial bypass is a ROADMAP line); `denyAlwaysReject` covering the gateway is intended;
  BYOI corp-CA-only ceiling accepted + documented; the 0.6.4 findings report stays off the tree.

## 4. Release state — deliberately NOT done

No tag, no push, no version bump, no CHANGELOG version heading (one consolidated `[Unreleased]` set under
`<!-- Audience-restructure campaign (0.7) -->`, below the UI campaign's). Gates at `86a298ee` (2026-08-29): `go test ./...` all packages ok · `make lint` 0 issues (image-pin gate OK) · file-size gate OK · `scripts/test-install-sh.sh` PASS · `scripts/test-desktop-profile.sh` PASS · PG-backed `internal/store`, `secretstore/pg`, `broker`, `api` ok against `wardyn-test-pg` :55432 · `scripts/run-ui-e2e.sh` 17/17 spec files · UI typecheck/`pnpm test` 94 files / 1319 tests / build OK (at `aca18da7`; `ui/` unchanged since) · `make screenshots` regenerated and committed.

## 5. Named gaps (ROADMAP lines exist for each)

Subscription/managed runs through the gateway (the published `agent-run` unsets `ANTHROPIC_BASE_URL` for
resident creds — image change, 0.8); a gateway needing its own header shape; a per-target direct-dial bypass
under a corporate upstream; air-gapped video mirror / config-driven CSP; member-BYO-Bedrock; the shell banner on
`/healthz.network_policy` + a substrate-honest `ConfinementChip`; the Network step's "N trusted CA certs";
`isModelProviderHost` is a plain `HasSuffix("anthropic.com")` (post-clamp `allowed_domains` is the load-bearing
half of the own-key arm — a label-suffix match would be tidier); `auth.mode: "disabled"` falls into the token lede
(unreachable per `setup.go`).

## 6. Working rules that cost time this campaign

- **Load rule:** ≤2 concurrent agents; every gate `nice -n 10 env GOMAXPROCS=8 go test -p 4 ./...`, `nice -n 10 make lint`,
  `nice -n 10 pnpm test -- --maxWorkers=4` (the `--` is required), ONE heavy gate at a time. A `wsl --shutdown`
  wipes `/tmp` (scratchpads, staged briefs, skill bundles) and stops `wardyn-test-pg` — pass briefs inline.
- **Reviews find what gates miss:** every lane passed its own gates and every review round still returned
  must-fixes (a gateway hostname lift on the sandbox CONNECT path; a fail-closed landing signal; a spec that
  imported a spec; a header-scoped click; `PUT ?owner=` ignored; the published `agent-base` stub never installing
  the interception CA). Counterfactual (red-then-green) every new assertion; a negative control must run on the
  SAME page shape as its positive.
- **Merges cross gates lanes clear alone** (the 1000-line file gate) — split by seam, never allowlist.
- **Citation guards:** `docs/AUDIT-ACTIONS.md` ±6 lines (re-point, never delete); no `file.go:NNN` in Go comments
  or the threat model; ENV.md two-way ratchet; a commit chain with an `echo` masks a red test — fail-fast chains.
- **Two committing agents never share a worktree**; lanes rebase + fast-forward; re-pin HEAD before branching.
