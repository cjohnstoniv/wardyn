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

const adoptIntegrationMock = vi.fn();
const putIntegrationMock = vi.fn();
const removeIntegrationMock = vi.fn();
vi.mock("../../../lib/api/integrations", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/integrations")>();
  return {
    ...actual,
    integrationsApi: { ...actual.integrationsApi, adoptIntegration: (...a: unknown[]) => adoptIntegrationMock(...a) },
    genericIntegrationsApi: {
      ...actual.genericIntegrationsApi,
      put: (...a: unknown[]) => putIntegrationMock(...a),
      remove: (...a: unknown[]) => removeIntegrationMock(...a),
    },
  };
});

import { canRotateInline, deleteIntegration, primarySecretName, setDefaultFor } from "./actions";
import type { WireIntegration } from "../../../lib/types/setup";

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

// adopt-then-PUT used to have no rollback: a rejected PUT (e.g. a half-set
// Bedrock row — region set, model empty — passes the checkbox gate but
// hard-400s server-side under validateIntegrationWrite) left the just-adopted
// row permanently stored even though the operator's action never succeeded.
describe("setDefaultFor", () => {
  function legacyWire(overrides: Partial<WireIntegration> = {}): WireIntegration {
    return { id: "bedrock", category: "ai_provider", type: "bedrock", source: "legacy", default_for: [], ...overrides };
  }

  beforeEach(() => {
    adoptIntegrationMock.mockReset().mockResolvedValue(undefined);
    putIntegrationMock.mockReset();
    removeIntegrationMock.mockReset().mockResolvedValue(undefined);
  });

  it("rolls back the adopt when the PUT rejects, and surfaces the real error", async () => {
    putIntegrationMock.mockRejectedValueOnce(new Error("bedrock region and model must be set together"));

    await expect(setDefaultFor(legacyWire(), "agent_runs", true)).rejects.toThrow(/region and model/);

    expect(adoptIntegrationMock).toHaveBeenCalledWith("bedrock");
    // The failed write leaves nothing stored — the just-adopted row is removed again.
    expect(removeIntegrationMock).toHaveBeenCalledWith("bedrock");
  });

  it("an already-stored row is left alone on a rejected PUT — nothing was adopted, so there's nothing to roll back", async () => {
    putIntegrationMock.mockRejectedValueOnce(new Error("500"));

    await expect(setDefaultFor(legacyWire({ source: "stored", default_for: ["agent_runs"] }), "agent_runs", false)).rejects.toThrow(
      "500",
    );

    expect(adoptIntegrationMock).not.toHaveBeenCalled();
    expect(removeIntegrationMock).not.toHaveBeenCalled();
  });

  it("a successful PUT never rolls back the adopt", async () => {
    putIntegrationMock.mockResolvedValue(undefined);

    await setDefaultFor(legacyWire(), "agent_runs", true);

    expect(adoptIntegrationMock).toHaveBeenCalledWith("bedrock");
    expect(removeIntegrationMock).not.toHaveBeenCalled();
  });
});
