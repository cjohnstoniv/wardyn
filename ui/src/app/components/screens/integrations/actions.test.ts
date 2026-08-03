/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// actions.ts had no dedicated test file before this wave — its deleteIntegration
// branching grew real logic this round (current-shape egress_redirects removal
// by index, legacy artifact_overrides removal by host, host_proxy now clearing
// two mutually-exclusive fields instead of one), so it earns its own checks
// rather than only being exercised indirectly through the screen's dialogs.
import { describe, it, expect, vi, beforeEach } from "vitest";
import type { IntegrationRow } from "../../../lib/api/integrations";
import type { SiteConfig } from "../../../lib/types";

const putSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: { putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a) },
}));

vi.mock("../../../lib/api/secrets", () => ({
  secrets: { deleteSecret: vi.fn() },
}));

vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: { harnessDisconnect: vi.fn() },
}));

import { canRotateInline, deleteIntegration, primarySecretName } from "./actions";

function mirrorRow(overrides: Partial<IntegrationRow> = {}): IntegrationRow {
  return {
    id: "mirror:artifactory.corp.internal",
    category: "artifact_mirror",
    name: "artifactory.corp.internal",
    typeLabel: "npm → artifactory.corp.internal",
    chips: [],
    residency: "proxy_injected",
    posture: { kind: "configured" },
    secretNames: [],
    checkIds: [],
    ...overrides,
  };
}

describe("deleteIntegration — artifact_mirror", () => {
  beforeEach(() => putSiteConfigMock.mockReset().mockResolvedValue(undefined));

  it("current shape: removes exactly the redirect at row.id's index from egress_redirects, leaving the rest", async () => {
    const siteConfig: SiteConfig = {
      egress_redirects: [
        { from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" },
        { from: "https://pypi.org/simple", to: "https://artifactory.corp.internal/api/pypi/pypi-remote/simple" },
      ],
    };
    const row = mirrorRow({ id: "mirror:0", redirect: siteConfig.egress_redirects![0] });

    await deleteIntegration(row, siteConfig);

    expect(putSiteConfigMock).toHaveBeenCalledWith({
      ...siteConfig,
      egress_redirects: [siteConfig.egress_redirects![1]],
    });
  });

  it("legacy shape (no row.redirect): drops every ecosystem override pointed at the row's host, keeps other hosts", async () => {
    const siteConfig: SiteConfig = {
      artifact_overrides: {
        npm: { base_url: "https://artifactory.corp.internal/api/npm/npm-remote" },
        pip: { base_url: "https://artifactory.corp.internal/api/pip/pip-remote" },
        go: { base_url: "https://other.corp.internal/api/go/go-remote" },
      },
    };
    const row = mirrorRow();

    await deleteIntegration(row, siteConfig);

    expect(putSiteConfigMock).toHaveBeenCalledWith({
      artifact_overrides: { go: siteConfig.artifact_overrides!.go },
    });
  });
});

describe("deleteIntegration — host_proxy", () => {
  beforeEach(() => putSiteConfigMock.mockReset().mockResolvedValue(undefined));

  // BUG FIX (this wave, alongside the derivation fix): the old code cleared
  // only upstream_proxy_secret_ref, so deleting a plain-URL proxy row left
  // upstream_proxy_url in place — the row would silently come right back.
  it("clears BOTH upstream_proxy_url and upstream_proxy_secret_ref, whichever was actually set", async () => {
    const row: IntegrationRow = {
      id: "proxy:host",
      category: "host_proxy",
      name: "Host proxy",
      typeLabel: "corporate proxy",
      chips: [],
      residency: "proxy_injected",
      posture: { kind: "configured" },
      secretNames: [],
      checkIds: [],
      proxyUrl: "http://proxy.corp.acme.com:8080",
    };
    const siteConfig: SiteConfig = { upstream_proxy_url: "http://proxy.corp.acme.com:8080", scm_hosts: ["github.com"] };

    await deleteIntegration(row, siteConfig);

    expect(putSiteConfigMock).toHaveBeenCalledWith({
      scm_hosts: ["github.com"],
      upstream_proxy_url: undefined,
      upstream_proxy_secret_ref: undefined,
    });
  });
});

describe("canRotateInline / primarySecretName — an egress redirect row", () => {
  it("a network-only redirect (no token) cannot be rotated inline", () => {
    expect(canRotateInline(mirrorRow())).toBe(false);
  });

  it("a redirect WITH a token can be rotated inline, targeting that token secret", () => {
    const row = mirrorRow({ secretNames: ["artifactory-token"] });
    expect(canRotateInline(row)).toBe(true);
    expect(primarySecretName(row)).toBe("artifactory-token");
  });
});
