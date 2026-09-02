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

## Your drive

A **drive** is persistent storage your admin registers once and allocates to
you, to a group you are in, or to everyone. It is not a workspace — you do not
onboard it, it holds no repo, and it is not something you can add yourself. It
mounts at `/home/agent/drive`, and it is yours alone: a run sees **your own
directory** inside the drive, never the drive's root and never anyone else's.
`GET /me` carries `user_drive` (`null` when none is allocated to you) and is the
ground truth for what you have.

**It mounts only when you ask, per run.** New run's Workspace card carries a
checkbox — "Mount my drive" — and nothing mounts without it. What the request
carries is a flag (`drive.enabled`), never a path: the server resolves which
drive and which directory from the identity you signed in with, so there is no
drive name, directory or size for you to type, and nothing you can point
somewhere else.

**Read-only is the default, and you can only narrow it.** When your allocation
is writable the checkbox is joined by "Mount read-only for this run", so you can
take the safer posture on a run you don't trust. The line under the checkbox
says which one you're getting — either
"What a run writes there persists to your next run."
or
"A run can read it and never change it."
There is no toggle the other way: `read_only: false` against a read-only
allocation is refused, never quietly honoured, because a run you believed was
writable would only reveal itself when the work failed to persist.

**A change your admin makes reaches you at your next run, not this one.**
Pausing your allocation, resizing it, or changing its mode takes effect the next
time you launch; a run already dispatched keeps what it mounted. Where the
checkbox would be, a paused allocation reads
"Your drive is paused by your admin."
and a launch that asks for it anyway is refused. One exception is slower: a
drive allocated to a **group** reaches you when you next **sign in**, because
your group membership is read once, at login.

**The refusals you can see**, at launch and, because `POST /runs/preflight`
resolves your drive the same way, in a dry run too:

- `drive: no user drive is allocated to you — ask an admin for an allocation`
- `drive: your allocation is paused by an admin`
- `drive: your allocation is read-only; read_only:false cannot widen it`
- `drive: directory <yours> does not exist on the share — ask an admin to create it` — a share drive only; Wardyn never invents a directory inside somebody's NAS.
- `drive: your <claim> cannot name a directory (lowercase letters and digits, then . _ -, up to 63 characters) — ask an admin to set your directory name`
- `drive: this deployment cannot mount your drive (<why>)` — the drive is real; this deployment's runner cannot bind it.
- `mounting a user drive is not allowed by your governance profile "<name>". Launch without drive.` — the one **403** of the set, and the only one that is audited. Before you launch, the console shows it where the checkbox would be, as `Your governance profile "<name>" does not allow mounting a drive.`

The first six are **422s** and none of them is audited: you were authorized and
simply had nothing to mount. `/home/agent/drive` is also reserved — a policy
that names it as a `workspace_mounts` target is refused with
`workspace_mounts[0]: target /home/agent/drive is reserved for the user drive`
(see [POLICIES.md](POLICIES.md)).

Preflight is honest about the **allocation**, not about the substrate: a drive
whose storage the runner cannot actually provision still passes preflight and
fails when the run is dispatched.

**The size you are shown is an allocation, not a quota.** Quoted as the console
quotes it:

> Wardyn never enforces a drive's size itself. On Kubernetes the size is the volume request and the storage class decides whether it binds — block disks do, network-share provisioners do not. On Docker a managed drive has no byte cap, the same gap disk_mib has. A share is bounded by its own quota. The size you see is the allocation, not a guarantee.

So treat a full drive as something to notice, not something Wardyn will stop for
you, and do not rely on the number as a backstop.

**Two more things that are not promises.** Your concurrent runs mount the *same*
drive — two agents writing one directory can corrupt each other's lock files, and
Wardyn does not warn about it. And there is no self-service reset: a drive you
have poisoned is reclaimed by your admin with a documented command, so ask.

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
- **A drive, a writable drive, or a bigger one** — all three are allocations
  only an admin writes; see [Your drive](#your-drive). A drive that has gone
  wrong is reclaimed by an admin command too, so ask for that rather than
  looking for a reset.
