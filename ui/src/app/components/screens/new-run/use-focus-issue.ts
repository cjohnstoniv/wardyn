/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { rowIdFromDomId } from "./access-rows-model";
import type { LaunchIssue } from "./new-run-launch-gates";

/**
 * The link above Launch for an issue: shows the panel that owns it and puts
 * focus on the control that fixes it. An Access row's issue also opens that
 * row first (one is open at a time), so what the sentence points at is on
 * screen when focus lands on it. `reveal` is the panel nav's own.
 */
export function useFocusIssue(
  reveal: (issue: Pick<LaunchIssue, "panel" | "focus">) => void,
  openRow: (id: string) => void,
) {
  return React.useCallback(
    (issue: Pick<LaunchIssue, "panel" | "focus">) => {
      const row = rowIdFromDomId(issue.focus);
      if (row !== undefined) openRow(row);
      reveal(issue);
    },
    [reveal, openRow],
  );
}
