/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The profile editor — the in-place form the Governance screen opens over its
// profiles list (docs/design/governance-mock/index.html, states 2 and 3).
//
// It authors ONE saved object: a name, a ceiling, and the Limits section (three
// launch-mode doors plus, since 0.7.2, three integer ceilings). The ceiling is
// the SHIPPED spec editor (PolicyPanel instance="policies", carrying its own
// templates, field help and SafetyMeter) — this file does not redraw any of it
// (prompt §3, "no second spec editor"), and the limits are a SECTION of the
// same form rather than a second card, because a second card implies a second
// write (§O Q3).
//
// Every product string comes from the copy modules. This file adds none.
import { nonNegativeInt } from "../../../lib/format";
import * as React from "react";
import { Loader2 } from "lucide-react";
import { setup as setupApi } from "../../../lib/api/setup";
import { HttpError } from "../../../lib/api/core";
import { governance as api, isGrantBoundError, type GovernanceLimits, type GovernanceProfile } from "../../../lib/api/governance";
import { getErrorMessage } from "../../../lib/format";
import { GOVERNANCE as GOV, RUN_LIMITS as RL, RUN_LIMIT_UNITS, runLimitUnit } from "../../../lib/governance-copy";
import { PEOPLE } from "../../../lib/people-access-copy";
import type { RunPolicySpec } from "../../../lib/types";
import type { StorageEnforcement } from "../../../lib/api/drives";
import { isUncappedEnforcement } from "../drives/display";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Mono } from "../../wardyn/code-block";
import { Field, Switch } from "../../wardyn/form-primitives";
import { POLICY_TEMPLATES, PolicyPanel, parseSpec } from "../../wardyn/policy-panel";
import { Note, withMono } from "./display";
import { ProfileRubric } from "./profile-rubric";

// A number field renders blank at 0/undefined — 0 IS "unlimited" for all three
// rows below, not a value worth spelling out in the input (storage-tab.tsx's
// numberField precedent).
const numberField = (v: number | undefined): number | "" => (v ? v : "");

