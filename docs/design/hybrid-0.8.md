# Hybrid local + remote — one org, two substrates, one person (0.8 design brief)

Status: **Phase 0 decided, 2026-09-19** — O1 `m′`-at-org, O2 offline runs continue with
durable evidence, O3 one audit chain per writer, O4 no placement field in Phase 1, O5
drive-as-source as Phase 3a; the defaults and their reasons are in
[0.8/PLAN.md](0.8/PLAN.md) § B, and Phase 1 is on the `0.8.0` milestone. Written in 0.7.2 as
research; in 0.8, only rung 3's enrolment and the one audit stream are built (see the
owner ruling below), and everything else here is still a proposal. This is the brief behind
[ROADMAP.md](../../ROADMAP.md)'s "Also new for 0.8: hybrid local + remote" row. Every
claim about today's tree is anchored to the file and line it came from, at
`feat/v0.7.2`; every claim about tomorrow is marked as a decision, an option or an
open question, and none of it is code.

**Owner ruling, 2026-09-23: the full rollout below moves to 0.9.** 0.8 ships only
rung 3 of §2.2's ladder (desktop enrolment to a remote control plane) and the one
audit stream (issues #102, #103, #106) — never per-run placement (§6), the disk link
(§7), or a laptop's runs deciding anywhere but locally. Rungs 4 and 5 and the
placement issues #107–#117 (except #115), T-27 (#687) and #475 are 0.9.0 work; see
[ROADMAP.md](../../ROADMAP.md)'s milestone table. What DID ship, and is no longer
this brief's proposal but its build: `docs/DESKTOP.md`'s "Enrolling into an org
control plane", `docs/OPERATIONS.md`'s "Managed laptops", and
[THREAT-MODEL.md](../../threatmodel/THREAT-MODEL.md) residuals #50–#52, all written
against the shipped code rather than this design's projection of it.

The shape of the ask, in one sentence: **the org runs the control plane on its
cluster, MDM installs Wardyn on the laptop, and the same person under the same
org-managed policy flexes a sandbox between local and remote hardware** — a quick
edit on the laptop's own CPU, a long build on the cluster's — with one identity, one
ceiling, one audit stream. The disk half follows from it: a local directory usable
inside a remote sandbox, and a remote drive readable locally.

---

## 1. The two tiers today

Wardyn ships exactly two deployment paths, and they do not know about each other.
The **org control plane** is the Helm chart on Kubernetes
([deploy/helm/wardyn/README.md](../../deploy/helm/wardyn/README.md),
[docs/OPERATIONS.md](../OPERATIONS.md)): a single-replica `wardynd`, SSO with a real
three-role model, NetworkPolicy-backed L1 egress confinement proven by a boot-time
canary, pods as sandboxes. The **desktop tier** is
[docs/DESKTOP.md](../DESKTOP.md): "A local daemon per laptop. No shared control
plane, no cluster." Its own Postgres, its own runs, its own audit fanout to the org
SIEM, four MDM-rendered files under `/etc/wardyn`, and a converge job that
re-asserts the stack every five minutes.

The two tiers already share almost everything that matters. They are the same
binary. They read the same `RunPolicySpec`. They both put every byte of sandbox
egress through a `wardyn-proxy` sidecar. They both write the same audit actions
into the same append-only, hash-chained table. Since 0.7.2 they also read the same
org provider policy: `SiteConfig.WorkspaceProviders` and `SiteConfig.AgentProviders`
are org documents the cluster serves over `PUT /site-config` and MDM delivers to a
laptop as `/etc/wardyn/site-config.json`
([deploy/desktop/wardyn.env.example:13](../../deploy/desktop/wardyn.env.example),
[docs/DESKTOP.md's topology diagram, :17-46](../DESKTOP.md)).

What they do NOT share is a control plane. A laptop's `wardynd` is authoritative for
its own runs and nothing else; the cluster's is authoritative for its own. Nothing
in either tier enrols a desktop into a remote control plane, nothing decides WHERE a
given run should execute, and nothing links a filesystem on one side to a sandbox on
the other. Hybrid is the deployment where those three gaps are closed and the rest
is already built. That is the honest framing, and it is half this brief's
credibility: the interesting work here is three specific holes, not a second
product.

---

## 2. Ground truth

### 2.1 What already exists, and must be reused rather than rebuilt

**Provider policy delivery is DONE, as of 0.7.2.** `SiteConfig.WorkspaceProviders`
is an org-authored policy over which git hosts a run may clone from, which
credential lanes it may use there, and the ephemeral/drive storage ceilings; its
sibling `AgentProviders` says which agents the org offers and how each reaches its
model. Both are carried forward when a `PUT /site-config` body does not name them
(`carryForwardUnnamedSiteConfigFields`, `internal/api/site_config.go`), which is
exactly what makes the MDM channel safe for them. Both tiers read one org document.
Rung 1 of the ladder in §2.2 needs nothing built.

**Identity is DONE for the profile hybrid requires.** Member mode (`m′`) already
makes the developer a non-operator against an org IdP: `WARDYN_LOCAL_MODE=false`
mandatory, OIDC required, `deriveRole` mapping the human to `member`, and
`validateMemberModePosture` (`cmd/wardynd/boot_posture.go:43`) refusing to boot
under any posture that would silently hand them admin back
([docs/DESKTOP.md:207-220](../DESKTOP.md) is the settings table and the invariant).
The other desktop topology, `a′`, is the developer AS the operator — loopback
callers are always admins — and it cannot be hybrid by construction: a person who
sets their own ceiling is not a person an org control plane bounds.

**An audited channel into a running sandbox is DONE.** The SSH gateway runs the
sandbox's own `sftp-server` as the subsystem's backing process — no SFTP protocol
reimplementation — and audits it as `ssh.sftp.transfer`
(`internal/api/sshgateway_channels.go:687-713`). That channel is the one existing
seam through which bytes can move between a human's machine and a sandbox under an
audit row, and §7 builds on it rather than inventing a transport.

**Per-user durable storage is DONE, on both substrates.** A `user_drive` row is one
admin-registered drive allocated to a person, a group or everyone. Four backends
exist; two of them — `host_path` and `k8s_pvc_static` — report `external`
enforcement, meaning something outside Wardyn (the NAS's own quota) binds the bytes
and Wardyn displays the allocation and claims nothing
(`internal/types/user_drive.go:234-236` for the constant,
`:247-256` for `EnforcementFor`). That pair is the whole mechanism §7's recommended
disk-link option needs.

### 2.2 The gaps, as a prerequisite ladder

| Rung | Today | Missing |
|---|---|---|
| **1 Provider policy delivery** | **Exists in 0.7.2.** `SiteConfig.WorkspaceProviders` / `AgentProviders`, MDM-delivered as `/etc/wardyn/site-config.json`; both tiers read one org document | nothing |
| **2 Identity** | `m′` has it (§2.1); `a′` is loopback-admin and cannot be hybrid | nothing new; hybrid REQUIRES `m′` |
| **3 Desktop enrolment to a REMOTE control plane** | a standalone daemon per laptop — its own Postgres, its own runs, its own audit fanout | **THE gap.** Either a *client mode* (the laptop runs no control plane; CLI and console talk to the org's `wardynd`, and a thin local agent offers the laptop as a runner target) or `m′` pointed at the org's `wardynd` for identity and policy while still owning its own runs. §5 |
| **4 Run placement** | `RunnerTarget` is per-DAEMON: one value chosen at boot (`cmd/wardynd/main.go:372`), validated against `knownRunnerTargets()` (`cmd/wardynd/boot_deps.go:257-265`), with an unknown value failing boot closed rather than advertising a target no stored object could match (`boot_deps.go:215-217`). `DriveBackend.RunnerTarget()` (`internal/types/user_drive.go:110-119`) refuses a k8s drive on a Docker deployment for the same reason | **per-RUN placement** (`local docker` vs `remote k8s`) with identical ceiling and provider policy on both. Every "what substrate is this deployment" becomes "what substrate is this run". §6 |
| **5 Local↔remote disk link** | nothing. `local_dir` is refused on Kubernetes (`errMountsUnsupported`; the chart README's known gaps) | the three options in §7 |

### 2.3 Non-gaps — asked for, already covered, and to be said out loud rather than built

- **Org-managed proxy.** Both HTTPS and SSH-over-443 git already ride
  `wardyn-proxy` to the upstream proxy named in site-config, which MDM ships. No
  per-image `.ssh/config` or `ProxyCommand` is needed on either side of a hybrid
  deployment, because the sandbox never dials the corporate proxy itself. The
  documented ceiling stays: self-hosted GHES/ADO Server over port-22 SSH is out of
  scope.
- **Per-user reusable disks.** These are user drives, with user/group/all
  overrides, already. Hybrid does not add a storage primitive; §7 adds a *placement*
  story for the one that exists.
- **One policy language.** A `RunPolicySpec` is substrate-agnostic today. Nothing
  in hybrid needs a second dialect, and adding one would be the fastest way to make
  the two halves disagree about what a ceiling means.

---

## 3. Decisions

These are decisions, not options. Each one is the smallest thing that works, and
each names what it rejected.

**D1 — Hybrid is `m′`-only.** A hybrid deployment requires member mode. `a′` is
excluded by construction, not by a check: the org cannot be the ceiling-setting
authority on a box where the person at the keyboard is an admin by loopback. The
practical consequence is that the enrolment story in §5 never has to answer "what
if the local admin disagrees with the org" — there is no local admin.

**D2 — Placement is a property of a RUN, not of a deployment.** The request gains a
placement field; the ceiling gains a term that bounds it; every existing
"what substrate is this deployment" read becomes "what substrate is this run". This
is rejected-alternative-shaped: the tempting cheap version is two daemons and a
console that shows both, which is not one product — it is two products with one
bookmark, and it cannot hold "one identity, one ceiling, one audit stream".

**D3 — The disk link ships as drive-as-source first (§7 option iii), with
sync-over-sftp (option ii) as the follow-on, and the reverse tunnel (option i)
permanently rejected.** §7 and §8 carry the argument. The short form: option iii
needs zero new mechanism, option ii needs a sync engine but no new protocol
surface, and option i requires re-opening a refusal the SSH gateway was designed
around.

**D4 — Nothing in 0.8 relaxes a refusal that exists today.** The gateway's channel
switch stays a closed `switch` with `default: reject`
(`internal/api/sshgateway.go:418-426`); global requests stay discarded, which is how
`ssh -R` is refused (`:374-378`). A sync lane adds one `case` and one audit action,
never an open dispatch. If a proposal in a later round needs one of those two
refusals lifted, that is the signal the proposal is the wrong shape.

**D5 — One audit stream means the LAPTOP's rows reach the org's table, not that the
org's table is copied to the laptop.** Direction matters for the threat argument in
§8: evidence flows toward the party the developer cannot edit.

---

## 4. Architecture

```
   org IdP ──────────────┐
                         │ OIDC (m′ on both sides)
                         ▼
  ┌────────────────────────────────────────────┐
  │  ORG CONTROL PLANE  (Kubernetes, Helm)     │
  │                                            │
  │   wardynd  ── SiteConfig ──▶ providers,    │
  │      │        ceilings, agent roster       │
  │      │                                     │
  │      ├──▶ pod sandbox  (remote placement)  │
  │      └──▶ audit table (append-only, chain) │
  └───────┬──────────────────────────┬─────────┘
          │  provider policy          ▲  audit rows,
          │  + ceiling, as today      │  run state
          │  (site-config.json)       │
          ▼                           │
  ┌────────────────────────────────────────────┐
  │  the developer's laptop (MDM, m′)          │
  │                                            │
  │   local agent ──▶ docker sandbox           │
  │                   (local placement)        │
  │   /etc/wardyn/*  ← MDM, unchanged          │
  └────────────────────────────────────────────┘
```

Read the diagram as three claims. **Down the left**, nothing new: the org's policy
already reaches the laptop as a file MDM renders, and 0.7.2 put the provider blocks
on that same channel. **Up the right**, the new thing that must exist for "one audit
stream" to be true: a laptop's run state and audit rows reaching the org's table
rather than only its SIEM webhook. **Across the middle**, the placement decision —
one run, two possible executors, one ceiling resolved once.

The piece the diagram deliberately does not draw is where `wardynd` lives on the
laptop, because that is §5's open decision: under client mode the laptop runs a thin
agent and no control plane; under `m′`-at-org it runs a full daemon that borrows
identity and policy from the org and keeps its own runs. Everything else in the
picture is the same either way, which is why the rest of this brief can be written
before that question is answered.

---

## 5. Enrolment

### 5.1 The two shapes

**Client mode.** The laptop runs no control plane at all. The CLI and the console
talk to the org's `wardynd` over HTTPS; a thin local agent registers the laptop as a
runner target and executes what it is given. Everything about identity, policy,
approvals, audit and run records is the org's by construction, so "one identity, one
ceiling, one audit stream" is true without anything reconciling. The cost is a new
component, a new registration protocol, and a laptop that does nothing useful
offline.

**`m′` pointed at the org.** The laptop keeps its full daemon and its own runs, and
borrows identity (the same IdP) and policy (the same site-config) from the org. This
is closer to what ships: the delta is a federation of run records and audit rows
upward rather than a new execution component. The cost is that "one audit stream"
becomes a synchronisation property rather than a structural one — two writers, one
table, and a hash chain that is append-only per writer.

Client mode is what makes §6's per-run placement genuinely possible, because
placement presupposes ONE scheduler choosing between two executors. `m′`-at-org
gives a weaker thing: two schedulers under one policy. **Open question O1 below;
this brief does not decide it.**

### 5.2 What the enrolment mint pulls, and what it must never pull

Today's desktop enrolment already has a sharp edge worth carrying forward rather
than repeating. `install.sh` mints the device's age key by running a `wardynd`
container **as root**, once, before MDM has delivered anything — by default from the
mutable `ghcr.io/cjohnstoniv/wardynd:latest` tag, unsigned, with nothing in the repo
verifying it ([docs/DESKTOP.md § What the enrolment mint pulls](../DESKTOP.md)).
The documented remedy is `WARDYN_INSTALL_IMAGE` pinned to a digest, and `install.sh`
warns when the ref it is about to run carries none.

A hybrid enrolment adds a second secret to that moment: whatever credential lets
this laptop speak to the org's control plane as itself. The rule this brief sets is
that **the enrolment mint pulls an image and mints a device-local key, and that is
all it may do.** It must never pull the org credential, because a bootstrap that
fetches a long-lived control-plane credential over an unverified image is the same
mistake `age.key` avoided by being minted locally instead of pushed from MDM. The
shape that fits what already ships: MDM delivers a short-lived enrolment token in
the `0600` `secret.env` it already owns, the daemon exchanges it once for a
device-scoped credential, and the exchange is one audit row on the org's table. That
keeps the wide-audience artifacts (MDM payload database, config-profile exports,
backups) carrying only something already expired by the time anyone reads them.

### 5.3 Offline, extended

[docs/DESKTOP.md § The laptop is sometimes offline](../DESKTOP.md) already tables
four network-touching sites and what each does when it cannot reach the network.
Hybrid adds rows to that table rather than replacing it, and the interesting ones
are the failures that are *correct*:

| Site | Offline behaviour hybrid must specify |
|---|---|
| Enrolment to the org control plane | **Needs the network, once** — the same inherent cost as `install.sh` today. Pre-enrol on-network. |
| Placement resolution | **Fails closed for `remote`.** A run the person asked to place on the cluster cannot run on the laptop instead: substituting a different substrate silently is the placement version of the cross-mechanism credential fallback 0.7.2 refused. Refuse, naming the placement and the reason. |
| Local placement, org unreachable | **The decision to make.** Under client mode there is nothing to run: no control plane, no run record, no ceiling. Under `m′`-at-org, a local run under the last-delivered ceiling is defensible — that ceiling is a file MDM re-asserts every five minutes — but it means evidence is buffered rather than recorded, which is the one failure mode the desktop tier already loses on (audit fanout is at-most-once past 4096 events). |
| Audit federation | **Buffered, bounded, and honest about the bound.** Never "we'll catch up eventually" without a number. |

---

## 6. Placement

### 6.1 The request field and the ceiling term

Placement enters as a request field with two values to start (`local`, `remote`),
resolved once and recorded on the run. It needs a matching ceiling term, because the
first thing an org will ask for is "contractors may not run this locally" or
"anything touching production credentials runs on the cluster". That term belongs on
`GovernanceLimits`, where the tiering already exists (user over group over all), and
it follows the rule that type already carries: **a zero value means unlimited**, so
adding the field changes no existing profile's meaning. A ceiling that DENIES a
placement refuses at create, naming the placement — the shape `DenyUserDrive` and
`deny_interactive` already use.

The clamp site matters as much as the field. 0.7.2's storage work learned this the
expensive way and landed both of its ceilings at exactly one site each — the
ephemeral clamp at dispatch, the drive clamp at the resolver fold — because a limit
applied in two places is a limit that will eventually disagree with itself.
Placement should do the same: one resolution, at create, recorded on the run record,
and every downstream reader asks the run rather than re-deriving.

### 6.2 Drive backends versus placement

This is the sharpest coupling in the whole design, and it is already half-written.
`DriveBackend.RunnerTarget()` (`internal/types/user_drive.go:110-119`) maps
`docker_volume`/`host_path` to `docker` and `k8s_pvc`/`k8s_pvc_static` to `k8s`, and
a mismatch is refused. Under per-run placement that predicate stops being a
deployment-level truth and becomes a per-run one: the same person's drive must
resolve on both sides, or their runs stop being placeable.

That is exactly why §7's recommendation is drive-as-source. A person with a
`docker_volume` drive has storage that exists only on one laptop; a person with one
`host_path` row locally and one `k8s_pvc_static` row remotely, both pointing at the
same export on the same NAS, has storage that resolves under either placement with
no new mechanism. The design consequence for 0.8 is a constraint, not a feature:
**placement-eligible drives are the `external`-enforcement backends**, and the
product should say so rather than discovering it at mount time.

### 6.3 Capability aggregation across two substrates

`/healthz` reports the actual running implementation per seam, and confinement
classes are advertised per substrate: the Docker substrate offers CC1/CC2/CC3
subject to runtime pins, the k8s substrate is CC1-only until `k8s.runtimeClasses`
pins the others ([docs/PLUGGABILITY.md](../PLUGGABILITY.md)). Under hybrid, "what
classes does this deployment offer" has two answers, and the honest aggregate is the
INTERSECTION for anything a policy can demand and the UNION only for what a reader
is being shown. A policy asking for CC3 must not be admitted because one of the two
substrates could have served it; a run pinned to a placement is bounded by that
placement's real capabilities, resolved before the run is created.

The same rule governs every other per-substrate capability already in
`runner.Capabilities`: BYOI/devcontainer builds and `local_dir` mounts exist on
Docker and not on Kubernetes, and the ephemeral-disk enforcement word differs by
substrate (`filesystem` or `none` on Docker, `eviction` on Kubernetes — the word
0.7.2 added to complete the five-word set). A hybrid Review rail that shows one number for a run whose placement is
not yet resolved is showing a guess, and 0.7.2's ephemeral-disk preview is the
precedent for the answer: ONE expression, called by both the preview and the
dispatch, rather than two that drift (`api.ephemeralDiskFor`).

---

## 7. The disk link

### 7.1 Option (i) — reverse tunnel + SSHFS/9p over the SSH gateway: REJECT

This is structurally refused today, and refused deliberately. `ssh.DiscardRequests`
replies false to every global request that wants a reply, which is exactly how
`-R` (the `tcpip-forward` global request, the remote/reverse port-forward a tunnel
needs) is refused — the code comment at `internal/api/sshgateway.go:374-378` says so
in those words. The channel switch a layer down rejects everything but `session` and
`direct-tcpip`, and its `default` arm names `auth-agent@openssh.com` and `x11`
specifically as the channel TYPES agent and X11 forwarding ride on (`:418-426`).

Enabling a reverse tunnel means reopening a refusal the gateway was designed around,
for the benefit of a transport whose other half is also bad: WAN-latency FUSE has
POSIX semantics no build tool expects, and the failure mode of a `git status` or an
`npm install` over a high-latency FUSE mount is a hang, not an error. The rejection
is permanent, and §8 carries the security half of the argument.

### 7.2 Option (ii) — file sync over the EXISTING sftp channel: the follow-on

The gateway already runs the sandbox's own `sftp-server` as the subsystem's backing
process and audits every session as `ssh.sftp.transfer`
(`internal/api/sshgateway_channels.go:687-713`). A mutagen- or rsync-shaped
bidirectional sync of ONE local directory against a per-user PVC needs **no new
protocol surface** — it is a client on a channel that already exists, under an audit
action that already exists, inside a session that is already run-scoped.

What it costs is a sync engine's conflict semantics (two writers, one tree, and a
developer who will `git checkout` on one side while the agent writes on the other)
and a long-lived session per run. Neither is a protocol problem, which is the point:
this option's risk is product risk, and product risk is the kind a phase can retire
with a pilot. It is the answer for the case option (iii) cannot serve — **the
laptop's own working tree**, which is not on a NAS and is not going to be.

### 7.3 Option (iii) — drive-as-source: the shipped answer

One `user_drive` row per person, on the same network storage, registered twice: as
`host_path` for the laptop's Docker substrate and as `k8s_pvc_static` for the
cluster. Both backends exist today, both mount today, and both report `external`
enforcement — Wardyn displays the allocation and claims nothing about bytes, because
the export's own quota is what binds them (`internal/types/user_drive.go:234-236`,
`:247-256`).

Zero new mechanism. The ceiling is honest and worth stating in the product rather
than the release notes: this needs storage both sides can reach, and it does **not**
link the laptop's own working tree — it links a shared location both sandboxes can
bind. For a team already on a corporate NAS or an EFS/Filestore export, that is the
whole feature, available for the price of a second drive row. For a team without
one, it is not an answer at all, which is why (ii) follows it rather than replacing
it.

### 7.4 Seams 0.7.2 already protects for this

Four, and they are protected on purpose:

1. `targetReservedForDrive` (`internal/runner/mount.go:95-102`) is a PREFIX rule
   over ONE constant — `tgt == DriveTarget || strings.HasPrefix(tgt,
   DriveTarget+"/")`, with `DriveTarget = "/home/agent/drive"` (`mount.go:63`).
   Read the protection precisely, because it is easy to overclaim: the prefix
   shape means the drive's own subtree needs no re-spelling, and it does **NOT**
   already cover a sibling family. `/home/agent/linked/<slug>` is reserved by
   nothing today — a second family is a second constant plus a second arm in this
   one predicate. What the seam buys is that the change stays additive and stays
   at a single site, not that it is already made.
2. `driveVolumeName` is the static literal `"drive"`
   (`internal/runner/k8s/drives.go:36`) because one principal has at most one
   drive — a LIMIT-1 invariant 0.8 must widen deliberately, in the resolver, not by
   accident in a singleton helper.
3. `GovernanceLimits`' zero-means-unlimited rule holds, so a 0.8 `max_linked_dirs`
   or a placement door is a new field rather than a migration of meaning.
4. The provider policy is NOT keyed by substrate, and no consumer assumes "this
   deployment's substrate", so a `placement: [local, remote]` axis is additive on
   the same row.

---

## 8. Security

### 8.1 What leaves the laptop, per option

**The security argument IS the ordering argument — (iii), then (ii), and never
(i).** Read the three paragraphs below as one ranking, not three descriptions: the
question each answers is "what does a compromised sandbox reach, and what new
audit action has to exist before anyone can tell".

Under **(iii)**, nothing leaves the laptop at all. Both sandboxes bind a path on
storage the org already owns and audits, so the laptop is a client of that storage
exactly as it is today. **New audit actions: none** — the rows are the ones that
already exist, `drive.*` plus the mount recorded on the run. A compromised sandbox
reaches one person's home directory on one share, bounded by the export's own
quota and by `ReadWriteOnce` / `read_only`: the same blast radius a drive has on
either tier today, which is the strongest thing that can be said for a design. It
adds no new reachable surface, so it goes first.

Under **(ii)**, the laptop's bytes do leave — but through an audited, run-scoped,
governed channel. **New audit actions: none required either**, because the sftp
channel already audits as `ssh.sftp.transfer`; what a sync lane should add is one action of
its own naming the selected directory, so a reader can distinguish a human's
interactive `sftp` session from a sync engine holding one for a run's lifetime.
The blast radius is the directory the human selected — not a mount namespace, not
`$HOME`, not whatever the agent can path-traverse to — and the selection is a
human act per run rather than a standing grant. The residual to write down when it
is built: a sync engine holds its session for the life of the run, so the window
in which that directory is reachable is the run's whole lifetime rather than a
single transfer. That is a real widening over (iii), and it is why (ii) is second
rather than first — it buys the one thing (iii) cannot serve, the laptop's own
working tree, at the price of a standing session.

Under **(i)**, a compromised sandbox gets a route BACK INTO the laptop's
filesystem, over a tunnel the gateway refuses by construction — and **the audit
question has no good answer**, because a FUSE mount's reads and writes are not
events the gateway sees at all, so there is no action to add. That is the
asymmetry that settles the ranking: (iii) and (ii) both move bytes outward through
a channel the org controls and can record, while (i) opens an inbound path into
the one machine in the picture the org controls least, unrecorded. The threat
model's agent-side chapter assumes a prompt-injected agent inside the sandbox as
the ordinary case; handing that agent a file-protocol route to the developer's
home directory is not a feature with a mitigation, it is the wrong direction.

### 8.2 What a hybrid deployment adds to the threat model

Three things, and each needs its own residual when it is built.

**A second execution site under one identity.** Today, a run's substrate is a
property of the deployment an operator chose; under hybrid it is a property of the
run, which means an attacker who can influence placement can influence which
confinement class actually applied. §6.3's intersection rule is the mitigation, and
the residual is that a person reading a run record must be able to see which
placement served it — the `disk_mib_filled` provenance bit 0.7.2 added is the
precedent for carrying that on the run rather than inferring it.

**A device-scoped credential that did not exist before.** §5.2's enrolment exchange
produces something a laptop holds continuously; the compromise story for it is the
compromise story for the laptop, and the mitigations are the ordinary ones —
short-lived, device-scoped, revocable from the control plane, and its use audited so
a revocation has evidence attached.

**A federated audit chain.** `audit_events` is append-only and hash-chained, and the
chain is per writer. Two writers means either two chains reconciled at read time or
one chain with a serialization point, and the choice is a security decision rather
than an implementation detail: a chain that can be reordered by a party the design
does not trust is tamper-evidence that does not evidence anything. This brief does
not decide it (open question O3), but it does state that the direction of flow is
laptop→org (D5), so the party who cannot edit the org's copy is the party being
audited.

---

## 9. Ops, docs and the MDM envelope

The MDM envelope changes by **one file's contents, and no new files**. Hybrid needs
`wardyn.env` to carry whatever posture variable selects client mode or `m′`-at-org
plus the org's control-plane URL, and `secret.env` (already `0600`, already
MDM-owned) to carry the short-lived enrolment token from §5.2. `policy.json` and
`site-config.json` are unchanged in shape: the ceiling and the provider policy are
already org documents, which is rung 1 of the ladder being already done.

The posture rule from [docs/DESKTOP.md § Posture switches are env vars, never
site-config](../DESKTOP.md) binds here and settles the placement of everything
above. Anything that decides what this daemon IS — whether it runs a control plane
at all, which org it answers to — belongs in `wardyn.env`, where a missing line is a
missing line. Anything that says what the org ALLOWS belongs in site-config, where
the 0.7.2 carry-forward makes a partial write safe. Placement mode is the first;
placement *limits* are the second.

Documentation lands as an extension of what exists rather than a third document.
[docs/DESKTOP.md](../DESKTOP.md) gains the hybrid topology beside `a′` and `m′` and
the offline rows from §5.3; [docs/OPERATIONS.md](../OPERATIONS.md) gains the
placement ceiling in its governance section and the two-substrate capability rule
from §6.3; [docs/USERS.md](../USERS.md) gains one short section, because
placement is the first thing in Wardyn a member will ask for by name. A fourth
document describing "hybrid" as a separate product would be the documentation
version of the mistake D2 rejects.

---

## 10. Tests and conformance

The bar is already set by what 0.7 did for the k8s substrate, and hybrid should not
be allowed to claim less. `test/conformance` runs against **both** targets today —
`conformance` on a live Docker daemon and `conformance-k8s` on a real kind cluster
with `disableDefaultCNI` and pinned Calico, because kind's default CNI does not
enforce `NetworkPolicy` — and a case with no k8s equivalent self-skips by design
rather than faking a result. Placement is a routing decision over exactly those two
already-conformance-tested substrates, so the new suite is a *routing* suite, not a
second substrate suite.

Three things need pinning that nothing today covers. **Placement is honoured or
refused, never substituted**: a run asking for `remote` when the cluster is
unreachable fails naming the placement, and a test proves no local sandbox was
created. The shape to copy is 0.7.2's own refused record launch, pinned by
`TestRecordLaunchRefusedMintsNothing`
(`internal/api/runs_dispatch_llm_mechanism_test.go:566`): the assertion is not
"the call returned an error" but "no sandbox exists and no credential was
composed". **The ceiling is resolved once**: a placement-denying profile refuses
at create, and the same request under a permitting profile produces a run whose
record names the placement it actually got. **Capability aggregation intersects**:
a policy demanding a confinement class only one substrate offers is refused before
placement is resolved, not admitted and then disappointed.

The audit half needs its own gate, because it is the claim most likely to rot: one
run, one placement, and every row the run produced present in the org's table with
the chain intact. That is an integration test with a real database on both ends, and
it is the test that decides whether "one audit stream" is a product claim or a
marketing one.

---

## 11. Phased plan

**Phase 0 — decide O1.** Client mode versus `m′`-at-org is the fork everything else
hangs off, and this brief deliberately does not pick it. The deliverable is a
decision with the enrolment protocol sketched to the depth §5.2 sketches the mint,
because the credential shape is the part that is expensive to change later.

**Phase 1 — enrolment, no placement.** A laptop enrols into the org control plane
and runs locally under the org's identity, ceiling and provider policy, with its
audit rows federating upward. Placement does not exist yet; every run is local. This
phase is worth shipping on its own, because it is the whole of "one identity, one
ceiling, one audit stream" for an org that only wants governance, not elasticity.

**Phase 2 — placement.** The request field, the ceiling term, the intersection rule
for capabilities, and the drive-backend constraint from §6.2. Two values only,
`local` and `remote`; no scheduling, no auto-placement, no cost model. A person
chooses, a ceiling bounds, a run record says which one served it.

**Phase 3 — the disk link.** Drive-as-source first (§7.3), which is documentation
and a product affordance over mechanism that already ships. Sync-over-sftp (§7.2)
follows it as its own phase with its own pilot, because its risk is conflict
semantics rather than protocol, and that is the kind of risk a pilot retires and a
design round does not.

---

## 12. Owner questions

**O1 — client mode, or `m′` pointed at the org?** §5.1. This is the fork. Client
mode makes placement structurally clean and makes the laptop useless offline;
`m′`-at-org is closer to what ships and makes "one audit stream" a synchronisation
property rather than a structural one.

**O2 — is a local run allowed while the org is unreachable?** §5.3. The ceiling is a
file MDM re-asserts every five minutes, so running under it offline is defensible;
the cost is that evidence is buffered rather than recorded, and the desktop tier
already loses evidence past its 4096-event buffer.

**O3 — one audit chain or two?** §8.2. Two writers into one append-only,
hash-chained table is a security decision, not an implementation detail.

**O4 — does placement default, and to what?** A default of `local` makes hybrid feel
free and quietly moves work onto laptops; a default of `remote` makes it feel like
the cluster product with a laptop attached; requiring an explicit choice is honest
and is one more field in front of every run.

**O5 — is drive-as-source enough to ship the disk link as "done"?** §7.3 serves a
team already on shared storage completely and a team without it not at all. Whether
that ships as the feature or as phase 3a, with sync-over-sftp named as the rest of
it, is a positioning call rather than a technical one.
