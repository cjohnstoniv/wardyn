/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// How people connect to Azure DevOps (#1428): the three-way choice on the row,
// the longest token life or expiry each choice carries, the organisation
// check, and the refusals an admin can meet (no client secret, a lifespan
// above the organisation's, the token permissions still on the app when Entra
// sign-in is picked, the organisation blocking token creation).
//
// It edits only the row's `entra` block, like entra-editor.tsx does; the
// screen's one Save is still the only write. Every string is ado-pat-copy.ts's.
import * as React from "react";
import { clsx } from "clsx";
import type { ADOEntraConfig, GitProvider } from "../../../lib/api/providers";
import { ADO_PAT } from "../../../lib/ado-pat-copy";
import { adoCapName } from "../../../lib/ado-access-copy";
import { adoTokenMode, formatClock, orgCheckView, patDaysOk, patHoursOk } from "../../../lib/ado-pat-display";
import { useAdoOrgCheck } from "../../../lib/hooks/use-ado-org-check";
import { useAdoPatRefusal } from "../../../lib/hooks/use-ado-pat-refusal";
import type { ADOOrgCheck } from "../../../lib/types/ado-pat";
import { usePrincipal } from "../../wardyn/operator-context";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Chip } from "../../wardyn/primitives";

type Mode = "minted_pat" | "bearer" | "own_pat";

// What the screen tells an Azure DevOps row that the row cannot know itself:
// the message a refused Save carried (for the refusals drawn inline), and how
// to save with a row turned on. providers-screen.tsx provides it, so the rows
// need no prop threaded through the git tab.
export const AdoRowsContext = React.createContext<{
  refusal: string | null;
  saveAndEnable?: (rowId: string) => void;
  saveBlocked: boolean;
}>({ refusal: null, saveBlocked: false });

const MODES: { mode: Mode; name: string; help: string }[] = [
  { mode: "minted_pat", name: ADO_PAT.MODE_MINTED, help: ADO_PAT.MODE_MINTED_HELP },
  { mode: "bearer", name: ADO_PAT.MODE_BEARER, help: ADO_PAT.MODE_BEARER_HELP },
  { mode: "own_pat", name: ADO_PAT.MODE_OWN, help: ADO_PAT.MODE_OWN_HELP },
];

// A whole-number field with its unit and range error. The text is held here so
// a half-typed or out-of-range entry stays on screen with its error; a whole
// number is committed to the row either way, and gitRowInvalid withholds Save
// on one the server would refuse.
function LifetimeField({
  id,
  label,
  unit,
  hint,
  rangeError,
  value,
  disabled,
  valid,
  onCommit,
}: {
  id: string;
  label: string;
  unit: string;
  hint?: string;
  rangeError: string;
  value: number;
  disabled: boolean;
  valid: (text: string) => boolean;
  onCommit: (n: number | undefined) => void;
}) {
  const [text, setText] = React.useState(String(value));
  const bad = !valid(text);
  return (
    <div className="mt-3">
      <Label htmlFor={id} className="text-meta font-medium text-muted-foreground">
        {label}
      </Label>
      <div className="mt-1 flex items-center gap-2">
        <Input
          id={id}
          className="w-24 font-mono"
          inputMode="numeric"
          disabled={disabled}
          aria-invalid={bad}
          value={text}
          onChange={(e) => {
            setText(e.target.value);
            const n = Number(e.target.value);
            onCommit(e.target.value.trim() === "" ? undefined : Number.isInteger(n) ? n : undefined);
          }}
        />
        <span className="text-body text-muted-foreground">{unit}</span>
      </div>
      {bad ? <p className="mt-1 text-meta text-danger">{rangeError}</p> : hint && <p className="mt-1 text-meta text-muted-foreground">{hint}</p>}
    </div>
  );
}

function CheckLine({ tone, children }: { tone: "ok" | "bad" | "warn" | "unknown"; children: React.ReactNode }) {
  const mark = tone === "ok" ? "✓" : tone === "bad" ? "✕" : tone === "unknown" ? "?" : "!";
  return (
    <li className="flex items-start gap-2 text-body">
      <span
        aria-hidden="true"
        className={clsx(
          "mt-px w-4 shrink-0 text-center font-bold",
          tone === "ok" ? "text-success" : tone === "bad" ? "text-danger" : tone === "unknown" ? "text-muted-foreground" : "text-warning",
        )}
      >
        {mark}
      </span>
      <span>{children}</span>
    </li>
  );
}

