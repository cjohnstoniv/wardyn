# Who writes the provider policy

Two doors write the organisation's provider policy: the console's `/admin/providers` screen
and `PUT /site-config` from the CLI or MDM. This says which one wins, and what each carries.

## Who writes the provider policy: console vs CLI/MDM

- An Azure DevOps row has no shared credential.
- Its `workspace_providers` row names how each person connects:
  - `token_mode` `minted_pat` (Wardyn creates a short-lived token for each run in the person's name), `bearer` (the person's Entra sign-in) or `own_pat` (a token the person adds themselves)
  - and an Azure DevOps Server row is `lanes: ["pat"]` with `credential_source: per_user`, git only.
- A shared `pat` or `ssh` lane, and an empty `lanes` on such a row, is a 400 at both write doors.
- See [docs/AZURE-DEVOPS.md](../AZURE-DEVOPS.md) for the app registration, the row's fields, and what a member sees.

- A deployment carries at most **one enabled** row that signs a person in on the `entra` lane (`minted_pat` or `bearer`): each person signs in to one Azure DevOps organisation.
- Both write doors refuse a second enabled one with a 400 (`git[N].lanes: git[M] already carries the "entra" lane …`); a disabled second row is accepted, and enabling it later is refused the same way.
- A document stored before that rule can still hold two; the setup checklist then warns (row `ado_entra_rows`, not blocking) until one is disabled.
- Rows in `own_pat` mode sign nobody in, so any number of them may be enabled at once.
- A `minted_pat` row must name the console's own OIDC application and the console must hold a client secret (`WARDYN_OIDC_CLIENT_SECRET`), or it is refused at write and left unusable at boot (an error in the daemon log naming `ado_pat_needs_console_app`).

**A disabled row stays closed, and says so.**

- A launch into a disabled git provider row's organisation is refused naming the row.
- Its hosts are left out of `effective_scm_hosts` and out of run egress only when no enabled row also names the same host: with several organisations on `dev.azure.com`, one off and another on, the host stays effective.
- `GET /site-config` also returns `withheld_scm_hosts`, read-only like `effective_scm_hosts` (a `PUT` ignores it, and it is never stored): one `{host, provider_id, provider_kind}` entry per host a disabled row claims that `effective_scm_hosts` omits.
- A host an enabled row also names is not listed.
- The list is absent when nothing is withheld.

**The upgrade that retired the shared Azure DevOps credentials (0.8.2).**

- Migration `0103_retire_ado_shared_credentials` rewrites the stored rows (a row left with no per-person lane is turned **off**, and the setup checklist warns with `ado_rows_off` until an admin turns it on),
  - and `wardynd` deletes the stored shared secrets **once**, at the first start after it:
  - `git-pat-<host>`, `ssh-key-<host>` and `known-hosts-<host>` for every Azure DevOps host, in the operator's namespace and every person's.

