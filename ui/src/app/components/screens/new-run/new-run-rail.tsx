/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run screen's right-hand rail: what this run can actually do, and the
// one button that commits it.
//
// Split out of new-run-screen.tsx by the same seam new-run-primitives.tsx took
// (the file was past the 1000-line gate): this takes props and renders. It owns
// no screen state — every sentence it shows is derived UP in the screen, so the
// rail cannot describe one run while Launch sends another.
//
// It stays a FIXED 320px beside the panels from lg up, bounded to the viewport
// with its own scroll. Below lg it is the page's persistent footer instead: the
// decision block and Launch always on screen, the sections behind one toggle.
// ONE element restyled by the breakpoint — never two rails in the
// accessibility tree, and never a second Launch.
import * as React from "react";
import { ChevronRight, Loader2, TriangleAlert } from "lucide-react";
import { Button, buttonVariants } from "../../ui/button";
import { cn } from "../../ui/utils";
import { ADO } from "../../../lib/ado-entra-copy";
import { PEOPLE } from "../../../lib/people-access-copy";
import { RAIL } from "../../wardyn/copy";
import { NEW_RUN_FLOW } from "../../wardyn/copy/new-run-flow";
import { useRecordingDisabled } from "../../../lib/hooks/use-recording-disabled";
import { useOperator, useUserViewSuperAdmin } from "../../wardyn/operator-context";
import { useViewAccess } from "../../wardyn/console-view";
import { PolicyRemedy } from "../../wardyn/policy-remedy";
import { useModelAccessDoor } from "../../wardyn/model-access-context";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../../ui/dialog";
import type { RunRailProps } from "./new-run-rail-types";
import { useDraftIdentity } from "./use-run-checks";
import { getAuthGeneration } from "../../../lib/api/core";
import { RunRailSummary } from "./new-run-rail-summary";
import { RailHold, RailVerdict } from "./new-run-rail-decision";
import { shownIssue } from "./new-run-launch-gates";

