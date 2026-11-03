/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings card: the macOS VM settings of the site config, `macos_vm` (types.SiteMacOSVM). The oldest base a run may
// use (`max_base_age_days`, empty is no maximum; the server stores 0 as the same), the switch for watching
// background runs (`allow_background_watch`, absent reads on) and the switch for a window on the Mac
// (`allow_attended_window`, absent reads off), both sent explicitly on every save. The kept-machine switch
// (`allow_kept_machines`, off by default) with its two numbers (`kept_machine_idle_days`, 1 to 90, and
// `max_kept_machines_per_person`, 1 to 4) is sent only once one of the three changes: the server carries a stored value
// forward when a body leaves it out. One Save covers every field; to add a field, add its state beside `watch`, its
// input below, its key in macosVMBody and its term in `dirty`.
//
// Saved through PUT /site-config with the GET's ETag as If-Match, the way the Custom components card does, so a
// save can't spread a stale document over a newer one. Super-admin only, by being mounted from AdminSettingsScreen.
import * as React from "react";
import { Loader2, RotateCw } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { health } from "../../../lib/api/health";
import { getErrorMessage } from "../../../lib/format";
import type { SiteConfig, SiteMacOSVM } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { COMPONENTS_ADMIN as C } from "../../wardyn/copy/components-admin";
import { FORM_ACTIONS } from "../../wardyn/copy/form-actions";
import { KEEP_SETTINGS as K } from "../../wardyn/copy/keep-machine";
import { MAC_SETTINGS as T } from "../../wardyn/copy/mac-settings";
import { INLINE_ACTIONS } from "../../wardyn/form-actions";
import { Switch } from "../../wardyn/form-primitives";

// What the field holds: empty, or a positive whole number of days. Anything else is not sent.
export const parseMaxBaseAge = (text: string): number | "" | null => {
  const t = text.trim();
  if (t === "") return "";
  if (!/^[1-9]\d*$/.test(t)) return null;
  const n = Number(t);
  return Number.isSafeInteger(n) ? n : null;
};

const KEPT_IDLE_DEFAULT = 30;
const KEPT_PER_DEFAULT = 1;

// A whole number of days from 1 to 90, or null.
export const parseKeptIdleDays = (text: string): number | null => {
  const t = text.trim();
  if (!/^\d{1,3}$/.test(t)) return null;
  const n = Number(t);
  return n >= 1 && n <= 90 ? n : null;
};

// A whole number of machines from 1 to 4, or null.
export const parseKeptPerPerson = (text: string): number | null => {
  const t = text.trim();
  return /^[1-4]$/.test(t) ? Number(t) : null;
};

export interface KeptSettings {
  allow: boolean;
  idleDays: number;
  perPerson: number;
}

// The block to send: the stored block with the days set (removed when the field is empty) and both switches
// written explicitly, true or false, every time. The server keeps a stored switch when a body names the block
// without it, so only an explicit value can change it. A block with no days still goes out (it names `macos_vm`),
// because an absent block is carried forward by the server.
export function macosVMBody(stored: SiteMacOSVM | undefined, days: number | "", watch = true, attended = false, kept?: KeptSettings): SiteMacOSVM {
  const { max_base_age_days: _days, allow_background_watch: _watch, allow_attended_window: _window, ...rest } = stored ?? {};
  const keptKeys = kept && { allow_kept_machines: kept.allow, kept_machine_idle_days: kept.idleDays, max_kept_machines_per_person: kept.perPerson };
  return { ...rest, ...(days !== "" && { max_base_age_days: days }), allow_background_watch: watch, allow_attended_window: attended, ...keptKeys };
}

const textOf = (m?: SiteMacOSVM) => (m?.max_base_age_days ? String(m.max_base_age_days) : "");
const watchOf = (m?: SiteMacOSVM) => m?.allow_background_watch !== false;
const windowOf = (m?: SiteMacOSVM) => m?.allow_attended_window === true;
const keptOf = (m?: SiteMacOSVM) => m?.allow_kept_machines === true;
const idleOf = (m?: SiteMacOSVM) => String(m?.kept_machine_idle_days || KEPT_IDLE_DEFAULT);
const perOf = (m?: SiteMacOSVM) => String(m?.max_kept_machines_per_person || KEPT_PER_DEFAULT);

// The kept-machine fields as the card holds them: the switch, and each number as typed.
interface KeptFields {
  on: boolean;
  idle: string;
  per: string;
}
const keptFieldsOf = (m?: SiteMacOSVM): KeptFields => ({ on: keptOf(m), idle: idleOf(m), per: perOf(m) });
const sameKept = (a: KeptFields, b: KeptFields) => a.on === b.on && a.idle.trim() === b.idle.trim() && a.per.trim() === b.per.trim();


