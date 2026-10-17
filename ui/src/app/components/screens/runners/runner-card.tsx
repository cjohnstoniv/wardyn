/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One runner on the Admin -> Runners tab (mock M-POOLS v6, owner review round 1: a card per runner, so
// where one runner ends and the next begins is plain). Header: its name, kind, state, Machine, owner and
// the pools it is in; body: its resource table. A self-hosted runner that is not claimed, or is revoked,
// has reported nothing that counts and takes no run, so it shows its header alone.
import * as React from "react";
import { absoluteTime, relativeTime } from "../../../lib/format";
import type { RemoteRunnerPoolRef, RemoteRunnerView } from "../../../lib/types/runner-pools";
import type { RunnerPoolRef, RunnerView } from "../../../lib/types/runners";
import { RES, RUNNER_KIND, RUNNER_V6 } from "../../wardyn/copy/runner-pools";
import { RUNNER, RUNNERS } from "../../wardyn/copy/runners";
import { Mono } from "../../wardyn/code-block";
import { Chip } from "../../wardyn/primitives";
import { OfflineNote, ResourceTable } from "./resource-table";
import { runnerRes } from "./resources";
import { RunnerChip } from "./runner-chip";
import { timeLeft } from "./runner-display";

/** A runner of either kind, as the Runners tab lists and searches them. */
export type FleetRunner = { kind: "self_hosted"; id: string; name: string; runner: RunnerView } | { kind: "remote_hosted"; id: string; name: string; remote: RemoteRunnerView };

export const fleetOf = (rows: RunnerView[], remote: RemoteRunnerView[]): FleetRunner[] => [
  ...rows.map((runner): FleetRunner => ({ kind: "self_hosted", id: runner.id, name: runner.name, runner })),
  ...remote.map((r): FleetRunner => ({ kind: "remote_hosted", id: r.id, name: r.name, remote: r })),
];

/** What the search box matches: name, Machine, owner (a remote-hosted runner's is the organization) and kind. */
export const searchText = (f: FleetRunner): string =>
  (f.kind === "self_hosted"
    ? `${f.name} ${f.runner.machine_name ?? ""} ${f.runner.owner ?? ""} ${RUNNER_KIND.SELF}`
    : `${f.name} ${RUNNER_V6.ORGANIZATION} ${RUNNER_KIND.REMOTE}`
  ).toLowerCase();

const LINK = "rounded-sm text-info underline underline-offset-2 hover:no-underline";

export function MachineText({ machine }: { machine?: string }) {
  return machine ? <Mono className="break-all text-foreground">{machine}</Mono> : <span className="text-muted-foreground">{RUNNER_V6.NOT_REPORTED}</span>;
}

/** The architecture a runner reported, beside its Machine; nothing until it reports one. */
export function ArchText({ arch }: { arch?: string }) {
  return arch ? (
    <div className="text-muted-foreground" data-arch>
      {RUNNER_V6.ARCH}: <Mono className="text-foreground">{arch}</Mono>
    </div>
  ) : null;
}

/** The pools a runner is in, each a link to its details; a remote-hosted runner's user pools name their owner. */
export function PoolLinks({ pools, onOpenPool }: { pools: (RunnerPoolRef | RemoteRunnerPoolRef)[]; onOpenPool: (id: string) => void }) {
  if (pools.length === 0) return <span className="text-muted-foreground">{RUNNER_V6.IN_NO_POOL}</span>;
  return (
    <>
      {pools.map((p, i) => {
        const owner = "owner" in p && p.scope === "user" ? p.owner : undefined;
        return (
          <React.Fragment key={p.id}>
            {i > 0 && ", "}
            <button type="button" className={LINK} onClick={() => onOpenPool(p.id)}>
              {p.name}
            </button>
            {owner && <span className="text-muted-foreground"> · {owner.split("@")[0]}</span>}
          </React.Fragment>
        );
      })}
    </>
  );
}

function Fact({ label, children, ...rest }: { label: string; children: React.ReactNode } & React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div className="min-w-0" {...rest}>
      <dt className="label-eyebrow mb-0.5">{label}</dt>
      <dd className="break-words text-body text-foreground">{children}</dd>
    </div>
  );
}

export function RunnerCard({ f, onOpen, onOpenPool }: { f: FleetRunner; onOpen: (f: FleetRunner) => void; onOpenPool: (id: string) => void }) {
  const self = f.kind === "self_hosted" ? f.runner : undefined;
  const shared = f.kind === "remote_hosted" ? f.remote : undefined;
  const { limits, usage, online, pools } = f.kind === "self_hosted" ? f.runner : f.remote;
  const waiting = self?.state === "unclaimed";
  // Only a runner that can take a run shows what it has: a claimed computer, or a remote-hosted runner.
  const counts = !self || self.state === "claimed";
  const res = runnerRes(limits, usage);
  return (
    <li aria-label={f.name} className="min-w-0 list-none overflow-hidden rounded-xl border border-border-strong bg-card">
      <header className={`bg-surface-2 px-3.5 py-3 ${counts ? "border-b border-border" : ""}`}>
        <div className="mb-2 flex flex-wrap items-center gap-x-2.5 gap-y-1.5">
          <button type="button" className="rounded-sm text-left text-sm font-semibold text-foreground hover:underline" onClick={() => onOpen(f)}>
            {f.name}
          </button>
          <Chip>{self ? RUNNER_KIND.SELF : RUNNER_KIND.REMOTE}</Chip>
          {self ? <RunnerChip runner={self} /> : <Chip dot>{online ? RUNNER.ONLINE : RUNNER.OFFLINE}</Chip>}
          {shared && !shared.configured && <Chip tone="warning">{RUNNER_KIND.NOT_CONFIGURED}</Chip>}
          {self && self.state === "claimed" && (online || self.last_seen_at) && (
            <span className="text-xs text-muted-foreground" title={self.last_seen_at ? absoluteTime(self.last_seen_at) : undefined}>
              {RUNNER_V6.LAST_SEEN(online ? "now" : relativeTime(self.last_seen_at ?? ""))}
            </span>
          )}
        </div>
        {waiting && self && (
          <p className="mb-2 text-xs text-muted-foreground">
            {[RUNNERS.WAITING_CLAIM(self.owner ?? ""), self.claim_expires_at && RUNNERS.EXPIRES_IN(timeLeft(self.claim_expires_at))].filter(Boolean).join(" · ")}
          </p>
        )}
        <dl className="grid grid-cols-1 gap-x-4 gap-y-2 md:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)_minmax(0,1.3fr)]">
          <Fact label={RUNNER_V6.MACHINE} data-machine>
            {self ? <><MachineText machine={self.machine_name} /><ArchText arch={self.arch} /></> : <span className="text-muted-foreground">{RUNNER_V6.NOT_APPLICABLE}</span>}
          </Fact>
          <Fact label={RUNNER_V6.OWNER}>{self ? self.owner : RUNNER_V6.ORGANIZATION}</Fact>
          <Fact label={RUNNER_V6.POOLS} data-pools>
            <PoolLinks pools={pools} onOpenPool={onOpenPool} />
          </Fact>
        </dl>
      </header>
      {counts && (
        <div className="min-w-0 px-3.5 pb-3 pt-2.5">
          {res ? <ResourceTable view={res} /> : <p className="text-body text-muted-foreground">{RES.LIMITS_NOT_SET}</p>}
          {!online && <OfflineNote />}
        </div>
      )}
    </li>
  );
}
