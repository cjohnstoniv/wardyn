/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The action bar of a long editor: Cancel and Save pinned to the bottom of the scrolling page, with how many
// changes are waiting. Dialogs get the same pinning from DialogFooter; this is for editors that live in the
// page. SectionAnchors is the matching row of jump links at the top of such an editor.
import { Loader2 } from "lucide-react";
import { Button } from "../ui/button";
import { cn } from "../ui/utils";
import { FORM_ACTIONS } from "./copy/form-actions";

/** How many top-level fields of `now` differ from `opened`, for the unsaved-change count. */
export function countChanged(now: object, opened: object): number {
  const a = now as Record<string, unknown>;
  const b = opened as Record<string, unknown>;
  return Object.keys({ ...a, ...b }).filter((k) => JSON.stringify(a[k]) !== JSON.stringify(b[k])).length;
}

export function FormActions({
  changeCount,
  onCancel,
  onSave,
  submit = false,
  className,
  saving = false,
  saveDisabled = false,
  saveLabel = FORM_ACTIONS.SAVE,
  cancelLabel = FORM_ACTIONS.CANCEL,
  describedBy,
}: {
  /** How many changes are waiting; the chip is hidden at 0. */
  changeCount: number;
  onCancel: () => void;
  /** Not needed when `submit` is set: Save then submits the enclosing form. */
  onSave?: () => void;
  submit?: boolean;
  /** Overrides the default full-bleed margins when the bar sits in a narrower column. */
  className?: string;
  saving?: boolean;
  saveDisabled?: boolean;
  saveLabel?: string;
  cancelLabel?: string;
  describedBy?: string;
}) {
  return (
    <div
      data-testid="form-actions"
      className={cn(
        "bg-background sticky bottom-0 z-10 -mx-6 mt-5 flex flex-wrap items-center justify-end gap-2 border-t border-border px-6 py-3",
        className,
      )}
    >
      {/* Always mounted, so the count is announced when it first appears. */}
      <span
        role="status"
        data-testid="form-actions-unsaved"
        className="mr-auto text-body font-medium text-warning"
      >
        {changeCount > 0 ? FORM_ACTIONS.UNSAVED(changeCount) : null}
      </span>
      <Button type="button" variant="ghost" onClick={onCancel} disabled={saving}>
        {cancelLabel}
      </Button>
      <Button type={submit ? "submit" : "button"} onClick={submit ? undefined : onSave} disabled={saveDisabled || saving} aria-describedby={describedBy}>
        {saving ? <Loader2 className="size-4 animate-spin" /> : null}
        {saveLabel}
      </Button>
    </div>
  );
}

/** Jump links to an editor's sections; each target needs a matching `id`. */
export function SectionAnchors({ items }: { items: { id: string; label: string }[] }) {
  return (
    <nav aria-label={FORM_ACTIONS.JUMP_TO} data-testid="section-anchors" className="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-body">
      {items.map((it) => (
        <a key={it.id} href={`#${it.id}`} className="text-primary hover:underline" onClick={(e) => {
          // A hash href would change the console's route; scroll to the section instead.
          e.preventDefault();
          document.getElementById(it.id)?.scrollIntoView({ block: "start" });
        }}>
          {it.label}
        </a>
      ))}
    </nav>
  );
}
