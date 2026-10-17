/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Audit → Retention (mock packet M4, surface A): the retention policy and the
// audit log's partitions, with the drop flow for an eligible one. Security tier
// only (GET /audit/retention is on securityOps). Whether a partition can be
// dropped is the SERVER's answer (`eligible` / `refusal`); the browser never
// works it out, so it cannot offer a drop the server would refuse.
import * as React from "react";
import { ChevronDown } from "lucide-react";
import type { AuditRetentionPartition, AuditRetentionStatus } from "../../lib/types";
import { audit as api, type PartitionExportForm } from "../../lib/api/audit";
import { getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../ui/dropdown-menu";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { Chip, SectionCard, SectionLabel } from "../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../wardyn/states";
import { Mono } from "../wardyn/code-block";
import { DROP, RETENTION, reasonKey } from "../wardyn/copy/audit-retention";
import { AuditRetentionDropDialog } from "./audit-retention-drop-dialog";
import { coversLabel, dayLabel, downloadPartition } from "./audit-retention-util";
import { toast } from "sonner";

function ExportMenu({ partition }: { partition: string }) {
  const [busy, setBusy] = React.useState(false);
  const run = (form: PartitionExportForm) => {
    setBusy(true);
    downloadPartition(partition, form)
      .catch((e) => toast.error(getErrorMessage(e)))
      .finally(() => setBusy(false));
  };
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" size="sm" disabled={busy} aria-label={`${RETENTION.EXPORT} ${partition}`}>
          {RETENTION.EXPORT}
          <ChevronDown className="size-3.5" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onClick={() => run("readable")}>{DROP.READABLE}</DropdownMenuItem>
        <DropdownMenuItem onClick={() => run("raw")}>{DROP.RAW}</DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

export function AuditRetention() {
  const [status, setStatus] = React.useState<AuditRetentionStatus | null>(null);
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [dropping, setDropping] = React.useState<AuditRetentionPartition | null>(null);

  const load = React.useCallback(() => {
    setState("loading");
    api
      .getRetention()
      .then((s) => {
        setStatus(s);
        setState("ready");
      })
      .catch(() => setState("error"));
  }, []);
  React.useEffect(load, [load]);

  if (state === "loading") {
    return (
      <div className="overflow-hidden rounded-xl border border-border bg-card">
        <TableSkeleton rows={4} cols={5} />
      </div>
    );
  }
  if (state === "error" || !status) {
    return (
      <div className="overflow-hidden rounded-xl border border-border bg-card">
        <ErrorState onRetry={load} />
      </div>
    );
  }

  const { policy, cutover, partitions, months_ahead } = status;
  const pending = policy.pending_days != null && policy.pending_effective_at;

  return (
    <SectionCard>
      <SectionLabel>{RETENTION.KEPT_FOR}</SectionLabel>
      <div className="mt-1 flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span className="text-lg font-semibold text-foreground">
          {policy.effective_days === 0 ? RETENTION.FOREVER : RETENTION.DAYS(policy.effective_days)}
        </span>
        {pending && (
          <span className="text-sm text-muted-foreground">
            {RETENTION.PENDING(policy.pending_days!, dayLabel(policy.pending_effective_at!))}
          </span>
        )}
      </div>
      <p className="mt-2 text-xs text-muted-foreground">{RETENTION.SOURCE}</p>
      <p className="mt-1 text-xs text-muted-foreground">{RETENTION.CUTOVER(dayLabel(cutover))}</p>
      <div className="mt-3">
        {months_ahead >= 3 ? (
          <Chip tone="success" dot>
            {RETENTION.AHEAD(months_ahead)}
          </Chip>
        ) : (
          <p role="status" className="text-xs text-warning">
            {RETENTION.AHEAD_LOW(months_ahead)}
          </p>
        )}
      </div>

      <div className="mt-4 overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>{RETENTION.COL_PARTITION}</TableHead>
              <TableHead>{RETENTION.COL_COVERS}</TableHead>
              <TableHead className="text-right">{RETENTION.COL_EVENTS}</TableHead>
              <TableHead>{RETENTION.COL_STATE}</TableHead>
              <TableHead>{RETENTION.COL_DROP}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {partitions.map((p) => (
              <TableRow key={p.name}>
                <TableCell>
                  <Mono className="text-xs">{p.name}</Mono>
                </TableCell>
                <TableCell className="whitespace-nowrap text-muted-foreground">{coversLabel(p, cutover)}</TableCell>
                <TableCell className="text-right tabular-nums">{p.rows.toLocaleString("en-US")}</TableCell>
                <TableCell className="text-muted-foreground">
                  {p.state === "closed" ? RETENTION.CLOSED : RETENTION.OPEN}
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap items-center gap-2">
                    {p.eligible ? (
                      <span className="text-sm text-foreground">{RETENTION.ELIGIBLE}</span>
                    ) : (
                      <span className="text-sm text-muted-foreground">
                        {RETENTION.WHY[reasonKey(p.refusal ?? "")] ?? p.refusal ?? ""}
                      </span>
                    )}
                    <span className="ml-auto flex items-center gap-2">
                      {p.state === "closed" && <ExportMenu partition={p.name} />}
                      {p.eligible && (
                        <Button variant="outline" size="sm" onClick={() => setDropping(p)}>
                          {RETENTION.DROP}
                        </Button>
                      )}
                    </span>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <p className="mt-3 text-xs text-muted-foreground">{RETENTION.FOOTER}</p>

      <AuditRetentionDropDialog
        partition={dropping}
        cutover={cutover}
        onOpenChange={(open) => !open && setDropping(null)}
        onDropped={load}
      />
    </SectionCard>
  );
}
