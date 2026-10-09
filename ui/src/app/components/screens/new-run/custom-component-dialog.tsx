/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The custom-component dialog (#1914): a person defines a service by the hosts
// it reaches and the secrets it needs, then either uses it for this run only or
// saves it to reuse. It is the one form behind New Run's Add control and Your
// account's saved components.
//
// A refused form names the field and keeps everything typed: nothing here
// resets on a refusal. A saved component's answer lists what it still needs
// (D18) with a way to add it. A person's definition has no plain-HTTP control
// (D6) and no shared secret; the server refuses both anyway.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2, Plus, TriangleAlert, X } from "lucide-react";
import { components as api } from "../../../lib/api/components";
import { HttpError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import type { Component, ComponentRef, ComponentSaved } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { CUSTOM_COMPONENT as T } from "../../wardyn/copy/components";
import { Field } from "../../wardyn/form-primitives";
import { Segmented } from "../../wardyn/segmented";
import {
  blankSecret,
  blankSetting,
  draftOf,
  firstErrorField,
  headerHostChoices,
  hostLines,
  placeServerError,
  requestOf,
  settingField,
  validateDraft,
  type ComponentDraft,
  type FormErrors,
} from "./custom-component-form-model";
import { SecretRow, FieldError, errorProps, secretDomId } from "./custom-component-secret-row";

export interface CustomComponentDialogProps {
  /** A saved row being edited; absent for a new component. */
  component?: Component;
  /** "run": opened from New Run's Add control (may be for this run only).
   *  "saved": opened from Your account, which always saves. */
  context: "run" | "saved";
  /** False when the deployment refuses env and file delivery for every component. */
  residentAllowed: boolean;
  /** Where a person adds a secret of their own. */
  secretsPath: string;
  /** Wraps a link that leaves the page, so unsaved work is asked about first. */
  guardLink?: (to: string) => (e: React.MouseEvent) => void;
  onClose: () => void;
  /** Run context: the attachment to add to the run. */
  onAttach?: (ref: ComponentRef) => void;
  /** A save landed. */
  onSaved?: (saved: ComponentSaved) => void;
}

type Keep = "run" | "save";

const settingId = (s: { key: number }, field: "name" | "value") => `ccd-set-${s.key}-${field}`;

/** The control a field path stands for, so a refusal can move focus to it. */
function domIdOf(field: string, d: ComponentDraft): string | undefined {
  if (field === "name") return "ccd-name";
  if (field === "hosts") return "ccd-hosts";
  const secret = /^secrets\.(\d+)\.(.+)$/.exec(field);
  if (secret) {
    const row = d.secrets.find((s) => s.key === Number(secret[1]));
    return row ? secretDomId(row, secret[2]) : undefined;
  }
  const setting = /^settings\.(\d+)\.(name|value)$/.exec(field);
  if (setting) {
    const row = d.settings.find((s) => s.key === Number(setting[1]));
    return row ? settingId(row, setting[2] as "name" | "value") : undefined;
  }
  return undefined;
}

export function CustomComponentDialog({
  component,
  context,
  residentAllowed,
  secretsPath,
  guardLink,
  onClose,
  onAttach,
  onSaved,
}: CustomComponentDialogProps) {
  const [draft, setDraft] = React.useState<ComponentDraft>(() => draftOf(component ?? null));
  const [keep, setKeep] = React.useState<Keep>(context === "run" ? "run" : "save");
  const [errors, setErrors] = React.useState<FormErrors>({});
  const [saving, setSaving] = React.useState(false);
  const [saved, setSaved] = React.useState<ComponentSaved | null>(null);
  const summaryRef = React.useRef<HTMLDivElement>(null);
  const hostChoices = headerHostChoices(hostLines(draft.hosts));
  const refused = Object.keys(errors).length > 0;

  // Focus the first control that owns a sentence, or the sentence block when none does.
  const focusFirst = (found: FormErrors, d: ComponentDraft) => {
    const field = firstErrorField(found, d);
    const target = field ? domIdOf(field, d) : undefined;
    window.requestAnimationFrame(() => {
      const el = target ? document.getElementById(target) : null;
      (el ?? summaryRef.current)?.focus();
    });
  };
  const refuse = (found: FormErrors) => {
    setErrors(found);
    focusFirst(found, draft);
  };

  const submit = async () => {
    const found = validateDraft(draft, { named: keep === "save", residentAllowed });
    if (Object.keys(found).length > 0) return refuse(found);
    setErrors({});
    const req = requestOf(draft);
    if (keep === "run") {
      onAttach?.({ inline: req.definition, ...(req.name && { name: req.name }) });
      onClose();
      return;
    }
    setSaving(true);
    try {
      const result = component ? await api.updateMine(component.id, req) : await api.saveMine(req);
      onSaved?.(result);
      if (context === "run") onAttach?.({ id: result.id });
      if ((result.requirements ?? []).some((r) => r.status === "missing")) setSaved(result);
      else onClose();
    } catch (e) {
      // A name already in use belongs to the name field; the rest carry their path.
      const message = getErrorMessage(e);
      refuse(e instanceof HttpError && e.reason === "component_name_conflict" ? { name: message } : placeServerError(message, draft));
    } finally {
      setSaving(false);
    }
  };

  const title = component ? T.TITLE_EDIT : T.TITLE_NEW;

  if (saved) {
    const missing = (saved.requirements ?? []).filter((r) => r.status === "missing");
    return (
      <Dialog open onOpenChange={(o) => !o && onClose()}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{T.SAVED(saved.name)}</DialogTitle>
            <DialogDescription>{T.NEEDS_TITLE}</DialogDescription>
          </DialogHeader>
          <ul className="space-y-2" data-testid="custom-component-needs">
            {missing.map((r) => (
              <li key={`${r.kind}:${r.name}`} className="text-sm">
                <div>{T.NEEDS_SECRET(r.name ?? "")}</div>
                {r.fix === "add_secret" && (
                  <Link to={secretsPath} className="text-xs font-medium text-info hover:underline" onClick={guardLink?.(secretsPath)}>
                    {T.NEEDS_ADD}
                  </Link>
                )}
              </li>
            ))}
          </ul>
          <DialogFooter>
            <Button onClick={onClose}>{T.DONE}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !saving && onClose()}>
      <DialogContent className="scroll-thin max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{T.LEAD}</DialogDescription>
        </DialogHeader>

        {refused && (
          <div ref={summaryRef} tabIndex={-1} role="alert" className="space-y-1 rounded-lg border border-danger/30 bg-danger-subtle px-3 py-2 text-sm text-danger outline-none">
            <p className="flex items-start gap-1.5 font-medium">
              <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
              {keep === "run" ? T.REFUSED_RUN : T.REFUSED}
            </p>
            {errors.form && <p>{errors.form}</p>}
            {errors.secrets && <p>{errors.secrets}</p>}
            {errors.settings && <p>{errors.settings}</p>}
          </div>
        )}

        {context === "run" && !component && (
          <Field label={T.KEEP}>
            <Segmented
              value={keep}
              options={[
                { value: "run", label: T.KEEP_RUN },
                { value: "save", label: T.KEEP_SAVE },
              ]}
              onChange={setKeep}
            />
          </Field>
        )}
        {context === "run" && !component && keep === "save" && <p className="-mt-2 text-xs text-muted-foreground">{T.KEEP_SAVE_HINT}</p>}

        <Field label={T.NAME} htmlFor="ccd-name" hint={keep === "save" ? T.NAME_HINT_SAVE : T.NAME_HINT_RUN} required={keep === "save"}>
          <Input
            id="ccd-name"
            autoFocus
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            autoComplete="off"
            {...errorProps("ccd-name", errors.name)}
          />
        </Field>
        <FieldError id="ccd-name" text={errors.name} />

        <Field label={T.HOSTS} htmlFor="ccd-hosts" hint={T.HOSTS_HINT}>
          <Textarea
            id="ccd-hosts"
            rows={3}
            value={draft.hosts}
            onChange={(e) => setDraft({ ...draft, hosts: e.target.value })}
            className="font-mono"
            spellCheck={false}
            {...errorProps("ccd-hosts", errors.hosts)}
          />
        </Field>
        <FieldError id="ccd-hosts" text={errors.hosts} />

        <fieldset className="space-y-3">
          <legend className="text-sm font-medium text-foreground">{T.SECRETS}</legend>
          <p className="text-xs text-muted-foreground">{T.SECRETS_HINT}</p>
          {draft.secrets.map((s, i) => (
            <SecretRow
              key={s.key}
              n={i + 1}
              secret={s}
              hostChoices={hostChoices}
              residentAllowed={residentAllowed}
              errors={errors}
              onChange={(next) => setDraft({ ...draft, secrets: draft.secrets.map((x) => (x.key === s.key ? next : x)) })}
              onRemove={() => setDraft({ ...draft, secrets: draft.secrets.filter((x) => x.key !== s.key) })}
            />
          ))}
          <Button type="button" variant="outline" size="sm" onClick={() => setDraft({ ...draft, secrets: [...draft.secrets, blankSecret()] })}>
            <Plus className="size-3.5" />
            {T.ADD_SECRET}
          </Button>
        </fieldset>

        <fieldset className="space-y-3">
          <legend className="text-sm font-medium text-foreground">{T.SETTINGS}</legend>
          <p className="text-xs text-muted-foreground">{T.SETTINGS_HINT}</p>
          {draft.settings.map((s, i) => (
            <div key={s.key} className="space-y-1">
              <div className="flex items-end gap-2">
                <Field label={T.SETTING_NAME} htmlFor={settingId(s, "name")} className="flex-1">
                  <Input
                    id={settingId(s, "name")}
                    value={s.name}
                    className="font-mono"
                    autoComplete="off"
                    onChange={(e) => setDraft({ ...draft, settings: draft.settings.map((x) => (x.key === s.key ? { ...x, name: e.target.value } : x)) })}
                    {...errorProps(settingId(s, "name"), errors[settingField(s, "name")])}
                  />
                </Field>
                <Field label={T.SETTING_VALUE} htmlFor={settingId(s, "value")} className="flex-1">
                  <Input
                    id={settingId(s, "value")}
                    value={s.value}
                    className="font-mono"
                    autoComplete="off"
                    onChange={(e) => setDraft({ ...draft, settings: draft.settings.map((x) => (x.key === s.key ? { ...x, value: e.target.value } : x)) })}
                    {...errorProps(settingId(s, "value"), errors[settingField(s, "value")])}
                  />
                </Field>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label={T.REMOVE_SETTING(i + 1)}
                  onClick={() => setDraft({ ...draft, settings: draft.settings.filter((x) => x.key !== s.key) })}
                >
                  <X className="size-4" />
                </Button>
              </div>
              <FieldError id={settingId(s, "name")} text={errors[settingField(s, "name")]} />
              <FieldError id={settingId(s, "value")} text={errors[settingField(s, "value")]} />
            </div>
          ))}
          <Button type="button" variant="outline" size="sm" onClick={() => setDraft({ ...draft, settings: [...draft.settings, blankSetting()] })}>
            <Plus className="size-3.5" />
            {T.ADD_SETTING}
          </Button>
        </fieldset>

        <DialogFooter>
          <Button variant="ghost" disabled={saving} onClick={onClose}>
            {T.CANCEL}
          </Button>
          <Button disabled={saving} onClick={() => void submit()}>
            {saving && <Loader2 className="size-3.5 animate-spin" />}
            {keep === "run" ? T.ADD_TO_RUN : T.SAVE}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
