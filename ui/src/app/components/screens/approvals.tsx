/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  Archive,
  Check,
  ChevronRight,
  Code2,
  Globe,
  KeyRound,
  ShieldCheck,
  SquareTerminal,
  X,
} from "lucide-react";
import { toast } from "sonner";
import {
  canDecideApproval,
  decisionArgs,
  isTerminalRunState,
  type AgentRun,
  type ApprovalKind,
  type ApprovalRequest,
  type ApprovalScope,
} from "../../lib/types";
import { approvals as api } from "../../lib/api/approvals";
import { anyCapabilityEnforced, capabilityAllowed, useMyCapabilities } from "../../lib/capabilities";
import { DENIED } from "../../lib/permissions-copy";
import type { MeCapabilities } from "../../lib/types";
import { LIST_LIMIT } from "../../lib/api/core";
import { usePoll } from "../../lib/use-poll";
import { getErrorMessage, relativeTime } from "../../lib/format";
import { Button } from "../ui/button";
import { Tabs, TabsList, TabsTrigger } from "../ui/tabs";
import { ApprovalKindChip, ApprovalStateBadge, Chip } from "../wardyn/primitives";
import { RunContextRow } from "../wardyn/run-context-row";
import { JsonBlock } from "../wardyn/code-block";
import { EmptyState, ErrorState, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { ReasonDialog } from "../wardyn/reason-dialog";
import { REAUTH_ROW, REAUTH_TITLE, reauthAudience, reauthRowHint, type ReauthAudience } from "../wardyn/model-access-copy";
import { useClaimModelAccessDoor, useModelAccessDoor } from "../wardyn/model-access-context";
import { useOperator, useRole, useSecurityOperator } from "../wardyn/operator-context";
import {
  APPROVAL,
  APPROVAL_BANNER_LABEL,
  APPROVAL_KIND_LABEL,
  CAPABILITY,
  SECURITY_ONLY_REASON,
  WIRE_TO_COPY,
  approvalScopeBadge,
  credentialKind,
  egressBlastRadius,
} from "../wardyn/copy";

type Filter = "PENDING" | "decided";
type Scope = Record<string, unknown>;

// ============================================================
// Blast-radius derivation (finding D1) — EVERY approval kind gets a two-line
// banner built from the REAL requested_scope. The rule is honesty: we only state
// capabilities the scope actually grants. Where a field is absent we fall back to
// wording keyed on the KIND alone (which we always know) and never invent one.
//
// Real scope shapes (from the Go backend):
//   egress_domain — { host }                       (internal/egress/proxy)
//   credential    — the grant spec's kind-specific scope (broker/sql):
//                     github_token { repos, permissions }
//                     api_key      { host, header, format, secret_name }
//                     git_pat      { host, secret_name, username? }
//   tool_call     — { tool, cmd, env } (raised by wardyn-toolgate, via the
//                    proxy's brokered approvals route — the only producer)
// The grant KIND is not carried in requested_scope, so we infer the credential
// sub-kind from which keys are present — importantly to keep the git_pat nuance
// (that token is readable by the agent's process, unlike a brokered credential).
// ============================================================

const KIND_ICON: Record<string, React.ElementType> = {
  credential: KeyRound,
  egress_domain: Globe,
  tool_call: SquareTerminal,
};

function kindLabel(kind: ApprovalKind): string {
  const ck = WIRE_TO_COPY[kind];
  return ck ? APPROVAL_KIND_LABEL[ck] : String(kind);
}

function str(scope: Scope, ...keys: string[]): string | undefined {
  for (const k of keys) {
    const v = scope[k];
    if (typeof v === "string" && v) return v;
    if (typeof v === "number") return String(v);
  }
  return undefined;
}

function ttlPhrase(scope: Scope): string {
  const raw = scope.ttl_sec ?? scope.ttl_seconds;
  const sec = typeof raw === "number" ? raw : undefined;
  if (sec == null || sec <= 0) return "";
  const min = Math.round(sec / 60);
  return min >= 1 ? ` for ${min} minute${min === 1 ? "" : "s"}` : ` for ${sec} seconds`;
}

// credentialKind + its CredentialKind type now live in ../wardyn/copy.ts —
// hoisted so live-approvals.tsx's credential filter reads the SAME heuristic
// (see copy.ts's H7 comment on the function itself for the ordering rules).

// Does the github_token scope's permissions object grant any write/admin/push?
function grantsWrite(scope: Scope): boolean {
  const perms = scope.permissions;
  if (perms && typeof perms === "object") {
    return Object.values(perms as Record<string, unknown>).some(
      (v) => typeof v === "string" && /write|admin|push|maintain/i.test(v),
    );
  }
  return false;
}

// The optional amber capability chip — shown only when the scope demonstrably
// grants write (never a reassuring green check, per honesty rule D2).
function capabilityLabel(kind: ApprovalKind, scope: Scope): string | undefined {
  if (kind !== "credential") return undefined;
  const ck = credentialKind(scope);
  if (ck === "git_pat" || ck === "ssh_key") return "grants git write";
  if (ck === "github_token" && grantsWrite(scope)) return "grants write";
  return undefined;
}

function deriveTitle(kind: ApprovalKind, scope: Scope): string {
  switch (kind) {
    case "egress_domain": {
      const host = str(scope, "host", "domain");
      return host ? `Reach ${host}` : "Open a network egress";
    }
    case "credential": {
      const ck = credentialKind(scope);
      const host = str(scope, "host");
      if (ck === "git_pat") return host ? `Hand git a token for ${host}` : "Hand git an access token";
      if (ck === "ssh_key") return host ? `Write an SSH key for ${host}` : "Write a private SSH key to disk";
      if (ck === "github_token") {
        const repos = Array.isArray(scope.repos) ? scope.repos.filter((r) => typeof r === "string") : [];
        return repos.length ? `Mint a GitHub token for ${repos[0]}` : "Mint a GitHub token";
      }
      if (ck === "api_key") return host ? `Mint an API credential for ${host}` : "Mint an API credential";
      return "Mint a scoped credential";
    }
    case "tool_call": {
      const cmd = str(scope, "cmd", "command", "tool");
      return cmd ? `Run ${cmd}` : "Run a tool call";
    }
    // The re-auth request is not a mint and not a decision (UX round B3):
    // mapping it to "credential" titled it "Mint a scoped credential" and
    // painted a blast-radius banner over a row that grants nothing.
    case "credential_reauth":
      return REAUTH_TITLE;
    default:
      return kindLabel(kind);
  }
}

interface Banner {
  what: string;
  blast: string;
}

// The fail-closed audience: no viewer in hand is "this is not yours to clear".
const NO_REAUTH_AUDIENCE: ReauthAudience = { canAct: false, shared: false, owner: "" };

// This is a PRE-decision preview, not a live readout of a scope in progress:
// PendingCard calls it before any scope has been chosen (the picker lives
// inside ReasonDialog, a separate component this one never sees into), so it
// always previews what a plain Approve — today's, and the default's,
// behavior — actually grants. There is deliberately no decisionScope
// parameter here: an earlier draft threaded one through for a "live" banner
// that would track the dialog's in-progress selection, but that would need
// lifting scope state out of ReasonDialog into this screen for no real gain —
// ReasonDialog already shows every scope's own honest one-liner side by side
// (APPROVAL_SCOPE_HINT/DENY_SCOPE_HINT in copy.ts), which is a better compare-
// before-you-pick UX than one line that changes under you. If a live banner is
// ever wanted here, it belongs inside ReasonDialog itself, driven by its own
// `scope` state — not bolted onto this pre-decision preview.
function deriveBanner(kind: ApprovalKind, scope: Scope, reauth?: ReauthAudience): Banner {
  const ttl = ttlPhrase(scope);
  switch (kind) {
    // NO BLAST RADIUS, because nothing is granted: this row asks its owner to
    // sign in again to a credential the deployment already configured. The
    // "what" states the need and the both-branches hint the row's own copy
    // carries — never a promise that a run will continue (Codex #5).
    case "credential_reauth":
      // The hint is AUDIENCE-DEPENDENT and the audience is the viewer, not the
      // scope: the door's own both-branches sentence for the person whose
      // sign-in clears it, and the sentence naming who CAN clear it for
      // everybody else (W6-U BLOCKER-1/BLOCKER-2). `reauth` is undefined only
      // for a caller with no viewer in hand, which grades as "not yours".
      return { what: REAUTH_ROW.label + ".", blast: reauthRowHint(reauth ?? NO_REAUTH_AUDIENCE) };
    case "egress_domain": {
      const host = str(scope, "host", "domain");
      // egressBlastRadius (copy.ts) is scope-aware — honesty rule: "we only
      // state capabilities the scope actually grants… never invent one." The
      // OLD hardcoded "for its remaining lifetime" claim was wrong for `once`
      // and hid `always`'s durability entirely. Always "run" here: see this
      // function's own doc for why that's correct, not a shortcut.
      return egressBlastRadius("run", host ?? "the requested host");
    }
    case "credential": {
      const ck = credentialKind(scope);
      const host = str(scope, "host");
      if (ck === "git_pat") {
        return {
          // #381 F3: this card is MEMBER-facing and this screen has no access
          // to the deployment's WARDYN_GIT_PAT_BROKER switch (the providers
          // endpoint it would read from is operator-only) — so it must never
          // assert a residency it cannot know. "authenticates" is true either
          // way; CAPABILITY.gitPatLine is deliberately NOT reused here (that
          // constant states the broker-ON default only).
          what: `A stored git access token${host ? ` for ${host}` : ""} authenticates this clone${ttl}.`,
          // The git_pat nuance: unlike a minted/scoped credential, the PAT
          // itself is a long-lived operator secret Wardyn cannot expire or
          // down-scope from its side — never claim otherwise here, and say
          // "stored" so this reads as the stored-PAT lane's limit, not every
          // credential kind's. The residency sentence names BOTH postures
          // rather than picking one this screen can't verify (#381 F3).
          blast: `Depending on this deployment's PAT broker setting, the token is attached to the request by the proxy on the outbound leg, or handed to git inside the sandbox where the process running there can read it. Either way, Wardyn can't expire or down-scope a stored PAT — it stays live until you revoke it on ${host ?? "the git host"}.`,
        };
      }
      if (ck === "ssh_key") {
        return {
          what: `A private SSH key${host ? ` for ${host}` : ""} is written to disk in the sandbox for a git-over-SSH clone${ttl}.`,
          // ssh_key has no credential-helper seam (git's SSH transport can't
          // take an injected credential), so unlike a brokered credential the
          // key MUST become a resident file the agent's process can read
          // during the clone — and, like a PAT, Wardyn can't expire or
          // down-scope it from its side.
          blast: `${CAPABILITY.sshKeyLine} Wardyn can't expire or down-scope it — it stays live until you revoke it on ${host ?? "the git host"}.`,
        };
      }
      if (ck === "github_token") {
        const repos = Array.isArray(scope.repos) ? scope.repos.filter((r): r is string => typeof r === "string") : [];
        const write = grantsWrite(scope);
        return {
          what: `The broker mints a short-lived GitHub token${repos.length ? ` scoped to ${repos.join(", ")}` : ""}${ttl}. The agent never sees your stored key.`,
          blast: `${CAPABILITY.brokerLine} ${write ? "It can push to the listed repos" : "It's limited to the listed read permissions"} until it expires — it can't mint a wider scope, and no other repo is reachable.`,
        };
      }
      if (ck === "api_key") {
        // Nothing short-lived is minted for api_key: the proxy fetches the
        // long-lived stored key once at startup and injects it per-request for
        // the run's whole lifetime — no brokerLine, no TTL claim.
        return {
          what: `Your stored key is injected proxy-side${host ? ` for ${host}` : ""} — it never enters the sandbox.`,
          blast: `The agent can call ${host ?? "the target API"} with it for the run's lifetime; no other host receives the key, and the scope can't widen.`,
        };
      }
      return {
        what: `The broker mints a short-lived, scoped credential bound to this run's identity${ttl}. The agent never sees your stored key.`,
        blast: `${CAPABILITY.brokerLine} The minted scope is exactly what's shown below — the broker can't widen it.`,
      };
    }
    case "tool_call": {
      const cmd = str(scope, "cmd", "command", "tool");
      const env = str(scope, "env", "environment", "target");
      // No backend component enforces a tool_call approval — the requesting
      // process reads the decision and proceeds. Never claim "once" or "only
      // this command"; both the cmd string and any execution guarantee are
      // requester-supplied.
      return {
        what: cmd
          ? `The run asks permission to run ${cmd}${env ? ` against ${env}` : ""} (as reported by the requester).`
          : "The run asks permission to run a tool call.",
        blast:
          "Wardyn records your decision for this tool call; the requesting process reads it and proceeds. Wardyn does not itself run, restrict, or re-execute the command — the run's existing egress and credential policy still gates everything it does.",
      };
    }
    default:
      return {
        what: `Approving grants the ${kindLabel(kind)} scope shown below to this run.`,
        blast: "The granted scope is exactly what's shown — nothing wider.",
      };
  }
}

export function ApprovalsScreen({ onChanged }: { onChanged?: () => void }) {
  // B3/prompt-v2 point 5: the list is already server-scoped to the member's
  // own runs (handleListApprovals's creator-pager branch) — role only picks
  // the empty-state copy here.
  const role = useRole();
  // The caller's own capability set, for the ONE member moment on this screen:
  // an egress_domain approval whose host isn't granted to them. Fetched here,
  // not per card — the answer is the same for every row.
  //
  // DELIBERATELY still useOperator (0.7 §B): capAllowed/capGranted stay on
  // isOperator (capabilities.go) — that is the invariant making /permissions
  // safe to hand to the security tier, so a security admin is
  // capability-BOUNDED exactly like a member and this set is real for them.
  // The stale-groups banner below reads it for the same reason. Only the
  // per-card DECISION gate moves to the security predicate.
  const operator = useOperator();
  const caps = useMyCapabilities(!operator);
  const [pendingItems, setPendingItems] = React.useState<ApprovalRequest[]>([]);
  const [decidedItems, setDecidedItems] = React.useState<ApprovalRequest[]>([]);
  const [longestList, setLongestList] = React.useState(0);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  // ?tab=decided lands on the Decided tab — the audit trail's "Released by
  // approval" chip links here for an approval that has, by construction,
  // already been decided; the default stays the pending queue.
  const [searchParams, setSearchParams] = useSearchParams();
  const [filter, setFilter] = React.useState<Filter>(searchParams.get("tab") === "decided" ? "decided" : "PENDING");
  // X3-F14: ?tab= is read at mount but was never written back — the Decided
  // view couldn't be reloaded/shared/reached by Back. Mirrors audit.tsx's
  // run_id filter, the setSearchParams-on-every-change idiom.
  const setFilterParam = React.useCallback(
    (f: Filter) => {
      setFilter(f);
      setSearchParams(f === "decided" ? { tab: "decided" } : {}, { replace: true });
    },
    [setSearchParams],
  );
  const [prompt, setPrompt] = React.useState<{ id: string; action: "approve" | "deny"; kind: ApprovalRequest["kind"] } | null>(null);

  // MEDIUM fix: EXPIRED approvals were never fetched, so a request that timed
  // out without a human decision silently vanished from the console. Include
  // EXPIRED in the decided view alongside APPROVED/DENIED (finding D12).
  // CANCELLED is fetched for the same reason (B4): a run's terminal transition
  // moves its PENDING approvals there, and a row that left the pending queue
  // without appearing in the decided one would just vanish.
  const fetchAll = React.useCallback(() => {
    return Promise.all([
      api.listApprovals("PENDING"),
      api.listApprovals("APPROVED"),
      api.listApprovals("DENIED"),
      api.listApprovals("EXPIRED"),
      api.listApprovals("CANCELLED"),
    ]).then(([pending, approved, denied, expired, cancelled]) => {
      setPendingItems(pending);
      // Each state is its own capped fetch, so the truncation test is per list:
      // the merged decidedItems length would false-positive on three large but
      // complete lists. Decided history only grows, so this one does get hit.
      setLongestList(Math.max(
        pending.length, approved.length, denied.length, expired.length, cancelled.length,
      ));
      setDecidedItems([...approved, ...denied, ...expired, ...cancelled].sort(
        (a, b) => Date.parse(b.requested_at) - Date.parse(a.requested_at),
      ));
    });
  }, []);

  const load = React.useCallback(() => {
    setStatus("loading");
    fetchAll()
      .then(() => setStatus("ready"))
      .catch(() => setStatus("error"));
  }, [fetchAll]);
  React.useEffect(load, [load]);

  // MEDIUM fix: a HITL queue is blocking — newly-arrived requests must surface
  // without a manual reload, and the nav badge (driven by onChanged) must not go
  // stale. Poll on an interval. We refresh silently (no loading flicker) by
  // re-fetching directly rather than calling load(), and notify the shell so the
  // pending-count badge updates.
  //
  // F5-F4: a single transient failure on mount left status="error" forever —
  // the poll kept silently filling pendingItems (and the nav badge) while the
  // main view stayed stuck on "Something went wrong". A successful tick now
  // HEALS status back to "ready" (audit.tsx's tick precedent); paused only
  // while the foreground load is in flight, so a tick during the error state
  // can still recover it.
  const POLL_MS = 10_000;
  usePoll(() => {
    fetchAll()
      .then(() => {
        setStatus("ready");
        onChanged?.();
      })
      .catch(() => {
        /* transient poll failure — keep the last good view, retry next tick */
      });
  }, POLL_MS, status === "loading");

  // HIGH fix (error handling): approve/deny can reject — a 409 (already decided
  // / expired), a 403, or a network drop. We surface the failure as a toast and
  // signal success/failure back to the dialog so it can reset its busy state and
  // only close on success. Returns true on success so ReasonDialog knows whether
  // to close.
  const decide = async (reason: string, decisionScope: ApprovalScope, until?: string): Promise<boolean> => {
    if (!prompt) return false;
    try {
      const args = decisionArgs(decisionScope, until);
      if (prompt.action === "approve") await api.approve(prompt.id, reason, ...args);
      else await api.deny(prompt.id, reason, ...args);
      toast.success(prompt.action === "approve" ? "Request approved" : "Request denied");
      setPrompt(null);
      // F5-F11: load() flips status back to "loading" first — the whole
      // queue flashed to a skeleton after every single decision, losing
      // scroll position. fetchAll() is the same silent refresh the poll
      // uses; a refresh failure here is not fatal — the decide itself
      // already succeeded, and the next poll tick will catch the view up.
      fetchAll().catch(() => {
        /* transient refresh failure — decide() itself already succeeded */
      });
      onChanged?.();
      return true;
    } catch (err) {
      toast.error(
        prompt.action === "approve" ? "Failed to approve request" : "Failed to deny request",
        { description: getErrorMessage(err) },
      );
      return false;
    }
  };

  return (
    <div className="mx-auto max-w-[880px] px-6 py-6">
      <PageHeader
        title="Approvals"
        description="Decisions that gate what agents can do — nothing privileged happens without one. Approving a credential authorizes the broker to mint a short-lived, scoped token."
      />

      {/* The snapshot ceiling, said where it costs something: group membership
          was recorded at sign-in, so a session that predates group recording
          can't resolve group grants at all. Only shown once a capability is
          actually being enforced against this caller. */}
      {caps?.groups_snapshot_stale && anyCapabilityEnforced(caps) && (
        <p className="mb-4 rounded-lg bg-muted px-3 py-2 text-xs text-muted-foreground">{DENIED.STALE_GROUPS}</p>
      )}

      <TruncatedNote count={longestList} cap={LIST_LIMIT} />

      <Tabs value={filter} onValueChange={(v) => setFilterParam(v as Filter)} className="mb-4">
        <TabsList>
          <TabsTrigger value="PENDING" className="gap-2">
            Pending
            {pendingItems.length > 0 && (
              <span className="rounded-full bg-warning-subtle px-1.5 text-meta font-semibold text-warning">
                {pendingItems.length}
              </span>
            )}
          </TabsTrigger>
          <TabsTrigger value="decided">Decided</TabsTrigger>
        </TabsList>
      </Tabs>

      {status === "loading" ? (
        <div className="rounded-xl border border-border bg-card">
          <TableSkeleton rows={4} cols={3} />
        </div>
      ) : status === "error" ? (
        <div className="rounded-xl border border-border bg-card">
          <ErrorState onRetry={load} />
        </div>
      ) : filter === "PENDING" ? (
        pendingItems.length === 0 ? (
          <div className="rounded-xl border border-border bg-card">
            <EmptyState
              icon={ShieldCheck}
              title={role === "member" ? "Approvals raised by your runs appear here." : "You're all caught up"}
              description={
                role === "member"
                  ? undefined
                  : "New credential, egress, and tool-call requests appear here the moment an agent needs you."
              }
              action={
                <Button variant="outline" size="sm" onClick={() => setFilterParam("decided")}>
                  See decided
                </Button>
              }
            />
          </div>
        ) : (
          <div className="space-y-3.5">
            {pendingItems.map((a) => (
              <PendingCard
                key={a.id}
                item={a}
                caps={caps}
                onAct={(action) => setPrompt({ id: a.id, action, kind: a.kind })}
              />
            ))}
          </div>
        )
      ) : decidedItems.length === 0 ? (
        <div className="rounded-xl border border-border bg-card">
          <EmptyState
            icon={Archive}
            title="No decisions yet"
            description="Approved, denied, and expired requests are archived here."
            action={
              <Button variant="outline" size="sm" onClick={() => setFilterParam("PENDING")}>
                Back to pending
              </Button>
            }
          />
        </div>
      ) : (
        <>
          <div className="overflow-hidden rounded-xl border border-border bg-card">
            {decidedItems.map((d) => (
              <DecidedRow key={d.id} item={d} />
            ))}
          </div>
          <p className="mt-3 flex items-center gap-2 text-xs text-muted-foreground">
            <Archive className="size-3.5" />
            Approved, denied, and expired requests are archived here — the full history lives in the audit trail.
          </p>
        </>
      )}

      {/* hasWorkspace: this standalone page never has the run in hand (no
          getRun fetch backs the list), so — unlike the two mounts that DO
          know a run's workspace_ids — it deliberately opts back into Always
          being offered rather than inheriting the fail-safe-disabled default;
          a member/no-workspace decide still gets the server's honest 400/403. */}
      <ReasonDialog prompt={prompt} hasWorkspace onClose={() => setPrompt(null)} onSubmit={decide} />
    </div>
  );
}

function PendingCard({
  item,
  caps,
  onAct,
}: {
  item: ApprovalRequest;
  // The viewer's own capability set, or null when the question doesn't apply
  // (an admin, or an answer still in flight).
  caps: MeCapabilities | null;
  onAct: (action: "approve" | "deny") => void;
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
  const reauth = reauthAudience(item, { operator: door.operator, principal: door.principal });
  const banner = deriveBanner(item.kind, scope, reauth);
  // Deciding an egress_domain approval on an owned run is a MEMBER act (B3,
  // decide() in approvals.go); credential and tool_call stay admin-only
  // regardless of ownership — see canDecideApproval's doc for why. This list
  // is already scoped to rows the caller owns (or every row, for an admin),
  // so ownership itself needs no re-check here.
  // useSecurityOperator, not useOperator (0.7 §B): authorizeMemberDecision
  // early-returns for isSecurityOperator (approvals.go:392) — the security
  // tier decides ANY kind on ANY run, org-wide. Deciding a verdict is that
  // tier's whole purpose; the caps fetch above stays on useOperator because
  // capAllowed does (capabilities.go).
  const securityOperator = useSecurityOperator();
  const kindDecidable = canDecideApproval(securityOperator, item.kind);
  // The `egress_host` capability bounds which hosts a member may DECIDE on —
  // the authorizeMemberDecision seam (approvals.go). Advisory here: the server
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

      <RunContextRow runId={item.run_id} onRun={setRun} />

      {/* Blast-radius banner (D1) — derived from the real scope above. */}
      <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
        <p className="text-foreground">
          <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span> {banner.what}
        </p>
        {/* NO BLAST RADIUS for a re-auth request (UX ruling B3, general S4):
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

      {/* P0.3 (R3-F001/F108/F145) — an egress_domain approval is HOST-WIDE, and
          has been all along: the proxy strips any port before it keys the
          decision (approvalHostKey). 0.7.2 aligns the surfaces to SAY so rather
          than rely on it quietly, because "allow api.example.com" reads as the
          one connection in front of you. The scope words above are canon and
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
          /* A DOOR, NOT A DECISION (UX round B3). The pair is REMOVED, not
             disabled: a disabled Approve reads as "an admin can do this", and
             no tier can — the server answers 409 to either verb. The one
             control opens the same dialog every other sign-in surface opens. */
          reauth.canAct ? <ReauthAction /> : null
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

function DecidedRow({ item }: { item: ApprovalRequest }) {
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

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-t border-border px-4 py-3 first:border-t-0">
      <ApprovalKindChip kind={item.kind} />
      <span className="min-w-0 flex-1 truncate text-sm text-foreground">{deriveTitle(item.kind, scope)}</span>
      <Link
        to={`/runs/${encodeURIComponent(item.run_id)}`}
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
function ReauthAction() {
  const door = useModelAccessDoor();
  useClaimModelAccessDoor(true);
  return (
    <Button size="sm" variant="info" aria-label={REAUTH_ROW.ariaLabel} onClick={() => door.openDoor()}>
      {REAUTH_ROW.action}
    </Button>
  );
}
