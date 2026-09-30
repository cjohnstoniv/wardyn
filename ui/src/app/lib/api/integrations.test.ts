/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { baseStatus } from "../../lib/test-fixtures";
import { T } from "../integrations";
import { aiServerId, deriveIntegrations } from "./integrations";
import type { SiteConfig } from "../types";

// The Add dialog resolves which wire row to adopt/PUT via this helper,
// BEFORE its first reload can hand it a derived IntegrationRow of its own —
// pinned here as the single source of truth deriveAiRows' own serverId
// values are drawn from, so the two can't drift apart.
describe("aiServerId", () => {
  it("matches deriveAiRows' own serverId for every type that has one", () => {
    expect(aiServerId("anthropic_api_key")).toBe("anthropic_api_key");
    expect(aiServerId("anthropic_subscription", true)).toBe("anthropic_subscription:resident_host");
    expect(aiServerId("anthropic_subscription", false)).toBe("anthropic_subscription:managed");
    expect(aiServerId("bedrock")).toBe("bedrock");
    expect(aiServerId("openai_api_key")).toBe("openai_api_key");
  });

  // Every AiType now maps to a server id. azure_openai has neither a
  // site-config field nor a SetupCheck id — it was removed in 0.5.
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

  // Azure OpenAI is gone as a model provider. It only ever powered Wardyn's own
  // features (the AI Run Composer, deleted), and harness.go is explicit that
  // neither agent tool can be pointed at an Azure deployment — so a row for it
  // was a connected-looking credential wired to nothing.
  it("azure_openai no longer derives a row, even with its conventional secret stored", () => {
    const rows = deriveIntegrations(baseStatus(), null, ["azure-openai-key"]).ai;
    expect(rows).toHaveLength(0);
  });

  // The stored secret is NOT deleted by this — an operator's existing key stays
  // in the secret store, inert, and is still visible/removable on /secrets.
  it("an azure key alongside a real provider leaves the real one untouched", () => {
    const rows = deriveIntegrations(baseStatus(), null, ["azure-openai-key", "anthropic-api-key"]).ai;
    expect(rows.map((r) => r.aiType)).toEqual(["anthropic_api_key"]);
  });
});

describe("deriveIntegrations — SCM hosts", () => {
  // Without this row, an add that registers the host into scm_hosts
  // (widening every future run's egress allowlist) but has no credential to
  // read would be a silent no-op, with no row anywhere to reveal, inspect,
  // or delete what was actually just widened.
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

  it("a github-pat secret yields a proxy_injected row (#381 default) when the switch is unknown or on", () => {
    const data = deriveIntegrations(baseStatus(), { scm_hosts: ["github.com"] }, ["git-pat-github-com"]);
    const [row] = data.scm;
    // patLaneMeta's ON shape (scm-provider.ts): since 0.7 WARDYN_GIT_PAT_BROKER
    // defaults on, so a stored PAT is attached by the proxy, not resident in
    // the sandbox — siteConfig here carries no workspace_providers block (a
    // member caller, or an operator whose GET hasn't loaded it yet), so this
    // shows the real 0.7.10 default rather than guessing the pre-0.7 one.
    expect(row.residency).toBe("proxy_injected");
    expect(row.isGithubApp).toBeFalsy();
    expect(row.canReCheck).toBeFalsy();
  });

  it("a github-pat secret yields a resident_env row when siteConfig reports the broker OFF (#381)", () => {
    // ticket: F8
    const data = deriveIntegrations(
      baseStatus(),
      { scm_hosts: ["github.com"], workspace_providers: { git_pat_broker_enabled: false } },
      ["git-pat-github-com"],
    );
    const [row] = data.scm;
    expect(row.residency).toBe("resident_env");
    expect(row.chips.find((c) => c.label === "PAT · in-sandbox")).toBeTruthy();
  });

  it("the GitHub App is Unknown until a later wave wires the real ref-confinement check, but Re-check is real", () => {
    const status = baseStatus({ secrets: { present: [], github_app: true } });
    const [row] = deriveIntegrations(status, { scm_hosts: ["github.com"] }, []).scm;
    expect(row.isGithubApp).toBe(true);
    expect(row.canReCheck).toBe(true);
    expect(row.posture).toEqual({ kind: "gh_verdict", verdict: "unknown", checkedLabel: "not yet" });
  });

  // effective_scm_hosts is the server's projected union — a host a
  // workspace-provider row CLAIMS (enabled or disabled) is removed from it
  // even though the legacy scm_hosts list still names it, so a disabled
  // provider's host must not keep reading "Connected" here.
  it("a host scm_hosts lists but effective_scm_hosts omits renders no row for it", () => {
    // No stored credential: with the host missing from effective_scm_hosts,
    // deriveProviders never buckets it — unlike a git-pat-github-com secret,
    // which would fall into the orphan-guess path regardless of the host
    // list and is exercised separately above.
    const data = deriveIntegrations(baseStatus(), { scm_hosts: ["github.com"], effective_scm_hosts: [] }, []);
    expect(data.scm).toHaveLength(0);
  });

  it("an older daemon with no effective_scm_hosts falls back to scm_hosts unchanged", () => {
    const data = deriveIntegrations(baseStatus(), { scm_hosts: ["github.com"] }, ["git-pat-github-com"]);
    expect(data.scm).toHaveLength(1);
    expect(data.scm[0].typeLabel).toBe("github.com");
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

// proxyBannerNeeded, blastRadius, describePosture and the whole
// genericIntegrations/groupForKind suite were pinned here. Each of those
// symbols existed for the deleted /integrations catalog page and went with it;
// generic integration kinds are no longer a shape Wardyn accepts. What this
// file still covers — AI providers, SCM hosts, and the ABSENCE pin above for
// network topology — is the half lib/readiness.ts and
// settings/connection-cards.tsx read.
