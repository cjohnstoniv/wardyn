/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { RadioCard, RadioCardGroup } from "../../../../wardyn/radio-card";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import { RUN_SCHEDULE } from "../../../../wardyn/copy/run-schedules";
import { emptyStart, firstRepeatStart, repeatProblem, scheduleDaysAhead, scheduledRuns, startInstant, startProblem, todayDate, weekdayName, whenText, zoneOptions, zoneWeekday, type ScheduleEvery, type StartChoice, type StartProblem } from "../../../../../lib/schedule";
import { heldStart, repeatLine, startHeldText } from "../../draft/launch-start";
import { QuestionBlock } from "./question-block";

const SELECT_CLASS =
  "border-input bg-input-background dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring h-9 w-full rounded-md border px-3 text-base outline-none focus-visible:ring-[3px] md:text-sm";

const PROBLEM_TEXT = (): Record<StartProblem, string> => ({ missing: NR_RUN.START_MISSING, clock: NR_RUN.START_CLOCK, past: NR_RUN.START_PAST, far: NR_RUN.START_FAR(scheduleDaysAhead()) });

// The Repeat select's value for a one-time start.
const NO_REPEAT = "none";

// The weekly option's weekday: the first start's, as the server takes it, or today's in the zone before a time is typed.
function weeklyWeekday(choice: StartChoice): string {
  const now = Date.now();
  const first = firstRepeatStart({ ...choice, repeat: "weekly" }, now);
  return weekdayName(zoneWeekday(first ?? now, choice.zone));
}

/**
 * When an Autonomous run starts: now, or at a date and time the person picks. Later is offered only while the
 * setup status carries features.scheduled_runs; otherwise Run now is the one answer, said as a sentence. A Later
 * start the draft still holds while the switch is off or not known is said as held, with Run now beside it.
 */
