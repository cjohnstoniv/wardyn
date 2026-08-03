/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { baseStatus } from "../../components/screens/setup/test-fixtures";
import { T } from "../integrations";
import {
  deriveIntegrations,
  proxyBannerNeeded,
  describePosture,
  blastRadius,
  type IntegrationRow,
} from "./integrations";
import type { SetupStatus, SiteConfig } from "../types";

describe("deriveIntegrations — empty inputs", () => {
  it("derives nothing when nothing is configured", () => {
    const data = deriveIntegrations(baseStatus(), null, []);
    expect(data.ai).toHaveLength(0);
    expect(data.scm).toHaveLength(0);
    expect(data.mirror).toHaveLength(0);
    expect(data.proxy).toHaveLength(0);
  });
});

describe("deriveIntegrations — AI providers", () => {
  it("anthropic-api-key -> a row whose Codex chip is the muted, verbatim-reason fact and Claude Code/Wardyn features carry '· default'", () => {
    const status = baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } });
    const [row] = deriveIntegrations(status, null, ["anthropic-api-key"]).ai;
    expect(row.typeLabel).toBe("anthropic · api key");
    expect(row.residency).toBe("proxy_injected");
    const codex = row.chips.find((c) => c.label === "Codex CLI")!;
    expect(codex.muted).toBe(true);
    expect(codex.tooltip).toBe(T.X_KEY_CODEX);
    expect(row.chips.find((c) => c.label === "Claude Code · default")).toBeTruthy();
    expect(row.chips.find((c) => c.label === "Wardyn features · default")).toBeTruthy();
    expect(row.secretNames).toEqual(["anthropic-api-key"]);
  });

  it("a resident host-CLI subscription omits the OFF Wardyn-features chip entirely", () => {
    const status = baseStatus({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }] });
    const [row] = deriveIntegrations(status, null, []).ai;
    expect(row.hostCli).toBe(true);
    expect(row.chips.map((c) => c.label)).toEqual(["Claude Code", "Codex CLI", "Direct API calls"]);
    expect(row.residency).toBe("resident_mount");
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
    expect(row.chips.some((c) => c.muted && c.label === "Claude Code · Codex CLI")).toBe(true);
  });
});

