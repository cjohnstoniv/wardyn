/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New, Edit and Copy to mine for a wall preset: a name, a description, who it is for, and the walls form the Your walls
// card uses. The audience is picked from where the caller may publish and never changes on an edit (PUT has no scope).
// A refusal from the server shows under what it names: the name, or the form section of the wire path it starts with.
import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import { WALL_PRESET_REASON, wallPresets } from "../../../lib/api/wall-presets";
import { TEMPLATE_REFUSAL } from "../../../lib/template-refusals";
import type { TemplatePublish } from "../../../lib/types/templates";
import type { WallPreset, WallPresetSaveRequest } from "../../../lib/types/wall-presets";
import { sectionOfRefusal, type FormSection } from "../../../lib/wall-preset-words";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { NR_SAVE } from "../../wardyn/copy/new-run/save";
import { NR_SHARED } from "../../wardyn/copy/new-run/shared";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";
import { formBlocked, toForm, toSpec, WallsForm, type Form } from "./walls-form";

export type PresetDialogMode = { kind: "new" } | { kind: "edit"; preset: WallPreset } | { kind: "copy"; preset: WallPreset };

const SELECT_CLASS =
  "border-input bg-input-background dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring h-9 w-full rounded-md border px-3 text-base outline-none focus-visible:ring-[3px] md:text-sm";

interface Audience {
  value: string;
  scope: "person" | "org" | "group";
  groupId?: string;
  label?: string;
}

/** Only where the caller may publish: themselves, the organisation in the Admin view, and the groups they manage and are verified in. */
function audiencesOf(publish: TemplatePublish): Audience[] {
  const groups = publish.groups_unverified ? [] : publish.groups;
  return [
    { value: "person", scope: "person" },
    ...(publish.org ? [{ value: "org", scope: "org" } as Audience] : []),
    ...groups.map((g): Audience => ({ value: `group:${g}`, scope: "group", groupId: g, label: publish.group_labels?.[g] })),
  ];
}

const audienceName = (a: Audience) => (a.scope === "person" ? W.ONLY_ME : a.scope === "org" ? NR_SAVE.SCOPE_ORG : a.label ? NR_SAVE.SCOPE_GROUP(a.label) : (a.groupId ?? ""));

