/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// First-run setup — GET /api/v1/setup/status (mirrors internal/api/setup.go
// SetupStatus). FROZEN CONTRACT — keep in exact sync with the Go struct.
// The wizard derives its per-step "done" state from these fields.
import type { ConfinementClass, ModelCredentialResidency } from "./runs";
import type { StorageEnforcement } from "../api/drives";

export type SetupCheckStatus = "ok" | "warn" | "fail" | "info";
export type SetupCheckPlatform = "linux" | "darwin" | "windows" | "wsl" | "any";

// One environment/readiness row. status "info" is a permanent, non-fixable
// condition (e.g. no /dev/kvm on macOS) — render it informationally, never as a
// clearable warning.
//
// blocking (0.7.8): the DAEMON's own decision that this row must confiscate
// the console (setup-gate.ts's setupGateActive reads it, and nothing else —
// no id list lives on this side any more). Internal/api/setup_checks.go's
// SetupCheck.Blocking doc names the three rows that ever carry it.
export interface SetupCheck {
  id: string;
  label: string;
  status: SetupCheckStatus;
  platform?: SetupCheckPlatform;
  detail?: string;
  fix?: string;
  blocking?: boolean;
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

// WireIntegrationProbe / WireIntegrationProbeStatus lived here. The
// verification-probe framework (POST /integrations/{id}/test) was removed in
// 0.5 with the integration catalog it served — see internal/types/workspace.go,
// which carries the same note server-side.

export interface WireIntegration {
  id: string;
  name?: string;
  kind: string;
  disabled?: boolean;
  secrets?: WireIntegrationSecret[];
  egress?: string[];
  // Kind-validated non-secret config (e.g. "lane", "region", "ecosystems").
  config?: Record<string, unknown>;
  docs?: string;
  // F6-F12 — capability names this integration does NOT support (mirrors
  // internal/types/workspace.go's Integration.DisabledCapabilities), so a
  // caller doesn't need a hardcoded per-provider capability matrix.
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
  // The org's answer for this row (0.7.2, SiteConfig.agent_providers): may a run
  // name this agent? True for every row when no roster exists — legacy open mode
  // — otherwise the row's own state, and false for a catalog agent the roster
  // does not mention. NOT optional in the wire shape (the server always sends
  // it), but optional HERE because an older daemon omits it: treat absent as
  // "unknown", never as false, and keep rendering today's picker.
  //
  // A disabled row is rendered DISABLED WITH A REASON, never hidden — hiding is
  // how "Claude Code is just gone" becomes a support ticket.
  enabled?: boolean;
  // The row's declared model-access lane and whose credential it uses, absent
  // when no roster exists or no row names this agent. The member-safe half of
  // the agent policy: a lane name and "shared"/"per_user", never the AWS access
  // portal URL (see the Go SetupHarnessTool doc).
  mechanism?: string;
  credential_source?: string;
  // Published for exactly ONE row shape: an enabled `per_user` + `bedrock_sso`
  // row, which is "sandbox". ABSENT for every other row, and absent is the
  // common case — not an older daemon, not an error.
  //
  // A roster cannot say where a credential lands; the lane that RESOLVES decides
  // that, and a GET has no run body to resolve one from. The per-user Bedrock SSO
  // row is the one exception in kind: it admits no other lane, that lane writes
  // the captured session into the sandbox whatever the run carries, and it is the
  // one state whose precise answer is unavailable (Preflight 422s a member who
  // has not signed in — the very person deciding whether to sign in).
  //
  // Everywhere else the console says "Resolved at launch." and offers Preflight,
  // which answers for the exact run. NEVER infer a sentence from `mechanism`
  // above: that is the DECLARED lane, and under a `shared` row it is satisfied by
  // a chain that fell through to a different, resident one.
  credential_residency?: ModelCredentialResidency;
}

// THIS PRINCIPAL's model-access state (internal/api.SetupModelAccess) — the
// per-person answer `llm_ready` below structurally cannot give, since that is a
// DEPLOYMENT fact and read green over a member's own lapsed AWS session.
//
// The chip renders the LABEL for `state` and the server's `action` verbatim
// underneath it (the action is the member's own words and is never reworded
// client-side). DRAFT canon, docs/design/workspace-providers-prompt.md §7.7:
//   live           → AGENTS.MODEL_ACCESS_LIVE, success tone, no action
//                    (expired-but-renewable folds in — dispatch renews it)
//   expiring       → AGENTS.MODEL_ACCESS_EXPIRING, warning; the server's own
//                    `action` is MODEL_ACCESS_EXPIRING_ACTION ("Sign in again
//                    before {ts}"), rendered verbatim — SIGN_IN_AWS is the
//                    BUTTON beside it, not the line
//   expired_signin → AGENTS.MODEL_ACCESS_EXPIRED, warning, SIGN_IN_AWS
//   not_configured → AGENTS.MODEL_ACCESS_NOT_CONFIGURED, warning, SIGN_IN_AWS
//   shared_expired → AGENTS.MODEL_ACCESS_SHARED_EXPIRED, warning, NO button —
//                    there is nothing the member can do but ask their admin
//   not_applicable → the caller is a mechanism, not a person (the shared admin
//                    bearer token under a per_user row) — no credential to
//                    grade, no sign-in it could complete, NO action
export interface SetupModelAccess {
  state:
    | "live"
    | "expiring"
    | "expired_signin"
    | "not_configured"
    | "shared_expired"
    | "not_applicable"
    | (string & {});
  // The declared lane, as the roster's wire value ("bedrock_sso"). Member-safe:
  // a lane name, never a portal URL or a secret name.
  mechanism?: string;
  // The one thing to do, already composed by the server ("" when nothing).
  action?: string;
  // The instant `action` names, RFC3339 UTC — the registration's lapse, or the
  // access token's expiry for a blob that cannot be renewed. ON THE WIRE since
  // 0.7.6 (in-process only before) for one reason: `expiring`'s sentence
  // carries a UTC stamp, and the surfaces that now render that state on EVERY
  // screen for 24 h have to show it on the reader's own clock (relativeTime in
  // the shell strip and the New Run rail, absoluteTime in the two card rows —
  // lib/workspace-providers-copy.ts's modelAccessActionLine). Absent for every state that
  // names no instant, and from a pre-0.7.6 daemon: render `action` verbatim
  // then. NEVER sent to a member under a `shared` row — memberModelAccess
  // builds a fresh struct that drops it, which is the leak that projection
  // exists to close.
  deadline?: string;
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
  // X3-F1 — true on a body the server stripped for this caller's tier
  // (redactSetupStatusForMember). It exists so the console can tell "withheld"
  // from "absent": an empty `checks` list used to be read as a FACT about the
  // deployment, and a member was shown "Image builder · Off" / operator-shaped
  // runner fix advice for detail that was merely hidden from them. Absent on an
  // operator's body and on any older daemon — treat absent as false.
  checks_redacted?: boolean;
  // The CALLER's own model-access state — kept through the member redaction on
  // purpose, and what the member's Getting Started chip reads INSTEAD of
  // llm_ready. Absent when there is nothing per-principal to say (no roster row
  // declares a lane for claude-code and no session is captured), in which case
  // the console renders today's chip.
  model_access?: SetupModelAccess;
  /** Whether an operator has finished (or deliberately left) the Getting
   *  Started funnel ON THIS INSTALL — SiteConfig.OnboardingCompletedAt
   *  flattened to one bit. A fact about the install, never the browser: the
   *  browser flags this replaces outlived wiped databases and were
   *  origin-scoped, so 127.0.0.1 and localhost disagreed. Optional for the
   *  same fixture-compat reason as llm_ready — older daemons omit it; absent
   *  reads as false (not onboarded), which opens the funnel rather than
   *  hiding it. */
  onboarding_complete?: boolean;
  /** Additional roots WARDYN_TRUSTED_CA_FILE loaded at boot (0/absent =
   *  unset) — a bare count, never PEM content or a host name, so it carries
   *  no detail a member is barred from. Rendered by the Network step as
   *  TRUSTED_CA_COUNT(n) (F22 — the data has shipped server-side since
   *  before this console read it). */
  trusted_ca_certs?: number;
  checks: SetupCheck[];
  auth: {
    mode: "local" | "sso" | "token" | "disabled";
    local_loopback: boolean;
    /** Whether this deployment may inject ONE operator's Anthropic subscription
     *  into runs. False on Kubernetes and whenever SSO is configured: sharing one
     *  person's subscription across users breaches the harness vendor's per-user
     *  authentication terms, and it is the operator who ends up in breach. */
    shared_subscription_allowed?: boolean;
    /** Why it is unavailable — rendered instead of the sign-in affordance, so the
     *  card explains rather than looking like "nobody has connected one yet". */
    shared_subscription_reason?: string;
  };
  runner: {
    driver: "docker" | "k8s" | "none" | (string & {});
    confinement_classes: ConfinementClass[];
    confinement_substrates?: Record<string, string>;
    // No network_policy_proven field on the wire (L2 review): the k8s
    // boot-time egress-canary verdict is consumed via the graded
    // k8s_egress_containment row in `checks` (environment-step.tsx finds it
    // by id) — a second, unconsumed copy of the same signal here would just
    // be surface for the two to drift.
    // What actually binds a run's Resources.DiskMiB on THIS deployment —
    // orchestrator-aggregated, weakest-across-substrates (runner.Capabilities.
    // EphemeralDiskEnforcement). Stripped for a member by redactSetupStatusForMember
    // (internal/api/setup.go) — only the /providers screen (SUPER) renders it,
    // under the ephemeral-scratch fields, via DRIVES.ENFORCEMENT_*. Absent on an
    // older daemon or on Docker with no runner detected; empty reads as "none".
    ephemeral_disk_enforcement?: StorageEnforcement;
  };
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
