# Wardyn for users

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

Your admin can see this page's world for themselves without a second login —
[docs/operations/member-mode.md](operations/member-mode.md)
— which is worth knowing when you report something: they can usually look at
exactly what you are looking at.

**Autonomy levels (0.8).** If your admin has set an autonomy rubric on your
profile, it caps what a run may do unattended, graded on what your run actually
reaches and holds — not on you personally. Four levels: `L0` (attended —
interactive only), `L1` (gated — non-interactive runs are allowed, but a
claude-code run's tool approvals are switched from `auto` to `hold`, so the
agent still stops for you at every gated call), `L2` (unattended — auto-approval
and seeded auto tools are allowed), `L3` (adds `task_mode=exec`, unsupervised
execution). Below `L3`, an interactive run with a task must use
`interactive_start=agent`; the shell startup form (`interactive_start` unset or
`shell`) runs your task at sandbox boot before anyone attaches, and is refused
(`runs.interactive_start`). The level is graded on your run's egress reach,
whether it holds a secret and how powerful one, and its confinement class — the
agent you picked does not change the level, though at `L1` it can get a
non-interactive run refused (below). A run whose shape exceeds what your
resolved level permits is refused `governance_profile`, the same refusal a
denied `task_mode` or interactive flag already gives you — see
[OPERATIONS.md § Every denial that isn't a 404](OPERATIONS.md#every-denial-that-isnt-a-404).
A non-interactive claude-code run at `L1` is not refused for lacking
supervision; it launches with its tool approvals derived to `hold` and the
create response carries a warning saying so. Any other agent (codex-cli, or a
bring-your-own image) has no tool-approval lane to derive a hold into, so the
same run is refused instead (`runs.agent`) — launch claude-code, or launch
interactively.

Three things worth naming here, because they read as bugs otherwise:

- A run id that isn't yours answers **404**, not 403 — Wardyn never confirms
  or denies that something exists for a principal who can't see it.
- Your unfiltered `GET /audit` (no `?run_id=`) comes back **empty, not an
  error** — see [Where your runs' audit lives](#where-your-runs-audit-lives).
- **A repository your org's git providers don't cover is refused before your
  policy is read.** Since 0.7.2 an admin says which git hosts — and which org
  paths on them — this deployment clones from. A repository outside that is
  refused at every door one reaches a clone through: onboarding or editing a
  workspace, scanning or building one, and creating a run. Your refusal says the
  host is not an enabled git provider and tells you to ask an admin — and that is
  deliberately all of it, identical at every door: the allowed addresses are the
  org's own topology and are never shown to a member, and varying the sentence by
  door would leak the shape of the policy one refusal at a time. No
  `inline_policy` can widen it, because it is not a policy decision. A separate
  refusal you may also meet — if your admin has turned on the
  `workspace_provider` capability kind — says your work may not come from that
  provider, and names the provider **kind** (`github`, `azure_devops`) only;
  that one is a grant your admin can write. A deployment whose admin has
  written no provider rows refuses neither. See [What to ask your admin
  for](#what-to-ask-your-admin-for).

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
directory** inside the drive, never the drive's root, and Wardyn binds nothing
named for anyone else — what your share's administrator does above that directory
is theirs (the threat model's residual #33).
`GET /me` is the ground truth for what you have, and it answers in **three**
keys, not one. The last two are **always present**, so a missing key means an
older daemon rather than "nothing is wrong":

- `user_drive` — your allocation, or `null`.
- `user_drive_denied_by_profile` — the **door**: the name of the governance
  profile that refuses to mount a drive for you, or `""` when none does.
- `user_drive_unavailable` — `""` when Wardyn could answer. Any other value is
  one of five tokens saying it could **not**, and which remedy that implies:
  - `groups_snapshot_stale` — your group membership cannot be read, so an
    allocation may exist and be invisible to this answer. Sign in again; until
    you do, a launch that asks for the drive is refused (`403`).
  - `user_type_unknown` — the user type you signed in with no longer exists
    (an admin deleted it), so which drive your type is given cannot be told.
    Ask an admin to give you another type, then sign in again; until you do, a
    launch that asks for the drive is refused (`403`).
  - `unmountable` — an allocation exists and this deployment cannot bind it: a
    directory that is not on the share, or a name that cannot name a directory.
    An admin's fix, not yours; a launch is a `422`.
  - `unavailable` — your allocation could not be read at all. A launch is a
    `500`; try again, and tell an admin if it persists.
  - `governance_unavailable` — your ceiling could not be resolved, so whether
    the door is open is unknown. Nothing is wrong with the allocation itself,
    and a launch is refused rather than guessed at.

So `user_drive: null` on its own **no longer means** "nothing is allocated to
you". It means that only when `user_drive_unavailable` is `""`; in the five
states above the obvious reading — ask an admin for an allocation — is the wrong
one, which is exactly why the third key exists.

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
- `drive: your <claim> cannot name a directory (lowercase letters and digits, then . _ -, up to 63 characters) — ask an admin to set your directory name` — on a **Kubernetes** deployment the rule is stricter and the refusal says so, appending `(on a Kubernetes deployment the rule is stricter: no _, and it may not end in - or .)`: a directory name there becomes part of a volume-claim name.
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
intended, not a failure. The same holds for your own run's Azure DevOps
access request if it sets `WARDYN_CAPABILITY_SECOND_HUMAN=1`.

## SSH keys

`wardyn ssh-key ensure` registers your key; `wardyn run ssh <run-id>` attaches to
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

If your laptop is one your organisation enrolled into a remote control plane,
this same audit trail is also forwarded upward into the organisation's own
table — this is automatic, admin-configured, and nothing you do here changes
it. It does not change where you read your own runs' audit: it is still this
page and this endpoint.

## Model providers (0.8)

Your admin sets up one or more model providers (Settings ▸ Model
providers), and your model credential is your own, whatever kind the provider is:
an Amazon Bedrock SSO sign-in, a Claude subscription sign-in, a typed
Anthropic, OpenAI or Bedrock API key, or a token for your admin's own
gateway. You connect it yourself — nobody else's runs can use it, and you are
never served an admin's credential in its place. If your admin set a base URL on
a Claude-subscription provider, your sign-in token is sent to that gateway rather
than to Anthropic.

- **Where you see it.** In the User view only (not on Getting started), a
  banner names a provider that is the default for one of your harnesses and
  still needs you — no key or token is available for it (it may never have
  been added, or been removed or unreadable since), or (Bedrock SSO only) your
  sign-in is expiring or no longer works; a provider nobody defaults to is not
  an alarm even if you never connected it. Two or more needing you collapse to one "N
  of your model connections need you" line with a Review link, rather than
  naming each. `GET /setup/status`'s `model_providers` and `provider_access`
  are the same answer, if you are scripting: one row per provider you may use,
  each with its own state (`not_configured`, `expiring`, `expired_signin`,
  `live`, or `not_applicable` when you have no credential namespace of your
  own, such as the admin token under OIDC).
- **A sign-in kind** (Bedrock SSO, Claude subscription) opens a short-lived
  login sandbox against your admin's own configuration — the access portal,
  region, and (Bedrock) the pinned account and role — and stores what it
  captures under your own principal, as the AWS sign-in flow below describes.
- **A typed key or token** is stored write-only, under your own namespace,
  with `PUT /api/v1/model-providers/{id}/credential` (`DELETE` to remove it) —
  the console's own "Add your key" / "Add your token" door calls the same
  route. Nothing ever reads a stored value back; `provider_access` reports
  only whether one is present.
- **Launching a run** chooses your provider in this order: the provider you
  name on the request, else your workspace's pin, else the agent's own
  default, else — when exactly one provider is left that serves the agent —
  that one; two or more with no usable default refuses the run so you choose.
  For a Claude subscription, a typed key/token, or a Bedrock API key, the
  credential injected is yours, proxy-side, and never resident in the
  sandbox. A Bedrock SSO sign-in is the one exception: the in-sandbox AWS SDK
  always derives short-lived AWS role credentials that stay resident in the
  sandbox for the run, and with `WARDYN_AWS_SSO_PROXY_INJECT=off` the SSO
  access token itself is written into the sandbox too — see
  [../threatmodel/THREAT-MODEL.md](../threatmodel/THREAT-MODEL.md) ("Derived
  AWS role credentials") and [ENV.md](ENV.md)
  (`WARDYN_AWS_SSO_PROXY_INJECT`).

## Your model connections

Add your own credential for a provider from Your account ▸ Your model
connections in the console — one row per provider your admin enabled for your
agents, each with its own sign-in or key button. There is no `PUT /secrets`
step: `PUT /secrets/<name>` refuses the model-credential names
(`anthropic-api-key`, `openai-api-key`, `bedrock-api-key` and the three AWS
SigV4 names `aws-access-key-id`, `aws-secret-access-key`, `aws-session-token`)
and points you back at Settings → Model providers. Your own row is visible only
to you and to your own runs — another member can never read or inject it, even
by naming it in their own inline policy.

**Per-person credentials: `owner_only`.** Any stored secret a grant names —
an `api_key`, `git_pat`, `ssh_key` or `env_secret` grant — resolves your own row
first and, if you have none, the operator's row of that name. For a credential
that belongs to one person (a personal access token, a personal API key) that
fallback hands the admin's identity to everyone who has not stored their own,
so the grant should say `"owner_only": true` (see
[POLICIES.md](POLICIES.md), the eligible-grant fields). Such a grant reads your
row and nothing else: with no row of your own the launch is refused and says
which secret to store; store it under the same name via `PUT /secrets/<name>`,
signed in as yourself, and launch again (an admin's own writes land in the
operator namespace, so an admin stores theirs from the user view). A stored policy an admin writes may name a secret nobody has
stored yet — each person's run checks their own. Ask your admin to mark
per-person grants `owner_only`; the run's `credential.mint` audit row
(`secret_scope`: `own` or `operator`) shows which row a mint used.

- **Signing in to AWS yourself.** For a Bedrock SSO provider the AWS session a
  run authenticates with is YOURS, and you sign in for it — the "Sign in to AWS"
  action beside your model-access chip launches a short-lived login sandbox
  against the ADMIN'S access portal (never one you choose), and what it captures
  is stored under your own principal. Nobody else's runs can use it, and you are
  not served an admin's if you have none.
  Renewal is Wardyn's while the session lasts; once the refresh token is spent or
  your identity provider revokes it, you sign in again. **If your SSO session
  reaches several AWS accounts, the admin pins which account and role that
  provider may use** — so you are never asked to guess which of your entitlements the
  model lives in, and a sign-in that cannot reach the pinned pair is
  refused on the login terminal (`wardyn: aws sso credential rejected: …`)
  rather than quietly capturing the wrong one; if nothing is pinned and your
  session reaches more than one, the sign-in asks you to choose. This sign-in
  is yours alone — your admin's own API calls, made with the shared admin
  token, cannot sign in on your behalf or read your model access for you.

## What is kept about your stored credentials

`GET /secrets` shows names only, `mine` included — never a value, not even
your own; nothing you or your admin ever store is read back through the API.
Rotating a value (setting it again under the same name) or removing it
(`DELETE /secrets/<name>`) takes effect within minutes for a run already
going, not just the next one: the proxy re-resolves an injected stored key
from the store every ten to fifteen minutes and drops it at once on a
definitive refusal (a short grace covers a transient store outage only). The
one exception is an approval-gated grant, which is minted once and stays
static for that run's whole life. Your
admin can erase every credential in your namespace in one step; once that
runs you keep nothing recoverable through Wardyn, though a database backup
your organisation took beforehand is a separate question its own retention
answers, not something erasing your namespace reaches into. Wardyn never
revokes anything upstream on your behalf: your own AWS, Anthropic or Azure
DevOps sign-in stays valid at the provider until you — or your admin, there,
not here — revoke it directly.

## What to ask your admin for

- **Model access.** Ask your admin to add a model provider for your agent
  (Settings ▸ Model providers); then connect your own credential for it — see
  [Your model connections](#your-model-connections) above.
- **A git provider your repo's host is on.** If onboarding a repository or
  launching a run against it is refused because its host is not an enabled git
  provider, only an admin can fix it — by enabling a provider row for that host,
  or by adding the org path your repository sits under to a row that already
  covers it. Give them the repository's full clone URL: the rows are matched by
  host and by URL prefix, so `https://dev.azure.com/acme` and
  `https://dev.azure.com/acme-labs` are two different answers. Azure DevOps has no shared
  administrator token: a run uses your own credential. On an organisation backed by Entra ID you connect
  once, when you sign in to Wardyn, and Wardyn creates a short-lived token in your name for each run (or,
  where your admin chose it, uses your Entra sign-in or a token you add yourself under Settings). On
  Azure DevOps Server you add your own token under Settings, for git only — see
  [AZURE-DEVOPS.md](AZURE-DEVOPS.md) for what that looks like. If
  instead the refusal says your work may not come from that provider, ask for a
  `workspace_provider` capability grant naming it.
- **A custom sandbox image** — an `image` capability grant.
- **A workspace root**, if you don't have one yet.
- **A wider egress ceiling** — the stored policy is your admin's to change,
  not yours.
- **A drive, a writable drive, or a bigger one** — all three are allocations
  only an admin writes; see [Your drive](#your-drive). A drive that has gone
  wrong is reclaimed by an admin command too, so ask for that rather than
  looking for a reset.
