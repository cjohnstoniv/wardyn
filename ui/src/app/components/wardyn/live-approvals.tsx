/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// LiveApprovals — an inline approve/deny strip co-located with the run's live
// view (the attached terminal on run detail, or the verify panel during a
// recording). It polls the run's PENDING approvals every 2s and decides them in
// place, so a human watching the terminal never has to leave it to unblock an
// off-policy egress request.
//
// It highlights a HELD request (first_use mode wait_for_review): the sandbox
// connection is parked live waiting for this decision, so approving it lets the
// request through transparently. A passive deny_with_review pending is shown too,
// but without the "waiting" urgency.
//
// It also carries the run's tool_call holds (a `tool_approvals: hold` run parks
// the agent on every mutating tool call), which is why some rows here have no
// scope caret — see decide().
import * as React from "react";
import { ShieldAlert, Clock, Check, ChevronDown, Loader2, X } from "lucide-react";
import { toast } from "sonner";
import { canDecideApproval, decisionArgs, type ApprovalRequest, type ApprovalScope } from "../../lib/types";
import { approvals as api } from "../../lib/api/approvals";
import { getErrorMessage } from "../../lib/format";
import { usePoll } from "../../lib/use-poll";
import { useDeferredBusy } from "../../lib/use-deferred-busy";
import { Button } from "../ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuTrigger } from "../ui/dropdown-menu";
import { cn } from "../ui/utils";
import { Mono } from "./code-block";
import { Chip, OperatorOnlyHint, SectionLabel } from "./primitives";
import { useSecurityOperator } from "./operator-context";
import { attentionRank } from "./run-state-glyph";
import {
  ALWAYS_NEEDS_WORKSPACE,
  APPROVAL_SCOPE_HINT,
  APPROVAL_SCOPE_LABEL,
  DENY_SCOPE_HINT,
  DENY_SCOPE_LABEL,
  OPERATOR_ONLY_REASON,
  TELEMETRY_TAG,
  UNTIL_PRESETS,
  credentialKind,
  denyDialogCopy,
} from "./copy";

const POLL_MS = 2000;

// A pending deny, carrying the scope the caret menu picked (or "run", the
// bare Deny button's default — today's behavior). Deny always confirms
// (W20-hold-fsm-6) regardless of scope, so picking a scope from the menu
// opens this same dialog rather than deciding immediately — unlike Approve,
// where a non-default scope decides on the spot (see decide() below).
interface DenyTarget {
  request: ApprovalRequest;
  scope: ApprovalScope;
  until?: string;
}

// The proxy's hold (ResolveWait, internal/egress/proxy/approvals.go) parks a
// wait_for_review connection for defaultHoldTimeout (30s) and then fails the
// request closed — the approval row itself stays PENDING for up to 24h
// (approvalExpiryAfter), so nothing server-side flips the mode once the real
// hold has already timed out.
// ponytail: hardcoded mirror of the server DEFAULT, not a config read — a
// policy's first_use_hold_seconds CAN override it (configureHold), so under
// a longer hold this chip stops flagging at 30s; wire the policy value
// through if anyone ships a policy that sets it.
const HOLD_TIMEOUT_MS = 30_000;

// A held request is one the sandbox is still parked on. TWO shapes reach that
// state and only one of them carries a mode:
//
//  - tool_call — wardyn-toolgate blocks the agent's tool call on the PENDING
//    row itself and polls until it is decided (cmd/wardyn-toolgate/main.go's
//    -deadline is a 24h ceiling for a control plane that stopped answering,
//    not a hold timeout), and the scope it raises is {tool,cmd,env} with no
//    mode at all (internal/egress/proxy/local_routes.go). PENDING alone IS the
//    hold here, so nothing client-side bounds it the way HOLD_TIMEOUT_MS
//    bounds the egress case — the row's own server-side expiry ends it.
//  - egress wait_for_review — the proxy carries the mode in the approval's
//    requested_scope so the UI can flag it, but PENDING alone doesn't mean
//    "still holding the sandbox": the connection fails closed at
//    HOLD_TIMEOUT_MS while the approval row itself stays PENDING for up to 24h
//    afterward (W20-hold-fsm-2).
//
// Exported because the run cockpit's command bar and the board's card state
// the same fact ("N waiting · sandbox held"). Two copies of this test would be
// two truths that can disagree, and the disagreement would read as "nothing is
// holding the sandbox" while the sandbox is, in fact, held.
export function isHeld(a: ApprovalRequest): boolean {
  if (a.kind === "tool_call") return true;
  if (String((a.requested_scope?.mode as string) ?? "") !== "wait_for_review") return false;
  const requestedAt = Date.parse(a.requested_at);
  if (Number.isNaN(requestedAt)) return true; // unparseable timestamp — fail toward showing the hold
  return Date.now() - requestedAt < HOLD_TIMEOUT_MS;
}

