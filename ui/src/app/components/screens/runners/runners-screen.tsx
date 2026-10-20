/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view -> Runners (0.9, packet M-RN-1 and mock M-POOLS v6): one tab strip, Runners | Pools. The
// Runners tab lists every runner, a card each: people's registered computers (self-hosted) and the Docker
// hosts and clusters this server is set up with (remote-hosted), with what each reports, its limits, how
// much is in use and the pools it is in. Both admin tiers read and revoke; only the super admin creates a
// registration token or turns runners on and off, and hiding is cosmetic (the server gates every route).
// An admin never claims: a claim is the owner's step, so no control here offers one. The tab and the open
// pool live in the address, so a runner's pool link and a pool's member link land on the right sheet.
import * as React from "react";
import { useSearchParams } from "react-router-dom";
import { AlertTriangle, Laptop } from "lucide-react";
import { remoteRunners } from "../../../lib/api/runner-pools";
import type { RunnerKind } from "../../../lib/types/runner-pools";
import { SECURITY_ONLY_REASON } from "../../wardyn/copy";
import { POOLS, RUNNER_V6 } from "../../wardyn/copy/runner-pools";
import { RUNNERS } from "../../wardyn/copy/runners";
import { useOperator } from "../../wardyn/operator-context";
import { AddComputerBlock } from "../../wardyn/add-computer-block";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { PageHeader } from "../../wardyn/page-header";
import { EmptyState, STATES, TableSkeleton } from "../../wardyn/states";
import { MintTokenPanel } from "./mint-token-panel";
import { PoolsTab } from "./pools/pools-tab";
import { RemoteRunnerDrawer } from "./remote-runner-drawer";
import { fleetOf, RunnerCard, searchText, type FleetRunner } from "./runner-card";
import { RunnerDrawer } from "./runner-drawer";
import { sortRunners } from "./runner-display";
import { RunnersSwitchDialog } from "./runners-switch";
import { UnusedTokens } from "./unused-tokens";
import { useRunnersData } from "./use-runners-data";

type Filter = "active" | "revoked" | "all";
const FILTERS: { id: Filter; label: string }[] = [
  { id: "active", label: RUNNERS.FILTER_ACTIVE },
  { id: "revoked", label: RUNNERS.FILTER_REVOKED },
  { id: "all", label: RUNNERS.FILTER_ALL },
];

/**
 * pending H11-P3: recommended text until the owner answers. The packet says the filtered count is announced
 * once in a polite status ("3 runners") but freezes no string for it.
 */
const COUNT = (n: number) => `${n} ${n === 1 ? "runner" : "runners"}`;

// ErrorState's title is fixed ("Something went wrong"); the packet's read-failed frame has its own.
function LoadFailed({ onRetry }: { onRetry: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 px-6 py-16 text-center">
      <div className="flex size-12 items-center justify-center rounded-xl border border-danger/30 bg-danger-subtle text-danger">
        <AlertTriangle className="size-5" />
      </div>
      <div className="space-y-1">
        <h2 className="text-foreground">{RUNNERS.FETCH_FAILED_TITLE}</h2>
        <p className="max-w-sm text-sm text-muted-foreground">{RUNNERS.FETCH_FAILED_BODY}</p>
      </div>
      <Button variant="outline" size="sm" onClick={onRetry}>
        {STATES.RETRY}
      </Button>
    </div>
  );
}

function FilterGroup({ filter, onChange }: { filter: Filter; onChange: (f: Filter) => void }) {
  const group = useRovingRadio(FILTERS.length, FILTERS.findIndex((f) => f.id === filter), (i) => onChange(FILTERS[i].id));
  return (
    <div role="radiogroup" aria-label={RUNNERS.COL_STATE} className="flex gap-1.5" {...group.containerProps}>
      {FILTERS.map((f, i) => {
        const { tabIndex, radioRef } = group.itemProps(i);
        return (
          <button
            key={f.id}
            ref={radioRef}
            tabIndex={tabIndex}
            type="button"
            role="radio"
            aria-checked={filter === f.id}
            onClick={() => onChange(f.id)}
            className={`rounded-full border px-2.5 py-0.5 text-xs ${filter === f.id ? "border-primary text-foreground" : "border-border text-muted-foreground"}`}
          >
            {f.label}
          </button>
        );
      })}
    </div>
  );
}

