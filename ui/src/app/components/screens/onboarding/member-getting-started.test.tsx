/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus, AgentRun } from "../../../lib/types";
import { baseMe, baseMeDrive, baseStatus } from "../../../lib/test-fixtures";

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

// The member's own ceiling, named by GET /policies/default. The default answer
// carries no governance_profile_name — an UNASSIGNED member, which is what
// every case below except the governance ones is.
const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: { getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a) },
}));

import { MemberGettingStarted } from "./member-getting-started";
import type { Me } from "../../../lib/api/health";
import { OperatorProvider } from "../../wardyn/operator-context";
import { MEMBER } from "../../../lib/governance-copy";
import { DRIVES, DRIVE_MEMBER as DM } from "../../../lib/user-drives-copy";

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

// The page reads its drive off the shell's ONE GET /me (operator-context's
// UserDriveContext), not a fetch of its own — so a case states its /me body
// here, exactly as app-shell hands it down.
function renderPage(me: Me = baseMe()) {
  return render(
    <MemoryRouter>
      <OperatorProvider
        operator={false}
        securityOperator={false}
        userDrive={me.user_drive}
        userDriveDeniedByProfile={me.user_drive_denied_by_profile}
      >
        <MemberGettingStarted />
      </OperatorProvider>
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
    getDefaultPolicyMock.mockReset().mockResolvedValue({ min_confinement_class: "CC1" });
  });

  // §7.6's second display moment: the chip names the profile, the line says
  // what having one means. Both render ONLY when one is assigned — with no
  // assignment there is no chip, no line and no placeholder, which is the
  // absent-row doctrine the resolver itself follows.
  it("names the assigned governance profile — chip and line together", async () => {
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC1",
      governance_profile_name: "walled",
    });
    renderPage();
    expect(await screen.findByText(MEMBER.GS_CHIP("walled"))).toBeInTheDocument();
    expect(screen.getByText(MEMBER.GS_BODY("walled"))).toBeInTheDocument();
  });

  it("an unassigned member gets neither", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "What's set up for you" });
    await waitFor(() => expect(getDefaultPolicyMock).toHaveBeenCalled());
    expect(screen.queryByText(/^Governance · /)).not.toBeInTheDocument();
    expect(screen.queryByText(/Your runs are bounded by/)).not.toBeInTheDocument();
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
  // §7.6's Getting Started moments, both keyed on /me.user_drive alone: with
  // no allocation there is no chip and no sentence, which is today's page.
  it("names the allocated drive — chip and the not-a-workspace sentence together", async () => {
    renderPage(baseMe({ user_drive: baseMeDrive() }));
    expect(
      await screen.findByText(DM.GS_DRIVE_CHIP("Scratch", DRIVES.SIZE_GIB(16), DRIVES.MODE_RW_INLINE)),
    ).toBeInTheDocument();
    expect(screen.getByText(DM.GS_DRIVE_BODY)).toBeInTheDocument();
  });

  it("takes the _NOSIZE twin for a share with no allocation shown", async () => {
    renderPage(
      baseMe({
        user_drive: baseMeDrive({
          name: "Corporate homes",
          backend: "k8s_pvc_static",
          size_mib: undefined,
          writable: false,
          enforcement: "external",
        }),
      }),
    );
    expect(
      await screen.findByText(DM.GS_DRIVE_CHIP_NOSIZE("Corporate homes", DRIVES.MODE_RO_INLINE)),
    ).toBeInTheDocument();
  });

  // Paused wins over the size and the mode: an allocation an admin disabled
  // mounts nothing next run, so naming its size would describe storage this
  // member cannot reach.
  it("says Paused instead of a size and a mode", async () => {
    renderPage(
      baseMe({ user_drive: baseMeDrive({ backend: "docker_volume", enforcement: "none", paused: true }) }),
    );
    expect(await screen.findByText(DM.GS_DRIVE_CHIP_PAUSED("Scratch"))).toBeInTheDocument();
    expect(
      screen.queryByText(DM.GS_DRIVE_CHIP("Scratch", DRIVES.SIZE_GIB(16), DRIVES.MODE_RW_INLINE)),
    ).not.toBeInTheDocument();
  });

  it("shows no chip and no sentence with nothing allocated — today's page", async () => {
    // The governance chip is the settle anchor: it appears only once the
    // async reads have flushed, so the two absences below are a resolved
    // state rather than a race with the /me read.
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC1",
      governance_profile_name: "walled",
    });
    renderPage();
    expect(await screen.findByText(MEMBER.GS_CHIP("walled"))).toBeInTheDocument();
    expect(screen.queryByText(DM.GS_DRIVE_BODY)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Drive · /)).not.toBeInTheDocument();
  });
});
