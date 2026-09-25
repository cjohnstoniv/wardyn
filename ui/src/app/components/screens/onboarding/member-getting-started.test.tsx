/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupStatus, AgentRun, SetupHarnessTool } from "../../../lib/types";
import { makeRun } from "../../../../test/factories";
import { WithDoor } from "../../../../test/door-harness";
import { MODEL_PROVIDERS, baseMe, baseMeDrive, baseStatus, providerStatus } from "../../../lib/test-fixtures";

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
const createRunMock = vi.fn();
const getRunMock = vi.fn();
const killRunMock = vi.fn();
const getGrantsMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    listRuns: (...a: unknown[]) => listRunsMock(...a),
    createRun: (...a: unknown[]) => createRunMock(...a),
    getRun: (...a: unknown[]) => getRunMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
    getGrants: (...a: unknown[]) => getGrantsMock(...a),
  },
}));

// DemoDetail (setup/demos-step.tsx, React.lazy'd below this page's demo
// sections) drags in the same runner graph demos-step.test.tsx stubs —
// xterm, live approvals, audit and the profile sheet are irrelevant to
// whether the RIGHT demo row pre-opens, so they stay inert markers here too.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: ({ runId }: { runId: string }) => <div data-testid="live-approvals">{runId}</div>,
}));
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
  demoAuditRows: () => [],
}));
vi.mock("../profile-review", () => ({
  ProfileReview: ({ runId }: { runId: string | null }) =>
    runId ? <div data-testid="profile-review">{runId}</div> : null,
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

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
import { ViewAccessProvider, type ViewAccess } from "../../wardyn/console-view";
import { MEMBER } from "../../../lib/governance-copy";
import { DRIVES, DRIVE_MEMBER as DM } from "../../../lib/user-drives-copy";
import { ADO } from "../../../lib/ado-entra-copy";
import { MEMBER_GETTING_STARTED as T } from "../../wardyn/copy";
import { CONNECTIONS } from "../../wardyn/copy/door";

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
  return makeRun({ id, created_at: "", updated_at: "" });
}

// A per_user roster row (the pre-provider per-person AWS-SSO lane) — restored
// for the per_user-lede tests below (fix review on #541).
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

// The page reads its drive off the shell's ONE GET /me (operator-context's
// MeIdentity.userDrive), not a fetch of its own — so a case states its /me body
// here, exactly as app-shell hands it down.
// `shell` is WithDoor's own /setup/status read — this page (since #541)
// mounts no door of its own, but WithDoor is still the harness every screen
// under the shared model-access context renders through. `search` is a
// `?step=<id>` deep link on the page's own URL.
function renderPage(me: Me = baseMe(), shell: SetupStatus | null = null, search = "", access: ViewAccess = "url") {
  return render(
    <WithDoor status={shell} path={`/setup${search}`} operator={false}>
      <ViewAccessProvider value={access}>
        <OperatorProvider
          operator={false}
          securityOperator={false}
          userDrive={me.user_drive}
          userDriveDeniedByProfile={me.user_drive_denied_by_profile}
        >
          <MemberGettingStarted />
        </OperatorProvider>
      </ViewAccessProvider>
    </WithDoor>,
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

  it("renders the six member sections (legacy: no model-providers block) and never the admin barrier picker", async () => {
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

  // #541 (§5.4, packet MP-D): the page's own passive summary chip — no
  // in-page action any more (that lives on Your model connections, reached
  // through the link below it). computed by connectionsSummary
  // (lib/model-connections.ts), the SAME predicate that page's own header
  // chip reads, so the two can never disagree.
  describe("the connections summary chip reads status.model_providers/provider_access", () => {
    it("no provider block: Not set up by your admin, and no sign-in button anywhere", async () => {
      renderPage();
      expect(await screen.findByText(CONNECTIONS.SUMMARY_NOT_SET_UP)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Sign in to AWS" })).not.toBeInTheDocument();
    });

    it("a provider block with every default connected: Ready", async () => {
      const s = providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "live" }]);
      getSetupStatusMock.mockResolvedValue(s);
      renderPage(baseMe(), s);
      expect(await screen.findByText(CONNECTIONS.SUMMARY_READY)).toBeInTheDocument();
    });

    // "expiring" still signs today — Ready, not Needs you (model-connections.ts's
    // own CONNECTED_STATES).
    it("the default provider expiring still reads Ready", async () => {
      const s = providerStatus([
        { provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "expiring" },
      ]);
      getSetupStatusMock.mockResolvedValue(s);
      renderPage(baseMe(), s);
      expect(await screen.findByText(CONNECTIONS.SUMMARY_READY)).toBeInTheDocument();
    });

    it("the default provider not connected: Needs you", async () => {
      const s = providerStatus([
        { provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "not_configured" },
      ]);
      getSetupStatusMock.mockResolvedValue(s);
      renderPage(baseMe(), s);
      expect(await screen.findByText(CONNECTIONS.SUMMARY_NEEDS_YOU)).toBeInTheDocument();
    });

    // Packet MP-D case (d): a non-default row sitting unconnected raises no
    // alarm — only the default's own state counts.
    it("a non-default row unconnected does not turn Ready into Needs you", async () => {
      const s = providerStatus([
        { provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "live" },
        { provider: MODEL_PROVIDERS.anthropicKey, state: "not_configured" },
      ]);
      getSetupStatusMock.mockResolvedValue(s);
      renderPage(baseMe(), s);
      expect(await screen.findByText(CONNECTIONS.SUMMARY_READY)).toBeInTheDocument();
    });

    // Every provider disabled: the row list is empty even though the block
    // itself is present — the same "No providers" state packet MP-D draws.
    it("every provider disabled reads Not set up by your admin, not Needs you", async () => {
      const s = providerStatus([
        { provider: { ...MODEL_PROVIDERS.bedrock, disabled: true }, defaultFor: ["claude-code"], state: "not_configured" },
      ]);
      getSetupStatusMock.mockResolvedValue(s);
      renderPage(baseMe(), s);
      expect(await screen.findByText(CONNECTIONS.SUMMARY_NOT_SET_UP)).toBeInTheDocument();
    });

    // Fix review on #541: with no model-providers block at all,
    // connectionsSummary's rows are always empty — legacySummary is the
    // fallback (lib/model-connections.ts), reading the SAME model_access the
    // retired "Your model key" card used to, so a working per_user AWS lane
    // still reads Ready rather than a false "Not set up by your admin".
    it("a working per_user AWS lane with no model-providers block reads Ready (legacySummary)", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "live" } }));
      renderPage();
      expect(await screen.findByText(CONNECTIONS.SUMMARY_READY)).toBeInTheDocument();
    });

    it("a per_user AWS lane not yet signed in reads Needs you (legacySummary)", async () => {
      getSetupStatusMock.mockResolvedValue(status({ model_access: { state: "not_configured" } }));
      renderPage();
      expect(await screen.findByText(CONNECTIONS.SUMMARY_NEEDS_YOU)).toBeInTheDocument();
    });

    // The shared-credential legacy install: no per-principal model_access at
    // all, but the deployment-wide llm_ready fallback is true — the exact
    // shape "Your model key" used to read "Provided by your admin" from.
    it("a shared legacy install with llm_ready reads Ready (legacySummary)", async () => {
      getSetupStatusMock.mockResolvedValue(status({ llm_ready: true }));
      renderPage();
      expect(await screen.findByText(CONNECTIONS.SUMMARY_READY)).toBeInTheDocument();
    });

    it("renders no chip at all before status has loaded", () => {
      renderPage();
      expect(screen.queryByText(CONNECTIONS.SUMMARY_NOT_SET_UP)).not.toBeInTheDocument();
      expect(screen.queryByText(CONNECTIONS.SUMMARY_READY)).not.toBeInTheDocument();
      expect(screen.queryByText(CONNECTIONS.SUMMARY_NEEDS_YOU)).not.toBeInTheDocument();
    });

    // Restored (fix review on #541): the "shared credentials … your runs
    // inherit them" lede is false under a per_user roster row — SAME reason
    // it's false under a real provider block.
    it("a per_user roster row shows the per_user lede, never the shared one", async () => {
      getSetupStatusMock.mockResolvedValue(
        status({ model_access: { state: "live" }, harnesses: perUserHarness }),
      );
      renderPage();
      expect(await screen.findByText(T.SETUP_SUMMARY_HELPER_PER_USER)).toBeInTheDocument();
      expect(screen.queryByText(T.SETUP_SUMMARY_HELPER)).not.toBeInTheDocument();
    });

    it("a plain shared install (no per_user row, no providers) shows the shared lede", async () => {
      getSetupStatusMock.mockResolvedValue(status({ llm_ready: true }));
      renderPage();
      await screen.findByText(CONNECTIONS.SUMMARY_READY);
      expect(screen.getByText(T.SETUP_SUMMARY_HELPER)).toBeInTheDocument();
      expect(screen.queryByText(T.SETUP_SUMMARY_HELPER_PER_USER)).not.toBeInTheDocument();
    });

    it("a real provider block shows the per_user lede too — every provider is per-person", async () => {
      const s = providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "live" }]);
      getSetupStatusMock.mockResolvedValue(s);
      renderPage(baseMe(), s);
      expect(await screen.findByText(T.SETUP_SUMMARY_HELPER_PER_USER)).toBeInTheDocument();
      expect(screen.queryByText(T.SETUP_SUMMARY_HELPER)).not.toBeInTheDocument();
    });
  });

  // Restored (fix review on #541): "Your model key" is the legacy install's
  // ONLY door until #548 converts every install to a provider block — pinned
  // in both directions so neither regresses silently again.
  describe("Your model key — the legacy door, gone once a real provider block exists", () => {
    it("legacy (no model_providers): the card renders, and Save calls secrets.setSecret", async () => {
      const user = userEvent.setup();
      getSetupStatusMock.mockResolvedValue(status({ llm_ready: true }));
      renderPage();
      expect(await screen.findByRole("heading", { name: "Your model key" })).toBeInTheDocument();

      await user.click(screen.getByRole("button", { name: "Use my own key instead" }));
      await user.type(screen.getByPlaceholderText("sk-ant-…"), "sk-ant-abcdefgh");
      await user.click(screen.getByRole("button", { name: "Save key" }));
      await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("anthropic-api-key", "sk-ant-abcdefgh"));
    });

    it("a real provider block: the card is absent — Your account is the only door", async () => {
      const s = providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "live" }]);
      getSetupStatusMock.mockResolvedValue(s);
      renderPage(baseMe(), s);
      await screen.findByText(CONNECTIONS.SUMMARY_READY);
      expect(screen.queryByRole("heading", { name: "Your model key" })).not.toBeInTheDocument();
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
      const link = screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN });
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

// M-6 (D5) — the two demo sections member-getting-started.tsx adds, gated by
// walkableDemos (setup/steps.ts) the same way the funnel's PHASES walk used
// to. #850's redaction (internal/api/setup.go's redactSetupStatusForUser)
// zeroes secrets.present and providers for a non-operator, so these cases
// exercise the exact shape a real SSO user's browser receives, not the
// admin-token D1 fixture the demos.spec/secrets-demos.spec e2e run as.
