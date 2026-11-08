/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { TriangleAlert } from "lucide-react";
import { timeLeft } from "../../../../../lib/time-left";
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectSeparator, SelectTrigger, SelectValue } from "../../../../ui/select";
import { NR_RUN_IN } from "../../../../wardyn/copy/new-run/run-in";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { Field, fieldHintId } from "../../../../wardyn/form-primitives";
import * as RL from "../../../../wardyn/copy/run-lifetime";
import { Chip } from "../../../../wardyn/primitives";
import { Note } from "../../../governance/display";
import { lengthWords } from "../../../elevation/request-phrases";
import { weekdayClock } from "../../../run-detail/run-ends-row";
import type { DraftAction } from "../../draft/draft-reducer";
import { QuestionBlock } from "./question-block";
import { itemFor, shown, withoutRunIn, withRunIn, type RunInItem } from "./run-in-model";
import type { RunInRead } from "./use-run-in";

export const RUN_IN_ID = "nr-run-in";
const GROUPS = [
  { group: "yours", label: NR_RUN_IN.GROUP_YOURS },
  { group: "elevated", label: NR_RUN_IN.GROUP_ELEVATED },
  { group: "ask", label: NR_RUN_IN.GROUP_ASK },
] as const;
// A value for the disabled trigger of a read that failed, where no unit is listed.
const HOME_PLACEHOLDER = "home";

const isHome = (i: RunInItem | undefined) => i?.group === "yours" && i.home;

function ItemRow({ item, now }: { item: RunInItem; now: number }) {
  const line =
    item.group === "elevated" ? NR_RUN_IN.ELEVATED_LINE(weekdayClock(item.endsAt), timeLeft(Date.parse(item.endsAt) - now))
    : item.group === "ask" ? NR_RUN_IN.ASK_LINE(lengthWords(item.max))
    : undefined;
  const chip = isHome(item) ? <Chip>{NR_RUN_IN.CHIP_HOME}</Chip>
    : item.group === "elevated" ? <Chip tone="info">{NR_RUN_IN.CHIP_ELEVATED}</Chip>
    : item.group === "ask" ? <Chip tone="warning">{NR_RUN_IN.CHIP_NEEDS}</Chip>
    : undefined;
  return (
    <span className="flex w-full items-center justify-between gap-3">
      <span className="flex min-w-0 flex-col">
        <span>{item.name}</span>
        {line && <span className="text-xs text-muted-foreground">{line}</span>}
      </span>
      {chip}
    </span>
  );
}

/**
 * New run's "Run in" question: the home unit by default, and the units a person holds an elevation in or may ask
 * for beside their own. `runIn` is the draft's choice and `notice` the sentence for a choice the units no longer
 * offer. While the units load nothing is drawn unless the draft already carries a choice, so a person with one
 * unit never sees the question flash by.
 */
export function RunInField({ run, runIn, notice, homeFallbackName, dispatch }: {
  run: RunInRead; runIn?: string; notice?: string; homeFallbackName?: string; dispatch: React.Dispatch<DraftAction>;
}) {
  const id = React.useId();
  const loading = run.status === "loading";
  const failed = run.status === "failed";
  const choices = run.status === "ready" ? run.choices : undefined;
  if (loading ? !runIn : failed ? !homeFallbackName && !runIn : !choices || !(shown(choices) || notice)) return null;

  const item = itemFor(choices, runIn);
  const selected = item ?? choices?.home;
  const home = !item || isHome(item);
  const value = failed ? HOME_PLACEHOLDER : selected?.unitId ?? "";
  const text = failed ? (homeFallbackName ? NR_RUN_IN.HOME_VALUE(homeFallbackName) : "") : isHome(selected) && selected ? NR_RUN_IN.HOME_VALUE(selected.name) : selected?.name;
  // An empty group is not drawn.
  const groups = GROUPS.map((g) => ({ ...g, rows: choices?.items.filter((i) => i.group === g.group) ?? [] })).filter((g) => g.rows.length > 0);
  const choose = (unitId: string) => {
    const next = choices?.items.find((i) => i.unitId === unitId);
    if (!next) return;
    if (notice) dispatch({ type: "guided", patch: { runInCleared: undefined } });
    dispatch({ type: "edit", apply: (i) => (isHome(next) ? withoutRunIn(i) : withRunIn(i, next.unitId)) });
  };
  return (
    <QuestionBlock id={RUN_IN_ID} label={NR_RUN_IN.QUESTION} marker={NR_SHARED.OPTIONAL}>
      <Field label={NR_RUN_IN.LABEL} htmlFor={id} hint={loading || home ? NR_RUN_IN.HINT : undefined}>
        <div className="flex items-center gap-2">
          <div className="min-w-0 flex-1">
            <Select value={value} onValueChange={choose} disabled={loading || failed}>
              <SelectTrigger id={id} aria-describedby={loading || home ? fieldHintId(id) : undefined}>
                <SelectValue placeholder={loading ? NR_RUN_IN.LOADING : undefined}>{text}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {groups.map(({ group, label, rows }, n) => (
                  <React.Fragment key={group}>
                    {n > 0 && <SelectSeparator />}
                    <SelectGroup>
                      <SelectLabel>{label}</SelectLabel>
                      {rows.map((r) => (
                        <SelectItem key={r.unitId} value={r.unitId} textValue={r.name}>
                          <ItemRow item={r} now={run.now} />
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </React.Fragment>
                ))}
              </SelectContent>
            </Select>
          </div>
          {item?.group === "elevated" && <Chip tone="info">{NR_RUN_IN.CHIP_ELEVATED}</Chip>}
        </div>
      </Field>
      {item?.group === "elevated" && <Note tone="info">{RL.endsElevation(weekdayClock(item.endsAt))}</Note>}
      {failed && <p className="mt-2 text-xs leading-snug text-muted-foreground">{NR_RUN_IN.UNITS_UNREADABLE}</p>}
      {notice && (
        <Note tone="amber" role="status">
          <span className="flex items-start gap-2 text-sm">
            <TriangleAlert aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
            <span className="min-w-0">{notice}</span>
          </span>
        </Note>
      )}
    </QuestionBlock>
  );
}
