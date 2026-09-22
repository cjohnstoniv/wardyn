/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AdoCapabilityCard — plan slice S10: the console's rendering of an Azure
// DevOps capability escalation (a tool_call raised by
// internal/api/injection_ado_capability.go's answerADOCapability) and of the
// Entra-consent-missing chain it can raise (a credential_reauth, raiseADOConsent).
// Shared by live-approvals.tsx (the run cockpit's live strip) and
// screens/approvals.tsx (the standalone queue) — docs/design/ado-entra-
// prompt.md §8 names this split; §7.6 is this card's frozen copy source.
//
// WHAT THIS CARD DOES NOT DRAW, AND WHY: the frozen mock (State 6) also draws
// an above-ceiling card, an always-refused card, a governance-refused card and
// an unclassified-write card. None of those states can ever reach this
// component: answerADOCapability's Grantable()/ceiling checks and
// raiseADOCapability's always_deny check all answer the SANDBOX synchronously
// (a 403), before any ApprovalRequest row is created — see that file's own
// comments. Drawing UI for an ApprovalRequest state the server cannot produce
// would be dead code, not a card. ado-capability-copy.ts's own doc comment
// lists every canon key this file does not carry for the same reason.
//
// THE SCOPE ASYMMETRY THIS CARD MUST RESPECT: adoDecisionRule
// (injection_ado_capability.go) treats a BODYLESS decide (no decision_scope
// field at all) as "once" for an Azure DevOps escalation — unlike every other
// approval kind, where the server's own Normalize() defaults a bodyless
// decide to "run". This card's default selection is "This run" (per the
// mock), so it can NEVER use lib/types/approvals.ts's decisionArgs() helper
// (which omits the field for "run", relying on the OTHER default) — see
// adoDecisionArgs below, which always sends an explicit decision_scope.
import * as React from "react";
import { Link } from "react-router-dom";
import { Check, CheckCircle2, ChevronDown, Loader2, X } from "lucide-react";
import type { ApprovalRequest, DecisionOptions } from "../../lib/types";
import { canDecideAdoCapability, isAdoConsentRequest, type AdoCapabilityScope, type AdoConsentScope } from "../../lib/types/approvals";
import { ADO_CAPABILITY } from "../../lib/ado-capability-copy";
import { APPROVAL } from "./copy";
import { Button } from "../ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuTrigger } from "../ui/dropdown-menu";
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
  read: ADO_CAPABILITY.CAP_READ,
  code_write: ADO_CAPABILITY.CAP_CODE_WRITE,
  pr: ADO_CAPABILITY.CAP_PR,
  policy_admin: ADO_CAPABILITY.CAP_POLICY_ADMIN,
  policy_bypass: ADO_CAPABILITY.CAP_POLICY_BYPASS,
  repo_admin: ADO_CAPABILITY.CAP_REPO_ADMIN,
  build_execute: ADO_CAPABILITY.CAP_BUILD_EXECUTE,
  work_write: ADO_CAPABILITY.CAP_WORK_WRITE,
  wiki_write: ADO_CAPABILITY.CAP_WIKI_WRITE,
};

function capabilityHeading(capability: string): React.ReactNode {
  const label = CAP_LABEL[capability];
  return label ?? <Mono>{capability}</Mono>;
}

// adoDecisionArgs ALWAYS sends an explicit decision_scope — see this file's
// own top comment for why decisionArgs()'s omit-for-"run" convention would
// silently decide "once" here instead.
function adoDecisionArgs(scope: "once" | "run"): [DecisionOptions] {
  return [{ scope }];
}

const SCOPE_LABEL: Record<"once" | "run", string> = { once: "Once", run: "This run" };

