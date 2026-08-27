# UI sandboxes

`wardynd` can relay **one declared loopback port** inside a running sandbox — a
code editor, a dev server — to an operator's browser. It rides the same exec
lane the SSH gateway's `-L` forward already uses (`socat` over
`Runner.ExecStream`): there is no pod-IP or container-IP dial, no
`NetworkPolicy` change, and **no new network path out of the sandbox**. The
bytes travel control plane → substrate exec → container, exactly like an
attach.

The gateway is off by default. It exists only when `WARDYN_UI_SANDBOX_LISTEN`
is set — see [docs/ENV.md](ENV.md) for all three variables
(`WARDYN_UI_SANDBOX_LISTEN`, `WARDYN_UI_SANDBOX_ADVERTISE`,
`WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE`). With it unset there is no listener and no
new surface: the relay's cookie key is not even generated.

It listens on a **second address**, and boot refuses one equal to `-listen` (or
to `-ssh-listen`). That is a security control, not a deployment convenience:
everything the relay serves is the sandbox's **own** HTML and JavaScript, and
the only thing keeping that code away from the console's session storage and
admin actions is that it arrives on a different browser origin. See
[Bounds](#bounds) for what that separation does and does not buy.

> **Not available on the desktop tier (0.7).** The relay is built and tested,
> but no `agent-vscode` or noVNC image is published, and a managed laptop has no
> repo and no build path — `wardyn-desktop.sh` runs `--no-build` on purpose. So
> `deploy/desktop/wardyn.env*.example` ship `WARDYN_UI_SANDBOX_LISTEN` commented
> out rather than publishing a port with nothing to serve. This lane works on a
> **developer checkout** (`make agent-images`, then `make test-e2e-ui-sandbox`).
> Publishing the UI images is deferred to 0.8; see
> [docs/DESKTOP.md](DESKTOP.md) "Named gap: the browser lane".
>
> The **one-line installer** (`install.sh`) is the same: it writes
> `WARDYN_UI_SANDBOX_PORT` but leaves the listener off, for the same reason.

## 1. Declare the app

A run's policy names the apps the gateway may relay —
[`ui_apps`](POLICIES.md#ui_apps--uiapp), operator-authored, capped at 8:

```json
{
  "ui_apps": [
    { "name": "vscode", "port": 8080, "path": "/" }
  ]
}
```

An app is a **name, a loopback port and a path — never a command string**, so a
policy can never choose what runs inside the sandbox. The relay serves **only**
a declared port; anything else is refused naming this field, and
[`ssh -L`](SSH.md#4--l-port-forwarding) stays the escape hatch for an
undeclared one. Declaring an app grants nothing on its own: the gateway still
has to be enabled, and every session still needs a ticket.

## 2. Ship a launcher in the image

Wardyn starts a declared app by exec'ing a **convention path** inside the
sandbox — `/usr/local/bin/wardyn-ui-<name>` — on demand, with no arguments and
no tty. `deploy/images/vscode/` is the worked example: `code-server` on
`127.0.0.1:8080` (`--auth none`, safe *because* nothing else can reach that
port), built by `make agent-image-vscode` and registered like any other image:

```sh
export WARDYN_AGENT_IMAGES='{"vscode":"wardyn/agent-vscode:local"}'
```

Your own image serves an app the same way — see
[Image contract](#image-contract-byoi).

## 3. Open an app

Two calls: mint a single-use attach ticket (the **same** ticket the browser
terminal uses — there is no second ticket type), then `GET` the enter URL on
the UI origin.

```sh
TICKET=$(curl -sf -X POST "$WARDYN_URL/api/v1/runs/$RUN_ID/attach-ticket" \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" | jq -r .ticket)
xdg-open "$UI_ORIGIN/__wardyn/enter?run=$RUN_ID&app=vscode&ticket=$TICKET"
```

`/healthz` publishes the exact form as `ui_sandbox.enter_url_template`, with
`{run}`, `{app}` and `{ticket}` placeholders. Read the origin from there rather
than composing it — it is deliberately not the console's.

The enter endpoint consumes the ticket and then **re-checks everything the
ticket cannot prove on its own** against freshly-loaded state: owner-or-admin
for this run, the run still `RUNNING` with a sandbox, and the app actually
declared in the run's **effective** policy. Only then does it set the relay
cookie — `wardyn_ui_sess`, `HttpOnly`, `SameSite=Lax`, `Path=/r/<run-id>/`, 8h
— and `302` to `/r/<run-id><path>`. Every later request rides that cookie, and
nothing else on this listener authenticates anything.

### From the console

The run detail page's **UI apps** lane is the affordance for this flow: one row
per declared app, and an **Open** button that mints the ticket and opens the
app in a new tab. Its states and strings are frozen in
[design/ui-sandboxes-prompt.md](design/ui-sandboxes-prompt.md). The app is
never embedded in the console page — an `<iframe>` on the console origin is
precisely what the second listener exists to prevent.

## 4. Deployment

| Set | To |
|---|---|
| `WARDYN_UI_SANDBOX_LISTEN` | the gateway's own address, e.g. `:8081` — never `WARDYN_LISTEN`'s |
| `WARDYN_UI_SANDBOX_ADVERTISE` | the externally-reachable base URL, e.g. `https://wardyn-ui.example.com` (advisory copy; unset falls back to the raw bind address and warns) |
| `WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE` | per-run origin, e.g. `https://run-{run}.ui.example.com` — needs wildcard DNS and a wildcard certificate. **The documented default for a production deployment**; leave unset only for a single-tenant/demo install willing to accept the shared-origin residual below |

**One certificate.** The gateway serves TLS with the *same* `-tls-cert`/
`-tls-key` as the console, so a distinct hostname needs a certificate that
covers it too (host mode needs a wildcard).

**Shared origin vs per-run origin.** Without the origin template every run's
apps are served from one origin, separated only by the path-scoped cookie —
boot says so in a warning, `/healthz` publishes it as `ui_sandbox.host_mode`,
and it is a published residual
([THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) §5). With the template set,
each run gets its own browser origin and an enter served on any other host is
refused.

Kubernetes: `uiSandbox.enabled` in the Helm chart
([deploy/helm/wardyn/README.md](../deploy/helm/wardyn/README.md#ui-sandbox-gateway)).
Compose: the loopback mapping (`WARDYN_UI_SANDBOX_PORT`, default `8081`) is
always published, and the gateway stays off until `WARDYN_UI_SANDBOX_LISTEN` is
also set — a mapped port with no listener behind it is inert.

**Getting `wardyn/agent-vscode` onto a cluster is yours to do.** No release or
publish workflow pushes it — `make agent-image-vscode` builds it locally as
`wardyn/agent-vscode:local`, it is deliberately not part of `agent-images`
(~+228 MiB over the base), and nothing in `.github/workflows/` tags it to a
registry. On compose the local tag is enough; a cluster's nodes cannot see your
daemon, so either push it to a registry the nodes can pull from and name that
ref, or `kind load docker-image wardyn/agent-vscode:local` on a `kind` cluster.
Either way the ref reaches wardynd through `WARDYN_AGENT_IMAGES` — the
agent-name → image-ref JSON map, e.g.
`{"vscode":"registry.example.com/agent-vscode:0.6.0"}` — which the chart carries
as an ordinary entry under `env` (`env.WARDYN_AGENT_IMAGES`; values render
inline in the pod spec, which is fine for an image ref). There is no dedicated
chart value for it, and none of this is `vscode`-specific: it is the same path
any Bring-Your-Own-Image agent takes, which is why the launcher contract below
is the whole story about serving an app.

## Image contract (BYOI)

The gateway runs three things **inside the sandbox**, by convention, never by
reimplementing anything:

| Purpose | Binary | Invocation |
|---|---|---|
| Start the app | `/usr/local/bin/wardyn-ui-<name>` | no arguments, no tty, stdout/stderr discarded; must bind `127.0.0.1:<declared port>` within 20s |
| Probe + relay | `socat` | `socat -u /dev/null TCP:127.0.0.1:<port>` (probe), `socat - TCP:127.0.0.1:<port>` (relay) |
| Probe script | `sh` | one exec runs the whole probe-launch-poll cycle |

Wardyn's own build images (`deploy/images/`) carry `sh` and `socat`; only the
`vscode` variant ships a launcher, because the app name and port are policy-
authored per run, not fixed at wrap time. A **Bring-Your-Own-Image** run that
declares an app its image cannot start gets a **clean 502 naming the path**,
never a hang:

```
no UI launcher in this image: /usr/local/bin/wardyn-ui-vscode not found
```

A launcher that runs but never opens the port gets its own 502 naming the port
and the timeout. The full launcher contract (lifetime, `--help`, size delta) is
in [deploy/images/README.md](../deploy/images/README.md#launcher-contract-byoi).

## Recording

**Nothing you do inside a relayed app is recorded.** No keystrokes, no screen,
no page content, no request or response bodies. The relay is an HTTP proxy over
an exec, and none of the recording pipeline is on this path — no tmux session,
no PTY recorder, no `internal/secretmask` masking, no asciicast upload. What is
recorded is that a session *happened*, in the append-only audit log:

| Action | When | Data |
|---|---|---|
| `ui.auth` | every enter — success and every denial | app, port; or the denial reason |
| `ui.start` | the launcher was run and the app came up | app, port, launcher path |
| `ui.open` | a relay connection opened | app, port |
| `ui.close` | that connection closed | app, port, `duration_sec` |

These are deliberately **not** `session.attach`: there is no PTY and no holder,
and a relay session must never appear in the run detail page's Recording tab as
though a replay of it existed. If you need the content of a working session
recorded, use the terminal — browser attach or `ssh`, both of which are
recorded ([SSH.md](SSH.md#recording)). There is no "record my editor" mode, and
the gap is published as a residual rather than implied away
([THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) §5).

## Bounds

**Auth.** Exactly one mechanism on this listener: the ticket handoff, then the
signed relay cookie. It never falls through to the console's session cookie or
the admin bearer token, because the caller on this origin may be
sandbox-authored JavaScript. Every cookie failure — absent, malformed, forged,
expired — answers the same 403, so there is no oracle to probe.

**Ticket.** Single-use, 30s TTL, owner-or-admin at mint
(`POST /runs/{id}/attach-ticket`). A stale or already-redeemed ticket is a 403
with a `ui.auth` denial in the log.

**Only declared ports.** The port is captured from the effective policy when
the ticket is redeemed and lives in the signed cookie, so no later request can
name a different one; the dial target is re-verified against the session on
every connection.

**Header hygiene, both directions.** Cookies are not port-scoped, so a shared
hostname would otherwise hand console cookies to sandbox code: every forwarded
request has **all `wardyn_*` cookies**, `Authorization`, `Proxy-Authorization`
and any `?ticket` stripped, and every response has `Set-Cookie: wardyn_*`
dropped (cookie tossing). `X-Forwarded-*` is removed and deliberately not
re-added — the sandbox has no business learning the operator's IP — and every
gateway response carries `Referrer-Policy: no-referrer` so the enter URL's
ticket cannot leak to whatever the app links to.

**Liveness.** Every new connection reloads the run: a stopped or killed run
stops serving with a 409 naming its state, not a hang.

**Resource bounds.** At most 8 concurrent relay connections per run — each one
is a live `socat` exec in the sandbox, so this is the sibling of the SSH
gateway's per-run channel cap — and idle pooled connections are closed after
90s, so a closed browser tab does not keep opening new ones.

Closing a relay connection is not the same as ending the exec behind it, and
the difference is a stated residual, not a bug we hid: neither substrate offers
"kill this exec", so all Wardyn can do is close the exec's streams. `socat`
exits when it sees both halves end, and a `socat` whose app-side half is still
held open by the relayed app can therefore outlive its relay connection and be
reaped only when the sandbox stops. The connection cap bounds how many the
relay can be *using* at once; it does not bound how many linger, so a very long
editing session on an app with a generous keep-alive can leave a handful of
idle `socat` processes in its own sandbox. `scripts/run-e2e-ui-sandbox.sh`
asserts the bound this actually promises (20 relayed requests must not become
20 execs), and prints what remains rather than claiming zero.

**Idle auto-stop.** Relayed requests reset the run's idle clock (`TouchRun`,
debounced 30s), exactly like an attach keepalive: a human reading code in an
editor is not idle and the reaper must not stop the run under them.

**Shared origin.** In path mode (no origin template) every run's apps share one
origin, separated by the path-scoped cookie. That is a real residual, published
in [THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) §5 — set
`WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE` where wildcard DNS is available.

## Native clients (exploratory)

This relay puts a sandbox's UI in a **browser**. Two questions follow it
around: what about a native IDE, and what about a desktop? Exactly one of them
has an answer today, and this section exists so the other one is not mistaken
for one. **Nothing in the table below is built, scheduled, or tested** —
[ROADMAP.md](../ROADMAP.md)'s v0.6 entry calls a general native lane
*exploratory*, and this is what that word means.

**What exists today: VS Code Remote-SSH**, over the SSH gateway, with one
client setting ([SSH.md §5](SSH.md#5-vs-code-remote-ssh)). That is the whole
native story. It needs no relay, no `ui_apps` entry and no second listener —
it is the SSH gateway doing what SSH does.

**What a further native lane would have to solve.** Each candidate is listed
with the work it implies, not with a plan:

| Candidate | What it would take | Where it stands |
|---|---|---|
| JetBrains Gateway | Gateway's normal flow has the **remote** host download a multi-gigabyte IDE backend; the sandbox's only egress is wardyn-proxy under the run's allowlist, so that download has nothing to reach — the same problem `remote.SSH.localServerDownload` solves for VS Code, but with a much larger artifact and no equally simple client switch. A backend baked into an image, or pushed over the existing SSH transport, is the shape it would take | Nothing built. No Wardyn image carries an IDE backend, and no one has run the client against a sandbox to find out where it stops |
| RDP (xrdp) | An X session plus `xrdp` inside the image, and a TCP path for the native client. That path may already exist: [`ssh -L`](SSH.md#4--l-port-forwarding) carries arbitrary **loopback** TCP into the sandbox, so this is plausibly an image question, not a server one. X11 forwarding is refused outright by the SSH gateway, so `-X` is not the route | Nothing built. No image ships an X session, and the desktop's audit/recording story is unwritten |
| Xpra | Same image problem, smaller: a rootless X server and per-app windows instead of a whole desktop, reached the same way (`-L`, or xpra's own ssh transport) | Nothing built |
| Browser desktop (VNC/noVNC) | Not a native lane at all — noVNC on a declared loopback port is an **image variant on this relay**, with no server change. It is the cheapest of the four for that reason | Nothing built; the primitive it would ride is the one documented above |

**Decision criteria.** Before any of these becomes work, all of the following
have to hold — they are the same properties that made the browser relay
shippable:

1. **The two lanes that exist cannot serve the demand.** Remote-SSH covers a
   remote-capable editor; the relay covers anything that speaks HTTP on
   loopback. A third lane needs a use those two genuinely fail.
2. **No new network path and no new listener.** It rides the exec lane or the
   SSH gateway's existing forward, exactly as everything here does — no
   pod-IP/container-IP dial, no `NetworkPolicy` delta, L0 intact.
3. **The recording sentence stays literally true.** Content inside a relayed
   or forwarded app is not recorded ([Recording](#recording)); a new lane must
   get its own distinct audit actions and its own honest bound, never a quiet
   reuse of `session.attach` that would imply a replay exists.
4. **Image cost is opt-in and pinned.** Its own `make` target, out of the
   default image, with the size delta published the way
   [`deploy/images/README.md`](../deploy/images/README.md) publishes
   `vscode`'s.
5. **Client setup fits in one documented block.** One setting, like
   `remote.SSH.localServerDownload`. A lane that needs per-user hand-holding
   is a support burden, not a feature.
6. **BYOI fails closed and says why.** A missing binary produces a clean error
   naming the path, like `sftp-server`/`socat` do today
   ([Image contract](#image-contract-byoi)) — never a hang.

Criterion 1 is the one that has not been met, which is why this is a section
and not a milestone.

## Internals

The gateway is `internal/api/uigateway.go` plus `execconn.go` — one
`httputil.ReverseProxy` whose `DialContext` returns an `execConn`, a `net.Conn`
over a `runner.ExecSession`, so the standard library handles the WebSocket
`101` upgrade code-server needs without a second protocol implementation.
Policy validation is `validateUIApps` (`internal/api/policy.go`), applied
wherever a policy enters — stored, inline, or `WARDYN_DEFAULT_POLICY`. A run's
declared apps are resolved from the `run.policy.effective` audit envelope, not
from `policy_id`: an inline or default policy has no row to fetch, and
resolving through the id would hand an inline-policy run the default policy's
apps.
