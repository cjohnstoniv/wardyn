/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Workloads screen (mock r16 T3-a): the named run requests Wardyn keeps running, applied from the command
// line or by a Kubernetes custom resource. One screen serves the User view (/workloads: the caller's own and
// those of the service identities they sponsor) and the Admin view (/admin/workloads: every owner's); the server
// scopes the list. A row opens the detail drawer; the run header's workload chip lands here with the drawer
// open (?workload=<name>&owner=<owner>).
import * as React from "react";
import { Layers } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import { workloads as workloadsApi } from "../../lib/api/workloads";
import { getErrorMessage } from "../../lib/format";
import type { Workload } from "../../lib/types/workloads";
import { Mono } from "../wardyn/code-block";
import { runPath, useConsoleMode } from "../wardyn/console-view";
import { agoShort, WORKLOADS as W } from "../wardyn/copy/workloads";
import { usePrincipal } from "../wardyn/operator-context";
import { PageContainer } from "../wardyn/page-container";
import { PageHeader } from "../wardyn/page-header";
import { ErrorState, TableSkeleton } from "../wardyn/states";
import { WorkloadDrawer } from "./workloads/workload-drawer";
import { WorkloadBadge } from "./workloads/workload-state";

const POLL_MS = 15_000;

const sameWorkload = (w: Workload, name: string | null, owner: string | null) => w.name === name && (!owner || w.owner === owner);

export function WorkloadsScreen() {
  const view = useConsoleMode();
  const principal = usePrincipal();
  const [params, setParams] = useSearchParams();
  const [rows, setRows] = React.useState<Workload[] | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [now, setNow] = React.useState(() => Date.now());

  // No "loading" flip here: this is also the poll and the refresh after an action, and the first read owns the skeleton.
  const load = React.useCallback(() => {
    workloadsApi
      .list()
      .then((list) => {
        setRows(list);
        setLoadError(null);
        setNow(Date.now());
      })
      .catch((e) => setLoadError(getErrorMessage(e)));
  }, []);
  React.useEffect(() => {
    load();
    const id = setInterval(load, POLL_MS);
    return () => clearInterval(id);
  }, [load]);

  const openName = params.get("workload");
  const openOwner = params.get("owner");
  const selected = rows?.find((w) => sameWorkload(w, openName, openOwner)) ?? null;
  const open = (w: Workload | null) => setParams(w ? { workload: w.name, owner: w.owner } : {}, { replace: true });

  let body: React.ReactNode;
  if (loadError !== null) body = <ErrorState message={loadError} onRetry={load} />;
  else if (!rows) body = <TableSkeleton rows={4} cols={6} />;
  else if (rows.length === 0)
    body = (
      <div className="flex flex-col items-center gap-2.5 px-6 py-12 text-center">
        <Layers className="size-5 text-muted-foreground" aria-hidden />
        <p className="text-sm text-foreground">
          {W.EMPTY_LEAD} <Mono className="text-foreground">{W.EMPTY_COMMAND}</Mono>
          {W.EMPTY_END}
        </p>
      </div>
    );
  else
    body = (
      <div className="overflow-x-auto">
        <table aria-label={W.LIST_ARIA} className="w-full border-collapse text-sm">
          <thead>
            <tr className="bg-surface-2 text-left">
              {[W.COL_NAME, W.COL_OWNER, W.COL_SOURCE, W.COL_RUN, W.COL_STATE, W.COL_UPDATED].map((c) => (
                <th key={c} scope="col" className="label-eyebrow px-4 py-2.5 font-medium">
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((w) => (
              <tr key={`${w.owner}/${w.name}`} data-testid="workload-row" className="border-t border-border hover:bg-surface-2/60">
                <td className="px-4 py-2.5">
                  <button
                    type="button"
                    onClick={() => open(w)}
                    className="rounded font-mono text-xs font-medium text-foreground hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
                  >
                    {w.name}
                  </button>
                </td>
                <td className="px-4 py-2.5">{w.owner === principal ? W.OWNER_YOU : w.owner || W.NONE}</td>
                <td className="px-4 py-2.5">
                  {w.source === "k8s" ? (
                    <>
                      {W.SOURCE_K8S_PREFIX}
                      <Mono className="text-foreground">{w.namespace}</Mono>
                    </>
                  ) : (
                    W.SOURCE_API
                  )}
                </td>
                <td className="px-4 py-2.5">
                  {w.run_id ? (
                    <Link to={runPath(view, w.run_id)} className="font-mono text-xs text-info hover:underline">
                      {w.run_id.slice(0, 8)}
                    </Link>
                  ) : (
                    W.NONE
                  )}
                </td>
                <td className="px-4 py-2.5">
                  <WorkloadBadge workload={w} />
                </td>
                <td className="px-4 py-2.5 text-muted-foreground">{agoShort(w.updated_at, now)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    );

  return (
    <PageContainer width="list" className="space-y-5 pb-24">
      <PageHeader title={W.TITLE} />
      <div className="overflow-hidden rounded-xl border border-border bg-card">{body}</div>
      <WorkloadDrawer workload={selected} view={view} now={now} onClose={() => open(null)} onChanged={load} />
    </PageContainer>
  );
}
