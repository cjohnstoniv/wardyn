/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Add a person" (0.8.6 ppl-p2, mock M12): POST /people, to pre-create someone before their first
// sign-in. The server's 422/409 sentences are shown as sent.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { people as peopleApi } from "../../../lib/api/people";
import { getErrorMessage } from "../../../lib/format";
import { PEOPLE_PAGE as P } from "../../wardyn/copy/people";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Field } from "../../wardyn/form-primitives";

export function AddPersonDialog({ open, onOpenChange, onAdded }: { open: boolean; onOpenChange: (open: boolean) => void; onAdded: () => void }) {
  const [principal, setPrincipal] = React.useState("");
  const [email, setEmail] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");

  React.useEffect(() => {
    if (open) {
      setPrincipal("");
      setEmail("");
      setError("");
    }
  }, [open]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await peopleApi.create(principal.trim(), email.trim());
      toast.success(`${P.ADD}: ${email.trim() || principal.trim()}`);
      onOpenChange(false);
      onAdded();
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !busy && onOpenChange(o)}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{P.ADD}</DialogTitle>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-3">
          <p className="text-body text-muted-foreground">{P.ADD_HINT}</p>
          <Field label="Subject" htmlFor="add-person-principal">
            <Input id="add-person-principal" autoFocus autoComplete="off" value={principal} onChange={(e) => setPrincipal(e.target.value)} />
          </Field>
          <Field label="Email" htmlFor="add-person-email">
            <Input id="add-person-email" type="email" autoComplete="off" value={email} onChange={(e) => setEmail(e.target.value)} />
          </Field>
          {error && (
            <p role="alert" className="text-body text-danger">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" disabled={busy} onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || principal.trim() === ""}>
              {busy && <Loader2 className="size-3.5 animate-spin" />}
              {P.ADD}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