export function MacOSVMSettingsCard() {
  const [base, setBase] = React.useState<SiteConfig | null>(null);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [saved, setSaved] = React.useState("");
  const [draft, setDraft] = React.useState("");
  const [savedWatch, setSavedWatch] = React.useState(true);
  const [watch, setWatch] = React.useState(true);
  const [savedAttended, setSavedAttended] = React.useState(false);
  const [attended, setAttended] = React.useState(false);
  const [savedKept, setSavedKept] = React.useState<KeptFields>({ on: false, idle: String(KEPT_IDLE_DEFAULT), per: String(KEPT_PER_DEFAULT) });
  const [kept, setKept] = React.useState<KeptFields>(savedKept);
  const [state, setState] = React.useState<"loading" | "ready" | "failed">("loading");
  const [saving, setSaving] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  const [savedElsewhere, setSavedElsewhere] = React.useState(false);
  const [staleKept, setStaleKept] = React.useState(false);

  // The 412 path reloads too. It resolves to the card's settings just read, or null if the read failed.
  const load = React.useCallback((): Promise<{ text: string; watch: boolean; window: boolean; kept: KeptFields } | null> => {
    return health
      .getSiteConfigSnapshot()
      .then(({ siteConfig, etag: tag }) => {
        const next = textOf(siteConfig.macos_vm);
        setBase(siteConfig);
        setEtag(tag);
        setSaved(next);
        setDraft(next);
        setSavedWatch(watchOf(siteConfig.macos_vm));
        setWatch(watchOf(siteConfig.macos_vm));
        setSavedAttended(windowOf(siteConfig.macos_vm));
        setAttended(windowOf(siteConfig.macos_vm));
        setSavedKept(keptFieldsOf(siteConfig.macos_vm));
        setKept(keptFieldsOf(siteConfig.macos_vm));
        setState("ready");
        return { text: next, watch: watchOf(siteConfig.macos_vm), window: windowOf(siteConfig.macos_vm), kept: keptFieldsOf(siteConfig.macos_vm) };
      })
      .catch(() => {
        setState("failed");
        return null;
      });
  }, []);
  React.useEffect(() => {
    void load();
  }, [load]);

  const parsed = parseMaxBaseAge(draft);
  const idleDays = parseKeptIdleDays(kept.idle);
  const perPerson = parseKeptPerPerson(kept.per);
  const baseInvalid = parsed === null;
  const invalid = baseInvalid || idleDays === null || perPerson === null;
  const dirty = draft.trim() !== saved || watch !== savedWatch || attended !== savedAttended || !sameKept(kept, savedKept);

  const save = async () => {
    if (!base || parsed === null || idleDays === null || perPerson === null) return;
    setSaving(true);
    setRefused(null);
    setSavedElsewhere(false);
    setStaleKept(false);
    try {
      const result = await health.putSiteConfig({ ...base, macos_vm: macosVMBody(base.macos_vm, parsed, watch, attended, sameKept(kept, savedKept) ? undefined : { allow: kept.on, idleDays, perPerson }) }, etag);
      const next = textOf(result.siteConfig.macos_vm);
      setBase(result.siteConfig);
      setEtag(result.etag);
      setSaved(next);
      setDraft(next);
      setSavedWatch(watchOf(result.siteConfig.macos_vm));
      setWatch(watchOf(result.siteConfig.macos_vm));
      setSavedAttended(windowOf(result.siteConfig.macos_vm));
      setAttended(windowOf(result.siteConfig.macos_vm));
      setSavedKept(keptFieldsOf(result.siteConfig.macos_vm));
      setKept(keptFieldsOf(result.siteConfig.macos_vm));
      toast.success(T.SAVED_TOAST);
    } catch (e) {
      if (e instanceof HttpError && e.status === 412) {
        // The ETag covers the whole site-config document, so another card's save also lands here. Only a change to
        // THIS card's block is "someone else"; otherwise the person's edit goes back on the fresh document.
        const fresh = await load();
        if (fresh && fresh.text === saved && fresh.watch === savedWatch && fresh.window === savedAttended && sameKept(fresh.kept, savedKept)) {
          setDraft(draft);
          setWatch(watch);
          setAttended(attended);
          setKept(kept);
          setStaleKept(true);
        } else {
          setSavedElsewhere(true);
        }
      } else if (e instanceof HttpError && e.status === 400) {
        setRefused(e.message);
      } else {
        toast.error(C.SETTINGS_SAVE_FAILED, { description: getErrorMessage(e) });
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <CollapsibleCard title={T.TITLE} testId="macos-vm-settings-card">
      {state === "loading" && <Loader2 className="mt-4 size-4 animate-spin text-muted-foreground" />}
      {state === "failed" && (
        <div className="mt-4 flex flex-wrap items-center gap-2">
          <p className="text-sm text-muted-foreground">{C.SETTINGS_LOAD_FAILED}</p>
          <Button variant="outline" size="sm" onClick={() => void load()}>
            <RotateCw className="size-3.5" />
            {C.SETTINGS_RETRY}
          </Button>
        </div>
      )}

      {state === "ready" && (
        <div className="mt-3">
          <label htmlFor="macos-max-base-age" className="text-body font-medium text-foreground">
            {T.P17}
          </label>
          <div className="mt-2 flex items-center gap-2">
            <Input
              id="macos-max-base-age"
              inputMode="numeric"
              autoComplete="off"
              className="w-24 font-mono"
              value={draft}
              disabled={saving}
              aria-invalid={baseInvalid}
              aria-describedby="macos-max-base-age-hint"
              onChange={(e) => setDraft(e.target.value)}
            />
            <span className="text-body text-muted-foreground">{T.UNIT}</span>
          </div>
          <p id="macos-max-base-age-hint" className="mt-1.5 text-xs text-muted-foreground">
            {T.P18}
          </p>
          {baseInvalid && (
            <p role="alert" className="mt-2 text-xs text-danger">
              {T.INVALID}
            </p>
          )}
          <div className="mt-5 flex items-center gap-2.5">
            <Switch disabled={saving} checked={watch} onChange={setWatch} label={T.WATCH_LABEL} />
            <span className="text-body font-medium text-foreground">{T.WATCH_LABEL}</span>
          </div>
          <p className="mt-1.5 text-xs text-muted-foreground">{T.WATCH_HINT}</p>
          <div className="mt-5 flex items-center gap-2.5">
            <Switch disabled={saving} checked={attended} onChange={setAttended} label={T.WINDOW_LABEL} />
            <span className="text-body font-medium text-foreground">{T.WINDOW_LABEL}</span>
          </div>
          <p className="mt-1.5 text-xs text-muted-foreground">{T.WINDOW_HINT}</p>
          <div className="mt-5 flex items-center gap-2.5">
            <Switch disabled={saving} checked={kept.on} onChange={(on) => setKept({ ...kept, on })} label={K.LABEL} />
            <span className="text-body font-medium text-foreground">{K.LABEL}</span>
          </div>
          <p className="mt-1.5 text-xs text-muted-foreground">{K.HINT}</p>
          <KeptNumber id="macos-kept-idle" label={K.IDLE_LABEL} hint={K.IDLE_HINT} unit={T.UNIT} value={kept.idle} dim={!kept.on} disabled={saving}
            invalid={idleDays === null} error={K.IDLE_INVALID} onChange={(idle) => setKept({ ...kept, idle })} />
          <KeptNumber id="macos-kept-per" label={K.PER_LABEL} hint={K.PER_HINT} value={kept.per} dim={!kept.on} disabled={saving}
            invalid={perPerson === null} error={K.PER_INVALID} onChange={(per) => setKept({ ...kept, per })} />
          {savedElsewhere && (
            <p role="status" className="my-3 rounded-md border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
              {C.SETTINGS_SAVED_ELSEWHERE}
            </p>
          )}
          {staleKept && (
            <p role="status" className="my-3 rounded-md border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
              {C.SETTINGS_STALE_KEPT}
            </p>
          )}
          {refused && (
            <p role="alert" className="mt-2 text-xs text-danger">
              {refused}
            </p>
          )}
          <div className={`${INLINE_ACTIONS} pt-3`}>
            <Button onClick={() => void save()} disabled={saving || !dirty || invalid}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : null}
              {FORM_ACTIONS.SAVE}
            </Button>
          </div>
        </div>
      )}
    </CollapsibleCard>
  );
}

// One of the kept-machine numbers. The numbers are dimmed while the switch is off, and still hold what the admin typed.
function KeptNumber({ id, label, hint, unit, value, dim, disabled, invalid, error, onChange }: {
  id: string; label: string; hint: string; unit?: string; value: string; dim: boolean; disabled: boolean; invalid: boolean; error: string; onChange: (v: string) => void;
}) {
  return (
    <div className={`mt-4 ${dim ? "opacity-60" : ""}`}>
      <label htmlFor={id} className="text-body font-medium text-foreground">
        {label}
      </label>
      <div className="mt-2 flex items-center gap-2">
        <Input id={id} inputMode="numeric" autoComplete="off" className="w-24 font-mono" value={value} disabled={disabled} aria-invalid={invalid} aria-describedby={`${id}-hint`} onChange={(e) => onChange(e.target.value)} />
        {unit && <span className="text-body text-muted-foreground">{unit}</span>}
      </div>
      <p id={`${id}-hint`} className="mt-1.5 text-xs text-muted-foreground">
        {hint}
      </p>
      {invalid && (
        <p role="alert" className="mt-2 text-xs text-danger">
          {error}
        </p>
      )}
    </div>
  );
}
