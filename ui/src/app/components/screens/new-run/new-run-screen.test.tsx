/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The rail's honesty rules, pinned where the SetupStatus is controlled. e2e
// cannot assert these: whether a model provider exists there depends on whether
// the machine running the suite has a logged-in Claude CLI, which setupProviders
// detects — so the same assertion passes on a laptop and fails on CI.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
// getDefaultPolicy names the caller's governance profile for the rail's ceiling
// section. The default answer carries no governance_profile_name — an
// UNASSIGNED caller, which is what every case below except the ceiling one is.
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
const railProps: Array<{ launch: { credentialRefused: boolean } }> = [];
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
import { GOVERNANCE as GOV, MEMBER, AUTONOMY_RAIL } from "../../../lib/governance-copy";
import { DRIVE_MEMBER as DM } from "../../../lib/user-drives-copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { HttpError } from "../../../lib/api/core";
import { ADO } from "../../../lib/ado-entra-copy";

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

// Regression: useWorkspaceList does NOT fetch on mount — each caller loads it
// itself. This screen didn't, so its only fetch was the Add-workspace dialog's
// onCreated callback, and the Workspace select offered nothing but "Ephemeral
// scratch". A workspace onboarded in Getting started or on the Workspaces
// screen could not be attached to a run at all: the only way into the list was
// to re-add it from this page, in this session. Caught by the demo recording
// driver, which onboards a workspace in one act and attaches it in the next.
describe("NewRunScreen — an already-onboarded workspace is attachable", () => {
  it("loads the workspace list on mount", async () => {
    renderScreen();
    await waitFor(() => expect(listWorkspacesMock).toHaveBeenCalled());
  });
});

// §7.6's first display moment. The name comes from GET /policies/default's
// governance_profile_name — the resolver's own answer for THIS caller — so the
// line names the profile that actually binds the run rather than inferring one
// from the spec.
describe("NewRunScreen — the rail names the governance ceiling", () => {
  it("renders MEMBER.CEILING_PROFILE when a profile is assigned", async () => {
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC1",
      governance_profile_name: "walled",
    });
    renderScreen();
    expect(await screen.findByText(MEMBER.CEILING_PROFILE("walled"))).toBeInTheDocument();
    expect(screen.getByText(GOV.CEILING_TITLE)).toBeInTheDocument();
  });

  // No assignment, no section — the absent-row doctrine, so an unassigned
  // member's rail is byte-for-byte what it was before this feature existed.
  it("renders nothing at all for an unassigned caller", async () => {
    renderScreen();
    await screen.findByText(/No model provider is connected/);
    expect(screen.queryByText(GOV.CEILING_TITLE)).not.toBeInTheDocument();
  });

  // A read that failed is UNKNOWN, never "no ceiling": naming none would be a
  // claim manufactured from an absence.
  it("names no ceiling when the read fails", async () => {
    getDefaultPolicyMock.mockRejectedValue(new Error("boom"));
    renderScreen();
    await screen.findByText(/No model provider is connected/);
    expect(screen.queryByText(GOV.CEILING_TITLE)).not.toBeInTheDocument();
  });
});

describe("NewRunScreen — the rail tells the truth about model access", () => {
  // An agent run with no provider launches and then fails its first model call.
  // Saying so at launch time is the difference between a warning and a support
  // ticket.
  it("warns when an AGENT run has no model provider", async () => {
    renderScreen();
    expect(await screen.findByText(/No model provider is connected/)).toBeInTheDocument();
  });

  it("says nothing when a provider IS connected", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }),
    );
    renderScreen();
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    expect(screen.queryByText(/No model provider is connected/)).not.toBeInTheDocument();
  });

  // A shell command needs no model at all — warning there would be noise.
  it("says nothing for a shell command, which needs no model", async () => {
    renderScreen();
    await screen.findByText(/No model provider is connected/);
    await user.click(screen.getByRole("radio", { name: "Shell command" }));
    expect(screen.queryByText(/No model provider is connected/)).not.toBeInTheDocument();
  });

  // getSetupStatus resolves a synthetic fallback rather than rejecting, so an
  // unknown answer must stay unknown — never an accusation on a blip.
  it("stays silent while the answer is unknown", async () => {
    // A deferred, not a never-settling promise: an unresolved fetch left
    // hanging times out vitest's teardown hook rather than the assertion.
    let settle: (v: unknown) => void = () => {};
    getSetupStatusMock.mockReturnValue(new Promise((r) => (settle = r)));
    renderScreen();
    expect(screen.queryByText(/No model provider is connected/)).not.toBeInTheDocument();
    settle(baseStatus());
    await waitFor(() => expect(screen.getByText(/No model provider is connected/)).toBeInTheDocument());
  });

  // F6-F3 (site 1) — lib/api/setup.ts's READY_FALLBACK resolves on ANY
  // non-401 failure (endpoint missing, network error, …), not just "no model
  // provider". `hasLlmPath` would read it as a genuine empty answer, so a
  // daemon that never answered would get the SAME accusation as a truly bare
  // host.
  it("does not warn on a synthetic READY_FALLBACK (unreachable) answer", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ unreachable: true }));
    renderScreen();
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    expect(screen.queryByText(/No model provider is connected/)).not.toBeInTheDocument();
  });
});

