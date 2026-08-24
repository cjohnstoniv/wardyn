> **REGENERATED 2026-08-23 at the enterprise-POC re-cut.** The earlier 0.6 handoff (written at
> `0eb1ce0e`) was superseded when the owner reopened 0.6 to pull the 0.7/0.8 enterprise work forward.
> That campaign is complete: Phase 2a (21 discovery fixes incl. the D1 egress-deny bypass), Batch 1
> (10 lanes: per-user API tokens, age-key rotation, hash-chained audit, env_secret + git_pat lease +
> second-human, auth.failed, session revocation, arm64 + supply-chain, desktop envelope, member-role
> backend M1) and Batch 2 (member M2 + desktop install lane + the mock-first console batch) — every
> lane and every integration delta Fable-verified (one flagged Opus fallback, §7). Migrations
> 0044–0049. Plan + full ledger: `~/.claude/plans/fluffy-orbiting-tiger.md`.

# Wardyn 0.6.0 — owner handoff

Everything the owner needs to cut 0.6.0. Written at `prep/v0.6` @ the re-cut release commit
`9afe60ff` (`release: 0.6.0`) plus its correction commits, 2026-08-23. Nothing below was done on
`main` or in the primary checkout, and nothing was pushed.

---

## 1. STATE

| | |
|---|---|
| Release commit | `9afe60ff` — `release: 0.6.0` (RELEASING.md steps 1 + 1b, template `0eb1ce0e` reapplied) |
| Corrections after it | `f6fd65ab`+`9950a1bf`+`2f33fb7d` (no parallel test suite inherits WARDYN_TEST_PG), `7aa32fed` (e2e-ssh admin key stamps role_checked_at + SSH.md note), + this handoff commit — **tag the branch TIP**, not `9afe60ff` |
| Branch | `prep/v0.6`, worktree `/home/cjohn/wt-v06-prep`, clean |
| `main` absorbed through | `8ba22fb2` — **main has since moved a lot** (demos-absorption restructure `10a2e144`); see the rehearsal below |
| Enterprise-POC scope | Phase 2a (4 lanes, 21 D-fixes) + Batch 1 (10 lanes) + Batch 2 (3 lanes) + integration fixups; migrations `0044`–`0049` (next free: `0050`) |

**Merge rehearsal — run 2026-08-23 at `f6fd65ab` against `main` @ `10a2e144`:**

```
$ git -C /home/cjohn/wt-v06-prep merge-base --is-ancestor main HEAD ; echo $?
1                                      # main is NOT absorbed — no longer a fast-forward
$ git -C /home/cjohn/wt-v06-prep merge-tree --write-tree main HEAD
… 12 CONFLICT lines …
```

`main`'s `10a2e144` (`feat(setup)!: Getting Started IS the demos surface — /demos absorbed`) is a
breaking UI restructure that collides with this campaign's console work. The 12 conflicts are NOT
demo-only:

- **modify/delete (hardest):** `ui/src/app/components/screens/new-run/network-dialog.tsx` and its
  test — **deleted on `main`**, modified here (the D33/D8 canon-string copy + its pin test live in
  them). At the re-sync, find where main moved the Unlisted-hosts UI and re-land the canon strings
  from `docs/design/ui-batch2-mock.md` (the mock stays the source of truth) plus the D33 pin.
- **content:** `CHANGELOG.md`, `demos/demo-runner.tsx`, `new-run/new-run-screen.tsx`,
  `new-run/wizard-types.ts`, `run-detail.tsx`, `setup/setup-layout.tsx`,
  `workspace-detail/record-pane.tsx`, `wardyn/live-approvals.tsx` + its test, `lib/types/policy.ts`.

Budget a real reconciliation pass (an hour, not minutes), then `make ci` + the ui suite again before
merging to `main`. Re-run both rehearsal lines first — `main` is still moving.

### Gate evidence

All at the re-cut tip (`2f33fb7d` + the handoff commit) unless noted; logs under
`/tmp/claude-1000/-home-cjohn-containerized-agent-envs/c3ceae31-29f9-4044-a0ef-7a79234f50c2/scratchpad/`.

