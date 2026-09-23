/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus, AgentRun, SetupHarnessTool } from "../../../lib/types";
import { baseMe, baseMeDrive, baseStatus } from "../../../lib/test-fixtures";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

// The connect popup + poll (#386) — mocked so the chip's CONNECT_ADO tests
// below drive the click without a real window.
const adoConnectMock = vi.fn();
let adoBlockedUrl: string | null = null;
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: adoConnectMock, connectFallback: adoConnectMock, blockedUrl: adoBlockedUrl }),
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
import { ADO } from "../../../lib/ado-entra-copy";
import { MEMBER_GETTING_STARTED as T, YOUR_MODEL_KEY as YMK } from "../../wardyn/copy";

// U-13 (a11y): the two "Sign in to AWS" buttons now carry DISTINCT accessible
// names (the visible text plus the section they are in), so a lookup by the
// visible name is a prefix match — the same query, still by what the button
// says, and the exact aria-labels are pinned in their own case below.
const SIGN_IN_AWS_NAME = new RegExp(`^${AGENTS.SIGN_IN_AWS}`);


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

// Appendix A finding 2 — a per_user roster row (modelKeyProvider's
// harnesses.find picks the first enabled row in the SERVER's catalog order;
// "claude-code" matches the default secret name every fixture above already
// uses). mechanism: bedrock_sso is the real wire shape this state pairs with
// (FIX PASS 1, REVIEW-1.md ruling R1).
const perUserHarness: SetupHarnessTool[] = [
  {
    id: "claude-code",
    display: "Claude Code",
    has_gateway: true,
    has_login: true,
    enabled: true,
    credential_source: "per_user",
    mechanism: "bedrock_sso",
  },
];

