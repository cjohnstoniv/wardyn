/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Barrier control — split out of new-run-screen.test.tsx, which was
// already at the check-file-size.sh ceiling. Its own copy of the screen's
// mock harness, same shape as new-run-screen-saved-policy.test.tsx's.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useNavigate, type NavigateFunction } from "react-router-dom";

// Which barriers this host can BUILD — read off getSetupStatusMock's
// runner.confinement_classes (the same field every other surface reads).
// Mutable because the clone cases need a host that has the tier the cloned
// run used; beforeEach installs a LAZY mockImplementation (not
// mockResolvedValue) that reads this variable at CALL time, so a test may
// set it before rendering without re-wiring the mock.
let mockConfinementClasses: Array<"CC1" | "CC2" | "CC3"> = ["CC1"];
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
const getDefaultPolicyMock = vi.fn();
const listPoliciesMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: (...a: unknown[]) => listPoliciesMock(...a),
    createPolicy: vi.fn(),
    getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a),
  },
}));
const createRunMock = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    isCredentialRefusal: actual.isCredentialRefusal,
    isGitCredentialRefusal: actual.isGitCredentialRefusal,
    runs: {
      createRun: (...a: unknown[]) => createRunMock(...a),
      listRuns: () => Promise.resolve([]),
      preflightRun: vi.fn(),
      gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
    },
  };
});
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: () => Promise.resolve([]) },
}));
const myCapabilitiesMock = vi.fn();
vi.mock("../../../lib/capabilities", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/capabilities")>(
    "../../../lib/capabilities",
  );
  return { ...actual, useMyCapabilities: (...a: unknown[]) => myCapabilitiesMock(...a) };
});

import { NewRunScreen } from "./new-run-screen";
import { baseStatus } from "../../../lib/test-fixtures";
import { OperatorProvider } from "../../wardyn/operator-context";
import { NO_BARRIER, RUN } from "../../wardyn/copy";
import { TIER_PICKER } from "../../../lib/tier-picker-copy";

const user = userEvent.setup({ pointerEventsCheck: 0 });

function renderScreen() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockConfinementClasses = ["CC1"];
  getSetupStatusMock.mockReset().mockImplementation(() =>
    Promise.resolve(
      baseStatus({ runner: { driver: "docker", confinement_classes: mockConfinementClasses } }),
    ),
  );
  createRunMock.mockReset().mockResolvedValue({ id: "run_1" });
  getDefaultPolicyMock.mockReset().mockResolvedValue({ min_confinement_class: "CC1" });
  listPoliciesMock.mockReset().mockResolvedValue([]);
  myCapabilitiesMock.mockReset().mockReturnValue(null);
});

// #1200 — the shared TierPicker DROPS a tier the floor forbids or the host
// can't build, rather than showing it disabled with a reason (the global
// "installed ∧ allowed" rule every user-facing picker now follows). Nothing
// here qualifies (floor CC3, host CC1-only), so the control collapses to the
// T-9 requirement card instead of a fully-disabled Seg.
describe("NewRunScreen — the barrier floor leaves nothing this run can use (T-9)", () => {
  it("shows the requirement card naming the floor, with no radiogroup at all", async () => {
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

    // The setup-status mock reports CC1 only: nothing installed reaches the
    // Vault floor, so the picker names the requirement instead of listing
    // three disabled options.
    expect(
      await screen.findByText(/This run's floor requires Vault, and this host can't run it/),
    ).toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: "Fence" })).toBeNull();
    expect(screen.queryByRole("radio", { name: "Wall" })).toBeNull();
    expect(screen.queryByRole("radio", { name: "Vault" })).toBeNull();
  });
});

// An untouched Barrier control must omit confinement_class, so the server's
// own strongest-installed-at-or-above-the-floor default
// (internal/api/runs_policy.go's strongestAdvertisedAtOrAbove) applies to a
// console launch, and the confinement_source audit field (requested vs
// defaulted) can read "defaulted" from this screen.
describe("NewRunScreen — an untouched Barrier omits confinement_class", () => {
  it("sends no confinement_class when the operator never touches the Barrier control", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderScreen();
    await screen.findByRole("button", { name: /Launch run/ });
    await user.type(screen.getByLabelText("Title"), "Untouched barrier");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].confinement_class).toBeUndefined();
  });

  it("sends the exact class once the operator clicks a tier", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderScreen();
    await screen.findByRole("button", { name: /Launch run/ });
    await user.type(screen.getByLabelText("Title"), "Touched barrier");
    await user.click(await screen.findByRole("radio", { name: "Wall" }));
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].confinement_class).toBe("CC2");
  });
});

