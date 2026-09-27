/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AvailabilityControl — the ONE "Available to" control every admin-set
// resource carries (user-types design §2.6; drawn once in
// docs/design/available-to-mock/states.html): Everyone (Admins only on an
// image), or Only these types/groups/people. Each caller supplies its own
// `kind`/`value`.
//
// Self-contained and independent of whatever draft/Save cycle the editor
// around it runs: availability is a securityOps fact even when drawn inside
// an operatorOnly editor (§7, "Available to edit rights"), so this control
// reads and writes /permissions/availability and /permissions/grants on its
// own, the moment a change is made — never queued into the editor's own
// draft, and never gated on the editor's own Save button. The one exception
// is a creation form (decision 4): AvailabilityDraft holds the choice until
// the resource exists, and writeAvailability writes it after.
//
// Never merely hidden in the console (§2.6's closing rule): this control is
// the WRITE side only. The read side — a picker's list already scoped to
// what the caller may use, and the server's own refusal when a restricted
// value is named anyway — is the server's job at the routes that already
// carry it; this file adds no client-side filtering of its own.
import * as React from "react";
import { Loader2, X } from "lucide-react";
import { permissions as api } from "../../lib/api/permissions";
import { getErrorMessage } from "../../lib/format";
import { AVAILABILITY } from "../../lib/availability-copy";
import { useDeferredBusy } from "../../lib/use-deferred-busy";
import { Segmented, SUBJECT_LABEL } from "../screens/permissions";
import type { AvailabilityView, CapabilitySubjectType } from "../../lib/types";
import { useSecurityOperator } from "./operator-context";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { RadioGroup, RadioGroupItem } from "../ui/radio-group";
import { Field } from "./form-primitives";
import { Chip } from "./primitives";

// The three audiences a resource's "Only…" list can name. Here the resource IS
// the audience picker, so all three are offered, each a plain typed value: a
// user type is a Wardyn-local id, not an external-directory claim.
type AudienceType = Exclude<CapabilitySubjectType, "all">;
const AUDIENCES: { value: AudienceType; label: string; hint: string }[] = [
  { value: "user_type", label: SUBJECT_LABEL.user_type, hint: AVAILABILITY.HINT_USER_TYPE },
  { value: "group", label: SUBJECT_LABEL.group, hint: AVAILABILITY.HINT_GROUP },
  { value: "user", label: SUBJECT_LABEL.user, hint: AVAILABILITY.HINT_USER },
];

export type Audience = { subject_type: CapabilitySubjectType; subject: string };

// The family lines an editor passes (canon Table 2). adminsOnly is the image
// family: its first choice reads Admins only, its one line sits under the
// choice, and the locked last chip names Admins only.
type FamilyProps = {
  onlyHint?: string;
  note?: string;
  adminsOnly?: boolean;
};

type AvailabilityControlProps = FamilyProps & { kind: string; value: string };

// Availability is a securityOps fact (§7), and so is its read: GET
// /permissions/availability refuses anyone else with a 403 and an
// authz.denied audit row. So the control is drawn, and read, only for a
// security admin or a super admin. Other callers who can open the same page
// get nothing, not an error line.
export function AvailabilityControl(props: AvailabilityControlProps) {
  return useSecurityOperator() ? <Control {...props} /> : null;
}