export function RunRail({
  panel,
  onIssue,
  guardLink,
  governanceProfile,
  governanceContact,
  savedPolicy,
  cc,
  showModelWarning,
  modelBlocked,
  startup,
  showHoldNote,
  toolRules,
  pushRules,
  unattended,
  launch,
  preflight,
  agentRow,
  modelProvider,
  adoDialog,
}: RunRailProps) {
  // #1328 review round 2, R2-1 — who can reach the Environment step from
  // here, see the noBarrier reason line below.
  const operator = useOperator();
  const identity = useDraftIdentity();
  const live = React.useRef({ identity, draftRevision: launch.draftRevision });
  live.current = { identity, draftRevision: launch.draftRevision };
  const mounted = React.useRef(true);
  React.useEffect(() => () => { mounted.current = false; }, []);
  const userViewSuperAdmin = useUserViewSuperAdmin();
  const access = useViewAccess();
  const canSetUpBarrier = operator || (access === "session-user" && userViewSuperAdmin);
  // Both of finding 1's facts, read rather than asserted: where the model
  // credential lands, and whether this deployment records anything at all.
  // `recordingDisabled` is tri-state — undefined until /healthz answers.
  const recordingDisabled = useRecordingDisabled();
  const door = useModelAccessDoor();

  // Focus returns to Launch, not to #main-content (which would drop the
  // member at the top of the form they were mid-way through), when a launch
  // or preflight refusal opened the door. The door owns the return target, so
  // nothing here races the dialog's own focus restoration.
  const launchRef = React.useRef<HTMLButtonElement>(null);

  // Below lg: whether the sections are showing. A view preference, not part
  // of the draft.
  const [sectionsOpen, setSectionsOpen] = React.useState(false);
  // Both bounds are MEASURED against the page's own scroller (app-shell.tsx's
  // <main>), which already excludes the header and any shell banner above it.
  // Below lg the rail is a sticky footer over that scroller, so the scroller
  // reserves the footer's height (it grows with an open summary or a wrapped
  // refusal): a control brought into view by focus never lands underneath it.
  // From lg up the rail sticks `top-6` below the scroller's top and may be no
  // taller than what is left of it, banner or not — the class's 100vh - 5rem
  // is only the first paint. 1024px is the theme's lg breakpoint.
  const asideRef = React.useRef<HTMLElement>(null);
  React.useEffect(() => {
    const aside = asideRef.current;
    const scroller = aside?.closest<HTMLElement>("main");
    if (!aside || !scroller || typeof window.matchMedia !== "function") return;
    const wide = window.matchMedia("(min-width: 1024px)");
    const sync = () => {
      scroller.style.scrollPaddingBottom = wide.matches ? "" : `${aside.offsetHeight}px`;
      aside.style.maxHeight = wide.matches ? `${scroller.clientHeight - parseFloat(getComputedStyle(aside).top)}px` : "";
    };
    const observer = new ResizeObserver(sync);
    observer.observe(aside);
    observer.observe(scroller);
    wide.addEventListener("change", sync);
    sync();
    return () => {
      observer.disconnect();
      wide.removeEventListener("change", sync);
      scroller.style.scrollPaddingBottom = "";
      aside.style.maxHeight = "";
    };
  }, []);

  // The issue named above Launch: the first one, unless the panel on screen
  // already prints it beside its own control.
  const shown = launch.issue ? shownIssue([launch.issue], panel) : null;

  // The server refused this click for the person's own model credential (422,
  // reason model_credential — the class failure-block.tsx grades a dead run by).
  // The door opens here, and the same launch fires again the moment the sign-in
  // lands, so a lapsed session costs one dialog rather than a trip to Getting
  // started. Launch stays the server's decision: nothing is pre-checked on the
  // cached status, which can be five minutes stale. The one exception is
  // preflight's own answer for this exact body: a 4xx it gave less than a minute
  // ago (never model_credential, never a 429) holds Launch (preflightBlock).
  // Once per click: a relaunch
  // refused again (a pin contradiction the same identity cannot repair) leaves
  // the sentence and waits for the person. Never over a door someone else
  // opened: openDoor overwrites the opener, and the strip's focus contract
  // (model-access-banner.tsx) reads it on close — and a click is consumed on
  // its first evaluation, whatever the door's state then, so a door that
  // closes later (Escape, a sign-in started from the strip) never brings this
  // dialog back with a relaunch armed for a click the person has moved past.
  // A pending relaunch does survive leaving the page with the dialog open
  // (the dialog is the shell's): a sign-in completed then launches the run
  // that click asked for and lands on it.
  const onLaunchRef = React.useRef(launch.onLaunch);
  onLaunchRef.current = launch.onLaunch;
  const onPreflightRef = React.useRef(preflight.onPreflight);
  onPreflightRef.current = preflight.onPreflight;
  // This screen's current body, read at fire time — the door is the shell's and
  // outlives the route, so it cannot be trusted to forget a stale closure.
  const bodyRef = React.useRef(launch.body);
  bodyRef.current = launch.body;
  // Only the Launch button's click arms a launch-after-sign-in; it records the
  // body the click was for, and the effect below hands it to the door once.
  const clickArm = React.useRef<{ body: string | null | undefined; principal: string; auth: number; draftRevision?: number } | null>(null);
  const autoOpened = React.useRef(false);
  const refusedProvider = launch.refusedProvider ?? "";
  React.useEffect(() => {
    if (!launch.credentialRefused || autoOpened.current) return;
    autoOpened.current = true;
    if (door.open) return;
    // #543 (§5.8): the door of the provider the refusal names — never the
    // agent or provider selected on screen, which may have moved since the
    // click (#146's ruling). A provider this person has no door for, or a
    // refusal naming no provider (a sign-in renewal that did not complete),
    // opens nothing (resolveDoor's null) and the sentence stands.
    const armed = clickArm.current;
    clickArm.current = null;
    if (refusedProvider) {
      let fired = false;
      door.openDoor({
        for: { provider: refusedProvider },
        returnTo: launchRef.current,
        // Launches only the body the click was for, once; anything else is a
        // re-check of what is on screen now. Not disarmed by onClosed, which
        // also runs on a successful sign-in.
        onSignedIn: () => {
          const owner = live.current.identity;
          const authCurrent = mounted.current ? owner.resolved && owner.authGeneration === getAuthGeneration() : armed?.auth === getAuthGeneration();
          if (armed && !fired && armed.body != null && armed.body === bodyRef.current && armed.principal === owner.principal && armed.draftRevision === live.current.draftRevision && authCurrent) {
            fired = true;
            return onLaunchRef.current();
          }
          return onPreflightRef.current?.();
        },
      });
    }
    // The strip catches up with what the server just said.
    void door.refresh();
  }, [launch.credentialRefused, refusedProvider, door]);

  // A preflight-origin refusal opens the same door, but its sign-in only
  // re-checks. At most once per body and provider, never over another door.
  const preflightOpened = React.useRef("");
  const refusal = preflight.refusal ?? null;
  React.useEffect(() => {
    if (!refusal?.provider) return;
    const key = `${refusal.provider}\n${refusal.body}`;
    if (preflightOpened.current === key) return;
    preflightOpened.current = key;
    if (door.open) return;
    door.openDoor({ for: { provider: refusal.provider }, returnTo: launchRef.current, onSignedIn: () => onPreflightRef.current?.() });
    void door.refresh();
  }, [refusal, door]);

  return (
    // Bounded to the viewport with its own scroll: with ceiling + tool rules +
    // 3 warnings the rail's real content runs past the fold at 1280x650. The
    // sections take the scroll first; a decision block too tall for what is
    // left joins the aside's own scroll, so its reason is never clipped and
    // Launch stays reachable.
    //
    // 100vh - 5rem, not -3rem: the sticky container is app-shell.tsx's
    // <main> (its own overflow-y:auto scroller), which starts below the
    // h-14 (3.5rem/56px) header — sticky's `top-6` (1.5rem/24px) offset is
    // relative to that scroller, not the viewport, so the rail's stuck
    // position sits at 3.5rem+1.5rem = 5rem from the viewport top, not 1.5rem.
    <aside
      ref={asideRef}
      aria-label={NEW_RUN_FLOW.RAIL_TITLE}
      className="scroll-thin sticky bottom-0 z-10 -mx-6 -mb-6 flex max-h-[60vh] flex-col overflow-y-auto border-t border-border bg-card px-6 py-3 lg:top-6 lg:bottom-auto lg:z-auto lg:mx-0 lg:mb-0 lg:h-fit lg:max-h-[calc(100vh-5rem)] lg:rounded-xl lg:border lg:p-4"
    >
      <button
        type="button"
        aria-expanded={sectionsOpen}
        aria-controls="nr-rail-sections"
        onClick={() => setSectionsOpen((open) => !open)}
        className="flex w-full shrink-0 items-center gap-1.5 py-0.5 text-left text-sm font-semibold text-foreground lg:hidden"
      >
        <ChevronRight className={cn("size-3 shrink-0", sectionsOpen && "rotate-90")} aria-hidden="true" />
        {NEW_RUN_FLOW.RAIL_TITLE}
      </button>
      <p className="mb-3 hidden shrink-0 text-sm font-semibold text-foreground lg:block">{NEW_RUN_FLOW.RAIL_TITLE}</p>

      {/* Focusable so a keyboard can scroll it; the summary comes before the
          decision block in reading order at every width. */}
      <div
        id="nr-rail-sections"
        tabIndex={0}
        role="group"
        aria-label={NEW_RUN_FLOW.RAIL_TITLE}
        className={cn("scroll-thin my-2 max-h-48 overflow-y-auto lg:my-0 lg:block lg:max-h-none lg:min-h-24", !sectionsOpen && "hidden")}
      >
        <RunRailSummary
          governanceProfile={governanceProfile}
          governanceContact={governanceContact}
          savedPolicy={savedPolicy}
          cc={cc}
          showModelWarning={showModelWarning}
          modelBlocked={modelBlocked}
          startup={startup}
          showHoldNote={showHoldNote}
          toolRules={toolRules}
          pushRules={pushRules}
          unattended={unattended}
          preflight={preflight}
          agentRow={agentRow}
          modelProvider={modelProvider}
          recordingDisabled={recordingDisabled}
          guardLink={guardLink}
        />
      </div>

      <RailVerdict preflight={preflight} />

      {launch.error && (
        // key={launch.errorSeq}: a re-announce of the SAME sentence still
        // needs a fresh DOM node — an update in place is silent to a screen
        // reader on a live region (#459).
        <p
          key={launch.errorSeq}
          role="alert"
          className="mt-3 flex items-start gap-1.5 text-xs text-danger"
        >
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          <span>
            <span className="sr-only">{RAIL.LAUNCH_ERROR_LABEL}</span> {launch.error}
          </span>
        </p>
      )}
      {launch.error && <PolicyRemedy policy={launch.policy} className="mt-1 block" />}

      <RailHold launch={launch} shown={shown} onIssue={(issue) => onIssue?.(issue)} canSetUpBarrier={canSetUpBarrier} guardLink={guardLink} />

      {/* Preflight lives on the Policy panel, next to the document it checks —
          one button, not two competing ones. Its result stays here, beside
          Launch, because "what would be clamped" is the last thing read before
          committing. #125: a 2xx launch (warnings or not) navigates straight to
          the run in the same tick, so there is no longer a held state for this
          button to become — any advisory warnings render on the run page
          instead (run-detail/launch-warnings-note.tsx). */}
      <div className="mt-4 flex gap-2">
        <Button
          ref={launchRef}
          type="button"
          className="flex-1"
          disabled={launch.disabled || !!launch.problem || !!launch.workspaceUnavailable || !!launch.noBarrier || !!launch.preflightBlock}
          onClick={() => {
            autoOpened.current = false;
            clickArm.current = { body: bodyRef.current, principal: identity.principal, auth: getAuthGeneration(), draftRevision: launch.draftRevision };
            void launch.onLaunch();
          }}
        >
          {/* The icon slot always renders (never just on launching) so the
              has-[>svg] padding rule and the icon+gap width never change —
              toggling `invisible` cannot shift "Launch run" sideways the way
              mounting/unmounting the icon would. */}
          <Loader2 className={launch.spinning ? "size-4 animate-spin" : "size-4 animate-spin invisible"} />
          Launch run
        </Button>
      </div>
      {/* #386's launch door (§2.4): opened automatically on a git_credential
          422, and closable without launching — the screen owns the popup
          (use-ado-connect.ts), this dialog only asks. `org` is the 422
          body's own (review finding F1): this dialog can be the very first
          thing a caller sees about the row, before any preflight verdict. */}
      <Dialog open={adoDialog.open} onOpenChange={(open) => !open && adoDialog.onCancel()}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{ADO.LAUNCH_DIALOG_TITLE}</DialogTitle>
            <DialogDescription>{ADO.LAUNCH_DIALOG_BODY(adoDialog.org)}</DialogDescription>
          </DialogHeader>
          {/* review finding F9: the browser refused the popup outright — a
              plain link is the fallback, opened by the browser itself. N1:
              the same connect poll starts alongside that navigation, so the
              dialog still advances when the person comes back connected. */}
          {adoDialog.blockedUrl && (
            <p className="text-xs text-muted-foreground">
              {ADO.CONNECT_POPUP_BLOCKED}{" "}
              <a
                href={adoDialog.blockedUrl}
                target="_blank"
                rel="noopener noreferrer"
                className={buttonVariants({ variant: "outline", size: "sm" })}
                onClick={adoDialog.onFallbackClick}
              >
                {ADO.CONNECT_POPUP_OPEN}
              </a>
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={adoDialog.onCancel}>
              {PEOPLE.CANCEL}
            </Button>
            <Button type="button" onClick={adoDialog.onConfirm} disabled={adoDialog.connecting}>
              <Loader2 className={adoDialog.connecting ? "size-4 animate-spin" : "size-4 animate-spin invisible"} />
              {ADO.CONNECT_CTA}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </aside>
  );
}

// Re-exported so new-run-screen.tsx's existing "./new-run-rail" import line
// covers it too — that file sits at its own 1000-line gate.
export { useAdoLaunchDoor } from "./use-ado-launch-door";

export { pushRulesIsSet } from "./new-run-rail-summary";
