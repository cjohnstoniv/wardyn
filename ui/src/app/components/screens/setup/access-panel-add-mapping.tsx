/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Loader2 } from "lucide-react";
import { access as api } from "../../../lib/api/access";
import { PendingChangeError } from "../../../lib/api/core";
import type { AccessResponse, AccessRole } from "../../../lib/types";
import { GUARD, PEOPLE } from "../../../lib/people-access-copy";
import { Button } from "../../ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";
import { Field } from "../../wardyn/form-primitives";
import { DirectoryCombobox } from "../../wardyn/directory-combobox";
import { Segmented } from "../permissions";
import { classifyWriteError, writeErrorNote, Note, type WriteErrorKind } from "./access-panel";

// Add-mapping form — the FIRST_ROW posture guard runs here, pre-emptively,
// using GET /access's own posture (before/after) rather than waiting for the
// server to 400 (which access.go documents as the SAME data, exposed early —
// see AccessPanel's header comment).
export function AddMappingForm({
  access,
  onReload,
  onHeld,
  prefillValue,
  onPrefillConsumed,
}: {
  access: AccessResponse;
  onReload: () => void;
  /** Whether the last write was held for a second person (202) rather than saved. */
  onHeld: (held: boolean) => void;
  // #913: a migrated row's "Choose a type" action sets this to that row's
  // value; consumed once (below) so a second click can prefill the same
  // value again.
  prefillValue?: string | null;
  onPrefillConsumed?: () => void;
}) {
  const [value, setValue] = React.useState("");
  const [role, setRole] = React.useState<AccessRole>("admin");
  // UT-7a: only meaningful at role "user", and only OFFERED once the org has
  // a real choice to make — a single-option dropdown ("Standard user", and
  // nothing else) is the plain toggle the design's own switch rule uses
  // (user-types-design.md rev 4 §2.6: "a plain toggle with one type"). Empty
  // means "let the server default to Standard user".
  const [userType, setUserType] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<WriteErrorKind | null>(null);
  const [guardOpen, setGuardOpen] = React.useState(false);
  const [ack, setAck] = React.useState(false);
  const [guardBeforeAfter, setGuardBeforeAfter] = React.useState<{ before: string; after: string } | null>(null);
  // R1-F112: a demotion revokes outstanding tokens SILENTLY on the wire —
  // this is the one-line receipt for it, cleared on the next attempt.
  const [tokensRevoked, setTokensRevoked] = React.useState<number | null>(null);

  // #913: adopt a "Choose a type" prefill once, then tell the table it was
  // consumed — the same value re-submitted here is the natural upsert-in-place
  // (the store's ON CONFLICT), which is what clears the migrated marker.
  React.useEffect(() => {
    if (!prefillValue) return;
    setValue(prefillValue);
    setRole("user");
    setUserType("");
    onPrefillConsumed?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- onPrefillConsumed is re-created per render; prefillValue is the trigger
  }, [prefillValue]);

  const wouldFlipPosture = access.posture.map_empty && access.posture.changes;

  const doSubmit = async (acknowledge: boolean) => {
    setSaving(true);
    setError(null);
    setTokensRevoked(null);
    try {
      const res = await api.upsertMapping({
        value: value.trim(),
        role,
        user_type: role === "user" && userType ? userType : undefined,
        acknowledge_access_change: acknowledge || undefined,
      });
      setValue("");
      setUserType("");
      setGuardOpen(false);
      setAck(false);
      setTokensRevoked(res.tokensRevoked ?? null);
      onHeld(false);
      onReload();
    } catch (e) {
      if (e instanceof PendingChangeError) {
        setValue("");
        setUserType("");
        setGuardOpen(false);
        setAck(false);
        onHeld(true);
        onReload();
        return;
      }
      const classified = classifyWriteError(e);
      if (classified.kind === "posture_flip") {
        setGuardBeforeAfter({ before: classified.before, after: classified.after });
        setGuardOpen(true);
      } else {
        setError(classified);
      }
    } finally {
      setSaving(false);
    }
  };

  const onAddClick = () => {
    if (!value.trim()) return;
    setError(null);
    if (wouldFlipPosture) {
      setGuardBeforeAfter({ before: access.posture.before, after: access.posture.after });
      setGuardOpen(true);
      setAck(false);
      return;
    }
    void doSubmit(false);
  };

  return (
    <div className="border-t border-border px-6 py-5">
      <h3 className="text-sm font-medium text-foreground">{PEOPLE.ADD_TITLE}</h3>
      <div className="mt-4 grid gap-4 md:grid-cols-[1fr_auto_auto]">
        {/* Kind-LESS, so the search is too ("any"): this one field takes an App
            Role, a group or an email, and VALUE_HINT still says so. The GUID
            landmine is worst exactly here — an Entra `groups` claim carries an
            object id, and this is the field that has to match it — and with no
            directory configured it stays the plain input it has always been. */}
        <Field label={PEOPLE.FIELD_VALUE} htmlFor="access-value" hint={PEOPLE.VALUE_HINT} required>
          <DirectoryCombobox id="access-value" value={value} onChange={setValue} />
        </Field>
        <Field label={PEOPLE.FIELD_ROLE}>
          <Segmented
            value={role}
            onChange={setRole}
            options={[
              // Highest tier first, same order as AccessRole's own union and
              // oidc's constant block. security_admin is offered here because
              // a role mapping is the ONLY way a session ever reaches it —
              // there is no operator-allowlist twin and it is refused as
              // WARDYN_OIDC_DEFAULT_ROLE.
              { value: "admin", label: PEOPLE.ROLE_ADMIN },
              { value: "security_admin", label: PEOPLE.ROLE_SECURITY_ADMIN },
              { value: "user", label: PEOPLE.ROLE_USER },
            ]}
          />
        </Field>
        <div className="flex items-end">
          <Button onClick={onAddClick} disabled={saving || !value.trim()}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : null}
            {PEOPLE.ADD_CTA}
          </Button>
        </div>
      </div>
      {/* Offered only at role "user", and only once there is a real choice
          beyond Standard user — see userType's own doc comment above. */}
      {role === "user" && (access.user_types ?? []).length > 1 && (
        <div className="mt-4 max-w-xs">
          <Field label={PEOPLE.FIELD_USER_TYPE} htmlFor="access-user-type">
            <Select value={userType} onValueChange={setUserType}>
              <SelectTrigger id="access-user-type">
                <SelectValue placeholder={PEOPLE.TYPE_STANDARD} />
              </SelectTrigger>
              <SelectContent>
                {(access.user_types ?? []).map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
        </div>
      )}
      {error && <Note tone="red">{writeErrorNote(error)}</Note>}
      {/* R1-F112: the server demoted this value's role and revoked its
          outstanding tokens as part of the SAME write — the count only
          renders when it's non-zero, so a fresh mapping (nothing to revoke)
          says nothing. */}
      {!error && !!tokensRevoked && <Note tone="plain">{PEOPLE.TOKENS_REVOKED_RECEIPT(tokensRevoked)}</Note>}

      <AlertDialog open={guardOpen} onOpenChange={(o) => !o && setGuardOpen(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{GUARD.FIRST_ROW_TITLE}</AlertDialogTitle>
            <AlertDialogDescription>
              {guardBeforeAfter ? GUARD.FIRST_ROW_BODY(guardBeforeAfter.before, guardBeforeAfter.after) : ""}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {access.operator_emails_present && <p className="text-xs text-muted-foreground">{GUARD.ALLOWLIST_NOTE}</p>}
          <label className="flex items-start gap-2 text-xs text-foreground">
            <input type="checkbox" className="mt-0.5" checked={ack} onChange={(e) => setAck(e.target.checked)} />
            {GUARD.GUARD_ACK_LABEL}
          </label>
          {error && <Note tone="red">{writeErrorNote(error)}</Note>}
          <AlertDialogFooter>
            <AlertDialogCancel onClick={() => setGuardOpen(false)}>{PEOPLE.CANCEL}</AlertDialogCancel>
            <AlertDialogAction
              disabled={!ack || saving}
              onClick={(e) => {
                e.preventDefault();
                void doSubmit(true);
              }}
            >
              {saving ? <Loader2 className="size-4 animate-spin" /> : null}
              {GUARD.FIRST_ROW_CONFIRM}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
