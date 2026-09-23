/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #217/#460 — the console's one navigation guard, and shared rather than
// per-screen: a dirty form (providers-screen.tsx's Git/Storage draft,
// agents-tab.tsx's own draft) registers itself in unsaved-registry.ts, and
// anything that can move the console away from it — EVERY sidebar link
// (app-shell.tsx#SidebarNav), every header link (top-bar.tsx), a Providers
// tab switch that would unmount a dirty tab (providers-screen.tsx), and the
// browser's own Back/Forward — asks this context before it navigates. The
// confirm is a BLOCKING dialog, never an inline banner (issue #217's binding
// default): a banner that never interrupts cannot prevent the loss it exists
// to prevent. There is no router `useBlocker` anywhere in the console — the
// app runs a plain BrowserRouter (main.tsx), which has no `useBlocker` at
// all, so in-app navigation is guarded by intercepting the link click itself
// (useGuardedNavClick) or a raw state change (useRequestLeave) rather than
// the router, and Back/Forward is guarded by listening to the native
// `popstate` event directly (below) — BrowserRouter offers no hook into it.
//
// #460: dirtiness is no longer tracked locally here — it reads unsaved-
// registry.ts's unsavedSnapshot() (a non-empty snapshot IS dirty), the same
// registry a sibling branch (#483, forced reauth) also reads, so that flow
// can see what's unsaved without depending on this file at all.
import * as React from "react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../components/ui/alert-dialog";
import { buttonVariants } from "../components/ui/button";
import { UNSAVED } from "./unsaved-copy";
import { unsavedSnapshot, useRegisterUnsaved } from "./unsaved-registry";

interface GuardContextValue {
  /** Runs `proceed` immediately when nothing is dirty; otherwise opens the
   *  confirm dialog and holds `proceed` until the person answers it. */
  requestLeave: (proceed: () => void) => void;
}

const noopGuard: GuardContextValue = {
  requestLeave: (proceed) => proceed(),
};

const GuardContext = React.createContext<GuardContextValue>(noopGuard);

// react-router (getUrlBasedHistory, the underlying implementation behind
// BrowserRouter) tags every entry's `window.history.state` with a monotonic
// `idx`, incrementing it on every push and reading it back on every pop — the
// SAME public, DOM-level fact this reads to compute a POP's own delta, with
// no dependency on react-router's internals beyond that one field it already
// writes to the browser's own history.state.
function getHistoryIndex(): number {
  const idx = (window.history.state as { idx?: number } | null)?.idx;
  return typeof idx === "number" ? idx : 0;
}

// A FORWARD navigation (react-router's own push, on every `navigate()` or
// <Link> click) also moves `history.state.idx` — and this file must track
// that too, to compute a later POP's delta correctly. But it can't lean on
// react-router's own location context to notice: UnsavedGuardProvider wraps
// screens that render with no Router in their tests (providers-screen.test.tsx
// among them), so a hard `useLocation()` dependency would crash there, and
// native `pushState`/`replaceState` fire no event of their own to listen for
// (a known browser API gap — `popstate` covers Back/Forward only). So this
// patches both, ONCE, to broadcast one: still router-agnostic (works with or
// without a Router in the tree), and only the listener below in
// UnsavedGuardProvider ever reacts to it.
const HISTORY_CHANGE_EVENT = "wardyn:historychange";
let historyPatched = false;
function ensureHistoryPatched(): void {
  if (historyPatched || typeof window === "undefined") return;
  historyPatched = true;
  const notify = () => window.dispatchEvent(new Event(HISTORY_CHANGE_EVENT));
  const originalPush = window.history.pushState.bind(window.history);
  window.history.pushState = (...args: Parameters<History["pushState"]>) => {
    originalPush(...args);
    notify();
  };
  const originalReplace = window.history.replaceState.bind(window.history);
  window.history.replaceState = (...args: Parameters<History["replaceState"]>) => {
    originalReplace(...args);
    notify();
  };
}

/** Wraps the shell (app-shell.tsx#AppShell) — above both every control that
 *  can navigate away and every screen that can register a dirty form below
 *  it — and owns the one blocking dialog every registered form shares. */
