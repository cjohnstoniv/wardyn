/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { runs } from "../../../lib/api/runs";
import { PUSH_DENY_ALL_SOURCE } from "../../../lib/wall-preset-words";
import type { AuditEvent, RunPolicyView } from "../../../lib/types";

/** The audit trail holds a push refused because every push is refused. */
export const hasDenyAllRefusal = (events: readonly AuditEvent[]): boolean => events.some((e) => e.data?.rule_source === PUSH_DENY_ALL_SOURCE);

/**
 * The run's wall preset as its policy view reads it, fetched once and only when the run has one and its audit trail
 * holds a deny-all refusal: that record says whether the preset is what refuses every push. A failed read says nothing.
 */
export function usePresetRefusal(runId: string, events: readonly AuditEvent[], hasPreset: boolean): RunPolicyView["wall_preset"] {
  const wanted = hasPreset && hasDenyAllRefusal(events);
  const [view, setView] = React.useState<RunPolicyView["wall_preset"]>();
  React.useEffect(() => {
    if (!wanted) return;
    let alive = true;
    runs.getPolicy(runId).then(
      (v) => alive && setView(v.wall_preset),
      () => {},
    );
    return () => {
      alive = false;
    };
  }, [runId, wanted]);
  return wanted ? view : undefined;
}