// No runner AT ALL (runner.driver:"none" — e2e's own `-runner none` backend,
// scripts/e2e-backend.sh) is UNKNOWN for this control, never "nothing
// installed": runs_create.go skips its capability gate entirely with no
// runner configured, so there is no real tier to prefer or disable either
// way, and every tier must stay selectable exactly as an inconclusive probe
// leaves them (item 2).
describe("NewRunScreen — no runner configured reads as unknown, not confirmed-absent", () => {
  it("keeps every tier selectable and offers no reason lines", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "none", confinement_classes: [] } }),
    );
    renderScreen();
    await screen.findByRole("button", { name: /Launch run/ });
    for (const name of ["Fence", "Wall", "Vault"]) {
      expect(screen.getByRole("radio", { name })).not.toBeDisabled();
    }
    expect(screen.queryByText(/isn't installed on this host/)).not.toBeInTheDocument();
  });

  // #1200 review P2-7 — "unknown" must not mean "offer a tier the ACTIVE
  // FLOOR already forbids": with a CC3 floor authored and the host probe
  // inconclusive, the picker still has to fall back to the floor's own
  // allowed set (allowedFromFloor), not the unfiltered ORDERED_CLASSES.
  it("a known floor still hides what it forbids even while host availability is unknown", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "none", confinement_classes: [] } }),
    );
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
    // Vault is the ONLY tier the floor allows, so the control collapses to
    // its decided row — never a 3-tier radiogroup with Fence/Wall offered.
    expect(await screen.findByText(RUN.BARRIER_ONLY_QUALIFIER)).toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: "Fence" })).toBeNull();
    expect(screen.queryByRole("radio", { name: "Wall" })).toBeNull();
  });
});

// #1200 review P2-1/P2-2/P2-8 — a NON-operator member (renderScreen/
// renderClone above are both operator:true, which is exactly the gap the
// review's mutation table found: M4a — turning off the governance
// fold-in survived every pre-existing New Run vitest case, caught only by
// e2e). This proves the fold-in itself at the unit level, both directions.
function renderAsMember() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={false}>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

