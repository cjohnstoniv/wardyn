/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Service widget on a live service's Overview, in the right column above Egress (mock r15 T1-c): how it restarts and
// is checked, the health wardynd
// last read from inside the sandbox, its restarts, and the lease. One reason sentence when there is a reason; the
// footnote always, because a compromised service could claim healthy: health never changes the run's state. A service
// started with Wardyn's own check (service.health.probe) adds its row under the sandbox's, renamed Sandbox says, a note
// only when the two disagree, and the lease line renewal's own rule gives (serviceHealthRenews).
import { HeartPulse } from "lucide-react";
import type { RunDetail, RunService, RunServiceProbe, RunServiceStatus } from "../../../lib/types";
import { cn } from "../../ui/utils";
import {
  DEFAULT_HEALTH_FAILURES, DEFAULT_HEALTH_INTERVAL_SEC, SERVICE_HEALTH, SERVICE_PANEL as T, agoText, probeOn, serviceHealthRenews, shownProbeHealth,
} from "../../wardyn/copy/run-service";
import { Chip, WidgetCard } from "../../wardyn/primitives";
import { keptUntilText } from "./run-ends-row";

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
    if (st.reason === "paused") return { text: T.PAUSED, failing: false };
  }
  return undefined;
}

/** What one finding of Wardyn's check says, in the words of its detail line. */
function findingText(s: RunService, p: RunServiceProbe): string {
  const port = s.port ?? 0;
  switch (p.reason) {
    case "refused":
      return T.PROBE_REFUSED(port);
    case "timeout":
      return T.PROBE_TIMEOUT;
    case "bad_answer":
      return T.PROBE_BAD_ANSWER;
  }
  return s.health?.path && p.http_status ? T.PROBE_HTTP(s.health.path, p.http_status) : T.PROBE_TCP(port);
}

/** The line under Wardyn's check's chip: why it cannot be read, a failure not yet at the threshold, or its finding. */
function probeDetail(s: RunService, p: RunServiceProbe): string {
  if (p.reason === "paused") return T.PAUSED;
  if (p.reason === "unreachable" || p.reason === "not_run") return T.REASON_UNREACHABLE;
  if (p.failures > 0 && p.health !== "unhealthy") return T.PROBE_FIRST_FAIL(findingText(s, p), s.health?.failure_threshold || DEFAULT_HEALTH_FAILURES);
  return findingText(s, p);
}

/** The Lease row. With Wardyn's check on, a capped lease reads renewal's own rule now: renewed while both checks allow it, else not now. */
function leaseText(run: RunDetail, uncapped: boolean, checked: boolean, now: number): string {
  if (!run.ends_at) return T.LEASE_NONE;
  const ends = keptUntilText(run.ends_at, now);
  if (uncapped) return ends;
  if (!checked) return T.LEASE_RENEWED(ends);
  return serviceHealthRenews(run, now) ? T.LEASE_BOTH(ends) : T.LEASE_NOT_NOW(ends);
}

export function ServicePanel({ run, now = Date.now() }: { run: RunDetail; now?: number }) {
  const s = run.service;
  if (!s || run.state !== "RUNNING") return null;
  const st = run.service_status;
  const health = SERVICE_HEALTH[st?.health ?? "starting"] ?? SERVICE_HEALTH.unknown;
  const reason = reasonOf(s, st);
  const checked = probeOn(s);
  const probe = st?.probe;
  // With no reading yet: Starting inside the supervisor's grace, Health unknown after it, as renewal reads it.
  const probeState = shownProbeHealth(run, now) ?? "starting";
  const probeHealth = SERVICE_HEALTH[probeState];
  // The notes name only the two combinations they describe; any other pair says nothing more than its rows.
  const disagreement =
    checked && st?.health === "healthy" && probe?.health === "unhealthy" ? T.DISAGREE_PROBE_FAILS(s.port ?? 0)
    : checked && st?.health === "unhealthy" && probe?.health === "healthy" ? T.DISAGREE_SANDBOX_FAILS(s.port ?? 0)
    : undefined;
  const restarts = st?.restarts ?? 0;
  // The server renews a service's lease only under a lifetime cap (planServiceRenewal), so an uncapped one ends at its lease.
  const uncapped = !!run.ends_at && !run.lifetime_cap_at;
  return (
    // The card is the root (not a wrapper div), so the tile's fill rule reaches it and the footnote is never clipped.
    <WidgetCard title={T.TITLE} Icon={HeartPulse} data-testid="service-panel" bodyClassName="space-y-3 px-3 py-2.5 text-body">
      <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5">
        <dt className="text-muted-foreground">{T.RESTART}</dt>
        <dd>{s.restart === "never" ? T.RESTART_NEVER : T.RESTART_ON_FAILURE}</dd>
        <dt className="text-muted-foreground">{T.CHECK}</dt>
        <dd className="font-mono text-xs leading-5">{checkText(s)}</dd>
        <dt className="text-muted-foreground">{checked ? T.SANDBOX_SAYS : T.HEALTH}</dt>
        <dd>
          <Chip tone={health.tone} dot>
            {health.word}
          </Chip>{" "}
          <span className="text-muted-foreground">· {st && st.health !== "starting" ? T.CHECKED(agoText(st.observed_at, now)) : T.WAITING_FIRST_CHECK}</span>
        </dd>
        {checked && (
          <>
            <dt className="text-muted-foreground">{T.PROBE}</dt>
            <dd>
              <Chip tone={probeHealth.tone} dot>
                {probeHealth.word}
              </Chip>{" "}
              {(probe || probeState === "starting") && (
                <span className="text-muted-foreground">· {probe ? T.CHECKED(agoText(probe.observed_at, now)) : T.WAITING_FIRST_CHECK}</span>
              )}
              {(probe || probeState !== "starting") && (
                <span className="mt-0.5 block text-xs leading-snug text-muted-foreground">{probe ? probeDetail(s, probe) : T.PROBE_NONE_YET}</span>
              )}
            </dd>
          </>
        )}
        <dt className="text-muted-foreground">{T.RESTARTS}</dt>
        <dd>{st?.last_exit_code !== undefined ? T.RESTARTS_WITH_EXIT(restarts, st.last_exit_code) : restarts}</dd>
        <dt className="text-muted-foreground">{T.LEASE}</dt>
        <dd>{leaseText(run, uncapped, checked, now)}</dd>
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
      {disagreement && (
        <p role="note" className="rounded-lg border border-warning/25 bg-warning-subtle px-2.5 py-2 text-foreground">
          {disagreement}
        </p>
      )}
      {uncapped && (
        <p role="note" className="rounded-lg border border-border bg-muted/40 px-2.5 py-2 text-foreground">
          {T.NO_CAP}
        </p>
      )}
      <p className="text-xs text-muted-foreground">{checked ? T.FOOTNOTE_PROBE : T.FOOTNOTE}</p>
    </WidgetCard>
  );
}
