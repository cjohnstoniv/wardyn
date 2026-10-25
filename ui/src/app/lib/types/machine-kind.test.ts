/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { isMacOSVM, isVMKind, macOSVMsEnabled } from "./machine-kind";
import type { CreateRunInput } from "./run-create";
import { makeMacOSVMRun, makeRun } from "../../../test/factories";

// tsc-checked: a create request may name a macOS VM.
const macCreate: CreateRunInput["machine_kind"] = "macos_vm";

describe("machine kind helpers", () => {
  it("a create request may name a macOS VM (type-checked by tsc)", () => {
    expect(macCreate).toBe("macos_vm");
  });

  it("isVMKind: both VM kinds, neither a container nor absent", () => {
    expect(isVMKind("linux_vm")).toBe(true);
    expect(isVMKind("macos_vm")).toBe(true);
    expect(isVMKind("container")).toBe(false);
    expect(isVMKind(undefined)).toBe(false);
  });

  it("isMacOSVM: only a macos_vm run", () => {
    expect(isMacOSVM(makeMacOSVMRun())).toBe(true);
    expect(isMacOSVM(makeRun({ machine_kind: "linux_vm" }))).toBe(false);
    expect(isMacOSVM(makeRun())).toBe(false);
  });

  it("macOSVMsEnabled: true only when the switch is on; absent, off or no status read as off", () => {
    expect(macOSVMsEnabled({ features: { macos_vms: true } })).toBe(true);
    expect(macOSVMsEnabled({ features: { macos_vms: false } })).toBe(false);
    expect(macOSVMsEnabled({ features: {} })).toBe(false);
    expect(macOSVMsEnabled({})).toBe(false);
    expect(macOSVMsEnabled(null)).toBe(false);
  });
});
