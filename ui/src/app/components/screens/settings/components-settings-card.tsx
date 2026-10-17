/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Settings card: the organisation's custom-component policy, the `components`
// block of the site config (types.ComponentSettings): the autonomy cap for
// runs that use a component the launcher defined, whether env-var and file
// delivery are allowed, and whether a component's header credential keeps the
// strongest-sandbox floor. "Who may define their own components" is not a
// site-config field: it is the `custom_component` feature rule on Permissions,
// so this card links there rather than holding a second copy of that switch.
//
// Saved through PUT /site-config with the GET's ETag as If-Match, like the
// sign-in help card, so a save can't spread a stale document over a newer one.
// Super-admin only, by being mounted from AdminSettingsScreen alone.
import * as React from "react";
import { Loader2, RotateCw } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { health } from "../../../lib/api/health";
import { getErrorMessage } from "../../../lib/format";
import type { ComponentSettings, SiteConfig } from "../../../lib/types";
import { Button } from "../../ui/button";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { COMPONENTS_ADMIN as T } from "../../wardyn/copy/components-admin";
import { Switch } from "../../wardyn/form-primitives";

type Cap = NonNullable<ComponentSettings["autonomy_cap"]>;

const CAPS: { value: Cap; label: string; hint: string }[] = [
  { value: "", label: T.CAP_NONE, hint: T.CAP_NONE_HINT },
  { value: "L1", label: T.CAP_L1, hint: T.CAP_L1_HINT },
  { value: "L0", label: T.CAP_L0, hint: T.CAP_L0_HINT },
];

// The block to send: only what differs from the default, so an all-default
// choice sends an empty block, which the server stores as none.
export function settingsBody(d: { cap: Cap; residentAllowed: boolean; vault: boolean }): ComponentSettings {
  return {
    ...(d.cap ? { autonomy_cap: d.cap } : {}),
    ...(d.residentAllowed ? {} : { deny_resident_delivery: true }),
    ...(d.vault ? { require_vault_for_credentials: true } : {}),
  };
}

const draftOf = (c?: ComponentSettings) => ({
  cap: c?.autonomy_cap ?? "",
  residentAllowed: !c?.deny_resident_delivery,
  vault: !!c?.require_vault_for_credentials,
});

function summaryOf(d: ReturnType<typeof draftOf>): string {
  return [
    T.SUMMARY_CAP[d.cap],
    !d.residentAllowed && T.SUMMARY_RESIDENT_OFF,
    d.vault && T.SUMMARY_VAULT_ON,
  ]
    .filter(Boolean)
    .join(" · ");
}

function Row({ label, hint, children }: { label: string; hint: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 py-3">
      <div className="max-w-[60ch]">
        <p className="text-body font-medium text-foreground">{label}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{hint}</p>
      </div>
      {children}
    </div>
  );
}

