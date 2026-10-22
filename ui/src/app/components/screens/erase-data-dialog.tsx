/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Erase someone's data (mock packet M4, surface C; R6 AdminForms #3054): the one erase dialog. The People drawer
// and a credentials row open it with the person fixed and only Credentials ticked; the Credentials header opens
// it with a People search and, for the security tier, today's default scopes.
//
// Two doors behind one dialog, each keeping its own route: Credentials alone is DELETE /people/{p}/credentials (it
// answers with a count, which the result line keeps); anything wider is the security tier's POST
// /people/{p}/erasure. A caller below the security tier is only ever shown Credentials, so nobody gains or loses
// an erase. A listed person (row, drawer or the search) asks for the name to confirm; a typed address that is not
// listed has no second field. Opt-in scopes stay unticked (OPT_IN).
import * as React from "react";
import { Loader2 } from "lucide-react";
import { credentials as credentialsApi, ErasureIncomplete, type ErasureScope } from "../../lib/api/credentials";
import { HttpError } from "../../lib/api/core";
import { people as peopleApi } from "../../lib/api/people";
import { getErrorMessage } from "../../lib/format";
import type { PersonSummary } from "../../lib/types";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Input } from "../ui/input";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Field } from "../wardyn/form-primitives";
import { useShellSetupStatus } from "../wardyn/model-access-context";
import { useSecurityOperator } from "../wardyn/operator-context";
import { Chip, SectionLabel } from "../wardyn/primitives";
import { COMPONENTS_ADMIN } from "../wardyn/copy/components-admin";
import { ERASE, ERASE_DATA } from "../wardyn/copy/credentials";
import { PEOPLE_PAGE } from "../wardyn/copy/people";

// The order the scopes are listed in (the order they run in is the server's).
const SCOPES: ErasureScope[] = [
  "credentials",
  "audit_personal_fields",
  "run_tasks",
  "run_outputs",
  "components",
  "mask_copies",
  "recordings",
  "run_setups",
  "runner_pools",
  "member_drives",
];

// The saved-components scope's words live with the rest of the component copy (lazy).
const scopeCopy = (s: ErasureScope) =>
  s === "components"
    ? { label: COMPONENTS_ADMIN.ERASE_SCOPE_LABEL, hint: COMPONENTS_ADMIN.ERASE_SCOPE_HINT }
    : ERASE_DATA.SCOPE[s];

// Opt-in: recordings, and the three scopes added after the dialog (deleting a person's runner pool
// ends the runs waiting for it).
const OPT_IN = new Set<ErasureScope>(["recordings", "run_setups", "runner_pools", "member_drives"]);
const DEFAULT_SCOPES = new Set<ErasureScope>(SCOPES.filter((s) => !OPT_IN.has(s)));

const CREDENTIALS_ONLY = new Set<ErasureScope>(["credentials"]);

// design F-6: the retention sentence follows the deployment-wide credential store.
function retentionLineFor(storage: string | undefined): string {
  switch (storage) {
    case "vault":
      return ERASE.RETENTION_VAULT;
    case "key_vault":
      return ERASE.RETENTION_KEY_VAULT;
    default:
      return ERASE.RETENTION_LOCAL;
  }
}

/** A person the dialog was opened for: the principal the routes take and the name the confirm asks for. */
export interface ErasePerson {
  principal: string;
  label: string;
}

const personOf = (p: PersonSummary): ErasePerson => ({ principal: p.principal, label: p.email || p.principal });

type Result =
  | { kind: "scopes"; person: string; erased: Set<string>; chosen: ErasureScope[]; complete: boolean }
  | { kind: "credentials"; person: string; count: number; store?: string; purged?: boolean; recoverable_days?: number };

