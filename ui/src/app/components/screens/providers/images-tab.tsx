/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Images tab (#923, decision 1): one row per image in the base-image
// catalog workspaces already fill, each with its "Available to" (kind image,
// value = the reference), and Add image. An image is admins-only until
// someone is listed (capImage widens), so the control's first choice reads
// Admins only (decision 2). Every string is availability-copy.ts's canon.
//
// Its own resource, like the Agents tab: it reads and writes itself, and its
// one teal button is Add image, so the screen's Save providers is withheld
// while it is open.
import * as React from "react";
import { AlertTriangle, Container, Loader2 } from "lucide-react";
import { baseImages } from "../../../lib/api/base-images";
import { getErrorMessage } from "../../../lib/format";
import { AVAILABILITY, IMAGES } from "../../../lib/availability-copy";
import { ACCESS_STATE, PEOPLE } from "../../../lib/people-access-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import type { BaseImageEntry } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import {
  AvailabilityControl,
  AvailabilityDraft,
  writeAvailability,
  type AvailabilityDraftValue,
} from "../../wardyn/availability-control";
import { makeMono } from "../../wardyn/code-block";
import { Field } from "../../wardyn/form-primitives";
import { EmptyState, TableSkeleton } from "../../wardyn/states";

const withMono = makeMono(["--image"]);

export function ImagesTab() {
  const [rows, setRows] = React.useState<BaseImageEntry[] | null>(null);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [adding, setAdding] = React.useState(false);

  const load = React.useCallback(() => {
    baseImages
      .list()
      .then((list) => {
        setRows(list);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  if (status === "loading") return <TableSkeleton rows={2} cols={2} />;
  if (status === "error" || !rows) {
    return (
      <EmptyState
        icon={AlertTriangle}
        title={PROVIDERS.FETCH_FAILED_TITLE}
        description={PROVIDERS.FETCH_FAILED_BODY}
        action={
          <Button variant="outline" size="sm" onClick={load}>
            {ACCESS_STATE.FETCH_FAILED_RETRY}
          </Button>
        }
      />
    );
  }

  const addButton = <Button onClick={() => setAdding(true)}>{IMAGES.ADD_CTA}</Button>;
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <p className="max-w-[70ch] text-body text-muted-foreground">{withMono(IMAGES.LEAD)}</p>
        {/* One teal per surface: while empty, the empty state's own Add image is it. */}
        {rows.length > 0 && addButton}
      </div>
      {rows.length === 0 ? (
        <EmptyState icon={Container} title={IMAGES.EMPTY_TITLE} description={IMAGES.EMPTY_BODY} action={addButton} />
      ) : (
        rows.map((r) => (
          <div key={r.id} data-testid={`image-row-${r.image}`} className="overflow-hidden rounded-lg border border-border">
            <div className="p-3">
              <div className="text-body font-medium">{r.name}</div>
              <div className="break-all font-mono text-meta text-muted-foreground">{r.image}</div>
            </div>
            <div className="border-t border-border p-3">
              <AvailabilityControl kind="image" value={r.image} adminsOnly note={AVAILABILITY.IMAGE_NOTE} />
            </div>
          </div>
        ))
      )}
      <AddImageDialog open={adding} onOpenChange={setAdding} onAdded={load} />
    </div>
  );
}

const START: AvailabilityDraftValue = { restricted: false, audiences: [] };

// Add image asks who gets it on the form (decision 4), starting at Admins
// only. Add image creates the catalog row, then writes the list, then Only
// these. A refused list write keeps the dialog open on the saved image, which
// stays admins-only meanwhile, so there is no window where it is open to all.
function AddImageDialog({
  open,
  onOpenChange,
  onAdded,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onAdded: () => void;
}) {
  const [ref, setRef] = React.useState("");
  const [draft, setDraft] = React.useState<AvailabilityDraftValue>(START);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [partial, setPartial] = React.useState<{ entry: BaseImageEntry; message: string } | null>(null);
  const { showSpinner } = useDeferredBusy(saving);

  React.useEffect(() => {
    if (!open) {
      setRef("");
      setDraft(START);
      setError(null);
      setPartial(null);
    }
  }, [open]);

  const submit = async () => {
    if (!ref.trim()) return;
    setSaving(true);
    setError(null);
    let entry: BaseImageEntry;
    try {
      entry = await baseImages.add(ref.trim());
    } catch (e) {
      setError(getErrorMessage(e));
      setSaving(false);
      return;
    }
    try {
      await writeAvailability("image", entry.image, draft);
      onOpenChange(false);
    } catch (e) {
      setPartial({ entry, message: getErrorMessage(e) });
    } finally {
      setSaving(false);
      onAdded();
    }
  };

  const cancel = (
    <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
      {PEOPLE.CANCEL}
    </Button>
  );
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg" aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{partial ? partial.entry.name : IMAGES.ADD_CTA}</DialogTitle>
        </DialogHeader>
        {partial ? (
          <div className="space-y-4">
            <div className="flex flex-col gap-1 rounded-lg bg-danger-subtle p-3 text-body text-danger">
              <b className="font-semibold">{AVAILABILITY.CREATE_PARTIAL_TITLE}</b>
              <span>{partial.message}</span>
              <span>{AVAILABILITY.CREATE_PARTIAL_IMAGE}</span>
            </div>
            <div className="border-t border-border pt-4">
              <AvailabilityControl kind="image" value={partial.entry.image} adminsOnly />
            </div>
            <DialogFooter>{cancel}</DialogFooter>
          </div>
        ) : (
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
          >
            <Field label={IMAGES.REF} htmlFor="add-image-ref" hint={IMAGES.REF_HINT} required>
              <Input
                id="add-image-ref"
                required
                autoFocus
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                disabled={saving}
                aria-invalid={error ? true : undefined}
                className="font-mono"
              />
            </Field>
            {error && <p className="text-xs leading-snug text-danger">{error}</p>}
            <div className="border-t border-border pt-4">
              <AvailabilityDraft draft={draft} onChange={setDraft} disabled={saving} adminsOnly />
            </div>
            <DialogFooter>
              {cancel}
              <Button type="submit" disabled={saving || !ref.trim()}>
                {showSpinner && <Loader2 className="size-3.5 animate-spin" />}
                {IMAGES.ADD_CTA}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
