/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run route element: the one draft, the URL grammar, the step guard,
// seeding, identity and leaving. Each screen is a lazy child; a screen whose
// lane has not landed renders its empty seam.

import * as React from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { modalLayerOpen } from "../../../lib/modal-layer-open";
import { useRequestLeave, useUnsavedGuard } from "../../../lib/use-unsaved-guard";
import { useRequestIdentity } from "../../wardyn/operator-context";
import type { CreateRunResult } from "../../../lib/types/run-create";
import { draftReducer, isDirty, type DraftAction } from "./draft/draft-reducer";
import { prefillSeed } from "./draft/prefill-seed";
import { NEW_RUN_PATH, newRunHref, parseNewRunSearch, type NewRunFrom, type NewRunLocation } from "./draft/new-run-url";
import { initialDraft, type NewRunDraft, type NewRunOrigin, type RunIntent } from "./draft/run-intent";
import { firstInvalidStep, localIssues } from "./lenses/local-issues";
import { NEW_RUN_STEPS, type NewRunStep } from "./lenses/step-issues";
import { seedLoaders } from "./steps/shape/seed-loaders";
import { AnnounceContext } from "./steps/step-shell";

const DraftContext = React.createContext<NewRunDraft | null>(null);
const DispatchContext = React.createContext<React.Dispatch<DraftAction>>(() => {});

export function useNewRunDraft(): NewRunDraft {
  const draft = React.useContext(DraftContext);
  if (!draft) throw new Error("useNewRunDraft outside NewRunDraftProvider");
  return draft;
}
export const useNewRunDispatch = () => React.useContext(DispatchContext);

export function NewRunDraftProvider({ owner, children }: { owner: string; children: React.ReactNode }) {
  const [draft, dispatch] = React.useReducer(draftReducer, owner, initialDraft);
  return (
    <DispatchContext.Provider value={dispatch}>
      <DraftContext.Provider value={draft}>{children}</DraftContext.Provider>
    </DispatchContext.Provider>
  );
}

/** What a seeding read answers: the server's document intent and where it came from. */
export interface Seed {
  origin: NewRunOrigin;
  intent: RunIntent;
}
export interface SeedLoaders {
  template: (ref: { id: string; revision?: number }, signal: AbortSignal) => Promise<Seed>;
  again: (runId: string, signal: AbortSignal) => Promise<Seed>;
}
export type NewRunScreenName = "start" | NewRunStep | "document" | "review" | "launched" | "seedFailed";
export interface NewRunScreenProps {
  location: NewRunLocation;
  /** Moves to another screen, keeping the template or run the draft was seeded from in the URL. */
  go: (loc: NewRunLocation, opts?: { replace?: boolean }) => void;
  seedError?: unknown;
  startFromScratch: () => void;
}
type Screen = React.ComponentType<NewRunScreenProps>;
export const NEW_RUN_SCREENS: Record<NewRunScreenName, Screen> = {
  start: React.lazy(() => import("./start/start-screen").then((m) => ({ default: m.StartScreen }))),
  shape: React.lazy(() => import("./steps/shape/shape-step").then((m) => ({ default: m.ShapeStep }))),
  run: React.lazy(() => import("./steps/run/run-step").then((m) => ({ default: m.RunStep }))),
  harnesses: React.lazy(() => import("./steps/harnesses/harnesses-step").then((m) => ({ default: m.HarnessesStep }))),
  files: React.lazy(() => import("./steps/files/files-step").then((m) => ({ default: m.FilesStep }))),
  tools: React.lazy(() => import("./steps/tools/tools-step").then((m) => ({ default: m.ToolsStep }))),
  document: React.lazy(() => import("./document/document-screen").then((m) => ({ default: m.DocumentScreen }))),
  review: React.lazy(() => import("./launch/review-with-launch").then((m) => ({ default: m.ReviewWithLaunch }))),
  launched: React.lazy(() => import("./launch/launched-screen").then((m) => ({ default: m.LaunchedScreen }))),
  seedFailed: React.lazy(() => import("./start/seed-failed").then((m) => ({ default: m.SeedFailed }))),
};

const screenOf = (loc: NewRunLocation): NewRunScreenName => (loc.screen === "step" ? loc.step : loc.screen);

