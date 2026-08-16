/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// First-run setup — GET /api/v1/setup/status (mirrors internal/api/setup.go
// SetupStatus). FROZEN CONTRACT — keep in exact sync with the Go struct.
// The wizard derives its per-step "done" state from these fields.
import type { ConfinementClass } from "./runs";

export type SetupCheckStatus = "ok" | "warn" | "fail" | "info";
export type SetupCheckPlatform = "linux" | "darwin" | "windows" | "wsl" | "any";

// One environment/readiness row. status "info" is a permanent, non-fixable
// condition (e.g. no /dev/kvm on macOS) — render it informationally, never as a
// clearable warning.
export interface SetupCheck {
  id: string;
  label: string;
  status: SetupCheckStatus;
  platform?: SetupCheckPlatform;
  detail?: string;
  fix?: string;
}

// Boot-snapshot readiness of one configured composer backend. key_secret is a
// secret NAME (never a value); key_resolved is whether it was present at boot.
export interface ComposerBackendReadiness {
  name: string;
  provider: string;
  model: string;
  wire: string;
  // Normalized transport: HTTP wires => "api"; otherwise the cli tool / fake
  // variant. Absent when the backend didn't report one.
  transport?: string;
  // openai/azure backends only: "apikey" | "entra". "" for other providers.
  auth?: string;
  enabled: boolean;
  needs_key: boolean;
  key_secret?: string;
  key_resolved: boolean;
}

// A resident coding-agent CLI detected on the wardynd host PATH. logged_in is
// ADVISORY (a home-dir credential-file heuristic).
export interface SetupProvider {
  tool: "claude" | "codex" | (string & {});
  installed: boolean;
  logged_in: boolean;
  login_detected_via?: string;
  // How the CLI authenticates, when detectable: "subscription" (a resident Claude
  // OAuth token is present — fresh OR expired; freshness lives in the llm_provider
  // check detail, not here). "api_key" is reserved in the contract but never
  // inferred for a CLI; codex stays "" (no auth-file parse). Absent => unknown.
  auth_mode?: "subscription" | "api_key" | (string & {});
}

// Amazon Bedrock Anthropic-transport readiness (an enterprise "Connect a
// model" path — no direct Anthropic egress, billed via AWS). region/model are
// non-secret boot-time operator config, safe to echo; creds_present is a bool
// derived from secret-name presence (the AWS credential VALUES are never
// echoed, same as every other secret in this contract).
export interface SetupBedrock {
  region?: string;
  model?: string;
  creds_present: boolean;
  // Additional credential sources resolveBedrockAuth accepts (any one is enough).
  // Optional for fixture-compat with an older daemon that predates them.
  aws_mount?: boolean;
  bearer_present?: boolean;
  // A captured, non-expired container-login AWS SSO session. The lane controls
  // read it from status.harness (which also carries the expiry to render); this
  // is the server folding the same fact into `ready` below.
  sso_present?: boolean;
  // Server-computed readiness (region+model+any credential source). Prefer this
  // over re-deriving in the UI so the two gates can't drift.
  ready?: boolean;
}

// A Wardyn-managed subscription credential captured via container login
// (setup-token) — mirrors internal/api SetupHarness. Presence + capture age only
// (honesty law: not live-verified). `aging` is a conservative age-based
// "reconnect soon" flag, never a hard expiry claim.
export interface SetupHarness {
  provider: string; // "anthropic" | "aws"
  captured: boolean;
  captured_at?: string;
  aging?: boolean;
  source_run_id?: string;
  // Real, machine-readable expiry — populated ONLY by providers whose credential
  // exposes one (AWS SSO does; an Anthropic setup-token does not, which is why
  // `aging` exists at all). Absent means "this provider can't tell you", never
  // "it doesn't expire".
  expires_at?: string;
  expired?: boolean;
  // The stored credential carries a refresh token, so it can be renewed without
  // a fresh interactive login (AWS `sso-session` profiles; legacy
  // sso_start_url profiles have none).
  renewable?: boolean;
}

// Host-proxy detection — mirrors internal/setup/detect_proxy.go. Every value is
// masked server-side (embedded credentials stripped); has_credentials flags that
// a credential WAS present in the raw value so the UI can prompt to store it as a
// secret. Display-only — the UI never writes any of this back.
export type HostProxySource =
  | "env"
  | "shell_profile"
  | "git_config"
  | "tool_config"
  | "os"
  // Forward-compat: tolerate a source a newer daemon emits.
  | (string & {});

