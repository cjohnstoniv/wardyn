> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Exercising member mode as an admin

You have an admin session and you want to see what a member sees. There
are two ways; they answer different questions, and they compose.

## 1. The switch — the User view

*Route, field and audit-action names below predate the 0.8 rename sweep
— see [Renamed in 0.8](../OPERATIONS.md#renamed-in-08) for the current
`POST /me/view` route and `/me` keys; this section's own copy is
repointed in #620.*

The **Console view** switch beside the wordmark offers **Admin view** |
**User view** to a signed-in SSO admin. Choosing **User view** sets a flag
on your EXISTING session cookie; your role is never rewritten, only the
*effective* role your requests resolve to, and only downward.

- The console reloads and you land exactly where a user lands: the
  User-view nav, the user's Getting Started, a `GET /me` answering
  `operator: false`, and every operator-only route 403-ing.
- The pressed **User view** segment says so on every screen, and
  **Admin view** is the way back — there is no band, because the User
  view is a normal state. Other tabs follow the session into the same
  view.
- Every audit row the session writes still names **your own sub**: no
  impersonation. Delegation is recorded as delegation, and there is no way
  to become anybody else.
- The transition itself is audited as `auth.user_view.set`
  (`auth.member_mode` dual-emitted alongside it through 0.8.x, [Renamed in
  0.8](../OPERATIONS.md#renamed-in-08)) with `enabled`, `real_role`, and
  `no_credential` on the preview below.
- Each `403` an **admin-tier gate** raises while the mode is on carries
  `user_view: true` on its `authz.denied` row — the two middleware
  chokepoints and every in-handler refusal that raises the same two
  reasons. A reviewer reads the burst as an admin walking the member path
  rather than as an incident. (Denials with a *different* `reason` — an
  ungranted capability, a foreign resource — are the ones a member would
  meet identically, and carry no marker.)

**Both admin tiers get the switch** — a `security_admin` as well as a
super admin — and both clamp to `user`, since the clamp knows only one
direction. Exiting restores whichever tier you were actually signed in as.
The switch is shown on every install that has both views.

On a single-operator install (local mode, or the admin token with no
identity provider) it only changes the URL. That is one shared credential
with no per-person role to pause, so nothing is clamped or POSTed, and
`POST /me/view` answers those callers `400` if called directly.

**Runs start in the User view.** A signed-in SSO admin or security admin
in the **Admin view** cannot start or preview a run: `POST /runs` and
`POST /runs/preflight` answer `409` with reason `admin_view`. Switch to the
User view to launch. The refusal keys on the browser session only — the
admin token, a `wdn_` token and a single-operator install launch as before.

## Viewing as a user type (0.8)

`POST /me/view` with `{"view": "user", "user_type": "<id>"}` enters the
user view looking through that type. Its grants, governance profile,
drives and run limits bind you exactly as they bind a person of that type,
beside your own user and group rows. The tier stays clamped to `user`, so
no admin route opens whatever the type.

- With no `user_type`, the view uses your previous choice (remembered per
  person, so it follows you across devices), then your own mapped type,
  then the built-in one. `{"view": "admin"}` exits.
- A run launched in the view records the type (`user_type` on the run and
  on its `run.create` row, with `user_view: true`).
- If the type is deleted while you are viewing as it, your next request
  is refused (`403` `user_view_type_deleted`, or `409` `admin_view` on a
  launch), and the view turns off.
- `GET /me` then answers as your real tier with `user_view_dropped`
  naming the type.

## The no-credential preview — "Preview as a new user"

The Permissions page header offers **Preview as a new user**. It's the
User view plus one thing: your OWN captured AWS SSO session reads as
absent for the rest of the session.

- On a `per_user` deployment that is the state every new member is in
  before they sign in — and it's the one state the plain toggle
  structurally can't show. It clamps your role and leaves your subject
  alone, so every per-principal credential lookup still finds your own.
- In the preview, `GET /setup/status` grades your model access
  `not_configured` with "Sign in to AWS", and a Claude Code run is refused
  at create with the same sentence a member who has not signed in meets.
- `POST /model-providers/{id}/sign-in` answers `409`: *"Exit the user view
  to sign in — the capture would land on your own identity."*
- Nothing is deleted: your session sits untouched in the store and comes
  back the moment you exit.
- The transition is audited as `auth.user_view.set` (`auth.member_mode`
  dual-emitted alongside it through 0.8.x) with `no_credential: true`
  beside `enabled` and `real_role`.

Inside the preview, **signing in is refused** — `POST /model-providers/{id}/sign-in`
answers `409` while the posture is on, deliberately. The preview shows a
new member's STATE, not their flow, and a sign-in completed there would
capture a credential against the admin's own principal. The preview's band
says so, and the sign-in pane offers no "Try again" for that refusal — the
way out is to exit the mode.

**It appears only where the org gives each person their own sign-in.** The
button is offered, and the posture granted, only when the model-access
agent's roster row is `per_user`.

- On a `shared` deployment there is no per-member sign-in to be missing,
  so the entry doesn't exist. A request for it enters the plain mode
  instead (`GET /me` publishes `user_preview_available`, and the toggle
  refuses to grant the posture regardless of what the console sends).
- One residual, by design rather than by omission: an admin already
  inside the preview when somebody flips the roster `per_user` → `shared`
  keeps the variant banner until they exit.

**It is MODEL ACCESS only, and THIS BROWSER SESSION only.** The posture
rides this session's cookie, so the same admin's CLI, their `wdn_` API
token and a second browser all still create and dispatch runs on their
real credential. "Runs that need it are refused" is true of this console
session and of nothing else. Everything outside model access is untouched
too: `GET /me` still returns the admin's own user-drive allocation, and
their own runs, workspaces and secrets are still theirs (ceilings 1 and
2).

## What stays live in the mode

| Action | Behaviour |
| --- | --- |
| Minting an API token (`POST /me/tokens`) | REFUSES instead of clamping, with `409`. A token carries a role stamp re-derived from your REAL role at your next sign-in, so one minted "as a member" would quietly become an admin credential that outlives the mode. Exit first |
| Registering an SSH key (`POST /me/ssh-keys`) | Allowed in the mode; the key is stored **capped** (migration `0070_ssh_key_view_capped`) — a member key for good. Your sign-in re-stamp leaves its role at `user`, and the SSH gateway never grants it the admin override, even while you are an admin. It reaches your own runs and nothing else. A break-glass key that reaches other people's runs is registered outside the mode |

> **It shows you what a member SEES. It is not proof that a member is
> REFUSED.** Four ceilings, all deliberate:
>
> 1. **Role only.** Runs, workspaces and secrets you created stay yours,
>    so owner-legal paths still pass for you where they would 404 for
>    someone else. `GET /me/capabilities` and every governance ceiling
>    resolve against your real group snapshot — the mode clamps the role,
>    never the group tier.
> 2. **Credentials you already hold are not clamped — the mode is
>    per-SESSION.** The SSH gateway reads the role stamped on the KEY in
>    the database, refreshed only at login (`WARDYN_SSH_ROLE_TTL`). So an
>    admin in member mode still holds the admin override on other
>    people's runs over SSH. By the identical argument, any `wdn_` API
>    token you already hold keeps its own stamped role (the token lane
>    replays the DB row, never the session), as does the deployment admin
>    bearer token. The `409` token door and the capped key door stop NEW
>    credentials — they cannot reach into old ones. Your browser session
>    is clamped; another credential of yours is a different session.
> 3. **Rolling upgrades.** The flag rides the existing session cookie
>    with no codec bump (a bump would sign every live session out
>    mid-rollout, which is worse). During a rolling Kubernetes upgrade a
>    replica still running the previous version ignores the flag and
>    answers your requests as an admin. Finish the rollout before you
>    rely on what you see.
> 4. **Model access and ownership still resolve to you.** The mode clamps
>    the role and deliberately leaves your subject alone. Under a
>    `per_user` roster row your own captured AWS SSO session is what
>    `/setup/status`, run create and dispatch all resolve — an admin who
>    has signed in sees a live credential while "viewing as member". This
>    is what the 0.7.4 field report was misled by.
>
> For the question "would a member actually be refused this?", use a real
> second identity — recipe below. The two compose: toggle for the fast
> look, second identity for the proof.
>
> **Ceiling 4, and the other posture.** The *second* posture — **Preview
> as a new user** — is what shows the not-signed-in state. It's reached
> from the Permissions header in the **Admin view**. It's not offered
> from inside the User view, and not at all on a deployment whose roster
> row is `shared` (there is nothing for the preview to hide) or against a
> pre-0.7.5 daemon. (`user_preview_available`, `internal/api/me.go`, is
> ANDed with the caller's EFFECTIVE (clamped) admin tier. That's the same
> clamp that made ceiling 4 true in the first place, so the button is
> gone the instant either posture clamps
> `isOperator`/`isSecurityOperator` false.)
> Inside that second posture the ceiling reads the other way round: your
> sign-in is *hidden, not removed*, and a rolling upgrade (ceiling 3)
> hides nothing at all.
>
> **The preview shows the STATE, not the FLOW.** Signing in is refused
> inside it (`409`), by design — a capture made there would land on your
> own identity and overwrite your real session. So it reproduces what a
> member with no credential SEES; it cannot rehearse a member's FIRST
> SIGN-IN. That still needs a real second identity —
> `member@wardyn.local` in the kind quickstart, `wardyn-member` on Entra.
>
> **Rolling upgrades, for the preview specifically.** The posture rides
> the same session cookie as the mode, as a second `omitempty` bool with
> no codec bump. A replica still running 0.7.4 ignores it: it shows you
> your OWN credential AND does not refuse the sign-in, so *"sign-in is
> refused inside the preview"* does not hold mid-upgrade.
>
> In the other direction a 0.7.5 console POSTing `no_credential` to a
> 0.7.4 replica gets a `400` from the strict body decode
> (`DisallowUnknownFields`), and the mode is NOT entered — the button
> says so. The switch keeps working throughout, because the console
> sends the key only for the new posture.
> Finish the rollout before you rely on what you see.

Note the name collision: the `WARDYN_USER_DESKTOP` environment variable
([ENV.md](../ENV.md)) is a different, unrelated thing. It's a boot-time
assertion that the human running a single-workstation daemon is a member.
It adds no middleware and has nothing to do with this toggle, which is
per-session and needs no configuration at all.

## 2. The genuine second identity (the proof)

Sign in as a second, real person. This is strictly more faithful than the
toggle: it exercises the server's own role derivation, its own session,
and its own ownership namespace.

| Environment | Logins provisioned |
| --- | --- |
| kind quickstart | The bundled Dex ships one login per role path: `admin@`, `member@`, `member2@`, `secadmin@`, `operator@` and `stranger@wardyn.local` (`deploy/kind/sso/dex.yaml`, role map in `deploy/kind/sso/values.yaml`) |
| Entra | The walk provisions `wardyn-admin`, `wardyn-member` and `wardyn-outsider` (`deploy/azure-entra-sso/03-people.sh`) |
| compose demo | `deploy/compose/dex.yaml` already ships two logins, `demo@wardyn.local` (admin) and `member@wardyn.local` (member); see [Second user, same host](../OPERATIONS.md#second-user-same-host), which adds a *third* `staticPasswords` entry to the bundled Dex |

> **Use a fresh browser profile / incognito window — or sign out of the
> IdP first.** A live session for the other account in the same tab is
> silently reused instead of prompting for credentials, and the step then
> "passes" without testing anything. This is the single most common way a
> member walk proves nothing at all. (`deploy/azure-entra-sso/README.md`
> says the same for its own walk, and for the same reason.)
