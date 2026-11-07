> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Signing in for a CLI component

How a person signs in to an AWS CLI or Azure CLI component, since the console has no button for it yet.

## The routes

Tools and access lists it under **Setup still needed** with no button; send each person to the route in the table.

| Component | How a person signs in | Where it lands |
| --- | --- | --- |
| AWS CLI | `POST /api/v1/components/{id}/sign-in`, which starts a sign-in sandbox seeded with the component's portal, region, account and role | Their own namespace, as `wardyn-cli-<component id>-sso`, audited `cli.signin.capture` |
| Azure CLI | Open `GET /api/v1/components-entra/signin?component=<id>` in the signed-in browser. It returns to `/?azure_cli_signin=connected&component=<id>`, or `/?azure_cli_signin_error=<code>&component=<id>` | Their own namespace, as `wardyn-cli-<component id>-entra`, audited `cli.signin.capture` |

- Azure records a run's Azure CLI calls as the signed-in person's own: its Activity Log keeps the request id, the caller's address and the method, not which client sent them. Match them to a run with the run's egress decisions for `management.azure.com`.
- A run's Azure CLI reaches Azure Resource Manager only. `az ad` commands (Microsoft Graph) and data-plane commands that sign in with `--auth-mode login` fail with `HTTP Error: 403`.
- The Azure sign-in needs the console's Entra application to hold the delegated permission "Azure Service Management · user_impersonation" with admin consent.
- Changing a component's pins, or deleting it, deletes everyone's sign-in for it. There is no per-person disconnect.
