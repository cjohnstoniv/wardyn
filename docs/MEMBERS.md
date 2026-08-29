# Wardyn for members

Someone else runs this control plane. You sign in, and you run governed
sandboxes inside the ceiling your admin set. There is nothing to install.

## Signing in

SSO only — the sign-in screen offers it when the server reports it
(`/healthz` `sso`). There is no admin token; that credential belongs to your
operator, not to you.

Your role is derived at login and stamped into your session. If you land on
"no Wardyn role assigned," your admin has not mapped you yet — see
[OPERATIONS.md § Multi-user: who can change what](OPERATIONS.md#multi-user-who-can-change-what).

`GET /me` is the ground truth for your own role and your workspace root.

## What you can and cannot do

The full matrix lives in
[OPERATIONS.md § Multi-user: who can change what](OPERATIONS.md#multi-user-who-can-change-what);
this page does not restate it. `GET /me/capabilities` tells you which
capability grants you personally hold.

Two things worth naming here, because they read as bugs otherwise:

- A run id that isn't yours answers **404**, not 403 — Wardyn never confirms
  or denies that something exists for a principal who can't see it.
- Your unfiltered `GET /audit` (no `?run_id=`) comes back **empty, not an
  error** — see [Where your runs' audit lives](#where-your-runs-audit-lives).

## Onboarding your own workspace

Members have owned their own workspaces since 0.6 — look for the Workspaces
entry in the console nav.

A `local_dir` source must sit under a root your admin configured
(`WARDYN_MEMBER_WORKSPACE_ROOTS`, or a per-member map that **replaces** the
shared list for you) — see [ENV.md](ENV.md). Unset means you may mount no
host directory; that is fail-closed by design, not a bug. `GET /me` shows the
root or roots that apply to you — a hint for the console; the server enforces the
boundary when a mount binds. Writability is a second, separate gate.

Offboarding — handing a workspace back to your admin — is an admin action
(`POST /workspaces/{id}/reassign`); see
[OPERATIONS.md § Multi-user: who can change what](OPERATIONS.md#multi-user-who-can-change-what).

## Your first run

Launching and killing a run is yours to do. Your `inline_policy` is clamped
to your admin's ceiling — `POST /runs/preflight` previews exactly what
launch will do, through the same clamp, so read its warnings: a dropped
grant or egress host is told to you there, before the run starts. The field
reference lives in [POLICIES.md](POLICIES.md); this page doesn't restate it.

## Deciding an egress approval on your own run

You may decide an `egress_domain` approval on a run you own. `credential`
and `tool_call` approvals stay admin-only regardless of who owns the run —
see OPERATIONS.md for why.

If your deployment sets `WARDYN_EGRESS_SECOND_HUMAN=1`, you cannot decide
your own run's egress approval yourself. That's four-eyes working as
intended, not a failure.

## SSH keys

`wardyn ssh-key ensure` registers your key; `wardyn ssh <run-id>` attaches to
a run you can reach. The full surface — registering, connecting, sftp, port
forwarding, VS Code Remote-SSH, scripted access — is
[SSH.md](SSH.md).

## Personal API tokens

`wdn_` tokens are yours: independently revocable, shown once, minted at
`POST /me/tokens`. Use them for scripts. Never share the admin token — you
don't have it, and you shouldn't need it.

## Where your runs' audit lives

`GET /audit?run_id=<your run>` — reachable from the run's Audit tab in the
console. Leave off `?run_id=` and you get an empty `200`: a collection
endpoint's answer when it has nothing scoped to show you, not an error. The
action vocabulary is [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md).

## Your model key

Store your own key under the provider-convention name from Getting Started ▸
Your model key (`anthropic-api-key` for Claude, `openai-api-key` for Codex)
via `PUT /secrets/<name>` or the console — `GET /secrets` shows it under
`mine`, never under a name another member wrote. Pick it under Model access
when you launch a run; your run then uses YOUR key, injected proxy-side
exactly like an operator's own (the value is never resident in the sandbox).
Setting your own key needs no operator integration or workspace requirement
first — an unpaired stored secret still needs one of those, but your own
key naming the provider convention does not. The same rule reaches a
workspace's integration requirement: when your admin's integration names a
credential the admin has not stored, a secret you store under that same
name is what your run injects — the integration's host, header and egress
stay the admin's; only the value is yours.

Bounds: this is API-key mode only — the resident Claude-subscription mount
stays operator-only (see [DESKTOP.md § Model access on
m′](DESKTOP.md#model-access-on-m)), and `bedrock-api-key`/AWS credential
names are refused for a member's own `PUT /secrets` regardless (Bedrock
stays the MDM-managed lane). Your own row is visible only to you and to your
own runs — another member can never read or inject it, even by naming it in
their own inline policy.

## What to ask your admin for

- **Model access, when you'd rather not store your own key.** An operator
  integration, or a workspace requirement, re-adds a model grant after your
  policy is clamped — see [DESKTOP.md § Model access on
  m′](DESKTOP.md#model-access-on-m). If you'd rather bring your own key, see
  [Your model key](#your-model-key) above — no admin action needed.
- **A custom sandbox image** — an `image` capability grant.
- **A workspace root**, if you don't have one yet.
- **A wider egress ceiling** — the stored policy is your admin's to change,
  not yours.
