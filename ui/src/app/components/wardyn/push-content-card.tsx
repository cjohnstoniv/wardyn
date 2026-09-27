/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// PushContentCard — the console's rendering of a push_content approval (#181,
// built on #180/#494's server side): a brokered git push that matched
// push_rules.require_review_paths and is parked at the proxy for an admin's
// decision (internal/egress/proxy/push_hold.go). Shared by
// screens/approvals.tsx's PendingCard and live-approvals.tsx's strip, the
// same two-mount pattern ado-capability-card.tsx uses and for the same
// reason: a held push needs fields (repository, branch, the paths under
// review) neither surface's generic row can show.
//
// ADMIN-ONLY, NO OWNERSHIP CARVE-OUT (unlike Azure DevOps's escalation card):
// canDecideApproval(securityOperator, "push_content") falls through to the
// same operator-only branch credential/tool_call take — a member approving
// their own run's workflow-file edit is the exfiltration the rule exists to
// stop (see internal/api/approvals_push.go's own doc). So this card takes no
// run-owner/viewerPrincipal props at all, only `run.state` — to withdraw the
// decision pair once the run has ended, the same defensive check
// screens/approvals.tsx's generic PendingCard makes for every other kind.
//
// NO SCOPE MENU: a push decision covers this push only (decide's rule 4
// refuses a decision_scope on this kind — approvals_push.go). Approve/Deny
// call straight through to the API with no ReasonDialog stop and no staged
// scope, the same direct-decide shape the ADO card's own onApprove/onDeny
// take, and for the same reason: the frozen mock (packet 7) draws this
// card's own control, not the generic reason dialog.
import * as React from "react";
import { Check, Loader2, X } from "lucide-react";
import type { AgentRun, ApprovalRequest } from "../../lib/types";
import { isHeld, isTerminalRunState, type PushContentScope } from "../../lib/types";
import { relativeTime } from "../../lib/format";
import { PUSH } from "./copy/push";
import { APPROVAL, APPROVAL_BANNER_LABEL, SECURITY_ONLY_REASON } from "./copy";
import { Button } from "../ui/button";
import { ApprovalKindChip, ApprovalStateBadge, Chip } from "./primitives";

// Lives here rather than beside isAdoCapabilityRequest/isAdoConsentRequest in
// lib/types/approvals.ts: that module is eager (isHeld is imported into the
// runs board — see board-groups.ts's own bundle-split note), and this guard
// is only ever called from the three lazy screens that already import
// PushContentCard below (live-approvals.tsx, run-detail-approvals-tab.tsx,
// screens/approvals.tsx). Keeping it here instead of the barrel keeps it out
// of the entry chunk; isHeld's own push_content branch checks `a.kind`
// directly and never calls this.
export function isPushContentRequest(
  a: Pick<ApprovalRequest, "kind" | "requested_scope">,
): a is ApprovalRequest & { requested_scope: PushContentScope } {
  return a.kind === "push_content";
}

