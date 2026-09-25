/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The strip — the console's per-person notification surface for model access,
// and the sign-in itself.
//
// Every actionable model-access state is
// published on /setup/status for every caller, and exactly two screens read it.
// The product had a per-person credential lifecycle and no per-person
// notification surface, so "not signed in", "lapsed" and "lapsing" reached a
// person only if they happened to open Getting Started — or by a run failing.
//
// Structure mirrors user-preview.tsx deliberately: one band inside the
// shell's `role="status"` region, `z-50` so the cockpit's focus-mode overlay
// (z-40) cannot paint over it,
// an underlined text button rather than a teal one (CONSOLE-RULES §6 allows one
// `default` Button per surface and this strip is on every surface), and no
// hiding in focus mode — Finding 4's mid-run re-auth needs exactly this surface
// on the cockpit.
//
// It rendered last in the shell's banner stack until #162 added
// ConfinementPostureBanner after it: a dead control plane or an unknown
// identity is the better explanation of what you are looking at, and is read
// first, but a per-person credential block outranks a cluster-wide posture
// note nobody but an admin can act on.

import * as React from "react";
import { AlertTriangle, Clock, Loader2 } from "lucide-react";
import { useLocation } from "react-router-dom";
import { toast } from "sonner";

import { Dialog, DialogContent, DialogDescription, DialogTitle } from "../ui/dialog";
import type { HarnessLoginPaneHandle } from "../screens/settings/harness-login-pane";
import { relativeTime, absoluteTime } from "../../lib/format";
import type { ModelAccessDoor } from "../../lib/model-access";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { useModelAccessDoor } from "./model-access-context";
import { usePrincipal } from "./operator-context";
import { screenPath, type ConsoleView } from "./console-view";

// Lazy, and that is a gate rather than a nicety: this strip is mounted by
// app-shell.tsx, which is in the entry chunk, and the login pane drags xterm +
// addon-fit + its stylesheet behind it. A static import put ~200 kB of terminal
// into the first paint of every screen — bundle-split.test.ts fails on exactly
// that. The chunk is fetched when somebody opens the door, which is the same
// rule App.tsx's lazy routes already follow for the same dependency.
const HarnessLoginPane = React.lazy(() =>
  import("../screens/settings/harness-login-pane").then((m) => ({ default: m.HarnessLoginPane })),
);

/** What the strip says for one door, for one viewer. Pure, and exported for
 *  the tests: the audience/state table is the feature. */
