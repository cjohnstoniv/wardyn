/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Identity widget's "Folders from your computer" rows (M4, #2982): the run's folder held by the
// person's own runner, as GET /runs/{id}'s computer_folder reports it. The first sync (waiting, synced,
// stopped), where it is in the sandbox, which way changes go, the files each way, and, once a Both ways
// run has ended, the changes that came back. One side edits at a time, so there is no live sync to show.
import { fmtBytes, shortTime } from "../../../../lib/format";
import { isTerminalRunState, type RunDetail } from "../../../../lib/types";
import { RUN_COMPUTER_FOLDER as C } from "../../../wardyn/copy/runners";

const count = (n: number) => n.toLocaleString("en");

export function ComputerFolderRows({ run }: { run: RunDetail }) {
  const f = run.computer_folder;
  if (!f) return null;
  const ended = isTerminalRunState(run.state);
  const status = f.state === "failed" ? C.STOPPED : f.state === "synced" ? (ended ? C.ENDED : C.SYNCED) : C.WAITING;
  const refused = f.seed?.refused.length ?? 0;
  return (
    <>
      <div className="col-span-2 border-t border-border" aria-hidden />
      <dt className="col-span-2 font-semibold text-foreground" data-testid="run-computer-folder">{C.TITLE}</dt>
      <dt className="text-muted-foreground">{f.name}</dt>
      <dd className="min-w-0 text-right text-foreground">{status}</dd>
      <dt className="text-muted-foreground">{C.IN_SANDBOX}</dt>
      <dd className="min-w-0 truncate text-right font-mono text-foreground" title={f.remote_dir}>{f.remote_dir}</dd>
      <dt className="text-muted-foreground">{C.CHANGES}</dt>
      <dd className="min-w-0 text-right text-foreground">{f.direction === "both" ? C.BOTH : C.COPY_IN}</dd>
      {f.seed && !f.seed.error && (
        <>
          <dt className="text-muted-foreground">{C.TO_SANDBOX}</dt>
          <dd className="min-w-0 text-right text-foreground">{C.AT_START(count(f.seed.files), fmtBytes(f.seed.bytes))}</dd>
        </>
      )}
      {f.back && !f.back.error && (
        <>
          <dt className="text-muted-foreground">{C.BACK}</dt>
          <dd className="min-w-0 text-right text-foreground">{C.FILES(count(f.back.files))}</dd>
        </>
      )}
      {f.state === "waiting" && !ended && <p className="col-span-2 text-meta text-muted-foreground">{C.WAITING_BODY}</p>}
      {f.state === "failed" && (
        <p role="alert" className="col-span-2 text-meta text-danger">
          {f.error} {C.NOTHING_RAN}
        </p>
      )}
      {refused > 0 && <p className="col-span-2 text-meta text-muted-foreground">{C.REFUSED(count(refused))}</p>}
      {ended && f.back && !f.back.error && <p className="col-span-2 text-meta text-muted-foreground">{C.BACK_AT(shortTime(f.back.at), count(f.back.files))}</p>}
      {f.back?.error && <p role="alert" className="col-span-2 text-meta text-danger">{C.BACK_FAILED(f.back.error)}</p>}
      {ended && f.state === "synced" && <p className="col-span-2 text-meta text-muted-foreground">{C.DELETED}</p>}
    </>
  );
}
