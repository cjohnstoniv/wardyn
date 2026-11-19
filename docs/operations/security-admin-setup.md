> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Security admin setup: a first-setup checklist

- A checklist for a security admin's first setup, in order. Each step names the page that owns the detail.
- A security admin is a mapped tier: you create one by mapping an IdP App Role or group to `security_admin` in the role map ([three roles](three-roles.md)).
- This page adds no settings of its own. Follow the link in each step for the exact knobs.

## The checklist

1. **Who gets in.** Set the OIDC issuer and the role map, then check who would become what.
2. **The secret store and keys.** Deliver boot secrets from files, and decide where the age key and stored credentials live.
3. **The boundary.** Set the outermost egress limits that every run sits inside.
4. **Audit sinks.** Send the audit stream to your SIEM.
5. **What can't be granted.** Know the limits of your own tier.

## 1. Who gets in

| Do | Owner page |
| --- | --- |
| Map IdP App Roles or groups to `user`, `security_admin` and `admin` with `WARDYN_OIDC_ROLE_MAP` | [who-gets-in.md](who-gets-in.md#who-decides-who-gets-in-chart-vs-console-vs-idp) |
| Know that the highest matching rank wins, and that no match with no default role means the person is denied | [who-gets-in.md](who-gets-in.md#who-decides-who-gets-in-chart-vs-console-vs-idp) |
| Know that `security_admin` is refused as the default role | [who-gets-in.md](who-gets-in.md#who-decides-who-gets-in-chart-vs-console-vs-idp) |
| Preview who a role row would make an admin before anyone signs in | [who-gets-in.md](who-gets-in.md#who-decides-who-gets-in-chart-vs-console-vs-idp) |
| See how long a console session lasts | [who-gets-in.md](who-gets-in.md#how-long-a-console-session-lasts) |
| Read what each of the three roles may do | [three-roles.md](three-roles.md) |

## 2. The secret store and keys

| Do | Owner page |
| --- | --- |
| Deliver boot secrets from files with the `<VAR>_FILE` twins | [secrets-and-keys.md](secrets-and-keys.md#secrets-from-files-vault-agent--csi) |
| Plan age-key rotation | [secrets-and-keys.md](secrets-and-keys.md#rotating-the-age-key) |
| Choose a key service (Vault Transit, Azure Key Vault, AWS KMS) or a store mode for credentials | [secrets-and-keys.md](secrets-and-keys.md#key-service-vault-transit) |
| Know the audit receipt key and what to do if it is stolen | [secrets-and-keys.md](secrets-and-keys.md#the-audit-receipt-key) |

## 3. The boundary

| Do | Owner page |
| --- | --- |
| Read the modes: a fresh install starts in `hard_limits` with nothing listed and floor `CC1` | [boundary.md](boundary.md#modes) |
| Set the egress limits and the confinement floor (`allowed_domains`, `denied_domains`, `min_confinement_class`) | [boundary.md](boundary.md#what-the-boundary-holds) |
| Change it: dry-run first, then `PUT`; a second security admin approves when the switch is on | [boundary.md](boundary.md#changing-it) |
| Know the private-IP guard: its audit rule source is `builtin:private-ip` | [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md) |
| Know where a declared internal host lifts the guard, and what stays closed | [hybrid-laptops.md](hybrid-laptops.md) |

## 4. Audit sinks

| Do | Owner page |
| --- | --- |
| Point `WARDYN_AUDIT_SINKS` at your SIEM; sinks are the event stream, metrics carry no per-run detail | [monitoring.md](monitoring.md) |
| Look up every audit action, including `governance.boundary.write` | [AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md) |

## 5. What can't be granted

| Limit | Owner page |
| --- | --- |
| A security admin cannot reach into a run, record a workspace session or touch host paths beyond keeping, read-only-ing or removing a mount | [three-roles.md](three-roles.md#security-admin-security_admin) |
| A security admin cannot touch the People page, integrations, site-config writes, base images or the deploy funnel | [three-roles.md](three-roles.md#security-admin-security_admin) |
| No capability grant can widen anyone to admin | [three-roles.md](three-roles.md#security-admin-security_admin) |
| The boundary binds every run, including super admins' and break-glass runs | [boundary.md](boundary.md#break-glass) |
| Some subjects never sign in (`admin-token` and the local operator among them) | [who-gets-in.md](who-gets-in.md#who-decides-who-gets-in-chart-vs-console-vs-idp) |
| Which writes are admin-only | [admin-only-writes.md](admin-only-writes.md) |
