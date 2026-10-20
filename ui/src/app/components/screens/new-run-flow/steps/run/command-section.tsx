/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { RadioCard, RadioCardGroup } from "../../../../wardyn/radio-card";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import type { UIApp } from "../../../../../lib/types/runs";
import type { AppKind } from "../../lenses/apps-lens";
import { PortsSection } from "./ports-section";
import { QuestionBlock } from "./question-block";

export const COMMAND_ID = "nr-command";

export interface CommandSectionProps {
  kind: AppKind;
  command: string;
  apps: readonly UIApp[];
  onKind: (kind: AppKind) => void;
  onCommand: (command: string) => void;
  onApps: { edit: (index: number, patch: Partial<UIApp>) => void; add: () => void; remove: (index: number) => void };
}

/** An Application is a script that runs on its own, or a server that keeps running and opens its ports in the browser. */
export function CommandSection({ kind, command, apps, onKind, onCommand, onApps }: CommandSectionProps) {
  const labelId = React.useId();
  const server = kind === "server";
  const help = server ? NR_RUN.START_COMMAND_HELP : NR_RUN.COMMAND_HELP;
  return (
    <QuestionBlock labelId={labelId} label={NR_RUN.APP_LABEL}>
      <RadioCardGroup name="nr-run-app" aria-labelledby={labelId} value={kind} onValueChange={(v) => onKind(v as AppKind)} className="grid gap-3 sm:grid-cols-2">
        <RadioCard value="automation" title={NR_RUN.APP_AUTOMATION} description={NR_RUN.APP_AUTOMATION_BODY} meta={NR_RUN.APP_AUTOMATION_META} />
        <RadioCard value="server" title={NR_RUN.APP_SERVER} description={NR_RUN.APP_SERVER_BODY} meta={NR_RUN.APP_AUTOMATION_META} />
      </RadioCardGroup>
      <div className="mt-4 flex flex-col gap-[18px]">
        {/* Make's nested field (App.tsx:1669-1685): the label's asterisk is visual, aria-required says it. */}
        <div className="flex flex-col gap-2">
          <Label htmlFor={COMMAND_ID}>{server ? NR_RUN.START_COMMAND_LABEL : NR_RUN.COMMAND_LABEL} <span aria-hidden="true">*</span></Label>
          <Input id={COMMAND_ID} className="font-mono" value={command} aria-required="true" aria-describedby={`${COMMAND_ID}-help`} onChange={(e) => onCommand(e.target.value)} />
          <p id={`${COMMAND_ID}-help`} className="text-xs leading-snug text-muted-foreground">{help}</p>
        </div>
        {server && <PortsSection apps={apps} {...onApps} />}
        {server && <p className="text-xs leading-snug text-muted-foreground">{NR_RUN.APP_TERMINAL_NOTE}</p>}
      </div>
    </QuestionBlock>
  );
}
