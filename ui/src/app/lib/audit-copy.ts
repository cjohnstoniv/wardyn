/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The plain-English wording of an audit event, shared by the Audit screen and the run page's Audit tab.
import type { AuditEvent } from "./types";
import { ERASED_VALUE, isNetworkFault } from "./types";
import { AUDIT } from "../components/screens/audit-copy";

// Short, human verbs for the actions we know about. Anything not in this table
// falls back to the raw dotted action — a guess at prose for a backend action we
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
  // A service's health read from inside the sandbox, and a restart of its command (rows written by wardynd).
  "run.service.health": "service health changed",
  "run.service.restart": "restarted the service",
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
  "run.aws_cli.configure": "configured the AWS CLI for the run",
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
  // One row per Pause-to-Resume session in a VM's desktop (#2972, board R11-2972-DesktopSignIns).
  "desktop.sign_in.pause": "paused the agent to sign in inside the desktop",
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
  "base_image.admit": "checked a prewrapped image",
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

/** A sentence in two parts: `lead` reads alone, `object` is the target it is about (empty when none). */
interface AuditParts {
  lead: string;
  sep: string;
  object: string;
}

const whole = (lead: string): AuditParts => ({ lead, sep: "", object: "" });

// Human description built ONLY from real fields (action + target). egress.* and
// llm.scan.* are dynamic families (the suffix is the decision/scan outcome), so
// they're handled directly rather than enumerated in ACTION_VERB.
function describeParts(e: AuditEvent): AuditParts {
  // An erased target reads as erased, never as the sentinel (surface D).
  if (e.target === ERASED_VALUE) e = { ...e, target: AUDIT.ERASED };
  // #3036: a target that is the row's own run id says nothing the run reference beside it doesn't.
  if (e.target && e.target === e.run_id) e = { ...e, target: "" };
  if (e.action.startsWith("egress.")) {
    const decision = e.action.slice("egress.".length);
    const verb = isNetworkFault(e)
      ? "Failed egress to"
      : decision === "allow"
        ? "Allowed egress to"
        : decision === "deny"
          ? "Denied egress to"
          : "Deferred egress to";
    return { lead: verb, sep: " ", object: e.target || "an unknown host" };
  }
  if (e.action.startsWith("llm.scan.")) {
    return { lead: `LLM content scan (${e.action.slice("llm.scan.".length)}) for`, sep: " ", object: e.target || "an unknown host" };
  }
  if (e.action === "inbound.allow" || e.action === "inbound.deny") {
    const verb = e.action === "inbound.allow" ? AUDIT.INBOUND_ALLOW : AUDIT.INBOUND_DENY;
    return { lead: `${verb} to`, sep: " ", object: e.target || AUDIT.INBOUND_NO_APP };
  }
  if (e.action === "credential.revoke") return whole(AUDIT.REVOKE_RUN_CREDENTIALS);
  if (e.action === "run.end.set" && e.data?.reason === "service_renewed") {
    return { lead: capitalize(AUDIT.SERVICE_RENEWED), sep: " — ", object: e.target ?? "" };
  }
  // "Removed" only on a row whose deletion happened; a failure row says what
  // was not removed. Any other outcome reads as the raw line.
  if (e.action === "credential.expired.delete" && (e.outcome === "success" || e.outcome === "failure")) {
    const ok = e.outcome === "success";
    const reason = e.data?.reason;
    if (reason === "invalid_grant") {
      if (e.data?.after_lost_reply === true) {
        return whole(ok ? AUDIT.EXPIRED_DELETE_LOST_REPLY : AUDIT.EXPIRED_DELETE_LOST_REPLY_FAILED);
      }
      return whole(ok ? AUDIT.EXPIRED_DELETE_REFUSED : AUDIT.EXPIRED_DELETE_REFUSED_FAILED);
    }
    if (reason === "expired") return whole(ok ? AUDIT.EXPIRED_DELETE_EXPIRED : AUDIT.EXPIRED_DELETE_EXPIRED_FAILED);
  }
  if (e.action === "run.build" && e.data?.cache_hit === true) {
    return { lead: AUDIT.REUSED_IMAGE, sep: " — ", object: e.target ?? "" };
  }
  const verb = ACTION_VERB[e.action];
  return { lead: verb ? capitalize(verb) : e.action, sep: " — ", object: e.target ?? "" };
}

export function describeEvent(e: AuditEvent): string {
  const { lead, sep, object } = describeParts(e);
  return object ? `${lead}${sep}${object}` : lead;
}