export function AdoCapabilityCard({
  item,
  operator,
  viewerPrincipal,
  runOwner,
  runEnded = false,
  busy,
  onApprove,
  onDeny,
}: {
  item: ApprovalRequest;
  // useSecurityOperator(), NOT useOperator() — ownsRunOrAdmin
  // (internal/api/helpers.go) bypasses ownership for isSecurityOperator
  // only, the same tier authorizeMemberDecision already special-cases before
  // it ever calls ownsRunOrAdmin. A plain (non-security) admin gets no
  // special bypass here — only the run's own owner or a security operator.
  operator: boolean;
  // The signed-in viewer's own subject — decides who the consent door is for
  // (adoConsentScopeBody.Owner) and, alongside runOwner, who "yours" means.
  viewerPrincipal?: string;
  // run.created_by — undefined while the run fetch is still in flight, which
  // this card treats exactly like "not yet known" (no decision either way).
  runOwner?: string;
  runEnded?: boolean;
  busy: boolean;
  onApprove: (opts: [DecisionOptions]) => void;
  onDeny: (opts: [DecisionOptions]) => void;
}) {
  const [scope, setScope] = React.useState<"once" | "run">("run");
  const [menuOpen, setMenuOpen] = React.useState(false);

  if (runEnded) {
    return (
      <div className="rounded-xl border border-border bg-card p-4" data-testid="ado-capability-card">
        <p className="text-sm text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
      </div>
    );
  }

  if (isAdoConsentRequest(item)) {
    return <AdoConsentCard item={item} viewerPrincipal={viewerPrincipal} />;
  }

  const scopeData = item.requested_scope as unknown as AdoCapabilityScope;
  const heading = capabilityHeading(scopeData.capability);
  const isOwner = !!runOwner && runOwner === viewerPrincipal;
  const decidable = canDecideAdoCapability(operator, isOwner);
  const where = scopeData.repo ? `${scopeData.org}/${scopeData.repo}` : scopeData.org;
  const source = ADO_CAPABILITY.REQ_SOURCE(relativeAbsolute(item.requested_at));

  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="ado-capability-card">
      <div className="flex flex-wrap items-center gap-2">
        <Chip tone="warning">{decidable ? ADO_CAPABILITY.REQ_WAITING : ADO_CAPABILITY.REQ_NOT_YOURS_CHIP}</Chip>
        <div className="min-w-0">
          <h4 className="text-sm font-semibold text-foreground">{heading}</h4>
          <span className="text-meta text-muted-foreground">{source}</span>
        </div>
      </div>

      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{ADO_CAPABILITY.REQ_FIELD_REPOSITORY}</dt>
        <dd className="font-mono text-xs text-foreground">{where || "—"}</dd>
        {scopeData.ref_class === "protected" && (
          <>
            {/* No ref NAME reaches the client (the canonical scope carries
                only ref_class) — see this file's own top comment. The
                capability heading above already says "past a branch policy"
                for this case; this row states the fact the task asks for
                without inventing a ref. */}
            <dt className="text-muted-foreground">Ref class</dt>
            <dd className="text-xs text-foreground">Protected by a branch policy</dd>
          </>
        )}
        <dt className="text-muted-foreground">{ADO_CAPABILITY.REQ_FIELD_COMMAND}</dt>
        <dd className="font-mono text-xs text-foreground">{scopeData.cmd}</dd>
      </dl>

      {decidable ? (
        <div className="mt-3 border-t border-border/60 pt-3">
          <div className="flex flex-wrap items-center gap-2">
            <Button
              size="sm"
              variant="info"
              className="rounded-r-none"
              disabled={busy}
              onClick={() => onApprove(adoDecisionArgs(scope))}
            >
              {busy ? <Loader2 className="size-3.5 animate-spin" /> : <Check className="size-3.5" />} Approve
            </Button>
            <AdoScopeMenu scope={scope} onPick={(s) => setScope(s)} open={menuOpen} onOpenChange={setMenuOpen} />
            <Button
              size="sm"
              variant="outline"
              className="ml-1"
              disabled={busy}
              onClick={() => onDeny(adoDecisionArgs(scope))}
            >
              {busy ? <Loader2 className="size-3.5 animate-spin" /> : <X className="size-3.5" />} Deny
            </Button>
            <span className="text-meta text-muted-foreground">{ADO_CAPABILITY.REQ_SCOPE_READOUT(SCOPE_LABEL[scope])}</span>
          </div>
          <p className="mt-2.5 text-meta text-muted-foreground">
            <b className="font-semibold text-foreground">Approving</b>{" "}
            {scope === "once"
              ? ADO_CAPABILITY.REQ_APPROVING_ONCE(ADO_CAPABILITY.REQ_FIELD_REQUEST.toLowerCase())
              : ADO_CAPABILITY.REQ_APPROVING_RUN(ADO_CAPABILITY.REQ_FIELD_REQUEST.toLowerCase())}
          </p>
          <p className="mt-0.5 text-meta text-muted-foreground">
            <b className="font-semibold text-foreground">Denying</b>{" "}
            {ADO_CAPABILITY.REQ_DENYING(ADO_CAPABILITY.REQ_FIELD_REQUEST.toLowerCase())}
          </p>
          <p className="mt-2.5 text-meta text-muted-foreground">
            {ADO_CAPABILITY.REQ_HELD(ADO_CAPABILITY.REQ_FIELD_REQUEST.toLowerCase())}
          </p>
        </div>
      ) : (
        <p className="mt-3 border-t border-border/60 pt-3 text-sm text-muted-foreground">
          {ADO_CAPABILITY.REQ_NOT_YOURS_BODY(runOwner ?? "the run's owner")}
        </p>
      )}
    </div>
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

const SCOPE_ITEM_CLS =
  "flex w-full flex-col items-start gap-0 rounded-sm px-2 py-1.5 text-left text-sm text-foreground hover:bg-accent hover:text-accent-foreground disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent";

// AdoScopeMenu — the caret half of the mock's "shipped Approve-and-scope
// control" (Q2). Unlike live-approvals.tsx's ScopeMenu (egress's four live
// scopes, each committing immediately on pick), this one STAGES a choice
// between exactly the two scopes adoDecisionRule accepts — Approve/Deny
// commit whichever is currently staged, matching the mock's "the pair on the
// right is what they read with Once selected instead of This run".
function AdoScopeMenu({
  scope,
  onPick,
  open,
  onOpenChange,
}: {
  scope: "once" | "run";
  onPick: (s: "once" | "run") => void;
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  return (
    <DropdownMenu open={open} onOpenChange={onOpenChange}>
      <DropdownMenuTrigger asChild>
        <Button size="sm" variant="outline" className="h-8 w-6 rounded-l-none p-0" aria-label="More options">
          <ChevronDown className="size-3.5" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-64 space-y-0.5 p-1">
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
              {s === "once"
                ? ADO_CAPABILITY.REQ_SCOPE_ONCE_HINT(ADO_CAPABILITY.REQ_FIELD_REQUEST.toLowerCase())
                : ADO_CAPABILITY.REQ_SCOPE_RUN_HINT(ADO_CAPABILITY.REQ_FIELD_REQUEST.toLowerCase())}
            </span>
          </button>
        ))}
        <div className={SCOPE_ITEM_CLS} aria-disabled>
          <span className="font-medium text-muted-foreground">Until…</span>
          <span className="text-meta text-muted-foreground">{ADO_CAPABILITY.REQ_SCOPE_UNTIL_REFUSED}</span>
        </div>
        <div className={SCOPE_ITEM_CLS} aria-disabled>
          <span className="font-medium text-muted-foreground">Always</span>
          <span className="text-meta text-muted-foreground">{ADO_CAPABILITY.REQ_SCOPE_ALWAYS_REFUSED}</span>
        </div>
      </DropdownMenuContent>
    </DropdownMenu>
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
// state — flagged in the S10 handoff.
//
// "Allow and continue" (REQ_CONSENT_CTA) links to Settings rather than
// starting a redemption: the actual Azure DevOps sign-in surface
// (screens/settings/ado-connection.tsx, §8 "Member sign-in") is a separate,
// not-yet-built slice on this branch.
function AdoConsentCard({
  item,
  viewerPrincipal,
}: {
  item: ApprovalRequest & { requested_scope: AdoConsentScope };
  viewerPrincipal?: string;
}) {
  const owner = item.requested_scope.owner;
  const isOwner = !!viewerPrincipal && viewerPrincipal === owner;
  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="ado-consent-card">
      <div className="flex flex-wrap items-center gap-2">
        <Chip tone="warning">{isOwner ? ADO_CAPABILITY.REQ_CONSENT_CHIP : ADO_CAPABILITY.REQ_WAITING_OTHER(owner)}</Chip>
        <div className="min-w-0">
          <h4 className="text-sm font-semibold text-foreground">Azure DevOps needs more access</h4>
          <span className="text-meta text-muted-foreground">{ADO_CAPABILITY.REQ_SOURCE(relativeAbsolute(item.requested_at))}</span>
        </div>
      </div>
      <p className="mt-3 border-t border-border/60 pt-3 text-sm text-muted-foreground">
        {isOwner
          ? "You'll need to allow Wardyn a bit more Azure DevOps access before this request can go through. Reconnecting asks Microsoft for it — you'll see a consent screen, and nothing else changes. The run's request stays held meanwhile."
          : `Only ${owner} can give Microsoft the extra permission this run needs — the run acts as them, and consent is theirs to give.`}
      </p>
      {isOwner && (
        <div className="mt-3">
          <Button asChild size="sm" variant="info">
            <Link to="/settings">{ADO_CAPABILITY.REQ_CONSENT_CTA}</Link>
          </Button>
        </div>
      )}
    </div>
  );
}