describe("deriveIntegrations — SCM hosts", () => {
  it("a registered host with no stored credential is not an integration yet", () => {
    const data = deriveIntegrations(baseStatus(), { scm_hosts: ["gitlab.com"] }, []);
    expect(data.scm).toHaveLength(0);
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

describe("deriveIntegrations — artifact mirrors (legacy artifact_overrides shape)", () => {
  it("groups ecosystems on the same mirror host into one row", () => {
    const siteConfig: SiteConfig = {
      artifact_overrides: {
        npm: { base_url: "https://artifactory.corp.internal/api/npm/npm-remote" },
        pip: { base_url: "https://artifactory.corp.internal/api/pip/pip-remote", token_secret_ref: "artifactory-token" },
      },
    };
    const [row] = deriveIntegrations(baseStatus(), siteConfig, []).mirror;
    expect(row.redirect).toBeUndefined();
    expect(row.name).toBe("artifactory.corp.internal");
    expect(row.chips.map((c) => c.label).sort()).toEqual(["powers npm (mirror)", "powers pip (mirror)"]);
    expect(row.secretNames).toEqual(["artifactory-token"]);
  });
});

// BUG FIX (this wave): deriveMirrorRows used to read ONLY artifact_overrides,
// so a redirect saved through the Corporate network step (which writes
// egress_redirects) never appeared on /integrations at all.
describe("deriveIntegrations — egress redirects (current SiteConfig.egress_redirects shape)", () => {
  it("a redirect saved via egress_redirects now appears — one row per entry, never grouped by destination host", () => {
    const siteConfig: SiteConfig = {
      egress_redirects: [
        {
          from: "https://registry.npmjs.org",
          to: "https://artifactory.corp.internal/api/npm/npm-remote",
          token_secret_ref: "artifactory-token",
          ecosystem: "npm",
        },
        {
          from: "https://pypi.org/simple",
          to: "https://artifactory.corp.internal/api/pypi/pypi-remote/simple",
          token_secret_ref: "artifactory-token",
          ecosystem: "pip",
        },
      ],
    };
    const { mirror } = deriveIntegrations(baseStatus(), siteConfig, []);
    // Same destination host, but the current shape never groups (unlike the
    // legacy artifact_overrides path above) — two redirects, two rows.
    expect(mirror).toHaveLength(2);
    expect(mirror[0].redirect).toEqual(siteConfig.egress_redirects![0]);
    expect(mirror[0].secretNames).toEqual(["artifactory-token"]);
    expect(mirror[1].redirect?.from).toBe("https://pypi.org/simple");
    expect(mirror[0].id).not.toBe(mirror[1].id);
  });

  it("ecosystem left unset (network-only) is preserved verbatim, not defaulted to an empty string or dropped", () => {
    const siteConfig: SiteConfig = { egress_redirects: [{ from: "telemetry.vendor-sdk.io", to: "10.40.2.11:8443" }] };
    const [row] = deriveIntegrations(baseStatus(), siteConfig, []).mirror;
    expect(row.redirect?.ecosystem).toBeUndefined();
    expect(row.secretNames).toEqual([]);
  });

  it("prefers egress_redirects over artifact_overrides when a deployment somehow carries both", () => {
    const siteConfig: SiteConfig = {
      artifact_overrides: { npm: { base_url: "https://artifactory.corp.internal/api/npm/npm-remote" } },
      egress_redirects: [{ from: "https://ghcr.io", to: "https://registry.corp.internal/ghcr-remote" }],
    };
    const { mirror } = deriveIntegrations(baseStatus(), siteConfig, []);
    expect(mirror).toHaveLength(1);
    expect(mirror[0].redirect?.from).toBe("https://ghcr.io");
  });

  it("falls back to the legacy grouped rendering when egress_redirects is absent — an unmigrated deployment keeps rendering", () => {
    const siteConfig: SiteConfig = {
      artifact_overrides: { npm: { base_url: "https://artifactory.corp.internal/api/npm/npm-remote" } },
    };
    const [row] = deriveIntegrations(baseStatus(), siteConfig, []).mirror;
    expect(row.redirect).toBeUndefined();
    expect(row.name).toBe("artifactory.corp.internal");
  });
});

describe("deriveIntegrations / proxyBannerNeeded — host proxy", () => {
  it("derives a row only once a secret ref is registered", () => {
    expect(deriveIntegrations(baseStatus(), null, []).proxy).toHaveLength(0);
    const [row] = deriveIntegrations(baseStatus(), { upstream_proxy_secret_ref: "upstream-proxy-url" }, []).proxy;
    expect(row.secretNames).toEqual(["upstream-proxy-url"]);
    expect(row.proxyUrl).toBeUndefined();
  });

  // BUG FIX (this wave): deriveProxyRows used to check ONLY
  // upstream_proxy_secret_ref, so a proxy saved through the Corporate network
  // step's default path (a plain URL) rendered the category as permanently
  // empty even though it really was configured.
  it("also derives a row from a plain upstream_proxy_url, with no secret involved", () => {
    const [row] = deriveIntegrations(baseStatus(), { upstream_proxy_url: "http://proxy.corp.acme.com:8080" }, []).proxy;
    expect(row.proxyUrl).toBe("http://proxy.corp.acme.com:8080");
    expect(row.secretNames).toEqual([]);
  });

  it("banner fires only when a proxy was detected AND nothing is connected yet", () => {
    const detected: SetupStatus = baseStatus({ host_proxy: { has_credentials: false, http_proxy: { value: "proxy.corp:8080", source: "env", has_credentials: false } } });
    expect(proxyBannerNeeded(detected, null)).toBe(true);
    expect(proxyBannerNeeded(detected, { upstream_proxy_secret_ref: "x" })).toBe(false);
    // Same fix as deriveProxyRows: a plain-URL proxy also counts as connected.
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

  it("names the specific redirect for a current-shape (egress_redirects) artifact_mirror row", () => {
    const row: IntegrationRow = {
      id: "mirror:0",
      category: "artifact_mirror",
      name: "artifactory.corp.internal",
      typeLabel: "https://registry.npmjs.org → https://artifactory.corp.internal/api/npm/npm-remote",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: [],
      checkIds: [],
      redirect: { from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" },
    };
    expect(blastRadius(row)).toEqual(["Runs reach https://registry.npmjs.org directly again — this redirect stops applying."]);
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
