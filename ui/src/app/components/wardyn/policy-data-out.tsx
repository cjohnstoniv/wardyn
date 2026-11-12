/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy editor's "Data out" section: the rule for content leaving a run, what each value does, and, for
// a member's own policy, the floor their ceiling already sets (a looser value is struck out, a stricter one
// stays open). It reads and writes the one document the textarea shows, and sets nothing until a value is
// picked: an absent field reads as allowed.
import * as React from "react";
import { policies as policiesApi } from "../../lib/api/policies";
import type { RunPolicySpec } from "../../lib/types";
import { cn } from "../ui/utils";
import { DATA_OUT as C, type DataOutLevel } from "./copy/data-out";
import { Chip, SectionLabel } from "./primitives";

const rank = (v: DataOutLevel | undefined) => C.LEVELS.findIndex((o) => o.value === v);

type Floor = { level: DataOutLevel; unit?: string };

// The member's own ceiling, from GET /policies/default. A failed read strikes nothing out: the server still clamps.
function useFloor(skip: boolean): Floor | undefined {
  const [floor, setFloor] = React.useState<Floor | undefined>();
  React.useEffect(() => {
    if (skip) return;
    let alive = true;
    Promise.resolve()
      .then(() => policiesApi.getDefaultPolicy())
      .then((p) => {
        if (alive && (p.data_out === "review" || p.data_out === "blocked")) setFloor({ level: p.data_out, unit: p.unit_name });
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [skip]);
  return floor;
}

export function DataOutSection({
  spec,
  onSpecChange,
  ceiling,
}: {
  spec: RunPolicySpec;
  onSpecChange: (next: RunPolicySpec) => void;
  /** The section edits a profile's ceiling: a blocked value there also turns off drives and kept machines. */
  ceiling?: boolean;
}) {
  const floor = useFloor(!!ceiling);
  const lower = floor ? C.LEVELS.filter((o) => rank(o.value) < rank(floor.level)) : [];
  const description =
    spec.data_out === "allowed"
      ? C.ALLOWED
      : spec.data_out === "review"
        ? C.REVIEW
        : spec.data_out === "blocked"
          ? ceiling
            ? C.BLOCKED_GOVERNED
            : C.BLOCKED_RUN
          : C.UNSET;
  return (
    <section aria-labelledby="policy-data-out-label" className="space-y-2" data-testid="policy-data-out">
      <div className="flex items-center gap-2">
        <SectionLabel>
          <span id="policy-data-out-label">{C.LABEL}</span>
        </SectionLabel>
        {floor && <Chip tone="warning">{C.SET_ABOVE}</Chip>}
      </div>
      <div role="radiogroup" aria-labelledby="policy-data-out-label" className="flex flex-wrap gap-1">
        {C.LEVELS.map((o) => {
          const below = lower.some((l) => l.value === o.value);
          return (
            <button
              key={o.value}
              type="button"
              role="radio"
              aria-checked={spec.data_out === o.value}
              disabled={below}
              className={cn(
                "rounded-md border border-border px-3 py-1 text-body",
                spec.data_out === o.value && "border-primary bg-primary-subtle",
                below && "line-through opacity-60",
              )}
              onClick={() => onSpecChange({ ...spec, data_out: o.value })}
            >
              {o.label}
            </button>
          );
        })}
      </div>
      <p className="text-meta text-muted-foreground">{description}</p>
      {floor?.unit && (
        <p className="text-meta text-muted-foreground" data-testid="policy-data-out-floor">
          {C.FLOOR(floor.unit, C.label(floor.level), lower.map((o) => o.label))}
        </p>
      )}
      <p className="text-meta text-muted-foreground">{C.NOT_COVERED}</p>
    </section>
  );
}