// rowLabel is the row's identity line: the host for an egress hold, the tool
// and its command for a tool hold. The full string is the Mono title; clip()
// keeps the strip one line tall (the Approvals screen renders the whole scope).
function rowLabel(a: ApprovalRequest): string {
  if (a.kind !== "tool_call") return String((a.requested_scope?.host as string) ?? "unknown host");
  const parts = [a.requested_scope?.tool, a.requested_scope?.cmd]
    .map((v) => (typeof v === "string" ? v.trim() : ""))
    .filter(Boolean);
  return parts.join(": ") || "a tool call";
}

// D7 — the agent CLI's own known telemetry endpoints (DATA-FLOW.md:27,
// DEMO-SCRIPT.md:701). Client-side recognition only — this is identification
// for the tag, not a policy; the row still decides through the normal
// Approve/Deny controls.
const KNOWN_TELEMETRY_HOSTS = ["http-intake.logs.us5.datadoghq.com"];

function isKnownTelemetryHost(host: string): boolean {
  return KNOWN_TELEMETRY_HOSTS.some((known) =>
    known.startsWith("*.") ? host === known.slice(2) || host.endsWith(known.slice(1)) : host === known,
  );
}

const STRIP_LABEL_MAX = 72;

function clip(s: string): string {
  return s.length > STRIP_LABEL_MAX ? s.slice(0, STRIP_LABEL_MAX - 1) + "…" : s;
}

// M3: two rows, then the overflow link — the strip sits UNDER the terminal it
// belongs to and must never grow past it. A fifth held row pushing the session
// off the screen is the strip defeating the surface it exists to serve.
const STRIP_ROWS = 2;

// The strip's own precedence, read from the ONE rank table the board's glyph
// reads (run-state-glyph.tsx): a held request — the sandbox is parked on this
// decision right now — is "permission" and outranks a passive pending, which is
// "monitoring". It used to be implicit in whatever order the API returned,
// which was harmless while every row was visible and is not harmless now that
// only two are: the held row must never be the one behind "Show all".
function rowRank(a: ApprovalRequest): number {
  return attentionRank(isHeld(a) ? "permission" : "monitoring");
}

