/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The member half of permissioning (0.6 pillar 2): the caller's own effective
// capability set, and the two questions the why-denied surfaces ask of it.
//
// This is ADVISORY ONLY. The enforcement points are server-side —
// authorizeUserDecision, resolveRunPolicy's member branch, denyUserRequest
// and handleListSecrets — and every one of them re-resolves the grants itself.
// What lives here exists so a member is told WHY before they click, instead of
// discovering it as a bare 403.
//
// The matchers below mirror internal/api/capabilities.go's capValueMatches AND
// capValueOverlaps exactly, including the host semantics they borrow from
// entryCoversAny. Two matchers that disagree is how a refusal becomes
// unexplainable, so any change to one belongs in the other in the same commit.
// capabilities.test.ts holds the shared table (CAP_MATCH_CASES /
// CAP_OVERLAP_CASES) that pins this file against the Go one row for row.
import * as React from "react";
import { permissions } from "./api/permissions";
import type { CapabilityKind } from "./permissions-copy";
import type { CapabilityGrant, MeCapabilities } from "./types";

// egressEntryHost (internal/api/runs_dispatch_gitbroker.go): lowercase, drop a
// ":port" suffix, drop a trailing dot. An entry with more than one ":" is an
// IPv6 literal and is never cut at a colon (ipv6EntryHost).
function egressEntryHost(entry: string): string {
  let h = entry.trim().toLowerCase();
  if (h.split(":").length > 2) return ipv6EntryHost(h);
  const colon = h.indexOf(":");
  if (colon >= 0) h = h.slice(0, colon);
  return h.endsWith(".") ? h.slice(0, -1) : h;
}

// ipv6EntryHost mirrors the Go multi-colon branch: brackets around a bare
// literal dropped, then domainmatch.Classify (trailing dots trimmed, a
// "[addr]:port" split only for a port in 1..65535, a "*." wildcard kept as is)
// and domainmatch.CanonHost (net.IP.String(): ::-compressed, a v4-mapped
// address printed dotted, a spelling it cannot parse left verbatim).
function ipv6EntryHost(entry: string): string {
  let h = entry.startsWith("[") && entry.endsWith("]") ? entry.slice(1, -1) : entry;
  h = h.replace(/\.+$/, "");
  const hp = /^\[([^[\]]*)\]:([^:[\]]*)$/.exec(h);
  if (hp && /^[+-]?\d+$/.test(hp[2]) && Number(hp[2]) >= 1 && Number(hp[2]) <= 65535) h = hp[1];
  if (h.startsWith("*.")) return h;
  if (h.endsWith(".")) h = h.slice(0, -1);
  if (!/^[0-9a-f:.]+$/.test(h)) return h;
  let addr: string;
  try {
    addr = new URL(`http://[${h}]/`).hostname.slice(1, -1);
  } catch {
    return h;
  }
  const v4 = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(addr);
  if (!v4) return addr;
  const [a, b] = [parseInt(v4[1], 16), parseInt(v4[2], 16)];
  return [a >> 8, a & 255, b >> 8, b & 255].join(".");
}

// Image grants may also be a registry prefix ("registry.example.com/team/*"),
// mirroring imagePrefixGrant / validImagePrefix / imageRepoAsTyped /
// imagePrefixCovers / imagePrefixOverlaps in internal/api/base_images.go. A
// prefix is compared as typed: nothing is normalised.
const IMAGE_PREFIX_SUFFIX = "/*";
const GO_SPACE = /[\t-\r \u0085\u00A0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]/;

function imageRefPathSafe(ref: string): boolean {
  if (ref.includes("\\")) return false;
  return ref.trim().split("/").every((seg) => seg !== "" && seg !== "." && seg !== "..");
}

function imageNameChars(s: string): boolean {
  return /^[a-z0-9._-]+$/.test(s);
}

function validImagePrefix(prefix: string): boolean {
  if (!imageRefPathSafe(prefix)) return false;
  const segs = prefix.split("/");
  const host = segs[0];
  const colons = host.split(":").length - 1;
  if ((!/[.:]/.test(host) && host !== "localhost") || colons > 1) return false;
  const cut = host.indexOf(":");
  const name = cut >= 0 ? host.slice(0, cut) : host;
  if (cut >= 0) {
    const port = host.slice(cut + 1);
    const n = Number(port);
    if (!/^[0-9]+$/.test(port) || n < 1 || n > 65535) return false;
  }
  if (name.replace(/^\.+|\.+$/g, "") === "" || !imageNameChars(name)) return false;
  return segs.slice(1).every(imageNameChars);
}

