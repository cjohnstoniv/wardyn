/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Archive, Check, CircleX, ShieldX, Square } from "lucide-react";
import type { RunState } from "../../lib/types";
import { Chip, RunStateBadge } from "./primitives";

// One run-status badge for the board, the table, and the detail header, so the
// same state always reads the same way. Terminal outcomes are differentiated by
// an ICON (C4); solid saturated red is reserved EXCLUSIVELY for Killed — the
// enforcement outcome. Non-terminal states delegate to RunStateBadge (the
// dot + pulse chip in primitives) so "live" runs keep their animated dot.
export function RunStatusBadge({ state }: { state: RunState }) {
  switch (String(state)) {
    case "COMPLETED":
      return <Chip tone="success" className="gap-1"><Check className="size-3" />Completed</Chip>;
    case "FAILED":
      return <Chip tone="danger" className="gap-1"><CircleX className="size-3" />Failed</Chip>;
    case "STOPPED":
      return <Chip tone="neutral" className="gap-1"><Square className="size-3" />Stopped</Chip>;
    case "ARCHIVED":
      return <Chip tone="neutral" className="gap-1"><Archive className="size-3" />Archived</Chip>;
    case "KILLED":
      // Enforcement — the one solid-red chip (matches the Audit kill event).
      return (
        <Chip tone="danger" className="gap-1 border-danger bg-danger text-danger-foreground">
          <ShieldX className="size-3" />
          Killed
        </Chip>
      );
    default:
      return <RunStateBadge state={state} />;
  }
}