| Gate | Result | Log |
|---|---|---|
| `make release-check` (ci + CHANGELOG + serialized PG lane) | **EXIT=0** — after closing the WARDYN_TEST_PG leak class: every parallel-package suite (unit/docker/k8s/race/test) now strips the DSN; only `test-report-pg` (`-p 1`) sees it, matching CI's job split | `release-check-3.log` |
| PG suite (real Postgres, migrations 0044–0049 applied) | **EXIT=0** inside release-check (`-p 1` — parallel packages raced the shared site_config/audit-chain singletons; both racers green in isolation, serialization is the fix) | same |
| Playwright ui-e2e (14 specs) | **EXIT=0, 14/14** on a quiet box. First run 13/14: the per-spec backend hit ERR_CONNECTION_REFUSED on `workspaces.spec.ts` under concurrent-suite load — no panic in the backend log; pure load race | `ui-e2e-rerun.log` |
| `make test-conformance-docker` | **EXIT=0** (`ok test/conformance 87.9s`), hermetic | `conf-docker-final.log` |
| `make test-e2e-ssh` | **EXIT=0, 15/15** — including the **first live run of the 0046 admin-override arm** (`data.override=true` audited). Its first execution FAILED exactly as the bounded-stale gate should: the arm's pre-0046 direct-SQL key had no `role_checked_at`; the script now stamps it and SSH.md's operator-mechanism note says so | `e2e-ssh-rerun.log` |
| `make test-e2e-ui-sandbox` | **EXIT=0**, hermetic (project `wardynv06uisbx`, torn down) | `e2e-uisbx-final.log` |
| `helm lint` + template matrix (4 rows) | **lint 0 failures**; defaults REFUSE (secret.yaml:42, admin token), under-configured REFUSE (secret.yaml:53, age key — actionable), minimal render EXIT=0, 0.5-shaped `uiSandbox/ssh=null` render EXIT=0 (`default dict` guards hold) | `helm-render-*.yaml` |
| `make helm-install-test` (kind `wardyn-helm-test`, image `wardyn/wardynd:kind-test`) | **EXIT=0** — postgres + install + rollout + /healthz proof on fresh kind cluster `wardyn-helm-test` (namespace self-cleaned; cluster owner-owed, §6) | `helm-install-2.log` |
| `make test-conformance-k8s` | **ENV-BLOCKED** on this WSL2 box (kind subnet collision, unchanged) — CI on the `main` push is the proof | — |
| Fable verification | Every lane (Phase 2a ×4, Batch 1 ×10, Batch 2 ×3) independently verified with revert-the-fix probes; three integration lenses over the merged deltas — ALL PASS, zero must_fix outstanding. One flagged Opus 5 fallback (§7) | ledger |

Logs live under `/tmp` and do not survive a reboot — copy anything you want to keep before restarting.

---

## 2. OWNER STEPS (RELEASING.md 2–6)

### (a) Coordinate with the demo session

Ask it to stop committing to `main` for the duration. Every re-sync below is invalidated the moment
`main` moves.

### (b) Final re-sync in `/home/cjohn/wt-v06-prep`

`git fetch` is not needed — `main` is local, and nothing here was ever pushed.

```sh
cd /home/cjohn/wt-v06-prep
git status --porcelain                                 # must be empty
git merge-base --is-ancestor main HEAD && echo "already absorbed — skip the merge"
```

If that prints nothing, `main` moved:

```sh
git merge-tree --write-tree main HEAD                  # rehearse; read the CONFLICT lines
git merge --no-ff main                                 # expect the 12 conflicts in §1 (UI + CHANGELOG)
#  … resolve: canon strings re-land per docs/design/ui-batch2-mock.md; run cd ui && npm test …
make ci                                                # must be EXIT=0 again
```

### (c) Merge into `main` — in the PRIMARY checkout

```sh
cd /home/cjohn/containerized-agent-envs
git checkout main
git merge --ff-only prep/v0.6                          # WILL FAIL until §b's re-sync absorbs main
```

After the §b re-sync it may fast-forward again; otherwise:

```sh
git merge --no-ff prep/v0.6 -m "merge: 0.6.0 release prep (enterprise POC)

Permissioning, UI sandboxes, SSH override + bounded-stale re-check, per-user
API tokens, hash-chained audit, session+token revocation, age-key rotation,
env_secret + git_pat lease + second-human, arm64 + supply chain, desktop
envelope + install lane, member-role workspaces (backend + console), V13 beat.
Gates at the prep tip: make ci, release-check (pg -p 1), and the hermetic
live suites — see local/RELEASE-0.6-HANDOFF.md §1."
```

Then, in the primary checkout:

```sh
make ci                                                # EXIT=0 before anything is tagged
```

### (d) Cut the release branch and tag

```sh
git checkout -b release/0.6 <prep tip>                 # the FINAL prep tip (release commit + corrections)
git tag v0.6.0
```

### (e) Push — `main` FIRST, then the branch and tag