// NewRunScreen — the saved-policy lane (F2-F1/F2-F2/F2-F4/F2-F5) lives in its
// own file (new-run-screen-saved-policy.test.tsx): this file was already at
// the check-file-size.sh ceiling.

// The Policy panel authors the spec; the run's own SELECTIONS are unioned in
// after the parse and named on screen. The governance claim the retired
// host-count tests made — count the UNION, not the sum — now lives here: what
// the JSON already carries must never be re-announced as something this screen
// added, and what only the selections know must never be silently merged.
describe("NewRunScreen — the additions line counts the union, not the sum", () => {
  // A grant whose injection host is already on the allowlist adds NOTHING. The
  // Minimal template is exactly that shape (api.anthropic.com, allowed), so a
  // fresh screen has nothing to announce.
  it("says nothing when the JSON already carries every implied host", async () => {
    renderScreen();
    await screen.findByLabelText(/Spec \(JSON\)/);
    expect(screen.queryByTestId("run-spec-additions")).not.toBeInTheDocument();
  });

  // The trap the merge rules exist for: proxy credential injection only rewrites
  // requests whose host is ALREADY on allowed_domains, and that holds under
  // allow_all_egress too. A hand-written api_key grant therefore needs its exact
  // host pinned — derived from the merged grant UNION, not from wizard state,
  // which knows nothing about a grant the operator typed.
  it("pins a hand-written api_key grant's host, allow-all included", async () => {
    renderScreen();
    const box = await screen.findByLabelText(/Spec \(JSON\)/);
    await user.clear(box);
    fireEvent.change(box, {
      target: {
        value: JSON.stringify({
          allowed_domains: [],
          allow_all_egress: true,
          first_use_approval: "always_deny",
          min_confinement_class: "CC1",
          eligible_grants: [
            {
              kind: "api_key",
              scope: { host: "llm.acme.internal", header: "x-api-key", secret_name: "acme-key" },
              requires_approval: false,
            },
          ],
        }),
      },
    });

    const added = await screen.findByTestId("run-spec-additions");
    expect(within(added).getByText("llm.acme.internal")).toBeInTheDocument();
  });
});

// The form's fields FOLLOW the run mode. A single Task box for every run,
// including interactive ones, would let an operator type a prompt the server
// ignores entirely for an interactive run. Task rides as an interactive run's
// optional boot seed (Part A1), but it is still never the field literally
// labeled "Task" — that label stays batch-only, and an interactive run gets
// its own "Initial prompt" / "Startup command" field instead
// (new-run-screen.tsx's isInteractive branch).
describe("NewRunScreen — the form matches the run mode", () => {
  it("asks an interactive run what to start with, not for a task", async () => {
    renderScreen();
    // Interactive is the default (initialWizardState), so this is the state the
    // screen opens in.
    expect(await screen.findByRole("radiogroup", { name: "Start with" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Task")).not.toBeInTheDocument();
  });

  // …and the DEFAULT of that radiogroup is the AGENT, not the shell. Pinned
  // because a live walk now depends on it: ui/e2e/live/sso-member.spec.ts
  // launches a run expecting claude-code to make a model call at boot, and it
  // does not touch this control. If the default ever flips to "Terminal" that
  // run comes up as an idle shell, calls nothing, and the walk fails 180
  // seconds later as a timeout on an assertion about AWS — which is the most
  // expensive possible place to discover a default changed.
  it("defaults an interactive run to launching the agent, not a bare shell", async () => {
    renderScreen();
    const group = await screen.findByRole("radiogroup", { name: "Start with" });
    const agent = within(group).getByRole("radio", { name: /launch it in the workspace$/ });
    expect(agent).toBeChecked();
    expect(within(group).getByRole("radio", { name: /^Terminal/ })).not.toBeChecked();
    // The agent arm is the one carrying the boot prompt (id nr-seed), which is
    // what the walk fills.
    expect(screen.getByLabelText(/^Initial prompt/)).toBeInTheDocument();
  });

  it("asks an autonomous run for a task, and drops the startup choice", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    expect(screen.getByLabelText("Task")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "Start with" })).not.toBeInTheDocument();
  });

  // A shell command is unattended by definition. Offering "Interactive" for one
  // would produce a run that silently never executes the command (the server
  // drops task_mode for an interactive run).
  it("hides the run mode for a shell command, which is always unattended", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: "Shell command" }));
    expect(screen.getByLabelText("Command")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "Run mode" })).not.toBeInTheDocument();
  });
});

