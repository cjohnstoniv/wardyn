/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What a launch attempt does once it reaches the wire — the member's drive
// bit, Preflight/Launch body parity, the Agent picker roster, the
// unparseable barrier-class hint, a launch's own warnings, and the
// credential/git-credential refusal doors — split out of
// new-run-screen.test.tsx, which was already at the check-file-size.sh
// ceiling. Its own copy of the screen's mock harness: NewRunScreen imports
// setup/policies/runs/workspaces/capabilities regardless of which suite
// mounts it.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => Promise.resolve([]),
    createPolicy: vi.fn(),
    getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a),
  },
}));
// The screen navigates on launch and on Esc — spy on it rather than asserting
// against a URL bar the MemoryRouter does not render.
const navigateMock = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => navigateMock };
});
const preflightRunMock = vi.fn();
const createRunMock = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    isCredentialRefusal: actual.isCredentialRefusal,
    isGitCredentialRefusal: actual.isGitCredentialRefusal,
    runs: {
      createRun: (...a: unknown[]) => createRunMock(...a),
      listRuns: () => Promise.resolve([]),
      preflightRun: (...a: unknown[]) => preflightRunMock(...a),
      // The PolicyPanel's SafetyMeter debounces a grade of the current spec; stub
      // it so the panel's meter has a resolvable call instead of hitting the net.
      gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
    },
  };
});
// The REAL rail with its props recorded — this file pins only what the screen hands it.
const railProps: Array<{ launch: { credentialRefused: boolean; genericFailure: boolean; onOpenRun: (() => void) | null } }> = [];
vi.mock("./new-run-rail", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./new-run-rail")>();
  return {
    ...actual,
    RunRail: (props: Parameters<typeof actual.RunRail>[0]) => {
      railProps.push(props);
      return actual.RunRail(props);
    },
  };
});
// The connect popup + poll (#386) — mocked so the launch-door tests below
// drive the screen's own dialog wiring without a real window.
const adoConnectMock = vi.fn();
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({
    connecting: false,
    connect: adoConnectMock,
    connectFallback: adoConnectMock,
    cancel: vi.fn(),
    blockedUrl: null,
  }),
}));
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a) },
}));
// A MEMBER's console asks GET /me/capabilities (useMyCapabilities is gated on
// !operator), so the member cases below need that hook answered. Only the hook
// is replaced — capabilityAllowed stays the real matcher, since it is what
// decides whether the selected workspace is annotated as ungranted.
const myCapabilitiesMock = vi.fn();
vi.mock("../../../lib/capabilities", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/capabilities")>(
    "../../../lib/capabilities",
  );
  return { ...actual, useMyCapabilities: (...a: unknown[]) => myCapabilitiesMock(...a) };
});

import { NewRunScreen } from "./new-run-screen";
import type { Me } from "../../../lib/api/health";
import { baseMe, baseMeDrive, baseStatus } from "../../../lib/test-fixtures";
import { OperatorProvider } from "../../wardyn/operator-context";
import { AUTONOMY_RAIL } from "../../../lib/governance-copy";
import { DRIVE_MEMBER as DM } from "../../../lib/user-drives-copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { HttpError } from "../../../lib/api/core";
import { ADO } from "../../../lib/ado-entra-copy";
import { RUN } from "../../wardyn/copy";

const user = userEvent.setup({ pointerEventsCheck: 0 });

