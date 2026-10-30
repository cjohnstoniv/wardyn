/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A Mac's bases on its runner drawer (board J6, "The runner's base list").
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import type { MacVMCaps, MacVMDerivedBase } from "../../../lib/types/runner-macvm";
import type { SetupStatus } from "../../../lib/types/setup";
import { MAC_BASES } from "../../wardyn/copy/mac-bases";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { MacVMSection } from "./macvm-section";

afterEach(cleanup);

const status = { harnesses: [{ id: "claude-code", display: "Claude Code" }, { id: "none", display: "Your own tools" }] } as SetupStatus;
const caps = (bases?: MacVMDerivedBase[]): MacVMCaps => ({
  max: 2,
  min_cpus: 4,
  min_memory_mib: 8192,
  harnesses: ["claude-code", "none"],
  desktop: "control",
  base: { state: "ready", guest_os: "macOS 26.6.2 (25G83)" },
  bases,
});
const show = (c: MacVMCaps) =>
  render(
    <ModelAccessProvider status={status} onRefresh={() => {}}>
      <MacVMSection caps={c} mac active={1} runner="mac-studio-07" />
    </ModelAccessProvider>,
  );
const list = () => screen.getByRole("heading", { name: MAC_BASES.B10 }).parentElement!;

describe("drawer bases list (J6-b)", () => {
  it("lists Standard and each other base: MAC-B10, MAC-B11, a Ready chip, the Rebuilding chip with its percent, and MAC-B13", () => {
    show(caps([
      { name: "qa-profiles", state: "rebuilding", items: 2, progress: 60 },
      { name: "xcode-16", state: "ready", items: 3 },
    ]));
    const rows = within(list()).getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      "Standard · macOS 26.6.2 (25G83) · Claude Code, Your own toolsReady",
      `qa-profiles · ${MAC_BASES.B11(2)}${MAC_BASES.B12}`,
      `xcode-16 · ${MAC_BASES.B11(3)}Ready`,
    ]);
    expect(within(rows[1]).getByText(MAC_BASES.B12)).toHaveAttribute("title", "60%");
    expect(screen.getByText(MAC_BASES.B13(2))).toBeInTheDocument();
  });

  it("says plus 1 item for one item, and shows a failed rebuild as Failed", () => {
    show(caps([{ name: "solo", state: "failed", items: 1 }]));
    expect(within(list()).getByText(/plus 1 item$/)).toBeInTheDocument();
    expect(within(list()).getByText("Failed")).toBeInTheDocument();
  });

  it("shows nothing for a Mac with no other base, so its drawer is as it was", () => {
    show(caps(undefined));
    expect(screen.queryByText(MAC_BASES.B10)).toBeNull();
    cleanup();
    show(caps([]));
    expect(screen.queryByText(MAC_BASES.B10)).toBeNull();
    expect(screen.queryByText(MAC_BASES.B13(2))).toBeNull();
  });
});
