/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance -> Runner credentials (packet M-RN-4, DP1/DP2): per credential class, how an
// organisation-held credential may reach a run on a person's own runner. A governance document like the
// others: the write is held for a second person on a deployment that requires one (202 -> SubmittedNote).
//
// EVERY user-visible string comes from governance-copy.ts's DELIVERY block (governance-prompt.md §7.14).
//
// The bounds (which modes a class may take) are lib/types/credential-delivery.ts's CLASS_MODES, pinned to
// the server's table by a wire test. A mode outside them is a DISABLED option with the bound's reason, never
// an absent one; the server's 400 stays the gate. An absent class means Don't allow, so the editor writes
// only the classes that are not set to it.
import * as React from "react";
import { AlertTriangle, Loader2 } from "lucide-react";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { credentialDelivery } from "../../../lib/api/credential-delivery";
import { getErrorMessage } from "../../../lib/format";
import { DELIVERY } from "../../../lib/governance-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import {
  CLASS_MODES,
  DELIVERY_CLASSES,
  FIXED_CLASSES,
  POSTURE_OSES,
  type ClassPolicy,
  type DeliveryClass,
  type DeliveryMode,
  type DeliveryPolicy,
  type PostureOS,
} from "../../../lib/types/credential-delivery";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { useSecurityOperator } from "../../wardyn/operator-context";
import { EmptyState, TableSkeleton } from "../../wardyn/states";
import { Chip } from "../../wardyn/primitives";
import { Ban } from "lucide-react";
import { Note } from "./display";
import { SubmittedNote } from "./submitted-note";

// The credential, the three options, then what people see: one grid for the header and every row.
// Joined from single tokens: the no-own-copy scan reads any quoted run of words as prose.
const ROW_GRID = ["grid", "gap-x-4", "gap-y-2", "px-6", "py-3", "md:grid-cols-[minmax(0,1.7fr)_repeat(3,minmax(0,1fr))_minmax(0,1fr)]", "md:items-start"].join(" ");

const MODES = ["via_org", "runner_resident", "refuse"] as const;
type EditableMode = (typeof MODES)[number];
// An absent class is Don't allow: the People see cell is quiet while it is that.
const DEFAULT_MODE: EditableMode = "refuse";

// What the editor does not edit: the two fixed classes, and the shared Azure sign-in, which 0.9 never
// creates (so a mode for it could not take effect). A stored value for the last rides through a save as it was.
const READ_ONLY: readonly DeliveryClass[] = [...FIXED_CLASSES, "azure_foundry_token"];

// The version a runner reports is dot-separated numbers (placement.versionAtLeast); anything else is never met.
const VERSION = /^\d+(\.\d+)*$/;

interface RowDraft {
  mode: EditableMode;
  mdm: boolean;
  disk: boolean;
  os: Record<PostureOS, string>;
  // A stored minimum for an OS the editor has no field for rides through a save as it was, like the shared Azure class.
  osOther: Record<string, string>;
}

type Draft = Record<string, RowDraft>;

function rowFrom(cp: ClassPolicy | undefined): RowDraft {
  const req = cp?.require_posture;
  return {
    mode: cp && (MODES as readonly string[]).includes(cp.mode) ? (cp.mode as EditableMode) : "refuse",
    mdm: !!req?.mdm_managed,
    disk: !!req?.disk_encrypted,
    os: { darwin: req?.os_min?.darwin ?? "", windows: req?.os_min?.windows ?? "", linux: req?.os_min?.linux ?? "" },
    osOther: Object.fromEntries(Object.entries(req?.os_min ?? {}).filter(([os]) => !(POSTURE_OSES as readonly string[]).includes(os))),
  };
}

const editable = DELIVERY_CLASSES.filter((c) => !READ_ONLY.includes(c));

function draftFrom(policy: DeliveryPolicy): Draft {
  return Object.fromEntries(editable.map((c) => [c, rowFrom(policy.classes[c])]));
}

