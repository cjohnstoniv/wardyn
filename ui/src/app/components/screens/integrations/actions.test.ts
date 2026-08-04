/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// deleteIntegration's site-config branches (egress_redirects removal by index,
// legacy artifact_overrides removal by host, clearing the two mutually-exclusive
// proxy fields) went with the categories that needed them — Corporate network
// owns a proxy and a redirect now, including their removal. What's left is one
// rule for every remaining row, and these tests pin BOTH halves of it plus the
// absence of any site-config write.
import { describe, it, expect, vi, beforeEach } from "vitest";
import type { IntegrationRow } from "../../../lib/api/integrations";

const putSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: { putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a) },
}));

const deleteSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { deleteSecret: (...a: unknown[]) => deleteSecretMock(...a) },
}));

const harnessDisconnectMock = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: { harnessDisconnect: (...a: unknown[]) => harnessDisconnectMock(...a) },
}));

import { canRotateInline, deleteIntegration, primarySecretName } from "./actions";

function scmRow(overrides: Partial<IntegrationRow> = {}): IntegrationRow {
  return {
    id: "scm:github.com",
    category: "scm_host",
    name: "GitHub",
    typeLabel: "github.com",
    chips: [],
    residency: "resident_env",
    posture: { kind: "configured" },
    secretNames: ["git-pat-github-com"],
    checkIds: [],
    ...overrides,
  };
}

describe("deleteIntegration", () => {
  beforeEach(() => {
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
    harnessDisconnectMock.mockReset().mockResolvedValue(undefined);
  });

  it("deletes every backing secret, and never touches the site config", async () => {
    await deleteIntegration(scmRow({ secretNames: ["github-app-id", "github-app-key"] }));

    expect(deleteSecretMock.mock.calls.flat()).toEqual(["github-app-id", "github-app-key"]);
    expect(putSiteConfigMock).not.toHaveBeenCalled();
  });

  it("a harness login is disconnected instead — its session is not a secret-store entry", async () => {
    await deleteIntegration(
      scmRow({ id: "ai:anthropic_subscription:managed", category: "ai_provider", harnessProvider: "anthropic", secretNames: [] }),
    );

    expect(harnessDisconnectMock).toHaveBeenCalledWith("anthropic");
    expect(deleteSecretMock).not.toHaveBeenCalled();
  });

  it("one rejected secret delete surfaces, it isn't swallowed by the others", async () => {
    deleteSecretMock.mockResolvedValueOnce(undefined).mockRejectedValueOnce(new Error("403 operator role required"));

    await expect(deleteIntegration(scmRow({ secretNames: ["a", "b"] }))).rejects.toThrow(/operator role/);
  });
});

describe("canRotateInline / primarySecretName", () => {
  it("a row with no stored secret cannot be rotated inline", () => {
    expect(canRotateInline(scmRow({ secretNames: [] }))).toBe(false);
  });

  it("a row WITH a secret can be, targeting the first one — except the GitHub App, which targets the PEM", () => {
    expect(canRotateInline(scmRow())).toBe(true);
    expect(primarySecretName(scmRow())).toBe("git-pat-github-com");
    expect(primarySecretName(scmRow({ isGithubApp: true, secretNames: ["github-app-id", "github-app-key"] }))).toBe(
      "github-app-key",
    );
  });
});
