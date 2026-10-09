/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A standalone profile's ceiling, inside the profile editor: the policy as it
// reads now, and — on "Edit policy" — the shared source editor under it. The
// ceiling is one authored string the profile editor holds; this file only
// shows it and routes every edit back through a parse.
import * as React from "react";
import { TIER_PICKER } from "../../../lib/tier-picker-copy";
import type { ConfinementClass, SetupModelProvider } from "../../../lib/types";
import { CC_ORDER } from "../../../lib/types";
import { Button } from "../../ui/button";
import { CC_META } from "../../wardyn/cc-meta";
import { POLICY_DOCUMENT as D } from "../../wardyn/copy/policy-document";
import { PolicyDocument, type PolicyView } from "../../wardyn/policy-document/policy-document";
import { OperationError, useEditModeFocus } from "../../wardyn/policy-document/policy-editor";
import {
  setSpecKey,
  type ParsedSpec,
  type PolicySourceError,
  type PolicySourceFormat,
} from "../../wardyn/policy-document/policy-source";
import { FIELD_HELP } from "../../wardyn/policy-field-help";
import { PolicyPanel, parseSpec } from "../../wardyn/policy-panel";
import { Segmented } from "../../wardyn/segmented";

const SOURCE_ID = "policy-spec-policies";
const EDIT_ID = "governance-ceiling-edit";

export function ProfileCeilingPolicy({
  source,
  format,
  onSourceChange,
  onFormatChange,
  disabled,
  modelProviders,
}: {
  /** The ceiling as typed. What is saved is what it parses to at Save. */
  source: string;
  format: PolicySourceFormat;
  onSourceChange: (source: string) => void;
  onFormatChange: (format: PolicySourceFormat) => void;
  disabled: boolean;
  modelProviders?: readonly SetupModelProvider[];
}) {
  const [view, setView] = React.useState<PolicyView>("summary");
  const [editing, setEditingState] = React.useState(false);
  const { setEditing } = useEditModeFocus(editing, setEditingState, SOURCE_ID, EDIT_ID);
  const parsed = React.useMemo(() => parseSpec(source, format), [source, format]);
  return (
    <>
      <AllowedBarriersField source={source} format={format} parsed={parsed} onChange={onSourceChange} disabled={disabled} />
      <PolicyDocument
        className="mt-3"
        spec={parsed.ok ? parsed.spec : null}
        view={view}
        onViewChange={setView}
        source={{ text: source, format }}
        headingLevel={4}
        notes={!parsed.ok && <p className="text-xs text-warning">{D.INVALID_GATE}</p>}
        actions={
          !editing && (
            <Button id={EDIT_ID} type="button" variant="outline" size="sm" onClick={() => setEditing(true)}>
              {D.EDIT}
            </Button>
          )
        }
      />
      {editing && (
        <div className="mt-3">
          <PolicyPanel
            instance="policies"
            value={source}
            onChange={onSourceChange}
            format={format}
            onFormatChange={onFormatChange}
            onDone={() => setEditing(false)}
            modelProviders={modelProviders}
          />
        </div>
      )}
    </>
  );
}

// #1200 §3a — "Allowed barriers": a labelled radio over the SAME
// min_confinement_class field the policy source carries, rather than a second,
// competing floor. Reads the CURRENT floor out of the parsed source (CC1 — the
// weakest tier — when the field is absent or unparseable, which reads
// identically to "no floor"), and on change rewrites just that one key in the
// text, so the two controls can never disagree and the rest of the source,
// comments included, stays as typed.
function AllowedBarriersField({
  source,
  format,
  parsed,
  onChange,
  disabled,
}: {
  source: string;
  format: PolicySourceFormat;
  parsed: ParsedSpec;
  onChange: (next: string) => void;
  disabled: boolean;
}) {
  const raw = parsed.ok ? (parsed.spec as { min_confinement_class?: unknown }).min_confinement_class : undefined;
  const floor: ConfinementClass = typeof raw === "string" && (CC_ORDER as string[]).includes(raw) ? (raw as ConfinementClass) : "CC1";
  // A refused write is said here, beside the control that asked, for as long as
  // the source it was refused on is the one on screen.
  const [refused, setRefused] = React.useState<{ source: string; error: PolicySourceError } | null>(null);

  const setFloor = (cc: ConfinementClass) => {
    const edited = setSpecKey(source, format, "min_confinement_class", cc);
    if (edited.ok) onChange(edited.source);
    else setRefused({ source, error: edited });
  };

  return (
    <div className="mt-3 rounded-lg border border-border bg-surface-2 p-3">
      <div className="flex items-center gap-2">
        <span className="text-xs font-medium text-foreground">{TIER_PICKER.ALLOWED_BARRIERS_LABEL}</span>
      </div>
      <p className="mt-0.5 text-xs text-muted-foreground">{FIELD_HELP.min_confinement_class.what}</p>
      <div className="mt-2">
        <Segmented
          value={floor}
          disabled={disabled || !parsed.ok}
          onChange={setFloor}
          options={CC_ORDER.map((cc) => ({ value: cc, label: CC_META[cc].label }))}
        />
      </div>
      <p className="mt-2 text-xs text-muted-foreground">{TIER_PICKER.ALLOWED_BARRIERS_SUMMARY(floor)}</p>
      {refused?.source === source && <OperationError error={refused.error} className="mt-2" />}
    </div>
  );
}
