/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings card: whether every desktop view must show the viewer's name and the run id, `desktop_watermark.required`
// (types.SiteDesktopWatermark). Off, each person may turn the mark on for their own view; on, nobody can turn it off.
// One switch, one Save. Off is sent as `desktop_watermark: {}`, which the server stores as no block.
//
// Saved through PUT /site-config with the GET's ETag as If-Match, the way the macOS card does, so a save can't spread a
// stale document over a newer one. Super-admin only, by being mounted from AdminSettingsScreen.
import * as React from "react";
import { Loader2, RotateCw } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { health } from "../../../lib/api/health";
import { getErrorMessage } from "../../../lib/format";
import type { SiteConfig } from "../../../lib/types";
import { Button } from "../../ui/button";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { COMPONENTS_ADMIN as C } from "../../wardyn/copy/components-admin";
import { DESKTOP_WATERMARK as T } from "../../wardyn/copy/desktop-watermark";
import { FORM_ACTIONS } from "../../wardyn/copy/form-actions";
import { INLINE_ACTIONS } from "../../wardyn/form-actions";
import { Switch } from "../../wardyn/form-primitives";
import { SavedElsewhereBanner } from "../../wardyn/saved-elsewhere-banner";

const requiredOf = (cfg: SiteConfig): boolean => cfg.desktop_watermark?.required === true;
// What the card sends for its block: off is an empty block, which the server stores as none.
const blockOf = (required: boolean): NonNullable<SiteConfig["desktop_watermark"]> => (required ? { required: true } : {});

export function DesktopWatermarkCard() {
  const [base, setBase] = React.useState<SiteConfig | null>(null);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [saved, setSaved] = React.useState(false);
  const [required, setRequired] = React.useState(false);
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
        setSaved(requiredOf(snap.siteConfig));
        setRequired(requiredOf(snap.siteConfig));
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

  const dirty = required !== saved;

  const save = async () => {
    if (!base) return;
    setSaving(true);
    setRefused(null);
    setSavedElsewhere(null);
    try {
      let doc = { siteConfig: base, etag };
      for (let attempt = 0; ; attempt++) {
        try {
          const result = await health.putSiteConfig({ ...doc.siteConfig, desktop_watermark: blockOf(required) }, doc.etag);
          setBase(result.siteConfig);
          setEtag(result.etag);
          setSaved(requiredOf(result.siteConfig));
          setRequired(requiredOf(result.siteConfig));
          toast.success(T.SAVED_TOAST);
          return;
        } catch (e) {
          if (!(e instanceof HttpError && e.status === 412)) throw e;
          // The ETag covers the whole site-config document, so another card's save also lands here. Only a change to THIS
          // card's block is "someone else"; otherwise the same save goes once more on the fresh document.
          const fresh = await load();
          if (!fresh || requiredOf(fresh.siteConfig) !== saved || attempt > 0) {
            if (fresh) setSavedElsewhere(JSON.stringify({ desktop_watermark: blockOf(required) }, null, 2));
            return;
          }
          doc = fresh;
          setRequired(required);
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
    <CollapsibleCard title={T.TITLE} testId="desktop-watermark-card">
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
            <Switch disabled={saving} checked={required} onChange={setRequired} label={T.LABEL} describedBy="desktop-watermark-hint" />
            <span className="text-body font-medium text-foreground">{T.LABEL}</span>
          </div>
          <div className="pl-11">
            <p id="desktop-watermark-hint" className="mt-1.5 text-xs text-muted-foreground">
              {T.HINT}
            </p>
            <p className="mt-1.5 text-xs text-muted-foreground">{T.HONEST}</p>
          </div>
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
