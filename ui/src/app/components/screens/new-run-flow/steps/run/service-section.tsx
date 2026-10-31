/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { Segmented } from "../../../../wardyn/segmented";
import { Switch } from "../../../../wardyn/form-primitives";
import { TriangleAlert } from "lucide-react";
import { DEFAULT_HEALTH_FAILURES, DEFAULT_HEALTH_INTERVAL_SEC, SERVICE_FORM as T, SERVICE_FORM_ERRORS as E, SERVICE_PAUSE_WARNING } from "../../../../wardyn/copy/run-service";
import type { RunService } from "../../../../../lib/types/runs";
import { serviceProblems, type ServiceField } from "../../lenses/service-lens";

export interface ServiceSectionProps {
  service: RunService | undefined;
  onKeepRunning: (on: boolean) => void;
  onRestart: (restart: RunService["restart"]) => void;
  onPort: (port: number) => void;
  onHealth: (patch: Partial<NonNullable<RunService["health"]>>) => void;
  /** The checks say the person's limits pause idle runs, which freezes a service that waits for requests. */
  pauses?: boolean;
}

/** Digits only: a half-typed number is 0 until it parses, and the server fills a health check's 0 with its default. */
const whole = (text: string): number => Number(text.replace(/\D/g, "").slice(0, 5)) || 0;
const shown = (n: number | undefined): string => (n ? String(n) : "");

const FIELD_ID: Record<ServiceField, string> = { port: "nr-service-port", path: "nr-service-path", interval: "nr-service-every", failures: "nr-service-failures" };
const FIELD_TEXT: Record<ServiceField, string> = { port: E.PORT_INVALID, path: E.PATH_INVALID, interval: E.EVERY_INVALID, failures: E.FAILURES_INVALID };

/** "Keep it running" under an Automation / script's command: restart it when it fails, and check it on a port inside the sandbox. */
export function ServiceSection({ service, onKeepRunning, onRestart, onPort, onHealth, pauses }: ServiceSectionProps) {
  const health = service?.health;
  const problems = serviceProblems(service);
  // A field's invalid mark and the id of the sentence under it (and of its help, when it has one).
  const attrs = (f: ServiceField, help?: string) => ({
    id: FIELD_ID[f],
    "aria-invalid": problems[f] ? true : undefined,
    "aria-describedby": [help, problems[f] ? `${FIELD_ID[f]}-error` : undefined].filter(Boolean).join(" ") || undefined,
  });
  const error = (f: ServiceField) => problems[f] && <p id={`${FIELD_ID[f]}-error`} role="alert" className="text-sm text-danger">{FIELD_TEXT[f]}</p>;
  // The check's fields apply only while a port is named; they keep their values while it is retyped.
  const noCheck = !health || !service?.port;
  return (
    <div data-testid="service-section" className="flex flex-col gap-4">
      <div className="flex flex-col gap-1.5">
        <div className="flex items-center gap-2.5">
          <Switch checked={!!service} onChange={onKeepRunning} label={T.LABEL} />
          <span aria-hidden="true" className="text-sm font-medium">{T.LABEL}</span>
        </div>
        <p className="text-xs leading-snug text-muted-foreground">{T.HELP}</p>
        {service && pauses && (
          <div role="note" className="flex items-start gap-2 rounded-lg border border-[color-mix(in_srgb,var(--warning)_35%,var(--border))] bg-warning-subtle px-3 py-2.5 text-sm">
            <TriangleAlert aria-hidden="true" className="mt-0.5 size-4 flex-none text-warning" />
            <span>{SERVICE_PAUSE_WARNING}</span>
          </div>
        )}
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
              <Label htmlFor={FIELD_ID.port}>{T.PORT_LABEL}</Label>
              <Input {...attrs("port", "nr-service-port-help")} className="font-mono" inputMode="numeric" value={shown(service.port)} onChange={(e) => onPort(whole(e.target.value))} />
              <p id="nr-service-port-help" className="text-xs leading-snug text-muted-foreground">{T.PORT_HELP}</p>
              {error("port")}
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor={FIELD_ID.path}>{T.PATH_LABEL}</Label>
              <Input {...attrs("path", "nr-service-path-help")} className="font-mono" placeholder="/healthz" disabled={noCheck} value={health?.path ?? ""} onChange={(e) => onHealth({ path: e.target.value })} />
              <p id="nr-service-path-help" className="text-xs leading-snug text-muted-foreground">{T.PATH_HELP}</p>
              {error("path")}
            </div>
          </div>
          <div className="flex flex-wrap items-end gap-x-6 gap-y-3">
            <div className="flex flex-col gap-2">
              <Label htmlFor={FIELD_ID.interval}>{T.EVERY_LABEL}</Label>
              <span className="flex items-center gap-2 text-sm">
                <Input {...attrs("interval")} className="w-20" inputMode="numeric" placeholder={String(DEFAULT_HEALTH_INTERVAL_SEC)} disabled={noCheck} value={shown(health?.interval_sec)} onChange={(e) => onHealth({ interval_sec: whole(e.target.value) })} />
                {T.SECONDS}
              </span>
              {error("interval")}
            </div>
            <div className="flex flex-col gap-2">
              <Label htmlFor={FIELD_ID.failures}>{T.FAILURES_LABEL}</Label>
              <span className="flex items-center gap-2 text-sm">
                <Input {...attrs("failures")} className="w-16" inputMode="numeric" placeholder={String(DEFAULT_HEALTH_FAILURES)} disabled={noCheck} value={shown(health?.failure_threshold)} onChange={(e) => onHealth({ failure_threshold: whole(e.target.value) })} />
                {T.FAILED_CHECKS}
              </span>
              {error("failures")}
            </div>
          </div>
        </>
      )}
    </div>
  );
}
