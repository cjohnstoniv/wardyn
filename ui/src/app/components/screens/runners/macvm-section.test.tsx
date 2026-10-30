/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A Mac runner's macOS virtual machines section and card badge (mocks M-R1, M-R2). The strings are written
// out, not read from the copy module, so a drift in them fails this suite.
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import type { MacVMCaps } from "../../../lib/types/runner-macvm";
import type { SetupStatus } from "../../../lib/types/setup";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { MacVMBadge, MacVMSection } from "./macvm-section";
import { macVMBadge } from "./macvm-view";

afterEach(cleanup);

const ready: MacVMCaps = {
  max: 2,
  min_cpus: 4,
  min_memory_mib: 8192,
  harnesses: ["claude-code", "codex-cli", "none"],
  desktop: "control",
  base: { state: "ready", guest_os: "macOS 27.0.1 (26A434)" },
};
const perRun = { cpu_millis: 4000, memory_mib: 8192, disk_mib: 0 };
// The harness names are the New Run picker's: the setup status roster's own display names.
const status = { harnesses: [{ id: "claude-code", display: "Claude Code" }, { id: "codex-cli", display: "Codex CLI" }, { id: "none", display: "Your own tools" }] } as SetupStatus;
const section = (caps: MacVMCaps | undefined, over: Partial<Parameters<typeof MacVMSection>[0]> = {}) =>
  render(
    <ModelAccessProvider status={status} onRefresh={() => {}}>
      <MacVMSection caps={caps} mac active={1} perRun={perRun} runner="mac-studio-07" {...over} />
    </ModelAccessProvider>,
  );

