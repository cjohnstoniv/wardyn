/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus, AgentRun } from "../../../lib/types";
import { baseStatus } from "../../../lib/test-fixtures";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

const listSecretsMineMock = vi.fn();
const setSecretMock = vi.fn();
const deleteSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecretsMine: (...a: unknown[]) => listSecretsMineMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
  },
}));

const listRunsMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { listRuns: (...a: unknown[]) => listRunsMock(...a) },
}));

const listKeysMock = vi.fn();
vi.mock("../../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: (...a: unknown[]) => listKeysMock(...a) },
}));

const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    scanWorkspace: vi.fn(),
  },
}));

import { MemberGettingStarted } from "./member-getting-started";

function status(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    ready: true,
    llm_ready: false,
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    auth: { mode: "local", local_loopback: true },
    ...overrides,
  });
}

function run(id: string): AgentRun {
  return { id, created_at: "", updated_at: "" } as AgentRun;
}

function renderPage() {
  return render(
    <MemoryRouter>
      <MemberGettingStarted />
    </MemoryRouter>,
  );
}

function defaultButtons(): Element[] {
  return Array.from(document.querySelectorAll('[data-slot="button"]')).filter(
    (el) => el.className.includes("bg-primary "),
  );
}

describe("MemberGettingStarted", () => {
  beforeEach(() => {
    localStorage.clear();
    getSetupStatusMock.mockReset().mockResolvedValue(status());
    listSecretsMineMock.mockReset().mockResolvedValue({ names: [], mine: [] });
    setSecretMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
    listRunsMock.mockReset().mockResolvedValue([]);
    listKeysMock.mockReset().mockResolvedValue([]);
    listWorkspacesMock.mockReset().mockResolvedValue([]);
  });

  it("renders the six member sections and never the admin barrier picker", async () => {
    renderPage();
    for (const title of [
      "What's set up for you",
      "Add your workspace",
      "Your model key",
      "Your first run",
      "Approvals you can decide",
      "Connect your tools",
    ]) {
      // Section titles are <h2> headings; "Your first run" also names an
      // episode row (id 06) in the Watch list below, so scope to the heading.
      expect(
        await screen.findByRole("heading", { name: title }),
      ).toBeInTheDocument();
    }
    expect(screen.queryByText("Pick your barrier")).not.toBeInTheDocument();
  });

  it("a redacted has_runs:true with an EMPTY own-runs list does NOT mark 'Your first run' done", async () => {
    getSetupStatusMock.mockResolvedValue(status({ has_runs: true }));
    listRunsMock.mockResolvedValue([]); // the member's own creator-scoped list
    renderPage();

    // The section stays actionable: done-state comes from the member's OWN
    // creator-scoped list, never the install-global has_runs.
    await screen.findByRole("heading", { name: "Your first run" });
    expect(
      await screen.findByRole("link", { name: "New run" }),
    ).toBeInTheDocument();
  });

  it("a rejecting listRuns() leaves the section not-done", async () => {
    listRunsMock.mockRejectedValue(new Error("network"));
    renderPage();

    // Fail-closed: no observed runs, the section stays actionable. (The old
    // per-browser "seen" flag this screen once wrote is gone — a member's
    // landing no longer depends on any browser state.)
    await screen.findByRole("link", { name: "New run" });
  });

  it("exactly one default-variant action cold, and zero once every actionable section is done", async () => {
    const cold = renderPage();
    await screen.findByRole("link", { name: "Add workspace" });
    expect(defaultButtons()).toHaveLength(1);
    cold.unmount();

    listWorkspacesMock.mockResolvedValue([{ id: "w1" }]);
    listSecretsMineMock.mockResolvedValue({
      names: ["anthropic-api-key"],
      mine: ["anthropic-api-key"],
    });
    listRunsMock.mockResolvedValue([run("r1")]);
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:abc", principal: "p", name: "k", public_key: "" },
    ]);

    const { unmount } = renderPage();
    await waitFor(() =>
      expect(screen.queryAllByText("Done").length).toBeGreaterThan(0),
    );
    expect(defaultButtons().length).toBe(0);
    unmount();
  });

  it("unreachable: no chip is marked done, and the alert well shows instead", async () => {
    getSetupStatusMock.mockResolvedValue({ ...status(), unreachable: true });
    listWorkspacesMock.mockResolvedValue([{ id: "w1" }]);
    listRunsMock.mockResolvedValue([run("r1")]);
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:abc", principal: "p", name: "k", public_key: "" },
    ]);
    listSecretsMineMock.mockResolvedValue({
      names: ["anthropic-api-key"],
      mine: ["anthropic-api-key"],
    });

    renderPage();
    expect(
      await screen.findByText("Couldn't reach Wardyn."),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Nothing below is marked done until it can be checked/),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.queryByText("Done")).not.toBeInTheDocument();
    });
  });

  // Regression: the page's own "Model access" summary chip and the "Your
  // model key" section used to read TWO independent copies of `mine` — after
  // a Save the summary kept saying "Provided by your admin", after a Remove
  // it kept saying "Your key". One fetch, passed down, fixes both at once.
  it("Save flips 'Model access' to Your key; Remove flips it back — one listSecretsMine call per settle", async () => {
    getSetupStatusMock.mockResolvedValue(status({ llm_ready: true }));
    listSecretsMineMock
      .mockResolvedValueOnce({ names: [], mine: [] })
      .mockResolvedValueOnce({
        names: ["anthropic-api-key"],
        mine: ["anthropic-api-key"],
      })
      .mockResolvedValueOnce({ names: [], mine: [] });
    const user = userEvent.setup();
    renderPage();

    expect(
      await screen.findByText("Model access · Provided by your admin"),
    ).toBeInTheDocument();
    expect(listSecretsMineMock).toHaveBeenCalledTimes(1);

    await user.click(
      screen.getByRole("button", { name: "Use my own key instead" }),
    );
    await user.type(screen.getByPlaceholderText("sk-ant-…"), "sk-ant-abcdefgh");
    await user.click(screen.getByRole("button", { name: "Save key" }));

    expect(
      await screen.findByText("Model access · Your key"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Model access · Provided by your admin"),
    ).not.toBeInTheDocument();
    expect(listSecretsMineMock).toHaveBeenCalledTimes(2);

    await user.click(screen.getByRole("button", { name: "Remove" }));

    expect(
      await screen.findByText("Model access · Provided by your admin"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Model access · Your key"),
    ).not.toBeInTheDocument();
    expect(listSecretsMineMock).toHaveBeenCalledTimes(3);
  });
});
