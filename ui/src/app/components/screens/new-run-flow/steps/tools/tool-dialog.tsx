/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The frame the four Add tools and access dialogs share: a title and intro, a
// form whose Enter submits from single-line fields, focus on the first field
// when it opens, Cancel as the quiet way out and one confirming action.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../../../ui/dialog";
import { MY_COMPONENTS } from "../../../../wardyn/copy/components";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";

export interface ToolDialogProps {
  title: string;
  /** Under the title; absent when the dialog's body says it (the MCP dialog's local tab). */
  intro?: string;
  /** Under the title and intro, e.g. the MCP dialog's tabs. */
  header?: React.ReactNode;
  /** The id of the field that takes focus first. */
  firstField: string;
  confirm: string;
  /** Keeps the confirming action shut, e.g. while a save is out. */
  busy?: boolean;
  onConfirm: () => void;
  onClose: () => void;
  /** A control beside Cancel, e.g. More options. */
  extra?: React.ReactNode;
  children: React.ReactNode;
}

export function ToolDialog({ title, intro, header, firstField, confirm, busy, onConfirm, onClose, extra, children }: ToolDialogProps) {
  return (
    <Dialog open onOpenChange={(open) => !open && !busy && onClose()}>
      <DialogContent {...(intro ? {} : { "aria-describedby": undefined })} onOpenAutoFocus={(e) => { e.preventDefault(); document.getElementById(firstField)?.focus(); }}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {intro && <DialogDescription>{intro}</DialogDescription>}
          {header}
        </DialogHeader>
        <form className="flex flex-col gap-4" onSubmit={(e) => { e.preventDefault(); if (!busy) onConfirm(); }}>
          {children}
          <DialogFooter>
            {extra}
            <Button type="button" variant="ghost" disabled={busy} onClick={onClose}>{NR_SHARED.CANCEL}</Button>
            <Button type="submit" disabled={busy}>{confirm}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** A labelled single field with its refusal under it. */
export function ToolField({ id, label, error, children }: { id: string; label: string; error?: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-2">
      <label htmlFor={id} className="text-sm font-medium">{label}</label>
      {children}
      {error && <p id={`${id}-err`} role="alert" className="text-sm text-danger">{error}</p>}
    </div>
  );
}

export const SELECT_CLASS =
  "h-9 w-full rounded-md border border-input bg-input-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring";

/** Saved secret names that could not be read: the answer's own words and a retry, never a quiet empty list. */
export interface NamesFailure {
  message: string;
  retry: () => void;
}

export function NamesFailed({ failure }: { failure?: NamesFailure }) {
  if (!failure) return null;
  return (
    <div role="alert" className="flex flex-wrap items-center gap-2 text-sm text-danger">
      <span>{failure.message}</span>
      <Button type="button" variant="outline" size="sm" onClick={failure.retry}>{MY_COMPONENTS.RETRY}</Button>
    </div>
  );
}
