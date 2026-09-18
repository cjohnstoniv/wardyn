/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// THE STRIP — the console's per-person notification surface for model access,
// and the sign-in itself.
//
// 0.7.6, field-report finding 2: every actionable model-access state is
// published on /setup/status for every caller, and exactly two screens read it.
// The product had a per-person credential lifecycle and no per-person
// notification surface, so "not signed in", "lapsed" and "lapsing" reached a
// person only if they happened to open Getting Started — or by a run failing.
//
// Structure mirrors member-mode-banner.tsx deliberately: one `role="status"`
// band, `z-50` so the cockpit's focus-mode overlay (z-40) cannot paint over it,
// an UNDERLINED TEXT button rather than a teal one (CONSOLE-RULES §6 allows one
// `default` Button per surface and this strip is on every surface), and no
// hiding in focus mode — Finding 4's mid-run re-auth needs exactly this surface
// on the cockpit.
//
// It renders LAST in the shell's banner stack: a dead control plane or an
// unknown identity is the better explanation of what you are looking at, and is
// read first.

import * as React from "react";
import { AlertTriangle, Clock } from "lucide-react";
import { useLocation } from "react-router-dom";
import { toast } from "sonner";

import { Dialog, DialogContent, DialogDescription, DialogTitle } from "../ui/dialog";
import { HarnessLoginPane, type HarnessLoginPaneHandle } from "../screens/settings/harness-login-pane";
import { relativeTime, absoluteTime } from "../../lib/format";
import type { ModelAccessDoor } from "../../lib/model-access";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { useModelAccessDoor } from "./model-access-context";
import { useOperator, usePrincipal } from "./operator-context";

/** What the strip says for one door, for one viewer. Pure, and exported for
 *  the tests: the audience/state table is the feature. */
export interface ModelAccessStripCopy {
  /** Our own sentence; "" for the one state where the server's action line
   *  renders ALONE (a member under a dead shared credential). */
  sentence: string;
  /** The server's action, rendered VERBATIM — only where it carries what the
   *  sentence and the button cannot. */
  action: string;
  /** The absolute deadline, as the sentence's `title`; "" when there is none. */
  title: string;
  tone: "warning" | "info";
  /** Whether this viewer may set the strip aside for the session. */
  dismissible: boolean;
}

const NOTHING: ModelAccessStripCopy = {
  sentence: "",
  action: "",
  title: "",
  tone: "warning",
  dismissible: false,
};

/** The server's action line adds nothing when it is byte-identical to the
 *  button's own label (modelaccess.go's modelAccessSignInAction IS
 *  AGENTS.SIGN_IN_AWS) — printing it would render the button's label as prose
 *  (round-2 UX S1). What survives this test is the pin-contradicted pair, which
 *  names two account/role pairs no sentence of ours could carry. */
function serverAction(door: ModelAccessDoor): string {
  return door.action && door.action !== AGENTS.SIGN_IN_AWS ? door.action : "";
}

/**
 * modelAccessStripCopy — the audience/state table, as one function.
 *
 * `claimed` is door ownership: when a page surface renders its own sign-in
 * control the strip drops its BUTTON and keeps its sentence, so `expired_signin`
 * swaps to the sentence without the imperative (round-2 UX S7) — "sign in
 * again" beside no button points at nothing.
 */