// User types are named on their chips (packet A), but a grant carries the id.
// A failed read falls back to the id rather than hiding the chip.
function useUserTypeNames(): (id: string) => string {
  const [names, setNames] = React.useState<Record<string, string>>({});
  React.useEffect(() => {
    let live = true;
    api
      .listUserTypes()
      .then((ts) => live && setNames(Object.fromEntries(ts.map((t) => [t.id, t.name]))))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);
  return (id) => names[id] ?? id;
}

function chipText(a: Audience, typeName: (id: string) => string): string {
  if (a.subject_type === "user_type") return AVAILABILITY.CHIP_TYPE(typeName(a.subject));
  if (a.subject_type === "group") return AVAILABILITY.CHIP_GROUP(a.subject);
  return AVAILABILITY.CHIP_USER(a.subject);
}

function Control({ kind, value, ...family }: AvailabilityControlProps) {
  const [view, setView] = React.useState<AvailabilityView | null>(null);
  const [status, setStatus] = React.useState<"loading" | "ready" | "error">("loading");
  const [busy, setBusy] = React.useState<"choice" | "add" | "remove" | null>(null);
  // A refusal is shown as sent, inline, where it happened (CONSOLE-RULES §9):
  // the choice's under the choice, an add's or a remove's under the adder.
  const [choiceError, setChoiceError] = React.useState<string | null>(null);
  const [listError, setListError] = React.useState<string | null>(null);
  const typeName = useUserTypeNames();

  const load = React.useCallback(async () => {
    try {
      setView(await api.getAvailability(kind, value));
      setStatus("ready");
    } catch {
      setStatus("error");
    }
  }, [kind, value]);
  React.useEffect(() => {
    setStatus("loading");
    void load();
  }, [load]);

  const run = async (what: "choice" | "add" | "remove", fn: () => Promise<void>): Promise<boolean> => {
    setBusy(what);
    setChoiceError(null);
    setListError(null);
    try {
      await fn();
      return true;
    } catch (e) {
      (what === "choice" ? setChoiceError : setListError)(getErrorMessage(e));
      return false;
    } finally {
      setBusy(null);
    }
  };

  if (status === "error") return <p className="text-meta text-muted-foreground">{AVAILABILITY.LOAD_FAILED}</p>;
  // No skeleton: this is one block inside an editor already carrying its own
  // loading state.
  if (status === "loading" || !view) return null;

  const grants = view.allowed_by;
  return (
    <AvailabilityFields
      {...family}
      restricted={view.restricted}
      audiences={grants}
      typeName={typeName}
      busy={busy}
      choiceError={choiceError}
      listError={listError}
      // `view` only ever reflects a call that landed, so a refused PUT leaves
      // the choice where the server has it.
      onRestrictedChange={(restricted) =>
        void run("choice", async () => setView(await api.putAvailability(kind, value, restricted)))
      }
      onAdd={(subject_type, subject) =>
        run("add", async () => {
          await api.upsertGrant({ subject_type, subject, capability: kind, value, effect: "allow" });
          await load();
        })
      }
      onRemove={(i) =>
        void run("remove", async () => {
          await api.deleteGrant(grants[i].id);
          await load();
        })
      }
    />
  );
}

// AvailabilityDraft is the control on a creation form (decision 4): the same
// fields, held locally until the resource exists. writeAvailability writes it.
export type AvailabilityDraftValue = { restricted: boolean; audiences: Audience[] };

export function AvailabilityDraft({
  draft,
  onChange,
  disabled,
  ...family
}: FamilyProps & { draft: AvailabilityDraftValue; onChange: (d: AvailabilityDraftValue) => void; disabled?: boolean }) {
  const typeName = useUserTypeNames();
  return (
    <AvailabilityFields
      {...family}
      restricted={draft.restricted}
      audiences={draft.audiences}
      typeName={typeName}
      busy={disabled ? "choice" : null}
      onRestrictedChange={(restricted) => onChange({ ...draft, restricted })}
      onAdd={async (subject_type, subject) => {
        const listed = draft.audiences.some((a) => a.subject_type === subject_type && a.subject === subject);
        if (!listed) onChange({ ...draft, audiences: [...draft.audiences, { subject_type, subject }] });
        return true;
      }}
      onRemove={(i) => onChange({ ...draft, audiences: draft.audiences.filter((_, j) => j !== i) })}
    />
  );
}

// writeAvailability writes a creation form's choice once the resource has its
// value, in the mock's order: the list, then Only these. Throws the first
// refusal; what landed before it stays.
export async function writeAvailability(kind: string, value: string, draft: AvailabilityDraftValue): Promise<void> {
  for (const a of draft.audiences) {
    await api.upsertGrant({ subject_type: a.subject_type, subject: a.subject, capability: kind, value, effect: "allow" });
  }
  if (draft.restricted) await api.putAvailability(kind, value, true);
}

function AvailabilityFields({
  restricted,
  audiences,
  typeName,
  busy,
  choiceError,
  listError,
  onRestrictedChange,
  onAdd,
  onRemove,
  onlyHint,
  note,
  adminsOnly,
}: FamilyProps & {
  restricted: boolean;
  audiences: Audience[];
  typeName: (id: string) => string;
  busy: "choice" | "add" | "remove" | null;
  choiceError?: string | null;
  listError?: string | null;
  onRestrictedChange: (restricted: boolean) => void;
  onAdd: (subject_type: AudienceType, subject: string) => Promise<boolean>;
  onRemove: (index: number) => void;
}) {
  const [audienceType, setAudienceType] = React.useState<AudienceType>("user_type");
  const [audience, setAudience] = React.useState("");
  const id = React.useId();
  const disabled = busy !== null;
  // The spinner is Add's alone, and only after ~200ms (CONSOLE-RULES §7); a
  // choice or a remove shows the disabled state only.
  const { showSpinner } = useDeferredBusy(busy === "add");

  // DELETE /permissions/grants has no restriction check, so removing the last
  // listed audience while Only these is on would leave the resource for
  // nobody. Its × stays disabled until the first choice is back.
  const lastLocked = restricted && audiences.length === 1;
  const lockedReason = adminsOnly ? AVAILABILITY.LAST_AUDIENCE_LOCKED_IMAGE : AVAILABILITY.LAST_AUDIENCE_LOCKED;

  const add = async () => {
    const subject = audience.trim();
    if (subject && (await onAdd(audienceType, subject))) setAudience("");
  };

  return (
    <Field label={AVAILABILITY.LABEL}>
      <RadioGroup
        aria-label={AVAILABILITY.LABEL}
        value={restricted ? "only" : "first"}
        onValueChange={(v) => onRestrictedChange(v === "only")}
        disabled={disabled}
        className="gap-2"
      >
        <div className="flex items-start gap-2 text-body">
          <RadioGroupItem id={`${id}-first`} value="first" className="mt-0.5" />
          <label htmlFor={`${id}-first`}>{adminsOnly ? AVAILABILITY.ADMINS_ONLY : AVAILABILITY.EVERYONE}</label>
        </div>
        {/* The family's "Only these" line continues the label, as the mock
            sets it; the rendered text is the canon string whole. */}
        <div className="flex items-start gap-2 text-body" data-testid="availability-only">
          <RadioGroupItem
            id={`${id}-only`}
            value="only"
            className="mt-0.5"
            aria-describedby={onlyHint ? `${id}-only-hint` : undefined}
          />
          <span>
            <label htmlFor={`${id}-only`}>{AVAILABILITY.ONLY}</label>
            {onlyHint && (
              <span id={`${id}-only-hint`} className="text-meta text-muted-foreground">
                {onlyHint.slice(AVAILABILITY.ONLY.length)}
              </span>
            )}
          </span>
        </div>
      </RadioGroup>
      {adminsOnly && <p className="text-meta text-muted-foreground">{AVAILABILITY.IMAGE_HINT}</p>}
      {choiceError && <p className="text-xs leading-snug text-danger">{choiceError}</p>}

      {audiences.length > 0 && (
        <div className="flex flex-wrap items-center gap-2">
          {audiences.map((a, i) => {
            const text = chipText(a, typeName);
            return (
              <Chip key={`${a.subject_type}:${a.subject}`} tone="info">
                <span className="flex items-center gap-1">
                  {text}
                  <button
                    type="button"
                    aria-label={AVAILABILITY.REMOVE_ARIA(text)}
                    title={lastLocked ? lockedReason : undefined}
                    disabled={disabled || lastLocked}
                    onClick={() => onRemove(i)}
                    className="ml-0.5 disabled:cursor-not-allowed disabled:opacity-50"
                  >
                    <X className="size-3" />
                  </button>
                </span>
              </Chip>
            );
          })}
        </div>
      )}
      {lastLocked && <p className="text-meta text-muted-foreground">{lockedReason}</p>}

      <div className="flex flex-wrap items-center gap-2">
        <Segmented
          value={audienceType}
          onChange={setAudienceType}
          disabled={disabled}
          options={AUDIENCES.map((a) => ({ value: a.value, label: a.label }))}
        />
        <Input
          aria-label={AUDIENCES.find((a) => a.value === audienceType)?.hint}
          placeholder={AVAILABILITY.ADD_PLACEHOLDER}
          value={audience}
          onChange={(e) => setAudience(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              void add();
            }
          }}
          disabled={disabled}
          className="h-8 max-w-[240px] font-mono"
        />
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={disabled || !audience.trim()}
          onClick={() => void add()}
        >
          {showSpinner && <Loader2 className="size-3.5 animate-spin" />}
          {AVAILABILITY.ADD_CTA}
        </Button>
      </div>
      {listError && <p className="text-xs leading-snug text-danger">{listError}</p>}

      {note && <p className="text-meta text-muted-foreground">{note}</p>}
    </Field>
  );
}