/** The sentence without its target, for a row that shows the target in a column of its own. */
export function describeAction(e: AuditEvent): string {
  return describeParts(e).lead;
}

/** The target for its own column: the row's target as it is, or the sentence's stand-in ("an unknown host"). */
export function describeObject(e: AuditEvent): string {
  return e.target || describeParts(e).object;
}

export const isInbound = (e: AuditEvent): boolean => e.action === "inbound.allow" || e.action === "inbound.deny";

/** An inbound.deny that carries no actor: the request showed no valid credential, so there is no caller to name. */
export const hasNoCaller = (e: AuditEvent): boolean => e.action === "inbound.deny" && !e.actor;

/** Where a service row's fact came from: "sandbox" for what wardynd read inside the run, "server" for a lease it renewed itself. Null for any other row. */
export function serviceSource(e: AuditEvent): string | null {
  if (e.action === "run.service.health" || e.action === "run.service.restart") return AUDIT.SOURCE_SANDBOX;
  return e.action === "run.end.set" && e.data?.reason === "service_renewed" ? AUDIT.SOURCE_SERVER : null;
}

// inboundDetail is the second line of an inbound.allow / inbound.deny row, from what the row recorded: the request
// (method and path, never the query), how the caller came (a token or a session) and who they matched, or why they
// were refused. `withRun` ends the line with the run's short id, for the Audit page, which lists several runs; a run's
// own Audit tab leaves it off. Null for any other row; a field the row lacks is left out, never guessed.
export function inboundDetail(e: AuditEvent, withRun = false): string | null {
  if (!isInbound(e)) return null;
  const d = e.data ?? {};
  const text = (k: string) => (typeof d[k] === "string" && d[k] ? (d[k] as string) : "");
  const request = [text("method"), text("path")].filter(Boolean).join(" ");
  const via = text("via") && `via ${text("via")}`;
  const parts =
    e.action === "inbound.allow"
      ? [request, via, text("caller") && `caller ${text("caller")}`]
      : [text("reason") && `reason ${text("reason")}`, via, request];
  const line = parts.filter(Boolean);
  if (line.length && withRun && e.run_id) line.push(e.run_id.replace(/^run_/, "").slice(0, 8));
  return line.join(" · ") || null;
}

const BEDROCK_SIGNING: Record<string, "proxy" | "role" | "session"> = {
  "sso-sigv4-proxy": "proxy",
  "sso-inject-proxy": "role",
  "sso-inject": "session",
};

// awsSigningLine is how dispatch set a run's AWS sign-in lane up, from its run.bedrock.configure or
// run.aws_cli.configure row, or null for any other row. A Bedrock row is read by its recorded mode (an
// older row carries no aws_signing), so only the three sign-in modes get a line; bearer and an unknown
// mode get none. Where the sign-in session sits comes from the mode, never from the reason.
export function awsSigningLine(e: AuditEvent): { text: string; sandbox: boolean } | null {
  const d = e.data ?? {};
  let where: "proxy" | "role" | "session" | null = null;
  if (e.action === "run.bedrock.configure") {
    where = Object.prototype.hasOwnProperty.call(BEDROCK_SIGNING, String(d.mode)) ? BEDROCK_SIGNING[String(d.mode)] : null;
  } else if (e.action === "run.aws_cli.configure") {
    where = d.aws_signing === "proxy" ? "proxy" : d.aws_signing === "sandbox" ? "role" : null;
  }
  if (where === null) return null;
  if (where === "proxy") return { text: AUDIT.AWS_SIGNING_PROXY, sandbox: false };
  const code = typeof d.aws_signing_reason === "string" ? d.aws_signing_reason : "";
  const host = typeof d.aws_signing_host === "string" ? d.aws_signing_host : "";
  const reason =
    code === ""
      ? AUDIT.AWS_SIGNING_NOT_RECORDED
      : code === "port"
        ? AUDIT.AWS_SIGNING_PORT(host)
        : code === "unsigned_host"
          ? AUDIT.AWS_SIGNING_UNSIGNED_HOST(host)
          : Object.prototype.hasOwnProperty.call(AUDIT.AWS_SIGNING_REASON, code)
            ? AUDIT.AWS_SIGNING_REASON[code]
            : code;
  return { text: where === "session" ? AUDIT.AWS_SIGNING_SANDBOX_SESSION(reason) : AUDIT.AWS_SIGNING_SANDBOX(reason), sandbox: true };
}
