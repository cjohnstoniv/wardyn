/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Workloads screen's detail drawer (mock r16 T3-a): the spec, its generation, the current run and the last
// refusal, with Restart and Delete. A custom resource's workload has one writer, Git: Delete is hidden and the
// drawer says where to change it. The last refusal is a plain sentence with its code in small print (owner
// answer q12). The mock's Inbound address row has no field on the wire yet, so it is not drawn.
import * as React from "react";
import { GitBranch, RotateCcw, Trash2 } from "lucide-react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { runs } from "../../../lib/api/runs";
import { workloads } from "../../../lib/api/workloads";
import { getErrorMessage } from "../../../lib/format";
import type { Workload, WorkloadEvent } from "../../../lib/types/workloads";
import { refusalSentence } from "../../../lib/workload-refusals";
import { Button } from "../../ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "../../ui/sheet";
import { Mono } from "../../wardyn/code-block";
import { agoShort, digestShort, WORKLOADS as W } from "../../wardyn/copy/workloads";
import { runPath, type ConsoleView } from "../../wardyn/console-view";
import { YamlBlock } from "../../wardyn/yaml-block";
import { ConfirmDialog } from "../runners/confirm-dialog";
import { refusedNow, WorkloadBadge } from "./workload-state";

/** The document a workload was applied as, which the Spec shows read-only. */
function specOf(w: Workload) {
  return {
    api_version: "wardyn/v1",
    kind: "Workload",
    name: w.name,
    ...(w.description ? { description: w.description } : {}),
    ...(w.run_in ? { run_in: w.run_in } : {}),
    ...(w.restart_key ? { restart_key: w.restart_key } : {}),
    intent: w.intent,
  };
}

const Row = ({ label, children }: { label: string; children: React.ReactNode }) => (
  <div className="grid grid-cols-[8rem_1fr] gap-x-3 py-1.5 text-sm">
    <span className="text-muted-foreground">{label}</span>
    <div className="min-w-0 [overflow-wrap:anywhere]">{children}</div>
  </div>
);

