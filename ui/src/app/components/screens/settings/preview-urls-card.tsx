/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings card: whether a run's ports may have preview URLs, and the widest one may be (`features.preview_urls`, types.PreviewURLsFeature).
// One switch, one choice of ceiling (Private or Organisation), one Save. The ceiling is dimmed while the switch is off and keeps its value.
//
// Saved through PUT /site-config with the GET's ETag as If-Match, the way the desktop watermark card does, so a save can't
// spread a stale document over a newer one. Only features.preview_urls is written: every other feature switch is sent back as read.
// The server's refusal of a save (the gateway is off, Organisation needs per-run hosts) is shown as it came. Super-admin
// only, by being mounted from AdminSettingsScreen.
import * as React from "react";
import { Loader2, RotateCw } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { health } from "../../../lib/api/health";
import { getErrorMessage } from "../../../lib/format";
import type { SiteConfig } from "../../../lib/types";
import { Button } from "../../ui/button";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { cn } from "../../ui/utils";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { COMPONENTS_ADMIN as C } from "../../wardyn/copy/components-admin";
import { PREVIEW_URLS as T } from "../../wardyn/copy/preview-urls";
import { RUN_PREVIEWS } from "../../wardyn/copy/run-previews";
import { FORM_ACTIONS } from "../../wardyn/copy/form-actions";
import { INLINE_ACTIONS } from "../../wardyn/form-actions";
import { Switch } from "../../wardyn/form-primitives";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";

type Ceiling = "private" | "org";
type Setting = { enabled: boolean; ceiling: Ceiling };

// A ceiling the card does not offer (public, which this release does not serve) reads as the widest it does.
const settingOf = (cfg: SiteConfig): Setting => {
  const p = cfg.features?.preview_urls;
  return { enabled: p?.enabled === true, ceiling: p?.max_visibility === "org" || p?.max_visibility === "public" ? "org" : "private" };
};
const same = (a: Setting, b: Setting) => a.enabled === b.enabled && a.ceiling === b.ceiling;
// What the card sends for its key: the switch and the ceiling, both written, so an off switch keeps the ceiling.
const blockOf = (s: Setting): NonNullable<NonNullable<SiteConfig["features"]>["preview_urls"]> => ({ enabled: s.enabled, max_visibility: s.ceiling });
const withBlock = (cfg: SiteConfig, s: Setting): SiteConfig => ({ ...cfg, features: { ...cfg.features, preview_urls: blockOf(s) } });

const CEILINGS: { value: Ceiling; title: string; body: string }[] = [
  { value: "private", title: RUN_PREVIEWS.PRIVATE, body: T.CEILING_PRIVATE },
  { value: "org", title: RUN_PREVIEWS.ORG, body: T.CEILING_ORG },
];

