/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Save the New Run draft as a template or a policy-only template. Saving never
// changes the draft: this dialog reads the intent and writes one catalogue row.
// Strings come in through `copy`, the save through `save`, so Review mounts it
// with its own copy module and tests mount it with fixtures.

import * as React from "react";

import { Button } from "../../../ui/button";
import { Checkbox } from "../../../ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../../ui/dialog";
import { Input } from "../../../ui/input";
import { Label } from "../../../ui/label";
import { RadioGroup, RadioGroupItem } from "../../../ui/radio-group";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SAVE } from "../../../wardyn/copy/new-run/save";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { getErrorMessage } from "../../../../lib/format";
import { TEMPLATE_REASON, TEMPLATE_REFUSAL } from "../../../../lib/template-refusals";
import type { Template, TemplateKind, TemplatePublish } from "../../../../lib/types/templates";
import type { RunIntent } from "../draft/run-intent";
import { saveTemplate, TemplateSaveError, type SaveTemplate } from "./save-as-dialog.api";
import {
  buildSaveRequest,
  leaveOutOwnDrives,
  missingDependencies,
  ONLY_ME,
  pickIntent,
  saveAudiences,
  savePartsOf,
  type SavePartId,
} from "./save-as-dialog.model";

export const SAVE_AS_COPY = {
  title: (kind: TemplateKind) => (kind === "template" ? NR_SAVE.TITLE_TEMPLATE : NR_SAVE.TITLE_POLICY),
  intro: (kind: TemplateKind) => NR_SAVE.INTRO(kind),
  nameLabel: NR_SAVE.NAME_LABEL,
  required: NR_SHARED.REQUIRED,
  namePlaceholder: (kind: TemplateKind) => (kind === "template" ? NR_SAVE.NAME_PLACEHOLDER_TEMPLATE : NR_SAVE.NAME_PLACEHOLDER_POLICY),
  availableTo: NR_SAVE.AVAILABLE_LABEL,
  onlyMe: NR_SAVE.SCOPE_ME,
  groupLabelled: (label: string) => NR_SAVE.SCOPE_GROUP(label),
  helperPlain: NR_SAVE.HELP_NO_GRANTS,
  helperManager: NR_SAVE.HELP_MANAGER,
  include: NR_SAVE.INCLUDE_LEGEND,
  everything: NR_SAVE.INCLUDE_ALL,
  subset: NR_SAVE.INCLUDE_SUBSET,
  omittedValues: NR_SAVE.OMITTED,
  partLabels: {
    run: NR_REVIEW.GROUP_RUN, where: NR_REVIEW.GROUP_WHERE, work: NR_REVIEW.GROUP_WORK, tools: NR_REVIEW.GROUP_TOOLS,
    access: NR_REVIEW.GROUP_ACCESS, limits: NR_REVIEW.GROUP_LIMITS,
    network: NR_SAVE.SUBSET_NETWORK, credentials: NR_SAVE.SUBSET_CREDENTIALS, access_rules: NR_SAVE.SUBSET_ACCESS_RULES,
  } as Record<SavePartId, string>,
  disclosure: NR_SAVE.SHARED_DISCLOSURE,
  nothingChanged: NR_REVIEW.NOTHING_CHANGED,
  cancel: NR_SAVE.CANCEL_SAVE,
  save: (kind: TemplateKind) => (kind === "template" ? NR_SAVE.CONFIRM_TEMPLATE : NR_SAVE.CONFIRM_POLICY),
  // TODO(E1d follow-up): the canon has no sentence for these two. Empty renders nothing; the behaviour behind them
  // (Save stays off with nothing checked, own drives are left out of a shared document) does not depend on the text.
  nothingPicked: "",
  ownDrivesLeftOut: "",
};

export type SaveAsCopy = typeof SAVE_AS_COPY;

