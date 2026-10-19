/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Input } from "../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { cn } from "../ui/utils";

// The "number + unit" pair behind every admin duration (the pool form's lifetimes, the governance
// profile's run limits): a number box beside the shared Select. The caller owns both values, so it
// decides how a unit change moves the number.
export function DurationField({
  id,
  count,
  unit,
  units,
  onCount,
  onUnit,
  countLabel,
  unitLabel,
  disabled,
  unitDisabled = disabled,
  invalid,
  describedBy,
  step,
  inputClassName,
  selectClassName,
}: {
  id: string;
  count: string;
  unit: string;
  units: readonly { value: string; label: string }[];
  onCount: (raw: string) => void;
  onUnit: (value: string) => void;
  /** The number box's accessible name, when no visible label points at it. */
  countLabel?: string;
  unitLabel: string;
  disabled?: boolean;
  unitDisabled?: boolean;
  invalid?: boolean;
  describedBy?: string;
  step?: string;
  inputClassName?: string;
  selectClassName?: string;
}) {
  return (
    <div className="flex items-center gap-2">
      <Input
        id={id}
        type="number"
        min={0}
        step={step}
        disabled={disabled}
        aria-label={countLabel}
        aria-invalid={invalid ? true : undefined}
        aria-describedby={describedBy}
        className={inputClassName}
        value={count}
        onChange={(e) => onCount(e.target.value)}
      />
      <Select value={unit} disabled={unitDisabled} onValueChange={onUnit}>
        <SelectTrigger aria-label={unitLabel} className={cn("min-w-[104px]", selectClassName)}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {units.map((u) => (
            <SelectItem key={u.value} value={u.value}>
              {u.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