// The Workspace card's drive block reads the shell's ONE GET /me off the
// context (operator-context's UserDriveContext), not a fetch of its own — so a
// case states its /me body here, exactly as app-shell hands it down. The
// default carries NEITHER /me drive bit: no allocation and no door, which is
// what every case below except the drive ones is, and which must render as
// today's card.
//
// `operator` stays TRUE — the context's own fail-open default, which this suite
// has always run on. It gates useMyCapabilities, not the drive.
function renderScreen(me: Me = baseMe()) {
  return render(
    <MemoryRouter>
      <OperatorProvider
        operator
        userDrive={me.user_drive}
        userDriveDeniedByProfile={me.user_drive_denied_by_profile}
      >
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

// The same screen for the tier that actually MOUNTS a drive. `operator={false}`
// is not cosmetic here: it is what turns useMyCapabilities on, so the member
// path runs code no admin case above reaches.
function renderAsMember(me: Me = baseMe()) {
  return render(
    <MemoryRouter>
      <OperatorProvider
        operator={false}
        securityOperator={false}
        userDrive={me.user_drive}
        userDriveDeniedByProfile={me.user_drive_denied_by_profile}
      >
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  listWorkspacesMock.mockReset().mockResolvedValue([]);
  preflightRunMock.mockReset();
  navigateMock.mockReset();
  createRunMock.mockReset().mockResolvedValue({ id: "run_1" });
  getDefaultPolicyMock.mockReset().mockResolvedValue({ min_confinement_class: "CC1" });
  // null is what the real hook returns for an admin (exempt) and for a set that
  // has not loaded — the answer every admin case above has always run on.
  myCapabilitiesMock.mockReset().mockReturnValue(null);
});

// The card's own render matrix lives in workspace-card.test.tsx. What is
// pinned HERE is the seam between them: that this screen reads /me, hands both
// bits to the card, and puts what the member ticked on the wire. A block that
// renders perfectly from props it is never given is the failure a component
// test cannot see.
describe("NewRunScreen — the member's drive reaches the wire", () => {
  const withDrive = baseMe({ user_drive: baseMeDrive() });

  it("sends drive {enabled, read_only} for a ticked box and a narrowed mount", async () => {
    renderScreen(withDrive);
    await user.click(await screen.findByLabelText(DM.NR_CHECKBOX));
    await user.click(screen.getByLabelText(DM.NR_READONLY_TOGGLE));
    await user.type(screen.getByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].drive).toEqual({ enabled: true, read_only: true });
  });

  it("sends no drive at all when the member never ticks it", async () => {
    renderScreen(withDrive);
    // The offer is on screen — this is a declined offer, not a missing one.
    expect(await screen.findByLabelText(DM.NR_CHECKBOX)).toBeInTheDocument();
    await user.type(screen.getByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].drive).toBeUndefined();
  });

  // §5 #4: every string a member is refused with is composed SERVER-SIDE and
  // rendered verbatim. The screen's existing launch-error path already does
  // that for every 4xx, so the drive refusals need no rendering of their own —
  // and this pins that they get none: a console that re-composed the 422 would
  // tell the member a different story than the audit row does.
  it("renders a drive refusal verbatim, off the wire, with nothing added", async () => {
    createRunMock.mockRejectedValue(new Error(DM.REFUSED_WRITABLE));
    renderScreen(withDrive);
    await user.click(await screen.findByLabelText(DM.NR_CHECKBOX));
    await user.type(screen.getByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    expect(await screen.findByText(DM.REFUSED_WRITABLE)).toBeInTheDocument();
  });

  // The door bit is a SIBLING of the allocation on the wire, and this is the
  // state that proves why: no drive to name, and a refusal that must still be
  // drawn before the member spends a launch discovering it.
  it("draws the door with no allocation at all", async () => {
    renderScreen(baseMe({ user_drive_denied_by_profile: "Greenfield contractors" }));
    expect(
      await screen.findByText(DM.NR_DENIED("Greenfield contractors")),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText(DM.NR_CHECKBOX)).toBeNull();
  });

  // Every case above runs on the context's fail-OPEN operator default, so the
  // MOUNTING tier needs its own coverage — a member's console additionally
  // resolves GET /me/capabilities. That flag gates real code
  // (useMyCapabilities' effect, and capabilityAllowed over a non-null set in
  // the same card the drive block lives in), and a drive offer that only
  // works under the admin default would ship green.
  it("offers the drive to a MEMBER — the tier that actually mounts one", async () => {
    myCapabilitiesMock.mockReturnValue({
      grants: [],
      enforcement: { workspace: false },
      session_groups: [],
      groups_snapshot_stale: false,
    });
    renderAsMember(withDrive);

    expect(await screen.findByLabelText(DM.NR_CHECKBOX)).toBeInTheDocument();
    await user.click(screen.getByLabelText(DM.NR_CHECKBOX));
    // The narrowing rides the MOUNT: it is offered once this run has a mount
    // to narrow, never over an unticked checkbox that sends no `drive` at all.
    expect(screen.getByLabelText(DM.NR_READONLY_TOGGLE)).toBeInTheDocument();
    await user.type(screen.getByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    // read_only is ABSENT, not false: the member did not narrow this run, and
    // the wizard sends the narrowing only when it is asked for.
    expect(createRunMock.mock.calls[0][0].drive).toEqual({ enabled: true });
  });
});

// "Review predicts launch", the SCREEN half.
//
// runs.wire.fields.test.ts proves runWireBody maps one input to byte-identical
// create/preflight bodies. Nothing proved the SCREEN hands both doors the same
// input: the four Preflight cases above assert only on the RESPONSE, and never
// read preflightRunMock's argument. Mutating the screen to preflight
// `{...buildRunInput(), drive: undefined, workspaces: undefined,
// integration_id: undefined}` — Review predicting a driveless, workspace-less,
// credential-less launch for a run that will carry all three — left the whole
// 1823-test suite green.
describe("NewRunScreen — Preflight sends the body Launch sends", () => {
  it("preflight's argument deep-equals createRun's, from one unchanged state", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [],
      enforced_confinement_class: "CC2",
      overall_risk: "low",
      warnings: [],
    });
    renderScreen(baseMe({ user_drive: baseMeDrive() }));
    await user.click(await screen.findByLabelText(DM.NR_CHECKBOX));
    await user.type(screen.getByLabelText("Title"), "Refund flow");

    await user.click(screen.getByRole("button", { name: /^Preflight$/ }));
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());

    // Nothing is touched between the two clicks: Review is a dry run of THIS
    // request, so any divergence is the prediction lying about the launch.
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());

    expect(preflightRunMock.mock.calls[0][0]).toEqual(createRunMock.mock.calls[0][0]);
  });

  it("...and the three fields a silent drop is invisible in survive on BOTH", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [],
      enforced_confinement_class: "CC2",
      overall_risk: "low",
      warnings: [],
    });
    renderScreen(baseMe({ user_drive: baseMeDrive() }));
    await user.click(await screen.findByLabelText(DM.NR_CHECKBOX));
    await user.type(screen.getByLabelText("Title"), "Refund flow");

    await user.click(screen.getByRole("button", { name: /^Preflight$/ }));
    await waitFor(() => expect(preflightRunMock).toHaveBeenCalled());
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());

    const flown = preflightRunMock.mock.calls[0][0];
    const launched = createRunMock.mock.calls[0][0];
    // The member ticked their drive: it must be on the predicted body too, or
    // Review answers for a run that is not the one about to start.
    expect(flown.drive).toEqual({ enabled: true });
    expect(flown.drive).toEqual(launched.drive);
    expect(flown.workspaces).toEqual(launched.workspaces);
    expect(flown.integration_id).toEqual(launched.integration_id);
  });
});

