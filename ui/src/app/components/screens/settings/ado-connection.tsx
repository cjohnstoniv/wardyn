/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps connected panel (#386, §2.2/§6 2b) — one of its TWO homes:
// Getting started renders the chip+cause+button inline in "What's set up for
// you" (member-getting-started.tsx); this is the Settings card, "a member
// checking months later what their runs can do looks in Settings, not in an
// onboarding page" (Q9). Both read scmAccessChip/scmAccessCause off the same
// SCMAccess answer, so the two homes cannot disagree about one connection.
//
// Absent entirely when no Azure DevOps row is configured (status.scm_access
// is absent or state "") — the same absent-row doctrine every other card
// here follows. The card shell (a bordered <section>, title + lede) matches
// connection-cards.tsx's own un-exported Card, rather than importing a
// shadcn Card this codebase does not have.
//
// id="azure-devops" + the hash-focus effect (#458): the capability card's
// consent CTA and the mid-run sign-in door (ado-capability-card.tsx) both
// land here via `/account#azure-devops` (M-1b: was `/settings#azure-devops`)
// — a five-card page with no anchor otherwise strands the reader at the top.
// tabIndex=-1 makes the section programmatically focusable without joining
// the page's Tab order; the browser's own focus-triggered scroll is the only scroll this does.
//
// PR #501 review F2: the effect used to depend on `access` (status.scm_access)
// itself, which is a NEW object after every Settings reload — Re-check, a
// secret save, a disconnect elsewhere on the page all call the page's own
// load() and hand this card a fresh (but often value-equal) `access` object,
// which reran the effect and yanked focus back here even though the URL
// never changed and the reader had since focused something else. `hasRow`
// (a boolean, not the object) plus `location.key` (which only changes on a
// real navigation, unlike `location.hash` alone once react-router settles
// the hash-only-navigate case the same way) are the correct dependencies —
// "did we land here" is a navigation event, not a data refresh.
//
// F3: a `.focus()` called from a click handler elsewhere on the page (e.g.
// the capability card's own consent-door click, which navigates here) can
// fail the browser's focus-visible heuristic — "the last interaction was a
// mouse click" — and render with no ring at all, silently. `focusVisible:
// true` forces the ring regardless of that heuristic; browsers that don't
// yet support the option simply ignore it and fall back to the default
// heuristic, so this is a strict improvement, never a regression.
import * as React from "react";
import { useLocation } from "react-router-dom";
import { Button } from "../../ui/button";
import { ADO } from "../../../lib/ado-entra-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { scmAccessCause, scmAccessChip, scmAccessNeedsConnect } from "../../../lib/scm-access-display";
import { useAdoConnect } from "../../../lib/hooks/use-ado-connect";
import type { SetupStatus } from "../../../lib/types";

export function AdoConnectionCard({ status, onChanged }: { status?: SetupStatus; onChanged: () => void }) {
  const { connecting, connect, connectFallback, blockedUrl } = useAdoConnect();
  const access = status?.scm_access;
  const hasRow = !!access && access.state !== "";
  const location = useLocation();
  const sectionRef = React.useRef<HTMLElement | null>(null);
  React.useEffect(() => {
    if (location.hash === "#azure-devops" && hasRow) {
      // `focusVisible: true` (F3): forces the ring even when the browser's
      // own heuristic would otherwise suppress it for a focus that followed
      // a mouse click (the consent door's own click, on the previous page).
      sectionRef.current?.focus({ focusVisible: true } as FocusOptions);
    }
    // location.key, not location.hash (F2): a real navigation is what should
    // re-run this, not every render this card happens to get.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [location.key, hasRow]);
  const handleConnect = async () => {
    if (await connect()) onChanged();
  };
  // review follow-up N1: the fallback link opens sign-in in a new tab; this
  // starts the SAME poll (bounded, connectFallback) so the card still
  // updates when the person comes back connected.
  const handleFallbackClick = () => {
    void connectFallback().then((ok) => {
      if (ok) onChanged();
    });
  };
  if (!access || access.state === "") return null;
  const chip = scmAccessChip(access.state, access.source, access.cause);

  return (
    <section
      id="azure-devops"
      ref={sectionRef}
      tabIndex={-1}
      className="rounded-xl border border-border bg-card p-4 outline-none focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px]"
    >
      <h3 className="text-sm font-medium text-foreground">{PROVIDERS.KIND_AZURE_DEVOPS}</h3>
      <div className="mt-3 space-y-2 text-body">
        {/* Q458-1: an admin-token/local-mode caller has no per-person Azure
            DevOps connection at all — one line, no chip, no button, unlike
            every other state below. */}
        {access.state === "not_applicable" && <p className="text-muted-foreground">{ADO.NOT_APPLICABLE_BODY}</p>}
        {chip && <p className={chip.tone === "success" ? "text-success" : "text-warning"}>{chip.label}</p>}
        {/* review finding F3: `live` branches on `source` — a SHARED row's
            `live` (no source) makes no per-person claim (§7.5's
            ACCESS_SHARED_NOTE) and must never render "Your Wardyn sign-in" /
            "Renewed while you keep using it", which are per-person facts. */}
        {access.state === "live" && access.source && (
          <>
            {access.org && (
              <p className="text-muted-foreground">
                {ADO.PANEL_ORG}: <span className="font-mono">{access.org}</span>
              </p>
            )}
            <p className="text-muted-foreground">
              {ADO.PANEL_HOW}: {access.source === "separate" ? ADO.PANEL_HOW_SEPARATE : ADO.PANEL_HOW_ORG}
            </p>
            <p className="text-muted-foreground">
              {ADO.PANEL_ENDS}: {ADO.PANEL_ENDS_RENEWED}
            </p>
            <p className="text-muted-foreground">{ADO.PANEL_ENDS_HINT}</p>
          </>
        )}
        {access.state === "live" && !access.source && <p className="text-muted-foreground">{ADO.ACCESS_SHARED_NOTE}</p>}
        {scmAccessNeedsConnect(access.state) && (
          <>
            <p className="text-warning">{scmAccessCause(access.cause)}</p>
            <Button size="sm" variant="outline" disabled={connecting} onClick={() => void handleConnect()}>
              {ADO.CONNECT_ADO}
            </Button>
            {blockedUrl && (
              <p className="text-xs text-muted-foreground">
                {ADO.CONNECT_POPUP_BLOCKED}{" "}
                <a
                  href={blockedUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="font-medium text-info hover:underline"
                  onClick={handleFallbackClick}
                >
                  {ADO.CONNECT_ADO}
                </a>
              </p>
            )}
          </>
        )}
        {access.state === "shared_expired" && <p className="text-warning">{ADO.ACCESS_SHARED_EXPIRED_ACTION}</p>}
      </div>
    </section>
  );
}