export function LiveApprovals({
  runId,
  reasonApprove = "approved live",
  reasonDeny = "rejected live",
  idleHint = "Watching for off-policy egress and held tool calls — anything this run tries that isn't allow-listed surfaces here to approve or deny, live.",
  // Whether THIS run resolves to an onboarded workspace — Always persists
  // there, so it's greyed out without one. Default false so a caller that
  // forgets to pass it shows the option disabled rather than offering a click
  // the server 400s. Every production mount passes it explicitly.
  hasWorkspace = false,
}: {
  runId: string;
  reasonApprove?: string;
  reasonDeny?: string;
  idleHint?: string;
  hasWorkspace?: boolean;
}) {
  // Decides here go straight to the API with no ReasonDialog stop, so this is
  // the one gate for all three mount sites (run detail, demo screen, the
  // record-mode verify panel) — see approvals.tsx's PendingCard for the
  // queue-screen equivalent.
  //
  // useSecurityOperator, not useOperator (0.7 §B): authorizeMemberDecision
  // early-returns for isSecurityOperator (approvals.go:392) and
  // decision_scope=always is its lockstep pair (approvals.go:604), so the
  // security tier decides any kind, on any run, at any scope.
  const securityOperator = useSecurityOperator();
  const [pending, setPending] = React.useState<ApprovalRequest[]>([]);
  const [busy, setBusy] = React.useState<string | null>(null);
  // A misclick on Deny (any scope) can't silently poison a host the operator
  // meant to keep — a confirm stop, mirroring DeleteConfirmDialog's pattern.
  // Approve's DEFAULT scope stays a single click: it is the low-risk,
  // correctable direction (a wrongly-approved request is still visible in the
  // audit log) — but Approve's non-default scopes (picked from the caret)
  // decide immediately too, same reasoning, one click either way.
  const [denyTarget, setDenyTarget] = React.useState<DenyTarget | null>(null);
  // Rulebook §7: `disabled` already follows `busy` immediately per-row below
  // (`busy === a.id`) — only the spinner needs deferring, so only showSpinner
  // is read from the hook.
  const { showSpinner: decidingSpinner } = useDeferredBusy(busy !== null);
  // W20-hold-fsm-5: a failed poll used to fall silently back to the last
  // snapshot, which for an empty snapshot renders the SAME affirmative
  // "Watching for…" idle text as a confirmed-empty poll — the one state where
  // silence is indistinguishable from "nothing pending." Tracked separately
  // from `pending` so a transient failure doesn't clear rows already shown.
  const [pollError, setPollError] = React.useState(false);
  // The strip caps at STRIP_ROWS; this is the operator asking for the rest.
  const [showAll, setShowAll] = React.useState(false);

  const refresh = React.useCallback(async () => {
    try {
      const all = await api.listApprovals("PENDING");
      // egress_domain + tool_call: both park the run live, so both belong on the
      // surface the human is already watching. tool_call gained its first
      // producer with the toolgate (a `hold` run raises one per mutating tool
      // call) and this strip is that run's primary decision surface.
      //
      // credential is admitted ONLY for an api_key mint: unlike a dispatch-time
      // grant, the secrets demos' authorized-not-issued mint is raised from
      // INSIDE the sandbox, mid-run, over the broker's own mint route — the
      // same decision-visible-where-it-happens principle the egress/tool_call
      // rows already follow, so it belongs on this strip too. git_pat/ssh_key
      // credential approvals stay OUT: those mounts also occur on run-detail and
      // twice in record-pane, where a mid-run mint's blast-radius banner (the
      // git_pat "the agent's process can read this" nuance, the resident-key
      // TTL) is the whole point of deciding it — a bare one-click host row
      // there would be a regression. Those route via the run detail's "Waiting
      // for your confirmation" banner to the Approvals screen's kind-aware
      // card instead.
      setPending(
        all.filter(
          (a) =>
            a.run_id === runId &&
            (a.kind === "egress_domain" ||
              a.kind === "tool_call" ||
              (a.kind === "credential" && credentialKind(a.requested_scope) === "api_key")),
        ),
      );
      setPollError(false);
    } catch {
      // transient poll error — keep the last pending snapshot, but flag it so
      // the idle render doesn't claim a confirmed-empty poll.
      setPollError(true);
    }
  }, [runId]);

  // usePoll drives the BACKGROUND refreshes only; the initial load is ours.
  React.useEffect(() => {
    void refresh();
  }, [refresh]);
  usePoll(refresh, POLL_MS, false);

  // scope defaults to "run" — the bare-click behavior, unchanged from before
  // this feature. decisionArgs omits the trailing options arg entirely for
  // "run" so this stays a literal 2-argument api call for the default path
  // (vitest's toHaveBeenCalledWith matches arity exactly).
  //
  // A tool_call row can only ever take that default path: decide rule 4
  // (approvals.go) 400s ANY explicit decision_scope on a non-egress approval,
  // so the caret is not rendered for those rows and nothing can hand one in.
  const decide = async (a: ApprovalRequest, approve: boolean, scope: ApprovalScope = "run", until?: string) => {
    setBusy(a.id);
    try {
      const args = decisionArgs(scope, until);
      if (approve) {
        // The SERVER writes the durable echo: approving a verify session's
        // egress request lands the host as an egress: requirement row in that
        // workspace's contract at the decide() chokepoint. No client-side
        // second write — the old onApproveHost callback wrote the legacy
        // approved_egress lane on top of it, two truths behind one click.
        await api.approve(a.id, reasonApprove, ...args);
      } else {
        await api.deny(a.id, reasonDeny, ...args);
      }
      await refresh();
    } catch (e) {
      toast.error(approve ? "Approve failed" : "Deny failed", {
        description: getErrorMessage(e),
      });
    } finally {
      setBusy(null);
    }
  };

  const confirmDeny = async () => {
    if (!denyTarget) return;
    await decide(denyTarget.request, false, denyTarget.scope, denyTarget.until);
    setDenyTarget(null);
  };

  if (pending.length === 0) {
    if (pollError) {
      return (
        <p className="text-meta text-warning" data-testid="live-approvals-poll-error">
          Couldn't check for pending approvals — retrying…
        </p>
      );
    }
    return (
      <p className="text-meta text-muted-foreground" data-testid="live-approvals-idle">
        {idleHint}
      </p>
    );
  }

  // Highest attention first (rowRank), then the API's own order — Array.sort is
  // stable, so equally-ranked rows keep the order the server sent.
  const ordered = [...pending].sort((a, b) => rowRank(a) - rowRank(b));
  const shown = showAll ? ordered : ordered.slice(0, STRIP_ROWS);
  const hiddenCount = ordered.length - shown.length;

  const anyHeld = pending.some(isHeld);
  // Never claim "egress" over a set that holds a tool call, and never claim
  // "held" over one nothing is waiting on.
  const heading = anyHeld
    ? "Sandbox is waiting — approve to let it through"
    : pending.every((a) => a.kind === "egress_domain")
      ? "Approval needed — off-policy egress"
      : "Approval needed — the agent is waiting on you";

  return (
    <div
      className="space-y-1.5 rounded-lg border border-warning/40 bg-warning-subtle p-2.5"
      data-testid="live-approvals"
    >
      <div className="flex items-center gap-2">
        <SectionLabel>{heading}</SectionLabel>
        {/* Named once for the whole panel, not per row — but only when it's
            actually true: F-12 fixed the row-level gate below to follow
            canDecideApproval (a member CAN decide their own run's
            egress_domain rows), so this hint must not claim "admin only"
            over a strip the viewer can, in fact, act on. */}
        {!securityOperator && pending.some((a) => !canDecideApproval(securityOperator, a.kind)) && (
          <OperatorOnlyHint />
        )}
      </div>
      {shown.map((a) => {
        const label = rowLabel(a);
        const held = isHeld(a);
        // Only egress decisions carry a scope (decide rule 4) — see decide().
        const scoped = a.kind === "egress_domain";
        const telemetry = scoped && isKnownTelemetryHost(label);
        return (
          <div key={a.id} className="flex items-center gap-2" data-testid="live-approval-row">
            {held ? (
              <Clock className="size-3.5 shrink-0 text-warning" aria-label="request held live" />
            ) : (
              <ShieldAlert className="size-3.5 shrink-0 text-warning" />
            )}
            <Mono className="flex-1 text-foreground" title={label}>
              {clip(label)}
            </Mono>
            {telemetry && (
              <Chip tone="neutral" className="h-5 shrink-0 px-1.5" title={TELEMETRY_TAG.title}>
                {TELEMETRY_TAG.label}
              </Chip>
            )}
            {held && <span className="text-meta uppercase tracking-wide text-warning">waiting</span>}
            {/* Split button: the bare click is "This run" (scope's default,
                unchanged from before this feature existed) — the caret opens
                the other three. Console page confirm buttons stay named
                exactly "Approve"/"Deny" for e2e; this caret's own accessible
                name must never contain "approve" (an unanchored /approve/i
                query in the suite would then match two buttons). */}
            {/* F-12: align with server truth (authorizeMemberDecision,
                internal/api/approvals.go) instead of a blanket !operator —
                canDecideApproval mirrors decide() exactly: a member may
                decide an egress_domain approval (this strip only ever shows
                rows on runs the viewer owns — see canDecideApproval's own
                doc), credential and tool_call stay admin-only regardless. */}
            <Button
              size="sm"
              variant="outline"
              className="h-7 rounded-r-none border-r-0"
              disabled={!canDecideApproval(securityOperator, a.kind) || busy === a.id}
              onClick={() => decide(a, true)}
            >
              {busy === a.id && decidingSpinner ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : (
                <Check className="size-3.5" />
              )}{" "}
              Approve
            </Button>
            {scoped && (
              <ScopeMenu
                verb="approve"
                hasWorkspace={hasWorkspace}
                securityOperator={securityOperator}
                triggerClassName="rounded-l-none"
                onPick={(scope, until) => decide(a, true, scope, until)}
              />
            )}
            <Button
              size="sm"
              variant="outline"
              className="h-7 ml-1 rounded-r-none border-r-0"
              disabled={!canDecideApproval(securityOperator, a.kind) || busy === a.id}
              onClick={() => setDenyTarget({ request: a, scope: "run" })}
            >
              {busy === a.id && decidingSpinner ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : (
                <X className="size-3.5" />
              )}{" "}
              Deny
            </Button>
            {scoped && (
              <ScopeMenu
                verb="deny"
                hasWorkspace={hasWorkspace}
                securityOperator={securityOperator}
                triggerClassName="rounded-l-none"
                onPick={(scope, until) => setDenyTarget({ request: a, scope, until })}
              />
            )}
          </div>
        );
      })}
      {/* The board's own disclosure string, so a strip and a card cannot
          disagree about what "there are more" sounds like. --info, not the teal
          accent (CONSOLE-RULES §2: teal means "press this to act", and showing
          two more rows is not a decision). */}
      {(hiddenCount > 0 || showAll) && (
        <button
          type="button"
          onClick={() => setShowAll((v) => !v)}
          className="text-meta font-semibold text-info hover:underline"
          data-testid="live-approvals-show-all"
        >
          {showAll ? "Show fewer" : `Show all ${ordered.length}`}
        </button>
      )}
      <AlertDialog open={!!denyTarget} onOpenChange={(o) => !o && setDenyTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Deny <Mono>{denyTarget ? clip(rowLabel(denyTarget.request)) : ""}</Mono>?
            </AlertDialogTitle>
            {/* Scope-dependent (Phase 0 §3) — replaces a sentence that was
                false for two of the four scopes: `once` re-raises on the next
                attempt, and `always` outlives "the session" AND can be undone
                in the workspace's egress settings, neither of which "no undo
                and no re-raise" allowed for. */}
            <AlertDialogDescription>
              {/* denyDialogCopy is egress-shaped ("blocks this host…") and every
                  one of its four scopes is false for a tool hold, which has no
                  host and no scope at all: a deny refuses THIS call, the agent
                  is told no, and the run carries on. It is equally false for a
                  credential mint refusal — denying withholds the CREDENTIAL,
                  not the host; the allowlist (unaffected by this decision)
                  still governs what the run can reach. Two strings, two uses —
                  they stay here rather than in copy.ts, same as the tool_call
                  one below. */}
              {denyTarget?.request.kind === "tool_call"
                ? "Denying refuses this tool call. The agent is told no and carries on — its next one asks again."
                : denyTarget?.request.kind === "credential"
                  ? "Denying refuses this credential mint — the agent gets no key. The allowlist still governs what the run can reach; this decision doesn't touch it."
                  : denyDialogCopy(denyTarget?.scope ?? "run", { until: denyTarget?.until })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                confirmDeny();
              }}
              className="bg-danger text-danger-foreground hover:bg-danger/90"
            >
              <X className="size-3.5" /> Deny
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

const SCOPE_ITEM_CLS =
  "flex w-full flex-col items-start gap-0 rounded-sm px-2 py-1.5 text-left text-sm text-foreground hover:bg-accent hover:text-accent-foreground disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent";

// ScopeMenu — the split button's caret content (Approve and Deny each mount
// their own instance). Renders every option as a plain <button>, not
// DropdownMenuItem: Always must carry a REAL disabled attribute when gated
// (see reason-dialog.tsx's identical note) — a div-based menu item can never
// have one, only aria-disabled, and Playwright will happily "click" that.
function ScopeMenu({
  verb,
  hasWorkspace,
  securityOperator,
  triggerClassName,
  onPick,
}: {
  verb: "approve" | "deny";
  hasWorkspace: boolean;
  // decision_scope=always is gated by isSecurityOperator (approvals.go:604),
  // NOT isOperator — named for the predicate it actually carries.
  securityOperator: boolean;
  triggerClassName?: string;
  onPick: (scope: ApprovalScope, until?: string) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [untilMode, setUntilMode] = React.useState(false);
  // The custom datetime-local's picked value, held here until the operator
  // explicitly confirms it — see the "Use this time" button below. A preset
  // click is already one deliberate, atomic action and commits straight
  // through pick(); this input is not, since the browser fires onChange on
  // every intermediate valid value while the operator is still scrubbing
  // hour/minute/AM-PM, and pick() commits the decision (or, for deny, opens
  // the confirm dialog pre-loaded with whatever value fired last).
  const [customUntil, setCustomUntil] = React.useState<string | null>(null);
  const labels = verb === "approve" ? APPROVAL_SCOPE_LABEL : DENY_SCOPE_LABEL;
  const hints = verb === "approve" ? APPROVAL_SCOPE_HINT : DENY_SCOPE_HINT;
  const alwaysDisabled = !hasWorkspace || !securityOperator;
  const alwaysReason = !securityOperator ? OPERATOR_ONLY_REASON : ALWAYS_NEEDS_WORKSPACE;

  const pick = (scope: ApprovalScope, until?: string) => {
    setOpen(false);
    setUntilMode(false);
    setCustomUntil(null);
    onPick(scope, until);
  };

  return (
    <DropdownMenu
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (!o) {
          setUntilMode(false);
          setCustomUntil(null);
        }
      }}
    >
      <DropdownMenuTrigger asChild>
        {/* Deliberately not named "…approve…"/"…deny…" — see the row comment
            above this component's two mount sites. */}
        <Button size="sm" variant="outline" className={cn("h-7 w-6 p-0", triggerClassName)} aria-label="More options">
          <ChevronDown className="size-3.5" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-64 space-y-0.5 p-1">
        {!untilMode ? (
          <>
            {(["once", "run"] as const).map((s) => (
              <button key={s} type="button" onClick={() => pick(s)} className={SCOPE_ITEM_CLS}>
                <span className="font-medium">{labels[s]}</span>
                <span className="text-meta text-muted-foreground">{hints[s]}</span>
              </button>
            ))}
            <button type="button" onClick={() => setUntilMode(true)} className={SCOPE_ITEM_CLS}>
              <span className="font-medium">{labels.until}</span>
              <span className="text-meta text-muted-foreground">{hints.until}</span>
            </button>
            <button
              type="button"
              disabled={alwaysDisabled}
              onClick={() => pick("always")}
              className={SCOPE_ITEM_CLS}
            >
              <span className="font-medium">{labels.always}</span>
              <span className="text-meta text-muted-foreground">
                {alwaysDisabled ? alwaysReason : hints.always}
              </span>
            </button>
          </>
        ) : (
          <>
            <button
              type="button"
              onClick={() => {
                setUntilMode(false);
                setCustomUntil(null);
              }}
              className="px-2 py-1 text-meta text-muted-foreground hover:text-foreground"
            >
              ← Back
            </button>
            {UNTIL_PRESETS.map((p) => (
              <button
                key={p.label}
                type="button"
                onClick={() => pick("until", new Date(Date.now() + p.ms).toISOString())}
                className={SCOPE_ITEM_CLS}
              >
                {p.label}
              </button>
            ))}
            <div className="space-y-1 px-2 py-1.5">
              {/* Sets state only — same as reason-dialog.tsx's identical input.
                  Does NOT pick(): the browser fires onChange on every
                  intermediate valid value while the operator is still
                  scrubbing hour/minute/AM-PM, and pick() commits (or, for
                  deny, opens the confirm dialog) — committing mid-scrub is
                  the bug this split guards against. */}
              <input
                type="datetime-local"
                aria-label="Pick a time"
                onChange={(e) => setCustomUntil(e.target.value ? new Date(e.target.value).toISOString() : null)}
                className="h-7 w-full rounded-md border border-border bg-background px-1.5 text-xs text-foreground"
              />
              <Button
                size="sm"
                variant="outline"
                className="h-6 w-full text-meta"
                disabled={!customUntil}
                onClick={() => pick("until", customUntil ?? undefined)}
              >
                Use this time
              </Button>
            </div>
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
