/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns resources.cpu_millis and resources.memory_mib (Review's Limits group), and a macOS VM's pre-fill of them.

import type { GuidedFacts, RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";

export const setCpu = (intent: RunIntent, cpuMillis: number): RunIntent =>
  withKeys(intent, { resources: { ...intent.resources, cpu_millis: cpuMillis } });

export const setMemory = (intent: RunIntent, memoryMib: number): RunIntent =>
  withKeys(intent, { resources: { ...intent.resources, memory_mib: memoryMib } });

/** The smallest run a macOS VM takes: the base's own minimum. */
export interface MacOSMinimum { cpuMillis: number; memoryMib: number }

/** A macOS VM starts the CPU and memory ask at the base's minimum, visibly; an ask already above it stays. */
export const startAtMinimum = (intent: RunIntent, min: MacOSMinimum): RunIntent =>
  withKeys(intent, {
    resources: {
      ...intent.resources,
      cpu_millis: Math.max(intent.resources?.cpu_millis ?? 0, min.cpuMillis),
      memory_mib: Math.max(intent.resources?.memory_mib ?? 0, min.memoryMib),
    },
  });

/** True when the ask is exactly the macOS minimum: the test for Review's "Mac minimum" chip, not a record of how it got there. */
export const atMinimum = (intent: RunIntent, min: MacOSMinimum): boolean =>
  intent.resources?.cpu_millis === min.cpuMillis && intent.resources?.memory_mib === min.memoryMib;

/** The figures choosing macOS filled in, with the numbers it set: one the ask already held at or above the minimum is the person's. */
export const minimumFill = (intent: RunIntent, min: MacOSMinimum): NonNullable<GuidedFacts["macosFill"]> => ({
  ...((intent.resources?.cpu_millis ?? 0) < min.cpuMillis && { cpu_millis: min.cpuMillis }),
  ...((intent.resources?.memory_mib ?? 0) < min.memoryMib && { memory_mib: min.memoryMib }),
});

/** The fill without one figure: a figure the person sets is theirs, even when they set it back to the number filled in. */
export function disownFill(fill: GuidedFacts["macosFill"], key: "cpu_millis" | "memory_mib"): GuidedFacts["macosFill"] {
  if (fill?.[key] === undefined) return fill;
  const { [key]: _owned, ...rest } = fill;
  return Object.keys(rest).length ? rest : undefined;
}

/** Leaving macOS takes back the figures the pre-fill added, and only while each is still the number it filled in. */
export function dropMinimum(intent: RunIntent, fill: GuidedFacts["macosFill"]): RunIntent {
  const { cpu_millis: cpu, memory_mib: memory, ...rest } = intent.resources ?? {};
  const dropCpu = fill?.cpu_millis !== undefined && cpu === fill.cpu_millis;
  const dropMemory = fill?.memory_mib !== undefined && memory === fill.memory_mib;
  if (!dropCpu && !dropMemory) return intent;
  const kept = { ...rest, ...(!dropCpu && cpu !== undefined && { cpu_millis: cpu }), ...(!dropMemory && memory !== undefined && { memory_mib: memory }) };
  return withKeys(intent, { resources: Object.keys(kept).length ? kept : undefined });
}

/** True when either figure of the ask is under the macOS minimum. */
export const belowMinimum = (intent: RunIntent, min: MacOSMinimum): boolean =>
  (intent.resources?.cpu_millis ?? min.cpuMillis) < min.cpuMillis || (intent.resources?.memory_mib ?? min.memoryMib) < min.memoryMib;

/** A CPU count as typed ("2", "0.5") in milli-CPU, or undefined when it is not a positive number. */
export function parseCpuMillis(text: string): number | undefined {
  const n = Number(text);
  return text.trim() !== "" && Number.isFinite(n) && Math.round(n * 1000) > 0 ? Math.round(n * 1000) : undefined;
}

/** A memory size as typed, in whole MiB, or undefined when it is not a positive whole number. */
export function parseMemoryMib(text: string): number | undefined {
  const n = Number(text);
  return text.trim() !== "" && Number.isInteger(n) && n > 0 ? n : undefined;
}