export interface ModelAccessStripCopy {
  /** Our own sentence; "" for the one state where the server's action line
   *  renders alone (a member under a dead shared credential). */
  sentence: string;
  /** The server's action, rendered verbatim — only where it carries what the
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
 *  button's own label (modelaccess.go's modelAccessSignInAction is
 *  AGENTS.SIGN_IN_AWS) — printing it would render the button's label as prose.
 *  What survives this test is the pin-contradicted pair, which
 *  names two account/role pairs no sentence of ours could carry. */
function serverAction(door: ModelAccessDoor): string {
  return door.action && door.action !== AGENTS.SIGN_IN_AWS ? door.action : "";
}

/**
 * modelAccessStripCopy — the audience/state table, as one function.
 *
 * `claimed` is door ownership: when a page surface renders its own sign-in
 * control the strip drops its button and keeps its sentence, so `expired_signin`
 * swaps to the sentence without the imperative — "sign in
 * again" beside no button points at nothing.
 */
export function modelAccessStripCopy(
  door: ModelAccessDoor,
  viewer: { operator: boolean },
  claimed: boolean,
): ModelAccessStripCopy {
  // relativeTime ("in 3h") in the sentence, absoluteTime (with seconds) as the
  // title: the server composes an RFC3339 UTC stamp, and a deadline 20 minutes
  // out — reachable for a no-refresh-token blob whose access token lapses —
  // needs urgency rather than a calendar stamp.
  const when = door.deadline ? relativeTime(door.deadline) : "";
  const title = door.deadline ? absoluteTime(door.deadline) : "";
  switch (door.state) {
    case "not_configured":
      return {
        sentence: MODEL_ACCESS_BANNER.NOT_SIGNED_IN,
        action: serverAction(door),
        title: "",
        tone: "warning",
        // "Not an error — it is the first-run state" (modelaccess.go). A member
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
        // A lapse that has not happened yet is a state, not an alarm: full
        // amber on every screen for 24 h in the same
        // tint as "Control plane unreachable" trains people to ignore amber.
        sentence: when
          ? (door.perUser ? MODEL_ACCESS_BANNER.EXPIRING : MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRING).replace(
              "{when}",
              when,
            )
          : "",
        // No separate action line — the deadline is in the sentence —
        // except against a daemon that sends no `deadline`, where the server's
        // own "Sign in again before <ts>" is all there is.
        action: when ? "" : door.action,
        title,
        tone: "info",
        dismissible: false,
      };
    case "shared_expired":
      // The one credential every run rides. For its admin that is a sentence
      // about blast radius and a repair they can make; for everybody else it is
      // the server's instruction, rendered alone — a second sentence of ours
      // would say the same fact twice.
      return viewer.operator
        ? {
            sentence: MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED,
            // …but never the server's "ask your admin" line: the reader is the
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
            // than the first-run case.
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
 *  sessionStorage survives a sign-out in the same tab: an
 *  unkeyed flag would pre-dismiss the strip for the next person on that tab.
 *  Every access is wrapped — storage throws in a hardened browser profile. */
function dismissKey(principal: string): string {
  return `wardyn.modelAccessDismissed.${principal}`;
}

function useSessionDismissal(principal: string): [boolean, () => void] {
  const key = dismissKey(principal);
  const read = React.useCallback(() => {
    // Never read (or write) an unkeyed flag: "" is /me unresolved or failed,
    // and a flag stored under it would belong to whoever sits at this tab next.
    if (!principal) return false;
    try {
      return window.sessionStorage.getItem(key) === "1";
    } catch {
      return false;
    }
  }, [key, principal]);
  const [dismissed, setDismissed] = React.useState(read);
  React.useEffect(() => setDismissed(read()), [read]);
  const dismiss = React.useCallback(() => {
    if (principal) {
      try {
        window.sessionStorage.setItem(key, "1");
      } catch {
        /* a tab that cannot remember simply keeps the strip — never a failure */
      }
    }
    // The in-memory hide stands either way: the click was a person saying "not
    // now", and honouring it for this mount costs nothing.
    setDismissed(true);
  }, [key, principal]);
  return [dismissed, dismiss];
}

/**
 * ModelAccessSignInDialog — the door itself. One instance, mounted by the strip
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
  onCloseAutoFocus,
}: {
  open: boolean;
  perUser: boolean;
  onCancel: () => void;
  onDone: () => void;
  /** Where focus goes when the dialog closes. Radix's default returns it to the
   *  trigger, which after a successful sign-in no longer exists (the strip is
   *  gone) — and focusing anything from an onDone/onCancel handler is too early:
   *  the FocusScope trap is still mounted and takes focus back, landing it on
   *  <body>. This is the one callback that fires after the trap is released. */
  onCloseAutoFocus: (event: Event) => void;
}) {
  const paneRef = React.useRef<HarnessLoginPaneHandle>(null);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (next) return;
        // Escape and an overlay click close the parent, and the pane's onCancel
        // is child-to-parent: without routing through the pane's own handle a
        // dismissal here would orphan a live "wardyn: sign-in running" run on
        // the member's board for up to 30 minutes.
        if (paneRef.current) paneRef.current.cancel();
        else onCancel();
      }}
    >
      <DialogContent
        onCloseAutoFocus={onCloseAutoFocus}
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
            {/* The same mark + spinner App.tsx's RouteFallback shows for a lazy
                route: a chunk in flight reads as the console still connecting,
                never as a broken dialog. */}
            <React.Suspense
              fallback={
                <div className="flex min-h-[8rem] items-center justify-center" role="status" aria-live="polite">
                  <Loader2 className="size-5 animate-spin text-muted-foreground" />
                  <span className="sr-only">Loading…</span>
                </div>
              }
            >
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
            </React.Suspense>
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
 *
 * `view` (admin-member-modes-design.md §4.2, M-3) — the Admin view shows only
 * the pre-MP shared-credential branch, until MP-4b gives it its own per-person
 * screen: a per-user deployment's strip is each person's own credential, which
 * belongs to the User view. Defaults to "user" (the permissive, unrestricted
 * reading) so every caller that mounts this strip directly — outside
 * app-shell.tsx, which always passes its resolved view — behaves exactly as it
 * did before the split.
 */
export function ModelAccessBanner({ view = "user" }: { view?: ConsoleView } = {}) {
  const door = useModelAccessDoor();
  // The door's own resolved answer, never a second useOperator(): that hook's
  // default is fail-open, and a suppression computed from it would withhold the
  // strip on /settings from the member it exists for, in exactly the window the
  // door refuses to grade.
  const operator = door.operator;
  const principal = usePrincipal();
  const { pathname } = useLocation();
  const [dismissed, dismiss] = useSessionDismissal(principal);
  // Whether this strip opened the door. A page surface that opened it restores
  // focus to its own neighbour (Launch, on New Run); the strip's own button no
  // longer exists once the state clears, and Radix would return focus to a
  // trigger that is gone.
  const openedHere = React.useRef(false);
  // Whether the door closed on a completed sign-in rather than a cancellation.
  // The difference decides where focus goes: a cancellation leaves every
  // control exactly where it was, a completion takes the surface away.
  const completed = React.useRef(false);

  const copy = modelAccessStripCopy(door, { operator }, door.claimed);
  // The same screen in either view (the Admin view's /admin/setup is /setup).
  const path = screenPath(pathname);
  const under = (prefix: string) => path === prefix || path.startsWith(`${prefix}/`);
  // Never on /setup — the page is the door. On /settings, /providers and
  // /account only for an operator: those pages already mount the same pane for
  // the same states, and a second control named "Sign in to AWS" on one page is the U-13
  // defect member-getting-started.tsx already fixed once. For a member the
  // Settings card's AWS button is `disabled={!operator}` ("Requires the admin
  // role."), so hiding the strip there would strand exactly the person the
  // refusal sentence sends there.
  const suppressed = under("/setup") || (operator && (under("/settings") || under("/providers") || under("/account")));
  // §4.2: a per-user deployment's states ("your own credentials") are a User-
  // view concern; the Admin view keeps only what a shared-credential
  // deployment would show (door.perUser is the deployment's own shape, not
  // this viewer's — see model-access.ts).
  const adminViewSuppressed = view === "admin" && door.perUser;
  const show = door.needsAttention && !suppressed && !adminViewSuppressed && !(copy.dismissible && dismissed);

  return (
    // No live region of its own: app-shell.tsx mounts the `role="status"`
    // wrapper eagerly around this lazy chunk, so the first state to arrive is a
    // text change inside a region that was already there — role="status"
    // announces changes, and mount content is the one thing it does not
    // reliably announce (Codex #15).
    <>
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
        onCancel={door.closeDoor}
        onDone={() => {
          completed.current = true;
          // The completion path, not closeDoor(): it also runs what the opener
          // asked for on a completed sign-in (the New Run rail's relaunch).
          door.signedIn();
          void door.refresh();
          // CONSOLE-RULES §9's transient case: the only other evidence is a
          // strip that disappears, and a surface vanishing is not a
          // confirmation.
          toast.success(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST);
        }}
        onCloseAutoFocus={(event) => {
          // This handler owns the restore, always: Radix's default focuses the
          // element it remembered when the door opened, and by the time it runs
          // that element may have been unmounted with the state that justified
          // it — which lands focus on <body>, where a keyboard user's next Tab
          // starts from the top of the document.
          event.preventDefault();
          const done = completed.current;
          const fromStrip = openedHere.current;
          completed.current = false;
          openedHere.current = false;
          // A cancellation changes nothing on the page, and a sign-in started
          // from a page control leaves that page's own control to return to
          // (the rail's Launch neighbour, the failure block's): go back to the
          // control the person activated, which is the ordinary dialog
          // contract. The one case that must not is the strip's own completed
          // sign-in — the strip is unmounting with the state it described.
          if ((!done || !fromStrip) && door.focusOpener()) return;
          // The skip-to-main target the shell already carries — the nearest
          // thing to what the person was reading (app-shell.tsx's
          // <main id="main-content" tabIndex={-1}>).
          document.getElementById("main-content")?.focus();
        }}
      />
    </>
  );
}
