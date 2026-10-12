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