// imagePrefixGrant: the part before "/*" of a well-formed prefix grant, else null.
function imagePrefixGrant(v: string): string | null {
  const t = v.trim();
  if (!t.endsWith(IMAGE_PREFIX_SUFFIX)) return null;
  const prefix = t.slice(0, -IMAGE_PREFIX_SUFFIX.length);
  return validImagePrefix(prefix) ? prefix : null;
}

// imageRepoAsTyped: the reference with its @digest and :tag cut off, as typed.
function imageRepoAsTyped(ref: string): string | null {
  let r = ref.trim();
  if (GO_SPACE.test(r) || r.includes("*") || !imageRefPathSafe(r)) return null;
  const at = r.indexOf("@");
  if (at >= 0) {
    if (r.slice(at).includes("/")) return null;
    r = r.slice(0, at);
  }
  const segs = r.split("/");
  const last = segs.length - 1;
  for (let i = 1; i < last; i++) if (segs[i].includes(":")) return null;
  if (last > 0) {
    const c = segs[last].indexOf(":");
    if (c >= 0) segs[last] = segs[last].slice(0, c);
  }
  return segs.join("/");
}

function imagePrefixCovers(prefix: string, want: string): boolean {
  const repo = imageRepoAsTyped(want);
  return repo !== null && repo.length > prefix.length + 1 && repo.startsWith(prefix + "/");
}

function imagePrefixOverlaps(grantValue: string, want: string): boolean {
  const wp = imagePrefixGrant(want);
  if (wp === null) return false;
  const gp = imagePrefixGrant(grantValue);
  if (gp === null) return imagePrefixCovers(wp, grantValue);
  return gp === wp || gp.startsWith(wp + "/") || wp.startsWith(gp + "/");
}

// An image value holding a "*" that is neither the wildcard nor a well-formed
// prefix: a stored row the server gives no match at all (imageStarRow).
function imageStarRow(v: string): boolean {
  const t = v.trim();
  return t !== "*" && t.includes("*") && imagePrefixGrant(t) === null;
}

// capabilityValueMatches: "*" is the per-kind wildcard; egress_host additionally
// honours a "*.suffix" pattern (note that "*.github.com" does NOT cover bare
// "github.com" — the suffix carries the dot, exactly as entryCoversAny does).
// Every other kind is an exact compare: a secret name, a workspace uuid and an
// image ref are identifiers where a near-miss must not match. An image grant may
// instead be a registry prefix ending in "/*" (above).
export function capabilityValueMatches(kind: string, grantValue: string, want: string): boolean {
  const g = grantValue.trim();
  if (g === "*") return true;
  if (kind === "egress_host") {
    const h = egressEntryHost(g);
    if (h.startsWith("*")) return egressEntryHost(want).endsWith(h.slice(1));
    return egressEntryHost(want) === h;
  }
  if (kind === "image") {
    const prefix = imagePrefixGrant(g);
    if (prefix !== null) return imagePrefixCovers(prefix, want);
    if (imageStarRow(g)) return false;
  }
  return g === want.trim();
}

// capabilityValueOverlaps: the DENY question, mirroring capValueOverlaps
// (internal/api/capabilities.go). An allow has to COVER the want; a deny only
// has to OVERLAP it. Only egress hosts are set-valued, so a want that is itself
// a wildcard ("*.example.com") contains the denied "secret.example.com" without
// being covered by it — asking capabilityValueMatches for both arms let that
// deny protect nothing on the console while the server refused. Both directions
// go through capabilityValueMatches, so there is still exactly one host matcher.
export function capabilityValueOverlaps(kind: string, grantValue: string, want: string): boolean {
  if (capabilityValueMatches(kind, grantValue, want)) return true;
  if (kind === "image") return imagePrefixOverlaps(grantValue, want);
  return kind === "egress_host" && capabilityValueMatches(kind, want, grantValue);
}

