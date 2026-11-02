> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# An external AI gateway: setup

- An AI gateway such as [agentgateway](https://agentgateway.dev) runs beside Wardyn and serves model and MCP traffic for every caller. [Pluggability](../PLUGGABILITY.md#an-external-ai-gateway-for-example-agentgateway) says where it fits.
- This page is the working setup: the config, the hostnames and the Wardyn settings that go with them. Its image is `agentgateway` v1.7.0, pinned by digest in [`deploy/kind/gateway/gateway.yaml`](../../deploy/kind/gateway/gateway.yaml).
- **Status.** The nightly job `ai-gateway-kind` runs this setup and is new. It has no scheduled green run yet, so nothing below is claimed as tested by CI until it has one.

## What the setup is

| Part | What it is |
|---|---|
| Gateway | The unmodified agentgateway image, one pod, one HTTPS listener on 8443 |
| Model hostname | `llm-gw`: serves the Anthropic Messages API for a `custom_endpoint` provider |
| MCP hostname | `mcp-gw`: serves MCP federation, with one target |
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

## What the gateway does with a request

The setup was checked against the pinned image in a one-container trial. These are the v1.7.0 behaviours it relies on.

| Request | Answer |
|---|---|
| `POST /v1/messages`, streamed or not | A Messages answer in the same shape, with the gateway's own `protocol=llm` log line |
| A bearer `Authorization` header | The upstream receives it as `x-api-key`, the form an Anthropic host expects |
| A path the table does not name | Forwarded to the upstream, which answers it: a `404` from the fake for a path it does not serve |
| An MCP call with no session | `400`. The client sends `initialize` first and repeats the `Mcp-Session-Id` it gets back |
| An MCP call that does not accept both `application/json` and `text/event-stream` | `406` |

- The job's `/_seen` listing names every path a harness reached outside the model route's own. That is the evidence for what the gateway forwards, and for the sentence in [Pluggability](../PLUGGABILITY.md#an-external-ai-gateway-for-example-agentgateway) about the other paths.

## What the job asserts

| Assertion | Read from |
|---|---|
| The proxy let the model call through to the model hostname | The run's audit rows: `egress.allow` on that host |
| The proxy let the tool call through to the MCP hostname | The run's audit rows: `egress.allow` on that host |
| The gateway served each route | Its access log: a `route=default/llm` and a `route=default/mcp` line |
| The made-up credential reached the model route, and the tool ran once | The fake's `/_seen` |
| The job reached its own closing line | `kind-gateway-run: PASS`, gated on exit codes |

- A run's exit code is not an assertion for the model run: Claude Code's exit says nothing about the gateway.
- The tool call comes from a `curl` in an exec run, not from a model.
- The job carries no token and no person's data. Run it by hand on a disposable quickstart cluster with `WARDYN_TEST_K8S=1 WARDYN_ACK_SHARED_QUICKSTART=1 scripts/kind-gateway-run.sh` ([the script](../../scripts/kind-gateway-run.sh)).
