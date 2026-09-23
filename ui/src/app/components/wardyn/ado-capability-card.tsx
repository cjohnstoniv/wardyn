/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AdoCapabilityCard — plan slice S10: the console's rendering of an Azure
// DevOps capability escalation (a tool_call raised by
// internal/api/injection_ado_capability.go's answerADOCapability) and of the
// Entra-consent-missing chain it can raise (a credential_reauth, raiseADOConsent).
// Shared by live-approvals.tsx (the run cockpit's live strip, mounted at FOUR
// sites — see that file's own comment), screens/approvals.tsx (the standalone
// queue) and run-detail.tsx's Approvals tab — docs/design/ado-entra-prompt.md
// §8 names the first two; the Approvals tab is round-2's own addition, so the
// same explicit-scope rule applies there too. §7.6 is this card's frozen copy
// source, plus §10 — both live in ado-entra-copy.ts's ADO namespace (N3,
// round 3: that file absorbed this card's own former copy subset,
// ado-capability-copy.ts, once #415 — which owns ado-entra-copy.ts — merged).
//
// WHAT THIS CARD DOES NOT DRAW, AND WHY: the frozen mock (State 6) also draws
// an above-ceiling card, an always-refused card, a governance-refused card and
// an unclassified-write card. None of those states can ever reach this
// component: answerADOCapability's Grantable()/ceiling checks and
// raiseADOCapability's always_deny check all answer the SANDBOX synchronously
// (a 403), before any ApprovalRequest row is created — see that file's own
// comments. Drawing UI for an ApprovalRequest state the server cannot produce
// would be dead code, not a card — so this component simply never reads
// ADO.REQ_CEILING_*/REQ_ALWAYS_DENIED_*/REQ_GOVERNANCE_*/REQ_UNCLASSIFIED_*,
// though ado-entra-copy.ts itself carries the full §7 canon those keys are
// part of (unlike this card's now-deleted former subset module).
//
// THE SCOPE ASYMMETRY THIS CARD MUST RESPECT: adoDecisionRule
// (injection_ado_capability.go) treats a BODYLESS decide (no decision_scope
// field at all) as "once" for an Azure DevOps escalation — unlike every other
// approval kind, where the server's own Normalize() defaults a bodyless
// decide to "run". This card's default selection is "This run" (per the
// mock), so it can NEVER use lib/types/approvals.ts's decisionArgs() helper
// (which omits the field for "run", relying on the OTHER default) — see
// adoDecisionArgs below, which always sends an explicit decision_scope.
//
// WHO MAY DECIDE (round-2 fix): canDecideAdoCapability(securityOperator,
// isRunOwner) — the run's OWNER or a security operator, mirroring
// authorizeMemberDecision/ownsRunOrAdmin exactly. securityOperator already
// INCLUDES a plain admin: isSecurityOperator (internal/api/http.go) is true
// for oidc.RoleAdmin as well as oidc.RoleSecurityAdmin ("a super admin is a
// security admin too — the tiers overlap on this surface"). There is no
// third "admin but not security operator" tier this card needs to reason
// about; every caller of this component passes useSecurityOperator()'s
// answer, never useOperator()'s.
import * as React from "react";
import { Link } from "react-router-dom";
import { Check, CheckCircle2, ChevronDown, Loader2, X } from "lucide-react";
import type { AgentRun, ApprovalRequest, DecisionOptions } from "../../lib/types";
import {
  ADO_HOLD_WINDOW_MS,
  canDecideAdoCapability,
  isAdoConsentRequest,
  type AdoCapabilityScope,
  type AdoConsentScope,
} from "../../lib/types/approvals";
import { isTerminalRunState } from "../../lib/types";
import { ADO } from "../../lib/ado-entra-copy";
import { Button } from "../ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { Chip } from "./primitives";
import { Mono } from "./code-block";
import { cn } from "../ui/utils";

// §7.4's card-facing capability labels — only the capabilities that are
// Grantable() (adoscope.Capability), since only those can ever reach a
// PENDING row. A capability outside this map (security_admin,
// serviceendpoint_admin, build_admin, packaging_write, project_admin — all
// grantable server-side, none yet given a §7.4 canon label) falls back to
// its raw wire name in mono, never a guessed label.
const CAP_LABEL: Record<string, string> = {
  read: ADO.CAP_READ,
  code_write: ADO.CAP_CODE_WRITE,
  pr: ADO.CAP_PR,
  policy_admin: ADO.CAP_POLICY_ADMIN,
  policy_bypass: ADO.CAP_POLICY_BYPASS,
  repo_admin: ADO.CAP_REPO_ADMIN,
  build_execute: ADO.CAP_BUILD_EXECUTE,
  work_write: ADO.CAP_WORK_WRITE,
  wiki_write: ADO.CAP_WIKI_WRITE,
};

