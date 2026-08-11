/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// deleteIntegration's site-config branches (egress_redirects removal by index,
// legacy artifact_overrides removal by host, clearing the two mutually-exclusive
// proxy fields) went with the categories that needed them — Corporate network
// owns a proxy and a redirect now, including their removal. What's left is one
// rule for every remaining row: the stored secret is never deleted (UI-WS-4 —
// it may be referenced elsewhere), and an scm_host row additionally drops its
// site-config scm_hosts registration when it's actually a member (SCM-SEAM-1).
import { describe, it, expect, vi, beforeEach } from "vitest";
import type { IntegrationRow } from "../../../lib/api/integrations";
import type { SiteConfig } from "../../../lib/types";

const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
  },
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
import { HttpError } from "../../../lib/api/core";
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
    getSiteConfigMock.mockReset().mockResolvedValue({} as SiteConfig);
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
    harnessDisconnectMock.mockReset().mockResolvedValue(undefined);
    removeIntegrationMock.mockReset().mockResolvedValue(undefined);
  });

  // UI-WS-4: the confirm copy (blastRadius) has always said the stored secret
  // is NOT deleted — remove it under Secrets. deleteIntegration used to delete
  // it anyway; a secret shared with another workspace/site-config field broke
  // every other consumer the instant an operator believed the copy.
  it("never deletes the backing secret — it may be referenced elsewhere", async () => {
    await deleteIntegration(scmRow({ secretNames: ["github-app-id", "github-app-key"] }));
    expect(deleteSecretMock).not.toHaveBeenCalled();
  });

  it("an ai_provider row (no site-config concept at all) never touches the site config", async () => {
    await deleteIntegration(
      scmRow({ id: "ai:anthropic_api_key", category: "ai_provider", typeLabel: "anthropic · api key", secretNames: ["anthropic-api-key"] }),
    );
    expect(getSiteConfigMock).not.toHaveBeenCalled();
    expect(putSiteConfigMock).not.toHaveBeenCalled();
  });

  it("a harness login is disconnected instead — its session is not a secret-store entry", async () => {
    await deleteIntegration(
      scmRow({ id: "ai:anthropic_subscription:managed", category: "ai_provider", harnessProvider: "anthropic", secretNames: [] }),
    );

    expect(harnessDisconnectMock).toHaveBeenCalledWith("anthropic");
    expect(deleteSecretMock).not.toHaveBeenCalled();
    expect(putSiteConfigMock).not.toHaveBeenCalled();
  });

  // SCM-SEAM-1: the removal control that died with the retired SCM Provider
  // step. Without this, "Runs stop inheriting <host>" is false — the host
  // stays unioned into every future run's egress allowlist forever, with no
  // remaining surface that can see or revoke it.
  it("drops a registered host from site-config scm_hosts on delete", async () => {
    getSiteConfigMock.mockResolvedValue({ scm_hosts: ["github.com", "gitlab.com"] } as SiteConfig);

    await deleteIntegration(scmRow({ typeLabel: "github.com" }));

    expect(putSiteConfigMock).toHaveBeenCalledWith(
      expect.objectContaining({ scm_hosts: ["gitlab.com"] }),
    );
  });

  it("matches the host case-insensitively — the operator may have typed it in mixed case", async () => {
    getSiteConfigMock.mockResolvedValue({ scm_hosts: ["GitHub.com"] } as SiteConfig);

    await deleteIntegration(scmRow({ typeLabel: "github.com" }));

    expect(putSiteConfigMock).toHaveBeenCalledWith(expect.objectContaining({ scm_hosts: [] }));
  });

  it("a host never registered (a derivedFrom guess row) triggers no site-config write", async () => {
    getSiteConfigMock.mockResolvedValue({ scm_hosts: ["gitlab.com"] } as SiteConfig);

    await deleteIntegration(scmRow({ typeLabel: "ghes.corp.internal" }));

    expect(putSiteConfigMock).not.toHaveBeenCalled();
  });

  it("no scm_hosts at all triggers no site-config write", async () => {
    getSiteConfigMock.mockResolvedValue({} as SiteConfig);

    await deleteIntegration(scmRow());

    expect(putSiteConfigMock).not.toHaveBeenCalled();
  });

  it("a rejected site-config write surfaces, it isn't swallowed", async () => {
    getSiteConfigMock.mockResolvedValue({ scm_hosts: ["github.com"] } as SiteConfig);
    putSiteConfigMock.mockRejectedValueOnce(new Error("403 operator role required"));

    await expect(deleteIntegration(scmRow())).rejects.toThrow(/operator role/);
  });

  // The bug this closes: the UI's Delete never called DELETE /integrations for
  // a legacy row, so an adopted default-holder kept its default_for standing
  // server-side even after the operator "deleted" it — agent runs went on
  // resolving the default the blast radius promised they'd lose.
  const aiRow = () =>
    scmRow({
      id: "ai:anthropic_api_key",
      category: "ai_provider",
      serverId: "anthropic_api_key",
      typeLabel: "anthropic · api key",
      secretNames: ["anthropic-api-key"],
    });

  it("removes the adopted stored integration for a legacy row that has a serverId", async () => {
    await deleteIntegration(aiRow());
    expect(removeIntegrationMock).toHaveBeenCalledWith("anthropic_api_key");
    // Still never the secret (UI-WS-4).
    expect(deleteSecretMock).not.toHaveBeenCalled();
  });

  it("swallows the 404 a never-adopted legacy row's delete returns — nothing was stored", async () => {
    removeIntegrationMock.mockRejectedValueOnce(new HttpError(404, 'no stored integration "anthropic_api_key"'));
    await expect(deleteIntegration(aiRow())).resolves.toBeUndefined();
  });

  it("propagates any non-404 failure from the stored-integration delete", async () => {
    removeIntegrationMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    await expect(deleteIntegration(aiRow())).rejects.toThrow(/boom/);
  });

  it("a derivedFrom guess SCM row (no serverId) makes no stored-integration delete", async () => {
    await deleteIntegration(scmRow({ serverId: undefined }));
    expect(removeIntegrationMock).not.toHaveBeenCalled();
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
