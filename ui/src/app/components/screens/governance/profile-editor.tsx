/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The profile editor — the in-place form the Governance screen opens over its
// profiles list (docs/design/governance-mock/index.html, states 2 and 3).
//
// It authors ONE saved object: a name, a ceiling, and the two launch-mode
// limits. The ceiling is the SHIPPED spec editor (PolicyPanel instance=
// "policies", carrying its own templates, field help and SafetyMeter) — this
// file does not redraw any of it (prompt §3, "no second spec editor"), and the
// limits are a SECTION of the same form rather than a second card, because a
// second card implies a second write (§O Q3).
//
// Every product string comes from the copy modules. This file adds none.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { governance as api, isGrantBoundError, type GovernanceLimits, type GovernanceProfile } from "../../../lib/api/governance";
import { getErrorMessage } from "../../../lib/format";
import { GOVERNANCE as GOV } from "../../../lib/governance-copy";
import { PEOPLE } from "../../../lib/people-access-copy";
import type { RunPolicySpec } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { cn } from "../../ui/utils";
import { Mono } from "../../wardyn/code-block";
import { Field } from "../../wardyn/form-primitives";
import { POLICY_TEMPLATES, PolicyPanel, parseSpec } from "../../wardyn/policy-panel";
import { Note, withMono } from "./display";

// The prefill for a NEW profile is the panel's own Minimal template — the same
// const policies.tsx's create editor starts from, so there is no second
// hand-maintained starter to drift.
const STARTER_SPEC: RunPolicySpec = POLICY_TEMPLATES.find((t) => t.id === "minimal")!.spec;

const specText = (spec: RunPolicySpec): string => JSON.stringify(spec, null, 2);

