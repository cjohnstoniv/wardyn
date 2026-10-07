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
// It stays a FIXED 320px beside the form and wraps UNDER it below lg, sections
// side by side. Squeezing a 320px rail into a phone column is how the
// consequences of a choice end up unreadable exactly where they are hardest to
// scroll back to.
import * as React from "react";
import { Link } from "react-router-dom";
import { Loader2, TriangleAlert } from "lucide-react";
import type { SetupModelProvider } from "../../../lib/types";
import { Button, buttonVariants } from "../../ui/button";
import { ConfinementChip, RiskBadge } from "../../wardyn/primitives";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { ADO } from "../../../lib/ado-entra-copy";
import { PEOPLE } from "../../../lib/people-access-copy";
import { NO_BARRIER, RAIL, RAIL_CHECK, RAIL_PROVIDER, RAIL_SETUP } from "../../wardyn/copy";
import { useRecordingDisabled } from "../../../lib/hooks/use-recording-disabled";
import { useOperator, useRequestIdentity, useUserViewSuperAdmin } from "../../wardyn/operator-context";
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
import { getAuthGeneration } from "../../../lib/api/core";
import { RunRailSummary } from "./new-run-rail-summary";

/** The exact sentence R5b/R5c's gate names — NOT_GRANTED for R5b;
 *  DEFAULT_OFF_ONLY with no other candidate, DEFAULT_OFF otherwise, for R5c —
 *  shared by ModelProviderSection's own inline line (new-run-rail-credentials.tsx)
 *  and RunRail's launch-problem caption (F4, Opus review round 2): the caption
 *  is suppressed ONLY when launch.problem is exactly this string, never for
 *  some OTHER, higher-priority problem (an empty title, …) that happens to be
 *  showing while a gate is also active. undefined with no gate (R9's shape, or
 *  the ordinary R1-R4/R6-R8 ones). */
function gateSentence(modelProvider: RunRailProps["modelProvider"]): string | undefined {
  const gate = modelProvider?.gate;
  if (!modelProvider || !gate) return undefined;
  if (gate.kind === "not_granted") return RAIL_PROVIDER.NOT_GRANTED(modelProvider.harnessLabel);
  const name = gate.provider.name ?? gate.provider.id;
  return modelProvider.candidates.length === 0
    ? RAIL_PROVIDER.DEFAULT_OFF_ONLY(name, modelProvider.harnessLabel)
    : RAIL_PROVIDER.DEFAULT_OFF(name, modelProvider.harnessLabel);
}

