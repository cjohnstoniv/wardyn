/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ApprovalKind as WireApprovalKind, ApprovalRequest, ApprovalScope } from "../../../lib/types";
import { shortTime } from "../../../lib/format";

// Said once, where a viewer would actually feel the consequence (the run's
// own pending-approval banner) — the read/launch/kill viewer tier still means
// their run blocks on an approval exactly like an admin's does; only the
// deciding is out of reach.
export const VIEWER_APPROVAL_BLOCKS_NOTE =
  "This run is blocked until an admin decides it — you can see the requested scope below, but deciding needs the admin role.";

// Approval blast-radius banners (D1) — every approval kind gets two lines:
// what you're approving, and the worst realistic outcome. The scope-specific
// text is filled by the Approvals screen; these are the fixed labels + intents.
export type ApprovalKind = "tool" | "credential" | "egress" | "reauth";
export const APPROVAL_BANNER_LABEL = {
  what: "What you're approving:",
  blast: "Blast radius:",
} as const;
export const APPROVAL_KIND_LABEL: Record<ApprovalKind, string> = {
  tool: "Tool call",
  credential: "Credential",
  egress: "Network egress",
  reauth: "AWS sign-in",
};

// The ONE console label for the CANCELLED approval state (the run ended before
// anyone decided it — internal/types/types.go ApprovalCancelled). Every other
// state is title-cased from the wire value by ApprovalStateBadge; this one is a
// key so the owner's wording lands in one place.
// DRAFT (M2) — not yet owner-frozen.
export const APPROVAL = {
  STATE_CANCELLED: "Cancelled",
  // B4, console half: what a human reads WHERE the Approve/Deny pair used to
  // be once the run has ended, and on the decided row the cascade wrote. One
  // string for both surfaces on purpose — they state the same fact, and two
  // copies would be two wordings of it.
  // DRAFT (M2 canon pending) — staged in workspace-providers-prompt.md §7.6.
  CANCELLED_BODY: "The run ended before anyone decided this. Nothing was approved and nothing was denied.",
  // P0.3 (R3-F001/F108/F145): an egress_domain approval is HOST-WIDE — the
  // server strips any port before it keys the decision (approvalHostKey,
  // internal/egress/proxy/approvals.go). 0.7.2 aligns the three surfaces to SAY
  // so rather than rely on it quietly; the port-scoped semantic is 0.8's.
  // DRAFT (M2 canon pending).
  HOST_WIDE_NOTE: "This covers the host, not one port — an approval here answers every port on it.",
} as const;

// Egress-approval decision scopes — egress_domain only. Canon strings from
// egress-scopes-PHASE0-COPY.md (the sign-off artifact; the mock/copy spec is
// UI source of truth in this repo — do not paraphrase these). Consumed by the
// three decision surfaces (live-approvals.tsx, reason-dialog.tsx) plus the
// blast-radius banner and decided-row badge (approvals.tsx, run-detail.tsx).
export const APPROVAL_SCOPE_ORDER: ApprovalScope[] = ["once", "run", "until", "always"];

// Menu label, approve flavor.
export const APPROVAL_SCOPE_LABEL: Record<ApprovalScope, string> = {
  once: "Once",
  run: "This run",
  until: "Until…",
  always: "Always",
};
// The honest sub-label under each approve option — Phase 0 §1. "Once"
// deliberately says CONNECTION, not request: on HTTPS the proxy sees one
// CONNECT tunnel and everything inside it rides one decision; on plain HTTP it
// re-decides per request. "Connection" is true for the first and understates
// the second, which is the safe direction.
export const APPROVAL_SCOPE_HINT: Record<ApprovalScope, string> = {
  once: "This one connection. The next attempt asks again.",
  run: "Every attempt until this run ends. (default)",
  until: "Every attempt until a time you pick, or this run ends.",
  always: "Also saves it to the workspace — future runs start with it.",
};
// Menu label, deny flavor — Phase 0 §2.
export const DENY_SCOPE_LABEL: Record<ApprovalScope, string> = {
  once: "Deny once",
  run: "Deny for this run",
  until: "Deny until…",
  always: "Deny always",
};
export const DENY_SCOPE_HINT: Record<ApprovalScope, string> = {
  once: "Blocks this one connection. The next attempt asks again.",
  run: "Blocks it for the rest of this run.",
  until: "Blocks it until a time you pick.",
  always: "Also saves the block to the workspace — future runs start blocked.",
};

// Preset durations behind "Until…" (Phase 0's control shape). ms, not sec —
// callers do `new Date(Date.now() + ms)`.
export const UNTIL_PRESETS: { label: string; ms: number }[] = [
  { label: "15 minutes", ms: 15 * 60_000 },
  { label: "1 hour", ms: 60 * 60_000 },
  { label: "8 hours", ms: 8 * 60 * 60_000 },
  { label: "24 hours", ms: 24 * 60 * 60_000 },
];

// The deny confirm dialog's description (Phase 0 §3) must not read "Denying
// blocks this host for the rest of the session — there is no undo and no
// re-raise once it's denied" — that is wrong for `once` (which re-raises on
// the next attempt) and wrong for `always` (which outlives "the session" AND
// can be undone in the workspace's egress settings). `until` must render the
// honest reason it's missing a time rather than silently rendering
// "undefined".
export function denyDialogCopy(scope: ApprovalScope, opts: { until?: string } = {}): string {
  switch (scope) {
    case "once":
      return "Denying blocks this one connection. The agent can try again, and you'll be asked again.";
    case "until":
      return `Denying blocks this host until ${opts.until ? shortTime(opts.until) : "the time you pick"}. After that you'll be asked again.`;
    case "always":
      return "Denying blocks this host for the rest of this run and saves the block to the workspace, so future runs start blocked. You can undo it in the workspace's egress settings.";
    case "run":
    default:
      return "Denying blocks this host for the rest of this run. The agent won't be able to reach it, and you won't be asked again.";
  }
}

