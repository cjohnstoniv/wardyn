/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { baseStatus } from "../../components/screens/setup/test-fixtures";
import { T } from "../integrations";
import {
  aiServerId,
  deriveIntegrations,
  proxyBannerNeeded,
  describePosture,
  blastRadius,
  type IntegrationRow,
} from "./integrations";
import type { SetupStatus, SiteConfig } from "../types";

// UI-WS-2: the Add dialog resolves which wire row to adopt/PUT via this
// helper, BEFORE its first reload can hand it a derived IntegrationRow of its
// own — pinned here as the single source of truth deriveAiRows' own serverId
// values are drawn from, so the two can't drift apart.
describe("aiServerId", () => {
  it("matches deriveAiRows' own serverId for every type that has one", () => {
    expect(aiServerId("anthropic_api_key")).toBe("anthropic_api_key");
    expect(aiServerId("anthropic_subscription", true)).toBe("anthropic_subscription:resident_host");
    expect(aiServerId("anthropic_subscription", false)).toBe("anthropic_subscription:managed");
    expect(aiServerId("bedrock")).toBe("bedrock");
    expect(aiServerId("openai_api_key")).toBe("openai_api_key");
  });

  it("is undefined for azure_openai — no site-config field/SetupCheck id exists to adopt", () => {
    expect(aiServerId("azure_openai")).toBeUndefined();
  });
});

describe("deriveIntegrations — empty inputs", () => {
  it("derives nothing when nothing is configured", () => {
    const data = deriveIntegrations(baseStatus(), null, []);
    expect(data.ai).toHaveLength(0);
    expect(data.scm).toHaveLength(0);
  });
});

