/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { useRef } from "react";
import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { UIApp } from "../../../../../lib/types/runs";
import { MAX_APPS, portProblems } from "../../lenses/apps-lens";
import { appIssueText } from "./app-issue-text";

const ROW = "grid grid-cols-[minmax(0,1fr)_120px_minmax(0,1fr)_auto] items-center gap-2 max-[520px]:grid-cols-[minmax(0,1fr)_96px]";

const FIELDS = ["name", "port", "path"] as const;
type Field = (typeof FIELDS)[number];

interface Props {
  apps: readonly UIApp[];
  edit: (index: number, patch: Partial<UIApp>) => void;
  add: () => void;
  remove: (index: number) => void;
}

/** The ports an Application / server opens from the run page: a name, a port and a landing path each. */
export function PortsSection({ apps, edit, add, remove }: Props) {
  const problems = portProblems(apps);
  const full = apps.length >= MAX_APPS;
  const addRef = useRef<HTMLButtonElement>(null);
  // The last row's Remove button leaves with its row; focus goes to the control that brings a row back.
  const removeRow = (i: number) => {
    remove(i);
    if (apps.length === 1) addRef.current?.focus();
  };
  return (
    <fieldset className="m-0 flex min-w-0 flex-col gap-2 border-0 p-0">
      <legend className="mb-1 p-0 text-sm font-medium">{NR_RUN.PORTS_LABEL}</legend>
      <p className="text-xs leading-snug text-muted-foreground">{NR_RUN.PORTS_HELP}</p>
      <div aria-hidden="true" className={`${ROW} pt-1 text-xs text-muted-foreground max-[520px]:hidden`}>
        <span>{NR_RUN.PORT_COL_NAME}</span><span>{NR_RUN.PORT_COL_PORT}</span><span>{NR_RUN.PORT_COL_PATH}</span><span />
      </div>
      {apps.map((app, i) => {
        const who = app.name || NR_RUN.APP_ROW(i + 1);
        const attrs = (f: Field) => ({
          id: `nr-app-${f}-${i}`,
          "aria-invalid": problems[i][f] ? true : undefined,
          "aria-describedby": problems[i][f] ? `nr-app-${f}-${i}-error` : undefined,
        });
        return (
          // Rows have no identity of their own; an index key keeps a focused field focused while it is typed in.
          <div key={i} data-app-row={i} className="flex flex-col gap-1">
            <div className={ROW}>
              <Input {...attrs("name")} aria-label={NR_RUN.APP_NAME_ARIA(i + 1)} value={app.name} onChange={(e) => edit(i, { name: e.target.value })} />
              <Input
                {...attrs("port")}
                aria-label={NR_RUN.APP_PORT_ARIA(who)}
                className="font-mono"
                inputMode="numeric"
                value={app.port ? String(app.port) : ""}
                onChange={(e) => edit(i, { port: Number(e.target.value.replace(/\D/g, "").slice(0, 6)) })}
              />
              <Input {...attrs("path")} aria-label={NR_RUN.APP_PATH_ARIA(who)} className="font-mono" value={app.path ?? ""} onChange={(e) => edit(i, { path: e.target.value })} />
              <Button type="button" variant="ghost" size="sm" aria-label={NR_RUN.APP_REMOVE_ARIA(who)} onClick={() => removeRow(i)}>{NR_SHARED.REMOVE}</Button>
            </div>
            {FIELDS.map((f) => {
              const problem = problems[i][f];
              return problem && <p key={f} id={`nr-app-${f}-${i}-error`} role="alert" className="text-sm text-danger">{appIssueText(problem)}</p>;
            })}
          </div>
        );
      })}
      {apps.length === 0 && <p role="alert" className="text-sm text-danger">{NR_RUN.PORT_REQUIRED}</p>}
      <div className="flex items-center gap-2.5">
        <Button ref={addRef} id="nr-app-add" type="button" variant="outline" size="sm" disabled={full} onClick={add}>{NR_RUN.PORT_ADD}</Button>
        {full && <span className="text-xs text-muted-foreground">{NR_RUN.PORTS_FULL}</span>}
      </div>
    </fieldset>
  );
}
