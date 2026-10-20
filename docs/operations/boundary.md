> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# The Wardyn boundary: the outermost layer of every run

- The Wardyn boundary is Wardyn's outermost layer. Every run's effective policy sits inside it, super admins' runs included, and the walls of profiles sit inside it.
- The organisation's boundary is everything outside Wardyn (your network, firewalls and accounts). Wardyn never sees it.
- The audit contract is [`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)'s `governance.boundary.write` and `run.break_glass.open` rows.

## Modes

- `no_limits` matches the organisation's boundary, so it narrows nothing. An install with no boundary row is in `no_limits`, and every run resolves exactly as before.
- `hard_limits` is a Wardyn allow and deny list inside the organisation's boundary.
- `hard_limits` cannot be set yet. A `PUT` that names it answers `409 boundary_hard_limits_gated` until the boundary bounds every brokered credential path a run is given (#3238). A `no_limits` document can be written and dry-run now.

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
- In `hard_limits` no one can approve an egress request for a host the boundary denies or does not allow. The refusal is `403 boundary_host`.

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
| Grants a launch adds after its policy resolves | The final seal bounds hosts only; grants follow with the brokered-credential checks. |
