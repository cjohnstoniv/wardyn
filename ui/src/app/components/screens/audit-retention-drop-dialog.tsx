/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The drop dialog (mock packet M4, surface B), opened by an eligible
// partition's Drop. The operator exports the partition, keeps the archive and
// pastes its footer digest; the server recomputes the digest and refuses a
// mismatch. A refusal shows its sentence under the field and the dialog stays
// open; a success switches to the result lines, like the erase dialog.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import type { AuditRetentionDrop, AuditRetentionPartition } from "../../lib/types";
import { audit as api, type PartitionExportForm } from "../../lib/api/audit";
import { HttpError } from "../../lib/api/core";
import { getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Field } from "../wardyn/form-primitives";
import { DROP, reasonKey } from "../wardyn/copy/audit-retention";
import { ERASE } from "../wardyn/copy/credentials";
import { downloadPartition, legacyDate, monthLabel } from "./audit-retention-util";

export function AuditRetentionDropDialog({
  partition,
  cutover,
  onOpenChange,
  onDropped,
}: {
  partition: AuditRetentionPartition | null;
  cutover: string;
  onOpenChange: (open: boolean) => void;
  onDropped: () => void;
}) {
  const [digest, setDigest] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [exporting, setExporting] = React.useState<PartitionExportForm | null>(null);
  const [error, setError] = React.useState("");
  const [result, setResult] = React.useState<AuditRetentionDrop | null>(null);

  React.useEffect(() => {
    if (partition) {
      setDigest("");
      setError("");
      setResult(null);
    }
  }, [partition]);

  if (!partition) return null;

  const title = partition.lo
    ? DROP.TITLE(monthLabel(partition.lo))
    : DROP.TITLE_LEGACY(legacyDate(partition, cutover));
  const rows = partition.rows.toLocaleString("en-US");

  const doExport = (form: PartitionExportForm) => {
    setExporting(form);
    downloadPartition(partition.name, form)
      .catch((e) => toast.error(getErrorMessage(e)))
      .finally(() => setExporting(null));
  };

  const doDrop = async () => {
    setBusy(true);
    setError("");
    try {
      setResult(await api.dropPartition(partition.name, digest.trim()));
    } catch (e) {
      // A >=500 may have partly run, so it gets its own sentence; a 4xx
      // refusal names its rule, and anything unmapped shows the server's
      // sentence as sent.
      if (e instanceof HttpError && e.status >= 500) setError(DROP.FAILED);
      else if (e instanceof HttpError && DROP.REFUSED[reasonKey(e.reason)]) setError(DROP.REFUSED[reasonKey(e.reason)]);
      else setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const close = (dropped: boolean) => {
    onOpenChange(false);
    if (dropped) onDropped();
  };

  return (
    <Dialog open onOpenChange={(o) => !o && close(!!result)}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>

        {result ? (
          <>
            <p className="text-body text-foreground">{DROP.DONE(result.rows.toLocaleString("en-US"))}</p>
            <p className="text-body text-muted-foreground">{DROP.DONE_AUDIT}</p>
            <DialogFooter>
              <Button variant="ghost" onClick={() => close(true)}>
                {ERASE.CLOSE}
              </Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <p className="text-body text-muted-foreground">{DROP.BODY(rows)}</p>
            <div className="flex flex-wrap gap-2">
              <Button variant="outline" size="sm" disabled={exporting !== null} onClick={() => doExport("readable")}>
                {exporting === "readable" && <Loader2 className="size-3.5 animate-spin" />}
                {DROP.READABLE}
              </Button>
              <Button variant="outline" size="sm" disabled={exporting !== null} onClick={() => doExport("raw")}>
                {exporting === "raw" && <Loader2 className="size-3.5 animate-spin" />}
                {DROP.RAW}
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">{DROP.RAW_HINT}</p>
            <Field label={DROP.DIGEST} htmlFor="drop-digest" hint={DROP.DIGEST_HINT}>
              <Input
                id="drop-digest"
                autoFocus
                autoComplete="off"
                spellCheck={false}
                className="font-mono"
                value={digest}
                onChange={(e) => setDigest(e.target.value)}
              />
            </Field>
            {error && (
              <p role="alert" className="text-body text-danger">
                {error}
              </p>
            )}
            <DialogFooter>
              <Button variant="ghost" disabled={busy} onClick={() => close(false)}>
                {ERASE.CANCEL}
              </Button>
              <Button variant="destructive" disabled={busy || digest.trim() === ""} onClick={doDrop}>
                {busy && <Loader2 className="size-3.5 animate-spin" />}
                {DROP.CONFIRM}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
