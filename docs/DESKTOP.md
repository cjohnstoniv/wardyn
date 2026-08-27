# Desktop tier: a governed agent daemon on a managed laptop

This is the deployment shape where Wardyn runs **on the developer's own
machine** — one local daemon per laptop — and the organization manages the
*envelope* it runs inside (policy, agent images, audit destination) through the
MDM it already uses to manage the laptop.

It is a configuration, not a build. Every variable in
[`deploy/desktop/wardyn.env.example`](../deploy/desktop/wardyn.env.example)
already exists and is already documented in [ENV.md](ENV.md).

Sibling shapes, for contrast: [the compose stack](../deploy/compose/README.md)
(one host, demo/single-tenant) and
[the Helm chart](../deploy/helm/wardyn/README.md) (a cluster, multi-user, SSO +
RBAC, no shared docker socket). This tier sits below both.

## Topology

```
       MDM (Jamf / Intune / …)
                │  renders + re-asserts 4 files per device
                ▼
  ┌─────────────────────────────────────────────┐
  │  the developer's laptop                     │
  │                                             │
  │   /etc/wardyn/wardyn.env       (envelope)   │
  │   /etc/wardyn/secret.env       (secrets)    │
  │   /etc/wardyn/policy.json      (ceiling)    │
  │   /etc/wardyn/site-config.json (proxy/SCM)  │
  │   /etc/wardyn/age.key   ← installer-minted, │
  │                            NEVER from MDM   │
  │                    │                        │
  │                    ▼                        │
  │   wardynd  ── 127.0.0.1:8080 ──▶ console    │
  │      │        (local mode: no SSO,          │
  │      │         the developer is admin)      │
  │      │                                      │
  │      ├──▶ agent sandbox container           │
  │      └──▶ wardyn-proxy sidecar ──▶ egress   │
  │                                             │
  └──────────────────────┬──────────────────────┘
                         │ audit fanout (webhook, ?device=<serial>)
                         ▼
                     org SIEM
```

Three properties define it:

- **A local daemon per laptop.** No shared control plane, no cluster. The
  daemon, its Postgres, its sandboxes and its proxy sidecars all live on the
  one machine. Nothing about one developer's runs is visible to another.
- **An org-managed envelope, delivered by MDM.** The four files above are
  rendered onto the device by the same management plane that already ships
  configuration profiles. Wardyn does not know MDM exists; it reads an env
  file, a policy file and a site-config file, exactly as any other deployment
  does.
