/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "What this type gets" (design §2.6) — the grid over GET
// /permissions/explain's rows (#739, AK-5). One row per capability kind's
// default (the "*" wildcard, the family's answer for a value no specific row
// names) plus every specific value the type (or `all`) holds an explicit
// grant for.
//
// This asks Explain with subject_type=user_type: until UT-3 widens
// capability_grants' CHECK constraint, no grant row can name a type, so every
// answer here comes from the `all` rows alone — correct, not merely a
// placeholder (the server's own doc comment on capabilitySubjectUserType).
import * as React from "react";
import { AlertTriangle, Loader2 } from "lucide-react";
import { permissions as api, type ExplainRow, type ExplainState } from "../../../lib/api/permissions";
import { CAPABILITY_KINDS, KIND } from "../../../lib/permissions-copy";
import { USER_TYPES as UT } from "../../../lib/user-types-copy";
import { Chip } from "../../wardyn/primitives";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";

const STATE_TONE: Record<ExplainState, "neutral" | "warning" | "danger"> = {
  everyone: "neutral",
  this_type: "neutral",
  blocked: "danger",
  admins_only: "warning",
  not_available: "warning",
};

function kindLabel(kind: string): string {
  return KIND[kind as keyof typeof KIND]?.label ?? kind;
}

// One kind's rows, folded to what the grid actually shows: the default ("*")
// row always renders, plus every specific value's row beneath it — so a kind
// with no per-value grant still gets one line, never a blank family.
function rowsByKind(rows: ExplainRow[]): Map<string, ExplainRow[]> {
  const byKind = new Map<string, ExplainRow[]>();
  for (const r of rows) {
    const list = byKind.get(r.kind) ?? [];
    list.push(r);
    byKind.set(r.kind, list);
  }
  return byKind;
}

export function ExplainGrid({ subject }: { subject: string }) {
  const [rows, setRows] = React.useState<ExplainRow[] | null>(null);
  const [failed, setFailed] = React.useState(false);

  React.useEffect(() => {
    let active = true;
    setRows(null);
    setFailed(false);
    api
      .explainCapabilities("user_type", subject, [...CAPABILITY_KINDS])
      .then((res) => active && setRows(res.rows))
      .catch(() => active && setFailed(true));
    return () => {
      active = false;
    };
  }, [subject]);

  if (failed) {
    return (
      <p className="flex items-center gap-2 text-body text-muted-foreground">
        <AlertTriangle className="size-3.5 shrink-0" />
        {UT.EXPLAIN_LOAD_FAILED}
      </p>
    );
  }
  if (!rows) {
    return (
      <p className="flex items-center gap-2 text-body text-muted-foreground">
        <Loader2 className="size-3.5 shrink-0 animate-spin" />
      </p>
    );
  }

  const byKind = rowsByKind(rows);
  const anyBlocked = rows.some((r) => r.state === "blocked");

  return (
    <>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>{UT.EXPLAIN_TITLE}</TableHead>
            <TableHead>Value</TableHead>
            <TableHead>State</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {CAPABILITY_KINDS.map((kind) =>
            (byKind.get(kind) ?? []).map((row) => (
              <TableRow key={`${row.kind}:${row.value}`}>
                <TableCell className="font-medium">{kindLabel(row.kind)}</TableCell>
                <TableCell className="font-mono text-xs">{row.value}</TableCell>
                <TableCell>
                  <Chip tone={STATE_TONE[row.state]}>{UT.EXPLAIN_STATE[row.state]}</Chip>
                </TableCell>
              </TableRow>
            )),
          )}
        </TableBody>
      </Table>
      {anyBlocked && <p className="mt-3 text-xs text-muted-foreground">{UT.WALL_WARNING}</p>}
    </>
  );
}
