<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Per-user AWS SSO behind a corporate proxy: a blocking field report (0.7.6 / 0.7.7)

**Date:** 2026-09-19
**Assessed against:** `v0.7.7` (the regression is almost certainly the 0.7.6 change; 0.7.7 is
simply the release it was seen on)
**Reported by:** an operator running Wardyn on a private-endpoint Kubernetes estate

Environment-agnostic throughout: substitute your own for `CORP_PROXY_HOST:PORT` and the account and
role names.

---

## Deployment shape

Kubernetes runner, CC1/Fence, an agent roster with one enabled row — `claude-code` / `bedrock_sso` /
`per_user`, account and role pinned. `SiteConfig.upstream_proxy_url` is set to a corporate proxy,
because on this estate every AWS endpoint resolves into CGNAT (RFC 6598) and the direct path is cut
by a middlebox. `internal_hosts` lifts the private-IP guard for the AWS suffixes.

**This worked end to end on 0.7.5.**

## What they observe

A Claude Code run comes up, reports `Sonnet 5 · Amazon Bedrock`, and the first model call fails. The
agent shows:

```
API Error: SyntaxError: JSON Parse error: Unexpected identifier "llm"
Deserialization error: to see the raw response, inspect the hidden field {error}.$response
```

The run's own egress panel and audit trail show the real story — a tight loop, 20+ pairs in seconds:

```
03:36:26  run.llm.bedrock    mode: sso-inject-proxy
03:36:31  egress.allow  CORP_PROXY_HOST                        builtin:upstream-proxy
03:36:45  egress.allow  portal.sso.<region>.amazonaws.com      policy:allowed
03:36:46  egress.deny   portal.sso.<region>.amazonaws.com      builtin:dial-failed
03:36:46  egress.allow  portal.sso.<region>.amazonaws.com      policy:allowed
03:36:46  egress.deny   portal.sso.<region>.amazonaws.com      builtin:dial-failed
— repeating —
```

Every attempt is **allowed by policy and then fails to dial**.

## Their diagnosis

`mode: sso-inject-proxy` is Phase B: the SSO access token is no longer resident in the sandbox, the
proxy MITMs `portal.sso.<region>` and injects `x-amz-sso_bearer_token` on the wire. To do that the
proxy must terminate TLS and **re-originate** a connection to the real portal. On this estate that
re-originated connection cannot succeed directly — `portal.sso` resolves into CGNAT and only the
corporate proxy can reach it.

They conclude the MITM lane does not honour `SiteConfig.upstream_proxy_url` while the plain
CONNECT-tunnel lane does: before Phase B this path was a blind tunnel, which traversed the upstream
proxy and worked; Phase B replaced the tunnel with terminate-and-re-originate and, on this shape,
lost the proxy hop with it.

They rule out the SSRF guard: `internal_hosts` is unchanged from 0.7.5, the decision is
`builtin:dial-failed` and not `builtin:private-ip`, policy explicitly allows the host on every
attempt, and the identical deployment reached `portal.sso` on 0.7.5 when the lane was a blind tunnel.

## The three asks

**Ask 1 — the MITM's re-originated connection must traverse `upstream_proxy_url`.** Whatever the
plain tunnel lane does with it, the MITM lane needs to do too. An operator who has declared an
upstream proxy has declared how this deployment reaches the internet; a new lane that re-originates
its own connections cannot opt out of that and still work.

They note this is the **third** instance of one class of bug reported in a fortnight:

1. the daemon's own AWS SSO refresh dialled direct and failed (`EOF`) — fixed in 0.7.6 by
   `WARDYN_DAEMON_PROXY_URL`;
2. `upstream_proxy_no_proxy` existed because the proxy was all-or-nothing (a 0.6.x report);
3. now the MITM re-origination.

> Each time a NEW outbound path was added, it did not inherit the operator's proxy configuration. A
> checklist item for anything that dials — *"does this path honour `upstream_proxy_url`?"* — would
> have caught all three.