export function EraseDataDialog({
  open,
  person,
  onOpenChange,
  onErased,
}: {
  open: boolean;
  /** A person fixed by the launch point (a credentials row, the People drawer); absent opens the search. */
  person?: ErasePerson | null;
  onOpenChange: (open: boolean) => void;
  onErased: () => void;
}) {
  // The by-scope erase is the security tier's; below it the dialog only ever shows Credentials.
  const securityOperator = useSecurityOperator();
  const { status } = useShellSetupStatus();
  const [picked, setPicked] = React.useState<ErasePerson | null>(null);
  const [query, setQuery] = React.useState("");
  const [listOpen, setListOpen] = React.useState(true);
  const [matches, setMatches] = React.useState<PersonSummary[] | null>(null);
  const [confirmValue, setConfirmValue] = React.useState("");
  const [scopes, setScopes] = React.useState<Set<ErasureScope>>(CREDENTIALS_ONLY);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [result, setResult] = React.useState<Result | null>(null);

  React.useEffect(() => {
    if (open) {
      setPicked(person ?? null);
      setQuery("");
      setListOpen(true);
      setMatches(null);
      setConfirmValue("");
      setScopes(new Set(person || !securityOperator ? CREDENTIALS_ONLY : DEFAULT_SCOPES));
      setError("");
      setResult(null);
    }
    // Reset only when the dialog opens: a tier or person change mid-dialog must not wipe what was typed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const typed = query.trim();
  React.useEffect(() => {
    if (!open || picked || !typed) {
      setMatches(null);
      return;
    }
    let live = true;
    setMatches(null);
    peopleApi
      .list({ q: typed, limit: 6 })
      .then((page) => live && setMatches(page.people))
      .catch(() => live && setMatches([]));
    return () => {
      live = false;
    };
  }, [open, picked, typed]);

  if (!open) return null;
  const visible = securityOperator ? SCOPES : SCOPES.filter((s) => CREDENTIALS_ONLY.has(s));
  const listed = picked !== null;
  const label = picked?.label ?? typed;
  const principal = picked?.principal ?? typed;
  const confirmOk = !listed || confirmValue.trim().toLowerCase() === picked.label.toLowerCase();
  const canConfirm = label.length > 0 && scopes.size > 0 && confirmOk;
  const fixedByLaunch = !!person;

  const toggle = (s: ErasureScope, on: boolean) =>
    setScopes((cur) => {
      const next = new Set(cur);
      if (on) next.add(s);
      else next.delete(s);
      return next;
    });

  const doErase = async () => {
    const chosen = visible.filter((s) => scopes.has(s));
    const credentialsOnly = chosen.length === 1 && chosen[0] === "credentials";
    setBusy(true);
    setError("");
    try {
      if (credentialsOnly) {
        const res = await credentialsApi.erase(principal);
        setResult({ kind: "credentials", person: label, ...res });
      } else {
        await credentialsApi.erasePerson(principal, chosen);
        setResult({ kind: "scopes", person: label, erased: new Set(chosen), chosen, complete: true });
      }
    } catch (e) {
      if (e instanceof ErasureIncomplete) {
        setResult({ kind: "scopes", person: label, erased: new Set(e.done), chosen, complete: false });
      } else {
        // A >=500 never finished, so it may have partly landed: its own sentence. A 4xx refusal (self-erasure, an
        // unknown scope, the operator namespace, an unknown address) shows the server's sentence as sent.
        const failed = credentialsOnly ? ERASE.FAILED(label) : ERASE_DATA.FAILED(label);
        setError(e instanceof HttpError && e.status >= 500 ? failed : getErrorMessage(e));
      }
    } finally {
      setBusy(false);
    }
  };

  const close = (touched: boolean) => {
    onOpenChange(false);
    if (touched) onErased();
  };

  const showCredentialsWarning = (fixedByLaunch || !securityOperator) && scopes.has("credentials");

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && close(!!result)}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{ERASE_DATA.BY_EMAIL_TITLE}</DialogTitle>
        </DialogHeader>

        {result?.kind === "credentials" ? (
          <>
            <p className="text-body text-foreground">
              {result.count === 0 ? ERASE.DONE_NONE(result.person) : ERASE.DONE(result.count, result.person)}
            </p>
            {result.count > 0 && result.store && !result.purged && !!result.recoverable_days && (
              <p className="text-body text-muted-foreground">{ERASE.KEY_VAULT_RECOVERABLE(result.recoverable_days)}</p>
            )}
            {result.count > 0 && <p className="text-body text-muted-foreground">{ERASE.DONE_AUDIT}</p>}
            <DialogFooter>
              <Button onClick={() => close(true)}>{ERASE.CLOSE}</Button>
            </DialogFooter>
          </>
        ) : result ? (
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
            {picked ? (
              <Field label={ERASE.FIELD} htmlFor="erase-data-person-chosen">
                <div className="flex items-center justify-between gap-3">
                  <span id="erase-data-person-chosen" className="text-sm text-foreground">
                    {picked.label}
                  </span>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busy}
                    onClick={() => {
                      setPicked(null);
                      setQuery("");
                      setListOpen(true);
                      setConfirmValue("");
                    }}
                  >
                    {ERASE_DATA.CHANGE}
                  </Button>
                </div>
              </Field>
            ) : (
              <Field label={ERASE.FIELD} htmlFor="erase-data-person" hint={securityOperator ? ERASE_DATA.HINT : ERASE.HINT}>
                <Input
                  id="erase-data-person"
                  autoFocus
                  autoComplete="off"
                  value={query}
                  onChange={(e) => {
                    setQuery(e.target.value);
                    setListOpen(true);
                  }}
                />
                {typed && listOpen && (
                  <ul className="mt-1 divide-y divide-border rounded-md border border-border">
                    {matches?.map((m) => (
                      <li key={m.principal}>
                        <button
                          type="button"
                          className="flex w-full flex-col items-start px-3 py-2 text-left text-sm hover:bg-accent"
                          onClick={() => {
                            setPicked(personOf(m));
                            setConfirmValue("");
                          }}
                        >
                          <span className="text-foreground">{m.email || m.principal}</span>
                          {m.email && <span className="text-xs text-muted-foreground">{m.principal}</span>}
                        </button>
                      </li>
                    ))}
                    {matches?.length === 0 && <li className="px-3 py-2 text-sm text-muted-foreground">{ERASE_DATA.PICK_NONE}</li>}
                    <li>
                      <button
                        type="button"
                        className="flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-sm hover:bg-accent"
                        onClick={() => setListOpen(false)}
                      >
                        <span className="text-foreground">{ERASE_DATA.PICK_USE(typed)}</span>
                        <Chip tone="neutral">{ERASE_DATA.NOT_LISTED}</Chip>
                      </button>
                    </li>
                  </ul>
                )}
              </Field>
            )}
            <fieldset className="space-y-2">
              <legend>
                <SectionLabel>{ERASE_DATA.SCOPES}</SectionLabel>
              </legend>
              {visible.map((s) => (
                <div key={s} className="flex items-start gap-2.5">
                  <Checkbox
                    id={`erase-scope-${s}`}
                    className="mt-0.5"
                    checked={scopes.has(s)}
                    onCheckedChange={(v) => toggle(s, v === true)}
                  />
                  <label htmlFor={`erase-scope-${s}`} className="cursor-pointer">
                    <span className="block text-sm text-foreground">{scopeCopy(s).label}</span>
                    <span className="block text-xs text-muted-foreground">
                      {s === "credentials" && showCredentialsWarning ? ERASE.BODY : scopeCopy(s).hint}
                    </span>
                  </label>
                </div>
              ))}
            </fieldset>
            {securityOperator && (
              <>
                <p className="text-xs text-muted-foreground">{ERASE_DATA.SEALING_NOTE}</p>
                <p className="text-xs text-muted-foreground">{ERASE_DATA.KEEPS}</p>
              </>
            )}
            <p className="text-xs text-muted-foreground">{retentionLineFor(status?.credential_storage)}</p>
            {listed && scopes.has("credentials") && (
              <p className="text-body text-foreground">{PEOPLE_PAGE.CREDS_ERASE_CONFIRM(picked.label)}</p>
            )}
            {listed && (
              <Field label={ERASE.CONFIRM_LABEL(picked.label)} htmlFor="erase-confirm">
                <Input
                  id="erase-confirm"
                  autoComplete="off"
                  value={confirmValue}
                  onChange={(e) => setConfirmValue(e.target.value)}
                />
              </Field>
            )}
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