Split the push. `ci.yml` runs on `push` to `main`, and `conformance-k8s` (`ci.yml:312`) carries no
`if:` guard — pushing `main` is what finally proves the one gate this box cannot run (§1). Let that
run go green before the tag exists; a tag pushed ahead of a red `conformance-k8s` is a published tag
you have to retract.

```sh
git push origin main                                   # CI proves conformance-k8s here
#   … watch it green first (§g) …
git push origin release/0.6 v0.6.0
```

`enforce_admins` is off on `main`'s branch protection, so this direct push is the sanctioned path
(RELEASING.md §Repo settings).

**`screenshots-fresh` cannot fire on this path.** `ci.yml:116` gates it on
`github.event_name == 'pull_request'`, so a direct push to `main` never runs it — screenshot
freshness stays unproven at the cut regardless of how green the run looks.

### (f) GitHub prerelease

The heading is `## [0.6.0] — 2026-08-23`. The section is far larger than the first cut (the whole
enterprise-POC scope); sanity-check the awk output length and its first/last lines before publishing.

```sh
gh release create v0.6.0 --prerelease --title "v0.6.0" \
  --notes-file <(awk '/^## \[0.6.0\]/{f=1;next} /^## \[/{f=0} f' CHANGELOG.md)
```

Sanity-check the body first: `awk '…' CHANGELOG.md | head -20`.

### (g) Watch the CI-only jobs on that push

Nothing below runs in the local merge gate; the push is their first and only proof.

- `conformance` (docker)
- **`conformance-k8s`** — env-blocked on this box (see §1). CI is the only evidence this gate holds.
- `envbuild-integration`
- `helm-install-test`
- `ui-e2e`
- ~~`screenshots-fresh`~~ — **will not run**: PR-only (`ci.yml:116`), see §e
- `sbom-stub`, `publish-image`, `release`

```sh
gh run watch "$(gh run list --branch main --limit 1 --json databaseId -q '.[0].databaseId')"
```

### (h) DATE caveat

`CHANGELOG.md` says **2026-08-23**. Cutting on any other day:

```sh
sed -i 's/^## \[0.6.0\] — 2026-08-23$/## [0.6.0] — YYYY-MM-DD/' CHANGELOG.md
go test ./cmd/wardyn -run Version
git commit -s --only CHANGELOG.md -m "release: 0.6.0 — correct the release date"
```

---

## 3. DEFERRED LIVE PROOFS

Run these at cut time **with the demo stack down**, or rely on CI.

```sh
docker ps                       # confirm nothing on compose project "compose" / :8080 / :2222
make test-e2e
```

`test/e2e/e2e.sh` uses compose project **`compose`** and calls **`down -v`** — running it while the
demo stack is up destroys the recording stack. That is why it was dropped from the campaign gates.
19 of its assertions failed pre-existing on this host and were **never re-characterized**; a red run
is not automatically a 0.6 regression, but it is not automatically fine either.

```sh
make kind-quickstart            # binds 127.0.0.1:8080 and :2222 — demo stack must be down
make test-e2e-ssh-k8s           # WARDYN_TEST_K8S=1; runs against the cluster quickstart leaves
```

`test-e2e-ssh-k8s` gained **§6 member-denial and §7 admin-override arms** during this campaign.
They are authored and pass `bash -n`; **they have never been run live.** First live run is owed at
the cut. `CHANGELOG.md`, `docs/CI.md` and the script header all now describe this lane as a manual
proof, not a CI job.

```sh
make test-conformance-k8s       # env-blocked here; expect EXIT=2 on this box
```

**macOS desktop smoke (owner hardware, once).** `docs/DESKTOP.md` carries a "run this once, paste
output" block for the install lane (`deploy/desktop/install.sh` → launchd → healthz → site-config
round-trip) — CI cannot cover it and the doc says so. Paste the dated output into DESKTOP.md.

---

## 4. DEMO RE-TAKES (handed off)

From `docs/DEMO-SCRIPT.md` §"Re-take at the release cut" at this tip. Numbering is `main`'s
twelve-episode series (`01 why-govern-agents` … `12 audit-and-attach`) plus 0.6's new `V13`.

