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
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { MEMBER_GETTING_STARTED as T } from "../../wardyn/copy";

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

  // The door outranks the allocation HERE too, and for the reason it does on
  // the New Run card: this page does not merely name the drive, GS_DRIVE_BODY
  // sends the member to New run to mount it — where NR_DENIED refuses them by
  // profile name one page load later. An offer that cannot be taken is a false
  // instruction, so neither moment renders; the absent row claims nothing.
  it("a shut governance door renders neither the drive chip nor the mount instruction", async () => {
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC1",
      governance_profile_name: "walled",
    });
    renderPage(
      baseMe({ user_drive: baseMeDrive(), user_drive_denied_by_profile: "Contractors" }),
    );
    // The governance chip settles the async reads, so the two absences below
    // are a resolved state rather than a race with the /me read.
    expect(await screen.findByText(MEMBER.GS_CHIP("walled"))).toBeInTheDocument();
    expect(screen.queryByText(DM.GS_DRIVE_BODY)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Drive · /)).not.toBeInTheDocument();
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

  // C4.5/C3 — the chip stops reading the deployment-wide llm_ready and reads
  // THIS caller's own SetupStatus.model_access instead: success tone ONLY for
  // "live", every other state warning with the server's own `action` verbatim
  // as the chip row's own line.
  describe("the Model access chip reads status.model_access", () => {
    it("live: success tone, no action line, no CTA", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "live" } }));
      renderPage();
      const chip = await screen.findByText(AGENTS.MODEL_ACCESS_LIVE);
      expect(chip).toBeInTheDocument();
      expect(chip.closest("span")?.className).toMatch(/success/);
      expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    });

    it("expiring: warning tone, the server's action verbatim, and the CTA", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "expiring", action: "Sign in again before 2026-09-12 09:00" } }),
      );
      renderPage();
      const chip = await screen.findByText(AGENTS.MODEL_ACCESS_EXPIRING);
      expect(chip.closest("span")?.className).toMatch(/warning/);
      expect(screen.getByText("Sign in again before 2026-09-12 09:00")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
    });

    it("expired_signin: warning tone and the CTA", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "expired_signin" } }));
      renderPage();
      await screen.findByText(AGENTS.MODEL_ACCESS_EXPIRED);
      expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
    });

    it("not_configured: warning tone and the CTA", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" } }));
      renderPage();
      await screen.findByText(AGENTS.MODEL_ACCESS_NOT_CONFIGURED);
      expect(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeInTheDocument();
    });

    it("shared_expired: warning tone, the action line, and NO button — nothing the member can do", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({
          model_access: {
            state: "shared_expired",
            action: "Your admin's model credential expired — ask them to reconnect it",
          },
        }),
      );
      renderPage();
      await screen.findByText(AGENTS.MODEL_ACCESS_SHARED_EXPIRED);
      expect(
        screen.getByText("Your admin's model credential expired — ask them to reconnect it"),
      ).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    });

    it("model_access absent (older daemon / a failed fetch) keeps today's llm_ready rendering", async () => {
      getSetupStatusMock.mockResolvedValue(status({ llm_ready: true }));
      renderPage();
      expect(await screen.findByText("Model access · Provided by your admin")).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    });

    it("clicking Sign in to AWS opens HarnessLoginPane in place", async () => {
      const user = userEvent.setup();
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" } }));
      renderPage();
      await user.click(await screen.findByRole("button", { name: AGENTS.SIGN_IN_AWS }));
      expect(screen.getByTestId("harness-login-pane")).toBeInTheDocument();
    });

    // V1 r2 LOW: the pane's start-URL field started EMPTY, so every member had to
    // find their org's access portal themselves — and the server ignores what
    // they type under a per_user row, signing in against the row's own
    // sso_start_url. The field was a control with no effect; the note is the fact.
    it("...and that pane asks for no access portal — the admin's is the one used", async () => {
      const user = userEvent.setup();
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" } }));
      renderPage();
      await user.click(await screen.findByRole("button", { name: AGENTS.SIGN_IN_AWS }));
      expect(screen.getByText(AGENTS.SSO_START_URL_MANAGED)).toBeInTheDocument();
      expect(document.getElementById("harness-login-start-url")).toBeNull();
      // The pane is at its consent gate, ready to launch — not stuck waiting on
      // a field it no longer shows.
      expect(screen.getByTestId("login-intro")).toBeInTheDocument();
    });

    // V1 r2 LOW: every unrecognised state fell through to
    // MODEL_ACCESS_NOT_CONFIGURED — so `expired_renewable`, which dispatch
    // RENEWS, told the member they were not signed in, with no CTA to fix it.
    it("a state outside the five renders NO chip and no CTA — unknown is not 'not configured'", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "expired_renewable" }, llm_ready: true }));
      renderPage();
      // Something from the card renders, so this is not an empty-page false pass.
      await screen.findByText(T.SETUP_SUMMARY_HELPER);
      expect(screen.queryByText(AGENTS.MODEL_ACCESS_NOT_CONFIGURED)).not.toBeInTheDocument();
      expect(screen.queryByText(AGENTS.MODEL_ACCESS_LIVE)).not.toBeInTheDocument();
      expect(screen.queryByText(/^Model access · /)).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    });

    // Appendix A finding 5: not_applicable is the admin-token principal's own
    // answer ("this is a shared token, not a person") — it carries NO chip
    // label (MODEL_ACCESS_CHIP_LABEL has no entry for it) and NO action. A
    // truthy `model_access` object must not short-circuit past the llm_ready
    // fallback just because it exists: the caller still lost the deployment-
    // wide "Provided by your admin" chip it is entitled to under llm_ready.
    it("not_applicable falls back to the llm_ready chip instead of rendering nothing", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_applicable" }, llm_ready: true }));
      renderPage();
      expect(await screen.findByText(T.MODEL_ACCESS_PROVIDED_CHIP)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    });

    // A member under not_applicable with no other model access must not be
    // offered a sign-in they structurally cannot complete (a shared token has
    // no person to sign in as).
    it("not_applicable with llm_ready false offers no sign-in CTA", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_applicable" }, llm_ready: false }));
      renderPage();
      await screen.findByText(T.SETUP_SUMMARY_HELPER);
      expect(screen.queryByText(T.MODEL_ACCESS_PROVIDED_CHIP)).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).not.toBeInTheDocument();
    });
  });
});
