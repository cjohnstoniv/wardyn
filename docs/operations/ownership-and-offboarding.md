# Ownership scoping, and offboarding a person

What a member owns, what stays operator-owned, and the ordered procedure for removing a
departed person: sessions, tokens, keys, runs, credentials, workspaces and drives.

## Ownership scoping, and offboarding a person

**Ownership scoping — real, not just admin-vs-everyone.**

- A member reaches their OWN resources the same way an admin reaches any of them (`ownsRunOrAdmin`/`getRunAuthorized`, [`internal/api/helpers.go`](../../internal/api/helpers.go)):
  - `GET`/kill/profile/grants on a run, minting its attach ticket, its recording replay,
  - and `GET /runs`/`GET /approvals` (each scoped to the caller's own `created_by` rows) all answer a foreign resource with the **byte-identical 404** a truly-missing one gets
  - never a 403, so probing another user's run id learns nothing.
- `GET /audit` is **run-scoped, not `created_by`-scoped**:
  - a member must pass `?run_id=` naming a run they own
  - no `run_id`, or one they don't own, both return an empty `200` list,
  - so a member's unfiltered audit feed is always empty by design
  - (the console reaches it from a run's Audit tab, whose "open full Audit" link carries `?run_id=`).
- That run-scoped read, and the run's grant list, leave out the name of an organisation's `shared` component secret:
  - this holds for every caller below the security tier, whatever action wrote the row, and covers `secret_name`, `key_secret_ref` and `known_hosts_secret_ref`;
  - `admin` and `security_admin` read the rows as recorded, and `run.policy.resolve` and `credential.mint` are recorded whole;
  - see [Custom components](custom-components.md#custom-components) and [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md).
- `GET /setup/status` redacts operator-diagnostic detail (checks, secret names, runner detail) for a member.

**Workspace ownership (0.6, migration `0048`)** and **Secret ownership (0.7, migration `0050`)** are the second and third owned nouns after runs.

`workspaces.owned_by` / `secrets.owned_by` hold the creating MEMBER's principal; `""` — every pre-migration row, and everything an admin writes — means **operator-owned**, i.e. exactly today's behavior.

- **Workspace CRUD/scan/build are owner-or-admin, not admin-only** (`getWorkspaceAuthorized`/`getWorkspaceReadable`, [`internal/api/helpers.go`](../../internal/api/helpers.go)).
  - Another member's owned workspace answers the **byte-identical 404** a missing id does.
  - An OPERATOR-owned workspace answers a member's mutation with a **403** — it is listable and readable by every member, so there is no existence to hide.
  - `GET /workspaces` returns the caller's own rows plus the operator-owned ones, never another member's.
- **A member's `local_dir` source is bounded by operator-set roots**: `WARDYN_USER_WORKSPACE_ROOTS` (and its per-member `_MAP`, which REPLACES the shared list for a principal that has an entry) in [ENV.md](../ENV.md).
  - Unset = no member `local_dir` mounts at all (fail closed); writability needs the separate `WARDYN_USER_WRITABLE_ROOTS` minus `WARDYN_USER_WRITABLE_DENY`.
- **Offboarding is `POST /workspaces/{id}/reassign`** (admin-only): returns the row to the operator (`owned_by=""`) and audits `workspace.reassign` with the departed member in `from_owner`.
  - Idempotent, so a sweep over a departing member's ids never fails halfway.
  - The row's `local_dir` sources stop being member-authored, so the member root and dotfile gates no longer bound them — they become ordinary operator mounts.
  - Treat it like creating the workspace.
- **Offboarding a USER DRIVE is two halves, and only one of them is a product action — and the ORDER is not the obvious one.**
  - Run `POST /drives/preview` **first, while the allocation still exists**, and write the object name down.
  - The preview answers "what does this deployment say about THIS principal" by running the ordinary resolver (`resolveUserDriveFor`, [`internal/api/user_drives_resolve.go`](../../internal/api/user_drives_resolve.go)), and the resolver matches on the **grant**:
    - delete the allocation first and the preview resolves nothing,
    - so the one call that names the object a person is about to lose stops being able to name it.
  - Paste the sign-in subject FIRST — on a `hash` drive the name keys on the first claim, and the API's `home_subject` says which claim it used (the console does not yet show it).
  - *Then* delete the allocation (`DELETE /drives/grants/{id}`, admin-only, audited `drive.grant.delete`).
  - It stops the mount at that person's next run and **deletes no data**
    - which is why the audit row carries the drive's declared `reclaim` intent (`retain` or `delete`),
    - so the log records what the operator was told to do about the directory this allocation was the last pointer to.
  - What is left behind is one object per person: a Docker named volume, or a subdirectory of the share the operator mounted host-side, or a PersistentVolumeClaim.
  - A **managed** object (`docker_volume`, `k8s_pvc`) carries the drive row's **id** and the person's home name as labels, so a departed member's objects stay findable after the row is gone.
  - That is the recovery path if you skipped the preview.
  - A share's subdirectory and a static claim carry **nothing**, so for those the preview is the only thing that names the object at all.
  - Reclaiming it is a deliberate command — `POST /drives/{id}/reclaim` (`wardyn drive reclaim`), or the substrate command by hand; both stay supported, and "[Reclaiming a departed person's storage](user-drives.md#reclaiming-a-departed-persons-storage)" below is the runbook for both.
  - Deleting the **drive row** itself is a `409` while any allocation still points at it (`ON DELETE RESTRICT`), so the deallocation is always its own audited event and offboarding can never silently widen anything.
- **Secret write/delete moved from admin-only to self-service.**
  - Any signed-in human may `PUT`/`DELETE /secrets/{name}` their OWN row (`secretOwnerFromRequest`: `""` for an operator, their own principal for a member).
  - A member's `DELETE` of another principal's row is structurally unreachable (`secretstore.Store.For(owner)` never resolves it) and answers the byte-identical 204 a never-set name gets.
  - The six model-credential names (`anthropic-api-key`, `openai-api-key`, `bedrock-api-key` and the three AWS SigV4 names `aws-access-key-id`/`aws-secret-access-key`/`aws-session-token`) are refused (403 `secret_name_reserved`) for every caller, the operator included:
    - a model credential is stored on a model provider, by the person it belongs to,
    - and wardynd deletes any left from before 0.8.2 at boot (`model_credential.retire` in [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).
- **`GET /secrets` returns `{names, mine}`.**
  - `mine` is always the queried namespace's own rows (reserved names filtered out).
  - `names` keeps its pre-0.7 meaning for an admin — the operator namespace, or one member's own rows with `?owner=<principal>`
    - and for a member narrows to the operator-owned names an eligible grant in the operator's ceiling actually pairs with a host, closing a name-enumeration gap.
- **A run resolves its owner's row, falling back to the operator's** — never another member's, even when an inline policy names it by hand.
  - The upstream (corporate) proxy secret always stays resolved from the operator namespace:
    - under a configured upstream the sidecar hands the corp proxy a HOSTNAME rather than a pinned address for every host it proxies (all of them, minus `upstream_proxy_no_proxy`),
    - so a member-substitutable value there would put a member in control of which proxy resolves and dials every one of them.
- **`?owner=<principal>` is admin-only** on `DELETE`/`GET /secrets`, refused with a constant 403 for anyone else.
  - **A `PUT` refuses it for everyone (0.8, `403`, audited `secret.write` `denied`)**:
    - a credential is set only by the person it belongs to,
    - so an admin can remove a person's credentials but never set one their runs would use under their name,
    - and pre-provisioning a member's key before they sign in is no longer possible.
  - The value names a HUMAN and is RESOLVED to the namespace key that person's own writes land in:
    - matched case-insensitively against the principals this deployment knows, and mapped from the email form through the same (principal, email) pairing `POST /sessions/revoke` matches on.
  - A subject is opaque and case-sensitive, so the fold cannot be a first-hit scan:
    - an EXACT subject match wins outright,
    - and a value that folds case-insensitively onto MORE THAN ONE known principal is refused `422` rather than resolved to whichever the directory happened to list first
    - guessing there would write a credential into the wrong human's namespace.
  - An email address that pairs to no known principal is refused 422 rather than silently creating a namespace its owner never reads, and a cross-namespace `DELETE` that removed nothing answers 404 rather than an idempotent 204.
- **Offboarding a person's credentials is `DELETE /people/{principal}/credentials`** (admin or `security_admin`, 0.8); `GET /model-providers/credentials` first lists what each person holds per model provider, with when it was added and last used.
  - The erase deletes every credential in that person's namespace — keys, tokens and captured sign-ins — and answers `{"count": N}`; the principal resolves as `?owner=` does.
  - In store mode each value leaves Vault or Key Vault before its row,
    - and the answer adds `store`, `purged` and, when Key Vault kept soft-deleted copies, `recoverable_days`:
    - the organisation can recover them for that long unless its vault operators purge them.
  - It never answers success with a credential left behind (`500`, audited `credential.erase` `failure` with the count it did delete; run it again), and it never erases the operator namespace.
  - A renewal of their AWS session, or an Azure DevOps refusal stamp, already in flight finishes first and is erased with the rest:
    - the erase waits for the owner's AWS lock, the Azure DevOps sign-in's lock and the own-token write lock (for a renewal, at most about 21 seconds),
    - so a success is final for everything Wardyn itself was writing.
  - That coordination lives in one process:
    - on a deployment with more than one replica a second one can still interleave,
    - and the erase's own re-list reports only a write it can see (tombstones are tracked in #1511).
  - An Azure DevOps sign-in configuration that cannot be read refuses the erase (`503`, `credential_erase_signin_config_unreadable`, nothing erased).
  - A person who reconnects afterwards writes new credentials, which stay.
  - The erase does not reach into a running run: one that already holds a credential in memory keeps it.
  - A run already going keeps a static key (an `api_key` injection is fetched once and cached for the run) until it ends, so also stop their runs (`POST /runs/{id}/kill`, the run kill switch).
  - **Wardyn cannot revoke anything upstream**, with one exception: it first revokes the live Azure DevOps tokens it created for the person's runs (`ado_pat.revoke`, reason `offboarding`; one it cannot revoke expires by itself).
  - Revoke the person's AWS, Anthropic and Azure DevOps sessions, any Azure DevOps token they pasted in themselves, and any gateway token, where they were issued, and disable them in the identity provider.
  - A refused erase (a blank principal `400`, or one naming nobody or several people `422`) is audited `credential.erase` `denied`.
  - **The erasure horizon, on the default (local Postgres) store, is your backup retention — not the API call.**
    - `DELETE /people/{principal}/credentials` removes the live row; it does not, and cannot, reach a `pg_dump` you already took, a replica, or Postgres WAL.
    - Until every backup made before the erase ages out of your retention window, the value is recoverable from it by whoever can read a backup,
      - exactly as it was live
      - (envelope v1 does not change this: the same key-encryption key that opened the row in Postgres opens the same bytes in a restored dump).
    - With `WARDYN_KEK=transit`, the backup is only as erased as the KEK:
      - rotating the Transit key past the old wrap (`wardynd -rewrap`, then raising `min_decryption_version` — "Key service: Vault Transit") is what actually forecloses an old backup,
      - the same way `-rotate-age-key` does for the local key.
    - In store mode (Vault, Azure Key Vault) the value itself never reaches your Postgres backup at all
      - the store's own deletion/retention is what governs it, as in "Removing a credential, and the erasure horizon" below for Key Vault, or your Vault KV engine's own versioning and delete-version policy.
- **Offboarding a person, in full.**
  - The erase removes stored credentials and nothing else.
  - In order:
    0. Find them and see what they hold: `GET /people?q=<email or subject>` (`wardyn people list --q`) lists each match with its live-session, token, SSH-key, credential and running-run counts, which the steps below act on.
    1. Disable the person in the identity provider, so no new sign-in succeeds.
    2. `POST /sessions/revoke` with their subject or email:
       - ends their console sessions, refuses a UI-app session at its next re-check (an attach ticket minted before the revoke is refused at redemption)
       - and revokes every `wdn_` API token they hold (its `tokens_revoked` count is the receipt; see "[Per-user API tokens: stop sharing the admin token](api-tokens.md#per-user-api-tokens-stop-sharing-the-admin-token)").
    3. Remove their registered SSH keys and end established SSH connections: [SSH access revocation](../SSH.md#revoking-access-during-an-incident).
    4. Kill their running runs (`POST /runs/{id}/kill`). A run keeps a static key it was handed (an `api_key` injection is cached for the run) until it ends, whatever the erase does.
    5. Erase their credentials: `DELETE /people/{principal}/credentials`.
    6. Hand back their workspaces (`POST /workspaces/{id}/reassign`) and their user drives (the two-halves order above).
    7. Revoke upstream what Wardyn cannot: their AWS, Anthropic and Azure DevOps sessions, a token they pasted in themselves and any gateway token.
  - One copy outlives all of this in memory: wardynd keeps an Azure DevOps sign-in's refresh token in its process-wide masking set, so output quoting it is still masked.
  - It is never served or injected from there; it is let go a grace period after the credential is replaced, or when wardynd restarts.
  - A run's own masking copies go the same grace after the run ends.
- **Dead sign-ins are not kept.**
  - A captured AWS or Azure DevOps sign-in whose refresh token the provider refuses for good (`invalid_grant`) is deleted at that renewal,
    - and a stored AWS sign-in is deleted by a daily sweep once it can no longer be used or renewed (its row's `expires_at`);
    - both audit `credential.expired.delete`.
  - A row the sweep cannot delete is kept, audited `failure`, and retried the next day.
  - The person is then shown as not connected and signs in again.
  - A Conditional Access refusal does not delete anything — the sign-in still works once the person is present.
  - An Azure DevOps sign-in records no expiry, because Entra publishes none for its refresh token: an unused one is kept until the provider refuses it or it is erased.
- **Cross-user admin access is queryable.**
  - An admin acting on a member-owned workspace stays the ADMIN in the audit actor (no impersonation; delegation is recorded as delegation — [Delegated run management](api-tokens.md#delegated-run-management-portals)) with `workspace_owner` naming the member;
  - `secret.write`/`secret.delete` carry `secret_owner` naming the non-"" namespace a write landed in (a member's own ordinary write included) — see [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md).
- **Member model access.**
  - A member's model access is their own credential on a model provider;
    - no integration row is derived from anyone's convention-named secret, and no inline `api_key` grant naming a model vendor's host is admitted from a member's own secrets.
  - See [USERS.md § Your model connections](../USERS.md#your-model-connections).

**Deciding an approval is kind-restricted, not just owner-restricted** (`decide()`, [`internal/api/approvals.go`](../../internal/api/approvals.go)): a member may approve or deny an `egress_domain` approval on a run they own.

- `credential` and `tool_call` approvals stay **admin-only regardless of ownership**
  - the shipped default policy requires approval on `github_token`,
  - so a member self-approving their own run's credential request would self-mint a real token, and self-approving a `tool_call` re-opens exactly what the clamp (below) exists to bound.