export function PreviewURLsCard() {
  const [base, setBase] = React.useState<SiteConfig | null>(null);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [saved, setSaved] = React.useState<Setting>({ enabled: false, ceiling: "private" });
  const [setting, setSetting] = React.useState<Setting>({ enabled: false, ceiling: "private" });
  const [state, setState] = React.useState<"loading" | "ready" | "failed">("loading");
  const [saving, setSaving] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  // The person's block as they left it, once a 412 shows someone else's save.
  const [savedElsewhere, setSavedElsewhere] = React.useState<string | null>(null);

  // The 412 path reloads too. It resolves to the document just read, or null if the read failed.
  const load = React.useCallback((): Promise<{ siteConfig: SiteConfig; etag: string | null } | null> => {
    return health
      .getSiteConfigSnapshot()
      .then((snap) => {
        setBase(snap.siteConfig);
        setEtag(snap.etag);
        setSaved(settingOf(snap.siteConfig));
        setSetting(settingOf(snap.siteConfig));
        setState("ready");
        return snap;
      })
      .catch(() => {
        setState("failed");
        return null;
      });
  }, []);
  React.useEffect(() => {
    void load();
  }, [load]);

  const dirty = !same(setting, saved);

  const save = async () => {
    if (!base) return;
    setSaving(true);
    setRefused(null);
    setSavedElsewhere(null);
    try {
      let doc = { siteConfig: base, etag };
      for (let attempt = 0; ; attempt++) {
        try {
          const result = await health.putSiteConfig(withBlock(doc.siteConfig, setting), doc.etag);
          setBase(result.siteConfig);
          setEtag(result.etag);
          setSaved(settingOf(result.siteConfig));
          setSetting(settingOf(result.siteConfig));
          toast.success(T.SAVED_TOAST);
          return;
        } catch (e) {
          if (!(e instanceof HttpError && e.status === 412)) throw e;
          // The ETag covers the whole site-config document, so another card's save also lands here. Only a change to THIS
          // card's key is "someone else"; otherwise the same save goes once more on the fresh document.
          const fresh = await load();
          if (!fresh || !same(settingOf(fresh.siteConfig), saved) || attempt > 0) {
            if (fresh) setSavedElsewhere(JSON.stringify({ features: { preview_urls: blockOf(setting) } }, null, 2));
            return;
          }
          doc = fresh;
          setSetting(setting);
        }
      }
    } catch (e) {
      if (e instanceof HttpError && e.status === 400) setRefused(e.message);
      else toast.error(C.SETTINGS_SAVE_FAILED, { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <CollapsibleCard title={T.TITLE} testId="preview-urls-card">
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
          <div className="mt-5 flex items-center gap-3">
            <Switch disabled={saving} checked={setting.enabled} onChange={(enabled) => setSetting((s) => ({ ...s, enabled }))} label={T.LABEL} describedBy="preview-urls-hint" />
            <span className="text-body font-medium text-foreground">{T.LABEL}</span>
          </div>
          <div className="pl-11">
            <p id="preview-urls-hint" className="mt-1.5 text-xs text-muted-foreground">
              {T.HINT}
            </p>
          </div>
          <p id="preview-urls-ceiling" className="mt-5 text-body font-medium text-foreground">
            {T.CEILING_LABEL}
          </p>
          <RadioGroup
            value={setting.ceiling}
            onValueChange={(v) => setSetting((s) => ({ ...s, ceiling: v as Ceiling }))}
            disabled={!setting.enabled || saving}
            aria-labelledby="preview-urls-ceiling"
            className="mt-2 gap-2"
          >
            {CEILINGS.map((o) => (
              <label
                key={o.value}
                className={cn(
                  "flex gap-3 rounded-lg border border-border bg-card p-3 text-left",
                  "has-[[data-state=checked]]:border-primary has-[[data-state=checked]]:bg-primary/10",
                  setting.enabled ? "cursor-pointer" : "cursor-not-allowed opacity-60",
                )}
              >
                <RadioGroupItem value={o.value} aria-label={o.title} aria-describedby={`preview-urls-${o.value}`} className="mt-0.5" />
                <span className="flex min-w-0 flex-1 flex-col gap-1">
                  <span className="text-sm font-medium text-foreground">{o.title}</span>
                  <span id={`preview-urls-${o.value}`} className="text-xs leading-snug text-muted-foreground">
                    {o.body}
                  </span>
                </span>
              </label>
            ))}
          </RadioGroup>
          <p className="mt-3 text-xs text-muted-foreground">{T.HONEST}</p>
          {refused && (
            <p role="alert" className="mt-2 text-xs text-danger">
              {refused}
            </p>
          )}
          {savedElsewhere !== null && (
            <div className="my-3">
              <SavedElsewhereBanner reloaded documentText={savedElsewhere} onDiscard={() => setSavedElsewhere(null)} />
            </div>
          )}
          <div className={`${INLINE_ACTIONS} pt-3`}>
            <Button onClick={() => void save()} disabled={saving || !dirty}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : null}
              {FORM_ACTIONS.SAVE}
            </Button>
          </div>
        </div>
      )}
    </CollapsibleCard>
  );
}
