/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useSearchParams } from "react-router-dom";
import {
  Archive,
  GitBranch,
  Globe,
  KeyRound,
  ShieldCheck,
  SquareTerminal,
} from "lucide-react";
import { toast } from "sonner";
import {
  decisionArgs,
  type ApprovalKind,
  type ApprovalRequest,
  type ApprovalScope,
  type DecisionOptions,
} from "../../lib/types";
import { approvals as api } from "../../lib/api/approvals";
import { requests as elevationApi } from "../../lib/api/governance-requests";
import type { GovernanceRequestList } from "../../lib/types/governance-requests";
import { anyCapabilityEnforced, useMyCapabilities } from "../../lib/capabilities";
import { DENIED } from "../../lib/permissions-copy";
import { HttpError, LIST_LIMIT } from "../../lib/api/core";
import { governance } from "../../lib/api/governance";
import { usePoll } from "../../lib/use-poll";
import { useRunTitles } from "../../lib/use-run-titles";
import { getErrorMessage } from "../../lib/format";
import { grantsWrite, scopeRepos } from "../../lib/grant-sentence";
import { Button } from "../ui/button";
import { Tabs, TabsList, TabsTrigger } from "../ui/tabs";
import { EmptyState, ErrorState, failureSentence, TableSkeleton, TruncatedNote } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { PageContainer } from "../wardyn/page-container";
import { ReasonDialog } from "../wardyn/reason-dialog";
import { useRefusedApprovals } from "../../lib/hooks/use-refused-approvals";
import { OIDC_HOLD_LANE, REAUTH_ROW, REAUTH_TITLE, reauthRowHint, type ReauthAudience } from "../wardyn/model-access-copy";
import { OIDC_SIGN_IN } from "../wardyn/copy/oidc-signin";
import { useOperator, useRole, useSecurityOperator } from "../wardyn/operator-context";
import { useConsoleMode } from "../wardyn/console-view";
import { ADO } from "../../lib/ado-entra-copy";
import { NEEDS_YOU } from "../wardyn/copy/needs-you";
import { APPROVALS } from "../../lib/approvals-copy";
import { PUSH } from "../wardyn/copy/push";
import { APPROVALS_TAB, APPROVAL_MCP_CALL } from "../wardyn/copy/approvals";
import { FILE_DOWNLOAD_CARD, HOST_CALL_CARD, KUBE_CALL_CARD } from "../wardyn/copy/files-review";
import { DECIDERS_INBOX } from "../wardyn/copy/deciders-inbox";
import { escapeUnsafeRunes } from "../../lib/escape-runes";
import {
  APPROVAL_KIND_LABEL,
  CAPABILITY,
  WIRE_TO_COPY,
  credentialKind,
  egressBlastRadius,
} from "../wardyn/copy";
import { ApprovalCard, DecidedRow } from "./approvals-cards";
import { ElevationDeciderCard } from "./elevation/elevation-decider-card";
import { ImageDeciderCard } from "./elevation/image-decider-card";
import { MyElevationRequests } from "./elevation/my-elevation-requests";

type Filter = "PENDING" | "waiting" | "decided";
// The server cuts the Waiting on you list here (maxDecidableByMe) and says so with X-Wardyn-Truncated.
const WAITING_CAP = 500;
type Scope = Record<string, unknown>;

// Blast-radius derivation (D1) — every approval kind gets a two-line
// banner built from the real requested_scope. The rule is honesty: we only state
// capabilities the scope actually grants. Where a field is absent we fall back to
// wording keyed on the kind alone (which we always know) and never invent one.
//
// Real scope shapes (from the Go backend):
//   egress_domain — { host }                       (internal/egress/proxy)
//   credential    — the grant spec's kind-specific scope (broker/sql):
//                     github_token { repos, permissions }
//                     api_key      { host, header, format, secret_name }
//                     git_pat      { host, secret_name, username? }
//   tool_call     — { tool, cmd, env } (raised by wardyn-toolgate, via the
//                    proxy's brokered approvals route — the only producer)
// The grant kind is not carried in requested_scope, so we infer the credential
// sub-kind from which keys are present — importantly to keep the git_pat nuance
// (that token is readable by the agent's process, unlike a brokered credential).