/** When the current run started, read from the run itself; the id alone shows until (or unless) the run is read. */
function useRunStart(runId: string | undefined): string | undefined {
  const [started, setStarted] = React.useState<{ id: string; at: string } | null>(null);
  React.useEffect(() => {
    if (!runId) return;
    let live = true;
    runs
      .getRun(runId)
      .then((r) => live && r && setStarted({ id: runId, at: r.created_at }))
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [runId]);
  return started && started.id === runId ? started.at : undefined;
}

/** The workload's history, read again when its spec or its run changes. A failed read is no history. */
function useHistory(workload: Workload | null): WorkloadEvent[] {
  const name = workload?.name;
  const owner = workload?.owner;
  const key = workload ? `${workload.owner}/${workload.name}/${workload.generation}/${workload.run_id ?? ""}` : "";
  const [got, setGot] = React.useState<{ key: string; events: WorkloadEvent[] } | null>(null);
  React.useEffect(() => {
    if (!name) return;
    let live = true;
    workloads
      .history(name, owner)
      .then((events) => live && setGot({ key, events }))
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [key, name, owner]);
  return got && got.key === key ? got.events : [];
}

/** The rows the board draws: a launch (with the run its replace ended) and a refusal. The apply row beside a launch is not drawn. */
function historyRows(events: WorkloadEvent[]): { event: WorkloadEvent; ended?: string }[] {
  const replaced = new Map<string, string>(); // a run that replaced another, and the one it ended
  for (const e of events) if (e.action === "replace" && e.run_id && e.ended_run_id) replaced.set(e.run_id, e.ended_run_id);
  return events.flatMap((event) =>
    event.action === "launch" ? [{ event, ended: event.run_id ? replaced.get(event.run_id) : undefined }] : event.action === "launch_refused" ? [{ event }] : [],
  );
}

export function WorkloadDrawer({
  workload,
  view,
  now,
  onClose,
  onChanged,
}: {
  workload: Workload | null;
  view: ConsoleView;
  now: number;
  onClose: () => void;
  onChanged: () => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const startedAt = useRunStart(workload?.run_id);
  const history = historyRows(useHistory(workload));
  if (!workload) return null;
  const w = workload;
  const k8s = w.source === "k8s";
  const refusal = w.last_refusal;
  // The owner's own request or, for a service identity's workload, the sponsor's: the server decides who may.
  const owner = w.owner;

  const restart = async () => {
    setBusy(true);
    try {
      const res = await workloads.restart(w.name, owner);
      if (res.launch.state === "refused") toast.error(refusalSentence(res.launch.reason ?? "", res.launch.message));
      else toast.success(W.RESTARTED(w.name));
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Sheet open onOpenChange={(o) => !o && onClose()}>
        <SheetContent className="scroll-thin w-full gap-0 overflow-y-auto sm:max-w-[560px]">
          <SheetHeader className="border-b border-border pb-4">
            <SheetTitle className="flex items-center gap-2.5 pr-8">
              <span className="font-mono text-base">{w.name}</span>
              <WorkloadBadge workload={w} />
            </SheetTitle>
            <SheetDescription className="sr-only">{W.TITLE}</SheetDescription>
          </SheetHeader>
          <div className="space-y-4 px-4 pb-6">
            {k8s && (
              <div className="flex items-start gap-2 rounded-lg border border-border bg-surface-2 p-3 text-sm">
                <GitBranch className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
                <span>
                  {W.MANAGED_LEAD} <Mono className="text-foreground">{w.namespace}</Mono>
                  {W.MANAGED_TAIL}
                </span>
              </div>
            )}
            <div>
              <Row label={W.GENERATION}>
                <Mono className="text-foreground">{w.generation}</Mono>
              </Row>
              {w.digest && (
                <Row label={W.DIGEST}>
                  <Mono className="text-xs text-foreground" title={w.digest}>
                    {digestShort(w.digest)}
                  </Mono>
                </Row>
              )}
              {w.run_id && (
                <Row label={W.CURRENT_RUN}>
                  <Link to={runPath(view, w.run_id)} className="font-mono text-xs text-info hover:underline">
                    {w.run_id.slice(0, 8)}
                  </Link>
                  {startedAt && <> · {W.STARTED(agoShort(startedAt, now))}</>}
                </Row>
              )}
              {refusal && (
                <Row label={W.LAST_REFUSAL}>
                  <div data-testid="workload-last-refusal" className="space-y-0.5 text-xs">
                    <p>
                      {W.REFUSAL_GENERATION(refusal.generation)} · {refusalSentence(refusal.reason, refusal.message)}
                      {/* Only a refusal of the spec as it stands leaves a run to keep running: an older one's run is not known. */}
                      {refusedNow(w) && w.run_id ? ` · ${W.PREVIOUS_KEPT}` : ""}
                    </p>
                    <p className="font-mono text-[11px] text-muted-foreground">{refusal.reason}</p>
                  </div>
                </Row>
              )}
            </div>
            <div>
              <p className="label-eyebrow mb-1.5">{W.SPEC}</p>
              <YamlBlock value={specOf(w)} />
            </div>
            {history.length > 0 && (
              <div>
                <p className="label-eyebrow mb-1.5">{W.HISTORY}</p>
                <ul data-testid="workload-history" className="space-y-1 text-xs">
                  {history.map(({ event, ended }) => (
                    <li key={`${event.time}/${event.action}`}>
                      {event.generation ? <Mono className="text-foreground">{W.HISTORY_GEN(event.generation)}</Mono> : null}{" "}
                      {event.action === "launch" ? W.HISTORY_LAUNCHED : W.HISTORY_REFUSED}
                      {event.action === "launch" && event.run_id && (
                        <>
                          {" "}
                          <Mono className="text-foreground">{event.run_id.slice(0, 8)}</Mono>
                        </>
                      )}
                      {ended && (
                        <>
                          , {W.HISTORY_ENDED} <Mono className="text-foreground">{ended.slice(0, 8)}</Mono>
                        </>
                      )}{" "}
                      · {agoShort(event.time, now)}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            <div className="flex flex-wrap items-center gap-2">
              <Button variant="outline" size="sm" disabled={busy} onClick={() => void restart()}>
                <RotateCcw className="size-4" /> {W.RESTART}
              </Button>
              {k8s ? (
                <span className="text-xs text-muted-foreground">{W.DELETE_HIDDEN}</span>
              ) : (
                <Button variant="destructive" size="sm" disabled={busy} onClick={() => setDeleting(true)}>
                  <Trash2 className="size-4" /> {W.DELETE}
                </Button>
              )}
            </div>
          </div>
        </SheetContent>
      </Sheet>
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        title={W.DELETE_TITLE(w.name)}
        body={[W.DELETE_BODY]}
        confirmLabel={W.DELETE}
        onConfirm={() => workloads.remove(w.name, owner)}
        onDone={() => {
          toast.success(W.DELETED(w.name));
          onClose();
          onChanged();
        }}
        onRaced={() => {
          onClose();
          onChanged();
        }}
      />
    </>
  );
}