// The organisation check's answer as the mock's card. A lifespan the check saw
// refused, and a refusal on the create policy, are the row's own alerts (below),
// not lines here.
function OrgCheckCard({ result }: { result: ADOOrgCheck }) {
  const v = orgCheckView(result);
  const clean = v.permissions === "granted" && v.lifespan?.state === "on";
  // "Couldn't tell" is not a warning: nothing is wrong that Wardyn knows of.
  const unsure = v.permissions === "granted" && v.lifespan?.state === "unknown";
  return (
    <div className="mt-3 rounded-lg border border-border p-3" data-testid="ado-org-check">
      <div className="flex items-center justify-between gap-2">
        <h5 className="text-sm font-medium text-foreground">{ADO_PAT.CHECK_TITLE}</h5>
        <Chip tone={clean ? "success" : unsure ? "neutral" : "warning"}>{ADO_PAT.CHECK_CHIP(formatClock(result.checked_at))}</Chip>
      </div>
      <ul className="mt-2 space-y-1.5">
        {v.permissions === "granted" && <CheckLine tone="ok">{ADO_PAT.CHECK_PERMS_OK}</CheckLine>}
        {v.permissions === "missing" && <CheckLine tone="bad">{ADO_PAT.CHECK_PERMS_MISSING}</CheckLine>}
        {v.lifespan?.state === "on" && <CheckLine tone="ok">{ADO_PAT.CHECK_LIFESPAN_ON(v.lifespan.hours)}</CheckLine>}
        {v.lifespan?.state === "off" && <CheckLine tone="warn">{ADO_PAT.CHECK_LIFESPAN_OFF}</CheckLine>}
        {v.lifespan?.state === "unknown" && <CheckLine tone="unknown">{ADO_PAT.CHECK_LIFESPAN_UNKNOWN(v.lifespan.error)}</CheckLine>}
      </ul>
    </div>
  );
}

function Alert({ children }: { children: React.ReactNode }) {
  return (
    <div
      role="alert"
      className="mt-3 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-body text-foreground"
    >
      {children}
    </div>
  );
}

export function AdoTokenMode({
  row,
  operator,
  onUpdate,
  check,
  checking,
  onCheck,
}: {
  row: GitProvider;
  operator: boolean;
  onUpdate: (next: GitProvider) => void;
  /** The last organisation check this visit, null before one has run. */
  check: ADOOrgCheck | null;
  checking: boolean;
  onCheck: () => void;
}) {
  const uid = React.useId();
  // The check runs with the admin's own connection, so "the person" is the admin.
  const person = usePrincipal();
  const { refusal } = React.useContext(AdoRowsContext);
  const cfg: ADOEntraConfig = row.entra ?? { tenant_id: "", client_id: "" };
  const mode = adoTokenMode(row);
  const set = (patch: Partial<ADOEntraConfig>) => onUpdate({ ...row, entra: { ...cfg, ...patch } });
  // bearer is the unset default; writing nothing keeps an untouched row's diff empty.
  const pick = (m: Mode) => set({ token_mode: m === "bearer" ? undefined : m });

  const view = check ? orgCheckView(check) : null;
  // The banner's person: the admin when their own check was refused, else the
  // person the server reports as refused on this row in the last seven days.
  const refused = useAdoPatRefusal(row.id, operator);
  const blockedPerson = view?.blocked ? person : mode === "minted_pat" ? refused?.person : undefined;
  // Entra sign-in while the app still holds the token permissions: known from
  // the last check, or from the server refusing this very Save.
  const bearerBlocked = mode === "bearer" && (view?.permissions === "granted" || refusal?.endsWith(ADO_PAT.BEARER_WITH_TOKEN_PERMS));
  const noSecret = mode === "minted_pat" && refusal?.endsWith(ADO_PAT.NO_CLIENT_SECRET);

  return (
    <div data-testid="ado-token-mode">
      <h4 className="text-sm font-medium text-foreground">{ADO_PAT.SECTION_TITLE}</h4>
      {blockedPerson && (
        <Alert>
          <span>{ADO_PAT.POLICY_BANNER(blockedPerson)}</span>
          {operator && mode !== "bearer" && (
            <div className="mt-2">
              <Button size="sm" variant="outline" onClick={() => pick("bearer")}>
                {ADO_PAT.POLICY_BANNER_BUTTON}
              </Button>
            </div>
          )}
        </Alert>
      )}
      <RadioGroup
        className="mt-2 gap-2"
        value={mode}
        disabled={!operator}
        aria-label={ADO_PAT.SECTION_TITLE}
        onValueChange={(v) => pick(v as Mode)}
      >
        {MODES.map(({ mode: m, name, help }) => (
          <div key={m} className={clsx("rounded-lg border p-3", mode === m ? "border-primary" : "border-border")}>
            <div className="flex items-start gap-2.5">
              <RadioGroupItem id={`${uid}-${m}`} value={m} className="mt-0.5" />
              <div className="min-w-0 flex-1">
                <Label htmlFor={`${uid}-${m}`} className="text-body font-medium text-foreground">
                  {name}
                </Label>
                <p className="mt-0.5 text-meta text-muted-foreground">{help}</p>

                {mode === m && m === "minted_pat" && (
                  <>
                    <p className="mt-2 text-body text-foreground">{ADO_PAT.MINTED_SETUP}</p>
                    <p className="mt-1 text-body text-foreground">{ADO_PAT.MINTED_SETUP_REDIRECT}</p>
                    {noSecret && <p className="mt-2 text-body text-danger">{ADO_PAT.NO_CLIENT_SECRET}</p>}
                    {operator && (
                      <div className="mt-2 flex flex-wrap items-center gap-2">
                        {/* The check acts on the saved, enabled row with the admin's own
                            connection: enable and save it, sign in, then check. */}
                        <Button size="sm" variant="outline" disabled={checking || !!row.disabled} onClick={onCheck}>
                          {ADO_PAT.CHECK_BUTTON}
                        </Button>
                        {check && <span className="text-meta text-muted-foreground">{ADO_PAT.CHECK_LAST(formatClock(check.checked_at))}</span>}
                      </div>
                    )}
                    {check && <OrgCheckCard result={check} />}
                    <LifetimeField
                      id={`${uid}-hours`}
                      label={ADO_PAT.TOKEN_LIFE_LABEL}
                      unit={ADO_PAT.TOKEN_LIFE_UNIT}
                      hint={ADO_PAT.TOKEN_LIFE_HINT}
                      rangeError={ADO_PAT.TOKEN_LIFE_RANGE}
                      value={cfg.pat_max_hours ?? 8}
                      disabled={!operator}
                      valid={patHoursOk}
                      onCommit={(n) => set({ pat_max_hours: n })}
                    />
                    {view?.tooLong && <Alert>{ADO_PAT.LIFESPAN_REFUSAL()}</Alert>}
                  </>
                )}

                {mode === m && m === "bearer" && bearerBlocked && <p className="mt-2 text-body text-danger">{ADO_PAT.BEARER_WITH_TOKEN_PERMS}</p>}

                {mode === m && m === "own_pat" && (
                  <LifetimeField
                    id={`${uid}-days`}
                    label={ADO_PAT.OWN_EXPIRY_LABEL}
                    unit={ADO_PAT.OWN_EXPIRY_UNIT}
                    rangeError={ADO_PAT.OWN_EXPIRY_RANGE}
                    hint={ADO_PAT.OWN_EXPIRY_RANGE}
                    value={cfg.pat_max_days ?? 30}
                    disabled={!operator}
                    valid={patDaysOk}
                    onCommit={(n) => set({ pat_max_days: n })}
                  />
                )}
              </div>
            </div>
          </div>
        ))}
      </RadioGroup>
      <p className="mt-2 rounded-lg bg-muted/40 px-3 py-2 text-body text-muted-foreground">{ADO_PAT.RECOMMENDED_SETTINGS}</p>
    </div>
  );
}

