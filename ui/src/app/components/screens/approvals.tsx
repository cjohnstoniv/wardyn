/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Link } from "react-router-dom";
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
import type { ApprovalKind, ApprovalRequest } from "../../lib/types";
import { approvals as api } from "../../lib/api/approvals";
import { usePoll } from "../../lib/use-poll";
import { getErrorMessage, relativeTime } from "../../lib/format";
import { Button } from "../ui/button";
import { Tabs, TabsList, TabsTrigger } from "../ui/tabs";
import { ApprovalKindChip, ApprovalStateBadge, Chip } from "../wardyn/primitives";
import { RunContextRow } from "../wardyn/run-context-row";
import { JsonBlock } from "../wardyn/code-block";
import { EmptyState, ErrorState, TableSkeleton } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { ReasonDialog } from "../wardyn/reason-dialog";
import {
  APPROVAL_BANNER_LABEL,
  APPROVAL_KIND_LABEL,
  CAPABILITY,
  type ApprovalKind as CopyKind,
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
//   tool_call     — { tool, cmd, env } (defensive: no current backend producer)
// The grant KIND is not carried in requested_scope, so we infer the credential
// sub-kind from which keys are present — importantly to keep the git_pat nuance
// (that token is readable by the agent's process, unlike a brokered credential).
// ============================================================

const WIRE_TO_COPY: Record<string, CopyKind> = {
  credential: "credential",
  egress_domain: "egress",
  tool_call: "tool",
};

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

type CredentialKind = "git_pat" | "github_token" | "api_key" | "ssh_key" | "generic";
// H7 fix: requested_scope never carries the grant KIND (ApprovalRequest.kind is
// only the wire-level "credential"/"egress_domain"/"tool_call"), so this stays a
// key-sniffing heuristic. Order matters:
//  - key_secret_ref is UNIQUE to ssh_key (broker.sshKeyScope) — must be checked
//    before the git_pat fallback, or every ssh_key approval (a resident,
//    agent-readable PRIVATE KEY, not a PAT) rendered the git_pat banner.
//  - api_key's scope ALSO carries secret_name (the broker requires it), so the
//    api_key discriminators (header/format) must be checked before the git_pat
//    fallback — otherwise every api_key approval renders the git_pat banner,
//    which claims the agent's process can read the key (the opposite of
//    api_key's proxy-side injection design).
// a MINIMAL api_key scope ({host,secret_name} only, header/format
// omitted since the broker defaults them) is indistinguishable from a minimal
// git_pat scope ({host,secret_name}, username omitted) by keys alone — genuine
// wire-format ambiguity, not fixable client-side. We bias the fallback toward
// git_pat: it's the safer direction (never under-claim exposure) and the only
// in-app path that produces a truly minimal scope today (git_pat with a blank
// username field). Upgrade path: expose the real GrantKind on ApprovalRequest.
function credentialKind(scope: Scope): CredentialKind {
  if ("repos" in scope || "permissions" in scope) return "github_token";
  if ("key_secret_ref" in scope) return "ssh_key";
  if ("header" in scope || "format" in scope) return "api_key";
  if ("secret_name" in scope || "username" in scope) return "git_pat";
  return "generic";
}

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
    default:
      return kindLabel(kind);
  }
}

interface Banner {
  what: string;
  blast: string;
}

