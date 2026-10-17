/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { EffectiveRunLimits } from "../../../../lib/types/runner-pools";
import { resolvedLifetime } from "./format";

const amount = (value: number, unlimited = false) => ({ value, unlimited });
const limit = (def: ReturnType<typeof amount>, cap: ReturnType<typeof amount>) => ({ default: def, cap });
const limits = (lifetime: ReturnType<typeof limit>, noEndAllowed = false) =>
  ({ lifetime_sec: lifetime, no_end_allowed: noEndAllowed }) as unknown as EffectiveRunLimits;

describe("resolvedLifetime", () => {
  it("is no end when nothing was asked and the default is no end, even under a finite cap", () => {
    expect(resolvedLifetime({}, limits(limit(amount(0, true), amount(86400))))).toBe("none");
  });

  it("is the asked lifetime, held to the cap, when one was asked", () => {
    expect(resolvedLifetime({ ends_after_sec: 7200 }, limits(limit(amount(0, true), amount(3600))))).toBe(3600);
  });

  it("is the default when it is finite", () => {
    expect(resolvedLifetime({}, limits(limit(amount(1800), amount(86400))))).toBe(1800);
  });

  it("does not promise no end to a run that asked for it where it is not allowed", () => {
    expect(resolvedLifetime({ no_end: true }, limits(limit(amount(0, true), amount(86400)), false))).toBeUndefined();
  });

  it("is no end for a run that asked for it where it is allowed", () => {
    expect(resolvedLifetime({ no_end: true }, limits(limit(amount(1800), amount(86400)), true))).toBe("none");
  });
});