**Ask 2 — `builtin:dial-failed` should say why it could not dial.** It is emitted with no underlying
error. A connection refused, a DNS failure, a TLS handshake rejection and a proxy that would not
`CONNECT` are four very different operator actions and they all render identically. The proxy has
the error in hand at the point it gives up. The model to copy is the private-address denial, which
names the gate, says it sits below policy, and names the field to change *and its shape*. A
`dial-failed` that said *"could not dial portal.sso.<region>.amazonaws.com:443 — connection refused
(direct; no upstream proxy used)"* would have been self-diagnosing.

> We cannot read the proxy sidecar's logs from our position, so the final confirmation — the dial
> error itself — is not in our hands. If `builtin:dial-failed` carried the underlying dial error
> into the audit datum, this report would have taken ten minutes instead of an hour. That is a
> request in itself.

**Ask 3 — a proxy-level failure should not reach the agent as a JSON parse error.** A non-JSON body
is returned to an AWS SDK call that requires JSON, so the SDK reports a deserialization failure and
the real cause never surfaces. From the operator's seat this reads as an agent or model bug; nothing
in it points at egress, credentials, or a proxy. Two options, either acceptable: return a body the
AWS SDK will surface intelligibly (AWS's own error JSON shape, carrying Wardyn's reason in the
message), or have the agent-facing harness translate a Wardyn refusal into a sentence, as it already
does for the egress-approval case. The run panel got this right — the `deny` rows are visible and
correct. It is the in-terminal experience that misleads, and the terminal is where the person is
looking.

## Impact, and the stopgap they asked about

**Blocking.** Model access via per-user AWS SSO does not work on this deployment on 0.7.6/0.7.7.
Runs start, the agent comes up, and the first model call starves. Rolling back to 0.7.5 would
restore the working lane (`mode: sso-inject`, token resident in the sandbox) at the cost of Phase B's
non-residency — a security regression they would rather not take unilaterally.

They asked whether `WARDYN_AWS_SSO_PROXY_INJECT=off` is the sanctioned stopgap, and deliberately did
not flip it first: it trades non-residency away, and that is a posture change they wanted recorded
rather than done quietly to unblock a test.

---

## Maintainer note — what the code says, and what is still open

Recorded here because the report's central inference is not supported by the code, and the
difference decides which fix is the right one.

**The MITM lane does consult the upstream proxy.** `serveMITMRequest` resolves its dial target
through `egressTarget`, which under a configured upstream deliberately returns the **hostname** so
the corp proxy resolves and dials; the forward then runs over the shared transport whose
`DialContext` chains `CONNECT` through that proxy for every host not on `upstream_proxy_no_proxy`.
`internal/egress/proxy/upstream.go`'s own comment says every forward-egress dial is included, "the
MITM LLM path" named explicitly.

**`builtin:upstream-proxy` is emitted once per run, at proxy construction — not once per dial.** So
"used exactly once and never again" is not something that datum can show; its presence proves only
that an upstream *was configured* for that run. (The console compounds this by rendering that row as
"Refused by the built-in guard" although it is an allow.)

**The `"llm"` in the parse error locates the writer.** `JSON Parse error: Unexpected identifier
"llm"` is what a JSON parser says about a bare leading identifier, and two of the proxy's plain-text
502 bodies begin with that word. So the failing response is the terminated portal lane's own 502,
read by the SDK as if it were an API answer — which also means the request got past policy and past
the vet, and the failure is in the dial or the handshake underneath.

**Candidates, all of which render as the same `builtin:dial-failed` today** — which is precisely why
Ask 2 leads the fix:

- a **bypass entry** covering the AWS suffixes in `upstream_proxy_no_proxy`, which routes the dial
  around the corp proxy to the CGNAT address `internal_hosts` lifted — the one candidate that makes
  the report's own diagnosis right, by a mechanism it does not name;
- the corp proxy refusing `CONNECT portal.sso…:443` (an immediate 403/407 — the trail's one-second
  allow→deny gap rules out the 15 s upstream-CONNECT stall);