// §5c.4 — the Agent picker reads SetupStatus.harnesses. Roster-unknown (the
// fetch never landed, or the field is absent — an older daemon) keeps today's
// two literals and marks nothing unavailable; once the roster arrives, every
// row renders, a disabled one WITH its reason, never hidden.
describe("NewRunScreen — the Agent picker reads the harness roster", () => {
  it("roster-unknown keeps today's two catalog literals, nothing marked unavailable", async () => {
    renderScreen();
    await user.click(await screen.findByRole("combobox", { name: "Agent" }));
    expect(await screen.findByRole("option", { name: "Claude Code" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Codex CLI" })).toBeInTheDocument();
    expect(screen.queryByText(AGENTS.UNAVAILABLE)).not.toBeInTheDocument();
  });

  it("a roster with a disabled row renders it disabled with AGENTS.UNAVAILABLE, never hidden", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        harnesses: [
          { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true },
          { id: "codex-cli", display: "Codex CLI", has_gateway: true, has_login: false, enabled: false },
        ],
      }),
    );
    renderScreen();
    await user.click(await screen.findByRole("combobox", { name: "Agent" }));
    const codex = await screen.findByRole("option", { name: /Codex CLI/ });
    expect(codex).toHaveAttribute("aria-disabled", "true");
    expect(codex).toHaveTextContent(AGENTS.UNAVAILABLE);
  });
});

