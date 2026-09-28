/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: { health: (...a: unknown[]) => healthMock(...a) },
}));

import { ThemeProvider } from "./theme-provider";
import { BrandingProvider } from "./branding-context";
import { brandColourCSS, brandTitle } from "./branded";
import { SignIn } from "../screens/sign-in";
import { TopBar } from "../screens/top-bar";
import type { ShellMeta } from "../screens/app-shell";
import type { Branding } from "../../lib/branding";

// Captured from the console BEFORE #1125 (origin/main a7553fc47): the top-bar
// wordmark and the sign-in lockup, unbranded. An unbranded console must render
// these byte for byte (#1125 done-when: "the unbranded console is
// byte-identical to today's").
const WORDMARK_BEFORE = "<span class=\"inline-flex items-center gap-2 select-none\"><svg viewBox=\"0 0 32 32\" class=\"size-7\" fill=\"none\" xmlns=\"http://www.w3.org/2000/svg\" aria-hidden=\"true\"><path d=\"M16 2.5 4 7v8.2c0 7.4 5 12.2 12 14.3 7-2.1 12-6.9 12-14.3V7L16 2.5Z\" class=\"fill-foreground/15 stroke-foreground\" stroke-width=\"1.6\"></path><path d=\"M11 16.2l3.4 3.4L21.5 12\" class=\"stroke-foreground\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\"></path></svg><span class=\"text-base font-semibold tracking-tight text-foreground hidden sm:inline\">Wardyn</span></span>";
const LOCKUP_BEFORE = "<div class=\"mb-7 flex flex-col items-center gap-3 text-center\"><div class=\"flex size-12 items-center justify-center rounded-full border border-primary/25 bg-primary/12\"><svg xmlns=\"http://www.w3.org/2000/svg\" width=\"24\" height=\"24\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\" stroke-linecap=\"round\" stroke-linejoin=\"round\" class=\"lucide lucide-shield-check size-6 text-primary\" aria-hidden=\"true\"><path d=\"M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z\"></path><path d=\"m9 12 2 2 4-4\"></path></svg></div><h1 class=\"text-xl font-semibold tracking-tight text-foreground\">Wardyn</h1></div>";

const BRAND: Branding = {
  org_name: "Example Corp",
  name_format: "prefix",
  primary: "#7c3aed",
  primary_text: "#ffffff",
  dark_primary: "#9058f0",
  dark_primary_text: "#0a0a0a",
  icon_url: "/api/v1/branding/logo?v=abc",
};

// What the daemon answers: GET /branding (anonymous) and /branding/settings.
let publicRead: () => Promise<Branding> = async () => ({});
let settingsRead: () => Promise<Branding> = async () => ({});
const reads: string[] = [];
beforeEach(() => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const path = new URL(String(input), "http://console.test").pathname;
    reads.push(path);
    const read = path === "/api/v1/branding" ? publicRead : path === "/api/v1/branding/settings" ? settingsRead : null;
    if (!read) return new Response("{}", { status: 404 });
    try {
      return Response.json(await read());
    } catch {
      return new Response("{}", { status: 500 });
    }
  });
  healthMock.mockResolvedValue({ status: "ok", sso: false, admin_token: true });
  document.title = "Wardyn"; // index.html's
});

afterEach(() => {
  vi.restoreAllMocks();
  reads.length = 0;
  publicRead = async () => ({});
  settingsRead = async () => ({});
});

function renderSignIn(provider = true) {
  const page = (
    <ThemeProvider>
      <SignIn onSignIn={() => {}} />
    </ThemeProvider>
  );
  return render(provider ? <BrandingProvider>{page}</BrandingProvider> : page);
}

const meta = {
  trustDomain: "wardyn.local", identityProvider: "embedded", principal: "admin-token", email: "", name: "",
  method: "token", resolved: true, identityResolved: true, operator: true, securityOperator: true, role: "admin",
  userViewTypes: [],
} as unknown as ShellMeta;

function renderTopBar(provider = true) {
  const bar = <TopBar onSignOut={() => {}} meta={meta} pendingApprovals={0} attentionCount={0} onNewRun={() => {}} />;
  return render(
    <MemoryRouter>
      <ThemeProvider>{provider ? <BrandingProvider>{bar}</BrandingProvider> : bar}</ThemeProvider>
    </MemoryRouter>,
  );
}

// The header's home link wraps the wordmark.
const wordmarkHTML = () => screen.getByRole("banner").querySelector("a")!.innerHTML;

function favicon() {
  let link = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
  if (!link) {
    link = document.createElement("link");
    link.rel = "icon";
    link.type = "image/svg+xml";
    link.setAttribute("href", "/favicon.svg");
    document.head.appendChild(link);
  }
  return link;
}