- **The developer is the operator.** `WARDYN_LOCAL_MODE=true` means there is no
  SSO and no bearer token, and local-mode callers are *always* admins
  (`Server.requireOperator`'s own doc states this: "Admin-token and local-mode
  callers are ALWAYS admins"). One human, their own machine, admin on their own
  daemon.

## The ceiling

**The developer is not the adversary in this tier.**

Everything below follows from that sentence, so it is worth being blunt about
what it does and does not claim.

**What the tier is for.** The agent is the thing being governed. An agent that
runs unattended, reads a workspace, calls out to the network and edits code is
a new and fairly wide surface on a machine that already holds credentials. This
tier gives the organization a real answer for that surface — a policy ceiling
the agent runs under by default, a pinned set of agent images, one egress route
through `wardyn-proxy`, a recording of the session, and an audit trail that
leaves the laptop.

**What the tier is not.** It is not a control against the person holding the
laptop. They are root on it. They can edit `/etc/wardyn/wardyn.env`, replace
`policy.json`, stop the daemon, or simply run the agent CLI directly with no
Wardyn at all. MDM re-asserts those files on its own schedule; it does not
prevent an edit in between, and re-asserting a file cannot un-run a run.

If your threat model *does* include the developer, this tier is the wrong one —
the agent has to execute somewhere the developer does not administer, which is
[the Kubernetes shape](../deploy/helm/wardyn/README.md), where the runner talks
to an API server under scoped RBAC and the human gets a member role rather than
admin.

See also [the threat model](../threatmodel/THREAT-MODEL.md) for what Wardyn as
a whole does not defend against.

## Tamper posture, stated honestly

There is one bypass worth naming explicitly, because it needs no root, no file
edit and no MDM race — it is an ordinary, documented, supported API call.

**A run may carry an `inline_policy`, and an admin's is not clamped.**
`Server.resolveRunPolicy` (`internal/api/inline_policy.go`) clamps a **member's**
`inline_policy` to the operator's `DefaultPolicy` via `composer.Clamp`, so a
member can never request wider egress, a lower confinement class or grant kinds
the operator did not allow. An **admin** is deliberately left unclamped —
admins are the ceiling-setting authority, and that is correct on every other
tier.

On this tier the developer *is* that admin. So:

> `WARDYN_DEFAULT_POLICY` is the ceiling for the developer who does not go out
> of their way. It is not a ceiling for the developer who does.

A run created with an `inline_policy` sets its own policy, through the normal
console/API path, without touching a single MDM-managed file.

**What still holds when that happens:**

- **It is on the record.** The unclamped inline spec is written to the audit
  feed as `policy.inline`, followed by `run.create`
  ([AUDIT-ACTIONS.md](AUDIT-ACTIONS.md)) — and `WARDYN_AUDIT_SINKS` fans both to
  the org SIEM, tagged with the device serial and the operator principal. A
  developer who widens their own ceiling produces evidence that they did, on a
  machine they cannot retroactively edit the org's copy of.
- **Egress still goes through the proxy.** The sandbox's only route off its
  per-run network is `wardyn-proxy`, whatever the policy says is allowed
  through it. Widening the allowlist is visible in the decision log; there is
  no policy value that removes the sidecar.
- **The session is still recorded.**

So the honest summary is: this tier converts "an agent is running loose on a
corporate laptop" into "an agent is running inside a declared envelope, and
every departure from that envelope is attributable." That is a governance
control, not a containment boundary against the operator, and it should be sold
as the first thing and never as the second.

### Where those three claims stop being true

Each bullet above has an edge. Read them before quoting the summary to an
auditor.

- **"Produces evidence that they did" — unless the evidence is dropped or
  self-approved.** The webhook sink is **at-most-once past a 4096-event
  buffer**: on a laptop that sleeps and flies, drops surface only on `/metrics`
  (nothing scrapes a laptop) and as a warning in the desktop log. A disk spool
  that drains on recovery is the at-least-once upgrade and is **deferred**.
  Separately, a member can **self-approve** their own `wait_for_review` egress
  request unless `WARDYN_EGRESS_SECOND_HUMAN` is set — and that check is
  **bypassed by the `admin-token` principal**, which on this tier is MDM-held
  automation. It constrains humans in the console, not the management plane.
- **"Egress still goes through the proxy" — for the SANDBOX.** When a run
  declares `ui_apps`, the relayed app's JavaScript executes in the *operator's
  own browser*, which Wardyn does not confine at all: that traffic is outside
  `wardyn-proxy` and outside any egress policy. The relay is **off by default
  on this tier** (see the envelope) precisely so this is an opt-in, but it stops
  being true the moment it is enabled.
- **"The session is still recorded" — the SHELL is; exec and sftp are not.**
  Turning on `WARDYN_SSH_LISTEN` makes both halves fleet-wide: `ssh` exec output
  and sftp payloads are **not** recorded and sftp uploads are not byte-counted,
  while the interactive SSH shell **is** recorded, unmasked, with **no
  delete-one route** — a secret pasted into a recorded terminal is stored in
  cleartext, permanently. And a `wardynd` restart mid-run (an MDM upgrade
  window, a crash, a laptop waking) wipes the in-memory masking snapshot, after
  which the stream passes through **unmasked** with a `success` audit event,
  because nothing in that path can tell "no secrets for this run" from "not my
  run".

Three more the tier inherits rather than introduces: the SSH gateway's admin
override is a **bounded-stale 24h role stamp**, never a live check; whoever
`POST`s a given SSH public key **first owns that fingerprint forever**, and the
only remediation is operator-side and out of band — which on m′ the developer
explicitly is not; and compose serves the console and the UI relay on a
**shared origin**, a documented residual of this topology.

### Named gap: the browser lane is not available on this tier

`wardyn ssh` works on a managed laptop as of 0.7. The **browser** half of
UI-in-container does not, and the envelope ships `WARDYN_UI_SANDBOX_LISTEN`
commented out rather than pretending otherwise.

The reason is images, not code. The relay itself is built and tested, but no
`agent-vscode` or noVNC image is published — `release.yml`'s matrix is
`wardynd`, `wardyn-proxy`, `agent-base`, `agent-codex-cli`, `agent-aws-sso` —
and `deploy/images/vscode/Dockerfile` builds `FROM wardyn/agent-claude-code:local`,
itself unpublished. A managed laptop has no repo and no build path: the launcher
runs `--no-build` specifically so it "refuses to fall back to building from
source on a laptop with no repo checkout". So enabling the listener here would
publish a port with nothing to serve.

It works today on a **developer checkout** (`make agent-images` then
`make test-e2e-ui-sandbox`). Publishing the UI images is deferred to 0.8.

## The member-mode profile (topology m′)

Everything above describes **topology a′: the developer is the operator**. It is
the default and it is honest about its ceiling — the person at the keyboard sets
the policy that bounds them.

Some deployments cannot accept that. If the box is org-managed *and* the
developer must not be able to reconfigure their own sandbox governance, run the
**member-mode profile**: the same daemon, the same MDM envelope, but the
governance authority is **elsewhere** — an org IdP and MDM-managed config — and
the developer is a **member**.

**What changes.** Three settings, and one derived invariant:

| Setting | a′ (default) | m′ (member mode) |
|---|---|---|
| `WARDYN_LOCAL_MODE` | `true` — loopback callers are always admins | **`false`**, mandatory. Local mode bypasses public-API auth and would hand the developer admin outright |
| OIDC | absent | **required** — the org IdP authenticates the developer and `deriveRole` maps them to `member`. `WARDYN_OIDC_ROLE_MAP` / `WARDYN_OIDC_OPERATOR_EMAILS` are MDM-set, and the developer is on neither |
| `WARDYN_ADMIN_TOKEN` | not used | a **process credential** MDM injects and the developer does not read. It is never surfaced to the browser UI |
| `WARDYN_MEMBER_MODE` | unset | **`true`** — asserts the above rather than enforcing anything new |

The invariant the whole profile turns on is: **`isOperator(ctx)` is false for the
developer's every request.** `WARDYN_MEMBER_MODE` adds no middleware — the
admin/member split in `internal/api` already does the enforcement — it makes the
assumption *checkable*, refusing to boot when local mode is on or OIDC is
unconfigured, either of which would silently make the developer an admin again.
See `validateMemberModePosture` (`cmd/wardynd/boot_posture.go`).

**What the developer can still do.** Use the product: launch and kill runs,
onboard workspaces they OWN, read what they own, author a *clamped* inline
policy. `workspaces.owned_by` (migration `0048`) makes ownership real — CRUD,
scan and build on their own workspaces, another member's answering the
byte-identical 404 a missing one does. What they cannot do is anything that
widens an egress ceiling, binds credential material, or writes the host: those
`/workspaces` routes, policy CRUD, secret writes and `PUT /site-config` all stay
admin-only. See [OPERATIONS.md § Multi-user](OPERATIONS.md#multi-user-who-can-change-what).

**Mounting their own project directory.** The one power m′ adds that no other
tier has is a NON-operator naming a host bind source. It is bounded by
operator/MDM-set env, never by anything the developer writes:

| Variable | What it bounds |
|---|---|
| `WARDYN_MEMBER_WORKSPACE_ROOTS` | the absolute host directories a member's `local_dir` source may resolve into. **Unset = members may not mount host directories at all** (repos and operator-owned workspaces still work) |
| `WARDYN_MEMBER_WORKSPACE_ROOTS_MAP` | per-member roots, JSON `{"<principal>": ["/abs/root"]}`. An entry **REPLACES** the shared list for that principal — per-member exists to narrow |
| `WARDYN_MEMBER_WRITABLE_ROOTS` | where a member may mark their own mount writable. **Unset = every member mount is read-only** |
| `WARDYN_MEMBER_WRITABLE_DENY` | carve-outs from the line above. **Deny wins**, and is checked first |

Point the roots at a dedicated projects directory. **Never `$HOME`, never `/`** —
boot warns and starts anyway (a malformed root, by contrast, refuses boot), and a
root that wide leaves the credential-dotfile deny-list as the only thing between
a member and `~/.ssh`. [ENV.md](ENV.md) carries the full semantics.

**Offboarding.** A departed member's owned workspaces point at an identity
nobody can sign in as. `POST /workspaces/{id}/reassign` (admin-only, idempotent)
returns each to the operator and audits `workspace.reassign` naming the
`from_owner`.

**The ceiling, restated for m′.** Member mode narrows the API surface the
developer reaches; it does not change who owns the laptop. They are still root
on it: they can edit `/etc/wardyn/wardyn.env` and restart the daemon in local
mode, at which point they are the operator again — MDM re-asserts the file on
its own schedule and the change is on the record, but nothing prevents the
window. So m′ buys **a governance boundary that holds for a developer who does
not go out of their way, and an audit trail for one who does** — the same shape
of promise as a′, drawn one tier tighter. If your threat model genuinely
includes the developer, the agent has to execute somewhere they do not
administer; that is [the Kubernetes shape](../deploy/helm/wardyn/README.md), not
this one. Residuals #25–#27 in
[the threat model](../threatmodel/THREAT-MODEL.md) state the member-mount and
admin-access limits verbatim.

## The MDM file table

| File | Owner | Mode | Contents | Why |
|---|---|---|---|---|
| `/etc/wardyn/wardyn.env` | **MDM** | `0644` | the non-secret envelope — `WARDYN_LOCAL_MODE`, `WARDYN_LOCAL_OPERATOR`, `WARDYN_DEFAULT_POLICY`, `WARDYN_AGENT_IMAGES`, `WARDYN_LISTEN`, `WARDYN_RUNNER`, `WARDYN_WORKSPACES_ROOT` | fleet-uniform, non-sensitive; readable is fine and makes support tractable |
| `/etc/wardyn/secret.env` | **MDM** | `0600` | secret-bearing variables — `WARDYN_AUDIT_SINKS` (its JSON carries the SIEM `bearer_token`), and `WARDYN_OIDC_CLIENT_SECRET` on the SSO variant | these are org credentials, uniform across the fleet, so MDM is the right delivery path — but they are not per-device secrets and `0600` does not make them ones |
| `/etc/wardyn/policy.json` | **MDM** | `0644` | the default `RunPolicySpec` — confinement class, allowed egress, eligible grant kinds ([POLICIES.md](POLICIES.md)) | this file *is* the managed ceiling; it is the reason the tier is called managed |
| `/etc/wardyn/site-config.json` | **MDM** | `0644` | corporate network facts — upstream proxy, artifact mirrors, SCM hosts (`wardyn site-config apply`) | environment-shaped, identical across the fleet, and re-applied after a reset |
| `/etc/wardyn/age.key` | **the installer, on the device** | `0600` | the age X25519 identity backing this laptop's secret store (`WARDYN_AGE_KEY`) | **never via MDM** — see below |

### Why `age.key` never rides in an MDM payload

`WARDYN_AGE_KEY` decrypts the secret store on the device. Minting it once,
locally, with `wardynd -gen-age-key` and writing it `0600` gives every laptop a
key that exists in exactly one place. Pushing it from MDM instead would give
the management plane one key that opens every laptop it was pushed to — and it
would sit in an MDM payload database, a config-profile export and a backup, all
of which have a wider audience than the device does.

The failure mode of the per-device key is that losing a laptop loses that
laptop's stored secrets. That is the intended cost, not a gap to design around.

A note on the mechanism: wardynd reads the key as a **value** in
`WARDYN_AGE_KEY`, not as a path — there is no `WARDYN_AGE_KEY_FILE`. So the
installer writes `/etc/wardyn/age.key` and whatever launches wardynd reads that
file into the variable. Leaving the variable unset is not a safe default: the
daemon then generates an **ephemeral** key per boot and fails closed on
anything persisted under the previous one.

### Posture switches are env vars, never site-config

Anything that changes the *security posture* of this deployment belongs in
`wardyn.env`, not in `site-config.json`. Site-config is applied as a
**full-document replace**, so a partial write silently drops whatever the
previous document held. Keep it to corporate network facts — proxy, mirrors,
SCM hosts — and keep posture in the env file, where a missing line is a missing
line and not a reverted setting.

## The install lane

Five files, all under [`deploy/desktop/`](../deploy/desktop/):

| File | Role |
|---|---|
| [`install.sh`](../deploy/desktop/install.sh) | Run once per device, as root (an MDM package's postinstall step, or by hand for a pilot). Creates `/etc/wardyn`, mints `age.key` if one doesn't already exist (`wardynd -gen-age-key`, `0600`, never overwritten), and registers the platform's converge job — [`com.wardyn.daemon.plist`](../deploy/desktop/com.wardyn.daemon.plist) with launchd on macOS, `wardyn.service` + `wardyn.timer` with systemd on Linux — pointed at `wardyn-desktop.sh` wherever the installer bundle sits on disk. `--uninstall` reverses it (keeping `age.key` and the database); `--uninstall --purge` destroys both. |
| `com.wardyn.daemon.plist` | The launchd `LaunchDaemon`. Runs `wardyn-desktop.sh up` at load and every 5 minutes after (`StartInterval`) — the same "re-assert, don't assume" posture MDM uses for the files it owns, not a foreground process launchd has to keep alive (`wardynd`'s own container carries `restart: unless-stopped`; this job's only work is making sure the *stack* is up). |
| [`wardyn.service`](../deploy/desktop/wardyn.service) + [`wardyn.timer`](../deploy/desktop/wardyn.timer) | The systemd analogue. `Type=oneshot` driven by the timer — `wardyn-desktop.sh up` converges and exits, exactly as the launchd job does, so a `Restart=` would fight the timer. `OnBootSec` mirrors `RunAtLoad` and `OnUnitActiveSec=300s` mirrors `StartInterval`; the two platforms must not drift, and `scripts/test-desktop-profile.sh` asserts they do not. Logs to journald rather than a file, which is where a Linux operator looks and which rotates on its own. |
| [`wardyn-desktop.sh`](../deploy/desktop/wardyn-desktop.sh) | What the plist actually runs. Reads the envelope out of `/etc/wardyn`, brings up [`deploy/desktop/docker-compose.yaml`](../deploy/desktop/docker-compose.yaml) (which `include:`s the same [compose stack](../deploy/compose/README.md) every other single-host deployment uses, and exports `WARDYN_MANAGED_DIR=/etc/wardyn` so that stack's own read-only mount gives `WARDYN_DEFAULT_POLICY` sight of the managed policy file), waits for `/healthz`, and idempotently applies `site-config.json` if MDM has delivered one. |

Both platforms ship. `install.sh` branches on `uname -s`: the macOS path is
unchanged, and the Linux path installs the systemd unit + timer. They are
genuinely different files rather than one portable script because the
divergence is not cosmetic — `chown root:wheel` is a **hard failure** on Debian
and Ubuntu, which have no `wheel` group, under `set -euo pipefail`.

Everything the plist and the wrapper do is exercised, machine-verifiable and
daemon-free: `scripts/test-desktop-profile.sh` (wired into `make test-scripts`)
checks the envelope parses, every variable it sets is a real documented one,
the policy path and the compose mount agree, the plist is valid XML, and — where `systemd-analyze` is present — the rendered systemd units verify.
`.github/workflows/ci.yml`'s `desktop-envelope` job goes further and actually
boots the compose profile with this commit's example envelope, then proves the
three things this document claims: `/policies/default` really does serve the
managed file, a run naming no policy really does resolve to that ceiling, and
a synthesized profile really is clamped to it (see "Tamper posture" above for
what "clamped" does and does not mean once the caller is an admin).

## Model access on m′

On the member-mode profile three shipped mechanisms compose into what looks
like a dead end, and it is worth walking because the obvious reading is wrong:

1. m′ makes **OIDC mandatory**.
2. With OIDC configured, `wardynd` **refuses subscription injection** — a shared
   subscription credential would serve other people's runs, which the harness
   vendor's terms prohibit.
3. Secret writes on m′ are **admin-only**, so the developer cannot
   `wardyn secret set anthropic-api-key` for themselves, and a member's own
   inline `api_key` grant is dropped unless the operator eligible-listed that
   exact {host, secret} pairing.

The daemon's own refusal message names the way out, and it is easy to read only
the first half of it: *"Give each user their own API key … **or use Bedrock**"*.

**Bedrock is the working path on m′, and it needs no member secret write.**

It is **daemon-level, MDM-set** configuration rather than a per-member
credential, so it routes around the wall above entirely:

| Variable | Set by |
|---|---|
| `WARDYN_BEDROCK_MODEL` | the envelope (a cross-region inference-profile id or an application-inference-profile ARN — **not** a bare foundation-model id) |
| `WARDYN_BEDROCK_REGION` | the envelope |
| `bedrock-api-key` secret | the **operator**, once, via the admin token |

With those three in place a member's run resolves Bedrock at dispatch. The
credential is never resident: a Bedrock API key is a static `Authorization`
header, so the proxy TLS-MITMs `bedrock-runtime` and injects it, and the
sandbox holds only a placeholder — the same trust parity as the api-key and
subscription lanes. No member grant, no workspace requirement, and nothing that
`filterMemberGrants` can drop.

**Constraint:** Bedrock resolution is scoped to the `claude-code` agent. A
member running `codex-cli` on m′ still needs an operator-provided OpenAI
credential.

**Rejected on the record:** `ROADMAP.md`'s alternative — re-running the
provider-convention model grant *after* `filterMemberGrants`, so a member's own
key survives with no integration behind it — is a real fix for **pure-BYOK for
members**, which is a different flow. It is not needed to give an m′ fleet model
access, and it reopens the secret-exfil guard `filterMemberGrants` exists for
(a member pairing an arbitrary stored secret with an allowlisted host). Left as
a Named gap for whoever wants BYOK-for-members specifically.

## Operational hygiene

**Log rotation.** The LaunchDaemon appends stdout *and* stderr to one
`/var/log/wardyn/desktop.log` every 300s forever, and compose sets no
`max-size` — nothing bounded it. `install.sh` now lays down
`/etc/newsyslog.d/wardyn.conf` (macOS). Linux logs to journald, which rotates
itself; the shipped `logrotate` fragment is only for an operator who has
redirected the converge job's output to a file.

It keeps **seven generations, not one**, deliberately: the audit-drop counter
above surfaces *only* as a warning in this file, because nothing scrapes
`/metrics` on a laptop. Rotating aggressively would destroy the evidence that
the SIEM fanout dropped events — on the tier that sells recorded evidence.

**Fleet posture.** There is no posture endpoint and none is needed:
`wardyn support-bundle` already collects it. Schedule it from MDM and collect
the file:

```sh
# MDM-scheduled, e.g. daily. The admin token is MDM-held; the developer never
# reads it, and on m′ they could not use it anyway.
wardyn support-bundle -o "/var/log/wardyn/support-$(date +%F).tar.gz"
```

**Leaked sandboxes.** A run row that is terminal but still carries a sandbox ref
can leave that sandbox running. The **boot** reconciler already tears this down
(its label-keyed sweep plus the store-state verdict), so what was missing was
not another boot pass but an **on-demand** one — for the shape a laptop actually
produces: suspend for a week, wake with dead sandboxes, never reboot, so no boot
pass ever runs:

```sh
curl -fsS -X POST -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" \
  http://127.0.0.1:8080/api/v1/admin/sandboxes/sweep
```

**Decision, recorded:** the on-demand route, **not a ticker, and not a second
boot pass**. A ticker was rejected on cost — the sweep calls `ListRuns` unpaged
and probes every terminal run carrying a ref, so it grows with run history
forever and would additionally need leader election. A second boot pass was
rejected on evidence: wiring it there makes the existing reconciler tear the
same sandbox down **twice**, which its own test asserts against. MDM can
schedule the route like the support bundle.

> ⚠️ **A restart mid-run can write live credentials into the recording, in
> cleartext, with a `success` audit event.** A single `wardynd` restart — an MDM
> upgrade window, a crash, a laptop waking — wipes the in-memory secret-masking
> snapshot. The masking writer then **passes the stream through unmasked**,
> because nothing in that path can distinguish *"no secrets for this run"* from
> *"not my run"*. This tier maximises the trigger: a 300s converge timer, an
> upgrade model that is a daemon restart, and a machine that sleeps. Weigh it
> against the recorded-evidence claim above before selling recordings as
> tamper-evident. Fixing it means failing closed on an empty snapshot for a
> non-terminal run, and is not done.

## The laptop is sometimes offline

A managed laptop lives in airplanes and coffee shops. Four things reach the
network; here is what each does when it cannot.

| Site | Offline behaviour |
|---|---|
| The converge job's image pull | **Fine.** It runs `--pull missing`, so an image already on the box is used as-is. (It used to be `--pull always` under `set -euo pipefail`, which killed the launcher and left the stack **down even though every image was local**.) |
| OIDC discovery at boot (m′ only) | **Fails boot, loudly, inside a 30s budget — and that is correct.** See below. |
| First-device enrolment (`install.sh`) | **Needs the network, once.** It mints `age.key` by running `wardynd -gen-age-key`, so it needs that image. This is inherent: enrolment cannot complete offline. Pre-seed the image, or enrol on-network. |
| Audit fanout to the SIEM | **Drops past the buffer.** At-most-once beyond 4096 events; see the ceiling above. This is the one that loses evidence rather than recovering. |

**Why the IdP case is not a bug.** On m′, OIDC is the only authentication, so a
daemon that came up *without* a working authenticator would be serving
authenticated routes with nothing behind them. `wardynd` therefore fails boot
rather than degrading. It is not stranded: `wardynd`'s container carries
`restart: unless-stopped`, and the converge job re-asserts every 300s, so the
retry loop already exists **one layer up** — and it is fail-closed by
construction, because a daemon that never finishes booting cannot serve
anything. It recovers on its own when the network returns.

What was missing was not retry but **diagnosability**: the launcher said only
*"wardynd did not become healthy"*. It now prints the daemon's last lines and
names this cause first.

**Three different "offline"s, because they fail differently.** A blackholed
host fails fast; an *unreachable* IdP is the quick case. A **captive portal**
is the slow one: DNS resolves and the connection hangs, so each attempt spends
the whole 30s budget. A laptop with no link at all still has loopback and the
Docker bridge, so the stack itself comes up — only the outward-facing
dependencies fail. When testing this, use the mode that matches the site:
reachable-but-hanging is the one that exercises the boot budget.

## Which Docker socket

**This is the tier's likeliest install failure, and it is invisible in CI.**

The converge job runs as **root** — a LaunchDaemon on macOS, a system systemd
unit on Linux — because it reads `/etc/wardyn/age.key` at `0600`. But Docker
Desktop, Colima, rootless Docker and Podman all expose a **per-user** socket.
Auto-detection shells `docker context inspect`, which as root reads *root's*
contexts, not the enrolled user's. CI's daemon is root-reachable, so the
question never arises there.

| Runtime | Root-reachable? | Set `WARDYN_DOCKER_SOCK`? |
|---|---|---|
| Docker Engine (Linux, default) | yes (`/var/run/docker.sock`) | no |
| Docker Desktop (macOS, default socket) | yes | no |
| Colima | **no** — `~/.colima/<profile>/docker.sock` | yes, absolute path |
| Rootless Docker / Podman (Linux) | **no** — `/run/user/<uid>/docker.sock` | yes, absolute path |

`wardyn-desktop.sh` reads `WARDYN_DOCKER_SOCK` **from the envelope** — so MDM
carries it to every device — and it takes precedence over auto-detection. If
nothing resolves, it **refuses to start and prints what it tried**, rather than
converging against the wrong daemon or hanging. That refusal is deliberate: a
converge that "succeeds" against a daemon the developer never uses is worse
than one that fails.

**Decision, recorded:** the unit is **system-scope**, matching the macOS
LaunchDaemon. A user-scope unit would resolve the per-user socket for free, but
it cannot read `/etc/wardyn` at `0600`, and MDM enrolment targets a device
rather than a login session. Fleets on a per-user runtime set one envelope
variable instead.

## Upgrade, rollback, uninstall

**Upgrade is an MDM rewrite of two lines.** `wardyn.env` pins
`WARDYN_WARDYND_IMAGE` and `WARDYN_PROXY_IMAGE` by digest; push a new envelope
with new digests and the next timer tick brings the stack up on them. No tag
moves under the fleet, and the launcher runs `--pull missing`, so a laptop that
is offline keeps running what it already has instead of failing to start.

**Rollback is the previous digest** — push the old envelope back.

> ⚠️ **Rollback does not roll the database back.** There are **zero** down
> migrations (`ls internal/db/migrations | grep -c down` → 0) and no version
> guard, so an older `wardynd` starts against a schema a newer one has already
> migrated **forward**, unguarded. It generally serves, because migrations have
> been additive — but that is a property of the migrations so far, not a
> promise. Treat a rollback across a migration boundary as untested, and
> capture `wardyn support-bundle` before you do it.

**Stopping.** `wardyn-desktop.sh down` stops the stack and keeps everything.
`wardyn-desktop.sh down --purge` additionally destroys the Postgres volume:
every run, every recording, and the whole append-only audit log. Note the
LaunchDaemon (or systemd timer) re-asserts the stack every 300s, so a plain
`docker compose down` does not stick — unload the daemon first:

```sh
sudo launchctl bootout system/com.wardyn.daemon      # macOS
sudo systemctl disable --now wardyn.timer            # Linux
sudo /usr/local/lib/wardyn/deploy/desktop/wardyn-desktop.sh down
```

**Uninstall keeps your data unless you ask otherwise**, matching `dpkg`/`rpm`
convention: it stops the stack, unloads the daemon and removes the payload, and
leaves `/etc/wardyn/age.key` and the Postgres volume in place, so a re-install
recovers the device. `--purge` removes them.

> ⚠️ **`/etc/wardyn/age.key` deletion is terminal.** It is the only identity
> that can decrypt this device's secret store, it is minted per-device and
> never rides in an MDM payload, so no copy exists anywhere else. Removing it
> orphans every secret stored on that laptop, permanently. That is the intended
> property — losing one device costs one device — but it means `--purge` has no
> undo.

**The one-line install (`install.sh`) has the same lifecycle**, in `~/.wardyn`:
re-run the installer at a new version to upgrade (it rewrites the pins in place
and leaves your age key, admin token and ports untouched), `docker compose down`
to stop, and remove `~/.wardyn` yourself to uninstall — with the same warning,
since `~/.wardyn/.env` holds `WARDYN_AGE_KEY` in cleartext.

## Try it, once, on a real Mac

Everything above is verified against a compose stack on a Linux CI runner.
Nobody has run the installer against real launchd on a real Mac — that step is
still owed. Run this once on a macOS machine you're willing to have `sudo`
install a LaunchDaemon on, and paste back the output (not a summary of it):

```sh
git clone https://github.com/cjohnstoniv/wardyn.git && cd wardyn
sudo ./deploy/desktop/install.sh

# MDM hasn't delivered wardyn.env/secret.env/policy.json yet on a bare pilot
# box — stand in for it by hand, using the example + the same demo.json
# ceiling the compose stack itself falls back to:
sudo cp deploy/desktop/wardyn.env.example /etc/wardyn/wardyn.env
# Three expressions, not one. The example ships both image pins uncommented (a
# real fleet needs them), and their @sha256:REPLACE_ME placeholders resolve to
# nothing — so without these the daemon dies on an image pull and the proxy
# sidecar is unresolvable, on the one piece of REAL-HARDWARE evidence this tier
# has. Substitute the current release's digests, or a published tag while you
# are only smoke-testing.
sudo sed -i '' -e 's/\$UPN/you@example.com/' \
               -e 's|^WARDYN_WARDYND_IMAGE=.*|WARDYN_WARDYND_IMAGE=ghcr.io/cjohnstoniv/wardynd:0.6.4|' \
               -e 's|^WARDYN_PROXY_IMAGE=.*|WARDYN_PROXY_IMAGE=ghcr.io/cjohnstoniv/wardyn-proxy:0.6.4|' \
               /etc/wardyn/wardyn.env
sudo cp examples/policies/demo.json /etc/wardyn/policy.json

sudo launchctl kickstart -k system/com.wardyn.daemon
sleep 5
sudo launchctl print system/com.wardyn.daemon | head -20
curl -fsS http://127.0.0.1:8080/healthz && echo OK
# The API re-marshals types.RunPolicySpec, whose `omitempty` tags drop
# denied_domains/allowed_methods when [] and auto_stop_after_sec when 0 even
# though demo.json spells them out — so a raw diff is red on a healthy stack.
# NORMALIZE drops those three keys from both sides when they carry their zero
# value before comparing.
NORMALIZE='import json,sys
d = json.load(sys.stdin)
for k, zero in (("denied_domains", []), ("allowed_methods", []), ("auto_stop_after_sec", 0)):
    if d.get(k) == zero:
        d.pop(k, None)
print(json.dumps(d, sort_keys=True))'
diff <(curl -fsS http://127.0.0.1:8080/api/v1/policies/default | python3 -c "${NORMALIZE}") <(python3 -c "${NORMALIZE}" < /etc/wardyn/policy.json) || true
```

That last line should print no diff at all — the point is confirming a real
launchd job, on a real Mac, against a real Docker Desktop or Colima install,
actually brings the stack up and serves the managed policy (once normalized
for the three `omitempty` keys above, a real content difference still shows).
Anything else it prints (a launchd load failure, a Colima `WARDYN_DOCKER_SOCK`
miss, a healthz timeout) is exactly the gap this smoke run exists to find.
