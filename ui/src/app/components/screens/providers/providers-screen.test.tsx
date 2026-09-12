/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /providers screen — the drives-screen.tsx precedent: forbidden (a 403 is
// the TIER, not the network), fetch-failed (distinct from empty), the legacy
// banner (zero rows), populated, and exactly ONE teal button at a time.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { baseStatus } from "../../../lib/test-fixtures";
import { ProvidersScreen } from "./providers-screen";

const getWorkspaceProvidersMock = vi.fn();
const putWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/providers")>("../../../lib/api/providers");
  return {
    ...actual,
    providers: {
      ...actual.providers,
      getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a),
      putWorkspaceProviders: (...a: unknown[]) => putWorkspaceProvidersMock(...a),
    },
  };
});

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return { ...actual, drives: { ...actual.drives, getDrives: () => Promise.resolve({ drives: [], grants: [], host_roots_configured: false, runner_target: "" }) } };
});

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => vi.fn() };
});

function renderScreen(operator = true) {
  return render(
    <OperatorProvider operator={operator}>
      <ProvidersScreen />
    </OperatorProvider>,
  );
}

beforeEach(() => {
  getWorkspaceProvidersMock.mockReset();
  putWorkspaceProvidersMock.mockReset();
  getSetupStatusMock.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
});

describe("ProvidersScreen", () => {
  it("a 403 renders the tier refusal, not a fetch-failed banner", async () => {
    getWorkspaceProvidersMock.mockRejectedValue(new HttpError(403, "Requires the admin role."));
    renderScreen();
    expect(await screen.findByText(/requires the admin role/i)).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.FETCH_FAILED_TITLE)).not.toBeInTheDocument();
  });

  it("a non-403 failure renders FETCH_FAILED with Retry, never a confident empty", async () => {
    getWorkspaceProvidersMock.mockRejectedValue(new Error("boom"));
    renderScreen();
    expect(await screen.findByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.FETCH_FAILED_BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });

  it("zero rows renders the Git tab's legacy-open banner by default", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"e0"' });
    renderScreen();
    expect(await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE)).toBeInTheDocument();
  });

  it("renders the populated Git tab with rows from the wire", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e1"',
    });
    renderScreen();
    expect(await screen.findByTestId("provider-row-github")).toBeInTheDocument();
  });

  it("carries exactly ONE teal (default) button at a time — legacy-open mode", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"e2"' });
    renderScreen();
    await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE);
    // The legacy banner's own Add provider is the state's one affirmative;
    // Save providers is withheld while it shows (providers-screen.tsx).
    const teal = screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));
    expect(teal.length).toBe(1);
    expect(teal[0]).toHaveTextContent(PROVIDERS.ADD_ROW_CTA);
  });

  // The pin that would have caught it: a populated row's OWN credential-lane
  // Save button (SecretLane, rendered by default on the row's initial
  // selected lane) must not compete with the screen's Save providers — the
  // lane's Save is `saveVariant="secondary"` for exactly this reason
  // (git-tab.tsx).
  it("carries exactly ONE teal (default) button at a time — a populated row", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e2b"',
    });
    renderScreen();
    await screen.findByTestId("provider-row-github");
    const teal = screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));
    expect(teal.length).toBe(1);
    expect(teal[0]).toHaveTextContent(PROVIDERS.SAVE_CTA);
  });

  it("saves the whole document and shows the saved toast", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e3"',
    });
    putWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e4"',
      sourcesNoLongerAdmitted: 0,
    });
    renderScreen();
    await screen.findByTestId("provider-row-github");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(putWorkspaceProvidersMock).toHaveBeenCalled();
  });

  it("a 412 renders the saved-elsewhere state and never overwrites", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e5"',
    });
    putWorkspaceProvidersMock.mockRejectedValue(new HttpError(412, "providers changed since you loaded them — reload and retry"));
    renderScreen();
    await screen.findByTestId("provider-row-github");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(await screen.findByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeInTheDocument();
  });

  it("a 400 renders the server's own refusal verbatim under SAVE_REFUSED_TITLE", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e6"',
    });
    putWorkspaceProvidersMock.mockRejectedValue(new HttpError(400, 'id "github" is not unique'));
    renderScreen();
    await screen.findByTestId("provider-row-github");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(await screen.findByText(PROVIDERS.SAVE_REFUSED_TITLE)).toBeInTheDocument();
    expect(screen.getByText('id "github" is not unique')).toBeInTheDocument();
  });
});
