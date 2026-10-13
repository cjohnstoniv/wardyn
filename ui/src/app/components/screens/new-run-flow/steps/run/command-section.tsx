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
import { Chip } from "../../../../wardyn/primitives";

export const COMMAND_ID = "nr-command";

/** An Application is a command that runs on its own; an Application / server is Coming soon. */
export function CommandSection({ command, onCommand }: { command: string; onCommand: (command: string) => void }) {
  const labelId = React.useId();
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-3">
        <div className="flex items-center gap-2">
          <span id={labelId} className="label-eyebrow">{NR_RUN.APP_LABEL}</span>
          <Chip>{NR_SHARED.REQUIRED}</Chip>
        </div>
        <RadioCardGroup name="nr-run-app" aria-labelledby={labelId} value="automation" onValueChange={() => {}} className="grid gap-3 sm:grid-cols-2">
          <RadioCard value="automation" title={NR_RUN.APP_AUTOMATION} description={NR_RUN.APP_AUTOMATION_BODY} meta={NR_RUN.APP_AUTOMATION_META} />
          <RadioCard value="server" title={NR_RUN.APP_SERVER} description={NR_RUN.APP_SERVER_BODY} unavailable={{ reason: NR_SHARED.COMING_SOON }} />
        </RadioCardGroup>
      </div>
      <div className="flex flex-col gap-2">
        <Label htmlFor={COMMAND_ID}>{NR_RUN.COMMAND_LABEL} <Chip>{NR_SHARED.REQUIRED}</Chip></Label>
        <Input id={COMMAND_ID} className="font-mono" value={command} aria-describedby={`${COMMAND_ID}-help`} onChange={(e) => onCommand(e.target.value)} />
        <p id={`${COMMAND_ID}-help`} className="text-xs leading-snug text-muted-foreground">{NR_RUN.COMMAND_HELP}</p>
      </div>
    </div>
  );
}
