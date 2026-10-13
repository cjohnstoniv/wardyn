/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run it / Ask first / Block it as a segmented control. An option the server or
// the policy rules out stays visible and greyed, and its reason is text beneath
// the control (CONSOLE-RULES section 8), never a tooltip.

import type { ToolEffect } from "../../../../../lib/tool-effect";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { cn } from "../../../../ui/utils";
import type { ChoiceState } from "./tool-facts";

export const EFFECT_LABEL: Record<ToolEffect, string> = { allow: NR_TOOLS.EFFECT_ALLOW, hold: NR_TOOLS.EFFECT_HOLD, deny: NR_TOOLS.EFFECT_DENY };

export interface EffectChoiceProps {
  name: string;
  /** The id of the element that labels the group. */
  labelledBy: string;
  value: ToolEffect | undefined;
  choices: readonly ChoiceState[];
  onChange: (effect: ToolEffect) => void;
  /** A line under one option that is available, e.g. who answers Ask first. */
  notes?: Partial<Record<ToolEffect, string>>;
}

export function EffectChoice({ name, labelledBy, value, choices, onChange, notes }: EffectChoiceProps) {
  const lines = choices.filter((c) => (!c.available && c.reason) || (c.available && notes?.[c.effect]));
  return (
    <div className="flex flex-col gap-2">
      <div role="radiogroup" aria-labelledby={labelledBy} className="inline-flex w-fit flex-wrap gap-1 rounded-md border border-border p-1">
        {choices.map((c) => (
          <label
            key={c.effect}
            className={cn(
              "flex min-h-8 cursor-pointer items-center rounded-sm px-3 text-sm has-[:focus-visible]:ring-[3px] has-[:focus-visible]:ring-ring",
              c.effect === value && "bg-primary text-primary-foreground",
              !c.available && "cursor-not-allowed opacity-50",
            )}
          >
            <input
              type="radio"
              name={name}
              value={c.effect}
              className="sr-only"
              checked={c.effect === value}
              disabled={!c.available}
              onChange={() => onChange(c.effect)}
            />
            {EFFECT_LABEL[c.effect]}
          </label>
        ))}
      </div>
      {lines.length > 0 && (
        <ul className="flex flex-col gap-1 text-sm text-muted-foreground">
          {lines.map((c) => (
            <li key={c.effect}>
              <span className="font-medium text-foreground">{EFFECT_LABEL[c.effect]}</span>{" "}
              {c.available ? notes?.[c.effect] : c.reason}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
