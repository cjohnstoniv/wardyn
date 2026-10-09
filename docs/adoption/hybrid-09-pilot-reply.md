# 0.9 hybrid: what an enterprise pilot needs from local+remote, and our answers to O1–O5

An enterprise pilot's reply to the hybrid brief's owner questions (2026-10-09)

**Read with:** `docs/design/hybrid-0.8.md`. This is a deployment's reply to §12's owner questions, not a feature request. Everything below is grounded in v0.8.9 source; the source symbol or file is given wherever we assert behaviour, because two of our conclusions contradict things we previously believed.

**Who is writing.** An enterprise pilot running the Kubernetes tier for a small group of developers: single-replica control plane, enterprise SSO, CC1-only (no gVisor/Kata runtime registered), the only reachable model endpoint an internal one over a private link, and a corporate forward proxy that is the sole route to it. Public package registries are blocked at the network layer by the corporate egress tier, so an internal artifact mirror is the only install path. We have the k8s tier working and governed. We are now being asked for two things it cannot do, and both land in 0.9.

---

## 1. The two requirements, as outcomes

**R1 — A developer's own working directory, on their own laptop, usable by a sandbox.** Not a share. Not a NAS. The actual directory they already work in, on the machine in front of them. Near-live sync is acceptable; a mount would be better but we understand why it is not on offer.

**R2 — A sandbox that executes on the laptop's own CPU/RAM/Docker, while the remote control plane remains the single authority** — one console at the remote URL, one ceiling, one audit log, one identity. The model our developers already understand from self-hosted CI runners: install a small local service, it registers outbound with the central system, the central system dispatches work to it.

R1 collapses into R2. `local_dir` is refused on Kubernetes — `errMountsUnsupported` in `internal/runner/k8s`, "host bind mounts are not supported on this substrate" — and correctly so, because on that substrate "host" means a cluster node, not the developer's machine. There is no local-disk story without a local executor. We think this is worth stating in the brief, because §7 reads as if the disk link were independent of §5's fork, and for the laptop-working-tree case it is not.

---

## 2. O1 — client mode, and the objection that would otherwise sink it

**Our answer: client mode.** §5.1's description is what our developers asked for, unprompted, in the vocabulary of self-hosted CI runners. Three reasons beyond "it is what we want":

1. **It is the only shape that satisfies R2 as written.** §5.1 already says it: "placement presupposes ONE scheduler choosing between two executors. `m′`-at-org gives a weaker thing: two schedulers under one policy." We do not want two schedulers. We want the cluster to be the control plane.

2. **We have measured what `m′`-at-org costs an operator, because we are living in its phase-1 form.** We enrolled nothing, but we read what we would be signing up for: the laptop keeps its own Postgres, its own governance resolution (`ResolveGovernanceProfile` reads the local store), its own OIDC, its own age key — and `internal/federation/client.go` has exactly three methods, `Enrol`, `Push`, `Heartbeat`. No policy-fetch route exists. So our four composed governance profiles and their assignments would not reach a laptop at all; the ceiling would arrive as an MDM-rendered `policy.json` we maintain by hand, in parallel, with drift invisible from both sides. D2 already names this outcome — "two products with one bookmark" — and we agree with D2 from the operator's chair. Maintaining a second copy of a ceiling is the single thing we would most like 0.9 not to ask of us.

3. **It shrinks the MDM surface rather than growing it, and removes a credential we currently cannot defend.** Today `m′` needs four files rendered per device, two of which are policy (`policy.json`, `site-config.json`), plus an OIDC **confidential** client secret. The `wardyn.env.m-prime.example` comment is admirably blunt about what that last one means: "On a fleet that means shipping ONE OAuth client secret to EVERY laptop, where any developer who is root on their own machine can read it out of `secret.env`. It is a shared secret, not a per-device one, and 0600 does not make it one." Under client mode the ceiling and provider policy come from the control plane and MDM ships two binaries and a registration token. The shared secret does not need mitigating; it stops existing.

**O3 dissolves under this answer.** "One audit chain or two" is only a question if there are two writers. Under client mode there is one. We read that as a further argument for client mode rather than as a separate decision.

### 2.1 The objection: on a developer-rooted laptop, every credential is readable

This is the thing we expect our own security reviewers to raise first, and we would rather hand you the answer than the problem. The exposure is irreducible, not a delivery-mode bug:

- On the Docker driver, `SecretEnv` rides as **plain container environment variables** (`internal/runner/docker` network driver), and the code comment calls this "the honest posture on this substrate" — correctly, because today wardynd and the daemon are presumed to share one controlled environment.
- Proxy-side injection does not rescue it. The proxy is a **sibling container on the same engine** (`internal/runner/docker` network driver) and its config arrives the same way, so a credential that is "never resident in the sandbox" is still readable by whoever holds the Docker socket.
- On a laptop, that is the developer, by definition.

