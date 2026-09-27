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
import { AlertTriangle, Clock } from "lucide-react";
import { Link, useLocation } from "react-router-dom";
import { toast } from "sonner";

import { relativeTime, absoluteTime } from "../../lib/format";
import {
  MODEL_ACCESS_AGENT,
  isPerUserSsoRow,
  providerAttention,
  type ModelAccessDoor,
  type ProviderAttention,
} from "../../lib/model-access";
import type { SetupStatus } from "../../lib/types";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { useModelAccessDoor, useShellSetupStatus } from "./model-access-context";
import { useOperatorResolved, usePrincipal } from "./operator-context";
import { screenPath, viewOfPath, type ConsoleView } from "./console-view";
import { DoorDialog } from "./door-dialog";
import { BANNER, CONNECTIONS } from "./copy/door";

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

/** One provider's line on the strip (§5.5, B1–B5), with the button that opens
 *  its door. `harnesses` is the display names of the agents whose default it
 *  is. Pure, and exported for the tests: the kind/state table is the feature.
 *  null for a state packet D draws no line for. */
export function providerStripLine(
  a: ProviderAttention,
  harnesses: string,
): { sentence: string; button: string; title: string; tone: "warning" | "info"; dismissible: boolean } | null {
  const name = a.provider.name || a.provider.id;
  switch (a.provider.kind) {
    case "bedrock_sso":
      if (a.state === "expired_signin")
        return { sentence: CONNECTIONS.C6_LINE(name), button: AGENTS.SIGN_IN_AWS, title: "", tone: "warning", dismissible: false };
      if (a.state === "expiring") {
        // {when} keeps its existing format (packet D): relative in the
        // sentence, the absolute instant as its title.
        const when = a.deadline ? relativeTime(a.deadline) : "";
        const title = a.deadline ? absoluteTime(a.deadline) : "";
        return when ? { sentence: BANNER.B3(name, when), button: AGENTS.SIGN_IN_AWS, title, tone: "info", dismissible: false } : null;
      }
      // B1 is the first-run state, the one line with "Not now".
      return { sentence: BANNER.B1(harnesses, name), button: AGENTS.SIGN_IN_AWS, title: "", tone: "warning", dismissible: true };
    case "anthropic_subscription":
      return { sentence: BANNER.B5(harnesses), button: CONNECTIONS.SIGN_IN_CLAUDE, title: "", tone: "warning", dismissible: false };
    default: {
      const token = a.provider.kind === "custom_endpoint";
      return {
        sentence: BANNER.B4(harnesses, name, token),
        button: token ? CONNECTIONS.ADD_TOKEN : CONNECTIONS.ADD_KEY,
        title: "",
        tone: "warning",
        dismissible: false,
      };
    }
  }
}

/** "Claude Code" / "Claude Code and Codex CLI", from the roster's own names. */
function harnessNames(status: SetupStatus | null, ids: string[]): string {
  return ids.map((id) => status?.harnesses?.find((h) => h.id === id)?.display || id).join(" and ");
}

/** "Not now", per provider (packet MP-D QD-3): dismissing one provider must not
 *  hide another that needs the person later. Same keying and storage rules as
 *  useSessionDismissal above. */
function useProviderDismissals(principal: string): [(id: string) => boolean, (id: string) => void] {
  const [local, setLocal] = React.useState<string[]>([]);
  const key = (id: string) => `${dismissKey(principal)}.${id}`;
  const dismissed = (id: string) => {
    if (local.includes(id)) return true;
    if (!principal) return false;
    try {
      return window.sessionStorage.getItem(key(id)) === "1";
    } catch {
      return false;
    }
  };
  const dismiss = (id: string) => {
    if (principal) {
      try {
        window.sessionStorage.setItem(key(id), "1");
      } catch {
        /* the in-memory hide below stands either way */
      }
    }
    setLocal((ids) => [...ids, id]);
  };
  return [dismissed, dismiss];
}

const STRIP_CLASS = "relative z-50 flex shrink-0 flex-wrap items-center gap-2 border-b px-4 py-2 text-sm ";
const TONE_CLASS = {
  info: "border-info/25 bg-info-subtle text-info",
  warning: "border-border bg-warning-subtle text-warning",
} as const;
const LINK_CLASS = "font-medium underline underline-offset-2";