// Make's centred page column (snapshot CSS .center-stage, .flow-shell/.editor-shell, .review-shell):
// Start 1040 px, the Guided steps and the document editor 900 px, Review 1120 px, Launched 580 px, a 20 px gutter each
// side, and Make's top and bottom padding; a phone narrows the gutter and the top padding as Make does.
const COLUMN = "mx-auto pb-[110px] max-[520px]:w-[calc(100%-24px)]";
const STEP_COLUMN = `${COLUMN} w-[min(900px,calc(100%-40px))] pt-11 max-[520px]:pt-7`;
const FRAME: Partial<Record<NewRunScreenName, string>> = {
  start: `${COLUMN} w-[min(1040px,calc(100%-40px))] pt-[clamp(65px,9vh,110px)] max-[520px]:pt-11`,
  shape: STEP_COLUMN,
  run: STEP_COLUMN,
  harnesses: STEP_COLUMN,
  files: STEP_COLUMN,
  tools: STEP_COLUMN,
  document: STEP_COLUMN,
  review: `${COLUMN} w-[min(1120px,calc(100%-40px))] pt-11 max-[520px]:pt-7`,
  // Make's .success-shell (index.css:489): 580 px, the top padding scales with the viewport.
  launched: "mx-auto w-[min(580px,calc(100%-40px))] pt-[clamp(80px,13vh,140px)] pb-[110px]",
};

/** Where the guard sends a scratch or composer draft, or undefined when the screen may open. */
function guardTarget(draft: NewRunDraft, loc: NewRunLocation): NewRunLocation | undefined {
  if (draft.origin.kind !== "scratch" && draft.origin.kind !== "composer") return undefined;
  const first = firstInvalidStep(draft.intent);
  if (!first) return undefined;
  const beyond = loc.screen === "review" || (loc.screen === "step" && NEW_RUN_STEPS.indexOf(loc.step) > NEW_RUN_STEPS.indexOf(first));
  return beyond ? { screen: "step", step: first } : undefined;
}

/** The router state a create hands the Launched screen. It names whose run it is. */
export interface LaunchedState {
  created: CreateRunResult;
  owner: string;
}
/** Launched state from another person (Back into their entry) is never shown. */
const foreignLaunched = (state: unknown, principal: string): boolean => {
  const s = state as Partial<LaunchedState> | null;
  return s?.created !== undefined && s.owner !== principal;
};

const seedKey = (from: NewRunFrom) =>
  from.template ? `template:${from.template.id}:${from.template.revision ?? ""}` : from.again ? `again:${from.again}` : "";