describe("deriveIntegrations — AI providers", () => {
  it("anthropic-api-key -> a row whose Codex chip is the muted, verbatim-reason fact and Claude Code/Wardyn features carry '· default'", () => {
    const status = baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } });
    const [row] = deriveIntegrations(status, null, ["anthropic-api-key"]).ai;
    expect(row.typeLabel).toBe("anthropic · api key");
    expect(row.residency).toBe("proxy_injected");
    const codex = row.chips.find((c) => c.label === "Codex CLI · n/a")!;
    expect(codex.muted).toBe(true);
    expect(codex.tooltip).toBe(T.X_KEY_CODEX);
    expect(row.chips.find((c) => c.label === "Claude Code · default")).toBeTruthy();
    expect(row.chips.find((c) => c.label === "Wardyn features · default")).toBeTruthy();
    expect(row.secretNames).toEqual(["anthropic-api-key"]);
  });

  // The chip used to be a pure function of CAPS.key()'s hardcoded `def: true`
  // — a checkbox elsewhere could mark a DIFFERENT row the real default and
  // this chip would never know. It must follow status.integrations' live
  // default_for instead, the same source the kebab checkbox itself reads.
  it("the '· default' chip follows the LIVE wire default_for, not the static per-type table", () => {
    const status = baseStatus({
      secrets: { present: ["anthropic-api-key"], github_app: false },
      bedrock: { region: "us-east-1", model: "anthropic.claude-3", creds_present: false },
      integrations: [
        { id: "anthropic_api_key", kind: "anthropic_api_key", source: "stored", default_for: [] },
        { id: "bedrock", kind: "bedrock", source: "stored", default_for: ["agent_runs"] },
      ],
    });
    const { ai } = deriveIntegrations(status, null, ["anthropic-api-key"]);
    const keyRow = ai.find((r) => r.id === "ai:anthropic_api_key")!;
    const bedrockRow = ai.find((r) => r.id === "ai:bedrock")!;
    // The static table (CAPS.key()) still says def:true for the key row — the
    // live wire row says otherwise, and live wins: the chip drops "· default".
    expect(keyRow.chips.find((c) => c.label === "Claude Code · default")).toBeUndefined();
    expect(keyRow.chips.find((c) => c.label === "Claude Code")).toBeTruthy();
    // Bedrock's static table (CAPS.bedrock()) never sets `def` at all — this
    // chip can only come from the live overlay.
    expect(bedrockRow.chips.find((c) => c.label === "Claude Code · default")).toBeTruthy();
  });

  it("a resident host-CLI subscription renders the impossible pair as '· n/a' and Wardyn features as the OFF-but-fixable '· off' chip, not omitted", () => {
    const status = baseStatus({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }] });
    const [row] = deriveIntegrations(status, null, []).ai;
    expect(row.hostCli).toBe(true);
    expect(row.chips.map((c) => c.label)).toEqual([
      "Claude Code",
      "Codex CLI · n/a",
      "Direct API calls · n/a",
      "Wardyn features · off",
    ]);
    const features = row.chips.find((c) => c.label === "Wardyn features · off")!;
    expect(features.muted).toBe(true);
    expect(features.tooltip).toBe("Off for this lane — no switch in this console turns it on.");
    expect(row.residency).toBe("resident_mount");
  });

  // UI-LIB-3: auth_mode is only ever set once Wardyn PEEKS a real subscription
  // token — but the server's own llm_provider check counts ANY logged-in CLI
  // as real access (an api-key session, or a subscription it simply can't
  // read). Without this row the console read "Needs setup" on a payload the
  // funnel's own Review step showed as "ok".
  it("a resident CLI login with no confirmed subscription still renders — real access either way", () => {
    const status = baseStatus({ providers: [{ tool: "claude", installed: true, logged_in: true }] });
    const [row] = deriveIntegrations(status, null, []).ai;
    expect(row.id).toBe("ai:anthropic_cli_login");
    expect(row.serverId).toBeUndefined();
    expect(row.hostCli).toBe(true);
    expect(row.chips.find((c) => c.label === "Claude Code" && !c.muted)).toBeTruthy();
  });

  // The two rows are mutually exclusive — a CONFIRMED subscription must never
  // also render the unconfirmed row's honest-but-vaguer name.
  it("a confirmed subscription (auth_mode: subscription) takes the real row, not the unconfirmed one", () => {
    const status = baseStatus({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }] });
    const { ai } = deriveIntegrations(status, null, []);
    expect(ai).toHaveLength(1);
    expect(ai[0].id).toBe("ai:anthropic_subscription:host");
  });

  it("a managed subscription reads Captured Xd ago when fresh, and Reconnect soon when aging", () => {
    const fresh = baseStatus({ harness: [{ provider: "anthropic", captured: true, captured_at: new Date(Date.now() - 3 * 86400_000).toISOString() }] });
    const freshRow = deriveIntegrations(fresh, null, []).ai.find((r) => r.id === "ai:anthropic_subscription:managed")!;
    expect(describePosture(freshRow.posture)).toEqual({ text: "Captured 3d ago", tone: "muted" });

    const aging = baseStatus({ harness: [{ provider: "anthropic", captured: true, aging: true }] });
    const agingRow = deriveIntegrations(aging, null, []).ai.find((r) => r.id === "ai:anthropic_subscription:managed")!;
    expect(describePosture(agingRow.posture)).toEqual({ text: "Reconnect soon", tone: "warning" });
  });

  it("Bedrock: region/model unset wins over an active lane", () => {
    const status = baseStatus({ bedrock: { creds_present: true } });
    const [row] = deriveIntegrations(status, null, []).ai;
    expect(describePosture(row.posture)).toEqual({ text: "Region/model unset", tone: "warning" });
  });

  it("Bedrock: precedence picks bearer over static keys, and residency/secrets follow the active lane", () => {
    const status = baseStatus({ bedrock: { region: "us-east-1", model: "anthropic.claude-3", bearer_present: true, creds_present: true } });
    const [row] = deriveIntegrations(status, null, []).ai;
    expect(row.bedrockLane).toBe("bearer");
    expect(row.residency).toBe("proxy_injected");
    expect(row.secretNames).toEqual(["bedrock-api-key"]);
  });

  it("Bedrock: an unexpired AWS SSO session reads 'Session expires HH:MM'", () => {
    const status = baseStatus({
      bedrock: { region: "us-east-1", model: "anthropic.claude-3", creds_present: false },
      harness: [{ provider: "aws", captured: true, expires_at: "2026-01-01T14:20:00Z", expired: false }],
    });
    const [row] = deriveIntegrations(status, null, []).ai;
    expect(row.bedrockLane).toBe("sso");
    expect(row.harnessProvider).toBe("aws");
    expect(describePosture(row.posture).text).toMatch(/^Session expires \d{1,2}:\d{2}/);
  });

  it("azure_openai derives from a composer backend even with no conventional secret name", () => {
    const status = baseStatus({ composer: { enabled: true, backends: [{ name: "corp-azure", provider: "azure", model: "gpt-4o", wire: "api", enabled: true, needs_key: true, key_resolved: true, key_secret: "corp-azure-key" }] } });
    const [row] = deriveIntegrations(status, null, []).ai;
    expect(row.name).toBe("corp-azure");
    expect(row.residency).toBe("control_plane");
    expect(row.secretNames).toEqual(["corp-azure-key"]);
    // Azure can drive neither agent tool — both collapse into ONE fact chip.
    expect(row.chips.some((c) => c.muted && c.label === "Claude Code · Codex CLI · n/a")).toBe(true);
  });
});

