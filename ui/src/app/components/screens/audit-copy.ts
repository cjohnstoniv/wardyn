/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { AuditEvent } from "../../lib/types";
import { ERASED_VALUE, isNetworkFault } from "../../lib/types";

// #459 — docs/design/announced-failures-canon.md's AUDIT.MEMBER_FEED_TITLE.
// Pulled out of audit.tsx's inline JSX literal (W25-W25.2-3's member-feed
// EmptyState title) into its own constant, byte-identical, so it can be
// pinned against the canon doc (owner ruling 2026-09-25, #726).
export const AUDIT = {
  MEMBER_FEED_TITLE: "The full audit feed is admin-only.",
  // Mock packet M4 (approved 2026-10-03), surface D: a sealed field whose key
  // was destroyed.
  ERASED: "Erased",
  ERASED_HINT: "Erased on request. The event and its place in the log remain.",
  // What a credential.* row says about a person's stored sign-in. A
  // credential.revoke row records the end of a run's OWN credentials and
  // touches no stored sign-in, with or without its `scope` field (rows written
  // before it existed are the ones that misled).
  REVOKE_RUN_CREDENTIALS: "Recorded the end of this run's own credentials. The person's sign-in is not affected.",
  EXPIRED_DELETE_REFUSED: "Removed a stored sign-in the provider refused",
  EXPIRED_DELETE_LOST_REPLY:
    "Removed a stored sign-in: the provider may have accepted a renewal whose reply was lost",
  EXPIRED_DELETE_EXPIRED: "Removed a stored sign-in that had expired",
  // The same three rows with a failure outcome: the deletion did not happen
  // and the sign-in is still stored.
  EXPIRED_DELETE_REFUSED_FAILED: "Could not remove a stored sign-in the provider refused",
  EXPIRED_DELETE_LOST_REPLY_FAILED:
    "Could not remove a stored sign-in after a renewal whose reply may have been lost",
  EXPIRED_DELETE_EXPIRED_FAILED: "Could not remove a stored sign-in that had expired",
  // A run.build row the image cache answered (data.cache_hit): nothing was built.
  REUSED_IMAGE: "Reused the cached sandbox image",
} as const;

// Short, human verbs for the actions we know about, shared by the Audit screen
// and a run's own Audit tab. Anything not in this table falls back to the raw
// dotted action — a guess at prose for a backend action we
// don't recognize would be dishonest; the raw verb is always true.
const ACTION_VERB: Record<string, string> = {
  "run.create": "created the run",
  "run.build": "built the sandbox image",
  "run.dispatch": "dispatched the run",
  "run.interactive.start": "started the run idle for attach",
  "run.exec": "executed the agent task",
  // Not "completed": the same action records a run that ended in failure, whose row
  // carries a red failure badge.
  "run.complete": "the run ended",
  "run.kill": "killed the run",
  "run.revoke": "revoked the run's identity",
  "run.autostop": "auto-stopped the idle run",
  "run.reconcile": "reconciled run state",
  // The EFFECTIVE policy the sandbox was actually given, recorded after every
  // widening phase, so this is what the proxy enforces, not what was asked for.
  "run.policy.resolve": "resolved the policy this run was given",
  // The operator's corporate upstream proxy, resolved per dispatch. A failure
  // row means the run went out with direct egress, not that the run was refused.
  "run.upstream_proxy.resolve": "checked the corporate proxy setting for the run's egress",
  "run.workspace.collide": "detected a workspace directory collision",
  "run.requirement.allow": "applied a workspace's required egress to the run",
  "run.requirement.grant": "granted a workspace's required secret to the run",
  "run.requirement.inject": "applied a workspace's required integration to the run",
  "run.record.synthesize": "synthesized a least-privilege profile from the recording",
  "credential.mint": "minted a credential",
  "identity.mint": "minted a workload identity",
  "identity.revoke": "revoked a workload identity",
  "run.bedrock.configure": "configured Bedrock model access for the run",
  // Deliberately NOT "the managed subscription credential": the same action
  // fires for source="subscription" (the operator's own resident Anthropic
  // OAuth token) as for source="managed" (the Wardyn-captured one), and only
  // the event's data carries which — naming one mode would be false on half
  // the rows.
  "run.subscription.inject": "injected the subscription credential at the proxy",
  "secret.read": "read a secret into the run",
  "secret.write": "stored a secret",
  "secret.delete": "deleted a secret",
  "approval.decide": "decided an approval request",
  "approval.expire": "an approval request expired",
  "policy.create": "created a policy",
  "policy.update": "updated a policy",
  "policy.delete": "deleted a policy",
  "policy.inline.apply": "applied an inline policy",
  "session.attach": "attached to the run's terminal",
  "session.detach": "detached from the run's terminal",
  "session.recording.write": "recorded the terminal session",
  "recording.upload": "uploaded the run recording",
  "ssh.authenticate": "an ssh authentication attempt against a run's terminal",
  "ssh.exec": "ran a command over the run's ssh channel",
  "ssh.sftp.transfer": "an sftp session against the run's sandbox",
  // pending H11-P2: recommended text until the owner answers; the packet names no label for wardyn sync's own row.
  "ssh.sync.transfer": "a wardyn sync session against the run's sandbox",
  "ssh.forward": "an ssh port forward into the run's sandbox",
  "kernel.process.exec": "observed a process exec",
  "kernel.network.connect": "observed a network connect",
  "kernel.file.write": "observed a write to a sensitive path",
  "kernel.sensor.ping": "sensor heartbeat",
  "kernel.sensor.bypass": "kernel sensor blind — no ground truth for this run",
  // DEADCODE-4: the tier-1/tier-2/integration surfaces' own actions — shipped
  // with zero rows here, so they rendered as raw dotted strings.
  "source.write": "added a source",
  "source.delete": "removed a source",
  "source.scan": "scanned a source",
  "base_image.write": "added a base image",
  "base_image.delete": "removed a base image",
  "integration.write": "stored an integration",
  "integration.delete": "deleted an integration",
  "workspace.requirement.write": "added a workspace requirement",
  "workspace.create": "created a workspace",
  "setup.onboarding.complete": "finished setup",
  "authz.denied": "was refused access to",
  "auth.fail": "failed to sign in to",
};

