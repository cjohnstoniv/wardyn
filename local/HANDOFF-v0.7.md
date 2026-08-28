# v0.7 handoff

**Branch `feat/v0.7-phase0`, 29 commits, rebased onto `origin/main` @ `4f6fc979`
(release: 0.6.6). Committed, NOT pushed, NOT tagged.** All gates green:
`make lint` clean, `go test ./...` 0 failures, `make test-scripts` 10 passing.

**Read this first, then hand to `~/.claude/plans/temporal-launching-crystal.md`**
(the Orca × Wardyn UI/UX campaign). **That plan hands on to
`~/.claude/plans/ultra-velvety-bee.md`** (the cleanup pass) when it is done — the
cleanup runs LAST, over everything 0.7 and the UI campaign land.

---

## 1. Corrections the next agent needs — the UI plan states these as facts and they moved

`temporal-launching-crystal.md`'s "Sequencing + ground-truth caveat" was written
against the fork. Four of its statements are now wrong or stale:

| It says | Actually |
|---|---|
| "the working tree here is a fork at 0.5.0, 567 commits behind trunk" | **No longer true.** `feat/v0.7-phase0` is rebased onto 0.6.6 and is 29 commits ahead of it. The fork at `/home/cjohn/containerized-agent-envs` is abandoned; work in `/home/cjohn/wt-v07-phase0`. |
| "UI-app images are local-build only in 0.7" | Still true, and now a **Named gap in ROADMAP.md** rather than an unstated posture. 0.7 also added a **second** UI image, `agent-novnc` (`make agent-image-novnc`), same local-build posture. |
| "0.7 is the last minor before the v0.8 alpha RC wire freeze" | **Unverified and probably not a real constraint.** `grep -rniE 'wire.?freeze'` over the repo returns nothing, and `CHANGELOG.md` says the opposite: *"Wardyn is pre-alpha and does not yet follow semantic versioning (interfaces are not stable)."* Treat it as external to the repo; confirm with the owner before letting it shape scope. |
| "F.4 re-shoots all ~14 demo episodes on the pre-polish console" | **Do not do this before the UI campaign.** The owner chose re-shoot-everything, but re-shooting before a console redesign means shooting twice. F.4 is deliberately NOT done in 0.7 — see §4. |

Two more facts that plan will want:

- `agent-claude-code` is retired and **D5 re-pointed `harness.go`'s `ImageKey` to
  `base`** — as it predicted. But note trunk's 0.6.6 independently made the setup
  probe pass a literal `"base"`, and the rebase merged both. Both now resolve to
  `agent-base`; `setup.go`'s probe row only draws the contrast when an operator
  has re-pinned `claude-code`.
- **`tool_call` approvals DO have a backend producer — do not repeat the claim
  that they do not.** `wardyn-toolgate` POSTs through the proxy's brokered
  approvals route (`handleBrokerCreateApproval`, `internal/egress/proxy/local_routes.go`),
  which forwards to the *internal* approvals endpoint (`internal/api/internal.go`).
  Reading only the public `internal/api/approvals.go` gives the wrong answer —
  it mentions `tool_call` solely in clamp/refusal comments. A stale comment in
  `approvals.tsx` said "no current backend producer"; corrected here.
  What IS true, and the rendered card copy states it correctly, is that nothing
  *enforces* the decision out of band: the toolgate is in-sandbox and
  cooperative. Keep that wording — it matches `AGENT-THREAT-MODEL.md`'s
  `partial` verdict and its named bypass class.
- **`tool_rules` shipped with ZERO UI.** Per-tool `allow`/`hold`/`deny` is
  proxy-side and settable only in the policy YAML/JSON — no console surface
  reads or writes it, and a rule-decided call never creates an approval card at
  all (it returns before the forward). **This is the single most obvious 0.7
  feature for the UI campaign to surface**, and it is the first rung of Phase G.

- `maxSSHSessionsPerRun = 4` is unchanged, and **B2 was not taken**: no number
  was measured, so none was invented. `TestSSHGateway_MixedChannelTypesShareOneCap`
  now pins the structural finding (the cap is shared across `session` and
  `direct-tcpip`, inverting OpenSSH) and a refusal is audited as
  `ssh.channel_rejected`.

---

## 2. The field-reported k8s bug — diagnosed, and it is not what was reported

**The reported mechanism is refuted, and the real cause already shipped in
0.6.6.** The report was: *on k8s an exec-mode run never reaches a terminal state,
because the driver execs into an idle main container and nothing transitions the
run.* Traced end to end; wrong on both halves.

- **The idle-main-container shape is not k8s-specific.** `internal/runner/k8s/sandbox.go`
  and `internal/runner/docker/driver.go` run a byte-identical `sh -c AgentIdleScript`.
