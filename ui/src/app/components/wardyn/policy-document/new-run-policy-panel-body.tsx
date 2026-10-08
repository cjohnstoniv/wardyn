/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// NewRunPolicyPanelBody — the body of New Run's Policy panel: the barrier
// slot, the three policy modes, one read-only view of the policy this run will
// get, and, in Custom mode, the source editor.
//
// Reading is the default. "Edit policy" opens the editor on the person's own
// custom source; a default or saved policy is never edited here, only copied
// into a custom one by "Customize for this run". What the read view shows is
// the server's authorized preview, which is never fed back into the editor.
//
// It imports nothing from the New Run screen. The screen owns the draft (mode,
// source string, format), the preview call and the saved-policy list, and
// mounts this with them.
import * as React from "react";
import { Loader2, ShieldCheck } from "lucide-react";
import type { ConfinementClass, RunPolicySpec, SetupModelProvider } from "../../../lib/types";
import type { PolicyPreviewResult } from "../../../lib/types/policy-preview";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
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
import { Button, buttonVariants } from "../../ui/button";
import { RAIL_CHECK } from "../copy";
import { POLICY_DOCUMENT as D } from "../copy/policy-document";
import { POLICY_TEMPLATE_COPY as C } from "../copy/policy-templates";
import { OptionCard } from "../form-primitives";
import { policyTemplates, templateText, type PolicyMode } from "../policy-panel";
import { PushRulesSection } from "../policy-push-rules";
import { templateProviders } from "../policy-template-providers";
import { ToolRulesSection } from "../policy-tool-rules";
import { SectionLabel } from "../primitives";
import { STATES } from "../states";
import { PolicyDocument, type PolicyView } from "./policy-document";
import { PolicyEditor, useEditModeFocus } from "./policy-editor";
import {
  applySpecChange,
  parseSpec,
  specToSource,
  type PolicySourceError,
  type PolicySourceFormat,
} from "./policy-source";

const SOURCE_ID = "policy-spec-run";
const EDIT_ID = "nr-policy-edit";

/** The screen's policy-preview read, as much of it as the read view needs. */
export interface NewRunPolicyPreview {
  /** The last authorized preview, if there is one. It may answer an earlier draft. */
  result: PolicyPreviewResult | null;
  /** A read is in flight. */
  busy: boolean;
  /** `result` answers the current draft and has not aged out. */
  fresh: boolean;
  /** The server's own sentence for a refused or failed read. */
  error?: string | null;
  /** A rate-limited read: the seconds its Retry-After named. */
  rateLimitSeconds?: number | null;
}

export interface NewRunPolicyPanelBodyProps {
  /** The screen's barrier picker, with its own notes. */
  barrier: React.ReactNode;

  mode: PolicyMode;
  /** Switching mode never touches the custom source: the screen keeps it, invalid text and comments included. */
  onModeChange: (mode: PolicyMode) => void;
  /** A gate sentence that belongs beside the mode cards (the one-workspace refusal). */
  modeProblem?: string | null;
  /** Default mode: the governance profile that sets the default, when one does. */
  defaultProfileName?: string;
  /** Default mode: the read of the default policy. A failed read never blocks Launch. */
  defaultStatus: "loading" | "ready" | "error";
  onRetryDefault: () => void;
  /** Saved mode: the screen's picker, with its own loading, empty and deleted states. */
  savedPicker: React.ReactNode;

  /** Custom mode: the authored source exactly as typed, and the format it is in. */
  source: string;
  format: PolicySourceFormat;
  onSourceChange: (source: string) => void;
  /** An explicit conversion: the new format and the source rewritten in it. */
  onFormatChange: (format: PolicySourceFormat, source: string) => void;
  /** Custom mode: the editor is open. A presentation choice; it never submits or discards. */
  editing: boolean;
  onEditingChange: (editing: boolean) => void;

  view: PolicyView;
  onViewChange: (view: PolicyView) => void;
  preview: NewRunPolicyPreview;
  /** The barrier this draft asks for. Shown as a request, never as what a run used. */
  requestedClass?: ConfinementClass;

  /** Default or saved mode: the source policy as this person may read it, or null when it cannot be read. */
  sourceSpec: RunPolicySpec | null;
  /** That read came back with values hidden; customizing then starts from `safeStarter`. */
  sourceRedacted: boolean;
  /** The source a fresh custom policy opens with. */
  safeStarter: string;
  /** The custom source is still what the screen seeded, so replacing it loses nothing. */
  customUntouched: boolean;
  /** The current custom source was seeded from `safeStarter` because its source was hidden. */
  safeCustom?: boolean;
  /** Start a custom policy from this seed: the screen sets mode, source, format and opens the editor. */
  onCustomize: (seed: { source: string; format: "yaml"; safe: boolean }) => void;

  /** Renders "Check again" when provided; the screen owns the call and its result. */
  onCheck?: () => void;
  checkBusy?: boolean;
  checkDisabled?: boolean;

  /** /setup/status model_providers; the template buttons follow them. */
  modelProviders?: readonly SetupModelProvider[];
  /** More controls that write the source through a parse, held with the rest while it is invalid. */
  structuredExtra?: (edit: { spec: RunPolicySpec; onSpecChange: (next: RunPolicySpec) => void }) => React.ReactNode;
}

