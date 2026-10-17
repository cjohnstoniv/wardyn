/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A main drive size as words, for the cap, the clamps, the summary, the mount map and Review: exact, never rounded.
// GiB only when the size is a whole tenth of a GiB, else whole MiB, so a 1000 MiB cap reads "1000 MiB" and a size is
// never read as more or less room than it is. Only halves of a GiB are whole tenths of one.

import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";

export const sizeWords = (mib: number): string => (mib % 512 === 0 ? NR_SHARED.GIB(mib / 1024) : NR_SHARED.MIB(mib));
