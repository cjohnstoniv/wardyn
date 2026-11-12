> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Model gateways and detectors: setup

- Wardyn plugs in an established model gateway or PII detector that you run yourself. It builds neither, bundles neither and does not change them.
- Every recipe here is a `custom_endpoint` provider (or the `detector_sidecar_url` policy field) pointed at your own deployment. Model providers › Your own endpoint can start from a preset for some of these gateways, which fills only the fields it names and records nothing; everything else is yours to set.
- [An external AI gateway](ai-gateway.md) covers agentgateway, run identity and MCP federation.

## Every gateway

1. Serve the gateway over `https://`. A provider's `base_url` must be `https://`.
2. Give wardynd the gateway's certificate authority: the Helm value `trustedCA`, or `WARDYN_TRUSTED_CA_FILE` outside Helm.
3. Add the gateway host to the policy's `allowed_domains`, exactly as [Internal model gateway](../OPERATIONS.md#internal-model-gateway) says.
4. Add a `custom_endpoint` provider whose `base_url` is the gateway. Each person's key is stored as for any provider. Wardyn sends it in the `auth` header and format you set (`Authorization` and `Bearer %s` by default) and drops the sandbox's own value.
5. Put any header the gateway routes on in the provider's `extra_headers` (below).

## Codex CLI on a Responses-style endpoint

- A `custom_endpoint` provider can serve `codex-cli`: enable `codex-cli` on the provider's harness list and set its Model. The harness catalogue accepts that kind for it (`harnessCatalog`, [`harness.go`](../../internal/api/harness.go)).
- The run's Codex CLI calls the proxy's `/wardyn/llm/openai` route with a placeholder key. The proxy sends `POST .../responses` to `base_url` plus the harness entry's `path`, with the person's own token in the provider's auth header.
- The endpoint must serve the OpenAI Responses API at that path. The base URL given to Codex CLI ends in `/v1` unless `base_url` plus `path` already carries a path (`codexBaseURL`, [`runs_dispatch_provider.go`](../../internal/api/runs_dispatch_provider.go)).
- Tests pin the path handling ([`codex_route_test.go`](../../internal/egress/proxy/codex_route_test.go)). No gateway's Responses support is tested.

## Recipes

### LiteLLM

- Serve TLS with `--ssl_keyfile_path` and `--ssl_certfile_path`.
- Virtual keys need Postgres. Give each person a key from `/key/generate` with `models` set to the aliases they may use.
- Self-serve keys need SSO, and OIDC sign-in to the gateway is an Enterprise feature, so people paste a key.
- On Bedrock or Vertex backends, set `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` in the `config` map of an organisation component. The model-env deny list does not hold it.

### Portkey, self-hosted

- Portkey serves no TLS, so put a TLS front before it.
- Add one provider per vendor key, with the extra header `x-portkey-provider: anthropic` for Claude Code.
- Codex CLI needs a second provider with `openai`, at its own hostname (not tested).
- A `pc-` config does not work.
- For a private upstream, add the extra header `x-portkey-custom-host` and list the host in the gateway's `TRUSTED_CUSTOM_HOSTS` (exact hostnames, Portkey 1.14.0 or later).

### Portkey, hosted

- Set the auth header to `x-portkey-api-key` and the format to `%s`.
- Add the extra header `x-portkey-provider: @slug` and use plain model names.
- Give the key a Config with `forward_headers: ["anthropic-beta"]`.
- Only the header contract is tested. No hosted account is.

### OpenRouter

- Use `https://openrouter.ai/api` with `Bearer` auth, on an organisation account with each person's own key. Its terms bar reselling or transferring access.
- `openrouter/auto` in `allowed_models` admits any model.
- Its Responses route is stateless, and Codex CLI through it is not tested.

## Extra headers

