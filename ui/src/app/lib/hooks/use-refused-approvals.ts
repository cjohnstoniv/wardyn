/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The requests the server refused to this viewer with capability_egress_host. The list cannot tell who holds the
// host, so the first refusal is what turns that request's buttons off; the live strip does the same inline.
// A personal_walls_host refusal is kept apart (wallsRefused): the run owner's own walls deny the host, so the row
// reads as walls-blocked rather than as a withheld grant. A wall_preset_host refusal is the same block set by the
// run's wall preset, kept in presetRefused so a row can say which.
import * as React from "react";
import { HttpError } from "../api/core";
import { WALL_PRESET_REASON } from "../api/wall-presets";

export function useRefusedApprovals(): {
  refused: ReadonlySet<string>;
  wallsRefused: ReadonlySet<string>;
  presetRefused: ReadonlySet<string>;
  noteRefusal: (id: string, e: unknown) => void;
} {
  const [refused, setRefused] = React.useState<ReadonlySet<string>>(new Set());
  const [wallsRefused, setWallsRefused] = React.useState<ReadonlySet<string>>(new Set());
  const [presetRefused, setPresetRefused] = React.useState<ReadonlySet<string>>(new Set());
  const noteRefusal = React.useCallback((id: string, e: unknown) => {
    if (!(e instanceof HttpError) || e.status !== 403) return;
    if (e.reason === "capability_egress_host") setRefused((cur) => new Set(cur).add(id));
    else if (e.reason === "personal_walls_host") setWallsRefused((cur) => new Set(cur).add(id));
    else if (e.reason === WALL_PRESET_REASON.HOST) setPresetRefused((cur) => new Set(cur).add(id));
  }, []);
  return { refused, wallsRefused, presetRefused, noteRefusal };
}