- the proxy pod unable to reach the corp proxy at all;
- **TLS interception on the tunnel.** The best explanation of "it worked on 0.7.5": on the old lane
  the *sandbox* was the TLS endpoint inside a blind tunnel, and the sandbox images bake `corp-ca.pem`
  at build; Phase B makes the **sidecar** terminate and re-originate, and the proxy image carries a
  corporate CA only if one was staged at its own build — otherwise it trusts one only through
  `WARDYN_TRUSTED_CA_FILE`. Unset, the re-origination fails `x509: certificate signed by unknown
  authority`, filed as `builtin:dial-failed` like everything else.

**What to send back today, before any release:**

1. `kubectl -n <runs-namespace> logs wardyn-proxy-<run-id>` while the run is alive — on Kubernetes
   the proxy is its **own pod**, not a sidecar container of the agent pod. The underlying error is
   already logged there, secret-masked but not topology-redacted.
2. The values of `upstream_proxy_no_proxy` and `internal_hosts`, and whether `trustedCA` /
   `WARDYN_TRUSTED_CA_FILE` is set on the wardynd deployment.
3. Yes — `WARDYN_AWS_SSO_PROXY_INJECT=off` is the sanctioned stopgap. It is a boot flag read once per
   dispatch, so it changes **new dispatches only**; a run already dispatched keeps the lane it was
   authored with. The trade-off to record: the SSO access token becomes resident in the sandbox
   again. The derived role credentials were resident either way.

## Follow-up after 0.7.8: the re-origination meets an HTTP/2 peer

With 0.7.8's `cause` and `via` fields the same operator traced the failure in one run. Every deny row
carried `via: "upstream-proxy"`, which settles the question above: the MITM lane does traverse
`upstream_proxy_url`. The `cause` held three well-formed HTTP/2 frames: SETTINGS, WINDOW_UPDATE, and
GOAWAY with `PROTOCOL_ERROR` and `last-stream-id = 0`. They arrived where the proxy's HTTP/1.1
transport expected a response. `trusted_ca_certs: 4` and the absence of any x509 error rule out
the CA hypothesis. Their table of paths on the same cluster, proxy and bundle shows the single
variable: every path where the client negotiates its own protocol works, including the control
plane's Go `http.DefaultTransport`, which offers h2. Only the proxy's re-origination fails.

They asked two questions the code answers:

- **Is the re-origination TLS or cleartext?** TLS. `upstreamSchemeFor`
  (`internal/egress/proxy/mitm_hosts.go`) returns `https` for this host, and HTTP/2 bytes ahead of
  the handshake would have failed it with a TLS error instead. So the frames came from inside a
  completed TLS session. The peer is whatever terminates TLS on that path. The proxy offered no
  ALPN at all, and that peer spoke HTTP/2 anyway.
- **Is `ForceAttemptHTTP2: false` a deliberate invariant?** No. It has been on the egress
  transport since the first public release, and nothing records a reason for it.

What 0.7.9 changes:

- The egress transport offers `h2,http/1.1` over ALPN and speaks HTTP/2 when the peer chooses it,
  as the control plane's transport already does (#360).
- A peer that speaks HTTP/2 without negotiating it is detected. The proxy remembers that host
  for the run and resends over HTTP/2 when the request body can be replayed (#360).
- When that still fails, the row is `builtin:upstream-protocol-mismatch` with the cause
  `peer answered HTTP/2 to an HTTP/1.1 request (ALPN: …)`, not `builtin:dial-failed`. It is
  answered with a 400, so the SDK stops retrying (#359).
- `upstream_proxy_no_proxy` CIDR entries match IP literals only. OPERATIONS.md now says so (#361).

What it cannot prove from here: nothing outside that estate reproduces its TLS peer. The fix is
tested against a peer built to behave the same way (HTTP/2 regardless of ALPN) and against a
peer that negotiates normally. If the lane still fails, the new cause names the negotiated
protocol, which is the next fact needed. `WARDYN_AWS_SSO_PROXY_INJECT=off` stays available as the
stopgap they have chosen not to take.