function deriveBanner(kind: ApprovalKind, scope: Scope): Banner {
  const ttl = ttlPhrase(scope);
  switch (kind) {
    case "egress_domain": {
      const host = str(scope, "host", "domain");
      return {
        what: host
          ? `Outbound network access from this run to ${host}.`
          : "Outbound network access from this run to the requested host.",
        blast: host
          ? `The run can reach ${host} for its remaining lifetime. No other new domain opens — every other host still gates.`
          : "The run can reach the approved host. No other new domain opens — every other host still gates.",
      };
    }
    case "credential": {
      const ck = credentialKind(scope);
      const host = str(scope, "host");
      if (ck === "git_pat") {
        return {
          what: `A git access token${host ? ` for ${host}` : ""} is handed to git inside the sandbox${ttl}.`,
          // The git_pat nuance: unlike a brokered credential, this value is
          // readable by the agent's process (CAPABILITY.gitPatLine), and the
          // PAT itself is a long-lived operator secret Wardyn cannot expire or
          // down-scope — never claim otherwise here.
          blast: `${CAPABILITY.gitPatLine} Wardyn can't expire or down-scope a PAT — it stays live until you revoke it on ${host ?? "the git host"}.`,
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
  const [pendingItems, setPendingItems] = React.useState<ApprovalRequest[]>([]);
  const [decidedItems, setDecidedItems] = React.useState<ApprovalRequest[]>([]);
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  const [filter, setFilter] = React.useState<Filter>("PENDING");
  const [prompt, setPrompt] = React.useState<{ id: string; action: "approve" | "deny"; kind: ApprovalRequest["kind"] } | null>(null);

  // MEDIUM fix: EXPIRED approvals were never fetched, so a request that timed
  // out without a human decision silently vanished from the console. Include
  // EXPIRED in the decided view alongside APPROVED/DENIED (finding D12).
  const fetchAll = React.useCallback(() => {
    return Promise.all([
      api.listApprovals("PENDING"),
      api.listApprovals("APPROVED"),
      api.listApprovals("DENIED"),
      api.listApprovals("EXPIRED"),
    ]).then(([pending, approved, denied, expired]) => {
      setPendingItems(pending);
      setDecidedItems([...approved, ...denied, ...expired].sort(
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
  const POLL_MS = 10_000;
  usePoll(() => {
    fetchAll()
      .then(() => onChanged?.())
      .catch(() => {
        /* transient poll failure — keep the last good view, retry next tick */
      });
  }, POLL_MS, false);

  // HIGH fix (error handling): approve/deny can reject — a 409 (already decided
  // / expired), a 403, or a network drop. We surface the failure as a toast and
  // signal success/failure back to the dialog so it can reset its busy state and
  // only close on success. Returns true on success so ReasonDialog knows whether
  // to close.
  const decide = async (reason: string): Promise<boolean> => {
    if (!prompt) return false;
    try {
      if (prompt.action === "approve") await api.approve(prompt.id, reason);
      else await api.deny(prompt.id, reason);
      toast.success(prompt.action === "approve" ? "Request approved" : "Request denied");
      setPrompt(null);
      load();
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

      <Tabs value={filter} onValueChange={(v) => setFilter(v as Filter)} className="mb-4">
        <TabsList>
          <TabsTrigger value="PENDING" className="gap-2">
            Pending
            {pendingItems.length > 0 && (
              <span className="rounded-full bg-warning-subtle px-1.5 text-[0.6875rem] font-semibold text-warning">
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
              title="You're all caught up"
              description="New credential, egress, and tool-call requests appear here the moment an agent needs you."
              action={
                <Button variant="outline" size="sm" onClick={() => setFilter("decided")}>
                  See decided
                </Button>
              }
            />
          </div>
        ) : (
          <div className="space-y-3.5">
            {pendingItems.map((a) => (
              <PendingCard key={a.id} item={a} onAct={(action) => setPrompt({ id: a.id, action, kind: a.kind })} />
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
              <Button variant="outline" size="sm" onClick={() => setFilter("PENDING")}>
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

      <ReasonDialog prompt={prompt} onClose={() => setPrompt(null)} onSubmit={decide} />
    </div>
  );
}

function PendingCard({
  item,
  onAct,
}: {
  item: ApprovalRequest;
  onAct: (action: "approve" | "deny") => void;
}) {
  const scope = item.requested_scope ?? {};
  const KindIcon = KIND_ICON[item.kind] ?? ShieldCheck;
  const cap = capabilityLabel(item.kind, scope);
  const banner = deriveBanner(item.kind, scope);

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

      <RunContextRow runId={item.run_id} />

      {/* Blast-radius banner (D1) — derived from the real scope above. */}
      <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
        <p className="text-foreground">
          <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span> {banner.what}
        </p>
        <p className="text-muted-foreground">
          <span className="font-semibold text-foreground/80">{APPROVAL_BANNER_LABEL.blast}</span> {banner.blast}
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

      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border/60 pt-3">
        <Button size="sm" variant="info" onClick={() => onAct("approve")}>
          <Check className="size-4" /> Approve
        </Button>
        <Button variant="outline" size="sm" onClick={() => onAct("deny")}>
          <X className="size-4" /> Deny
        </Button>
        <span className="ml-auto text-xs text-muted-foreground" title={item.requested_at}>
          requested {relativeTime(item.requested_at)}
        </span>
      </div>
    </div>
  );
}

function DecidedRow({ item }: { item: ApprovalRequest }) {
  const scope = item.requested_scope ?? {};
  const who = item.decided_by || (item.state === "EXPIRED" ? "unanswered" : "system");
  const when = relativeTime(item.decided_at ?? item.requested_at);
  const meta = item.state === "EXPIRED" && !item.decided_by ? `${who} · ${when}` : `by ${who} · ${when}`;

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
      <span className="whitespace-nowrap text-xs text-muted-foreground">{meta}</span>
    </div>
  );
}

