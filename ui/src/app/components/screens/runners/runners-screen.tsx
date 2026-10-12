/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view -> Runners (0.9, packet M-RN-1): every person's registered computer, whether it is connected,
// and what it says about itself, with revoke. Both admin tiers read and revoke; only the super admin
// creates a registration token or turns runners on and off, and hiding is cosmetic (the server gates every
// route). An admin never claims: a claim is the owner's step, so no control here offers one.
import * as React from "react";
import { AlertTriangle, Laptop } from "lucide-react";
import { SECURITY_ONLY_REASON } from "../../wardyn/copy";
import { RUNNERS } from "../../wardyn/copy/runners";
import { useOperator } from "../../wardyn/operator-context";
import { useRovingRadio } from "../../wardyn/use-roving-radio";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { PageHeader } from "../../wardyn/page-header";
import { EmptyState, STATES, TableSkeleton } from "../../wardyn/states";
import { relativeTime, absoluteTime } from "../../../lib/format";
import type { RunnerView } from "../../../lib/types/runners";
import { MintTokenDialog } from "./mint-token-dialog";
import { RunnerChip } from "./runner-chip";
import { RunnerDrawer } from "./runner-drawer";
import { capacityShort, postureShort, sortRunners, timeLeft } from "./runner-display";
import { RunnersSwitchDialog } from "./runners-switch";
import { UnusedTokens } from "./unused-tokens";
import { useRunnersData } from "./use-runners-data";
import { runners as runnersApi } from "../../../lib/api/runners";

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

function RunnerRow({ r, onOpen }: { r: RunnerView; onOpen: (r: RunnerView) => void }) {
  const waiting = r.state === "unclaimed";
  return (
    <TableRow className="cursor-pointer" onClick={() => onOpen(r)}>
      <TableCell className="font-medium">
        <button
          type="button"
          className="text-left hover:underline"
          onClick={(e) => {
            e.stopPropagation();
            onOpen(r);
          }}
        >
          {r.name}
        </button>
        <div className="font-normal text-muted-foreground sm:hidden">{r.owner}</div>
      </TableCell>
      <TableCell className="hidden sm:table-cell">{r.owner}</TableCell>
      <TableCell>
        <RunnerChip runner={r} />
        {waiting && (
          <div className="mt-1 text-meta text-muted-foreground">
            {[RUNNERS.WAITING_CLAIM(r.owner ?? ""), r.claim_expires_at && RUNNERS.EXPIRES_IN(timeLeft(r.claim_expires_at))].filter(Boolean).join(" · ")}
          </div>
        )}
      </TableCell>
      <TableCell className="text-muted-foreground" title={r.last_seen_at ? absoluteTime(r.last_seen_at) : undefined}>
        {r.online ? "now" : r.last_seen_at ? relativeTime(r.last_seen_at) : "—"}
      </TableCell>
      <TableCell className="hidden text-muted-foreground lg:table-cell">{waiting ? "" : postureShort(r.posture)}</TableCell>
      <TableCell className="hidden text-muted-foreground lg:table-cell">{waiting ? "" : capacityShort(r.capacity)}</TableCell>
    </TableRow>
  );
}

