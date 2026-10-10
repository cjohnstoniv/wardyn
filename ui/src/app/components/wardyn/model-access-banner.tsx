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
import { harnessDisplayNames, providerAttention, type ProviderAttention } from "../../lib/model-access";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { MODEL_ACCESS_BANNER } from "./model-access-copy";
import { useModelAccessDoor, useShellSetupStatus } from "./model-access-context";
import { useOperatorResolved, usePrincipal } from "./operator-context";
import { screenPath, viewOfPath } from "./console-view";
import { DoorDialog } from "./door-dialog";
import { BANNER, CONNECTIONS } from "./copy/door";

/** Per-viewer, per-browsing-context. Keyed on the viewer's subject because
 *  sessionStorage survives a sign-out in the same tab: an unkeyed flag would
 *  pre-dismiss the strip for the next person on that tab. Every access is
 *  wrapped — storage throws in a hardened browser profile. "" (/me unresolved
 *  or failed) never reads or writes a flag. */
function dismissKey(principal: string): string {
  return `wardyn.modelAccessDismissed.${principal}`;
}

/** One provider's line on the strip (§5.5, B1–B5), with the button that opens
 *  its door. `harnesses` is the display names of the agents whose default it
 *  is. Pure, and exported for the tests: the kind/state table is the feature.
 *  null for a state packet D draws no line for. `action` is the server's own
 *  line, verbatim, only where it says what the sentence cannot (#993): the
 *  pin-contradicted pair or another access portal, never the button's label. */
export function providerStripLine(
  a: ProviderAttention,
  harnesses: string,
): { sentence: string; action?: string; button: string; title: string; tone: "warning" | "info"; dismissible: boolean } | null {
  const name = a.provider.name || a.provider.id;
  switch (a.provider.kind) {
    case "bedrock_sso":
      if (a.state === "expired_signin")
        return {
          sentence: CONNECTIONS.C6_LINE(name),
          action: a.action && a.action !== AGENTS.SIGN_IN_AWS ? a.action : "",
          button: AGENTS.SIGN_IN_AWS,
          title: "",
          tone: "warning",
          dismissible: false,
        };
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

/** "Not now", per provider (packet MP-D QD-3): dismissing one provider must not
 *  hide another that needs the person later. Keyed as dismissKey above. */
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
 * The strip speaks per model provider (packet MP-D §5.5), in the User view
 * only: every credential is a person's own, which belongs to the User view.
 */
export function ModelAccessBanner() {
  const door = useModelAccessDoor();
  const { status } = useShellSetupStatus();
  const resolved = useOperatorResolved();
  const principal = usePrincipal();
  const { pathname } = useLocation();
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
  // `model_providers` is `omitzero` on the wire, so an admin who set up
  // providers but granted THIS caller none reads `[]` — a real block — never
  // the same wire shape as no block at all (absent). `!= null` (not `!!`) is
  // what tells the two apart.
  const providerMode = status?.model_providers != null;

  // Says nothing until /me answers: the dismissal is keyed on who is looking.
  const lines =
    providerMode && userView && resolved && !under("/setup")
      ? providerAttention(status).flatMap((a) => {
          const line = providerStripLine(a, harnessDisplayNames(status, a.defaultFor));
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
          {/* The server's own words, verbatim — never reworded client-side. */}
          {one.line.action && <span>{one.line.action}</span>}
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
      <DoorDialog
        target={door.target}
        // /setup/status's credential_storage (design F-3) — the key door's
        // store-mode notice line and remove-confirm retention line key off it.
        credentialStorage={status?.credential_storage}
        access={status?.provider_access}
        onRecheck={door.refresh}
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
        onRemoved={(message) => {
          door.closeDoor();
          void door.refresh();
          // Packet F §3: a completed Remove now earns a toast the same way a
          // completed save does.
          toast.success(message);
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
