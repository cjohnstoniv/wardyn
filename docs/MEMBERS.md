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

Members cannot store their own model credential yet — this is current, known
truth, not a bug you've found. Until that lands, see
[What to ask your admin for](#what-to-ask-your-admin-for) below: your admin
grants model access through an operator integration or a workspace
requirement. (This section will change, and gain a "Your model key" step in
Getting Started, once per-principal secrets ship.)

## What to ask your admin for

- **Model access.** Today this means an operator integration, or a workspace
  requirement, that re-adds a model grant after your policy is clamped — see
  [DESKTOP.md § Model access on m′](DESKTOP.md#model-access-on-m) and
  [ROADMAP.md](../ROADMAP.md) ("A member's inline model-access grant needs an
  operator integration").
- **A custom sandbox image** — an `image` capability grant.
- **A workspace root**, if you don't have one yet.
- **A wider egress ceiling** — the stored policy is your admin's to change,
  not yours.
