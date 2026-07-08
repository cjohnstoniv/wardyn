/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
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

  it("uses the wire field name as a mono hint, not a magic-number readout", () => {
    renderStep();
    expect(screen.getByText("Keep running until I stop it")).toBeInTheDocument();
    expect(screen.queryByText(/auto_stop_after_sec = -1/)).toBeNull();
    expect(screen.getAllByText("auto_stop_after_sec").length).toBeGreaterThan(0);
  });
});