export function TimingSection({ schedule, onSchedule }: { schedule: StartChoice | undefined; onSchedule: (next: StartChoice | undefined) => void }) {
  const labelId = React.useId();
  const { status } = useShellSetupStatus();
  const switchOn = scheduledRuns(status);
  const on = switchOn === true;
  const held = heldStart(schedule, switchOn);
  const later = on && schedule !== undefined;
  const repeat = later ? schedule.repeat : undefined;
  const problem = later ? (repeat ? repeatProblem(schedule) : startProblem(schedule, Date.now())) : null;
  const at = later && !problem && !repeat ? startInstant(schedule) : null;
  const repeats = later && !problem && repeat ? repeatLine(schedule, Date.now()) : null;
  const zones = React.useMemo(() => zoneOptions(schedule?.zone), [schedule?.zone]);
  // A problem is told once the person has typed in the picker (or came back to values already typed), not at the
  // moment it opens empty: an alert nobody caused is read out over what they are doing.
  const [touched, setTouched] = React.useState(false);
  const told = problem !== null && (touched || (!repeat && !!schedule?.date) || !!schedule?.time);
  const set = (patch: Partial<StartChoice>) => {
    setTouched(true);
    if (schedule) onSchedule({ ...schedule, ...patch });
  };
  const helpId = `${labelId}-help`;
  const errorId = `${labelId}-error`;
  const describedBy = told ? `${errorId} ${helpId}` : helpId;
  // Run now on a held start unmounts the button that had focus: focus goes to the Run now sentence that replaces it.
  const nowLine = React.useRef<HTMLParagraphElement>(null);
  const ranNow = React.useRef(false);
  React.useEffect(() => {
    if (!ranNow.current || held) return;
    ranNow.current = false;
    nowLine.current?.focus();
  }, [held]);

  // A held start: why, in the dialog's words, and Run now to clear it. Shown unasked, so polite.
  if (held) {
    return (
      <QuestionBlock labelId={labelId} label={NR_RUN.TIMING_LABEL}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p aria-labelledby={labelId} role="status" className="min-w-0 flex-[1_1_20rem] text-sm text-danger">{startHeldText(held)}</p>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              ranNow.current = true;
              onSchedule(undefined);
            }}
          >
            {NR_RUN.TIMING_NOW}
          </Button>
        </div>
      </QuestionBlock>
    );
  }

  // Switch off: no start time exists on the wire, so the one answer is a sentence, not a radio (#3045).
  if (!on) {
    return (
      <QuestionBlock labelId={labelId} label={NR_RUN.TIMING_LABEL}>
        <p ref={nowLine} tabIndex={-1} aria-labelledby={labelId} className="text-sm outline-none">
          <strong className="font-medium">{NR_RUN.TIMING_NOW}</strong>
          <span className="text-muted-foreground"> · {NR_RUN.TIMING_NOW_BODY}</span>
        </p>
      </QuestionBlock>
    );
  }

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
        />
      </RadioCardGroup>
      {later && (
        <div className="mt-4 flex flex-col gap-2.5" data-testid="nr-run-start">
          <div className={repeat ? "grid gap-3 sm:grid-cols-[1fr_140px_1.3fr]" : "grid gap-3"}>
            <div className={repeat ? "flex flex-col gap-2" : "flex flex-col gap-2 sm:max-w-xs"}>
              <Label htmlFor="nr-start-repeat">{RUN_SCHEDULE.REPEAT}</Label>
              <select
                id="nr-start-repeat"
                className={SELECT_CLASS}
                value={repeat ?? NO_REPEAT}
                onChange={(e) => set({ repeat: e.target.value === NO_REPEAT ? undefined : (e.target.value as ScheduleEvery) })}
              >
                <option value={NO_REPEAT}>{RUN_SCHEDULE.REPEAT_NONE}</option>
                <option value="daily">{RUN_SCHEDULE.REPEAT_DAILY}</option>
                <option value="weekdays">{RUN_SCHEDULE.REPEAT_WEEKDAYS}</option>
                <option value="weekly">{RUN_SCHEDULE.REPEAT_WEEKLY(weeklyWeekday(schedule))}</option>
              </select>
            </div>
            {repeat && <TimeAndZone schedule={schedule} told={told} describedBy={describedBy} helpId={helpId} zones={zones} set={set} />}
          </div>
          {!repeat && (
            <div className="grid gap-3 sm:grid-cols-[1fr_140px_1.3fr]">
              <div className="flex flex-col gap-2">
                <Label htmlFor="nr-start-date">{NR_RUN.START_DATE}</Label>
                <Input id="nr-start-date" type="date" min={todayDate(schedule.zone)} value={schedule.date} aria-invalid={told} aria-describedby={describedBy} onChange={(e) => set({ date: e.target.value })} />
              </div>
              <TimeAndZone schedule={schedule} told={told} describedBy={describedBy} helpId={helpId} zones={zones} set={set} />
            </div>
          )}
          {/* An alert only after the person typed; values they come back to are told politely. */}
          {told && problem && <p id={errorId} role={touched ? "alert" : "status"} className="text-sm text-danger">{repeat ? RUN_SCHEDULE.REPEAT_MISSING : PROBLEM_TEXT()[problem]}</p>}
          {at !== null && <p role="status" className="text-sm"><strong>{NR_RUN.STARTS(whenText(at, schedule.zone))}</strong></p>}
          {repeats !== null && <p role="status" className="text-sm"><strong>{repeats}</strong></p>}
          <p id={helpId} className="text-xs text-muted-foreground">{repeat ? RUN_SCHEDULE.REPEAT_HELP : NR_RUN.START_HELP(scheduleDaysAhead())}</p>
        </div>
      )}
    </QuestionBlock>
  );
}

// The Time and Time zone fields, beside the date for a one-time start and beside Repeat for a repeating one.
function TimeAndZone({ schedule, told, describedBy, helpId, zones, set }: {
  schedule: StartChoice;
  told: boolean;
  describedBy: string;
  helpId: string;
  zones: string[];
  set: (patch: Partial<StartChoice>) => void;
}) {
  return (
    <>
      <div className="flex flex-col gap-2">
        <Label htmlFor="nr-start-time">{NR_RUN.START_TIME}</Label>
        <Input id="nr-start-time" type="time" value={schedule.time} aria-invalid={told} aria-describedby={describedBy} onChange={(e) => set({ time: e.target.value })} />
      </div>
      <div className="flex flex-col gap-2">
        <Label htmlFor="nr-start-zone">{NR_RUN.START_ZONE}</Label>
        <select id="nr-start-zone" className={SELECT_CLASS} value={schedule.zone} aria-describedby={helpId} onChange={(e) => set({ zone: e.target.value })}>
          {zones.map((z) => <option key={z} value={z}>{z}</option>)}
        </select>
      </div>
    </>
  );
}