const osMinOf = (row: RowDraft) => ({
  ...row.osOther,
  ...Object.fromEntries(POSTURE_OSES.map((os) => [os, row.os[os].trim()] as const).filter(([, v]) => v !== "")),
});

// The document the draft writes: only classes that are not Don't allow, a posture only under Store on the runner.
function policyFrom(draft: Draft, stored: DeliveryPolicy): DeliveryPolicy {
  const classes: DeliveryPolicy["classes"] = {};
  for (const c of editable) {
    const row = draft[c];
    if (row.mode === "refuse") continue;
    const cp: ClassPolicy = { mode: row.mode };
    if (row.mode === "runner_resident") {
      const os_min = osMinOf(row);
      const req = {
        ...(row.mdm && { mdm_managed: true }),
        ...(row.disk && { disk_encrypted: true }),
        ...(Object.keys(os_min).length > 0 && { os_min }),
      };
      if (Object.keys(req).length > 0) cp.require_posture = req;
    }
    classes[c] = cp;
  }
  const azure = stored.classes.azure_foundry_token;
  if (azure && azure.mode !== "refuse") classes.azure_foundry_token = azure;
  // Per-trust-class overrides have no editor control yet; a save keeps them as stored.
  return stored.by_trust ? { classes, by_trust: stored.by_trust } : { classes };
}

const versionInvalid = (v: string) => v.trim() !== "" && !VERSION.test(v.trim());

// Why a class cannot take a mode, "" when it can. Only the bounds the table has can be missing: no class lacks
// Store on the runner except GitHub App tokens, and an organisation's Azure DevOps tokens may be stored on one.
function boundReason(c: DeliveryClass, mode: DeliveryMode): string {
  if ((CLASS_MODES[c] as readonly DeliveryMode[]).includes(mode)) return "";
  return mode === "via_org" ? DELIVERY.BOUND_NO_VIA_ORG : DELIVERY.BOUND_NO_RESIDENT_GITHUB;
}