| Video | What 0.6 changed | Re-take at the cut? |
|---|---|---|
| **V13** — your terminal, our cluster | **New.** Terminal-only (`scripts/demo-beats/13-terminal-to-the-cluster.sh`), shot against the `make kind-quickstart` cluster | **Yes.** One take was shot and graded during the campaign, but nothing is published to `/demos` and neither the take nor its verify report is on this branch — the cut re-shoots it |
| **V02–V12** (every episode with a browser lane) | No spec and no caption changed, but the **console did**: `Permissions` is a new sidebar entry, so every take on file films a sidebar the shipped console no longer has. This is *every* spec except `01-why-govern-agents` — V11 and V12 are terminal-**first**, not terminal-only, and their browser halves render the same shell | **Yes — restaging only.** The narration is still true; the chrome is stale |
| **V01** — why govern agents | Nothing. It films `ui/e2e/demo/assets/primer.html`, a local deck, and never loads the console | No |
| **V09** — record a run (old 06) | Source comments only. The frozen-counter note the spec carried is retired (0.6 fixed the correlation index), but **no caption, no on-screen string and no beat changed** | Only as part of the V02–V12 restaging above |
| **V12** — audit & attach (old 10) | Untouched in content. Its "dark here, because that sensor is opt-in" line stays true | Restaging only, for its browser half's sidebar |
| **V11** — CI & headless (old 09) | Untouched in content | Restaging only, for its browser half's sidebar |

The enterprise-POC scope likely owes **at least one new episode** (the managed-desktop install +
member-mode story) — recorded in the plan as owed, deliberately not shot by this campaign. Slot it
after the owner smoke run so the video films the honest flow.

