/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { RunIntent } from "../draft/run-intent";
import { atMinimum, belowMinimum, dropMinimum, minimumFill, startAtMinimum } from "./limits-lens";
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
    const start: RunIntent = { ...BASE, resources: { disk_mib: 20480 } };
    const filled = startAtMinimum(start, MIN);
    expect(atMinimum(filled, MIN)).toBe(true);
    expect(dropMinimum(filled, minimumFill(start, MIN)).resources).toEqual({ disk_mib: 20480 });
    const raisedStart: RunIntent = { ...BASE, resources: { cpu_millis: 6000, memory_mib: 8192 } };
    const raised = startAtMinimum(raisedStart, MIN);
    expect(dropMinimum(raised, minimumFill(raisedStart, MIN))).toBe(raised);
  });

  it("keeps values the person set equal to the minimum before choosing macOS, and takes back only the figure it added", () => {
    const explicit: RunIntent = { ...BASE, resources: { cpu_millis: 4000, memory_mib: 8192 } };
    expect(minimumFill(explicit, MIN)).toEqual({});
    expect(dropMinimum(startAtMinimum(explicit, MIN), minimumFill(explicit, MIN)).resources).toEqual({ cpu_millis: 4000, memory_mib: 8192 });
    const memoryOnly: RunIntent = { ...BASE, resources: { memory_mib: 8192 } };
    expect(minimumFill(memoryOnly, MIN)).toEqual({ cpu_millis: 4000 });
    expect(dropMinimum(startAtMinimum(memoryOnly, MIN), minimumFill(memoryOnly, MIN)).resources).toEqual({ memory_mib: 8192 });
    const changed = { ...startAtMinimum(BASE, MIN), resources: { cpu_millis: 5000, memory_mib: 8192 } };
    expect(dropMinimum(changed, minimumFill(BASE, MIN)).resources).toEqual({ cpu_millis: 5000 });
    expect(dropMinimum(startAtMinimum(BASE, MIN), undefined).resources).toEqual({ cpu_millis: 4000, memory_mib: 8192 });
  });

  it("reads an ask under the minimum on either figure, and an unset one as not under", () => {
    expect(belowMinimum({ ...BASE, resources: { cpu_millis: 2000, memory_mib: 8192 } }, MIN)).toBe(true);
    expect(belowMinimum({ ...BASE, resources: { cpu_millis: 4000, memory_mib: 4096 } }, MIN)).toBe(true);
    expect(belowMinimum({ ...BASE, resources: { cpu_millis: 4000, memory_mib: 8192 } }, MIN)).toBe(false);
    expect(belowMinimum(BASE, MIN)).toBe(false);
  });
});
