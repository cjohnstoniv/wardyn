/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { getAuthGeneration, HttpError, isSignedOutHold, onAuthChange } from "../../../lib/api/core";
import { previewRunPolicy } from "../../../lib/api/policy-preview";
import { isCredentialRefusal, runs, type RunWireInput } from "../../../lib/api/runs";
import type { PreflightResult } from "../../../lib/types";
import type { PolicyPreviewResult } from "../../../lib/types/policy-preview";
import { getErrorMessage } from "../../../lib/format";

export const PREFLIGHT_FRESH_MS = 60_000;
export const PREFLIGHT_DEBOUNCE_MS = 800;
const NEVER_BLOCKS = new Set(["model_credential", "preflight_rate_limited"]);
type Endpoint = "preflight" | "preview";
const ENDPOINTS: Endpoint[] = ["preflight", "preview"];

export interface DraftIdentity {
  principal: string;
  resolved: boolean;
  revision: number;
  authGeneration: number;
}

interface Params {
  input: RunWireInput | null;
  body: string | null;
  selectionKey: string;
  identity: DraftIdentity;
  externalRevision: string;
  sourceRefreshPending: boolean;
  autoCheck: { local: boolean; backendArm: boolean; modelArm: boolean };
  doorOpen: boolean;
  adoDoorOpen: boolean;
}

interface Check {
  generation: number;
  result: PreflightResult | PolicyPreviewResult | null;
  error: unknown;
  at: number;
  resultAt: number | null;
  stale: boolean;
  status: number;
  busy: boolean;
  errorSeq: number;
}
interface Flight {
  controller?: AbortController;
  sequence: number;
  pending: boolean;
  due: number;
  retries: number;
  stopped: boolean;
}
const emptyCheck = (): Check => ({ generation: -1, result: null, error: null, at: 0, resultAt: null, stale: true, status: 0, busy: false, errorSeq: 0 });
const emptyFlight = (): Flight => ({ sequence: 0, pending: true, due: 0, retries: 0, stopped: false });

/** Retry-After is either delay seconds or an HTTP date; overflowing timers are manual-only. */
export function retryAfterDelay(value: string | undefined, now: number): number | null {
  if (!value) return null;
  const seconds = /^\d+$/.test(value) ? Number(value) : null;
  const date = /^[A-Za-z]{3}, /.test(value) ? Date.parse(value) : NaN;
  const delay = seconds !== null ? seconds * 1000 : Math.max(0, date - now);
  return Number.isFinite(delay) && delay >= 0 && delay <= 2_147_483_647 ? delay : null;
}