| Rule | Value |
|---|---|
| Where | A `custom_endpoint` provider only. None by default. |
| How many | At most 8, each value 1 to 2048 bytes |
| Order on the wire | The stored credential, then the extra headers, which replace a sandbox header of the same name |
| Refused names | Credential headers the proxy owns, hop-by-hop and framing headers, `X-Wardyn-*`, the provider's own auth header, and any name that reads as a credential (`key`, `token`, `secret`, `auth`, `password`, `cookie`, `credential`) |
| Refused values | A value that looks like an API key or matches a secret format |
| On a change | Every person's stored credential for the provider is removed, as for a `base_url` change |

- `extra_headers` values are stored in clear in the model-provider block. They are not secrets: put a credential in the auth header instead.
- The `model_provider.write` audit row lists the header names by provider, never a value ([audit actions](../AUDIT-ACTIONS.md)).
- Two harnesses of one run share a gateway host only through the same provider. Otherwise the run is refused, so give each harness's provider its own hostname.
- On a gateway host the proxy drops every `x-portkey-*` header the sandbox sends, except the header the stored credential owns.

## What Wardyn checks, and what the gateway does

| Check | Where it runs |
|---|---|
| `allowed_models` reads the request's `model` and nothing else | Wardyn, before the gateway |
| `fallbacks`, `models`, `context_window_fallbacks`, `content_policy_fallbacks`, `router_settings_override` and `preset` in a body are refused in `block` and logged in `alert` | Wardyn, before the gateway ([rerouting keys](../POLICIES.md#llm_inspection--llminspectionspec)) |
| Prompt inspection and the known-secret match | Wardyn, unless the provider sets `gateway_identity.inspection` to `gateway` |
| Routing, fallbacks, rate limits, guardrails and the gateway's own logs | The gateway |

- `allowed_models` lists the aliases a person may name. A gateway can serve a different model, and metering records the model the answer names, or the request's when the answer names none.
- With `inspection: gateway`, Wardyn still matches the run's secret values and skips the rerouting-key check. The gateway must then run its own guardrails: LiteLLM needs `default_on: true` on each.
- Wardyn's checks are a guardrail, [not DLP](../../threatmodel/THREAT-MODEL.md).

## Presidio as the detector

1. Run the Presidio analyzer yourself. It listens on port `3000`, and `POST /analyze` is the route Wardyn calls.
2. Add its host to the policy's `allowed_domains`.
3. Set the policy's `llm_inspection`:

```json
{
  "llm_inspection": {
    "mode": "block",
    "detector_sidecar_url": "http://presidio.corp.example:3000/analyze",
    "detector_sidecar_format": "presidio",
    "block_min_severity": "medium",
    "on_scanner_error": "block"
  }
}
```

- The URL is the full `/analyze` address. Wardyn sends `{text, language: "en", score_threshold: 0.5}` and names no entities, so your recognizers decide what is found.
- `block` mode needs `block_min_severity` of at least `medium`. Names, locations, organisations, nationality, religious and political groups, dates, URLs, UUIDs, MAC and IP addresses and US driver licences are `low`; other entities are `high` from a score of `0.85` and `medium` below it.
- A Presidio outage lets the request through. Set `on_scanner_error: block` when an outage must block it.
- Every hit is category `pii` with detector `sidecar:<entity type>`. A finding carries no content and no name of the secret.

## Pinned versions

| Component | Version | Pinned and asserted by |
|---|---|---|
| LiteLLM | `v1.83.10-stable` | `TestGatewayRecipes_Live` in [`gateway_recipes_docker_test.go`](../../internal/egress/proxy/gateway_recipes_docker_test.go) |
| Portkey | `1.15.2` | the same test |
| Presidio analyzer | `2.2.363` | the same test |

- The test pins and asserts these versions; it passes in the nightly. It runs in the `docker-tagged-live` nightly against [`test/gatewayfake`](../../test/gatewayfake), with the images pinned by digest. It asserts the credential swap, the Portkey headers, metered streaming usage, the rerouting refusal, and a Presidio hit and a code-like prompt it does not block.
- Hosted Portkey's header contract is covered by [`gateway_portkey_strip_test.go`](../../internal/egress/proxy/gateway_portkey_strip_test.go). OpenRouter and Codex CLI through Portkey or OpenRouter are not tested.
