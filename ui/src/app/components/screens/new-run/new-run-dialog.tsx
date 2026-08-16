/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// NewRunDialog — the entry point for launching a run. Workspace-first: it
// always opens on "which workspace?" (an onboarded workspace, or the explicit
// "No workspace — ad-hoc run" escape) BEFORE it ever asks how. A pick lands
// straight on Describe (the AI Run Composer: compose → review → launch) when
// the composer is enabled — there is no separate "how" choice screen.
// "Configure manually" is a footer escape FROM Describe into the existing
// 5-step PermissionWizard, pre-seeded with the same workspace pick (and Run
// mode — see wizard.tsx's initialInteractive).
//
// The composer is OPTIONAL: if it's disabled (404) or has zero backends,
// Describe mode is hidden and choosing a workspace (or ad-hoc) drops straight
// into the manual wizard instead, pre-seeded with that pick — never a crash.
// "Edit in wizard" hands a composer proposal to the wizard, prefilled (its own
// workspace wins over the one picked here — see wizard.tsx's initialWorkspaces).
import * as React from "react";
import { toast } from "sonner";
import { Plus } from "lucide-react";
import type {
  AgentRun,
  ComposeAttachment,
  ComposeMode,
  ComposeQA,
  ComposeQuestion,
  ComposeRequest,
  ComposeResponse,
  ComposeResult,
  ComposerBackend,
  SetupItem,
} from "../../../lib/types";
import { composer as composerApi } from "../../../lib/api/compose";
import { runs as runsApi } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { HttpError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { Chip } from "../../wardyn/primitives";
import { StatusChip } from "../../wardyn/status-chip";
import { Mono } from "../../wardyn/code-block";
import { deriveReadiness } from "../onboarding/intro";
import { Button } from "../../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "../../ui/dialog";
import { OptionCard } from "../../wardyn/form-primitives";
import { ComposeForm } from "./compose-form";
import { ComposeProgress } from "./compose-progress";
import { ComposeQandA } from "./compose-qanda";
import { ComposeReview } from "./compose-review";
import { PermissionWizard } from "./wizard";
import { WorkspaceWizard } from "../workspace-wizard/wizard";
import { AddSecretDialog } from "../secrets";
import { surfaceRunWarnings, useAddSecretFix } from "./run-warnings";
import {
  toRunWorkspacesWire,
  wizardStateFromProposal,
  workspaceProfileOptions,
  type RunWorkspaceSelection,
  type WizardState,
} from "./wizard-types";
import { KIND_META } from "../workspaces";
import { isUsable, statusTone, statusWord } from "../../../lib/workspace-status";

// "workspace" is the entry mode (Stage 3: workspace-first) — the dialog opens
// on "which workspace?" before it ever asks HOW (Describe vs Configure). Every
// other mode is reached only after that choice (or its explicit "no
// workspace" escape) is made. There is no separate "choose" step any more —
// a workspace pick lands straight on "describe" (composer enabled) or
// "wizard" (disabled); "Configure manually" is a footer button on describe.
type Mode = "workspace" | "describe" | "clarify" | "review" | "wizard";

export function NewRunDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (run: AgentRun) => void;
}) {
  const [mode, setMode] = React.useState<Mode>("workspace");
  // UI-RUN-4: `mode` must already read "workspace" on the SAME commit `open`
  // flips true — the dialog stays permanently mounted at every call site, so
  // a stale mode==="wizard" from the PRIOR (manual) session otherwise takes
  // the `mode === "wizard"` early return below and mounts the ABANDONED
  // wizard for one commit, firing its own real health()/listSecrets()/
  // reloadWorkspaces()/listPolicies() calls — before the passive reset effect
  // (which fires AFTER children's effects) swaps it back. React's documented
  // render-time state-adjustment pattern (comparing against a STATE-held
  // previous value, not a ref — react.dev/learn/you-might-not-need-an-effect)
  // fixes it: catch the open transition DURING render, synchronously, so the
  // very first render after opening already reflects it and the wizard never
  // mounts at all.
  const [prevOpen, setPrevOpen] = React.useState(open);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (open) setMode("workspace");
  }
  const [backends, setBackends] = React.useState<ComposerBackend[] | null>(null);
  // Onboarded workspaces — fetched once per dialog-open. Feeds the Describe-mode
  // multi-select picker AND (for "Edit in wizard") resolves a composed
  // proposal's raw mount source / repo string back into a WorkspaceSelection
  // (see wizardStateFromProposal). Best-effort: a failed fetch degrades to an
  // empty picker / "re-pick it in Basics", never a crash.
  const {
    workspaces,
    loading: workspacesLoading,
    reload: reloadWorkspaces,
    scanAndReload,
  } = useWorkspaceList();
  const [addWorkspaceOpen, setAddWorkspaceOpen] = React.useState(false);

  // compose form state
  const [prompt, setPrompt] = React.useState("");
  // Onboarded-workspace multi-select (mirrors the manual wizard's Basics step).
  // Empty => ephemeral; composerApi.compose() resolves these against `workspaces`.
  // Typed RunWorkspaceSelection: ComposeForm's picker has
  // optionalRequirementsEnabled={true}, so enabledOptional is live here, not
  // just a placeholder for a later flag flip.
  const [workspaceSelections, setWorkspaceSelections] = React.useState<RunWorkspaceSelection[]>([]);
  const [attachments, setAttachments] = React.useState<ComposeAttachment[]>([]);
  const [sources, setSources] = React.useState<string[]>([]);
  const [backend, setBackend] = React.useState("");
  const [composeMode, setComposeMode] = React.useState<ComposeMode>("auto");
  const [composing, setComposing] = React.useState(false);
  // Persistent inline error from the last compose attempt (surfaced in the form so
  // a failed compose isn't just a transient toast that looks like "nothing happened").
  const [composeError, setComposeError] = React.useState<string | null>(null);
  // Live SSE pipeline-stage key from composerApi.compose's onStage callback (see
  // ComposeProgress / compose-stages.ts for the user-facing copy).
  const [stage, setStage] = React.useState<string | undefined>(undefined);

  // clarify (interactive Q&A) state
  const [questions, setQuestions] = React.useState<ComposeQuestion[] | null>(null);
  const [assumptions, setAssumptions] = React.useState<string[]>([]);
  const [notes, setNotes] = React.useState("");
  const [transcript, setTranscript] = React.useState<ComposeQA[]>([]);
  const [round, setRound] = React.useState(0);

  // review state
  const [result, setResult] = React.useState<ComposeResponse | null>(null);
  // Launch mode, captured UPFRONT on Describe (ComposeForm's ModeToggle) and
  // re-seeded from each proposal's echoed run.interactive — Review renders it
  // as a read-only fact chip, never a second control (see compose-review.tsx).
  const [interactive, setInteractive] = React.useState(false);
  // Whether the operator actually touched the mode on Describe. The compose
  // default (autonomous) and the wizard default (interactive, task-optional)
  // deliberately differ — only an EXPLICIT pick may cross the manual hand-off,
  // or every wizard entry would seed autonomous and dead-end Basics on the
  // now-required empty Task.
  const [modeTouched, setModeTouched] = React.useState(false);
  const [acknowledged, setAcknowledged] = React.useState(false);
  const [launching, setLaunching] = React.useState(false);
  // Launch (create-run) error surfaced INLINE on the review panel, not as a corner
  // toast — an actionable failure (e.g. an api_key grant referencing a not-yet-stored
  // secret) keeps the operator on the proposal with a fix in reach.
  const [launchError, setLaunchError] = React.useState<string | null>(null);

  // wizard prefill (set when "Edit in wizard" hands off a proposal)
  const [wizardInitial, setWizardInitial] = React.useState<WizardState | undefined>(undefined);

  // Client-owned compose SESSION id (decision 1: no server-side session store) —
  // minted once on entering describe mode and resent unchanged on every compose
  // round and on the eventual launch (compose_session_id), so the audit feed can
  // reconstruct the whole conversation later by filtering on it.
  const [sessionId, setSessionId] = React.useState("");

  // Pre-compose readiness hint (B3/B6, intro.tsx's deriveReadiness — the SAME
  // derivation the Getting-started funnel uses). A full setup checklist is
  // impossible before a proposal exists (items derive from the clamped spec),
  // but this coarse hint catches the common case — no integration powers
  // Wardyn's own AI features yet — before the operator burns a compose round.
  // llmReady is deliberately NOT carried here any more (H8): the ModelAccessCard
  // below states that fact on its own, more precisely.
  const [setupHint, setSetupHint] = React.useState<{ composerReady: boolean } | null>(null);

  // Setup-checklist items the composer's LAST proposal named (compose.go's
  // deriveSetupItems), overlaid with any client-side "re-flip" (decision 9: no
  // recheck endpoint in v1 — after a fix, flip the item from data we already
  // have rather than losing the proposal to a re-compose). Keyed by item.id.
  const [satisfiedOverrides, setSatisfiedOverrides] = React.useState<Set<string>>(new Set());
  const setupItems = React.useMemo<SetupItem[] | undefined>(() => {
    if (!result?.setup_items) return undefined;
    return result.setup_items.map((item) =>
      satisfiedOverrides.has(item.id) ? { ...item, status: "satisfied" as const } : item,
    );
  }, [result, satisfiedOverrides]);

  const composerEnabled = !!backends && backends.length > 0;

  // Item 12: the recorded-profiles hint under Describe's "Configure manually" —
  // reads the PRIMARY selection against the workspace list this dialog already
  // loads (no new fetch). Saved policies get no hint here (they aren't
  // workspace-scoped, so one would be noise) — this dialog has no concept of
  // "saved policies" at all, only the manual wizard's own Basics step does.
  const primaryWorkspace = workspaceSelections[0]
    ? workspaces.find((w) => w.id === workspaceSelections[0].workspaceId)
    : undefined;
  const recordedProfiles = workspaceProfileOptions(primaryWorkspace);

  // On open: probe backends. While probing we show the chooser skeleton; once we
  // know whether the composer is available we land on the right initial mode. A
  // disabled composer (404 / empty / error) goes straight to the manual wizard.
  React.useEffect(() => {
    if (!open) return;
    // reset all OTHER transient state for a clean dialog each open — `mode`
    // itself already reset synchronously above, on the same commit (UI-RUN-4).
    setBackends(null);
    setPrompt("");
    setWorkspaceSelections([]);
    setAttachments([]);
    setSources([]);
    setComposeMode("auto");
    setComposing(false);
    setQuestions(null);
    setAssumptions([]);
    setNotes("");
    setTranscript([]);
    setRound(0);
    setResult(null);
    setInteractive(false);
    setModeTouched(false);
    setAcknowledged(false);
    setLaunching(false);
    setWizardInitial(undefined);
    setSessionId("");
    setSetupHint(null);
    setSatisfiedOverrides(new Set());
    // `true` = clear first: a re-opened dialog must not flash the last session's list.
    reloadWorkspaces(true);

    let alive = true;
    composerApi
      .listComposerBackends()
      .then((bs) => {
        if (!alive) return;
        setBackends(bs);
        if (bs.length > 0) setBackend(bs.find((b) => b.is_default)?.name ?? bs[0].name);
        // Never yank the operator off the workspace-first step the instant this
        // probe resolves (typically well before they've picked anything) — only
        // steer mode once they've already left it via chooseWorkspace(). From
        // "describe" onward, an empty/failed backend list still bails to the
        // manual wizard (composer not configured — manual is the only path),
        // matching pre-Stage-3 behaviour for that later transition.
        setMode((m) => (m === "workspace" ? m : bs.length === 0 ? "wizard" : m));
      })
      .catch(() => {
        if (!alive) return;
        setBackends([]);
        setMode((m) => (m === "workspace" ? m : "wizard"));
      });
    // Best-effort readiness hint for the amber banner above the Describe form —
    // never blocks the chooser, never throws (getSetupStatus already degrades to
    // READY_FALLBACK on any failure, so this simply resolves to "no hint").
    setupApi
      .getSetupStatus()
      .then((status) => {
        if (!alive) return;
        const r = deriveReadiness(status);
        setSetupHint({ composerReady: r.composerReady });
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [open, reloadWorkspaces]);

  // compose() returns EITHER clarifying questions or a final proposal; route on
  // the discriminant. `extra` carries the per-call mode/transcript/round.
  const submitCompose = async (extra: Partial<ComposeRequest>) => {
    setComposing(true);
    setComposeError(null);
    setLaunchError(null);
    setStage(undefined);
    try {
      const res: ComposeResult = await composerApi.compose(
        {
          prompt: prompt.trim(),
          workspaceSelections,
          // Per-workspace optional-requirement opt-ins / read-only narrowing —
          // same wire shape and same "non-default only" filter as the manual
          // wizard's buildSpec (toRunWorkspacesWire). ComposeForm's picker has
          // optionalRequirementsEnabled={true}, so a real opt-in/read-only pick
          // reaches this field, not just an empty array.
          workspaceOptions: toRunWorkspacesWire(workspaceSelections),
          attachments,
          sources,
          backend: backend || undefined,
          mode: composeMode,
          interactive,
          // Raw persisted default tier as a per-run floor; the server caps it.
          confinementFloor: getDefaultCc() ?? undefined,
          // Resent unchanged on every round of this describe-mode conversation
          // (decision 1: the server holds no session state).
          sessionId: sessionId || undefined,
          ...extra,
        },
        workspaces,
        setStage,
      );
      if (res.kind === "questions") {
        setQuestions(res.questions);
        setAssumptions(res.assumptions ?? []);
        setNotes(res.notes ?? "");
        setRound(res.round);
        setMode("clarify");
      } else {
        setResult(res);
        setInteractive(!!res.proposed.run.interactive);
        setAcknowledged(false);
        // A fresh proposal starts a fresh checklist — any earlier re-flip no
        // longer applies to (possibly different) setup_items ids.
        setSatisfiedOverrides(new Set());
        setMode("review");
      }
    } catch (e) {
      const msg = composeErrorMessage(e);
      setComposeError(msg);
      toast.error("Couldn't compose a run", { description: msg });
    } finally {
      setComposing(false);
      setStage(undefined);
    }
  };

  // First "Compose" click: round 0, no transcript yet.
  const runCompose = () => {
    setTranscript([]);
    setRound(0);
    void submitCompose({ round: 0, transcript: [] });
  };

  // Answer the current round's questions: accumulate the transcript and advance.
  const submitAnswers = (newAnswers: ComposeQA[]) => {
    const merged = [...transcript, ...newAnswers];
    const nextRound = round + 1;
    setTranscript(merged);
    void submitCompose({ round: nextRound, transcript: merged });
  };

  // Skip remaining questions and propose now (one-shot, keeping any answers so far).
  const skipToProposal = () => {
    void submitCompose({ mode: "skip", round, transcript });
  };

  const approveLaunch = async () => {
    if (!result) return;
    setLaunchError(null);
    setLaunching(true);
    try {
      const created = await runsApi.createRun({
        ...result.proposed.run,
        interactive,
        inline_policy: result.proposed.inline_policy,
        // Correlates the launched run's audit row back to the compose
        // conversation that produced it (absent when the dialog never entered
        // describe mode, e.g. a straight-to-wizard launch has no session).
        compose_session_id: sessionId || undefined,
        // The proposal's echoed workspace_selections (composeRequest.
        // WorkspaceSelections, forwarded unchanged from what submitCompose
        // sent) — so this composed launch goes through the SAME
        // seedRequestWorkspace + resolveWorkspaceSelections fold a manual
        // wizard launch does, instead of silently discarding every optional
        // opt-in and read-only narrowing the operator made.
        workspaces: result.proposed.workspace_selections,
      });
      setLaunching(false);
      onOpenChange(false);
      onCreated(created);
      // Advisory POST /runs warnings (e.g. a workspace-dir collision): surface
      // without blocking — the run already launched.
      surfaceRunWarnings(created);
    } catch (e) {
      setLaunching(false);
      // Keep the panel open and surface the error INLINE (ComposeReview) — most
      // launch failures are fixable right here (a missing secret, a policy nit) and
      // dropping the operator to a corner toast loses the whole proposal.
      setLaunchError(getErrorMessage(e));
    }
  };

  // Decision 9 (no recheck endpoint in v1): after AddSecretDialog saves a name,
  // re-flip in place from data we already have rather than losing the proposal to
  // a re-compose. (a) optimistically satisfy every checklist item this exact
  // secret name fixes, and (b) re-fire the existing setup-status probe — the same
  // secret may ALSO be the model's own key, so llm_access gets re-flipped from
  // REAL server state, never guessed client-side.
  const flipSecretSatisfied = (name: string) => {
    const items = result?.setup_items;
    if (!items) return;
    const bySecret = items.filter((i) => i.fix?.secret_name === name).map((i) => i.id);
    if (bySecret.length) {
      setSatisfiedOverrides((prev) => new Set([...prev, ...bySecret]));
    }
    setupApi
      .getSetupStatus()
      .then((status) => {
        if (!deriveReadiness(status).llmReady) return;
        const llmIds = items.filter((i) => i.kind === "llm_access").map((i) => i.id);
        if (llmIds.length) setSatisfiedOverrides((prev) => new Set([...prev, ...llmIds]));
      })
      .catch(() => {});
  };

  // Shared add-secret recovery (run-warnings.ts): every save re-flips the checklist
  // (flipSecretSatisfied); the launch-error fix ALSO re-launches. ComposeReview's
  // single onAddSecret routes the launch-error banner through openFix (retry) and
  // the no-model / checklist affordances through openManual — matching the old
  // "retry iff a launchError was set" behaviour, decided at click time.
  const secretFix = useAddSecretFix({
    onManual: (name) => flipSecretSatisfied(name),
    onRetry: (name) => {
      flipSecretSatisfied(name);
      setLaunchError(null);
      void approveLaunch();
    },
  });

  // Workspace checklist items re-flip from the workspace list the dialog already
  // loads (decision 9) — once a scan lands the workspace at "ready", satisfy any
  // still-open workspace item that named it via Fix.WorkspaceID.
  // the `i.id.slice(...)` fallback leans on compose_setup.go's stable
  // "<kind>:<key>" id contract instead of threading a workspace id separately
  // through every item — fine while that contract holds; if a future kind's key
  // ever isn't the workspace id, give workspace items their own explicit field.
  React.useEffect(() => {
    const items = result?.setup_items;
    if (!items) return;
    const ids = items
      .filter((i) => i.kind === "workspace" && i.status !== "satisfied")
      .filter((i) => {
        const wsId = i.fix?.workspace_id ?? i.id.slice(i.kind.length + 1);
        const found = workspaces.find((w) => w.id === wsId);
        return !!found && isUsable(found.status);
      })
      .map((i) => i.id);
    if (ids.length) setSatisfiedOverrides((prev) => new Set([...prev, ...ids]));
  }, [workspaces, result]);

  const editInWizard = () => {
    if (!result) return;
    setLaunchError(null);
    setWizardInitial(
      wizardStateFromProposal(
        { ...result.proposed.run, interactive },
        result.proposed.inline_policy,
        workspaces,
        // Echo whatever Optional opt-ins / read-only narrowing the operator
        // made on this composed proposal's WorkspacePicker — without this,
        // "Edit in wizard" silently dropped every one of them (the matched
        // selection only ever carried the mount's inferred read-only).
        result.proposed.workspace_selections,
      ),
    );
    setMode("wizard");
  };

  // Edit prompt (Review's retreat, §2): prompt, workspaceSelections,
  // attachments, sources, backend, composeMode and sessionId all already live
  // in this component and are only ever cleared on dialog open — so returning
  // to Describe needs nothing saved. The next "Compose" click naturally resets
  // transcript/round to 0 via runCompose (above), resending the SAME session
  // id, so the audit feed can still reconstruct the whole conversation.
  const editPrompt = () => setMode("describe");

  // Stage 3 — workspace-first entry: the FIRST decision in this dialog is which
  // workspace (or none), before Describe-vs-Configure is even offered. Seeds
  // workspaceSelections — already the AI path's own state (submitCompose reads
  // it unchanged) — so both paths start from the identical pick; the manual
  // path gets it via PermissionWizard's initialWorkspaces below. `sel` is null
  // for the explicit "No workspace — ad-hoc run" escape, which must reproduce
  // today's exact behaviour: an empty selection, nothing more, an ephemeral
  // scratch run.
  //
  // A workspace pick lands straight on Describe now (no intermediate "choose"
  // screen) — minting the compose-session id here, where a compose
  // conversation actually starts (moved from the deleted choose card's own
  // onClick); resent unchanged on every round + at launch. Composer disabled
  // still routes straight to the manual wizard, unchanged.
  const chooseWorkspace = (sel: RunWorkspaceSelection | null) => {
    setWorkspaceSelections(sel ? [sel] : []);
    if (!composerEnabled) {
      setMode("wizard");
      return;
    }
    setSessionId(crypto.randomUUID());
    setMode("describe");
  };

  // Manual mode renders the existing wizard as its own Dialog. It owns its
  // chrome, so we hand off entirely (and pass the prefill when editing).
  // initialWorkspaces seeds a FRESH entry's Basics selection from the
  // workspace-first pick above; editInWizard's initialState (when set) already
  // carries its own resolved workspaces and wins (wizard.tsx ignores
  // initialWorkspaces once initialState is provided).
  if (mode === "wizard") {
    return (
      <PermissionWizard
        open={open}
        onOpenChange={onOpenChange}
        onCreated={onCreated}
        initialState={wizardInitial}
        initialWorkspaces={workspaceSelections}
        initialInteractive={modeTouched ? interactive : undefined}
      />
    );
  }

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="flex max-h-[88vh] flex-col gap-0 sm:max-w-2xl lg:max-w-5xl xl:max-w-6xl">
        <DialogHeader className="border-b border-border pb-4">
          <DialogTitle>
            {mode === "review"
              ? "Proposed setup"
              : mode === "clarify"
                ? "A few questions"
                : "New run"}
          </DialogTitle>
          <DialogDescription>
            {mode === "review"
              ? "Review Wardyn's proposed confinement before launching. Wardyn graded this deterministically — not the model."
              : mode === "clarify"
                ? "Wardyn's composer needs a little more detail to propose a least-privilege run. Your answers shape the proposal only — Wardyn still grades and clamps it."
                : mode === "workspace"
                  ? "Start from an onboarded workspace and the requirements it comes with, or run ad-hoc. Everything here stays editable in the steps that follow."
                  : "Describe what you want done in this workspace. Wardyn proposes a confined run for you to review before anything starts."}
          </DialogDescription>
        </DialogHeader>

        <div className="scroll-thin -mx-1 flex-1 overflow-y-auto px-1 py-4">
          {mode === "workspace" && (
            <div className="space-y-3">
              {workspacesLoading && workspaces.length === 0 ? (
                <p className="text-sm text-muted-foreground">Loading workspaces…</p>
              ) : (
                <>
                  {/* UI-RUN-7: the composer-backends probe still gates every
                      card below (chooseWorkspace reads composerEnabled AT
                      CLICK TIME — see its own comment) — say so instead of
                      just greying the whole grid out with no explanation. */}
                  {backends === null && (
                    <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
                      <StatusChip status="checking" /> Checking which run modes are available…
                    </p>
                  )}
                  {/* UX-5: the first decision of the first-run journey must
                      never present an empty grid with no explanation and no
                      way to fix it — WorkspacePicker's own empty state
                      (workspace-picker.tsx) says the same thing. */}
                  {workspaces.length === 0 && (
                    <p className="text-xs text-muted-foreground">
                      No workspaces added yet — runs can only attach what exists here.
                    </p>
                  )}
                  <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                    {workspaces.map((w) => {
                      const KindIcon = (KIND_META[w.kind] ?? KIND_META.local_dir).Icon;
                      return (
                        <OptionCard
                          key={w.id}
                          selected={false}
                          // The backends probe is still in flight while
                          // backends === null — chooseWorkspace reads
                          // composerEnabled (derived from it) AT CLICK TIME, so
                          // a click landing inside that window would route to
                          // the manual wizard even when the composer is really
                          // enabled, with no way back to "Describe your task".
                          disabled={backends === null}
                          onClick={() => chooseWorkspace({ workspaceId: w.id })}
                          className="h-full"
                          title={
                            <span className="flex items-center gap-2">
                              <KindIcon
                                className="size-4 shrink-0 text-muted-foreground"
                                aria-hidden="true"
                              />
                              {w.name}
                              <Chip tone={statusTone(w.status).tone}>{statusWord(w.status)}</Chip>
                            </span>
                          }
                          hint={<Mono className="text-[0.6875rem]">{w.source}</Mono>}
                        />
                      );
                    })}
                    {/* UX-5: the workspace-first screen asks the operator to
                        pick an onboarded workspace but, until now, offered no
                        way to onboard one from here — opens the SAME
                        already-mounted origin="run" wizard ComposeForm's own
                        Add-workspace affordance does. Not gated on the
                        backends probe — it never reads composerEnabled. */}
                    <OptionCard
                      selected={false}
                      onClick={() => setAddWorkspaceOpen(true)}
                      className="h-full"
                      title={
                        <span className="flex items-center gap-2">
                          <Plus className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                          Add a workspace
                        </span>
                      }
                      hint="Onboard a new directory or repo, then attach it to this run."
                    />
                    <OptionCard
                      selected={false}
                      disabled={backends === null}
                      onClick={() => chooseWorkspace(null)}
                      className="h-full"
                      title="No workspace — ad-hoc run"
                      hint="An empty scratch directory inside the sandbox. Nothing on your machine is reachable."
                    />
                  </div>
                </>
              )}
            </div>
          )}

          {/* Halved (H8): composerReady is the ONLY fact ComposeForm's own
              banner states here — the ModelAccessCard just above it in that
              form states the llmReady fact more precisely (resolved
              integration + "Applies because"), so repeating it would say the
              same thing twice. composerReady still needs its own banner: it
              answers "does an integration power Wardyn's OWN AI features"
              (this composer included), which the ModelAccessCard — scoped to
              the AGENT's model access — never claims either way. Body order
              (§1) puts it after model access, before "More options", so it
              renders INSIDE ComposeForm now, not above the whole form. */}
          {mode === "describe" && composerEnabled && (
            <ComposeForm
              prompt={prompt}
              workspaceSelections={workspaceSelections}
              workspaces={workspaces}
              workspacesLoading={workspacesLoading}
              onAddWorkspace={() => setAddWorkspaceOpen(true)}
              attachments={attachments}
              sources={sources}
              backend={backend}
              backends={backends!}
              mode={composeMode}
              interactive={interactive}
              composing={composing}
              onPromptChange={setPrompt}
              onWorkspaceSelectionsChange={setWorkspaceSelections}
              onAttachmentsChange={setAttachments}
              onSourcesChange={setSources}
              onBackendChange={setBackend}
              onModeChange={setComposeMode}
              onInteractiveChange={(v) => {
                setInteractive(v);
                setModeTouched(true);
              }}
              onCompose={runCompose}
              error={composeError}
              setupHint={setupHint}
            />
          )}
          {mode === "describe" && composing && (
            <div className="mt-4">
              <ComposeProgress stage={stage} />
            </div>
          )}

          {mode === "clarify" && questions && (
            <ComposeQandA
              key={round}
              questions={questions}
              assumptions={assumptions}
              notes={notes}
              round={round}
              submitting={composing}
              onSubmit={submitAnswers}
              onSkip={skipToProposal}
              onBack={() => setMode("describe")}
            />
          )}
          {mode === "clarify" && composing && (
            <div className="mt-4">
              <ComposeProgress stage={stage} />
            </div>
          )}

          {mode === "review" && result && (
            <ComposeReview
              result={result}
              setupItems={setupItems}
              interactive={interactive}
              acknowledged={acknowledged}
              launching={launching}
              launchError={launchError}
              onAcknowledge={setAcknowledged}
              onApproveLaunch={approveLaunch}
              onAddSecret={(name) => (launchError ? secretFix.openFix(name) : secretFix.openManual(name))}
              onFixWorkspace={scanAndReload}
              onEditPrompt={editPrompt}
              onEditInWizard={editInWizard}
              onCancel={() => onOpenChange(false)}
            />
          )}
        </div>

        {/* Describe's own footer retreat: Back returns to the workspace-first
            pick; Configure manually hands off to the wizard, pre-seeded with
            the same workspace choice — exactly what the deleted "choose"
            card's own button did (workspaceSelections already carries it). */}
        {mode === "describe" && (
          <div className="space-y-1.5 border-t border-border pt-4">
            <div className="flex items-center gap-2">
              <Button variant="ghost" onClick={() => setMode("workspace")} disabled={composing}>
                Back
              </Button>
              <Button variant="ghost" onClick={() => setMode("wizard")} disabled={composing}>
                Configure manually
              </Button>
            </div>
            {/* Item 12: cheapest coherent treatment for recorded profiles — they
                stay manual-only, but this names the count so the operator
                doesn't have to open the wizard to discover they exist. One
                template-literal expression (not split JSX text) — a line break
                between adjacent {expr} runs gets trimmed by JSX's whitespace
                collapsing, which would silently eat the separating space. */}
            {recordedProfiles.length > 0 && (
              <p className="pl-1 text-[0.6875rem] leading-snug text-muted-foreground">
                {`This workspace has ${recordedProfiles.length} recorded setup${
                  recordedProfiles.length > 1 ? "s" : ""
                } — Configure manually to start from one.`}
              </p>
            )}
          </div>
        )}
        </DialogContent>
      </Dialog>

      {/* origin="run": Done's primary action is "Attach to this run" (onAttach)
          instead of "Open workspace" — the wizard already scans/builds/verifies
          along the way, so there's no separate scanAndReload kick to make here
          (contrast onFixWorkspace above, which re-scans an EXISTING pick). */}
      {addWorkspaceOpen && (
        <WorkspaceWizard
          origin="run"
          onClose={() => {
            setAddWorkspaceOpen(false);
            reloadWorkspaces();
          }}
          onAttach={(workspaceId) => {
            // Mirrors the manual wizard's Basics step: auto-attach so the
            // operator doesn't have to re-open the picker for what they just
            // onboarded.
            setWorkspaceSelections((sel) => [...sel, { workspaceId }]);
            setAddWorkspaceOpen(false);
            reloadWorkspaces();
          }}
        />
      )}

      {/* Opened from ComposeReview: the launch-error helper, the no-model-access
          banner, or a checklist row's "Add secret" action. Every caller gets the
          same re-flip (decision 9); only the launch-error path also auto-retries
          the launch (its whole point is "fix and go" with no lost proposal). The
          retry-vs-manual branch lives in the shared useAddSecretFix hook. */}
      <AddSecretDialog {...secretFix.dialogProps} />
    </>
  );
}

// Turn a compose() failure into a human description keyed off the HTTP status,
// matching the endpoint's status semantics (see composerApi.compose / compose.go).
function composeErrorMessage(e: unknown): string {
  if (e instanceof HttpError) {
    switch (e.status) {
      case 404:
        return "The AI Run Composer is not enabled on this control plane.";
      case 400:
        return "The request was rejected. Check your prompt and provider, then try again.";
      case 413:
        return "Your prompt and attachments are too large. Trim them and try again.";
      case 502: {
        // Surface the backend's own reason when it has one (rate limit, max_turns,
        // auth, refusal) — it's actionable; fall back to the generic line otherwise.
        const detail = e.message && e.message !== "compose failed" ? ` (${e.message})` : "";
        return `The composer backend failed to respond${detail}. Try again, or configure manually.`;
      }
      default:
        return e.message;
    }
  }
  return getErrorMessage(e);
}
