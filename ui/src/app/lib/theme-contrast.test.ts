/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

// C004: the semantic tokens are used as button/badge text (white on them) and
// as text/dot colors on their grounds + their own -subtle tint, so each must
// clear WCAG AA 4.5:1 for normal text. This test reads theme.css and re-proves
// it for BOTH themes, so a revert to the 500-family brights (light success
// 2.25:1, dark danger 4.10:1) fails here instead of shipping sub-AA text.

function lum(hex: string): number {
  const ch = (i: number) => parseInt(hex.slice(i, i + 2), 16) / 255;
  const lin = (c: number) => (c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4));
  return 0.2126 * lin(ch(1)) + 0.7152 * lin(ch(3)) + 0.0722 * lin(ch(5));
}
function ratio(a: string, b: string): number {
  const la = lum(a);
  const lb = lum(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

const css = readFileSync("src/styles/theme.css", "utf8");
// The light theme is the :root { ... } block; slice to its closing brace (the first
// "\n}" line) rather than to ".dark" — the header comments mention .dark, which would
// truncate the slice to nothing.
const rootStart = css.indexOf(":root");
const root = css.slice(rootStart, css.indexOf("\n}", rootStart));

// F7-F15: --destructive is now `var(--danger)` (never a hand-copied hex that
// can drift from it again — theme.css:79's old comment claimed "same value
// as --danger" while dark had quietly drifted to a different literal).
// Resolves ONE level of var(--x) indirection; the theme has no deeper chain.
function resolveVar(root: string, value: string): string {
  const v = /^var\(--([a-zA-Z0-9-]+)\)$/.exec(value.trim());
  if (!v) return value;
  const m = root.match(new RegExp(`--${v[1]}:\\s*(#[0-9a-fA-F]{6}|var\\([^)]+\\))`));
  if (!m) throw new Error(`--${v[1]} not found while resolving var(--${v[1]})`);
  return resolveVar(root, m[1]);
}

function token(name: string): string {
  const m = root.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6}|var\\([^)]+\\))`));
  if (!m) throw new Error(`--${name} not found in :root`);
  return resolveVar(root, m[1]);
}
// The -subtle tint composited over white — the actual background the text sits on.
function subtleBg(name: string): string {
  const m = root.match(new RegExp(`--${name}-subtle:\\s*rgba\\((\\d+),\\s*(\\d+),\\s*(\\d+),\\s*([\\d.]+)\\)`));
  if (!m) throw new Error(`--${name}-subtle not found in :root`);
  const [r, g, b, a] = [+m[1], +m[2], +m[3], +m[4]];
  const over = (v: number) => Math.round(a * v + (1 - a) * 255);
  const hex = (v: number) => v.toString(16).padStart(2, "0");
  return "#" + hex(over(r)) + hex(over(g)) + hex(over(b));
}

// A field that paints nothing (bg-transparent) or paints translucently
// (dark:bg-input/30) shows whatever its CONTAINER paints, so a gate on
// --placeholder-foreground has to model the TIGHTEST such chain in its theme,
// not the handiest backdrop. Layers are innermost-first, each [token, alpha];
// they composite outermost-first onto an opaque --background root, which is
// where every chain terminates (nothing above an opaque paint can reach the
// field). `tok` selects the theme.
function fieldBackdrop(tok: (name: string) => string, layers: [string, number][]): string {
  const ch = (name: string, k: number) => parseInt(tok(name).slice(1 + 2 * k, 3 + 2 * k), 16);
  let bg = [0, 1, 2].map((k) => ch("background", k));
  for (const [t, a] of [...layers].reverse()) {
    bg = bg.map((v, k) => Math.round(a * ch(t, k) + (1 - a) * v));
  }
  return "#" + bg.map((v) => v.toString(16).padStart(2, "0")).join("");
}

// Read the dark translucency from the primitive rather than restating it. The
// modelled field is a <Textarea>, so textarea.tsx — not input.tsx — is the file
// that decides it; the assertion below pins the two to the same alpha.
function darkFieldAlpha(primitive: "input" | "textarea"): number {
  const m = readFileSync(`src/app/components/ui/${primitive}.tsx`, "utf8").match(/dark:bg-input\/(\d+)\b/);
  if (!m) throw new Error(`dark:bg-input/NN not found in ui/${primitive}.tsx`);
  return +m[1] / 100;
}

// The build-steps Textarea's chain, shared by both themes: bg-surface-2/40
// inside the selected image card's bg-primary/10, on the wizard dialog's
// bg-background (workspace-wizard/step-base-image.tsx + wizard.tsx).
const BUILD_STEPS_PANES: [string, number][] = [
  ["surface-2", 0.4],
  ["primary", 0.1],
];

const WHITE = "#ffffff";

// All .tsx under src/app, for the source-scanning guards below.
function walkTsx(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    return e.isDirectory() ? walkTsx(p) : p.endsWith(".tsx") ? [p] : [];
  });
}

describe("light-theme WCAG AA contrast (C004)", () => {
  it("white button text on --primary is >= 4.5:1", () => {
    expect(ratio(WHITE, token("primary"))).toBeGreaterThanOrEqual(4.5);
  });

  it("white text on --danger (KILLED badge) is >= 4.5:1", () => {
    expect(ratio(WHITE, token("danger"))).toBeGreaterThanOrEqual(4.5);
  });

  // --destructive was missed by the C004 darkening pass and stayed red-500
  // (3.76:1), under white button text and as error text on --popover.
  it("white text on --destructive (destructive buttons/dialogs) is >= 4.5:1", () => {
    expect(ratio(WHITE, token("destructive"))).toBeGreaterThanOrEqual(4.5);
  });

  // F7-F15: --destructive is `var(--danger)`, never a second hand-typed hex —
  // theme.css's old comment claimed "same value as --danger" while dark had
  // silently drifted to a different literal (#dc2626 vs #f87171). Structural,
  // both themes: this can never drift again because there is only one value.
  it("--destructive equals --danger by construction (light and dark)", () => {
    expect(token("destructive")).toBe(token("danger"));
  });

  // F7-F3/F7-F8: AgentBadge's claude/success chips put white text on a fill
  // that fails AA — claude 3.12:1 in BOTH themes, success 2.54:1 in dark only
  // (light already passes at 5.48:1, kept white there). --agent-claude
  // shares one hex across themes and needs a dark foreground in both;
  // --success-foreground follows --danger/--info-foreground's existing
  // per-theme pattern (white in light, near-black in dark).
  it("--agent-claude-foreground clears AA on --agent-claude (both themes — same fill hue)", () => {
    expect(ratio(token("agent-claude-foreground"), token("agent-claude"))).toBeGreaterThanOrEqual(4.5);
  });
  it("--success-foreground clears AA on --success (light theme)", () => {
    expect(ratio(token("success-foreground"), token("success"))).toBeGreaterThanOrEqual(4.5);
  });

  // info + cyan joined the guarded set when their 500-family values measured
  // 3.68:1 / 2.43:1 as 12px chip text (Starting badge, egress-domain chip).
  for (const t of ["success", "warning", "danger", "info", "cyan"]) {
    it(`--${t} text is >= 4.5:1 on white and on its own -subtle tint`, () => {
      expect(ratio(token(t), WHITE)).toBeGreaterThanOrEqual(4.5);
      expect(ratio(token(t), subtleBg(t))).toBeGreaterThanOrEqual(4.5);
    });
  }

  // F7-F1: the old #737373 measured 4.7417:1 on white — why this test alone
  // stayed green — but 4.3139:1 on --muted/--surface-2 (#f4f4f5), the ground
  // the real sites (the neutral Chip, 93 call sites, + both Notes) actually
  // sit on. TabsList was REFUTED as a real site (its trigger overrides to
  // text-foreground); CodeBlock's use is an icon (1.4.11's 3:1, passes at
  // 4.11:1 either value). #6b6b6b clears AA on BOTH grounds.
  it("--muted-foreground text is >= 4.5:1 on white AND on --muted/--surface-2 (the real Chip/Note ground)", () => {
    expect(ratio(token("muted-foreground"), WHITE)).toBeGreaterThanOrEqual(4.5);
    expect(ratio(token("muted-foreground"), token("muted"))).toBeGreaterThanOrEqual(4.5);
  });

  // Placeholder text is still text (WCAG 1.4.3, 4.5:1) as well as needing to
  // read as distinctly not-a-value (>=3:1 from filled --foreground) — a dark-
  // theme regression once satisfied only the second half (see below).
  // Light mode has no dark: override, so a field normally IS --input-background
  // (#ffffff) — but that is the loosest backdrop in the theme, and one shipped
  // field is not on it: the build-steps Textarea's own bg-transparent evicts
  // bg-input-background (tailwind-merge folds both into its one bg-color group,
  // and the built sheet emits .bg-transparent later at equal specificity), so it
  // paints nothing and sits on the tinted panes below. Gating on
  // --input-background certified #737373, which renders 4.19:1 there.
  it("--placeholder-foreground is >= 4.5:1 on the TIGHTEST real field and >= 3:1 separated from filled-value text", () => {
    const tightestField = fieldBackdrop(token, BUILD_STEPS_PANES);
    expect(ratio(token("placeholder-foreground"), tightestField)).toBeGreaterThanOrEqual(4.5);
    // The other 56 fields DO sit on --input-background. Clearing the tinted one
    // only implies clearing them while that token stays the lighter of the two —
    // which is a coincidence in this theme, not a law (dark already separates
    // --input-background from --background). Assert it, so giving light inputs a
    // fill can't put 56 fields under AA with this gate still green.
    expect(ratio(token("placeholder-foreground"), token("input-background"))).toBeGreaterThanOrEqual(4.5);
    expect(ratio(token("placeholder-foreground"), token("foreground"))).toBeGreaterThanOrEqual(3);
  });

  // The DARK theme is the default (theme-provider is dark-first), so its status
  // text needs the same guard — the old header claim "dark already passes"
  // measured false for danger (4.10:1 on the bg, 3.59:1 on subtle-over-card).
  // Text sits on --background and on -subtle composited over --card; fills
  // (--danger + --danger-foreground) invert to dark text like dark --primary.
  describe("dark-theme WCAG AA contrast", () => {
    const darkStart = css.indexOf(".dark {");
    const dark = css.slice(darkStart, css.indexOf("\n}", darkStart));
    const dtoken = (name: string): string => {
      const m = dark.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6}|var\\([^)]+\\))`));
      if (!m) throw new Error(`--${name} not found in .dark`);
      return resolveVar(dark, m[1]);
    };
    const dsubtleOverCard = (name: string): string => {
      const m = dark.match(
        new RegExp(`--${name}-subtle:\\s*rgba\\((\\d+),\\s*(\\d+),\\s*(\\d+),\\s*([\\d.]+)\\)`),
      );
      if (!m) throw new Error(`--${name}-subtle not found in .dark`);
      const card = dtoken("card");
      const cch = (i: number) => parseInt(card.slice(i, i + 2), 16);
      const [r, g, b, a] = [+m[1], +m[2], +m[3], +m[4]];
      const over = (v: number, w: number) => Math.round(a * v + (1 - a) * w);
      const hex = (v: number) => v.toString(16).padStart(2, "0");
      return "#" + hex(over(r, cch(1))) + hex(over(g, cch(3))) + hex(over(b, cch(5)));
    };
    // Input/Textarea's dark:bg-input/30 wins over the plain bg-input-background
    // class (its :is(.dark *) selector has higher specificity — confirmed in the
    // built CSS), so no field is ever --input-background itself: it is --input,
    // translucent, over whatever the field's CONTAINER paints. A gate on that
    // token therefore has to model the LIGHTEST real container — the first
    // version of this test modelled input/30-over---card and certified a value
    // that four shipped fields, sitting on a bg-surface-2/40 pane, rendered
    // under AA. The lightest real field in the app is the build-steps Textarea
    // (workspace-wizard/step-base-image.tsx:163): bg-surface-2/40 (:144) inside
    // the selected card's bg-primary/10 (:229-232, the disclosed body renders
    // only when selected), on the wizard dialog's bg-background. Clear AA there
    // and every darker-backed field follows.
    const dTightestField = (): string =>
      fieldBackdrop(dtoken, [["input", darkFieldAlpha("textarea")], ...BUILD_STEPS_PANES]);

    for (const t of ["success", "warning", "danger", "info", "cyan"]) {
      it(`dark --${t} text is >= 4.5:1 on --background and on -subtle over --card`, () => {
        expect(ratio(dtoken(t), dtoken("background"))).toBeGreaterThanOrEqual(4.5);
        expect(ratio(dtoken(t), dsubtleOverCard(t))).toBeGreaterThanOrEqual(4.5);
      });
    }
    it("dark --danger-foreground on the --danger fill is >= 4.5:1 (kill/delete buttons)", () => {
      expect(ratio(dtoken("danger-foreground"), dtoken("danger"))).toBeGreaterThanOrEqual(4.5);
    });

    it("dark --destructive equals --danger (F7-F15 — was #dc2626 vs #f87171, silently different)", () => {
      expect(dtoken("destructive")).toBe(dtoken("danger"));
    });

    it("dark --agent-claude-foreground clears AA on --agent-claude (same fill hue as light)", () => {
      expect(ratio(dtoken("agent-claude-foreground"), dtoken("agent-claude"))).toBeGreaterThanOrEqual(4.5);
    });
    it("dark --success-foreground clears AA on --success — the DARK-only failure (2.54:1 with white)", () => {
      expect(ratio(dtoken("success-foreground"), dtoken("success"))).toBeGreaterThanOrEqual(4.5);
    });

    it("dark --placeholder-foreground is >= 4.5:1 on the LIGHTEST real field and >= 3:1 separated from filled-value text", () => {
      expect(ratio(dtoken("placeholder-foreground"), dTightestField())).toBeGreaterThanOrEqual(4.5);
      expect(ratio(dtoken("placeholder-foreground"), dtoken("foreground"))).toBeGreaterThanOrEqual(3);
    });

    // "one alpha lightens every field at once" is only true while the two field
    // primitives carry the same one. Without this, a textarea-only alpha change
    // would lighten the very field the gate above pins, unseen.
    it("Input and Textarea carry the same dark:bg-input alpha", () => {
      expect(darkFieldAlpha("input")).toBe(darkFieldAlpha("textarea"));
    });
  });

  // --muted-foreground clears AA at full strength but NOT diluted — a
  // `text-muted-foreground/70` at 11px composites to ~2.7:1. Forbid the diluted
  // form on text tokens so a de-emphasis tweak can't silently drop below AA.
  // F7-F3/F7-F15: widened to all FIVE semantic tokens (success/warning/danger/
  // info/cyan) — the same class of bug as muted-foreground/NN (a real
  // instance shipped as error-boundary.tsx's `text-danger/80`, ~4.5:1 * 0.8 ≈
  // 3.6:1, below AA) — plus muted-foreground itself, six guarded tokens total.
  it("no opacity-diluted semantic/muted-foreground text anywhere in src/app", () => {
    const offenders: string[] = [];
    const tokens = ["muted-foreground", "success", "warning", "danger", "info", "cyan"];
    const re = new RegExp(`\\btext-(?:${tokens.join("|")})\\/(\\d{1,3})\\b`);
    for (const f of walkTsx("src/app")) {
      readFileSync(f, "utf8")
        .split("\n")
        .forEach((ln, i) => {
          // TESTSPEC-5: numeric compare, not a multiples-of-ten regex — the old
          // /[0-9]0/ pattern matched /70 or /90 but missed /75, a standard
          // Tailwind step this comment already claimed was caught.
          const m = ln.match(re);
          if (m && +m[1] < 100) offenders.push(`${f}:${i + 1}`);
        });
    }
    expect(offenders, `diluted semantic/muted-foreground text — use the full token:\n${offenders.join("\n")}`).toHaveLength(0);
  });

  // R-01: the FILL-side twin of the text-side gate above. A diluted `bg-<token>/NN`
  // composites the fill toward --background, which can drag whatever *-foreground
  // text sits on it below AA even though the token itself is AA-clear at full
  // strength (proven for danger/destructive above) — exactly what
  // `dark:bg-destructive/60` did to `text-danger-foreground` on the destructive
  // button (9.46:1 -> 3.18:1). The text-side gate above never matches a `bg-`
  // class, so this slipped through it; scanned over ALL of src/app, INCLUDING
  // components/ui/ (unlike the text-white/raw-palette gates below, a diluted
  // fill under a vendored shadcn primitive is just as real a regression).
  //
  // Scoped to the actual failure mode — a BASE or `dark:`-only diluted fill
  // sharing its className with a `*-foreground` text token — not every diluted
  // fill in the app: `hover:`/`focus:`/`data-[...]:`-gated dilutions (e.g. the
  // ~10 pre-existing `bg-danger text-danger-foreground hover:bg-danger/90`
  // delete-button hovers, dropdown-menu's `data-[variant=destructive]:focus:
  // bg-destructive/10`) are a different, pre-existing, interaction-state
  // pattern where the dilution is minor (90%) and momentary, not this lane's
  // regression class; and a diluted fill with no co-located `*-foreground`
  // text (e.g. `bg-info/15` badges, `bg-danger/15` chips using `text-danger`
  // directly) never had an opaque-fill-calibrated text token riding on it in
  // the first place. Auditing those ~20 pre-existing sites is out of scope
  // for R-01; this gate pins the class of bug R-01 actually is.
  it("no BASE/dark: opacity-diluted fill sharing a *-foreground text token, anywhere in src/app, incl. components/ui/", () => {
    const offenders: string[] = [];
    const tokens = ["destructive", "success", "warning", "danger", "info", "cyan"];
    const bgClassRe = new RegExp(`^(?:dark:)?bg-(?:${tokens.join("|")})\\/(\\d{1,3})$`);
    const fgRe = /-foreground\b/;
    for (const f of walkTsx("src/app")) {
      readFileSync(f, "utf8")
        .split("\n")
        .forEach((ln, i) => {
          if (!fgRe.test(ln)) return;
          const diluted = ln.split(/\s+/).some((word) => {
            const m = bgClassRe.exec(word.replace(/^["'`]+|["'`,]+$/g, ""));
            return m !== null && +m[1] < 100;
          });
          if (diluted) offenders.push(`${f}:${i + 1}`);
        });
    }
    expect(offenders, `diluted BASE/dark: fill under a *-foreground text token:\n${offenders.join("\n")}`).toHaveLength(0);
  });

  // F7-F3/F7-F8: text-white bypasses every contrast guard above — it is
  // neither the AA-proven semantic tokens nor a token this file can check at
  // all. AgentBadge (claude 3.12:1 both themes) and PhaseRail's success step
  // (2.54:1 dark) both shipped it. components/ui/ (vendored shadcn — uses its
  // own *-foreground tokens once fixed there, exempted only because it is
  // vendored) and terminal chrome (a fixed dark theatre regardless of theme —
  // asciinema-style player text, not app UI) are the two legitimate uses.
  it("no raw text-white outside components/ui/ and terminal chrome", () => {
    const offenders: string[] = [];
    for (const f of walkTsx("src/app")) {
      if (f.includes("components/ui/") || /terminal/i.test(f)) continue;
      readFileSync(f, "utf8")
        .split("\n")
        .forEach((ln, i) => {
          if (/\btext-white\b/.test(ln)) offenders.push(`${f}:${i + 1}`);
        });
    }
    expect(offenders, `raw text-white — use the matching *-foreground token:\n${offenders.join("\n")}`).toHaveLength(0);
  });

  // UI-LIB-2: placeholder:text-muted-foreground is the exact dark-theme defect
  // this file documents below (2.16:1) — input.tsx/textarea.tsx moved to
  // placeholder:text-placeholder-foreground; forbid the old token everywhere,
  // INCLUDING components/ui/ (command.tsx was the regression this gate
  // couldn't see, since it only checked the token definition, never call sites).
  it("no placeholder:text-muted-foreground anywhere in src/app", () => {
    const offenders: string[] = [];
    for (const f of walkTsx("src/app")) {
      readFileSync(f, "utf8")
        .split("\n")
        .forEach((ln, i) => {
          if (ln.includes("placeholder:text-muted-foreground")) offenders.push(`${f}:${i + 1}`);
        });
    }
    expect(
      offenders,
      `placeholder:text-muted-foreground — use placeholder:text-placeholder-foreground:\n${offenders.join("\n")}`,
    ).toHaveLength(0);
  });

  // /S3: raw Tailwind palette *text* colors (e.g. text-amber-600 ≈ 3.4:1)
  // bypass the WCAG-verified semantic tokens proven above. Forbid them so a
  // revert of the compose-Q&A risk text (text-warning) back to text-amber-600 —
  // or any new palette-color status text — fails here instead of shipping
  // sub-AA text. (Palette border-/bg- utilities are unaffected; only text- is.)
  //
  // text-destructive is ALSO forbidden as text: --destructive is the button
  // FILL token (white text goes on it), tuned for that job, and as dark-theme
  // text it measured 3.9:1 on --popover. Error copy uses text-danger, whose
  // both-theme text ratios the assertions above prove. The vendored shadcn
  // primitives under components/ui/ are exempt (same vendored treatment as the
  // coverage config): their destructive MENU variant is upstream API, and no
  // app call site uses it for body/error copy.
  it("no raw palette text colors in src/app — use text-warning/success/danger", () => {
    // TESTSPEC-5: the full Tailwind default palette (22 families) — the old
    // list omitted sky/fuchsia and the five gray-scale families (slate/gray/
    // zinc/neutral/stone), each just as capable of shipping sub-AA text as
    // the 15 already guarded.
    const rawText = /\btext-(amber|red|green|yellow|orange|emerald|rose|lime|teal|cyan|sky|blue|indigo|violet|purple|pink|fuchsia|slate|gray|zinc|neutral|stone)-[0-9]{2,3}\b|\btext-destructive\b/;
    const offenders: string[] = [];
    for (const f of walkTsx("src/app")) {
      if (f.includes("components/ui/")) continue; // vendored shadcn primitives
      readFileSync(f, "utf8")
        .split("\n")
        .forEach((ln, i) => {
          if (rawText.test(ln)) offenders.push(`${f}:${i + 1}`);
        });
    }
    expect(
      offenders,
      `raw palette text color — use the guarded semantic token:\n${offenders.join("\n")}`,
    ).toHaveLength(0);
  });
});

// F7-F5 — vestibular-motion safety (WCAG 2.3.3 / prefers-reduced-motion): 82
// `animate-` lines in src/app, only 1 (run-state-glyph.tsx's spinner)
// hand-guarded with motion-reduce:animate-none. Neither tw-animate-css 1.4.0
// nor Tailwind 4.3.3 ships a reduced-motion guard on its own — a spinning
// Loader2 or a sliding dialog with no per-call-site opt-out is exactly what
// triggers vestibular symptoms for a reduced-motion user, on every one of the
// 81 unguarded sites at once. ONE global block in theme.css covers all of
// them without touching a single call site (Playwright's suite re-run is the
// regression control — nothing here changes non-reduced-motion behaviour).
describe("prefers-reduced-motion — one global guard (F7-F5)", () => {
  it("theme.css declares a global @layer base reduced-motion block that neutralizes animation/transition", () => {
    const layerBaseStart = css.indexOf("@layer base");
    expect(layerBaseStart, "@layer base not found in theme.css").toBeGreaterThanOrEqual(0);
    const mediaStart = css.indexOf("@media (prefers-reduced-motion: reduce)");
    expect(mediaStart, "prefers-reduced-motion block not found").toBeGreaterThanOrEqual(0);
    expect(mediaStart, "the reduced-motion block must live inside @layer base").toBeGreaterThan(layerBaseStart);
    const block = /@media\s*\(prefers-reduced-motion:\s*reduce\)\s*\{([\s\S]*?)\n\s*\}\s*\n\s*\}/.exec(css);
    expect(block, "prefers-reduced-motion block not found or not closed").not.toBeNull();
    const body = block![1];
    // Universal selector, not a per-utility list — a NEW animate- class added
    // later (Tailwind or tw-animate-css) is covered automatically.
    expect(body).toMatch(/\*/);
    // animation-duration: 0.01ms (not `animation: none`) — Radix's
    // animate-in/animate-out rely on the animationend event to unmount an
    // exiting dialog/popover; `none` would never fire it and hang the exit.
    // Near-zero duration still fires the event, just imperceptibly.
    expect(body).toMatch(/animation-duration:\s*0\.01ms/);
    expect(body).toMatch(/transition-duration:\s*0\.01ms/);
  });
});
