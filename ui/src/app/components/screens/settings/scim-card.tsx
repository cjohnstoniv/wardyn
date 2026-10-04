/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.8.6 scim-a7 (mock packet M5, approved 2026-10-03): the Settings card that shows where leaver
// deprovisioning stands. It reads one securityOps GET (/scim/status) and writes nothing. Set up: the
// endpoint, which token slot matched last, the purge delay and what happens to workspaces, then three
// lists (deactivated people, deprovisioning steps still failing, drives a purge listed). Not set up
// (D5): the card stays, saying what the gap is and where the fix is, so the leaver gap is never hidden.
//
// Mounted from AdminSettingsScreen, which only a super admin reaches, so the card carries no operator
// check of its own (the same rule as AdminSshKeysCard). Copy is lib/scim-copy.ts, byte for byte M5's.
import * as React from "react";
import { scim as scimApi } from "../../../lib/api/scim";
import { appURL } from "../../../lib/base-path";
import { absoluteTime, relativeTime } from "../../../lib/format";
import { SCIM, SCIM_RUNBOOK_URL } from "../../../lib/scim-copy";
import type { SCIMStatus } from "../../../lib/types";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { Mono } from "../../wardyn/code-block";
import { SectionLabel } from "../../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../../wardyn/states";

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4 py-1.5">
      <span className="text-body text-muted-foreground">{label}</span>
      <span className="text-right text-body text-foreground">{children}</span>
    </div>
  );
}

function When({ iso }: { iso: string }) {
  return (
    <span className="text-xs text-muted-foreground" title={absoluteTime(iso)}>
      {relativeTime(iso)}
    </span>
  );
}

function List({ title, empty, children }: { title: string; empty: string | null; children: React.ReactNode }) {
  return (
    <div className="mt-4">
      <SectionLabel className="mb-1.5">{title}</SectionLabel>
      {empty !== null ? (
        <p className="text-body text-muted-foreground">{empty}</p>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border">{children}</div>
      )}
    </div>
  );
}

const purgeDelay = (seconds: number) =>
  seconds > 0 ? SCIM.PURGE_DAYS(Math.max(1, Math.round(seconds / 86400))) : <span className="text-muted-foreground">{SCIM.PURGE_OFF}</span>;

function Facts({ s }: { s: SCIMStatus }) {
  const token = s.last_token_slot === "primary" ? SCIM.TOKEN_PRIMARY : s.last_token_slot === "next" ? SCIM.TOKEN_NEXT : SCIM.TOKEN_NONE;
  return (
    <div className="divide-y divide-border">
      <Endpoint />
      <div>
        <Fact label={SCIM.TOKEN_LABEL}>{token}</Fact>
        {s.last_token_slot === "next" && <p className="pb-1.5 text-meta text-warning">{SCIM.TOKEN_NEXT_HINT}</p>}
      </div>
      <Fact label={SCIM.PURGE_DELAY}>{purgeDelay(s.purge_after_seconds)}</Fact>
      <Fact label={SCIM.WORKSPACES}>{s.keep_workspaces ? SCIM.WS_KEEP : SCIM.WS_REASSIGN}</Fact>
    </div>
  );
}

// The tenant URL an identity provider is pointed at: this console's own origin and base path.
function Endpoint() {
  return (
    <Fact label={SCIM.ENDPOINT}>
      <Mono className="text-foreground">{`${window.location.origin}${appURL("/scim/v2")}`}</Mono>
    </Fact>
  );
}

function Lists({ s }: { s: SCIMStatus }) {
  return (
    <>
      <List title={SCIM.DEACTIVATED_TITLE} empty={s.deactivated.length === 0 ? SCIM.DEACTIVATED_EMPTY : null}>
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>{SCIM.COL_PERSON}</TableHead>
              <TableHead>{SCIM.COL_SINCE}</TableHead>
              <TableHead>{SCIM.COL_PURGE}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.deactivated.map((d) => (
              <TableRow key={d.person}>
                <TableCell>{d.person}</TableCell>
                <TableCell>
                  <When iso={d.deactivated_at} />
                </TableCell>
                <TableCell>
                  {d.purge_after ? <When iso={d.purge_after} /> : <span className="text-xs text-muted-foreground">{SCIM.PURGE_NOT_SCHEDULED}</span>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </List>

      <List title={SCIM.PENDING_TITLE} empty={s.pending.length === 0 ? SCIM.PENDING_EMPTY : null}>
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>{SCIM.COL_PERSON}</TableHead>
              <TableHead>{SCIM.COL_STEP}</TableHead>
              <TableHead>{SCIM.COL_ERROR}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.pending.map((p) => (
              <TableRow key={`${p.person}/${p.step}`}>
                <TableCell>{p.person}</TableCell>
                <TableCell>{SCIM.STEP[p.step] ?? <Mono>{p.step}</Mono>}</TableCell>
                <TableCell className="whitespace-normal">
                  <Mono className="break-words">{p.last_error}</Mono>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </List>
      {s.pending.length > 0 && <p className="mt-2 text-meta text-muted-foreground">{SCIM.PENDING_HINT}</p>}

      <List title={SCIM.DRIVES_TITLE} empty={s.drives.length === 0 ? SCIM.DRIVES_EMPTY : null}>
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>{SCIM.COL_PERSON}</TableHead>
              <TableHead>{SCIM.COL_DRIVE}</TableHead>
              <TableHead>{SCIM.COL_PURGED}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.drives.map((d) => (
              <TableRow key={`${d.person}/${d.drive}`}>
                <TableCell>{d.person}</TableCell>
                <TableCell>{d.drive}</TableCell>
                <TableCell>
                  <When iso={d.purged_at} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </List>
      {s.drives.length > 0 && <p className="mt-2 text-meta text-muted-foreground">{SCIM.DRIVES_HINT}</p>}
    </>
  );
}

export function ScimCard() {
  const [status, setStatus] = React.useState<SCIMStatus | null>(null);
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");

  const load = React.useCallback(() => {
    setState("loading");
    scimApi
      .getStatus()
      .then((s) => {
        setStatus(s);
        setState("ready");
      })
      .catch(() => setState("error"));
  }, []);
  React.useEffect(load, [load]);

  // Absent while unloaded or failed: ErrorState's own heading already says so once the card is open.
  const unfinished = status ? new Set(status.pending.map((p) => p.person)).size : 0;
  const summary =
    state !== "ready" || !status ? undefined : status.configured ? SCIM.SUMMARY_ON(status.deactivated.length, unfinished) : SCIM.SUMMARY_OFF;

  return (
    <CollapsibleCard title={SCIM.TITLE} summary={summary} testId="scim-card">
      <p className="text-body leading-snug text-muted-foreground">{SCIM.LEAD}</p>
      <div className="mt-3">
        {state === "loading" && <TableSkeleton rows={2} cols={3} />}
        {state === "error" && <ErrorState onRetry={load} />}
        {state === "ready" && status && !status.configured && (
          <>
            <p className="text-body text-foreground">{SCIM.OFF_BODY}</p>
            <p className="mt-2 text-body text-muted-foreground">
              {SCIM.OFF_HOW}{" "}
              <a href={SCIM_RUNBOOK_URL} target="_blank" rel="noreferrer" className="text-info underline-offset-2 hover:underline">
                {SCIM.OFF_LINK}
              </a>
            </p>
            <div className="mt-3 border-t border-border">
              <Endpoint />
            </div>
          </>
        )}
        {state === "ready" && status && status.configured && (
          <>
            <Facts s={status} />
            <Lists s={status} />
          </>
        )}
      </div>
    </CollapsibleCard>
  );
}
