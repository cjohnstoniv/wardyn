/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A main drive size as words. Under 0.1 GiB one decimal would read a valid size as zero, so sizes are whole MiB
// there and GiB with one decimal from 0.1 GiB up; a cap or a clamp is exact, so it reads in GiB only when it is a
// whole tenth of a GiB.

import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { mibToGibText, underTenthGiB } from "../../lenses/files-lens";

/** A stored size as the size field reads it. */
export const fieldSizeWords = (mib: number, rounding: "round" | "floor" = "round"): string =>
  underTenthGiB(mib) ? NR_SHARED.MIB(mib) : NR_SHARED.GIB(mibToGibText(mib, rounding));

/** A cap or clamp: "{n} GiB" when it is a whole tenth of a GiB, else "{n} MiB". Only halves of a GiB are whole MiB tenths. */
export const sizeWords = (mib: number): string => (mib % 512 === 0 ? NR_SHARED.GIB(mib / 1024) : NR_SHARED.MIB(mib));
