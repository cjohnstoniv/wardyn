/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings: the Wall presets card, under Your walls (boards WP-1). A preset is a named, narrow-only walls document a
// person picks per run at New Run. The list shows the built-ins, the organisation's, each group's and the person's own;
// the card is absent for a caller who is no person (as Your walls is) and where the store keeps no wall presets.
import * as React from "react";
import { Check, ChevronDown, ChevronRight } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { wallPresets, wallPresetsAbsent } from "../../../lib/api/wall-presets";
import { walls as wallsApi } from "../../../lib/api/walls";
import { relativeTime } from "../../../lib/format";
import type { TemplatePublish } from "../../../lib/types/templates";
import type { WallPreset, WallPresetList } from "../../../lib/types/wall-presets";
import { presetChips } from "../../../lib/wall-preset-words";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";
import { Chip } from "../../wardyn/primitives";
import { TableSkeleton } from "../../wardyn/states";
import { WallPresetDetail } from "./wall-preset-detail";
import { WallPresetDialog, type PresetDialogMode } from "./wall-preset-dialog";

type State = "loading" | "absent" | "error" | "ready";

interface Section {
  key: string;
  heading: string;
  rows: WallPreset[];
}

function sectionsOf(list: WallPresetList): Section[] {
  const of = (pick: (p: WallPreset) => boolean) => list.presets.filter(pick);
  const groups = [...new Set(list.presets.filter((p) => p.scope === "group").map((p) => p.group_id ?? ""))];
  const sections: Section[] = [
    { key: "builtin", heading: W.BUILT_IN, rows: of((p) => p.scope === "builtin") },
    { key: "org", heading: W.ORGANISATION, rows: of((p) => p.scope === "org") },
    ...groups.map((g) => ({ key: `group:${g}`, heading: W.GROUP_OF(list.publish.group_labels?.[g] ?? g), rows: of((p) => p.scope === "group" && (p.group_id ?? "") === g) })),
    { key: "person", heading: W.YOURS, rows: of((p) => p.scope === "person") },
  ];
  return sections.filter((s) => s.rows.length > 0);
}

/** Where the caller may write: their own presets; the organisation's in the Admin view; a group they manage and are verified in. The server decides. */
const mayWrite = (p: WallPreset, publish: TemplatePublish, admin: boolean): boolean =>
  p.scope === "person" || (p.scope === "org" && admin && publish.org) || (p.scope === "group" && !publish.groups_unverified && publish.groups.includes(p.group_id ?? ""));

