/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Owns resources.cpu_millis and resources.memory_mib (Review's Limits group).

import type { RunIntent } from "../draft/run-intent";
import { withKeys } from "./lens-keys";

export const setCpu = (intent: RunIntent, cpuMillis: number): RunIntent =>
  withKeys(intent, { resources: { ...intent.resources, cpu_millis: cpuMillis } });

export const setMemory = (intent: RunIntent, memoryMib: number): RunIntent =>
  withKeys(intent, { resources: { ...intent.resources, memory_mib: memoryMib } });

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
