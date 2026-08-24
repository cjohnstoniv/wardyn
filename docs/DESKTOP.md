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

Three files, all under [`deploy/desktop/`](../deploy/desktop/):

| File | Role |
|---|---|
| [`install.sh`](../deploy/desktop/install.sh) | Run once per device, as root (an MDM package's postinstall step, or by hand for a pilot). Creates `/etc/wardyn`, mints `age.key` if one doesn't already exist (`wardynd -gen-age-key`, `0600`, never overwritten), and registers [`com.wardyn.daemon.plist`](../deploy/desktop/com.wardyn.daemon.plist) with launchd at wherever the installer bundle happens to be sitting on disk. |
| `com.wardyn.daemon.plist` | The launchd `LaunchDaemon`. Runs `wardyn-desktop.sh up` at load and every 5 minutes after (`StartInterval`) — the same "re-assert, don't assume" posture MDM uses for the files it owns, not a foreground process launchd has to keep alive (`wardynd`'s own container carries `restart: unless-stopped`; this job's only work is making sure the *stack* is up). |
| [`wardyn-desktop.sh`](../deploy/desktop/wardyn-desktop.sh) | What the plist actually runs. Reads the envelope out of `/etc/wardyn`, brings up [`deploy/desktop/docker-compose.yaml`](../deploy/desktop/docker-compose.yaml) (which `include:`s the same [compose stack](../deploy/compose/README.md) every other single-host deployment uses, adding only the `/etc/wardyn:/etc/wardyn:ro` mount `WARDYN_DEFAULT_POLICY` needs to actually see the managed policy file), waits for `/healthz`, and idempotently applies `site-config.json` if MDM has delivered one. |

This is a macOS/launchd installer today; the Linux/systemd path described in
the topology diagram above is not built yet.

Everything the plist and the wrapper do is exercised, machine-verifiable and
daemon-free: `scripts/test-desktop-profile.sh` (wired into `make test-scripts`)
checks the envelope parses, every variable it sets is a real documented one,
the policy path and the compose mount agree, and the plist is valid XML.
`.github/workflows/ci.yml`'s `desktop-envelope` job goes further and actually
boots the compose profile with this commit's example envelope, then proves the
three things this document claims: `/policies/default` really does serve the
managed file, a run naming no policy really does resolve to that ceiling, and
a synthesized profile really is clamped to it (see "Tamper posture" above for
what "clamped" does and does not mean once the caller is an admin).

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
sudo sed -i '' 's/\$UPN/you@example.com/' /etc/wardyn/wardyn.env
sudo cp examples/policies/demo.json /etc/wardyn/policy.json

sudo launchctl kickstart -k system/com.wardyn.daemon
sleep 5
sudo launchctl print system/com.wardyn.daemon | head -20
curl -fsS http://127.0.0.1:8080/healthz && echo OK
curl -fsS http://127.0.0.1:8080/api/v1/policies/default | diff - <(cat /etc/wardyn/policy.json | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)))') || true
```

That last line is expected to differ in whitespace only — the point is
confirming a real launchd job, on a real Mac, against a real Docker Desktop or
Colima install, actually brings the stack up and serves the managed policy.
Anything else it prints (a launchd load failure, a Colima `WARDYN_DOCKER_SOCK`
miss, a healthz timeout) is exactly the gap this smoke run exists to find.