export function RunRail({
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
  const identity = useRequestIdentity();
  const live = React.useRef({ identity, draftRevision: launch.draftRevision });
  live.current = { identity, draftRevision: launch.draftRevision };
  const mounted = React.useRef(true);
  React.useEffect(() => () => { mounted.current = false; }, []);
  const userViewSuperAdmin = useUserViewSuperAdmin();
  const access = useViewAccess();
  const canSetUpBarrier = operator || (access === "session-user" && userViewSuperAdmin);
  // M1 S1: only rows that need attention; `satisfied` stays hidden.
  const setupRows = (preflight.result?.setup_items ?? []).filter((i) => i.status === "missing" || i.status === "unverified");
  // Both of finding 1's facts, read rather than asserted: where the model
  // credential lands, and whether this deployment records anything at all.
  // `recordingDisabled` is tri-state — undefined until /healthz answers.
  const recordingDisabled = useRecordingDisabled();
  const door = useModelAccessDoor();

  // Focus returns to Launch, not to #main-content (which would drop the
  // member at the top of the form they were mid-way through), when this
  // rail's own control opened the door. The door owns the return target: the
  // rail's own sign-in control unmounts the moment the state it described
  // clears (a completed sign-in), so by the time the dialog's onCloseAutoFocus
  // runs, document.activeElement — what a bare openDoor() would have
  // captured — is a detached node and focusOpener() fails, falling through to
  // #main-content; a separate effect here racing Radix's own FocusScope exit
  // trap cannot reliably win either. Passing Launch explicitly as `returnTo`
  // makes it the captured opener directly.
  const launchRef = React.useRef<HTMLButtonElement>(null);

  const onProviderSignIn = (p: SetupModelProvider) => door.openDoor({ for: { provider: p.id }, returnTo: launchRef.current });

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
    // A sticky box is clamped by its containing block — with
    // ceiling + tool rules + 3 warnings (member/warnings path) the rail's
    // real content runs ~700-730px, below the fold at 1280x650 with no way
    // to reach Launch. Bounded to the viewport with its own scroll.
    //
    // 100vh - 5rem, not -3rem: the sticky container is app-shell.tsx's
    // <main> (its own overflow-y:auto scroller), which starts below the
    // h-14 (3.5rem/56px) header — sticky's `top-6` (1.5rem/24px) offset is
    // relative to that scroller, not the viewport, so the rail's stuck
    // position sits at 3.5rem+1.5rem = 5rem from the viewport top, not 1.5rem.
    <aside className="h-fit rounded-xl border border-border bg-card p-4 lg:sticky lg:top-6 lg:max-h-[calc(100vh-5rem)] lg:overflow-y-auto">
      <p className="mb-3 text-sm font-semibold text-foreground">What this run can do</p>

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
        onProviderSignIn={onProviderSignIn}
      />

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
      {/* A disabled button that doesn't say why is a dead end: without
          client-side validation, an empty form would launch and the server's
          rejection would arrive after the fact. Suppressed ONLY when
          launch.problem IS the gate's (R5c's) own sentence — Opus review
          round 2, F4: ModelProviderSection above already names that exact
          fact inline, beside the select itself, so repeating it below would
          only echo it — but a DIFFERENT, higher-priority problem (an empty
          title, an unparseable policy, …) must still show here even while a
          gate is also active, since it's a separate reason nothing has
          launched yet. */}
      {launch.problem && !launch.inFlight && launch.problem !== gateSentence(modelProvider) && (
        <p className="mt-2 text-center text-xs text-muted-foreground">
          {launch.problem}
          {/* f-f4: the same CTA, under the same operator/super-admin rule, as
              the no-barrier line below. */}
          {launch.problem === RAIL_SETUP.BACKEND_BLOCK && canSetUpBarrier && (
            <>
              {" "}
              <Link to={NO_BARRIER.ADMIN_ROUTE} className="font-medium text-info hover:underline">
                {NO_BARRIER.CTA}
              </Link>
              .
            </>
          )}
          {launch.problemLink && (
            <>
              {" "}
              <Link to={launch.problemLink.to} className="font-medium text-info hover:underline">
                {launch.problemLink.label}
              </Link>
            </>
          )}
        </p>
      )}
      {/* #214 — the one control that genuinely cannot work says so beside
          itself, not in a tooltip, with a route to the step that fixes it.
          A separate line from `problem` above (never both: the Barrier
          section's own TierPicker card already gives the detailed reason;
          this is Launch's own, short pointer to the fix).

          #1328 review round 2, R2-1 — the CTA itself renders only for a
          caller who can actually reach the Environment step: an operator
          (already resolves NO_BARRIER.ADMIN_ROUTE directly, whichever view
          they're in) or a super admin in the User view (#1335: session-user
          AND /me's user_view_super_admin — ViewGate's own "to-admin"
          interstitial asks before switching, see NO_BARRIER's doc comment).
          Everyone else reads the reason alone, a security admin in the User
          view included; there is nothing behind that route they may open. */}
      {launch.noBarrier && !launch.inFlight && (
        <p className="mt-2 text-center text-xs text-muted-foreground">
          {NO_BARRIER.LAUNCH_REASON}
          {canSetUpBarrier && (
            <>
              {" "}
              <Link to={NO_BARRIER.ADMIN_ROUTE} className="font-medium text-info hover:underline">
                {NO_BARRIER.CTA}
              </Link>
              .
            </>
          )}
        </p>
      )}

      {(preflight.checking || preflight.notChecked) && (
        <p data-testid="preflight-check-state" className="mt-2 flex items-center justify-center gap-1.5 text-xs text-muted-foreground">
          {preflight.checking ? (
            <>
              <Loader2 className="size-3 animate-spin" />
              {RAIL_CHECK.CHECKING}
            </>
          ) : (
            RAIL_CHECK.NOT_CHECKED
          )}
        </p>
      )}

      {preflight.error && (
        <p
          key={preflight.errorSeq}
          role="alert"
          className="mt-3 flex items-start gap-1.5 text-xs text-danger"
        >
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          <span>
            <span className="sr-only">{RAIL.PREFLIGHT_ERROR_LABEL}</span> {preflight.error}
          </span>
        </p>
      )}
      {/* Unframed: a bordered box inside the rail card is a card in a card
          (CONSOLE-RULES §9). A divider is what separates a section from the
          section above it. */}
      {preflight.result && (
        <div className="mt-4 border-t border-border pt-3" data-testid="preflight-result">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            {preflight.result.overall_risk && <RiskBadge level={preflight.result.overall_risk} />}
            <ConfinementChip value={preflight.result.enforced_confinement_class} />
          </div>
          {setupRows.length > 0 && (
            <div className="mb-1.5">
              <p className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">{RAIL_SETUP.HEADING}</p>
              <ul className="space-y-0.5 text-xs">
                {setupRows.map((r) => (
                  <li key={r.id} className={r.kind === "backend" && r.status === "missing" ? "text-danger" : "text-warning"}>
                    {r.label}
                    {r.detail ? ` — ${r.detail}` : ""}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {preflight.result.warnings && preflight.result.warnings.length > 0 ? (
            <ul className="list-disc space-y-0.5 pl-4 text-xs text-warning">
              {preflight.result.warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          ) : (
            <p className="text-xs text-muted-foreground">{AGENTS.EFFECTIVE_NONE}</p>
          )}
        </div>
      )}

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
