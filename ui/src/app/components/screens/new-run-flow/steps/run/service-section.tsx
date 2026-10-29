/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { Segmented } from "../../../../wardyn/segmented";
import { Switch } from "../../../../wardyn/form-primitives";
import { DEFAULT_HEALTH_FAILURES, DEFAULT_HEALTH_INTERVAL_SEC, SERVICE_FORM as T } from "../../../../wardyn/copy/run-service";
import type { RunService } from "../../../../../lib/types/runs";

export interface ServiceSectionProps {
  service: RunService | undefined;
  onKeepRunning: (on: boolean) => void;
  onRestart: (restart: RunService["restart"]) => void;
  onPort: (port: number) => void;
  onHealth: (patch: Partial<NonNullable<RunService["health"]>>) => void;
}

/** Digits only: a half-typed number is 0 until it parses, and the server fills a health check's 0 with its default. */
const whole = (text: string): number => Number(text.replace(/\D/g, "").slice(0, 5)) || 0;
const shown = (n: number | undefined): string => (n ? String(n) : "");

/** "Keep it running" under an Automation / script's command: restart it when it fails, and check it on a port inside the sandbox. */
export function ServiceSection({ service, onKeepRunning, onRestart, onPort, onHealth }: ServiceSectionProps) {
  const id = React.useId();
  const health = service?.health;
  return (
    <div data-testid="service-section" className="flex flex-col gap-4">
      <div className="flex flex-col gap-1.5">
        <div className="flex items-center gap-2.5">
          <Switch checked={!!service} onChange={onKeepRunning} label={T.LABEL} />
          <span aria-hidden="true" className="text-sm font-medium">{T.LABEL}</span>
        </div>
        <p className="text-xs leading-snug text-muted-foreground">{T.HELP}</p>
      </div>
      {service && (
        <>
          <div className="flex flex-col gap-2">
            <span className="text-sm font-medium">{T.RESTART_LABEL}</span>
            <Segmented
              value={service.restart}
              onChange={onRestart}
              size="compact"
              options={[
                { value: "on_failure", label: T.RESTART_ON_FAILURE },
                { value: "never", label: T.RESTART_NEVER },
              ]}
            />
          </div>
          <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_2fr]">
            <div className="flex flex-col gap-2">
              <Label htmlFor={`${id}-port`}>{T.PORT_LABEL}</Label>
              <Input id={`${id}-port`} className="font-mono" inputMode="numeric" value={shown(service.port)} aria-describedby={`${id}-port-help`} onChange={(e) => onPort(whole(e.target.value))} />
              <p id={`${id}-port-help`} className="text-xs leading-snug text-muted-foreground">{T.PORT_HELP}</p>
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor={`${id}-path`}>{T.PATH_LABEL}</Label>
              <Input id={`${id}-path`} className="font-mono" placeholder="/healthz" disabled={!health} value={health?.path ?? ""} aria-describedby={`${id}-path-help`} onChange={(e) => onHealth({ path: e.target.value })} />
              <p id={`${id}-path-help`} className="text-xs leading-snug text-muted-foreground">{T.PATH_HELP}</p>
            </div>
          </div>
          <div className="flex flex-wrap items-end gap-x-6 gap-y-3">
            <div className="flex flex-col gap-2">
              <Label htmlFor={`${id}-every`}>{T.EVERY_LABEL}</Label>
              <span className="flex items-center gap-2 text-sm">
                <Input id={`${id}-every`} className="w-20" inputMode="numeric" placeholder={String(DEFAULT_HEALTH_INTERVAL_SEC)} disabled={!health} value={shown(health?.interval_sec)} onChange={(e) => onHealth({ interval_sec: whole(e.target.value) })} />
                {T.SECONDS}
              </span>
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor={`${id}-failures`}>{T.FAILURES_LABEL}</Label>
              <span className="flex items-center gap-2 text-sm">
                <Input id={`${id}-failures`} className="w-16" inputMode="numeric" placeholder={String(DEFAULT_HEALTH_FAILURES)} disabled={!health} value={shown(health?.failure_threshold)} onChange={(e) => onHealth({ failure_threshold: whole(e.target.value) })} />
                {T.FAILED_CHECKS}
              </span>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
