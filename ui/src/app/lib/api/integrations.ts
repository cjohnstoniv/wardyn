/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations — GET /api/v1/integrations does not exist yet (a later wave
// adds it). Until it does, this is a thin adapter: `list()` composes the
// three endpoints that DO exist (setup status, site config, secret names) and
// `deriveIntegrations` reshapes them into rows, exactly the discipline
// lib/scm-provider.ts already established for the SCM Provider step (a
// "provider row" there is not a backend entity either). No invented server
// concepts: every field on IntegrationRow traces back to a real response
// field, named in the comment next to it. When the real endpoint lands, only
// `list()`'s body should need to change — every caller keeps working.
//
// W5: marks every seam that's a deliberate stand-in for something a later
// wave should replace with a real field/endpoint (GitHub ref-confinement,
// per-integration "default" persistence, an Azure endpoint URL, workspace
// pin-counts for the blast radius).
import type { BedrockLane, IntegrationCategory, ResidencyKind } from "../integrations";
import { AI_TYPES, BEDROCK_LANE_META, SUBSCRIPTION_LANE_META, type AiType } from "../integrations";
import { deriveProviders, LANE_META, slugHost, type Lane } from "../scm-provider";
import { relativeTime, clockTime } from "../format";
import type { SetupStatus, SiteConfig } from "../types";
import { setup as setupApi } from "./setup";
import { health } from "./health";
import { secrets as secretsApi } from "./secrets";

export type { IntegrationCategory };

export interface RowChip {
  label: string;
  tone: "info" | "success" | "warning" | "neutral";
  /** Impossible-as-fact chip: 60%-opacity, tooltip carries the verbatim reason. */
  muted?: boolean;
  tooltip?: string;
}

// Presence/aging language ONLY (never a live probe), except `gh_verdict` — the
// one GitHub-ref-confinement exception the whole module exists to carry.
export type Posture =
  | { kind: "configured" }
  | { kind: "captured"; ageLabel: string }
  | { kind: "reconnect_soon" }
  | { kind: "session_expires"; when: string }
  | { kind: "region_model_unset" }
  | { kind: "gh_verdict"; verdict: "ref_confined" | "unconfined" | "unknown"; checkedLabel: string };

export interface IntegrationRow {
  id: string;
  category: IntegrationCategory;
  name: string;
  /** Mono sub-line under the name, e.g. "anthropic · api key" or a host. */
  typeLabel: string;
  chips: RowChip[];
  residency: ResidencyKind;
  posture: Posture;
  /** Secret name(s) this row is backed by — rotate/delete act on these. */
  secretNames: string[];
  /** Set when the credential is a harness login (rotate/delete use harnessAuth,
   *  not the generic secret store) rather than a plain secret. */
  harnessProvider?: string;
  aiType?: AiType;
  /** anthropic_subscription only: which of the two lanes this row is. */
  hostCli?: boolean;
  bedrockLane?: BedrockLane;
  /** status.checks ids relevant to this row, rendered verbatim via CheckRow. */
  checkIds: string[];
  /** True only for the GitHub App row — the one row with a live check. */
  canReCheck?: boolean;
  isGithubApp?: boolean;
}

export interface IntegrationsData {
  ai: IntegrationRow[];
  scm: IntegrationRow[];
}

function hasSecret(present: string[], name: string): boolean {
  return present.includes(name);
}

// ---- AI providers ----------------------------------------------------------

const AI_TYPE_LABEL: Record<string, string> = {
  anthropic_api_key: "anthropic · api key",
  bedrock: "aws · bedrock",
  openai_api_key: "openai · api key",
  azure_openai: "azure · openai key",
};

function subscriptionTypeLabel(hostCli: boolean): string {
  return hostCli ? "anthropic · host cli login" : "anthropic · managed login";
}

// The default Name a newly-added row of this type gets (the Add dialog's
// editable Name field default, and what a freshly-derived row is called until
// the operator renames it) — ONE naming function so the dialog's preview can
// never drift from what deriveAiRows above actually produces.
export function aiRowName(type: AiType, hostCli?: boolean): string {
  switch (type) {
    case "anthropic_api_key":
      return "Anthropic (API key)";
    case "anthropic_subscription":
      return hostCli ? "Claude subscription (host CLI)" : "Claude subscription (managed)";
    case "bedrock":
      return "AWS Bedrock";
    case "openai_api_key":
      return "OpenAI (API key)";
    case "azure_openai":
      return "Azure OpenAI";
  }
}

// The compact chip list a list row shows: ON rows (capChip, `· default` when
// the type's own matrix says so) and impossible rows (factChip, muted, the
// verbatim reason as tooltip) — OFF rows are omitted, matching the mock.
export function capabilityChips(type: AiType, hostCli?: boolean): RowChip[] {
  return AI_TYPES[type]
    .capabilityPreview(hostCli)
    .filter((r) => r.on || r.fact)
    .map((r) =>
      r.fact
        ? { label: r.label, tone: "neutral", muted: true, tooltip: r.fact }
        : { label: r.def ? `${r.label} · default` : r.label, tone: "info" },
    );
}

export function aiResidency(type: AiType, hostCli: boolean | undefined, lane: BedrockLane | undefined): ResidencyKind {
  if (type === "anthropic_subscription") return SUBSCRIPTION_LANE_META[hostCli ? "resident_host" : "managed"].residency;
  if (type === "bedrock") return lane ? BEDROCK_LANE_META[lane].residency : "varies";
  if (type === "azure_openai") return "control_plane";
  return "proxy_injected"; // anthropic_api_key / openai_api_key
}

// Which of the currently-derived AI rows already "holds" the default for a
// capability slot (its own matrix's `def` flag for that capability) — used by
// the Add dialog to preview a "replaces <name>" note instead of guessing.
export function defaultHolder(rows: IntegrationRow[], capability: RegExp): IntegrationRow | undefined {
  return rows.find((r) => r.chips.some((c) => !c.muted && capability.test(c.label) && c.label.includes("· default")));
}

// resolveBedrockAuth's precedence (internal/api/runs_bedrock.go, mirrored in
// llm-access.tsx's bedrockRow): bearer > AWS SSO session > host ~/.aws mount >
// static access keys. Undefined when only region/model are set — no
// credential lane is actually active yet.
function activeBedrockLane(status: SetupStatus): BedrockLane | undefined {
  const b = status.bedrock;
  if (!b) return undefined;
  const sso = status.harness?.find((h) => h.provider === "aws" && h.captured && !h.expired);
  if (b.bearer_present) return "bearer";
  if (sso) return "sso";
  if (b.aws_mount) return "aws_dir";
  if (b.creds_present) return "static";
  return undefined;
}

function bedrockSecretNames(lane: BedrockLane | undefined, present: string[]): string[] {
  if (lane === "bearer") return ["bedrock-api-key"];
  if (lane === "static") return ["aws-access-key-id", "aws-secret-access-key", "aws-session-token"].filter((n) => hasSecret(present, n));
  return []; // sso is a harness credential; aws_dir is boot config — neither is a secret-store entry
}

function deriveAiRows(status: SetupStatus, present: string[]): IntegrationRow[] {
  const rows: IntegrationRow[] = [];
  const claude = status.providers.find((p) => p.tool === "claude");

  if (hasSecret(present, "anthropic-api-key")) {
    rows.push({
      id: "ai:anthropic_api_key",
      category: "ai_provider",
      name: aiRowName("anthropic_api_key"),
      typeLabel: AI_TYPE_LABEL.anthropic_api_key,
      chips: capabilityChips("anthropic_api_key"),
      residency: aiResidency("anthropic_api_key", undefined, undefined),
      posture: { kind: "configured" },
      secretNames: ["anthropic-api-key"],
      aiType: "anthropic_api_key",
      checkIds: ["llm_provider"],
    });
  }

  if (claude?.logged_in && claude.auth_mode === "subscription") {
    rows.push({
      id: "ai:anthropic_subscription:host",
      category: "ai_provider",
      name: aiRowName("anthropic_subscription", true),
      typeLabel: subscriptionTypeLabel(true),
      chips: capabilityChips("anthropic_subscription", true),
      residency: aiResidency("anthropic_subscription", true, undefined),
      // A resident host login carries no capture timestamp Wardyn can see.
      posture: { kind: "configured" },
      secretNames: [],
      aiType: "anthropic_subscription",
      hostCli: true,
      checkIds: ["llm_provider"],
    });
  }

  const managed = status.harness?.find((h) => h.provider === "anthropic" && h.captured);
  if (managed) {
    rows.push({
      id: "ai:anthropic_subscription:managed",
      category: "ai_provider",
      name: aiRowName("anthropic_subscription", false),
      typeLabel: subscriptionTypeLabel(false),
      chips: capabilityChips("anthropic_subscription", false),
      residency: aiResidency("anthropic_subscription", false, undefined),
      posture: managed.aging
        ? { kind: "reconnect_soon" }
        : managed.captured_at
          ? { kind: "captured", ageLabel: relativeTime(managed.captured_at) }
          : { kind: "configured" },
      secretNames: [],
      harnessProvider: "anthropic",
      aiType: "anthropic_subscription",
      hostCli: false,
      checkIds: ["harness_credential", "claude_subscription_staging"],
    });
  }

  const bedrock = status.bedrock;
  const bedrockConfigured = !!(bedrock && (bedrock.region || bedrock.model || bedrock.creds_present || bedrock.aws_mount || bedrock.bearer_present));
  if (bedrockConfigured) {
    const lane = activeBedrockLane(status);
    const sso = status.harness?.find((h) => h.provider === "aws" && h.captured);
    let posture: Posture = { kind: "configured" };
    if (!bedrock!.region || !bedrock!.model) posture = { kind: "region_model_unset" };
    else if (lane === "sso" && sso) {
      posture =
        sso.expired || !sso.expires_at
          ? { kind: "reconnect_soon" }
          : { kind: "session_expires", when: clockTime(sso.expires_at) };
    }
    rows.push({
      id: "ai:bedrock",
      category: "ai_provider",
      name: aiRowName("bedrock"),
      typeLabel: AI_TYPE_LABEL.bedrock,
      chips: capabilityChips("bedrock"),
      residency: aiResidency("bedrock", undefined, lane),
      posture,
      secretNames: bedrockSecretNames(lane, present),
      harnessProvider: lane === "sso" ? "aws" : undefined,
      aiType: "bedrock",
      bedrockLane: lane,
      checkIds: ["bedrock_provider"],
    });
  }

  if (hasSecret(present, "openai-api-key")) {
    rows.push({
      id: "ai:openai_api_key",
      category: "ai_provider",
      name: aiRowName("openai_api_key"),
      typeLabel: AI_TYPE_LABEL.openai_api_key,
      chips: capabilityChips("openai_api_key"),
      residency: aiResidency("openai_api_key", undefined, undefined),
      posture: { kind: "configured" },
      secretNames: ["openai-api-key"],
      aiType: "openai_api_key",
      checkIds: ["llm_provider"],
    });
  }

  // W5: no site-config field for an Azure endpoint yet, and no dedicated
  // SetupCheck id either — the composer backend registry is the only real
  // signal; the conventional secret name is the fallback so a key stored
  // ahead of a composer config still surfaces as an integration.
  const azureBackend = status.composer.backends.find((b) => b.provider === "azure");
  if (azureBackend || hasSecret(present, "azure-openai-key")) {
    const secretName = azureBackend?.key_secret ?? "azure-openai-key";
    // key_resolved is composer's own "was this present at boot" verdict for
    // THIS backend's key — more direct than re-checking the general secret
    // list, which is what it's for (see ComposerBackendReadiness's doc comment).
    const resolved = azureBackend ? azureBackend.key_resolved : hasSecret(present, secretName);
    rows.push({
      id: "ai:azure_openai",
      category: "ai_provider",
      name: azureBackend?.name ?? "Azure OpenAI",
      typeLabel: AI_TYPE_LABEL.azure_openai,
      chips: capabilityChips("azure_openai"),
      residency: aiResidency("azure_openai", undefined, undefined),
      posture: resolved ? { kind: "configured" } : { kind: "region_model_unset" },
      secretNames: resolved ? [secretName] : [],
      aiType: "azure_openai",
      checkIds: [],
    });
  }

  return rows;
}

// ---- SCM hosts --------------------------------------------------------------

// Only the kinds a stored credential can actually produce (deriveProviders
// only pushes a lane when its secret/App flag is real) — brokered for the App,
// resident for a written key/token. A host with zero lanes isn't a stored
// connection yet (just an egress-allowlist entry from the SCM Provider step),
// so it's not "an integration" on this screen.
const LANE_RESIDENCY: Record<Lane, ResidencyKind> = {
  app: "brokered_mint",
  pat: "resident_env",
  ssh: "resident_mount",
};

function scmResidency(lanes: Lane[]): ResidencyKind {
  const kinds = new Set(lanes.map((l) => LANE_RESIDENCY[l]));
  return kinds.size === 1 ? [...kinds][0] : "varies";
}

function deriveScmRows(status: SetupStatus, siteConfig: SiteConfig | null, present: string[]): IntegrationRow[] {
  const rows = deriveProviders(present, siteConfig?.scm_hosts ?? [], status.secrets.github_app);
  return rows
    .filter((r) => r.lanes.length > 0)
    .map((r) => {
      const isGithubApp = r.host === "github.com" && r.lanes.includes("app");
      const slug = slugHost(r.host);
      const secretNames = isGithubApp
        ? ["github-app-id", "github-app-key"]
        : r.lanes
            .filter((l) => l !== "app")
            .map((l) => (l === "ssh" ? `ssh-key-${slug}` : `git-pat-${slug}`))
            .filter((n) => hasSecret(present, n));
      return {
        id: `scm:${r.host}`,
        category: "scm_host" as const,
        name: r.brand,
        typeLabel: r.host,
        chips: r.lanes.map((l) => ({ label: LANE_META[l].label, tone: LANE_META[l].tone, tooltip: LANE_META[l].tooltip })),
        residency: scmResidency(r.lanes),
        // W5: the ref-confinement check that would answer Ref-confined/
        // Unconfined doesn't exist server-side yet — Unknown is the honest
        // default until it does. Re-check still refreshes real local facts
        // (e.g. the Stored check below), so it's wired, not decorative.
        posture: isGithubApp ? { kind: "gh_verdict", verdict: "unknown", checkedLabel: "not yet" } : { kind: "configured" },
        secretNames,
        checkIds: ["scm_provider"],
        canReCheck: isGithubApp,
        isGithubApp,
      };
    });
}

// ---- Host proxy / egress redirection: NOT derived here ----------------------
// Both used to be categories on this page. They aren't integrations: an
// integration is an account with a system outside Wardyn, while a proxy and an
// internal mirror are network topology — and a redirect carries a proof
// obligation the Corporate network step's gate enforces (every configured row
// must test "reached" before that step hands off). Corporate network is their
// single home now; nothing below derives a row for either. The one thing that
// stayed is the detection banner, which points there.

// A corporate proxy was DETECTED on the host but nothing is connected yet —
// the one condition T.PROXY_BANNER exists for. A plain upstream_proxy_url
// counts as configured just as much as a secret ref does (the two are
// mutually exclusive in practice — see SiteConfig.upstream_proxy_url's doc
// comment), so either one silences the banner.
export function proxyBannerNeeded(status: SetupStatus, siteConfig: SiteConfig | null): boolean {
  if (siteConfig?.upstream_proxy_url || siteConfig?.upstream_proxy_secret_ref) return false;
  const d = status.host_proxy;
  return !!(d && (d.http_proxy || d.https_proxy || d.all_proxy || d.pac));
}

export function deriveIntegrations(status: SetupStatus, siteConfig: SiteConfig | null, secretNames: string[]): IntegrationsData {
  const present = secretNames.length ? secretNames : status.secrets.present;
  return {
    ai: deriveAiRows(status, present),
    scm: deriveScmRows(status, siteConfig, present),
  };
}

export function allRows(data: IntegrationsData): IntegrationRow[] {
  return [...data.ai, ...data.scm];
}

export function findRow(data: IntegrationsData, id: string): IntegrationRow | undefined {
  return allRows(data).find((r) => r.id === id);
}

// ---- Posture -> display text/tone -------------------------------------------

const GH_VERDICT_LABEL = { ref_confined: "Ref-confined", unconfined: "Unconfined", unknown: "Unknown" } as const;
const GH_VERDICT_TONE = { ref_confined: "success", unconfined: "warning", unknown: "muted" } as const;

export function describePosture(p: Posture): { text: string; tone: "success" | "warning" | "muted" } {
  switch (p.kind) {
    case "configured":
      return { text: "Configured", tone: "muted" };
    case "captured":
      return { text: `Captured ${p.ageLabel}`, tone: "muted" };
    case "reconnect_soon":
      return { text: "Reconnect soon", tone: "warning" };
    case "session_expires":
      return { text: `Session expires ${p.when}`, tone: "muted" };
    case "region_model_unset":
      return { text: "Region/model unset", tone: "warning" };
    case "gh_verdict":
      return { text: `${GH_VERDICT_LABEL[p.verdict]} · checked ${p.checkedLabel}`, tone: GH_VERDICT_TONE[p.verdict] };
  }
}

// ---- Danger zone / blast radius ----------------------------------------------

// Computed from what THIS adapter can actually see — never a fabricated
// workspace count (W5: workspace LLM-cred pins aren't one of this module's
// three source endpoints, so that line is a plain statement, not a number).
export function blastRadius(row: IntegrationRow, opts: { isDefaultAgent?: boolean; isDefaultFeatures?: boolean } = {}): string[] {
  const lines: string[] = [];
  if (row.category === "ai_provider") {
    if (opts.isDefaultAgent) lines.push("Agent runs that resolve the server default lose model access — their first model call fails.");
    if (opts.isDefaultFeatures) lines.push("Wardyn’s Composer loses its backend — ‘Describe your task’ disappears from New Run.");
    lines.push("Workspaces pinned to this integration fall back to the server default.");
    if (row.harnessProvider) {
      lines.push(`The stored session is not deleted by this — disconnecting IS the removal for a harness login.`);
    } else if (row.secretNames.length) {
      lines.push(`The stored secret ${row.secretNames.join(" and ")} is not deleted — remove it under Secrets.`);
    }
  } else if (row.category === "scm_host") {
    lines.push(`Runs stop inheriting ${row.typeLabel} in their egress allowlist.`);
    if (row.secretNames.length) lines.push(`The stored credential is not deleted — remove it under Secrets.`);
  }
  return lines;
}

export const integrationsApi = {
  // W5: GET /api/v1/integrations doesn't exist yet — compose the three real
  // endpoints and derive rows client-side until it does.
  async list(): Promise<IntegrationsData> {
    const [status, siteConfig, secretNames] = await Promise.all([
      setupApi.getSetupStatus(),
      health.getSiteConfig(),
      secretsApi.listSecrets(),
    ]);
    return deriveIntegrations(status, siteConfig, secretNames);
  },
};