export function UnsavedGuardProvider({ children }: { children: React.ReactNode }) {
  const [pending, setPending] = React.useState<(() => void) | null>(null);

  const requestLeave = React.useCallback((proceed: () => void) => {
    if (unsavedSnapshot() !== null) setPending(() => proceed);
    else proceed();
  }, []);

  // Browser Back/Forward: BrowserRouter has no blocker for this, and by the
  // time `popstate` fires the browser has ALREADY moved — window.location and
  // history.state already read as the NEW entry. So a dirty pop is undone
  // immediately (`history.go(-delta)`, which fires its own native popstate
  // that react-router's own listener resyncs from, exactly like a real
  // back/forward — never a raw pushState, which it would NOT pick up) and
  // only THEN asked about; "Discard changes" replays the ORIGINAL delta.
  const historyIndexRef = React.useRef(getHistoryIndex());
  // Set once by OUR OWN restore/replay go() call, so the popstate IT fires
  // is applied silently instead of being treated as a second real pop.
  const suppressPopRef = React.useRef(false);
  React.useEffect(() => {
    ensureHistoryPatched();
    // A push/replace moved the index — stay current so the NEXT pop's delta
    // is computed against where we actually are, not a stale mount-time read.
    const onHistoryChange = () => {
      historyIndexRef.current = getHistoryIndex();
    };
    const onPopState = () => {
      if (suppressPopRef.current) {
        suppressPopRef.current = false;
        historyIndexRef.current = getHistoryIndex();
        return;
      }
      const newIndex = getHistoryIndex();
      const delta = newIndex - historyIndexRef.current;
      historyIndexRef.current = newIndex;
      if (delta === 0 || unsavedSnapshot() === null) return;
      suppressPopRef.current = true;
      window.history.go(-delta);
      requestLeave(() => {
        suppressPopRef.current = true;
        window.history.go(delta);
      });
    };
    window.addEventListener(HISTORY_CHANGE_EVENT, onHistoryChange);
    window.addEventListener("popstate", onPopState);
    return () => {
      window.removeEventListener(HISTORY_CHANGE_EVENT, onHistoryChange);
      window.removeEventListener("popstate", onPopState);
    };
  }, [requestLeave]);

  const value = React.useMemo<GuardContextValue>(() => ({ requestLeave }), [requestLeave]);

  return (
    <GuardContext.Provider value={value}>
      {children}
      <AlertDialog open={pending !== null} onOpenChange={(open) => !open && setPending(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{UNSAVED.TITLE}</AlertDialogTitle>
            <AlertDialogDescription>{UNSAVED.BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            {/* Back-out is quiet (CONSOLE-RULES §6): ghost, not the shipped
                outline default. */}
            <AlertDialogCancel className={buttonVariants({ variant: "ghost" })}>{UNSAVED.STAY}</AlertDialogCancel>
            {/* The irreversible arm wears the weight (§6): discarding typed
                edits is destructive, never the teal default. */}
            <AlertDialogAction
              className={buttonVariants({ variant: "destructive" })}
              onClick={() => {
                const proceed = pending;
                setPending(null);
                proceed?.();
              }}
            >
              {UNSAVED.DISCARD}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </GuardContext.Provider>
  );
}

/** Called from a dirty-form screen (providers-screen.tsx, agents-tab.tsx —
 *  the actual OWNERS of a draft and its Save action, so the registration
 *  survives a Git/Storage tab switch inside the same screen). Registers
 *  `getText` in unsaved-registry.ts while `dirty`, and arms `beforeunload`
 *  over that same window — the console's first use of either. `id` must be
 *  stable and unique per mounted editor. */
export function useUnsavedGuard(id: string, dirty: boolean, getText: () => string): void {
  useRegisterUnsaved(id, dirty, getText);

  React.useEffect(() => {
    if (!dirty) return undefined;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);
}

/** Called from a control that can navigate the console away from a dirty form
 *  (app-shell.tsx#SidebarNav's nav links). Returns a click handler for a
 *  given destination: a modifier/middle click is left alone (it opens a new
 *  tab — the dirty form in THIS one is untouched, so there is nothing to
 *  guard), an ordinary click with nothing dirty passes straight through, and
 *  a dirty one is intercepted and re-fired only once "Discard changes" wins. */
export function useGuardedNavClick(
  navigate: (to: string) => void,
): (to: string, after?: () => void) => (e: React.MouseEvent) => void {
  const { requestLeave } = React.useContext(GuardContext);
  return React.useCallback(
    (to: string, after?: () => void) => (e: React.MouseEvent) => {
      if (
        unsavedSnapshot() === null ||
        e.defaultPrevented ||
        e.button !== 0 ||
        e.metaKey ||
        e.ctrlKey ||
        e.shiftKey ||
        e.altKey
      ) {
        after?.();
        return;
      }
      e.preventDefault();
      requestLeave(() => {
        navigate(to);
        after?.();
      });
    },
    [requestLeave, navigate],
  );
}

/** The same confirm, for a state change that isn't a click on a link — today
 *  providers-screen.tsx's Segmented tab switch, which would otherwise unmount
 *  the Agents tab (and its dirty draft with it) with no warning. Runs
 *  `proceed` immediately when nothing is dirty. */
export function useRequestLeave(): (proceed: () => void) => void {
  const { requestLeave } = React.useContext(GuardContext);
  return requestLeave;
}
