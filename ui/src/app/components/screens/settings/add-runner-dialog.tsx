/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// My pools -> Add runner (mock M-POOLS v6 B13): the runners a person may put in a pool of theirs: their
// own claimed computers, and the remote-hosted runners an admin made available to people's pools. Another
// person's computer is never offered, because the server never lists it to them.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { runnerPools } from "../../../lib/api/runner-pools";
import { getErrorMessage } from "../../../lib/format";
import type { RemoteRunnerView, RunnerKind } from "../../../lib/types/runner-pools";
import type { RunnerView } from "../../../lib/types/runners";
import { MY_POOLS, PICKER, RES, RUNNER_V6 } from "../../wardyn/copy/runner-pools";
import { RUNNER } from "../../wardyn/copy/runners";
import { Chip } from "../../wardyn/primitives";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { KindChip } from "../runners/resource-table";
import type { MyPool } from "./use-my-pools";

interface Candidate {
  kind: RunnerKind;
  id: string;
  name: string;
  online: boolean;
  line: string;
}

export function candidatesFor(pool: MyPool, runners: RunnerView[], remote: RemoteRunnerView[]): Candidate[] {
  const inPool = new Set(pool.members.map((m) => `${m.kind}:${m.id}`));
  const runs = (n?: number) => (n ? ` · ${RES.RUNS_AT_ONCE(n)}` : "");
  return [
    ...runners
      .filter((r) => r.state === "claimed")
      .map((r): Candidate => ({ kind: "self_hosted", id: r.id, name: r.name, online: r.online, line: `${RUNNER_V6.MACHINE}: ${r.machine_name || RUNNER_V6.NOT_REPORTED}${runs(r.limits?.runs_at_once)}` })),
    ...remote.map((r): Candidate => ({ kind: "remote_hosted", id: r.id, name: r.name, online: r.online, line: `${PICKER.REMOTE_LINE}${runs(r.limits?.runs_at_once)}` })),
  ].filter((c) => !inPool.has(`${c.kind}:${c.id}`));
}

export function AddRunnerDialog({
  pool,
  runners,
  remote,
  onOpenChange,
  onAdded,
}: {
  /** The pool a runner is added to; null when the dialog is closed. */
  pool: MyPool | null;
  runners: RunnerView[];
  remote: RemoteRunnerView[];
  onOpenChange: (open: boolean) => void;
  onAdded: () => void;
}) {
  const last = React.useRef(pool);
  if (pool) last.current = pool;
  const shown = pool ?? last.current;
  const [picked, setPicked] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const id = pool?.pool.id;
  React.useEffect(() => {
    setPicked("");
    setError("");
  }, [id]);
  if (!shown) return null;
  const candidates = candidatesFor(shown, runners, remote);
  const hasAny = runners.some((r) => r.state === "claimed") || remote.length > 0;
  const choice = candidates.find((c) => `${c.kind}:${c.id}` === picked) ?? candidates[0];
  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!choice) return;
    setBusy(true);
    setError("");
    try {
      if (choice.kind === "self_hosted") await runnerPools.addMyRunner(shown.pool.id, choice.id);
      else await runnerPools.addMyRemoteRunner(shown.pool.id, choice.id);
      toast.success(MY_POOLS.ADDED(choice.name, shown.pool.name));
      onOpenChange(false);
      onAdded();
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open={pool !== null} onOpenChange={(o) => !busy && onOpenChange(o)}>
      <DialogContent className="sm:max-w-md">
        <form onSubmit={add} className="space-y-3">
          <DialogHeader>
            <DialogTitle>{PICKER.TITLE(shown.pool.name)}</DialogTitle>
            <DialogDescription>{PICKER.LEDE}</DialogDescription>
          </DialogHeader>
          <p className="text-xs text-muted-foreground">{PICKER.ORDER}</p>
          {candidates.length > 0 ? (
            <div role="radiogroup" aria-label={PICKER.LIST_LABEL} className="grid gap-2">
              {candidates.map((c) => {
                const key = `${c.kind}:${c.id}`;
                return (
                  <label key={key} className="flex min-w-0 cursor-pointer items-start gap-2 rounded-lg border border-border px-3 py-2.5 text-body">
                    <input type="radio" name="pick" className="mt-1" value={key} checked={choice === c} onChange={() => setPicked(key)} />
                    <span className="min-w-0">
                      <span className="flex flex-wrap items-center gap-1.5">
                        <strong className="font-semibold">{c.name}</strong>
                        <KindChip kind={c.kind} />
                        <Chip dot>{c.online ? RUNNER.ONLINE : RUNNER.OFFLINE}</Chip>
                      </span>
                      <span className="mt-0.5 block break-words text-xs text-muted-foreground">{c.line}</span>
                    </span>
                  </label>
                );
              })}
            </div>
          ) : (
            <p className="text-body text-foreground">{hasAny ? PICKER.NOTHING : PICKER.NONE}</p>
          )}
          {error && (
            <p role="alert" className="text-body text-danger">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" disabled={busy} onClick={() => onOpenChange(false)}>
              {PICKER.CANCEL}
            </Button>
            <Button type="submit" disabled={busy || !choice}>
              {busy && <Loader2 className="size-3.5 animate-spin" />}
              {PICKER.ADD}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
