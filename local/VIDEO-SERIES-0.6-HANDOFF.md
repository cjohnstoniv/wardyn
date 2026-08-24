# Handoff: reconcile the demo video series with 0.6 for GA — a PLANNING brief

Written 2026-08-24 by the 0.6 enterprise-POC agent for a fresh agent started in **plan mode**.
Nothing below has been done. The task is to PLAN (then, once the owner approves, execute):

1. Which **existing** episodes change to account for 0.6.
2. Which **new** episodes / **structural** changes the series needs for 0.6 — and whether the series
   should split into **paths** (desktop vs cloud; a shared base plus a deployment path; and/or
   operator/admin vs user sub-paths).

This merges 0.6 with the videos and finalizes the series for **full GA with 0.6**. The owner's own
framing: "it might be acceptable to have 2 different video paths… one for desktop vs. cloud… and/or
a set of base common videos that apply to both with a desktop vs. cloud deployment path. Maybe even
for each/either of these having sub-paths for operators/admins vs. users."

---

## 0. Two branches, two owners — read this first

| | `main` (demo session's, primary checkout `/home/cjohn/containerized-agent-envs`) | `prep/v0.6` (worktree `/home/cjohn/wt-v06-prep`) |
|---|---|---|
| Owns | the VIDEO SERIES: specs, beats, DEMO-SCRIPT.md, record/verify harness, the persona review protocol, the in-product demos catalog, today's takes | the 0.6 PRODUCT: everything the videos must now teach |
| Tip at handoff | `7867cb83` (V03 mega-episode); takes for 01–10 re-shot **today 2026-08-24** | `a83954ac` — release commit `9afe60ff` + corrections; 3-lens Fable GO; owner has NOT merged/tagged |
| Merge state | `prep → main` is **NOT a fast-forward**: 12 conflicts, because `main` absorbed `/demos` into Getting Started (`10a2e144`) and **deleted `network-dialog.tsx`**, which carries 0.6's D33/D8 canon strings | reconciliation guidance: `local/RELEASE-0.6-HANDOFF.md` §1/§2b (canon strings re-land from `docs/design/ui-batch2-mock.md`) |

**Sequencing matters:** the video work films the 0.6 console, so it must run on the **merged** tree
(`main` after the owner lands `prep/v0.6`). Until that merge exists, plan against both: the series
harness from `main`, the product truth from `prep/v0.6`. Do not shoot anything before the merge — the
console the takes film would be wrong on either branch alone.

Standing laws (memory, still binding): all work in worktrees; the demo session owns `main` +
primary checkout while it records; HEADLESS recording only; shared-host live-suite rules
(`docker ps` BOTH daemons; never `make test-e2e` / `kind-quickstart` / `test-e2e-ssh-k8s` while the
demo stack is up); commits `cjohnstoniv`, `-s`, no Claude trailers; Fable verifies every lane
(Opus fallback flagged); the persona review loop (`scripts/demo-review/personas/protocol.md`) is the
series' quality gate — the owner's external-review gate sits BEFORE any take.

---

## 1. The series as it stands (on `main`, 2026-08-24)

The demo session restructured the series this week (its plan: `~/.claude/plans/merry-snacking-harbor.md`,
Workstream C). Twelve numbered episodes + one terminal-only cluster episode staged on prep:

| # | Episode (spec `ui/e2e/demo/NN-*.spec.ts`) | What it teaches | Lane |
|---|---|---|---|
| 01 | why-govern-agents | the primer deck (`assets/primer.html`); never loads the console | browser (deck) |
| 02 | set-up-the-host | the install on camera (`make setup`, reset owns the clean slate) | terminal+browser |
| 03 | what-it-stops | **mega-episode**: the whole demos surface — 4 boundary demos + agent-in-the-box + record-a-policy + once-or-for-good + the 5 per-kind secrets mechanisms (key-never-in-the-box, write-only, authorized-not-issued, rest-api-token, pat-stdout-only, sealed-box) | browser |
| 04 | add-a-workspace | the blast radius; workspace onboarding (operator) | browser |
| 05 | your-first-policy | the panel, templates, **the safety meter**, save (absorbed the retired barrier-tier episode) | browser |
| 06 | your-first-run | launches against the policy 05 saved | browser |
| 07 | interactive-runs | an agent with a hand on the wheel; the managed-subscription decoy beat | browser |
| 08 | autonomous-agent | name it / aim it / fence it; the one prompt; interrupts | browser |
| 09 | record-a-run | the card that learns; record → promote; replay confined | browser |
| 10 | approvals-and-egress | held at the door; the four-scope ladder (once/run/until/always) | browser |
| 11 | ci-and-headless | the pipeline's run on the board (`scripts/demo-beats/11-*.sh`) | terminal+browser |
| 12 | audit-and-attach | the trail; SSH attach; who did what (`scripts/demo-beats/12-*.sh`) | terminal+browser |
| 13 | terminal-to-the-cluster | **prep only** (`scripts/demo-beats/13-terminal-to-the-cluster.sh`, grader `scripts/lib/verify-demo-take-13.sh`): k8s runner from a laptop terminal — never shot | terminal |

Also on `main`: `walkthrough.spec.ts` (the no-flag end-to-end walkthrough, acts 1–6),
`retiring-policies-and-confinement.spec.ts` (retired INTO 05 — a demolition candidate), the
in-product **demos catalog** (`ui/src/app/components/screens/demos/demo-catalog.ts`, 15 ids — episode
03 films it), the recorder (`scripts/record-demo.sh`, `--video NN`, `--terminal-script`), the grader
(`scripts/verify-demo-take.sh`, per-video `check_video_NN` dispatch — note the function NAMES still
carry pre-renumber numbers: `09) check_video_06_record`, `06) check_video_03_first_run` — deliberate,
documented at line ~891), narration (captions are spoken; mux collisions under 1.5s), and
`scripts/demo-review/personas/protocol.md` (4 personas: dana-eng-leader, morgan-citizen-dev,
priya-junior-dev, sam-senior-dev; per-video quiz table — **still keyed `00`–`10`, the OLD numbering**,
a known main-owned drift; series-level review after each round).

Takes on disk: `/mnt/c/Users/Chaz/Videos/wardyn-NN-*-20260824T*.mp4` (+ `-narrated`, `-ffwd`) for
01–10 as of this writing; 11/12 were quota-blocked earlier in the week (memory: 06/07-class episodes
need model answers — unretryable on weekly quota).

`docs/DEMO-SCRIPT.md` (main: harness doc + beat sheet; prep: adds §"Re-take at the release cut" at
line ~235 with the 0.6 retake table and the old→new numbering map). Prep's retake table predates
Phase 2b — it only knows about Permissions/V13; treat it as a floor, not the answer.

---

## 2. What 0.6 shipped that the series does not yet teach

Read `CHANGELOG.md` on prep (`## [0.6.0]`, ~640 lines) for the full truth. The features with a
**demonstrable console/CLI surface**, grouped by who they are for:

### Operator / admin surfaces (new or changed)
- **Permissions** sidebar entry + capability grants/enforcement (0.6 pillar; every browser take on file
  predates it — the sidebar changed, so every episode's staging drifted).
- **Per-user API tokens** (`POST /me/tokens`, admin list/revoke): a member's CLI credential.
- **Session revocation** (`wardyn sessions revoke --sub|--all`, sweeps the target's tokens too).
- **Hash-chained audit** (`GET /audit/chain/verify`; head-hash rides the SIEM stream) + `?actor=`
  filter + NDJSON export + `WARDYN_AUDIT_SOURCE` + Splunk/webhook recipes + `auth.failed` events.
- **Age-key rotation** (`wardynd -rotate-age-key`, maintenance mode).
- **`env_secret` grant kind**, **`git_pat` per-run lease**, **second-human egress approval**
  (`WARDYN_EGRESS_SECOND_HUMAN`; audited admin break-glass).
- **If-Match** on enforcement + site-config apply. **SSH admin override is bounded-stale**
  (`WARDYN_SSH_ROLE_TTL`, re-stamped at OIDC login).
- **Configurable hold window** (`first_use_hold_seconds`/`max_holds`), **CONNECT refusal headers**
  (`X-Wardyn-Egress: denied|approval-pending`).
- **Support bundle** (`wardyn support-bundle`), OIDC transient retry, failure_hint on failed runs.
- **Workspace reassign** (`POST /workspaces/{id}/reassign`, admin) — offboarding.
- **Trailing-dot egress-deny bypass FIXED** (the D1 blocker) — a "what it stops" beat candidate.

### Deployment paths (the owner's "desktop vs cloud" axis is REAL in 0.6)
- **Desktop (topology a′):** `docs/DESKTOP.md` (Topology / The ceiling / Tamper posture / member-mode
  profile / MDM file table / install lane / "Try it, once, on a real Mac"), `deploy/desktop/`
  (`install.sh`, `com.wardyn.daemon.plist`, `wardyn-desktop.sh`, `wardyn.env.example`), arm64 images,
  Colima socket support, `scripts/test-desktop-profile.sh`. The developer IS the operator; the
  ceiling is stated verbatim ("the developer is not the adversary in this tier").
- **Member-mode desktop (topology m′):** `WARDYN_MEMBER_MODE`, `WARDYN_MEMBER_WORKSPACE_ROOTS[_MAP]`,
  `WARDYN_MEMBER_WRITABLE_ROOTS/_DENY` — the developer is a MEMBER whose envelope binds them.
- **Cloud / cluster:** the k8s runner (0.5) + helm chart at 0.6.0, `helm-install-test`, V13's
  terminal-to-the-cluster beat, `docs/wardyn-k8s-setup` skill knowledge, Entra App Roles RBAC.
- **CI/headless:** unchanged surface (V11), now with arm64 + cosign/SBOM/trivy on the agent images.

### Member / user surfaces (new — the "user sub-path" axis is REAL too)
- **Members onboard their own workspaces** from the console (Workspaces screen + New-Run wizard's
  Add-a-workspace): `owned_by`, ownership-404 parity, mount boundary shown in place
  (`member_local_dir_root` on `GET /me`), no writable checkbox for members. The mock is law:
  `docs/design/ui-batch2-mock.md` (canon strings).
- **Member CLI via API token**; member SSH keys; member approvals on their own runs; the
  no-impersonation audit marker (`workspace_owner`) when an admin touches their workspace.
- Discoverability residual: members reach Workspaces by URL or the wizard — **no member nav entry**
  (the mock chose not to; owner may order one — mock round first). No console reassign affordance.

### Copy/render changes that hit existing takes directly
- D33: the Unlisted-hosts copy now says refused-and-raised / approve once / retry gets through
  (episode 10's "held at the door" language and episode 05's policy panel film this text).
- D8: hold-window copy is per-held-connection. D7: "Agent telemetry" tag names the CLI's own
  diagnostics endpoint (the FIRST pending approval a pilot used to see — episode 10/08 beats may have
  filmed that approval; it is now suppressed by default).
- D9: FAILED runs show `failure_hint` beside the badge.
- Episode 03 films the demos catalog; the catalog itself is main-owned and current — but 0.6's D1
  fix and the CONNECT headers are new "what it stops" evidence not in it.

---

## 3. Structural candidates for the plan (the owner's question — decide, don't assume)

The owner floated three shapes; the plan should evaluate them against the persona protocol and the
GA audience, and RECOMMEND one:

**Shape A — one linear series, 0.6-patched.** Restage/re-caption the 12 (+13), add 1–3 episodes
(desktop install; members; admin ops). Cheapest; keeps one course; risks a 15+ episode wall.

**Shape B — base + deployment paths.** A common core (01 primer → 03 what-it-stops → 05 policy →
06 first run → 08/09/10 the run lifecycle) then a branch: **Desktop path** (02-desktop: `install.sh` +
launchd + the managed envelope; 04-desktop: member onboarding under the mount boundary; 12-desktop:
support-bundle + revoke) vs **Cloud path** (02-cloud: helm install; 13 terminal-to-the-cluster;
11 CI; 12 audit at fleet scale + SIEM). Matches the product's real topologies (a′/m′/k8s).

**Shape C — B plus role sub-paths.** Within each deployment path, **operator/admin** episodes
(permissions, tokens admin, revoke, rotate, hash-chain verify, reassign, second-human) vs **user**
episodes (member workspace, own tokens, own approvals, interactive/autonomous runs). This is the
shape the 0.6 RBAC model actually has (operator vs member is a hard product boundary), and the
persona set already spans it (dana = leader, sam/priya = devs, morgan = citizen dev).

Considerations the plan must weigh: (1) the **persona quiz table** is the acceptance oracle — any new
shape needs new quiz rows and a re-keyed table (it is already stale at `00`–`10`); (2) the recorder
dispatches on `--video NN` with per-video graders — paths need a naming scheme the harness can
dispatch on (`--video desktop-02`? path prefixes?), and `verify-demo-take.sh`'s dispatch + the
`WARDYN_DEMO_WORK_DIR` handoff conventions must follow; (3) the **in-product demos catalog** is
where "what it stops" lives — new 0.6 stops (D1 trailing-dot, CONNECT headers, second-human) belong
there first (product), then in episode 03 (film); (4) narration budget: captions are spoken and
collide under 1.5s — long admin episodes need beats, not walls; (5) **quota**: model-answer
episodes (06/07/08-class) are unretryable on the weekly quota — schedule takes accordingly;
(6) the owner's macOS smoke run (DESKTOP.md "Try it, once") is **owed and unshot** — a desktop
episode cannot be filmed honestly before that flow has run once on real hardware; (7) V13 has
never been shot (needs a live cluster; grader has no dry-run mode).

---

## 4. Concrete deliverables the plan should produce

1. **Episode-by-episode delta table** for 01–13: what 0.6 changed on screen / in copy / in claims →
   restage only | re-caption | re-shoot | retire | split. Source the deltas from `CHANGELOG.md`
   `[0.6.0]`, `docs/design/ui-batch2-mock.md` (canon strings), and a diff of each spec's captions
   against the merged console (grep the spec's `caption(` strings against the app's copy modules —
   the mock-first law means canon strings == app strings, so a caption that no longer matches the
   app is a defect the grader would catch on the next take).
2. **New-episode list** with one-line teaching goal, lane (browser/terminal), persona quiz row,
   prerequisites (owner smoke run; live cluster; merge landed), and grader arm.
3. **The structure decision** (A/B/C or a hybrid) with the harness changes it implies (recorder
   dispatch, grader dispatch, DEMO-SCRIPT sections, persona table re-key, catalog ids).
4. **Harness/doc drift to fix regardless** (main-owned, listed in `local/RELEASE-0.6-HANDOFF.md` §5):
   persona quiz numbering; `record-demo.sh` "ten/eleven" counts; DEMO-SCRIPT reset-owner line and
   the stale V8→V2→V3 window; `cast-convert.py` docstring; the `check_video_NN` name/number mismatch
   (decide: keep-and-document or rename).
5. **A sequenced execution plan** with the standing fan-out shape (decompose Fable → Sonnet/Opus
   lanes per episode → Fable verify each spec/take; rehearse-first; the owner's external-review gate
   before any take; per-take ffmpeg socket probe + retry-on-flap; poll innerText not toContainText
   for xterm), and the two hard gates: the prep→main merge, and the owner's macOS smoke run.

---

## 5. Pointers

- 0.6 truth: `/home/cjohn/wt-v06-prep` — `CHANGELOG.md`, `docs/DESKTOP.md`, `docs/OPERATIONS.md`,
  `docs/AUDIT-ACTIONS.md`, `docs/design/member-role-desktop.md`, `docs/design/ui-batch2-mock.md`,
  `local/RELEASE-0.6-HANDOFF.md`, `local/enterprise-poc-review/REGISTER.md`.
- Series truth: primary checkout on `main` — `docs/DEMO-SCRIPT.md`, `ui/e2e/demo/*.spec.ts`,
  `scripts/record-demo.sh`, `scripts/verify-demo-take.sh`, `scripts/demo-beats/`,
  `scripts/demo-review/personas/protocol.md`, `scripts/demo-review/build-corpus.py`,
  `ui/src/app/components/screens/demos/demo-catalog.ts`.
- Campaign ledgers: `~/.claude/plans/fluffy-orbiting-tiger.md` (0.6 enterprise POC, complete);
  `~/.claude/plans/merry-snacking-harbor.md` (the demo session's restructure — read Workstream C for
  why the series is shaped as it is; do not undo it without the same rationale).
- Memories: `demo-recording-harness`, `demo-series-reorder-2026-08-21`, `persona-review-loop-method`,
  `wardyn-shared-host-live-suites`, `wardyn-06-campaign`, `mock-first-ui-rule`.

Nothing in this file has been executed. Plan first; the owner approves the plan before takes.
