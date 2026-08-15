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

  // HIGH-4: a member's redacted runner is Driver:"" (the Go zero value, not
  // the "none" sentinel) — must read identically to "none", not fall through
  // to the wrong ("start the Docker daemon") fix text.
  it("(HIGH-4) an empty driver string reads as no-driver, with the -runner docker fix (not the daemon fix)", () => {
    const status = baseStatus({ runner: { driver: "", confinement_classes: [] } });
    renderStep({ status });
    expect(screen.getByText(/No sandbox runner/)).toBeInTheDocument();
    expect(screen.getByText(/-runner docker/)).toBeInTheDocument();
    expect(screen.queryByText(/start the Docker daemon/)).not.toBeInTheDocument();
  });
});

// B4: k8s variant rows — Runner/Egress containment/Confinement classes/Agent
// images. Absent entirely on a non-k8s driver; the tier matrix itself is
// unaffected either way (same builder, k8s-shaped status data).
describe("EnvironmentStep — k8s rows (prompt-v4)", () => {
  function k8sStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
    return baseStatus({
      runner: { driver: "k8s", confinement_classes: ["CC1"], confinement_substrates: { CC1: "k8s/(default)" } },
      checks: [{ id: "agent_image", label: "Agent image toolchains", status: "info", detail: "Configured claude-code agent image: ghcr.io/x" }],
      ...overrides,
    });
  }

  it("absent on a docker driver", () => {
    renderStep({ status: baseStatus() });
    expect(screen.queryByLabelText("Kubernetes environment")).not.toBeInTheDocument();
  });

  it("renders Runner/Egress containment/Confinement classes/Agent images on a k8s driver", () => {
    renderStep({
      status: k8sStatus({
        checks: [
          {
            id: "k8s_egress_containment",
            label: "Egress containment",
            status: "ok",
            detail: "Enforcing · NetworkPolicy (the boot-time canary proved a deny-all policy actually blocks egress).",
          },
          { id: "agent_image", label: "Agent image toolchains", status: "info", detail: "Configured claude-code agent image: ghcr.io/x" },
        ],
      }),
    });
    const panel = screen.getByLabelText("Kubernetes environment");
    expect(within(panel).getByText("Runner")).toBeInTheDocument();
    expect(within(panel).getByText("Kubernetes")).toBeInTheDocument();
    expect(within(panel).getByText("Egress containment")).toBeInTheDocument();
    expect(within(panel).getByText(/Enforcing · NetworkPolicy/)).toBeInTheDocument();
    expect(within(panel).getByText("Confinement classes")).toBeInTheDocument();
    expect(within(panel).getByText(/CC1 \(k8s\/\(default\)\)/)).toBeInTheDocument();
    expect(within(panel).getByText("Agent image toolchains")).toBeInTheDocument();
  });

  it("Not-enforcing reuses the backend row's exact dual-form fix — never dropped to Enforcing's style", () => {
    renderStep({
      status: k8sStatus({
        checks: [
          {
            id: "k8s_egress_containment",
            label: "Egress containment",
            status: "fail",
            detail: "Not enforcing — the boot-time canary proved this cluster's CNI does not enforce NetworkPolicy.",
            fix: "Unset WARDYN_K8S_ALLOW_UNENFORCED_NETPOL (helm: env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL) and fix the cluster's CNI/NetworkPolicy support to restore real confinement.",
          },
        ],
      }),
    });
    const panel = screen.getByLabelText("Kubernetes environment");
    expect(within(panel).getByText(/Not enforcing/)).toBeInTheDocument();
    expect(within(panel).getByText(/helm: env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL/)).toBeInTheDocument();
    expect(within(panel).queryByText(/Enforcing · NetworkPolicy \(canary proved/)).not.toBeInTheDocument();
  });

  it("Indeterminate (field/row absent despite a k8s driver) never borrows Enforcing's style", () => {
    // No k8s_egress_containment row in checks at all — the honest local fallback.
    renderStep({ status: k8sStatus({ checks: [] }) });
    const panel = screen.getByLabelText("Kubernetes environment");
    expect(within(panel).getByText(/Indeterminate/)).toBeInTheDocument();
    expect(within(panel).queryByText(/^Enforcing/)).not.toBeInTheDocument();
  });

  it("the honest ceiling sentence names what CC2/CC3 need when only CC1 is live", () => {
    renderStep({ status: k8sStatus() }); // confinement_classes: ["CC1"] only
    const panel = screen.getByLabelText("Kubernetes environment");
    expect(within(panel).getByText(/CC2 needs a gVisor RuntimeClass; CC3 needs Kata\./)).toBeInTheDocument();
  });

  it("no ceiling sentence once the cluster maps all three classes", () => {
    renderStep({
      status: k8sStatus({
        runner: {
          driver: "k8s",
          confinement_classes: ["CC1", "CC2", "CC3"],
          confinement_substrates: { CC1: "k8s/(default)", CC2: "k8s/runsc", CC3: "k8s/kata-qemu" },
        },
      }),
    });
    const panel = screen.getByLabelText("Kubernetes environment");
    expect(within(panel).queryByText(/needs a gVisor RuntimeClass/)).not.toBeInTheDocument();
    expect(within(panel).getByText(/CC1 \(k8s\/\(default\)\), CC2 \(k8s\/runsc\), CC3 \(k8s\/kata-qemu\)/)).toBeInTheDocument();
  });

  // W4-S1-5/W27-S1-4: the tier matrix's own per-column guidance (below the
  // k8s rows above) used to stay docker-shaped no matter the driver — a k8s
  // operator got `wardyn setup wall`, "not listed in docker info runtimes",
  // and a KVM-bind-mount /dev/kvm verdict probed on wardynd's own host, none
  // of which apply to a cluster. The real lever (k8s.runtimeClasses) was
  // never named anywhere on this step.
  describe("tier-matrix guidance is driver-aware", () => {
    it("a k8s driver's needs-setup column shows the Helm RuntimeClass command, never `wardyn setup wall`", async () => {
      // CC1+CC3 live so CC2 is the ONLY needs-setup column — isolates the
      // Show-setup-command button the same way the incompatible test below does.
      renderStep({
        status: k8sStatus({
          runner: { driver: "k8s", confinement_classes: ["CC1", "CC3"], confinement_substrates: { CC1: "k8s/(default)", CC3: "k8s/kata-qemu" } },
        }),
      });
      await user.click(screen.getByRole("button", { name: BTN.showSetupCommand }));
      expect(screen.queryByText(/wardyn setup wall/)).not.toBeInTheDocument();
      expect(screen.getByText(/k8s\.runtimeClasses\.CC2/)).toBeInTheDocument();
    });

    it("a k8s driver's still-not-detected line is RuntimeClass-shaped, never `docker info`", async () => {
      const status = k8sStatus({
        runner: { driver: "k8s", confinement_classes: ["CC1", "CC3"], confinement_substrates: { CC1: "k8s/(default)", CC3: "k8s/kata-qemu" } },
      });
      const { rerender } = renderStep({ status, recheckToken: 0 });
      await user.click(screen.getByRole("button", { name: BTN.showSetupCommand }));
      rerender(<EnvironmentStep status={status} selected={null} onSelect={vi.fn()} recheckToken={1} />);
      expect(screen.queryByText(/docker info/)).not.toBeInTheDocument();
      expect(screen.getByText(/no RuntimeClass is pinned to CC2/)).toBeInTheDocument();
    });

    it("a KVM-less k8s cluster never marks Vault 'Incompatible here' — a docker host's own /dev/kvm probe says nothing about cluster nodes", () => {
      // CC2 also live so CC3 is the ONLY needs-setup/incompatible column —
      // isolates the assertion to Vault specifically.
      renderStep({
        status: k8sStatus({
          runner: { driver: "k8s", confinement_classes: ["CC1", "CC2"], confinement_substrates: { CC1: "k8s/(default)", CC2: "k8s/runsc" } },
          platform: { os: "linux", wsl: false, kvm: false },
        }),
      });
      expect(screen.queryByText("Incompatible here")).not.toBeInTheDocument();
      expect(screen.queryByText(/doesn't expose \/dev\/kvm/)).not.toBeInTheDocument();
      const vault = screen.getByRole("radio", { name: /Vault/ });
      expect(within(vault.closest("th")!).getByRole("button", { name: BTN.showSetupCommand })).toBeInTheDocument();
    });
  });
});