// unquoteGitPath decodes a path exactly as internal/egress/proxy/push_hold.go's
// quotePath produced it (git's own core.quotePath=true / quote_c_style):
// quotePath leaves a path with no control byte, no '"'/'\\' and no byte past
// ASCII UNCHANGED (no wrapping quotes at all); anything else it double-quotes,
// with \" and \\ for those two bytes, \a\b\t\n\v\f\r for 0x07-0x0d, and a
// three-digit octal escape for every other such byte. A review-matched
// "café.yml" therefore arrives on the wire as `"caf\303\251.yml"`.
//
// DISPLAY ONLY: this walks the escapes back to raw BYTES (never runes — one
// octal escape is one UTF-8 byte, and a real character can take several in a
// row) and decodes the byte string as UTF-8 only at the very end. Every other
// reader of a push_content path — the stored list, the audit export, the
// dedup digest — keeps the quoted wire form exactly as the sidecar sent it;
// nothing about what is stored, exported or approved changes here.
//
// A string that isn't quoted at all is returned unchanged (quotePath's own
// "nothing special" case). Anything this can't parse as a well-formed
// quote_c_style string — an unterminated quote, an unrecognized escape, a
// byte sequence that isn't valid UTF-8 once decoded — falls back to the RAW
// input rather than guess at a name.
//
// SECURITY (this is an APPROVAL surface): decoding must never let a path
// spoof what it names to the approver. Two ways it could:
//  - a control-byte escape (\a\b\t\n\v\f\r, or a raw octal like \033) puts an
//    actual control character on screen — a bare \n or ESC can rewrite what a
//    terminal-backed viewer, or a careless CSS rule, shows;
//  - an octal escape can assemble a Unicode FORMAT/bidi character no escape
//    shorthand names at all — U+202E RIGHT-TO-LEFT OVERRIDE turns
//    "evil<RLO>txt.sh" into something that reads as "evilhs.txt".
// So \a\b\t\n\v\f\r are not decoded at all — those bytes fall back to the
// raw wire form via the "unrecognized escape" branch below, same as any
// other malformed escape — and the fully decoded string is refused (falling
// back to the raw quoted form) if it contains any C0/C1 control character
// (Cc) or Unicode format/bidi character (Cf: U+200B-200F, U+202A-202E,
// U+2060-2064, U+2066-2069, U+FEFF among others). Only printable text
// (accented letters, CJK, and so on) is ever decoded for display.
export function unquoteGitPath(raw: string): string {
  if (raw.length < 2 || raw[0] !== '"' || raw[raw.length - 1] !== '"') return raw;
  const inner = raw.slice(1, -1);
  const bytes: number[] = [];
  for (let i = 0; i < inner.length; i++) {
    const c = inner[i];
    if (c === "\\") {
      const next = inner[i + 1];
      if (next === '"' || next === "\\") {
        bytes.push(next.charCodeAt(0));
        i += 1;
      } else {
        // No \a\b\t\n\v\f\r shorthand decode (see SECURITY doc above) — only
        // octal survives past this point, and only if the post-decode
        // control/format check below lets the result through.
        const octal = inner.slice(i + 1, i + 4);
        if (!/^[0-7]{3}$/.test(octal)) return raw; // unrecognized escape — malformed
        bytes.push(parseInt(octal, 8));
        i += 3;
      }
    } else {
      const code = c.charCodeAt(0);
      // quotePath escapes every byte outside plain printable ASCII, so a
      // literal byte inside the quotes must already be one — anything else
      // means this string was never quotePath's own output.
      if (code >= 0x80) return raw;
      bytes.push(code);
    }
  }
  let decoded: string;
  try {
    // fatal: true refuses to substitute U+FFFD for invalid UTF-8 — past a
    // single malformed escape the rest of the byte string may not decode.
    decoded = new TextDecoder("utf-8", { fatal: true }).decode(new Uint8Array(bytes));
  } catch {
    return raw;
  }
  // Cc (control) + Cf (format/bidi, the RLO/ZWSP/BOM family included) — the
  // check that makes decoding safe on an approval surface. Any hit refuses
  // the WHOLE decode, not just the offending character.
  if (/\p{Cc}|\p{Cf}/u.test(decoded)) return raw;
  return decoded;
}

// The run this card needs to know about — only whether it has ENDED (the
// same defensive re-check screens/approvals.tsx's generic PendingCard makes:
// a terminal run's PENDING approvals are cancelled by the lifecycle cascade a
// beat later, so offering Approve/Deny in that window would be a dead
// control). No `created_by`: decidability here is role-only, never ownership.
export type PushCardRun = Pick<AgentRun, "state">;

