/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings: the Your walls card (T4-a, owner-approved board R15). One private document per
// person, read and written whole at /me/walls. Every knob only narrows what the organisation allows;
// none the person leaves out is a limit. The card is absent for a caller who is no person (the admin
// token, local mode) and where the store keeps no personal settings (walls.ts's wallsAbsent).
import * as React from "react";
import { Check } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { walls as api, wallsAbsent } from "../../../lib/api/walls";
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
import { Button, buttonVariants } from "../../ui/button";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { YOUR_WALLS as T } from "../../wardyn/copy/personal-walls";
import { TableSkeleton } from "../../wardyn/states";
import { sectionOfRefusal, type FormSection } from "../../../lib/wall-preset-words";
import { formBlocked, toForm, toSpec, WallsForm, type Form } from "./walls-form";

export function YourWallsCard() {
  const [state, setState] = React.useState<"loading" | "absent" | "error" | "ready">("loading");
  const [form, setForm] = React.useState<Form>(() => toForm({}));
  const [busy, setBusy] = React.useState(false);
  const [saved, setSaved] = React.useState(false);
  // A refusal the server words: under the section it names, else above the buttons with its reason code.
  const [refusal, setRefusal] = React.useState<{ message: string; reason: string } | null>(null);
  const [sectionErrors, setSectionErrors] = React.useState<Partial<Record<FormSection, string>>>({});

  const load = React.useCallback(() => {
    setState("loading");
    api
      .get()
      .then((w) => {
        setForm(toForm(w.spec));
        setState("ready");
      })
      .catch((e) => setState(wallsAbsent(e) ? "absent" : "error"));
  }, []);
  React.useEffect(load, [load]);

  const edit = (patch: Partial<Form>) => {
    setForm((f) => ({ ...f, ...patch }));
    setSaved(false);
    setSectionErrors({});
  };
  const fail = (e: unknown) => {
    if (!(e instanceof HttpError)) return setRefusal({ message: T.SAVE_FAILED, reason: "" });
    const { section, text } = sectionOfRefusal(e.message);
    if (e.reason === "walls_invalid" && section && section !== "name" && section !== "description") return setSectionErrors({ [section]: text });
    setRefusal({ message: e.reason === "walls_invalid" ? text : e.message, reason: e.reason });
  };
  const run = async (act: () => Promise<void>): Promise<boolean> => {
    setBusy(true);
    setRefusal(null);
    setSectionErrors({});
    try {
      await act();
      return true;
    } catch (e) {
      fail(e);
      return false;
    } finally {
      setBusy(false);
    }
  };
  const save = () =>
    run(async () => {
      const w = await api.put(toSpec(form));
      setForm(toForm(w.spec));
      setSaved(true);
    });
  const removeAll = () =>
    run(async () => {
      await api.clear();
      setForm(toForm({}));
      setSaved(false);
    });
  const [confirmingRemove, setConfirmingRemove] = React.useState(false);

  if (state === "absent") return null;
  return (
    <CollapsibleCard title={T.TITLE} summary={T.LEDE} testId="your-walls-card">
      {state === "loading" && <TableSkeleton />}
      {state === "error" && (
        <div className="flex items-center justify-between gap-3">
          <p className="text-sm text-danger">{T.LOAD_FAILED}</p>
          <Button variant="outline" size="sm" onClick={load}>
            {T.RETRY}
          </Button>
        </div>
      )}
      {state === "ready" && (
        <div className="flex flex-col gap-[18px]">
          {/* Read-only while a save or remove is in flight: its answer replaces the form, so an edit made
              meanwhile would be dropped while the card says Saved. */}
          <fieldset disabled={busy} className="m-0 min-w-0 border-0 p-0">
            <WallsForm form={form} onEdit={edit} errors={sectionErrors} />
          </fieldset>
          {refusal && (
            <p role="alert" className="text-sm text-danger">
              {refusal.message}
              {refusal.reason && <span className="ml-2 font-mono text-[11px] text-muted-foreground">{refusal.reason}</span>}
            </p>
          )}
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" disabled={busy || formBlocked(form)} onClick={() => void save()}>
              {T.SAVE}
            </Button>
            <Button size="sm" variant="outline" disabled={busy} onClick={() => setConfirmingRemove(true)}>
              {T.REMOVE_ALL}
            </Button>
          </div>
          {saved && (
            <p role="status" className="flex items-center gap-2 rounded-md border border-success/25 bg-success-subtle px-3 py-2 text-xs text-success">
              <Check className="size-3.5" aria-hidden="true" />
              {T.SAVED}
            </p>
          )}
          <p className="text-xs text-muted-foreground">{T.RESTART_NOTE}</p>
        </div>
      )}
      <AlertDialog open={confirmingRemove} onOpenChange={(o) => !o && !busy && setConfirmingRemove(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{T.REMOVE_ALL_TITLE}</AlertDialogTitle>
            <AlertDialogDescription>{T.REMOVE_ALL_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          {refusal && (
            <p role="alert" className="text-sm text-danger">
              {refusal.message}
              {refusal.reason && <span className="ml-2 font-mono text-[11px] text-muted-foreground">{refusal.reason}</span>}
            </p>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>{T.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              disabled={busy}
              onClick={(e) => {
                e.preventDefault();
                void removeAll().then((ok) => ok && setConfirmingRemove(false));
              }}
            >
              {T.REMOVE_ALL}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </CollapsibleCard>
  );
}