// C5's one real trap: a class the JSON parses but this build can't spell
// silently sets no floor. Say so under the field.
describe("NewRunScreen — the unparseable barrier-class hint", () => {
  it("renders when the JSON parses but min_confinement_class names no real class", async () => {
    renderScreen();
    const textarea = await screen.findByLabelText("Spec (JSON)");
    fireEvent.change(textarea, {
      target: { value: JSON.stringify({ min_confinement_class: "vault", allowed_domains: [] }, null, 2) },
    });
    expect(await screen.findByText(AGENTS.FLOOR_UNPARSEABLE("vault"))).toBeInTheDocument();
  });

  it("says nothing when the field is simply absent", async () => {
    renderScreen();
    const textarea = await screen.findByLabelText("Spec (JSON)");
    fireEvent.change(textarea, { target: { value: JSON.stringify({ allowed_domains: [] }, null, 2) } });
    expect(screen.queryByText(/isn't a barrier class/)).not.toBeInTheDocument();
  });
});

// §5c.8 — a run that launched WITH advisories.
//
// The screen must hold, not navigate on a timer: a timer would race every
// other way off the screen (Esc and the ghost "Runs" button both land on
// /runs, and a timer would then yank the member to the run), and it would
// give a multi-line advisory a fixed beat nobody finishes reading. The
// warnings stay listed and the primary button becomes "Open run", which is
// the only thing that navigates.
describe("NewRunScreen — the 201's warnings hold the screen, no timer", () => {
  async function launchWith(warnings?: string[]) {
    createRunMock.mockResolvedValue({ id: "run_9", warnings });
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
  }

  it("navigates immediately when the 201 carries no warnings", async () => {
    await launchWith();
    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith("/runs/run_9"));
    expect(screen.queryByRole("button", { name: AGENTS.OPEN_RUN_CTA })).toBeNull();
  });

  it("lists the warnings and navigates NOWHERE until Open run is clicked", async () => {
    await launchWith([
      "egress_host: internal.example.com was dropped — not granted to you",
      "secret: DEPLOY_KEY was dropped — not granted to you",
    ]);

    expect(await screen.findByText(AGENTS.LAUNCH_WARNING_TITLE)).toBeInTheDocument();
    expect(
      screen.getByText("egress_host: internal.example.com was dropped — not granted to you"),
    ).toBeInTheDocument();
    expect(screen.getByText("secret: DEPLOY_KEY was dropped — not granted to you")).toBeInTheDocument();
    // Launch is gone: the run is launched, and re-firing it is not the next move.
    expect(screen.queryByRole("button", { name: /Launch run/ })).toBeNull();
    expect(navigateMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: AGENTS.OPEN_RUN_CTA }));
    expect(navigateMock).toHaveBeenCalledWith("/runs/run_9");
  });

  // The load-bearing check: NOTHING is pending. Real timers here would only
  // prove nothing fired within an arbitrary window, not that nothing was
  // scheduled.
  it("leaves no pending navigation behind — the member backs out and stays out", async () => {
    await launchWith(["secret: DEPLOY_KEY was dropped — not granted to you"]);
    await screen.findByText(AGENTS.LAUNCH_WARNING_TITLE);

    vi.useFakeTimers();
    try {
      // The ghost "Runs" button is a way out; nothing schedules a competing
      // navigation.
      fireEvent.click(screen.getByRole("button", { name: "Runs" }));
      expect(navigateMock).toHaveBeenCalledWith("/runs");
      navigateMock.mockReset();
      // Ten seconds — long enough that any stray scheduled navigation would
      // have fired.
      vi.advanceTimersByTime(10_000);
      expect(navigateMock).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});

// 0.7.6 field report: the server's credential refusal (422, reason model_credential) reaches
// the rail as `credentialRefused` — what opens the door — and no other failure does.
describe("NewRunScreen — the server's credential refusal reaches the rail", () => {
  const lastRail = () => railProps[railProps.length - 1];
  async function titled() {
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    return screen.getByRole("button", { name: /Launch run/ });
  }

  it("a 422 carrying reason model_credential sets credentialRefused, keeps the sentence, and the next Launch clears it", async () => {
    createRunMock.mockRejectedValueOnce(new HttpError(422, "sign in to AWS first", "model_credential"));
    const launch = await titled();
    await user.click(launch);
    expect(await screen.findByText("sign in to AWS first")).toBeInTheDocument();
    expect(lastRail().launch.credentialRefused).toBe(true);

    createRunMock.mockResolvedValueOnce({ id: "run_2" });
    await user.click(launch);
    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith("/runs/run_2"));
    expect(lastRail().launch.credentialRefused).toBe(false);
  });

  it("a 422 without a reason — a policy error — never sets it", async () => {
    createRunMock.mockRejectedValueOnce(new HttpError(422, 'workspaces[0]: unknown secret "prod-db"'));
    const launch = await titled();
    await user.click(launch);
    expect(await screen.findByText('workspaces[0]: unknown secret "prod-db"')).toBeInTheDocument();
    expect(lastRail().launch.credentialRefused).toBe(false);
  });
});

// SF-26 (see #214's own genericFailure doc comment): the ONE case
// createRun's caller cannot tell a server-composed refusal from a connection
// that died with nothing to say — an HttpError whose message is genuinely ""
// (a bodiless 5xx; main's own invariant is "no refusal can become a failed
// run", so a REAL refusal always carries a body, and this is what a response
// with none actually decodes to — see errEnvelope's res.statusText fallback,
// which HTTP/2 leaves empty). Before this fix the card told the member a run
// WAS created; the honest fact is that Wardyn never said either way.
describe("NewRunScreen — the launch failure card never claims a run was created (SF-26)", () => {
  const lastRail = () => railProps[railProps.length - 1];

  it("createRun rejecting with no message at all never claims the run was created, and offers no Open run", async () => {
    createRunMock.mockRejectedValueOnce(new HttpError(502, ""));
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));

    expect(await screen.findByText(RUN.LAUNCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(/was created/i)).toBeNull();
    expect(lastRail().launch.genericFailure).toBe(true);
    expect(lastRail().launch.onOpenRun).toBeNull();
    expect(navigateMock).not.toHaveBeenCalled();
  });
});

