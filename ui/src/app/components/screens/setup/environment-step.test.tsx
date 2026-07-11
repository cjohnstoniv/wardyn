/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupStatus } from "../../../lib/types";
import { EnvironmentStep, recommendedTier } from "./environment-step";
import { CONFINEMENT_CONSTANT_NOTE, CC_META } from "../../wardyn/cc-meta";
import { BTN, RESIDUAL_PREFIX } from "../../wardyn/copy";
import { baseStatus as sharedBaseStatus } from "./test-fixtures";

// Only the fields EnvironmentStep reads (runner + platform) carry meaning; the
// rest satisfy the type. CC1 + CC2 live, KVM-capable host so CC3 is "needs
// setup" (not incompatible). This suite's own pin is empty `providers`.
function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return sharedBaseStatus({ providers: [], ...overrides });
}

// Disabled radios/buttons (#5) mean default pointerEventsCheck now passes — a
// click on a disabled control simply no-ops instead of tripping the guard.
const user = userEvent.setup();

function renderStep(props: Partial<React.ComponentProps<typeof EnvironmentStep>> = {}) {
  const onSelect = vi.fn();
  const utils = render(
    <EnvironmentStep
      status={props.status ?? baseStatus()}
      selected={props.selected ?? null}
      onSelect={props.onSelect ?? onSelect}
      recheckToken={props.recheckToken}
      rechecking={props.rechecking}
    />,
  );
  return { onSelect: props.onSelect ?? onSelect, ...utils };
}

