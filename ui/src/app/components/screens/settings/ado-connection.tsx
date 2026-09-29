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
// here follows. The card shell is CollapsibleCard (#1200 compact cards) —
// this was connection-cards.tsx's own un-exported Card's shape before that,
// rather than importing a shadcn Card this codebase does not have.
//
// id="azure-devops" + the hash-focus effect (#458): the capability card's
// consent CTA and the mid-run sign-in door (ado-capability-card.tsx) both
// land here via `/account#azure-devops` (M-1b: was `/settings#azure-devops`)
// — a five-card page with no anchor otherwise strands the reader at the top.
// tabIndex=-1 makes the section programmatically focusable without joining
// the page's Tab order; the browser's own focus-triggered scroll is the only scroll this does.
// #1200 compact cards: a card collapsed by default would otherwise hide the
// very connection state a deep link exists to show, so the same effect that
// focuses the section also force-opens it — the one card whose open state is
// CONTROLLED rather than left to CollapsibleCard's own uncontrolled default.
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
import { Button, buttonVariants } from "../../ui/button";
import { ADO } from "../../../lib/ado-entra-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { scmAccessCause, scmAccessChip, scmAccessNeedsConnect } from "../../../lib/scm-access-display";
import { useAdoConnect } from "../../../lib/hooks/use-ado-connect";
import type { SetupStatus } from "../../../lib/types";
import { CollapsibleCard } from "../../wardyn/collapsible-card";

export function AdoConnectionCard({ status, onChanged }: { status?: SetupStatus; onChanged: () => void }) {
  const { connecting, connect, connectFallback, blockedUrl } = useAdoConnect();
  const access = status?.scm_access;
  const hasRow = !!access && access.state !== "";
  const location = useLocation();
  const sectionRef = React.useRef<HTMLElement | null>(null);
  // #1200 compact cards: controlled, not CollapsibleCard's own uncontrolled
  // default — a card collapsed by default must not also hide itself from the
  // deep link that exists to show exactly this state. Manual toggling still
  // works afterward (onOpenChange below).
  const [open, setOpen] = React.useState(false);
  React.useEffect(() => {
    if (location.hash === "#azure-devops" && hasRow) {
      setOpen(true);
      // `focusVisible: true` (F3): forces the ring even when the browser's
      // own heuristic would otherwise suppress it for a focus that followed
      // a mouse click (the consent door's own click, on the previous page).
      sectionRef.current?.focus({ focusVisible: true } as FocusOptions);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- location.key, not location.hash (F2): a real navigation is what should re-run this, not every render this card happens to get
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
  // #1200 compact cards — reuses the exact same first line the body already
  // rendered (never a new sentence): the plain not_applicable line, or the
  // chip's own label toned the same way.
  const summary =
    access.state === "not_applicable" ? (
      ADO.NOT_APPLICABLE_BODY
    ) : chip ? (
      <span className={chip.tone === "success" ? "text-success" : "text-warning"}>{chip.label}</span>
    ) : undefined;

  return (
    <CollapsibleCard
      title={PROVIDERS.KIND_AZURE_DEVOPS}
      summary={summary}
      id="azure-devops"
      ref={sectionRef}
      tabIndex={-1}
      open={open}
      onOpenChange={setOpen}
      className="outline-none focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px]"
    >
      <div className="space-y-2 text-body">
        {/* Q458-1's one canon line, and the chip label below it, are now the
            header's `summary` (#1200 compact cards) — stated once, not
            repeated verbatim here once expanded. */}
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
                  className={buttonVariants({ variant: "outline", size: "sm" })}
                  onClick={handleFallbackClick}
                >
                  {ADO.CONNECT_POPUP_OPEN}
                </a>
              </p>
            )}
          </>
        )}
        {access.state === "shared_expired" && <p className="text-warning">{ADO.ACCESS_SHARED_EXPIRED_ACTION}</p>}
      </div>
    </CollapsibleCard>
  );
}