function NewRunFlow({ loaders, screens }: { loaders: SeedLoaders; screens: Record<NewRunScreenName, Screen> }) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const navigate = useNavigate();
  const location = useLocation();
  const [params] = useSearchParams();
  const { principal, operatorResolved } = useRequestIdentity();
  const parsed = React.useMemo(() => parseNewRunSearch(params), [params]);
  const from = React.useMemo<NewRunFrom>(() => ({ template: parsed.template, again: parsed.again }), [parsed]);
  const key = seedKey(from);
  const owned = operatorResolved && !!principal && draft.owner === principal;
  const [seeded, setSeeded] = React.useState<{ key: string; error?: unknown }>({ key: "" });

  // A different person: everything of the previous one goes, the URL included.
  // The link they had open is never read as the new person, even for the one
  // render before the replace lands.
  const previousLink = React.useRef("");
  if (!key) previousLink.current = "";
  React.useEffect(() => {
    if (!operatorResolved || !principal || draft.owner === principal) return;
    const previous = draft.owner;
    dispatch({ type: "reset", owner: principal });
    setSeeded({ key: "" });
    if (!previous) return;
    previousLink.current = key;
    void navigate(NEW_RUN_PATH, { replace: true });
  }, [operatorResolved, principal, draft.owner, dispatch, navigate, key]);
  const stale = key !== "" && previousLink.current === key;

  React.useEffect(() => {
    if (parsed.normalised) void navigate(NEW_RUN_PATH, { replace: true });
  }, [parsed.normalised, navigate]);

  // Router-state prefill is read once, then dropped so a reload cannot re-apply it.
  const prefillRead = React.useRef(false);
  React.useEffect(() => {
    if (!owned || prefillRead.current) return;
    prefillRead.current = true;
    const seed = prefillSeed(location.state);
    if (!seed) return;
    dispatch({ type: "seed", ...seed });
    const next: NewRunLocation = seed.origin.kind === "composer" ? { screen: "step", step: "run" } : { screen: "review", view: "pretty" };
    void navigate(newRunHref(next), { replace: true, state: null });
  }, [owned, location.state, dispatch, navigate]);

  const foreign = owned && foreignLaunched(location.state, principal);
  React.useEffect(() => {
    if (foreign) void navigate(`${location.pathname}${location.search}`, { replace: true, state: null });
  }, [foreign, location.pathname, location.search, navigate]);

  React.useEffect(() => {
    if (!owned || !key || stale || seeded.key === key) return;
    // A change of person or link aborts the read, so a late answer is never applied.
    const controller = new AbortController();
    const read = from.template ? loaders.template(from.template, controller.signal) : loaders.again(from.again ?? "", controller.signal);
    read.then(
      (seed) => {
        if (controller.signal.aborted) return;
        dispatch({ type: "seed", origin: seed.origin, intent: seed.intent });
        setSeeded({ key });
      },
      (error: unknown) => {
        if (!controller.signal.aborted) setSeeded({ key, error });
      },
    );
    return () => controller.abort();
  }, [owned, key, stale, from, seeded.key, loaders, dispatch, draft.owner]);

  const target = owned && !key ? guardTarget(draft, parsed.location) : undefined;
  const targetHref = target ? newRunHref(target) : "";
  React.useEffect(() => {
    if (targetHref) void navigate(targetHref, { replace: true });
  }, [targetHref, navigate]);

  const requestLeave = useRequestLeave();
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented || modalLayerOpen()) return;
      requestLeave(() => void navigate("/runs"));
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [requestLeave, navigate]);
  // The draft lives as long as New Run is open on the same seed, so moving between its own screens (Back,
  // Forward) loses nothing; a move to another template or run would seed over it.
  const staysWithDraft = React.useCallback(
    (href: string) => {
      const url = new URL(href, window.location.origin);
      const to = parseNewRunSearch(url.searchParams);
      return url.pathname.endsWith(NEW_RUN_PATH) && seedKey({ template: to.template, again: to.again }) === key;
    },
    [key],
  );
  useUnsavedGuard("new-run-flow", isDirty(draft), () =>
    [draft.launch.title, draft.launch.description, draft.intent.workload?.task, draft.doc?.text].filter(Boolean).join("\n\n"), staysWithDraft);

  const go = React.useCallback((loc: NewRunLocation, opts?: { replace?: boolean }) =>
    void navigate(newRunHref(loc, from), { replace: opts?.replace }), [navigate, from]);
  const startFromScratch = React.useCallback(() => {
    dispatch({ type: "reset", owner: draft.owner });
    setSeeded({ key: "" });
    void navigate(NEW_RUN_PATH);
  }, [dispatch, draft.owner, navigate]);

  if (!owned || stale || foreign || parsed.normalised || target) return null;
  if (key && seeded.key !== key) return null;
  const name = seeded.error !== undefined && key ? "seedFailed" : screenOf(parsed.location);
  const Current = screens[name];
  return (
    <div className={FRAME[name]}>
      <Current location={parsed.location} go={go} seedError={seeded.error} startFromScratch={startFromScratch} />
    </div>
  );
}

export function NewRunRoute({ loaders = seedLoaders, screens = NEW_RUN_SCREENS }: { loaders?: SeedLoaders; screens?: Record<NewRunScreenName, Screen> }) {
  const { principal, operatorResolved } = useRequestIdentity();
  const [message, setMessage] = React.useState("");
  return (
    <NewRunDraftProvider owner={operatorResolved ? principal : ""}>
      <AnnounceContext.Provider value={setMessage}>
        <React.Suspense fallback={null}>
          <NewRunFlow loaders={loaders} screens={screens} />
        </React.Suspense>
        <div role="status" aria-live="polite" className="sr-only">{message}</div>
      </AnnounceContext.Provider>
    </NewRunDraftProvider>
  );
}

/** Every local issue of the draft, for Review's blocker list and the checks' body gate. */
export const useLocalIssues = () => {
  const draft = useNewRunDraft();
  return React.useMemo(() => localIssues(draft.intent, {}, draft.doc), [draft.intent, draft.doc]);
};
