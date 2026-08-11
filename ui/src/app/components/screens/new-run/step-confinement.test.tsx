/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen, fireEvent, within } from "@testing-library/react";
import { StepConfinement } from "./step-confinement";
import { initialWizardState } from "./wizard-types";

// The confinement step must (1) show the friendly Fence/Wall/Vault labels with
// honest "Doesn't stop:" (D11) residual-risk copy, (2) keep the ACCURATE
// isolation substrate (runc / gVisor / Kata microVM) available — it lives in
// the tier card's / ConfinementChip's hover tooltip rather than as body text
// or a visible CC1/CC2/CC3 label — while NOT fabricating security semantics
// the backend doesn't tie to the class (credential brokering, egress
// filtering, HITL approvals are policy-driven, not substrate-driven), and (3)
// drive the real barrier picker (click a card => patch({ confinementClass })).
describe("StepConfinement copy", () => {
  function renderStep(overrides?: Partial<Parameters<typeof StepConfinement>[0]>) {
    return render(
      <StepConfinement
        state={initialWizardState()}
        patch={() => {}}
        availableClasses={["CC1", "CC2", "CC3"]}
        {...overrides}
      />,
    );
  }

  it("shows the Fence/Wall/Vault labels and honest residual-risk copy", () => {
    renderStep();
    expect(screen.getByText("Fence")).toBeInTheDocument();
    expect(screen.getByText("Wall")).toBeInTheDocument();
    expect(screen.getByText("Vault")).toBeInTheDocument();
    // The honest "Doesn't stop:" (D11) prefix is shown for every tier, not hidden.
    expect(screen.getAllByText("Doesn't stop:").length).toBe(3);
  });

  it("keeps the accurate substrate in the tooltips (runc / gVisor / Kata microVM / /dev/kvm), never a bare CC1/CC2/CC3 label", () => {
    const { container } = renderStep();
    const titles = Array.from(container.querySelectorAll("[title]"))
      .map((el) => el.getAttribute("title") || "")
      .join(" || ");
    expect(titles).toMatch(/runc/i);
    expect(titles).toMatch(/gVisor/i);
    expect(titles).toMatch(/Kata microVM/i);
    expect(titles).toMatch(/dev\/kvm/i);
    // The wire code is tooltip-only — never rendered as visible body text.
    expect(screen.queryByText(/^CC1$/)).toBeNull();
    expect(screen.queryByText(/^CC2$/)).toBeNull();
    expect(screen.queryByText(/^CC3$/)).toBeNull();
  });

  it("does NOT fabricate security semantics the backend doesn't implement", () => {
    renderStep();
    expect(screen.queryByText(/no credential brokering/i)).toBeNull();
    expect(screen.queryByText(/scoped credentials \+ egress filtering/i)).toBeNull();
    expect(screen.queryByText(/human-in-the-loop approvals required/i)).toBeNull();
  });

  it("marks an unavailable tier with the cause-neutral launch-time reason, and never lets it be picked", () => {
    let patched: Partial<ReturnType<typeof initialWizardState>> | null = null;
    renderStep({
      availableClasses: ["CC1", "CC2"],
      patch: (p) => {
        patched = p;
      },
    });
    expect(screen.getByText("Unavailable here")).toBeInTheDocument();
    // Cause-neutral launch-time fact — the wizard must NOT claim a hardware
    // cause (Getting Started owns the probed incompatible-vs-needs-setup split).
    expect(screen.getByText(/No Vault \(Kata microVM\) runtime on this runner/i)).toBeInTheDocument();
    const vaultCard = screen.getByRole("button", { name: /Vault/ });
    expect(vaultCard).toBeDisabled();
    fireEvent.click(vaultCard);
    expect(patched).toBeNull();
  });

  it("picking an available tier patches confinementClass", () => {
    let patched: Partial<ReturnType<typeof initialWizardState>> | null = null;
    renderStep({
      patch: (p) => {
        patched = p;
      },
    });
    fireEvent.click(screen.getByRole("button", { name: /Wall/ }));
    expect(patched).toEqual({ confinementClass: "CC2" });
  });

  // D12/claim8: the raw wire field name used to print next to BOTH lifecycle
  // radios (identical text discriminating nothing) — the only raw snake_case
  // field rendered as visible copy anywhere in New Run, contradicting this
  // file's own honesty header comment. The Review YamlBlock is the sanctioned
  // escape hatch for wire-shape detail; this card stays plain-English only.
  it("never renders the raw wire field name auto_stop_after_sec as visible copy", () => {
    renderStep();
    expect(screen.getByText("Keep running until I stop it")).toBeInTheDocument();
    expect(screen.getByText("Auto-stop after")).toBeInTheDocument();
    expect(screen.queryByText(/auto_stop_after_sec/)).toBeNull();
  });

  // N6: the number input patched { autoStopMinutes, lifecycle: "auto" } on
  // change, but stayed `disabled` until "auto" was ALREADY selected — the
  // lifecycle half of that patch was unreachable. Dropping `disabled` makes
  // the code's own intent (type a number, the radio follows) actually work.
  it("stays enabled while 'never' is selected, so typing it selects the auto radio (N6)", () => {
    let patched: Partial<ReturnType<typeof initialWizardState>> | null = null;
    renderStep({
      state: { ...initialWizardState(), lifecycle: "never" },
      patch: (p) => {
        patched = p;
      },
    });
    const input = screen.getByDisplayValue("60"); // initialWizardState's autoStopMinutes
    expect(input).not.toBeDisabled();
    fireEvent.change(input, { target: { value: "45" } });
    expect(patched).toEqual({ autoStopMinutes: 45, lifecycle: "auto" });
  });

  it("badges the strongest available tier's tile Recommended, same nudge as Getting started's tier picker", () => {
    renderStep({ availableClasses: ["CC1", "CC2"] });
    expect(screen.getAllByText("Recommended")).toHaveLength(1);
    const wallCard = screen.getByRole("button", { name: /Wall/ });
    expect(within(wallCard).getByText("Recommended")).toBeInTheDocument();
  });

  // N5/D13: the wizard seeds state.confinementClass from resolveDefaultCc,
  // which honors a still-runnable persisted pick over the strongest tier —
  // so the persisted pick and this step's own "Recommended" chip can
  // legitimately disagree. The screen must not argue with itself about it.
  describe("persisted default vs strongest available (N5/D13)", () => {
    it("splits the chips when the persisted pick differs from the strongest available tier", () => {
      renderStep({
        state: { ...initialWizardState(), confinementClass: "CC2" },
        availableClasses: ["CC1", "CC2", "CC3"],
        persistedDefaultCc: "CC2",
      });
      const vaultCard = screen.getByRole("button", { name: /Vault/ });
      const wallCard = screen.getByRole("button", { name: /Wall/ });
      expect(within(vaultCard).getByText("Strongest available")).toBeInTheDocument();
      expect(within(wallCard).getByText("Your saved default")).toBeInTheDocument();
      // Neither card claims plain "Recommended" once they disagree.
      expect(screen.queryByText("Recommended")).toBeNull();
    });

    it("keeps the single Recommended chip when the persisted pick and the strongest tier coincide", () => {
      renderStep({
        state: { ...initialWizardState(), confinementClass: "CC2" },
        availableClasses: ["CC1", "CC2"],
        persistedDefaultCc: "CC2",
      });
      expect(screen.getAllByText("Recommended")).toHaveLength(1);
      expect(screen.queryByText("Strongest available")).toBeNull();
      expect(screen.queryByText("Your saved default")).toBeNull();
    });

    it("keeps the single Recommended chip when nothing is persisted yet", () => {
      renderStep({ availableClasses: ["CC1", "CC2", "CC3"], persistedDefaultCc: null });
      expect(screen.getAllByText("Recommended")).toHaveLength(1);
      expect(screen.queryByText("Your saved default")).toBeNull();
    });

    it("keeps the single Recommended chip when the persisted pick isn't runnable here", () => {
      // resolveDefaultCc would already have fallen back to strongest in this
      // case (default-confinement.ts), so there's no genuine disagreement to
      // show — a "Your saved default" chip on an unavailable tier would be
      // pointing at a pick the wizard isn't honoring anyway.
      renderStep({ availableClasses: ["CC1", "CC2"], persistedDefaultCc: "CC3" });
      expect(screen.getAllByText("Recommended")).toHaveLength(1);
      expect(screen.queryByText("Your saved default")).toBeNull();
    });

    it("never forks while still probing (availableClasses null)", () => {
      renderStep({ availableClasses: null, persistedDefaultCc: "CC1" });
      expect(screen.queryByText("Strongest available")).toBeNull();
      expect(screen.queryByText("Your saved default")).toBeNull();
    });
  });
});