/**
 * ModelAccessBanner — the strip, and the one door beside it (door-dialog.tsx).
 *
 * Renders nothing (but keeps its live region mounted) when there is nothing to
 * say, so the shell needs no conditional of its own.
 *
 * Two strips, never both: with a model-provider block the strip speaks per
 * provider (packet MP-D §5.5), in the User view only; without one it is
 * today's single AWS strip, graded from model_access.
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
  const { status } = useShellSetupStatus();
  const resolved = useOperatorResolved();
  // The door's own resolved answer, never a second useOperator(): that hook's
  // default is fail-open, and a suppression computed from it would withhold the
  // strip on /settings from the member it exists for, in exactly the window the
  // door refuses to grade.
  const operator = door.operator;
  const principal = usePrincipal();
  const { pathname } = useLocation();
  const [dismissed, dismiss] = useSessionDismissal(principal);
  const [providerDismissed, dismissProvider] = useProviderDismissals(principal);
  // Whether this strip opened the door. A page surface that opened it restores
  // focus to its own neighbour (Launch, on New Run); the strip's own button no
  // longer exists once the state clears, and Radix would return focus to a
  // trigger that is gone.
  const openedHere = React.useRef(false);
  // Whether the door closed on a completed sign-in rather than a cancellation.
  // The difference decides where focus goes: a cancellation leaves every
  // control exactly where it was, a completion takes the surface away.
  const completed = React.useRef(false);

  // The same screen in either view (the Admin view's /admin/setup is /setup).
  const path = screenPath(pathname);
  const under = (prefix: string) => path === prefix || path.startsWith(`${prefix}/`);
  const userView = viewOfPath(pathname) === "user";
  const providerMode = !!status?.model_providers;

  const copy = modelAccessStripCopy(door, { operator }, door.claimed);
  // Never on /setup — the page is the door. On /settings, /providers and
  // /account only for an operator: those pages carry their own sign-in button
  // for the same states, and a second control named "Sign in to AWS" on one
  // page is the U-13 defect member-getting-started.tsx already fixed once. For
  // a member the Settings card's AWS button is `disabled={!operator}`
  // ("Requires the admin role."), so hiding the strip there would strand
  // exactly the person the refusal sentence sends there.
  const suppressed = under("/setup") || (operator && (under("/settings") || under("/providers") || under("/account")));
  // §4.2: a per-user deployment's states ("your own credentials") are a User-
  // view concern; the Admin view keeps only what a shared-credential
  // deployment would show (door.perUser is the deployment's own shape, not
  // this viewer's — see model-access.ts).
  const adminViewSuppressed = view === "admin" && door.perUser;
  const show = !providerMode && door.needsAttention && !suppressed && !adminViewSuppressed && !(copy.dismissible && dismissed);

  // Said nothing until /me answers, for the legacy strip's reason: the
  // dismissal is keyed on who is looking.
  const lines =
    providerMode && userView && resolved && !under("/setup")
      ? providerAttention(status).flatMap((a) => {
          const line = providerStripLine(a, harnessNames(status, a.defaultFor));
          return line && !(line.dismissible && providerDismissed(a.provider.id)) ? [{ a, line }] : [];
        })
      : [];
  const one = lines.length === 1 ? lines[0] : null;

  return (
    // No live region of its own: app-shell.tsx mounts the `role="status"`
    // wrapper eagerly around this lazy chunk, so the first state to arrive is a
    // text change inside a region that was already there — role="status"
    // announces changes, and mount content is the one thing it does not
    // reliably announce (Codex #15).
    <>
      {/* B9 (#146): a relaunch the server refused after its screen was gone.
          The server's sentence, verbatim, and no heading (packet MP-E Q146-1). */}
      {door.refusal && userView && (
        <div className={STRIP_CLASS + TONE_CLASS.warning}>
          <AlertTriangle className="size-4 shrink-0" />
          <span>{door.refusal}</span>
          <button type="button" onClick={door.dismissRefusal} className={LINK_CLASS}>
            {MODEL_ACCESS_BANNER.REFUSAL_DISMISS}
          </button>
        </div>
      )}
      {lines.length > 1 && (
        // B8: two or more collapse to a count (packet MP-D QD-2).
        <div className={STRIP_CLASS + TONE_CLASS.warning}>
          <AlertTriangle className="size-4 shrink-0" />
          <span>{BANNER.B8(lines.length)}</span>
          <Link to="/account" className={LINK_CLASS}>
            {BANNER.REVIEW}
          </Link>
        </div>
      )}
      {one && (
        <div className={STRIP_CLASS + TONE_CLASS[one.line.tone]}>
          {one.line.tone === "info" ? <Clock className="size-4 shrink-0" /> : <AlertTriangle className="size-4 shrink-0" />}
          <span title={one.line.title || undefined}>{one.line.sentence}</span>
          {!door.claimed && (
            <button
              type="button"
              onClick={() => {
                openedHere.current = true;
                door.openDoor({ for: { provider: one.a.provider.id } });
              }}
              className={LINK_CLASS}
            >
              {one.line.button}
            </button>
          )}
          {one.line.dismissible && (
            <button type="button" onClick={() => dismissProvider(one.a.provider.id)} className={LINK_CLASS + " opacity-75"}>
              {MODEL_ACCESS_BANNER.NOT_NOW}
            </button>
          )}
        </div>
      )}
      {show && (copy.sentence || copy.action) && (
        <div className={STRIP_CLASS + TONE_CLASS[copy.tone]}>
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
              className={LINK_CLASS}
            >
              {AGENTS.SIGN_IN_AWS}
            </button>
          )}
          {copy.dismissible && (
            <button type="button" onClick={dismiss} className={LINK_CLASS + " opacity-75"}>
              {MODEL_ACCESS_BANNER.NOT_NOW}
            </button>
          )}
        </div>
      )}
      <DoorDialog
        target={door.target}
        // The rule the three pane mounts this one replaced followed (#544):
        // Settings and the Agents tab read the settled claude-code row, not
        // the graded door; Getting started, a member's page, never asked —
        // only an admin can sign in under a shared row, the one row with no
        // portal stored.
        perUser={!operator || !!status?.harnesses?.some((h) => h.id === MODEL_ACCESS_AGENT && isPerUserSsoRow(h))}
        focusSeq={door.focusSeq}
        onCancel={door.closeDoor}
        onDone={(message) => {
          completed.current = true;
          // The completion path, not closeDoor(): it also runs what the opener
          // asked for on a completed sign-in (the New Run rail's relaunch).
          door.signedIn();
          void door.refresh();
          // CONSOLE-RULES §9's transient case: the only other evidence is a
          // strip that disappears, and a surface vanishing is not a
          // confirmation.
          toast.success(message);
        }}
        onRemoved={() => {
          door.closeDoor();
          void door.refresh();
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