export const KIND_ICON: Record<string, React.ElementType> = {
  credential: KeyRound,
  egress_domain: Globe,
  tool_call: SquareTerminal,
  push_content: GitBranch,
};

function kindLabel(kind: ApprovalKind): string {
  const ck = WIRE_TO_COPY[kind];
  return ck ? APPROVAL_KIND_LABEL[ck] : String(kind);
}

export function str(scope: Scope, ...keys: string[]): string | undefined {
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

// The optional amber capability chip — shown only when the scope demonstrably
// grants write (never a reassuring green check, per honesty rule D2).
export function capabilityLabel(kind: ApprovalKind, scope: Scope): string | undefined {
  if (kind !== "credential") return undefined;
  const ck = credentialKind(scope);
  if (ck === "git_pat" || ck === "ssh_key") return "grants git write";
  if (ck === "github_token" && grantsWrite(scope)) return "grants write";
  return undefined;
}

/** `signIn` is the provider's name for an endpoint sign-in's hold, when the caller has mapped the scope's provider
 *  uid to it; without it the title reads as the unnamed provider. */
export function deriveTitle(kind: ApprovalKind, scope: Scope, signIn?: string): string {
  switch (kind) {
    case "egress_domain": {
      const host = str(scope, "host", "domain");
      return host ? `Reach ${host}` : "Open a network egress";
    }
    case "credential": {
      const ck = credentialKind(scope);
      const host = str(scope, "host");
      if (ck === "git_pat") return host ? `Hand git a token for ${host}` : "Hand git an access token";
      if (ck === "ssh_key") return host ? `Sign Git's SSH sign-in for ${host}` : "Sign Git's SSH sign-in";
      if (ck === "github_token") {
        const repos = scopeRepos(scope);
        return repos.length ? `Mint a GitHub token for ${repos[0]}` : "Mint a GitHub token";
      }
      if (ck === "api_key") return host ? `Mint an API credential for ${host}` : "Mint an API credential";
      return "Mint a scoped credential";
    }
    case "file_download":
      return FILE_DOWNLOAD_CARD.LIST_TITLE(escapeUnsafeRunes(str(scope, "name") ?? ""));
    case "tool_call": {
      // One held kubectl write (a call_sha256 and a cluster): the list names its method and namespace.
      if (str(scope, "call_sha256") && str(scope, "cluster")) {
        return KUBE_CALL_CARD.LIST_TITLE(escapeUnsafeRunes(str(scope, "method") ?? ""), escapeUnsafeRunes(str(scope, "namespace") ?? ""));
      }
      // One held host request (a call_sha256 and a host, no cluster): the list names its method, path and host.
      if (str(scope, "call_sha256") && str(scope, "host")) {
        return HOST_CALL_CARD.LIST_TITLE(escapeUnsafeRunes(str(scope, "method") ?? ""), escapeUnsafeRunes(str(scope, "path") ?? ""), escapeUnsafeRunes(str(scope, "host") ?? ""));
      }
      // One MCP call (call_sha256 and a server): the list names the tool and the server it was for.
      if (str(scope, "call_sha256") && str(scope, "mcp_server")) {
        return APPROVAL_MCP_CALL.LIST_TITLE(escapeUnsafeRunes(str(scope, "tool") ?? ""), escapeUnsafeRunes(str(scope, "mcp_server") ?? ""));
      }
      const cmd = str(scope, "cmd", "command", "tool");
      return cmd ? `Run ${cmd}` : "Run a tool call";
    }
    // The re-auth request is not a mint and not a decision (UX round B3):
    // mapping it to "credential" titled it "Mint a scoped credential" and
    // painted a blast-radius banner over a row that grants nothing.
    case "credential_reauth":
      // An endpoint's sign-in is not AWS's: its hold names its own lane.
      return scope.lane === OIDC_HOLD_LANE ? OIDC_SIGN_IN.APPROVAL_LABEL(signIn || OIDC_SIGN_IN.APPROVAL_UNNAMED) : REAUTH_TITLE;
    // Only reached by DecidedRow — ApprovalCard's own push_content branch
    // returns before this function is ever called (it renders PUSH.CARD_TITLE
    // directly, which is PENDING-tense and would read wrong on a decided row).
    case "push_content": {
      const repo = str(scope, "repo");
      return repo ? PUSH.LIST_TITLE(repo) : PUSH.KIND_LABEL;
    }
    default:
      return kindLabel(kind);
  }
}

interface Banner {
  what: string;
  blast: string;
}

// The fail-closed audience: no viewer in hand is "this is not yours to clear".
const NO_REAUTH_AUDIENCE: ReauthAudience = { canAct: false, shared: false, owner: "", provider: "", mine: false, oidc: false, holdUID: "" };

// This is a PRE-decision preview, not a live readout of a scope in progress:
// ApprovalCard calls it before any scope has been chosen (the picker lives
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
export function deriveBanner(kind: ApprovalKind, scope: Scope, reauth?: ReauthAudience): Banner {
  const ttl = ttlPhrase(scope);
  switch (kind) {
    // No blast radius, because nothing is granted: this row asks its owner to
    // sign in again to a credential the deployment already configured. The
    // "what" states the need and the both-branches hint the row's own copy
    // carries — never a promise that a run will continue (Codex #5).
    case "credential_reauth":
      // The hint is AUDIENCE-DEPENDENT and the audience is the viewer, not the
      // scope: the door's own both-branches sentence for the person whose
      // sign-in clears it, and the sentence naming who CAN clear it for
      // everybody else (W6-U BLOCKER-1/BLOCKER-2). `reauth` is undefined only
      // for a caller with no viewer in hand, which grades as "not yours".
      return {
        what: (reauth?.oidc ? OIDC_SIGN_IN.APPROVAL_LABEL(reauth.signIn ?? OIDC_SIGN_IN.APPROVAL_UNNAMED) : REAUTH_ROW.label) + ".",
        blast: reauthRowHint(reauth ?? NO_REAUTH_AUDIENCE),
      };
    case "egress_domain": {
      const host = str(scope, "host", "domain");
      // egressBlastRadius (copy.ts) is scope-aware — honesty rule: "we only
      // state capabilities the scope actually grants… never invent one." A
      // hardcoded "for its remaining lifetime" claim would be wrong for
      // `once` and would hide `always`'s durability entirely, so this always
      // passes "run" here: see this function's own doc for why that's
      // correct, not a shortcut.
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
          what: `Approving lets the proxy sign this run's Git SSH sign-in${host ? ` for ${host}` : ""}${ttl}; the key stays at the proxy.`,
          // ssh_key has no credential-helper seam (git's SSH transport can't
          // take an injected credential), so the proxy holds the key and signs
          // the sign-in as the sandbox's ssh agent — and, like a PAT, Wardyn
          // can't expire or down-scope it from its side.
          blast: `${CAPABILITY.sshKeyLine} Wardyn can't expire or down-scope it — it stays live until you revoke it on ${host ?? "the git host"}.`,
        };
      }
      if (ck === "github_token") {
        const repos = scopeRepos(scope);
        const write = grantsWrite(scope);
        return {
          what: `The broker mints a short-lived GitHub token${repos.length ? ` scoped to ${repos.join(", ")}` : ""}${ttl}. The agent never sees your stored key.`,
          blast: `${write ? "It can push to the listed repos" : "It's limited to the listed read permissions"} until it expires — it can't mint a wider scope, and no other repo is reachable.`,
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
        what: `Wardyn mints a credential bound to this run's identity${ttl}. The grant's kind says whether it enters the sandbox.`,
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
      //
      // STAYS TRUE HERE: an Azure DevOps escalation never reaches this
      // branch — isAdoCapabilityRequest routes it to AdoCapabilityCard
      // before deriveBanner is ever called (see ApprovalCard below), because
      // for THAT one lane the claim above is false (ADO.
      // TOOL_CALL_NOTE, rendered once on this screen, says why). Every other
      // tool_call still lands here, and the sentence below is still honest
      // for all of them.
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
  const runTitles = useRunTitles();
  const [pendingItems, setPendingItems] = React.useState<ApprovalRequest[]>([]);
  const [decidedItems, setDecidedItems] = React.useState<ApprovalRequest[]>([]);
  const [decidedLongest, setDecidedLongest] = React.useState(0);
  // The decided history is fetched when the Decided tab is open, not on every visit to the page.
  const [decidedStatus, setDecidedStatus] = React.useState<"idle" | "loading" | "error" | "ready">("idle");
  const [decidedMessage, setDecidedMessage] = React.useState<string | undefined>();
  const [pendingMessage, setPendingMessage] = React.useState<string | undefined>();
  // The list is asked for in the view the page is in, the scope the nav badge counts: the User view is
  // the person's own runs' approvals, the Admin view everyone's.
  const view = useConsoleMode();
  const [status, setStatus] = React.useState<"loading" | "error" | "ready">("loading");
  // ?tab=decided lands on the Decided tab — the audit trail's "Released by
  // approval" chip links here for an approval that has, by construction,
  // already been decided; the default stays the pending queue.
  const [searchParams, setSearchParams] = useSearchParams();
  const tabParam = searchParams.get("tab");
  const [filter, setFilter] = React.useState<Filter>(tabParam === "decided" ? "decided" : tabParam === "waiting" ? "waiting" : "PENDING");
  // X3-F14: ?tab= must be written back too, not just read at mount, or the
  // Decided view can't be reloaded/shared/reached by Back. Mirrors
  // audit.tsx's run_id filter, the setSearchParams-on-every-change idiom.
  const setFilterParam = React.useCallback(
    (f: Filter) => {
      setFilter(f);
      setSearchParams(f === "decided" ? { tab: "decided" } : f === "waiting" ? { tab: "waiting" } : {}, { replace: true });
    },
    [setSearchParams],
  );
  const { refused, wallsRefused, presetRefused, noteRefusal } = useRefusedApprovals();
  const [prompt, setPrompt] = React.useState<{
    id: string;
    action: "approve" | "deny";
    kind: ApprovalRequest["kind"];
    maxScope?: ApprovalScope;
  } | null>(null);

  // Waiting on you: the pending requests this person may decide under a unit's deciders rule, other people's included.
  // A security operator never gets the tab (their Pending lists every request). Everyone else asks once whether the
  // server knows the filter, reads the list while it does, and the tab is drawn once a row carried requested_by (a
  // request on someone else's run) and for as long as this screen is mounted.
  const securityOperator = useSecurityOperator();
  const [knows, setKnows] = React.useState<boolean | null>(null);
  const [waitingRows, setWaitingRows] = React.useState<ApprovalRequest[]>([]);
  const [waitingTruncated, setWaitingTruncated] = React.useState(false);
  const [waitingStatus, setWaitingStatus] = React.useState<"idle" | "loading" | "error" | "ready">("idle");
  const [latched, setLatched] = React.useState(false);
  // The held elevation requests: the person's own ("Your requests") and the ones they may decide. A failed read
  // after a good one keeps the last good list.
  const [elevation, setElevation] = React.useState<GovernanceRequestList | null | "loading" | "error">("loading");
  // What the failed read threw, so the "error" state can say whether Wardyn answered.
  const [elevationError, setElevationError] = React.useState<unknown>(null);
  const deciderRequests =
    !securityOperator && typeof elevation === "object" && elevation !== null
      ? elevation.requests.filter((r) => (r.kind === "elevation" || r.kind === "image") && r.decidable && r.state === "pending")
      : [];
  const waitingOn = !securityOperator && knows === true;
  const waitingOnRef = React.useRef(waitingOn);
  waitingOnRef.current = waitingOn;
  const tabDrawn = (waitingOn && latched) || (!securityOperator && deciderRequests.length > 0);
  const shown: Filter = filter === "waiting" && !tabDrawn ? "PENDING" : filter;
  // Unit names for the basis line of a decision made as a unit admin, read once and only when such a row is on screen.
  const [unitNames, setUnitNames] = React.useState<Record<string, string> | undefined>(undefined);
  const unitNamesAsked = React.useRef(false);

  // EXPIRED approvals must be fetched too, or a request that timed out
  // without a human decision silently vanishes from the console. Include
  // EXPIRED in the decided view alongside APPROVED/DENIED.
  // CANCELLED is fetched for the same reason (B4): a run's terminal transition
  // moves its PENDING approvals there, and a row that left the pending queue
  // without appearing in the decided one would just vanish.
  const fetchPending = React.useCallback(
    () => api.listApprovals("PENDING", "", view).then(setPendingItems),
    [view],
  );
  const fetchWaiting = React.useCallback(
    () =>
      api.listDecidableByMe().then(({ rows, truncated }) => {
        setWaitingRows(rows);
        setWaitingTruncated(truncated);
        setWaitingStatus("ready");
        if (rows.some((r) => !!r.requested_by)) setLatched(true);
      }),
    [],
  );
  const fetchElevation = React.useCallback(
    () => elevationApi.list().then(setElevation),
    [],
  );
  const fetchDecided = React.useCallback(() => {
    return Promise.all([
      api.listApprovals("APPROVED", "", view),
      api.listApprovals("DENIED", "", view),
      api.listApprovals("EXPIRED", "", view),
      api.listApprovals("CANCELLED", "", view),
    ]).then(([approved, denied, expired, cancelled]) => {
      // Each state is its own capped fetch, so the truncation test is per list:
      // the merged decidedItems length would false-positive on three large but
      // complete lists. Decided history only grows, so this one does get hit.
      setDecidedLongest(Math.max(approved.length, denied.length, expired.length, cancelled.length));
      setDecidedItems([...approved, ...denied, ...expired, ...cancelled].sort(
        (a, b) => Date.parse(b.requested_at) - Date.parse(a.requested_at),
      ));
    });
  }, [view]);
  // The tab the poll and a decision refresh: the pending queue always, the decided history only while it is
  // the one on screen (#2487: it is up to four capped lists, and only grows).
  const filterRef = React.useRef(filter);
  filterRef.current = filter;
  const viewRef = React.useRef(view);
  viewRef.current = view;
  // A refresh that loads the decided lists for the view still on screen also heals a failed first load of the tab.
  // The Waiting on you list rides every refresh while the tab is possible; its failure is its own, never the screen's.
  const fetchAll = React.useCallback(() => {
    const waitingRead = waitingOnRef.current ? fetchWaiting().catch(() => undefined) : undefined;
    const main =
      filterRef.current === "decided"
        ? Promise.all([
            fetchPending(),
            fetchDecided().then(() => {
              if (viewRef.current === view) setDecidedStatus("ready");
            }),
          ]).then(() => undefined)
        : fetchPending();
    const elevationRead = fetchElevation().catch((e: unknown) => {
      setElevationError(e);
      setElevation((cur) => (typeof cur === "object" ? cur : "error"));
    });
    return Promise.all([main, waitingRead, elevationRead]).then(() => undefined);
  }, [fetchPending, fetchDecided, fetchWaiting, fetchElevation, view]);
  const loadWaiting = React.useCallback(() => {
    setWaitingStatus((st) => (st === "ready" ? st : "loading"));
    fetchWaiting().catch(() => setWaitingStatus((st) => (st === "ready" ? st : "error")));
  }, [fetchWaiting]);
  React.useEffect(() => {
    if (securityOperator) {
      setKnows(null);
      return;
    }
    let alive = true;
    // Any failure, a module that has no such read included, is "does not know": nothing new is drawn.
    void Promise.resolve()
      .then(() => api.knowsDecidableBy())
      .catch(() => false)
      .then((k) => alive && setKnows(k));
    return () => {
      alive = false;
    };
  }, [securityOperator]);
  React.useEffect(() => {
    if (waitingOn) loadWaiting();
  }, [waitingOn, loadWaiting]);
  const loadDecided = React.useCallback(() => {
    setDecidedStatus((st) => (st === "ready" ? st : "loading"));
    fetchDecided()
      .then(() => setDecidedStatus("ready"))
      .catch((e) => {
        setDecidedMessage(failureSentence(e));
        setDecidedStatus((st) => (st === "ready" ? st : "error"));
      });
  }, [fetchDecided]);
  // A view switch keeps this screen mounted: the other view's decided rows are not "ready" for this one.
  React.useEffect(() => setDecidedStatus("idle"), [view]);
  React.useEffect(() => {
    if (filter === "decided") loadDecided();
  }, [filter, loadDecided]);

  const wantsUnitNames = filter === "decided" && decidedItems.some((d) => d.decision_basis?.startsWith("unit_admin:"));
  React.useEffect(() => {
    if (!wantsUnitNames || unitNamesAsked.current) return;
    unitNamesAsked.current = true;
    // The unit-admin tier serves this read: a plain member or a group decider is refused and the row says "a unit admin".
    governance
      .getGovernance()
      .then((s) => setUnitNames({ ...s.unit_names, ...Object.fromEntries(s.profiles.map((p) => [p.id, p.name])) }))
      .catch(() => undefined);
  }, [wantsUnitNames]);

  React.useEffect(() => {
    void fetchElevation().catch((e: unknown) => {
      setElevationError(e);
      setElevation((cur) => (typeof cur === "object" ? cur : "error"));
    });
  }, [fetchElevation]);
  const reloadElevation = React.useCallback(() => {
    setElevation((cur) => (cur === "error" ? "loading" : cur));
    fetchElevation().catch((e: unknown) => {
      setElevationError(e);
      setElevation((cur) => (typeof cur === "object" ? cur : "error"));
    });
  }, [fetchElevation]);

  const load = React.useCallback(() => {
    setStatus("loading");
    fetchPending()
      .then(() => setStatus("ready"))
      .catch((e) => {
        setPendingMessage(failureSentence(e));
        setStatus("error");
      });
  }, [fetchPending]);
  React.useEffect(load, [load]);

  // A HITL queue is blocking — newly-arrived requests must surface
  // without a manual reload, and the nav badge (driven by onChanged) must not go
  // stale. Poll on an interval. We refresh silently (no loading flicker) by
  // re-fetching directly rather than calling load(), and notify the shell so the
  // pending-count badge updates.
  //
  // F5-F4: a single transient failure on mount must not leave status="error"
  // forever — the poll would keep silently filling pendingItems (and the nav
  // badge) while the main view stays stuck on "Something went wrong". A
  // successful tick heals status back to "ready" (audit.tsx's tick
  // precedent); paused only while the foreground load is in flight, so a
  // tick during the error state can still recover it.
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

  // approve/deny can reject — a 409 (already decided / expired), a 403, or a
  // network drop. We surface the failure as a toast and signal
  // success/failure back to the dialog so it can reset its busy state and
  // only close on success. Returns true on success so ReasonDialog knows
  // whether to close.
  // The deciders refusals each get their own toast; any other failure keeps the generic one.
  const refusalToast = (err: unknown): boolean => {
    if (!(err instanceof HttpError)) return false;
    if (err.reason === "not_a_decider") toast.error(DECIDERS_INBOX.TOAST_NOT_A_DECIDER, { description: err.message });
    else if (err.reason === "decision_scope_above_rule") toast.error(DECIDERS_INBOX.TOAST_SCOPE_ABOVE_RULE, { description: err.message });
    else if (err.reason === "deciders_unreadable") toast.error(DECIDERS_INBOX.TOAST_UNREADABLE, { description: DECIDERS_INBOX.TOAST_UNREADABLE_BODY });
    else return false;
    return true;
  };

  const decide = async (reason: string, decisionScope: ApprovalScope, until?: string): Promise<boolean> => {
    if (!prompt) return false;
    try {
      const args = decisionArgs(decisionScope, until);
      if (prompt.action === "approve") await api.approve(prompt.id, reason, ...args);
      else await api.deny(prompt.id, reason, ...args);
      toast.success(prompt.action === "approve" ? APPROVALS.TOAST_APPROVED : APPROVALS.TOAST_DENIED);
      setPrompt(null);
      // F5-F11: load() would flip status back to "loading" first — the whole
      // queue would flash to a skeleton after every single decision, losing
      // scroll position. fetchAll() is the same silent refresh the poll
      // uses instead; a refresh failure here is not fatal — the decide
      // itself already succeeded, and the next poll tick will catch the
      // view up.
      fetchAll().catch(() => {
        /* transient refresh failure — decide() itself already succeeded */
      });
      onChanged?.();
      return true;
    } catch (err) {
      noteRefusal(prompt.id, err);
      if (!refusalToast(err)) {
        toast.error(
          prompt.action === "approve" ? APPROVALS.TOAST_APPROVE_FAILED : APPROVALS.TOAST_DENY_FAILED,
          { description: getErrorMessage(err) },
        );
      }
      return false;
    }
  };

  // decideAdoDirect — the ADO capability card's own decide path. It bypasses
  // ReasonDialog entirely: the frozen mock (§9 Q2) draws the card's own
  // Approve/Deny-and-scope control with no reason field and no modal, unlike
  // every other kind on this screen. `opts` always carries an explicit
  // decision_scope — see ado-capability-card.tsx's adoDecisionArgs for why a
  // bodyless decide can't be used here. The push card passes no opts at all.
  const decideAdoDirect = async (id: string, approve: boolean, opts: [] | [DecisionOptions]): Promise<void> => {
    try {
      if (approve) await api.approve(id, "approved", ...opts);
      else await api.deny(id, "denied", ...opts);
      toast.success(approve ? APPROVALS.TOAST_APPROVED : APPROVALS.TOAST_DENIED);
      fetchAll().catch(() => {
        /* transient refresh failure — the decide itself already succeeded */
      });
      onChanged?.();
    } catch (err) {
      if (refusalToast(err)) return;
      toast.error(approve ? APPROVALS.TOAST_APPROVE_FAILED : APPROVALS.TOAST_DENY_FAILED, {
        description: getErrorMessage(err),
      });
    }
  };

  // decidePushDirect — the push_content card's own decide path. NO scope
  // args at all (decide's rule 4 refuses a decision_scope on this kind —
  // approvals_push.go), and no ReasonDialog: the frozen mock (packet 7)
  // draws this card's own Approve/Deny pair, the same direct-decide shape
  // decideAdoDirect above takes for its own kind, minus the scope.
  const decidePushDirect = (id: string, approve: boolean): Promise<void> => decideAdoDirect(id, approve, []);

  // #2963 — a "Needs you" request's Decline: not a decide() verb (approvals_takeover.go).
  const declineTakeover = async (id: string): Promise<void> => {
    try {
      await api.decline(id);
      fetchAll().catch(() => {
        /* transient refresh failure — the decline itself already succeeded */
      });
      onChanged?.();
    } catch (err) {
      toast.error(NEEDS_YOU.failed.decline, { description: getErrorMessage(err) });
    }
  };

  // The pending queue's own tool_call items, and only those — the screen-
  // level honesty note (ADO.TOOL_CALL_NOTE) is worth a line only
  // when a tool_call approval is actually in view.
  // One card for both lists; the Waiting on you tab passes waiting so the card trusts the server's verdict on it.
  const elevationDecided = () => {
    fetchAll().catch(() => {
      /* transient refresh failure — the decision itself already succeeded */
    });
    onChanged?.();
  };
  // data-focus-row: when a decision removes the card whose button opened the dialog, focus moves to the next card.
  const approvalCard = (a: ApprovalRequest, waiting: boolean) => (
    <div key={a.id} data-focus-row>
      <ApprovalCard
        item={wallsRefused.has(a.id) || presetRefused.has(a.id) ? { ...a, walls_block: true } : a}
        presetBlocked={presetRefused.has(a.id)}
        caps={caps}
        waiting={waiting}
        onAct={(action) => setPrompt({ id: a.id, action, kind: a.kind, maxScope: a.max_scope })}
        onAdoDecide={decideAdoDirect}
        onPushDecide={decidePushDirect}
        onDecline={declineTakeover}
        refused={refused.has(a.id)}
      />
    </div>
  );

  const waitingCount = (waitingStatus === "ready" ? waitingRows.length : 0) + deciderRequests.length;
  const anyToolCall = pendingItems.some((a) => a.kind === "tool_call") || decidedItems.some((a) => a.kind === "tool_call");

  return (
    <PageContainer width="form">
      <PageHeader
        title="Approvals"
        description="Requests an agent raised that need a person's decision. Each card says exactly what approving grants."
      />

      {/* The snapshot ceiling, said where it costs something: group membership
          was recorded at sign-in, so a session that predates group recording
          can't resolve group grants at all. Only shown once a capability is
          actually being enforced against this caller. */}
      {caps?.groups_snapshot_stale && anyCapabilityEnforced(caps) && (
        <p className="mb-4 rounded-lg bg-muted px-3 py-2 text-xs text-muted-foreground">{DENIED.STALE_GROUPS}</p>
      )}

      <TruncatedNote count={Math.max(pendingItems.length, decidedLongest)} cap={LIST_LIMIT} />

      {/* S10: replaces the sentence this design falsifies (§7.8
          TOOL_CALL_NOTE) — an Azure DevOps escalation IS enforced, held at
          the proxy until decided; every OTHER tool_call approval is still
          the record-only kind deriveBanner's tool_call case describes. */}
      {anyToolCall && (
        <p className="mb-4 text-xs text-muted-foreground">{ADO.TOOL_CALL_NOTE}</p>
      )}

      <MyElevationRequests list={elevation} error={elevationError} onReload={reloadElevation} />

      <Tabs value={shown} onValueChange={(v) => setFilterParam(v as Filter)} className="mb-4">
        <TabsList>
          <TabsTrigger value="PENDING" className="gap-2">
            Pending
            {pendingItems.length > 0 && (
              <span className="rounded-full bg-warning-subtle px-1.5 text-meta font-semibold text-warning">
                {pendingItems.length}
              </span>
            )}
          </TabsTrigger>
          {tabDrawn && (
            <TabsTrigger value="waiting" className="gap-2">
              {APPROVALS_TAB.WAITING}
              {waitingCount > 0 && (
                <span className="rounded-full bg-warning-subtle px-1.5 text-meta font-semibold text-warning">
                  {waitingCount}
                </span>
              )}
            </TabsTrigger>
          )}
          <TabsTrigger value="decided">Decided</TabsTrigger>
        </TabsList>
      </Tabs>
      {tabDrawn && <p className="-mt-2 mb-4 text-xs text-muted-foreground">{DECIDERS_INBOX.HELP}</p>}

      {/* The tab is drawn only after a read has succeeded, so this body never shows a first-read loading or error state. */}
      {shown === "waiting" ? (
        waitingRows.length === 0 && deciderRequests.length === 0 ? (
          <div className="rounded-xl border border-border bg-card">
            <EmptyState
              icon={ShieldCheck}
              title={DECIDERS_INBOX.EMPTY_TITLE}
              description={DECIDERS_INBOX.EMPTY_BODY}
              action={
                <Button variant="outline" size="sm" onClick={() => setFilterParam("decided")}>
                  {DECIDERS_INBOX.EMPTY_ACTION}
                </Button>
              }
            />
          </div>
        ) : (
          <>
            {waitingTruncated && (
              <TruncatedNote count={WAITING_CAP} cap={WAITING_CAP}>
                {DECIDERS_INBOX.TRUNCATED}
              </TruncatedNote>
            )}
            <div className="space-y-3.5">
              {deciderRequests.map((r) =>
                r.kind === "image" ? (
                  <ImageDeciderCard key={r.id} request={r} onDone={elevationDecided} />
                ) : (
                  <ElevationDeciderCard key={r.id} request={r} onDone={elevationDecided} />
                ),
              )}
              {waitingRows.map((a) => approvalCard(a, true))}
            </div>
          </>
        )
      ) : shown === "PENDING" && status === "loading" ? (
        <div className="rounded-xl border border-border bg-card">
          <TableSkeleton rows={4} cols={3} />
        </div>
      ) : shown === "PENDING" && status === "error" ? (
        <div className="rounded-xl border border-border bg-card">
          <ErrorState message={pendingMessage} onRetry={load} />
        </div>
      ) : shown === "PENDING" ? (
        pendingItems.length === 0 ? (
          <div className="rounded-xl border border-border bg-card">
            <EmptyState
              icon={ShieldCheck}
              title={APPROVALS.EMPTY_TITLE}
              description={role === "user" ? APPROVALS.EMPTY_MEMBER : APPROVALS.EMPTY_ADMIN}
              action={
                <Button variant="outline" size="sm" onClick={() => setFilterParam("decided")}>
                  See decided
                </Button>
              }
            />
          </div>
        ) : (
          <div className="space-y-3.5">
            {pendingItems.map((a) => approvalCard(a, false))}
          </div>
        )
      ) : decidedStatus === "error" ? (
        <div className="rounded-xl border border-border bg-card">
          <ErrorState message={decidedMessage} onRetry={loadDecided} />
        </div>
      ) : decidedStatus !== "ready" ? (
        <div className="rounded-xl border border-border bg-card">
          <TableSkeleton rows={4} cols={3} />
        </div>
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
              <DecidedRow key={d.id} item={d} runTitle={runTitles.get(d.run_id)} unitNames={unitNames} />
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
    </PageContainer>
  );
}
