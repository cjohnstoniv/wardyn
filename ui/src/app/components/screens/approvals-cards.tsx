/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link } from "react-router-dom";
import { Check, ChevronRight, Code2, ShieldCheck, X } from "lucide-react";
import {
  canDecideApproval,
  isAdoCapabilityRequest,
  isAdoConsentRequest,
  isTerminalRunState,
  type AgentRun,
  type ApprovalRequest,
  type DecisionOptions,
} from "../../lib/types";
import { capabilityAllowed } from "../../lib/capabilities";
import { DENIED } from "../../lib/permissions-copy";
import type { MeCapabilities } from "../../lib/types";
import { relativeTime } from "../../lib/format";
import { Button } from "../ui/button";
import { ApprovalKindChip, ApprovalStateBadge, Chip } from "../wardyn/primitives";
import { RunContextRow } from "../wardyn/run-context-row";
import { JsonBlock } from "../wardyn/code-block";
import { AdoCapabilityCard } from "../wardyn/ado-capability-card";
import { isPushContentRequest, PushContentCard } from "../wardyn/push-content-card";
import { REAUTH_ROW, reauthAudience } from "../wardyn/model-access-copy";
import { useClaimModelAccessDoor, useModelAccessDoor, useShellSetupStatus } from "../wardyn/model-access-context";
import { resolveDoor } from "../../lib/model-access";
import { usePrincipal, useSecurityOperator } from "../wardyn/operator-context";
import { OpenInUserView, runPath, useConsoleMode } from "../wardyn/console-view";
import { PUSH } from "../wardyn/copy/push";
import { APPROVAL, APPROVAL_BANNER_LABEL, SECURITY_ONLY_REASON, approvalScopeBadge } from "../wardyn/copy";
import { KIND_ICON, capabilityLabel, deriveTitle, deriveBanner, str } from "./approvals";

