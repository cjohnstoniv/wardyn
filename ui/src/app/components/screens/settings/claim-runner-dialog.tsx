/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// My runner -> Claim (M-RN-2 MR5). The claim is the security step, so it takes the whole fingerprint from
// the person's own computer (the CLI first, or a paste) and never offers one from this page: the field
// starts empty, and the runner row shows only an abbreviated fingerprint. A mismatch marks the field and
// keeps the dialog open.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { runners as runnersApi } from "../../../lib/api/runners";
import { getErrorMessage } from "../../../lib/format";
import type { RunnerView } from "../../../lib/types/runners";
import { MY_RUNNER, RUNNER } from "../../wardyn/copy/runners";
import { makeMono } from "../../wardyn/code-block";
import { CopyButton } from "../../wardyn/copy-button";
import { Field } from "../../wardyn/form-primitives";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";

const MISMATCH = "runner_claim_mismatch";
const COMMAND = "wardyn runner claim";
const withMono = makeMono([COMMAND]);

export function ClaimRunnerDialog({
  runner,
  onOpenChange,
  onClaimed,
}: {
  /** The runner being claimed; null when the dialog is closed. */
  runner: RunnerView | null;
  onOpenChange: (open: boolean) => void;
  onClaimed: () => void;
}) {
  const [fingerprint, setFingerprint] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [mismatch, setMismatch] = React.useState(false);
  const [error, setError] = React.useState("");
  const id = runner?.id;
  React.useEffect(() => {
    setFingerprint("");
    setMismatch(false);
    setError("");
  }, [id]);
  // The name stays while the dialog closes, so its words do not blank during the exit.
  const last = React.useRef(runner);
  if (runner) last.current = runner;
  const shown = runner ?? last.current;
  if (!shown) return null;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setMismatch(false);
    setError("");
    try {
      await runnersApi.claim(shown.id, fingerprint.trim());
      toast.success(MY_RUNNER.CLAIMED_TOAST(shown.name));
      onOpenChange(false);
      onClaimed();
    } catch (err) {
      if (err instanceof HttpError && err.reason === MISMATCH) setMismatch(true);
      else setError(getErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const errId = "claim-runner-mismatch";
  return (
    <Dialog open={runner !== null} onOpenChange={(o) => !busy && onOpenChange(o)}>
      <DialogContent className="sm:max-w-lg">
        <form onSubmit={submit} className="space-y-3">
          <DialogHeader>
            <DialogTitle>{MY_RUNNER.CLAIM_TITLE(shown.name)}</DialogTitle>
            <DialogDescription>{withMono(MY_RUNNER.CLAIM_LEAD(shown.name))}</DialogDescription>
          </DialogHeader>
          <div className="flex items-center justify-between gap-2 rounded-lg border border-border bg-surface-2/60 p-3">
            <code className="font-mono text-xs">{COMMAND}</code>
            <CopyButton
              text={COMMAND}
              label={RUNNER.COPY_COMMAND}
              className="gap-1.5 rounded-md border border-border bg-card px-2.5 py-1 text-xs text-foreground hover:bg-surface-2"
            >
              {RUNNER.COPY_COMMAND}
            </CopyButton>
          </div>
          <Field label={MY_RUNNER.FIELD_FINGERPRINT} htmlFor="claim-runner-fingerprint" hint={MY_RUNNER.FINGERPRINT_HINT(shown.name)} required>
            <Input
              id="claim-runner-fingerprint"
              autoFocus
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              placeholder="SHA256:"
              value={fingerprint}
              aria-invalid={mismatch || undefined}
              aria-describedby={mismatch ? errId : undefined}
              onChange={(e) => setFingerprint(e.target.value)}
            />
          </Field>
          {mismatch && (
            <p id={errId} className="text-body text-danger">
              {MY_RUNNER.CLAIM_MISMATCH(shown.name)}
            </p>
          )}
          {error && (
            <p role="alert" className="text-body text-danger">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" disabled={busy} onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || fingerprint.trim() === ""}>
              {busy && <Loader2 className="size-3.5 animate-spin" />}
              {MY_RUNNER.CLAIM_SUBMIT}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
