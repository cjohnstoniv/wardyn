/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { RadioCard, RadioCardGroup } from "../../../../wardyn/radio-card";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { emptyStart, scheduledRunsOn, startInstant, startProblem, todayDate, whenText, zoneOptions, type StartChoice, type StartProblem } from "../../../../../lib/schedule";
import { QuestionBlock } from "./question-block";

const SELECT_CLASS =
  "border-input bg-input-background dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring h-9 w-full rounded-md border px-3 text-base outline-none focus-visible:ring-[3px] md:text-sm";

const PROBLEM_TEXT: Record<StartProblem, string> = { missing: NR_RUN.START_MISSING, past: NR_RUN.START_PAST, far: NR_RUN.START_FAR };

/**
 * When an Autonomous run starts: now, or at a date and time the person picks. Later is offered only while the
 * setup status carries features.scheduled_runs; otherwise it stays Coming soon and Run now is the only choice.
 */
export function TimingSection({ schedule, onSchedule }: { schedule: StartChoice | undefined; onSchedule: (next: StartChoice | undefined) => void }) {
  const labelId = React.useId();
  const { status } = useShellSetupStatus();
  const on = scheduledRunsOn(status);
  const later = on && schedule !== undefined;
  const problem = later ? startProblem(schedule, Date.now()) : null;
  const at = later && !problem ? startInstant(schedule) : null;
  const zones = React.useMemo(() => zoneOptions(schedule?.zone), [schedule?.zone]);
  const set = (patch: Partial<StartChoice>) => schedule && onSchedule({ ...schedule, ...patch });
  const invalid = problem !== null;

  return (
    <QuestionBlock labelId={labelId} label={NR_RUN.TIMING_LABEL}>
      <RadioCardGroup
        name="nr-run-timing"
        aria-labelledby={labelId}
        value={later ? "later" : "now"}
        onValueChange={(v) => onSchedule(v === "later" ? (schedule ?? emptyStart()) : undefined)}
        className="grid gap-3 sm:grid-cols-2"
      >
        <RadioCard value="now" title={NR_RUN.TIMING_NOW} description={NR_RUN.TIMING_NOW_BODY} />
        <RadioCard
          value="later"
          title={NR_RUN.TIMING_LATER}
          description={NR_RUN.TIMING_LATER_BODY}
          unavailable={on ? undefined : { reason: NR_SHARED.COMING_SOON }}
        />
      </RadioCardGroup>
      {later && (
        <div className="mt-4 flex flex-col gap-2.5" data-testid="nr-run-start">
          <div className="grid gap-3 sm:grid-cols-[1fr_140px_1.3fr]">
            <div className="flex flex-col gap-2">
              <Label htmlFor="nr-start-date">{NR_RUN.START_DATE}</Label>
              <Input id="nr-start-date" type="date" min={todayDate()} value={schedule.date} aria-invalid={invalid} onChange={(e) => set({ date: e.target.value })} />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="nr-start-time">{NR_RUN.START_TIME}</Label>
              <Input id="nr-start-time" type="time" value={schedule.time} aria-invalid={invalid} onChange={(e) => set({ time: e.target.value })} />
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor="nr-start-zone">{NR_RUN.START_ZONE}</Label>
              <select id="nr-start-zone" className={SELECT_CLASS} value={schedule.zone} onChange={(e) => set({ zone: e.target.value })}>
                {zones.map((z) => <option key={z} value={z}>{z}</option>)}
              </select>
            </div>
          </div>
          {problem && <p role="alert" className="text-sm text-danger">{PROBLEM_TEXT[problem]}</p>}
          {at !== null && <p role="status" className="text-sm"><strong>{NR_RUN.STARTS(whenText(at, schedule.zone))}</strong></p>}
          <p className="text-xs text-muted-foreground">{NR_RUN.START_HELP}</p>
        </div>
      )}
    </QuestionBlock>
  );
}