export function RunnersScreen() {
  const superAdmin = useOperator();
  const { status, enabled, rows, tokens, reload } = useRunnersData();
  const [filter, setFilter] = React.useState<Filter>("active");
  const [search, setSearch] = React.useState("");
  const [q, setQ] = React.useState("");
  const [selected, setSelected] = React.useState<RunnerView | null>(null);
  const [minting, setMinting] = React.useState(false);
  const [switching, setSwitching] = React.useState<"on" | "off" | null>(null);

  // The box debounces into q, so each keystroke is not a pass over the rows.
  React.useEffect(() => {
    const id = setTimeout(() => setQ(search.trim()), 200);
    return () => clearTimeout(id);
  }, [search]);

  // The open runner follows the page's own reads, so its chip changes in place.
  React.useEffect(() => {
    setSelected((cur) => (cur ? (rows.find((r) => r.id === cur.id) ?? cur) : cur));
  }, [rows]);

  const visible = React.useMemo(() => {
    const needle = q.toLowerCase();
    const byFilter = rows.filter((r) => (filter === "all" ? true : filter === "revoked" ? r.state === "revoked" : r.state !== "revoked"));
    return sortRunners(byFilter.filter((r) => !needle || r.name.toLowerCase().includes(needle) || (r.owner ?? "").toLowerCase().includes(needle)));
  }, [rows, filter, q]);

  // The count is announced only when it changes, and never per poll.
  const count = visible.length;
  const [announced, setAnnounced] = React.useState("");
  React.useEffect(() => setAnnounced(COUNT(count)), [count]);

  const changed = (id: string) => {
    reload();
    void runnersApi.get(id).then(setSelected).catch(() => {});
  };

  const createToken = superAdmin && enabled && status === "ready";
  const empty = rows.length === 0;
  const actions = createToken ? (
    <div className="flex items-center gap-2">
      <Button variant="outline" onClick={() => setSwitching("off")}>
        {RUNNERS.TURN_OFF}
      </Button>
      {/* The page's one default. */}
      <Button onClick={() => setMinting(true)}>{RUNNERS.MINT_CTA}</Button>
    </div>
  ) : undefined;

  let body: React.ReactNode;
  if (status === "loading") body = <TableSkeleton rows={5} cols={6} />;
  else if (status === "forbidden") body = <p className="p-4 text-sm text-muted-foreground">{SECURITY_ONLY_REASON}</p>;
  else if (status === "error") body = <LoadFailed onRetry={reload} />;
  else if (!enabled) {
    body = (
      <EmptyState
        icon={Laptop}
        title={RUNNERS.OFF_TITLE}
        description={superAdmin ? RUNNERS.OFF_BODY : `${RUNNERS.OFF_BODY} ${RUNNERS.OFF_SECURITY}`}
        action={superAdmin ? <Button onClick={() => setSwitching("on")}>{RUNNERS.TURN_ON}</Button> : undefined}
      />
    );
  } else if (empty) {
    body = (
      <EmptyState
        icon={Laptop}
        title={RUNNERS.EMPTY_TITLE}
        description={RUNNERS.EMPTY_BODY}
        action={superAdmin ? <Button onClick={() => setMinting(true)}>{RUNNERS.MINT_CTA}</Button> : undefined}
      />
    );
  } else {
    body = (
      <>
        <div className="flex flex-wrap items-center gap-2 border-b border-border p-3">
          <Input aria-label={RUNNERS.SEARCH} placeholder={RUNNERS.SEARCH} className="max-w-xs" autoComplete="off" value={search} onChange={(e) => setSearch(e.target.value)} />
          <FilterGroup filter={filter} onChange={setFilter} />
          <span role="status" className="sr-only">
            {announced}
          </span>
        </div>
        {visible.length === 0 ? (
          <p className="p-4 text-sm text-muted-foreground">
            {q ? RUNNERS.SEARCH_EMPTY(q) : filter === "revoked" ? RUNNERS.FILTER_EMPTY_REVOKED : RUNNERS.FILTER_EMPTY_ACTIVE}
          </p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{RUNNERS.COL_RUNNER}</TableHead>
                  <TableHead className="hidden sm:table-cell">{RUNNERS.COL_OWNER}</TableHead>
                  <TableHead>{RUNNERS.COL_STATE}</TableHead>
                  <TableHead>{RUNNERS.COL_LAST_SEEN}</TableHead>
                  <TableHead className="hidden lg:table-cell">{RUNNERS.COL_POSTURE}</TableHead>
                  <TableHead className="hidden lg:table-cell">{RUNNERS.COL_CAPACITY}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {visible.map((r) => (
                  <RunnerRow key={r.id} r={r} onOpen={setSelected} />
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </>
    );
  }

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <PageHeader title={RUNNERS.NAV} description={RUNNERS.LEAD} actions={actions} />
      <div className="overflow-hidden rounded-xl border border-border bg-card">{body}</div>
      {status === "ready" && <UnusedTokens tokens={tokens} onChanged={reload} />}
      <RunnerDrawer runner={selected} onClose={() => setSelected(null)} onChanged={changed} />
      <MintTokenDialog open={minting} onOpenChange={setMinting} onMinted={reload} />
      <RunnersSwitchDialog mode={switching} onOpenChange={(o) => !o && setSwitching(null)} onChanged={reload} />
    </div>
  );
}
