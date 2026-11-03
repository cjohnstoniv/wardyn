/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings card: the macOS VM settings of the site config, `macos_vm` (types.SiteMacOSVM). The oldest base a run may
// use (`max_base_age_days`, empty is no maximum; the server stores 0 as the same) and the switch for watching
// background runs (`allow_background_watch`, absent reads on, sent explicitly on every save). One Save covers every field; to add a field, add
// its state beside `watch`, its input below, its key in macosVMBody and its term in `dirty`.
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

// The block to send: the stored block with the days set (removed when the field is empty) and the watch switch
// written explicitly, true or false, every time. The server keeps a stored switch when a body names the block
// without it, so only an explicit value can change it. A block with no days still goes out (it names `macos_vm`),
// because an absent block is carried forward by the server.
export function macosVMBody(stored: SiteMacOSVM | undefined, days: number | "", watch = true): SiteMacOSVM {
  const { max_base_age_days: _days, allow_background_watch: _watch, ...rest } = stored ?? {};
  return { ...rest, ...(days !== "" && { max_base_age_days: days }), allow_background_watch: watch };
}

const textOf = (m?: SiteMacOSVM) => (m?.max_base_age_days ? String(m.max_base_age_days) : "");
const watchOf = (m?: SiteMacOSVM) => m?.allow_background_watch !== false;

export function MacOSVMSettingsCard() {
  const [base, setBase] = React.useState<SiteConfig | null>(null);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [saved, setSaved] = React.useState("");
  const [draft, setDraft] = React.useState("");
  const [savedWatch, setSavedWatch] = React.useState(true);
  const [watch, setWatch] = React.useState(true);
  const [state, setState] = React.useState<"loading" | "ready" | "failed">("loading");
  const [saving, setSaving] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  const [savedElsewhere, setSavedElsewhere] = React.useState(false);
  const [staleKept, setStaleKept] = React.useState(false);

  // The 412 path reloads too. It resolves to the card's settings just read, or null if the read failed.
  const load = React.useCallback((): Promise<{ text: string; watch: boolean } | null> => {
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
        setState("ready");
        return { text: next, watch: watchOf(siteConfig.macos_vm) };
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
  const invalid = parsed === null;
  const dirty = draft.trim() !== saved || watch !== savedWatch;

  const save = async () => {
    if (!base || parsed === null) return;
    setSaving(true);
    setRefused(null);
    setSavedElsewhere(false);
    setStaleKept(false);
    try {
      const result = await health.putSiteConfig({ ...base, macos_vm: macosVMBody(base.macos_vm, parsed, watch) }, etag);
      const next = textOf(result.siteConfig.macos_vm);
      setBase(result.siteConfig);
      setEtag(result.etag);
      setSaved(next);
      setDraft(next);
      setSavedWatch(watchOf(result.siteConfig.macos_vm));
      setWatch(watchOf(result.siteConfig.macos_vm));
      toast.success(T.SAVED_TOAST);
    } catch (e) {
      if (e instanceof HttpError && e.status === 412) {
        // The ETag covers the whole site-config document, so another card's save also lands here. Only a change to
        // THIS card's block is "someone else"; otherwise the person's edit goes back on the fresh document.
        const fresh = await load();
        if (fresh && fresh.text === saved && fresh.watch === savedWatch) {
          setDraft(draft);
          setWatch(watch);
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
              aria-invalid={invalid}
              aria-describedby="macos-max-base-age-hint"
              onChange={(e) => setDraft(e.target.value)}
            />
            <span className="text-body text-muted-foreground">{T.UNIT}</span>
          </div>
          <p id="macos-max-base-age-hint" className="mt-1.5 text-xs text-muted-foreground">
            {T.P18}
          </p>
          {invalid && (
            <p role="alert" className="mt-2 text-xs text-danger">
              {T.INVALID}
            </p>
          )}
          <div className="mt-5 flex items-center gap-2.5">
            <Switch checked={watch} onChange={setWatch} label={T.WATCH_LABEL} />
            <span className="text-body font-medium text-foreground">{T.WATCH_LABEL}</span>
          </div>
          <p className="mt-1.5 text-xs text-muted-foreground">{T.WATCH_HINT}</p>
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
