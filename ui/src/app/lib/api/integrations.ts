/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations — GET /api/v1/integrations exists (server.go registers it,
// returning the same effective-integration-plus-capabilities rows
// SetupStatus.integrations carries). This module still derives the
// LEGACY SCM-host category client-side from setup status +
// site config + secret names instead of reading that endpoint directly,
// exactly the discipline lib/scm-provider.ts already established for the SCM
// Provider step (a "provider row" there is not a backend entity either):
// the wire row is a flat credential record with no posture (captured/aging/
// reconnect-soon), no per-lane breakdown (ssh vs. pat vs. app), and no
// capability CHIPS — only the raw matrix — none of which the server derives
// for this category the way `deriveScmRows` below does.
// The eight GENERIC categories have no such legacy derivation (see
// genericIntegrations further down) and read the wire row as-is. No invented
// server concepts: every field on IntegrationRow traces back to a real
// response field, named in the comment next to it.
//
// Some seams below are deliberate stand-ins for something a later wave
// should replace with a real field/endpoint (GitHub ref-confinement,
// per-integration "default" persistence, an Azure endpoint URL, workspace
// pin-counts for the blast radius).
import type { IntegrationCategory, ResidencyKind } from "../integrations";
import { deriveProviders, LANE_META, patLaneMeta, slugHost, type Lane } from "../scm-provider";
import type { SetupStatus, SiteConfig } from "../types";
import { setup as setupApi } from "./setup";
import { health } from "./health";
import { secrets as secretsApi } from "./secrets";

export type { IntegrationCategory };

export interface RowChip {
  label: string;
  tone: "info" | "success" | "warning" | "neutral";
  tooltip?: string;
}

// Presence/aging language ONLY (never a live probe), except `gh_verdict` — the
// one GitHub-ref-confinement exception the whole module exists to carry.
export type Posture =
  | { kind: "configured" }
  | { kind: "captured"; ageLabel: string }
  | { kind: "reconnect_soon" }
  | { kind: "session_expires"; when: string }
  | { kind: "gh_verdict"; verdict: "ref_confined" | "unconfined" | "unknown"; checkedLabel: string };

export interface IntegrationRow {
  id: string;
  /** The SERVER-side integration id this row corresponds to — stored, or
   *  adoptable via POST /integrations/{serverId}/adopt. Client row ids are a
   *  display namespace ("scm:…"); contracts must name THIS one. Absent when the
   *  row has no server-side identity to adopt. */
  serverId?: string;
  category: IntegrationCategory;
  name: string;
  /** Mono sub-line under the name, e.g. a host. */
  typeLabel: string;
  chips: RowChip[];
  residency: ResidencyKind;
  posture: Posture;
  /** Secret name(s) this row is backed by — rotate/delete act on these. */
  secretNames: string[];
  /** status.checks ids relevant to this row, rendered verbatim via CheckRow. */
  checkIds: string[];
  /** True only for the GitHub App row — the one row with a live check. */
  canReCheck?: boolean;
  isGithubApp?: boolean;
}

export interface IntegrationsData {
  scm: IntegrationRow[];
}

// SCM hosts

// #381 F3: pat's meta depends on the real WARDYN_GIT_PAT_BROKER switch;
// app/ssh keep reading the static LANE_META. Shared by scmResidency and the
// chip generation below so the two can never disagree with each other.
function laneMeta(l: Lane, patBrokerEnabled: boolean) {
  return l === "pat" ? patLaneMeta(patBrokerEnabled) : LANE_META[l];
}

// Only the kinds a stored credential can actually produce (deriveProviders
// only pushes a lane when its secret/App flag is real) — brokered for the App,
// resident for a written key/token.
function scmResidency(lanes: Lane[], patBrokerEnabled: boolean): ResidencyKind {
  const kinds = new Set(lanes.map((l) => laneMeta(l, patBrokerEnabled).residency));
  return kinds.size === 1 ? [...kinds][0] : "varies";
}

function deriveScmRows(status: SetupStatus, siteConfig: SiteConfig | null, present: string[]): IntegrationRow[] {
  // #381 F3: the real switch when siteConfig carries it (an operator's own
  // GET /site-config projects workspace_providers.git_pat_broker_enabled the
  // same way GET /workspace-providers does) — the 0.7.10 default otherwise,
  // for a member caller (siteConfig is operator-only and null for them) or a
  // never-configured install (absent until a git row exists).
  const patBrokerEnabled = siteConfig?.workspace_providers?.git_pat_broker_enabled ?? true;
  // effective_scm_hosts is the server's projected union (workspace providers
  // MINUS every host a provider row claims, UNION every enabled row's hosts) —
  // the one spelling of the claim rule, in Go (internal/api/workspace_providers.go
  // effectiveScmHosts). Older daemons omit it, so a disabled/claimed host still
  // reads as "Connected" against them; `?? scm_hosts` is that fallback, not a
  // second opinion about the rule.
  const rows = deriveProviders(present, siteConfig?.effective_scm_hosts ?? siteConfig?.scm_hosts ?? [], status.secrets.github_app);
  return rows.map((r) => {
    // A host registered in scm_hosts but with no stored credential yet still
    // widens every future run's egress allowlist the moment it's added, so
    // dropping the row here would make that add look like a no-op while it
    // silently keeps widening egress. Render it as a minimal, real, deletable
    // row instead of hiding it. `derivedFrom` guess rows never reach this
    // branch (deriveProviders always seeds one lane for those), so this is
    // exactly the scm_hosts-registered, credential-less case.
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
      chips: r.lanes.map((l) => {
        const m = laneMeta(l, patBrokerEnabled);
        return { label: m.label, tone: m.tone, tooltip: m.tooltip };
      }),
      residency: scmResidency(r.lanes, patBrokerEnabled),
      // The ref-confinement check that would answer Ref-confined/Unconfined
      // doesn't exist server-side yet — Unknown is the honest default until
      // it does. Re-check still refreshes real local facts (e.g. the Stored
      // check below), so it's wired, not decorative.
      posture: isGithubApp ? { kind: "gh_verdict", verdict: "unknown", checkedLabel: "not yet" } : { kind: "configured" },
      secretNames,
      checkIds: ["scm_provider"],
      canReCheck: isGithubApp,
      isGithubApp,
    };
  });
}

