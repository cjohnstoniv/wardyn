/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The profile editor's Rubric section (0.8 #93/#96) — three posture groups,
// three rows each, one select per row. Split out of profile-editor.tsx so the
// rubric has its own unit test (issue #93's own ask) and a cleaner diff on a
// file that already carries the ceiling + limits sections.
//
// Every product string comes from governance-copy.ts's RUBRIC and
// components/wardyn/autonomy-meta.ts's AUTONOMY_META. This file adds none.
import { AUTONOMY_LEVEL_ORDER, AUTONOMY_RUBRIC_ROW_KEYS, type AutonomyRubric, type AutonomyRubricRowKey } from "../../../lib/api/governance";
import { foldAutonomyRubric, RUBRIC } from "../../../lib/governance-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";

// Radix <Select.Item value=""> throws (runs.tsx's own precedent) — the wire's
// "no cap" is an ABSENT field, not an empty string select value, so the
// control needs a sentinel to round-trip through.
const NO_CAP = "none";

// The three groups, in AUTONOMY_RUBRIC_ROW_KEYS' own fixed order — the same
// order the server folds tied causes in, so the editor's row order matches
// the rail's bound_by sentence order.
const GROUPS: { title: string; hint?: string; keys: AutonomyRubricRowKey[] }[] = [
  { title: RUBRIC.GROUP_EGRESS, keys: AUTONOMY_RUBRIC_ROW_KEYS.slice(0, 3) },
  { title: RUBRIC.GROUP_SECRETS, keys: AUTONOMY_RUBRIC_ROW_KEYS.slice(3, 6) },
  { title: RUBRIC.GROUP_BARRIER, hint: RUBRIC.GROUP_BARRIER_HINT, keys: AUTONOMY_RUBRIC_ROW_KEYS.slice(6, 9) },
];

function RubricRow({
  rowKey,
  value,
  disabled,
  onChange,
}: {
  rowKey: AutonomyRubricRowKey;
  value: AutonomyRubric;
  disabled: boolean;
  onChange: (next: AutonomyRubric) => void;
}) {
  const [label, why] = RUBRIC.ROWS[rowKey];
  const current = value[rowKey] ?? NO_CAP;
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 border-t border-border py-2.5 first-of-type:border-t-0">
      <div className="min-w-0">
        <p className="text-body font-medium text-foreground">{label}</p>
        <p className="mt-0.5 max-w-[56ch] text-xs text-muted-foreground">{why}</p>
      </div>
      <Select
        value={current}
        onValueChange={(v) => onChange({ ...value, [rowKey]: v === NO_CAP ? undefined : v })}
        disabled={disabled}
      >
        <SelectTrigger aria-label={label} className="w-[190px]">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={NO_CAP}>{RUBRIC.NOCAP}</SelectItem>
          {AUTONOMY_LEVEL_ORDER.map((level) => (
            <SelectItem key={level} value={level}>
              {AUTONOMY_META[level].label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

export function ProfileRubric({
  value,
  disabled,
  onChange,
}: {
  value: AutonomyRubric;
  disabled: boolean;
  onChange: (next: AutonomyRubric) => void;
}) {
  const { setKeys, lowest } = foldAutonomyRubric(value);
  return (
    <section className="mt-6">
      <h4 className="text-body font-medium text-foreground">{RUBRIC.HEADING}</h4>
      <p className="mt-0.5 max-w-[82ch] text-body text-muted-foreground">{RUBRIC.INTRO}</p>
      <div className="mt-3 overflow-hidden rounded-md border border-border">
        {GROUPS.map((g) => (
          <div key={g.title} className="border-t border-border first-of-type:border-t-0">
            <div className="flex items-center gap-2 bg-muted px-3 py-2">
              <span className="label-eyebrow">{g.title}</span>
              {g.hint && <span className="text-xs text-muted-foreground">{g.hint}</span>}
            </div>
            <div className="px-3">
              {g.keys.map((k) => (
                <RubricRow key={k} rowKey={k} value={value} disabled={disabled} onChange={onChange} />
              ))}
            </div>
          </div>
        ))}
      </div>
      <p className="mt-2.5 max-w-[80ch] text-xs text-muted-foreground">
        {setKeys.length === 0 ? RUBRIC.EMPTY_NOTE : RUBRIC.SET_NOTE(setKeys.length, AUTONOMY_META[lowest!].label)}
      </p>
    </section>
  );
}