describe("NewRunScreen — a member's governance ceiling folds into the Barrier control (P2-8)", () => {
  it("a Vault floor, with Vault installed, collapses to the decided row naming the admin", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC3",
      governance_profile_name: "vault-required",
    });
    renderAsMember();
    expect(await screen.findByText(/Vault · set by your admin/)).toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: "Fence" })).toBeNull();
  });

  it("a Vault floor this host cannot build shows the requirement card, never a silent Wall fallback", async () => {
    mockConfinementClasses = ["CC1", "CC2"];
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC3",
      governance_profile_name: "vault-required",
    });
    renderAsMember();
    expect(
      await screen.findByText(/Your admin requires Vault, and this host can't run it/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Wall · set by your admin/)).toBeNull();
    expect(screen.queryByRole("radio", { name: "Wall" })).toBeNull();
  });

  // Review P2-2(a): an ADMIN is never clamped, even with a floored deployment
  // default — this is the false-hide the pre-review build introduced.
  it("an ADMIN's inline policy is never clamped to the deployment default — Fence stays offered", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    getDefaultPolicyMock.mockResolvedValue({ min_confinement_class: "CC2" });
    renderScreen(); // operator:true
    expect(await screen.findByRole("radio", { name: "Fence" })).toBeInTheDocument();
    expect(screen.queryByText(/set by your admin/)).toBeNull();
  });

  // Every mount-time read has resolved and re-rendered, so a line that only
  // appears once /policies/default lands cannot be missed.
  async function settled() {
    await waitFor(() => expect(getDefaultPolicyMock).toHaveBeenCalled());
    await waitFor(() => expect(getSetupStatusMock).toHaveBeenCalled());
    await act(async () => {
      await getDefaultPolicyMock.mock.results[0].value;
      await getSetupStatusMock.mock.results[0].value;
    });
  }

  // Review R2-1 probe P2b — the deployment default's Fence floor removed
  // nothing on a Fence-only host: the line must be the neutral one.
  it("an UNASSIGNED member on a Fence-only host is never told their admin set it (P2b)", async () => {
    mockConfinementClasses = ["CC1"];
    renderAsMember(); // /policies/default: Fence floor, no profile (beforeEach)
    await settled();
    expect(screen.getByText(RUN.BARRIER_ONLY_QUALIFIER)).toBeInTheDocument();
    expect(screen.queryByText(/set by your admin/)).toBeNull();
  });

  // Review R2-1 probe P2c — a profile requiring Wall on a Wall-only host:
  // the floor removed no installed tier, so the line is neutral too.
  it("a Wall floor on a Wall-only host removes nothing, so the line is neutral (P2c)", async () => {
    mockConfinementClasses = ["CC2"];
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC2",
      governance_profile_name: "wall-required",
    });
    renderAsMember();
    await settled();
    expect(screen.getByText(RUN.BARRIER_ONLY_QUALIFIER)).toBeInTheDocument();
    expect(screen.queryByText(/set by your admin/)).toBeNull();
  });

  // Review R2-2 — the saved-policy half of govFloorApplies: an unassigned
  // member's saved policy is not raised to the deployment default
  // (inline_policy.go: `ceiling.Profile != nil`), while their inline one is.
  it("an UNASSIGNED member's saved policy keeps Fence although the deployment default floors at Wall", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    getDefaultPolicyMock.mockResolvedValue({ min_confinement_class: "CC2" });
    listPoliciesMock.mockResolvedValue([
      {
        id: "pol_fence",
        name: "Fence policy",
        spec: { allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC1" },
      },
    ]);
    renderAsMember();
    // The Custom lane IS clamped for every non-operator.
    await waitFor(() => {
      expect(screen.getByRole("radio", { name: "Wall" })).toBeInTheDocument();
      expect(screen.queryByRole("radio", { name: "Fence" })).toBeNull();
    });
    await user.click(screen.getByRole("button", { name: /Reuse a saved policy/ }));
    await user.click(screen.getByRole("combobox", { name: "Saved policy" }));
    await user.click(await screen.findByRole("option", { name: "Fence policy" }));
    expect(await screen.findByRole("radio", { name: "Fence" })).toBeInTheDocument();
  });

  // Review R2-4 — the Vault reason follows the driver, as environment-step's
  // tierState does: on k8s the remedy is a Kata RuntimeClass, never /dev/kvm.
  it.each([
    ["k8s", /Kata RuntimeClass/, /\/dev\/kvm/],
    ["docker", /\/dev\/kvm/, /Kata RuntimeClass/],
  ])("a Vault floor on a KVM-less %s host names that driver's remedy", async (driver, want, never) => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        runner: { driver, confinement_classes: ["CC1", "CC2"] },
        platform: { os: "linux", wsl: false, kvm: false },
      }),
    );
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC3",
      governance_profile_name: "vault-required",
    });
    renderAsMember();
    expect(await screen.findByText(want)).toBeInTheDocument();
    expect(screen.queryByText(never)).toBeNull();
  });
});