- **k8s does track an agent exec id, and better than docker does.** It adds an
  ephemeral container named `wardyn-agent` and returns that name, re-read from the
  apiserver on every call — so it survives a wardynd restart, which docker's
  in-memory `agentExecs` map does not.
- **Something does mark the run terminal.** `runs_dispatch.go` persists the exec id
  then calls `startCompletionWatcher`; `runs_lifecycle.go` waits, CASes
  RUNNING→COMPLETED/FAILED and emits `run.complete`. All substrate-agnostic, and
  present in 0.6.5 too.
- **The timing evidence cannot discriminate.** `runSiteConfigProbe` calls
  `dispatchRun` synchronously and starts its wait budget immediately after, so the
  clock starts within milliseconds of the `run.exec` stamp. *Any* cause of
  non-completion fails at exactly `run.exec + <budget>`. "Exactly +50s three times"
  is evidence of a compiled constant, not of a completion mechanism.

**What actually happened on 0.6.5.** `cmd/wardyn-rec` uploads the cast *before*
`os.Exit`, and 0.6.5 did that on a bare 60s client over the stdlib default
(unbounded-connect) transport. A control plane the proxy pod cannot reach — a TCP
connection accepted and held — kept a *finished* task's exec alive up to 60s.
Budget 50s < tail 60s → KILLED at exactly +50s, regardless of the curl target,
which is why an internal endpoint answering in milliseconds timed out identically.
Docker never hit it because on compose the proxy reaches the control plane over a
local network in milliseconds.

Fixed in 0.6.6, both ends: `wardyn-rec` gained a 5s dial / 20s client timeout on a
cloned transport, and the probe budget went 50s → **90s** with the worst-case
arithmetic written into the comment. 0.6.6 also shipped `ErrExecNeverStarted` for a
hard `Waiting` reason, the `timed_out` verdict, and the named `Waiting` reason —
which is the console half of this.

**Severity: probe-only. Not a release blocker.** `wardyn run --task-mode exec --wait`
uses the CLI's own budget — default **30 minutes**, settable via `--timeout` /
`WARDYN_CI_TIMEOUT`. A ≤60s completion tail is invisible to it; the run genuinely
completes and `run.complete` genuinely fires. The probe's compiled 50s was the sole
victim. **The CI lane in `docs/CI.md` was never affected.**

**Both field-proposed fixes were rejected, on the record.** (a) *"complete an
exec-mode run when its exec exits"* is a zero-line diff — it is already the
implementation. (b) *"have the probe wait on the exec rather than the run"* loses
the COMPLETED/FAILED/KILLED distinction `probeFailureDetail` reads, skips
`finalizeRunTail`'s revoke cascade and `StopSandbox`, puts a substrate-shaped
completion path into `internal/api`, and would not have fixed the reported failure
anyway — the exec was still running (uploading) at the 50s mark.

### What the trace DID find: a separate live k8s defect, now fixed

`internal/runner/k8s/exec.go`'s `AgentStatus` fell through to a **definitive
terminal state with a nil error** whenever the pod Get succeeded and the exec was
present in `Spec.EphemeralContainers` but the kubelet had not yet published a
`Status.EphemeralContainerStatuses` entry — i.e. a healthy exec that just started.
Both consumers finalize on exactly that pair (`sweepRunWatchers` and
`reconcileWatch` in `reconcile.go`), and with a nil ExitCode both pick `RunFailed`
and call `StopSandbox`. **A wardynd restart or a watcher-lease handoff landing in
that window killed a healthy k8s run and reported it FAILED.**

The docker driver refuses that call for the identical ambiguity (an exec-404 while
the container still runs returns an *error*, GAP-RECONCILE-1) precisely so the
reconciler retries. k8s had strictly better evidence and used it worse.

Fixed here: present-in-Spec-but-not-in-Status now returns `RunStarting`; absent
from Spec keeps the terminal answer. Pinned by
`TestAgentStatus_ExecAddedButNotYetStarted_MustNotReadTerminal`, proven to fail on
the pre-fix commit and pass after.

**Residual, unproven:** `Wait`'s poll loop has no bound if a status entry never
appears. Unreachable on any cluster whose kubelet works — a structural note, not a
demonstrated defect.

### The UX half stands, and belongs to the UI campaign

0.6.6's `timed_out` verdict and named `Waiting` reason address part of it. What
remains: the verdict still reads under the *proxy* heading with advice to fix the
proxy, and the wait budget is a compiled constant with no operator-side
mitigation. The reporter found this only by reading the audit trail through the
API — after eight hours. **A governance product whose failure states are
unreadable from its own console is the UX gap, in one concrete instance.**

