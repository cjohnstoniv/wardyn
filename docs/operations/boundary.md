> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# The Wardyn boundary: the outermost layer of every run

- The Wardyn boundary is Wardyn's outermost layer. Every run's effective policy sits inside it, super admins' runs included, and the walls of profiles sit inside it.
- The organisation's boundary is everything outside Wardyn (your network, firewalls and accounts). Wardyn never sees it.
- The audit contract is [`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)'s `governance.boundary.write` and `run.break_glass.open` rows.

## Modes

- `no_limits` matches the organisation's boundary, so it narrows nothing. An upgraded install stays in `no_limits`, and every run resolves exactly as before.
- A fresh install's first boot starts the boundary in `hard_limits` with nothing listed and floor `CC1`, which reaches what `no_limits` does, so an admin only adds limits. The boot writes `governance.boundary.seed` once. A boundary a person wrote first is kept.
- `hard_limits` is a Wardyn allow and deny list inside the organisation's boundary.

## What the boundary holds

| Knob | Effect in `hard_limits` |
| --- | --- |
| `spec.allowed_domains` | A run reaches only hosts on this list. A policy that allows all egress is read as this list. |
| `spec.denied_domains` | These hosts are added to every run's deny list. |
| `spec.allow_all_egress` | `false` turns allow-all off everywhere. Naming `allowed_domains` implies it. |
| `spec.min_confinement_class` | Every run's confinement floor is raised to at least this class. |
| `spec.eligible_grants` | A credential grant the boundary's grants do not cover is dropped from the run's eligible grants. |
| `limits.max_end_ahead_sec` | No run's end may be set further ahead than this, at create or by a later change, including a run created in `no_limits`. |

A knob the boundary does not name is left as the layers below set it. `contact` names who owns the boundary and how to ask for a change.

## How it narrows

- The boundary meets the result of the profile chain, the deployment default, a stored policy, an inline policy, an operator's own run and a Record Mode session. It only ever narrows.
- A Record Mode session learns inside the boundary: its allow-all becomes the boundary's allowlist, the boundary's denies are added and its floor is raised. It is never a break-glass run.
- A policy wider than the boundary is narrowed with a warning (`the Wardyn boundary narrowed <knob>`). A run is never refused for having been wider.
- The run's policy record names the layer `boundary` for each knob the boundary decided.
- At dispatch and revive the final seal meets the run's assembled egress with the boundary again, after every host a launch adds (workspace, clone, component, model and source-control hosts). A host outside its allowlist is cut and its denies are added. If the boundary cannot be read there, the run is sealed to no egress.
- The seal keeps an explicit `allow_all_egress: false` on its own: with no `allowed_domains` it turns allow-all off and keeps the run's own allowlist, so a paused run that allowed all egress is revived closed.
- In `hard_limits` no one can approve an egress request for a host the boundary denies or does not allow. The refusal is `403 boundary_host`.

## Credentials withheld

After the seal, at dispatch and at revive, every credential lane that reaches a host the boundary denies or does not allow is withheld:

- the git broker, when the boundary withholds any host it re-originates to (`github.com`, `api.github.com`, `codeload.github.com`, `*.githubusercontent.com`), with the grant id in the sandbox's env;
- the PAT broker, per host; the GitHub CLI and Azure CLI doors; the Bedrock lane;
- each header injection, a component's interception entry and an MCP server with its harness bindings.

A revive whose git broker or Azure CLI the boundary no longer allows is refused (`409`); start a new run.

`spec.eligible_grants` binds every grant a run holds, the ones its launch adds included (model provider, workspace, integration and source-control credentials). A grant outside it is dropped at dispatch with its injection, its broker entry and its env or file secret. The run's `run.ceiling.reassert` row carries `boundary: true` and the `withheld_grants` ids, also for a run no profile applies to.

A captured AWS SSO session held in the sandbox (`WARDYN_AWS_SSO_PROXY_INJECT` off) is judged as the `api_key` grant the proxy-injected session gets: its portal host, `x-amz-sso_bearer_token` and `aws-sso-access-token`. So a boundary admits or withholds it alike under either setting. A withheld session's Bedrock lane is dropped and listed in `dropped_broker_lanes`.

The mint and redeem doors check the same rules live, before anything is minted: a grant the boundary or the run's profile now withholds is refused with `403 credential_withheld` and a `credential.mint` denied row. If the run, its grants, its profile or the boundary cannot be read, the answer is `503` (`credential_withheld_unread` at mint; at redeem `run_unreadable` or `store_unavailable`, with a `secret.read` failure row); retry. The AWS SSO renewer skips a grant it withholds.

## Changing it

1. Draft the document and send it to `POST /api/v1/governance/dry-run`. Nothing is written. The dry run judges the draft as `hard_limits`, whatever its `mode`.
2. Read the answer. It lists the deployment default, every profile, every stored policy and every assignment the boundary would narrow, knob by knob.
3. Send the document to `PUT /api/v1/governance/boundary`. Pass the `ETag` of your last `GET` in `If-Match`.
   - `PUT` replaces the whole document. `mode` is required (`400 boundary_invalid` without it), so a spec-only edit cannot change the mode.
4. With `WARDYN_GOVERNANCE_SECOND_HUMAN` on, the write is held as a pending change. A second security admin approves it (see [governance-writes.md](governance-writes.md)).

The dry run calls the same function the resolve calls in `hard_limits`, so its report is the effect.

## Break-glass

- The boundary binds every run: the admin token's, local mode's and super admins' included. Break-glass lifts only a profile's walls, never the boundary.
- An admin-token run is a break-glass run when the boundary is in `hard_limits` and at least one governance profile exists. It must carry a reason in the `X-Wardyn-Break-Glass-Reason` header (`wardyn run --break-glass-reason`), at most 512 characters.
- Without a reason the run is refused with `400 break_glass_reason_required`, and so are its preflight and policy preview. With no profile the run lifts nothing, so no reason is asked and the run is not tagged.
- The run record carries `break_glass: true`. The `run.break_glass.open` audit row and the run's `run.create` row record the reason.
- In `hard_limits`, a run whose confinement class is below the boundary's floor is not dispatched, and a paused one is not revived (`409 confinement_class_conflict`). The run-limit sweep holds every live run to `limits.max_end_ahead_sec`, profiled or not, a break-glass run's included, and a later change to a run's end is held to the maximum as it stands then.

## Not bound yet

| Gap | Follow-up |
| --- | --- |
| Runner placement, concurrent-run caps and door switches | Planned boundary knobs; not held by this document yet. |
| A model credential the deployment holds, not a grant (a host-configured Bedrock key) | Withheld by its host only; `eligible_grants` cannot name it. |
