> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# An external AI gateway: setup

- An AI gateway such as [agentgateway](https://agentgateway.dev) runs beside Wardyn and serves model and MCP traffic for every caller. [Pluggability](../PLUGGABILITY.md#an-external-ai-gateway-for-example-agentgateway) says where it fits.
- This page is the intended setup: the config, the hostnames and the Wardyn settings that go with them. Its image is `agentgateway` v1.7.0, pinned by digest in [`deploy/kind/gateway/gateway.yaml`](../../deploy/kind/gateway/gateway.yaml).
- **Status.** The nightly job `ai-gateway-kind` runs this setup and is new. It has no scheduled green run yet, so nothing below is claimed as tested by CI until it has one.

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
4. Add the model hostname to the policy's `allowed_domains`. The model route needs no `internal_hosts` entry, and adding one lifts the private-address guard for that host on every path ([Internal model gateway](../OPERATIONS.md#internal-model-gateway)).
5. Allow the MCP hostname in the run's policy, and add it to `internal_hosts` when it names a private address. Wardyn then governs whether a run reaches the gateway, and the gateway decides which tools a caller gets.

## The gateway's config

| Setting | Why it is there |
|---|---|
| `hostnames` on each route | The two routes share one listener and are told apart by the name the client asked for |
| `policies.ai.routes`: `"/v1/messages": messages` | Without the table the gateway reads the request as a chat completion and answers in that shape, which Claude Code cannot read |
| `"/v1/messages/count_tokens": anthropicTokenCount` | The harness asks for token counts, and the gateway answers them from the upstream |
| `"*": passthrough` | Every other path is forwarded to the provider's host untouched |
| `ai.hostOverride` | Where the provider's calls go: the fake here, a vendor's host in a real setup |
| `mcp.targets` | The servers behind the one MCP endpoint a client sees |
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

- The gateway image is the operator's own choice to trust. This setup takes it unmodified and pins it by digest.
- Trivy 0.70.0 (`trivy image --severity CRITICAL`, run on a `docker save` of the digest) found no CRITICAL findings on the base layers on 2026-11-02. It cannot see the Rust binary's crates.
- For the crates, `Cargo.lock` at the v1.7.0 tag lists 804 registry packages. Querying them against OSV (`api.osv.dev/v1/querybatch`) flags 6.
  - The flagged `rmcp` 0.10.0 has six advisory ids. `crates/agentgateway/Cargo.toml` at the tag declares it under `[dev-dependencies]`, so it appears not to be linked into the binary.
  - The other five (`atomic-polyfill`, `crypto-bigint`, `ctutils`, `paste`, `pulldown-cmark`): whether they are linked is not determined.

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
- With a provider's run identity on, `wardyn-proxy` sends a signed token in the `X-Wardyn-Identity` header on that provider's gateway route only. It never uses `Authorization`, which carries a person's own credential.
- The token is ES256 with the key's `kid` in its header, and lives 10 minutes. No claim comes from the request or the sandbox. Every claim is in the table.

| Claim | Value | Notes |
|---|---|---|
| `iss` | The trust domain, as `spiffe://<trust domain>` | `GET /api/v1/model-providers` reports it as `gateway_identity_issuer`. Two Wardynds on the default `wardyn.local` domain behind one gateway present one issuer, so set `WARDYN_TRUST_DOMAIN` on each |
| `aud` | The provider's `gateway_identity.audience` | Set by an admin. A gateway accepts only the audiences it lists |
| `sub` | The run's subject: a person, or a service identity on a service run | A local install gives a fixed seat. A run started with the admin token gives `admin-token` |
| `wardyn_run` | The run id | Flat, so a gateway rule needs no SPIFFE parsing |
| `wardyn_runner` | The runner's id, else the runner target | An operator-chosen slug, possibly a hostname, or `docker` |
| `wardyn_provider` | The model provider's id | |
| `sponsor` | The accountable person of a service identity | Present only on a service run |
| `jti` | A fresh UUID per token | |
| `iat` | When the token was issued | Unix seconds |
| `exp` | `iat` plus 10 minutes | A gateway cannot learn that a run was killed, so a killed run's token is spendable until `exp` |
| `wardyn_format` | `experimental-1` | |
| `act` | `{"sub": "spiffe://<trust domain>/agent-run/<run id>"}` | The agent run acting for `sub`, in the run token's shape |

- There is no `nbf`. A Wardyn clock ahead of the gateway's drops a token early and one behind gets a `401`: both fail closed.
- A test holds this table equal to the signer's claim struct.

## The key set's address

- The key set is the anonymous `GET /.well-known/wardyn-experimental/gateway-jwks.json` on wardynd, under `WARDYN_BASE_PATH` when that is set.
- The console shows the address built from the browser's own origin. A gateway that reaches wardynd by another address, such as a cluster Service name, uses that address with the same path.
- The gateway reads the key set when it loads its config and does not start if the read fails. Start wardynd first, and save a provider with run identity on, which creates the key.

## Joining a run's audit rows to the gateway's log

Wardyn makes a fresh call id for every attempt to a model gateway route and sends it as `X-Wardyn-Call-Id`. The gateway's access log and the run's audit row then carry the same value.

1. Log the header on the gateway. In agentgateway, add `request.headers["x-wardyn-call-id"]` to `frontendPolicies.accessLog.add`, as the setup above does.
2. Read the run's `egress.allow` and `egress.deny` rows (`wardyn audit <run id> --json`) and take `data.gateway_call_id`.
3. Search the gateway's log for that value. The line is that attempt's, with the token's subject and run id beside it when the gateway logs `jwt.sub` and `jwt.wardyn_run`.

- Only a gateway log line that passed the token check is trustworthy. A sandbox that reaches the gateway through an opaque tunnel can send any `X-Wardyn-Call-Id`, so run the gateway's `jwtAuth` in `strict` mode.
- An attempt refused or failed after the proxy made its id has a row with the id, and no gateway line when nothing was sent. A call refused before that has no id.
- The id is Wardyn's own. It is not a request id the gateway made.
- MCP has a call id and no identity token.

## Defaults on a route that carries the token

| Default | Effect |
|---|---|
| The response scrub is forced on | `WARDYN_PROXY_RESPONSE_SCRUB=off` does not turn it off here, so a gateway that echoes the token back cannot hand it to the sandbox |
| `Accept-Encoding` is deleted from the request | The answer arrives in bytes the scrub can read |
| A compressed answer gets a `502` | The proxy refuses bytes it cannot scrub rather than relay them |
| A running run keeps the routes it started with | A run keeps its routes. Once a provider's gateway changes, the mint refuses that run, so its gateway calls fail once its token expires, within 10 minutes. Create a new run to pick the change up |
