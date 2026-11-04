/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-3, the Desktop tile (km3-desktop-before / -after): a disk kept on purpose is the machine's, never a failed removal.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { KEEP_RUN } from "../../wardyn/copy/keep-machine";
import { MacOSDiskNote, macOSRunFactsOf } from "./macos-run-facts";

afterEach(cleanup);

const MACHINE = { id: "m-dev", name: "dev", keep_at_end: true, scrubbed: true, runner_name: "Maya's MacBook", deleted: false };

describe("macOS run facts: a kept machine's disk", () => {
  it("km3-desktop-after: reason kept_machine reads K-43, not a failed removal", () => {
    const facts = macOSRunFactsOf({ macos_disk: { state: "kept", reason: "kept_machine" }, machine: MACHINE });
    expect(facts?.disk).toEqual({ state: "kept", mac: "Maya's MacBook", name: "dev" });
    render(<MacOSDiskNote disk={facts?.disk} />);
    expect(screen.getByText(KEEP_RUN.DISK_KEPT("Maya's MacBook", "dev"))).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("with no machine to name, it says nothing rather than a failure", () => {
    expect(macOSRunFactsOf({ macos_disk: { state: "kept", reason: "kept_machine" } })).toBeUndefined();
  });

  it("km3-desktop-before: any other reason is still the failed removal", () => {
    const facts = macOSRunFactsOf({ macos_disk: { state: "kept", reason: "remove_failed" }, machine: MACHINE });
    expect(facts?.disk).toMatchObject({ state: "failed" });
    render(<MacOSDiskNote disk={facts?.disk} />);
    expect(screen.getByRole("alert")).toHaveTextContent(DESKTOP.diskRemoveFailed(DESKTOP.diskReasonDeleteFailed));
  });
});
