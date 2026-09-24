/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AvailabilityControl — the ONE "Available to" control every admin-set
// resource carries (user-types design mock-08/user-types-design.md §2.6,
// UT-7b): Everyone, or Only these types/groups/people. Embedded inline in a
// git provider row, an agent roster row and an org workspace's detail page
// (each caller supplies its own `kind`/`value`).
//
// Self-contained and independent of whatever draft/Save cycle the editor
// around it runs: availability is a securityOps fact even when drawn inside
// an operatorOnly editor (§7, "Available to edit rights"), so this control
// reads and writes /permissions/availability and /permissions/grants on its
// own, the moment a change is made — never queued into the editor's own
// draft, and never gated on the editor's own Save button.
//
// Never merely hidden in the console (§2.6's closing rule): this control is
// the WRITE side only. The read side — a picker's list already scoped to
// what the caller may use, and the server's own refusal when a restricted
// value is named anyway — is the server's job at the routes that already
// carry it; this file adds no client-side filtering of its own.
import * as React from "react";
import { Loader2, X } from "lucide-react";
import { toast } from "sonner";
import { permissions as api } from "../../lib/api/permissions";
import { getErrorMessage } from "../../lib/format";
import { AVAILABILITY } from "../../lib/availability-copy";
import { PERM } from "../../lib/permissions-copy";
import { Segmented, SUBJECT_LABEL, subjectText } from "../screens/permissions";
import type { AvailabilityView, CapabilityGrant, CapabilitySubjectType } from "../../lib/types";
import { useSecurityOperator } from "./operator-context";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Field } from "./form-primitives";
import { Chip } from "./primitives";

// The three audiences a resource's "Only…" list can name — a superset of
// permissions.tsx's own PickableSubjectType (which deliberately leaves
// user_type out of the GENERIC grant form until UT-7a's own subject picker
// lands). Here the resource IS the audience picker, so all three are offered
// from day one, each a plain typed value like user/group already are — no
// directory lookup exists for a user type (it is a Wardyn-local id, not an
// external-directory claim).
type AudienceType = Exclude<CapabilitySubjectType, "all">;
const AUDIENCES: { value: AudienceType; label: string; hint: string }[] = [
  { value: "user_type", label: SUBJECT_LABEL.user_type, hint: AVAILABILITY.HINT_USER_TYPE },
  { value: "group", label: SUBJECT_LABEL.group, hint: AVAILABILITY.HINT_GROUP },
  { value: "user", label: SUBJECT_LABEL.user, hint: AVAILABILITY.HINT_USER },
];

type AvailabilityControlProps = {
  kind: string;
  value: string;
  /** The family's line after the Everyone/Only toggle, from the mock (the
   *  git provider row's AVAILABILITY.PROVIDER_ONLY_HINT). */
  onlyHint?: string;
  /** The family's note under the list (AVAILABILITY.PROVIDER_NOTE). */
  note?: string;
};

// Availability is a securityOps fact (§7), and so is its read: GET
// /permissions/availability refuses anyone else with a 403 and an
// authz.denied audit row. So the control is drawn, and read, only for a
// security admin or a super admin. Other callers who can open the same page
// (their own workspace's detail page, say) get nothing, not an error line.
export function AvailabilityControl(props: AvailabilityControlProps) {
  return useSecurityOperator() ? <Control {...props} /> : null;
}

