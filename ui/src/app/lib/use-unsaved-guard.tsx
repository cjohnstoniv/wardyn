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
//
// #460 review round 2 — the popstate listener below is installed at MODULE
// scope (a top-level call, evaluated on import), not inside a React effect.
// That ordering is load-bearing, not style: BrowserRouter attaches its OWN
// popstate listener from a layout effect when it mounts, and a plain
// (passive) `useEffect` in this file mounts AFTER that — so react-router's
// listener saw every Back/Forward FIRST, had already scheduled the popped
// route, and the dirty editor unmounted (losing its draft) before this
// file's own effect-based handler ever got a chance to undo it. A module
// evaluates fully before `main.tsx` ever calls `render()`, so a listener
// registered here is GUARANTEED to be attached before <BrowserRouter> ever
// mounts and attaches its own — and on a blocked pop it calls
// `event.stopImmediatePropagation()`, so react-router's LATER-registered
// listener never runs for that event at all. Nothing downstream ever learns
// the pop happened, so nothing unmounts.
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
// writes to the browser's own history.state. null (not 0) when absent: a
// fragment-only jump (a skip link's `href="#main-content"`, or any other
// entry react-router never tagged) carries no idx at all, and treating that
// as "0" invented a fake delta against whatever page happened to be open —
// see installUnsavedGuard's own null check below.
function getHistoryIndex(): number | null {
  const idx = (window.history.state as { idx?: number } | null)?.idx;
  return typeof idx === "number" ? idx : null;
}

// The one fact a MODULE-SCOPE listener (below) can't get from a React
// component's props: which dialog to open. UnsavedGuardProvider sets this on
// mount and clears it on unmount — null (the default) makes the listener a
// no-op, so a popstate anywhere the provider isn't mounted (every screen test
// that doesn't wrap it) is simply ignored.
let activeRequestLeave: ((proceed: () => void) => void) | null = null;

// The index this guard currently considers itself PARKED at — i.e. the
// entry it believes is on screen. Only three things move it: a clean push/
// replace/pop (nothing to guard), an intentional Discard (see below), or the
// very first pop this session sees (nothing to compare it against yet).
let historyIndex: number | null = null;

function trackIndex(): void {
  const idx = getHistoryIndex();
  if (idx !== null) historyIndex = idx;
}

// Installed ONCE, at module evaluation — before `main.tsx` ever calls
// render() and before <BrowserRouter> exists to attach its OWN popstate
// listener (see this file's header note for why that ordering is
// load-bearing). native pushState/replaceState fire no event of their own
// (a known browser API gap — popstate covers Back/Forward only), so a
// forward navigation's own index move is tracked right where it happens,
// inline in the patch, rather than via a second listener elsewhere.
function installUnsavedGuard(): void {
  if (typeof window === "undefined") return;

  const originalPush = window.history.pushState.bind(window.history);
  window.history.pushState = (...args: Parameters<History["pushState"]>) => {
    originalPush(...args);
    trackIndex();
  };
  const originalReplace = window.history.replaceState.bind(window.history);
  window.history.replaceState = (...args: Parameters<History["replaceState"]>) => {
    originalReplace(...args);
    trackIndex();
  };

  window.addEventListener("popstate", (event) => {
    const newIndex = getHistoryIndex();
    if (newIndex === null) {
      // A fragment jump or any other untagged entry — nothing to compute a
      // delta against. Leave it alone (never stop its propagation) and
      // never touch `historyIndex`, so the NEXT real pop still compares
      // against the correct, still-valid baseline instead of a corrupted one.
      return;
    }
    if (historyIndex === null || newIndex === historyIndex || unsavedSnapshot() === null || !activeRequestLeave) {
      // No established baseline yet, no actual move, nothing dirty, or no
      // mounted provider to ask through — accept it as the new baseline and
      // let react-router see it normally.
      historyIndex = newIndex;
      return;
    }
    const delta = newIndex - historyIndex;
    // Block it before react-router (or anything else downstream) ever
    // learns this event happened — `historyIndex` stays at the PARKED
    // value, deliberately not updated here, so the popstate this restore
    // itself fires computes delta 0 below and is absorbed silently, no
    // matter how many more Back/Forward presses arrive while the dialog is
    // still open (each one re-blocked and re-restored the same way).
    event.stopImmediatePropagation();
    window.history.go(-delta);
    activeRequestLeave(() => {
      // Discard: update the parked index FIRST, so when THIS go()'s own
      // popstate arrives it computes delta 0 too and is let through
      // untouched — react-router sees it normally and renders the real
      // destination, exactly as an unguarded pop would have.
      historyIndex = newIndex;
      window.history.go(delta);
    });
  });
}
installUnsavedGuard();

/** Wraps the shell (app-shell.tsx#AppShell) — above both every control that
 *  can navigate away and every screen that can register a dirty form below
 *  it — and owns the one blocking dialog every registered form shares. */
export function UnsavedGuardProvider({ children }: { children: React.ReactNode }) {
  const [pending, setPending] = React.useState<(() => void) | null>(null);

  const requestLeave = React.useCallback((proceed: () => void) => {
    if (unsavedSnapshot() !== null) setPending(() => proceed);
    else proceed();
  }, []);

  React.useEffect(() => {
    activeRequestLeave = requestLeave;
    return () => {
      if (activeRequestLeave === requestLeave) activeRequestLeave = null;
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
