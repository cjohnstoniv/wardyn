/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Add control's list (#1914): what a person may add to this run. A custom
// component only when GET /me/components says they may define their own
// (`may_define`); their saved components with it; the organisation's that were
// granted to them always. Repositories are not here: they stay in Workspace.
//
// Loaded when the dialog opens, so nothing is fetched until someone asks. The
// custom form is its own lazy chunk.
import * as React from "react";
import { Loader2, Plus } from "lucide-react";
import { components as api } from "../../../lib/api/components";
import type { ComponentRef, MyComponents } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { ADD_ACCESS as T } from "../../wardyn/copy/components";
import { ErrorState } from "../../wardyn/states";
import { isAttached } from "./custom-component-form-model";

const CustomComponentDialog = React.lazy(() =>
  import("./custom-component-dialog").then((m) => ({ default: m.CustomComponentDialog })),
);

/** types.MaxComponentRefs: the most one run carries. */
export const MAX_COMPONENT_REFS = 8;

export interface AddAccessDialogProps {
  /** What the run carries now. */
  refs: ComponentRef[];
  secretsPath: string;
  guardLink?: (to: string) => (e: React.MouseEvent) => void;
  onAdd: (ref: ComponentRef) => void;
  onClose: () => void;
}

function Item({ name, hosts, added, full, onAdd }: { name: string; hosts: string[]; added: boolean; full: boolean; onAdd: () => void }) {
  return (
    <li className="flex items-center justify-between gap-3 rounded-lg border border-border px-3 py-2">
      <span className="min-w-0">
        <span className="block truncate text-sm font-medium text-foreground">{name}</span>
        <span className="block truncate font-mono text-xs text-muted-foreground">{T.REACHES(hosts)}</span>
      </span>
      <Button type="button" variant="outline" size="sm" disabled={added || full} aria-label={`${added ? T.ADDED : T.ADD} ${name}`} onClick={onAdd}>
        {added ? T.ADDED : T.ADD}
      </Button>
    </li>
  );
}

export function AddAccessDialog({ refs, secretsPath, guardLink, onAdd, onClose }: AddAccessDialogProps) {
  const [data, setData] = React.useState<MyComponents | null>(null);
  const [failed, setFailed] = React.useState(false);
  const [custom, setCustom] = React.useState(false);

  const load = React.useCallback(() => {
    setFailed(false);
    api
      .mine()
      .then(setData)
      .catch(() => setFailed(true));
  }, []);
  React.useEffect(load, [load]);

  const full = refs.length >= MAX_COMPONENT_REFS;
  const add = (ref: ComponentRef) => {
    onAdd(ref);
    onClose();
  };

  if (custom && data) {
    return (
      <React.Suspense fallback={null}>
        <CustomComponentDialog
          context="run"
          residentAllowed={data.resident_delivery_allowed}
          secretsPath={secretsPath}
          guardLink={guardLink}
          onClose={onClose}
          onAttach={onAdd}
        />
      </React.Suspense>
    );
  }

  const mine = data?.may_define ? data.mine : [];
  const org = data?.org ?? [];

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="scroll-thin max-h-[90vh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{T.TITLE}</DialogTitle>
          <DialogDescription>{T.LEAD}</DialogDescription>
        </DialogHeader>

        {failed && <ErrorState message={T.LOAD_FAILED} onRetry={load} />}
        {!failed && data === null && (
          <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" aria-hidden="true" />
            {T.LOADING}
          </p>
        )}

        {data && (
          <div className="space-y-4">
            {full && <p className="text-sm text-muted-foreground">{T.FULL(MAX_COMPONENT_REFS)}</p>}
            {data.may_define ? (
              <button
                type="button"
                disabled={full}
                onClick={() => setCustom(true)}
                className="flex w-full items-start gap-3 rounded-lg border border-border px-3 py-2 text-left outline-none hover:border-border-strong focus-visible:ring-[3px] focus-visible:ring-ring disabled:opacity-50"
              >
                <Plus className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                <span>
                  <span className="block text-sm font-medium text-foreground">{T.CUSTOM}</span>
                  <span className="block text-xs text-muted-foreground">{T.CUSTOM_HINT}</span>
                </span>
              </button>
            ) : (
              <p className="text-sm text-muted-foreground">{T.NOT_ALLOWED}</p>
            )}

            {mine.length > 0 && (
              <section aria-label={T.MINE} className="space-y-2">
                <h3 className="text-xs font-medium text-muted-foreground">{T.MINE}</h3>
                <ul className="space-y-2">
                  {mine.map((c) => (
                    <Item key={c.id} name={c.name} hosts={c.definition.hosts} added={isAttached(refs, c.id)} full={full} onAdd={() => add({ id: c.id })} />
                  ))}
                </ul>
              </section>
            )}
            {org.length > 0 && (
              <section aria-label={T.ORG} className="space-y-2">
                <h3 className="text-xs font-medium text-muted-foreground">{T.ORG}</h3>
                <ul className="space-y-2">
                  {org.map((c) => (
                    <Item key={c.id} name={c.name} hosts={c.hosts} added={isAttached(refs, c.id)} full={full} onAdd={() => add({ id: c.id })} />
                  ))}
                </ul>
              </section>
            )}
          </div>
        )}

        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            {T.CLOSE}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