// §10.1's per-capability noun for the consequence sentences' {thing} — the
// mock (State 5) picks a different word per capability ("push"/"change"/
// "action"), never the generic "request" round 1 used. A capability outside
// this map falls back to "request", same reasoning as CAP_LABEL's fallback.
const CAP_THING: Record<string, string> = {
  read: ADO.CAP_THING_READ,
  code_write: ADO.CAP_THING_CODE_WRITE,
  pr: ADO.CAP_THING_PR,
  policy_admin: ADO.CAP_THING_POLICY_ADMIN,
  policy_bypass: ADO.CAP_THING_POLICY_BYPASS,
  repo_admin: ADO.CAP_THING_REPO_ADMIN,
  build_execute: ADO.CAP_THING_BUILD_EXECUTE,
  work_write: ADO.CAP_THING_WORK_WRITE,
  wiki_write: ADO.CAP_THING_WIKI_WRITE,
};

// Q3 (mock §9): teal Approve / plain Deny on an ordinary card; on the two
// capabilities that move something PAST a policy or CHANGE the policy
// itself, nothing is teal and Deny is destructive. "Same rule, one fewer
// button" — the ruling was given against the three-button drawing and
// re-read onto the shipped Approve+caret+Deny control (Q2).
const DESTRUCTIVE_CAPABILITIES = new Set(["policy_bypass", "policy_admin"]);

function capabilityHeading(capability: string): React.ReactNode {
  const label = CAP_LABEL[capability];
  return label ?? <Mono>{capability}</Mono>;
}

function capabilityThing(capability: string): string {
  return CAP_THING[capability] ?? ADO.REQ_FIELD_REQUEST.toLowerCase();
}

// adoDecisionArgs ALWAYS sends an explicit decision_scope — see this file's
// own top comment for why decisionArgs()'s omit-for-"run" convention would
// silently decide "once" here instead.
function adoDecisionArgs(scope: "once" | "run"): [DecisionOptions] {
  return [{ scope }];
}

const SCOPE_LABEL: Record<"once" | "run", string> = { once: "Once", run: "This run" };

// A request is still HELD (the proxy is parked on it) for up to four
// minutes from when it was raised — the same ceiling REQ_HELD's own text
// names. No expiry timestamp reaches the client (unlike egress's
// HOLD_TIMEOUT_MS), so this is derived from requested_at, exactly the way
// isHeld (lib/types/approvals.ts) derives egress's own HOLD_TIMEOUT_MS check.
// ADO_HOLD_WINDOW_MS is the SAME 240_000 isHeld itself now reads for this
// exact row shape (#725/F1) — imported rather than redeclared so the two
// never drift apart again.
function stillHeld(requestedAt: string): boolean {
  const at = Date.parse(requestedAt);
  return Number.isNaN(at) ? true : Date.now() - at < ADO_HOLD_WINDOW_MS; // unparseable — fail toward showing the hold
}

// boldFirstWord — round-2 fix N2: REQ_APPROVING_ONCE/RUN and REQ_DENYING
// already OPEN with "Approving"/"Denying" (the mock, index.html:512, bolds
// exactly that first word and renders the rest plain, ONE sentence). Round 1
// additionally hand-wrote a bold "Approving"/"Denying" label in front of the
// canon string, so the rendered text doubled: "Approving Approving lets…".
// This renders the canon string's own first word in bold instead of
// prepending a second one.
function boldFirstWord(sentence: string): React.ReactNode {
  const idx = sentence.indexOf(" ");
  if (idx === -1) return sentence;
  return (
    <>
      <b className="font-semibold text-foreground">{sentence.slice(0, idx)}</b>
      {sentence.slice(idx)}
    </>
  );
}

