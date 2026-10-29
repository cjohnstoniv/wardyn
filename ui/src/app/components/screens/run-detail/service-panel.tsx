/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Service panel on a live service's Overview (mock r15 T1-c): how it restarts and is checked, the health wardynd
// last read from inside the sandbox, its restarts, and the lease. One reason sentence when there is a reason; the
// footnote always, because a compromised service could claim healthy: health never changes the run's state.
import { HeartPulse } from "lucide-react";
import type { RunDetail, RunService, RunServiceStatus } from "../../../lib/types";
import { cn } from "../../ui/utils";
import { DEFAULT_HEALTH_FAILURES, DEFAULT_HEALTH_INTERVAL_SEC, SERVICE_HEALTH, SERVICE_PANEL as T, agoText } from "../../wardyn/copy/run-service";
import { Chip, WidgetCard } from "../../wardyn/primitives";
import { weekdayClock } from "./run-ends-row";

function checkText(s: RunService): string {
  if (!s.port || !s.health) return T.CHECK_NONE;
  const sec = s.health.interval_sec || DEFAULT_HEALTH_INTERVAL_SEC;
  return s.health.path ? T.CHECK_HTTP(s.health.path, s.port, sec) : T.CHECK_TCP(s.port, sec);
}

/** The one sentence a state's reason has, with its tone: warning while the service is failing, neutral while Wardyn cannot tell. */
function reasonOf(s: RunService, st: RunServiceStatus | undefined): { text: string; failing: boolean } | undefined {
  if (st?.health === "unhealthy") {
    if (st.reason === "restarting") return { text: T.REASON_RESTARTING, failing: true };
    if (st.reason === "probe_failing") {
      const text = s.restart === "never" ? T.REASON_PROBE_FAILING : T.REASON_PROBE_RESTARTED(s.health?.failure_threshold || DEFAULT_HEALTH_FAILURES);
      return { text, failing: true };
    }
  }
  if (st?.health === "unknown") {
    if (st.reason === "supervisor_missing") return { text: T.REASON_OLD_IMAGE, failing: false };
    if (st.reason === "unreachable") return { text: T.REASON_UNREACHABLE, failing: false };
  }
  return undefined;
}

export function ServicePanel({ run, now = Date.now() }: { run: RunDetail; now?: number }) {
  const s = run.service;
  if (!s || run.state !== "RUNNING") return null;
  const st = run.service_status;
  const health = SERVICE_HEALTH[st?.health ?? "starting"] ?? SERVICE_HEALTH.unknown;
  const reason = reasonOf(s, st);
  const restarts = st?.restarts ?? 0;
  return (
    <div className="shrink-0 px-4 pt-3" data-testid="service-panel">
      <WidgetCard title={T.TITLE} Icon={HeartPulse} bodyClassName="space-y-3 px-3 py-2.5 text-body">
        <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5">
          <dt className="text-muted-foreground">{T.RESTART}</dt>
          <dd>{s.restart === "never" ? T.RESTART_NEVER : T.RESTART_ON_FAILURE}</dd>
          <dt className="text-muted-foreground">{T.CHECK}</dt>
          <dd className="font-mono text-xs leading-5">{checkText(s)}</dd>
          <dt className="text-muted-foreground">{T.HEALTH}</dt>
          <dd>
            <Chip tone={health.tone} dot>
              {health.word}
            </Chip>{" "}
            <span className="text-muted-foreground">· {st && st.health !== "starting" ? T.CHECKED(agoText(st.observed_at, now)) : T.WAITING_FIRST_CHECK}</span>
          </dd>
          <dt className="text-muted-foreground">{T.RESTARTS}</dt>
          <dd>{st?.last_exit_code !== undefined ? T.RESTARTS_WITH_EXIT(restarts, st.last_exit_code) : restarts}</dd>
          <dt className="text-muted-foreground">{T.LEASE}</dt>
          <dd>{run.ends_at ? T.LEASE_RENEWED(weekdayClock(run.ends_at)) : T.LEASE_NONE}</dd>
        </dl>
        {reason && (
          <p
            role="note"
            className={cn(
              "rounded-lg border px-2.5 py-2",
              reason.failing ? "border-warning/25 bg-warning-subtle text-foreground" : "border-border bg-muted/40 text-foreground",
            )}
          >
            {reason.text}
          </p>
        )}
        <p className="text-xs text-muted-foreground">{T.FOOTNOTE}</p>
      </WidgetCard>
    </div>
  );
}
