# Wardyn 0.6.0 — owner handoff

Everything the owner needs to cut 0.6.0. Written at `prep/v0.6` @ `0eb1ce0e`, 2026-08-23.
Nothing below was done on `main` or in the primary checkout.

---

## 1. STATE

| | |
|---|---|
| Release commit | `0eb1ce0e9ae08689e6d6764a09d8e0d398e91473` — `release: 0.6.0` (RELEASING.md steps 1 + 1b) |
| Branch | `prep/v0.6`, worktree `/home/cjohn/wt-v06-prep`, clean |
| Campaign base | `50848161d32296beb3ff6dca87f88d28406bc703` (ancestor of HEAD — verified) |
| `main` absorbed through | `8ba22fb2` (merge `cf6a4c3f`) |

**Merge rehearsal — run 2026-08-23 at `0eb1ce0e`:**

```
$ git -C /home/cjohn/wt-v06-prep merge-base --is-ancestor main HEAD ; echo $?
0
$ git -C /home/cjohn/wt-v06-prep merge-tree --write-tree main HEAD ; echo $?
3f7569bc92c51132c11d3a59833f9729f046435a
0
```

`git merge-base main HEAD` == `8ba22fb2` == `main` tip. **Zero conflicts; `prep/v0.6` → `main` is a
fast-forward.** Re-run both lines before merging — the demo session owns `main` and it moved four
times during the campaign. A non-zero `merge-tree` exit prints `CONFLICT` lines; expect them only in
demo files (`docs/DEMO-SCRIPT.md`, `scripts/demo-beats/*`). `scripts/verify-demo-take.sh` conflicts
are now **main-only text** — the V13 grader was split out to `scripts/lib/verify-demo-take-13.sh`
precisely so the two sides stop colliding.

### Gate evidence

| Gate | Result | Log (in-log `EXIT=`) |
|---|---|---|
| `make ci` (daemon-free merge gate) | **EXIT=0** at final tip | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/c3ceae31-29f9-4044-a0ef-7a79234f50c2/scratchpad/ci-final.log` |
| `make release-check` **after** the release commit | **EXIT=0** (`release-check PASSED`); `cmd/wardyn` ran fresh (2.462s, not cached) so `TestVersionMatchesChangelog` / `TestShippedVersionStringsAgree` are covered | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/wf3-gates/release-check.log` |
| PG suite (`store`/`db`/`secretstore`/`broker`/`api`/`apie2e`/`recording`/`wardynd`) | **EXIT=0**, 1970 pass / 0 fail / 1 skip, coverage 76.2% — run at `45cb3367`, not re-run at the release tip | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/wf1-gates/test-report-pg.log` |
| Playwright ui-e2e (all specs) | **EXIT=0**, **14/14** spec files, 0 failed — at final tip, after the `secrets.spec.ts` strict-mode repair | `…/c3ceae31-…/scratchpad/ui-e2e-full2.log` |
| `make test-conformance-docker` | **EXIT=0**, **32 pass / 0 fail / 7 skip** incl. `ExecStreamLoopbackRelay` — run at `45cb3367` | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/wf1-gates/conformance-docker.log` |
| `make test-e2e-ssh` | **EXIT=0**, **15/15**, hermetic (project `wardynv05e2e`, own image tags) incl. member-key denial (`rc=255`, `ssh.auth` failure `reason="not the run owner"`) **and** admin override (`ssh.auth` success, `data.override=true`) | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/wf2b-gates/gate3-e2e-ssh.log` |
| `make test-e2e-ui-sandbox` | **EXIT=0**, **20/20**, hermetic (project `wardynv06uisbx`), agent image rebuilt from this tree | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/wf2b-gates/gate4-e2e-ui-sandbox.log` |
| `make helm-install-test` | **EXIT=0**, PASS on the port-free kind cluster `wardyn-wf1-conf` (torn down) — run at `3a9b0f5a` | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/wf2-gates/helm-install-test.log` |
| `helm template` matrix (4 rows) | **EXIT=0** at final tip: defaults refuse (`secret.yaml:42`); a 0.5-shaped values map (`uiSandbox=null`, `readinessProbe=null`) renders `path: "/readyz"` with and without `k8s.enabled`; `uiSandbox.port==service.port` refuses; `ssh.port==service.port` refuses. The same four assertions are now permanent lines in `make helm-lint`. | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/wf2b-gates/gate5-helm-matrix.log` |
| `make test-conformance-k8s` | **ENV-BLOCKED**, EXIT=2 on this WSL2 box — kind subnet `172.21.0.0/16` collides with host `eth0` `172.21.96/20`. **CI is the proof.** | `/tmp/claude-1000/-home-cjohn-containerized-agent-envs/wf2-gates/test-conformance-k8s.log` |

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
git merge --no-ff main                                 # expect demo-file conflicts only
make ci                                                # must be EXIT=0 again
```

### (c) Merge into `main` — in the PRIMARY checkout

```sh
cd /home/cjohn/containerized-agent-envs
git checkout main
git merge --ff-only prep/v0.6                          # a fast-forward as of 0eb1ce0e
```

If it is no longer a fast-forward:

```sh
git merge --no-ff prep/v0.6 -m "merge: 0.6.0 release prep

