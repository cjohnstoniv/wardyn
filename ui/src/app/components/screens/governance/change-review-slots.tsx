/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Four empty mount points on a change's own page, for the review assistant to fill by editing this file only. Each
// takes the change and renders nothing today: no copy, no state and no fetch live here.
import type { GovernanceChange } from "../../../lib/types";

type SlotProps = { change: GovernanceChange };

// While this is false the page's wide grid has two tracks. A true value adds a third column, after the approvals
// panel, for the assistant.
export const REVIEW_ASSISTANT_COLUMN = false;

/** The third column beside the approvals panel. */
export function ReviewAssistantColumn(_props: SlotProps) {
  return null;
}

/** The header's button that opens the assistant in a sheet, at every width. */
export function ReviewAssistantSheetButton(_props: SlotProps) {
  return null;
}

/** Directly under the impact section, in the Impact tab and in the stacked order. */
export function ReviewReplaySlot(_props: SlotProps) {
  return null;
}

/** In the header beside the kind, operation and target. */
export function ReviewHeaderChip(_props: SlotProps) {
  return null;
}