// Finding 2 (#339 review): the server derives a hold (runs_autonomy.go's
// autonomyDerive) at L1 when the run is non-interactive, the agent has a
// hold lane (claude-code) and the request did NOT already ask for hold. The
// rail's note used to render in the OPPOSITE case — only when hold was
// already picked, which is exactly when nothing was derived.
describe("NewRunScreen — the derived-hold note follows the server's own derivation case", () => {
  function mockPreflightAtL1() {
    preflightRunMock.mockResolvedValue({
      setup_items: [],
      enforced_confinement_class: "CC1",
      overall_risk: "low",
      warnings: [],
      autonomy: {
        level: "L1",
        posture: { egress: "sealed", secrets: "powerful", confinement: "CC1" },
        bound_by: ["secrets_powerful"],
      },
    });
  }

  it("auto chosen at L1, non-interactive: the note shows", async () => {
    mockPreflightAtL1();
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    // toolApprovals defaults to "auto" — never touched.
    await user.click(screen.getByRole("button", { name: /^Preflight$/ }));
    expect(await screen.findByText(AUTONOMY_RAIL.DERIVED_HOLD_NOTE)).toBeInTheDocument();
  });

  it("hold chosen: the note does not claim a derivation", async () => {
    mockPreflightAtL1();
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    await user.click(screen.getByRole("radio", { name: /^Hold in Wardyn/ }));
    await user.click(screen.getByRole("button", { name: /^Preflight$/ }));
    await screen.findByTestId("preflight-result");
    expect(screen.queryByText(AUTONOMY_RAIL.DERIVED_HOLD_NOTE)).toBeNull();
  });

  it("interactive: no note, whatever tool approvals would hold", async () => {
    mockPreflightAtL1();
    renderScreen();
    // Interactive is the default (initialWizardState) — left untouched.
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("button", { name: /^Preflight$/ }));
    await screen.findByTestId("preflight-result");
    expect(screen.queryByText(AUTONOMY_RAIL.DERIVED_HOLD_NOTE)).toBeNull();
  });
});

