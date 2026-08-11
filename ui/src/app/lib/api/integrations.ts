/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations — GET /api/v1/integrations exists (server.go registers it,
// returning the same effective-integration-plus-capabilities rows
// SetupStatus.integrations carries). This module still derives the two
// LEGACY categories (AI provider, SCM host) client-side from setup status +
// site config + secret names instead of reading that endpoint directly,
// exactly the discipline lib/scm-provider.ts already established for the SCM
// Provider step (a "provider row" there is not a backend entity either):
// the wire row is a flat credential record with no posture (captured/aging/
// reconnect-soon), no per-lane breakdown (ssh vs. pat vs. app), and no
// capability CHIPS — only the raw matrix — none of which the server derives
// for these two categories the way `deriveAiRows`/`deriveScmRows` below do.
// The eight GENERIC categories have no such legacy derivation (see
// genericIntegrations further down) and read the wire row as-is. No invented
// server concepts: every field on IntegrationRow traces back to a real
// response field, named in the comment next to it.
//
// W5: marks every seam that's a deliberate stand-in for something a later
// wave should replace with a real field/endpoint (GitHub ref-confinement,
// per-integration "default" persistence, an Azure endpoint URL, workspace
// pin-counts for the blast radius).
import type { BedrockLane, IntegrationCategory, ResidencyKind } from "../integrations";
import { AI_TYPES, BEDROCK_LANE_META, SUBSCRIPTION_LANE_META, type AiType, type CapabilityRow } from "../integrations";
import { deriveProviders, LANE_META, slugHost, type Lane } from "../scm-provider";
import { relativeTime, clockTime } from "../format";
import type { SetupStatus, SiteConfig } from "../types";
import {
  INTEGRATION_GROUPS,
  integrationTypeById,
  type IntegrationGroup,
  type IntegrationTypeMeta,
} from "../integration-catalog";
import type { WireIntegration } from "../types/setup";
import { HttpError, wfetch, errText } from "./core";
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
  /** The SERVER-side integration id this row corresponds to — stored, or
   *  adoptable via POST /integrations/{serverId}/adopt. Client row ids are a
   *  display namespace ("ai:…"); contracts must name THIS one. Absent when the
   *  row has no server-side identity to adopt. */
  serverId?: string;
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

const AGENT_SLOT = /Claude Code|Codex/;
const FEATURES_SLOT = /^Wardyn features/;

// The server-side adoptable id for a legacy AI row of this type/lane — the
// SAME ids deriveAiRows below stamps as serverId, factored out so a caller
// that hasn't loaded a derived row yet (the Add dialog, before its first
// reload) can still resolve which wire row to adopt/PUT (UI-WS-2). Undefined
// for azure_openai: no site-config field / SetupCheck id exists for it yet
// (see the azure branch below), so there is nothing to adopt.
export function aiServerId(type: AiType, hostCli?: boolean): string | undefined {
  switch (type) {
    case "anthropic_api_key":
      return "anthropic_api_key";
    case "anthropic_subscription":
      return hostCli ? "anthropic_subscription:resident_host" : "anthropic_subscription:managed";
    case "bedrock":
      return "bedrock";
    case "openai_api_key":
      return "openai_api_key";
    case "azure_openai":
      return undefined;
  }
}

// Overlays the REAL default_for state onto a type's static capability
// preview: `def` (the "default" chip/label) flips on live state once a server
// identity (`wire`) backs the row, instead of the static per-type guess the
// preview ships with (CAPS.key()'s hardcoded `def: true`, etc). A row with no
// wire yet keeps the static preview verbatim — there's nothing live to read.
// Shared by capabilityChips below (so the list chip, the Add dialog's
// defaultHolder() and onboarding's llmLabel all read one source) and the
// detail page's CapabilityTable, so no two surfaces can independently drift
// on which row actually holds a mark the way the list chip and the kebab
// checkbox once did.
export function liveCapRows(rows: CapabilityRow[], wire: WireIntegration | undefined, defAgent: boolean, defFeat: boolean): CapabilityRow[] {
  if (!wire) return rows;
  return rows.map((r) => {
    if (!r.on) return r;
    if (AGENT_SLOT.test(r.label)) return { ...r, def: defAgent, makeDefault: !defAgent };
    if (FEATURES_SLOT.test(r.label)) return { ...r, def: defFeat, makeDefault: !defFeat };
    return r;
  });
}

// The compact chip list a list row shows: ON rows (capChip, `· default` when
// `wire`'s live default_for says so, falling back to the type's static matrix
// when there's no wire), impossible rows (factChip, muted, the verbatim
// reason as tooltip, labeled "· n/a" so the state reads without the tooltip)
// and OFF-but-fixable rows (muted, labeled "· off" — a `note` with no `fact`
// means it's not an impossibility, just not on for this instance; the
// anthropic_subscription hostCli lane's Wardyn-features row is the one CAPS
// entry shaped this way today). A row that is neither on, a fact, nor noted
// is a true nothing-to-say OFF and stays omitted, matching the mock.
export function capabilityChips(type: AiType, hostCli?: boolean, wire?: WireIntegration): RowChip[] {
  const defAgent = !!wire?.default_for?.includes("agent_runs");
  const defFeat = !!wire?.default_for?.includes("wardyn_features");
  return liveCapRows(AI_TYPES[type].capabilityPreview(hostCli), wire, defAgent, defFeat)
    .filter((r) => r.on || r.fact || r.note)
    .map((r) =>
      r.fact
        ? { label: `${r.label} · n/a`, tone: "neutral", muted: true, tooltip: r.fact }
        : r.on
          ? { label: r.def ? `${r.label} · default` : r.label, tone: "info" }
          : { label: `${r.label} · off`, tone: "neutral", muted: true, tooltip: r.note },
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

// resolveBedrockAuth's precedence (internal/api/runs_bedrock.go): bearer >
// AWS SSO session > host ~/.aws mount > static access keys. Undefined when
// only region/model are set — no credential lane is actually active yet.
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
  if (lane === "static") return ["aws-access-key-id", "aws-secret-access-key", "aws-session-token"].filter((n) => present.includes(n));
  return []; // sso is a harness credential; aws_dir is boot config — neither is a secret-store entry
}

function deriveAiRows(status: SetupStatus, present: string[]): IntegrationRow[] {
  const rows: IntegrationRow[] = [];
  const claude = status.providers.find((p) => p.tool === "claude");
  // The live wire row behind each id below (status.integrations) — the SAME
  // lookup integrations-screen.tsx's own wireById performs for the kebab
  // checkbox, so capabilityChips can overlay the real default_for instead of
  // guessing from the static per-type table.
  const wireById = new Map((status.integrations ?? []).map((w) => [w.id, w]));

  if (present.includes("anthropic-api-key")) {
    rows.push({
      id: "ai:anthropic_api_key",
      serverId: aiServerId("anthropic_api_key"),
      category: "ai_provider",
      name: aiRowName("anthropic_api_key"),
      typeLabel: AI_TYPE_LABEL.anthropic_api_key,
      chips: capabilityChips("anthropic_api_key", undefined, wireById.get("anthropic_api_key")),
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
      serverId: aiServerId("anthropic_subscription", true),
      category: "ai_provider",
      name: aiRowName("anthropic_subscription", true),
      typeLabel: subscriptionTypeLabel(true),
      chips: capabilityChips("anthropic_subscription", true, wireById.get("anthropic_subscription:resident_host")),
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
      serverId: aiServerId("anthropic_subscription", false),
      category: "ai_provider",
      name: aiRowName("anthropic_subscription", false),
      typeLabel: subscriptionTypeLabel(false),
      chips: capabilityChips("anthropic_subscription", false, wireById.get("anthropic_subscription:managed")),
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
      serverId: aiServerId("bedrock"),
      category: "ai_provider",
      name: aiRowName("bedrock"),
      typeLabel: AI_TYPE_LABEL.bedrock,
      chips: capabilityChips("bedrock", undefined, wireById.get("bedrock")),
      residency: aiResidency("bedrock", undefined, lane),
      posture,
      secretNames: bedrockSecretNames(lane, present),
      harnessProvider: lane === "sso" ? "aws" : undefined,
      aiType: "bedrock",
      bedrockLane: lane,
      checkIds: ["bedrock_provider"],
    });
  }

  if (present.includes("openai-api-key")) {
    rows.push({
      id: "ai:openai_api_key",
      serverId: aiServerId("openai_api_key"),
      category: "ai_provider",
      name: aiRowName("openai_api_key"),
      typeLabel: AI_TYPE_LABEL.openai_api_key,
      chips: capabilityChips("openai_api_key", undefined, wireById.get("openai_api_key")),
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
  if (azureBackend || present.includes("azure-openai-key")) {
    const secretName = azureBackend?.key_secret ?? "azure-openai-key";
    // key_resolved is composer's own "was this present at boot" verdict for
    // THIS backend's key — more direct than re-checking the general secret
    // list, which is what it's for (see ComposerBackendReadiness's doc comment).
    const resolved = azureBackend ? azureBackend.key_resolved : present.includes(secretName);
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
// resident for a written key/token.
function scmResidency(lanes: Lane[]): ResidencyKind {
  const kinds = new Set(lanes.map((l) => LANE_META[l].residency));
  return kinds.size === 1 ? [...kinds][0] : "varies";
}

function deriveScmRows(status: SetupStatus, siteConfig: SiteConfig | null, present: string[]): IntegrationRow[] {
  const rows = deriveProviders(present, siteConfig?.scm_hosts ?? [], status.secrets.github_app);
  return rows.map((r) => {
    // SCM-SEAM-2: a host registered in scm_hosts but with no stored credential
    // yet still widens every future run's egress allowlist the moment it's
    // added — dropping the row here (as this used to) makes that add look
    // like a no-op while it silently keeps widening egress. Render it as a
    // minimal, real, deletable row instead of hiding it. `derivedFrom` guess
    // rows never reach this branch (deriveProviders always seeds one lane for
    // those), so this is exactly the scm_hosts-registered, credential-less case.
    if (r.lanes.length === 0) {
      return {
        id: `scm:${r.host}`,
        category: "scm_host" as const,
        name: r.brand,
        typeLabel: r.host,
        chips: [],
        residency: "notbuilt" as const,
        posture: { kind: "configured" as const },
        secretNames: [],
        checkIds: [],
      };
    }
    const isGithubApp = r.host === "github.com" && r.lanes.includes("app");
    const slug = slugHost(r.host);
    const secretNames = isGithubApp
      ? ["github-app-id", "github-app-key"]
      : r.lanes
          .filter((l) => l !== "app")
          .map((l) => (l === "ssh" ? `ssh-key-${slug}` : `git-pat-${slug}`))
          .filter((n) => present.includes(n));
    return {
      id: `scm:${r.host}`,
      // The server-side adoptable id for this host (internal/api/
      // integrations.go): the GitHub App row is minted as the fixed id
      // "github_app" (:517), every other git host as "git_host:<host>"
      // (:660) — even github.com gets that id when the app lane isn't the
      // one present. Without this, an SCM host can never be adopted/named
      // in a workspace requirements contract the way every AI row already can.
      serverId: isGithubApp ? "github_app" : `git_host:${r.host}`,
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

export function findRow(data: IntegrationsData, id: string): IntegrationRow | undefined {
  return [...data.ai, ...data.scm].find((r) => r.id === id);
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
  // POST /api/v1/integrations/{id}/adopt — persist a DERIVED legacy row so it
  // becomes a real stored Integration (verbatim, same id). 409 = already
  // stored, which callers treat as success (the goal state holds).
  async adoptIntegration(serverId: string): Promise<void> {
    const res = await wfetch(`/integrations/${encodeURIComponent(serverId)}/adopt`, { method: "POST" });
    if (!res.ok && res.status !== 409) throw new HttpError(res.status, await errText(res));
  },

  // GET /api/v1/integrations exists, but returns the flat wire row (see the
  // module header above) — no posture, no SCM lane breakdown, no capability
  // chips — so this composes the three endpoints that carry those facts and
  // derives rows client-side, same as it always has.
  async list(): Promise<IntegrationsData> {
    const [status, siteConfig, secretNames] = await Promise.all([
      setupApi.getSetupStatus(),
      health.getSiteConfig(),
      secretsApi.listSecrets(),
    ]);
    return deriveIntegrations(status, siteConfig, secretNames);
  },
};

// ─── Generic integrations: the server's own rows ─────────────────────────────
//
// Everything above derives the two LEGACY categories (AI providers, SCM hosts)
// client-side from the endpoints that predate the entity. Everything below is
// the real thing: the server returns the effective set on SetupStatus, and the
// eight GENERIC categories — package feeds, container registries, cloud, data
// stores, MCP, work tracking, observability and the catch-all — have no
// client-side derivation at all, because there is nothing older to derive them
// from. A row carries its own hosts, header and secret, which is the whole
// contract.

/** One generic integration ready to render: the wire row plus its catalog facts. */
export interface GenericIntegrationRow {
  wire: WireIntegration;
  group: IntegrationGroup;
  /** The catalog entry when the type is one Wardyn knows; absent for a hand-named service. */
  meta?: IntegrationTypeMeta;
  name: string;
  hosts: string[];
  /** Stated fact, derived from the row itself — never an operator's choice. */
  delivery: ResidencyKind;
}

/** category -> group, so a wire row finds the section it belongs in. */
const GROUP_BY_CATEGORY = new Map(INTEGRATION_GROUPS.map((g) => [g.category as string, g]));

// deliveryForRow states how THIS row's credential reaches a request, from the row
// itself rather than from its type: a header naming a stored secret is
// proxy-injected, and anything else honestly has no lane. The catalog's own
// delivery is the fallback for the types with bespoke lanes (varies, brokered)
// that a generic row never has.
function deliveryForRow(wire: WireIntegration, meta?: IntegrationTypeMeta): ResidencyKind {
  if (wire.header && wire.credentials?.token) return "proxy_injected";
  if (meta && meta.delivery !== "proxy_injected") return meta.delivery;
  return "notbuilt";
}

// genericIntegrations selects the rows belonging to the eight generic categories
// and pairs each with its section and catalog entry. Rows in the two legacy
// categories are left out deliberately — they are rendered from the derivation
// above, which knows about lanes, posture and capability chips a generic row
// simply doesn't have.
export function genericIntegrations(status: SetupStatus): GenericIntegrationRow[] {
  const rows: GenericIntegrationRow[] = [];
  for (const wire of status.integrations ?? []) {
    const group = GROUP_BY_CATEGORY.get(wire.category);
    if (!group || group.id === "model" || group.id === "scm") continue;
    const meta = integrationTypeById(wire.type);
    rows.push({
      wire,
      group,
      meta,
      name: wire.name || meta?.label || wire.id,
      hosts: wire.hosts ?? [],
      delivery: deliveryForRow(wire, meta),
    });
  }
  return rows;
}

/** Rows grouped into their sections, in the catalog's own order, empties dropped. */
export function genericSections(rows: GenericIntegrationRow[]): { group: IntegrationGroup; rows: GenericIntegrationRow[] }[] {
  return INTEGRATION_GROUPS.filter((g) => g.id !== "model" && g.id !== "scm")
    .map((group) => ({ group, rows: rows.filter((r) => r.group.id === group.id) }))
    .filter((s) => s.rows.length > 0);
}

/** The body PUT /integrations/{id} takes — every operator-settable field.
 *  REPLACE semantics, not a merge (putIntegrationRequest's own doc comment):
 *  a field omitted here is written back as its zero value, silently wiping
 *  whatever the stored row had. Round-trip a GET and spread onto it before
 *  changing one field, same discipline as SiteConfig's PUT. */
export interface IntegrationWrite {
  name: string;
  category: string;
  type: string;
  disabled?: boolean;
  hosts?: string[];
  header?: string;
  format?: string;
  docs?: string;
  credentials?: Record<string, string>;
  /** Type-specific knobs (e.g. "lane", "ecosystems") — arbitrary JSON, mirrors
   *  the server's json.RawMessage (putIntegrationRequest.Config). */
  config?: Record<string, unknown>;
  /** Capability ids the operator turned off individually, overriding what the
   *  live matrix would otherwise report. */
  disabled_capabilities?: string[];
  /** What this integration is the operator-chosen default for (e.g.
   *  "agent_runs"). RADIO semantics server-side: naming a mark here clears it
   *  from every OTHER stored row in the same write. */
  default_for?: string[];
}

export const genericIntegrationsApi = {
  // PUT /api/v1/integrations/{id} — create or REPLACE (no partial merge).
  async put(id: string, body: IntegrationWrite): Promise<void> {
    const res = await wfetch(`/integrations/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(await errText(res));
  },
  // DELETE /api/v1/integrations/{id}. The operator's stored secrets are NOT
  // deleted — the surface says so where it offers this. Throws HttpError (not a
  // bare Error) so a caller can tell a 404 ("no stored integration" — a legacy
  // row that was never adopted has nothing to delete) from a real failure;
  // actions.ts's deleteIntegration relies on that distinction.
  async remove(id: string): Promise<void> {
    const res = await wfetch(`/integrations/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
  },
};
