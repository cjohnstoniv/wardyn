/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The drive editor — the in-place form the /drives screen opens under its
// drives table (docs/design/user-drives-mock/index.html, state 3).
//
// Three things here are decisions, not styling:
//
//  1. IT OFFERS ONLY THIS RUNNER'S TWO BACKENDS (Q3). A backend must match the
//     deployment's runner, so the picker never shows a pair whose every save
//     would meet a 400; on Docker with no WARDYN_USER_DRIVE_HOST_ROOTS the
//     `host_path` option is DISABLED WITH ITS REASON rather than offered and
//     refused. The 400 stays on the API path, where `wardyn drive apply` will
//     meet it.
//  2. A SHARE NAMES ITS OWN HOMES. The derived (`hash`) directory option is
//     disabled for share backends and HOME_HINT says why — the same refusal the
//     server raises, avoided rather than met.
//  3. THE SERVER COMPOSES ITS OWN REFUSALS. The roots are an env-borne ceiling
//     the console cannot read, so a host-root refusal is POST-ATTEMPT: the
//     console contributes SAVE_REFUSED_TITLE and the body is the server's text,
//     verbatim. SAVE_ERROR is the other failure — no answer at all.
//
// Every product string comes from user-drives-copy.ts. This file adds none.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import {
  backendsFor,
  drives as api,
  isManagedBackend,
  type DriveBackend,
  type DriveReclaim,
  type HomeTemplate,
  type UserDrive,
  type UserDriveInput,
} from "../../../lib/api/drives";
import { DRIVES, PEOPLE } from "../../../lib/user-drives-copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Label } from "../../ui/label";
import { Field, OptionCard, Switch } from "../../wardyn/form-primitives";
import { Note, withMono } from "./display";

// The three templates in admin-surface order (types.HomeTemplates). There is no
// whole-email option: an address carries an "@", which no directory segment can
// hold, so it would validate and then refuse every real caller.
// The four long select-option labels — this editor is the ONLY place they
// render; a table cell shows the kind chip over the backend's wire value
// instead. It lived in display.tsx until this stayed its one consumer.
const BACKEND_LABEL: Record<DriveBackend, string> = {
  docker_volume: DRIVES.BACKEND_DOCKER_VOLUME,
  host_path: DRIVES.BACKEND_HOST_PATH,
  k8s_pvc: DRIVES.BACKEND_K8S_PVC,
  k8s_pvc_static: DRIVES.BACKEND_K8S_PVC_STATIC,
};

const HOME_OPTIONS: { value: HomeTemplate; label: string; hint: string }[] = [
  { value: "hash", label: DRIVES.HOME_HASH, hint: DRIVES.HOME_HASH_HINT },
  { value: "sub", label: DRIVES.HOME_SUB, hint: DRIVES.HOME_SUB_HINT },
  { value: "email_local", label: DRIVES.HOME_EMAIL_LOCAL, hint: DRIVES.HOME_EMAIL_LOCAL_HINT },
];

const RECLAIM_OPTIONS: { value: DriveReclaim; label: string }[] = [
  { value: "retain", label: DRIVES.RECLAIM_RETAIN },
  { value: "delete", label: DRIVES.RECLAIM_DELETE },
];

