/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { QuestionBlock } from "./question-block";

export const COMMAND_ID = "nr-command";

/** An Application is a command that runs on its own. The one answer is a sentence, not a radio (#3045). */
export function CommandSection({ command, onCommand }: { command: string; onCommand: (command: string) => void }) {
  const labelId = React.useId();
  return (
    <QuestionBlock labelId={labelId} label={NR_RUN.APP_LABEL}>
      <p className="text-sm">
        <strong className="font-medium">{NR_RUN.APP_AUTOMATION}</strong>
        <span className="text-muted-foreground"> · {NR_RUN.APP_AUTOMATION_BODY}</span>
      </p>
      {/* Make's nested field (App.tsx:1669-1685): the label's asterisk is visual, aria-required says it. */}
      <div className="mt-3 flex flex-col gap-2">
        <Label htmlFor={COMMAND_ID}>{NR_RUN.COMMAND_LABEL} <span aria-hidden="true">*</span></Label>
        <Input id={COMMAND_ID} className="font-mono" value={command} aria-required="true" aria-describedby={`${COMMAND_ID}-help`} onChange={(e) => onCommand(e.target.value)} />
        <p id={`${COMMAND_ID}-help`} className="text-xs leading-snug text-muted-foreground">{NR_RUN.COMMAND_HELP}</p>
      </div>
    </QuestionBlock>
  );
}
