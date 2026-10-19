> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Org walls: the outermost limit on every run

- The org walls are one site-wide document that bounds every run's effective policy, super admins' runs included.
- They are off until a security admin turns them on. While off, every run resolves exactly as it did before.
- The audit contract is [`docs/AUDIT-ACTIONS.md`](../AUDIT-ACTIONS.md)'s `governance.walls.write`, `governance.walls.enable`, `governance.walls.disable` and `run.break_glass.open` rows.

## What the walls hold

| Knob | Effect while on |
| --- | --- |
| `spec.allowed_domains` | A run reaches only hosts on this list. A policy that allows all egress is read as this list. |
| `spec.denied_domains` | These hosts are added to every run's deny list. |
| `spec.allow_all_egress` | `false` turns allow-all off everywhere. Naming `allowed_domains` implies it. |
| `spec.min_confinement_class` | Every run's confinement floor is raised to at least this class. |
| `spec.eligible_grants` | A credential grant the walls' grants do not cover is dropped from the run's eligible grants. |
| `limits.max_end_ahead_sec` | No run's end may be set further ahead than this, at create or by a later change. |

A knob the walls do not name is left as the layers below set it.

## How they narrow

- The walls meet the result of the profile chain, the deployment default, a stored policy, an inline policy and an operator's own run. They only ever narrow.
- A policy wider than the walls is narrowed with a warning (`the org walls narrowed <knob>`). A run is never refused for having been wider.
- The run's policy record names the layer `walls` for each knob the walls decided.
- While they are on, no one can approve an egress request for a host the walls deny or do not allow. The refusal is `403 org_walls_host`.

## Turning them on

1. Draft the document and send it to `POST /api/v1/governance/walls/dry-run`. Nothing is written.
2. Read the answer. It lists the deployment default, every profile, every stored policy and every assignment the walls would narrow, knob by knob.
3. Send the same document with `"enabled": true` to `PUT /api/v1/governance/walls`. Pass the `ETag` of your last `GET` in `If-Match`.
4. With `WARDYN_GOVERNANCE_SECOND_HUMAN` on, the write is held as a pending change. A second security admin approves it (see [governance-writes.md](governance-writes.md)).

The dry run calls the same function the resolve calls once the walls are on, so its report is the effect.

## Break-glass

- While the walls are on, a run started with the admin token is a break-glass run. It must carry a reason in the `X-Wardyn-Break-Glass-Reason` header, at most 512 characters.
- Without a reason the run is refused with `400 break_glass_reason_required`.
- A break-glass run is not narrowed by the walls. Every other limit still applies, and its end is still capped at `limits.max_end_ahead_sec`.
- The run record carries `break_glass: true`. The `run.break_glass.open` audit row and the run's `run.create` row record the reason.

## Not bound yet

| Gap | Follow-up |
| --- | --- |
| Runner placement, concurrent-run caps and door switches | Planned walls knobs; not held by this document yet. |
| Hosts and grants a launch adds after its policy resolves (workspace and component hosts) | The final launch seal closes this. |
| A queued break-glass run | It starts walled, because the queue re-resolves it without the reason. |