// D33's pin MOVED to policy-panel.test.tsx ("first_use_approval states what
// each mode does, and bounds the hold"). The Confined card whose body it pinned
// died with the custom form — this screen now authors the spec through the
// shared PolicyPanel, whose helper rail carries the canon string instead.

// Before this, the screen had NO client-side validation at all: an empty form
// launched, and the server's answer arrived after the fact.
describe("NewRunScreen — Launch says what it is waiting for", () => {
  it("is disabled without a title, and says so", async () => {
    renderScreen();
    const launch = await screen.findByRole("button", { name: /Launch run/ });
    expect(launch).toBeDisabled();
    expect(screen.getByText("Give this run a title.")).toBeInTheDocument();

    await user.type(screen.getByLabelText("Title"), "Refund flow");
    expect(launch).toBeEnabled();
  });

  it("still waits for the task on an autonomous run", async () => {
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("radio", { name: /^Autonomous/ }));
    const launch = screen.getByRole("button", { name: /Launch run/ });
    expect(launch).toBeDisabled();
    expect(screen.getByText(/needs a task to perform/)).toBeInTheDocument();

    await user.type(screen.getByLabelText("Task"), "fix the flaky test");
    expect(launch).toBeEnabled();
  });
});

// Phase 3: preflightRun wired into a real caller. Same request payload as
// Launch (buildRunInput), rendered inline next to the actions instead of
// blocking them.
describe("NewRunScreen — Preflight", () => {
  async function readyScreen() {
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    return screen.getByRole("button", { name: /^Preflight$/ });
  }

  it("renders the warnings, risk grade, and enforced confinement class on success", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [],
      enforced_confinement_class: "CC2",
      overall_risk: "medium",
      warnings: ["Egress narrowed to api.anthropic.com by member policy."],
    });
    const button = await readyScreen();
    await user.click(button);

    const result = await screen.findByTestId("preflight-result");
    expect(within(result).getByText("Egress narrowed to api.anthropic.com by member policy.")).toBeInTheDocument();
    expect(within(result).getByText("Medium")).toBeInTheDocument();
    expect(within(result).getByText("Wall")).toBeInTheDocument();
  });

  it("collapses to a quiet line when there are no warnings", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [],
      enforced_confinement_class: "CC1",
      overall_risk: "low",
      warnings: [],
    });
    const button = await readyScreen();
    await user.click(button);

    const result = await screen.findByTestId("preflight-result");
    expect(within(result).getByText(AGENTS.EFFECTIVE_NONE)).toBeInTheDocument();
    expect(within(result).getByText("Low")).toBeInTheDocument();
    expect(within(result).getByText("Fence")).toBeInTheDocument();
  });

  it("renders the server's field-path error verbatim on a 4xx", async () => {
    preflightRunMock.mockRejectedValue(new Error('workspaces[0]: unknown secret "prod-db"'));
    const button = await readyScreen();
    await user.click(button);

    expect(await screen.findByText('workspaces[0]: unknown secret "prod-db"')).toBeInTheDocument();
  });

  it("disables the button and shows a loading spinner while in flight", async () => {
    let resolve: (v: unknown) => void = () => {};
    preflightRunMock.mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );
    const button = await readyScreen();
    await user.click(button);

    expect(button).toBeDisabled();
    resolve({ setup_items: [], enforced_confinement_class: "CC1", warnings: [] });
    await waitFor(() => expect(button).toBeEnabled());
  });

  // The verdict sits directly above Launch, "the last thing read
  // before committing" — so it may only be shown while it is still a verdict
  // about the body Launch would send. It must not survive ANY edit: preflight
  // a title, change the run, and the graded-elsewhere badge would stay beside
  // the button while createRun ships something the verdict never saw.
  it("drops the verdict as soon as the run body changes — a stale grade is never rendered beside Launch", async () => {
    preflightRunMock.mockResolvedValue({
      setup_items: [],
      enforced_confinement_class: "CC1",
      overall_risk: "low",
      warnings: [],
    });
    const button = await readyScreen();
    await user.click(button);
    expect(await screen.findByTestId("preflight-result")).toBeInTheDocument();

    // Any field that reaches the wire body — the title is the cheapest one.
    await user.type(screen.getByLabelText("Title"), " v2 PROD");

    await waitFor(() => expect(screen.queryByTestId("preflight-result")).toBeNull());
  });

  it("drops a preflight ERROR on the same edit — it graded a body that no longer exists", async () => {
    preflightRunMock.mockRejectedValue(new Error('workspaces[0]: unknown secret "prod-db"'));
    const button = await readyScreen();
    await user.click(button);
    expect(await screen.findByText('workspaces[0]: unknown secret "prod-db"')).toBeInTheDocument();

    await user.type(screen.getByLabelText("Title"), " v2 PROD");

    await waitFor(() =>
      expect(screen.queryByText('workspaces[0]: unknown secret "prod-db"')).toBeNull(),
    );
  });
});