export function WallPresetsCard({ admin = false }: { admin?: boolean }) {
  const [state, setState] = React.useState<State>("loading");
  const [list, setList] = React.useState<WallPresetList>();
  const [dialog, setDialog] = React.useState<(PresetDialogMode & { nonce: number }) | null>(null);
  const [deleting, setDeleting] = React.useState<WallPreset | null>(null);
  const [deleteError, setDeleteError] = React.useState("");
  const [open, setOpen] = React.useState<ReadonlySet<string>>(new Set());
  const [saved, setSaved] = React.useState(false);

  const read = React.useCallback(async (): Promise<WallPresetList | undefined> => {
    try {
      // A caller who is no person has no walls and no presets to keep (the admin token, local mode).
      const person = await wallsApi.get().then(() => true, (e) => !(e instanceof HttpError && e.reason === "walls_need_person"));
      if (!person) return void setState("absent");
      const l = await wallPresets.list();
      setList(l);
      setState("ready");
      return l;
    } catch (e) {
      setState(wallPresetsAbsent(e) ? "absent" : "error");
    }
  }, []);
  React.useEffect(() => {
    void read();
  }, [read]);

  if (state === "absent") return null;
  const publish = list?.publish ?? { org: false, groups: [], groups_unverified: false };
  const toggle = (id: string) =>
    setOpen((cur) => {
      const next = new Set(cur);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  const reload = async (id: string) => {
    const l = await read();
    const fresh = l?.presets.find((p) => p.id === id);
    setDialog(fresh ? { kind: "edit", preset: fresh, nonce: Date.now() } : null);
  };
  const remove = async (p: WallPreset) => {
    setDeleteError("");
    try {
      await wallPresets.remove(p.id);
      setDeleting(null);
      setSaved(false);
      void read();
    } catch (e) {
      setDeleteError(e instanceof HttpError ? e.message : W.DELETE_FAILED);
    }
  };

  return (
    <CollapsibleCard title={W.TITLE} summary={W.SUMMARY} testId="wall-presets-card">
      {state === "loading" && <TableSkeleton />}
      {state === "error" && (
        <div className="flex items-center justify-between gap-3">
          <p className="text-sm text-danger">{W.LOAD_FAILED}</p>
          <Button variant="outline" size="sm" onClick={() => void read()}>
            {W.RETRY}
          </Button>
        </div>
      )}
      {state === "ready" && list && (
        <div className="flex flex-col gap-4">
          {sectionsOf(list).map((s) => (
            <section key={s.key} aria-label={s.heading} className="flex flex-col gap-1.5">
              <h4 className="label-eyebrow">{s.heading}</h4>
              <ul className="divide-y divide-border rounded-lg border border-border">
                {s.rows.map((p) => (
                  <li key={p.id} data-testid={`wall-preset-${p.id}`} className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 p-3">
                    <div className="flex min-w-0 flex-1 basis-60 flex-col gap-1.5">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="font-medium">{p.name}</span>
                        {p.scope === "builtin" && <Chip tone="info">{W.BUILT_IN}</Chip>}
                      </div>
                      {p.description && <p className="text-sm text-muted-foreground">{p.description}</p>}
                      <div className="flex flex-wrap gap-1.5">
                        {presetChips(p.spec).map((c) => (
                          <Chip key={c}>{c}</Chip>
                        ))}
                      </div>
                      <p className="text-xs text-muted-foreground">
                        {p.updated_by && p.updated_at ? W.META_SHARED(p.version, p.updated_by, relativeTime(p.updated_at)) : W.META_PLAIN(p.version)}
                      </p>
                      {p.scope === "builtin" && Object.keys(p.spec).length > 0 && (
                        <>
                          <button type="button" aria-expanded={open.has(p.id)} className="flex w-fit items-center gap-1 text-xs font-medium text-primary hover:underline" onClick={() => toggle(p.id)}>
                            {open.has(p.id) ? <ChevronDown className="size-3.5" aria-hidden="true" /> : <ChevronRight className="size-3.5" aria-hidden="true" />}
                            {open.has(p.id) ? W.HIDE : W.SHOW}
                          </button>
                          {open.has(p.id) && <WallPresetDetail spec={p.spec} />}
                        </>
                      )}
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      {mayWrite(p, publish, admin) && (
                        <>
                          <Button size="sm" variant="ghost" onClick={() => setDialog({ kind: "edit", preset: p, nonce: 0 })}>
                            {W.EDIT}
                          </Button>
                          <Button size="sm" variant="ghost" className="text-danger" onClick={() => (setDeleteError(""), setDeleting(p))}>
                            {W.DELETE}
                          </Button>
                        </>
                      )}
                      <Button size="sm" variant="ghost" onClick={() => setDialog({ kind: "copy", preset: p, nonce: 0 })}>
                        {W.COPY_TO_MINE}
                      </Button>
                    </div>
                  </li>
                ))}
              </ul>
            </section>
          ))}
          <div>
            <Button size="sm" onClick={() => setDialog({ kind: "new", nonce: 0 })}>
              {W.NEW_PRESET}
            </Button>
          </div>
          {saved && (
            <p role="status" className="flex items-center gap-2 rounded-md border border-success/25 bg-success-subtle px-3 py-2 text-xs text-success">
              <Check className="size-3.5" aria-hidden="true" />
              {W.SAVED}
            </p>
          )}
        </div>
      )}
      {dialog && (
        <WallPresetDialog
          key={`${dialog.kind}:${"preset" in dialog ? dialog.preset.id : ""}:${dialog.nonce}`}
          mode={dialog}
          publish={publish}
          onClose={() => setDialog(null)}
          onSaved={() => {
            setDialog(null);
            setSaved(true);
            void read();
          }}
          onReload={(id) => void reload(id)}
        />
      )}
      {deleting && (
        <Dialog open onOpenChange={(o) => !o && setDeleting(null)}>
          <DialogContent>
            <DialogTitle>{W.DELETE_TITLE(deleting.name)}</DialogTitle>
            <DialogDescription>{W.DELETE_BODY}</DialogDescription>
            {deleteError && (
              <p role="alert" className="text-sm text-danger">
                {deleteError}
              </p>
            )}
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setDeleting(null)}>
                {W.CANCEL}
              </Button>
              <Button type="button" variant="destructive" onClick={() => void remove(deleting)}>
                {W.DELETE_CONFIRM}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </CollapsibleCard>
  );
}
