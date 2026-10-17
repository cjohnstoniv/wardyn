/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Identity widget's Sync rows (0.9, packet M-RN-3 RS1/RS1b): what `wardyn sync` moved, as bytes,
// directories, times and open sessions, and never a claim about files ("in sync", counts, names): the
// gateway cannot see them. Ended sessions are the run's `ssh.sync.transfer` audit rows (bytes_in is into the
// sandbox); sessions open now are `RunDetail.sync.open`, read on the replica that answers. Plain sftp
// transfers are their own row. A local run has no Sync rows: its disk link is the bind through its runner.
import * as React from "react";
import { audit as auditApi } from "../../../../lib/api/audit";
import { health as healthApi } from "../../../../lib/api/health";
import { absoluteTime, fmtBytes, shortTime } from "../../../../lib/format";
import type { AuditEvent, RunDetail } from "../../../../lib/types";
import { RUN_SYNC } from "../../../wardyn/copy/runners";

const MAX_DIRS = 3;
/** While a run is live its sync rows are read again this often: a session can open and close between two of the page's polls. */
export const SYNC_REREAD_MS = 15_000;

interface DirSync {
  dir: string;
  /** Bytes into / out of the sandbox, summed over the directory's ended sessions. */
  to: number;
  from: number;
  /** Time of the most recent ended session; absent while only an open one exists. */
  last?: string;
  /** The latest ended session's own error text, verbatim. */
  error?: string;
  open: string[];
}

const num = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) ? v : 0);
const text = (v: unknown): string | undefined => (typeof v === "string" && v !== "" ? v : undefined);
const byTime = (a: AuditEvent, b: AuditEvent) => Date.parse(a.time) - Date.parse(b.time);

/** Directories, most recently active first, from the ended-session rows and the open sessions. */
export function syncDirs(audit: AuditEvent[], open: { dir: string; opened_at: string }[] = []): DirSync[] {
  const dirs = new Map<string, DirSync>();
  const entry = (dir: string) => {
    let d = dirs.get(dir);
    if (!d) dirs.set(dir, (d = { dir, to: 0, from: 0, open: [] }));
    return d;
  };
  // Oldest first, so the last row of a directory is its latest session.
  for (const e of audit.filter((x) => x.action === "ssh.sync.transfer").sort(byTime)) {
    const dir = text(e.data?.dir);
    // A session refused before it had a directory belongs to none.
    if (!dir) continue;
    const d = entry(dir);
    d.to += num(e.data?.bytes_in);
    d.from += num(e.data?.bytes_out);
    d.last = e.time;
    d.error = text(e.data?.error);
  }
  for (const o of open) entry(o.dir).open.push(o.opened_at);
  const activity = (d: DirSync) => Math.max(d.last ? Date.parse(d.last) : 0, ...d.open.map((t) => Date.parse(t)));
  return [...dirs.values()].sort((a, b) => activity(b) - activity(a));
}

/** The run's plain sftp bytes. Rows from before 0.9 carry only `bytes`, which was sandbox to client. */
export function sftpTotals(audit: AuditEvent[]): { to: number; from: number } | null {
  const rows = audit.filter((e) => e.action === "ssh.sftp.transfer");
  if (rows.length === 0) return null;
  return rows.reduce((t, e) => ({ to: t.to + num(e.data?.bytes_in), from: t.from + num(e.data?.bytes_out ?? e.data?.bytes) }), { to: 0, from: 0 });
}

// Time of day when it is today, the full time otherwise.
function when(iso: string): string {
  return new Date(iso).toDateString() === new Date().toDateString() ? shortTime(iso) : absoluteTime(iso);
}

/** Whether /healthz reports SSH on; false until it answers, so the empty row never speaks off a guess. */
function useSshOn(enabled: boolean): boolean {
  const [on, setOn] = React.useState(false);
  React.useEffect(() => {
    if (!enabled) return;
    let alive = true;
    void healthApi.health().then((h) => alive && setOn(h.status === "ok" && !!h.ssh?.enabled));
    return () => {
      alive = false;
    };
  }, [enabled]);
  return on;
}

/**
 * The run's sync rows read by action, as the page reads its endings: the page's trail stops at its row cap,
 * so a long run's syncs fall past it. Read again when a session ends (it leaves `sync.open` as its row is
 * written) and every SYNC_REREAD_MS while the run is live. Null until a read has succeeded, so the empty row never speaks off a partial trail; a later failed read keeps the last good one.
 */