// M4/rulebook §8: the keyboard contract. Launching a run is consequential, so
// "which key commits it" is not a detail — no key commits it, and Esc leaves
// without taking the work with it.
describe("NewRunScreen — the keyboard contract", () => {
  it("opens with focus on Title, not on the back-out button", async () => {
    renderScreen();
    await waitFor(() => expect(screen.getByLabelText("Title")).toHaveFocus());
  });

  // The title input carries the <datalist> of known run titles, and choosing a
  // suggestion with Enter dispatches keydown Enter on the input (Chrome) — so
  // an Enter-to-launch binding here turned "pick Nightly audit off the list"
  // into "launch the run". Completion and commit cannot share a key.
  it("never launches on Enter from the title — that key belongs to the datalist", async () => {
    renderScreen();
    const title = await screen.findByLabelText("Title");
    await user.type(title, "Nightly audit");

    fireEvent.keyDown(title, { key: "Enter" });
    expect(createRunMock).not.toHaveBeenCalled();
  });

  it("never launches on Enter from a textarea — there it is a newline", async () => {
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("radio", { name: /^Autonomous/ }));
    const task = screen.getByLabelText("Task");
    await user.type(task, "fix the flaky test");

    fireEvent.keyDown(task, { key: "Enter" });
    expect(createRunMock).not.toHaveBeenCalled();
  });

  it("Esc backs out of an untouched form, and leaves a dirty one alone", async () => {
    renderScreen();
    await screen.findByLabelText("Title");

    fireEvent.keyDown(window, { key: "Escape" });
    expect(navigateMock).toHaveBeenCalledWith("/runs");

    navigateMock.mockReset();
    await user.type(screen.getByLabelText("Title"), "Refund flow");
    fireEvent.keyDown(window, { key: "Escape" });
    // Leaving is still one click on the ghost "Runs" button — it just does not
    // happen by accident with unsaved work on screen.
    expect(navigateMock).not.toHaveBeenCalled();
  });

  // Radix's DismissableLayer preventDefaults Escape on document CAPTURE and then
  // dismisses; the event still bubbles on to window. Without a defaultPrevented
  // guard, closing a Select or the Add-workspace dialog ALSO left the screen.
  it("ignores an Escape another layer already handled", async () => {
    renderScreen();
    const title = await screen.findByLabelText("Title");

    // Control: an unhandled Escape from inside the form does reach the window
    // listener — so the assertion below cannot pass for the wrong reason.
    fireEvent.keyDown(title, { key: "Escape" });
    expect(navigateMock).toHaveBeenCalledWith("/runs");

    navigateMock.mockReset();
    const dismiss = (e: Event) => e.preventDefault();
    document.addEventListener("keydown", dismiss, true);
    fireEvent.keyDown(title, { key: "Escape" });
    document.removeEventListener("keydown", dismiss, true);
    expect(navigateMock).not.toHaveBeenCalled();
  });

  // The dirty predicate must read the policy body too, not just
  // title/description/task/policy-id — otherwise a form whose ONLY work was
  // an authored policy body counts as untouched, and Esc throws the document
  // away without a word.
  it("counts an edited policy body as dirty on its own", async () => {
    renderScreen();
    const spec = (await screen.findByLabelText("Spec (JSON)")) as HTMLTextAreaElement;

    fireEvent.change(spec, {
      target: { value: '{"min_confinement_class": "CC1", "auto_stop_after_sec": 7200}' },
    });
    expect((screen.getByLabelText("Title") as HTMLInputElement).value).toBe("");

    fireEvent.keyDown(window, { key: "Escape" });
    expect(navigateMock).not.toHaveBeenCalled();
  });
});

// The one validation-error state. aria-invalid is what paints it — the Input
// primitive owns the ring, and this screen must never hand-paint one.
describe("NewRunScreen — the title's error state", () => {
  it("stays quiet until the operator has been in the field and left it empty", async () => {
    renderScreen();
    const title = await screen.findByLabelText("Title");
    expect(title).not.toHaveAttribute("aria-invalid");

    fireEvent.blur(title);
    await waitFor(() => expect(title).toHaveAttribute("aria-invalid", "true"));

    await user.type(title, "Refund flow");
    expect(title).not.toHaveAttribute("aria-invalid");
  });
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