const NAME_MAX = 128;
const NO_DRIVES: ReadonlySet<string> = new Set();
export const SELECT_CLASS =
  "border-input bg-input-background dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring h-9 w-full rounded-md border px-3 text-base outline-none focus-visible:ring-[3px] md:text-sm";

export interface SaveAsDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  kind: TemplateKind;
  /** The draft's intent as it stands; the dialog never writes to it. */
  intent: RunIntent;
  /** The draft still has blockers: a template of it is saved as partial. */
  hasBlockers: boolean;
  /** `GET /templates` publish block: the audiences the person can write. */
  publish: TemplatePublish;
  /** The saver's own member drives; a shared template leaves them out. */
  ownDriveIds?: ReadonlySet<string>;
  copy?: SaveAsCopy;
  save?: SaveTemplate;
  onSaved: (template: Template) => void;
}

export function SaveAsDialog(props: SaveAsDialogProps) {
  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent
        onOpenAutoFocus={(e) => {
          // Input forwards no ref under React 18, so the first field is found by id.
          e.preventDefault();
          (e.currentTarget as HTMLElement).querySelector<HTMLInputElement>("#save-as-name")?.focus();
        }}
      >
        <SaveAsForm {...props} />
      </DialogContent>
    </Dialog>
  );
}