export function PendingCard({
  item,
  caps,
  onAct,
  onAdoDecide,
  onPushDecide,
}: {
  item: ApprovalRequest;
  // The viewer's own capability set, or null when the question doesn't apply
  // (an admin, or an answer still in flight).
  caps: MeCapabilities | null;
  onAct: (action: "approve" | "deny") => void;
  // S10 — the Azure DevOps capability card's own decide path; see
  // decideAdoDirect's doc above for why it bypasses onAct/ReasonDialog.
  onAdoDecide: (id: string, approve: boolean, opts: [DecisionOptions]) => Promise<void>;
  // #181 — the push_content card's own decide path; see decidePushDirect's
  // doc above for why it bypasses onAct/ReasonDialog too, minus the scope.
  onPushDecide: (id: string, approve: boolean) => Promise<void>;
}) {
  const scope = item.requested_scope ?? {};
  const KindIcon = KIND_ICON[item.kind] ?? ShieldCheck;
  const cap = capabilityLabel(item.kind, scope);
  // ONE ownership rule, shared with the cockpit row (live-approvals.tsx's
  // ReauthRow): the door renders only for the viewer whose own sign-in the
  // server would accept for THIS row. door.operator / door.principal rather
  // than useOperator() / usePrincipal(), because those answer the fail-open
  // default in exactly the window the answer is audience-dependent and the
  // audience is unknown.
  const door = useModelAccessDoor();
  const view = useConsoleMode();
  const reauth = reauthAudience(item, { operator: door.operator, principal: door.principal, view });
  const { status } = useShellSetupStatus();
  const reauthProvider = reauth.provider
    ? (status?.model_providers?.find((p) => p.id === reauth.provider)?.name || reauth.providerName || reauth.provider)
    : "";
  // M-7 (admin-member-modes-design.md §4.6, §6): the admin queue carries no
  // personal reauth door either, even on the admin's own row — same rule as
  // the cockpit's ReauthRow, with a switch link back to it there instead. The
  // shared lane (an admin-mode control until MP-4b) is unaffected.
  const reauthCanAct = view === "admin" && !reauth.shared ? false : reauth.canAct;
  const reauthOwnRow = view === "admin" && !reauth.shared && reauth.mine;
  // A hold whose provider this person has no door for any more (removed, or no
  // agent of theirs uses it) gets its hint alone, never a button that opens
  // nothing — the failure block's rule (ProviderDoor).
  const reauthDoor = reauthCanAct && (!reauth.provider || !!resolveDoor(status, { provider: reauth.provider }, "user"));
  const banner = deriveBanner(item.kind, scope, reauthCanAct === reauth.canAct ? reauth : { ...reauth, canAct: reauthCanAct });
  // Deciding an egress_domain approval on an owned run is a MEMBER act (B3,
  // decide() in approvals.go); credential and tool_call stay admin-only
  // regardless of ownership — see canDecideApproval's doc for why. This list
  // is already scoped to rows the caller owns (or every row, for an admin),
  // so ownership itself needs no re-check here.
  // useSecurityOperator, not useOperator (0.7 §B): authorizeUserDecision
  // early-returns for isSecurityOperator (approvals.go:392) — the security
  // tier decides ANY kind on ANY run, org-wide. Deciding a verdict is that
  // tier's whole purpose; the caps fetch above stays on useOperator because
  // capAllowed does (capabilities.go).
  const securityOperator = useSecurityOperator();
  const kindDecidable = canDecideApproval(securityOperator, item.kind);
  // The `egress_host` capability bounds which hosts a member may DECIDE on —
  // the authorizeUserDecision seam (approvals.go). Advisory here: the server
  // refuses it anyway, this just says so before the click instead of after.
  // Guarded on the security tier in the same ORDER the server checks: its
  // early return happens BEFORE this capability leg, so a security admin is
  // never bounded by it.
  const host = item.kind === "egress_domain" ? str(scope, "host", "domain") : undefined;
  const hostUngranted = !securityOperator && !!host && !capabilityAllowed(caps, "egress_host", host);
  // B4 — the run this approval gates, from the context row below's OWN fetch.
  // A run that has ended cancels its PENDING approvals (types.ApprovalCancelled;
  // the finalizeRunTail / handleKillRun cascade), so Approve and Deny here
  // answer a question nobody is waiting on: the sandbox is torn down, the
  // identity revoked, and the server refuses the decision. A dead control on a
  // governance surface is worse than no control — it reads as the system still
  // being in your hands.
  //
  // undefined (still fetching) and null (gone, or unreadable by this caller)
  // both leave the controls exactly as they were: the screen withdraws them on
  // a KNOWN terminal state, never on a guess, which is the same direction the
  // rest of this card errs in.
  const [run, setRun] = React.useState<AgentRun | null | undefined>(undefined);
  const runEnded = !!run && isTerminalRunState(run.state);
  const canDecide = kindDecidable && !hostUngranted && !runEnded;

  // S10 — an Azure DevOps escalation (or the Entra-consent chain it can
  // raise) gets ITS OWN card, not this generic one: it needs fields
  // (repository, ref class, the composed command) and a scope control (Once
  // / This run only, never until/always) that deriveBanner/canDecideApproval
  // above don't model — see AdoCapabilityCard's own doc comment. `run` is
  // handed through AS-IS (undefined/null/loaded) — the card itself renders
  // the loading/error states now (round-2 fix F10), rather than this caller
  // collapsing "still loading" into "not yours".
  // usePrincipal(), not door.principal: the consent door's ownership question
  // here is "is the viewer the row's OWNER subject" (adoConsentScopeBody.Owner
  // on the wire), a plain identity comparison, not the model-access door's
  // own audience predicate.
  const principal = usePrincipal();
  // "approve" | "deny" while that decision is in flight, else null (#458) —
  // see AdoCapabilityCard's own `busy` doc for why a single boolean isn't
  // enough to spin only the pressed button. The push card follows the same rule.
  const [adoBusy, setAdoBusy] = React.useState<"approve" | "deny" | null>(null);
  const [pushBusy, setPushBusy] = React.useState<"approve" | "deny" | null>(null);
  // N1 (round 2): PendingCard only ever receives PENDING rows today
  // (pendingItems is fetched via api.listApprovals("PENDING")), but the
  // state check is explicit here too — defense-in-depth against this
  // component ever being reused for a broader list, and the single rule
  // "AdoCapabilityCard only ever renders a PENDING row" stays true
  // everywhere it mounts, not just by construction at the one caller that
  // happens to pre-filter today.
  if ((isAdoCapabilityRequest(item) || isAdoConsentRequest(item)) && item.state === "PENDING") {
    return (
      <div className="space-y-2">
        <RunContextRow runId={item.run_id} onRun={setRun} />
        <AdoCapabilityCard
          item={item}
          securityOperator={securityOperator}
          viewerPrincipal={principal}
          run={run}
          busy={adoBusy}
          onApprove={async (opts) => {
            setAdoBusy("approve");
            await onAdoDecide(item.id, true, opts);
            setAdoBusy(null);
          }}
          onDeny={async (opts) => {
            setAdoBusy("deny");
            await onAdoDecide(item.id, false, opts);
            setAdoBusy(null);
          }}
        />
      </div>
    );
  }

  // #181 — a held push gets its OWN card too: it needs fields (repository,
  // branch, the paths under review) this generic card has no slot for, and
  // an admin-only decide with no scope menu (see push-content-card.tsx's own
  // doc). Same "PENDING only" defense-in-depth as the ADO branch above.
  if (isPushContentRequest(item) && item.state === "PENDING") {
    return (
      <div className="space-y-2">
        <RunContextRow runId={item.run_id} onRun={setRun} />
        <PushContentCard
          item={item}
          securityOperator={securityOperator}
          run={run}
          busy={pushBusy}
          onApprove={async () => {
            setPushBusy("approve");
            await onPushDecide(item.id, true);
            setPushBusy(null);
          }}
          onDeny={async () => {
            setPushBusy("deny");
            await onPushDecide(item.id, false);
            setPushBusy(null);
          }}
        />
      </div>
    );
  }

  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4">
      <div className="flex flex-wrap items-center gap-2">
        <ApprovalKindChip kind={item.kind} />
        <span className="inline-flex items-center gap-1.5 text-sm font-semibold text-foreground">
          <KindIcon className="size-3.5 text-muted-foreground" />
          {deriveTitle(item.kind, scope)}
        </span>
        {cap && <Chip tone="warning">{cap}</Chip>}
        <span className="ml-auto">
          <ApprovalStateBadge state={item.state} />
        </span>
      </div>

      {/* #543: which AWS provider the hold is for — a sign-in to another
          one cannot clear it. */}
      {item.kind === "credential_reauth" && reauthProvider && (
        <p className="mt-1 text-xs text-muted-foreground">{REAUTH_ROW.PROVIDER(reauthProvider)}</p>
      )}

      <RunContextRow runId={item.run_id} onRun={setRun} />

      {/* Blast-radius banner (D1) — derived from the real scope above. */}
      <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
        <p className="text-foreground">
          <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span> {banner.what}
        </p>
        {/* No blast radius for a re-auth request (UX ruling B3, general S4):
            the row grants nothing — it asks its owner to sign in again to a
            credential the deployment already configured — and "Blast radius:"
            over the row's own both-branches hint claimed a capability that does
            not exist. The hint still renders; only the label that made it a
            capability claim is gone. */}
        <p className="text-muted-foreground">
          {item.kind !== "credential_reauth" && (
            <span className="font-semibold text-foreground/80">{APPROVAL_BANNER_LABEL.blast}</span>
          )}{" "}
          {banner.blast}
        </p>
        {item.minted_jti && (
          <p className="pt-0.5 font-mono text-xs text-muted-foreground">minted jti: {item.minted_jti}</p>
        )}
      </div>

      <details className="group mt-2.5">
        <summary className="inline-flex cursor-pointer list-none items-center gap-1 text-xs text-muted-foreground hover:text-foreground [&::-webkit-details-marker]:hidden">
          <ChevronRight className="size-3.5 transition-transform group-open:rotate-90" />
          <Code2 className="size-3.5" /> View requested scope
        </summary>
        <JsonBlock value={scope} className="mt-2" />
      </details>

      {/* P0.3 (R3-F001/F108/F145) — an egress_domain approval is host-wide, and
          has been all along: the proxy strips any port before it keys the
          decision (approvalHostKey). The UI must say so rather than rely on
          it quietly, because "allow api.example.com" reads as the one
          connection in front of you. The scope words above are canon and
          not paraphrased, so this is its own line. */}
      {item.kind === "egress_domain" && (
        <p className="mt-2.5 max-w-[72ch] text-xs text-muted-foreground">{APPROVAL.HOST_WIDE_NOTE}</p>
      )}

      {/* B4 — the decision pair is GONE, not merely disabled, once the run has
          ended: there is nothing left to decide and no state a retry could
          reach. A disabled Approve would still read as "this is yours to
          answer". */}
      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border/60 pt-3">
        {runEnded ? (
          <p className="max-w-[72ch] text-xs text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
        ) : (
          item.kind === "credential_reauth" ? (
          /* A door, not a decision (UX round B3). The pair is removed, not
             disabled: a disabled Approve reads as "an admin can do this", and
             no tier can — the server answers 409 to either verb. The one
             control opens the same dialog every other sign-in surface opens. */
          reauthDoor ? (
            <ReauthAction provider={reauth.provider} />
          ) : reauthOwnRow ? (
            <OpenInUserView />
          ) : null
        ) : (
          <>
            <Button size="sm" variant="info" onClick={() => onAct("approve")} disabled={!canDecide}>
              <Check className="size-4" /> Approve
            </Button>
            <Button variant="outline" size="sm" onClick={() => onAct("deny")} disabled={!canDecide}>
              <X className="size-4" /> Deny
            </Button>
            {/* ui-member-cluster review finding: this fallback is reached
                only via kindDecidable = canDecideApproval(securityOperator,
                kind) — a SECURITY-tier gate, not an operator-tier one (a
                security admin decides any kind). OPERATOR_ONLY_REASON named
                a role the caller may not need. */}
            {!canDecide && (
              <Chip tone="neutral">{hostUngranted ? DENIED.APPROVE_CHIP : SECURITY_ONLY_REASON}</Chip>
            )}
          </>
          )
        )}
        <span className="ml-auto text-xs text-muted-foreground" title={item.requested_at}>
          requested {relativeTime(item.requested_at)}
        </span>
      </div>

      {/* Say what the denial costs and the way out, at the moment it happens —
          and that a grant would still not hand this member the Always scope. */}
      {hostUngranted && host && (
        <>
          <p className="mt-2.5 max-w-[72ch] text-xs text-muted-foreground">{DENIED.APPROVE_BODY(host)}</p>
          <p className="mt-1.5 max-w-[72ch] text-xs text-muted-foreground">{DENIED.ALWAYS_STILL_ADMIN}</p>
        </>
      )}
    </div>
  );
}