export interface HostProxySetting {
  value: string;
  source: HostProxySource;
  detail?: string;
  has_credentials: boolean;
}

export interface HostProxyGitConfig {
  http_proxy?: HostProxySetting;
  https_proxy?: HostProxySetting;
}

export interface HostProxyToolConfig {
  tool: string;
  path: string;
  setting: HostProxySetting;
}

export interface HostProxyPAC {
  url: string;
  source: HostProxySource;
  detail?: string;
}

export interface HostProxyDetection {
  http_proxy?: HostProxySetting;
  https_proxy?: HostProxySetting;
  all_proxy?: HostProxySetting;
  no_proxy?: HostProxySetting;
  // "UPPER/lower" env-var pairs whose values disagree (httpoxy hygiene warning).
  env_case_mismatch?: string[];
  git_proxy?: HostProxyGitConfig;
  tool_configs?: HostProxyToolConfig[];
  pac?: HostProxyPAC;
  has_credentials: boolean;
}

// Presence-only host git-credential posture (mirrors internal/setup/detect.go's
// SCMPosture) — used only to recommend a safer credential-ladder rung on the SCM
// Provider step; Wardyn never reads the files it detects, only stats/git-configs
// their presence.
export interface SCMPosture {
  gh_cli: boolean;
  credential_helper: string;
  git_credentials_file: boolean;
  netrc: boolean;
}

// One cell of the server's live capability matrix (internal/api.Capability),
// which ships snake_case json keys like every other DTO on this payload.
// `reason` and `residency` are omitempty server-side, hence optional here.
export interface WireCapability {
  id: string;
  state: string;
  reason?: string;
  residency?: string;
}

// An integration exactly as the server returns it (internal/api.SetupIntegration
// over types.Integration): where it lives, what credential it takes, how that
// credential reaches the request, and what it powers. `source` discriminates a
// row an operator WROTE from one derived read-only from pre-existing config — a
// derived row keeps working untouched and becomes editable only once adopted.
/** How ONE secret reaches the run (types.IntegrationDelivery). */
export interface WireIntegrationDelivery {
  mode: "proxy_header" | "resident_file" | "resident_env" | (string & {});
  header?: string;
  format?: string;
  path?: string;
  var?: string;
}

/** One required secret: role + store ref + delivery (types.IntegrationSecret).
 *  delivery is absent on a closed kind's bespoke brokered lanes. */
export interface WireIntegrationSecret {
  role: string;
  secret_name: string;
  delivery?: WireIntegrationDelivery;
}

/** The optional verification request behind "Test" (types.IntegrationProbe). */
export interface WireIntegrationProbe {
  method: string;
  url: string;
}

/** Server-cached, read-only last probe result (types.IntegrationProbeStatus). */
export interface WireIntegrationProbeStatus {
  state: "passed" | "failed" | "not_tested" | (string & {});
  checked_at?: string;
  detail?: string;
}

// The base-component shape (types.Integration): one `kind` replacing the old
// Category+Type split, secrets[] with per-row delivery, egress[], kind-
// validated config. The server folds pre-base-component stored rows forward
// at read, so this is the ONLY wire shape a client ever sees.
export interface WireIntegration {
  id: string;
  name?: string;
  kind: string;
  disabled?: boolean;
  secrets?: WireIntegrationSecret[];
  egress?: string[];
  // Kind-validated non-secret config (e.g. "lane", "region", "ecosystems").
  // UI-LIB-5: PUT /integrations/{id} is a full replacement (IntegrationWrite,
  // api/integrations.ts), so a round-tripping caller needs this field on the
  // GET shape too, or it silently writes the zero value back.
  config?: Record<string, unknown>;
  probe?: WireIntegrationProbe;
  // Read-only; never sent back on a write.
  probe_status?: WireIntegrationProbeStatus;
  docs?: string;
  // Capability ids the operator turned off individually. Same round-trip
  // reason as `config` above (UI-LIB-5).
  disabled_capabilities?: string[];
  default_for?: string[];
  source?: "stored" | "legacy" | (string & {});
  // The server's live per-capability matrix for this row
  // (integrationsWithCapabilities) — not read client-side yet; CAPS in
  // lib/integrations.ts hand-mirrors the same facts as a SECOND derivation,
  // pending consolidation onto this field. Optional for the same
  // fixture-compat reason as SetupStatus.bedrock below.
  capabilities?: WireCapability[];
}