> [!WARNING]
> The deletion is irreversible and audited per namespace as `ado_shared_credential.retire` ([`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)).

- The `boot_once` row named `ado_shared_credential_retire` holds the marker; the sweep runs only while its `done_at` is null.
- A secret store that cannot answer at that first start **refuses boot** rather than leave a retired credential in place.
- A host that a GitHub or other non-Azure DevOps row also names is skipped and logged, since the name could be that forge's own credential: remove a shared Azure DevOps credential there by hand.

- 0.7.2's two provider blocks
  - `workspace_providers` (which git hosts and org paths a run may clone from, which credential lanes it may use there, and the ephemeral/drive storage ceilings)
  - and `agent_providers` (which agents this deployment offers, the one model-access lane each may use, and whether that credential is shared or captured per person)
  - are **policy fields on `SiteConfig`**, not tables of their own.
- That is deliberate: `SiteConfig` is the org→desktop channel MDM already delivers as `/etc/wardyn/site-config.json` ([DESKTOP.md](../DESKTOP.md)), so a provider policy reaches a managed laptop with no new plumbing and no DDL.
- It also means **two doors write them**, and the grid below is what tells them apart.

| | Dedicated endpoints (the console's `/admin/providers` screen) | `PUT /site-config` (the CLI / MDM door) |
|---|---|---|
| **Route** | `GET`/`PUT /workspace-providers`, `GET`/`PUT /agent-providers` — both verbs admin-only, for the reason the tier table above gives: a base URL names corporate topology and an `sso_start_url` names the org's IdP | `PUT /site-config`, admin-only, a **full-document replace** of everything except integrations |
| **Writes what** | exactly one block, replaced whole; `{}` is the clear form | the whole document, provider blocks included when the body NAMES them |
| **A block the body does NOT name** | n/a — the route IS the block | **carried forward**, not cleared (`carryForwardUnnamedSiteConfigFields`, [`internal/api/site_config.go`](../../internal/api/site_config.go)). Without this, every 5-minute converge on a laptop whose MDM file predates 0.7.2 would silently delete the org's provider policy |
| **Clearing a block** | `{}` | `{}` on both doors; an explicit `null` clears over raw HTTP but not through `wardyn site-config set`; see [below](#clearing-a-block) |
| **Audit row** | `workspace_provider.write` / `agent_provider.write` — the block's own shape, including `base_urls` in the clear (a provider address is topology, not a credential) and never the `sso_start_url` | `site_config.write`, whose datum carries `git_providers`, `storage_configured`, `agent_providers` and — when the body named `workspace_providers` — `sources_no_longer_admitted`, so an MDM-applied narrowing is reviewable with nobody watching a console |
| **Narrowing is never silent** | the `PUT` response counts the already-onboarded repo sources and library sources the new block refuses; the console renders it on the save toast | the same count, on the response and in `site_config.write` |

**`https://github.com/<org>` bounds HTTPS clones only — SSH is host-level.**

- An SSH clone URL carries no path a base URL can be compared against (`git@github.com:acme/x.git` is not `/acme/x`),
  - so a row scoped to one org admits an SSH clone of ANY org on that host, with the deployment's `ssh-key-<host>` secret.
- That is a documented ceiling of 0.7.2, not an oversight, and it is never silent:
  - the `/admin/providers` screen says it under the row's lanes,
  - and run create puts it on the 201 as a warning (with a `run.provider.admit` audit row) whenever a path-scoped row admits an SSH repository.
- **The remedy is the row's own `lanes` list** — drop `ssh` from a path-scoped row and its addresses bind again, over the one transport that carries a path.
- Dot-segment and percent-encoded paths do NOT reach this question at all: `https://github.com/acme/../evil/repo.git` and `…/acme%2Fevil/…` are refused outright at every admission door and at both write doors, because git and the server would read such a path differently.
- The one escape that is admitted is an Azure DevOps project or repository name, which may carry spaces and most punctuation:
  - every door stores such an address in one spelling — `https://dev.azure.com/acme/Payments Platform/_git/Card Auth (v2).Service` is stored as `…/Payments%20Platform/_git/Card%20Auth%20(v2).Service` —
  - and an escape that decodes to a separator, a dot segment or a control character is still refused.
- An `azure_devops` row may likewise be scoped to such a project (`https://tfs.corp.example/Payments Platform`); names compare as written, case included.
- Such a row cannot name a project whose name holds ``& ' $ ; | < > " ` `` (site-config values refuse them); scope the row to the organisation instead. An Azure DevOps Server host takes these names only when an `azure_devops` provider row names it.

**On the desktop tier this grid has a winner.**

- `wardyn-desktop.sh` re-applies `/etc/wardyn/site-config.json` on every converge tick,
  - so on `a′` — where the developer IS the admin and can open `/admin/providers` — an MDM file that NAMES a provider block overwrites a local console edit within five minutes,
  - and one that omits it leaves the edit standing.
- See [DESKTOP.md § Posture switches are env vars, never site-config](../DESKTOP.md#posture-switches-are-env-vars-never-site-config).

### Clearing a block

- `{}`.
- Over raw HTTP an explicit `null` also clears.
- Through `wardyn site-config set` it does **not** — the CLI strict-decodes into the pointer field and re-marshals it ABSENT under `omitempty`, so `null` in a file reads as "unnamed" and carries forward.
- Use `{}` on both doors and the question never arises.