function useSyncAudit(run: RunDetail, enabled: boolean): AuditEvent[] | null {
  const [read, setRead] = React.useState<{ id: string; rows: AuditEvent[] } | null>(null);
  const openKey = (run.sync?.open ?? []).map((o) => `${o.dir}@${o.opened_at}`).join("|");
  // Bumped on a timer while the run is live, so a session that opened and ended between two polls is still read.
  const [tick, setTick] = React.useState(0);
  const live = run.state === "RUNNING";
  React.useEffect(() => {
    if (!enabled || !live) return;
    const id = setInterval(() => setTick((n) => n + 1), SYNC_REREAD_MS);
    return () => clearInterval(id);
  }, [enabled, live]);
  React.useEffect(() => {
    if (!enabled) return;
    let alive = true;
    Promise.all([
      auditApi.listAudit(run.id, { action: "ssh.sync.transfer" }),
      auditApi.listAudit(run.id, { action: "ssh.sftp.transfer" }),
    ])
      .then(([sync, sftp]) => alive && setRead({ id: run.id, rows: [...sync, ...sftp] }))
      .catch(() => {}); // a re-read that fails keeps what the last good read found
    return () => {
      alive = false;
    };
  }, [enabled, run.id, run.state, openKey, tick]);
  return read?.id === run.id ? read.rows : null;
}

export function SyncRows({ run, audit, onGoAudit }: { run: RunDetail; audit: AuditEvent[]; onGoAudit: () => void }) {
  const local = run.placement === "local";
  const live = run.state === "RUNNING";
  const sshOn = useSshOn(!local && live);
  const scoped = useSyncAudit(run, !local);
  if (local) return null;

  const seen = new Set(audit.map((e) => e.id));
  const rows = [...audit, ...(scoped ?? []).filter((e) => !seen.has(e.id))];
  const dirs = syncDirs(rows, run.sync?.open);
  const sftp = sftpTotals(rows);
  const shown = dirs.slice(0, MAX_DIRS);
  const hidden = dirs.length - shown.length;
  const nothing = dirs.length === 0 && live && sshOn && scoped !== null;
  if (dirs.length === 0 && !sftp && !nothing) return null;

  return (
    <>
      <div className="col-span-2 border-t border-border" aria-hidden />
      {nothing && (
        <>
          <dt className="text-muted-foreground">{RUN_SYNC.LABEL}</dt>
          <dd className="min-w-0 text-right text-foreground">{RUN_SYNC.NONE}</dd>
        </>
      )}
      {shown.map((d, i) => (
        <React.Fragment key={d.dir}>
          <dt className="text-muted-foreground">{i === 0 ? RUN_SYNC.LABEL : ""}</dt>
          <dd className="min-w-0 space-y-0.5 text-right">
            <div className="truncate font-mono text-foreground" title={d.dir}>
              {d.dir}
            </div>
            {d.last && <div className="text-foreground">{RUN_SYNC.BYTES(fmtBytes(d.to), fmtBytes(d.from))}</div>}
            {d.last && <div className="text-muted-foreground">{RUN_SYNC.LAST(when(d.last))}</div>}
            {d.open.map((t) => (
              <div key={t} className="text-muted-foreground">
                {RUN_SYNC.OPEN_SINCE(when(t))}
              </div>
            ))}
            {d.open.length > 0 && <div className="text-muted-foreground">{RUN_SYNC.OPEN_NOTE}</div>}
            {d.error && <div className="text-muted-foreground">{RUN_SYNC.ERROR(d.error)}</div>}
          </dd>
        </React.Fragment>
      ))}
      {hidden > 0 && (
        <>
          <dt />
          <dd className="text-right">
            <button type="button" onClick={onGoAudit} className="text-info hover:underline">
              {RUN_SYNC.MORE_DIRS(hidden)}
            </button>
          </dd>
        </>
      )}
      {sftp && (
        <>
          <dt className="font-mono text-muted-foreground">{RUN_SYNC.SFTP_LABEL}</dt>
          <dd className="min-w-0 text-right text-foreground">{RUN_SYNC.BYTES(fmtBytes(sftp.to), fmtBytes(sftp.from))}</dd>
        </>
      )}
    </>
  );
}
