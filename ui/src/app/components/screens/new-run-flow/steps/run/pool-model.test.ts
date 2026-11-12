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
  it("reads GiB only for a whole tenth of a GiB, else whole MiB", () => {
    expect(drive(5000)).toBe("5000 MiB");
    expect(drive(1000)).toBe("1000 MiB");
    expect(drive(10240)).toBe("10 GiB");
    expect(drive(64)).toBe("64 MiB");
  });
});

describe("the pool's lifetime limit", () => {
  const life = (value: number) => {
    const l = limits(1000) as unknown as Record<string, unknown>;
    l.lifetime_sec = { default: { value: 3600 }, cap: { value }, cap_source: "governance" };
    return limitCells(l as unknown as EffectiveRunLimits, undefined).find((c) => c.key === "lifetime")?.value;
  };
  it("reads the server's own ceiling as no maximum, never as 596523.24 hours", () => {
    expect(life(2147483647)).toBe("No maximum");
  });
  it("still reads a real cap in hours", () => {
    expect(life(86400)).toBe("24 hours");
  });
});
