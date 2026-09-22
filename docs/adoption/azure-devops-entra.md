<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Azure DevOps, per-user, on Entra ID

**Applies to:** an Azure DevOps organisation whose sign-in is backed by Microsoft Entra ID
(`dev.azure.com/<org>` or `<org>.visualstudio.com`), with a git provider row set to the `entra` lane.

By default a git provider row shares one credential — an admin connects a personal access token
(PAT) and every run clones and pushes as that one account. On an Entra-backed Azure DevOps
organisation you can set the row's credential source to per-user instead: each person signs in to
Azure DevOps with their own Entra identity, and their runs authorise every clone and every REST call
as themselves. A sandbox never holds that identity's full access — it gets only the capabilities an
administrator has granted, and anything beyond that is held for a decision rather than silently
allowed or silently refused.

## The app registration

There is no new app to create. Wire this into the same app registration Wardyn already uses for
console sign-in — the one the organisation's Entra tenant already trusts and that people already
consent to when they sign in to Wardyn itself.

On that app registration, add **delegated permissions** for the Azure DevOps API resource
(`499b84ac-1321-427f-aa17-267ca6975798`). The scopes to add are the granular `vso.*` permissions that
back the capability ceiling you intend to grant (see [Capabilities](#capabilities) below) — grant only
what the ceiling needs, not every scope Azure DevOps offers:

| Scope | Grants |
|---|---|
| `vso.code` | Read source code, history, branches, and pull requests |
| `vso.code_write` | Push commits, open and update pull requests |
| `vso.code_manage` | Create/delete repositories, manage branch policies |
| `vso.work_write` | Read and update work items and boards |
| `vso.build` | Read build results, definitions, and requests |
| `vso.build_execute` | Queue a build, update build properties |
| `vso.packaging_write` | Read and publish packages and feeds |
| `vso.wiki_write` | Read and update wiki pages |
| `vso.security_manage` | Read and change Azure DevOps permission assignments |
| `vso.serviceendpoint_manage` | Read, create, and manage service connections |
| `vso.pats` | Mint and revoke the signed-in user's own personal access tokens — **add this only if you plan to turn on minted-token mode** ([Bearer or minted token](#bearer-or-minted-token)); a bearer-mode row never needs it |

Also add `openid` and `offline_access` — Wardyn holds a refresh token per person, not a one-time
code, so it can renew an access token at dispatch without asking anyone to sign in again for every
run.

Microsoft's own scope reference flags `vso.code_write`, `vso.code_manage`, `vso.build_execute`,
`vso.packaging_write`, `vso.security_manage`, and `vso.serviceendpoint_manage` as **high privilege**.
That is not a reason to avoid them — a contributor needs `vso.code_write` to push — but it is the
reason the capability ceiling exists as a second, narrower gate on top of the token (see
[Two layers of enforcement](#two-layers-of-enforcement)): granting the scope on the app registration
makes a capability *available* to be granted per row; it does not hand every signed-in person
`security_manage` or `serviceendpoint_manage` just because the app registration can ask for it. Add
only the rows in the table above whose capability you actually intend some row's ceiling to reach —
`vso.security_manage` and `vso.serviceendpoint_manage` in particular are worth a second look before
adding, since they reach permission assignments and service-connection secrets respectively rather
than anything scoped to a single repository.

Grant **administrator consent** for these scopes — an individual user cannot consent to
`vso.security_manage` or `vso.serviceendpoint_manage` on their own, and Wardyn's sign-in expects
consent to already be in place rather than prompting for it mid-run.

Add the callback as a registered **redirect URI**, a **web** platform entry pointing at your own
Wardyn address:

```
https://<your-wardyn-address>/api/v1/scm/azure-devops/callback
```

**Nothing new is pasted into Wardyn for this.** The exchange is authorization-code with PKCE, and it
reuses whatever credential your console sign-in already has: on the usual web-platform registration
that is the client secret already configured for OIDC login (`WARDYN_OIDC_CLIENT_SECRET`), and on a
public-client registration there is no secret to hold at all. Either way the only new configuration is
the tenant and client IDs on the row itself, plus the redirect URI above on the app registration.

## The row your admin adds

An administrator turns this on per git provider row, not globally. The fields that matter:

```jsonc
{
  "base_urls": ["https://dev.azure.com/<org>"],   // your organisation's address
  "lanes": ["entra"],                              // the new credential lane
  "credential_source": "per_user",                 // each person signs in themselves
  "entra": {
    "tenant_id": "<your Entra tenant guid>",
    "client_id": "<the app registration above>",
    "capability_ceiling": ["read", "code_write", "pr", "policy_admin"],
    "default_profile": ["read"],                   // what a run starts with, before any escalation
    "token_mode": "bearer"                          // "minted_pat" is the opt-in fallback
  }
}
```

No secret is pasted onto this row. The tenant and client IDs identify the app registration; the
credential itself is captured per person at sign-in and never touches the row.

**The ceiling is the hard bound; the default profile is where a run starts.** A run may ask for
anything up to the ceiling and have it held for approval; it can never reach past the ceiling at all.
`read` is the recommended default profile — every write, including push, then starts as something a
run has to ask for rather than something it already has.

## What a member sees

Someone whose org path is served by an `entra` row signs in once — a redirect to Azure DevOps'
own login, usually a single consent click or none at all, since they are typically already signed in
to Wardyn through the same tenant. From then on, their clones and their Azure DevOps REST calls (a
pull request, a work-item update, a build queued from a pipeline task) are authorised as *them*, not
as a shared account.

That has two consequences worth knowing before you hit them:

- **Azure DevOps' own audit and push history name the person**, not a shared service account. There
  is nothing Wardyn-side to reconcile — ask Azure DevOps who pushed a commit and it answers with the
  real person.
- **A repository they cannot read answers `TF401019`** — Azure DevOps' own "you do not have
  permission" response, not a Wardyn refusal. Per-user authorization is enforced by the forge itself,
  for free, the moment the bearer belongs to a real person instead of an admin's shared token.

## Capabilities

The capability ceiling is expressed in plain, purpose-shaped terms, not raw `vso.*` scopes — the row
above grants some subset of these, and a run can never be handed one the ceiling does not list:

| Capability | What it allows |
|---|---|
| `read` | Clone, browse history, view work items, boards, builds, packages, and wiki pages |
| `code_write` | Push commits; open and update a pull request |
| `pr` | Act on a pull request beyond opening it — comment, vote, complete a normal (non-bypassing) merge |
| `policy_admin` | Create or change branch policies — required reviewers, build validation, merge strategy |
| `policy_bypass` | Complete a pull request with a policy bypass, or move a protected ref directly, skipping a policy rather than satisfying it |
| `repo_admin` | Create, rename, or delete a repository; change its default branch |
| `security_admin` | Read or change Azure DevOps permission assignments |
| `serviceendpoint_admin` | Read, create, or change service connections |
| `build_execute` | Queue a build; update a build's properties |
| `build_admin` | Create or change a build or release pipeline definition |
| `work_write` | Read and update work items, queries, and board metadata |
| `wiki_write` | Read and create wiki pages |
| `packaging_write` | Read and publish packages and feeds |
| `project_admin` | Create, read, update, or delete projects and teams |

A handful of Azure DevOps surfaces are never reachable through this lane at all, ceiling or no
ceiling: minting or revoking someone else's personal access tokens, managing service hooks, and
installing or removing extensions. Widening the ceiling does not open them.

**A request beyond what the run currently holds is held, not silently allowed or silently refused.**
The person (or an admin) sees it in the Wardyn UI and answers **allow once** — this one request only
— or **allow for this run** — the capability joins what the rest of the run may do without asking
again. Either way, an answer can never reach past the row's capability ceiling: that ceiling is the
one thing nobody, including an admin approving in the moment, can grant past.

## Two layers of enforcement

Two independent things stand between a sandbox and an unwanted write, and it takes both:

1. **The token's own scope, enforced by Azure DevOps.** The access token injected on the wire only
   carries the `vso.*` scopes the row's ceiling maps to — Azure DevOps itself refuses anything the
   token's scope doesn't cover.
2. **The request check, enforced by Wardyn's proxy**, in front of that token. This layer exists
   because token scope alone cannot express "contribute, but not administer": `vso.code_write` — the
   scope an ordinary contributor needs to push — is also what Azure DevOps requires to edit branch
   policies and to complete a pull request with a policy bypass. A token scoped for "push code" is,
   at Azure DevOps' own layer, also scoped for "override the policy that was supposed to stop a bad
   merge." Wardyn's proxy classifies every request by what it actually does, not by what scope
   authorized it, and holds or refuses the ones that don't match what was granted or already
   approved — which is how `policy_bypass` and `policy_admin` end up as their own capabilities even
   though Azure DevOps has no scope that separates them from `code_write`.

**The residual, stated plainly.** Wardyn's classifier recognizes the Azure DevOps REST routes it has
been taught. A request against a route it does not recognize is refused rather than guessed at — it
does not fall back to whatever the token's scope would technically allow. That means a new Azure
DevOps REST API, or a materially reshaped existing one, can require a Wardyn update before a run can
use it. That is a deliberate trade: an unrecognized request being refused is a support ticket; an
unrecognized request being classified wrong in the permissive direction is a policy bypass nobody
saw coming.

## Why not device-code sign-in

Wardyn's Azure DevOps sign-in is an ordinary browser redirect, never the device-code flow. Two
reasons, both load-bearing:

- **Microsoft's own Conditional Access guidance recommends blocking device-code sign-in** by default
  across an estate, precisely because it is a favored phishing vector — the flow gives an attacker a
  code to relay to a real user with no way for that user to see what they are actually approving.
  Building a feature that leans on a flow Microsoft tells its own customers to block is not a trade
  worth making.
- **A device code cannot be bound to the Wardyn session that requests it.** The identity binding this
  lane relies on ties the Azure DevOps capture to the same browser session that is already signed in
  to Wardyn (so a sign-in provably belongs to the person driving it, not to whoever happened to redeem
  a code). A device code is entered on a separate, out-of-band device with no such session to bind to.

## Conditional Access and token lifetime

An Azure DevOps access token issued this way is short-lived — on the order of an hour, up to about
ninety minutes — and Wardyn's control plane renews it from the stored refresh token at dispatch,
without the person doing anything, for as long as that refresh token keeps redeeming.

Two things that surface as a sign-in prompt rather than a hard failure:

- **A Conditional Access policy that blocks the control plane's server-side token refresh** shows up
  as a held request inside a run, asking the person to sign in again — not as a run that fails
  outright. The run resumes once they do.
- **Revoking someone's Entra sessions ends their Wardyn access at the next refresh**, not
  immediately. Wardyn holds a refresh token, not a live session; the next time it tries to redeem
  that refresh token, Entra refuses it, and the person is asked to sign in again before their run
  continues.

## Bearer or minted token

Two ways the injected credential can exist, and the honest trade-off between them:

- **Bearer (the default).** The access token is redeemed by the control plane and injected on the
  wire for that one request. It is never written to the sandbox, never stored, and does not outlive
  the request it was minted for. This is the mode to prefer.
- **Minted personal access token (opt-in fallback).** Some tools cannot take a bearer token at all —
  only Basic auth with a PAT. For those, a row can opt into `token_mode: minted_pat`: Wardyn mints a
  scoped, time-bounded PAT through Azure DevOps' own PAT Lifecycle API and injects that instead. The
  honest difference: a minted PAT **exists at Azure DevOps until it is revoked**, unlike a bearer that
  never exists outside the one request it rode in on. It is scoped to the run's own capabilities and
  torn down when the run ends, but it is a real, standing credential for as long as it lives — which
  is exactly why it is the fallback, off by default, rather than the default mode.

Some Azure DevOps organisations restrict or forbid personal access token creation by policy. Where
that is the case, Wardyn reports that policy as the reason a minted-token request was refused rather
than retrying it — it does not attempt to work around an organisation's own PAT policy.

## Azure DevOps Server is out of scope

This lane is for Azure DevOps Services (`dev.azure.com` / `*.visualstudio.com`) only. A self-hosted
Azure DevOps Server does not accept Microsoft Entra tokens — Microsoft's own documentation limits
OAuth 2.0 (device-code, authorization-code, or otherwise) to Azure DevOps Services and directs
on-premises installations to personal access tokens or Windows authentication instead. A Server
organisation keeps using the existing shared-PAT lane; there is no per-user path for it today.

---

**What only your own organisation can confirm.** Everything above describes what Wardyn asks for and
enforces; whether Azure DevOps actually granted what was asked is a fact of your tenant, not
something this document or Wardyn's test suite can promise on your behalf. In particular: **the first
time you stand up an `entra` row, check the granted-scope string on the resulting audit row against
the ceiling you configured.** No Microsoft documentation guarantees the exact contents of a
narrowed-scope token response, and nothing short of your own organisation's token can prove that a
requested subset of scopes came back as a narrower token rather than something wider. If your Entra
tenant enforces additional Conditional Access policies (a compliant-device requirement, a login
frequency policy, a location restriction), how those interact with a server-side refresh is also
something only a real sign-in against your tenant will tell you — this document states how the
common case behaves, not how every policy combination in your tenant will.
