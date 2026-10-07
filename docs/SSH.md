# SSH gateway

`wardynd` can serve native SSH directly into a running sandbox's tmux
session — the same one the browser terminal (run detail's "Live terminal" /
`wardyn run attach`) shows. It authenticates registered **public keys only** (no
passwords) and is **owner-only**: a human may SSH into a run they
created. A super admin's key reaches a run only when it has no personal owner
(an operator-owned service or local run); it opens no shell in a person's run
(`run_owner_only`, #1476). That admin carve-out is a bounded-stale stamp, not a
live role check; see [Bounds](#bounds) for the ceiling that comes with it.

The gateway is off by default. It exists only when `WARDYN_SSH_LISTEN` is
set — see [docs/ENV.md](ENV.md) for both variables (`WARDYN_SSH_LISTEN`,
`WARDYN_SSH_ADVERTISE`). With it unset there is no listener and no new attack
surface: the daemon does not even generate a host key.

> **Recording ceiling, and 0.7 makes it fleet-wide.** The desktop envelope now
> ships `WARDYN_SSH_LISTEN` ON, so every managed laptop runs this gateway. Two
> halves, in opposite directions:
> `ssh` **exec** output and **sftp** payloads are **not recorded** (and sftp
> uploads are not byte-counted), so work done over those paths leaves no
> session evidence — do not present Remote-SSH as the recommended developer
> path without saying so. The interactive SSH **shell** *is* recorded through
> the browser terminal's same masking pipeline, but an unregistered secret can
> remain in cleartext. There is **no delete-one route**; age-based retention
> (default: keep forever) is the removal mechanism. See [Recording](#recording).

## 1. Register a public key

**SSO deployment (OIDC configured):** User view sidebar → **Your account**
(`/account`) → **Add key** — paste your public key (the console never asks
for a private key; the paste field's own helper line says so, and pasting
one is refused server-side with a specific error). A key registered against
your SSO session lands under your OIDC `sub` — the only principal the
gateway's owner check (below) will ever match against a run you created. A
key added here is capped: it never carries the admin override, even for a
super admin — see [Bounds](#bounds).

**An admin key no longer reaches other people's runs.** Admin view → Settings →
**Admin SSH keys** (super admins only) still registers a key stamped `admin`,
but that stamp now opens only runs with no personal owner. There is no
break-glass key into a person's run: until the owner-consented support session
(#1509) a super admin who needs one asks its owner. Kill, approve, policy,
grants, revoke and audit stay with the admin.

**Admin-token / no-SSO / CI deployment only** — the bearer-token curl below
registers the key against the shared, non-human `admin-token` principal, not
any human's own identity. With OIDC configured, `POST` from a bare admin
token now 422s for exactly this reason instead of silently writing a key
that can never authorize anyone's run — use the console (above) instead. A
key already stored under a reserved principal (`admin-token`, the local
operator seat, a `device:` name) is refused at the gateway while OIDC is
configured; without OIDC it keeps working:

```sh
curl -sf -X POST "$WARDYN_URL/api/v1/me/ssh-keys" \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" \
  -d '{"name":"laptop","public_key":"'"$(cat ~/.ssh/id_ed25519.pub)"'"}'
```

The key is scoped to **your own principal** — there is no admin view of
another human's keys, and `DELETE` on a key you don't own 404s exactly like a
nonexistent one (no existence leak). The fingerprint returned (and shown in
the console) is the `SHA256:…` form `ssh-keygen -lf` prints; it is **public
by design** — it identifies the server/key, it authenticates no one, so
showing it is not a disclosure.

Registration is first-come, first-served, max `20` keys per principal, and a
duplicate add (`POST` of a key already registered — by you or by someone
else) 409s with a deliberately generic message: it does not confirm the key
exists under a DIFFERENT account, only that this exact `POST` didn't take.

### Reclaiming a squatted fingerprint

> **0.6, migration `0046`:** a direct-SQL registration that sets `role='admin'` must ALSO set
> `role_checked_at = now()`, or the gateway refuses the override as never-checked
> (`admin override stale`). The API registration path stamps it for you.

The fingerprint primary key is **global** — correct for auth, since a key
must map to exactly one principal, never two. That means it is also, by
construction, possible for someone else to register a public key you also
hold before you do (e.g. a key whose public half you've posted somewhere,
like a GitHub profile), after which your own `POST` 409s indefinitely — the
API never confirms who holds it, so there is no self-service resolution.
An operator can free the slot at the database directly, once the rightful
owner is verified out-of-band:

```sql
DELETE FROM ssh_public_keys WHERE fingerprint = 'SHA256:...';
```

The freed fingerprint can then be re-registered by anyone — including,
again, whoever squatted it — so pair this with actually identifying who the
key belongs to, not just running the query.

### Revoking access during an incident

A per-user API token can register an SSH key through `wardyn ssh-key ensure`.
Deleting that token alone leaves the key registered. Revoking the person's
sessions with `wardyn session revoke --sub '<subject-or-email>'` also revokes
their API tokens and removes their registered SSH keys. `--all` applies those
three actions deployment-wide, including the calling admin's credentials.
A registration already in flight cannot escape that cutoff: registration time
is stamped before reading the request body, and SSH authentication and new
channels check it against session revocation. A fresh sign-in can register a
new key after the cutoff; revocation does not disable the account.

For incident response or offboarding:

1. Prevent further sign-in or key registration through the deployment's
   identity/access controls.
2. Revoke the person's sessions and check the result. A `500` can mean the
   named session cutoff succeeded but a token, canonical-subject cutoff or
   SSH-key operation failed; the
   `session.revoke` audit records `tokens_revoked` and `ssh_keys_deleted` for
   the completed work. Resolve the failure and retry.
3. To remove only SSH keys, an admin or `security_admin` can call
   `DELETE /api/v1/people/{principal}/ssh-keys`. Percent-encode the principal
   as one path segment. The route resolves a known subject or email and
   returns `200` with `{"count": N}`. An unresolved email or ambiguous name is `422`;
   a lookup or deletion failure is `500`. Owners can still remove individual
   keys through **Your account** (`/account`), `wardyn ssh-key delete <fingerprint>`,
   or `DELETE /api/v1/me/ssh-keys/{fingerprint}`.
4. End existing access with `wardyn run kill <run-id>` and verify teardown
   succeeded. Include operator-owned runs reached through an admin override. Key
   deletion prevents new authentication and new `session` or `direct-tcpip`
   channels on an established connection; an already-open shell, transfer or
   forward continues until it closes or the run is torn down.
5. Erase the person's stored credentials with
   `DELETE /api/v1/people/{principal}/credentials` and follow the
   [workspace and drive offboarding procedure](OPERATIONS.md#multi-user-who-can-change-what).
   Disabling sign-in and deleting SSH keys do not erase stored model or forge
   credentials or reclaim workspace data.

The gateway rechecks the authenticated registration before each new channel.
A missing, changed, cut-off or unreadable key is refused; an unreadable
session cutoff also refuses access. Registering the same public
key again does not restore an old connection. Admin override role, cap and
freshness checks apply at this point too, and so does the owner rule: an
override connection on a run that is not operator-owned is refused. `WARDYN_SSH_ROLE_TTL`
still bounds only the admin override (now the operator-owned carve-out), not
access to runs the key's principal owns.

The `ssh_key.add`, `ssh_key.delete`, `session.revoke`, `ssh.authenticate` and
`ssh.channel.reject` events identify registrations, completed revocations and
refused access; see [Audit actions](AUDIT-ACTIONS.md).

## 2. Connect

The run detail page's "Attach from your terminal" card shows the exact
command for a run you own while it is RUNNING:

```sh
ssh <run-id>@<advertise-host> -p <port>
```

Or let the CLI assemble it: `wardyn run ssh <run-id>` reads the gateway's
address off `/healthz` and execs your local `ssh(1)` against it, so there is
no connect string to copy. `wardyn run ssh --print <run-id>` emits that command
instead of running it (for a script or a demo) and `--config` emits the
`ssh_config` block below — both byte-identical to what the card renders. It
is a separate command from `wardyn run attach`, deliberately: `attach` mints a
single-use ticket with your configured token (`WARDYN_TOKEN` or
`WARDYN_ADMIN_TOKEN`) and carries it over a WebSocket — the same door the
console's own terminal uses, so a member needs no admin credential to attach
to a run they own — `ssh` carries your registered public key over the real
SSH protocol instead.

`<run-id>` **is** the SSH username — the gateway has no session cookie to
carry it any other way, so the run id is the addressing, the same way a
hostname addresses a machine. `<advertise-host>` is whatever the operator set
`WARDYN_SSH_ADVERTISE` to (shown on `/healthz`); it is advisory copy only —
changing it does not move the listener.

Or drop this in `~/.ssh/config` (the card's collapsible block, copyable
as-is):

```
Host wardyn-<short-id>
  HostName <advertise-host>
  Port <port>
  User <run-id>
  ProxyCommand <proxy-command>
```

The `ProxyCommand` line is there only when the operator set
`WARDYN_SSH_PROXY_COMMAND` (shown on `/healthz` as `ssh.proxy_command`; see
[SSH on a 443-only estate](#ssh-on-a-443-only-estate)). With it, `--print`
emits `ssh -o ProxyCommand='<proxy-command>' <run-id>@<advertise-host> -p <port>`,
`--json` carries it as `proxy_command`, and `wardyn run ssh` connects through
it only with `--advertised-proxy`: it is a command from the deployment that
runs on your computer, so without the flag the CLI shows it and stops.

**Verify on first connect**: the card also shows the host key fingerprint
(`ED25519 SHA256:…`). Check it against what your client prompts before
trusting a new host, same as any SSH server — the fingerprint is
`ssh-keygen`'s own presentation, nothing custom.

A stopped or SSH-disabled run shows no card at all — there is nothing to
connect to, and no "try anyway" affordance that would just fail.

### From a cluster

Nothing above changes when wardynd runs in Kubernetes — the gateway is the
same listener, and the client commands are identical. What the operator owes
is a route to it and an address to advertise. Four pieces, all documented
where they are implemented:

- **Getting traffic in.** `ssh.enabled` adds the SSH port to wardynd's
  *existing* Service, so by default SSH inherits whatever exposure HTTP has.
  `make kind-quickstart` publishes it as a NodePort and prints the ready-made
  `ssh -p 2222 <run-id>@127.0.0.1` line — POC-grade, single command
  ([chart README, Quickstart](../deploy/helm/wardyn/README.md#quickstart)).
  To expose SSH differently from the console — its own `LoadBalancer` while
  HTTP stays internal `ClusterIP` — the chart README carries a minimal
  bring-your-own Service targeting the same pods
  ([Split SSH exposure](../deploy/helm/wardyn/README.md#split-ssh-exposure)).
- **The address clients are told to use.** `ssh.advertiseHost`
  (`WARDYN_SSH_ADVERTISE`) is what the run-detail card and `/healthz` print.
  It is advisory copy only — the gateway binds `WARDYN_SSH_LISTEN`, not this
  — but in a cluster the bind and the reachable address always differ, so an
  unset value hands every user a `127.0.0.1` that is not theirs. The chart
  never guesses it; see
  [`values.yaml`'s `ssh` block](../deploy/helm/wardyn/values.yaml) and
  [ENV.md](ENV.md).
- **The host key across pod churn.** There is no host key in the chart and no
  volume for one: wardynd generates an ed25519 key on first boot and persists
  it in the secret store, so the fingerprint your users pinned survives a
  rolling upgrade — *as long as the age key does*. Lose the age key and the
  pod crash-loops before the gateway listens, which is a connection refused
  rather than a silently changed fingerprint. See OPERATIONS,
  ["The SSH host key survives restarts"](OPERATIONS.md#the-ssh-host-key-survives-restarts--because-the-age-key-does).
- **Caveat, unchanged by the substrate.** `sftp` and `-L` exec binaries inside
  the *sandbox*, not in wardynd's pod, so a BYOI run still needs
  `sftp-server`/`socat` — see [Image contract](#image-contract-byoi). A
  cluster install does not supply them on the image's behalf.

### SSH on a 443-only estate

Some estates let nothing in but port 443, terminated by a TLS-terminating
listener in front of the cluster (an Istio ingress gateway, say). The SSH
gateway's listener is plain TCP, so `ssh` cannot reach it there as it is. The
recipe wraps SSH in TLS on each person's computer and lets the listener
unwrap it:

1. The listener terminates TLS on 443 under a hostname of its own and passes
   the bytes, as plain TCP, to the Service's `ssh` port.
2. `ssh.proxyCommand` (`WARDYN_SSH_PROXY_COMMAND`) tells each person's `ssh`
   to open that TLS connection first. The run-detail card and
   `wardyn run ssh` carry it, so the copy-paste command works.

In your own values file (Istio installed from its own charts; the Wardyn
chart ships no Istio template, and `extraObjects` keeps these objects beside
your values instead of in a fork of the chart):

```yaml
ssh:
  enabled: true
  advertiseHost: ssh.example.com   # the listener's own hostname for SSH
  proxyCommand: openssl s_client -quiet -verify_return_error -verify_hostname %h -connect %h:443 -servername %h

networkPolicy:
  ingress:
    from:
      - podSelector: {}            # keep the same-namespace default peer
      - namespaceSelector:         # and admit the ingress gateway's namespace
          matchLabels:
            kubernetes.io/metadata.name: istio-ingress

extraObjects:
  - apiVersion: networking.istio.io/v1
    kind: Gateway
    metadata:
      name: wardyn-ssh
    spec:
      selector:
        istio: ingressgateway      # your ingress gateway pods' labels
      servers:
        - port:
            number: 443
            name: tls-wardyn-ssh
            protocol: TLS          # never HTTPS
          tls:
            mode: SIMPLE
            credentialName: wardyn-ssh-tls
          hosts:
            - ssh.example.com
  - apiVersion: networking.istio.io/v1
    kind: VirtualService
    metadata:
      name: wardyn-ssh
    spec:
      hosts:
        - ssh.example.com
      gateways:
        - wardyn-ssh
      tcp:
        - match:
            - port: 443
          route:
            - destination:
                host: '{{ include "wardyn.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local'
                port:
                  # Keep ssh.port above 1023: the gateway listens on 443 and
                  # wardynd cannot, and the chart refuses a lower port because it
                  # would roll out green with no listener behind it.
                  number: 2222     # ssh.port
```

- **`protocol: TLS`, not `HTTPS`.** `HTTPS` attaches an HTTP filter to the
  server and breaks SSH; `TLS` with `tls.mode: SIMPLE` terminates TLS and
  hands the `tcp:` route the raw stream.
- **The certificate.** `credentialName` names a `kubernetes.io/tls` Secret in
  the ingress gateway pods' namespace (not the Gateway object's), for the SSH
  hostname.
- **The NetworkPolicy.** Setting `networkPolicy.ingress.from` replaces the
  same-namespace default, so list it again; the ssh rule passes the other
  named peers through.
- **The proxy command.** `-servername %h` only sends SNI, which the listener
  routes on; `-verify_hostname %h` is what checks that the certificate names
  the host, and `-verify_return_error` makes a failed check end the
  connection. Port 443 is written out because `%p` expands to the advertised
  SSH port (the chart always renders `host:ssh.port`; the `-p <port>` on the
  card is harmless, the proxy command ignores it). When the listener's
  certificate is not from a publicly trusted CA, add `-CAfile <path>` naming
  a file every person keeps the CA at, e.g. `-CAfile ~/.ssh/wardyn-ca.pem`
  (ssh runs the command through the person's shell, so `~` expands). Each
  person needs `openssl` on their computer. The daemon never runs this value
  and refuses to boot on a control character, a newline, a single quote, or
  more than 512 bytes ([ENV.md](ENV.md)).
- **Host keys are unaffected.** The SSH handshake runs end to end inside the
  tunnel, so the fingerprint on the card is still the one to verify.
- **Audit source IPs become the listener's.** The gateway sees the ingress
  gateway pod's address, so `ssh.*` audit rows record that, not the person's.
- **Nothing else needs it.** The browser terminal and `wardyn run attach`
  already ride the console's own 443.

## 3. sftp

Native `sftp`/`scp` work unmodified:

```sh
sftp <run-id>@<advertise-host> -P <port>
scp ./local-file <run-id>@<advertise-host>:/home/agent/  -P <port>
```

The gateway runs the sandbox's **own** `/usr/lib/openssh/sftp-server -e` as
the subsystem's backing process — no SFTP protocol reimplementation on
Wardyn's side. See [Image contract](#image-contract-byoi) if this 404s.

## 4. `-L` port forwarding

```sh
ssh -L 8080:127.0.0.1:3000 <run-id>@<advertise-host> -p <port>
```

reaches port 3000 **inside the sandbox's own network namespace** via
`socat`, run the same way sftp is — the sandbox's own binary, no protocol
reimplementation. The destination is **restricted to the sandbox's own
loopback** (`127.0.0.1` / `::1` / `localhost`); anything else is refused
before any command runs, with a reason, because the sandbox has no other
egress to forward to regardless (L0 structural confinement — invariant 3 is
unaffected by this feature: no new network path is opened, forwarding rides
inside the existing sandbox network namespace). `-R` (remote/reverse
forwarding) and agent/X11 forwarding are refused outright — see
[Bounds](#bounds).

The forward dials `127.0.0.1` specifically (IPv4) — a service inside the
sandbox that binds only an IPv6 loopback (`::1`) or a v6-only wildcard is
not reached this way. Bind `127.0.0.1` (or `0.0.0.0`) for anything you want
to reach over `-L`.

## 5. VS Code Remote-SSH

Add the `Host` block above to `~/.ssh/config`, then Remote-SSH → Connect to
Host → pick it. **One setting is required first**, in VS Code's
`settings.json`:

```json
"remote.SSH.localServerDownload": "always"
```

**Why**: the sandbox has no internet — its only egress is the wardyn-proxy
sidecar under the run's own allowlist, which does not include
`vscode-cdn`/`update.code.visualstudio.com`/etc. Remote-SSH's default
behavior tries to have the **remote host** download its own server binary;
inside a Wardyn sandbox that download has nothing to reach and stalls or
fails. `"always"` makes your **local** VS Code fetch the server and push it
over the SSH connection instead — which is a normal file transfer over the
tunnel this gateway already provides, needs nothing from the sandbox's
egress policy, and works identically on every deployment regardless of what
that policy allows.

## 6. Other tools over SSH (scripted access)

Everything above assumes a human typing `ssh` or pasting a `Host` block by
hand. An external tool — an IDE or an agent workbench that wants to drive the
sandbox itself — needs the same three facts (host, port, username) without a
human copying them out of the console, plus one thing the console card never
had to solve: knowing when the sandbox is actually ready to open, not merely
RUNNING. Four commands, each doing one part:

```sh
wardyn ssh-key ensure                                    # once per machine

# --description "external:<tool>" is what makes the run page say tool-managed.
# This command prints the run, including its id.
wardyn run --agent claude-code --interactive --json \
  --description "external:<tool>" \
  --policy-file examples/policies/remote-workspace.yaml
wardyn run wait-ready <id> --json                        # -> {"workspace":{"vcs":"git","path":"..."}}
wardyn run ssh <id> --json                                   # -> {"host","port","username","host_key_fingerprint","command"}
```

**`wardyn ssh-key ensure`** (`cmd/wardyn/sshkey.go`) generates an ed25519
keypair at `~/.wardyn/id_ed25519` the first time it runs (`0600`, plus a
`0644` `.pub` sibling) and registers the public half via `POST
/api/v1/me/ssh-keys` unless its fingerprint is already on the account.
Idempotent, so a script can run it on every launch with no "already done"
branch to write — a second call neither regenerates the key nor re-registers
it. `--path` picks a different keypair; the default is deliberately **not**
`~/.ssh/id_ed25519` — a key an external tool dials sandboxes with should be
revocable from Your account (`/account`) without touching your everyday identity.

**`wardyn run wait-ready <run-id>`** (`cmd/wardyn/run_wait_ready.go`) blocks
past what `run --wait` waits for. `--wait` waits for a TERMINAL state (and is
refused outright on an interactive run, which never reaches one on its own);
`wait-ready` waits for the run to become **usable from outside**: RUNNING,
*and* its workspace readable through the same in-sandbox exec channel the
console's Files-changed widget polls (`GET /runs/{id}/files`,
[`internal/api/run_files.go`](../internal/api/run_files.go)). Those are
different moments — the workspace clone lands **after** the sandbox comes
up, so a tool that opens the instant the run turns RUNNING routinely finds an
empty directory. A run that names a repo (`--repo`, or a workspace whose
sources include a repo) automatically waits for `vcs:"git"`; pass `--expect-git`
to require that for a workspace-sourced run too. A terminal state reached
before ready fails fast — FAILED exits `1` (with the dispatch failure reason
when audit carries one, the same lookup `run --wait` uses), any other
terminal state exits `2` — and `--timeout` (default `5m`, must be positive)
exits `124` rather than hanging a script forever. The timeout bounds the
requests too, and does not stop the run: on a `124` the run keeps running and
holds its sandbox and credentials until it ends (see
[ci-jobs-as-runs.md](ci-jobs-as-runs.md), "On `124`, kill the run").

**`wardyn run ssh <run-id> --json`** (`cmd/wardyn/ssh.go`) is the same `/healthz`
read `--print`/`--config` use, shaped for a program instead of a terminal:
`{"host", "port", "username", "host_key_fingerprint", "command"}`. `port` is
**always populated** — `22` when the gateway's advertised address names none
— so a caller never has to reimplement ssh's own default the way a bare
`--print` command's absent `-p` flag implies it. `host_key_fingerprint` comes
straight off `/healthz` (the same `ED25519 SHA256:…` line the console card
shows) — verify it out-of-band the same way you would any new SSH host;
`wardyn run ssh` does not do that for you. `command` is the literal `ssh …`
invocation `--print` would emit, so a caller that just wants to shell out
rather than reimplement the client can.

**Never-reap and push namespace.** A session like this has no Wardyn-visible
activity between whatever the human or the tool's own agent does in that
tool's own UI, so set `auto_stop_after_sec: -1` — never `0`: both mean "never
reaped" to the reaper, but `-1` states the omission was deliberate on a run
like this, not a policy that simply forgot the field (see
[`examples/policies/remote-workspace.yaml`](../examples/policies/remote-workspace.yaml)).
If the tool's own agent commits and pushes under a branch name it picked
itself — not the `wardyn/<run-id>/*` namespace `agent-run` sets up — the
git-broker's default confinement refuses that push with no way to tell the
external tool why; `git_push_any_branch: true` is the per-run opt-out (see
[docs/POLICIES.md](POLICIES.md)'s `git_push_any_branch` row).

**Channel budget.** `session` (shell/exec/sftp) and `direct-tcpip` (`-L`
forwards) draw from the **same** per-run cap —
[`maxSSHSessionsPerRun`](../internal/api/sshgateway.go) (`4`, today) — so an
editor holding open a shell, an sftp session and two `-L` forwards has used
the whole budget; a fifth channel of any kind is refused, not queued.

**Under SSO, register the key as yourself.** Authorization is owner-only
(above): the run has to be created by the **same principal** that registered
the key, and a key registered with the deployment's admin token can never
satisfy that for a human's run — `POST /me/ssh-keys` 422s the attempt
outright once OIDC is configured ([§1](#1-register-a-public-key)). Point
`wardyn ssh-key ensure` and `wardyn run` at your **own** API token
(`WARDYN_TOKEN`) rather than `WARDYN_ADMIN_TOKEN`, or register the key from
the console instead of the CLI.

## Image contract (BYOI)

The gateway execs two binaries **inside the sandbox**, by convention, never
by reimplementing their protocols:

| Feature | Binary | Invocation |
|---|---|---|
| sftp subsystem | `/usr/lib/openssh/sftp-server` | `sftp-server -e` |
| `-L` forwarding | `socat` | `socat - TCP:127.0.0.1:<port>` |

Wardyn's own build images (`deploy/images/`) carry both. A **Bring-Your-Own-Image**
run that omits either gets a **clean channel error naming the missing
binary** — never a hang, never a bare connection reset — the moment that
specific feature is used; the interactive shell (plain `ssh` with no `-s
sftp`/`-L`) is unaffected either way, since it rides the existing Attach/tmux
path and touches neither binary. Install both if you want full parity with
the built-in images.

## Recording

Every SSH **shell** session (`ssh <run-id>@host`, no subsystem/forward) is
recorded exactly like the browser terminal: same tmux session, same masked
live recorder (`internal/secretmask`), same asciicast pipeline. It appears in
the run detail page's Recording tab with **zero extra UI** — the session
picker there already lists every attach session by its audit-trail key; an
SSH session's key is simply prefixed `ssh-` instead of a bare session UUID, so
it is distinguishable at a glance from a browser attach. `sftp`/`-L` traffic
is binary protocol data, not terminal output, and is never recorded (masking
and asciicast framing both assume text; recording binary transfer bytes
would neither work nor mean anything).

**Masking scope, stated plainly.** `internal/secretmask` masks values registered
for the run, including credentials minted while a session is attached. An
unregistered value a human **types** into the shell — pastes an API key,
exports a token by hand — can appear verbatim in the recorded terminal output,
subject to the retention window `WARDYN_RECORDING_RETENTION_DAYS` (default:
forever). There is no route to delete or redact one recording in isolation
once it exists; the only lever is the age-based retention sweep, which acts
on all eligible recordings, not one. If a human types an unregistered secret
into an SSH (or browser-attach) session, treat that recording as holding it in
the clear until retention deletes it.

**What is masked at all (0.8.6).** Only the **shell**. SSH **exec**
(`ssh <run-id>@host <command>`), **SFTP** and **direct-tcpip** (`-L`) were never
masked: their bytes go from the sandbox to your client through no masking
pipeline, so a registered secret a command prints reaches your terminal verbatim.
The shell itself now refuses to open, with an error line and an
`authz.denied` row (`mask_state_unavailable`, target `ssh.shell`), when this
server cannot prove the run's masking corpus complete: a run dispatched before
0.8.6 after a restart, or a run whose person is being erased. A shell already
open ends within about two seconds of that.

## Bounds

**Auth.** Registered public keys only — no password, no keyboard-interactive.
`MaxAuthTries` is bounded per connection; an unknown key or a malformed
username (anything that isn't a run id) is rejected and audited (`ssh.authenticate`,
`outcome=failure`), so a scan against the gateway leaves a trail.

**Owner-only, and the admin carve-out is a bounded-stale stamp — weaker than
the web terminal's, but no longer unboundedly so.** SSH authorization is
`run.created_by == the key's registered principal`, OR the run has no personal
owner (`operator_owned`) AND the key's `role` column (migration `0043`) reads
`admin` AND its `role_checked_at` (migration `0046`) is no older than
`WARDYN_SSH_ROLE_TTL` (default `24h`). A fresh admin key on a person's run is
refused with reason `run_owner_only`, at connect and again at every new channel
of an open connection. A member's key never satisfies the override — only the
owner check does, same as before. Existing admin-stamped keys are not revoked:
they keep working on the runs they own and on operator-owned ones. The
`role` column is stamped at `POST /me/ssh-keys` time, from the role the
registering session actually held **then** — but it is now also RE-stamped,
along with `role_checked_at`, on every OIDC login for that principal
(`oidc.Config.OnLogin`, wired to `RefreshSSHKeyRoles` in
`cmd/wardynd/boot_deps.go`): a live read of the human's current role, applied
to every key they hold, no re-registration required. The gateway still never
consults the human's role live at connect time — SSH carries no session for
`requireOperator` to read — so this stays **bounded-stale, not live**, unlike
the browser terminal's `requireOperator` gate, which reads the session's role
fresh on every attach. What bounds the staleness now: **a demoted admin's
already-registered key keeps its override only until whichever comes first —
their own next login (re-stamping `role=user`), `role_checked_at` aging past
`WARDYN_SSH_ROLE_TTL` (the TTL bites even if they never log in again), or the
key being deleted/re-registered.** An operator who wants the override gone
immediately can use `DELETE /people/{principal}/ssh-keys` as an admin or
`security_admin`, or revoke the person's sessions to remove their tokens and
keys together. Owners can remove individual keys through
`DELETE /me/ssh-keys/{fingerprint}`. Existing channels still require teardown
as described under [incident revocation](#revoking-access-during-an-incident).
Re-registration (delete, then re-`POST`)
still works too, and still re-stamps immediately; it is no longer the ONLY way
to force a refresh, just the immediate one that does not wait on either a
login or the TTL. There is still no in-place "update this key's role"
endpoint.

**The run's governance profile is a third door.** Even the owner is refused,
with an `ssh.authenticate` failure naming the profile, when the profile the run
was created under carries `deny_interactive`; only a super admin key is exempt,
and a profile that cannot be read refuses too. A limit set later reaches new
connections and does not end ones already open (see "Limits that reach a running
run" in [OPERATIONS.md](OPERATIONS.md)).

**A key registered in the user view is capped, for good.** An admin whose
console session is in the user view (member mode) can register a key; it is
stored with `capped = true` (migration `0070_ssh_key_view_capped`) and role
`user`. A capped key never gains the admin override: the sign-in re-stamp
(`RefreshSSHKeyRoles`) refreshes its `role_checked_at` but leaves its role
`user`, the database refuses a capped row that reads `admin`, and the gateway
refuses the override for a capped key before it reads the role. The refusal is
audited as `ssh.authenticate`, `outcome=failure`, reason "capped key (registered in the
user view): no admin override". The key still reaches its owner's own runs. An
uncapped admin key is registered outside the user view, from the Admin SSH keys
card (Admin view → Settings, super admins only — [§1](#1-register-a-public-key));
it reaches only runs with no personal owner. The `ssh_key.add` audit row marks a
capped key with `capped: true`.

**Upgrading from 0.5 (or from pre-`0046`): your existing key is a `user`
key, and even an `admin`-stamped key loses the override until it is
refreshed.** `role` is stamped at registration, and migration `0043`
backfilled every pre-0.6 row as `member` (`0074` renames it `user`) — the fail-closed value, because
nothing in the schema knows what role a pre-0.6 registrant actually held, and
guessing `admin` would hand every key already in the deployment a cross-user
reach it was never granted. Migration `0046` adds a second fail-closed
backfill on top: `role_checked_at` defaults `NULL` for every pre-existing row,
and `sshAuth` treats `NULL` as infinitely stale — so **an `admin`-stamped key
that predates `0046` has no override until its owner does ONE of two things:
log in again** (the ordinary path now — `oidc.Config.OnLogin` re-stamps both
`role` and `role_checked_at` for every key that principal owns, no
re-registration needed) **or `DELETE`/`POST` the key again** (still supported,
still immediate, useful when you want the refresh before your next login
rather than after). Which of your own keys carries the `admin` stamp is
visible without reading the database: Your account (`/account`) badges the
row **Admin override**. That badge reflects the STORED `role` only — it does not
currently show whether `role_checked_at` has aged past `WARDYN_SSH_ROLE_TTL`,
so a badged key can still be refused by the gateway once its stamp goes stale;
the audit log (`ssh.authenticate`, `outcome=failure`, reason "admin override stale")
is the authoritative signal for that, not the badge. It is still a
self-service view only — there is no console listing of another human's keys,
for the same reason the API has none.

An override connection is audited distinctly: the `ssh.authenticate` success event
carries `override:true` in its data whenever the owner check did NOT match
and the admin-role check is what let the connection through — so "who used
the override, and when" is a normal audit-log query, not something you have
to infer from `run.created_by` mismatches after the fact. Tracked as
threat-model residual #15 in
[../threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md), which now
documents the staleness ceiling above rather than the older "no override at
all" gap.

**Pre-auth listener DoS bounds.** `ssh.NewServerConn` blocks with no default
timeout, so an unauthenticated pre-auth connection is a real containment
surface, not a nuisance:

- a per-connection handshake deadline (cleared once auth succeeds — an
  established session is never killed by it);
- a bounded total concurrent-connection count (a connection over the cap is
  closed immediately, before any handshake byte is exchanged);
- a small, documented cap on concurrent SSH channels — `session` (shell/
  exec/sftp) AND `direct-tcpip` (`-L` forwards) draw from the SAME per-run
  counter — so one run cannot exhaust the daemon's own resources by opening
  unbounded parallel shells OR unbounded forwards.

**Idle auto-stop.** A shell, a running `exec`, an open sftp subsystem, and a
held `-L` forward ALL reset the run's idle clock when they open and every 30
seconds while they are open — a long `scp`, a slow `ssh run 'make build'`, or a
tunnel held open in another terminal is exactly as protected as the
interactive shell is. So an attached run is not stopped by
`auto_stop_after_sec`, on every channel kind, as long as those writes succeed
(they are best effort, and a failed one is dropped). A run nobody is attached
to is idle like any other: with the shipped default policy a session's `-1`
becomes the member ceiling of 3600, so a run left detached for an hour is
stopped (`run.autostop`). That hour is the ceiling, not a lease. To lengthen it,
raise `auto_stop_after_sec` in the ceiling; a ceiling of `0` removes the cap but
also turns idle stop off for every run that does not set its own. Idle pause,
the lease (`ends_at`) and `WARDYN_RUN_MAX_AGE` are separate clocks: see
[Run lifetime](operations/run-lifetime.md#the-clocks-that-end-or-freeze-a-run).

**Env allowlist.** A non-interactive `ssh <run-id>@host <cmd>` forwards only
`TERM`/`LANG`/`LC_*` from the client's environment into the exec — nothing
else the client's shell happens to export reaches the sandbox.

**Audit actions**: `ssh.authenticate` (every attempt, including failures),
`session.attach` with `transport:ssh` in its data (the shell path — same
action name the browser terminal uses, so both show up together in a run's
timeline), `ssh.exec` (`argv`, `exit`), `ssh.sftp.transfer` (`bytes` transferred),
`ssh.forward` (`port`, `bytes`). This is the source of record for these four;
[`docs/AUDIT-ACTIONS.md`](AUDIT-ACTIONS.md) is the vocabulary reference for
every other audit action in the system and points back here for these.

## Migration & internals

Registered keys live in `ssh_public_keys` (migration `0033`), keyed by the
SHA256 fingerprint (computed server-side from the parsed key — a caller
cannot choose or forge one). The gateway and the `/me/ssh-keys` REST surface
are both in `internal/api` (`sshgateway.go`, `sshgateway_channels.go`,
`sshkeys.go`), next to `attach.go` — the SSH shell path bridges through the
exact same `Runner.Attach` call and masked recorder the browser terminal
uses, so there is only one interactive-shell code path to keep correct, not
two that could drift.