function capitalize(s: string): string {
  return s.length ? s[0].toUpperCase() + s.slice(1) : s;
}

function egressVerb(e: AuditEvent): string {
  const decision = e.action.slice("egress.".length);
  if (isNetworkFault(e)) return "Failed egress";
  if (decision === "allow") return "Allowed egress";
  if (decision === "deny") return "Denied egress";
  return "Deferred egress";
}

// The sentence for a row, plus how the row's target joins it. egress.* and
// llm.scan.* are dynamic families (the suffix is the decision/scan outcome), so
// they're handled directly rather than enumerated in ACTION_VERB.
interface Described {
  head: string;
  join?: { sep: string; fallback?: string };
}

function describe(e: AuditEvent): Described {
  if (e.action.startsWith("egress.")) {
    return { head: egressVerb(e), join: { sep: " to ", fallback: "an unknown host" } };
  }
  if (e.action.startsWith("llm.scan.")) {
    return {
      head: `LLM content scan (${e.action.slice("llm.scan.".length)})`,
      join: { sep: " for ", fallback: "an unknown host" },
    };
  }
  if (e.action === "credential.revoke") return { head: AUDIT.REVOKE_RUN_CREDENTIALS };
  // "Removed" only on a row whose deletion happened; a failure row says what
  // was not removed. Any other outcome reads as the raw line.
  if (e.action === "credential.expired.delete" && (e.outcome === "success" || e.outcome === "failure")) {
    const ok = e.outcome === "success";
    const reason = e.data?.reason;
    if (reason === "invalid_grant") {
      if (e.data?.after_lost_reply === true) {
        return { head: ok ? AUDIT.EXPIRED_DELETE_LOST_REPLY : AUDIT.EXPIRED_DELETE_LOST_REPLY_FAILED };
      }
      return { head: ok ? AUDIT.EXPIRED_DELETE_REFUSED : AUDIT.EXPIRED_DELETE_REFUSED_FAILED };
    }
    if (reason === "expired") return { head: ok ? AUDIT.EXPIRED_DELETE_EXPIRED : AUDIT.EXPIRED_DELETE_EXPIRED_FAILED };
  }
  const join = { sep: " — " };
  if (e.action === "run.build" && e.data?.cache_hit === true) return { head: AUDIT.REUSED_IMAGE, join };
  const verb = ACTION_VERB[e.action];
  return { head: verb ? capitalize(verb) : e.action, join };
}

// An erased target reads as erased, never as the sentinel (surface D).
function shown(e: AuditEvent): AuditEvent {
  return e.target === ERASED_VALUE ? { ...e, target: AUDIT.ERASED } : e;
}

/** The row's sentence with no target: what happened, for a surface that shows
 *  the target in a column of its own. The raw key stays the caller's title=. */
export function describeAction(e: AuditEvent): string {
  return describe(shown(e)).head;
}

/** The row's whole sentence, target included, built only from real fields. */
export function describeEvent(e: AuditEvent): string {
  e = shown(e);
  const { head, join } = describe(e);
  if (!join) return head;
  const target = e.target || join.fallback;
  return target ? `${head}${join.sep}${target}` : head;
}