describe("EnvironmentStep — matrix-as-picker", () => {
  it("(1) renders a radiogroup with three tier radios named Fence/Wall/Vault", () => {
    renderStep();
    expect(screen.getByRole("radiogroup", { name: "Barrier tier" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Fence/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Wall/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Vault/ })).toBeInTheDocument();
    expect(screen.getAllByRole("radio")).toHaveLength(3);
  });

  it("(2a) selecting a column by click calls onSelect with its wire class", async () => {
    const { onSelect } = renderStep();
    await user.click(screen.getByRole("radio", { name: /Wall/ }));
    expect(onSelect).toHaveBeenCalledWith("CC2");
  });

  it("(2b) selecting via keyboard (focus + Enter) calls onSelect", async () => {
    const { onSelect } = renderStep();
    const wall = screen.getByRole("radio", { name: /Wall/ });
    wall.focus();
    await user.keyboard("{Enter}");
    expect(onSelect).toHaveBeenCalledWith("CC2");
  });

  it("(2c) aria-checked follows the selected prop", () => {
    renderStep({ selected: "CC2" });
    expect(screen.getByRole("radio", { name: /Wall/ })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("radio", { name: /Fence/ })).toHaveAttribute("aria-checked", "false");
    expect(screen.getByRole("radio", { name: /Vault/ })).toHaveAttribute("aria-checked", "false");
  });

  it("(3) an unavailable tier radio is disabled and never selects", async () => {
    const { onSelect } = renderStep();
    const vault = screen.getByRole("radio", { name: /Vault/ });
    expect(vault).toBeDisabled();
    await user.click(vault);
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("(4) clicking a selectable column's body cell selects it", async () => {
    const { onSelect } = renderStep();
    // The Wall column's Mechanism cell — clicking anywhere in the column selects.
    await user.click(screen.getByText(/gVisor userspace kernel intercepts syscalls/));
    expect(onSelect).toHaveBeenCalledWith("CC2");
  });

  it("(5) Show setup command reveals the command; a re-check then shows still-not-detected", async () => {
    // Only CC2 is needs-setup here (CC3 is incompatible with no KVM), so there is
    // exactly one Show-setup-command button to drive.
    const status = baseStatus({
      runner: { driver: "docker", confinement_classes: ["CC1"] },
      platform: { os: "linux", wsl: false, kvm: false },
    });
    const { rerender } = renderStep({ status, recheckToken: 0 });

    expect(screen.queryByText(/wardyn setup wall/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: BTN.showSetupCommand }));
    expect(screen.getByText(/wardyn setup wall/)).toBeInTheDocument();
    expect(screen.queryByText(/Still not detected/)).not.toBeInTheDocument();

    // A completed host re-check bumps the token while the panel is open.
    rerender(
      <EnvironmentStep
        status={status}
        selected={null}
        onSelect={vi.fn()}
        recheckToken={1}
      />,
    );
    expect(screen.getByText(/Still not detected/)).toBeInTheDocument();
  });

  it("(6) a KVM-less host marks Vault incompatible with the concrete /dev/kvm reason", () => {
    const status = baseStatus({ platform: { os: "linux", wsl: false, kvm: false } });
    renderStep({ status });
    // Phrase unique to the incompatible-reason paragraph (the CC3 mechanism row
    // also mentions /dev/kvm, so match the reason's own wording).
    expect(screen.getByText(/doesn't expose \/dev\/kvm/)).toBeInTheDocument();
    expect(screen.getByText("Incompatible here")).toBeInTheDocument();
  });

  it("(7) a ready tier shows the substrate it runs as", () => {
    renderStep();
    // Both live tiers (CC1, CC2) show their substrate; the exact runtime is unique.
    expect(screen.getAllByText(/Running here as/)).toHaveLength(2);
    expect(screen.getByText("oci/runc")).toBeInTheDocument();
    expect(screen.getByText("oci/runsc")).toBeInTheDocument();
  });

  it("(8) the every-tier constant note appears exactly once", () => {
    renderStep();
    expect(
      screen.getAllByText(
        /Whatever the barrier, every run still gets Wardyn's egress filtering/,
      ),
    ).toHaveLength(1);
    // Sanity: the reused verbatim string is the one being rendered.
    expect(CONFINEMENT_CONSTANT_NOTE).toMatch(/every run still gets Wardyn's egress filtering/);
  });

  it("(9) no runner: shows the danger card and still renders all three tier names", () => {
    const status = baseStatus({ runner: { driver: "none", confinement_classes: [] } });
    renderStep({ status });
    expect(screen.getByText(/No sandbox runner/)).toBeInTheDocument();
    expect(screen.getAllByRole("radio")).toHaveLength(3);
    // The three friendly names still render inside the read-only matrix.
    const group = screen.getByRole("radiogroup", { name: "Barrier tier" });
    expect(within(group).getByText("Fence")).toBeInTheDocument();
    expect(within(group).getByText("Wall")).toBeInTheDocument();
    expect(within(group).getByText("Vault")).toBeInTheDocument();
  });

  it("(10) exactly one Recommended chip renders", () => {
    // KVM-less ⇒ recommendedTier steps down from Vault to Wall (CC2) — the only
    // reachable way to pin a non-Vault recommendation (see (R1) below).
    const status = baseStatus({ platform: { os: "linux", wsl: false, kvm: false } });
    renderStep({ status });
    expect(screen.getAllByText("Recommended")).toHaveLength(1);
  });

  // ── Honesty invariants (delete-the-row must fail the suite) ─────────────────
  it("(H1) the permanent Doesn't-stop row renders RESIDUAL_PREFIX + each tier's residual", () => {
    renderStep();
    expect(screen.getByText(RESIDUAL_PREFIX)).toBeInTheDocument();
    for (const cc of ["CC1", "CC2", "CC3"] as const) {
      expect(screen.getByText(CC_META[cc].doesntProtect)).toBeInTheDocument();
    }
  });

  it("(H2) a caveat matrix cell's title === RESIDUAL_PREFIX + its tier's residual", () => {
    renderStep();
    // CC2 carries caveat marks (kernel-exploit + full-break-in rows). Its cell
    // title must reuse the residual copy verbatim — no re-authored risk wording.
    const expected = `${RESIDUAL_PREFIX} ${CC_META.CC2.doesntProtect}`;
    const caveats = screen.getAllByLabelText("Yes, with caveat");
    expect(caveats.some((el) => el.getAttribute("title") === expected)).toBe(true);
  });

  // ── recommendedTier helper (exported for tests only) ─────────────────────────
  it("(R1) recommendedTier picks the strongest COMPATIBLE tier, not the strongest installed", () => {
    // kvm-capable ⇒ Vault is recommended even though CC3 isn't in confinement_classes.
    expect(recommendedTier(baseStatus())).toBe("CC3");
    // KVM-less ⇒ Vault is hardware-impossible, so it steps down to Wall.
    expect(
      recommendedTier(baseStatus({ platform: { os: "linux", wsl: false, kvm: false } })),
    ).toBe("CC2");
  });

  // ── #11 additions ───────────────────────────────────────────────────────────
  it("(11a) clicking a NON-selectable column's body cell does NOT call onSelect", async () => {
    const { onSelect } = renderStep(); // CC3 is needs-setup here (not ready) ⇒ unselectable
    await user.click(screen.getByText(/Kata microVM/)); // the Vault Mechanism cell
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("(11b) no runner: all three tier radios render disabled", () => {
    const status = baseStatus({ runner: { driver: "none", confinement_classes: [] } });
    renderStep({ status });
    for (const radio of screen.getAllByRole("radio")) {
      expect(radio).toBeDisabled();
    }
  });

  it("(11c) ArrowRight moves selection to the next selectable tier (Fence → Wall)", async () => {
    const { onSelect } = renderStep();
    screen.getByRole("radio", { name: /Fence/ }).focus();
    await user.keyboard("{ArrowRight}");
    expect(onSelect).toHaveBeenCalledWith("CC2");
  });

  it("(11c-wrap) ArrowRight from Wall wraps past disabled Vault back to Fence", async () => {
    // Default fixture: CC1+CC2 selectable, CC3 needs-setup (kvm:true, so NOT
    // hardware-incompatible) and therefore unselectable — the wrap must SKIP
    // Vault and land on Fence.
    const { onSelect } = renderStep();
    screen.getByRole("radio", { name: /Wall/ }).focus();
    await user.keyboard("{ArrowRight}");
    expect(onSelect).toHaveBeenCalledWith("CC1");
  });

  it("radio click fires onSelect exactly once (no th-bubble double-fire)", async () => {
    const { onSelect } = renderStep();
    await user.click(screen.getByRole("radio", { name: /Wall/ }));
    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  it("(11d) rechecking shows Checking… chips and hides the Still-not-detected line", async () => {
    const status = baseStatus({
      runner: { driver: "docker", confinement_classes: ["CC1"] },
      platform: { os: "linux", wsl: false, kvm: false },
    });
    // Reveal the command, bump the token, then flip rechecking on: the still-not-
    // detected line must yield to the in-flight "Checking…" state.
    const { rerender } = renderStep({ status, recheckToken: 0 });
    await user.click(screen.getByRole("button", { name: BTN.showSetupCommand }));
    rerender(
      <EnvironmentStep
        status={status}
        selected={null}
        onSelect={vi.fn()}
        recheckToken={1}
        rechecking
      />,
    );
    expect(screen.queryByText(/Still not detected/)).not.toBeInTheDocument();
    expect(screen.getAllByText(/Checking/).length).toBeGreaterThan(0);
  });
});
