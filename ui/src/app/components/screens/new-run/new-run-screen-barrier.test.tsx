/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Barrier control — split out of new-run-screen.test.tsx, which was
// already at the check-file-size.sh ceiling. Its own copy of the screen's
// mock harness, same shape as new-run-screen-saved-policy.test.tsx's.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

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
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => Promise.resolve([]),
    createPolicy: vi.fn(),
    getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a),
  },
}));
const createRunMock = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    isCredentialRefusal: actual.isCredentialRefusal,
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
  myCapabilitiesMock.mockReset().mockReturnValue(null);
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

    // The setup-status mock reports CC1 only, so Wall/Vault are unavailable AND
    // below-floor — one reason each, never two — while Fence, which this host
    // builds fine, is disabled for the floor alone. Every tier disabled is
    // fail-closed on purpose; preflight and launch name the cause.
    expect(await screen.findByText(/Fence is below the policy's floor \(Vault\)/)).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Fence" })).toBeDisabled();
    expect(screen.getByText(/Wall isn't installed on this host/)).toBeInTheDocument();
    expect(screen.queryByText(/Wall is below the policy's floor/)).not.toBeInTheDocument();
  });

  // DONE WHEN: a test fails if an unavailable class becomes selectable again.
  // Nothing here qualifies (floor CC3, host CC1-only), so every radio is
  // disabled and a click on any of them must be a no-op — never a selection.
  it("never selects a disabled (unavailable-or-below-floor) tier on click", async () => {
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
    await screen.findByText(/Fence is below the policy's floor \(Vault\)/);
    // Every tier is disabled fail-closed here (floor CC3, host CC1-only) — the
    // floor up-clamp already forces the ONE checked radio to Vault on its
    // own. The assertion that matters is that clicking a disabled button
    // never MOVES that checked state, whichever radio it started on.
    const checkedBefore = ["Fence", "Wall", "Vault"].map(
      (name) => screen.getByRole("radio", { name }).getAttribute("aria-checked"),
    );
    for (const name of ["Fence", "Wall", "Vault"]) {
      const radio = screen.getByRole("radio", { name });
      expect(radio).toBeDisabled();
      await user.click(radio);
    }
    const checkedAfter = ["Fence", "Wall", "Vault"].map(
      (name) => screen.getByRole("radio", { name }).getAttribute("aria-checked"),
    );
    expect(checkedAfter).toEqual(checkedBefore);
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
    // Unknown never disables Launch FOR THIS REASON — the "no barrier" text
    // is for a SETTLED empty read (below), not an unresolved probe. (A title
    // is still required, which is why this doesn't also assert `not
    // toBeDisabled()` here — that's `problem`, a different gate.)
    expect(screen.queryByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).not.toBeInTheDocument();
  });
});

// #214 — a settled probe reporting zero classes (a real runner, nothing it
// can build): Launch is disabled too, not just the three tiers, with the
// reason stated beside it and a route to the step that fixes it.
describe("NewRunScreen — a host that can build no barrier disables Launch itself (#214)", () => {
  it("disables Launch and states the reason with a route to Environment", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ runner: { driver: "docker", confinement_classes: [] } }),
    );
    renderScreen();
    const launchBtn = await screen.findByRole("button", { name: "Launch run" });
    await waitFor(() => expect(launchBtn).toBeDisabled());
    expect(screen.getByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).toBeInTheDocument();
    const link = screen.getByRole("link", { name: NO_BARRIER.CTA });
    expect(link).toHaveAttribute("href", NO_BARRIER.ROUTE);
  });

  it("a host with at least one barrier leaves Launch alone", async () => {
    renderScreen(); // beforeEach's default: CC1 only
    const launchBtn = await screen.findByRole("button", { name: "Launch run" });
    await user.type(screen.getByLabelText("Title"), "Fine host");
    await waitFor(() => expect(launchBtn).not.toBeDisabled());
    expect(screen.queryByText(NO_BARRIER.LAUNCH_REASON, { exact: false })).not.toBeInTheDocument();
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
  // tier falls back like any fresh run and the picker says why in its own
  // words beside the disabled option — and it stops counting as an explicit
  // pick, so the request omits confinement_class and the SERVER'S OWN default
  // decides, rather than this screen silently substituting a guess.
  it("falls back and defers to the server when this host cannot build the cloned tier, and says so", async () => {
    mockConfinementClasses = ["CC1"];
    renderClone();
    await screen.findByRole("button", { name: /Launch run/ });
    expect(await screen.findByText(/Vault isn't installed on this host\./)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].confinement_class).toBeUndefined();
  });

  // The negative control: an ordinary /runs/new is untouched by any of this.
  it("leaves a NON-clone alone, banner and all", async () => {
    mockConfinementClasses = ["CC1", "CC2", "CC3"];
    renderScreen();
    await screen.findByRole("button", { name: /Launch run/ });
    expect(screen.queryByText(RUN.CLONE_NOTE)).toBeNull();
    expect(screen.queryByText(RUN.CLONE_CEILING_NOTE)).toBeNull();
  });
});