/** One dispatcher owns the two advisory reads; neither is authority to launch. */
export function useRunChecks(params: Params) {
  const [authGeneration, setAuthGeneration] = React.useState(getAuthGeneration);
  const [refresh, setRefresh] = React.useState(0);
  const [checks, setChecks] = React.useState({ preflight: emptyCheck(), preview: emptyCheck() });
  const [ageTick, tick] = React.useState(0);
  const latest = React.useRef(params);
  latest.current = params;
  const reads = React.useRef(checks);
  reads.current = checks;
  const flights = React.useRef({ preflight: emptyFlight(), preview: emptyFlight() });
  const mounted = React.useRef(false);
  const timer = React.useRef<ReturnType<typeof setTimeout>>();
  const timerAt = React.useRef(Infinity);
  const dispatcher = React.useRef<(manual?: boolean) => Promise<void>>(async () => {});
  const { identity, body, selectionKey, externalRevision, sourceRefreshPending, autoCheck, doorOpen, adoDoorOpen } = params;
  const key = JSON.stringify([body, selectionKey, identity, authGeneration, externalRevision, refresh]);
  const scope = React.useRef({ key, generation: 0 });
  // A round trip A→B→A must not revive A's old answer, even before effect cleanup.
  if (scope.current.key !== key) scope.current = { key, generation: scope.current.generation + 1 };
  const generation = scope.current.generation;

  const clearTimer = React.useCallback(() => {
    clearTimeout(timer.current);
    timer.current = undefined;
    timerAt.current = Infinity;
  }, []);
  const schedule = React.useCallback((at = Date.now() + PREFLIGHT_DEBOUNCE_MS) => {
    if (timerAt.current <= at) return;
    clearTimer();
    timerAt.current = at;
    timer.current = setTimeout(() => {
      clearTimer();
      void dispatcher.current();
    }, Math.max(0, at - Date.now()));
  }, [clearTimer]);
  const cancel = React.useCallback((kind: Endpoint) => {
    const flight = flights.current[kind];
    flight.controller?.abort();
    flight.controller = undefined;
    flight.sequence++;
  }, []);
  const canRead = (manual: boolean) => {
    const p = latest.current;
    return mounted.current && p.input !== null && p.body !== null && p.identity.resolved && !!p.identity.principal &&
      p.identity.authGeneration === getAuthGeneration() && !isSignedOutHold() && (manual || !p.sourceRefreshPending);
  };

  const read = async (kind: Endpoint) => {
    const p = latest.current;
    if (!p.input || p.body === null) return;
    const flight = flights.current[kind];
    const controller = new AbortController();
    const sequence = ++flight.sequence;
    const started = scope.current.generation;
    const auth = getAuthGeneration();
    flight.controller = controller;
    flight.pending = false;
    const current = () => mounted.current && !controller.signal.aborted && sequence === flight.sequence &&
      started === scope.current.generation && auth === getAuthGeneration() && !isSignedOutHold();
    setChecks((old) => ({ ...old, [kind]: { ...old[kind], busy: true, stale: kind === "preview" || old[kind].stale } }));
    try {
      const result = kind === "preflight" ? await runs.preflightRun(p.input, controller.signal) : await previewRunPolicy(p.input, controller.signal);
      if (!current()) return;
      setChecks((old) => ({ ...old, [kind]: { generation: started, result, error: null, status: 200, at: Date.now(), resultAt: Date.now(), stale: false, busy: false, errorSeq: old[kind].errorSeq } }));
    } catch (error) {
      if (!current()) return;
      const status = error instanceof HttpError ? error.status : 0;
      setChecks((old) => {
        const previous = old[kind];
        const keep = kind === "preview" && status === 429 && previous.generation === started;
        return { ...old, [kind]: {
          generation: started, result: keep ? previous.result : null, resultAt: keep ? previous.resultAt : null,
          error, status, at: Date.now(), stale: true, busy: false, errorSeq: previous.errorSeq + 1,
        } };
      });
      if (status === 429) {
        const delay = error instanceof HttpError ? retryAfterDelay(error.retryAfter, Date.now()) : null;
        if (kind === "preview" && flight.retries === 0 && delay !== null) {
          flight.retries++;
          flight.pending = true;
          flight.due = Date.now() + delay;
          schedule(flight.due);
        } else flight.stopped = true;
      }
    } finally {
      if (sequence === flight.sequence) {
        flight.controller = undefined;
        if (current()) setChecks((old) => ({ ...old, [kind]: { ...old[kind], busy: false } }));
      }
    }
  };

  dispatcher.current = async (manual = false) => {
    if (!canRead(manual)) return;
    const requests: Promise<void>[] = [];
    for (const kind of ENDPOINTS) {
      const flight = flights.current[kind];
      if (kind === "preflight" && !manual && !latest.current.autoCheck.local) continue;
      if (!flight.pending || flight.controller || flight.stopped) continue;
      if (flight.due > Date.now()) { schedule(flight.due); continue; }
      requests.push(read(kind));
    }
    await Promise.all(requests);
  };

  React.useEffect(() => {
    mounted.current = true;
    const unsubscribe = onAuthChange(() => {
      clearTimer();
      ENDPOINTS.forEach(cancel);
      setAuthGeneration(getAuthGeneration());
    });
    return () => {
      mounted.current = false;
      unsubscribe();
      clearTimer();
      ENDPOINTS.forEach(cancel);
    };
  }, [cancel, clearTimer]);

  React.useEffect(() => {
    clearTimer();
    for (const kind of ENDPOINTS) {
      cancel(kind);
      flights.current[kind] = emptyFlight();
    }
    setChecks({ preflight: emptyCheck(), preview: emptyCheck() });
    if (body !== null) schedule();
  }, [generation, body, cancel, clearTimer, schedule]);

  React.useEffect(() => {
    if (sourceRefreshPending) {
      clearTimer();
      ENDPOINTS.forEach(cancel);
      setChecks((old) => ({ preflight: { ...old.preflight, busy: false }, preview: { ...old.preview, busy: false } }));
    } else schedule();
  }, [sourceRefreshPending, cancel, clearTimer, schedule]);
  React.useEffect(() => {
    if (autoCheck.local) schedule();
    else {
      const flight = flights.current.preflight;
      if (flight.controller) {
        cancel("preflight");
        flight.pending = true;
        setChecks((old) => ({ ...old, preflight: { ...old.preflight, busy: false } }));
      }
    }
  }, [autoCheck.local, cancel, schedule]);

  const invalidate = React.useCallback(() => setRefresh((n) => n + 1), []);
  const previousDoors = React.useRef({ doorOpen, adoDoorOpen });
  React.useEffect(() => {
    if ((previousDoors.current.doorOpen && !doorOpen) || (previousDoors.current.adoDoorOpen && !adoDoorOpen)) invalidate();
    previousDoors.current = { doorOpen, adoDoorOpen };
  }, [doorOpen, adoDoorOpen, invalidate]);
  React.useEffect(() => {
    const focus = () => {
      let pending = false;
      for (const kind of ENDPOINTS) {
        const flight = flights.current[kind];
        const checked = reads.current[kind];
        if (flight.controller || flight.stopped || (checked.generation >= 0 && Date.now() - checked.at < PREFLIGHT_FRESH_MS)) continue;
        flight.pending = true;
        pending = true;
      }
      if (pending) schedule();
    };
    window.addEventListener("focus", focus);
    return () => window.removeEventListener("focus", focus);
  }, [schedule]);
  React.useEffect(() => {
    const times = ENDPOINTS.flatMap((kind) => {
      const checked = checks[kind];
      const at = kind === "preview" ? checked.stale ? null : checked.resultAt : checked.generation >= 0 ? checked.at : null;
      return at === null ? [] : [at + PREFLIGHT_FRESH_MS - Date.now()];
    }).filter((n) => n >= 0);
    if (!times.length) return;
    const timeout = setTimeout(() => tick((n) => n + 1), Math.min(...times) + 1);
    return () => clearTimeout(timeout);
  }, [checks, ageTick]);

  const preflight = React.useCallback(async () => {
    clearTimer();
    cancel("preflight");
    const flight = flights.current.preflight;
    flight.pending = true;
    flight.stopped = false;
    flight.due = 0;
    await dispatcher.current(true);
  }, [cancel, clearTimer]);
  const previewAgain = React.useCallback(async () => {
    setChecks((old) => ({ ...old, preview: { ...old.preview, stale: true } }));
    clearTimer();
    cancel("preview");
    Object.assign(flights.current.preview, { pending: true, stopped: false, due: 0, retries: 0 });
    await dispatcher.current();
  }, [cancel, clearTimer]);
  const currentRead = (kind: Endpoint) => checks[kind].generation === generation && identity.authGeneration === getAuthGeneration() && !isSignedOutHold();
  const preflightIsCurrent = currentRead("preflight");
  const checked = checks.preflight;
  const preflightFresh = preflightIsCurrent && Date.now() - checked.at < PREFLIGHT_FRESH_MS;
  const result = preflightIsCurrent ? checked.result as PreflightResult | null : null;
  const error = checked.error;
  const reason = error instanceof HttpError ? error.reason : "";
  const refused = checked.status >= 400 && checked.status < 500 && checked.status !== 401 && checked.status !== 429 && !NEVER_BLOCKS.has(reason);
  const missing = (kind: string) => !!result?.setup_items?.some((row) => row.kind === kind && row.status === "missing");
  const previewInScope = currentRead("preview");
  const previewIsCurrent = previewInScope && checks.preview.result !== null && !checks.preview.stale;

  return {
    preflight,
    invalidateChecks: invalidate,
    preflighting: checks.preflight.busy,
    preflightResult: result,
    preflightError: preflightIsCurrent && checked.status !== 429 && error ? getErrorMessage(error) || "No reason was given." : null,
    preflightErrorSeq: checked.errorSeq,
    preflightIsCurrent,
    preflightFresh,
    preflightBlock: preflightFresh && (refused || (autoCheck.backendArm && missing("backend")) || (autoCheck.modelArm && missing("llm_access"))),
    preflightNotChecked: preflightIsCurrent && checked.status === 429,
    preflightRefusal: preflightIsCurrent && isCredentialRefusal(error) && body !== null ? { body, provider: error instanceof HttpError ? error.provider : "" } : null,
    preview: {
      result: previewInScope ? checks.preview.result as PolicyPreviewResult | null : null,
      error: previewInScope ? checks.preview.error : null,
      busy: checks.preview.busy,
      current: previewIsCurrent,
      fresh: previewIsCurrent && checks.preview.resultAt !== null && Date.now() - checks.preview.resultAt < PREFLIGHT_FRESH_MS,
      retry: previewAgain,
    },
  };
}
