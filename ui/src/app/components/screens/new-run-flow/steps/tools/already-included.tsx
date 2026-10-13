/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the earlier steps bring: one row per harness, repository, drive and
// command from `access.rows`, each opening to the hosts and credentials it
// adds. Nothing here is chosen on this step, so nothing here is added or removed.

import * as React from "react";
import { ChevronDown } from "lucide-react";
import { Button } from "../../../../ui/button";
import { Chip } from "../../../../wardyn/primitives";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { AccessFacts, AccessRow } from "../../../../../lib/types/new-run-access";
import type { RunIntent } from "../../draft/run-intent";
import { rowLabel, startsWithRun, type HarnessNames } from "../../lenses/tools-lens";
import { includedMark } from "./catalogue-pending";
import { IconTile } from "./icon-tile";
import { credentialLabel, deliveryLabel } from "./totals";

const FROM: Record<string, string> = {
  harness: NR_TOOLS.FROM_HARNESS,
  repository: NR_TOOLS.FROM_REPOSITORY,
  drive: NR_TOOLS.FROM_DRIVE,
  command: NR_TOOLS.FROM_COMMAND,
};

function subLine(row: AccessRow, access: AccessFacts, intent: RunIntent): string | undefined {
  if (row.from === "harness") return startsWithRun(intent, row.name) ? NR_TOOLS.SUB_STARTS : NR_TOOLS.SUB_AVAILABLE;
  if (row.from === "command") return NR_TOOLS.SUB_COMMAND;
  if (row.from === "repository" && access.credentials.some((c) => c.row === row.id && c.delivery === "git_broker")) return NR_TOOLS.REPO_SUB;
  return undefined;
}

function Status({ status, answered }: { status: string; answered: boolean }) {
  if (!answered) return <Chip>{NR_TOOLS.NET_REQUESTED}</Chip>;
  if (status === "ready") return <Chip tone="success" dot>{NR_TOOLS.STATUS_READY}</Chip>;
  if (status === "needs_setup") return <Chip tone="warning" dot>{NR_TOOLS.STATUS_NEEDS_SETUP}</Chip>;
  return null;
}

function Detail({ row, access, providers }: { row: AccessRow; access: AccessFacts; providers?: ReadonlyMap<string, string> }) {
  const credentials = access.credentials.filter((c) => row.credentials.includes(c.id));
  const bare = row.hosts.length === 0 && credentials.length === 0;
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
      <dt className="text-muted-foreground">{NR_TOOLS.DETAIL_NETWORK}</dt>
      <dd className="flex flex-wrap gap-1.5">
        {row.hosts.length ? row.hosts.map((h) => <span key={h} className="font-mono [overflow-wrap:anywhere]">{h}</span>) : <span className="text-muted-foreground">{NR_TOOLS.DETAIL_NONE}</span>}
      </dd>
      <dt className="text-muted-foreground">{NR_TOOLS.DETAIL_CREDENTIAL}</dt>
      <dd className="flex flex-col gap-1">
        {credentials.length ? credentials.map((c) => {
          const label = credentialLabel(c, row.name, providers);
          const delivery = deliveryLabel(c.delivery);
          return label || delivery ? (
            <span key={c.id} className="flex flex-wrap items-center gap-1.5">
              {label}
              {delivery && <Chip className="whitespace-normal">{delivery}</Chip>}
            </span>
          ) : null;
        }) : <span className="text-muted-foreground">{NR_TOOLS.DETAIL_NONE}</span>}
      </dd>
      {(row.from === "command" || bare) && (
        <>
          <dt className="text-muted-foreground">{NR_TOOLS.DETAIL_NOTE}</dt>
          <dd>{row.from === "command" ? NR_TOOLS.NOTE_COMMAND : NR_TOOLS.NOTE_NONE_NEEDED}</dd>
        </>
      )}
    </dl>
  );
}

function IncludedRow({ row, access, answered, intent, names, providers }: { row: AccessRow; access: AccessFacts; answered: boolean; intent: RunIntent; names?: HarnessNames; providers?: ReadonlyMap<string, string> }) {
  const [open, setOpen] = React.useState(false);
  const detail = React.useId();
  const sub = subLine(row, access, intent);
  const name = rowLabel(row, names);
  return (
    <li className="flex flex-col gap-3 rounded-lg border border-border bg-surface-2 p-3">
      <div className="flex flex-wrap items-center gap-3">
        <IconTile mark={includedMark(row)} />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <strong className="font-medium">{name}</strong>
            <Chip>{FROM[row.from] ?? row.from}</Chip>
          </div>
          {sub && <p className="text-sm text-muted-foreground">{sub}</p>}
        </div>
        <Status status={row.status} answered={answered} />
        <Button type="button" variant="ghost" size="sm" aria-expanded={open} aria-controls={open ? detail : undefined} onClick={() => setOpen(!open)}>
          <ChevronDown aria-hidden className={open ? "rotate-180" : undefined} />
          <span className="sr-only">{NR_TOOLS.DETAILS_FOR(name)}</span>
        </Button>
      </div>
      {open && <div id={detail}><Detail row={row} access={access} providers={providers} /></div>}
    </li>
  );
}

export function AlreadyIncluded({ access, answered, intent, names, providers }: { access: AccessFacts | undefined; answered: boolean; intent: RunIntent; names?: HarnessNames; providers?: ReadonlyMap<string, string> }) {
  const headingId = React.useId();
  const rows = access?.rows.filter((r) => r.from in FROM) ?? [];
  return (
    <section aria-labelledby={headingId} className="flex flex-col gap-3.5 rounded-xl border border-border bg-card p-[18px]">
      <div>
        <h2 id={headingId} className="font-semibold">{NR_TOOLS.INCLUDED_TITLE}</h2>
        <p className="text-sm text-muted-foreground">{NR_TOOLS.INCLUDED_BODY}</p>
      </div>
      {access && rows.length > 0 && (
        <ul className="flex flex-col gap-2">
          {rows.map((row) => <IncludedRow key={row.id} row={row} access={access} answered={answered} intent={intent} names={names} providers={providers} />)}
        </ul>
      )}
    </section>
  );
}
