/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// /admin/components: the organisation's custom components and who may use each.
// Super-admin only, with no nav item: it is reached from the Settings card.
// GET /components is operatorOnly, so a security admin who types the URL meets
// a 403, answered as the tier it is rather than as an outage.
//
// A new component is "Available to: Nobody yet" from the moment it exists (the
// server writes its restriction in the same transaction as the row), so nothing
// here ever offers "Only these" with nobody listed (AvailabilityControl's
// nobodyYet). Strings: copy/components-admin.ts.
import * as React from "react";
import { Blocks, ChevronDown, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { components as api } from "../../../lib/api/components";
import { permissions } from "../../../lib/api/permissions";
import { getErrorMessage } from "../../../lib/format";
import type { AvailabilityView, Component } from "../../../lib/types";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { Button } from "../../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../ui/table";
import { AvailabilityControl } from "../../wardyn/availability-control";
import { COMPONENTS_ADMIN as T } from "../../wardyn/copy/components-admin";
import { PageHeader } from "../../wardyn/page-header";
import { Chip, OperatorOnlyHint } from "../../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton, loadFailStatus, type ScreenStatus } from "../../wardyn/states";
import { ComponentEditor } from "./component-editor";

// What the row says about who may use it. Read per row, because the restriction
// bit and the allow list are two facts and only the pair says "nobody".
function AvailabilityChip({ id }: { id: string }) {
  const [view, setView] = React.useState<AvailabilityView | "error" | null>(null);
  React.useEffect(() => {
    let live = true;
    permissions
      .getAvailability("component", id)
      .then((v) => live && setView(v))
      .catch(() => live && setView("error"));
    return () => {
      live = false;
    };
  }, [id]);
  if (view === null) return <Loader2 className="size-3.5 animate-spin text-muted-foreground" aria-hidden="true" />;
  if (view === "error") return <Chip tone="neutral">{T.AVAIL_UNKNOWN}</Chip>;
  if (!view.restricted) return <Chip tone="warning">{T.AVAIL_EVERYONE}</Chip>;
  return view.allowed_by.length === 0 ? (
    <Chip tone="neutral">{T.AVAIL_NOBODY}</Chip>
  ) : (
    <Chip tone="info">{T.AVAIL_ONLY(view.allowed_by.length)}</Chip>
  );
}

export function ComponentsScreen() {
  const [rows, setRows] = React.useState<Component[]>([]);
  const [status, setStatus] = React.useState<ScreenStatus>("loading");
  // null = closed; {component: null} = a new one.
  const [editing, setEditing] = React.useState<{ component: Component | null } | null>(null);
  // One row's "Available to" open at a time.
  const [openId, setOpenId] = React.useState<string | null>(null);
  const [toDelete, setToDelete] = React.useState<Component | null>(null);
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(() => {
    api
      .list()
      .then((r) => {
        setRows(r);
        setStatus("ready");
      })
      .catch((e) => setStatus(loadFailStatus(e)));
  }, []);
  React.useEffect(load, [load]);

  const toggleWho = (id: string) => setOpenId((cur) => (cur === id ? null : id));

  const remove = async (c: Component) => {
    setBusy(true);
    try {
      await api.remove(c.id);
      setToDelete(null);
      setOpenId((cur) => (cur === c.id ? null : cur));
      toast.success(T.DELETED_TOAST);
      load();
    } catch (e) {
      setToDelete(null);
      toast.error(T.DELETE_FAILED, { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const empty = status === "ready" && rows.length === 0;

  return (
    <div className="mx-auto max-w-[1120px] px-6 py-6">
      <PageHeader
        title={T.TITLE}
        description={T.LEAD}
        actions={
          status === "ready" && !empty ? (
            <Button onClick={() => setEditing({ component: null })}>{T.ADD}</Button>
          ) : undefined
        }
      />

      {status === "loading" && <TableSkeleton rows={4} cols={5} />}
      {status === "forbidden" && (
        <div className="mt-6">
          <OperatorOnlyHint />
        </div>
      )}
      {status === "error" && <ErrorState onRetry={load} />}
      {empty && (
        <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
          <EmptyState
            icon={Blocks}
            title={T.EMPTY_TITLE}
            description={T.EMPTY_BODY}
            action={<Button onClick={() => setEditing({ component: null })}>{T.ADD}</Button>}
          />
        </section>
      )}

      {status === "ready" && !empty && (
        <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{T.COL_NAME}</TableHead>
                <TableHead>{T.COL_REACHES}</TableHead>
                <TableHead>{T.COL_SECRETS}</TableHead>
                <TableHead>{T.COL_AVAILABLE}</TableHead>
                <TableHead className="w-[1%]" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((c) => (
                <React.Fragment key={c.id}>
                  <TableRow>
                    <TableCell className="font-medium text-foreground">{c.name}</TableCell>
                    <TableCell>{T.N_HOSTS(c.definition.hosts.length)}</TableCell>
                    <TableCell>{T.N_SECRETS((c.definition.secrets ?? []).length)}</TableCell>
                    {/* Unmounted while the control is open, so closing it re-reads what it changed. */}
                    <TableCell>{openId === c.id ? null : <AvailabilityChip id={c.id} />}</TableCell>
                    <TableCell>
                      <div className="flex items-center justify-end gap-1 whitespace-nowrap">
                        <Button
                          variant="outline"
                          size="sm"
                          aria-expanded={openId === c.id}
                          aria-label={T.WHO_ARIA(c.name)}
                          onClick={() => toggleWho(c.id)}
                        >
                          {T.WHO}
                          <ChevronDown className={openId === c.id ? "size-3.5 rotate-180" : "size-3.5"} />
                        </Button>
                        <Button variant="ghost" size="sm" aria-label={T.EDIT_ARIA(c.name)} onClick={() => setEditing({ component: c })}>
                          {T.EDIT}
                        </Button>
                        <Button variant="ghost" size="sm" aria-label={T.DELETE_ARIA(c.name)} onClick={() => setToDelete(c)}>
                          {T.DELETE}
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                  {openId === c.id && (
                    <TableRow className="hover:bg-transparent">
                      <TableCell colSpan={5} className="whitespace-normal bg-surface-2 px-6 py-4">
                        <AvailabilityControl
                          kind="component"
                          value={c.id}
                          onlyHint={T.ONLY_HINT}
                          nobodyYet={{ note: T.NOBODY_YET, addFirst: T.ADD_FIRST }}
                        />
                      </TableCell>
                    </TableRow>
                  )}
                </React.Fragment>
              ))}
            </TableBody>
          </Table>
        </section>
      )}

      {editing && (
        <ComponentEditor
          component={editing.component}
          onClose={() => setEditing(null)}
          onSaved={(id, created) => {
            setEditing(null);
            toast.success(T.SAVED_TOAST);
            // A new component is available to nobody: open the control that fixes that.
            if (created) setOpenId(id);
            load();
          }}
        />
      )}

      <AlertDialog open={!!toDelete} onOpenChange={(o) => !o && setToDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{T.DELETE_TITLE(toDelete?.name ?? "")}</AlertDialogTitle>
            <AlertDialogDescription>{T.DELETE_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{T.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-danger text-danger-foreground hover:bg-danger/90"
              onClick={(e) => {
                e.preventDefault();
                if (toDelete) void remove(toDelete);
              }}
            >
              {busy ? <Loader2 className="size-4 animate-spin" /> : null}
              {T.DELETE_CONFIRM}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