// FIX PASS 1 — a NON-per_user row whose mechanism is Bedrock (ruling R2(b)):
// a member's own key can never satisfy it either, but the row is "shared".
const sharedBedrockHarness: SetupHarnessTool[] = [
  { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true, mechanism: "bedrock_sso" },
];

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
        userType={me.user_type ?? null}
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

  // UT-7a: the subtitle introduces the caller's own type off /me.user_type.
  it("introduces the caller's user type with its description", async () => {
    renderPage(baseMe({ user_type: { id: "pm", name: "Portfolio manager", description: "Runs an agent over one portfolio." } }));
    expect(
      await screen.findByText(
        "You're set up as Portfolio manager: Runs an agent over one portfolio. Your admin set the ceiling; you run inside it.",
      ),
    ).toBeInTheDocument();
  });

  it("introduces a type with no description by its name alone", async () => {
    renderPage(baseMe({ user_type: { id: "standard", name: "Standard user" } }));
    expect(
      await screen.findByText("You're set up as Standard user. Your admin set the ceiling; you run inside it."),
    ).toBeInTheDocument();
    expect(screen.getByText(T.SUBTITLE("Standard user"))).toBeInTheDocument();
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

  // The chip reads THIS caller's own SetupStatus.model_access, never the
  // deployment-wide llm_ready: success tone ONLY for "live", every other
  // state warning with the server's own `action` verbatim as the chip row's
  // own line.
  describe("the Model access chip reads status.model_access", () => {
    // U-1 — these two carry the PER_USER row: "Your AWS sign-in" and
    // "Expiring" are per-person labels, and the server emits the same two
    // states for a SHARED row's admin credential, where they would name a
    // sign-in this member does not have (the U-1 cases below).
    it("live: success tone, no action line, no CTA", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "live" }, harnesses: perUserHarness }));
      renderPage();
      const chip = await screen.findByText(AGENTS.MODEL_ACCESS_LIVE);
      expect(chip).toBeInTheDocument();
      expect(chip.closest("span")?.className).toMatch(/success/);
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
    });

    it("expiring: warning tone, the server's action verbatim, and the CTA", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({
          model_access: { state: "expiring", action: "Sign in again before 2026-09-12 09:00" },
          harnesses: perUserHarness,
        }),
      );
      renderPage();
      const chip = await screen.findByText(AGENTS.MODEL_ACCESS_EXPIRING);
      expect(chip.closest("span")?.className).toMatch(/warning/);
      expect(screen.getByText("Sign in again before 2026-09-12 09:00")).toBeInTheDocument();
      expect(screen.getAllByRole("button", { name: SIGN_IN_AWS_NAME }).length).toBeGreaterThan(0);
    });

    it("expired_signin: warning tone and the CTA", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "expired_signin" } }));
      renderPage();
      await screen.findByText(AGENTS.MODEL_ACCESS_EXPIRED);
      expect(screen.getByRole("button", { name: SIGN_IN_AWS_NAME })).toBeInTheDocument();
    });

    it("not_configured: warning tone and the CTA", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" } }));
      renderPage();
      await screen.findByText(AGENTS.MODEL_ACCESS_NOT_CONFIGURED);
      expect(screen.getByRole("button", { name: SIGN_IN_AWS_NAME })).toBeInTheDocument();
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
      // Appendix A finding 2 — the card now ALSO renders this same label
      // (shared_expired is a "shared/none" row's own state, so the card's
      // truth table lands there too, correctly scoped instead of the old
      // "Provided by your admin"/empty-form guess). Scope to the chip row's
      // own section to keep this test about the chip row alone.
      const summarySection = (await screen.findByRole("heading", { name: T.SETUP_SUMMARY_TITLE })).closest("section")!;
      expect(within(summarySection).getByText(AGENTS.MODEL_ACCESS_SHARED_EXPIRED)).toBeInTheDocument();
      expect(
        screen.getByText("Your admin's model credential expired — ask them to reconnect it"),
      ).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
      // The card's own body: correctly scoped, not "Provided by your admin".
      expect(screen.getByText(YMK.SHARED_EXPIRED_BODY)).toBeInTheDocument();
      expect(screen.queryByText(YMK.PROVIDED_CHIP)).not.toBeInTheDocument();
    });

    it("model_access absent (older daemon / a failed fetch) keeps today's llm_ready rendering", async () => {
      getSetupStatusMock.mockResolvedValue(status({ llm_ready: true }));
      renderPage();
      expect(await screen.findByText("Model access · Provided by your admin")).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
    });

    // Appendix A finding 2 — a per_user row's not_configured state is
    // actionable on BOTH the chip row (unchanged) and the card (new): TWO
    // "Sign in to AWS" buttons, one pane, either one opens it (both call the
    // same setAwsLoginOpen(true)).
    it("clicking Sign in to AWS opens HarnessLoginPane in place", async () => {
      const user = userEvent.setup();
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" }, harnesses: perUserHarness }));
      renderPage();
      const buttons = await screen.findAllByRole("button", { name: SIGN_IN_AWS_NAME });
      expect(buttons).toHaveLength(2);
      await user.click(buttons[0]);
      expect(screen.getByTestId("harness-login-pane")).toBeInTheDocument();
    });

    // FIX PASS 1 (REVIEW-1.md H1) — the SAME scenario, but through the
    // CARD's own button (buttons[1]) instead of the chip row's: both must
    // reach the identical single pane mount.
    it("...and so does the CARD's own Sign in to AWS button", async () => {
      const user = userEvent.setup();
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" }, harnesses: perUserHarness }));
      renderPage();
      const buttons = await screen.findAllByRole("button", { name: SIGN_IN_AWS_NAME });
      expect(buttons).toHaveLength(2);
      await user.click(buttons[1]);
      expect(screen.getByTestId("harness-login-pane")).toBeInTheDocument();
    });

    // The pane's start-URL field would otherwise be EMPTY, leaving every
    // member to find their org's access portal themselves — and the server
    // ignores what they type under a per_user row, signing in against the
    // row's own sso_start_url anyway. A field with no effect is worse than
    // none; the note is the fact.
    it("...and that pane asks for no access portal — the admin's is the one used", async () => {
      const user = userEvent.setup();
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" }, harnesses: perUserHarness }));
      renderPage();
      const buttons = await screen.findAllByRole("button", { name: SIGN_IN_AWS_NAME });
      expect(buttons).toHaveLength(2);
      await user.click(buttons[0]);
      expect(screen.getByText(AGENTS.SSO_START_URL_MANAGED)).toBeInTheDocument();
      expect(document.getElementById("harness-login-start-url")).toBeNull();
      // The pane is at its consent gate, ready to launch — not stuck waiting on
      // a field it no longer shows.
      expect(screen.getByTestId("login-intro")).toBeInTheDocument();
    });

    // Appendix A finding 2 — the card's own claim now follows the SAME
    // per-principal state as the chip row, instead of the deployment-wide
    // llm_ready that made a not-signed-in member's card say "Provided by
    // your admin".
    it("not_configured + llm_ready:true (the field report's own scenario): card says Not signed in, checklist NOT done, two buttons, one pane", async () => {
      // FIX PASS 1 (REVIEW-1.md M1) — a workspace present makes "model-key"
      // the page's first NOT-done actionable section IF (and only if)
      // modelKeyDone is actually false: reverting modelKeyDone to the old
      // `hasOwnKey || llmReady` predicate (llmReady:true here) would flip it
      // true, hand the page's one `default`-variant slot to "first-run"
      // instead, and leave this whole test green — which is exactly the gap
      // the reviewer's probe found. Assert the OBSERVABLE: it's still the
      // card's own Sign-in button that gets `default`, not "New run".
      listWorkspacesMock.mockResolvedValue([{ id: "w1" }]);
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "not_configured" }, llm_ready: true, harnesses: perUserHarness }),
      );
      renderPage();
      expect(await screen.findByText(YMK.NOT_SIGNED_IN_CHIP)).toBeInTheDocument();
      expect(screen.getByText(YMK.NOT_SIGNED_IN_BODY)).toBeInTheDocument();
      expect(screen.queryByText(YMK.PROVIDED_CHIP)).not.toBeInTheDocument();
      // NOT done: the "Your model key" section's own header carries no Done
      // chip (scoped to its <section> — Workspace's Done/not-done is a
      // different section and out of scope here).
      const modelKeySection = screen.getByRole("heading", { name: "Your model key" }).closest("section")!;
      expect(within(modelKeySection).queryByText("Done")).not.toBeInTheDocument();
      const buttons = await screen.findAllByRole("button", { name: SIGN_IN_AWS_NAME });
      expect(buttons).toHaveLength(2);
      expect(screen.getByText(T.SETUP_SUMMARY_HELPER_PER_USER)).toBeInTheDocument();
      // M1's observable: modelKeyDone false -> model-key wins the ONE
      // `default` CTA over "New run" (which stays outline).
      await waitFor(() => expect(defaultButtons().length).toBe(1));
      expect(defaultButtons()[0].textContent).toContain(AGENTS.SIGN_IN_AWS);
      expect(screen.getByRole("link", { name: "New run" }).className).not.toContain("bg-primary ");
    });

    it("live + llm_ready:true: card says Your AWS sign-in; lede says the per_user variant", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "live" }, llm_ready: true, harnesses: perUserHarness }),
      );
      renderPage();
      expect(await screen.findByText(YMK.SIGNED_IN_CHIP)).toBeInTheDocument();
      expect(screen.getByText(YMK.SIGNED_IN_BODY)).toBeInTheDocument();
      expect(screen.getByText(T.SETUP_SUMMARY_HELPER_PER_USER)).toBeInTheDocument();
      expect(screen.queryByText(T.SETUP_SUMMARY_HELPER)).not.toBeInTheDocument();
    });

    // Appendix A finding 2, plan item 5 — hasOwn is IGNORED under a per_user
    // row: a stale `mine` write from before the roster switched this member
    // to per_user must not read as "Your key" over a lane that can never use
    // it, and the checklist must not mark the section done.
    // FIX PASS 1 (REVIEW-1.md H1) — gating on the old bare `hasOwnKey` alone
    // would leave this exact fixture showing MODEL_ACCESS_OWN_CHIP ("Your
    // key", success) with the chip row AND the card's own Sign-in button
    // dead: H1's probe catches PROBE pane present after click = false.
    it("hasOwn:true + per_user + not_configured: still Not signed in, checklist NOT done, reveal absent, own chip absent, card's own button mounts the pane", async () => {
      const user = userEvent.setup();
      // FIX PASS 1 (M1) — same observable-of-modelKeyDone technique as the
      // test above: a workspace present makes model-key the page's sole
      // `default`-variant section IFF modelKeyDone is actually false.
      listWorkspacesMock.mockResolvedValue([{ id: "w1" }]);
      listSecretsMineMock.mockResolvedValue({ names: ["anthropic-api-key"], mine: ["anthropic-api-key"] });
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" }, harnesses: perUserHarness }));
      renderPage();
      expect(await screen.findByText(YMK.NOT_SIGNED_IN_CHIP)).toBeInTheDocument();
      const modelKeySection = screen.getByRole("heading", { name: "Your model key" }).closest("section")!;
      expect(within(modelKeySection).queryByText("Done")).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Use my own key instead" })).not.toBeInTheDocument();
      // H1 — the chip row's own success chip must not render over a lane
      // this member's key cannot use, and the card's "Provided by your
      // admin" fallback must not either.
      expect(screen.queryByText(T.MODEL_ACCESS_OWN_CHIP)).not.toBeInTheDocument();
      expect(screen.queryByText(YMK.PROVIDED_CHIP)).not.toBeInTheDocument();
      // H1 — TWO buttons (chip row + card), and the CARD's own (buttons[1])
      // is wired to the SAME pane, not dead.
      const buttons = await screen.findAllByRole("button", { name: SIGN_IN_AWS_NAME });
      expect(buttons).toHaveLength(2);
      // M1's observable: modelKeyDone false -> model-key still wins the ONE
      // `default` CTA (would flip to "New run" if hasOwnKey alone marked it
      // done, since hasOwnKey is true in this fixture).
      await waitFor(() => expect(defaultButtons().length).toBe(1));
      expect(defaultButtons()[0].textContent).toContain(AGENTS.SIGN_IN_AWS);
      await user.click(buttons[1]);
      expect(screen.getByTestId("harness-login-pane")).toBeInTheDocument();
    });

    // Negative control for the reveal-hidden assertion above: a shared/api-key
    // row (no per_user credential_source) keeps today's reveal.
    it("negative control: reveal IS present under a shared/api-key row", async () => {
      getSetupStatusMock.mockResolvedValue(status({ llm_ready: true }));
      renderPage();
      expect(await screen.findByRole("button", { name: "Use my own key instead" })).toBeInTheDocument();
    });

    // An unrecognised state must not fall through to MODEL_ACCESS_NOT_CONFIGURED
    // — `expired_renewable`, which dispatch RENEWS, would otherwise tell the
    // member they are not signed in, with no CTA to fix it.
    it("a state outside the five renders NO chip and no CTA — unknown is not 'not configured'", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "expired_renewable" }, llm_ready: true }));
      renderPage();
      // Something from the card renders, so this is not an empty-page false pass.
      await screen.findByText(T.SETUP_SUMMARY_HELPER);
      expect(screen.queryByText(AGENTS.MODEL_ACCESS_NOT_CONFIGURED)).not.toBeInTheDocument();
      expect(screen.queryByText(AGENTS.MODEL_ACCESS_LIVE)).not.toBeInTheDocument();
      expect(screen.queryByText(/^Model access · /)).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
    });

    // Appendix A finding 5: not_applicable is the admin-token principal's own
    // answer ("this is a shared token, not a person") and carries NO action.
    // A truthy `model_access` object must not short-circuit past the
    // llm_ready fallback just because it exists: the caller still gets the
    // deployment-wide "Provided by your admin" chip it is entitled to under
    // llm_ready — #158 adds its OWN chip beside that fallback rather than in
    // place of it.
    it("not_applicable falls back to the llm_ready chip AND renders its own chip beside it", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_applicable" }, llm_ready: true }));
      renderPage();
      expect(await screen.findByText(T.MODEL_ACCESS_PROVIDED_CHIP)).toBeInTheDocument();
      expect(screen.getByText(AGENTS.MODEL_ACCESS_NOT_APPLICABLE)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
    });

    // #158 — REWRITTEN: this used to assert that not_applicable with
    // llm_ready false rendered NO chip at all, which was the bug the issue
    // fixes (unknown ≠ a deliberate answer). It now asserts the opposite: its
    // own neutral chip renders even with no llm_ready fallback to ride beside
    // — and a member under not_applicable still gets no sign-in CTA, since a
    // shared token has no person to sign in as.
    it("not_applicable with llm_ready false renders its own chip, still no sign-in CTA", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_applicable" }, llm_ready: false }));
      renderPage();
      expect(await screen.findByText(AGENTS.MODEL_ACCESS_NOT_APPLICABLE)).toBeInTheDocument();
      expect(screen.queryByText(T.MODEL_ACCESS_PROVIDED_CHIP)).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
    });

    // FIX PASS 1 (REVIEW-1.md H1b/M2) — the two fixtures above (both WITHOUT
    // a per_user harness row) stay byte-identical; THIS is the combination
    // the server actually produces (not_applicable is emitted only for the
    // admin-token principal on an ENABLED per_user row,
    // awsSSOScopeIsMechanism in internal/api/modelaccess.go). Under it: the
    // card shows PER_PERSON_NA_BODY, no form, no chip, no sign-in button —
    // and neither PROVIDED_* string appears anywhere on the page, including
    // the chip row's own llm_ready fallback (H1b: that fallback must not
    // fire under a per_user row, or it would contradict the card and the
    // per_user lede beside it).
    it("not_applicable WITH the per_user harness fixture: PER_PERSON_NA_BODY, no form/chip/button, no PROVIDED_* anywhere", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "not_applicable" }, llm_ready: true, harnesses: perUserHarness }),
      );
      renderPage();
      expect(await screen.findByText(YMK.PER_PERSON_NA_BODY)).toBeInTheDocument();
      expect(screen.queryByPlaceholderText("sk-ant-…")).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Use my own key instead" })).not.toBeInTheDocument();
      expect(screen.queryByText(T.MODEL_ACCESS_PROVIDED_CHIP)).not.toBeInTheDocument();
      expect(screen.queryByText(YMK.PROVIDED_CHIP)).not.toBeInTheDocument();
      expect(screen.queryByText(YMK.PROVIDED_BODY)).not.toBeInTheDocument();
      const modelKeySection = screen.getByRole("heading", { name: "Your model key" }).closest("section")!;
      expect(within(modelKeySection).queryByText("Done")).not.toBeInTheDocument();
    });

    // FIX PASS 1 (REVIEW-1.md R2(b)/I1) — a NON-per_user row whose mechanism
    // is Bedrock: the deployment-wide llm_ready IS the right answer (this is
    // a shared credential, graded per-deployment, not per-principal), so the
    // chip row's fallback still fires — H1b's suppression is per_user-only.
    it("shared row with a Bedrock mechanism, llm_ready:true: chip row STILL shows Provided by your admin", async () => {
      getSetupStatusMock.mockResolvedValue(status({ llm_ready: true, harnesses: sharedBedrockHarness }));
      renderPage();
      expect(await screen.findByText(T.MODEL_ACCESS_PROVIDED_CHIP)).toBeInTheDocument();
    });

    // U-1 (W6 blind lens) — the wire shape the server really emits for a shared
    // bedrock_sso row: memberModelAccess (internal/api/modelaccess.go) projects
    // `live` for the ADMIN's credential, so the fixture above (no model_access at
    // all) never exercises the branch that actually renders. Without this, the
    // chip row would read "Model access · Your AWS sign-in" — a sign-in this
    // member does not have — over a card saying "Provided by your admin", while
    // New Run's rail says "Admin's credential". live/expiring under a
    // NOT-per_user row is the admin's shared credential, so the chip row must
    // say what the card says.
    it("U-1: shared bedrock_sso row + model_access live renders the PROVIDED chip, never 'Your AWS sign-in'", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "live" }, llm_ready: true, harnesses: sharedBedrockHarness }),
      );
      renderPage();
      expect(await screen.findByText(T.MODEL_ACCESS_PROVIDED_CHIP)).toBeInTheDocument();
      expect(screen.queryByText(AGENTS.MODEL_ACCESS_LIVE)).not.toBeInTheDocument();
    });

    // …and the same for `expiring`: the admin's credential is the one expiring,
    // and the member has no sign-in of their own to renew.
    it("U-1: shared bedrock_sso row + model_access expiring renders the PROVIDED chip too", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "expiring" }, llm_ready: true, harnesses: sharedBedrockHarness }),
      );
      renderPage();
      expect(await screen.findByText(T.MODEL_ACCESS_PROVIDED_CHIP)).toBeInTheDocument();
      expect(screen.queryByText(AGENTS.MODEL_ACCESS_EXPIRING)).not.toBeInTheDocument();
    });

    // U-1's negative control: under a PER_USER row `live` is genuinely this
    // member's own sign-in, and the success chip stays exactly as it is.
    it("U-1 negative control: per_user + live still renders MODEL_ACCESS_LIVE", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "live" }, llm_ready: true, harnesses: perUserHarness }),
      );
      renderPage();
      expect(await screen.findByText(AGENTS.MODEL_ACCESS_LIVE)).toBeInTheDocument();
      expect(screen.queryByText(T.MODEL_ACCESS_PROVIDED_CHIP)).not.toBeInTheDocument();
    });

    // U-13 (a11y, W6 blind lens) — without distinct names, the page's two
    // sign-in buttons would carry the IDENTICAL accessible name "Sign in to
    // AWS" (plus a plain-text action line saying the same words), and the
    // card's one opens a pane mounted in the card ABOVE it, moving no focus
    // and staying enabled as a no-op once it is open.
    it("U-13: the two Sign in to AWS buttons have distinct accessible names, and the card's hides while the pane is open", async () => {
      const user = userEvent.setup();
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "not_configured" }, harnesses: perUserHarness }),
      );
      renderPage();
      const buttons = await screen.findAllByRole("button", { name: SIGN_IN_AWS_NAME });
      expect(buttons).toHaveLength(2);
      expect(buttons.map((b) => b.getAttribute("aria-label"))).toEqual([
        T.SIGN_IN_AWS_ARIA_SUMMARY,
        YMK.SIGN_IN_AWS_ARIA_CARD,
      ]);
      // Both still SAY "Sign in to AWS" — only the accessible name gained the
      // section, so the visible console is byte-identical.
      for (const b of buttons) expect(b).toHaveTextContent(AGENTS.SIGN_IN_AWS);
      await user.click(buttons[0]);
      expect(screen.getByTestId("harness-login-pane")).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: SIGN_IN_AWS_NAME })).not.toBeInTheDocument();
    });

    // U-9 (W6 blind lens) — the page must call modelKeyState WITH `mechanism`,
    // matching the card: without it, under a shared BEDROCK row with a
    // leftover own key, the page would grade "own" (done) while the card
    // grades shared_expired (not done). The observable is the colour
    // budget — the one teal would move to "New run" while the card says the
    // credential expired.
    it("U-9: shared bedrock row + own key + shared_expired — the page agrees with the card, 'New run' is not teal", async () => {
      listWorkspacesMock.mockResolvedValue([{ id: "w1" }]);
      listSecretsMineMock.mockResolvedValue({ names: ["anthropic-api-key"], mine: ["anthropic-api-key"] });
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "shared_expired" }, llm_ready: true, harnesses: sharedBedrockHarness }),
      );
      renderPage();
      expect(await screen.findByText(YMK.SHARED_EXPIRED_BODY)).toBeInTheDocument();
      const modelKeySection = screen.getByRole("heading", { name: "Your model key" }).closest("section")!;
      expect(within(modelKeySection).queryByText("Done")).not.toBeInTheDocument();
      await waitFor(() =>
        expect(screen.getByRole("link", { name: "New run" }).className).not.toContain("bg-primary "),
      );
    });
  });

  // #386: one more chip from the six states, reusing the vocabulary for a
  // second subject (§6.2/§7.5) — no button in the common case, a named cause
  // in the fallback, never for `not_applicable` (unreachable from a browser).
  describe("the Azure DevOps chip reads status.scm_access", () => {
    beforeEach(() => {
      adoConnectMock.mockReset();
      adoBlockedUrl = null;
    });

    it("live via the org's sign-in: success tone, no action line, no button", async () => {
      getSetupStatusMock.mockResolvedValue(status({ scm_access: { state: "live", source: "org" } }));
      renderPage();
      const chip = await screen.findByText(ADO.ACCESS_LIVE_ORG);
      expect(chip.closest("span")?.className).toMatch(/success/);
      expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
    });

    it("live via a separate connect: its own chip label", async () => {
      getSetupStatusMock.mockResolvedValue(status({ scm_access: { state: "live", source: "separate" } }));
      renderPage();
      expect(await screen.findByText(ADO.ACCESS_LIVE_SEPARATE)).toBeInTheDocument();
    });

    it("live on a shared row (no source): the shared chip, still no button — makes no per-person claim", async () => {
      getSetupStatusMock.mockResolvedValue(status({ scm_access: { state: "live" } }));
      renderPage();
      expect(await screen.findByText(ADO.ACCESS_SHARED_LIVE)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
    });

    it("not_configured: warning tone, the row-is-newer cause line, and CONNECT_ADO", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({ scm_access: { state: "not_configured", cause: "row_is_newer" } }),
      );
      renderPage();
      const chip = await screen.findByText(ADO.ACCESS_NOT_CONNECTED);
      expect(chip.closest("span")?.className).toMatch(/warning/);
      expect(screen.getByText(ADO.CAUSE_ROW_IS_NEWER)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: ADO.CONNECT_ADO })).toBeInTheDocument();
    });

    it("clicking Connect Azure DevOps drives the popup flow and reloads status on success", async () => {
      adoConnectMock.mockResolvedValueOnce(true);
      getSetupStatusMock.mockResolvedValue(
        status({ scm_access: { state: "not_configured", cause: "row_is_newer" } }),
      );
      renderPage();
      await userEvent.click(await screen.findByRole("button", { name: ADO.CONNECT_ADO }));
      expect(adoConnectMock).toHaveBeenCalledTimes(1);
      await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalledTimes(2));
    });

    // Review follow-ups F9/N1: a blocked popup shows the canon sentence and
    // fallback link, and clicking it starts the same poll (bounded).
    it("a blocked popup shows the canon sentence and fallback link, which reloads status once connected", async () => {
      adoBlockedUrl = "/api/v1/scm/azure-devops/signin";
      adoConnectMock.mockResolvedValueOnce(true);
      getSetupStatusMock.mockResolvedValue(
        status({ scm_access: { state: "not_configured", cause: "row_is_newer" } }),
      );
      renderPage();
      expect(await screen.findByText(ADO.CONNECT_POPUP_BLOCKED)).toBeInTheDocument();
      const link = screen.getByRole("link", { name: ADO.CONNECT_ADO });
      expect(link).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin");
      await userEvent.click(link);
      expect(adoConnectMock).toHaveBeenCalledTimes(1);
      await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalledTimes(2));
    });

    it("shared_expired: warning tone, the action line, and NO button — nothing the member can do", async () => {
      getSetupStatusMock.mockResolvedValue(status({ scm_access: { state: "shared_expired" } }));
      renderPage();
      const chip = await screen.findByText(ADO.ACCESS_SHARED_EXPIRED);
      expect(chip.closest("span")?.className).toMatch(/warning/);
      expect(screen.getByText(ADO.ACCESS_SHARED_EXPIRED_ACTION)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
    });

    it("scm_access absent (no Azure DevOps row) renders no chip at all", async () => {
      getSetupStatusMock.mockResolvedValue(status());
      renderPage();
      await screen.findByRole("heading", { name: T.SETUP_SUMMARY_TITLE });
      expect(screen.queryByText(ADO.ACCESS_LIVE_ORG)).not.toBeInTheDocument();
      expect(screen.queryByText(ADO.ACCESS_NOT_CONNECTED)).not.toBeInTheDocument();
    });

    it("not_applicable renders no chip — unreachable from a browser session, and §7.5 freezes none for it", async () => {
      getSetupStatusMock.mockResolvedValue(status({ scm_access: { state: "not_applicable" } }));
      renderPage();
      await screen.findByRole("heading", { name: T.SETUP_SUMMARY_TITLE });
      expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
    });
  });
});
