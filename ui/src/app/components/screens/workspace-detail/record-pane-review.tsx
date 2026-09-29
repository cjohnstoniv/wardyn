/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Check, Info, Save, ShieldAlert, ShieldCheck, TriangleAlert } from "lucide-react";
import type { RecordResult, Workspace, WorkspaceProfile } from "../../../lib/types";
import { AuthModeLine, HonestyNote } from "./record-pane-chips";
import { approvedEgressSet, isEmptyCapture, egressPromotionDiff, policyNameFor } from "./session-helpers";
import { Observations } from "../profile-review";
import { Chip, OperatorOnlyHint, SectionLabel } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { useOperator } from "../../wardyn/operator-context";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";

// Per-session review card, shown once the open recording settles. Renders the
// same Observations block profile-review uses, a one-click egress-promotion
// diff, secrets proven-used chips, a Save-profile hand-off to the
// ProfileReview drawer, and the honesty notes.
export function RecordReviewCard({
  ws,
  sessionKey,
  rr,
  onPromoteEgress,
  onOpenProfile,
}: {
  ws: Workspace;
  sessionKey: string;
  rr: RecordResult;
  onPromoteEgress: (taskKey: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  const empty = isEmptyCapture(rr);

  // Empty capture is a failure, never a success — render the reachability hint and
  // stop (there are no trustworthy observations to promote from).
  if (rr.status === "record_failed" || empty) {
    return (
      <div
        className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
        data-testid="record-empty-capture"
      >
        <TriangleAlert className="mt-0.5 size-4 shrink-0" />
        <p>
          {rr.failure_hint ||
            "The recording captured no egress. This almost always means the sandbox couldn't reach the control plane to report its decisions (e.g. WSL2 NAT) — NOT that the task needs no egress. Fix reachability, then re-record."}
        </p>
      </div>
    );
  }

  // Egress promotion diff: hosts observed (allow_count>0) bucketed into
  // approvable / already-approved / platform-plumbing — one
  // function so a host can't land in more than one bucket. selfHost mirrors
  // the server's own control-plane-host exclusion; the console is always
  // same-origin with wardynd (lib/api/core.ts's relative BASE), so the
  // browser's own hostname IS that host.
  const diff = egressPromotionDiff(ws, sessionKey, window.location.hostname);
  const newHosts = diff.approvable;
  const alreadyApproved = diff.alreadyApproved;
  // Distinct from "nothing new because it's already allowed": these hosts
  // were never approvable at all (harness/control-plane plumbing), so
  // claiming "already allowed" would misattribute them to an operator
  // decision that never happened.
  const onlyPlumbingObserved = newHosts.length === 0 && alreadyApproved.length === 0 && diff.plumbing.length > 0;

  // Secrets proven-used = the workspace's declared required-secret names that this
  // run actually minted a grant for. Render-derived intersection — never mutates
  // the scan-owned profile.
  const profile = (ws.profile ?? {}) as WorkspaceProfile;
  const required = (profile.required_secrets ?? []).map((s) => s.name);
  const minted = rr.secret_names_minted ?? [];
  const proven = required.filter((n) => minted.includes(n));

  return (
    <div className="space-y-4 rounded-lg border border-border p-3" data-testid="record-review">
      {/* --- observed egress + one-click promotion --- */}
      <section className="space-y-2">
        <SectionLabel>Observed egress</SectionLabel>
        {/* egress_promoted is a boolean the server flips on any promoted>0, and
            which hosts a promote landed is not persisted — so a partial promote
            (now reachable: the confirm's checkboxes send a subset) can't be
            reported as "N of M". The only honest count is what's still
            approvable right now, straight from the buckets; the remainder keeps
            its listing and its Approve button instead of hiding behind a
            green all-done chip. */}
        {rr.egress_promoted && (
          <Chip tone={newHosts.length === 0 ? "success" : "warning"} dot>
            <Check className="size-3.5" />
            {newHosts.length === 0
              ? "Promoted"
              : `Promoted — ${newHosts.length} still need${newHosts.length === 1 ? "s" : ""} approval`}
          </Chip>
        )}
        {newHosts.length === 0 ? (
          !rr.egress_promoted && (
            <p className="text-meta text-muted-foreground">
              {onlyPlumbingObserved
                ? "Nothing needed approval — observed hosts were platform plumbing."
                : "No new hosts to approve — everything this task reached is already allowed."}
            </p>
          )
        ) : (
          <>
            <ul className="space-y-1.5" data-testid="record-new-hosts">
              {newHosts.map((h) => (
                <li key={h}>
                  <Mono className="text-foreground">{h}</Mono>
                </li>
              ))}
            </ul>
            <Button size="sm" variant="outline" onClick={() => onPromoteEgress(sessionKey)}>
              <Check className="size-3.5" /> Approve {newHosts.length} observed host
              {newHosts.length === 1 ? "" : "s"}
            </Button>
          </>
        )}
        {alreadyApproved.length > 0 && (
          <ul className="space-y-1 pt-1" aria-label="Already approved">
            {alreadyApproved.map((h) => (
              <li key={h} className="flex items-center gap-1.5 text-meta text-muted-foreground">
                <Check className="size-3 shrink-0" />
                <span className="font-mono line-through">{h}</span>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* --- secrets proven used (intersection, render-derived) --- */}
      {proven.length > 0 && (
        <section className="space-y-1.5">
          <SectionLabel>Secrets proven used</SectionLabel>
          <div className="flex flex-wrap gap-1.5" data-testid="record-proven-secrets">
            {proven.map((n) => (
              <Chip key={n} tone="info" mono>
                {n}
              </Chip>
            ))}
          </div>
        </section>
      )}

      {/* --- the raw observations block (same as profile-review) --- */}
      {rr.observations && <Observations observations={rr.observations} />}

      {/* --- honesty notes (all non-negotiable) --- */}
      <div className="space-y-1.5">
        {(rr.caveats?.length
          ? rr.caveats
          : [
              "Secret masking is seed-ahead: any secret NOT declared in Requirements that this open run touched is not masked in the logs or observations above. Treat anything here as sensitive.",
            ]
        ).map((c, i) => (
          <HonestyNote key={i} text={c} />
        ))}
        {rr.kernel_sensor_blind && (
          <HonestyNote text="This task recorded inside a hardware VM (Vault/CC3); the syscall sensor can't see into it, so exec, file-write, and connect observations may be incomplete. Egress (proxy-side) is still complete." />
        )}
      </div>

      {/* --- optional: persist this session's synthesized least-privilege profile --- */}
      <div className="flex justify-end">
        <Button
          size="sm"
          variant="ghost"
          onClick={() => onOpenProfile(rr.run_id, policyNameFor(ws.name, rr.label ?? "recorded"))}
        >
          <Save className="size-3.5" /> Save session profile
        </Button>
      </div>
    </div>
  );
}

// Review card for a settled confined replay. Unlike the open-record card
// (which promotes newly-observed hosts), this proves least privilege: it splits
// what the run reached into allowed (worked within the approved set), blocked
// (off-policy, denied live — the containment proof), and pending (first-use,
// awaiting approval). Blocked/pending hosts are one click to approve if they're
// legitimately needed. All counts come straight from the capture — no extra fetch.
export function ConfinedReviewCard({
  ws,
  rr,
  replayName,
  onApproveHosts,
  onOpenProfile,
}: {
  ws: Workspace;
  rr: RecordResult;
  // Present only where a re-replay is possible (a named session, not an
  // orphaned confined run) — gates the guided "approve selected + replay
  // again" button; the per-host Approve buttons render either way.
  replayName?: string;
  onApproveHosts: (hosts: string[], replayName?: string) => void;
  onOpenProfile: (runId: string, suggestedName?: string) => void;
}) {
  // record_failed is a real failure (couldn't reach the control plane to report) —
  // show the hint and stop; there's nothing trustworthy to render.
  if (rr.status === "record_failed") {
    return (
      <div
        className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
        data-testid="verify-session-failed"
      >
        <TriangleAlert className="mt-0.5 size-4 shrink-0" />
        <p>{rr.failure_hint || "The confined replay captured no egress decisions — fix reachability and re-run."}</p>
      </div>
    );
  }

  const domains = rr.observations?.domains ?? [];
  // Subtract the same union egressPromotionDiff subtracts (legacy
  // ApprovedEgress + profile.egress_domains + egress: requirement rows), not
  // ws.approved_egress alone — promote and the per-host approve both write the
  // requirements lane now, so a bucket reading only the legacy lane must not
  // list hosts that are already granted, under a button that would fold in
  // nothing. (Chip vs bucket can diverge, by design: the chip is the server's
  // verdict for the replay as it happened — an immutable fact — while these
  // buckets are "what is still off-policy right now". Approve a caught host
  // and the bucket empties while "Replayed — caught 2" stands, because it did.
  // The loop's answer to a stale verdict is a fresh replay, not a re-render.)
  const approved = approvedEgressSet(ws);
  const allowed = domains.filter((d) => d.allow_count > 0).map((d) => d.host);
  // One row per caught host: a host both denied and held must not render twice
  // (two independent filters), and the checkbox list below can't have a host
  // in two states at once. `denied` wins the label — it's the stronger fact,
  // and it's what defaults the checkbox off.
  const caught = domains
    .filter((d) => (d.deny_count > 0 || d.pending_count > 0) && !approved.has(d.host))
    .map((d) => ({ host: d.host, denied: d.deny_count > 0 }));

  return (
    <div className="space-y-4 rounded-lg border border-border p-3" data-testid="verify-session-review">
      <AuthModeLine rr={rr} />
      {/* worked within the approved set */}
      <section className="space-y-2">
        <SectionLabel>Ran within your approved access</SectionLabel>
        {allowed.length === 0 ? (
          <p className="text-meta text-muted-foreground">
            No egress captured yet — re-run your build/test/agent steps in the session above.
          </p>
        ) : (
          <p className="flex items-center gap-1.5 text-sm text-success">
            <ShieldCheck className="size-4" /> {allowed.length} host{allowed.length === 1 ? "" : "s"} reached,
            all allowed.
          </p>
        )}
      </section>

      {/* off-policy attempts caught — the containment proof, and the loop's
          one-step continuation (approve what was genuinely missed, replay). */}
      {caught.length > 0 && (
        <CaughtHosts caught={caught} replayName={replayName} onApproveHosts={onApproveHosts} />
      )}

      {/* the raw observations block (same as profile-review / open-record card) */}
      {rr.observations && <Observations observations={rr.observations} />}

      <div className="flex justify-end">
        <Button
          size="sm"
          variant="ghost"
          onClick={() => onOpenProfile(rr.run_id, policyNameFor(ws.name, rr.label ?? "recorded"))}
        >
          <Save className="size-3.5" /> Save session profile
        </Button>
      </div>
    </div>
  );
}

// The off-policy hosts a confined replay caught, each selectable, plus the
// loop's one-step continuation: approve the selection and replay again.
//
// Bulk-approving everything caught is a footgun the moment one of them is a
// genuine villain — which is the whole reason the replay ran. So a host that
// was denied live (deny_count > 0) starts unchecked and a merely-held one
// starts checked: the default is "approve the misses, leave the denials out",
// and changing it is a visible, deliberate click. The per-host Approve buttons
// stay for the one-off case (and route through the untrusted-content confirm
// exactly as before).
function CaughtHosts({
  caught,
  replayName,
  onApproveHosts,
}: {
  caught: { host: string; denied: boolean }[];
  replayName?: string;
  onApproveHosts: (hosts: string[], replayName?: string) => void;
}) {
  // A settled replay's observations are immutable, so seeding once is right —
  // and it means an operator's un/checking is never stomped by the detail
  // page's poll. (A re-replay unmounts this card via the "replaying" stage.)
  // approveHosts writes PUT /workspaces/{id}/requirements — operatorOnly, not
  // the securityOps tier the pane's fieldset gates. Without this second gate a
  // security admin would get live Approve buttons over a route the server
  // refuses, and the guided approve→replay chain would silently never fire (F031).
  const operator = useOperator();
  const [selected, setSelected] = React.useState<Set<string>>(
    () => new Set(caught.filter((c) => !c.denied).map((c) => c.host)),
  );
  // Intersect with what's still caught: approving a host shrinks the list
  // under us, and a stale selection must never widen the next write.
  const picked = caught.filter((c) => selected.has(c.host)).map((c) => c.host);

  return (
    <section className="space-y-2" data-testid="verify-session-blocked">
      <SectionLabel>Off-policy attempts caught</SectionLabel>
      <ul className="space-y-1.5">
        {caught.map(({ host, denied }) => (
          <li key={host} className="flex items-center gap-2">
            <Checkbox
              id={`caught-${host}`}
              checked={selected.has(host)}
              onCheckedChange={(v) =>
                setSelected((prev) => {
                  const next = new Set(prev);
                  if (v === true) next.add(host);
                  else next.delete(host);
                  return next;
                })
              }
              aria-label={`Approve ${host}`}
            />
            {denied ? (
              <ShieldAlert className="size-3.5 shrink-0 text-danger" />
            ) : (
              <Info className="size-3.5 shrink-0 text-warning" />
            )}
            <label htmlFor={`caught-${host}`} className="flex-1 cursor-pointer">
              <Mono className="text-foreground">{host}</Mono>
            </label>
            <span className={denied ? "text-meta text-danger" : "text-meta text-warning"}>
              {denied ? "blocked" : "pending approval"}
            </span>
            <Button
              size="sm"
              variant="outline"
              className="h-7"
              disabled={!operator}
              onClick={() => onApproveHosts([host])}
            >
              <Check className="size-3.5" /> Approve
            </Button>
            {!operator && <OperatorOnlyHint />}
          </li>
        ))}
      </ul>
      {replayName && (
        <Button
          size="sm"
          disabled={!operator || picked.length === 0}
          onClick={() => onApproveHosts(picked, replayName)}
        >
          <ShieldCheck className="size-3.5" /> Approve {picked.length} selected host
          {picked.length === 1 ? "" : "s"} and replay again
        </Button>
      )}
      {replayName && !operator && <p className="text-meta text-muted-foreground">{OPERATOR_ONLY_REASON}</p>}
      <p className="text-meta leading-snug text-muted-foreground">
        These were denied or held for approval because they aren&apos;t in your approved set. Approve one
        only if this workspace legitimately needs it — otherwise leave it blocked. Anything denied live
        starts unchecked.
      </p>
    </section>
  );
}