// relativeAbsolute — REQ_SOURCE's {ts} placeholder wants a short clock
// reading (the mock draws "14:02:11"), not a relative phrase;
// relativeTime (lib/format) is built for the latter. A locale time string is
// the honest middle ground available from an ISO timestamp alone.
function relativeAbsolute(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

// The run this card needs to know about — created_by (who it acts as / owns
// the decision) and state (has it ended). Every real caller passes a real
// AgentRun/RunDetail; this Pick is what the two shapes have in common.
export type AdoCardRun = Pick<AgentRun, "created_by" | "state">;

export function AdoCapabilityCard({
  item,
  securityOperator,
  viewerPrincipal,
  run,
  ownershipScopedList = false,
  busy,
  onApprove,
  onDeny,
}: {
  item: ApprovalRequest;
  securityOperator: boolean;
  // The signed-in viewer's own subject — decides who the consent door is for
  // (adoConsentScopeBody.Owner) and, alongside run.created_by, who "yours"
  // means for an escalation.
  viewerPrincipal?: string;
  // undefined = still loading (render a neutral skeleton, never "Not yours"
  // — round-2 fix F10); null = the fetch failed or the run is unreadable by
  // this caller (render a generic error, never "Not yours"); an object =
  // loaded. A securityOperator viewer never needs this at all (their
  // decidability doesn't depend on ownership), so loading/null never gates
  // THEM — only a non-security viewer waits on it. See ownershipScopedList
  // for the other way run can legitimately be null/undefined.
  run: AdoCardRun | null | undefined;
  // Round-2 fix N4: true when the CALLER's own list fetch is already
  // ownership-gated server-side — live-approvals.tsx's listApprovals(state,
  // run_id) is exactly this for every one of its four mount sites (a member
  // only ever gets back rows for runs they own; an admin gets everyone's),
  // so a row appearing in that list at all already proves this viewer may
  // decide it, with or without a `run` object in hand. When true, `run` being
  // null/undefined never shows a loading skeleton or a run-fetch error and
  // never blocks decidability — it only means "Acts as" has nothing to show.
  // Defaults false: screens/approvals.tsx and run-detail.tsx's Approvals tab
  // both list org-wide/other-owned rows too (an admin's view), so a row
  // being visible there does NOT by itself prove ownership — those callers
  // keep the real loading/error/ownership gates.
  ownershipScopedList?: boolean;
  // "approve" | "deny" while THAT decision is in flight, else null (#458):
  // the old single boolean correctly disabled BOTH buttons but ALSO spun
  // BOTH of them, so a reader couldn't tell which action their click had
  // actually started. Both are still disabled whenever busy !== null; now
  // only the pressed one shows the spinner.
  busy: "approve" | "deny" | null;
  onApprove: (opts: [DecisionOptions]) => void;
  onDeny: (opts: [DecisionOptions]) => void;
}) {
  const [scope, setScope] = React.useState<"once" | "run">("run");
  const [menuOpen, setMenuOpen] = React.useState(false);
  // The hold window's own timer (#458): stillHeld(requested_at) was read only
  // at render, so REQ_HELD survived past its own 4-minute window until some
  // UNRELATED re-render happened to catch it up. `held` is seeded from the
  // same check and then flipped false by a timer sized to the remaining
  // window, so the card corrects itself with no other trigger needed.
  const [held, setHeld] = React.useState(() => stillHeld(item.requested_at));
  React.useEffect(() => {
    if (isAdoConsentRequest(item)) return; // this card's own timer, not the consent card's
    setHeld(stillHeld(item.requested_at));
    const at = Date.parse(item.requested_at);
    if (Number.isNaN(at)) return;
    const msLeft = ADO_HOLD_WINDOW_MS - (Date.now() - at);
    if (msLeft <= 0) return; // already past the window — no timer to set
    const timer = setTimeout(() => setHeld(false), msLeft);
    return () => clearTimeout(timer);
  }, [item]);

  // The consent card FIRST, before any run-loading/error/ended gate below:
  // its decidability is "is the viewer the row's own owner" (a plain string
  // compare against the wire scope's own `owner` field), never run
  // ownership — it needs no `run` at all, so it must never be blocked behind
  // a run fetch the escalation card below actually depends on.
  if (isAdoConsentRequest(item)) {
    return <AdoConsentCard item={item} viewerPrincipal={viewerPrincipal} />;
  }

  const trusted = ownershipScopedList && (run === null || run === undefined);

  if (!securityOperator && !trusted && run === undefined) {
    return (
      <div className="rounded-xl border border-border bg-card p-4" data-testid="ado-capability-card">
        <span className="block h-5 w-48 animate-pulse rounded bg-muted" aria-label="loading" />
      </div>
    );
  }
  if (!securityOperator && !trusted && run === null) {
    return (
      <div className="rounded-xl border border-border bg-card p-4" data-testid="ado-capability-card">
        <p className="text-sm text-muted-foreground">{ADO.REQ_RUN_UNAVAILABLE}</p>
      </div>
    );
  }

  const runEnded = !!run && isTerminalRunState(run.state);
  if (runEnded) {
    return (
      <div className="rounded-xl border border-border bg-card p-4" data-testid="ado-capability-card">
        <div className="flex items-center gap-2">
          <Chip tone="neutral">{ADO.LIST_ENDED_CHIP}</Chip>
        </div>
        <p className="mt-2 text-sm text-muted-foreground">{ADO.LIST_ENDED_BODY}</p>
      </div>
    );
  }

  const scopeData = item.requested_scope as unknown as AdoCapabilityScope;
  const heading = capabilityHeading(scopeData.capability);
  const thing = capabilityThing(scopeData.capability);
  const destructive = DESTRUCTIVE_CAPABILITIES.has(scopeData.capability);
  const runOwner = run?.created_by;
  const isOwner = !!runOwner && runOwner === viewerPrincipal;
  const decidable = trusted || canDecideAdoCapability(securityOperator, isOwner);
  const where = scopeData.repo ? `${scopeData.org}/${scopeData.repo}` : scopeData.org;
  const source = ADO.REQ_SOURCE(relativeAbsolute(item.requested_at));

  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="ado-capability-card">
      <div className="flex flex-wrap items-center gap-2">
        <Chip tone={decidable ? "warning" : "neutral"}>
          {decidable ? ADO.REQ_WAITING : ADO.REQ_NOT_YOURS_CHIP}
        </Chip>
        <div className="min-w-0">
          <h4 className="text-sm font-semibold text-foreground">{heading}</h4>
          <span className="text-meta text-muted-foreground">{source}</span>
        </div>
      </div>

      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{ADO.REQ_FIELD_REPOSITORY}</dt>
        <dd className="font-mono text-xs text-foreground">{where || "—"}</dd>
        {scopeData.ref_class === "protected" && (
          // No ref NAME reaches the client (the canonical scope carries only
          // ref_class) — see this file's own top comment. The capability
          // heading above already says "past a branch policy" for this
          // case; this row states the fact §2.3 asks for without inventing
          // a ref.
          <>
            <dt className="text-muted-foreground">{ADO.REQ_FIELD_REF_CLASS}</dt>
            <dd className="text-xs text-foreground">{ADO.REQ_REF_CLASS_PROTECTED}</dd>
          </>
        )}
        <dt className="text-muted-foreground">{ADO.REQ_FIELD_COMMAND}</dt>
        <dd className="font-mono text-xs text-foreground">{scopeData.cmd}</dd>
        {runOwner && (
          <>
            <dt className="text-muted-foreground">{ADO.REQ_FIELD_ACTS_AS}</dt>
            <dd className="text-xs text-foreground">
              {runOwner}
              <span className="mt-0.5 block text-meta text-muted-foreground">{ADO.REQ_ACTS_AS_HINT(runOwner)}</span>
            </dd>
          </>
        )}
      </dl>

      {decidable ? (
        <div className="mt-3 border-t border-border/60 pt-3">
          <div className="flex flex-wrap items-center gap-2">
            <Button
              size="sm"
              variant={destructive ? "outline" : "info"}
              className="rounded-r-none"
              disabled={busy !== null}
              onClick={() => onApprove(adoDecisionArgs(scope))}
            >
              {busy === "approve" ? <Loader2 className="size-3.5 animate-spin" /> : <Check className="size-3.5" />} Approve
            </Button>
            <AdoScopeMenu scope={scope} thing={thing} onPick={(s) => setScope(s)} open={menuOpen} onOpenChange={setMenuOpen} />
            {/* The scope readout sits directly after Approve's own group,
                BEFORE Deny (F7/round-2): a deny sticks for the rest of the
                run regardless of which scope was staged (#414), so a reader
                must never see "Scope: Once" positioned as if it governed
                Deny too. */}
            <span className="text-meta text-muted-foreground">{ADO.REQ_SCOPE_READOUT(SCOPE_LABEL[scope])}</span>
            <Button
              size="sm"
              variant={destructive ? "destructive" : "outline"}
              className="ml-1"
              disabled={busy !== null}
              onClick={() => onDeny(adoDecisionArgs(scope))}
            >
              {busy === "deny" ? <Loader2 className="size-3.5 animate-spin" /> : <X className="size-3.5" />} Deny
            </Button>
          </div>
          <p className="mt-2.5 text-meta text-muted-foreground">
            {boldFirstWord(scope === "once" ? ADO.REQ_APPROVING_ONCE(thing) : ADO.REQ_APPROVING_RUN(thing))}
          </p>
          <p className="mt-0.5 text-meta text-muted-foreground">{boldFirstWord(ADO.REQ_DENYING(thing))}</p>
          <p className="mt-2.5 text-meta text-muted-foreground">
            {held ? ADO.REQ_HELD(thing) : ADO.REQ_HELD_EXPIRED(thing)}
          </p>
        </div>
      ) : (
        <p className="mt-3 border-t border-border/60 pt-3 text-sm text-muted-foreground">
          {/* `||`, not `??`: an empty owner names nobody exactly as much as a
              missing one does — both get the fallback (#458). */}
          {ADO.REQ_NOT_YOURS_BODY(runOwner || ADO.REQ_OWNER_FALLBACK)}
        </p>
      )}
    </div>
  );
}