// The blast-radius banner's egress_domain case (Phase 0 §4) — consumed by
// approvals.tsx's deriveBanner ONLY, always called with the "run" scope: the
// PENDING card previews what a plain Approve does, before any scope is
// chosen, and that preview never sees the scope picker inside ReasonDialog
// (a separate component). reason-dialog.tsx does NOT read this function — its
// own live per-option comparison is APPROVAL_SCOPE_HINT/DENY_SCOPE_HINT below,
// a deliberately shorter, host-less copy family (all four scopes shown side
// by side, so brevity matters more than the fuller prose here). The two
// families are NOT single-sourced and can drift; keep them saying the same
// thing about each scope by hand when either changes.
//
// The Phase 0 spec interpolates a {workspace} name into the `always` case;
// this console has no workspace-name lookup in hand on either call site (only
// workspace_ids — see runs.ts), so it says "the workspace" rather than
// inventing or fetching one. Upgrade path: thread a name through once a
// caller actually has one resolved.
export function egressBlastRadius(scope: ApprovalScope, host: string, until?: string): { what: string; blast: string } {
  switch (scope) {
    case "once":
      return {
        what: `One outbound connection to ${host}.`,
        blast: "That one connection. The next attempt stops and asks you again.",
      };
    case "until":
      return {
        what: `Outbound access to ${host} until ${until ? shortTime(until) : "the time you choose"}.`,
        blast: `The run can reach ${host} until then; after that it asks again.`,
      };
    case "always":
      return {
        what: `Outbound access to ${host}, saved to the workspace.`,
        blast: `This run and every future run of the workspace can reach ${host} without asking. Undo it in the workspace's egress settings.`,
      };
    case "run":
    default:
      return {
        what: `Outbound access to ${host} from this run.`,
        blast: `The run can reach ${host} until it ends. No other new domain opens.`,
      };
  }
}

// Disabled-`Always` reason, no-workspace case (Phase 0 §5) — the OTHER gate,
// non-operator, reuses OPERATOR_ONLY_REASON below rather than a second string.
export const ALWAYS_NEEDS_WORKSPACE = "Always needs a workspace — this run isn't attached to one.";

// Decided-row scope badge (Phase 0 §6) — "once" / "this run" / "until 5:04 PM"
// / "always" / "expired". undefined for a non-egress_domain kind, a row with
// no recorded decision (PENDING, or decided before this feature shipped), or
// EXPIRED (ExpireStale writes the zero scope deliberately — see
// internal/approval/approval.go — an expiry is a sweep nobody decided, not a
// scope worth badging). Callers pair this with the existing
// ApprovalStateBadge, which already says Approved/Denied — this is just the
// "· once" half.
export function approvalScopeBadge(
  item: Pick<ApprovalRequest, "kind" | "state" | "decision_scope" | "decision_expires_at">,
): string | undefined {
  if (item.kind !== "egress_domain") return undefined;
  if (item.state !== "APPROVED" && item.state !== "DENIED") return undefined;
  const scope = item.decision_scope;
  if (!scope) return undefined;
  if (scope === "until") {
    const expired = !!item.decision_expires_at && new Date(item.decision_expires_at).getTime() <= Date.now();
    return expired ? "expired" : item.decision_expires_at ? `until ${shortTime(item.decision_expires_at)}` : "until";
  }
  return APPROVAL_SCOPE_LABEL[scope].toLowerCase();
}

// D7 — the tag LiveApprovals shows on a pending row whose host matches the
// agent CLI's own known telemetry endpoint (WARDYN_ALLOW_AGENT_TELEMETRY
// opt-in, or a historical run from before that switch defaulted to
// suppressing it). Cockpit-adjacent, not a screen of its own — kept beside
// RUN_COCKPIT rather than folded into it.
export const TELEMETRY_TAG = {
  label: "Agent telemetry",
  title: "The agent CLI's usual diagnostics endpoint. Approve or deny it like any other host.",
};


// ui-approvals-2: the wire kind (ApprovalRequest.kind, e.g. "egress_domain")
// -> this file's copy-vocabulary kind (ApprovalKind above). Hoisted from
// approvals.tsx (its own kindLabel() reads it via APPROVAL_KIND_LABEL) so
// ApprovalKindChip (primitives.tsx) renders the SAME human label as every
// title/banner derived from APPROVAL_KIND_LABEL, instead of keeping a
// second, wire-shaped label table of its own.
export const WIRE_TO_COPY: Record<WireApprovalKind, ApprovalKind> = {
  credential: "credential",
  egress_domain: "egress",
  tool_call: "tool",
  // Its OWN kind, not "credential" (UX round B3): mapping it there would make
  // /approvals title the row "Mint a scoped credential" and paint the
  // blast-radius banner over a request that mints nothing.
  credential_reauth: "reauth",
};

// Hoisted from approvals.tsx (kept out of live-approvals.tsx's own module,
// which is a static import — see live-approvals.tsx's H2 comment for why the
// screen module itself can't be the source instead) so both files read the
// SAME kind-sniffing heuristic rather than keeping two that could drift.
export type CredentialKind = "git_pat" | "github_token" | "api_key" | "ssh_key" | "generic";
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
export function credentialKind(scope: Record<string, unknown>): CredentialKind {
  if ("repos" in scope || "permissions" in scope) return "github_token";
  if ("key_secret_ref" in scope) return "ssh_key";
  if ("header" in scope || "format" in scope) return "api_key";
  if ("secret_name" in scope || "username" in scope) return "git_pat";
  return "generic";
}

