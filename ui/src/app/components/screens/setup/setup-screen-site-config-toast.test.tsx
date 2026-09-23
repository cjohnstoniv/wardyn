/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F6-F6 (Appendix A V8): SetupScreen's saveSiteConfig used to discard
// putSiteConfig's four advisory signals outright — an admin saving a proxy
// config naming a missing secret was told it saved cleanly. Kept out of
// setup-screen.test.tsx, which sits at the 1000-line file-size cap.
//
// #492: also covers saveSiteConfig's 412 handling — a stale write must
// reload the document (and its ETag) rather than silently spread the old GET
// over whatever changed it, mirroring sign-in-help-card.test.tsx's own case
// for the People step's sign-in-help card.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { HostProxyDetection, SetupStatus } from "../../../lib/types";
import { HttpError } from "../../../lib/api/core";
import { SITE } from "../../wardyn/copy";

const toastWarning = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: { warning: (...a: unknown[]) => toastWarning(...a), success: vi.fn(), error: (...a: unknown[]) => toastError(...a) },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a), completeOnboarding: vi.fn() },
}));
const listSecretsMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a), setSecret: vi.fn() },
}));
const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
const testProxyMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    health: () => Promise.resolve({ confinement_classes: ["CC1", "CC2"] }),
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    // #492: setup-screen.tsx's reloadSiteConfig now reads the ETag-carrying
    // snapshot — routed through the SAME mock these tests already drive.
    getSiteConfigSnapshot: async (...a: unknown[]) => ({ siteConfig: await getSiteConfigMock(...a), etag: null }),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
    testProxy: (...a: unknown[]) => testProxyMock(...a),
    testRedirect: vi.fn(),
  },
}));
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a), createWorkspace: vi.fn() },
}));
vi.mock("../../../lib/api/policies", () => ({ policies: { listPolicies: () => Promise.resolve([]), createPolicy: vi.fn() } }));
const getDrivesMock = vi.fn();
vi.mock("../../../lib/api/drives", () => ({ drives: { getDrives: (...a: unknown[]) => getDrivesMock(...a) } }));
const getWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", () => ({
  providers: { getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a) },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { createRun: vi.fn(), getRun: vi.fn(), killRun: vi.fn() } }));
vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
vi.mock("../../wardyn/live-approvals", () => ({ LiveApprovals: () => null }));

import { SetupScreen } from "./setup-screen";
import { baseStatus as sharedBaseStatus } from "../../../lib/test-fixtures";

function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return sharedBaseStatus({
    checks: [{ id: "gvisor", label: "gVisor runtime", status: "ok", detail: "runsc detected" }],
    ...overrides,
  });
}

function renderScreen() {
  return render(
    <MemoryRouter initialEntries={["/setup"]}>
      <SetupScreen onDone={() => {}} />
    </MemoryRouter>,
  );
}

const DETECTION: HostProxyDetection = {
  has_credentials: false,
  http_proxy: { value: "http://proxy.corp.acme.com:8080", source: "env", has_credentials: false },
};

beforeEach(() => {
  toastWarning.mockClear();
  toastError.mockClear();
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus({ host_proxy: DETECTION }));
  listSecretsMock.mockReset().mockResolvedValue([]);
  getSiteConfigMock.mockReset().mockResolvedValue({});
  putSiteConfigMock.mockReset();
  testProxyMock.mockReset().mockResolvedValue({ state: "no_runner", detail: "nothing to launch a probe with" });
  listWorkspacesMock.mockReset().mockResolvedValue([]);
  getDrivesMock.mockReset().mockResolvedValue({ drives: [], grants: [], host_roots_configured: false, runner_target: "docker" });
  getWorkspaceProvidersMock.mockReset().mockResolvedValue({ providers: {}, etag: null });
});

async function saveDetectedProxy(user: ReturnType<typeof userEvent.setup>) {
  renderScreen();
  await screen.findByText("Fence");
  await user.click(screen.getByRole("button", { name: /^next:/i })); // -> people
  await screen.findAllByText("Single-user");
  await user.click(screen.getByRole("button", { name: /^next:/i })); // -> corp_network
  await screen.findByRole("heading", { name: /^network$/i });
  await user.click(screen.getByRole("button", { name: /use detected proxy/i }));
}

describe("SetupScreen — the Corporate-network save toast (F6-F6)", () => {
  it("warns on dangling_secret_refs, as an ADDITIONAL toast beside the step's own success toast", async () => {
    putSiteConfigMock.mockResolvedValue({
      siteConfig: { upstream_proxy_url: "http://proxy.corp.acme.com:8080" },
      danglingSecretRefs: ["upstream-proxy-secret"],
      onboardingCompletedAtIgnored: false,
      appliesFrom: "next_dispatch",
      sourcesNoLongerAdmitted: null,
    });
    const user = userEvent.setup();
    await saveDetectedProxy(user);

    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalled());
    await waitFor(() => expect(toastWarning).toHaveBeenCalledWith(expect.stringContaining("upstream-proxy-secret")));
  });

  // Negative control: a clean save (no dangling refs) never fires the F6-F6
  // warning — the step's own plain success toast (a different module, never
  // mocked here) is untouched by this fix.
  it("a clean save never fires the dangling-refs warning", async () => {
    putSiteConfigMock.mockResolvedValue({
      siteConfig: { upstream_proxy_url: "http://proxy.corp.acme.com:8080" },
      danglingSecretRefs: [],
      onboardingCompletedAtIgnored: false,
      appliesFrom: "next_dispatch",
      sourcesNoLongerAdmitted: null,
    });
    const user = userEvent.setup();
    await saveDetectedProxy(user);

    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalled());
    expect(toastWarning).not.toHaveBeenCalled();
  });
});

describe("SetupScreen — the Corporate-network save path's If-Match (#492)", () => {
  it("on a 412, reloads the site config (fresh ETag for a retry) and tells the operator, instead of silently accepting a stale write", async () => {
    putSiteConfigMock.mockRejectedValueOnce(
      new HttpError(412, "If-Match does not match the current site config — GET /site-config again and retry"),
    );
    const user = userEvent.setup();
    await saveDetectedProxy(user);

    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalled());
    // The bug itself: this save now sends the mount read's ETag as a SECOND
    // argument (If-Match) — before this fix, putSiteConfig was called with
    // the document alone, and a stale write went through unrefused.
    expect(putSiteConfigMock).toHaveBeenCalledWith(expect.anything(), null);
    // The orchestrator's own mount read (1), CorpNetworkStep's own mount read
    // via useSiteConfigStep (2), plus the reload the 412 path forces (3) — a
    // fresh document, and a fresh ETag, so a retry has something to succeed
    // against.
    await waitFor(() => expect(getSiteConfigMock).toHaveBeenCalledTimes(3));
    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith(expect.any(String), expect.objectContaining({ description: SITE.SAVED_ELSEWHERE })),
    );
    // Never the F6-F6 advisory toasts — a 412 never reaches the code that
    // reads putSiteConfig's result.
    expect(toastWarning).not.toHaveBeenCalled();
  });
});