Permissioning (capability grants + enforcement switches), UI sandboxes,
SSH gateway admin override, k8s runner hardening, V13 demo beat.
Gates: make ci, release-check, ui-e2e 14/14, conformance-docker 32/0,
test-e2e-ssh 15/15, test-e2e-ui-sandbox 20/20, helm-install-test."
```

Then, in the primary checkout:

```sh
make ci                                                # EXIT=0 before anything is tagged
```

### (d) Cut the release branch and tag

```sh
git checkout -b release/0.6 0eb1ce0e                   # the release commit, now on main
git tag v0.6.0
```

### (e) Push

```sh
git push origin main release/0.6 v0.6.0
```

`enforce_admins` is off on `main`'s branch protection, so this direct push is the sanctioned path
(RELEASING.md §Repo settings).

### (f) GitHub prerelease

The heading is `## [0.6.0] — 2026-08-23`; the awk below was run against the real file and extracts
424 lines, ending at the next `## [` heading.

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
- `screenshots-fresh`
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
  `fail-open-state-drift` by the background security hook. **P4 verdict: _(pending — fill in from
  WF-3's G3 gate before the cut)_.** Note prep already carries the test-side fallout of this commit:
  `cf6a4c3f` resolved the vitest selector collision and `0cc907bc` fixed five Playwright strict-mode
  violations in `ui/e2e/secrets.spec.ts` (`getByLabel("Value", { exact: true })`).

---

## 6. KNOWN RESIDUALS at 0.6.0

Documented, deliberate, and shipping as-is.

- **`PUT /permissions/enforcement` is a full-map replace** with no `If-Match`/version guard. An
  omitted kind is an enforced kind silently switched off; two admins writing concurrently, last write
  wins. Audited, not prevented — re-fetch `GET /permissions` immediately before writing.
  (`docs/OPERATIONS.md` §Capabilities, `threatmodel/THREAT-MODEL.md` §5 residual #20.)
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
  `wardyn-dex` container, `wardyntest_*` volumes, stale `wardyn-v05-*` kind containers, `kubectl`
  `current-context` left unset by the kind lane, CI-tagged images `wardyn/*:kind-test`.
- **gitleaks**: `.gitleaksignore:64` carries the sentinel fingerprint
  `81c0984168857a706ed2884b7f8cf22bd80df4fd:scripts/demo-beats/10-audit-and-attach.sh:curl-auth-header:556`.
  The value spells `wardyn-inert-sentinel` and is not a credential, but `gitleaks git` scans **every
  ref**, so the entry is needed regardless of which branch the commit sits on. The commit is
  reachable only from `main`, and the ignore file lives only on `prep/v0.6` — **`main` is red on
  gitleaks until prep merges.** Merging fixes it; do not "fix" it on `main` separately.

---

## 7. FABLE / OPUS FALLBACKS

**None.** WF-1 (4 agents), WF-2a (13 agents) and WF-2b (13 agents) all completed with **0 fallbacks** —
every review lens ran on Fable as pinned.