// RL-14: the run-limit fields are seconds on the wire (types.RunLimits) but
// the packet's own examples are whole days/hours/minutes, so LimitDurationRow
// below converts between the two rather than asking an admin to type seconds.
const SEC_PER_MINUTE = 60;
const SEC_PER_HOUR = 3600;
const SEC_PER_DAY = 86400;

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
  // The AUTHORING daemon's disk-cap driver (§6.2) — read once, the same call
  // the /providers Storage tab makes, never a laptop's. Absent (older daemon,
  // no runner detected) reads as "can enforce": no warning, and so does every
  // word but `none` — see isUncappedEnforcement, which both surfaces share.
  const [enforcement, setEnforcement] = React.useState<StorageEnforcement | undefined>(undefined);
  React.useEffect(() => {
    let alive = true;
    void setupApi.getSetupStatus().then((s) => {
      if (alive) setEnforcement(s.runner.ephemeral_disk_enforcement);
    });
    return () => {
      alive = false;
    };
  }, []);
  const dockerUncapped = isUncappedEnforcement(enforcement);

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
        {/* The user-drive door (0.7 user drives, §5 #5). Its two strings live
            in governance-prompt.md §7.2 and governance-copy.ts, never in the
            drives module: the security admin's authority over drives is this
            row and nothing else, /drives itself being SUPER. */}
        <LimitRow
          label={GOV.LIMIT_DRIVE_LABEL}
          hint={GOV.LIMIT_DRIVE_HINT}
          checked={!!limits.deny_user_drive}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, deny_user_drive: v }))}
        />
        {/* R4/F032: three integer limits, one row shape (a Switch has no
            number to carry). 0 = unlimited on every one, stated in each
            hint. The ephemeral row (and only it) carries the Docker uncapped
            warning the Storage tab already renders under its own two disk
            fields — same string, never a second copy. */}
        <LimitNumberRow
          id="governance-limit-concurrent"
          label={GOV.LIMIT_CONCURRENT_LABEL}
          hint={GOV.LIMIT_CONCURRENT_HINT}
          value={limits.max_concurrent_runs}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, max_concurrent_runs: v }))}
        />
        <LimitNumberRow
          id="governance-limit-ephemeral"
          label={GOV.LIMIT_EPHEMERAL_LABEL}
          hint={GOV.LIMIT_EPHEMERAL_HINT}
          value={limits.max_ephemeral_disk_mib}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, max_ephemeral_disk_mib: v }))}
          warning={dockerUncapped ? PROVIDERS.DOCKER_UNCAPPED_WARN : undefined}
        />
        <LimitNumberRow
          id="governance-limit-drive-size"
          label={GOV.LIMIT_DRIVE_SIZE_LABEL}
          hint={GOV.LIMIT_DRIVE_SIZE_HINT}
          value={limits.max_drive_size_mib}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, max_drive_size_mib: v }))}
        />
      </section>

      {/* RL-14 (0.8, #579): the lease and wait bounds (long-holds-design.md
          rev 4 §2.2). A separate section from Limits above — those are doors
          and quotas a run either may or may not open; these are TIME bounds,
          and share one gate (user_changes_limits) none of the doors do. */}
      <section className="mt-6">
        <h4 className="text-body font-medium text-foreground">{RL.SECTION_TITLE}</h4>
        <LimitDurationRow
          id="governance-limit-max-end"
          label={RL.MAX_END_LABEL}
          hint={RL.MAX_END_HINT}
          unitSec={SEC_PER_DAY}
          valueSec={limits.max_end_ahead_sec}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, max_end_ahead_sec: v }))}
        />
        <LimitDurationRow
          id="governance-limit-default-end"
          label={RL.DEFAULT_END_LABEL}
          unitSec={SEC_PER_DAY}
          valueSec={limits.default_end_sec}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, default_end_sec: v }))}
        />
        <LimitRow
          label={RL.ALLOW_NO_END_LABEL}
          hint={RL.ALLOW_NO_END_HINT}
          checked={!!limits.allow_no_end}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, allow_no_end: v }))}
        />
        <LimitDurationRow
          id="governance-limit-max-wait"
          label={RL.MAX_WAIT_LABEL}
          hint={RL.MAX_WAIT_HINT}
          unitSec={SEC_PER_HOUR}
          valueSec={limits.max_wait_sec}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, max_wait_sec: v }))}
        />
        <LimitDurationRow
          id="governance-limit-default-wait"
          label={RL.DEFAULT_WAIT_LABEL}
          unitSec={SEC_PER_HOUR}
          valueSec={limits.default_wait_sec}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, default_wait_sec: v }))}
        />
        <LimitRow
          label={RL.USER_CHANGES_LABEL}
          hint={RL.USER_CHANGES_HINT}
          checked={!!limits.user_changes_limits}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, user_changes_limits: v }))}
        />
        <LimitDurationRow
          id="governance-limit-pause-idle"
          label={RL.PAUSE_IDLE_LABEL}
          hint={RL.PAUSE_IDLE_HINT}
          unitSec={SEC_PER_MINUTE}
          valueSec={limits.pause_idle_after_sec}
          disabled={disabled}
          onChange={(v) => setLimits((l) => ({ ...l, pause_idle_after_sec: v }))}
        />
      </section>

      <ProfileRubric
        value={limits.autonomy_rubric ?? {}}
        disabled={disabled}
        onChange={(rubric) => setLimits((l) => ({ ...l, autonomy_rubric: rubric }))}
      />

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
// The switch itself lives in wardyn/form-primitives.tsx, shared across
// screens. This row is still local: it is the LIMITS layout (switch beside a
// label and a sentence), not a form field, and nothing else renders that shape.
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
      <Switch checked={checked} onChange={onChange} disabled={disabled} label={label} className="mt-0.5" />
      <div className="min-w-0">
        <p className="text-body font-medium text-foreground">{label}</p>
        <p className="mt-0.5 max-w-[62ch] text-xs text-muted-foreground">{hint}</p>
      </div>
    </div>
  );
}

