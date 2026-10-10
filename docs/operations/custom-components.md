> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Custom components

What an admin controls on a component row, and what a launch leaves behind on the
[policy](../POLICIES.md#custom-components) a run carries.

- A **component** is a named set of destinations and secrets that a run carries beside its policy.
- [POLICIES.md](../POLICIES.md#custom-components) gives the definition and its rules. This section is what an admin controls and what a launch leaves behind.
- A component never enforces anything itself. At launch its hosts join the run's allowed domains and each secret becomes a grant, so every later check is the one policy already has.

| Source | Written by | Who may attach it |
|---|---|---|
| Organisation | an admin: `GET`/`PUT`/`DELETE /components` | a person granted its id, under the `component` capability |
| Saved | the person: `/me/components` | its owner, while the `custom_component` feature allows |
| Inline | the run request: `components[].inline` | the person who wrote it, under the same feature |

- A secret reaches the run in one of three ways:
  - `header`: the egress proxy adds it to requests for one host, and the sandbox holds no copy;
  - `env`: a variable in the sandbox environment, for the whole run;
  - `file`: a file under `/run/wardyn/secrets`, for the whole run.
- Variable and file delivery put the value where any code in the sandbox can read it. [CREDENTIALS.md](../CREDENTIALS.md) states what each leaves readable and what bounds it.
- A `header` delivery names a bare host with no port, and the header goes to that host's standard TLS port only.
- The proxy terminates TLS for each such host, because it can add a header only to a request it can read.

**The `components` settings.**

- They are the `components` block of the site configuration (`PUT /site-config`; [`internal/types/site_config.go#ComponentSettings`](../../internal/types/site_config.go)).
- A deployment with no block reads every field at its default, and `PUT` stores an all-default block as none.
- Each change is audited: `site_config.write` carries `components_require_vault`, `components_deny_resident_delivery` and `components_autonomy_cap`.

| Field | Default | What it does |
|---|---|---|
| `require_vault_for_credentials` | `false` | Off, a component's header credential does not raise the run's confinement floor. On, it floors the run to `CC3` as any `api_key` grant to a host outside the coding-agent baseline does, for organisation and person components alike |
| `deny_resident_delivery` | `false` | On, a run carrying a component with `env` or `file` delivery is refused with `component_resident_delivery_denied`, deployment-wide and for every source. `header` delivery keeps working. Saved components are unchanged |
| `autonomy_cap` | `""` (no cap) | `L1` holds the run's tool calls; `L0` refuses an unattended run. Applies to a run carrying a component its launcher defined, inline or saved, with or without a governance profile. The cap only tightens, and any other value is a `400` (`site_config_invalid`) on `PUT /site-config` |

- An organisation's component never counts toward `autonomy_cap`.
- With no cap set, a run with a self-defined component is only marked: `run.create` carries `self_added_reach`.
- The refusal is `component_autonomy` ([Every denial that isn't a 404](denials.md#every-denial-that-isnt-a-404)).

**Who may attach one.**

- The `component` capability narrows which organisation component a person may attach (`value` is its uuid).
  - A new organisation component is created restricted, in the same transaction as its row. Nobody may attach it until an allow row names its id; a wildcard allow does not list anyone.
  - Deleting it keeps the restriction, writing it back (audited `capability.availability.write`) if it had been lifted.
  - Lifting the restriction of an id that is no organisation component is a `404` `component_not_found`.
  - A held availability change that lifts a component deleted since it was proposed cannot apply. It stays pending until someone rejects it.
- The `custom_component` value of the `feature` capability gates defining a component of one's own, saved or inline.
  - It is on for everyone until a deny row names it. Once `feature` is enforced, a person needs an allow row for `custom_component` or `*`.
  - Deleting one's own saved component is never refused for want of it.
- A person below the admin tier never reads an organisation component they are not granted. A refused id and an absent one answer with the same bytes.

**Saving.**

- Every save answers `requirements[]`: one row per secret the component names, `present` or `missing`, with the fix `add_secret` for a missing one. It lists names and verdicts only, never a header or config value, and never refuses a save.
- A person's component is looked up in their own namespace; an organisation's `shared` secret in the operator's.
- A person holds at most 32 saved components, the organisation 256.

**One credential per host.**

- The proxy keys an injected credential by bare host, so a host carries one credential on a run.
- A component's header host that already has one is refused `component_host_collision`.
- For every run, with or without components, two credentials bound to one host are refused `credential_host_collision` at create, Review and the policy preview, and dispatch checks again.
- On the per-person Azure DevOps lane a redirect for a lane host loses its token instead ([Egress redirects: two tiers](../OPERATIONS.md#egress-redirects-two-tiers)); a component header or policy `api_key` on a lane host is refused.

**What a launch leaves behind.**

- `run.create` carries `components`, and `run.component.attach` is written once per component. A refused launch writes `run.component.refuse`. A component that widened egress writes `run.egress.add` with `kind` `component` ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).
- Audit rows never carry a person's component hosts, header name or secret names: they carry the ordinal, shape and counts. An organisation's component keeps its name in the row.
- The run's resolved policy and grants are the run's own record. A finished run keeps the hosts and secret names it used, as it keeps any other policy host, and so do its `run.env_secret.resolve`, `run.file_secret.resolve` and `secret.read` rows.
- Migration `0136_components` holds saved components; a split-role install grants the app role `SELECT, INSERT, UPDATE, DELETE` on `components`.
- Migration `0137_run_components` holds each run's snapshot; grant the app role `SELECT, INSERT, UPDATE` on `run_components`, and keep it in backups.
- A revive, restart or end extension re-checks the owner's doors from that snapshot. An organisation component that was deleted refuses it with `component_gone`; one no longer granted, or a `custom_component` no longer allowed, refuses with the capability reason.

**An organisation's shared secret.**

- A `shared` secret is the operator's credential, read from the operator's namespace and used by a member's run. Only an organisation's component can carry one, and only as a header.
- Its name is withheld from a caller whose audit reads are narrowed to one run: the run's owner, an API token, an SSH-authenticated caller.
  - They read the run's audit rows, its grant list, the component view and a policy export without the name.
  - A refusal relayed into the sandbox does not name it, and the `secret.read` row targets the grant id.
- `admin` and `security_admin` read rows as recorded: the audit read, the run's policy view and its export.
- `run.policy.resolve` and `credential.mint` are recorded whole, so the sinks and the partition export carry the name.
- Not covered: operator secret names that already reached a run's owner on rows unrelated to components, such as `run.artifact.redirect`, and `workspace_mounts[].source` on `run.policy.resolve`.
- A run launched with a shared secret has a policy export that is a record, not a policy to reuse; see [The policy a run got](../OPERATIONS.md#the-policy-a-run-got).

**Erasing a person.** The `components` scope is in [Erasing a person](../OPERATIONS.md#erasing-a-person). Wardyn does not revoke a person's secret at its issuer when a component or a person is erased.
