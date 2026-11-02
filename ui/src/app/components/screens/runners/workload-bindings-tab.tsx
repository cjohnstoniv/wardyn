/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin -> Runners -> Workload bindings (mock r16 T3-b): which namespace's custom resources launch runs as which
// service identity, on which pool. A binding is standing authority over the identity, so it is the security
// tier's to make and delete; both admin tiers reach the tab, and the server refuses anyone else.
import * as React from "react";
import { Plus } from "lucide-react";
import { toast } from "sonner";
import { runnerPools } from "../../../lib/api/runner-pools";
import { serviceIdentities } from "../../../lib/api/service-identities";
import { workloadBindings } from "../../../lib/api/workloads";
import { getErrorMessage } from "../../../lib/format";
import type { ServiceIdentity } from "../../../lib/types/service-identities";
import type { WorkloadBinding } from "../../../lib/types/workloads";
import { Button } from "../../ui/button";
import { Mono } from "../../wardyn/code-block";
import { agoLong, WORKLOAD_BINDINGS as B } from "../../wardyn/copy/workloads";
import { ErrorState, TableSkeleton } from "../../wardyn/states";
import { ConfirmDialog } from "./confirm-dialog";
import { BindNamespaceDialog, servicePrincipal } from "./workload-bind-dialog";

interface PoolName {
  id: string;
  name: string;
  scope: string;
}

export function WorkloadBindingsTab() {
  const [bindings, setBindings] = React.useState<WorkloadBinding[] | null>(null);
  const [pools, setPools] = React.useState<PoolName[]>([]);
  const [services, setServices] = React.useState<ServiceIdentity[]>([]);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [binding, setBinding] = React.useState(false);
  const [deleting, setDeleting] = React.useState<WorkloadBinding | null>(null);
  const [now, setNow] = React.useState(() => Date.now());

  const load = React.useCallback(() => {
    Promise.all([workloadBindings.list(), runnerPools.list(), serviceIdentities.listAll()])
      .then(([list, catalogue, identities]) => {
        setBindings(list);
        setPools(catalogue.pools.map((p) => ({ id: p.id, name: p.name, scope: p.scope })));
        setServices(identities.filter((s) => s.state !== "suspended" && s.state !== "retired"));
        setLoadError(null);
        setNow(Date.now());
      })
      .catch((e) => setLoadError(getErrorMessage(e)));
  }, []);
  React.useEffect(load, [load]);

  const poolName = (id: string) => pools.find((p) => p.id === id)?.name ?? id;

  let body: React.ReactNode;
  if (loadError !== null) body = <ErrorState message={loadError} onRetry={load} />;
  else if (!bindings) body = <TableSkeleton rows={3} cols={5} />;
  else if (bindings.length === 0) body = <p className="px-4 py-8 text-center text-sm text-muted-foreground">{B.EMPTY}</p>;
  else
    body = (
      <div className="overflow-x-auto">
        <table className="w-full border-collapse text-sm">
          <thead>
            <tr className="bg-surface-2 text-left">
              {[B.COL_POOL, B.COL_NAMESPACE, B.COL_SERVICE, B.COL_SPONSOR, B.COL_CREATED].map((c) => (
                <th key={c} scope="col" className="label-eyebrow px-4 py-2.5 font-medium">
                  {c}
                </th>
              ))}
              <th scope="col" className="w-0 px-4 py-2.5">
                <span className="sr-only">{B.DELETE}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {bindings.map((b) => (
              <tr key={b.id} data-testid="binding-row" className="border-t border-border">
                <td className="px-4 py-2.5">{poolName(b.pool_id)}</td>
                <td className="px-4 py-2.5">
                  <Mono className="text-foreground">{b.namespace}</Mono>
                </td>
                <td className="px-4 py-2.5">
                  <Mono className="text-foreground">{servicePrincipal(b.service)}</Mono>
                </td>
                <td className="px-4 py-2.5">{b.sponsor}</td>
                <td className="px-4 py-2.5 text-muted-foreground">{agoLong(b.created_at, now)}</td>
                <td className="px-4 py-2.5 text-right">
                  <Button variant="ghost" size="sm" aria-label={`${B.DELETE} ${b.namespace}`} onClick={() => setDeleting(b)}>
                    {B.DELETE}
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    );

  return (
    <div className="space-y-2.5">
      <div className="flex justify-end">
        <Button size="sm" onClick={() => setBinding(true)}>
          <Plus className="size-4" /> {B.BIND}
        </Button>
      </div>
      <div className="overflow-hidden rounded-xl border border-border bg-card">{body}</div>
      {binding && (
        <BindNamespaceDialog
          pools={pools}
          services={services}
          onClose={() => setBinding(false)}
          onBound={(ns) => {
            setBinding(false);
            toast.success(B.BOUND(ns));
            load();
          }}
        />
      )}
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={B.DELETE_TITLE(deleting?.namespace ?? "")}
        body={[B.DELETE_BODY]}
        confirmLabel={B.DELETE_CONFIRM}
        onConfirm={() => workloadBindings.remove(deleting?.id ?? "")}
        onDone={() => {
          toast.success(B.UNBOUND(deleting?.namespace ?? ""));
          load();
        }}
        onRaced={load}
      />
    </div>
  );
}
