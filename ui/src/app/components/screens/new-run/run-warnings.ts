/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { toast } from "sonner";
import type { CreateRunResult } from "../../../lib/types";

// surfaceRunWarnings — after a run is created, POST /runs may return an advisory
// `warnings[]` (e.g. the run's workspace directory collides with another active
// run's). The run STILL launched, so we never block on these — we raise one
// non-blocking sonner warning toast per message. Shared by every create path
// (the manual wizard + the composer review) so the behaviour is identical.
export function surfaceRunWarnings(created: CreateRunResult): void {
  const warnings = created.warnings ?? [];
  for (const w of warnings) {
    toast.warning("Run launched with a warning", { description: w });
  }
}

// parseMissingSecret — extract the secret name from a create-run failure that
// named a not-yet-stored secret (inline_policy.go's validateInlineSecretRefs:
// `... unknown secret "name" ...`, now also hit by the stored/default policy
// path — H1). Shared by the composer review panel (compose-review.tsx) and the
// manual wizard (wizard.tsx) so both launch paths offer the same one-click
// "add it and retry" fix instead of each re-deriving the name from the string.
export function parseMissingSecret(launchError?: string | null): string | null {
  return launchError?.match(/unknown secret "([^"]+)"/)?.[1] ?? null;
}

// useAddSecretFix — the shared "add a missing secret and recover" wiring behind the
// launch-error banner. Both the manual wizard and the composer review panel offer
// the same affordance: open AddSecretDialog prefilled with a name, and on save
// either RETRY the launch (the fix flow) or apply the saved name (the manual flow).
// This owns {open, name, retrying} + the two openers + the dialog's onSaved branch,
// so each caller only supplies its two behaviours (onManual / onRetry) instead of
// re-deriving the whole retry-vs-manual dance. The preflight checklist is the third
// intended consumer.
//
// Returns dialogProps to spread onto AddSecretDialog; callers add existingNames
// themselves when they have it (the wizard does; the composer panel doesn't).
export function useAddSecretFix(opts: {
  // A MANUAL save (openManual path): the UNIVERSAL part every opener needs
  // (reload the secret list, refresh whatever readiness check depends on it) —
  // NEVER a specific WizardState field (UI-RUN-1: this used to also apply the
  // saved name to llmSecretName unconditionally, so the Git-PAT card's own
  // "Add secret" — and any non-llm_access checklist row's — silently composed
  // an unrelated api_key grant to api.anthropic.com). A field-specific patch
  // belongs to the PER-CALL `onSet` below, chosen by whichever control opened
  // the dialog.
  onManual: (name: string) => void;
  // A FIX save (openFix path): the missing secret now exists — re-run the launch.
  onRetry: (name: string) => void;
}): {
  // `onSet`, when given, fires ONLY on a manual save (never a retry) — the
  // opening control's own per-call-site setter (e.g. StepAccess's Git-PAT card
  // sets gitPatSecretName; StepReview's llm_access-kind checklist row sets
  // llmSecretName; every other opener passes none, so a save just makes the
  // ALREADY-referenced name exist without touching any WizardState field).
  openManual: (name?: string, onSet?: (name: string) => void) => void;
  openFix: (name: string) => void;
  dialogProps: {
    open: boolean;
    onOpenChange: (o: boolean) => void;
    initialName: string;
    onSaved: (name: string) => void;
  };
} {
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState("");
  const [retrying, setRetrying] = React.useState(false);
  // Bridges click time (openManual, which knows the per-call setter) to save
  // time (onSaved, fired later by the dialog) — a plain closure var would go
  // stale across that gap; a ref survives it. Always set (or cleared) on
  // every open, so a caller that omits `onSet` never inherits a PRIOR call's.
  const onSetRef = React.useRef<((name: string) => void) | undefined>(undefined);

  const openManual = React.useCallback((n = "", onSet?: (name: string) => void) => {
    setName(n);
    setRetrying(false);
    onSetRef.current = onSet;
    setOpen(true);
  }, []);
  const openFix = React.useCallback((n: string) => {
    setName(n);
    setRetrying(true);
    onSetRef.current = undefined;
    setOpen(true);
  }, []);

  // onSaved is rebuilt each render, so it always reads the latest `retrying` and
  // the latest opts callbacks — no stale-closure branch.
  const onSaved = (saved: string) => {
    setOpen(false);
    if (retrying) {
      setRetrying(false);
      opts.onRetry(saved);
    } else {
      opts.onManual(saved);
      onSetRef.current?.(saved);
    }
  };

  return {
    openManual,
    openFix,
    dialogProps: { open, onOpenChange: setOpen, initialName: name, onSaved },
  };
}
