/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

/** Make's `.inline-choice` box and its label (snap-2125Z index.css:139-141). */
export const INLINE_CHOICE = "mt-3 flex flex-col rounded-lg bg-muted p-4";
export const INLINE_LABEL = "mb-[7px] text-xs font-semibold";

/**
 * The row a picker shows: the one matching `isSelected`, else the first, as Make's select
 * defaults to its first option. The shown row is reported through `onPick` (keep it stable).
 */
export function useShown<T>(rows: readonly T[], isSelected: (row: T) => boolean, onPick: (row: T) => void): T | undefined {
  const shown = rows.find(isSelected) ?? rows[0];
  React.useEffect(() => {
    if (shown !== undefined) onPick(shown);
  }, [shown, onPick]);
  return shown;
}