export function modelAccessStripCopy(
  door: ModelAccessDoor,
  viewer: { operator: boolean },
  claimed: boolean,
): ModelAccessStripCopy {
  // relativeTime ("in 3h") in the sentence, absoluteTime (with seconds) as the
  // title: the server composes an RFC3339 UTC stamp, and a deadline 20 minutes
  // out — reachable for a no-refresh-token blob whose ACCESS token lapses —
  // needs urgency rather than a calendar stamp (round-2 UX B4/B6).
  const when = door.deadline ? relativeTime(door.deadline) : "";
  const title = door.deadline ? absoluteTime(door.deadline) : "";
  switch (door.state) {
    case "not_configured":
      return {
        sentence: MODEL_ACCESS_BANNER.NOT_SIGNED_IN,
        action: serverAction(door),
        title: "",
        tone: "warning",
        // "NOT an error — it is the first-run state" (modelaccess.go). A member
        // who never launches an agent would otherwise read a warning on every
        // page forever, while the rail and Getting Started keep saying it.
        dismissible: true,
      };
    case "expired_signin":
      return {
        sentence: claimed ? MODEL_ACCESS_BANNER.EXPIRED_SHORT : MODEL_ACCESS_BANNER.EXPIRED,
        action: serverAction(door),
        title: "",
        tone: "warning",
        dismissible: false,
      };
    case "expiring":
      return {
        // A lapse that has not happened yet is a STATE, not an alarm (round-1
        // UX S3, round-2 S14): full amber on every screen for 24 h in the same
        // tint as "Control plane unreachable" trains people to ignore amber.
        sentence: when
          ? (door.perUser ? MODEL_ACCESS_BANNER.EXPIRING : MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRING).replace(
              "{when}",
              when,
            )
          : "",
        // No separate action line — the deadline is IN the sentence (S1) —
        // EXCEPT against a daemon that sends no `deadline`, where the server's
        // own "Sign in again before <ts>" is all there is.
        action: when ? "" : door.action,
        title,
        tone: "info",
        dismissible: false,
      };
    case "shared_expired":
      // The one credential every run rides. For its ADMIN that is a sentence
      // about blast radius and a repair they can make; for everybody else it is
      // the server's instruction, rendered alone — a second sentence of ours
      // would say the same fact twice (round-1 UX S13).
      return viewer.operator
        ? {
            sentence: MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED,
            // …but never the server's "ask your admin" line: the reader IS the
            // admin. Only a pin-contradicted pair would survive here.
            action: "",
            title: "",
            tone: "warning",
            dismissible: false,
          }
        : {
            sentence: "",
            action: door.action,
            title: "",
            tone: "warning",
            // The one state where the viewer cannot act at all: an
            // undismissable actionless nag on every screen forever is worse
            // than the first-run case (round-2 UX S4).
            dismissible: true,
          };
    default:
      // live, not_applicable, "" (a legacy install), and any state a future
      // daemon grades that this console does not know: say nothing. Inventing a
      // sentence for an unknown state is how "Not signed in" got painted over a
      // credential nobody had a reading of.
      return NOTHING;
  }
}

/** Per-viewer, per-browsing-context. Keyed on the viewer's subject because
 *  sessionStorage survives a sign-out in the same tab (round-2 UX S3): an
 *  unkeyed flag would pre-dismiss the strip for the NEXT person on that tab.
 *  Every access is wrapped — storage throws in a hardened browser profile. */
function dismissKey(principal: string): string {
  return `wardyn.modelAccessDismissed.${principal}`;
}

function useSessionDismissal(principal: string): [boolean, () => void] {
  const key = dismissKey(principal);
  const read = React.useCallback(() => {
    try {
      return window.sessionStorage.getItem(key) === "1";
    } catch {
      return false;
    }
  }, [key]);
  const [dismissed, setDismissed] = React.useState(read);
  React.useEffect(() => setDismissed(read()), [read]);
  const dismiss = React.useCallback(() => {
    try {
      window.sessionStorage.setItem(key, "1");
    } catch {
      /* a tab that cannot remember simply keeps the strip — never a failure */
    }
    setDismissed(true);
  }, [key]);
  return [dismissed, dismiss];
}

/**
 * ModelAccessSignInDialog — the door itself. ONE instance, mounted by the strip
 * (which the shell mounts once); every caller opens it through the context.
 *
 * The style override is connection-cards.tsx's, verbatim and for its reasons:
 * DialogContent's own `sm:max-w-lg` wins the cascade against any class, and
 * `translate`/`transform` are separate CSS properties in Tailwind v4, so the
 * centring has to be stated as a fact rather than raced. `min-w-0` lets the
 * 512-column login terminal (LOGIN_PTY_COLS) shrink inside the grid.
 */