// One row of the static coding-agent harness catalog (internal/api.
// SetupHarnessTool, setupHarnessTools()) — which tools Wardyn knows how to
// run, mirrors SetupStatus.harnesses below.
export interface SetupHarnessTool {
  id: string;
  display: string;
  has_gateway: boolean;
  has_login: boolean;
  no_managed_auth?: boolean;
}

export interface SetupStatus {
  ready: boolean;
  // Server-computed "does SOME run/compose LLM access path exist" (resident
  // CLI login, a resolved composer backend key, an api-key-ish secret,
  // Bedrock, a managed harness token, or a configured ai_provider
  // Integration) — computed BEFORE the member redaction pass and left
  // untouched by it (see the Go SetupStatus.LLMReady doc comment), so a
  // member's console can answer the question the (redacted-away) `checks` /
  // `providers` detail used to answer. Optional for the same fixture-compat
  // reason as `bedrock` — READY_FALLBACK and older daemons omit it; treat
  // absent as "unknown", not "false".
  llm_ready?: boolean;
  checks: SetupCheck[];
  auth: { mode: "local" | "sso" | "token" | "disabled"; local_loopback: boolean };
  runner: {
    driver: "docker" | "k8s" | "none" | (string & {});
    confinement_classes: ConfinementClass[];
    confinement_substrates?: Record<string, string>;
    // No network_policy_proven field on the wire (L2 review): the k8s
    // boot-time egress-canary verdict is consumed via the graded
    // k8s_egress_containment row in `checks` (environment-step.tsx finds it
    // by id) — a second, unconsumed copy of the same signal here would just
    // be surface for the two to drift.
  };
  composer: { enabled: boolean; default?: string; backends: ComposerBackendReadiness[] };
  providers: SetupProvider[];
  secrets: { present: string[]; github_app: boolean };
  age_key: { durable: boolean };
  has_runs: boolean;
  platform: { os: string; wsl: boolean; kvm?: boolean };
  // Optional: absent on an older/fallback status (e.g. READY_FALLBACK, or a
  // daemon build that predates this field) rather than a required breaking
  // change to every existing SetupStatus fixture.
  bedrock?: SetupBedrock;
  // Masked host-proxy detection (see HostProxyDetection). Optional for the same
  // fixture-compat reason as `bedrock` — READY_FALLBACK and older daemons omit it.
  host_proxy?: HostProxyDetection;
  // Presence-only host git-credential posture (see SCMPosture) — feeds the
  // scm_provider check's grading and the ScmProviderStep gh-CLI advisory line.
  // Optional for the same fixture-compat reason as `bedrock`.
  scm?: SCMPosture;
  // Whether wardynd itself sees a resident Claude login (host mode) vs is blind to
  // it (compose/container). host_like === false is why the LLM-access check reads
  // "no login" in compose even when the operator IS logged in on the host. Optional
  // for the same fixture-compat reason as `bedrock`.
  deployment?: { host_like: boolean };
  // Wardyn-managed subscription credentials captured via container login
  // (setup-token). Present per provider that has a stored token; empty/absent
  // when none. Optional for the same fixture-compat reason as `bedrock`.
  harness?: SetupHarness[];
  // The EFFECTIVE integration set (stored ∪ legacy-derived) with each row's live
  // capabilities — the server's own answer, as opposed to the rows
  // lib/api/integrations.ts derives client-side for the two legacy categories.
  // Optional for the same fixture-compat reason as `bedrock`.
  integrations?: WireIntegration[];
  // The STATIC coding-agent harness catalog (harnessCatalog, harness.go) —
  // which tools Wardyn knows how to run and whether it can wire each one a
  // managed model credential or a container-login subscription. Distinct
  // from `harness` above (a CAPTURED credential's live readiness). DEADCODE-2:
  // not read client-side yet (same as `capabilities` above) — new-run's
  // WizardAgent literal union is a hand-maintained copy of the same facts,
  // pending consolidation onto this field. Optional for the same
  // fixture-compat reason as `bedrock`.
  harnesses?: SetupHarnessTool[];
  // UI-ONLY, never on the wire: set by api.getSetupStatus()'s fallback when the
  // daemon couldn't answer (network error / non-ok). The Go contract does not
  // emit it. Consumers must treat the rest of the payload as UNTRUSTWORTHY —
  // e.g. app-shell.tsx's barrier chip and runs.tsx's no-barrier blocker both
  // skip repainting from it rather than reading empty confinement_classes as
  // "no barrier installed".
  unreachable?: boolean;
}
