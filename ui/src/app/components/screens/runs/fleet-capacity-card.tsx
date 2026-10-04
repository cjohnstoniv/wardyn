/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.8.6 fleet-fl4 (M6, approved 2026-10-03) — the admin Fleet capacity card on
// Runs, over GET /admin/runs/capacity. Collapsed it is one summary line (plus
// a "Waiting for room" chip when any run is); expanded it shows what Wardyn
// asked each runner to set aside. Every figure is a configured reservation,
// never a measurement, and Kubernetes and Docker are never summed together:
// one block per runner kind. Security operators only, like the route.
import * as React from "react";
import { Link } from "react-router-dom";
import { runs as runsApi } from "../../../lib/api/runs";
import { relativeTime } from "../../../lib/format";
import type { RunCapacityResponse, RunCapacitySums, RunCapacityUnschedulable } from "../../../lib/types";
import { Button } from "../../ui/button";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { Mono } from "../../wardyn/code-block";
import { runPath, useConsoleMode } from "../../wardyn/console-view";
import { FLEET_CAPACITY as FC, FLEET_CAPACITY_AGE_LABELS } from "../../wardyn/copy/fleet-capacity";
import { useSecurityOperator } from "../../wardyn/operator-context";
import { Chip, SectionLabel, runStateLabel } from "../../wardyn/primitives";
import { ErrorState } from "../../wardyn/states";
import { STARTING_UNSCHEDULABLE } from "../run-status-detail";

const OWNERS_COLLAPSED = 5;

// One decimal, no trailing ".0": 36000m -> "36", 2500m -> "2.5".
function oneDecimal(n: number): string {
  return String(Math.round(n * 10) / 10);
}
const cpu = (millis: number) => oneDecimal(millis / 1000);
const gib = (mib: number) => oneDecimal(mib / 1024);

function Figure({ label, cpuMillis, memMiB, noCap }: { label: string; cpuMillis: number; memMiB: number; noCap?: boolean }) {
  return (
    <span title={`${cpuMillis}m / ${memMiB}Mi`}>
      <span className="text-muted-foreground">{label}</span>{" "}
      <span className="text-foreground">
        {noCap ? FC.NO_CAP : `${cpu(cpuMillis)} CPU`} · {gib(memMiB)} GiB
      </span>
    </span>
  );
}

// The runner kind the response's totals describe: the by_runner entry the
// totals equal (the server fills totals from the deployment's current kind).
function currentKind(d: RunCapacityResponse): string | undefined {
  const keys = Object.keys(d.by_runner).sort();
  return (
    keys.find((k) => {
      const r = d.by_runner[k];
      return r.basis === d.totals.basis && r.holding === d.totals.holding && r.held_cpu_millis === d.totals.held_cpu_millis;
    }) ?? keys[0]
  );
}

function reservation(u: RunCapacityUnschedulable): string | null {
  if (u.agent_cpu_request_millis == null || u.agent_memory_request_mib == null) return null;
  const c = u.agent_cpu_request_millis + (u.proxy_cpu_millis ?? 0);
  const m = u.agent_memory_request_mib + (u.proxy_memory_mib ?? 0);
  return `${cpu(c)} CPU · ${gib(m)} GiB`;
}

function RunnerBlock({ kind, d }: { kind: string; d: RunCapacityResponse }) {
  const r = d.by_runner[kind];
  const k8s = r.basis === "requests";
  const proxyNoCap = !k8s && r.proxy_cpu_uncapped > 0 && r.proxy_cpu_millis === 0;
  return (
    <div className="mt-3" data-testid={`fleet-capacity-runner-${kind}`}>
      <SectionLabel>{k8s ? FC.RUNNER_K8S : FC.RUNNER_DOCKER}</SectionLabel>
      <p className="mt-1 text-meta text-muted-foreground">{k8s ? FC.BASIS_REQUESTS : FC.BASIS_CAPS}</p>
      <div className="mt-1 flex flex-wrap gap-x-5 gap-y-1 text-body">
        {k8s ? (
          <>
            <Figure label={FC.AGENT_REQUESTS} cpuMillis={r.agent_cpu_request_millis} memMiB={r.agent_memory_request_mib} />
            <Figure label={FC.AGENT_LIMITS} cpuMillis={r.agent_cpu_limit_millis} memMiB={r.agent_memory_limit_mib} />
          </>
        ) : (
          <Figure label={FC.AGENT_CAPS} cpuMillis={r.agent_cpu_limit_millis} memMiB={r.agent_memory_limit_mib} />
        )}
        <Figure label={FC.PROXY} cpuMillis={r.proxy_cpu_millis} memMiB={r.proxy_memory_mib} noCap={proxyNoCap} />
      </div>
      {r.unknown > 0 && (
        <p className="mt-1 text-meta text-muted-foreground">
          {FC.UNKNOWN(r.unknown)} {FC.UNKNOWN_HINT}
        </p>
      )}
    </div>
  );
}

function ownerSums(by: Record<string, RunCapacitySums>, kind: string | undefined): RunCapacitySums | undefined {
  return kind ? by[kind] : undefined;
}

