/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { ChevronDown } from "lucide-react";
import { Popover, PopoverContent, PopoverTrigger } from "../../../../ui/popover";
import { RadioGroup, RadioGroupItem } from "../../../../ui/radio-group";
import { MAC_BASES } from "../../../../wardyn/copy/mac-bases";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { harnessDisplayNames } from "../../../../../lib/model-access";
import { baseChoices } from "./macos-bases";
import type { MacOSOffer } from "./macos-offer";

/**
 * The macOS base under the OS row (board J6): the base the run clones, as a menu of Standard and the pool's other bases.
 * A base that is not ready on every Mac stays in the menu, greyed, with why. `value` is the draft's base, absent for Standard.
 */
export function MacOSBasePicker({ offer, value, onChoose }: { offer: Extract<MacOSOffer, { ok: true }>; value: string | undefined; onChoose: (base: string | undefined) => void }) {
  const labelId = React.useId();
  const [open, setOpen] = React.useState(false);
  const { status } = useShellSetupStatus();
  // Your own tools last, as the runner drawer lists them.
  const harnessNames = (ids: readonly string[]) => [...ids.filter((id) => id !== "none"), ...ids.filter((id) => id === "none")].map((id) => harnessDisplayNames(status, [id])).join(", ");
  const choices = baseChoices(offer, value, harnessNames);
  const current = value ?? MAC_BASES.B04;
  return (
    <div className="flex flex-col gap-2">
      <span id={labelId} className="text-xs font-semibold">{MAC_BASES.B01}</span>
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger
          aria-labelledby={labelId}
          className="flex h-9 w-full items-center justify-between rounded-md border border-border bg-input-background px-3 text-sm outline-none focus-visible:ring-[3px] focus-visible:ring-ring"
        >
          <span>{current}</span>
          <ChevronDown aria-hidden="true" className="size-4 text-muted-foreground" />
        </PopoverTrigger>
        <PopoverContent align="start" className="w-[520px] p-1.5">
          <RadioGroup
            aria-labelledby={labelId}
            value={current}
            className="gap-0.5"
            onValueChange={(name) => {
              onChoose(name === MAC_BASES.B04 ? undefined : name);
              setOpen(false);
            }}
          >
            {choices.map((c, i) => {
              const id = `${labelId}-${i}`;
              return (
                <label key={c.name} className={`flex items-start gap-2.5 rounded-sm px-2 py-1.5 text-sm ${c.reason ? "cursor-not-allowed text-muted-foreground" : "cursor-pointer hover:bg-accent"}`}>
                  <RadioGroupItem value={c.name} disabled={!!c.reason} aria-labelledby={`${id}-name`} aria-describedby={`${id}-why`} className="mt-0.5" />
                  <span className="flex min-w-0 flex-col">
                    <span id={`${id}-name`} className="font-medium">{c.name}</span>
                    <span id={`${id}-why`} className="flex flex-col text-xs">
                      {c.meta && <span className="text-muted-foreground">{c.meta}</span>}
                      {c.reason && <span className="text-warning">{c.reason}</span>}
                    </span>
                  </span>
                </label>
              );
            })}
          </RadioGroup>
        </PopoverContent>
      </Popover>
      <p className="text-xs leading-snug text-muted-foreground">{MAC_BASES.B02}</p>
    </div>
  );
}