So: **anything a locally-placed run can use, the developer can read.** No TTL, enclave or masking design changes that, and we think any 0.9 proposal that implies otherwise will not survive review at a deployment like ours.

### 2.2 Our proposed rule: placement eligibility is a function of the grant set

Do not try to hide credentials from the laptop's owner. Instead:

> **A run may be placed locally only if every credential it would receive is one whose plaintext the launching principal is already entitled to hold on their own machine.**

Under that rule, local placement grants the developer **no authority they did not already have**, and the irreducible exposure above stops being an exposure at all. The product already has the distinction this needs:

| Credential | Locally placeable? | Why |
|---|---|---|
| the person's own stored secret (own namespace) | **yes** | already theirs; `secretOwnerFromRequest` already scopes this |
| their own SSH key / their own PAT | **yes** | already on their machine in practice |
| their own model-provider credential | **yes** | connected by them, for them |
| a component secret marked `shared` | **no** | the operator's value; residual 64(b) already flags the echo risk |
| an operator-namespace (`""`) secret | **no** | explicitly not theirs to read |
| a **brokered** repo token | **no** — and this is the sharp one | the broker's value is "repo-scoped, minted proxy-side, never in the sandbox". On a local runner the proxy is local, so the token becomes developer-readable plaintext carrying a scope they could not otherwise mint. That is a real regression in the broker's trust model and we think it must be refused rather than documented. |

Existing primitives that express it: grants already carry `OwnerOnly`; component secrets already carry `shared`; the operator-vs-member secret namespace split already exists.

**This also answers O4, and we think it answers it better than any default would.** Placement should not default — it should be **derived**. A ceiling term says whether local placement is permitted at all; the grant set then decides eligibility per run; a run naming a non-eligible credential is refused local placement with a reason that names the credential. Nobody argues about whether the default quietly moved work onto laptops, because there is no default to argue about, and the refusal is self-explaining in the way 0.8's denial reasons already are.

One consequence worth stating: under this rule a locally-placed run cannot use the git broker, so the local lane's SCM story is the developer's own credential. For R1 — their own working tree, which they already have checked out and already have credentials for — that is exactly right, and it is a reason the rule is a good fit rather than a tax.

---

## 3. O2 — a local run while the org is unreachable

**Our answer: no, and we would rather it be a clean refusal than a buffered one.** Under client mode we accept a laptop that cannot start a governed run offline. The thing we cannot accept is a run whose authorization nobody recorded, because the audit trail is most of why we are deploying this at all. Two caveats on that answer:

- Please keep the existing local-only tier available as a **separate, clearly-labelled** deployment rather than folding it in as a degraded mode of the governed one. "Offline" and "ungoverned" being the same state by accident is the failure we are trying to design out.
- Refusing at run creation is fine. Killing a run **already in flight** when the link drops is not — a developer mid-task should not lose work to a network blip. The existing revocation gate's shape is right here: `Server.createRun` refuses new work and leaves running work alone (`internal/api/org_revocation.go`).

---

## 4. O5 — is drive-as-source enough? No, not for us

Direct evidence, since the brief asks for exactly this. §7.3's ceiling is stated honestly and it is the ceiling that excludes us: drive-as-source "does **not** link the laptop's own working tree — it links a shared location both sandboxes can bind."

We do not have network storage reachable from both substrates, and **we are not going to provision one for this.** Our developers' working trees are on their laptops and will stay there. So for us option (iii) is, in the brief's own words, "not an answer at all."

**Option (ii) — sync over the existing sftp channel — is the requirement, not the follow-on.** We would rather see (ii) land in 0.9 with (iii) documented as the shortcut for teams who happen to have shared storage, than see (iii) ship as "the disk link" and (ii) slip. We recognise that is a positioning call and that we are one deployment; we are stating a preference, with the reason that (iii)'s precondition is a storage procurement we would have to win, and (ii)'s is not.

We agree with the rejection of option (i). We verified the two halves independently and both hold: `ssh -R` is refused structurally (`internal/api/sshgateway.go`), and FUSE-over-WAN failing as a hang rather than an error matches our own experience with build tooling.

### 4.1 What option (ii) needs that is not there yet

We looked at whether we could pilot (ii) today on the existing channel. Mostly yes, with four concrete gaps:

1. **No sync-capable binary in the published agent images.** The claude-code image installs exactly `git curl ca-certificates asciinema tmux openssh-client openssh-sftp-server socat corkscrew` (`deploy/images/claude-code/Dockerfile`, package install layer). `rsync` is absent, so `rsync --server` cannot run; `unison` likewise. **Adding `rsync` is one line and would unlock the well-trodden client today**, independent of the rest of 0.9. We would take that change on its own.
   - Tools that ship their own agent (Mutagen, and VS Code's remote server) should work on the existing channel, since §5's Remote-SSH recipe already establishes "local tool pushes its own binary in" as a supported pattern via `remote.SSH.localServerDownload: "always"`. We have not yet proven Mutagen specifically.
   - Note bring-your-own-image is not a workaround for a deployment with the per-run image builder off: a `--image` run is rejected outright, so the binary has to be in the published image.

2. **The 4-channel budget is too tight for this use case.** `session` (shell/exec/sftp) and `direct-tcpip` share one per-run cap of 4, and a fifth is refused rather than queued. An editor holding a shell, a sync session and a couple of forwards exhausts it — and that is the exact combination (ii) is for. Either the sync lane wants its own budget, or the cap wants to be configurable.

3. **The sync lane would become the largest unaudited data path in the product, in both directions.** Today `sftp` payloads are not recorded and uploads are not byte-counted; SSH.md's own warning says "do not present Remote-SSH as the recommended developer path without saying so." We accept that for an occasional file copy. If (ii) makes continuous bidirectional sync a first-class developer path, that warning is no longer proportionate to the traffic. We are not asking for payload recording — we are asking that the decision be made deliberately, and that **upload byte accounting** exist at minimum, so the volume of a sync lane is visible even when its content is not.

4. **Conflict semantics.** We agree with the brief that this is product risk, not protocol risk, and that a pilot is the right instrument. We are willing to be that pilot (§7).

---

## 5. Engineering inventory: what we verified transfers, and what does not

Offered because a proposal that ignores the hard parts deserves to be dismissed. All at v0.8.9.

**Transfers with little or no change**

- **The Docker driver's proxy shape is already the laptop shape.** Sibling container, same per-run network, IP pinned into the agent's `/etc/hosts`. One engine, two containers — which is one laptop. (The k8s driver uses a separate pod, so Docker is the pattern to reuse.)
- **Recording upload already assumes zero inbound reachability.** `wardyn-rec` runs inside the sandbox and pushes to its own local proxy, which forwards up (`internal/runner/docker/driver_exec.go`, `internal/runner/k8s/exec.go`). Substrate-agnostic by construction; this is the piece with no gap at all.
- **Federation's transport scaffolding.** Outbound-only, bearer-authenticated, redirect-refusing, with revocation detection already built. Right plumbing, wrong payload.
- **`Capabilities` is already the vocabulary a scheduler needs** — `ConfinementClasses` (advertised only where the runtime is actually registered), `ManagedFiles`, `UserDrives`, `Freeze`, `StructuralEgress`, `EphemeralDiskEnforcement`. Nothing reads it to choose *between* runners yet, but the schema exists and fails closed, which is the hard half.
- **The device identity and revocation half already shipped.** Enrolment token, `wdd_`-prefixed per-device bearer scoped to that device's ingest routes only, durable local revocation mark, `createRun` gate. Client mode needs a different payload on this channel, not a different trust model.

**From scratch, in severity order**

1. **`Runner` is 11 synchronous in-process methods** plus a dozen optional capability interfaces, all direct blocking calls. A remote runner wraps every one in an RPC or stream — including `ExecStream`'s raw PTY bytes and the `OnWaiting` callbacks that assume same-process execution. This is the central refactor, not a shim.
2. **Exactly one runner per daemon, resolved once at boot** (`-runner`/`WARDYN_RUNNER` → `substrate.New`, wired for the process lifetime). There is no placement or scheduler layer in any form; the only "placement" in the tree is Kubernetes pod placement. A fleet of laptop runners under one control plane is a new component. Rung 4 says this, and we are confirming it from the other side: `DriveBackend.RunnerTarget()` already refuses a k8s drive on a Docker deployment, so the per-deployment assumption is load-bearing in more than one place.
3. **No job-claim channel.** `Heartbeat` POSTs an empty body and receives `DeviceAck{AckedSeq}` — one integer. The control plane can currently send a device **nothing**. Outbound-initiated job claim is new wire format and new server logic end to end.
4. **Attach has no relay on the Docker driver.** It is a bare Docker exec hijack from wardynd to the socket (`internal/runner/docker/session.go`). The k8s driver at least goes through the apiserver, which is an indirection a laptop relay could mimic; Docker has none. Since a usable attach is most of what makes R2 pleasant, we flag this as bigger than it looks.
5. **Credential delivery** — see §2.1/§2.2. We think this is a design decision rather than an engineering gap, and we have proposed the rule we would want.

---

## 6. Prerequisites 0.9 would otherwise inherit broken

Three things we hit this quarter that are not hybrid features but would land underneath hybrid.

**1. The egress risk classifier is inverted for corporate estates.** `safeBaselineDomains` (`internal/composer/risk.go`) is a hardcoded map of 24 **public** SaaS and registry hosts, with no env var, site-config key or policy field that extends it — we grepped for one. Any run whose `allowed_domains` contains only internal hosts therefore has a non-empty `beyondBaseline` and grades egress `OPEN`, the worst value, **no matter how narrow the allowlist is** (`internal/composer/autonomy.go`). A deployment allowlisting one internal model endpoint grades worse than one allowlisting the whole public registry set. `internal_hosts` does not feed it; we confirmed `internal/composer` never imports it.

- Consequence for 0.9: if placement or autonomy keys off egress posture, it inherits a classifier that is backwards for precisely the estates that would buy hybrid. The same map also drives `apiKeyToNonBaselineHost`, so an `api_key` to our own internal endpoint trips a CC3 floor that a CC1-only deployment cannot satisfy.
- Ask: make the baseline set extensible by the operator, or score against the deployment's own declared internal hosts.

**2. There is no middle autonomy rung.** The managed-settings documents are frozen literals with no operator override. L0/L1 set `defaultMode: default` **and** the three guardrail locks (`disableBypassPermissionsMode`, `allowManagedHooksOnly`, `allowManagedPermissionRulesOnly`); L2 moves to `acceptEdits` and **drops all three**; L3 gets no document at all.

- So "let the agent work without prompting me, but do not let it rewrite its own hooks and permission rules" is unexpressible. That is the posture we want on a trusted developer's laptop, and we expect it to be the common hybrid ask.
- Ask: separate `defaultMode` from the guardrail locks, so the prompting behaviour and the self-modification locks are independent axes.

**3. Components union into the allowlist without the ceiling being consulted, and that interacts badly with local placement.** the comment on `componentHostsBounded` (`internal/api/components_run.go`) says "The allowlist is deliberately not consulted: a component may reach anything the organisation has not blocked," and `expand()` calls `unionAllowedDomains`, a one-way append. We understand and accept the design intent on the cluster, where deny lists are the backstop and the proxy is ours.

- On a local runner the proxy is on a machine the developer administers, so a self-defined component is a member widening their own egress on hardware they are root on. `autonomy_cap` caps the autonomy level of such a run but does not bound its destinations.
- Ask: state explicitly how components and placement interact, and consider whether a locally-placed run may carry a self-defined component at all. This is cheaper to decide now than to retrofit.

---

## 7. What we can offer

- **A pilot for option (ii).** One developer, one laptop, their real working tree, a real estate with a corporate proxy and a blocked public registry path. We are willing to run it and report what breaks, including the conflict semantics §7.2 flags as the real risk.
- **A second substrate for conformance.** Our estate has properties that are awkward and common: CC1-only, an internal model endpoint over a private link, a forward proxy that is the sole route to it, public registries blocked at the network layer. If the §10 conformance suite wants a deployment where the easy assumptions fail, we are one.
- **Review of the O1 enrolment protocol sketch** from the operator side before it sets, since §11 Phase 0 notes the credential shape is the expensive part to change later.

## 8. Summary of asks

| # | Ask | Depends on 0.9? |
|---|---|---|
| 1 | Choose **client mode** for O1 | yes — this is the fork |
| 2 | Make local placement eligibility **derived from the grant set** (§2.2); no placement default | yes |
| 3 | Refuse brokered-credential runs from local placement | yes |
| 4 | Ship **option (ii)**, sync over the existing sftp channel, as the disk link | yes |
| 5 | Raise or make configurable the **4-channel per-run cap** | no |
| 6 | Add **`rsync`** to the published agent images | no — one line, useful immediately |
| 7 | **Upload byte accounting** on the sftp channel | no |
| 8 | Make `safeBaselineDomains` operator-extensible | no — but 0.9 inherits it |
| 9 | Separate `defaultMode` from the guardrail locks in the managed-settings documents | no — but 0.9 inherits it |
| 10 | State how components interact with placement | yes |

Our answers, in one line each: **O1** client mode. **O2** no offline governed runs, but never kill one in flight. **O3** dissolves under client mode — one writer. **O4** no default; derive eligibility from the grant set. **O5** no, drive-as-source does not serve us; option (ii) is the requirement.