function Control({ kind, value, onlyHint, note }: AvailabilityControlProps) {
  const [view, setView] = React.useState<AvailabilityView | null>(null);
  const [status, setStatus] = React.useState<"loading" | "ready" | "error">("loading");
  const [busy, setBusy] = React.useState(false);
  const [audienceType, setAudienceType] = React.useState<AudienceType>("user_type");
  const [audience, setAudience] = React.useState("");

  const load = React.useCallback(() => {
    setStatus("loading");
    api
      .getAvailability(kind, value)
      .then((v) => {
        setView(v);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, [kind, value]);
  React.useEffect(load, [load]);

  // Flips the restricted bit. A 400 (nobody listed yet) is rendered verbatim
  // under the control — the server's own availabilityOnlyEmptyMsg, never a
  // console reword — and the radio stays wherever the server's answer left
  // it, since `view` only ever reflects a call that actually landed.
  const [putError, setPutError] = React.useState<string | null>(null);
  const setRestricted = async (restricted: boolean) => {
    setBusy(true);
    setPutError(null);
    try {
      setView(await api.putAvailability(kind, value, restricted));
    } catch (e) {
      setPutError(getErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const addAudience = async () => {
    const subject = audience.trim();
    if (!subject) return;
    setBusy(true);
    try {
      await api.upsertGrant({ subject_type: audienceType, subject, capability: kind, value, effect: "allow" });
      setAudience("");
      load();
    } catch (e) {
      toast.error(AVAILABILITY.ADD_CTA, { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const removeAudience = async (g: CapabilityGrant) => {
    setBusy(true);
    try {
      await api.deleteGrant(g.id);
      load();
    } catch (e) {
      toast.error(PERM.REMOVE, { description: getErrorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  if (status === "loading" || status === "error" || !view) {
    // No skeleton: this is one line inside an editor already carrying its
    // own loading state, and an error here (a transient GET) is retried for
    // free the next time the row re-renders with a fresh `value`, same as
    // the rest of this control's fail-quiet reads.
    return status === "error" ? (
      <p className="text-meta text-muted-foreground">{AVAILABILITY.LOAD_FAILED}</p>
    ) : null;
  }

  // DELETE /permissions/grants has no restriction check, so removing the
  // last listed audience while "Only these" is on would leave the resource
  // for nobody, silently. The last one's × stays disabled until Everyone.
  const lastLocked = view.restricted && view.allowed_by.length === 1;

  return (
    <Field label={AVAILABILITY.LABEL}>
      <Segmented
        value={view.restricted ? "only" : "everyone"}
        onChange={(v) => setRestricted(v === "only")}
        disabled={busy}
        options={[
          { value: "everyone", label: AVAILABILITY.EVERYONE },
          { value: "only", label: AVAILABILITY.ONLY },
        ]}
      />
      {onlyHint && <p className="text-meta text-muted-foreground">{onlyHint}</p>}
      {putError && <p className="text-xs leading-snug text-danger">{putError}</p>}

      {view.allowed_by.length > 0 && (
        <div className="flex flex-wrap items-center gap-2">
          {view.allowed_by.map((g) => (
            <Chip key={g.id} tone="info">
              <span className="flex items-center gap-1">
                {SUBJECT_LABEL[g.subject_type] ?? g.subject_type}
                {g.subject_type !== "all" && `: ${subjectText(g)}`}
                <button
                  type="button"
                  aria-label={`${PERM.REMOVE} ${SUBJECT_LABEL[g.subject_type] ?? g.subject_type} ${subjectText(g)}`}
                  title={lastLocked ? AVAILABILITY.LAST_AUDIENCE_LOCKED : undefined}
                  disabled={busy || lastLocked}
                  onClick={() => removeAudience(g)}
                  className="ml-0.5 disabled:cursor-not-allowed disabled:opacity-50"
                >
                  <X className="size-3" />
                </button>
              </span>
            </Chip>
          ))}
        </div>
      )}
      {lastLocked && <p className="text-meta text-muted-foreground">{AVAILABILITY.LAST_AUDIENCE_LOCKED}</p>}

      <div className="flex flex-wrap items-center gap-2">
        <Segmented
          value={audienceType}
          onChange={setAudienceType}
          disabled={busy}
          options={AUDIENCES.map((a) => ({ value: a.value, label: a.label }))}
        />
        <Input
          aria-label={AUDIENCES.find((a) => a.value === audienceType)?.hint}
          placeholder={AVAILABILITY.ADD_PLACEHOLDER}
          value={audience}
          onChange={(e) => setAudience(e.target.value)}
          disabled={busy}
          className="h-8 max-w-[220px] font-mono"
        />
        <Button variant="outline" size="sm" disabled={busy || !audience.trim()} onClick={addAudience}>
          {busy ? <Loader2 className="size-3.5 animate-spin" /> : null}
          {AVAILABILITY.ADD_CTA}
        </Button>
      </div>

      {note && <p className="text-meta text-muted-foreground">{note}</p>}
    </Field>
  );
}
