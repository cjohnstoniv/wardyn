/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #217 — the console's first navigation guard, and shared rather than
// per-screen: a dirty form (providers-screen.tsx's Git/Storage draft,
// agents-tab.tsx's own draft) registers itself here, and anything that can
// move the console away from it — today, a sidebar link
// (app-shell.tsx#SidebarNav) — asks this context before it navigates. The
// confirm is a BLOCKING dialog, never an inline banner (issue #217's binding
// default): a banner that never interrupts cannot prevent the loss it exists
// to prevent. There is no `beforeunload` or router `useBlocker` anywhere in
// the console before this — the app runs a plain BrowserRouter (main.tsx),
// which has no `useBlocker` at all, so in-app navigation is guarded by
// intercepting the link click itself rather than the router.
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
import { UNSAVED_GUARD } from "../components/wardyn/copy";

interface GuardContextValue {
  register: (id: string, dirty: boolean) => void;
  /** True while ANY registered form is dirty — the one fact a nav link needs
   *  before it decides whether to ask first. */
  isDirty: () => boolean;
  /** Runs `proceed` immediately when nothing is dirty; otherwise opens the
   *  confirm dialog and holds `proceed` until the person answers it. */
  requestLeave: (proceed: () => void) => void;
}

const noopGuard: GuardContextValue = {
  register: () => {},
  isDirty: () => false,
  requestLeave: (proceed) => proceed(),
};

const GuardContext = React.createContext<GuardContextValue>(noopGuard);

/** Wraps the shell (app-shell.tsx#AppShell) — above both the sidebar that can
 *  navigate away and every screen that can register a dirty form below it —
 *  and owns the one blocking dialog every registered form shares. */
export function UnsavedGuardProvider({ children }: { children: React.ReactNode }) {
  // A Set of ids, not one boolean: more than one dirty form can be mounted at
  // once in principle, and a second caller registering "clean" must never
  // clear a FIRST caller's still-dirty flag.
  const dirtyIds = React.useRef(new Set<string>());
  const [pending, setPending] = React.useState<(() => void) | null>(null);

  const isDirty = React.useCallback(() => dirtyIds.current.size > 0, []);
  const register = React.useCallback((id: string, dirty: boolean) => {
    if (dirty) dirtyIds.current.add(id);
    else dirtyIds.current.delete(id);
  }, []);
  const requestLeave = React.useCallback(
    (proceed: () => void) => {
      if (isDirty()) setPending(() => proceed);
      else proceed();
    },
    [isDirty],
  );

  const value = React.useMemo<GuardContextValue>(
    () => ({ register, isDirty, requestLeave }),
    [register, isDirty, requestLeave],
  );

  return (
    <GuardContext.Provider value={value}>
      {children}
      <AlertDialog open={pending !== null} onOpenChange={(open) => !open && setPending(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{UNSAVED_GUARD.TITLE}</AlertDialogTitle>
            <AlertDialogDescription>{UNSAVED_GUARD.BODY}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            {/* Back-out is quiet (CONSOLE-RULES §6): ghost, not the shipped
                outline default. */}
            <AlertDialogCancel className={buttonVariants({ variant: "ghost" })}>
              {UNSAVED_GUARD.STAY}
            </AlertDialogCancel>
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
              {UNSAVED_GUARD.LEAVE}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </GuardContext.Provider>
  );
}

let nextGuardId = 0;

/** Called from a dirty-form screen (providers-screen.tsx, agents-tab.tsx —
 *  the actual OWNERS of a draft and its Save action, so the registration
 *  survives a Git/Storage tab switch inside the same screen). Registers
 *  `dirty` with the shared guard above, and arms `beforeunload` while it is
 *  true — the console's first use of either. */
export function useUnsavedGuard(dirty: boolean): void {
  const { register } = React.useContext(GuardContext);
  const id = React.useRef(`guard-${++nextGuardId}`).current;

  React.useEffect(() => {
    register(id, dirty);
    return () => register(id, false);
  }, [register, id, dirty]);

  React.useEffect(() => {
    if (!dirty) return;
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
 *  guard), an ordinary click on a clean console passes straight through, and
 *  a dirty one is intercepted and re-fired only once "Discard changes" wins. */
export function useGuardedNavClick(
  navigate: (to: string) => void,
): (to: string, after?: () => void) => (e: React.MouseEvent) => void {
  const { isDirty, requestLeave } = React.useContext(GuardContext);
  return React.useCallback(
    (to: string, after?: () => void) => (e: React.MouseEvent) => {
      if (!isDirty() || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
        after?.();
        return;
      }
      e.preventDefault();
      requestLeave(() => {
        navigate(to);
        after?.();
      });
    },
    [isDirty, requestLeave, navigate],
  );
}
