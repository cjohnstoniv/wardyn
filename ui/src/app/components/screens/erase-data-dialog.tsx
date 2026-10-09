/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Erase someone's data (mock packet M4, surface C): the security tier's
// by-scope erase of a person, POST /people/{principal}/erasure. It sits beside
// the shipped credentials-only erase and leaves it untouched. By email, so the
// rule is the shipped by-email one: no second type-to-confirm. Everything is
// ticked when it opens except Recordings, which stays opt-in.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { credentials as credentialsApi, ErasureIncomplete, type ErasureScope } from "../../lib/api/credentials";
import { HttpError } from "../../lib/api/core";
import { getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Input } from "../ui/input";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Field } from "../wardyn/form-primitives";
import { Chip, SectionLabel } from "../wardyn/primitives";
import { COMPONENTS_ADMIN } from "../wardyn/copy/components-admin";
import { ERASE, ERASE_DATA } from "../wardyn/copy/credentials";

// The order the scopes are listed in (the order they run in is the server's).
const SCOPES: ErasureScope[] = [
  "credentials",
  "audit_personal_fields",
  "run_tasks",
  "run_outputs",
  "components",
  "mask_copies",
  "recordings",
];

// The saved-components scope's words live with the rest of the component copy (lazy).
const scopeCopy = (s: ErasureScope) =>
  s === "components"
    ? { label: COMPONENTS_ADMIN.ERASE_SCOPE_LABEL, hint: COMPONENTS_ADMIN.ERASE_SCOPE_HINT }
    : ERASE_DATA.SCOPE[s];

const DEFAULT_SCOPES = new Set<ErasureScope>(SCOPES.filter((s) => s !== "recordings"));

type Result = { person: string; erased: Set<string>; chosen: ErasureScope[]; complete: boolean };

export function EraseDataDialog({
  open,
  retentionLine,
  onOpenChange,
  onErased,
}: {
  open: boolean;
  /** The deployment's backup/retention sentence for stored credentials (ERASE.RETENTION_*). */
  retentionLine: string;
  onOpenChange: (open: boolean) => void;
  onErased: () => void;
}) {
  const [person, setPerson] = React.useState("");
  const [scopes, setScopes] = React.useState<Set<ErasureScope>>(DEFAULT_SCOPES);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [result, setResult] = React.useState<Result | null>(null);

  React.useEffect(() => {
    if (open) {
      setPerson("");
      setScopes(new Set(DEFAULT_SCOPES));
      setError("");
      setResult(null);
    }
  }, [open]);

  if (!open) return null;
  const label = person.trim();
  const canConfirm = label.length > 0 && scopes.size > 0;

  const toggle = (s: ErasureScope, on: boolean) =>
    setScopes((cur) => {
      const next = new Set(cur);
      if (on) next.add(s);
      else next.delete(s);
      return next;
    });

  const doErase = async () => {
    const chosen = SCOPES.filter((s) => scopes.has(s));
    setBusy(true);
    setError("");
    try {
      await credentialsApi.erasePerson(label, chosen);
      setResult({ person: label, erased: new Set(chosen), chosen, complete: true });
    } catch (e) {
      if (e instanceof ErasureIncomplete) {
        setResult({ person: label, erased: new Set(e.done), chosen, complete: false });
      } else {
        // A >=500 never finished, so it may have partly landed: its own
        // sentence. A 4xx refusal (self-erasure, an unknown scope, the
        // operator namespace) shows the server's sentence as sent.
        setError(e instanceof HttpError && e.status >= 500 ? ERASE_DATA.FAILED(label) : getErrorMessage(e));
      }
    } finally {
      setBusy(false);
    }
  };

  const close = (touched: boolean) => {
    onOpenChange(false);
    if (touched) onErased();
  };

  return (
    <Dialog open onOpenChange={(o) => !o && close(!!result)}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{ERASE_DATA.BY_EMAIL_TITLE}</DialogTitle>
        </DialogHeader>

        {result ? (
          <>
            <ul className="space-y-2">
              {result.chosen.map((s) => (
                <li key={s} className="flex items-center justify-between gap-3 text-sm text-foreground">
                  <span>{scopeCopy(s).label}</span>
                  {result.erased.has(s) ? (
                    <Chip tone="success">{ERASE_DATA.ERASED}</Chip>
                  ) : (
                    <Chip tone="danger">{ERASE_DATA.NOT_DONE}</Chip>
                  )}
                </li>
              ))}
            </ul>
            <p className="text-body text-foreground">
              {result.complete ? ERASE_DATA.DONE(result.person) : ERASE_DATA.PARTIAL(result.person)}
            </p>
            <p className="text-body text-muted-foreground">{ERASE_DATA.DONE_AUDIT}</p>
            <DialogFooter>
              <Button variant="ghost" onClick={() => close(true)}>
                {ERASE.CLOSE}
              </Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <Field label={ERASE.FIELD} htmlFor="erase-data-person" hint={ERASE_DATA.HINT}>
              <Input
                id="erase-data-person"
                autoFocus
                autoComplete="off"
                value={person}
                onChange={(e) => setPerson(e.target.value)}
              />
            </Field>
            <fieldset className="space-y-2">
              <legend>
                <SectionLabel>{ERASE_DATA.SCOPES}</SectionLabel>
              </legend>
              {SCOPES.map((s) => (
                <div key={s} className="flex items-start gap-2.5">
                  <Checkbox
                    id={`erase-scope-${s}`}
                    className="mt-0.5"
                    checked={scopes.has(s)}
                    onCheckedChange={(v) => toggle(s, v === true)}
                  />
                  <label htmlFor={`erase-scope-${s}`} className="cursor-pointer">
                    <span className="block text-sm text-foreground">{scopeCopy(s).label}</span>
                    <span className="block text-xs text-muted-foreground">{scopeCopy(s).hint}</span>
                  </label>
                </div>
              ))}
            </fieldset>
            <p className="text-xs text-muted-foreground">{ERASE_DATA.SEALING_NOTE}</p>
            <p className="text-xs text-muted-foreground">{ERASE_DATA.KEEPS}</p>
            <p className="text-xs text-muted-foreground">{retentionLine}</p>
            {error && (
              <p role="alert" className="text-body text-danger">
                {error}
              </p>
            )}
            <DialogFooter>
              <Button variant="ghost" disabled={busy} onClick={() => close(false)}>
                {ERASE.CANCEL}
              </Button>
              <Button variant="destructive" disabled={busy || !canConfirm} onClick={doErase}>
                {busy && <Loader2 className="size-3.5 animate-spin" />}
                {ERASE_DATA.CONFIRM}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