// The row the upgrade converted and switched off: the note that says why, the
// choice (when the row signs in at all), what it may ever do, and the one action
// that saves the choice and turns the row on.
export function AdoConvertedRow({
  row,
  operator,
  onUpdate,
}: {
  row: GitProvider;
  operator: boolean;
  onUpdate: (next: GitProvider) => void;
}) {
  const { saveAndEnable, saveBlocked } = React.useContext(AdoRowsContext);
  const { result, checking, check } = useAdoOrgCheck(row.id);
  const ceiling = (row.entra?.capability_ceiling ?? []).map(adoCapName).join(", ");
  return (
    <div className="space-y-3" data-testid="ado-converted-row">
      <div role="status" className="rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-body text-foreground">
        {ADO_PAT.CONVERTED_NOTE}
      </div>
      {row.entra && <AdoTokenMode row={row} operator={operator} onUpdate={onUpdate} check={result} checking={checking} onCheck={check} />}
      {ceiling && <p className="text-meta text-muted-foreground">{ADO_PAT.CONVERTED_CEILING(ceiling)}</p>}
      <div className="flex justify-end">
        <Button disabled={!operator || saveBlocked || !saveAndEnable} onClick={() => saveAndEnable?.(row.id)}>
          {ADO_PAT.CONVERTED_SAVE_ON}
        </Button>
      </div>
    </div>
  );
}

// An Azure DevOps Server row: no Entra sign-in, so each person adds their own
// token. It is the one choice, so it is stated, not offered.
export function AdoServerRow() {
  return (
    <div className="space-y-1 border-t border-border pt-4" data-testid="ado-server-row">
      <h4 className="text-sm font-medium text-foreground">{ADO_PAT.SECTION_TITLE}</h4>
      <p className="text-body font-medium text-foreground">{ADO_PAT.MODE_OWN}</p>
      <p className="text-meta text-muted-foreground">{ADO_PAT.MODE_OWN_HELP}</p>
      <p className="text-meta text-muted-foreground">{ADO_PAT.OWN_SERVER_NOTE}</p>
    </div>
  );
}