V13 is a **first publish**, not a first take: `check_video_13` and the `13)` dispatch arm exist, the
beat script and grader agree byte-for-byte on `${WARDYN_DEMO_WORK_DIR:-…/demo-video-13}/v13-run-id.txt`,
and the grader has been exercised end to end once — but there is no dry-run mode (it reads a live
cluster's audit trail), and that cluster, take and report are all gone. Re-create the cluster, re-shoot,
and run `scripts/verify-demo-take.sh` per take.

---

## 5. MAIN-OWNED DEMO-DOC DRIFT (for the demo session — deliberately not edited in prep)

Found by the 0.6 review lenses. All of it lives in files `main` owns; editing them here would have
guaranteed merge conflicts with the recording session.

- **`docs/DEMO-SCRIPT.md:65`** — "`reset-all` runs for **video 01 and the no-flag walkthrough only**".
  False: `scripts/record-demo.sh:131` reads `[[ -n "${VIDEO}" && "${VIDEO}" != "02" && … ]] && DO_RESET=0`
  — the reset defaults **on for `--video 02`**, not 01.
- **`docs/DEMO-SCRIPT.md:72`** — the "**V8 → V2 → V3**" window is stale after the renumber.
- **`scripts/demo-review/personas/protocol.md:60-70`** — the question table is keyed `00 (primer)`
  through `10`. The series is now `01`…`13`.
- **`scripts/record-demo.sh:197-199`** — "The **ten** series specs live beside it now … an empty
  filter would run **all eleven** back to back". Both numbers are pre-renumber.
- **`scripts/cast-convert.py`** — the module docstring's account of the `script(1)` header/byte
  accounting contradicts what the code actually does with the first line.
- **`main`'s `11ac05ae`** (`secrets.tsx` masked Value field + reveal toggle) was flagged
  `fail-open-state-drift` by the background security hook. **P4 (G3) verdict: REAL, minor** —
  `AddSecretDialog` stays mounted; `reveal` state is not reset in the `if (open)` effect
  (`secrets.tsx:333,340-347`), so after one reveal + Cancel/Save the next Add/Rotate opens with the
  Value field in plaintext. No value leaks to another principal. One-line fix — `setReveal(false)` in
  the open-effect — plus a CHANGELOG line; **RESOLVED: merged into `prep/v0.6` at the reopen**
  (`c73d448b`), ships in this cut.
  Note prep already carries the test-side fallout of this commit:
  `cf6a4c3f` resolved the vitest selector collision and `0cc907bc` fixed five Playwright strict-mode
  violations in `ui/e2e/secrets.spec.ts` (`getByLabel("Value", { exact: true })`).

---

## 6. KNOWN RESIDUALS at 0.6.0

Documented, deliberate, and shipping as-is.

- **`PUT /permissions/enforcement` and site-config apply are full replaces** — now with an
  optional `If-Match`/ETag guard (0.6): a stale ETag is refused with 412, but an omitted header keeps
  last-write-wins and an omitted kind is still an enforced kind silently switched off. Send If-Match.
- **Redundant index `capability_grants_subject_idx`** (`internal/db/migrations/0042_capability_grants.sql:50`)
  — subsumed by the composite the per-request query actually uses. Harmless; not worth a migration.
- **SSH keys registered before 0.6 never gain the admin override.** `0043_ssh_key_role.sql` backfills
  `'member'` as the fail-closed value because nothing in the schema knows a pre-0.6 registrant's role.
  No boot backfill, no re-stamp sweep. Remedy: `DELETE` then `POST` the same key. (`docs/SSH.md`
  §Bounds, `docs/OPERATIONS.md`, CHANGELOG.)
- **A group DENY is not evaluated for a group missing from the session snapshot** (off the 2048-byte
  `maxSessionGroupsBytes` cut) **or from a pre-0.6 cookie** (`Groups` nil). None of that group's rows
  are evaluated — denies included — which with the kind unenforced resolves as *permitted*. Remedy:
  sign in again, or write the deny against the user directly (sub or email).
- **Every enforcement switch ships OFF** — fail-open by design, documented as such.
- **Host residue from earlier campaigns** — destructive to remove, left for the owner:
  `wardyn-dex` container, `wardyntest_*` volumes, stale `wardyn-v05-*`/`wardyn-b1b-test` kind
  containers, CI-tagged images `wardyn/*:kind-test`, **and this cut's `wardyn-helm-test` kind
  cluster** (`kind delete cluster --name wardyn-helm-test` — deletion is deny-listed for the
  agent session, so it is yours). `kubectl current-context` now points at `kind-wardyn-helm-test`.
- **G1 hygiene — FOUR files are tracked under `/local/`.** `.gitignore:64` is `/local/`, yet
  `local/gt-diagnosis.md`, `local/ponytail-audit-0.6.md`, `local/enterprise-poc-review/REGISTER.md`
  (the 35-finding discovery register, added during the enterprise-POC campaign) and
  `local/RELEASE-0.6-HANDOFF.md` (this file) are tracked on `prep/v0.6`; an ignore rule does not
  untrack what is already indexed. `main` tracks nothing under `local/`, so the merge carries all
  four onto `main` and into the release tag. Keep them as release provenance, or drop them before
  the merge — owner's discretion:
  `git rm --cached local/gt-diagnosis.md local/ponytail-audit-0.6.md local/enterprise-poc-review/REGISTER.md local/RELEASE-0.6-HANDOFF.md`.
- **gitleaks**: `.gitleaksignore:64` carries the sentinel fingerprint
  `81c0984168857a706ed2884b7f8cf22bd80df4fd:scripts/demo-beats/10-audit-and-attach.sh:curl-auth-header:556`.
  The value spells `wardyn-inert-sentinel` and is not a credential, but `gitleaks git` scans **every
  ref**, so the entry is needed regardless of which branch the commit sits on. The commit is
  reachable only from `main`, and the ignore file lives only on `prep/v0.6` — **`main` is red on
  gitleaks until prep merges.** Merging fixes it; do not "fix" it on `main` separately.

- **Second-human egress approval has an audited break-glass**: the shared admin token carries no
  per-human identity and bypasses the four-eyes rule (`approval.second_human.bypass`). Documented,
  deliberate.
- **`env_secret` grants are resident-by-design** — whole-run env exposure, no mint/TTL/revocation;
  own THREAT-MODEL row; admin-only unless `WARDYN_ALLOW_MEMBER_ENV_SECRET`.
- **The audit hash chain is tamper-EVIDENT, not tamper-proof** — a database owner can rewrite the
  whole chain; the head-hash in the sink stream is what an external SIEM alarms on.
- **Member-desktop residuals 25/26/27** (`threatmodel/THREAT-MODEL.md`): mount TOCTOU bounded not
  closed; reckless roots WARN not refuse (owner chose warn); `workspace_owner` is visible-not-gating.
- **"Revoke a human" is deployment-wide on `--all`** — every live `wdn_` token goes, the calling
  admin's own included; a mid-sweep store error records a `failure` audit row and is retry-safe.
- **Member console discoverability**: members reach the Workspaces screen by URL or the New-Run
  wizard's Add-a-workspace (the mock deliberately added no nav entry); no console reassign
  affordance exists (route + CLI only). Owner may order both in 0.6.x — mock round first.
- **UI suite is load-sensitive**: under a concurrent full CI + second suite the 5s-default tests
  time out broadly (seen once, all-timeout signature); quiet-box runs are 1038/1038. The one
  chronically tight test now carries a 30s cap.

---

## 7. FABLE / OPUS FALLBACKS

Original campaign: **0 fallbacks** across WF-1/2a/2b. Enterprise-POC expansion: every lane verify,
design verify and integration lens ran on Fable as pinned, with **one exception** — the Batch-1
closure re-verify lens died mid-run to a Fable safeguards false-positive (`[reasoning_extraction]`)
and was re-run as a fresh **Opus 5 fallback (flagged)**: verdict PASS, 0 must_fix, with byte-identity
and revert-probe evidence recorded in the ledger. The Batch-2 workflow carried an in-script Opus
fallback wrapper; it never fired.