// Host proxy / egress redirection are not derived here. Neither is an
// integration: an integration is an account with a system outside Wardyn,
// while a proxy and an internal mirror are network topology. Corporate
// network is their single home; the "a proxy was detected but nothing is
// connected" banner is driven by isProxyConfigured/proxyDetected
// (screens/setup/corp-network-proxy.tsx).

export function deriveIntegrations(status: SetupStatus, siteConfig: SiteConfig | null, secretNames: string[]): IntegrationsData {
  const present = secretNames.length ? secretNames : status.secrets.present;
  return {
    scm: deriveScmRows(status, siteConfig, present),
  };
}

export const integrationsApi = {
  // GET /api/v1/integrations exists, but returns the flat wire row — no posture,
  // no SCM lane breakdown, no capability chips — so this composes the three
  // endpoints that carry those facts and derives rows client-side. There is no
  // promote-a-derived-row-into-a-stored-one call: nothing in the console adopts
  // a row through this client.
  async list(): Promise<IntegrationsData> {
    const [status, siteConfig, secretNames] = await Promise.all([
      setupApi.getSetupStatus(),
      health.getSiteConfig(),
      secretsApi.listSecrets(),
    ]);
    return deriveIntegrations(status, siteConfig, secretNames);
  },
};

// Generic integration kinds are no longer a kind Wardyn accepts — there is no
// /integrations catalog page or Add dialog. What remains above is the SCM
// derivation. The model-key rows it once carried are gone: a model credential
// comes only from a model provider (#548), read from SetupStatus.model_providers.