describe("M-R2 runner card badge", () => {
  it("1: macOS VMs 1/2 with one run on it", () => {
    render(<MacVMBadge caps={ready} active={1} perRun={perRun} />);
    expect(screen.getByText("macOS VMs 1/2")).toBeInTheDocument();
  });
  it("2: warning tone at the limit, 2/2", () => {
    expect(macVMBadge(ready, 2, perRun)).toEqual({ tone: "warning", text: "macOS VMs 2/2" });
    expect(macVMBadge(ready, 1, perRun)?.tone).toBe("neutral");
  });
  it("3: preparing with its percent", () => {
    expect(macVMBadge({ max: 2, base: { state: "preparing", progress: 42 } }, 1)).toEqual({ tone: "info", text: "macOS VMs · preparing 42%" });
  });
  it("4: not offered on a refusal, a base that is not ready or a per-run size under the minimum", () => {
    const text = "macOS VMs · not offered";
    expect(macVMBadge({ ...ready, reason: "ledger_damaged" }, 0, perRun)?.text).toBe(text);
    expect(macVMBadge({ max: 0, base: { state: "failed", error: "x" } }, 0)?.text).toBe(text);
    expect(macVMBadge({ max: 0, base: { state: "update_needed" } }, 0)?.text).toBe(text);
    expect(macVMBadge(ready, 0, { ...perRun, memory_mib: 6144 })?.text).toBe(text);
  });
  it("shows no badge without caps (off, or not a Mac)", () => {
    const { container } = render(<MacVMBadge caps={undefined} active={0} />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("M-R1 runner drawer section", () => {
  it("0: no section on a runner that is not a Mac and sends no caps", () => {
    const { container } = section(undefined, { mac: false });
    expect(container).toBeEmptyDOMElement();
  });
  it("1: off", () => {
    const { container } = section(undefined);
    expect(screen.getByRole("heading", { name: "macOS virtual machines" })).toBeInTheDocument();
    expect(screen.getByText("Reported by this runner")).toBeInTheDocument();
    expect(screen.getByText("Off")).toBeInTheDocument();
    expect(container.textContent).toContain("Off on this Mac. Turn it on with wardyn-runnerd setup on the Mac, or with the MacOSVM key in its managed preferences.");
    expect(container.querySelectorAll("code, .font-mono").length).toBeGreaterThan(0);
  });
  it("10: ready, with the facts and the licence hint", () => {
    const { container } = section(ready);
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByText("Ready: macOS 27.0.1 (26A434)")).toBeInTheDocument();
    for (const t of ["Guest", "macOS 27.0.1 (26A434)", "Running now: 1 of 2", "Smallest run it can take", "4 CPUs · 8 GB memory", "Harnesses", "Claude Code, Codex CLI, Your own tools", "Desktop", "Opens from the run page"]) {
      expect(screen.getByText(t)).toBeInTheDocument();
    }
    expect(container.textContent).toContain("Apple's licence allows 2 macOS VMs on a Mac. Preparing or updating macOS uses one of them.");
  });
  it("11: desktop observe-only", () => {
    section({ ...ready, desktop: "observe" });
    expect(screen.getByText("Desktop: view only")).toBeInTheDocument();
    expect(screen.getByText("The macOS desktop isn't available on this Mac yet: macOS turned Screen Sharing on for viewing only.")).toBeInTheDocument();
    expect(screen.queryByText("Opens from the run page")).not.toBeInTheDocument();
    expect(screen.queryByText(/Apple's licence/)).not.toBeInTheDocument();
  });
  it("9b: preparing shows the chip and its percent bar", () => {
    section({ max: 2, base: { state: "preparing", progress: 42 } });
    expect(screen.getByText("Preparing")).toBeInTheDocument();
    expect(screen.getByRole("img", { name: "42%" })).toBeInTheDocument();
  });
  it("13: update needed: the tools update", () => {
    section({ ...ready, base: { state: "update_needed", guest_os: "macOS 27.0.1 (26A434)" } });
    expect(screen.getByText("Update needed")).toBeInTheDocument();
    expect(screen.getByText("This Mac's runner was upgraded, so the macOS base's tools are being updated. It offers no macOS VMs until that finishes.")).toBeInTheDocument();
  });
  it("13b: update needed: a base of an older format is prepared again, and no tools update is promised", () => {
    const { container } = section({ ...ready, base: { state: "update_needed", failure: "format", guest_os: "macOS 27.0.1 (26A434)" } });
    expect(screen.getByText("Update needed")).toBeInTheDocument();
    expect(container.textContent).toContain(
      "This Mac's runner was upgraded and can't use the macOS base it has. Run wardyn-runnerd macos-vm prepare to prepare the base again. It offers no macOS VMs until then.",
    );
    expect(container.textContent).not.toContain("tools are being updated");
  });
  it("15: failed names the runner's own error", () => {
    section({ max: 0, base: { state: "failed", error: "connection reset by peer" } });
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.getByText("connection reset by peer")).toBeInTheDocument();
  });
  it("16: per-run size below the minimum, memory first and the CPU variant beneath", () => {
    const { container } = section(ready, { perRun: { cpu_millis: 2000, memory_mib: 6144, disk_mib: 0 } });
    expect(screen.getByText("Not offered")).toBeInTheDocument();
    expect(container.textContent).toContain("Per-run memory on this runner is 6 GB; a macOS VM needs at least 8 GB. Its owner raises it to 8 GB or more in Settings → My runner, or with wardyn runner set mac-studio-07 --per-run-memory 8.");
    expect(container.textContent).toContain("Per-run CPU on this runner is 2 CPUs; a macOS VM needs at least 4. Its owner raises it to 4 or more in Settings → My runner, or with wardyn runner set mac-studio-07 --per-run-cpu 4.");
  });
  it("19 / R10: not offered words the ledger reason from the approved text, not the runner's sentence", () => {
    const { container } = section({ max: 0, reason: "ledger_damaged", refusal: "a sentence only the runner wrote", base: { state: "none" } });
    expect(screen.getByText("Not offered")).toBeInTheDocument();
    expect(screen.getByText("This Mac's macOS VM ledger is missing or damaged. Reinstall the Wardyn runner package.")).toBeInTheDocument();
    expect(container.textContent).not.toContain("a sentence only the runner wrote");
  });
});

// The runner's own refusal text is Go's. These pin it to the approved copy.
describe("Go parity", () => {
  const root = (() => {
    let dir = resolve(process.cwd());
    for (let i = 0; i < 8; i++) {
      if (existsSync(join(dir, "go.mod"))) return dir;
      dir = dirname(dir);
    }
    throw new Error("go.mod not found");
  })();
  const go = readFileSync(join(root, "cmd/wardyn-runnerd/macos_vm.go"), "utf8");
  it("the ledger refusal is the approved text", () => {
    expect(go).toContain('const macOSVMLedgerDamaged = "This Mac\'s macOS VM ledger is missing or damaged. Reinstall the Wardyn runner package."');
  });
  it("MacVMCaps is mirrored key for key", () => {
    const wire = readFileSync(join(root, "internal/runnerwire/payload.go"), "utf8");
    const tags = (name: string) => {
      const m = new RegExp(`type\\s+${name}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(wire);
      return [...(m?.[1] ?? "").matchAll(/json:"([^",]+)/g)].map((t) => t[1]).sort();
    };
    const ts = readFileSync(join(root, "ui/src/app/lib/types/runner-macvm.ts"), "utf8");
    const keys = (name: string) => {
      const m = new RegExp(`export interface ${name} \\{([\\s\\S]*?)\\n\\}`).exec(ts);
      return [...(m?.[1] ?? "").matchAll(/^\s{2}([a-z_]+)\??:/gm)].map((t) => t[1]).sort();
    };
    expect(keys("MacVMCaps")).toEqual(tags("MacVMCaps"));
    expect(keys("MacVMBase")).toEqual(tags("MacVMBase"));
  });
  it("MacVMFailure is mirrored value for value", () => {
    const wire = readFileSync(join(root, "internal/runnerwire/payload.go"), "utf8");
    const goValues = [...wire.matchAll(/^\s*MacVMFailure\w+\s*=\s*"([^"]+)"/gm)].map((m) => m[1]).sort();
    const ts = readFileSync(join(root, "ui/src/app/lib/types/runner-macvm.ts"), "utf8");
    const union = /export type MacVMFailure = ([^;]+);/.exec(ts)?.[1] ?? "";
    const tsValues = [...union.matchAll(/"([^"]+)"/g)].map((m) => m[1]).sort();
    expect(goValues).toContain("format");
    expect(tsValues).toEqual(goValues);
  });
});
