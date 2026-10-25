/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns resources.cpu_millis and resources.memory_mib (Review's Limits group), and a macOS VM's pre-fill of them.

import type { RunIntent } from "../draft/run-intent";
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

/** True when the ask is exactly the macOS minimum: the pre-fill the person has not changed. */
export const atMinimum = (intent: RunIntent, min: MacOSMinimum): boolean =>
  intent.resources?.cpu_millis === min.cpuMillis && intent.resources?.memory_mib === min.memoryMib;

/** Leaving macOS takes back the pre-fill, and only an ask still exactly the minimum. */
export function dropMinimum(intent: RunIntent, min: MacOSMinimum): RunIntent {
  if (!atMinimum(intent, min)) return intent;
  const { cpu_millis: _cpu, memory_mib: _memory, ...rest } = intent.resources ?? {};
  return withKeys(intent, { resources: Object.keys(rest).length ? rest : undefined });
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