describe("unbranded console — byte-identical to before #1125", () => {
  it("the top bar with no provider at all", () => {
    renderTopBar(false);
    expect(wordmarkHTML()).toBe(WORDMARK_BEFORE);
    expect(screen.queryByRole("link", { name: "Support" })).toBeNull();
  });

  it("the top bar when the read answers {}", async () => {
    renderTopBar();
    await waitFor(() => expect(reads).toContain("/api/v1/branding"));
    expect(wordmarkHTML()).toBe(WORDMARK_BEFORE);
    expect(screen.queryByRole("link", { name: "Support" })).toBeNull();
    expect(reads).not.toContain("/api/v1/branding/settings");
  });

  it("the sign-in lockup, tab title, icon and colours when the read answers {}", async () => {
    const link = favicon();
    renderSignIn();
    const h1 = await screen.findByRole("heading", { level: 1 });
    await waitFor(() => expect(reads).toContain("/api/v1/branding"));
    expect(h1.parentElement!.outerHTML).toBe(LOCKUP_BEFORE);
    expect(document.querySelector("style[data-wardyn-brand]")).toBeNull();
    expect(link.getAttribute("href")).toBe("/favicon.svg");
    expect(link.getAttribute("type")).toBe("image/svg+xml");
    expect(document.title).toBe("Wardyn");
  });

  it("a failed read is unbranded too", async () => {
    publicRead = async () => {
      throw new Error("down");
    };
    renderSignIn();
    const h1 = await screen.findByRole("heading", { level: 1 });
    await waitFor(() => expect(reads).toContain("/api/v1/branding"));
    expect(h1.parentElement!.outerHTML).toBe(LOCKUP_BEFORE);
  });
});

describe("branded console (#1125)", () => {
  it("the sign-in page shows the product name, the monogram, the tab title, icon and colours", async () => {
    publicRead = async () => BRAND;
    const link = favicon();
    const { unmount } = renderSignIn();
    expect(await screen.findByRole("heading", { level: 1, name: "Example Corp Wardyn" })).toBeInTheDocument();
    expect(screen.getByText("EC")).toBeInTheDocument();
    await waitFor(() => expect(document.title).toBe("Example Corp Wardyn"));
    await waitFor(() => expect(link.getAttribute("href")).toBe("/api/v1/branding/logo?v=abc"));
    expect(document.querySelector("style[data-wardyn-brand]")?.textContent).toBe(
      "html:root{--primary:#7c3aed;--primary-foreground:#ffffff}html.dark{--primary:#9058f0;--primary-foreground:#0a0a0a}",
    );
    // A later title write by the console (the shell's view effect) is renamed too.
    document.title = "Wardyn admin";
    await waitFor(() => expect(document.title).toBe("Example Corp Wardyn admin"));
    unmount();
    // Leaving restores what was there.
    expect(document.querySelector("style[data-wardyn-brand]")).toBeNull();
    expect(link.getAttribute("href")).toBe("/favicon.svg");
    expect(document.title).toBe("Wardyn admin");
  });

  it("reads Wardyn for <Company> in the suffix format, with the uploaded logo", async () => {
    publicRead = async () => ({ ...BRAND, name_format: "suffix", logo_url: "/api/v1/branding/logo?v=abc" });
    renderSignIn();
    const h1 = await screen.findByRole("heading", { level: 1, name: "Wardyn for Example Corp" });
    expect(h1.parentElement!.querySelector("img")?.getAttribute("src")).toBe("/api/v1/branding/logo?v=abc");
  });

  it("the top bar carries the name and one https Support link: new tab, no opener, no referrer", async () => {
    publicRead = async () => BRAND;
    settingsRead = async () => ({ ...BRAND, support_url: "https://status.example.com" });
    renderTopBar();
    const support = await screen.findByRole("link", { name: "Support" });
    expect(support.getAttribute("href")).toBe("https://status.example.com");
    expect(support.getAttribute("target")).toBe("_blank");
    expect(support.getAttribute("rel")).toBe("noopener noreferrer");
    expect(screen.getByText("Example Corp Wardyn")).toBeInTheDocument();
  });

  it("never draws a Support link that is not https", async () => {
    publicRead = async () => BRAND;
    settingsRead = async () => ({ ...BRAND, support_url: "http://status.example.com" });
    renderTopBar();
    await screen.findByText("Example Corp Wardyn");
    await waitFor(() => expect(reads).toContain("/api/v1/branding/settings"));
    expect(screen.queryByRole("link", { name: "Support" })).toBeNull();
  });

  it("injects nothing unless all four values are hex colours", () => {
    expect(brandColourCSS({ ...BRAND, primary: "red;}body{x:1" })).toBeNull();
    expect(brandColourCSS({ ...BRAND, dark_primary: undefined })).toBeNull();
  });

  it("renames only the console's own titles, keeping the Admin view cue", () => {
    expect(brandTitle("Wardyn", "Example Corp Wardyn")).toBe("Example Corp Wardyn");
    expect(brandTitle("Wardyn admin", "Wardyn for Example Corp")).toBe("Wardyn for Example Corp admin");
    expect(brandTitle("Something else", "Example Corp Wardyn")).toBe("Something else");
  });
});