// One integer limit (0 = unlimited): a Switch has no number to carry, so
// MaxConcurrentRuns/MaxEphemeralDiskMiB/MaxDriveSizeMiB share this row instead
// of LimitRow's above. `warning` renders only for the ephemeral row, under its
// own input — the Docker uncapped sentence is PROVIDERS.DOCKER_UNCAPPED_WARN,
// never retyped here.
function LimitNumberRow({
  id,
  label,
  hint,
  value,
  disabled,
  onChange,
  warning,
}: {
  id: string;
  label: string;
  hint: React.ReactNode;
  value: number | undefined;
  disabled: boolean;
  onChange: (next: number) => void;
  warning?: React.ReactNode;
}) {
  return (
    <div className="mt-3 border-t border-border pt-3 first-of-type:border-t-0" data-testid={id}>
      <Field label={label} htmlFor={id} hint={hint} className="max-w-[28rem]">
        <Input
          id={id}
          type="number"
          min={0}
          className="max-w-[12rem] font-mono"
          disabled={disabled}
          value={numberField(value)}
          onChange={(e) => onChange(nonNegativeInt(e.target.value))}
        />
      </Field>
      {warning && <p className="mt-1.5 max-w-[62ch] text-meta leading-snug text-warning">{warning}</p>}
    </div>
  );
}

// RL-14: one run-limit duration (0 = unlimited, the same rule LimitNumberRow's
// three rows follow). The wire is seconds; the input takes a whole number of
// the unit picked beside it. The picker opens on the row's own unit (the
// packet's example for that field) unless the stored value is not a whole
// number of it — then on the largest unit it is (runLimitUnit, the chip's
// rule), so the input never shows a rounded or blank value for a stored one.
// Seconds are offered only when a stored value needs them. A hint is optional
// — two of the seven rows (Default end, Default wait) are paired with a row
// just above that already explains the pair.
function LimitDurationRow({
  id,
  label,
  hint,
  valueSec,
  unitSec,
  disabled,
  onChange,
}: {
  id: string;
  label: string;
  hint?: React.ReactNode;
  valueSec: number | undefined;
  unitSec: number;
  disabled: boolean;
  onChange: (nextSec: number) => void;
}) {
  const [unit, setUnit] = React.useState(() =>
    valueSec && valueSec % unitSec !== 0 ? runLimitUnit(valueSec).sec : unitSec,
  );
  const [offerSeconds] = React.useState(unit === 1);
  const count = valueSec ? valueSec / unit : undefined;
  return (
    <div className="mt-3 border-t border-border pt-3 first-of-type:border-t-0" data-testid={id}>
      <Field label={label} htmlFor={id} hint={hint} className="max-w-[28rem]">
        <div className="flex items-center gap-2">
          <Input
            id={id}
            type="number"
            min={0}
            className="max-w-[8rem] font-mono"
            disabled={disabled}
            value={numberField(count)}
            onChange={(e) => onChange(nonNegativeInt(e.target.value) * unit)}
          />
          {/* Changing the unit keeps the number typed: "30", then minutes. */}
          <Select
            value={String(unit)}
            disabled={disabled}
            onValueChange={(v) => {
              setUnit(Number(v));
              if (count) onChange(count * Number(v));
            }}
          >
            <SelectTrigger aria-label={RL.UNIT_PICKER_LABEL(label)} className="w-[8rem]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {RUN_LIMIT_UNITS.filter((u) => u.sec > 1 || offerSeconds).map((u) => (
                <SelectItem key={u.sec} value={String(u.sec)}>
                  {u.many}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </Field>
    </div>
  );
}
