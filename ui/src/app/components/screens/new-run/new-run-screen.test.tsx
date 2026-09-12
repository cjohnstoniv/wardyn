/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The rail's honesty rules, pinned where the SetupStatus is controlled. e2e
// cannot assert these: whether a model provider exists there depends on whether
// the machine running the suite has a logged-in Claude CLI, which setupProviders
// detects — so the same assertion passes on a laptop and fails on CI.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
// Which barriers this host can BUILD. Mutable because the clone cases below
// need a host that has the tier the cloned run used — the default stays the
// CC1-only host every other case here has always run on.
let mockConfinementClasses: string[] = ["CC1"];
vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ confinement_classes: mockConfinementClasses }) },
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
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    createRun: (...a: unknown[]) => createRunMock(...a),
    listRuns: () => Promise.resolve([]),
    preflightRun: (...a: unknown[]) => preflightRunMock(...a),
    // The PolicyPanel's SafetyMeter debounces a grade of the current spec; stub
    // it so the panel's meter has a resolvable call instead of hitting the net.
    gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
  },
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
import { GOVERNANCE as GOV, MEMBER } from "../../../lib/governance-copy";
import { DRIVE_MEMBER as DM } from "../../../lib/user-drives-copy";
import { RUN } from "../../wardyn/copy";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { setDefaultCc } from "../../wardyn/default-confinement";
import { lsSet } from "../../../lib/storage";

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
  mockConfinementClasses = ["CC1"];
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
});

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

