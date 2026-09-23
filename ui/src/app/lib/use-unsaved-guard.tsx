/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #217/#460 — the console's one navigation guard, and shared rather than
// per-screen: a dirty form (providers-screen.tsx's Git/Storage draft,
// agents-tab.tsx's own draft) registers itself in unsaved-registry.ts, and
// anything that can move the console away from it — today, a sidebar link
// (app-shell.tsx#SidebarNav) — asks this context before it navigates. The
// confirm is a BLOCKING dialog, never an inline banner (issue #217's binding
// default): a banner that never interrupts cannot prevent the loss it exists
// to prevent. There is no `beforeunload` or router `useBlocker` anywhere in
// the console before this — the app runs a plain BrowserRouter (main.tsx),
// which has no `useBlocker` at all, so in-app navigation is guarded by
// intercepting the link click itself rather than the router.
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

/** Wraps the shell (app-shell.tsx#AppShell) — above both the sidebar that can
 *  navigate away and every screen that can register a dirty form below it —
 *  and owns the one blocking dialog every registered form shares. */
export function UnsavedGuardProvider({ children }: { children: React.ReactNode }) {
  const [pending, setPending] = React.useState<(() => void) | null>(null);

  const requestLeave = React.useCallback((proceed: () => void) => {
    if (unsavedSnapshot() !== null) setPending(() => proceed);
    else proceed();
  }, []);

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
