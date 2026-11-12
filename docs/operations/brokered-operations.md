> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Brokered operations: commands, databases and kubectl

- Three recipes for operational work from a run, with the credential kept out of the sandbox and each change able to wait for a person's yes.
- They use components, tool rules and approvals that already exist. Nothing here is a new switch, except the kubectl row's `restart` access and `approve_each`.
- The recipes are not run in CI. Try each on a test namespace or a test database first.

## Which recipe

| You need | Recipe | The credential lives |
|---|---|---|
| A named command on a jump host or service | [Ops commands through an MCP server](#ops-commands-through-an-organisation-mcp-server) | On the organisation's server |
| A read-only query | [Read-only database access](#read-only-database-access) | On the proxy (HTTP header) or on the server |
| Restart or change a workload | [kubectl: restart, and approve each change](#kubectl-restart-and-approve-each-change) | In the operator namespace |

## Ops commands through an organisation MCP server

- Put the commands behind an organisation's remote MCP server. The server holds the jump-host key, the run holds none.
- An admin adds the server as an organisation component (`mcp.transport` `http`, an `https` `url`, its host among the component's `hosts`). Its secret goes to the server as a header over TLS and never into the sandbox.
- Set `mcp.gateway_audience` on that component. The proxy then sends the server a signed token in `X-Wardyn-Identity` and a fresh id in `X-Wardyn-Call-Id` on every attempt. The claims are in [the claims a gateway receives](ai-gateway.md#the-claims-a-gateway-receives).
- Run identity is turned on per provider first, and only an organisation's remote server may set the audience ([setup step 6](ai-gateway.md#set-it-up)).

**What the server must do.**

| Rule | Why |
|---|---|
| One tool per command (`ping`, `query_status`, `restart_service`) with typed parameters | A person approves a call by its exact arguments, so a short typed list is readable |
| Run only a fixed command template for each tool, with no shell | A free-form command string turns every tool into a shell |
| Verify `X-Wardyn-Identity` on every call, then strip it and `X-Wardyn-Call-Id` | A sandbox that reaches the server another way can send any value in either header |
| Take the person from `sub` and the run from `wardyn_run`, never from a parameter | The token's claims come from Wardyn, not from the request or the sandbox |
| Log the call id, `sub`, the tool and the result | The call id joins the server's log to the run's audit rows |
| Give the jump-host account only what the tools need | The server is the boundary, and its account is what a stolen call would use |
| Keep each write in a tool of its own, apart from the read tools | Rules are set per tool name |

**What the server must not do.**

- Accept a command string, a script or a host name as a parameter.
- Return a credential, key or token in a result: results reach the agent, the terminal recording and any host the run may reach.
- Trust a header it did not verify, or a caller that skipped the token check.
- Offer raw onward SSH. That is out of scope for this recipe.

**Rules for the tools.**

1. Name every tool in the policy's `tool_rules` by its exact name, `mcp__<server key>__<tool>`.
2. Set read tools to `allow`: no card, still recorded.
3. Set write tools to `approve_each`: a person approves each call with its exact arguments, and the proxy sends exactly that call, once.
4. Set the tools you do not want called to `deny`.
5. Leave `*` alone unless you mean it: it sets the default for the harness's own tools too.

- `approve_each` applies to one tool of a remote server and never to a pattern or to `*`. A call batched with others, arguments over 32 KiB and a repeated key are refused before anyone is asked.
- A declined call stays declined for the rest of the run. The run's owner decides, once.
- More on the effect: [`tool_rules`](../POLICIES.md#tool_rules--toolrule).

**What the audit shows.**

- The allow row (`brokered:mcp`) names the tools of the `tools/call` messages in `cause`, as `tool "restart_service"` or `tools "a", "b"`. A batch lists its first 8 names, then `and N more`.
- The row never carries arguments or output.
- The approval (`approval.decide`) holds the tool, the server key, `call_sha256` and the decision. The exact arguments are on the approval.
- The server's log has the output. Join them with `data.gateway_call_id` from `wardyn audit <run id> --json`.
- A recorded terminal shows what the agent saw.

## Read-only database access

- Read-only is a property of the database, never of a filter in front of it. Give Wardyn's side a role that has `SELECT` only and runs read-only transactions.
- A statement filter is not a control: a `SELECT` can call a function that writes. Wardyn does not parse statements, and this recipe does not ask it to.

| The database speaks | Use | How the credential is kept out |
|---|---|---|
| An HTTP SQL API | An organisation custom component with `delivery.mode` `header` on the API host | The proxy adds the header to requests for that host. The sandbox holds no copy |
| Its own wire protocol | An organisation MCP server with a `query` tool, as above | The server holds the connection. A `header` delivery reaches HTTP requests only, and a wire protocol has no such header |

- A `header` delivery names a bare host with no port, and the header goes to that host's standard TLS port only. The host carries one credential on a run ([custom components](custom-components.md)).
- On the server, give the `query` tool one parameter, the statement, and run it as the read-only role. Cap rows, bytes and time.
- Rows the query returns are readable by the agent. Do not point the role at tables the run may not see.
- A read tool is `allow`. Make a database that must be written a separate tool and `approve_each`, as above.

## kubectl: restart, and approve each change

- A kubectl component row reaches a registered cluster's namespaces through the proxy with a 10-minute service-account token the sandbox never holds. Turn the kind on with `WARDYN_KUBECTL_CLI` ([details](../ENV.md#wardyn_kubectl_cli)).
- The door does not allowlist verbs or resources. The service account's RBAC does, so write one Role per component row.

| `access` | The door admits | Grant the account (per listed namespace) |
|---|---|---|
| `read` | `GET` | `get`, `list`, `watch` on the resources the run may read |
| `restart` | `GET`, and the one patch `kubectl rollout restart` sends to a named deployment | The same reads, plus `get`, `patch` on `deployments` in `apps` |
| `write` | Any write method on a namespaced resource | The verbs and resources the run needs, and no more |

- The door refuses Secrets, `exec`, `attach`, `port-forward`, token requests and impersonation whatever the Role says. Do not grant them.
- The account also needs `create` on `serviceaccounts/token` for itself ([setup](../ENV.md#wardyn_kubectl_cli)).

A Role and binding for a `restart` row, once per listed namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: wardyn-restart
  namespace: payments
rules:
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: [""]
    resources: ["pods", "events"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: wardyn-restart
  namespace: payments
subjects:
  - kind: ServiceAccount
    name: wardyn-runs
    namespace: wardyn
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: wardyn-restart
```

The row, as `PUT /api/v1/components/{id}` takes it:

```json
{
  "name": "payments-restart",
  "definition": {
    "hosts": [],
    "cli": {
      "kind": "kubectl",
      "kubectl": {
        "server": "https://k8s.example.com:6443",
        "ca_pem": "<PEM bundle>",
        "service_account": { "namespace": "wardyn", "name": "wardyn-runs" },
        "namespaces": ["payments"],
        "access": "restart",
        "approve_each": true
      }
    }
  }
}
```

**What the yes binds.**

- `approve_each: true` is valid with `restart` or `write` only. It holds every `POST`, `PUT`, `PATCH` and `DELETE` the door admits. A `GET` is never held, and a request the door refuses never asks anyone.
- The approval names the cluster, namespace, method, path, query, media type and body. A person sees all of it, and the yes covers exactly that request.
- A different query, such as `dryRun` against a real write, a different media type or a different body asks again.
- A restart is keyed by what it restarts, not by its `restartedAt` time, so running the same `kubectl rollout restart` of that deployment again is the same request and uses the same approval.
- The proxy sends the query and body that were shown, once. A second identical request asks again.
- A declined change stays refused for the rest of the run.
- A body must be JSON, not encoded, and at most 32 KiB. Anything else is refused: protobuf, a YAML apply, a repeated key, a `Content-Encoding`.
- A unit's [deciders](deciders.md) rule for `kube_call` changes who decides (owner, unit admins or a group), may require a second person and caps the scope; an owner the rule leaves out gets 403 `not_a_decider`.
- The approval kind is `tool_call`. Without a rule the run's owner decides it, as for one MCP call. With `WARDYN_CAPABILITY_SECOND_HUMAN` set, someone other than the run's creator must.

**The hold window.**

- A held write waits 90 seconds, under the two-minute timeout a harness gives a shell command.
- Then kubectl prints a `403` naming the approval and the request stays pending.
- Run the same command again once it is approved. A retry joins the pending approval.

**The proxy image.**

- Update the `wardyn-proxy` image before you use `restart` or `approve_each`. An older proxy decodes its config strictly and refuses to start on a config that carries either key ([`WARDYN_PROXY_IMAGE`](../ENV.md#wardyn_proxy_image) names the image).
- An older control plane loads the row but drops `approve_each`. Read the "Before you downgrade" note in the [changelog](../../CHANGELOG.md) before rolling back.

## A pool for one unit

- A cluster or a jump host is often reachable only from some runners. Put those runners in an organisation pool, and let only one unit's runs use it.
- A pool's use policy can name a unit. Set it with `PUT /api/v1/runner-pools/{id}/use-policy`, a security-tier write that a second human holds under `WARDYN_GOVERNANCE_SECOND_HUMAN`.

```json
{ "subjects": [ { "subject_type": "unit", "subject": "<unit id, lowercase>" } ] }
```

- The row admits runs launched in that unit or in a unit below it. A queued start and a revive use the unit the run froze.
- A run in no unit, and a unit that no longer exists, match nothing. A use policy only narrows.
- The write is refused for an id that is not a unit.
- An older binary matches a unit row to nobody, so after a downgrade the pool admits fewer runs, never more.
- Pools in general: [runner pools](runner-pools.md).
