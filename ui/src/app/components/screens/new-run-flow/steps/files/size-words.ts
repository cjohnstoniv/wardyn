/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A main drive size as words. Under 0.1 GiB one decimal would read a valid size as zero, so sizes are whole MiB
// there and GiB with one decimal from 0.1 GiB up. Caps and clamps follow the same rule.

import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { mibToGibText, sizeField, underTenthGiB } from "../../lenses/files-lens";

/** A size as the size field reads it: whole MiB under 0.1 GiB, else GiB with one decimal. */
export const fieldSizeWords = (mib: number, rounding: "round" | "floor" = "round"): string =>
  underTenthGiB(mib) ? NR_SHARED.MIB(mib) : NR_SHARED.GIB(mibToGibText(mib, rounding));

/**
 * A cap or a clamp reads by the same rule, rounded down: it must never claim more room than there is, so 1000 MiB is
 * "0.9 GiB" and 5000 MiB is "4.8 GiB". The exact MiB is the "Exactly N MiB" caption's job.
 */
export const sizeWords = (mib: number): string => fieldSizeWords(mib, "floor");

/**
 * A stored size in words for the summary, the mount map and Review: it reads as the field does, so a size clamped to
 * a cap (which the field shows rounded down) is not rounded up past that cap. `shown` is the field's own text.
 */
export function storedSizeWords(mib: number, shown?: string): string {
  const floored = shown !== undefined && shown === sizeField(mib, "floor").text && shown !== sizeField(mib, "round").text;
  return fieldSizeWords(mib, floored ? "floor" : "round");
}
