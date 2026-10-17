/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 1 becomes "1st", 3 becomes "3rd". Kept out of the New Run flow so the Runs list (eager) can use it without
// pulling the lazy New Run copy into the entry chunk.
const SUFFIX: Record<string, string> = { one: "st", two: "nd", few: "rd", other: "th" };
const ORDINALS = new Intl.PluralRules("en-US", { type: "ordinal" });
export const ordinal = (n: number): string => `${n}${SUFFIX[ORDINALS.select(n)]}`;