// scan mirrors capScan's loop: DENY is asked with the overlap matcher, ALLOW
// with the covering one.
function scan(grants: CapabilityGrant[], kind: string, value: string): { deny: boolean; allow: boolean } {
  let allow = false;
  for (const g of grants) {
    if (g.capability !== kind) continue;
    if (g.effect === "deny") {
      if (capabilityValueOverlaps(kind, g.value, value)) return { deny: true, allow: false }; // deny is final
      continue;
    }
    if (capabilityValueMatches(kind, g.value, value)) allow = true;
  }
  return { deny: false, allow };
}

// capabilityAnswer — the THREE answers capScan can give, because two is one
// short. "unknown" is capScan's stale arm (capabilities.go, capUnresolvableGroupDeny):
// when the caller's group snapshot is unanswerable the server asks whether a
// group DENY row of this kind could cover the value and refuses if one could —
// and that arm sits ABOVE the enforcement switch. handleMeCapabilities passes
// groups=nil on a stale snapshot (permissions.go), so those very rows are
// ABSENT from `grants`: the console cannot rule the deny in or out, and a clean
// local scan is therefore not evidence of permission.
//
// Deliberately NOT folded into a boolean here. Answering "denied" would disable
// controls the server honours on every deployment that holds no group deny rows
// at all (the overwhelming majority) — the one thing this file's header forbids
// — so the third answer is surfaced instead and each caller decides what to say.
export type CapabilityAnswer = "allowed" | "denied" | "unknown";

export function capabilityAnswer(
  caps: MeCapabilities | null,
  kind: CapabilityKind,
  value: string,
): CapabilityAnswer {
  if (!caps) return "allowed";
  const { deny, allow } = scan(caps.grants, kind, value);
  if (deny) return "denied";
  // A local refusal is still a refusal: the stale arm only ever ADDS a deny
  // server-side, so it can downgrade a would-be "allowed" to "unknown" and
  // never upgrade a refusal. Answering "unknown" for an enforced kind with no
  // matching grant would have silently un-refused today's correct answer.
  if (!allow && caps.enforcement[kind]) return "denied";
  // A group DENY beats an allow server-side, so an unresolvable snapshot
  // leaves even an allowed value unconfirmable.
  return caps.groups_snapshot_stale ? "unknown" : "allowed";
}

// The NARROWING answer (capAllowed): deny beats allow beats the enforcement
// switch. `caps === null` means the question doesn't apply to this caller — an
// admin (exempt) or a set that hasn't loaded — and answers ALLOWED, the same
// fail-open default useOperator carries: an advisory check must never be the
// thing that disables a control the server would have honoured. "unknown" folds
// to allowed for the same reason — see capabilityAnswer.
export function capabilityAllowed(caps: MeCapabilities | null, kind: CapabilityKind, value: string): boolean {
  return capabilityAnswer(caps, kind, value) !== "denied";
}

// useMyCapabilities — GET /me/capabilities, once, for the surfaces that explain
// a refusal. `enabled` is the caller's "this question applies to me" gate (in
// practice `!operator`): an admin is exempt server-side, so asking would only
// produce an answer no seam will ever act on.
//
// A failed fetch stays null, which reads as "not bounded" everywhere above —
// deliberate: a network blip must not annotate a member's whole console with
// refusals that aren't happening.
export function useMyCapabilities(enabled: boolean, scope = ""): MeCapabilities | null {
  const [caps, setCaps] = React.useState<MeCapabilities | null>(null);
  const [loadedScope, setLoadedScope] = React.useState(scope);
  React.useEffect(() => {
    if (!enabled) {
      setCaps(null);
      return;
    }
    let live = true;
    setCaps(null);
    permissions
      .getMyCapabilities()
      .then((c) => { if (live) { setLoadedScope(scope); setCaps(c); } })
      .catch(() => {
        /* leave null: advisory copy stays silent rather than guessing */
      });
    return () => {
      live = false;
    };
  }, [enabled, scope]);
  return enabled && loadedScope === scope ? caps : null;
}

// Whether ANY kind is being enforced against this caller. The stale-group hint
// hangs off this: a snapshot that predates group recording only costs a member
// something once something is actually being refused.
export function anyCapabilityEnforced(caps: MeCapabilities | null): boolean {
  return !!caps && Object.values(caps.enforcement).some(Boolean);
}