describe("deriveIntegrations — SCM hosts", () => {
  // SCM-SEAM-2: this used to drop the row entirely — an add that registered
  // the host into scm_hosts (widening every future run's egress allowlist)
  // but skipped the optional credential read as a silent no-op, with no row
  // anywhere to reveal, inspect, or delete what was actually just widened.
  it("a registered host with no stored credential still renders — egress only, no chips, no secret", () => {
    const data = deriveIntegrations(baseStatus(), { scm_hosts: ["gitlab.com"] }, []);
    expect(data.scm).toHaveLength(1);
    const [row] = data.scm;
    expect(row.typeLabel).toBe("gitlab.com");
    expect(row.residency).toBe("notbuilt");
    expect(row.chips).toEqual([]);
    expect(row.secretNames).toEqual([]);
    // No server-side integration entity to adopt for a credential-less row.
    expect(row.serverId).toBeUndefined();
  });

  it("an orphan secret's derivedFrom guess (never registered) is unaffected — it always had a real lane", () => {
    // git-pat-unknown-host matches no scmHosts entry, so deriveProviders
    // reconstructs a guessed host from the secret name itself — that row has
    // a real PAT lane from the start, never the zero-lane branch.
    const data = deriveIntegrations(baseStatus(), null, ["git-pat-unknown-host"]);
    expect(data.scm).toHaveLength(1);
    expect(data.scm[0].secretNames).toEqual(["git-pat-unknown-host"]);
    expect(data.scm[0].residency).not.toBe("notbuilt");
  });

  it("a github-pat secret yields a resident_env row, not the live-check row", () => {
    const data = deriveIntegrations(baseStatus(), { scm_hosts: ["github.com"] }, ["git-pat-github-com"]);
    const [row] = data.scm;
    expect(row.residency).toBe("resident_env");
    expect(row.isGithubApp).toBeFalsy();
    expect(row.canReCheck).toBeFalsy();
  });

  it("the GitHub App is Unknown until a later wave wires the real ref-confinement check, but Re-check is real", () => {
    const status = baseStatus({ secrets: { present: [], github_app: true } });
    const [row] = deriveIntegrations(status, { scm_hosts: ["github.com"] }, []).scm;
    expect(row.isGithubApp).toBe(true);
    expect(row.canReCheck).toBe(true);
    expect(describePosture(row.posture)).toEqual({ text: "Unknown · checked not yet", tone: "muted" });
  });
});

// Corporate network is the single home for a proxy and for egress redirects
// now, so neither is derived as an integration row any more — an integration is
// an account with an outside system; these are network topology, and a redirect
// carries a proof obligation only that step's gate can enforce. Pinned as an
// ABSENCE so re-adding a category can't happen by accident.
describe("deriveIntegrations — network topology is not an integration", () => {
  it("derives no rows at all from a proxy or from egress redirects, in either shape", () => {
    const siteConfig: SiteConfig = {
      upstream_proxy_url: "http://proxy.corp.acme.com:8080",
      upstream_proxy_secret_ref: "upstream-proxy-url",
      egress_redirects: [
        { from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote", ecosystem: "npm" },
      ],
    };
    const data = deriveIntegrations(baseStatus(), siteConfig, []);
    expect([...data.ai, ...data.scm]).toHaveLength(0);
    expect(Object.keys(data).sort()).toEqual(["ai", "scm"]);
  });

  it("the legacy artifact_overrides map derives nothing either", () => {
    const siteConfig: SiteConfig = {
      artifact_overrides: { npm: { base_url: "https://artifactory.corp.internal/api/npm/npm-remote" } },
    };
    const data = deriveIntegrations(baseStatus(), siteConfig, []);
    expect([...data.ai, ...data.scm]).toHaveLength(0);
  });
});

// The banner survived the consolidation — it's detection, not configuration,
// and it points at Corporate network (T.PROXY_BANNER).
describe("proxyBannerNeeded", () => {
  it("fires only when a proxy was detected AND nothing is connected yet", () => {
    const detected: SetupStatus = baseStatus({ host_proxy: { has_credentials: false, http_proxy: { value: "proxy.corp:8080", source: "env", has_credentials: false } } });
    expect(proxyBannerNeeded(detected, null)).toBe(true);
    expect(proxyBannerNeeded(detected, { upstream_proxy_secret_ref: "x" })).toBe(false);
    // A plain-URL proxy counts as connected too, not just a secret ref.
    expect(proxyBannerNeeded(detected, { upstream_proxy_url: "http://proxy.corp.acme.com:8080" })).toBe(false);
    expect(proxyBannerNeeded(baseStatus(), null)).toBe(false);
  });
});

describe("blastRadius", () => {
  it("names the held defaults and leaves the secret un-deleted for a generic AI credential", () => {
    const row: IntegrationRow = {
      id: "ai:anthropic_api_key",
      category: "ai_provider",
      name: "Anthropic (API key)",
      typeLabel: "anthropic · api key",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: ["anthropic-api-key"],
      checkIds: [],
    };
    const lines = blastRadius(row, { isDefaultAgent: true, isDefaultFeatures: true });
    expect(lines[0]).toBe("Agent runs that resolve the server default lose model access — their first model call fails.");
    expect(lines.some((l) => l.includes("Composer loses its backend"))).toBe(true);
    expect(lines.at(-1)).toBe("The stored secret anthropic-api-key is not deleted — remove it under Secrets.");
  });

  it("a harness-backed subscription says disconnecting IS the removal, not 'not deleted'", () => {
    const row: IntegrationRow = {
      id: "ai:anthropic_subscription:managed",
      category: "ai_provider",
      name: "Claude subscription (managed)",
      typeLabel: "anthropic · managed login",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: [],
      harnessProvider: "anthropic",
      checkIds: [],
    };
    const lines = blastRadius(row);
    expect(lines.some((l) => /disconnecting IS the removal/.test(l))).toBe(true);
    expect(lines.some((l) => /not deleted — remove it under Secrets/.test(l))).toBe(false);
  });
});
