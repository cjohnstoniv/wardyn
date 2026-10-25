/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../draft/run-intent";
import { atMinimum, belowMinimum, dropMinimum, startAtMinimum } from "./limits-lens";
import { chooseMachineKind } from "./run-lens";

const MIN = { cpuMillis: 4000, memoryMib: 8192 };
const BASE: RunIntent = { shape: "agent_task", experience: "interactive", confinement_class: "CC3" };

describe("chooseMachineKind: macOS", () => {
  it("writes macos_vm and drops the container barrier, as a Linux VM does", () => {
    expect(chooseMachineKind(BASE, "macos_vm")).toEqual({ shape: "agent_task", experience: "interactive", machine_kind: "macos_vm" });
    expect(chooseMachineKind(chooseMachineKind(BASE, "macos_vm"), "container").machine_kind).toBeUndefined();
  });
});

describe("the macOS minimum", () => {
  it("starts an unset ask at the minimum and keeps one above it", () => {
    expect(startAtMinimum(BASE, MIN).resources).toEqual({ cpu_millis: 4000, memory_mib: 8192 });
    expect(startAtMinimum({ ...BASE, resources: { cpu_millis: 6000, memory_mib: 4096 } }, MIN).resources).toEqual({ cpu_millis: 6000, memory_mib: 8192 });
  });

  it("takes back an untouched pre-fill only, keeping any other resource key", () => {
    const filled = startAtMinimum({ ...BASE, resources: { disk_mib: 20480 } }, MIN);
    expect(atMinimum(filled, MIN)).toBe(true);
    expect(dropMinimum(filled, MIN).resources).toEqual({ disk_mib: 20480 });
    const raised = startAtMinimum({ ...BASE, resources: { cpu_millis: 6000, memory_mib: 8192 } }, MIN);
    expect(dropMinimum(raised, MIN)).toBe(raised);
  });

  it("reads an ask under the minimum on either figure, and an unset one as not under", () => {
    expect(belowMinimum({ ...BASE, resources: { cpu_millis: 2000, memory_mib: 8192 } }, MIN)).toBe(true);
    expect(belowMinimum({ ...BASE, resources: { cpu_millis: 4000, memory_mib: 4096 } }, MIN)).toBe(true);
    expect(belowMinimum({ ...BASE, resources: { cpu_millis: 4000, memory_mib: 8192 } }, MIN)).toBe(false);
    expect(belowMinimum(BASE, MIN)).toBe(false);
  });
});