export function RunnerCredentials({ onSubmitted }: { onSubmitted?: () => void }) {
  const securityOperator = useSecurityOperator();
  const [status, setStatus] = React.useState<"loading" | "ready" | "error">("loading");
  const [stored, setStored] = React.useState<DeliveryPolicy>({ classes: {} });
  const [draft, setDraft] = React.useState<Draft>(() => draftFrom({ classes: {} }));
  const [runnersOn, setRunnersOn] = React.useState<boolean | null>(null);
  const [outcome, setOutcome] = React.useState<{ kind: "saved" | "submitted" } | { kind: "refused"; message: string } | null>(null);
  const [busy, setBusy] = React.useState(false);
  // The draft as it stood when a save met a 412 (someone saved first): shown in the shared saved-elsewhere banner so
  // the person can copy it, while the screen itself is reloaded to the stored document and its ETag.
  const [savedElsewhere, setSavedElsewhere] = React.useState<string | null>(null);
  // The ETag of the document the draft was read from (or last saved), sent back as If-Match.
  const etagRef = React.useRef<string | null>(null);
  // The document last held for a second person: until the draft changes again it is not a new change.
  const [submittedJson, setSubmittedJson] = React.useState<string | null>(null);
  const { showSpinner } = useDeferredBusy(busy);
  const refusedRef = React.useRef<HTMLHeadingElement>(null);

  const load = React.useCallback(() => {
    setStatus("loading");
    Promise.all([credentialDelivery.get(), credentialDelivery.runnersEnabled().catch(() => null)])
      .then(([doc, enabled]) => {
        setStored(doc.policy);
        etagRef.current = doc.etag ?? null;
        setSubmittedJson(null);
        setDraft(draftFrom(doc.policy));
        setRunnersOn(enabled);
        setStatus("ready");
      })
      .catch(() => setStatus("error"));
  }, []);
  React.useEffect(load, [load]);

  const policy = React.useMemo(() => policyFrom(draft, stored), [draft, stored]);
  const baseline = React.useMemo(() => JSON.stringify(policyFrom(draftFrom(stored), stored)), [stored]);
  const dirty = JSON.stringify(policy) !== baseline && JSON.stringify(policy) !== submittedJson;
  const invalid = editable.some((c) => draft[c].mode === "runner_resident" && POSTURE_OSES.some((os) => versionInvalid(draft[c].os[os])));
  // The refused draft lives on in the saved-elsewhere banner after the reload made the editor clean: leaving drops it too.
  useUnsavedGuard("runner-credentials", dirty || savedElsewhere !== null, () => savedElsewhere ?? JSON.stringify(policy, null, 2));

  React.useEffect(() => {
    if (outcome?.kind === "refused") refusedRef.current?.focus();
  }, [outcome]);

  const edit = (c: DeliveryClass, patch: Partial<RowDraft>) => {
    setOutcome(null);
    setDraft((d) => ({ ...d, [c]: { ...d[c], ...patch } }));
  };

  const save = async () => {
    setBusy(true);
    setOutcome(null);
    try {
      const doc = await credentialDelivery.put(policy, etagRef.current);
      setStored(doc.policy ?? policy);
      etagRef.current = doc.etag ?? null;
      setOutcome({ kind: "saved" });
      setSavedElsewhere(null);
    } catch (e) {
      if (e instanceof HttpError && e.status === 412) {
        // Never the server's raw If-Match sentence: reload the stored document and its ETag (so the next Save is
        // against what is stored now) and offer the draft that was refused.
        setSavedElsewhere(JSON.stringify(policy, null, 2));
        load();
      } else if (e instanceof PendingChangeError) {
        setSubmittedJson(JSON.stringify(policy));
        setOutcome({ kind: "submitted" });
        onSubmitted?.();
      } else {
        setOutcome({ kind: "refused", message: e instanceof HttpError ? e.message : getErrorMessage(e) });
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="mt-6 overflow-hidden rounded-xl border border-border bg-card">
      <div className="px-6 pt-5">
        <h2 className="text-sm font-medium text-foreground">{DELIVERY.TAB}</h2>
        <p className="mt-1 max-w-[82ch] text-body text-muted-foreground">{DELIVERY.LEAD}</p>
        {status === "ready" && runnersOn === false && <Note>{DELIVERY.RUNNERS_OFF_NOTE}</Note>}
      </div>

      {status === "loading" && (
        <div className="mt-4">
          <TableSkeleton rows={8} cols={3} />
        </div>
      )}

      {status === "error" && (
        <EmptyState
          icon={AlertTriangle}
          title={DELIVERY.FETCH_FAILED_TITLE}
          description={DELIVERY.FETCH_FAILED_BODY}
          action={
            <Button variant="outline" size="sm" onClick={load}>
              {ACCESS_STATE.FETCH_FAILED_RETRY}
            </Button>
          }
        />
      )}

      {status === "ready" && (
        <>
          <div className="mt-4 divide-y divide-border border-t border-border">
            <div className={`${ROW_GRID} label-eyebrow max-md:hidden`}>
              <span>{DELIVERY.COL_CREDENTIAL}</span>
              {MODES.map((mode) => (
                <span key={mode}>{DELIVERY.MODE[mode]}</span>
              ))}
              <span>{DELIVERY.COL_PEOPLE_SEE}</span>
            </div>
            {DELIVERY_CLASSES.map((c) =>
              READ_ONLY.includes(c) ? (
                <ReadOnlyRow key={c} cls={c} />
              ) : (
                <ClassRow
                  key={c}
                  cls={c}
                  row={draft[c]}
                  disabled={!securityOperator || busy}
                  onEdit={(patch) => edit(c, patch)}
                />
              ),
            )}
          </div>

          <div className="flex flex-col gap-2 px-6 py-4">
            {savedElsewhere !== null && <SavedElsewhereBanner reloaded documentText={savedElsewhere} onDiscard={() => setSavedElsewhere(null)} />}
            {outcome?.kind === "saved" && <Note role="status">{DELIVERY.SAVED}</Note>}
            {outcome?.kind === "submitted" && <SubmittedNote />}
            {outcome?.kind === "refused" && (
              <Note tone="red" role="alert">
                <h3 ref={refusedRef} tabIndex={-1} className="font-semibold outline-none">
                  {DELIVERY.REFUSED_TITLE}
                </h3>
                <span>{outcome.message}</span>
              </Note>
            )}
            <div className="flex justify-end">
              <Button disabled={!securityOperator || !dirty || invalid || busy} onClick={() => void save()}>
                {showSpinner ? <Loader2 className="size-4 animate-spin" /> : null}
                {DELIVERY.SAVE}
              </Button>
            </div>
          </div>
        </>
      )}
    </section>
  );
}

// The classes the editor shows but never writes: its value says why.
function ReadOnlyRow({ cls }: { cls: DeliveryClass }) {
  const value =
    cls === "azure_foundry_token" ? DELIVERY.NOT_IN_RELEASE : cls === "oauth_subscription" ? DELIVERY.FIXED_OWN : DELIVERY.FIXED_CLOUD_STS;
  const hint = cls === "azure_foundry_token" ? DELIVERY.CLASS_HINT.azure_foundry_token : null;
  return (
    <div className={ROW_GRID}>
      <div>
        <h3 className="text-sm font-medium text-foreground">{DELIVERY.CLASS[cls]}</h3>
        {hint && <p className="mt-0.5 text-xs text-muted-foreground">{hint}</p>}
      </div>
      <p className="text-sm text-muted-foreground md:col-span-4">{value}</p>
    </div>
  );
}

type WritableClass = Exclude<DeliveryClass, "oauth_subscription" | "cloud_sts" | "azure_foundry_token">;

function ClassRow({
  cls,
  row,
  disabled,
  onEdit,
}: {
  cls: DeliveryClass;
  row: RowDraft;
  disabled: boolean;
  onEdit: (patch: Partial<RowDraft>) => void;
}) {
  const key = cls as WritableClass;
  const titleId = `dc-${cls}`;
  const resident = row.mode === "runner_resident";
  return (
    <div>
      <RadioGroup
        aria-labelledby={titleId}
        value={row.mode}
        disabled={disabled}
        onValueChange={(v) => onEdit({ mode: v as EditableMode })}
        className={ROW_GRID}
      >
        <div>
          <h3 id={titleId} className="text-sm font-medium text-foreground">
            {DELIVERY.CLASS[key]}
          </h3>
          <p className="mt-0.5 text-xs text-muted-foreground">{DELIVERY.CLASS_HINT[key]}</p>
        </div>
        {MODES.map((mode) => {
          const reason = boundReason(cls, mode);
          const id = `dc-${cls}-${mode}`;
          const note = reason || (cls === "ado_minted_pat" && mode === "runner_resident" ? DELIVERY.ADO_RESIDENT_HINT : "");
          return (
            <div key={mode} className="flex flex-col gap-1">
              <div className="flex items-center gap-2">
                <RadioGroupItem id={id} value={mode} disabled={!!reason} aria-describedby={`${id}-d`} className="peer" />
                {/* The column header names the option on wide screens; on a phone each cell names itself. */}
                <Label htmlFor={id} className="text-sm md:sr-only">
                  {DELIVERY.MODE[mode]}
                </Label>
              </div>
              {/* An enabled option's hint is its accessible description; a bound's reason is read in its cell. */}
              <p id={`${id}-d`} className={note ? "flex gap-1 text-xs text-muted-foreground" : "sr-only"}>
                {reason && <Ban aria-hidden className="mt-0.5 size-3 shrink-0" />}
                <span>{note || DELIVERY.MODE_HINT[mode]}</span>
              </p>
            </div>
          );
        })}
        <div className="text-sm">
          <span className="mb-0.5 block text-xs text-muted-foreground md:hidden">{DELIVERY.COL_PEOPLE_SEE}</span>
          {row.mode === DEFAULT_MODE ? (
            <span data-people-see="default" className="text-muted-foreground">
              {DELIVERY.LOCAL_DELIVERY[row.mode]}
            </span>
          ) : (
            <span data-people-see="differs">
              <Chip tone="info">{DELIVERY.LOCAL_DELIVERY[row.mode]}</Chip>
            </span>
          )}
        </div>
      </RadioGroup>
      {resident && (
        <div className="px-6 pb-4">
          <PostureBlock cls={cls} row={row} disabled={disabled} onEdit={onEdit} />
        </div>
      )}
    </div>
  );
}

function PostureBlock({
  cls,
  row,
  disabled,
  onEdit,
}: {
  cls: DeliveryClass;
  row: RowDraft;
  disabled: boolean;
  onEdit: (patch: Partial<RowDraft>) => void;
}) {
  const titleId = `dc-${cls}-posture`;
  const hintId = `dc-${cls}-os-hint`;
  return (
    <div className="mt-3 flex flex-col gap-3 rounded-lg bg-muted/40 p-3">
      <p id={titleId} className="text-xs font-medium text-foreground">
        {DELIVERY.POSTURE_TITLE}
      </p>
      <div role="group" aria-labelledby={titleId} className="flex flex-wrap gap-x-5 gap-y-2">
        <div className="flex items-center gap-2">
          <Checkbox id={`dc-${cls}-mdm`} checked={row.mdm} disabled={disabled} onCheckedChange={(v) => onEdit({ mdm: v === true })} />
          <Label htmlFor={`dc-${cls}-mdm`} className="text-sm">
            {DELIVERY.POSTURE_MDM}
          </Label>
        </div>
        <div className="flex items-center gap-2">
          <Checkbox id={`dc-${cls}-disk`} checked={row.disk} disabled={disabled} onCheckedChange={(v) => onEdit({ disk: v === true })} />
          <Label htmlFor={`dc-${cls}-disk`} className="text-sm">
            {DELIVERY.POSTURE_DISK}
          </Label>
        </div>
      </div>
      <div className="grid gap-3 sm:grid-cols-3">
        {POSTURE_OSES.map((os) => {
          const id = `dc-${cls}-os-${os}`;
          const bad = versionInvalid(row.os[os]);
          return (
            <div key={os} className="flex flex-col gap-1">
              <Label htmlFor={id} className="text-xs">
                {DELIVERY.OS_MIN(DELIVERY.OS_NAME[os])}
              </Label>
              <Input
                id={id}
                value={row.os[os]}
                disabled={disabled}
                inputMode="decimal"
                aria-invalid={bad || undefined}
                aria-describedby={bad ? `${id}-e ${hintId}` : hintId}
                onChange={(e) => onEdit({ os: { ...row.os, [os]: e.target.value } })}
              />
              {bad && (
                <p id={`${id}-e`} className="text-xs text-danger">
                  {DELIVERY.OS_MIN_INVALID}
                </p>
              )}
            </div>
          );
        })}
      </div>
      <p id={hintId} className="text-xs text-muted-foreground">
        {DELIVERY.OS_MIN_HINT}
      </p>
      <p className="text-xs text-muted-foreground">{DELIVERY.POSTURE_REPORTED}</p>
      <p className="text-xs text-muted-foreground">{DELIVERY.RESIDENT_HONESTY}</p>
      <p className="text-xs text-muted-foreground">{DELIVERY.RESIDENT_FLOOR}</p>
    </div>
  );
}
