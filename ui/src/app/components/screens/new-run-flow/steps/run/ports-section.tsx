/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { UIApp } from "../../../../../lib/types/runs";
import { MAX_APPS, portProblems } from "../../lenses/apps-lens";

const ROW = "grid grid-cols-[minmax(0,1fr)_120px_minmax(0,1fr)_auto] items-center gap-2 max-[520px]:grid-cols-[minmax(0,1fr)_96px]";

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
  return (
    <fieldset className="m-0 flex min-w-0 flex-col gap-2 border-0 p-0">
      <legend className="mb-1 p-0 text-sm font-medium">{NR_RUN.PORTS_LABEL}</legend>
      <p className="text-xs leading-snug text-muted-foreground">{NR_RUN.PORTS_HELP}</p>
      <div aria-hidden="true" className={`${ROW} pt-1 text-xs text-muted-foreground max-[520px]:hidden`}>
        <span>{NR_RUN.PORT_COL_NAME}</span><span>{NR_RUN.PORT_COL_PORT}</span><span>{NR_RUN.PORT_COL_PATH}</span><span />
      </div>
      {apps.map((app, i) => {
        const problem = problems[i];
        const error = problem && (problem.code === "app_port_invalid" ? NR_RUN.PORT_INVALID : NR_RUN.PORT_DUPLICATE(problem.port));
        return (
          // Rows have no identity of their own; an index key keeps a focused field focused while it is typed in.
          <div key={i} data-app-row={i} className="flex flex-col gap-1">
            <div className={ROW}>
              <Input aria-label={NR_RUN.PORT_COL_NAME} value={app.name} onChange={(e) => edit(i, { name: e.target.value })} />
              <Input
                id={`nr-app-port-${i}`}
                aria-label={NR_RUN.PORT_COL_PORT}
                className="font-mono"
                inputMode="numeric"
                value={app.port ? String(app.port) : ""}
                aria-invalid={!!problem || undefined}
                aria-describedby={error ? `nr-app-port-${i}-error` : undefined}
                onChange={(e) => edit(i, { port: Number(e.target.value.replace(/\D/g, "").slice(0, 6)) })}
              />
              <Input aria-label={NR_RUN.PORT_COL_PATH} className="font-mono" value={app.path ?? ""} onChange={(e) => edit(i, { path: e.target.value })} />
              <Button type="button" variant="ghost" size="sm" onClick={() => remove(i)}>{NR_SHARED.REMOVE}</Button>
            </div>
            {error && <p id={`nr-app-port-${i}-error`} role="alert" className="text-sm text-danger">{error}</p>}
          </div>
        );
      })}
      {apps.length === 0 && <p role="alert" className="text-sm text-danger">{NR_RUN.PORT_REQUIRED}</p>}
      <div className="flex items-center gap-2.5">
        <Button id="nr-app-add" type="button" variant="outline" size="sm" disabled={full} onClick={add}>{NR_RUN.PORT_ADD}</Button>
        {full && <span className="text-xs text-muted-foreground">{NR_RUN.PORTS_FULL}</span>}
      </div>
    </fieldset>
  );
}
