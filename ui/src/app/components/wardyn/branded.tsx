/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Console branding (#1125): everything that DRAWS a brand, loaded only once
// one is set — branding-context.tsx's BrandSlot, through one lazy component
// switching on `part` so the entry chunk carries a single lazy import.
//
// The colours reach the page as ONE <style> element carrying two validated hex
// pairs on --primary/--primary-foreground, per theme — nothing else is
// brandable, so danger/warning/success/info can never be recoloured. An inline
// <style> is what style-src 'unsafe-inline' already admits; the logo and tab
// icon come from 'self'. The CSP is unchanged.
import * as React from "react";
import { cn } from "../ui/utils";
import { Chip } from "./primitives";
import { CONSOLE_VIEW } from "./copy/console-view";
import { asJson, wfetch } from "../../lib/api/core";
import { isHexColour, monogram, productName, type Branding } from "../../lib/branding";
import { BRAND_HEADER } from "../../lib/branding-copy";

// head: colours, tab icon and title (document.head); gate: the sign-in
// lockup; bar: the top-bar wordmark; link: the header's Support link.
export type Part = "head" | "gate" | "bar" | "link";

export default function Branded({ part, brand }: { part: Part; brand: Branding }) {
  switch (part) {
    case "head":
      return <BrandEffects brand={brand} />;
    case "gate":
      // sign-in.tsx's lockup shape.
      return (
        <div className="mb-7 flex flex-col items-center gap-3 text-center">
          <BrandMark brand={brand} className="size-12 text-base" />
          <h1 className="text-xl font-semibold tracking-tight text-foreground">{productName(brand)}</h1>
        </div>
      );
    case "bar":
      // WardynWordmark's shape (logo.tsx), compact="sm".
      return (
        <span className="inline-flex items-center gap-2 select-none">
          <BrandMark brand={brand} />
          <span className="hidden text-base font-semibold tracking-tight text-foreground sm:inline">{productName(brand)}</span>
        </span>
      );
    case "link":
      return <SupportLink brand={brand} />;
  }
}

/**
 * The two primary pairs, or null when any value is not a hex colour. The dark
 * pair is the one the server says is in effect (derived when none was set).
 */
export function brandColourCSS(b: Branding): string | null {
  const light = [b.primary ?? "", b.primary_text ?? ""];
  const dark = [b.dark_primary ?? "", b.dark_primary_text ?? ""];
  if (![...light, ...dark].every(isHexColour)) return null;
  // html:root / html.dark outrank theme.css's :root / .dark whatever the
  // stylesheet order; the dark rule comes second, so it wins in dark mode.
  return (
    `html:root{--primary:${light[0]};--primary-foreground:${light[1]}}` +
    `html.dark{--primary:${dark[0]};--primary-foreground:${dark[1]}}`
  );
}

/**
 * The tab title a brand shows for the console's own: "Wardyn" -> the product
 * name, "Wardyn admin" -> "<name> admin" (the Admin view cue stays). Any other
 * title is left alone.
 */
export function brandTitle(title: string, name: string): string {
  return title === CONSOLE_VIEW.TITLE_USER || title === CONSOLE_VIEW.TITLE_ADMIN
    ? name + title.slice(CONSOLE_VIEW.TITLE_USER.length)
    : title;
}

// Colours, tab icon and tab title while a brand is set; leaving restores all
// three. The title is renamed wherever the console sets it (index.html, the
// shell's view effect) by watching <head>, so those writers stay as they were.
function BrandEffects({ brand }: { brand: Branding }) {
  const css = brandColourCSS(brand);
  React.useEffect(() => {
    if (!css) return;
    const el = document.createElement("style");
    el.dataset.wardynBrand = "";
    el.textContent = css;
    document.head.appendChild(el);
    return () => el.remove();
  }, [css]);
  const icon = brand.icon_url;
  React.useEffect(() => {
    const link = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
    if (!icon || !link) return;
    const href = link.getAttribute("href");
    const type = link.getAttribute("type");
    link.setAttribute("href", icon);
    link.removeAttribute("type"); // the logo may be a PNG; the browser reads the served type
    return () => {
      if (href !== null) link.setAttribute("href", href);
      if (type !== null) link.setAttribute("type", type);
    };
  }, [icon]);
  const name = productName(brand);
  React.useEffect(() => {
    const rename = () => {
      const next = brandTitle(document.title, name);
      if (next !== document.title) document.title = next;
    };
    rename();
    const watch = new MutationObserver(rename);
    watch.observe(document.head, { childList: true, characterData: true, subtree: true });
    return () => {
      watch.disconnect();
      if (document.title.startsWith(name)) document.title = CONSOLE_VIEW.TITLE_USER + document.title.slice(name.length);
    };
  }, [name]);
  return null;
}

/** A brand's mark: its logo, or its initials on the primary colour. */
function BrandMark({ brand, className }: { brand: Branding; className?: string }) {
  if (brand.logo_url) {
    return <img src={brand.logo_url} alt="" className={cn("size-7 rounded-md object-contain", className)} />;
  }
  return (
    <span
      aria-hidden
      className={cn(
        "flex size-7 items-center justify-center rounded-md bg-primary text-xs font-bold text-primary-foreground",
        className,
      )}
    >
      {monogram(brand.org_name ?? "")}
    </span>
  );
}

/**
 * The brand's one header link (B-5), read from the signed-in settings (the
 * anonymous read never carries it): https only — the server refuses anything
 * else, and this re-checks — a new tab, no opener and no referrer.
 */
function SupportLink({ brand }: { brand: Branding }) {
  const [href, setHref] = React.useState("");
  React.useEffect(() => {
    wfetch("/branding/settings")
      .then((r) => asJson<Branding>(r))
      .then((b) => setHref(b.support_url ?? ""))
      .catch(() => setHref(""));
  }, [brand]); // re-read when the brand is saved
  if (!href.startsWith("https://")) return null;
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="rounded-sm focus-visible:outline focus-visible:outline-2 focus-visible:outline-ring"
    >
      <Chip tone="info">{BRAND_HEADER.SUPPORT_CHIP}</Chip>
    </a>
  );
}
