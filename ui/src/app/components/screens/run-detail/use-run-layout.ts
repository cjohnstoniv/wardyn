/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// useRunLayout — the cockpit canvas's layout state: load the caller's saved
// arrangement for one preset, keep it optimistically local while they drag,
// and write it back on a debounce.
//
// Three rules this hook exists to hold:
//  1. A SAVED layout beats the situational default, always. An empty saved
//     layout is "never saved" (a 200 with layout: []), not a failure, so it
//     falls through to the preset default — and so does any GET error, since a
//     cockpit with no widgets is worse than one arranged by the machine.
//  2. A 501 means this deployment's store cannot persist layouts at all. That
//     is a DEPLOYMENT fact, not a user error: remember it, stop writing, and
//     let the session keep working. No toast, or every drag would fire one.
//  3. Nothing here ever writes on mount. Only an explicit gesture (drag,
//     resize, catalog click, reset, save) schedules a PUT — an auto-save on
//     load would give every human a "saved layout" the instant they opened a
//     run, and they would never see a default improve again.
import * as React from "react";
import { getExpectedPrincipal, HttpError } from "../../../lib/api/core";
import {
  runLayout as runLayoutApi,
  type RunLayoutPreset,
  type RunLayoutWidget,
} from "../../../lib/api/run-layout";
import { DESKTOP_REMOVED, forMachineRun, forRun, forVMRun, isDesktopRemoved, isMachineRemoved, isServiceRemoved, MACHINE_REMOVED, normalizeLayout, presetLayout, SERVICE_REMOVED } from "./widget-registry";

// Long enough that a drag across the canvas is one PUT, short enough that a
// human who drags and immediately closes the tab keeps the arrangement (and
// the unmount flush below covers the rest).
const SAVE_DEBOUNCE_MS = 800;

// The three genuinely different outcomes of a layout write. "unsupported" is a
// permanent property of the DEPLOYMENT (the endpoint 501s and never will not);
// "failed" is this ATTEMPT, and the next gesture may well succeed. They need
// opposite sentences, so they cannot share a boolean.
export type SaveResult = "ok" | "unsupported" | "failed";

// What the person can be told about the last write: "saving" while it is pending or in flight, "saved" once the server
// stored it, "failed" when it did not (retryable). Nothing is said before the first gesture, or when the deployment
// cannot store layouts at all (that has its own standing note).
export type SaveStatus = "idle" | "saving" | "saved" | "failed";

// One preset's write queue: the request in flight, the newest body waiting behind it, the body of a write that failed
// (what Retry repeats), and who is awaiting the outcome. `person` arranged the waiting body, so it is stored as theirs.
type WriteSlot = { inflight: boolean; pending: RunLayoutWidget[] | null; person: string; failed: RunLayoutWidget[] | null; waiters: Array<(r: SaveResult) => void> };

function slotFor(slots: Partial<Record<RunLayoutPreset, WriteSlot>>, p: RunLayoutPreset): WriteSlot {
  return (slots[p] ??= { inflight: false, pending: null, person: "", failed: null, waiters: [] });
}

export type RunLayoutState = {
  layout: RunLayoutWidget[];
  /** The GET has settled. The layout is usable before this (the preset
   *  default), so this only reports whether a saved one is still in flight. */
  loaded: boolean;
  /** False once the server answered 501: in-session only from here on. */
  persistable: boolean;
  /** The state of the latest write, so a failed one is never invisible. "failed" if either preset's last write failed
   *  (see `failedPreset`); otherwise the current preset's own. */
  status: SaveStatus;
  /** Which preset's write failed, so the sentence can name it when a preset switch happened since. */
  failedPreset: RunLayoutPreset | null;
  /** Optimistic local update + a debounced PUT. */
  apply: (next: RunLayoutWidget[]) => void;
  /** Write the last arrangement now. */
  save: () => Promise<SaveResult>;
  /** Repeat the write that failed: that preset, that exact body — whatever preset is showing now. */
  retry: () => void;
  /** Back to the preset default, and clear the saved row (PUT []). */
  reset: () => void;
};