// #386's launch door: a 422 carrying reason git_credential opens the Connect
// Azure DevOps dialog. Review finding F8: connecting does NOT relaunch —
// RELAUNCH_TOAST_BODY's own words are "launch when you're ready", so the
// form stays exactly as it stood and the person presses Launch themselves.
describe("NewRunScreen — the git_credential refusal opens the Connect Azure DevOps dialog", () => {
  async function titled() {
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    return screen.getByRole("button", { name: /Launch run/ });
  }

  beforeEach(() => adoConnectMock.mockReset());

  it("opens the dialog automatically, with no click on it", async () => {
    createRunMock.mockRejectedValueOnce(new HttpError(422, "you are not connected to Azure DevOps", "git_credential"));
    const launch = await titled();
    await user.click(launch);
    expect(await screen.findByText("you are not connected to Azure DevOps")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeInTheDocument();
  });

  // Review finding F1: the org comes from the 422 body itself, so the dialog
  // names it even with NO preflight verdict ever having run (this screen
  // fires preflight on a debounce; a fast Launch click can beat it there).
  it("F1: names the org from the 422 body, with no preflight verdict having run", async () => {
    createRunMock.mockRejectedValueOnce(
      new HttpError(422, "git_credential: you are not connected to Azure DevOps — connect and start the run again", "git_credential", "https://dev.azure.com/contoso"),
    );
    const launch = await titled();
    await user.click(launch);
    await screen.findByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE });
    expect(screen.getByText(ADO.LAUNCH_DIALOG_BODY("https://dev.azure.com/contoso"))).toBeInTheDocument();
  });

  it("F8: confirming connects and closes the dialog, but never relaunches — the person presses Launch themselves", async () => {
    createRunMock.mockRejectedValueOnce(new HttpError(422, "not connected", "git_credential"));
    adoConnectMock.mockResolvedValueOnce(true);
    const launch = await titled();
    await user.click(launch);
    await screen.findByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE });
    await user.click(screen.getByRole("button", { name: ADO.CONNECT_CTA }));
    expect(adoConnectMock).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.queryByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeNull());
    // No second createRun call, no navigation — the form stays as it stood.
    expect(createRunMock).toHaveBeenCalledTimes(1);
    expect(navigateMock).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled();
  });

  it("a popup that closes without connecting closes the dialog and launches nothing", async () => {
    createRunMock.mockRejectedValueOnce(new HttpError(422, "not connected", "git_credential"));
    adoConnectMock.mockResolvedValueOnce(false);
    const launch = await titled();
    await user.click(launch);
    await screen.findByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE });
    await user.click(screen.getByRole("button", { name: ADO.CONNECT_CTA }));
    expect(adoConnectMock).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.queryByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeNull());
    expect(createRunMock).toHaveBeenCalledTimes(1); // the original refused attempt only
  });

  it("Cancel closes the dialog without ever calling connect", async () => {
    createRunMock.mockRejectedValueOnce(new HttpError(422, "not connected", "git_credential"));
    const launch = await titled();
    await user.click(launch);
    await screen.findByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE });
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(adoConnectMock).not.toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeNull();
  });
});
