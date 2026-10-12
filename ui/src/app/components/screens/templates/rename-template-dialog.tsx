/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Rename saves a new revision with the same document. The document is read at the revision the person
// saw and sent back with that revision as expected_revision, so a template that moved on is refused
// (template_revision_conflict). A refusal keeps the typed name and the dialog, so the edit is applied
// again, not retyped, once the list has reloaded.
import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import { TEMPLATE_REASON, TEMPLATE_REFUSAL } from "../../../lib/template-refusals";
import type { TemplateSummary } from "../../../lib/types/templates";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { NR_TEMPLATES as T } from "../../wardyn/copy/new-run/templates";
import { Field } from "../../wardyn/form-primitives";
import { nameTaken } from "./template-display";
import { templatesApi } from "./templates-api";

// The server's limit on a template name, in characters.
const NAME_MAX = 128;

export function RenameTemplateDialog({
  template,
  rows,
  onClose,
  onRenamed,
  onStale,
}: {
  template: TemplateSummary;
  /** Every listed template, for the same-audience name clash. */
  rows: readonly TemplateSummary[];
  onClose: () => void;
  onRenamed: (name: string) => void;
  /** The template changed or went away under this dialog: reload the list, keep the dialog and its edit. */
  onStale: () => void;
}) {
  const [name, setName] = React.useState(template.name);
  const [error, setError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  const inFlight = React.useRef(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (inFlight.current) return;
    const next = name.trim();
    if (!next) return setError(T.NAME_EMPTY);
    if (next === template.name) return onClose();
    if (nameTaken(rows, template, next)) return setError(TEMPLATE_REFUSAL.NAME_TAKEN());
    inFlight.current = true;
    setBusy(true);
    setError(null);
    try {
      const seen = await templatesApi.getRevision(template.id, template.revision);
      // A document that carries its own name must agree with the request's, or the server refuses the save.
      const document = "name" in seen.document ? { ...seen.document, name: next } : seen.document;
      const saved = await templatesApi.update(template.id, {
        scope: template.scope,
        group_id: template.group_id,
        kind: template.kind,
        name: next,
        description: seen.description,
        document,
        expected_revision: template.revision,
      });
      onRenamed(saved.name);
    } catch (err) {
      setError(getErrorMessage(err));
      if (err instanceof HttpError && (err.reason === TEMPLATE_REASON.REVISION_CONFLICT || err.reason === TEMPLATE_REASON.NOT_FOUND)) onStale();
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>{T.RENAME_TITLE}</DialogTitle>
            <DialogDescription>{T.RENAME_BODY}</DialogDescription>
          </DialogHeader>
          <Field label={T.NAME_LABEL} htmlFor="rename-template-name">
            <Input
              id="rename-template-name"
              value={name}
              maxLength={NAME_MAX}
              aria-invalid={error ? true : undefined}
              aria-describedby={error ? "rename-template-error" : undefined}
              onChange={(e) => setName(e.target.value)}
            />
            {error && (
              <p id="rename-template-error" role="alert" className="text-xs text-danger">
                {error}
              </p>
            )}
          </Field>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onClose}>
              {T.CANCEL}
            </Button>
            <Button type="submit" disabled={busy}>
              {T.RENAME_CONFIRM}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
