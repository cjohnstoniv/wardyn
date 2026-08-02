# Policy reference (`RunPolicySpec`)

Every governed run resolves to one `RunPolicySpec` — the whole configuration
surface. This is the field list; [`examples/policies/`](../examples/policies/) is
the worked set, and `wardyn policy render -f <file>` converts YAML→JSON and
rejects a misspelled field before you launch.

The spec is the same object everywhere: `--policy-file`, `policy create/update -f`,
an inline policy on a create-run request, and `WARDYN_DEFAULT_POLICY`. Unknown
fields are refused (`DisallowUnknownFields`), so JSON carries no comments — use
YAML if you want them.

Source of truth: `types.RunPolicySpec` (`internal/types/types.go`); the legal
values below are what `validatePolicySpec` (`internal/api/policy.go`) enforces at
write time. A guard test fails if a field here drifts from the struct.

## Top level

| Field | Type | Default | What it does |
|---|---|---|---|
| `allowed_domains` | `[]string` | `[]` (deny all) | L2 egress allowlist: exact hosts or `*.` wildcards. Empty under default-deny means the sandbox reaches nothing. Each entry must be a shape the proxy's matcher can match — a mid-label wildcard, a URL, or a bad `:port` is rejected at write time rather than shipped as a rule that silently matches nothing. |
| `denied_domains` | `[]string` | `[]` | Always wins over `allowed_domains`, in both egress modes. Same entry-shape validation. |
| `allow_all_egress` | `bool` | `false` | Switches egress from allowlist-only to deny-list-only: any non-denied **public** host is allowed. The SSRF/private-IP guard is unaffected (metadata, loopback, link-local and private ranges stay denied unconditionally), and credential injection still requires an exact `allowed_domains` entry — allow-all never widens where a secret may go. `first_use_approval` is inert under it. |
| `first_use_approval` | `string` | `always_deny` | How an unknown domain is handled. See the three modes below. A legacy boolean still decodes (`true`→`deny_with_review`, `false`→`always_deny`). |
| `allowed_methods` | `[]string` | `[]` (all) | Optional HTTP method restriction. |
| `min_confinement_class` | `string` | — (**required**) | `CC1` (hardened runc), `CC2` (gVisor), or `CC3` (Kata microVM). The run refuses to launch below it; an unrecognised value is rejected at write time and would otherwise rank below CC1. |
| `eligible_grants` | `[]GrantSpec` | `[]` | The ceiling of credential scopes this run may request. Eligibility is not issuance — the broker still mints. |
| `auto_stop_after_sec` | `int` | `0` | Idle auto-stop. `> 0` = stop after that many seconds of wall-clock idleness plus a fixed 30s activity-debounce slack (egress-driven clock resets are coalesced to one per 30s, so the slack guarantees an active run is never read as idle; the `run.autostop` audit event's `threshold_sec` records the effective value, configured + 30); `0` = never reaped; `< 0` = never reaped, stated explicitly (what an interactive run should set, so the reaper does not stop it the moment it looks idle). Idleness is `updated_at` age — an attach or an egress call resets it, local CPU/disk work does not (`internal/lifecycle`). |
| `workspace_mounts` | `[]WorkspaceMount` | `[]` | Operator-authored host bind mounts. Never agent-chosen. |
| `workspace_repos` | `[]WorkspaceRepo` | `[]` | Additional git repos cloned into the run — the clone counterpart of `workspace_mounts`. |
| `llm_inspection` | `LLMInspectionSpec` | omitted = **off** | Outbound content inspection on brokered LLM routes. |
| `resources` | `ResourceLimits` | omitted = platform defaults | Sandbox CPU/memory/PID/disk caps. |

### `first_use_approval` modes

| Value | Behavior |
|---|---|
| `always_deny` | Hard-deny the unknown domain and log it. No approval is ever raised. |
| `deny_with_review` | Raise a pending approval **and** deny the in-flight request. Once approved, a retry passes. The connection is never held. |
| `wait_for_review` | Raise a pending approval and **hold the connection** until it is decided or the proxy's hold deadline passes. Approved in time, the same in-flight request completes; on deadline it fails closed (403) with the approval still pending. |

Empty or unrecognised normalises to `always_deny` at runtime (fail closed), but
an unrecognised literal is rejected at write time.

## `eligible_grants[]` — `GrantSpec`

| Field | Type | Default | What it does |
|---|---|---|---|
| `kind` | `string` | — (required) | `github_token`, `cloud_sts`, `api_key`, `git_pat`, or `ssh_key`. Anything else is rejected. |
| `scope` | object | — | Kind-specific; see the table below. |
| `ttl_seconds` | `int` | `3600` | TTL of the minted credential. 1h is both the default and the maximum. Negative is rejected. |
| `requires_approval` | `bool` | `false` | Force a human approval before the broker will mint, instead of auto-minting on policy. |

| `kind` | `scope` shape | Write-time rules |
|---|---|---|
| `github_token` | `{"repos":[…],"permissions":{…}}` | Validated with the broker's own mint-time predicate, so a malformed permission is a 400 at policy write, not a mint failure mid-run. |
| `cloud_sts` | `{}` | Must decode as a JSON object if present. Hard-requires the SPIRE identity provider, which does not ship — it mints nothing today. |
| `api_key` | `{"host":"…","header":"…"}` | Proxy-side injection only; the value never enters the sandbox. Referencing a reserved platform secret (`wardyn-signing-key`, `wardyn-session-key`) is refused. |
| `git_pat` | `{"host":"…","secret_name":"…","username":"…"}` | `host` + `secret_name` required; reserved secret names refused. The stored PAT **value** is handed to the git credential helper (ADO/GitLab have no injectable seam), so it is resident for the git operation. `username` defaults by convention (ADO `pat`, GitLab `oauth2`). |
| `ssh_key` | `{"host":"…","key_secret_ref":"…","username":"…","known_hosts_secret_ref":"…"}` | `host` + `key_secret_ref` required; reserved secret names refused for either ref. `host` must be an SSH-over-443 provider Wardyn supports (`github.com`, `dev.azure.com`). A **documented exception** to the no-resident-secret rule: the key lands as a 0400 file for the clone and is wiped right after. |

## `workspace_mounts[]` — `WorkspaceMount`

| Field | Type | Default | What it does |
|---|---|---|---|
| `source` | `string` | — (required) | Host path. Must be absolute and cleaned, and not under a denied host location — the same deny-list the docker driver enforces, checked here so a bad mount is a 400 at write time too. |
| `target` | `string` | — (required) | In-container path, under an allowed prefix (`/home/agent`, `/work`, `/workspace`). Must be unique across all `workspace_mounts` **and** `workspace_repos` targets, so a clone can never land on a bind target. |
| `read_only` | `bool` | `true` when omitted | Omitting it means read-only — the safe direction. Read-write requires an explicit `"read_only": false`. |

## `workspace_repos[]` — `WorkspaceRepo`

| Field | Type | Default | What it does |
|---|---|---|---|
| `repo` | `string` | — (required) | Repo slug or URL, validated like a run's `--repo`. |
| `target` | `string` | (unset) | Optional clone destination; validated and collision-checked against every other target when set. Unset defers to the `~/work/<name>` convention. |

## `llm_inspection` — `LLMInspectionSpec`

A guardrail and visibility layer, **not** exfiltration prevention (see
`threatmodel/THREAT-MODEL.md` §5.1). Omitting the block, or `mode: "off"`, is off.

| Field | Type | Default | What it does |
|---|---|---|---|
| `mode` | `string` | `off` | `off`, `alert` (scan + audit, forward unchanged), or `block` (a qualifying finding refuses the request). When not off, at least one detector — a `detect_*`, a sidecar URL, or `classified_markers` — must be enabled. |
| `workspace_secret_values` | `[]string` | `[]` | Operator-declared known secret **values** the run must not leak into a prompt. The v1 detection corpus. Never logged; values below the masking floor are ignored. |
| `detect_secrets` | `bool` | `false` | Exact match against `workspace_secret_values`. |
| `detect_secret_patterns` | `bool` | `false` | Regex catalog of well-known secret *formats* (AWS/GitHub/Slack/Google keys, PEM, JWTs, Stripe). Higher precision than entropy; false-positives on example keys in code. |
| `detect_entropy` | `bool` | `false` | Shannon-entropy detector. High false-positive rate in code; emits medium severity so a strict `block_min_severity` can exclude it. |
| `detect_pii` | `bool` | `false` | Regex/Luhn PII detector. Best-effort visibility signal, never a control. |
| `detector_sidecar_url` | `string` | (unset) | Out-of-process detector the proxy POSTs each span to (e.g. Presidio, LLM-Guard). Must be an `http(s)://` URL. A sidecar error respects `on_scanner_error` like any other scanner error. |
| `classified_markers` | `[]string` | `[]` | Literal markers (`INTERNAL ONLY`, `CONFIDENTIAL//NOFORN`) whose presence flags a classified-content leak. Case-insensitive substring match. |
| `scan_attachments` | `bool` | `false` | Decode and scan base64 image/document attachment bytes. Off by default: binary, large, high-FP. |
| `inspect_forward_egress` | `bool` | `false` | Extend inspection from the LLM routes to the generic plaintext-HTTP forward path. HTTPS connectors tunnel via opaque CONNECT and stay uninspected unless MITM'd. |
| `max_scan_bytes` | `int` | `1048576` (1 MiB) | Cap on a single scanned span. A larger span is skipped fail-open and recorded. Negative is rejected. |
| `on_scanner_error` | `string` | `pass` | `pass` (fail-open, the request still flows) or `block` (fail-closed in block mode). |
| `require_inspectable_llm` | `bool` | `false` | Refuse to schedule a run whose resolved LLM transport is opaque (subscription-OAuth / Bedrock CONNECT) and therefore uninspectable. Requires `intercept_tls` — only TLS-MITM gives a runtime guarantee. Default only warns. |
| `intercept_tls` | `bool` | `false` | Opt the run into TLS-MITM of opaque CONNECT tunnels to known LLM hosts, making the subscription-OAuth path inspectable. The control plane provisions a per-run CA: the private key goes only to the proxy sidecar, the sandbox trusts only the public cert. Adds a CA trust dependency inside the sandbox (threatmodel §5.1a). |
| `block_min_severity` | `string` | `low` | Minimum finding severity that blocks in `block` mode: `low`, `medium`, `high`, `critical`. |

## `resources` — `ResourceLimits`

A zero or omitted field means "platform default". Dispatch fills conservative
defaults so **every** run is capped even under a policy that sets nothing.

| Field | Type | Default | What it does |
|---|---|---|---|
| `cpu_millis` | `int` | `2000` (2 vCPU) | Milli-CPU cap. |
| `memory_mib` | `int` | `4096` | Hard memory cap, MiB. |
| `pids_limit` | `int` | `512` | Max processes/threads — the fork-bomb guard. |
| `disk_mib` | `int` | (storage-driver default) | Writable-storage cap, MiB. Best-effort: it needs a storage driver that supports a per-container quota, and warns/fails closed when a cap is demanded but unsupported. |
