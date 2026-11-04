/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-3, the Desktop tile's sign-in note (km3-desktop-signins-before / -after): K-45 replaces the run-disk note on a run
// that keeps its machine, and only there.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { KEEP_RUN } from "../../wardyn/copy/keep-machine";
import { DesktopSignInsNote, type DesktopSignIns } from "./desktop-sign-ins";

afterEach(cleanup);

const s = (store: "run_disk" | "memory"): DesktopSignIns => ({ view: { store, paused: false, can_pause: true, agent: "Claude Code" }, busy: false, error: null, toggle: vi.fn() });

describe("Desktop sign-in note on a kept machine", () => {
  it("km3-desktop-signins-before: a run that keeps nothing says the run disk's note", () => {
    render(<DesktopSignInsNote s={s("run_disk")} />);
    expect(screen.getByText(DESKTOP.signInsNoteRunDisk)).toBeInTheDocument();
  });
  it("km3-desktop-signins-after: a run that keeps its machine says K-45", () => {
    render(<DesktopSignInsNote s={s("run_disk")} keptOn="Maya's MacBook" />);
    expect(screen.getByText(KEEP_RUN.SIGN_INS_NOTE("Maya's MacBook"))).toBeInTheDocument();
    expect(screen.queryByText(DESKTOP.signInsNoteRunDisk)).toBeNull();
  });
  it("a store in memory keeps its own note, kept or not", () => {
    render(<DesktopSignInsNote s={s("memory")} keptOn="Maya's MacBook" />);
    expect(screen.getByText(DESKTOP.signInsNote)).toBeInTheDocument();
  });
});
