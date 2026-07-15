/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// NewRunDialog — the entry point for launching a run. It chooses between two
// entry modes:
//   - "Describe your task"   → the AI Run Composer (compose → review → launch)
//   - "Configure manually"   → the existing 5-step PermissionWizard
//
// The composer is OPTIONAL: if it's disabled (404) or has zero backends, Describe
// mode is hidden and the dialog opens straight into the manual wizard — never a
// crash. "Edit in wizard" hands a composer proposal to the wizard, prefilled.
import * as React from "react";
import { Link } from "react-router-dom";
import { Settings2, Sparkles, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
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
  Workspace,
  WorkspaceSelection,
} from "../../../lib/types";
import { api, HttpError } from "../../../lib/api";
import { getErrorMessage } from "../../../lib/format";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { deriveReadiness } from "../onboarding/intro";
import { Button } from "../../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "../../ui/dialog";
import { OptionCard } from "./step-shell";
import { ComposeForm } from "./compose-form";
import { ComposeProgress } from "./compose-progress";
import { ComposeQandA } from "./compose-qanda";
import { ComposeReview } from "./compose-review";
import { PermissionWizard } from "./wizard";
import { AddWorkspaceDialog } from "../workspaces";
import { AddSecretDialog } from "../secrets";
import { surfaceRunWarnings, useAddSecretFix } from "./run-warnings";
import { wizardStateFromProposal, type WizardState } from "./wizard-types";

type Mode = "choose" | "describe" | "clarify" | "review" | "wizard";

