# SSH gateway

[Watch — Audit & attach (2:00–2:30)](README.md)

`wardynd` can serve native SSH directly into a running sandbox's tmux
session — the same one the browser terminal (run detail's "Live terminal" /
`wardyn attach`) shows. It authenticates registered **public keys only** (no
passwords) and is **owner-or-admin**: a human may SSH into a run they
created, or — if their key was registered while they held the admin role —
into anyone's. That admin half is a bounded-stale stamp, not a live role
check; see [Bounds](#bounds) for the ceiling that comes with it.

The gateway is off by default. It exists only when `WARDYN_SSH_LISTEN` is
set — see [docs/ENV.md](ENV.md) for both variables (`WARDYN_SSH_LISTEN`,
`WARDYN_SSH_ADVERTISE`). With it unset there is no listener and no new attack
surface: the daemon does not even generate a host key.

## 1. Register a public key

**SSO deployment (OIDC configured):** Account menu → **SSH keys** → **Add
key** — paste your public key (the console never asks for a private key; the
paste field's own helper line says so, and pasting one is refused
server-side with a specific error). A key registered against your SSO
session lands under your OIDC `sub` — the only principal the gateway's
owner check (below) will ever match against a run you created.

**Admin-token / no-SSO / CI deployment only** — the bearer-token curl below
registers the key against the shared, non-human `admin-token` principal, not
any human's own identity. With OIDC configured, `POST` from a bare admin
token now 422s for exactly this reason instead of silently writing a key
that can never authorize anyone's run — use the console (above) instead:

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

## 2. Connect

The run detail page's "Attach from your terminal" card shows the exact
command for a run you own while it is RUNNING:

```sh
ssh <run-id>@<advertise-host> -p <port>
```

Or let the CLI assemble it: `wardyn ssh <run-id>` reads the gateway's
address off `/healthz` and execs your local `ssh(1)` against it, so there is
no connect string to copy. `wardyn ssh --print <run-id>` emits that command
instead of running it (for a script or a demo) and `--config` emits the
`ssh_config` block below — both byte-identical to what the card renders. It
is a separate command from `wardyn attach`, deliberately: `attach` carries
the admin bearer over a WebSocket, `ssh` carries your registered public key
over the real SSH protocol.

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
```

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

**Masking scope, stated plainly.** `internal/secretmask` masks values it was
told about — platform-managed secrets and minted credentials registered into
it at run start. A value a human **types** into the shell — pastes an API key,
exports a token by hand — is not in that registry and is never masked: it
lands in the recorded asciicast verbatim, permanently, subject to whatever
retention window `WARDYN_RECORDING_RETENTION_DAYS` is set to (default:
forever). There is no route to delete or redact one recording in isolation
once it exists; the only lever is the age-based retention sweep, which acts
on all eligible recordings, not one. If a human types a secret into an SSH (or
browser-attach) session, treat that recording as holding it in the clear until
retention deletes it.

## Bounds

**Auth.** Registered public keys only — no password, no keyboard-interactive.
`MaxAuthTries` is bounded per connection; an unknown key or a malformed
username (anything that isn't a run id) is rejected and audited (`ssh.auth`,
`outcome=failure`), so a scan against the gateway leaves a trail.

**Owner-or-admin, and the admin half is a bounded-stale stamp — weaker than
the web terminal's, but no longer unboundedly so.** SSH authorization is
`run.created_by == the key's registered principal`, OR the key's `role`
column (migration `0043`) reads `admin` AND its `role_checked_at` (migration
`0046`) is no older than `WARDYN_SSH_ROLE_TTL` (default `24h`). A member's key
never satisfies the override — only the owner check does, same as before. The
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
their own next login (re-stamping `role=member`), `role_checked_at` aging past
`WARDYN_SSH_ROLE_TTL` (the TTL bites even if they never log in again), or the
key being deleted/re-registered.** An operator who wants the override gone
immediately (rather than waiting out the TTL, or waiting for the demoted human
to log in) has the same lever as before: delete that principal's key
(`DELETE /me/ssh-keys/{fingerprint}`, self-service only — there is no admin
view of another human's keys, so this means asking them, or an operator with
direct store access, to remove it). Re-registration (delete, then re-`POST`)
still works too, and still re-stamps immediately; it is no longer the ONLY way
to force a refresh, just the immediate one that does not wait on either a
login or the TTL. There is still no in-place "update this key's role"
endpoint.

**Upgrading from 0.5 (or from pre-`0046`): your existing key is a `member`
key, and even an `admin`-stamped key loses the override until it is
refreshed.** `role` is stamped at registration, and migration `0043`
backfilled every pre-0.6 row as `member` — the fail-closed value, because
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
visible without reading the database: Settings → SSH keys badges the row
**Admin override**. That badge reflects the STORED `role` only — it does not
currently show whether `role_checked_at` has aged past `WARDYN_SSH_ROLE_TTL`,
so a badged key can still be refused by the gateway once its stamp goes stale;
the audit log (`ssh.auth`, `outcome=failure`, reason "admin override stale")
is the authoritative signal for that, not the badge. It is still a
self-service view only — there is no console listing of another human's keys,
for the same reason the API has none.

An override connection is audited distinctly: the `ssh.auth` success event
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
held `-L` forward ALL keep the run's idle clock reset (`TouchRun`) for as
long as they're open — a long `scp`, a slow `ssh run 'make build'`, or a
tunnel held open in another terminal is exactly as protected as the
interactive shell is. `auto_stop_after_sec` governs an SSH session
identically to any other activity, on every channel kind, not just the
shell.

**Env allowlist.** A non-interactive `ssh <run-id>@host <cmd>` forwards only
`TERM`/`LANG`/`LC_*` from the client's environment into the exec — nothing
else the client's shell happens to export reaches the sandbox.

**Audit actions**: `ssh.auth` (every attempt, including failures),
`session.attach` with `transport:ssh` in its data (the shell path — same
action name the browser terminal uses, so both show up together in a run's
timeline), `ssh.exec` (`argv`, `exit`), `ssh.sftp` (`bytes` transferred),
`ssh.forward` (`port`, `bytes`). This is the source of record for these four;
[`docs/AUDIT-ACTIONS.md`](AUDIT-ACTIONS.md) is the vocabulary reference for
every other audit action in the system and points back here for these.

## Migration & internals

Registered keys live in `ssh_public_keys` (migration `0032`), keyed by the
SHA256 fingerprint (computed server-side from the parsed key — a caller
cannot choose or forge one). The gateway and the `/me/ssh-keys` REST surface
are both in `internal/api` (`sshgateway.go`, `sshgateway_channels.go`,
`sshkeys.go`), next to `attach.go` — the SSH shell path bridges through the
exact same `Runner.Attach` call and masked recorder the browser terminal
uses, so there is only one interactive-shell code path to keep correct, not
two that could drift.