export function PushContentCard({
  item,
  securityOperator,
  run,
  busy,
  onApprove,
  onDeny,
}: {
  item: ApprovalRequest & { requested_scope: PushContentScope };
  securityOperator: boolean;
  run: PushCardRun | null | undefined;
  /** Which decide call is in flight, else null — both buttons disable, only
   *  the pressed one spins (#458, the same shape as ado-capability-card.tsx). */
  busy: "approve" | "deny" | null;
  onApprove: () => void;
  onDeny: () => void;
}) {
  const scope = item.requested_scope;
  const runEnded = !!run && isTerminalRunState(run.state);
  const shown = scope.paths ?? [];
  const moreCount = Math.max(0, (scope.paths_total ?? shown.length) - shown.length);

  // Review finding 2 — the proxy's own hold is BOUNDED (isHeld's own
  // push_content doc, lib/types/approvals.ts): HELD_NOTE ("lets it through
  // now") is only true while it is, so the card has to flip to HELD_EXPIRED
  // on its own once the window passes, the same live-timer shape
  // ado-capability-card.tsx's held-state timer takes — a poll tick
  // eventually catches it too, but a member sitting on this card between
  // ticks must not keep reading a promise that already lapsed.
  //
  // #1197 L1b: the window itself is now item.held_until (the server's own
  // projection, internal/approval.Hold), not a client-guessed ceiling.
  const [, forceRerenderAtWindowEnd] = React.useReducer((n: number) => n + 1, 0);
  React.useEffect(() => {
    if (!item.held_until) return;
    const msLeft = Date.parse(item.held_until) - Date.now();
    if (!(msLeft > 0)) return; // already past the window (or unparseable)
    const id = setTimeout(forceRerenderAtWindowEnd, msLeft);
    return () => clearTimeout(id);
  }, [item.held_until]);
  const held = isHeld(item);

  return (
    <div className="rounded-xl border border-warning/30 bg-warning/5 p-4" data-testid="push-content-card">
      <div className="flex flex-wrap items-center gap-2">
        <ApprovalKindChip kind="push_content" />
        <span className="text-sm font-semibold text-foreground">{PUSH.CARD_TITLE}</span>
        <span className="ml-auto">
          <ApprovalStateBadge state={item.state} />
        </span>
      </div>

      <dl className="mt-3 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-muted-foreground">{PUSH.FIELD_REPOSITORY}</dt>
        <dd className="font-mono text-xs text-foreground">{scope.repo}</dd>
        <dt className="text-muted-foreground">{PUSH.FIELD_BRANCH}</dt>
        <dd className="font-mono text-xs text-foreground">{scope.branch}</dd>
        {/* acts_as_label ONLY — never scope.acts_as, a credential reference
            ("<grant kind>:<uuid>"), not a principal. */}
        <dt className="text-muted-foreground">{PUSH.FIELD_ACTS_AS}</dt>
        <dd className="text-xs text-foreground">{scope.acts_as_label || "—"}</dd>
      </dl>

      <div className="mt-3 space-y-1 rounded-lg border border-border bg-background px-3 py-2.5 text-sm leading-relaxed">
        <p className="text-foreground">
          <span className="font-semibold">{APPROVAL_BANNER_LABEL.what}</span>{" "}
          {PUSH.WHAT(scope.repo, scope.acts_as_label || "—")}
        </p>
        <p className="text-muted-foreground">
          <span className="font-semibold text-foreground/80">{APPROVAL_BANNER_LABEL.blast}</span> {PUSH.BLAST}
        </p>
      </div>

      <div className="mt-3">
        <p className="text-xs font-semibold text-foreground">{PUSH.PATHS_TITLE}</p>
        {/* Q181-2: ten paths, then "+N more" — no expanding. The audit row
            holds the list's digest; the audit export inlines the full list
            (paths_total may exceed the ten names here), not a control on this
            card. Each path is UNQUOTED for display only (unquoteGitPath) —
            the key stays the raw wire string, which is still unique. */}
        <ul className="mt-1 space-y-0.5">
          {shown.map((p) => {
            const display = unquoteGitPath(p);
            return (
              <li key={p} className="truncate font-mono text-xs text-muted-foreground" title={display}>
                {display}
              </li>
            );
          })}
        </ul>
        {moreCount > 0 && <p className="mt-1 text-xs text-muted-foreground">{PUSH.PATHS_MORE(moreCount)}</p>}
        <p className="mt-1.5 text-xs text-muted-foreground">{PUSH.PATHS_NOTE}</p>
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border/60 pt-3">
        {runEnded ? (
          <p className="max-w-[72ch] text-xs text-muted-foreground">{APPROVAL.CANCELLED_BODY}</p>
        ) : securityOperator ? (
          <>
            <Button size="sm" variant="info" disabled={busy !== null} onClick={onApprove}>
              {busy === "approve" ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />} Approve
            </Button>
            <Button size="sm" variant="outline" disabled={busy !== null} onClick={onDeny}>
              {busy === "deny" ? <Loader2 className="size-4 animate-spin" /> : <X className="size-4" />} Deny
            </Button>
          </>
        ) : (
          <Chip tone="neutral">{SECURITY_ONLY_REASON}</Chip>
        )}
        <span className="ml-auto text-xs text-muted-foreground" title={item.requested_at}>
          requested {relativeTime(item.requested_at)}
        </span>
      </div>

      {!runEnded && (
        <>
          <p className="mt-2.5 max-w-[72ch] text-xs text-muted-foreground">
            {held ? PUSH.HELD_NOTE : PUSH.HELD_EXPIRED}
          </p>
          {securityOperator && <p className="mt-1 max-w-[72ch] text-xs text-muted-foreground">{PUSH.APPROVE_NOTE}</p>}
        </>
      )}
    </div>
  );
}