function SaveAsForm({
  onOpenChange, kind, intent, hasBlockers, publish, ownDriveIds = NO_DRIVES, copy = SAVE_AS_COPY, save = saveTemplate, onSaved,
}: SaveAsDialogProps) {
  const [name, setName] = React.useState("");
  const [audienceValue, setAudienceValue] = React.useState(ONLY_ME);
  const [everything, setEverything] = React.useState(true);
  const [unchecked, setUnchecked] = React.useState<ReadonlySet<SavePartId>>(new Set());
  const [saving, setSaving] = React.useState(false);
  const [nameTaken, setNameTaken] = React.useState("");
  const [failure, setFailure] = React.useState("");

  const audiences = saveAudiences(publish);
  const audience = audiences.find((a) => a.value === audienceValue) ?? audiences[0];
  const shared = audience.scope !== "person";
  const audienceIntent = kind === "template" && shared ? leaveOutOwnDrives(intent, ownDriveIds) : { intent, leftOut: false };
  const { parts, unassigned } = savePartsOf(kind, audienceIntent.intent);
  const nothingToSave = parts.length === 0 && unassigned.length === 0;
  const fields = everything
    ? [...parts.flatMap((p) => p.fields), ...unassigned]
    : parts.filter((p) => !unchecked.has(p.id)).flatMap((p) => p.fields);
  const reason = nothingToSave ? copy.nothingChanged : copy.nothingPicked;
  const canSave = name.trim() !== "" && !nothingToSave && fields.length > 0 && !saving;

  const submit = async () => {
    setSaving(true);
    setNameTaken("");
    setFailure("");
    // Leaving the saver's own drives out makes a shared template partial: it no longer attaches everything.
    const coverage = kind === "template" && everything && !hasBlockers && !audienceIntent.leftOut ? "full" : "partial";
    const request = buildSaveRequest({ kind, audience, name, intent: pickIntent(audienceIntent.intent, fields), coverage });
    try {
      let saved: Template;
      try {
        saved = await save(request);
      } catch (e) {
        // A part left out that another kept part depends on becomes Needs setup, listed by the server's own words.
        const needs = e instanceof TemplateSaveError && e.reason === TEMPLATE_REASON.DEPENDENCY_MISSING && coverage === "partial" ? missingDependencies(e.diagnostics) : [];
        if (needs.length === 0) throw e;
        saved = await save({ ...request, document: { ...request.document, needs_setup: needs } });
      }
      onSaved(saved);
      onOpenChange(false);
    } catch (e) {
      if (e instanceof TemplateSaveError && e.reason === TEMPLATE_REASON.NAME_TAKEN) setNameTaken(e.message);
      else setFailure(getErrorMessage(e));
      setSaving(false);
    }
  };

  const helper = publish.groups_unverified ? "" : audiences.length > 1 ? copy.helperManager : copy.helperPlain;
  return (
    <form
      className="grid gap-4"
      aria-busy={saving}
      onSubmit={(e) => {
        e.preventDefault();
        if (canSave) void submit();
      }}
    >
      <DialogTitle>{copy.title(kind)}</DialogTitle>
      <DialogDescription>{copy.intro(kind)}</DialogDescription>

      <div className="space-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor="save-as-name">{copy.nameLabel}</Label>
          <span className="text-xs text-muted-foreground">{copy.required}</span>
        </div>
        <Input
          id="save-as-name"
          value={name}
          maxLength={NAME_MAX}
          placeholder={copy.namePlaceholder(kind)}
          aria-invalid={nameTaken !== ""}
          aria-describedby={nameTaken ? "save-as-name-error" : undefined}
          onChange={(e) => {
            setName(e.target.value);
            setNameTaken("");
          }}
        />
        {nameTaken && (
          <p id="save-as-name-error" role="alert" className="text-xs text-danger">
            {nameTaken}
          </p>
        )}
      </div>

      <div className="space-y-2">
        <Label htmlFor="save-as-audience">{copy.availableTo}</Label>
        <select
          id="save-as-audience"
          className={SELECT_CLASS}
          value={audience.value}
          aria-describedby="save-as-audience-hint"
          onChange={(e) => setAudienceValue(e.target.value)}
        >
          {audiences.map((a) => (
            <option key={a.value} value={a.value}>
              {a.scope === "person" ? copy.onlyMe : a.label ? copy.groupLabelled(a.label) : a.groupId}
            </option>
          ))}
        </select>
        <p id="save-as-audience-hint" className="text-xs text-muted-foreground">
          {publish.groups_unverified ? TEMPLATE_REFUSAL.GROUP_UNVERIFIED() : helper}
        </p>
      </div>

      <fieldset className="space-y-2">
        <legend className="mb-2 text-sm font-medium">{copy.include}</legend>
        <RadioGroup value={everything ? "all" : "subset"} onValueChange={(v) => setEverything(v === "all")}>
          <div className="flex items-center gap-2">
            <RadioGroupItem id="save-as-all" value="all" />
            <Label htmlFor="save-as-all">{copy.everything}</Label>
          </div>
          <div className="flex items-center gap-2">
            <RadioGroupItem id="save-as-subset" value="subset" />
            <Label htmlFor="save-as-subset">{copy.subset}</Label>
          </div>
        </RadioGroup>
        {!everything && (
          <ul aria-label={copy.subset} className="grid gap-2 pl-6">
            {parts.map((p) => (
              <li key={p.id} className="flex items-center gap-2">
                <Checkbox
                  id={`save-as-part-${p.id}`}
                  checked={!unchecked.has(p.id)}
                  onCheckedChange={(on) => setUnchecked((prev) => { const next = new Set(prev); if (on) next.delete(p.id); else next.add(p.id); return next; })}
                />
                <Label htmlFor={`save-as-part-${p.id}`}>{copy.partLabels[p.id]}</Label>
              </li>
            ))}
          </ul>
        )}
        <p className="text-xs text-muted-foreground">{copy.omittedValues}</p>
        {audienceIntent.leftOut && copy.ownDrivesLeftOut && <p className="text-xs text-muted-foreground">{copy.ownDrivesLeftOut}</p>}
        {(nothingToSave || fields.length === 0) && reason && <p className="text-xs text-muted-foreground">{reason}</p>}
      </fieldset>

      {shared && <p className="text-xs text-muted-foreground">{copy.disclosure}</p>}
      {failure && (
        <p role="alert" className="text-sm text-danger">
          {failure}
        </p>
      )}

      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          {copy.cancel}
        </Button>
        <Button type="submit" disabled={!canSave}>
          {copy.save(kind)}
        </Button>
      </DialogFooter>
    </form>
  );
}
