/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The People page's per-person drawer (0.8.6 ppl-p2, mock M12 D3): one section per existing door, each
// destructive action behind a confirm that names the person. The actions are the doors that already
// exist: POST /sessions/revoke, DELETE /people/{p}/ssh-keys, DELETE /people/{p}/credentials, plus the
// token list. Nothing here is a new server path.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { credentials as credentialsApi } from "../../../lib/api/credentials";
import { people as peopleApi } from "../../../lib/api/people";
import { getErrorMessage, relativeTime } from "../../../lib/format";
import type { PersonSummary, PersonToken } from "../../../lib/types";
import { PEOPLE_PAGE as P } from "../../wardyn/copy/people";
import { roleLabel } from "./people-format";
import { Button } from "../../ui/button";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "../../ui/sheet";

type Action = "sign_out" | "ssh" | "creds";

// The directory's API-token count (store_people_directory.go) counts the same rows: neither revoked nor expired.
const isActiveToken = (t: PersonToken) => !t.revoked_at && (!t.expires_at || Date.parse(t.expires_at) > Date.now());

function Section({ label, value, children }: { label: string; value: React.ReactNode; children?: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 border-b border-border py-3">
      <div>
        <div className="text-body font-medium text-foreground">{label}</div>
        <div className="text-meta text-muted-foreground">{value}</div>
      </div>
      {children}
    </div>
  );
}

export function PersonDrawer({
  person,
  onClose,
  onChanged,
}: {
  person: PersonSummary | null;
  onClose: () => void;
  onChanged: () => void;
}) {
  const [confirm, setConfirm] = React.useState<Action | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [tokens, setTokens] = React.useState<PersonToken[]>([]);
  const principal = person?.principal;

  React.useEffect(() => {
    setConfirm(null);
    setTokens([]);
    if (!principal) return;
    let live = true;
    peopleApi
      .tokens(principal)
      .then((t) => live && setTokens(t.filter(isActiveToken)))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [principal]);

  if (!person) return null;
  const label = person.email || person.principal;
  const message: Record<Action, { confirm: string; run: () => Promise<unknown>; button: string }> = {
    sign_out: { confirm: P.SIGN_OUT_CONFIRM(label), run: () => peopleApi.signOutEverywhere(person.principal), button: P.SIGN_OUT },
    ssh: { confirm: P.SSH_REMOVE_CONFIRM(label), run: () => peopleApi.removeSSHKeys(person.principal), button: P.SSH_REMOVE },
    creds: { confirm: P.CREDS_ERASE_CONFIRM(label), run: () => credentialsApi.erase(person.principal), button: P.CREDS_ERASE },
  };

  const run = async (action: Action) => {
    setBusy(true);
    try {
      await message[action].run();
      toast.success(`${message[action].button}: ${label}`);
      setConfirm(null);
      onChanged();
    } catch (e) {
      toast.error(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Sheet open onOpenChange={(o) => !o && onClose()}>
        <SheetContent className="scroll-thin w-full gap-0 overflow-y-auto sm:max-w-[480px]">
          <SheetHeader className="border-b border-border pb-4">
            <SheetTitle>
              {label}
              {person.role ? ` · ${roleLabel(person.role)}` : ""}
            </SheetTitle>
          </SheetHeader>
          <div className="px-4">
            <Section label={P.SESSIONS} value={P.SESSIONS_ACTIVE(person.active_sessions)}>
              <Button size="sm" variant="outline" onClick={() => setConfirm("sign_out")}>
                {P.SIGN_OUT}
              </Button>
            </Section>
            <Section label={P.TOKENS} value={person.api_tokens}>
              {tokens.length > 0 && (
                <ul className="text-meta text-muted-foreground">
                  {tokens.map((t) => (
                    <li key={t.id}>
                      {t.name}
                      {t.last_used_at ? ` · ${relativeTime(t.last_used_at)}` : ""}
                    </li>
                  ))}
                </ul>
              )}
            </Section>
            <Section label={P.SSH} value={person.ssh_keys}>
              <Button size="sm" variant="outline" disabled={person.ssh_keys === 0} onClick={() => setConfirm("ssh")}>
                {P.SSH_REMOVE}
              </Button>
            </Section>
            <Section label={P.CREDS} value={person.credentials}>
              <Button size="sm" variant="outline" disabled={person.credentials === 0} onClick={() => setConfirm("creds")}>
                {P.CREDS_ERASE}
              </Button>
            </Section>
            <Section label={P.RUNS} value={P.RUNNING_COUNT(person.active_runs)}>
              <Link to={`/admin/runs?q=${encodeURIComponent(person.principal)}`} className="text-body text-info hover:underline">
                {P.RUNS_LINK} →
              </Link>
            </Section>
          </div>
        </SheetContent>
      </Sheet>

      <AlertDialog open={confirm !== null} onOpenChange={(o) => !o && !busy && setConfirm(null)}>
        <AlertDialogContent>
          {confirm && (
            <>
              <AlertDialogHeader>
                <AlertDialogTitle>{message[confirm].button}</AlertDialogTitle>
                <AlertDialogDescription>{message[confirm].confirm}</AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel disabled={busy}>Cancel</AlertDialogCancel>
                <Button variant="destructive" disabled={busy} onClick={() => run(confirm)}>
                  {busy && <Loader2 className="size-3.5 animate-spin" />}
                  {message[confirm].button}
                </Button>
              </AlertDialogFooter>
            </>
          )}
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
