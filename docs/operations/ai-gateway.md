> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# An external AI gateway: setup

- An AI gateway such as [agentgateway](https://agentgateway.dev) runs beside Wardyn and serves model and MCP traffic for every caller. [Pluggability](../PLUGGABILITY.md#an-external-ai-gateway-for-example-agentgateway) says where it fits.
- This page is the intended setup: the config, the hostnames and the Wardyn settings that go with them.
- LiteLLM, Portkey, OpenRouter and Presidio have their own page: [Model gateways and detectors](model-gateways.md).

## What the setup is

| Part | What it is |
|---|---|
| Gateway | The unmodified agentgateway image, one pod, one HTTPS listener on 8443 |
| Model hostname | `llm-gw`: serves the Anthropic Messages API for a `custom_endpoint` provider |
| MCP hostname | `mcp-gw`: serves MCP federation, with one target |
| Key set | The gateway fetches wardynd's key set from the in-cluster Service address, so wardynd must serve it before the gateway starts |
| Behind it | [`test/gatewayfake`](../../test/gatewayfake): a canned model reply, one MCP tool and a `/_seen` listing of what reached it |
| Credential | A value the job makes up. The fake sits behind the gateway, so no provider key exists in the job |

## Two hostnames, not one

- A component may not reach a host that serves a model on the deployment. Wardyn refuses it as `component_host_serves_model` ([`internal/api/components_run.go`](../../internal/api/components_run.go)).
- An MCP gateway on the model route's host could therefore not be attached as a component.
- Give the model route and MCP federation a hostname each. They can share one gateway pod and one certificate that names both.

## Set it up

1. Serve the gateway over `https://`. A provider's `base_url` must be `https://`, and the certificate must name every hostname the gateway serves.
2. Give wardynd the certificate authority. Set the Helm value `trustedCA`, or `WARDYN_TRUSTED_CA_FILE` outside Helm. The run's proxy and the sandbox's own TLS clients then trust it.
3. Add a `custom_endpoint` provider whose `base_url` is the model hostname and port, for example `https://llm-gw.wardyn.svc.cluster.local:8443`. A person's credential is stored as for any provider.
   - Two providers with identity on can share one gateway only on paths apart, such as `/anthropic` and `/openai`. A run that uses both on one path is refused.
   - A Bedrock provider with a bearer key can send identity too. Put the gateway in its `bedrock.base_url`, and each person's key goes to the gateway.
   - A Bedrock provider with SSO sign-in can send identity only while Wardyn signs AWS requests at its proxy: [Bedrock with SSO sign-in](#bedrock-with-sso-sign-in).
4. Add the model hostname to the policy's `allowed_domains`. The model route needs no `internal_hosts` entry, and adding one lifts the private-address guard for that host on every path ([Internal model gateway](../OPERATIONS.md#internal-model-gateway)).
5. Allow the MCP hostname in the run's policy, and add it to `internal_hosts` when it names a private address. Wardyn then governs whether a run reaches the gateway, and the gateway decides which tools a caller gets.
6. To have the gateway know which run and person made an MCP call, set `mcp.gateway_audience` on the organisation's MCP component (its `url` is the gateway's MCP address). Run identity is turned on per model provider and makes the signing key, so turn it on for a provider first.
   - Only an organisation's remote server may set it: a host a person names could otherwise be sent a token good at a real gateway.
   - One audience belongs to one address, across model providers and MCP servers.
   - An MCP component edited after a run started is refused a new token for that run, as a changed provider is.
   - An MCP server whose address falls under another destination's identity route is refused (`502`) rather than sent that destination's token.

## Bedrock with SSO sign-in

A `bedrock_sso` provider can send run identity to a gateway, and AWS still checks each person's own signature. It is off unless an admin turns `gateway_identity` on for the row.

- **It needs** `WARDYN_AWS_SIGV4_AT_PROXY` on, with `WARDYN_AWS_SSO_PROXY_INJECT` on and `WARDYN_AWS_SSO_ENDPOINT_OVERRIDE` unset. Otherwise turning identity on for the row is refused at save. `gateway_identity_bedrock_sso` in `GET /api/v1/model-providers` says whether this install can.
- If the install stops signing at the proxy after the save, the row's runs are refused with the reason. They never fall back to role credentials in the sandbox.
- **Set** `bedrock.base_url` to the gateway, for example `https://ai-gw.corp.example/bedrock`, and `bedrock.region` to the region whose `bedrock-runtime.<region>.amazonaws.com` the gateway forwards to.
- **Give the gateway a DNS name of your own.** A `bedrock.base_url` on any AWS name is refused, so the token never goes to AWS. That includes a PrivateLink `vpce` name and a name AWS made for the gateway, such as an API Gateway `execute-api` name or a load balancer's `elb` name.
- The proxy signs each Bedrock call as the person, for that regional address with the gateway's path prefix cut. It sends the call to the gateway with `X-Wardyn-Identity` and `X-Wardyn-Call-Id`. The role credentials stay in the proxy, and the sandbox holds a placeholder key pair.

The gateway must:

- verify the token, then strip `X-Wardyn-Identity` and `X-Wardyn-Call-Id`;
- cut its path prefix, set `Host` to `bedrock-runtime.<region>.amazonaws.com` and forward the call there;
- forward the method, the path bytes, `Authorization`, `X-Amz-Date`, `X-Amz-Security-Token`, `X-Amz-Content-Sha256`, every other signed header and the body unchanged. It must not decode or normalise the path: a `%2F` in an inference-profile ARN stays `%2F`.

The body is signed:

- A call goes to the gateway only with its body hash signed. The proxy refuses `UNSIGNED-PAYLOAD`, a streaming unsigned payload, and a declared hash that does not match the body.
- A body over 16 MiB is refused, because the proxy reads the whole body to hash it.

Limits:

- Bedrock's bidirectional-stream API is refused, because the proxy refuses event-stream request bodies on every call it signs. Converse, InvokeModel and the response-stream calls work.
- Control-plane calls to `bedrock.<region>.amazonaws.com` go to AWS directly, signed at the proxy, not through the gateway.
- A signed Bedrock call gets the generic forward scan (`inspect_forward_egress`), as direct signing at the proxy does, not the model prompt checks.
- A run on a person's own runner is refused, because that runner cannot reach the token mint.
- A gateway that signs again with an AWS role of its own also works, but AWS then sees the gateway's role, not the person.
- The signature was checked against an independent SigV4 signer, not against AWS or a named gateway product.

What the gateway gets:

- Each call signed as the person: the prompt, the access key id, the session token and the signature, never the secret key.
- It cannot change the call or send it to another AWS service or region, because those are signed.
- It can send the same call again, more than once, for a few minutes after it was signed.
- A gateway in front of a bearer-key row gets the person's long-lived key instead.

## The gateway's config

| Setting | Why it is there |
|---|---|
| `hostnames` on each route | The two routes share one listener and are told apart by the name the client asked for |
| `policies.ai.routes`: `"/v1/messages": messages` | Without the table the gateway reads the request as a chat completion and answers in that shape, which Claude Code cannot read |
| `"/v1/messages/count_tokens": anthropicTokenCount` | The harness asks for token counts, and the gateway answers them from the upstream |
| `"*": passthrough` | Every other path is forwarded to the provider's host untouched |
| `ai.hostOverride` | Where the provider's calls go: the fake here, a vendor's host in a real setup |
| `mcp.targets` | The servers behind the one MCP endpoint a client sees |
| `policies.jwtAuth` on the MCP route: `mode: strict`, `location.header.name: x-wardyn-identity`, `providers[].issuer`, `audiences` (the component's `mcp.gateway_audience`), `jwks.url` | Verifies the same token on MCP calls. `wardyn_mcp` names the server |
| `policies.jwtAuth` on the model route: `mode: strict`, `location.header.name: x-wardyn-identity`, `providers[].issuer`, `audiences`, `jwks.url` | Verifies the run's identity token (below). `preserveToken` is left unset, so the gateway consumes the header and the upstream never receives it |
| `frontendPolicies.accessLog.add`: `jwt.sub`, `jwt.wardyn_run`, `request.headers["x-wardyn-call-id"]` | Puts the subject, the run id and Wardyn's call id on each access-log line |

## What the gateway does with a request

The setup was checked against the pinned image in a one-container trial. These are the v1.7.0 behaviours it relies on.

| Request | Answer |
|---|---|
| `POST /v1/messages`, streamed or not | A Messages answer in the same shape, with the gateway's own `protocol=llm` log line |
| A bearer `Authorization` header | The upstream receives it as `x-api-key`, the form an Anthropic host expects |
| A path other than the two the table names (the `*` row) | Forwarded to the upstream, which answers it: a `404` from the fake for a path it does not serve |
| An MCP call with no session | `400`. The client sends `initialize` first and repeats the `Mcp-Session-Id` it gets back |
| An MCP call that does not accept both `application/json` and `text/event-stream` | `406` |

- The job saves the fake's `/_seen` listing, which names every path a harness reached outside the model route's own. It is evidence for what the gateway forwards and for the sentence in [Pluggability](../PLUGGABILITY.md#an-external-ai-gateway-for-example-agentgateway) about the other paths. The job does not assert on it.

## What the job asserts

| Assertion | Read from |
|---|---|
| The proxy let the model call through to the model hostname | The run's audit rows: `egress.allow` on that host |
| The proxy let the tool call through to the MCP hostname | The run's audit rows: `egress.allow` on that host |
| The gateway served each route | Its access log: a `route=default/llm` and a `route=default/mcp` line |
| The made-up credential reached the model route, and the tool ran once | The fake's `/_seen` |
| The gateway validated the token from the live key set: its model line names the run's subject (`admin-token`) and run id | The gateway's access log: `wardyn_sub` and `wardyn_run` |
| The identity header did not reach the fake: the gateway consumed it | The fake's `/_seen`: no `x-wardyn-identity` on a model request |
| The gateway refuses a call with no token, and a call with a token another key signed | A direct `401` for each |
| Wardyn's call id is on both logs | A `gateway_call_id` on the model run's `egress.allow` row equals a `wardyn_call` in the gateway's log, at least once |
| The job reached its own closing line | `kind-gateway-run: PASS`, gated on exit codes |

- A run's exit code is not an assertion for the model run: Claude Code's exit says nothing about the gateway.
- The tool call comes from a `curl` in an exec run, not from a model.
- The job attaches no component, so it does not exercise the `component_host_serves_model` refusal above.
- The model provider has run identity on, with the audience `wardyn-ai-gateway-job`. The job holds no person's data: the subject is the quickstart's fixed admin-token seat. Run it by hand on a disposable quickstart cluster with `WARDYN_TEST_K8S=1 WARDYN_ACK_SHARED_QUICKSTART=1 scripts/kind-gateway-run.sh` ([the script](../../scripts/kind-gateway-run.sh)).

## The image

The gateway image is the operator's own choice to trust. This setup uses `agentgateway` v1.7.0 unmodified, pinned by digest in [`deploy/kind/gateway/gateway.yaml`](../../deploy/kind/gateway/gateway.yaml).

## If the gateway verifies tokens from a JWKS URL

- With `jwtAuth` and a `jwks.url`, the gateway loads the key set when it reads its config. It then fails to start with `failed to load JWKS` if the URL is unreachable, so serve the key set before the gateway starts. This was run against the pinned image.
- The refresh timings come from reading `crates/agentgateway/src/resource_manager.rs` at the v1.7.0 tag. They were not run.

| Setting | Value read |
|---|---|
| Default refresh (`JWKS_TTL`) | 15 minutes |
| A `Cache-Control: max-age` or `Expires` header | Replaces the default, with a floor of 60 seconds |
| A failed refresh | Retried after 15 seconds |

## The claims a gateway receives

- Experimental until 0.11. The token format (`wardyn_format` is `experimental-1`) and the key set's address may change before then.
- With a provider's run identity on, `wardyn-proxy` sends a signed token in the `X-Wardyn-Identity` header on that provider's gateway route only. It does the same on an organisation MCP server's address when the component sets `mcp.gateway_audience`. It never uses `Authorization`, which carries a person's own credential.
- The token is ES256 with the key's `kid` in its header, and lives 10 minutes unless the provider sets a shorter life (see "How long a token lives"). An MCP server's token always lives 10 minutes. No claim comes from the request or the sandbox. Every claim is in the table.

| Claim | Value | Notes |
|---|---|---|
| `iss` | The trust domain, as `spiffe://<trust domain>` | `GET /api/v1/model-providers` reports it as `gateway_identity_issuer`. Two Wardynds on the default `wardyn.local` domain behind one gateway present one issuer, so set `WARDYN_TRUST_DOMAIN` on each |
| `aud` | The provider's `gateway_identity.audience`, or an MCP component's `mcp.gateway_audience` | Set by an admin. A gateway accepts only the audiences it lists. One audience is never on two addresses, so a model route's token is no good on an MCP route |
| `sub` | The run's subject: a person, or a service identity on a service run | A local install gives a fixed seat. A run started with the admin token gives `admin-token` |
| `wardyn_run` | The run id | Flat, so a gateway rule needs no SPIFFE parsing |
| `wardyn_runner` | The runner's id, else the runner target | An operator-chosen slug, possibly a hostname, or `docker` |
| `wardyn_provider` | The model provider's id | Absent on an MCP server's token |
| `wardyn_mcp` | The MCP server's key, the `<server>` of `mcp__<server>__<tool>` | Present on an MCP server's token alone |
| `sponsor` | The accountable person of a service identity | Present only on a service run |
| `jti` | A fresh UUID per token | |
| `iat` | When the token was issued | Unix seconds |
| `exp` | `iat` plus the provider's token lifetime, 10 minutes unless set | A gateway cannot learn that a run was killed, so a killed run's token is spendable until `exp` |
| `wardyn_format` | `experimental-1` | |
| `wardyn_delegate` | The portal's id | Present only on a run a portal launched for the person; see [api-tokens.md](api-tokens.md) |
| `act` | `{"sub": "spiffe://<trust domain>/agent-run/<run id>"}` | The agent run acting for `sub`, in the run token's shape. On a portal run whose portal named the acting agent it nests that agent: `{"sub": "spiffe://<trust domain>/agent-run/<run id>", "act": {"sub": "<agent>", "iss": "<issuer>"}}` |

- The nested agent is provenance. RFC 8693 section 4.1 says prior actors are informational and not to be used in access control decisions, so a gateway rule that reads `act.act.sub` does so on its own judgement.
- There is no `nbf`. A Wardyn clock ahead of the gateway's drops a token early and one behind gets a `401`: both fail closed.
- A test holds this table equal to the signer's claim struct.

## How long a token lives

- A provider's `gateway_identity.token_lifetime_seconds` sets it, from 60 to 600 seconds. Unset means 600, so an install that sets nothing changes nothing. Any other value is refused when saved.
- A shorter life limits how long a leaked token is good for, and so how long a killed run's token can still be spent.
- The setting applies to tokens issued after a save. A running run keeps working, because the life is not part of the route the run started with.
- `wardyn-proxy` asks for a new token at half the remaining life. If the mint fails, it keeps the held token until `exp`, so a run survives a wardynd outage of about half the lifetime.
- While wardynd is unreachable, the first call after half the life, and then the first in each retry interval, waits up to 15 seconds for that mint before the held token serves it.

| Lifetime | Outage a run survives | A mint, per run and provider, every |
|---|---|---|
| 600 seconds | 5 minutes | 5 minutes |
| 300 seconds | 2 minutes 30 seconds | 2 minutes 30 seconds |
| 120 seconds | 1 minute | 1 minute |
| 60 seconds | 30 seconds | 30 seconds |

- Each mint is about five store reads. At 60 seconds, 500 concurrent runs come to about 83 reads a second.
- A rolling wardynd upgrade longer than the outage a run survives fails that run's model calls, so a short life is the admin's trade.

## When a run's gateway access ends

- Each mint checks the run's person, its service identity and sponsor, the portal that launched it, and the provider or MCP server. A refused mint gives no new token.
- The bound: within half the token lifetime while wardynd is reachable, and up to the held token's expiry if it is not. Requests already forwarded and open streams run to completion.

| Event | A running run's gateway access |
|---|---|
| Person suspended or fully revoked | ends: `403` `identity_deactivated` |
| Service identity suspended or retired | ends: `403` `service_identity_inactive` |
| Sponsor suspended or fully revoked | ends: `403` `identity_deactivated` |
| Sponsor handover | continues; new tokens name the new sponsor |
| Provider or MCP server changed or turned off | ends: `403` `gateway_identity_changed` |
| Portal revoked | continues, as before |
| Portal revoked with `cut_in_flight=true` | ends for good: `403` `gateway_access_cut` |

- A cut stops gateway calls only. The run, its files, its other egress, a model key its proxy already holds and an open terminal socket carry on. `POST /api/v1/runs/{id}/kill` stops the run.
- A token already issued stays valid at the gateway until `exp`, but only the run's proxy holds it. A 60-second `token_lifetime_seconds` cuts within about 30 seconds.
- A revive of a cut run is refused `409` `gateway_access_cut`.

## The key set's address

- The key set is the anonymous `GET /.well-known/wardyn-experimental/gateway-jwks.json` on wardynd, under `WARDYN_BASE_PATH` when that is set.
- The console shows the address built from the browser's own origin. A gateway that reaches wardynd by another address, such as a cluster Service name, uses that address with the same path.
- The gateway reads the key set when it loads its config and does not start if the read fails. Start wardynd first, and save a provider with run identity on, which creates the key.
- With run identity off, every `GET` under `/.well-known/` that names no file in the console's directory answers a plain `404`, this address included. A gateway whose `jwks.url` points here cannot load a key set from it. A file an operator places under `/.well-known/` in the console's directory is still served.

## The TLS chain

A model call crosses three TLS hops. Only the first two are Wardyn's.

| Hop | Who terminates TLS | Who trusts what | What each side sees |
|---|---|---|---|
| 1. Sandbox to `wardyn-proxy` | `wardyn-proxy`, with a leaf from the run's own certificate authority. It terminates only a host it is set to intercept; any other `CONNECT` is an opaque tunnel | The sandbox trusts the run's authority plus the `WARDYN_TRUSTED_CA_FILE` roots | The proxy reads the whole request, the person's bearer included. The sandbox never holds the identity token, and the proxy deletes any `X-Wardyn-Identity` the sandbox sends on a run with a route |
| 2. `wardyn-proxy` to the gateway | The gateway | The proxy trusts the system roots plus the `WARDYN_TRUSTED_CA_FILE` bundle. It presents no client certificate | The gateway sees `wardyn-proxy` as its caller, with `X-Wardyn-Identity`, `X-Wardyn-Call-Id` and the person's bearer |
| 3. The gateway to the model vendor | The vendor | Whatever the gateway's own config says | Nothing on this hop is Wardyn's |

- Identity is a header, not a client certificate. The gateway checks the token's signature against the key set, and nothing at the TLS layer names the caller.
- With an upstream proxy set, hop 2 is a `CONNECT` through it, unless the gateway host is on `upstream_proxy_no_proxy`.

> [!WARNING]
> A TLS-inspecting corporate proxy on hop 2 terminates TLS with its own authority, so it reads `X-Wardyn-Identity` and the person's bearer in clear. To stop that, keep it out of the path with the recipe below. Trusting the inspector's root in `WARDYN_TRUSTED_CA_FILE` makes it a party to every credential on that hop.

To exempt the gateway host from the upstream proxy:

1. Check that the `wardyn-proxy` sidecar can resolve the gateway's name and reach it directly. A bypassed host is dialled by the sidecar itself, not by the corporate proxy. The model route needs no `internal_hosts` entry, as step 4 of the setup says.
2. Add the gateway's host to `upstream_proxy_no_proxy` in site config. A name or a domain suffix matches a hostname; a CIDR matches only an address written as an IP literal.
3. Start a run and confirm the gateway's access log shows the call.

- The list and its matching rules are under "[Upstream proxy: the bypass list](../OPERATIONS.md#upstream-proxy-the-bypass-list-upstream_proxy_no_proxy)".

## Joining a run's audit rows to the gateway's log

Wardyn makes a fresh call id for every attempt to a model gateway route and sends it as `X-Wardyn-Call-Id`. The gateway's access log and the run's audit row then carry the same value.

1. Log the header on the gateway. In agentgateway, add `request.headers["x-wardyn-call-id"]` to `frontendPolicies.accessLog.add`, as the setup above does.
2. Read the run's `egress.allow` and `egress.deny` rows (`wardyn audit <run id> --json`) and take `data.gateway_call_id`.
3. Search the gateway's log for that value. The line is that attempt's, with the token's subject and run id beside it when the gateway logs `jwt.sub` and `jwt.wardyn_run`.

- Only a gateway log line that passed the token check is trustworthy. A sandbox that reaches the gateway through an opaque tunnel can send any `X-Wardyn-Call-Id`, so run the gateway's `jwtAuth` in `strict` mode.
- An attempt refused or failed after the proxy made its id has a row with the id, and no gateway line when nothing was sent. A call refused before that has no id.
- The id is Wardyn's own. It is not a request id the gateway made.
- A run's token counts come from the same answers. Compare one call with the gateway's own figure by its call id.
- An MCP server has a call id. It also gets an identity token when it is an organisation's remote server with `mcp.gateway_audience`, and no token otherwise.

## Who inspects

With content inspection on (`llm_inspection` in the run's policy), Wardyn checks each prompt before it leaves. A gateway that checks prompts too runs the same checks twice. A provider with run identity on can leave them to its gateway: `gateway_identity.inspection: gateway`, the console's "Let this gateway check prompts".

| Check | `inspection` unset | `inspection: gateway` |
|---|---|---|
| The run's own secret values, exact match (`detect_secrets`) | Wardyn | Wardyn |
| Secret formats, entropy, personal data, classified markers, the detector sidecar, attachments | Wardyn | The gateway |

- Only a call on the provider's route is left to the gateway: its host, port and path prefix, the match that carries the token. Another host, a path outside the prefix, a path with a dot segment and the plain HTTP lane keep the whole policy.
- The run's own secret values exist only in Wardyn's memory, so no gateway can look for them. That check stays on whenever the policy has it, and in block mode it still refuses the call.
- With no `detect_secrets` on the policy, Wardyn checks nothing on that route.
- Each prompt left to the gateway has an `llm.scan.skip` row with `data.skip_reason: deferred_to_gateway` and `data.deferred_to` set to the provider id. A secret found there is an alert or block row with the same `deferred_to`. The console's audit reads "Left prompt checks to" the provider, and the run's Policy tab reads "On, except prompts to" its name.
- In block mode the gateway's rules then decide what the other checks would have refused. A policy author who relies on Wardyn's blocks loses them on that route.
- A prompt-bearing path the proxy cannot parse is still refused when the policy blocks on scanner errors.
- A Bedrock provider cannot take the setting, and saving it is refused. Its calls are scanned only by `inspect_forward_egress`, which no gateway takes over.
- The setting is part of the route a run starts with. Changing it refuses the run's next token, so its gateway calls stop within 10 minutes. Create a new run to pick it up.
- Any value other than `gateway` is refused when saved.

## Defaults on a route that carries the token

| Default | Effect |
|---|---|
| The response scrub is forced on | `WARDYN_PROXY_RESPONSE_SCRUB=off` does not turn it off here, so a gateway that echoes the token back cannot hand it to the sandbox |
| `Accept-Encoding` is deleted from the request | The answer arrives in bytes the scrub can read |
| A compressed answer gets a `502` | The proxy refuses bytes it cannot scrub rather than relay them |
| A running run keeps the routes it started with | A run keeps its routes. Once a provider's gateway changes, the mint refuses that run, so its gateway calls fail once its token expires, within 10 minutes at most. Create a new run to pick the change up. |
