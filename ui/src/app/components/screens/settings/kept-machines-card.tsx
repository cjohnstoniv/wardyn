/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account: Your kept machines (board KM-4), read from GET /me/machines and deleted with DELETE /me/machines/{id}.
// A person keeps a macOS VM on their own Mac between runs; this lists them with their Mac, base, size and dates. The card
// is not drawn while nothing is kept and the person may not keep one (the site switch off, a governance profile that
// denies it, no Mac of their own), nor for a caller who is no person. Machines already kept when the switch goes off stay
// listed and deletable.
import * as React from "react";
import { useLocation } from "react-router-dom";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { deleteMyMachine, myMachines } from "../../../lib/api/me-machines";
import { mediumDate } from "../../../lib/format";
import type { KeptMachine, MyMachines } from "../../../lib/types";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "../../ui/alert-dialog";
import { Button, buttonVariants } from "../../ui/button";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { gbOf, KEEP_LIST as T } from "../../wardyn/copy/keep-machine";
import { Chip } from "../../wardyn/primitives";
import { ErrorState, failureSentence } from "../../wardyn/states";
import { useHolderLabels, shortRunId } from "./use-holder-labels";
import { macOf, macsOf, rowOf } from "./kept-machines-model";

export function KeptMachinesCard() {
  const [data, setData] = React.useState<MyMachines | null>(null);
  const [state, setState] = React.useState<"loading" | "absent" | "failed" | "ready">("loading");
  const [loadMessage, setLoadMessage] = React.useState<string | undefined>();
  const [toDelete, setToDelete] = React.useState<KeptMachine | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [refusal, setRefusal] = React.useState<string | null>(null);
  const labels = useHolderLabels(data?.machines ?? []);
  const location = useLocation();
  const ref = React.useRef<HTMLElement | null>(null);
  const [open, setOpen] = React.useState(false);

  const load = React.useCallback(() => {
    setState((s) => (s === "ready" ? s : "loading"));
    myMachines()
      .then((d) => {
        setData(d);
        setState("ready");
      })
      .catch((e) => {
        setLoadMessage(failureSentence(e));
        setState(e instanceof HttpError && (e.status === 403 || e.status === 404) ? "absent" : "failed");
      });
  }, []);
  React.useEffect(load, [load]);

  const shown = !(state === "absent" || state === "loading" || (state === "ready" && data && data.machines.length === 0 && !data.allowed));
  // The run page's Machine tile links here: open and focus the card once the list is drawn, once per navigation and never
  // on a data refresh, so a person's later collapse stays.
  React.useEffect(() => {
    if (shown && location.hash === "#kept-machines") {
      setOpen(true);
      ref.current?.focus({ focusVisible: true } as FocusOptions);
    }
  }, [location.key, shown]); // eslint-disable-line react-hooks/exhaustive-deps -- location.key, not the hash

  const confirmDelete = async () => {
    if (!toDelete || busy) return;
    const m = toDelete;
    setBusy(true);
    setRefusal(null);
    try {
      const waiting = await deleteMyMachine(m.id);
      toast.success(waiting ? T.TOAST_DEFERRED(m.name, macOf(m)) : T.TOAST_DELETED(m.name, macOf(m)));
    } catch (e) {
      // The server's own sentence: a machine a run holds (409), a delete it could not confirm or the Mac refused (503).
      setRefusal(e instanceof HttpError ? e.message : T.LOAD_FAILED);
    } finally {
      setBusy(false);
      setToDelete(null);
      load();
    }
  };

  if (!shown) return null;
  const machines = data?.machines ?? [];
  const now = Date.now();
  return (
    <CollapsibleCard title={T.TITLE} summary={state === "ready" ? (machines.length ? T.SUMMARY(machines.length, macsOf(machines).join(", ")) : T.NONE) : undefined} testId="kept-machines-card" id="kept-machines" ref={ref} tabIndex={-1} open={open} onOpenChange={setOpen} className="outline-none focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px]">
      {state === "failed" ? (
        <ErrorState size="compact" title={T.LOAD_FAILED} message={loadMessage} onRetry={load} />
      ) : (
        <div className="space-y-3">
          <p className="text-xs text-muted-foreground">{T.LEDE(data?.idle_days)}</p>
          {machines.length === 0 ? (
            <p className="text-sm text-muted-foreground">{T.EMPTY}</p>
          ) : (
            <ul className="overflow-hidden rounded-lg border border-border">
              {machines.map((m) => {
                const row = rowOf(m, labels[m.run_id ?? ""] ?? (m.run_id ? shortRunId(m.run_id) : ""), now);
                return (
                  <li key={m.id} className="flex items-start justify-between gap-3 border-b border-border px-3 py-2.5 last:border-b-0">
                    <span className="min-w-0">
                      <span className="flex flex-wrap items-center gap-2">
                        <span className="break-words text-sm font-semibold text-foreground">{m.name}</span>
                        <Chip tone={row.tone}>{row.chip}</Chip>
                      </span>
                      <span className="block text-xs text-muted-foreground">{T.META(macOf(m), m.base, gbOf(m.disk_bytes))}</span>
                      <span className="block text-xs text-muted-foreground">{T.DATES(mediumDate(m.last_used_at), mediumDate(m.expires_at))}</span>
                      {row.line && <span className="block text-xs text-muted-foreground">{row.line}</span>}
                    </span>
                    <Button type="button" variant="ghost" size="sm" disabled={!row.canDelete || busy} aria-label={`${T.DELETE} ${m.name}`} onClick={() => setToDelete(m)}>
                      {T.DELETE}
                    </Button>
                  </li>
                );
              })}
            </ul>
          )}
          {refusal && (
            <p role="alert" className="text-xs text-danger">
              {refusal}
            </p>
          )}
        </div>
      )}
      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && !busy && setToDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{toDelete ? T.CONFIRM_TITLE(toDelete.name) : ""}</AlertDialogTitle>
            <AlertDialogDescription>{toDelete ? T.CONFIRM_BODY(toDelete.name, macOf(toDelete)) : ""}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>{T.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={busy}
              onClick={(e) => {
                e.preventDefault();
                void confirmDelete();
              }}
            >
              {T.DELETE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </CollapsibleCard>
  );
}
