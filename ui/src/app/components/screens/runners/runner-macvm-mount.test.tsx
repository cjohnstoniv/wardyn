/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The macOS VM section and badge are on the runner drawer and card, fed from RunnerView (mocks M-R1, M-R2).
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { aheadByHours } from "../../../lib/test-clock";
import type { MacVMCaps } from "../../../lib/types/runner-macvm";
import type { RunnerView } from "../../../lib/types/runners";
import type { SetupStatus } from "../../../lib/types/setup";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { RunnerCard, type FleetRunner } from "./runner-card";
import { RunnerDrawer } from "./runner-drawer";

afterEach(cleanup);

const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
function runner(over: Partial<RunnerView> = {}): RunnerView {
  return {
    id: "r-mac", name: "mac-studio-07", state: "claimed", owner: "dana@example.com", owner_kind: "person", trust_class: "managed_device", online: true,
    key_fingerprint: "SHA256:fp", key_fingerprint_abbreviated: false, created_at: aheadByHours(-72), claimed_at: aheadByHours(-71), kind: "self_hosted",
    posture: { mdm_managed: true, disk_encrypted: true, os: "darwin", os_version: "27.0.1", evidence: "mdm_file" },
    limits: { runs_at_once: 2, total: { cpu_millis: 8000, memory_mib: 20480, disk_mib: 0 }, per_run: { cpu_millis: 4000, memory_mib: 8192, disk_mib: 0 } },
    usage: { online: true, in_use: { runs: 1, cpu_millis: 4000, memory_mib: 8192, disk_mib: 0, macos_vm_runs: 1 }, available: none, disk_capped: true },
    pools: [], ...over,
  };
}
const READY: MacVMCaps = {
  max: 2, min_cpus: 4, min_memory_mib: 8192, harnesses: ["claude-code", "codex-cli", "none"], desktop: "control",
  base: { state: "ready", guest_os: "macOS 27.0.1 (26A434)" },
};
const STATUS = {
  harnesses: [{ id: "claude-code", display: "Claude Code" }, { id: "codex-cli", display: "Codex CLI" }, { id: "none", display: "Your own tools" }],
} as SetupStatus;

const drawer = (r: RunnerView) =>
  render(
    <ModelAccessProvider status={STATUS} onRefresh={() => {}}>
      <RunnerDrawer runner={r} onClose={vi.fn()} onChanged={vi.fn()} onOpenPool={vi.fn()} />
    </ModelAccessProvider>,
  );
const card = (r: RunnerView) => {
  const f: FleetRunner = { kind: "self_hosted", id: r.id, name: r.name, runner: r };
  return render(<RunnerCard f={f} onOpen={vi.fn()} onOpenPool={vi.fn()} />);
};
const section = () => document.querySelector("[data-macvm]") as HTMLElement | null;

describe("runner drawer, macOS virtual machines (M-R1)", () => {
  it("ready: the facts from the runner's caps and its live runs, between Resources and Trust class", () => {
    drawer(runner({ macos_vm: READY }));
    const s = within(section()!);
    expect(s.getByText("Ready: macOS 27.0.1 (26A434)")).toBeInTheDocument();
    expect(s.getByText("Running now: 1 of 2")).toBeInTheDocument();
    expect(s.getByText("4 CPUs · 8 GB memory")).toBeInTheDocument();
    expect(s.getByText("Claude Code, Codex CLI, Your own tools")).toBeInTheDocument();
    const order = [...document.querySelectorAll("[data-resources], [data-macvm], [data-trust]")].map((e) => Object.keys((e as HTMLElement).dataset)[0]);
    expect(order).toEqual(["resources", "macvm", "trust"]);
  });
  it("ready with no macOS VM run counts none, not an unknown", () => {
    drawer(runner({ macos_vm: READY, usage: { ...runner().usage, in_use: { ...runner().usage.in_use, macos_vm_runs: undefined } } }));
    expect(screen.getByText("Running now: 0 of 2")).toBeInTheDocument();
  });
  it("preparing: the chip and its percent", () => {
    drawer(runner({ macos_vm: { max: 2, base: { state: "preparing", progress: 42 } } }));
    expect(within(section()!).getByText("Preparing")).toBeInTheDocument();
    expect(within(section()!).getByRole("img", { name: "42%" })).toBeInTheDocument();
  });
  it("failed: the runner's own error, as it sent it", () => {
    drawer(runner({ macos_vm: { max: 0, base: { state: "failed", error: "connection reset by peer" } } }));
    expect(within(section()!).getByText("Failed")).toBeInTheDocument();
    expect(within(section()!).getByText("connection reset by peer")).toBeInTheDocument();
  });
  it("not offered when the owner's per-run size is under the base's minimum", () => {
    drawer(runner({ macos_vm: READY, limits: { ...runner().limits!, per_run: { cpu_millis: 4000, memory_mib: 6144, disk_mib: 0 } } }));
    expect(within(section()!).getByText("Not offered")).toBeInTheDocument();
    expect(section()!.textContent).toContain("Per-run memory on this runner is 6 GB; a macOS VM needs at least 8 GB.");
  });
  it("no section on a runner that sent no macOS VM caps, a Mac or not", () => {
    drawer(runner());
    expect(section()).toBeNull();
  });
});

describe("runner card, macOS VMs badge (M-R2)", () => {
  it("the count while it offers", () => {
    card(runner({ macos_vm: READY }));
    expect(screen.getByText("macOS VMs 1/2")).toBeInTheDocument();
  });
  it("preparing with its percent", () => {
    card(runner({ macos_vm: { max: 2, base: { state: "preparing", progress: 42 } } }));
    expect(screen.getByText("macOS VMs · preparing 42%")).toBeInTheDocument();
  });
  it("not offered", () => {
    card(runner({ macos_vm: { max: 0, base: { state: "failed", error: "x" } } }));
    expect(screen.getByText("macOS VMs · not offered")).toBeInTheDocument();
  });
  it("no badge without caps", () => {
    card(runner());
    expect(screen.queryByText(/macOS VMs/)).not.toBeInTheDocument();
  });
});