export function DriveEditor({
  drive,
  runnerTarget,
  hostRootsConfigured,
  disabled,
  onCancel,
  onSaved,
}: {
  /** The drive being edited, or null for a new one. */
  drive: UserDrive | null;
  /** GET /drives's runner_target — which two backends this deployment can mount. */
  runnerTarget: string;
  /** GET /drives's host_roots_configured — whether a host path may back a drive at all. */
  hostRootsConfigured: boolean;
  disabled: boolean;
  onCancel: () => void;
  onSaved: () => void;
}) {
  const offered = backendsFor(runnerTarget);
  const [name, setName] = React.useState(drive?.name ?? "");
  const [backend, setBackend] = React.useState<DriveBackend>(drive?.backend ?? offered[0] ?? "docker_volume");
  const [hostRoot, setHostRoot] = React.useState(drive?.host_root ?? "");
  const [storageClass, setStorageClass] = React.useState(drive?.storage_class ?? "");
  const [home, setHome] = React.useState<HomeTemplate>(drive?.home_template ?? "hash");
  const [size, setSize] = React.useState(String(drive?.size_mib ?? 0));
  const [writable, setWritable] = React.useState(!!drive?.writable);
  const [reclaim, setReclaim] = React.useState<DriveReclaim>(drive?.reclaim ?? "retain");
  const [saving, setSaving] = React.useState(false);
  // `title` is set for a refusal the SERVER composed; a transport failure has no
  // server text at all and renders SAVE_ERROR alone.
  const [error, setError] = React.useState<{ title?: string; message: string } | null>(null);

  const managed = isManagedBackend(backend);
  // The kinds take OPPOSITE halves of the template list, and both halves are
  // refused on the API path. A share's directories are named by the
  // corporation's own directory, so the derived id cannot name one. A managed
  // drive is the reverse: Wardyn mints the volume, and a claim-derived name is
  // not unique across email domains (alice@corp and alice@partner would share
  // one), so only the derived id is safe there — a readable directory for one
  // person is that person's own directory-name override on their allocation.
  const homeDisabled = (t: HomeTemplate) => (t === "hash") !== managed;
  // Switching kinds while the other half's template is chosen would author
  // exactly the row the server refuses, so the choice moves with the backend
  // rather than waiting to be refused.
  const pickBackend = (b: DriveBackend) => {
    setBackend(b);
    if (!isManagedBackend(b) && home === "hash") setHome("sub");
    if (isManagedBackend(b) && home !== "hash") setHome("hash");
  };

  const save = async () => {
    setSaving(true);
    setError(null);
    const input: UserDriveInput = {
      name: name.trim(),
      backend,
      host_root: backend === "host_path" ? hostRoot.trim() : "",
      storage_class: backend === "k8s_pvc" ? storageClass.trim() : "",
      home_template: home,
      size_mib: Number(size) || 0,
      writable,
      reclaim,
    };
    try {
      if (drive) await api.updateDrive(drive.id, input);
      else await api.createDrive(input);
      onSaved();
    } catch (e) {
      // The server names the path, the roots, the prefix, the backend and the
      // runner — all of which a frozen sentence would have had to drop. The
      // console contributes only the heading.
      setError(
        e instanceof HttpError
          ? { title: DRIVES.SAVE_REFUSED_TITLE, message: e.message }
          : { message: DRIVES.SAVE_ERROR },
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="border-t border-border px-6 py-5" data-testid="drives-drive-editor">
      <h3 className="text-sm font-medium text-foreground">
        {drive ? DRIVES.EDITOR_TITLE_EDIT(drive.name) : DRIVES.EDITOR_TITLE_NEW}
      </h3>

      <div className="mt-4 grid gap-5 md:grid-cols-2">
        <Field label={DRIVES.FIELD_NAME} htmlFor="drive-name" hint={DRIVES.NAME_HINT} required>
          <Input
            id="drive-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={disabled}
            autoComplete="off"
            required
          />
        </Field>

        <Field label={DRIVES.FIELD_BACKEND} hint={DRIVES.BACKEND_HINT}>
          <div className="grid gap-2">
            {offered.map((b) => {
              // Docker with no roots set: offered-and-explained, never
              // offered-and-refused. The reason IS the option's hint.
              const off = b === "host_path" && !hostRootsConfigured;
              return (
                <OptionCard
                  key={b}
                  selected={backend === b}
                  disabled={disabled || off}
                  onClick={() => pickBackend(b)}
                  title={BACKEND_LABEL[b]}
                  hint={off ? withMono(DRIVES.BACKEND_UNAVAILABLE_DOCKER_ROOTS) : undefined}
                />
              );
            })}
          </div>
        </Field>

        {backend === "host_path" && (
          <Field
            label={DRIVES.FIELD_HOST_ROOT}
            htmlFor="drive-host-root"
            hint={withMono(DRIVES.HOST_ROOT_HINT)}
          >
            <Input
              id="drive-host-root"
              value={hostRoot}
              onChange={(e) => setHostRoot(e.target.value)}
              disabled={disabled}
              className="font-mono"
              autoComplete="off"
            />
          </Field>
        )}

        {backend === "k8s_pvc" && (
          <Field label={DRIVES.FIELD_STORAGE_CLASS} htmlFor="drive-storage-class" hint={DRIVES.STORAGE_CLASS_HINT}>
            <Input
              id="drive-storage-class"
              value={storageClass}
              onChange={(e) => setStorageClass(e.target.value)}
              disabled={disabled}
              className="font-mono"
              autoComplete="off"
            />
          </Field>
        )}

        <Field
          label={DRIVES.FIELD_HOME}
          hint={
            <>
              <span className="block">{DRIVES.HOME_HINT}</span>
              {/* HOME_RULE renders under the field for EVERY option: the rule is
                  what a claim is measured against whichever template names it. */}
              <span className="mt-1 block">{withMono(DRIVES.HOME_RULE)}</span>
            </>
          }
        >
          <div className="grid gap-2">
            {HOME_OPTIONS.map((o) => (
              <OptionCard
                key={o.value}
                selected={home === o.value}
                disabled={disabled || homeDisabled(o.value)}
                onClick={() => setHome(o.value)}
                title={o.label}
                hint={o.hint}
              />
            ))}
          </div>
        </Field>

        <Field
          label={DRIVES.FIELD_SIZE}
          htmlFor="drive-size"
          // Q7: a k8s_pvc claim cannot request zero, and there is no
          // required-marker glyph — the hint carries the word.
          hint={backend === "k8s_pvc" ? DRIVES.SIZE_HINT_REQUIRED : DRIVES.SIZE_HINT}
        >
          <Input
            id="drive-size"
            type="number"
            min={0}
            value={size}
            onChange={(e) => setSize(e.target.value)}
            disabled={disabled}
            className="font-mono"
          />
        </Field>

        <Field label={DRIVES.FIELD_WRITABLE} hint={DRIVES.WRITABLE_HINT}>
          <Switch
            checked={writable}
            onChange={setWritable}
            disabled={disabled}
            label={DRIVES.FIELD_WRITABLE}
          />
        </Field>

        <Field label={DRIVES.FIELD_RECLAIM} hint={DRIVES.RECLAIM_HINT}>
          <RadioGroup
            value={reclaim}
            onValueChange={(v) => setReclaim(v as DriveReclaim)}
            disabled={disabled}
            className="flex flex-col gap-1.5"
          >
            {RECLAIM_OPTIONS.map((o) => (
              <span key={o.value} className="flex items-center gap-2">
                <RadioGroupItem value={o.value} id={`drive-reclaim-${o.value}`} />
                <Label htmlFor={`drive-reclaim-${o.value}`} className="cursor-pointer font-normal">
                  {o.label}
                </Label>
              </span>
            ))}
          </RadioGroup>
        </Field>
      </div>

      {error && (
        <Note tone="red" role="alert">
          {error.title && <b className="font-semibold">{error.title}</b>}
          {/* The server's own prose, verbatim and PLAIN. It is a sentence that
              happens to quote a path, an env var and a wire value — monoing the
              whole of it says the sentence is a literal, which it is not, and
              the terms inside it are already set off by the server's own
              quotes and parentheses. */}
          <span>{error.message}</span>
        </Note>
      )}

      <div className="mt-5 flex justify-end gap-2">
        <Button variant="ghost" onClick={onCancel} disabled={saving}>
          {PEOPLE.CANCEL}
        </Button>
        {/* The screen's ONE `default` button while the editor is open — the
            allocation form below collapses and takes its teal with it. */}
        <Button onClick={save} disabled={disabled || saving || !name.trim()}>
          {saving ? <Loader2 className="size-4 animate-spin" /> : null}
          {DRIVES.SAVE_CTA}
        </Button>
      </div>
    </div>
  );
}