---

## 3. What 0.7 shipped

Thirty commits. The full detail is in `CHANGELOG.md`'s `[Unreleased]`; this
is the shape.

**Two defects that made shipped features not work at all:**
- **`desktop-envelope` had failed on EVERY run since the job was added** — never
  once green. The BYOI wrap COPYed `wardyn-git-helper` onto PATH and wired
  nothing to it, so any policy declaring a `github_token` grant failed its own
  selftest. That is the desktop tier's own managed ceiling, so it broke the whole
  BYOI lane.
- **Both documented install paths 404'd.** `releases/latest` excludes
  pre-releases and every Wardyn release is one, so `curl … | sh` died and the
  README's Helm blocks silently installed **unpinned**.

**The desktop tier, made real:** Linux/systemd unit + timer, an uninstaller
(`--purge` gated), `.deb`/`.rpm`/tarball built from a **clean git tree** (never
the working directory — `deploy/compose/.env` holds a live age key), the m′
member envelope, the SSH gateway actually reachable, digest pins that work (the
launcher's own `export` used to beat `--env-file`), and the proxy sidecar
actually pulled (nothing ever pulled it, on either install path).

**Policy and honesty:** `tool_rules` (per-tool allow/hold/deny, proxy-side),
never-resident git PATs for non-GitHub forges, `--agent` optional for exec runs
with an image, scan-seeded egress requiring operator provenance by default, the
`tool_approvals`-on-interactive contradiction refused rather than discarded, and
the dead `steps` surface deleted (the field kept — it is a UNIQUE index
component).

**Docs that were false:** the export-control obligation that was never owed, the
GPL offer covering an unpublished image while missing a published one,
`RELEASING.md`'s tag gate naming a deleted job and omitting the copyleft one,
`PLUGGABILITY.md`'s selection convention, and nine rotted threat-model citations
— each now with a guard.

**New:** `threatmodel/AGENT-THREAT-MODEL.md`, a portable agent threat model with
fourteen categories, each carrying a Wardyn coverage verdict citing code.

---

## 4. Owner-gated — nothing here is blocked on engineering

| | What | Blocker |
|---|---|---|
| **A9** | The owed macOS smoke run | A real Mac. It runs `sudo ./install.sh`, which mints `/etc/wardyn/age.key` and loads a **root LaunchDaemon on a corporate-managed laptop** — confirm that is acceptable, and prove `--purge` uninstall first. |
| **A2b** | Signed/notarized `.pkg` | **No Apple Developer ID.** The unsigned `.pkg` can be built and installed behind a Gatekeeper step; signing is a Named gap until a cert exists. |
| **A2c** | One worked MDM example | A Jamf tenant, which arrives with the same Mac. |
| **A5 / B4** | Two UI slices — the member Workspaces nav entry, and the UI-sandbox off-state doc link | **Mock round first** (owner law). Deliberately not shipped unmocked. **These belong to the UI campaign, not to a 0.7 patch.** |
| **A1** | Live systemd install + one timer fire | No passwordless `sudo` on this box, and enabling the timer would bind 8080/2222/8081 against another session's stack. |
| **B1a/B1b** | VS Code Remote-SSH channel measurement | A human at a GUI client. **B2 stays dropped until measured** — do not ship a number nobody measured. |
| **F.4** | Re-shoot ~14 demo episodes | Owner chose re-shoot-everything. **Do it AFTER the UI campaign**, or every episode is shot twice. G7 (weekly model quota) is the real constraint: the 06/07/08-class episodes are unretryable. |
| **F.5** | EAR notification | **Resolved — nothing owed.** §742.15(b)(1) puts publicly available 5D002 source outside the EAR, and BIS's 2021 rule narrowed the notification to non-standard cryptography, which Wardyn does not implement. |
| **02b** | The managed-desktop demo episode | No longer Mac-gated (owner: no per-OS videos). Filmable on Linux; deferred to the video campaign with a ROADMAP line. |

---

## 5. Release state — deliberately NOT done

0.7 is **prepped, not released**, per the owner. Done: CHANGELOG `[Unreleased]`
carries every item, ROADMAP is reconciled, the GPL offer is regenerated, the tag
gate's job list is corrected.

**NOT done, and correctly so:** no version bump (still 0.6.6 in all four version
strings), no tag, no push, no `release.yml` dry run, no `desktop-envelope`
promotion to a required context.

Two things the release will need that the plan documents but 0.7 could not do:
- **F.1a** — 0.6.2 and 0.6.3 shipped with **no GitHub Release at all** while the
  workflow was green (`release.yml`'s fallback created an untagged draft and the
  assertion resolved to it). The fix is in the plan; verify before tagging.
- **F.7** — `docs/VERIFY.md`'s blocks fail at the version they pin. Re-run
  against whatever tag is actually cut.

---

## 6. Working rules that cost time to learn

- **Run the FULL `go test ./...`**, never `./internal/...`. The audit-citation and
  env-doc guards live in `cmd/wardynd` and caught rotted citations three separate
  times, including after the 0.6.6 rebase.
- **Negative-control every new assertion.** Four of mine were vacuous on the
  first pass — a `grep` matching a comment instead of code, a substring surviving
  the deletion it guarded, a regex whose escaping never matched.
- **The file-size gate bites at 1000 lines** and is in `make lint`, a required
  context. Split by seam; do not allowlist.
- **Two daemons.** `runsc`/`kata`/`krun` are on `/var/run/wardyn-docker.sock`; the
  GPU and Docker Desktop are on the default socket. `docker info --format
  '{{.Runtimes}}'` against both before trusting either.
- **`docker ps` on BOTH daemons before starting anything.** An idle stack holds
  8080/2222/8081, and container names collide across projects.
- **`make notices` DELETES tracked files when `ui/node_modules` is absent** —
  including under `make release-check`, whose `--check` name promises
  read-only. A fresh worktree has no UI deps, so the pnpm scan finds nothing,
  the regenerated notices drop the npm half, and **44 `licenses/texts/npm/*`
  files plus `THIRD-PARTY-NOTICES.md` show up deleted/modified in `git status`**
  — a red gate that looks like a real copyleft regression and is not.
  `pnpm install --frozen-lockfile` in `ui/` first; `git checkout -- licenses/
  THIRD-PARTY-NOTICES.md` to undo it.
- **A rebase can silently eat the CHANGELOG.** Trunk's 0.6.6 commit reset
  `## [Unreleased]` to empty and opened `## [0.6.6]`. The rebase took trunk's
  side of that hunk and **dropped all 529 lines of 0.7 entries** — no conflict,
  no warning, and `make release-check` stayed green because it only greps that
  the `[Unreleased]` heading exists. Restored from
  `feat/v0.7-phase0-prerebase`. **After any rebase onto a release commit, diff
  `CHANGELOG.md`'s `[Unreleased]` against the pre-rebase branch**, and re-read
  restored entries for claims the new trunk made stale (one did: 0.6.6 fixed
  the site-config probe's image key a different way than D5 did).
- **`[Unreleased]` has eleven interleaved `### Added`/`### Fixed`/`### Changed`
  headings**, one per commit, per the plan's F.0b rule. **F.1 must consolidate
  them into one of each before the rename** — `RELEASING.md` step 6 pastes that
  section verbatim into the GitHub Release body.
- **Build it and run it.** Authoring alone missed a size estimate by 2×, a
  readiness loop calling an uninstalled binary, two packages declaring `noarch`
  while shipping a compiled binary, and a `set -e`+`pipefail` bug that killed a
  script silently.

---

## 7. Field signals — already scoped into the plan

`~/.claude/plans/crystalline-gliding-quill.md` carries two new phases from two
rounds of sanitized field notes. **Briefs:
`~/.claude/plans/v07-field-signals/BRIEF-agnostic.md` and `BRIEF-round2.md`.**

⚠️ **Both are sanitized and must stay that way.** No participant names, no
internal product or platform names, no organisation. Write findings as Wardyn
product reasoning — never "a customer said". Nothing identifying may reach
Wardyn docs, code, commits or the public ROADMAP.

- **Phase G — posture-gated autonomy.** An org-defined rubric mapping containment
  posture to permitted autonomy. Independently requested by the field, in the
  same shape. **0.8**, with `tool_rules` shipped in 0.7 as its first rung.
- **Phase H — field signals.** What we under-claim (the two-substrate
  conformance fact is our strongest pluggability claim and read as a footnote),
  what is misframed (`wardyn-toolgate` is **not** an MCP gateway), and a list of
  seams to **resist** building.

**Round 2 adds three things the UI campaign should weigh:**
1. **"The underlying sandboxing architecture was viewed as comparatively easy.
   The major challenge is workflow design, UX, discoverability, task
   orchestration and usability."** That is the UI campaign's mandate, stated by
   the field.
2. **The architectural idea**: Wardyn as the only approved host-level install,
   with agent tools running *inside* it — which is precisely
   `temporal-launching-crystal.md`'s "Orca as a UI Wardyn launches".
3. **Local and cloud should feel like one UI** — launch either, move workloads
   between them, one governance model, one approval process. Today the substrate
   is a per-deployment daemon flag and is not selectable per run.
