/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Branding card (#1125, packet B-1): Admin view Settings, super admin only.
// Every check below is also the server's (internal/api/branding.go), which is
// what decides; the card says it live so a Save is never a surprise. The
// preview column draws the header with the draft, next to the security cues
// no brand can touch (B-3).
import * as React from "react";
import { AlertCircle } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { Field, OptionCard } from "../../wardyn/form-primitives";
import { ApprovalStateBadge } from "../../wardyn/primitives";
import { CollapsibleCard } from "../../wardyn/collapsible-card";
import { useThemeName } from "../../wardyn/theme-provider";
import { BrandingContext } from "../../wardyn/branding-context";
import { branding as brandingApi, type BrandingSave } from "../../../lib/api/branding";
import { getErrorMessage } from "../../../lib/format";
import {
  contrastRatio,
  deriveDarkPrimary,
  isHexColour,
  LOGO_MAX_BYTES,
  logoSizeText,
  MIN_CONTRAST,
  monogram,
  productName,
  ratioText,
  type Branding,
  type NameFormat,
} from "../../../lib/branding";
import { BRAND_NAME, BRANDING } from "../../../lib/branding-copy";

interface Draft {
  orgName: string;
  nameFormat: NameFormat;
  primary: string;
  text: string;
  darkOn: boolean;
  darkPrimary: string;
  darkText: string;
  link: string;
  logo: { name: string; size: number; type: string; file: File } | null;
}

function draftFrom(b: Branding | null): Draft {
  return {
    orgName: b?.org_name ?? "",
    nameFormat: b?.name_format ?? "prefix",
    primary: b?.primary ?? "",
    text: b?.primary_text ?? "",
    darkOn: !!b?.dark_custom,
    darkPrimary: b?.dark_custom ? (b.dark_primary ?? "") : "",
    darkText: b?.dark_custom ? (b.dark_primary_text ?? "") : "",
    link: b?.support_url ?? "",
    logo: null,
  };
}

/** A colour pair's live verdict: which field is not a colour, or the ratio. */
function pairCheck(fill: string, text: string) {
  const fillBad = !isHexColour(fill);
  const textBad = !isHexColour(text);
  const ratio = fillBad || textBad ? null : contrastRatio(fill, text);
  return { fillBad, textBad, ratio, low: ratio !== null && ratio < MIN_CONTRAST };
}

function ErrLine({ children }: { children: React.ReactNode }) {
  return (
    <p role="alert" className="flex items-start gap-1.5 text-xs text-danger">
      <AlertCircle className="mt-0.5 size-3.5 shrink-0" />
      <span>{children}</span>
    </p>
  );
}

function ColourField({ id, label, value, bad, onChange, children }: {
  id: string;
  label: string;
  value: string;
  bad: boolean;
  onChange: (v: string) => void;
  children?: React.ReactNode;
}) {
  return (
    <Field label={label} htmlFor={id}>
      <div className="space-y-1.5">
        <div className="flex items-center gap-2">
          <span
            aria-hidden
            className="size-7 shrink-0 rounded-md border border-border bg-surface-2"
            style={bad ? undefined : { background: value }}
          />
          <Input id={id} value={value} aria-invalid={bad || undefined} className="font-mono"
            onChange={(e) => onChange(e.target.value.trim())} />
        </div>
        {bad && <ErrLine>{BRANDING.ERR_COLOR}</ErrLine>}
        {children}
      </div>
    </Field>
  );
}

function ContrastLine({ ratio, low }: { ratio: number | null; low: boolean }) {
  if (ratio === null) return null;
  if (low) return <ErrLine>{BRANDING.ERR_CONTRAST(ratioText(ratio))}</ErrLine>;
  return <p className="text-xs text-muted-foreground">{BRANDING.CONTRAST_OK(ratioText(ratio))}</p>;
}

function readBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result).replace(/^data:[^,]*,/, ""));
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}