// outline-none + the three focus-visible: classes are CONSOLE-RULES.md §34's
// standard ring (button.tsx#buttonVariants carries the same three) — these
// buttons went keyboard-reachable under a Popover (review finding F3) and,
// without this, showed the browser's default outline instead (review
// finding 5).
const SCOPE_ITEM_CLS =
  "flex w-full flex-col items-start gap-0 rounded-sm px-2 py-1.5 text-left text-sm text-foreground outline-none hover:bg-accent hover:text-accent-foreground focus-visible:border-ring focus-visible:ring-ring focus-visible:ring-[3px] disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent";

// AdoScopeMenu — the caret half of the mock's "shipped Approve-and-scope
// control" (Q2). Unlike live-approvals.tsx's ScopeMenu (egress's four live
// scopes, each committing immediately on pick), this one STAGES a choice
// between exactly the two scopes adoDecisionRule accepts — Approve/Deny
// commit whichever is currently staged, matching the mock's "the pair on the
// right is what they read with Once selected instead of This run".
//
// Held in a Popover, not a DropdownMenu (review finding F3): a DropdownMenu's
// roving-tabindex focus manager only covers registered DropdownMenuItems and
// swallows Tab, so the plain <button>s below (needed for the always/until
// rows' real `disabled`, same reason as live-approvals.tsx's ScopeMenu) were
// unreachable by keyboard. Popover's content does not manage focus that way,
// so Tab walks the buttons in plain DOM order and Enter/Space pick one.
function AdoScopeMenu({
  scope,
  thing,
  onPick,
  open,
  onOpenChange,
}: {
  scope: "once" | "run";
  // F1 (round 2) — the hints under Once/This run use the SAME per-capability
  // noun the consequence sentences do ("push", not "request"); the mock
  // (State 5, Q2's "drawn open" variant) shows it there too.
  thing: string;
  onPick: (s: "once" | "run") => void;
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  return (
    <Popover open={open} onOpenChange={onOpenChange}>
      <PopoverTrigger asChild>
        <Button size="sm" variant="outline" className="h-8 w-6 rounded-l-none p-0" aria-label="More options">
          <ChevronDown className="size-3.5" />
        </Button>
      </PopoverTrigger>
      {/* review finding 6: PopoverContent renders role="dialog" with no
          accessible name by default — label it to match the trigger it
          opens from. */}
      <PopoverContent align="start" className="w-64 space-y-0.5 p-1" aria-label="More options">
        {(["once", "run"] as const).map((s) => (
          <button
            key={s}
            type="button"
            onClick={() => {
              onPick(s);
              onOpenChange(false);
            }}
            className={cn(SCOPE_ITEM_CLS, scope === s && "bg-accent")}
          >
            <span className="inline-flex items-center gap-1 font-medium">
              {scope === s && <CheckCircle2 className="size-3 text-info" />}
              {SCOPE_LABEL[s]}
            </span>
            <span className="text-meta text-muted-foreground">
              {s === "once" ? ADO.REQ_SCOPE_ONCE_HINT(thing) : ADO.REQ_SCOPE_RUN_HINT(thing)}
            </span>
          </button>
        ))}
        <div className={SCOPE_ITEM_CLS} aria-disabled>
          <span className="font-medium text-muted-foreground">{ADO.SCOPE_UNTIL_LABEL}</span>
          <span className="text-meta text-muted-foreground">{ADO.REQ_SCOPE_UNTIL_REFUSED}</span>
        </div>
        <div className={SCOPE_ITEM_CLS} aria-disabled>
          <span className="font-medium text-muted-foreground">{ADO.SCOPE_ALWAYS_LABEL}</span>
          <span className="text-meta text-muted-foreground">{ADO.REQ_SCOPE_ALWAYS_REFUSED}</span>
        </div>
      </PopoverContent>
    </Popover>
  );
}

// AdoConsentCard — the Entra-consent-missing chain (raiseADOConsent's
// credential_reauth row). NOT the mock's exact drawing: the mock pairs this
// state with the capability escalation it blocked ("Change a branch policy
// / … you allowed this at 14:09:52 · still held"), but adoConsentScopeBody
// (the real wire shape) carries no capability name and no reference to the
// escalation that triggered it — only lane, mechanism, owner, provider_id
// and the raw Entra scope strings still needed. Pairing the two rows client-
// side would require guessing which escalation a consent row answers for,
// which is exactly the kind of unverifiable claim AGENTS.md and §5's
// never-claim rule refuse. This card is therefore its own, honest, simpler
// state, titled by REQ_CONSENT_HEADING (§10.2) rather than a guessed
// capability name — flagged in the S10 handoff. REQ_CONSENT_BODY (round 2)
// is a plain string, not a function: it no longer claims a capability name
// (or "you allowed it") the wire scope cannot back on every path — see its
// own doc note in ado-entra-copy.ts.
//
// REQ_CONSENT_CTA links to /settings#azure-devops, not a route of its own:
// F9 (S10 round 3) pointed it at the same Azure DevOps connection surface
// #415 built (screens/settings/ado-connection.tsx's AdoConnectionCard,
// mounted unconditionally on the one settings-screen.tsx page — no tabs, no
// query param). #458 added the `#azure-devops` anchor and matched the
// label to that card's own CTA ("Connect Azure DevOps", CONNECT_ADO) — a
// bare `<Link to="/settings">` landed at the top of a five-card page with
// no way to find the one card this door is actually about, and the two CTAs
// named the same act two different ways.
function AdoConsentCard({
  item,
  viewerPrincipal,
}: {
  item: ApprovalRequest & { requested_scope: AdoConsentScope };
  viewerPrincipal?: string;
}) {
  const owner = item.requested_scope.owner;
  const isOwner = !!viewerPrincipal && viewerPrincipal === owner;
  // A mid-run sign-in request (holdForADOSignIn) is the same card with its
  // own copy: nothing about it is a consent, and "four minutes" (§7.6's
  // REQ_REAUTH_BODY) is not this hold's bound, so §10.6 carries its body.
  const signIn = item.requested_scope.mechanism === "entra_signin";
  const copy = signIn
    ? { chip: ADO.REQ_REAUTH_CHIP, heading: ADO.REQ_REAUTH_TITLE, body: ADO.REQ_REAUTH_HELD_BODY,
        other: ADO.REQ_REAUTH_OTHER_BODY, cta: ADO.CONNECT_ADO }
    : { chip: ADO.REQ_CONSENT_CHIP, heading: ADO.REQ_CONSENT_HEADING, body: ADO.REQ_CONSENT_BODY,
        other: ADO.REQ_CONSENT_OTHER_BODY, cta: ADO.REQ_CONSENT_CTA };
  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="ado-consent-card">
      <div className="flex flex-wrap items-center gap-2">
        <Chip tone="warning">{isOwner ? copy.chip : ADO.REQ_WAITING_OTHER(owner)}</Chip>
        <div className="min-w-0">
          <h4 className="text-sm font-semibold text-foreground">{copy.heading}</h4>
          <span className="text-meta text-muted-foreground">{ADO.REQ_SOURCE(relativeAbsolute(item.requested_at))}</span>
        </div>
      </div>
      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{ADO.REQ_FIELD_ACTS_AS}</dt>
        <dd className="text-xs text-foreground">
          {owner}
          <span className="mt-0.5 block text-meta text-muted-foreground">{ADO.REQ_ACTS_AS_HINT(owner)}</span>
        </dd>
      </dl>
      <p className="mt-3 border-t border-border/60 pt-3 text-sm text-muted-foreground">
        {isOwner ? copy.body : copy.other(owner)}
      </p>
      {isOwner && (
        <div className="mt-3">
          <Button asChild size="sm" variant="info">
            <Link to="/settings#azure-devops">{copy.cta}</Link>
          </Button>
        </div>
      )}
    </div>
  );
}