// B4b — a clone, MOUNTED.
//
// wizard-spec.test.ts proves runPrefill + initialWizardState compose the right
// state. It cannot prove the SCREEN keeps it: the mount-time /setup/status read
// must not re-seed confinementClass (and pristineCc with it) over a clone's
// already-applied prefill — silently launching a cloned run weaker than its
// source, while the banner still promises the barrier carried over, is the one
// direction a governance product must never allow. Only a mounted test that
// reads the WIRE BODY can catch it.
//
// There is no persisted default left to disagree with (the server picks the
// strongest installed class at or above the floor) — the carve-out now guards
// against the SAME class of bug for a different reason: an untouched Barrier
// control omits confinement_class so the server decides (see the sibling
// describe above), and a clone's carried-over class must still reach the wire
// explicitly rather than silently falling into that omit.
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

  function renderClone(state: unknown = { prefill }) {
    return render(
      <MemoryRouter initialEntries={[{ pathname: "/runs/new", state }]}>
        <OperatorProvider operator>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
  }

  it("launches at the SOURCE run's barrier, not the server's own default", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderClone();
    // Wait for the barrier probe to settle — this is the effect that can
    // overwrite the prefill, so asserting before it lands would pass regardless.
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
  // tier falls back like any fresh run, and it stops counting as an explicit
  // pick, so the request omits confinement_class and the SERVER'S OWN default
  // decides, rather than this screen silently substituting a guess. #1200:
  // Fence is now the ONLY tier this host can build, so the picker collapses
  // to its own decided row rather than naming Vault as disabled.
  //
  // Review P2-1: this caller is an OPERATOR (renderClone's own
  // OperatorProvider) with no governance profile at all — the decided line
  // must be the NEUTRAL "only barrier this run can use" sentence, never
  // "set by your admin" (nobody's admin narrowed anything here; the HOST
  // itself only has one tier).
  it("falls back and defers to the server when this host cannot build the cloned tier", async () => {
    mockConfinementClasses = ["CC1"];
    renderClone();
    await screen.findByRole("button", { name: /Launch run/ });
    expect(await screen.findByText(RUN.BARRIER_ONLY_QUALIFIER)).toBeInTheDocument();
    expect(screen.queryByText(/set by your admin/)).toBeNull();
    expect(screen.queryByRole("radio", { name: "Vault" })).toBeNull();

    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].confinement_class).toBeUndefined();
  });

  // The top bar's New run navigates to /runs/new with no state while this
  // screen stays mounted. The mount-time /setup/status read must not run again
  // off the cleared state and reset the barrier under the rest of the clone.
  it("keeps the cloned barrier when a stateless /runs/new navigation lands on the mounted form", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    let navigate!: NavigateFunction;
    function CaptureNavigate() {
      navigate = useNavigate();
      return null;
    }
    render(
      <MemoryRouter
        initialEntries={[
          { pathname: "/runs/new", state: { prefill: { ...prefill, state: { ...prefill.state, confinementClass: "CC2" } } } },
        ]}
      >
        <CaptureNavigate />
        <OperatorProvider operator>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
    await waitFor(() =>
      expect(screen.getByRole("radio", { name: "Wall" })).toHaveAttribute("aria-checked", "true"),
    );

    act(() => {
      void navigate("/runs/new");
    });
    expect(getSetupStatusMock).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].confinement_class).toBe("CC2");
  });

  // The negative control: an ordinary /runs/new is untouched by any of this.
  it("leaves a NON-clone alone, banner and all", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderScreen();
    await screen.findByRole("button", { name: /Launch run/ });
    expect(screen.queryByText(RUN.CLONE_NOTE)).toBeNull();
    expect(screen.queryByText(RUN.CLONE_CEILING_NOTE)).toBeNull();
  });

  // Review F4 (#1197 L3): the Runs landing page's composer rides this SAME
  // prefill channel (task + an optional workspace) but has no source run —
  // `source: "composer"` must suppress the whole banner (both sentences),
  // never just re-word it, while the task itself still prefills the form
  // exactly like a clone's would.
  it("a composer prefill (source: 'composer') shows no clone banner at all, but still prefills the task", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderClone({
      prefill: { inlinePolicy: false, source: "composer", state: { task: "from the composer" } },
    });
    await screen.findByRole("button", { name: /Launch run/ });
    // Default mode is interactive (freshWizardState) — same label the task
    // field carries for any prefill that doesn't also set `mode`.
    expect(screen.getByLabelText("Initial prompt (optional)")).toHaveValue("from the composer");
    expect(screen.queryByText(RUN.CLONE_NOTE)).toBeNull();
    expect(screen.queryByText(RUN.CLONE_CEILING_NOTE)).toBeNull();
  });
});

