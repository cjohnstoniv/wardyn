/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { useRef } from "react";
import { Button } from "../../../../ui/button";
import { Checkbox } from "../../../../ui/checkbox";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { UIApp } from "../../../../../lib/types/runs";
import { MAX_APPS, portProblems } from "../../lenses/apps-lens";
import { appIssueText } from "./app-issue-text";
import { CallersEditor } from "./callers-editor";

const ROW = "grid grid-cols-[minmax(0,1fr)_120px_minmax(0,1fr)_auto] items-end gap-2 max-[520px]:grid-cols-[minmax(0,1fr)_96px]";

const FIELDS = ["name", "port", "path"] as const;
type Field = (typeof FIELDS)[number];

interface Props {
  /** The id of the section's title, which names the group of ports. */
  labelId: string;
  apps: readonly UIApp[];
  edit: (index: number, patch: Partial<UIApp>) => void;
  add: () => void;
  remove: (index: number) => void;
  /** A service's ports: none is fine. A server needs one. */
  optional?: boolean;
  /** The server refused a port callable from outside: say so under the checkbox. */
  inboundRefused?: boolean;
}

/** The ports an Application / server opens from the run page, one card each: a name, a port and a landing path, and whether it is callable from outside. */
export function PortsSection({ labelId, apps, edit, add, remove, optional, inboundRefused }: Props) {
  const problems = portProblems(apps);
  const full = apps.length >= MAX_APPS;
  const addRef = useRef<HTMLButtonElement>(null);
  const removeRefs = useRef<(HTMLButtonElement | null)[]>([]);
  // The bottom row's Remove button leaves with its row. Focus goes to the row above's Remove, or to Add when no row
  // is left (a full list's Add is still disabled at this moment, so the row above is the only choice there).
  const removeRow = (i: number) => {
    remove(i);
    if (i !== apps.length - 1) return;
    if (i > 0) removeRefs.current[i - 1]?.focus();
    else addRef.current?.focus();
  };
  return (
    <fieldset aria-labelledby={labelId} className="m-0 flex min-w-0 flex-col gap-3 border-0 p-0">
      <p className="text-xs leading-snug text-muted-foreground">{NR_RUN.PORTS_HELP}</p>
      {apps.map((app, i) => {
        const who = app.name || NR_RUN.APP_ROW(i + 1);
        const attrs = (f: Field) => ({
          id: `nr-app-${f}-${i}`,
          "aria-invalid": problems[i][f] ? true : undefined,
          "aria-describedby": problems[i][f] ? `nr-app-${f}-${i}-error` : undefined,
        });
        const inboundId = `nr-app-inbound-${i}`;
        return (
          // Rows have no identity of their own; an index key keeps a focused field focused while it is typed in.
          <div key={i} data-app-row={i} role="group" aria-label={who} className="flex flex-col gap-3 rounded-lg border border-border p-3.5">
            <div className="flex flex-col gap-1">
              <div className={ROW}>
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor={`nr-app-name-${i}`}>{NR_RUN.PORT_COL_NAME}</Label>
                  <Input {...attrs("name")} aria-label={NR_RUN.APP_NAME_ARIA(i + 1)} value={app.name} onChange={(e) => edit(i, { name: e.target.value })} />
                </div>
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor={`nr-app-port-${i}`}>{NR_RUN.PORT_COL_PORT}</Label>
                  <Input
                    {...attrs("port")}
                    aria-label={NR_RUN.APP_PORT_ARIA(who)}
                    className="font-mono"
                    inputMode="numeric"
                    value={app.port ? String(app.port) : ""}
                    onChange={(e) => edit(i, { port: Number(e.target.value.replace(/\D/g, "").slice(0, 6)) })}
                  />
                </div>
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor={`nr-app-path-${i}`}>{NR_RUN.PORT_COL_PATH}</Label>
                  <Input {...attrs("path")} aria-label={NR_RUN.APP_PATH_ARIA(who)} className="font-mono" value={app.path ?? ""} onChange={(e) => edit(i, { path: e.target.value })} />
                </div>
                <Button ref={(el) => { removeRefs.current[i] = el; }} type="button" variant="ghost" size="sm" aria-label={NR_RUN.APP_REMOVE_ARIA(who)} onClick={() => removeRow(i)}>{NR_SHARED.REMOVE}</Button>
              </div>
              {FIELDS.map((f) => {
                const problem = problems[i][f];
                return problem && <p key={f} id={`nr-app-${f}-${i}-error`} role="alert" className="text-sm text-danger">{appIssueText(problem)}</p>;
              })}
            </div>
            <div className="flex items-start gap-2.5 border-t border-border pt-3">
              <Checkbox
                id={inboundId}
                className="mt-0.5"
                checked={!!app.inbound}
                aria-describedby={`${inboundId}-help`}
                // Callers mean something only on an inbound port (the server refuses them otherwise), so they leave with it.
                onCheckedChange={(on) => edit(i, on === true ? { inbound: true } : { inbound: undefined, callers: undefined })}
              />
              <div className="flex min-w-0 flex-1 flex-col gap-1.5">
                <Label htmlFor={inboundId}>{NR_RUN.PORT_INBOUND_LABEL}</Label>
                <p id={`${inboundId}-help`} className="text-xs leading-snug text-muted-foreground">{NR_RUN.PORT_INBOUND_HELP}</p>
                {app.inbound && inboundRefused && <p role="alert" className="text-sm text-danger">{NR_RUN.PORT_INBOUND_REFUSED}</p>}
                {app.inbound && <CallersEditor app={app} index={i} who={who} onChange={(callers) => edit(i, { callers: callers.length ? callers : undefined })} />}
              </div>
            </div>
          </div>
        );
      })}
      {apps.length === 0 && !optional && <p role="alert" className="text-sm text-danger">{NR_RUN.PORT_REQUIRED}</p>}
      <div className="flex items-center gap-2.5">
        <Button ref={addRef} id="nr-app-add" type="button" variant="outline" size="sm" disabled={full} onClick={add}>{NR_RUN.PORT_ADD}</Button>
        {full && <span className="text-xs text-muted-foreground">{NR_RUN.PORTS_FULL}</span>}
      </div>
    </fieldset>
  );
}