/** `service`: the run is a service, so its live layout carries the Service panel. `vm`: the run is a VM run, so a saved layout carries the Desktop tile. `machine`: the run keeps or resumed a kept machine, so a layout carries the Machine tile. */
export function useRunLayout(preset: RunLayoutPreset, service = false, vm = false, machine = false): RunLayoutState {
  // Seeded with the preset default rather than empty: the canvas can paint the
  // machine's arrangement immediately and swap once a saved one arrives, which
  // beats one round-trip of empty canvas for the majority who never saved one.
  const [layout, setLayout] = React.useState<RunLayoutWidget[]>(() => presetLayout(preset, service, vm, machine));
  const [loaded, setLoaded] = React.useState(false);
  const [persistable, setPersistable] = React.useState(true);
  const [statuses, setStatuses] = React.useState<Partial<Record<RunLayoutPreset, SaveStatus>>>({});

  const layoutRef = React.useRef(layout);
  const persistRef = React.useRef(true);
  // The body the last gesture meant to store (a reset is [], not the layout on screen), so a retry repeats THAT.
  const lastBody = React.useRef<RunLayoutWidget[] | null>(null);
  // Each preset's write queue (see enqueue).
  const slots = React.useRef<Partial<Record<RunLayoutPreset, WriteSlot>>>({});
  // The debounced write not yet sent, bound to the preset and arrangement it was made under.
  const timer = React.useRef<{ id: ReturnType<typeof setTimeout>; preset: RunLayoutPreset; body: RunLayoutWidget[]; person: string } | null>(null);
  // Has the human arranged anything since this preset's GET was issued? A slow
  // control plane made the load's answer land AFTER a drag/remove/add, and the
  // load committed it unconditionally — silently undoing the gesture on screen
  // while the debounced PUT (which closes over the human's layout, not
  // layoutRef) still wrote the edit to the server. Screen and server then
  // disagreed until the next page load. The load now drops its own answer the
  // same way it already drops it after unmount.
  const dirty = React.useRef(false);
  // The person took the Service panel out of this preset's saved layout. A saved layout with no Service entry reads
  // as one that predates the panel, which gets it, so the removal is stored as SERVICE_REMOVED and kept out of `layout`.
  const serviceRemoved = React.useRef(false);
  // The same for the Desktop tile, which a saved layout from before the tile has no entry for: stored as DESKTOP_REMOVED,
  // kept out of `layout`, and kept in the saved row by every write (also from a run with no desktop to show).
  const desktopRemoved = React.useRef(false);
  // The same for the Machine tile, which a saved layout from before the tile has no entry for.
  const machineRemoved = React.useRef(false);

  const commit = (next: RunLayoutWidget[]) => {
    layoutRef.current = next;
    setLayout(next);
  };

  const cancelPending = () => {
    if (timer.current) {
      clearTimeout(timer.current.id);
      timer.current = null;
    }
  };

  const setStatusFor = React.useCallback((p: RunLayoutPreset, next: SaveStatus) => {
    setStatuses((prev) => (prev[p] === next ? prev : { ...prev, [p]: next }));
  }, []);

  /** The wire body: the arrangement plus the markers for what the person took out. */
  const wire = React.useCallback(
    (body: RunLayoutWidget[]) => [
      ...body,
      ...(serviceRemoved.current ? [SERVICE_REMOVED] : []),
      ...(desktopRemoved.current ? [DESKTOP_REMOVED] : []),
      ...(machineRemoved.current ? [MACHINE_REMOVED] : []),
    ],
    [],
  );

  // The server's upsert is unconditional, so two PUTs for one preset in flight at once can commit in either order and
  // leave the OLDER layout stored. So: at most one request in flight per preset, and one slot for the newest body
  // wanted. When the request settles the newest body goes out; a newer gesture supersedes an older one, it never races it.
  const pump = React.useCallback(
    async (p: RunLayoutPreset): Promise<void> => {
      const slot = slotFor(slots.current, p);
      if (slot.inflight || !slot.pending) return;
      const body = slot.pending;
      const person = slot.person;
      const waiters = slot.waiters;
      slot.pending = null;
      slot.waiters = [];
      slot.inflight = true;
      let result: SaveResult = "ok";
      try {
        await runLayoutApi.putLayout(p, body, person);
      } catch (err) {
        // Any non-501 failure stays retryable. Reported DISTINCTLY from "unsupported": the two need opposite
        // sentences ("this deployment can't store layouts" is a permanent fact; a 400 or a blip is not).
        result = err instanceof HttpError && err.status === 501 ? "unsupported" : "failed";
      }
      slot.inflight = false;
      if (result === "unsupported") {
        persistRef.current = false;
        setPersistable(false);
        slot.pending = null;
        slot.failed = null;
        setStatusFor(p, "idle");
        for (const w of [...waiters, ...slot.waiters.splice(0)]) w("unsupported");
        return;
      }
      if (slot.pending) {
        // A newer arrangement is waiting and supersedes this one, failed or not.
        slot.failed = null;
        void pump(p);
      } else {
        slot.failed = result === "failed" ? body : null;
        // A debounced write for this preset is still to come: the status stays "saving" until it lands.
        if (timer.current?.preset !== p) setStatusFor(p, result === "ok" ? "saved" : "failed");
      }
      for (const w of waiters) w(result);
    },
    [setStatusFor],
  );

  /** Queue the newest wire body for a preset. Resolves with the outcome of the request that carried it (or its successor). */
  const enqueue = React.useCallback(
    (p: RunLayoutPreset, body: RunLayoutWidget[], person = getExpectedPrincipal()): Promise<SaveResult> => {
      if (!persistRef.current) return Promise.resolve("unsupported");
      const slot = slotFor(slots.current, p);
      slot.pending = body;
      slot.person = person;
      slot.failed = null;
      setStatusFor(p, "saving");
      return new Promise<SaveResult>((resolve) => {
        slot.waiters.push(resolve);
        void pump(p);
      });
    },
    [pump, setStatusFor],
  );

  const apply = React.useCallback(
    (next: RunLayoutWidget[]) => {
      const norm = normalizeLayout(next, preset);
      dirty.current = true;
      // A service run's live layout always shows the panel until it is taken out; a run with no panel says nothing.
      if (service && preset === "live") serviceRemoved.current = !norm.some((w) => w.widget === "service");
      // A VM run's saved layout shows the tile until it is taken out; a run with no desktop says nothing.
      if (vm) desktopRemoved.current = !norm.some((w) => w.widget === "desktop");
      // A run with a kept machine shows the tile until it is taken out; a run with none says nothing.
      if (machine) machineRemoved.current = !norm.some((w) => w.widget === "machine");
      commit(norm);
      cancelPending();
      lastBody.current = norm;
      if (persistRef.current) setStatusFor(preset, "saving");
      const person = getExpectedPrincipal();
      timer.current = {
        id: setTimeout(() => {
          timer.current = null;
          void enqueue(preset, wire(norm), person);
        }, SAVE_DEBOUNCE_MS),
        preset,
        body: norm,
        person,
      };
    },
    [preset, service, vm, machine, enqueue, setStatusFor, wire],
  );

  const save = React.useCallback(async (): Promise<SaveResult> => {
    cancelPending();
    return enqueue(preset, wire(lastBody.current ?? layoutRef.current));
  }, [preset, enqueue, wire]);

  const retry = React.useCallback(() => {
    const other: RunLayoutPreset = preset === "live" ? "finished" : "live";
    const p = slotFor(slots.current, preset).failed ? preset : slotFor(slots.current, other).failed ? other : null;
    const body = p && slotFor(slots.current, p).failed;
    if (p && body) void enqueue(p, body);
  }, [preset, enqueue]);

  const reset = React.useCallback(() => {
    cancelPending();
    dirty.current = true;
    serviceRemoved.current = false;
    desktopRemoved.current = false;
    machineRemoved.current = false;
    commit(presetLayout(preset, service, vm, machine));
    lastBody.current = [];
    // PUT [] is the reset: there is no DELETE route, and storing today's
    // default as a personal choice would freeze this human out of every future
    // improvement to it. An empty saved layout reads as "never saved" on load.
    void enqueue(preset, wire([]));
  }, [preset, service, vm, machine, enqueue, wire]);

  React.useEffect(() => {
    let alive = true;
    // A fresh request for a (possibly new) preset: nothing has been arranged
    // against THIS load yet, so a gesture made under the previous preset must
    // not suppress this one's answer.
    dirty.current = false;
    lastBody.current = null;
    serviceRemoved.current = false;
    desktopRemoved.current = false;
    machineRemoved.current = false;
    setLoaded(false);
    runLayoutApi
      .getLayout(preset)
      .then((res) => {
        if (!alive || dirty.current) {
          if (alive) setLoaded(true);
          return;
        }
        serviceRemoved.current = res.layout.some(isServiceRemoved);
        desktopRemoved.current = res.layout.some(isDesktopRemoved);
        machineRemoved.current = res.layout.some(isMachineRemoved);
        const saved = res.layout.filter((w) => !isServiceRemoved(w) && !isDesktopRemoved(w) && !isMachineRemoved(w));
        const shown = normalizeLayout(saved, preset);
        const base = saved.length === 0 ? presetLayout(preset, service && !serviceRemoved.current, vm, machine) : serviceRemoved.current ? shown : forRun(shown, preset, service);
        // Saved with the tile taken out: the preset's own tile (nothing saved but the marker) goes too.
        const withDesk = desktopRemoved.current ? base.filter((w) => w.widget !== "desktop") : saved.length === 0 ? base : forVMRun(base, vm);
        // Saved with the Machine tile taken out: the preset's own goes too. A saved layout with no entry gets it on a run that has a machine.
        commit(machineRemoved.current ? withDesk.filter((w) => w.widget !== "machine") : saved.length === 0 ? withDesk : forMachineRun(withDesk, machine));
        setLoaded(true);
      })
      .catch(() => {
        if (!alive || dirty.current) {
          if (alive) setLoaded(true);
          return;
        }
        commit(presetLayout(preset, service, vm, machine));
        setLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, [preset, service, vm, machine]);

  // Finish a debounced write the human never saw land — on unmount (they
  // navigated away mid-debounce) and on a preset switch, where the cleanup
  // sends the arrangement to the preset it was made under.
  React.useEffect(
    () => () => {
      const t = timer.current;
      if (t) {
        clearTimeout(t.id);
        timer.current = null;
        void enqueue(t.preset, wire(t.body), t.person);
      }
    },
    [preset, enqueue, wire],
  );

  const failedPreset = (["live", "finished"] as const).find((p) => p === preset && statuses[p] === "failed") ?? (["live", "finished"] as const).find((p) => statuses[p] === "failed") ?? null;
  const status: SaveStatus = failedPreset ? "failed" : (statuses[preset] ?? "idle");

  return { layout, loaded, persistable, status, failedPreset, apply, save, retry, reset };
}
