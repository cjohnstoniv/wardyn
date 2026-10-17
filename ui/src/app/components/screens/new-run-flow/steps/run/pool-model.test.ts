/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { EffectiveRunLimits } from "../../../../../lib/types/runner-pools";
import { limitCells } from "./pool-model";

const limits = (diskMib: number) => {
  const one = (value: number) => ({ default: { value }, cap: { value }, cap_source: "pool" });
  return { cpu_millis: one(2000), memory_mib: one(4096), lifetime_sec: one(3600), disk_mib: one(diskMib) } as unknown as EffectiveRunLimits;
};
const drive = (mib: number) => limitCells(limits(mib), undefined).find((c) => c.key === "drive")?.value;

describe("the pool's main drive limit (DR-0160)", () => {
  it("reads GiB with one decimal from 0.1 GiB up, rounded down, and whole MiB below it", () => {
    expect(drive(5000)).toBe("4.8 GiB");
    expect(drive(1000)).toBe("0.9 GiB");
    expect(drive(10240)).toBe("10 GiB");
    expect(drive(64)).toBe("64 MiB");
  });
});