export function NewRunDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onCreated: (run: AgentRun) => void;
}) {
  const [mode, setMode] = React.useState<Mode>("choose");
  const [backends, setBackends] = React.useState<ComposerBackend[] | null>(null);
  // Onboarded workspaces — fetched once per dialog-open. Feeds the Describe-mode
  // multi-select picker AND (for "Edit in wizard") resolves a composed
  // proposal's raw mount source / repo string back into a WorkspaceSelection
  // (see wizardStateFromProposal). Best-effort: a failed fetch degrades to an
  // empty picker / "re-pick it in Basics", never a crash.
  const [workspaces, setWorkspaces] = React.useState<Workspace[]>([]);
  const [workspacesLoading, setWorkspacesLoading] = React.useState(false);
  const [addWorkspaceOpen, setAddWorkspaceOpen] = React.useState(false);

  const loadWorkspaces = React.useCallback(() => {
    setWorkspacesLoading(true);
    api
      .listWorkspaces()
      .then(setWorkspaces)
      .catch(() => setWorkspaces([]))
      .finally(() => setWorkspacesLoading(false));
  }, []);

  // compose form state
  const [prompt, setPrompt] = React.useState("");
  // Onboarded-workspace multi-select (mirrors the manual wizard's Basics step).
  // Empty => ephemeral; api.compose() resolves these against `workspaces`.
  const [workspaceSelections, setWorkspaceSelections] = React.useState<WorkspaceSelection[]>([]);
  const [attachments, setAttachments] = React.useState<ComposeAttachment[]>([]);
  const [sources, setSources] = React.useState<string[]>([]);
  const [backend, setBackend] = React.useState("");
  const [composeMode, setComposeMode] = React.useState<ComposeMode>("auto");
  const [composing, setComposing] = React.useState(false);
  // Persistent inline error from the last compose attempt (surfaced in the form so
  // a failed compose isn't just a transient toast that looks like "nothing happened").
  const [composeError, setComposeError] = React.useState<string | null>(null);
  // Live SSE pipeline-stage key from api.compose's onStage callback (see
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
  // Launch mode, seeded from the proposal and overridable in the review screen.
  const [interactive, setInteractive] = React.useState(false);
  // Per-run Claude-subscription opt-in (threaded to compose; server-gated).
  const [useSubscription, setUseSubscription] = React.useState(false);
  const [acknowledged, setAcknowledged] = React.useState(false);
  const [launching, setLaunching] = React.useState(false);
  // Launch (create-run) error surfaced INLINE on the review panel, not as a corner
  // toast — an actionable failure (e.g. an api_key grant referencing a not-yet-stored
  // secret) keeps the operator on the proposal with a fix in reach.
  const [launchError, setLaunchError] = React.useState<string | null>(null);

  // wizard prefill (set when "Edit in wizard" hands off a proposal)
  const [wizardInitial, setWizardInitial] = React.useState<WizardState | undefined>(undefined);

  // Per-dialog-open correlation id for the client telemetry beacon (mode
  // transitions only — see api.telemetry). Regenerated each time the dialog opens.
  const [correlationId, setCorrelationId] = React.useState("");

  // Client-owned compose SESSION id (decision 1: no server-side session store) —
  // minted once on entering describe mode and resent unchanged on every compose
  // round and on the eventual launch (compose_session_id), so the audit feed can
  // reconstruct the whole conversation later by filtering on it.
  const [sessionId, setSessionId] = React.useState("");

  // Pre-compose readiness hint (B3/B6, intro.tsx's deriveReadiness — the SAME
  // derivation the Getting-started funnel uses). A full setup checklist is
  // impossible before a proposal exists (items derive from the clamped spec), but
  // this coarse hint catches the common case — no model access configured yet —
  // before the operator burns a compose round on a run that can't call a model.
  const [setupHint, setSetupHint] = React.useState<{ composerReady: boolean; llmReady: boolean } | null>(
    null,
  );

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

  // On open: probe backends. While probing we show the chooser skeleton; once we
  // know whether the composer is available we land on the right initial mode. A
  // disabled composer (404 / empty / error) goes straight to the manual wizard.
  React.useEffect(() => {
    if (!open) return;
    // reset all transient state for a clean dialog each open
    setMode("choose");
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
    setAcknowledged(false);
    setLaunching(false);
    setWizardInitial(undefined);
    setCorrelationId(crypto.randomUUID());
    setSessionId("");
    setSetupHint(null);
    setSatisfiedOverrides(new Set());
    setWorkspaces([]);
    loadWorkspaces();

    let alive = true;
    api
      .listComposerBackends()
      .then((bs) => {
        if (!alive) return;
        setBackends(bs);
        if (bs.length === 0) {
          // Composer not configured — manual is the only path.
          setMode("wizard");
          return;
        }
        setBackend(bs.find((b) => b.is_default)?.name ?? bs[0].name);
        setMode("choose");
      })
      .catch(() => {
        if (!alive) return;
        setBackends([]);
        setMode("wizard");
      });
    // Best-effort readiness hint for the amber banner above the Describe form —
    // never blocks the chooser, never throws (getSetupStatus already degrades to
    // READY_FALLBACK on any failure, so this simply resolves to "no hint").
    api
      .getSetupStatus()
      .then((status) => {
        if (!alive) return;
        const r = deriveReadiness(status);
        setSetupHint({ composerReady: r.composerReady, llmReady: r.llmReady });
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [open, loadWorkspaces]);

  // Client telemetry beacon: fires once per mode transition (choose → describe →
  // clarify → review → wizard). Best-effort, fire-and-forget — mode + correlation
  // id ONLY, never prompt/secret content (api.telemetry swallows its own errors).
  React.useEffect(() => {
    if (!open || !correlationId) return;
    void api.telemetry({ mode, correlation_id: correlationId });
  }, [open, mode, correlationId]);

  // compose() returns EITHER clarifying questions or a final proposal; route on
  // the discriminant. `extra` carries the per-call mode/transcript/round.
  const submitCompose = async (extra: Partial<ComposeRequest>) => {
    setComposing(true);
    setComposeError(null);
    setLaunchError(null);
    setStage(undefined);
    try {
      const res: ComposeResult = await api.compose(
        {
          prompt: prompt.trim(),
          workspaceSelections,
          attachments,
          sources,
          backend: backend || undefined,
          mode: composeMode,
          interactive,
          useSubscription,
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
      const created = await api.createRun({
        ...result.proposed.run,
        interactive,
        inline_policy: result.proposed.inline_policy,
        // Correlates the launched run's audit row back to the compose
        // conversation that produced it (absent when the dialog never entered
        // describe mode, e.g. a straight-to-wizard launch has no session).
        compose_session_id: sessionId || undefined,
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
    api
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

  // scan_workspace fix (Fix.WorkspaceID): the same best-effort re-scan already
  // wired below on AddWorkspaceDialog.onSaved — kick it off, then reload the
  // workspace list the re-flip effect (below) watches.
  const fixWorkspace = (workspaceId: string) => {
    api
      .scanWorkspace(workspaceId)
      .catch(() => {})
      .finally(loadWorkspaces);
  };

  // Workspace checklist items re-flip from the workspace list the dialog already
  // loads (decision 9) — once a scan lands the workspace at "ready", satisfy any
  // still-open workspace item that named it via Fix.WorkspaceID.
  // ponytail: the `i.id.slice(...)` fallback leans on compose_setup.go's stable
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
        return workspaces.find((w) => w.id === wsId)?.status === "ready";
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
      ),
    );
    setMode("wizard");
  };

  // Manual mode renders the existing wizard as its own Dialog. It owns its
  // chrome, so we hand off entirely (and pass the prefill when editing).
  if (mode === "wizard") {
    return (
      <PermissionWizard
        open={open}
        onOpenChange={onOpenChange}
        onCreated={onCreated}
        initialState={wizardInitial}
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
                : "Describe your task and let Wardyn propose a confined run, or configure the permission envelope by hand."}
          </DialogDescription>
        </DialogHeader>

        <div className="scroll-thin -mx-1 flex-1 overflow-y-auto px-1 py-4">
          {mode === "choose" && (
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <OptionCard
                selected={false}
                onClick={() => {
                  // Mint the compose-session id on entering describe mode (not on
                  // dialog open) — this is where a compose conversation actually
                  // starts; resent unchanged on every round + at launch.
                  setSessionId(crypto.randomUUID());
                  setMode("describe");
                }}
                className="h-full"
                title={
                  <span className="flex items-center gap-2">
                    <Sparkles className="size-4 text-primary" /> Describe your task
                  </span>
                }
                hint="Write what you want done in plain language. Wardyn proposes a confined run setup for you to review."
              />
              <OptionCard
                selected={false}
                onClick={() => setMode("wizard")}
                className="h-full"
                title={
                  <span className="flex items-center gap-2">
                    <Settings2 className="size-4" /> Configure manually
                  </span>
                }
                hint="Compose the permission envelope step by step in the wizard."
              />
            </div>
          )}

          {mode === "describe" && composerEnabled && setupHint && (!setupHint.composerReady || !setupHint.llmReady) && (
            <div className="mb-3 flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle p-3 text-xs leading-relaxed text-warning">
              <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
              <p>
                Wardyn doesn&apos;t have model access configured yet — Wardyn can still draft a
                proposal, but a launched run won&apos;t be able to call a model until this is fixed.{" "}
                <Link to="/setup" className="font-medium underline underline-offset-2 hover:text-warning">
                  Finish Getting started
                </Link>
                .
              </p>
            </div>
          )}

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
              useSubscription={useSubscription}
              composing={composing}
              onPromptChange={setPrompt}
              onWorkspaceSelectionsChange={setWorkspaceSelections}
              onAttachmentsChange={setAttachments}
              onSourcesChange={setSources}
              onBackendChange={setBackend}
              onModeChange={setComposeMode}
              onInteractiveChange={setInteractive}
              onUseSubscriptionChange={setUseSubscription}
              onCompose={runCompose}
              error={composeError}
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
              onInteractiveChange={setInteractive}
              onAcknowledge={setAcknowledged}
              onApproveLaunch={approveLaunch}
              onAddSecret={(name) => (launchError ? secretFix.openFix(name) : secretFix.openManual(name))}
              onFixWorkspace={fixWorkspace}
              onEditInWizard={editInWizard}
              onCancel={() => onOpenChange(false)}
            />
          )}
        </div>

        {mode === "describe" && (
          <div className="flex items-center justify-between border-t border-border pt-4">
            <Button variant="ghost" onClick={() => setMode("choose")} disabled={composing}>
              Back
            </Button>
          </div>
        )}
        </DialogContent>
      </Dialog>

      <AddWorkspaceDialog
        open={addWorkspaceOpen}
        onOpenChange={setAddWorkspaceOpen}
        onSaved={(ws) => {
          // Auto-attach the newly onboarded workspace (mirrors the manual
          // wizard's Basics step) so the operator doesn't have to re-open the
          // picker to select what they just added.
          setWorkspaceSelections((sel) => [...sel, { workspaceId: ws.id }]);
          loadWorkspaces();
          // Best-effort scan so the inline path isn't left stuck in pending_scan
          // (matches the Workspaces screen). Refresh once it settles.
          api.scanWorkspace(ws.id).catch(() => {}).finally(loadWorkspaces);
        }}
      />

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
// matching the endpoint's status semantics (see api.compose / compose.go).
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