export function FleetCapacityCard() {
  const securityOperator = useSecurityOperator();
  const view = useConsoleMode();
  const [data, setData] = React.useState<RunCapacityResponse | null>(null);
  const [failed, setFailed] = React.useState(false);
  const [showAll, setShowAll] = React.useState(false);

  const load = React.useCallback(() => {
    setFailed(false);
    runsApi
      .getAdminRunCapacity()
      .then(setData)
      .catch(() => setFailed(true));
  }, []);

  React.useEffect(() => {
    if (securityOperator) load();
  }, [securityOperator, load]);

  if (!securityOperator) return null;

  const summary = data ? (
    <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-1">
      <span>
        {data.totals.holding === 0
          ? FC.SUMMARY_NONE
          : FC.SUMMARY(data.totals.holding, cpu(data.totals.held_cpu_millis), gib(data.totals.held_memory_mib))}
        {data.totals.unknown > 0 && ` ${FC.SUMMARY_UNKNOWN(data.totals.unknown)}`}
      </span>
      {data.unschedulable_total > 0 && <Chip tone="warning">{FC.WAITING_CHIP(data.unschedulable_total)}</Chip>}
    </span>
  ) : undefined;

  const kind = data ? currentKind(data) : undefined;
  const owners = data ? (showAll ? data.by_owner : data.by_owner.slice(0, OWNERS_COLLAPSED)) : [];
  const stateLine = data
    ? [
        [runStateLabel("RUNNING"), data.states.RUNNING ?? 0],
        [runStateLabel("STARTING"), data.states.STARTING ?? 0],
        [runStateLabel("WAITING_FOR_CONFIRMATION"), data.states.WAITING_FOR_CONFIRMATION ?? 0],
        [FC.STATES_PAUSED, data.paused],
        [runStateLabel("PENDING"), data.states.PENDING ?? 0],
        [FC.STATES_KEPT, data.kept],
      ]
        .map(([label, n]) => `${label} ${n}`)
        .join(" · ")
    : "";

  return (
    <CollapsibleCard title={FC.TITLE} summary={summary} testId="fleet-capacity-card" className="mb-4">
      {failed ? (
        <ErrorState onRetry={load} as="h4" />
      ) : !data ? null : (
        <div className="space-y-3 text-body">
          <p className="text-muted-foreground">{FC.LEDE}</p>
          <div>
            <p data-testid="fleet-capacity-states">{stateLine}</p>
            <p className="mt-0.5 text-meta text-muted-foreground">{FC.STATES_HINT}</p>
          </div>

          {Object.keys(data.by_runner)
            .sort()
            .map((k) => (
              <RunnerBlock key={k} kind={k} d={data} />
            ))}

          {data.unschedulable_total > 0 && (
            <div data-testid="fleet-capacity-waiting">
              <SectionLabel>{FC.WAITING_CHIP(data.unschedulable_total)}</SectionLabel>
              <p className="mt-1 text-meta text-muted-foreground">{FC.WAITING_LEAD}</p>
              <ul className="mt-1 space-y-1">
                {data.unschedulable.map((u) => {
                  const res = reservation(u);
                  return (
                    <li key={u.id} className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
                      <Link to={runPath(view, u.id)} className="text-info hover:underline">
                        <Mono>{u.id.slice(0, 8)}</Mono>
                      </Link>
                      <span>{u.owner}</span>
                      <span className="text-muted-foreground">{STARTING_UNSCHEDULABLE}</span>
                      <span className="text-muted-foreground">
                        {FC.WAITING_SINCE(relativeTime(new Date(Date.now() - u.waited_seconds * 1000).toISOString()))}
                      </span>
                      {res && <span>{res}</span>}
                    </li>
                  );
                })}
              </ul>
              {data.unschedulable_total > data.unschedulable.length && (
                <p className="mt-1 text-meta text-muted-foreground">
                  {FC.WAITING_MORE(data.unschedulable.length, data.unschedulable_total)}
                </p>
              )}
            </div>
          )}

          <div data-testid="fleet-capacity-age">
            <SectionLabel>{FC.AGE_TITLE}</SectionLabel>
            <p className="mt-1">
              {data.age_buckets.map((b) => `${FLEET_CAPACITY_AGE_LABELS[b.bucket] ?? b.bucket} ${b.count}`).join(" · ")}
            </p>
          </div>

          {data.by_owner.length > 0 && (
            <div data-testid="fleet-capacity-owners">
              <SectionLabel>{FC.OWNERS_TITLE}</SectionLabel>
              <table className="mt-1 w-full text-left">
                <thead className="text-meta text-muted-foreground">
                  <tr>
                    <th className="py-1 font-normal">{FC.OWNERS_COLS.OWNER}</th>
                    <th className="py-1 font-normal">{FC.OWNERS_COLS.RUNS}</th>
                    <th className="py-1 font-normal">{FC.OWNERS_COLS.CPU}</th>
                    <th className="py-1 font-normal">{FC.OWNERS_COLS.MEMORY}</th>
                  </tr>
                </thead>
                <tbody>
                  {owners.map((o) => {
                    const s = ownerSums(o.by_runner, kind);
                    return (
                      <tr key={o.owner}>
                        <td className="py-0.5">{o.owner}</td>
                        <td className="py-0.5">{o.holding}</td>
                        <td className="py-0.5">{s ? `${cpu(s.held_cpu_millis)} CPU` : "—"}</td>
                        <td className="py-0.5">{s ? `${gib(s.held_memory_mib)} GiB` : "—"}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              {!showAll && data.by_owner.length > OWNERS_COLLAPSED && (
                <Button variant="link" size="sm" className="px-0" onClick={() => setShowAll(true)}>
                  {FC.OWNERS_SHOW_ALL(data.by_owner.length)}
                </Button>
              )}
              {data.by_owner_truncated && <p className="mt-1 text-meta text-muted-foreground">{FC.OWNERS_TRUNCATED}</p>}
            </div>
          )}

          <p className="text-meta text-muted-foreground">{FC.RESIDUAL_NOTE}</p>
        </div>
      )}
    </CollapsibleCard>
  );
}
