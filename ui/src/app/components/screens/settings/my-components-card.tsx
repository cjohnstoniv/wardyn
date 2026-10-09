/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account: the components a person saved to reuse (#1914), read from and
// written to GET/POST/PUT/DELETE /me/components. A run only gets one when it is
// added on New run. Deleting is always offered, even once the person may no
// longer define components: removing a row only narrows what they can attach.
//
// The form is the New Run dialog's own, in its saving-only shape, loaded when
// first needed.
import * as React from "react";
import { Pencil, Plus, Trash2 } from "lucide-react";
import { components as api } from "../../../lib/api/components";
import type { Component, MyComponents } from "../../../lib/types";
import { Button } from "../../ui/button";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { MY_COMPONENTS as T } from "../../wardyn/copy/components";
import { DeleteConfirmDialog } from "../../wardyn/delete-confirm-dialog";

const CustomComponentDialog = React.lazy(() =>
  import("../new-run/custom-component-dialog").then((m) => ({ default: m.CustomComponentDialog })),
);

export function MyComponentsCard({ secretsPath = "/secrets" }: { secretsPath?: string }) {
  const [data, setData] = React.useState<MyComponents | null>(null);
  const [failed, setFailed] = React.useState(false);
  // undefined: closed; null: a new component; a row: editing it.
  const [editing, setEditing] = React.useState<Component | null | undefined>(undefined);
  const [toDelete, setToDelete] = React.useState<Component | null>(null);

  const load = React.useCallback(() => {
    setFailed(false);
    api
      .mine()
      .then(setData)
      .catch(() => setFailed(true));
  }, []);
  React.useEffect(load, [load]);

  // Nothing to show or do: no right to define one and none saved.
  if (data && !data.may_define && data.mine.length === 0) return null;

  return (
    <CollapsibleCard title={T.TITLE} summary={data ? T.SUMMARY(data.mine.length) : undefined} testId="my-components-card">
      {failed ? (
        <div className="flex items-center justify-between gap-3">
          <p className="text-sm text-danger">{T.LOAD_FAILED}</p>
          <Button variant="outline" size="sm" onClick={load}>
            {T.RETRY}
          </Button>
        </div>
      ) : (
        data && (
          <div className="space-y-3">
            <p className="text-xs text-muted-foreground">{T.LEAD}</p>
            {!data.may_define && <p className="text-xs text-muted-foreground">{T.NOT_ALLOWED}</p>}
            {data.mine.length === 0 ? (
              <p className="text-sm text-muted-foreground">{T.EMPTY}</p>
            ) : (
              <ul className="space-y-2">
                {data.mine.map((c) => (
                  <li key={c.id} className="flex items-center justify-between gap-3 rounded-lg border border-border px-3 py-2">
                    <span className="min-w-0">
                      <span className="block truncate text-sm font-medium text-foreground">{c.name}</span>
                      <span className="block text-xs text-muted-foreground">
                        {T.REACHES(c.definition.hosts.length)} · {T.SECRETS(c.definition.secrets?.length ?? 0)}
                      </span>
                    </span>
                    <span className="flex shrink-0 items-center gap-1">
                      {data.may_define && (
                        <Button type="button" variant="ghost" size="sm" aria-label={`${T.EDIT} ${c.name}`} onClick={() => setEditing(c)}>
                          <Pencil className="size-3.5" /> {T.EDIT}
                        </Button>
                      )}
                      <Button type="button" variant="ghost" size="sm" aria-label={`${T.DELETE} ${c.name}`} onClick={() => setToDelete(c)}>
                        <Trash2 className="size-3.5" /> {T.DELETE}
                      </Button>
                    </span>
                  </li>
                ))}
              </ul>
            )}
            {data.may_define && (
              <Button type="button" variant="outline" size="sm" onClick={() => setEditing(null)}>
                <Plus className="size-3.5" /> {T.NEW}
              </Button>
            )}
          </div>
        )
      )}

      {editing !== undefined && data && (
        <React.Suspense fallback={null}>
          <CustomComponentDialog
            context="saved"
            component={editing ?? undefined}
            residentAllowed={data.resident_delivery_allowed}
            secretsPath={secretsPath}
            onClose={() => setEditing(undefined)}
            onSaved={load}
          />
        </React.Suspense>
      )}

      <DeleteConfirmDialog
        name={toDelete?.name ?? null}
        entity="component"
        description={T.DELETE_BODY}
        allowed
        onOpenChange={(o) => !o && setToDelete(null)}
        onDelete={() => api.deleteMine(toDelete!.id)}
        onDeleted={() => {
          setToDelete(null);
          load();
        }}
      />
    </CollapsibleCard>
  );
}