// Every successful parse re-reads the floor the document authors, and the Seg
// DISABLES every tier below it. A one-time up-clamp alone would re-open the
// below-floor 422 the moment the operator lowered the Seg afterwards.
describe("NewRunScreen — the barrier floor disables what it forbids", () => {
  it("names the floor as its own reason, separate from what the host can build", async () => {
    renderScreen();
    const box = await screen.findByLabelText(/Spec \(JSON\)/);
    fireEvent.change(box, {
      target: {
        value: JSON.stringify({
          allowed_domains: [],
          first_use_approval: "always_deny",
          min_confinement_class: "CC3",
        }),
      },
    });

    // The health mock reports CC1 only, so Wall/Vault are unavailable AND
    // below-floor — one reason each, never two — while Fence, which this host
    // builds fine, is disabled for the floor alone. Every tier disabled is
    // fail-closed on purpose; preflight and launch name the cause.
    expect(await screen.findByText(/Fence is below the policy's floor \(Vault\)/)).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Fence" })).toBeDisabled();
    expect(screen.getByText(/Wall isn't installed on this host/)).toBeInTheDocument();
    expect(screen.queryByText(/Wall is below the policy's floor/)).not.toBeInTheDocument();
  });
});

// The form's fields FOLLOW the run mode. This screen used to show one Task box
// for every run, including interactive ones — where the server used to ignore
// task entirely, so the operator typed a prompt nothing would ever read. Task
// now rides as an interactive run's optional boot seed (Part A1), but it is
// still never the field literally labeled "Task" — that label stays batch-only,
// and an interactive run gets its own "Initial prompt" / "Startup command"
// field instead (new-run-screen.tsx's isInteractive branch).
describe("NewRunScreen — the form matches the run mode", () => {
  it("asks an interactive run what to start with, not for a task", async () => {
    renderScreen();
    // Interactive is the default (initialWizardState), so this is the state the
    // screen opens in.
    expect(await screen.findByRole("radiogroup", { name: "Start with" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Task")).not.toBeInTheDocument();
  });

  it("asks an autonomous run for a task, and drops the startup choice", async () => {
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: /^Autonomous/ }));
    expect(screen.getByLabelText("Task")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "Start with" })).not.toBeInTheDocument();
  });

  // A shell command is unattended by definition. Offering "Interactive" for one
  // used to produce a run that silently never executed the command (the server
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
    expect(within(result).getByText("No adjustments.")).toBeInTheDocument();
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

  // R4-F090: the verdict sits directly above Launch, "the last thing read
  // before committing" — so it may only be shown while it is still a verdict
  // about the body Launch would send. It used to survive ANY edit: preflight a
  // title, change the run, and the graded-elsewhere badge stayed beside the
  // button while createRun shipped something the verdict never saw.
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

  // The predicate used to read title/description/task/policy-id only, so a form
  // whose ONLY work was an authored policy body counted as untouched — Esc threw
  // the document away without a word.
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

  // Every case above runs on the context's fail-OPEN operator default, so until
  // now the MOUNTING tier was never rendered in vitest at all — a member, whose
  // console additionally resolves GET /me/capabilities. That flag gates real
  // code (useMyCapabilities' effect, and capabilityAllowed over a non-null set
  // in the same card the drive block lives in), and a drive offer that only
  // survives the admin default would ship green.
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

// R4-F118 — "Review predicts launch", the SCREEN half.
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

// ═══════════════════════════════════════════════════════════════════════════
// B4b — a clone, MOUNTED.
//
// wizard-spec.test.ts proves runPrefill + initialWizardState compose the right
// state. It cannot prove the SCREEN keeps it: the mount-time barrier probe
// re-seeded confinementClass from the operator's persisted default a tick after
// the prefill applied — and moved pristineCc with it, so the overwrite did not
// even read as dirty. A CC3 run cloned on a CC1-default machine launched at CC1
// while the banner promised the barrier carried over. Silently weaker than the
// run it copies is the one direction a governance product must never err, and
// only a mounted test that reads the WIRE BODY can see it.
// ═══════════════════════════════════════════════════════════════════════════
describe("NewRunScreen — a cloned run reaches the wire as the run it cloned", () => {
  const prefill = {
    inlinePolicy: false,
    state: {
      title: "Migration 0062",
      task: "rerun the migration",
      confinementClass: "CC3" as const,
      toolApprovals: "hold" as const,
      mode: "batch" as const,
    },
  };

  // THE fixture that makes this test able to fail: the operator's persisted
  // default has to DISAGREE with the cloned run's barrier. With nothing
  // persisted, resolveDefaultCc falls through to the strongest tier the host
  // has — which is CC3 here, so the bug and the fix would agree and the case
  // would pass on both.
  beforeEach(() => setDefaultCc("CC1"));
  afterEach(() => lsSet("wardyn-default-confinement", null));

  function renderClone(state: unknown = { prefill }) {
    return render(
      <MemoryRouter initialEntries={[{ pathname: "/runs/new", state }]}>
        <OperatorProvider operator>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
  }

  it("launches at the SOURCE run's barrier, not the operator's persisted default", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderClone();
    // Wait for the barrier probe to settle — this is the effect that used to
    // overwrite the prefill, so asserting before it lands would pass on the bug.
    await screen.findByRole("button", { name: /Launch run/ });
    await waitFor(() =>
      expect(screen.getByRole("radio", { name: "Vault" })).toHaveAttribute("aria-checked", "true"),
    );

    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].confinement_class).toBe("CC3");
  });

  it("says what carried over and that the ceiling re-applies at launch", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderClone();
    expect(await screen.findByText(RUN.CLONE_NOTE)).toBeInTheDocument();
    expect(screen.getByText(RUN.CLONE_CEILING_NOTE)).toBeInTheDocument();
    // Not an inline-policy clone, so no ceiling sentence about a lost policy.
    expect(screen.queryByText(RUN.CLONE_INLINE_POLICY_CEILING)).toBeNull();
  });

  it("names the inline policy it could not carry", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderClone({ prefill: { ...prefill, inlinePolicy: true } });
    expect(await screen.findByText(RUN.CLONE_INLINE_POLICY_CEILING)).toBeInTheDocument();
  });

  // The fallback, and it is not silent: a host that cannot BUILD the cloned
  // tier resolves to the persisted default and the picker says why in its own
  // words beside the disabled option.
  it("falls back to the default when this host cannot build the cloned tier, and says so", async () => {
    mockConfinementClasses = ["CC1"];
    renderClone();
    await screen.findByRole("button", { name: /Launch run/ });
    expect(await screen.findByText(/Vault isn't installed on this host\./)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].confinement_class).toBe("CC1");
  });

  // The negative control: an ordinary /runs/new is untouched by any of this —
  // it still gets the persisted default re-resolved against the real host.
  it("leaves a NON-clone on the persisted default, banner and all", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderScreen();
    await screen.findByRole("button", { name: /Launch run/ });
    expect(screen.queryByText(RUN.CLONE_NOTE)).toBeNull();
    expect(screen.queryByText(RUN.CLONE_CEILING_NOTE)).toBeNull();
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