export function ComponentsSettingsCard() {
  const navigate = useNavigate();
  const [base, setBase] = React.useState<SiteConfig | null>(null);
  const [etag, setEtag] = React.useState<string | null>(null);
  const [saved, setSaved] = React.useState(draftOf());
  const [draft, setDraft] = React.useState(draftOf());
  const [state, setState] = React.useState<"loading" | "ready" | "failed">("loading");
  const [saving, setSaving] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);
  const [savedElsewhere, setSavedElsewhere] = React.useState(false);

  // The 412 path reloads too, which also drops the stale draft: the three settings are one block.
  const load = React.useCallback(() => {
    return health
      .getSiteConfigSnapshot()
      .then(({ siteConfig, etag: tag }) => {
        const next = draftOf(siteConfig.components);
        setBase(siteConfig);
        setEtag(tag);
        setSaved(next);
        setDraft(next);
        setState("ready");
      })
      .catch(() => setState("failed"));
  }, []);
  React.useEffect(() => {
    void load();
  }, [load]);

  const dirty = draft.cap !== saved.cap || draft.residentAllowed !== saved.residentAllowed || draft.vault !== saved.vault;

  const save = async () => {
    if (!base) return;
    setSaving(true);
    setRefused(null);
    setSavedElsewhere(false);
    try {
      const result = await health.putSiteConfig({ ...base, components: settingsBody(draft) }, etag);
      const next = draftOf(result.siteConfig.components);
      setBase(result.siteConfig);
      setEtag(result.etag);
      setSaved(next);
      setDraft(next);
      toast.success(T.SETTINGS_SAVED_TOAST);
    } catch (e) {
      if (e instanceof HttpError && e.status === 412) {
        await load();
        setSavedElsewhere(true);
      } else if (e instanceof HttpError && e.status === 400) {
        setRefused(e.message);
      } else {
        toast.error(T.SETTINGS_SAVE_FAILED, { description: getErrorMessage(e) });
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <CollapsibleCard
      title={T.SETTINGS_TITLE}
      summary={state === "ready" ? summaryOf(saved) : undefined}
      testId="components-settings-card"
    >
      <p className="text-body leading-snug text-muted-foreground">{T.SETTINGS_LEAD}</p>

      {state === "loading" && <Loader2 className="mt-4 size-4 animate-spin text-muted-foreground" />}
      {state === "failed" && (
        <div className="mt-4 flex flex-wrap items-center gap-2">
          <p className="text-sm text-muted-foreground">{T.SETTINGS_LOAD_FAILED}</p>
          <Button variant="outline" size="sm" onClick={() => void load()}>
            <RotateCw className="size-3.5" />
            {T.SETTINGS_RETRY}
          </Button>
        </div>
      )}

      {state === "ready" && (
        <div className="mt-3 divide-y divide-border">
          <fieldset className="py-3">
            <legend className="text-body font-medium text-foreground">{T.CAP_LABEL}</legend>
            <p className="mt-0.5 text-xs text-muted-foreground">{T.CAP_HINT}</p>
            <RadioGroup
              aria-label={T.CAP_LABEL}
              value={draft.cap || "none"}
              onValueChange={(v) => setDraft({ ...draft, cap: v === "none" ? "" : (v as Cap) })}
              className="mt-3 gap-3"
            >
              {CAPS.map((c) => (
                <div key={c.value || "none"} className="flex items-start gap-2 text-body">
                  <RadioGroupItem id={`components-cap-${c.value || "none"}`} value={c.value || "none"} className="mt-0.5" />
                  <label htmlFor={`components-cap-${c.value || "none"}`} className="cursor-pointer">
                    <span className="block text-foreground">{c.label}</span>
                    <span className="block text-xs text-muted-foreground">{c.hint}</span>
                  </label>
                </div>
              ))}
            </RadioGroup>
          </fieldset>

          <Row label={T.RESIDENT_LABEL} hint={T.RESIDENT_HINT}>
            <Switch checked={draft.residentAllowed} onChange={(v) => setDraft({ ...draft, residentAllowed: v })} label={T.RESIDENT_LABEL} />
          </Row>
          <Row label={T.VAULT_LABEL} hint={T.VAULT_HINT}>
            <Switch checked={draft.vault} onChange={(v) => setDraft({ ...draft, vault: v })} label={T.VAULT_LABEL} />
          </Row>

          {savedElsewhere && (
            <p role="status" className="my-3 rounded-md border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
              {T.SETTINGS_SAVED_ELSEWHERE}
            </p>
          )}
          {refused && (
            <p role="alert" className="py-3 text-xs text-danger">
              {refused}
            </p>
          )}
          <div className="flex justify-end py-3">
            <Button variant="outline" size="sm" onClick={() => void save()} disabled={saving || !dirty}>
              {saving ? <Loader2 className="size-4 animate-spin" /> : null}
              {T.SETTINGS_SAVE}
            </Button>
          </div>

          <Row label={T.WHO_LABEL} hint={T.WHO_BODY}>
            <Button variant="outline" size="sm" onClick={() => navigate("/admin/permissions")}>
              {T.WHO_CTA}
            </Button>
          </Row>
          <Row label={T.MANAGE_LABEL} hint={T.MANAGE_BODY}>
            <Button variant="outline" size="sm" onClick={() => navigate("/admin/components")}>
              {T.MANAGE_CTA}
            </Button>
          </Row>
        </div>
      )}
    </CollapsibleCard>
  );
}