export function BrandingCard() {
  // The console's own read (BrandingContext) is the anonymous subset; the card
  // seeds from the signed-in one, which adds the Support link and whether the
  // dark pair was set.
  const [, setConsoleBrand] = React.useContext(BrandingContext);
  const [brand, setBrand] = React.useState<Branding | null>(null);
  React.useEffect(() => {
    brandingApi.getSettings().then((b) => setBrand(b.org_name ? b : null), () => {});
  }, []);
  const [draft, setDraft] = React.useState<Draft>(() => draftFrom(brand));
  const [saving, setSaving] = React.useState(false);
  // Seed from the brand when it arrives, unless the admin has started typing.
  const touched = React.useRef(false);
  React.useEffect(() => {
    if (!touched.current) setDraft(draftFrom(brand));
  }, [brand]);
  const set = (patch: Partial<Draft>) => {
    touched.current = true;
    setDraft((d) => ({ ...d, ...patch }));
  };

  const light = pairCheck(draft.primary, draft.text);
  const dark = draft.darkOn ? pairCheck(draft.darkPrimary, draft.darkText) : null;
  const linkBad = draft.link !== "" && !draft.link.startsWith("https://");
  const logoBad = !!draft.logo && draft.logo.size > LOGO_MAX_BYTES;
  const problems = [light.fillBad, light.textBad || light.low, !!dark?.fillBad, !!dark && (dark.textBad || dark.low), linkBad, logoBad]
    .filter(Boolean).length;
  const canSave = problems === 0 && draft.orgName.trim() !== "" && !saving;

  const save = async () => {
    setSaving(true);
    try {
      const body: BrandingSave = {
        org_name: draft.orgName.trim(), name_format: draft.nameFormat,
        primary: draft.primary, primary_text: draft.text, support_url: draft.link || undefined,
        ...(draft.darkOn ? { dark_primary: draft.darkPrimary, dark_primary_text: draft.darkText } : {}),
      };
      if (draft.logo) body.logo = { content_type: draft.logo.type, data: await readBase64(draft.logo.file) };
      const saved = await brandingApi.save(body);
      touched.current = false;
      setBrand(saved);
      setConsoleBrand(saved);
      toast.success(BRANDING.SAVED);
    } catch (e) {
      toast.error(BRANDING.SAVE_FAILED, { description: getErrorMessage(e) });
    } finally {
      setSaving(false);
    }
  };

  const orgName = draft.orgName.trim();
  const previewBrand: Branding | null = orgName ? { org_name: orgName, name_format: draft.nameFormat } : null;
  const inDark = useThemeName() === "dark";
  const derived = isHexColour(draft.primary) ? deriveDarkPrimary(draft.primary) : null;
  const fill = inDark ? (draft.darkOn ? draft.darkPrimary : derived?.primary) : draft.primary;
  const ink = inDark ? (draft.darkOn ? draft.darkText : derived?.text) : draft.text;
  const swatch = fill && ink && isHexColour(fill) && isHexColour(ink) ? { background: fill, color: ink } : undefined;
  // #1200 compact cards — one line, collapsed or expanded.
  const brandingSummary = brand ? `Custom — ${brand.org_name}` : "Default branding";

  return (
    <CollapsibleCard
      title={BRANDING.TITLE}
      summary={brandingSummary}
      headingId="branding-title"
      aria-labelledby="branding-title"
    >
      <p className="text-body leading-snug text-muted-foreground">{BRANDING.LEDE}</p>
      <div className="mt-4 grid gap-6 md:grid-cols-2">
        <div className="space-y-4">
          <Field label={BRANDING.ORG_NAME_LABEL} htmlFor="brand-org">
            <Input id="brand-org" value={draft.orgName} maxLength={64} onChange={(e) => set({ orgName: e.target.value })} />
          </Field>
          <Field label={BRANDING.NAME_FORMAT_LABEL}>
            <div className="grid gap-2">
              {(["prefix", "suffix"] as const).map((f) => (
                <OptionCard
                  key={f}
                  selected={draft.nameFormat === f}
                  onClick={() => set({ nameFormat: f })}
                  title={orgName ? (f === "prefix" ? BRAND_NAME.PREFIX(orgName) : BRAND_NAME.SUFFIX(orgName)) : (f === "prefix" ? BRANDING.FORMAT_PREFIX_HINT : BRANDING.FORMAT_SUFFIX_HINT)}
                  hint={f === "prefix" ? BRANDING.FORMAT_PREFIX_HINT : BRANDING.FORMAT_SUFFIX_HINT}
                />
              ))}
            </div>
          </Field>
          <ColourField id="brand-primary" label={BRANDING.PRIMARY_LABEL} value={draft.primary} bad={light.fillBad}
            onChange={(primary) => set({ primary })} />
          <ColourField id="brand-text" label={BRANDING.TEXT_LABEL} value={draft.text} bad={light.textBad}
            onChange={(text) => set({ text })}>
            <ContrastLine ratio={light.ratio} low={light.low} />
          </ColourField>
          <Field label={BRANDING.LOGO_LABEL} htmlFor="brand-logo" hint={BRANDING.LOGO_HINT}>
            <Input id="brand-logo" type="file" accept="image/svg+xml,image/png"
              onChange={(e) => {
                const file = e.target.files?.[0];
                set({ logo: file ? { name: file.name, size: file.size, type: file.type, file } : null });
              }} />
          </Field>
          {draft.logo && (
            <div className="space-y-1.5">
              <div className="flex items-center justify-between gap-2 text-xs">
                <span className="truncate font-mono">{draft.logo.name}</span>
                <span className="text-muted-foreground">{logoSizeText(draft.logo.size)}</span>
              </div>
              {logoBad && <ErrLine>{BRANDING.ERR_LOGO(logoSizeText(draft.logo.size))}</ErrLine>}
            </div>
          )}
          <Field label={BRANDING.LINK_LABEL} htmlFor="brand-link" hint={BRANDING.LINK_HINT}>
            <Input id="brand-link" value={draft.link} className="font-mono" aria-invalid={linkBad || undefined}
              onChange={(e) => set({ link: e.target.value.trim() })} />
          </Field>
          {linkBad && <ErrLine>{BRANDING.ERR_LINK}</ErrLine>}
          <div className="flex items-center gap-2">
            <Checkbox id="brand-dark" checked={draft.darkOn} onCheckedChange={(v) => set({ darkOn: v === true })} />
            <Label htmlFor="brand-dark">{BRANDING.DARK_LABEL}</Label>
          </div>
          {dark && (
            <>
              <ColourField id="brand-dark-primary" label={BRANDING.PRIMARY_LABEL} value={draft.darkPrimary}
                bad={dark.fillBad} onChange={(darkPrimary) => set({ darkPrimary })} />
              <ColourField id="brand-dark-text" label={BRANDING.TEXT_LABEL} value={draft.darkText}
                bad={dark.textBad} onChange={(darkText) => set({ darkText })}>
                <ContrastLine ratio={dark.ratio} low={dark.low} />
              </ColourField>
            </>
          )}
          <div className="flex items-center gap-3">
            <Button onClick={() => void save()} disabled={!canSave}>{BRANDING.SAVE}</Button>
            {problems > 0 && (
              <span className="text-meta text-muted-foreground">{problems > 1 ? BRANDING.FIX_MANY : BRANDING.FIX_ONE}</span>
            )}
          </div>
        </div>
        <div className="space-y-3">
          <div className="text-sm font-medium text-foreground">{BRANDING.PREVIEW_TITLE}</div>
          <div data-testid="branding-preview" className="flex items-center gap-2 rounded-lg border border-border bg-card px-3 py-2">
            {draft.logo || !brand?.logo_url ? (
              <span aria-hidden className="flex size-7 items-center justify-center rounded-md bg-primary text-xs font-bold text-primary-foreground" style={swatch}>
                {monogram(orgName)}
              </span>
            ) : (
              <img src={brand.logo_url} alt="" className="size-7 rounded-md object-contain" />
            )}
            <span className="text-base font-semibold tracking-tight text-foreground">{productName(previewBrand)}</span>
            <Button size="sm" className="ml-auto" style={swatch} tabIndex={-1}>New run</Button>
          </div>
          <div className="label-eyebrow">{BRANDING.FIXED_TITLE}</div>
          <div className="flex flex-wrap gap-2">
            {(["PENDING", "APPROVED", "DENIED", "EXPIRED", "CANCELLED"] as const).map((s) => (
              <ApprovalStateBadge key={s} state={s} />
            ))}
          </div>
        </div>
      </div>
    </CollapsibleCard>
  );
}