type Tab = "runners" | "pools";

function Tabs({ tab, onChange }: { tab: Tab; onChange: (t: Tab) => void }) {
  const item = (id: Tab, label: string) => (
    <button
      type="button"
      aria-current={tab === id ? "page" : "false"}
      onClick={() => onChange(id)}
      className={`rounded-lg px-3 py-1 text-body font-medium ${tab === id ? "bg-background text-foreground ring-1 ring-border" : "text-muted-foreground hover:text-foreground"}`}
    >
      {label}
    </button>
  );
  return (
    <nav aria-label={RUNNER_V6.TABS_LABEL} className="mb-4 inline-flex max-w-full gap-0.5 rounded-xl border border-border bg-surface-2 p-0.5">
      {item("runners", RUNNER_V6.TAB_RUNNERS)}
      {item("pools", RUNNER_V6.TAB_POOLS)}
    </nav>
  );
}

export function RunnersScreen() {
  const superAdmin = useOperator();
  const { status, enabled, rows, remote, tokens, reload } = useRunnersData();
  const [params, setParams] = useSearchParams();
  const tab: Tab = params.get("tab") === "pools" ? "pools" : "runners";
  const openPoolId = params.get("pool");
  const [filter, setFilter] = React.useState<Filter>("active");
  const [search, setSearch] = React.useState("");
  const [q, setQ] = React.useState("");
  const [selected, setSelected] = React.useState<{ kind: RunnerKind; id: string } | null>(null);
  const [switching, setSwitching] = React.useState<"on" | "off" | null>(null);

  // The box debounces into q, so each keystroke is not a pass over the rows.
  React.useEffect(() => {
    const id = setTimeout(() => setQ(search.trim()), 200);
    return () => clearTimeout(id);
  }, [search]);

  // A tab change replaces the entry; opening a pool or a runner out of one adds an entry, so Back returns to where it was.
  const go = (next: Tab, pool?: string, push = false) => {
    setParams(next === "pools" ? { tab: "pools", ...(pool ? { pool } : {}) } : {}, { replace: !pool && !push });
  };
  const openPool = (id: string) => {
    setSelected(null);
    go("pools", id);
  };
  const openRunner = (kind: RunnerKind, id: string) => {
    go("runners", undefined, true);
    setSelected({ kind, id });
  };

  const visible = React.useMemo(() => {
    const needle = q.toLowerCase();
    const mine = sortRunners(rows.filter((r) => (filter === "all" ? true : filter === "revoked" ? r.state === "revoked" : r.state !== "revoked")));
    // A remote-hosted runner is never revoked: the Revoked filter shows people's computers alone.
    const shared = filter === "revoked" ? [] : [...remote].sort((a, b) => a.name.localeCompare(b.name));
    return fleetOf(mine, shared).filter((f) => !needle || searchText(f).includes(needle));
  }, [rows, remote, filter, q]);

  // The count is announced only when it changes, and never per poll.
  const count = visible.length;
  const [announced, setAnnounced] = React.useState("");
  React.useEffect(() => setAnnounced(COUNT(count)), [count]);

  // The open sheet follows the page's own reads, so its chip and numbers change in place.
  const selectedRunner = selected?.kind === "self_hosted" ? (rows.find((r) => r.id === selected.id) ?? null) : null;
  const selectedRemote = selected?.kind === "remote_hosted" ? (remote.find((r) => r.id === selected.id) ?? null) : null;

  const addComputer = superAdmin && enabled && status === "ready";
  const empty = rows.length === 0 && remote.length === 0;
  const actions =
    addComputer && tab === "runners" ? (
      <Button variant="outline" onClick={() => setSwitching("off")}>
        {RUNNERS.TURN_OFF}
      </Button>
    ) : undefined;

  const off = (
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      <EmptyState
        icon={Laptop}
        title={RUNNERS.OFF_TITLE}
        description={superAdmin ? RUNNERS.OFF_BODY : `${RUNNERS.OFF_BODY} ${RUNNERS.OFF_SECURITY}`}
        action={superAdmin ? <Button onClick={() => setSwitching("on")}>{RUNNERS.TURN_ON}</Button> : undefined}
      />
    </div>
  );
  const list = (
    <>
      <div className="mb-2.5 flex flex-wrap items-end gap-x-3 gap-y-2">
        <div className="w-full max-w-[380px] space-y-2">
          <Label htmlFor="rn-search">{RUNNERS.SEARCH}</Label>
          <Input id="rn-search" type="search" placeholder={RUNNER_V6.SEARCH_PLACEHOLDER} autoComplete="off" value={search} onChange={(e) => setSearch(e.target.value)} />
        </div>
        <div className="pb-1.5">
          <FilterGroup filter={filter} onChange={setFilter} />
        </div>
        <span role="status" className="sr-only">
          {announced}
        </span>
      </div>
      {visible.length === 0 ? (
        <p className="mt-2 text-sm text-muted-foreground">{q ? RUNNERS.SEARCH_EMPTY(q) : filter === "revoked" ? RUNNERS.FILTER_EMPTY_REVOKED : RUNNERS.FILTER_EMPTY_ACTIVE}</p>
      ) : (
        <ul aria-label={RUNNER_V6.LIST_LABEL} className="grid gap-3.5">
          {visible.map((f) => (
            <RunnerCard key={`${f.kind}:${f.id}`} f={f} onOpen={(x: FleetRunner) => setSelected({ kind: x.kind, id: x.id })} onOpenPool={openPool} />
          ))}
        </ul>
      )}
      <p className="mt-3 text-meta text-muted-foreground">{RUNNER_V6.MACHINE_LEGEND}</p>
    </>
  );

  let body: React.ReactNode;
  if (status === "loading") body = <Framed><TableSkeleton rows={5} cols={6} /></Framed>;
  else if (status === "forbidden") body = <Framed><p className="p-4 text-sm text-muted-foreground">{SECURITY_ONLY_REASON}</p></Framed>;
  else if (status === "error") body = <Framed><LoadFailed onRetry={reload} /></Framed>;
  else if (tab === "pools") body = <PoolsTab runners={rows} remote={remote} openPoolId={openPoolId} onOpenPool={(id) => go("pools", id ?? undefined)} onOpenRunner={openRunner} />;
  else if (empty) {
    body = enabled ? (
      <Framed>
        <EmptyState
          icon={Laptop}
          title={RUNNERS.EMPTY_TITLE}
          description={RUNNERS.EMPTY_BODY}
        />
      </Framed>
    ) : (
      off
    );
  } else {
    // With runners off, people's computers take nothing, and the remote-hosted runners still do.
    body = (
      <>
        {!enabled && <div className="mb-4">{off}</div>}
        {list}
      </>
    );
  }

  const ready = status === "ready";
  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <PageHeader title={RUNNERS.NAV} description={<span className="block max-w-[68ch]">{tab === "pools" ? POOLS.LEAD : RUNNERS.LEAD}</span>} actions={actions} />
      {tab === "runners" && <p className="-mt-2 mb-3 max-w-[80ch] text-body text-muted-foreground">{RUNNER_V6.TAB_PURPOSE}</p>}
      {/* Kept mounted (hidden) on the Pools tab, so a minted token that was not copied survives a tab switch. */}
      {addComputer && (
        <div className="mb-4" hidden={tab !== "runners"}>
          <AddComputerBlock mdm={<MintTokenPanel onMinted={reload} />} />
        </div>
      )}
      {ready && <Tabs tab={tab} onChange={(t) => go(t)} />}
      {body}
      {ready && tab === "runners" && <UnusedTokens tokens={tokens} onChanged={reload} />}
      <RunnerDrawer runner={selectedRunner} onClose={() => setSelected(null)} onChanged={reload} onOpenPool={openPool} />
      <RemoteRunnerDrawer
        runner={selectedRemote}
        onClose={() => setSelected(null)}
        onChanged={reload}
        onOpenPool={openPool}
        onSaveLimits={(r, limits) => remoteRunners.update(r.id, { revision: r.revision, limits })}
      />
      <RunnersSwitchDialog mode={switching} onOpenChange={(o) => !o && setSwitching(null)} onChanged={reload} />
    </div>
  );
}

const Framed = ({ children }: { children: React.ReactNode }) => <div className="overflow-hidden rounded-xl border border-border bg-card">{children}</div>;