export function WallPresetDialog({
  mode, publish, onClose, onSaved, onReload,
}: {
  mode: PresetDialogMode;
  publish: TemplatePublish;
  onClose: () => void;
  onSaved: () => void;
  /** Reads the preset again after a version conflict, and reopens the edit on what is stored now. */
  onReload: (id: string) => void;
}) {
  const source = mode.kind === "new" ? undefined : mode.preset;
  const [name, setName] = React.useState(mode.kind === "copy" ? W.COPY_SUFFIX(mode.preset.name) : (source?.name ?? ""));
  const [description, setDescription] = React.useState(source?.description ?? "");
  const [audienceValue, setAudienceValue] = React.useState("person");
  const [form, setForm] = React.useState<Form>(() => toForm(source?.spec ?? {}));
  const [busy, setBusy] = React.useState(false);
  const [nameError, setNameError] = React.useState("");
  const [descriptionError, setDescriptionError] = React.useState("");
  const [sectionErrors, setSectionErrors] = React.useState<Partial<Record<FormSection, string>>>({});
  const [failure, setFailure] = React.useState("");
  const [conflict, setConflict] = React.useState(false);

  const audiences = audiencesOf(publish);
  const audience = audiences.find((a) => a.value === audienceValue) ?? audiences[0];
  const spec = toSpec(form);
  const empty = Object.keys(spec).length === 0;
  const blocked = formBlocked(form);
  const copyOfOpen = mode.kind === "copy" && Object.keys(mode.preset.spec).length === 0;
  const title = mode.kind === "new" ? W.DIALOG_NEW : mode.kind === "edit" ? W.DIALOG_EDIT : W.DIALOG_COPY;
  const helper = publish.groups_unverified ? TEMPLATE_REFUSAL.GROUP_UNVERIFIED() : publish.org ? W.HELP_ORG : audiences.length > 1 ? W.HELP_GROUPS : W.HELP_ONLY_ME;

  const clear = () => {
    setNameError("");
    setDescriptionError("");
    setSectionErrors({});
    setFailure("");
    setConflict(false);
  };
  const edit = (patch: Partial<Form>) => {
    setForm((f) => ({ ...f, ...patch }));
    clear();
  };
  const refuse = (e: unknown) => {
    if (!(e instanceof HttpError)) return setFailure(W.SAVE_FAILED);
    if (e.reason === WALL_PRESET_REASON.NAME_TAKEN) return setNameError(e.message);
    if (e.reason === WALL_PRESET_REASON.VERSION_CONFLICT) {
      setConflict(true);
      return setFailure(e.message);
    }
    if (e.reason === WALL_PRESET_REASON.INVALID) {
      const { section, text } = sectionOfRefusal(e.message);
      if (section === "name") return setNameError(text);
      if (section === "description") return setDescriptionError(text);
      if (section) return setSectionErrors({ [section]: text });
      return setFailure(text);
    }
    setFailure(e.message || W.SAVE_FAILED);
  };
  const submit = async () => {
    setBusy(true);
    clear();
    const body: WallPresetSaveRequest = { name: name.trim(), spec, ...(description.trim() && { description: description.trim() }) };
    try {
      if (mode.kind === "edit") await wallPresets.update(mode.preset.id, { ...body, expected_version: mode.preset.version });
      else {
        await wallPresets.create({ ...body, scope: audience.scope, ...(audience.groupId && { group_id: audience.groupId }) });
      }
      onSaved();
    } catch (e) {
      refuse(e);
      setBusy(false);
    }
  };

  return (
    // A save still in flight keeps its dialog and its form: dismissed, its answer would close a dialog opened since,
    // and an edit made during it would not be in what it saves.
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent className="sm:max-w-xl">
        <form
          className="grid gap-4"
          aria-busy={busy}
          onSubmit={(e) => {
            e.preventDefault();
            if (!empty && !blocked && name.trim() && !busy) void submit();
          }}
        >
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription className="sr-only">{W.SUMMARY}</DialogDescription>
          <div className="space-y-2">
            <div className="flex items-center gap-2">
              <Label htmlFor="wall-preset-name">{W.NAME}</Label>
              <span className="text-xs text-muted-foreground">{NR_SHARED.REQUIRED}</span>
            </div>
            <Input id="wall-preset-name" disabled={busy} value={name} maxLength={128} aria-invalid={nameError !== ""} aria-describedby={nameError ? "wall-preset-name-error" : undefined} onChange={(e) => (setName(e.target.value), clear())} />
            {nameError && (
              <p id="wall-preset-name-error" role="alert" className="text-xs text-danger">
                {nameError}
              </p>
            )}
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-2">
              <Label htmlFor="wall-preset-description">{W.DESCRIPTION}</Label>
              <span className="text-xs text-muted-foreground">{W.OPTIONAL}</span>
            </div>
            <Input id="wall-preset-description" disabled={busy} value={description} placeholder={W.DESCRIPTION_PLACEHOLDER} aria-invalid={descriptionError !== ""} aria-describedby={descriptionError ? "wall-preset-description-error" : undefined} onChange={(e) => (setDescription(e.target.value), clear())} />
            {descriptionError && (
              <p id="wall-preset-description-error" role="alert" className="text-xs text-danger">
                {descriptionError}
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="wall-preset-audience">{W.AUDIENCE}</Label>
            {mode.kind === "edit" ? (
              <>
                <p id="wall-preset-audience" className="text-sm">{audienceName(audienceOfPreset(mode.preset, publish))}</p>
                <p className="text-xs text-muted-foreground">{W.HELP_AUDIENCE_FIXED}</p>
              </>
            ) : (
              <>
                <select id="wall-preset-audience" className={SELECT_CLASS} disabled={busy} value={audience.value} aria-describedby="wall-preset-audience-hint" onChange={(e) => setAudienceValue(e.target.value)}>
                  {audiences.map((a) => (
                    <option key={a.value} value={a.value}>{audienceName(a)}</option>
                  ))}
                </select>
                <p id="wall-preset-audience-hint" className="text-xs text-muted-foreground">{helper}</p>
              </>
            )}
          </div>
          <hr className="border-border" />
          {copyOfOpen && empty && <p className="text-sm text-muted-foreground">{W.COPY_OPEN}</p>}
          <fieldset disabled={busy} className="m-0 min-w-0 border-0 p-0">
            <WallsForm form={form} onEdit={edit} errors={sectionErrors} />
          </fieldset>
          {mode.kind === "edit" && <p className="text-xs text-muted-foreground">{W.EDITING_VERSION(mode.preset.version)}</p>}
          {failure && (
            <p role="alert" className="flex flex-wrap items-center gap-2 text-sm text-danger">
              {failure}
              {conflict && source && (
                <Button type="button" size="sm" variant="outline" onClick={() => onReload(source.id)}>
                  {W.RELOAD}
                </Button>
              )}
            </p>
          )}
          <DialogFooter>
            {empty && !blocked && <p className="text-xs text-muted-foreground sm:mr-auto sm:self-center">{W.NEEDS_A_LIMIT}</p>}
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
              {W.CANCEL}
            </Button>
            <Button type="submit" disabled={empty || blocked || !name.trim() || busy}>
              {W.SAVE}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

const audienceOfPreset = (p: WallPreset, publish: TemplatePublish): Audience =>
  p.scope === "org"
    ? { value: "org", scope: "org" }
    : p.scope === "group"
      ? { value: `group:${p.group_id}`, scope: "group", groupId: p.group_id, label: publish.group_labels?.[p.group_id ?? ""] }
      : { value: "person", scope: "person" };