// #214 — a SETTLED probe reporting zero classes (Docker reachable, nothing
// installed): the host genuinely cannot build any barrier, so Launch itself
// must be disabled, not just every tier — the one control that cannot work
// must not be the one that looks ready. Distinct from the T-9 floor-only
// case above (host HAS a tier; the run's own floor forbids all of them) —
// that stays a policy choice, never a reason Launch can never work here.
describe("NewRunScreen — #214: no barrier at all on this host disables Launch", () => {
  it("disables Launch and states the reason with a route to the Environment step", async () => {
    mockConfinementClasses = [];
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "No barrier host");
    expect(await screen.findByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).toBeInTheDocument();
    const link = screen.getByRole("link", { name: NO_BARRIER.CTA });
    expect(link).toHaveAttribute("href", NO_BARRIER.ADMIN_ROUTE);
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeDisabled();
  });

  it("a host WITH a barrier never shows the no-barrier reason, and Launch is not disabled by it", async () => {
    mockConfinementClasses = ["CC1"];
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Has a barrier");
    await waitFor(() => expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled());
    expect(screen.queryByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).toBeNull();
  });

  // #1328 review round 2, R2-1 — a member (never an operator, never a
  // session-user) reads the reason alone: there is nothing behind
  // NO_BARRIER.ADMIN_ROUTE they may open, so no link renders at all.
  it("a non-operator sees the reason with no CTA", async () => {
    mockConfinementClasses = [];
    render(
      <MemoryRouter>
        <OperatorProvider operator={false}>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
    await user.type(await screen.findByLabelText("Title"), "No barrier host");
    expect(await screen.findByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: NO_BARRIER.CTA })).toBeNull();
  });
});

