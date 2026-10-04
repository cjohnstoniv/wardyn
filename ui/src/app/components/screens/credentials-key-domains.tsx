/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin → Credentials, under the inventory (mock packet M5 S2/S4): the key
// domains the deployment declares, and the assignments that say which one a
// person's next key is made in. Domains are declared in deploy configuration;
// this card only lists them and sets or removes assignments through
// /key-domains. A write held for a second person (202) shows the amber
// Submitted note and leaves the table as it was: a queued change is never
// shown as applied. A deployment with no Postgres store (501) shows no card.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { keyDomains as keyDomainsApi, type KeyDomains, type KeyDomainSubjectType, type KeyDomainWrite } from "../../lib/api/key-domains";
import { HttpError } from "../../lib/api/core";
import { getErrorMessage } from "../../lib/format";
import { AlertDialog, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "../ui/alert-dialog";
import { Button, buttonVariants } from "../ui/button";
import { Input } from "../ui/input";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { cn } from "../ui/utils";
import { Field } from "../wardyn/form-primitives";
import { Chip, SectionCard } from "../wardyn/primitives";
import { ErrorState, TableSkeleton } from "../wardyn/states";
import { ERASE, KEY_DOMAINS } from "../wardyn/copy/credentials";

type Load = { kind: "loading" } | { kind: "error" } | { kind: "absent" } | { kind: "ready"; data: KeyDomains };

// The sentence is a question and its consequence; the dialog titles itself with the question.
function splitQuestion(s: string): [string, string] {
  const at = s.lastIndexOf("? ");
  return at < 0 ? [s, ""] : [s.slice(0, at + 1), s.slice(at + 2)];
}

const subjectLabel = (a: { subject_type: KeyDomainSubjectType; subject: string }) =>
  a.subject_type === "all" ? KEY_DOMAINS.SOURCE_ALL : a.subject;

export function KeyDomainsSection() {
  const [load, setLoad] = React.useState<Load>({ kind: "loading" });
  const [pending, setPending] = React.useState(false);
  const [assigning, setAssigning] = React.useState(false);
  const [removing, setRemoving] = React.useState<{ type: KeyDomainSubjectType; subject: string } | null>(null);

  const reload = React.useCallback(() => {
    keyDomainsApi
      .list()
      .then((data) => setLoad({ kind: "ready", data }))
      .catch((e) => setLoad(e instanceof HttpError && (e.status === 501 || e.status === 403) ? { kind: "absent" } : { kind: "error" }));
  }, []);
  React.useEffect(reload, [reload]);

  // A write's answer: applied now (the lists are read again), or held (the Note, and nothing else changes).
  const wrote = (w: KeyDomainWrite) => {
    setPending(w === "pending");
    if (w === "applied") reload();
  };

  if (load.kind === "absent") return null;
  return (
    <div className="mt-6">
      <SectionCard
        title={KEY_DOMAINS.TITLE}
        right={
          load.kind === "ready" ? (
            <Button size="sm" variant="outline" onClick={() => setAssigning(true)}>
              {KEY_DOMAINS.ASSIGN_CTA}
            </Button>
          ) : undefined
        }
      >
        {load.kind === "loading" ? (
          <TableSkeleton rows={3} cols={3} />
        ) : load.kind === "error" ? (
          <ErrorState
            onRetry={() => {
              setLoad({ kind: "loading" });
              reload();
            }}
          />
        ) : (
          <>
            <p className="text-xs text-muted-foreground">{KEY_DOMAINS.LEDE}</p>
            {!load.data.principal_keys && <p className="mt-2 text-xs text-muted-foreground">{KEY_DOMAINS.OFF_NOTE}</p>}
            <div className="mt-3 overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>{KEY_DOMAINS.COL_DOMAIN}</TableHead>
                    <TableHead>{KEY_DOMAINS.COL_KEY}</TableHead>
                    <TableHead>{KEY_DOMAINS.COL_BOOT}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {load.data.domains.map((d) => (
                    <TableRow key={d.domain}>
                      <TableCell className="font-mono">{d.domain}</TableCell>
                      <TableCell className="text-muted-foreground">{d.key ?? ""}</TableCell>
                      <TableCell>
                        <Chip tone={d.proven ? "success" : "danger"}>{d.proven ? KEY_DOMAINS.PROVEN : KEY_DOMAINS.NOT_PROVEN}</Chip>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
            <p className="mt-2 text-xs text-muted-foreground">{KEY_DOMAINS.DEFAULT_NOTE}</p>

            <h3 className="mt-5 text-sm font-semibold text-foreground">{KEY_DOMAINS.ASSIGN_TITLE}</h3>
            {pending && (
              <div role="status" className="mt-2 flex flex-col gap-1 rounded-lg bg-warning-subtle px-3 py-2 text-xs leading-relaxed text-warning">
                <span className="font-semibold">{KEY_DOMAINS.SUBMITTED_TITLE}</span>
                <span>
                  {KEY_DOMAINS.SUBMITTED_BODY}{" "}
                  <Link to="/admin/governance?tab=changes" className="underline">
                    {KEY_DOMAINS.SUBMITTED_LINK}
                  </Link>
                </span>
              </div>
            )}
            <p className="mt-1 text-xs text-muted-foreground">{KEY_DOMAINS.PRECEDENCE}</p>
            {load.data.assignments.length > 0 && (
              <div className="mt-3 overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead>{KEY_DOMAINS.COL_SUBJECT}</TableHead>
                      <TableHead>{KEY_DOMAINS.COL_DOMAIN}</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {load.data.assignments.map((a) => (
                      <TableRow key={`${a.subject_type}:${a.subject}`}>
                        <TableCell className="font-medium">{subjectLabel(a)}</TableCell>
                        <TableCell className="font-mono">{a.domain}</TableCell>
                        <TableCell className="text-right">
                          <Button size="sm" variant="ghost" onClick={() => setRemoving({ type: a.subject_type, subject: a.subject })}>
                            {KEY_DOMAINS.REMOVE}
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
          </>
        )}
      </SectionCard>

      {load.kind === "ready" && assigning && (
        <AssignDialog
          domains={load.data.domains.filter((d) => d.declared).map((d) => d.domain)}
          onClose={() => setAssigning(false)}
          onWrote={wrote}
        />
      )}
      <RemoveDialog target={removing} onClose={() => setRemoving(null)} onWrote={wrote} />
    </div>
  );
}

const TYPES: { value: KeyDomainSubjectType; label: string }[] = [
  { value: "user", label: KEY_DOMAINS.TYPE_USER },
  { value: "group", label: KEY_DOMAINS.TYPE_GROUP },
  { value: "all", label: KEY_DOMAINS.TYPE_ALL },
];

function AssignDialog({ domains, onClose, onWrote }: { domains: string[]; onClose: () => void; onWrote: (w: KeyDomainWrite) => void }) {
  const [type, setType] = React.useState<KeyDomainSubjectType>("user");
  const [subject, setSubject] = React.useState("");
  const [domain, setDomain] = React.useState(domains[0] ?? "default");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const typeLabel = TYPES.find((t) => t.value === type)!.label;
  const canSave = !busy && (type === "all" || subject.trim() !== "");

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      onWrote(await keyDomainsApi.set(type, subject.trim(), domain));
      onClose();
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{KEY_DOMAINS.ASSIGN_CTA}</DialogTitle>
        </DialogHeader>
        <fieldset className="space-y-2">
          <legend className="text-sm font-medium text-foreground">{KEY_DOMAINS.COL_SUBJECT}</legend>
          <div className="flex flex-wrap gap-4">
            {TYPES.map((t) => (
              <label key={t.value} className="flex items-center gap-1.5 text-sm text-foreground">
                <input type="radio" name="key-domain-subject-type" checked={type === t.value} onChange={() => setType(t.value)} />
                {t.label}
              </label>
            ))}
          </div>
        </fieldset>
        {type !== "all" && (
          <Field label={typeLabel} htmlFor="key-domain-subject">
            <Input id="key-domain-subject" autoFocus autoComplete="off" value={subject} onChange={(e) => setSubject(e.target.value)} />
          </Field>
        )}
        <Field label={KEY_DOMAINS.FIELD_DOMAIN} htmlFor="key-domain-domain" hint={KEY_DOMAINS.ASSIGN_HINT}>
          <select
            id="key-domain-domain"
            value={domain}
            onChange={(e) => setDomain(e.target.value)}
            className="border-input bg-input-background focus-visible:border-ring focus-visible:ring-ring dark:bg-input/30 h-9 w-full rounded-md border px-2 font-mono text-body outline-none focus-visible:ring-[3px]"
          >
            {domains.map((d) => (
              <option key={d} value={d}>
                {d}
              </option>
            ))}
          </select>
        </Field>
        {error && (
          <p role="alert" className="text-body text-danger">
            {error}
          </p>
        )}
        <DialogFooter>
          <Button variant="ghost" disabled={busy} onClick={onClose}>
            {ERASE.CANCEL}
          </Button>
          <Button disabled={!canSave} onClick={save}>
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            {KEY_DOMAINS.ASSIGN_CTA}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function RemoveDialog({
  target,
  onClose,
  onWrote,
}: {
  target: { type: KeyDomainSubjectType; subject: string } | null;
  onClose: () => void;
  onWrote: (w: KeyDomainWrite) => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  React.useEffect(() => setError(""), [target]);
  const [question, consequence] = splitQuestion(KEY_DOMAINS.REMOVE_CONFIRM(target ? subjectLabel({ subject_type: target.type, subject: target.subject }) : ""));

  const remove = async () => {
    if (!target) return;
    setBusy(true);
    setError("");
    try {
      onWrote(await keyDomainsApi.remove(target.type, target.subject));
      onClose();
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <AlertDialog open={!!target} onOpenChange={(o) => !o && !busy && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{question}</AlertDialogTitle>
          <AlertDialogDescription>{consequence}</AlertDialogDescription>
        </AlertDialogHeader>
        {error && (
          <p role="alert" className="text-body text-danger">
            {error}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>{ERASE.CANCEL}</AlertDialogCancel>
          {/* A plain Button, not AlertDialogAction: the dialog stays open and shows the server's
              sentence when the write is refused. */}
          <button type="button" className={cn(buttonVariants({ variant: "destructive" }))} disabled={busy} onClick={remove}>
            {busy && <Loader2 className="mr-1 size-3.5 animate-spin" />}
            {KEY_DOMAINS.REMOVE}
          </button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