export function ProfileEditor({
  profile,
  disabled,
  onCancel,
  onSaved,
}: {
  /** The profile being edited, or null for a new one. */
  profile: GovernanceProfile | null;
  disabled: boolean;
  onCancel: () => void;
  /** Hands the write's OMISSION warnings up: the screen renders them after the
   *  save, non-blocking, which is Q6's adopted variant. */
  onSaved: (warnings: string[]) => void;
}) {
  const [name, setName] = React.useState(profile?.name ?? "");
  const [spec, setSpec] = React.useState(() => specText(profile?.ceiling ?? STARTER_SPEC));
  const [limits, setLimits] = React.useState<GovernanceLimits>(profile?.limits ?? {});
  const [saving, setSaving] = React.useState(false);
  // `title` is set only for the grant-bound refusal: the console contributes a
  // heading over the SERVER's message there (§7.4). Every other failure renders
  // the message alone — a frozen sentence would be less specific than what the
  // server already said.
  const [error, setError] = React.useState<{ title?: string; message: string } | null>(null);

  const save = async () => {
    const parsed = parseSpec(spec);
    if (!parsed.ok) {
      setError({ message: parsed.message });
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const input = { name: name.trim(), ceiling: parsed.spec, limits };
      const res = profile ? await api.updateProfile(profile.id, input) : await api.createProfile(input);
      onSaved(res.warnings);
    } catch (e) {
      // A refusal the SERVER composed is rendered verbatim — it names the
      // failing leg, the grant, the host and the two TTLs, all of which a
      // frozen sentence would have had to drop (§7.4). SAVE_ERROR is for the
      // other failure: no answer at all (the mock's state 3).
      setError(
        isGrantBoundError(e)
          ? { title: GOV.GRANT_BOUND_TITLE, message: getErrorMessage(e) }
          : e instanceof HttpError
            ? { message: e.message }
            : { message: GOV.SAVE_ERROR },
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="border-t border-border px-6 py-5" data-testid="governance-profile-editor">
      <h3 className="text-sm font-medium text-foreground">
        {profile ? GOV.EDITOR_TITLE_EDIT(profile.name) : GOV.EDITOR_TITLE_NEW}
      </h3>

      <div className="mt-4">
        <Field label={GOV.FIELD_NAME} htmlFor="governance-profile-name" hint={GOV.NAME_HINT} required>
          <Input
            id="governance-profile-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={disabled}
            autoComplete="off"
            required
          />
        </Field>
      </div>

      <section className="mt-6">
        <h4 className="text-body font-medium text-foreground">{GOV.CEILING_TITLE}</h4>
        <p className="mt-0.5 max-w-[82ch] text-body text-muted-foreground">{GOV.CEILING_LEAD}</p>
        <div className="mt-3">
          <PolicyPanel instance="policies" value={spec} onChange={setSpec} />
        </div>
        <p className="mt-2 text-xs text-muted-foreground">{GOV.GRADE_NOTE}</p>
      </section>

      <section className="mt-6">
        <h4 className="text-body font-medium text-foreground">{GOV.LIMITS_TITLE}</h4>
        <p className="mt-0.5 max-w-[82ch] text-body text-muted-foreground">{GOV.LIMITS_LEAD}</p>
        <LimitRow
          label={GOV.LIMIT_EXEC_LABEL}
          hint={withMono(GOV.LIMIT_EXEC_HINT)}
          checked={!!limits.deny_task_mode_exec}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, deny_task_mode_exec: v }))}
        />
        <LimitRow
          label={GOV.LIMIT_INTERACTIVE_LABEL}
          hint={GOV.LIMIT_INTERACTIVE_HINT}
          checked={!!limits.deny_interactive}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, deny_interactive: v }))}
        />
      </section>

      {error && (
        <Note tone="red" role="alert">
          {error.title && <b className="font-semibold">{error.title}</b>}
          {/* The server's own prose, verbatim — mono because it names wire
              values (grant kinds, secrets, hosts, TTLs). */}
          <Mono className="text-inherit">{error.message}</Mono>
        </Note>
      )}

      <div className="mt-5 flex justify-end gap-2">
        <Button variant="ghost" onClick={onCancel} disabled={saving}>
          {PEOPLE.CANCEL}
        </Button>
        {/* The surface's ONE `default` button while the editor is open — the
            add-assignment form below collapses and takes its teal with it. */}
        <Button onClick={save} disabled={disabled || saving || !name.trim()}>
          {saving ? <Loader2 className="size-4 animate-spin" /> : null}
          {GOV.SAVE}
        </Button>
      </div>
    </div>
  );
}

// One limit: the switch, its label, and the sentence saying why a ceiling
// cannot reach that launch mode.
//
// ponytail: a local `role="switch"` button, the same ten-line shape
// permissions.tsx's KindRow uses — NOT a shared primitive and NOT a restored
// @radix-ui/react-switch. Hoist the two into wardyn/form-primitives the day a
// third screen needs one; two copies of ten lines is cheaper than a primitive
// nobody else asks for.
function LimitRow({
  label,
  hint,
  checked,
  disabled,
  onChange,
}: {
  label: string;
  hint: React.ReactNode;
  checked: boolean;
  disabled: boolean;
  onChange: (next: boolean) => void;
}) {
  return (
    <div className="mt-3 flex items-start gap-3 border-t border-border pt-3 first-of-type:border-t-0">
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        aria-label={label}
        disabled={disabled}
        onClick={() => onChange(!checked)}
        className={cn(
          "mt-0.5 inline-flex h-[1.15rem] w-8 shrink-0 items-center rounded-full border border-transparent p-[1px] transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
          checked ? "bg-primary" : "bg-muted",
        )}
      >
        <span
          aria-hidden="true"
          className={cn(
            "pointer-events-none block size-4 rounded-full bg-card shadow-sm transition-transform",
            checked ? "translate-x-[calc(100%-2px)]" : "translate-x-0",
          )}
        />
      </button>
      <div className="min-w-0">
        <p className="text-body font-medium text-foreground">{label}</p>
        <p className="mt-0.5 max-w-[62ch] text-xs text-muted-foreground">{hint}</p>
      </div>
    </div>
  );
}