export function DecidedRow({ item }: { item: ApprovalRequest }) {
  const scope = item.requested_scope ?? {};
  // A CANCELLED row carries decided_by="system" (the terminal-run cascade wrote
  // it), so it reads with the same "by …" prefix an expiry-with-a-writer does —
  // honest: something, not someone, ended it.
  const who = item.decided_by || (item.state === "EXPIRED" ? "unanswered" : "system");
  const when = relativeTime(item.decided_at ?? item.requested_at);
  const meta = item.state === "EXPIRED" && !item.decided_by ? `${who} · ${when}` : `by ${who} · ${when}`;
  // egress_domain only, and only when a decision actually recorded one (Phase
  // 0 §6) — undefined for EXPIRED (ExpireStale deliberately writes no scope:
  // an expiry is a sweep nobody decided) and for every other kind.
  const scopeBadge = approvalScopeBadge(item);
  const view = useConsoleMode();

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-t border-border px-4 py-3 first:border-t-0">
      <ApprovalKindChip kind={item.kind} />
      <span className="min-w-0 flex-1 truncate text-sm text-foreground">{deriveTitle(item.kind, scope)}</span>
      <Link
        to={runPath(view, item.run_id)}
        className="font-mono text-xs text-muted-foreground hover:text-foreground"
        title={`Open run ${item.run_id}`}
      >
        {item.run_id}
      </Link>
      <ApprovalStateBadge state={item.state} />
      {scopeBadge && <Chip tone="neutral">{scopeBadge}</Chip>}
      <span className="whitespace-nowrap text-xs text-muted-foreground">{meta}</span>
      {/* B4 — CANCELLED is the one archived state whose badge does not explain
          itself: "by system · 3h ago" beside a word that could mean anybody
          withdrew it. The row says which of the three things happened, because
          none of them did: nothing was approved, nothing was denied, and the
          run ended first. Full width (basis-full) so it reads as a sentence
          under the row rather than a fourth column. */}
      {item.state === "CANCELLED" && (
        <p className="basis-full text-xs text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
      )}
      {/* #181 — a held push that timed out: refused, and honest about why
          (nobody answered), same "basis-full sentence under the row" shape
          the CANCELLED case above uses. */}
      {item.kind === "push_content" && item.state === "EXPIRED" && (
        <p className="basis-full text-xs text-muted-foreground">{PUSH.TIMEOUT_BODY}</p>
      )}
    </div>
  );
}


/**
 * ReauthAction — the /approvals card's control for a mid-run AWS sign-in
 * request: ONE button, opening the same dialog every other sign-in surface
 * opens, and claiming the door while it renders so the global strip drops its
 * own button on this page.
 *
 * No Approve, no Deny, no disabled pair: the server answers 409 to either verb
 * (decide()'s rule 3b), and a disabled control would name a role that could
 * decide it — none can.
 *
 * Rendered ONLY for a viewer reauthAudience grades as able to clear the row;
 * everyone else reads the card's hint, which names whose sign-in is awaited,
 * and gets no control at all.
 */
function ReauthAction({ provider }: { provider: string }) {
  const door = useModelAccessDoor();
  useClaimModelAccessDoor(true);
  // The hold's OWN provider's door (#543): the claude-code default may be
  // another AWS provider, whose sign-in cannot clear it.
  const open = () => door.openDoor(provider ? { for: { provider } } : undefined);
  return (
    <Button size="sm" variant="info" aria-label={REAUTH_ROW.ariaLabel} onClick={open}>
      {REAUTH_ROW.action}
    </Button>
  );
}