function ModelAccessSignInDialog({
  open,
  perUser,
  onCancel,
  onDone,
}: {
  open: boolean;
  perUser: boolean;
  onCancel: () => void;
  onDone: () => void;
}) {
  const paneRef = React.useRef<HarnessLoginPaneHandle>(null);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (next) return;
        // Escape and an overlay click close the PARENT, and the pane's onCancel
        // is child-to-parent: without routing through the pane's own handle a
        // dismissal here would orphan a live "wardyn: sign-in running" run on
        // the member's board for up to 30 minutes (round-1 UX S4).
        if (paneRef.current) paneRef.current.cancel();
        else onCancel();
      }}
    >
      <DialogContent
        className="scroll-thin inset-0 top-0 left-0 m-auto h-fit max-h-[92vh] overflow-y-auto"
        style={{
          width: "min(96vw, 72rem)",
          maxWidth: "min(96vw, 72rem)",
          translate: "none",
          transform: "none",
        }}
      >
        <DialogTitle>{MODEL_ACCESS_BANNER.DIALOG_TITLE}</DialogTitle>
        <DialogDescription className="sr-only">
          {MODEL_ACCESS_BANNER.DIALOG_DESCRIPTION}
        </DialogDescription>
        {open && (
          <div className="min-w-0">
            <HarnessLoginPane
              provider="aws"
              // The same rule agents-tab.tsx and connection-cards.tsx already
              // follow: under a per_user row the org's access portal is stored
              // and the server uses it, so asking for one is a field whose
              // value cannot take effect. A shared row has nothing stored.
              startURLManaged={perUser}
              paneRef={paneRef}
              onDone={onDone}
              onCancel={onCancel}
            />
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}

/**
 * ModelAccessBanner — the strip, and the one dialog instance beside it.
 *
 * Renders nothing (but keeps its live region mounted) when there is nothing to
 * say, so the shell needs no conditional of its own.
 */
export function ModelAccessBanner() {
  const door = useModelAccessDoor();
  const operator = useOperator();
  const principal = usePrincipal();
  const { pathname } = useLocation();
  const [dismissed, dismiss] = useSessionDismissal(principal);
  // Whether THIS strip opened the door. A page surface that opened it restores
  // focus to its own neighbour (Launch, on New Run); the strip's own button no
  // longer exists once the state clears, and Radix would return focus to a
  // trigger that is gone.
  const openedHere = React.useRef(false);

  const copy = modelAccessStripCopy(door, { operator }, door.claimed);
  const under = (prefix: string) => pathname === prefix || pathname.startsWith(`${prefix}/`);
  // Never on /setup — the page IS the door. On /settings and /providers only
  // for an OPERATOR: those pages already mount the same pane for the same
  // states, and a second control named "Sign in to AWS" on one page is the U-13
  // defect member-getting-started.tsx already fixed once. For a MEMBER the
  // Settings card's AWS button is `disabled={!operator}` ("Requires the admin
  // role."), so hiding the strip there would strand exactly the person the
  // refusal sentence sends there (round-1 UX B1/S12).
  const suppressed = under("/setup") || (operator && (under("/settings") || under("/providers")));
  const show = door.needsAttention && !suppressed && !(copy.dismissible && dismissed);

  return (
    // The live region stays MOUNTED and its text is updated in place — never
    // remounted by key (Codex #15): role="status" announces CHANGES, and a
    // remount is a mount, which is the one thing it does not reliably announce.
    <div role="status">
      {show && (copy.sentence || copy.action) && (
        <div
          className={
            "relative z-50 flex shrink-0 flex-wrap items-center gap-2 border-b px-4 py-2 text-sm " +
            (copy.tone === "info"
              ? "border-info/25 bg-info-subtle text-info"
              : "border-border bg-warning-subtle text-warning")
          }
        >
          {copy.tone === "info" ? (
            <Clock className="size-4 shrink-0" />
          ) : (
            <AlertTriangle className="size-4 shrink-0" />
          )}
          {copy.sentence && <span title={copy.title || undefined}>{copy.sentence}</span>}
          {/* The server's own words, verbatim — never reworded client-side. */}
          {copy.action && <span>{copy.action}</span>}
          {door.actionable && !door.claimed && (
            <button
              type="button"
              onClick={() => {
                openedHere.current = true;
                door.openDoor();
              }}
              className="font-medium underline underline-offset-2"
            >
              {AGENTS.SIGN_IN_AWS}
            </button>
          )}
          {copy.dismissible && (
            <button
              type="button"
              onClick={dismiss}
              className="font-medium underline underline-offset-2 opacity-75"
            >
              {MODEL_ACCESS_BANNER.NOT_NOW}
            </button>
          )}
        </div>
      )}
      <ModelAccessSignInDialog
        open={door.open}
        perUser={door.perUser}
        onCancel={() => {
          openedHere.current = false;
          door.closeDoor();
        }}
        onDone={() => {
          door.closeDoor();
          void door.refresh();
          // CONSOLE-RULES §9's transient case: the only other evidence is a
          // strip that disappears, and a surface vanishing is not a
          // confirmation (round-1 UX S5).
          toast.success(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST);
          if (openedHere.current) {
            openedHere.current = false;
            // The skip-to-main target the shell already carries
            // (app-shell.tsx's <main id="main-content" tabIndex={-1}>).
            document.getElementById("main-content")?.focus();
          }
        }}
      />
    </div>
  );
}