// Two specs say the same thing when they differ only in key order or in values
// that mean "not set" (null, false, zero, an empty string, list or mapping): the
// server's preview spells every field out, the authored source rarely does.
function canonical(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonical);
  if (value === null || typeof value !== "object") return value;
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(value).sort()) {
    const v = canonical((value as Record<string, unknown>)[key]);
    const unset =
      v === null || v === undefined || v === false || v === 0 || v === "" ||
      (Array.isArray(v) && v.length === 0) ||
      (typeof v === "object" && !Array.isArray(v) && Object.keys(v as object).length === 0);
    if (!unset) out[key] = v;
  }
  return out;
}

export const sameSpec = (a: unknown, b: unknown) => JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));

function Note({ warn, children }: { warn?: boolean; children: React.ReactNode }) {
  return (
    <p role="status" className={warn ? "text-xs text-warning" : "text-xs text-muted-foreground"}>
      {children}
    </p>
  );
}

export function NewRunPolicyPanelBody(props: NewRunPolicyPanelBodyProps) {
  const { mode, source, format, preview, modelProviders } = props;
  const custom = mode === "custom";
  const editing = custom && props.editing;
  const parsed = React.useMemo(() => parseSpec(source, format), [source, format]);
  const invalid = custom && !parsed.ok;
  const [refused, setRefused] = React.useState<PolicySourceError | null>(null);
  const [confirmReplace, setConfirmReplace] = React.useState(false);
  // The replace dialog hands focus to the source, not back to the button it removed.
  const replaced = React.useRef(false);
  const { setEditing, expect } = useEditModeFocus(editing, props.onEditingChange, SOURCE_ID, EDIT_ID);
  const { showSpinner } = useDeferredBusy(mode === "default" && props.defaultStatus === "loading", 1000);
  const templates = React.useMemo(() => policyTemplates(modelProviders), [modelProviders]);
  const providerNames = templateProviders(modelProviders).names;

  const setSource = (next: string) => {
    setRefused(null);
    props.onSourceChange(next);
  };
  const onSpecChange = (next: RunPolicySpec) => {
    if (!parsed.ok) return;
    const edit = applySpecChange(source, format, parsed.spec, next);
    if (edit.ok) setSource(edit.source);
    else setRefused(edit);
  };

  // What the read view shows: the server's preview of this draft, and how far to trust it.
  const result = preview.result;
  let shown: RunPolicySpec | null = null;
  let stale = false;
  const notes: React.ReactNode[] = [];
  if (custom && props.safeCustom) notes.push(<Note key="safe">{D.SAFE_CUSTOM}</Note>);
  if (invalid) {
    // An earlier preview may stay, marked out of date; without one, nothing is invented.
    if (result) {
      shown = result.spec;
      stale = true;
      notes.push(<Note key="invalid" warn>{D.INVALID_PREVIEW}</Note>);
    }
  } else if (props.modeProblem) {
    // The request builder refuses this draft, so there is nothing to preview.
  } else if (preview.error) {
    notes.push(<Note key="error" warn>{preview.error}</Note>);
  } else if (preview.rateLimitSeconds != null) {
    notes.push(<Note key="limit" warn>{D.RATE_LIMIT(preview.rateLimitSeconds)}</Note>);
    if (result) {
      shown = result.spec;
      stale = true;
      notes.push(<Note key="stale" warn>{D.STALE_PREVIEW}</Note>);
    }
  } else if (result) {
    shown = result.spec;
    stale = !preview.fresh;
    notes.push(stale ? <Note key="stale" warn>{D.STALE_PREVIEW}</Note> : <Note key="provisional">{D.PROVISIONAL}</Note>);
  } else if (preview.busy) {
    notes.push(<Note key="checking">{RAIL_CHECK.CHECKING}</Note>);
  }
  // One honest fact: the read view holds things the person did not type.
  const authored = custom ? (parsed.ok ? parsed.spec : null) : props.sourceSpec;
  const merged = !!shown && !!authored && !sameSpec(shown, authored);

  const seed = () => {
    // Only an authorized, unredacted source is copied; a hidden value never becomes editable text.
    const safe = !props.sourceSpec || props.sourceRedacted;
    return { source: safe || !props.sourceSpec ? props.safeStarter : specToSource(props.sourceSpec), format: "yaml" as const, safe };
  };
  const customize = () => {
    expect();
    props.onCustomize(seed());
  };

  return (
    <div className="flex min-w-0 flex-col gap-3" data-testid="nr-policy-panel-body">
      {props.barrier}

      <div className="grid gap-2 [grid-template-columns:repeat(auto-fit,minmax(min(170px,100%),1fr))]">
        <OptionCard
          selected={mode === "default"}
          onClick={() => props.onModeChange("default")}
          title={C.DEFAULT_TITLE}
          hint={props.defaultProfileName ? C.DEFAULT_HINT_PROFILE(props.defaultProfileName) : C.DEFAULT_HINT}
        />
        <OptionCard selected={mode === "saved"} onClick={() => props.onModeChange("saved")} title={C.SAVED_TITLE} hint={C.SAVED_HINT} />
        <OptionCard selected={custom} onClick={() => props.onModeChange("custom")} title={C.CUSTOM_TITLE} hint={C.CUSTOM_HINT} />
      </div>
      {props.modeProblem && (
        <p role="status" className="text-xs text-warning">
          {props.modeProblem}
        </p>
      )}

      {mode === "saved" && props.savedPicker}
      {mode === "default" && (
        <div className="flex flex-col gap-2">
          <SectionLabel>{C.DEFAULT_PREVIEW}</SectionLabel>
          {props.defaultStatus === "loading" && showSpinner && (
            <p role="status" className="flex items-center gap-2 text-xs text-muted-foreground">
              <Loader2 className="size-3.5 animate-spin" aria-hidden />
              {C.DEFAULT_LOADING}
            </p>
          )}
          {props.defaultStatus === "error" && (
            <div role="status" className="flex flex-wrap items-center gap-2">
              <p className="text-xs text-muted-foreground">{C.DEFAULT_UNAVAILABLE}</p>
              <Button type="button" variant="outline" size="sm" onClick={props.onRetryDefault}>
                {STATES.RETRY}
              </Button>
            </div>
          )}
          {props.defaultStatus === "ready" && <p className="text-xs text-muted-foreground">{C.DEFAULT_NOTE}</p>}
        </div>
      )}

      <section aria-labelledby="nr-policy-read-title" className="flex min-w-0 flex-col gap-3">
        <div className="flex flex-col gap-1">
          <h3 id="nr-policy-read-title" className="text-sm font-semibold text-foreground">
            {D.THIS_RUN}
          </h3>
          <p className="text-sm text-muted-foreground">
            {editing ? D.READ_UPDATES : custom ? D.READ_EDIT : D.READ_CUSTOMIZE}
          </p>
          {merged && <p className="text-sm text-muted-foreground">{D.MERGED}</p>}
        </div>
        <PolicyDocument
          spec={shown}
          view={props.view}
          onViewChange={props.onViewChange}
          redacted={!!shown && !!result?.redacted}
          facts={{ requestedClass: props.requestedClass }}
          pending={shown ? result?.pending : undefined}
          stale={stale}
          notes={notes}
          actions={
            <>
              {custom && !editing && (
                <Button id={EDIT_ID} type="button" variant="outline" size="sm" onClick={() => setEditing(true)}>
                  {D.EDIT}
                </Button>
              )}
              {!custom && (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => (props.customUntouched ? customize() : setConfirmReplace(true))}
                >
                  {D.CUSTOMIZE}
                </Button>
              )}
              {props.onCheck && (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={props.onCheck}
                  disabled={props.checkBusy || props.checkDisabled || invalid}
                >
                  <ShieldCheck className="size-4" />
                  {RAIL_CHECK.BUTTON}
                </Button>
              )}
            </>
          }
        />
        {props.onCheck && <p className="text-xs text-muted-foreground">{RAIL_CHECK.HINT}</p>}
      </section>

      {editing && (
        <PolicyEditor
          id={SOURCE_ID}
          source={source}
          format={format}
          parsed={parsed.ok ? { ok: true, value: parsed.spec } : parsed}
          onSourceChange={setSource}
          onFormatChange={(next, text) => {
            setRefused(null);
            props.onFormatChange(next, text);
          }}
          onDone={() => setEditing(false)}
          operationError={refused}
          structured={
            <>
              <div>
                <SectionLabel className="mb-1.5">{C.START}</SectionLabel>
                <div className="flex flex-wrap gap-1.5">
                  {templates.map((t) => (
                    <Button key={t.id} type="button" variant="outline" size="sm" title={t.hint} onClick={() => setSource(templateText(t, format))}>
                      {t.label}
                    </Button>
                  ))}
                </div>
                {providerNames.length > 0 && (
                  <p className="mt-1.5 text-meta text-muted-foreground">{C.FROM_PROVIDERS(providerNames)}</p>
                )}
              </div>
              {parsed.ok && <ToolRulesSection spec={parsed.spec} onSpecChange={onSpecChange} />}
              {parsed.ok && <PushRulesSection spec={parsed.spec} onSpecChange={onSpecChange} />}
              {parsed.ok && props.structuredExtra?.({ spec: parsed.spec, onSpecChange })}
            </>
          }
        />
      )}

      <AlertDialog open={confirmReplace} onOpenChange={setConfirmReplace}>
        <AlertDialogContent
          className="sm:max-w-md"
          onCloseAutoFocus={(e) => {
            if (!replaced.current) return;
            replaced.current = false;
            e.preventDefault();
            document.getElementById(SOURCE_ID)?.focus();
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{D.REPLACE_TITLE}</AlertDialogTitle>
            <AlertDialogDescription>{D.REPLACE_BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel className={buttonVariants({ variant: "ghost" })}>{D.KEEP_CUSTOM}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                replaced.current = true;
                customize();
              }}
            >
              {D.REPLACE_CUSTOM}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