// #1238 — the picker's follow-ups: the states the console must not draw as
// something the server contradicts.
describe("NewRunScreen — #1238 tier picker states", () => {
  // The member view of /setup/status blanks runner.driver, so the only way the
  // console can tell a Kubernetes install from a Docker host is the
  // runner.kubernetes bit. Before it, a member on Kubernetes was told to
  // bind-mount /dev/kvm — a Docker-host remedy that is wrong there.
  it("a member on Kubernetes is never handed the /dev/kvm remedy for Vault", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        runner: { driver: "", kubernetes: true, confinement_classes: ["CC1", "CC2"] },
        platform: { os: "linux", wsl: false, kvm: false },
      }),
    );
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC3",
      governance_profile_name: "vault-required",
    });
    renderAsMember();
    expect(
      await screen.findByText("Your admin requires Vault, and this host can't run it: Vault isn't installed on this host."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Vault needs|\/dev\/kvm|RuntimeClass/)).toBeNull();
  });

  it("a member on a Docker host with no /dev/kvm still gets the KVM reason", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({
        runner: { driver: "", confinement_classes: ["CC1", "CC2"] },
        platform: { os: "linux", wsl: false, kvm: false },
      }),
    );
    getDefaultPolicyMock.mockResolvedValue({
      min_confinement_class: "CC3",
      governance_profile_name: "vault-required",
    });
    renderAsMember();
    expect(await screen.findByText(/Vault needs KVM virtualization/)).toBeInTheDocument();
  });

  it("an admin on Kubernetes still gets the RuntimeClass remedy", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "k8s", kubernetes: true, confinement_classes: ["CC1", "CC2"] } }),
    );
    renderScreen();
    fireEvent.change(await screen.findByLabelText(/Spec \(JSON\)/), {
      target: {
        value: JSON.stringify({ allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC3" }),
      },
    });
    expect(await screen.findByText(/Vault needs a Kata RuntimeClass/)).toBeInTheDocument();
  });

  // The unknown-barrier line was gated on a state no path could reach: the
  // probe only settled beside a real class list.
  it.each([
    ["an unreachable /setup/status", () => baseStatus({ unreachable: true, runner: { driver: "none", confinement_classes: [] } })],
    ["no runner configured", () => baseStatus({ runner: { driver: "none", confinement_classes: [] } })],
  ])("%s draws the unknown-barrier line and marks the rows Unverified", async (_name, status) => {
    getSetupStatusMock.mockResolvedValue(status());
    renderScreen();
    expect(await screen.findByText(RUN.BARRIER_UNKNOWN)).toBeInTheDocument();
    expect(screen.getAllByText("Unverified").length).toBeGreaterThan(0);
    expect(screen.queryByText("Ready")).toBeNull();
    // Unknown never blocks launch.
    expect(screen.getByRole("button", { name: /Launch run/ })).not.toBeDisabled();
  });

  it("a rejected /setup/status read draws the unknown-barrier line too", async () => {
    getSetupStatusMock.mockRejectedValue(new Error("network"));
    renderScreen();
    expect(await screen.findByText(RUN.BARRIER_UNKNOWN)).toBeInTheDocument();
  });

  // The owner-approved pending word: Checking\u2026 (single-character ellipsis)
  // until the probe lands, then the real answer.
  it("reads Checking\u2026 while the barrier probe is pending, then Ready", async () => {
    let release: (v: unknown) => void = () => {};
    getSetupStatusMock.mockImplementation(
      () => new Promise((r) => { release = r; }),
    );
    renderScreen();
    expect((await screen.findAllByText("Checking\u2026")).length).toBeGreaterThan(0);
    expect(screen.queryByText("Ready")).toBeNull();
    expect(screen.queryByText("Unverified")).toBeNull();
    expect(screen.queryByText(RUN.BARRIER_UNKNOWN)).toBeNull();
    await act(async () => {
      release(baseStatus({ runner: { driver: "docker", confinement_classes: ["CC1", "CC2"] } }));
    });
    await waitFor(() => expect(screen.getAllByText("Ready").length).toBeGreaterThan(0));
    expect(screen.queryByText("Checking\u2026")).toBeNull();
  });

  it("a probed host says Ready and never the unknown line", async () => {
    mockConfinementClasses = ["CC1", "CC2"];
    renderScreen();
    await waitFor(() => expect(screen.getAllByText("Ready").length).toBeGreaterThan(0));
    expect(screen.queryByText("Unverified")).toBeNull();
    expect(screen.queryByText(RUN.BARRIER_UNKNOWN)).toBeNull();
  });

  // The U2 review, round 2: with no barrier on the host, the fresh form's
  // default CC1 floor made the card name Fence as the missing piece.
  it("a host with no barrier shows the no-runner title, not 'this run's floor requires Fence'", async () => {
    mockConfinementClasses = [];
    renderScreen();
    expect(await screen.findByText(TIER_PICKER.REQUIREMENT_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(/floor requires/)).toBeNull();
    expect(screen.queryByText(/isn't installed on this host/)).toBeNull();
  });

  // OperatorProvider's fail-open `operator` is TRUE until /me answers; the
  // governance floor binds every non-operator, so a caller whose role is not
  // yet known must be drawn as a member — the server still refuses the run.
  it("draws the governance floor until /me has answered, then lifts it for an operator", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    getDefaultPolicyMock.mockResolvedValue({ min_confinement_class: "CC2", governance_profile_name: "wall-required" });
    const ui = (resolved: boolean) => (
      <MemoryRouter>
        <OperatorProvider operator operatorResolved={resolved}>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>
    );
    const { rerender } = render(ui(false));
    await waitFor(() => expect(screen.getByRole("radio", { name: "Wall" })).toBeInTheDocument());
    expect(screen.queryByRole("radio", { name: "Fence" })).toBeNull();
    rerender(ui(true));
    expect(await screen.findByRole("radio", { name: "Fence" })).toBeInTheDocument();
  });

  // Review F1 — the up-clamp only raises. While /me is unresolved the floor
  // binds an admin too, so an untouched pick is raised to a tier the host
  // lacks; once /me lifts the floor it must re-seed to what now qualifies.
  it("re-seeds an untouched pick to the strongest installed tier once /me lifts a governance floor", async () => {
    mockConfinementClasses = ["CC1", "CC2"];
    getDefaultPolicyMock.mockResolvedValue({ min_confinement_class: "CC3", governance_profile_name: "vault-required" });
    const ui = (resolved: boolean) => (
      <MemoryRouter>
        <OperatorProvider operator operatorResolved={resolved}>
          <NewRunScreen />
        </OperatorProvider>
      </MemoryRouter>
    );
    const { rerender } = render(ui(false));
    expect(await screen.findByText(/Your admin requires Vault/)).toBeInTheDocument();
    rerender(ui(true));
    expect(await screen.findByRole("radio", { name: "Wall" })).toHaveAttribute("aria-checked", "true");
    expect(screen.queryByText("Vault")).toBeNull();
  });
});
