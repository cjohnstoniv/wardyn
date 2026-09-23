/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Model behind the redesigned SCM Provider step (Claude Design export, Page 5).
// A "provider row" is not a backend entity — Wardyn stores no such thing and
// exposes no provider API. A row is purely a RESHAPING of data the console
// already has: stored secret names (GET /api/v1/secrets), siteConfig.scm_hosts,
// and status.secrets.github_app. That's also why there is no verify /
// test-connection concept anywhere in this file: there is nothing server-side
// to verify against, and Wardyn has no standing way to reach an arbitrary git
// host to test-connect to it. Pure TS — no React, no fetch, no DOM.

import { CAPABILITY } from "../components/wardyn/copy";
import type { ResidencyKind } from "./integrations";

// The three ways a run can end up with a credential for a host.
export type Lane = "app" | "pat" | "ssh";

// Canonical secret-name slug for a host: trim -> lowercase -> collapse any
// run of non [a-z0-9.] characters to "-" -> "." to "-" -> collapse repeated
// "-" -> strip leading/trailing "-". Verbatim port of wardyn-frames.js:617.
//
// Prefixed with "git-pat-" / "ssh-key-" this is the CONVENTIONAL secret name
// for a host — a convention, not a binding. No Go code performs a host->name
// transform: the only server-side reader of the prefix is scmProviderCheck
// (internal/api/setup.go:865), which GRADES posture and gates nothing. What
// actually binds a credential to a host is a per-run grant that names both
// explicitly — git_pat {host, secret_name} / ssh_key {host, key_secret_ref},
// both required (internal/api/runs_scm.go:223-260). Following the convention
// makes the posture check grade well and lets deriveProviders below rebuild
// rows from names alone; an off-convention name is equally usable.
//
// It has no length cap of its own — an unusually long host can still push the
// prefixed name past the server's 128-char limit (see scm-provider.test.ts);
// hostError() below is the guard, applied by the Add Provider dialog.
export function slugHost(host: string): string {
  return host
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

// Server-mirrored shape rule for a site-config SCM host: validSiteHost
// (internal/api/site_config.go:52) -> workspacescan.ValidApprovedHost, i.e.
// suggestedHostRE plus a REQUIRED dot — so "localhost" is rejected. Mirrored
// here so the Settings Git host card can reject a bad host BEFORE the secret is
// stored, instead of the operator only discovering it when the final
// scm_hosts write 400s with the credential already saved.
const SITE_HOST_RE = /^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$/;

// Why here and not slugHost: the server's secret-name limit (secretNameRE,
// internal/api/secrets.go:21) applies to the PREFIXED name, so a shape-valid
// host can still be unusable. The card derives the secret name from the host,
// so the host is the only field left to fix.
// Returns null when `host` is usable (empty included: that's "not filled in
// yet", which the caller gates on separately).
export function hostError(host: string): string | null {
  const h = host.trim().toLowerCase();
  if (!h) return null;
  if (!h.includes(".") || !SITE_HOST_RE.test(h)) {
    return "Must be a bare hostname containing a dot — lowercase letters, digits, '.' and '-' only, no scheme, port or path.";
  }
  const len = `git-pat-${slugHost(h)}`.length;
  if (len > 128) {
    return `Too long: the secret name this derives would be ${len} characters and the store's limit is 128.`;
  }
  return null;
}

// github-app-* -> app, ssh-key-* -> ssh, everything else -> pat (the default
// covers both git-pat-* names and any unrelated secret, e.g. an API key).
export function laneOfName(name: string): Lane {
  if (/^github-app-/.test(name)) return "app";
  if (/^ssh-key-/.test(name)) return "ssh";
  return "pat";
}

export type ChipTone = "success" | "info" | "warning";

export interface LaneMeta {
  /** Chip text — note the U+00B7 middle dot, not a hyphen. */
  label: string;
  /** Chip tooltip: the honesty canon for this lane, verbatim. */
  tooltip: string;
  tone: ChipTone;
  /** Where this lane's credential actually lives (integrations.ts's ResidencyKind). */
  residency: ResidencyKind;
}

// Lane copy is the product's honesty canon (wardyn-frames.js:613-615): what
// actually happens to the credential at run time, stated plainly rather than
// softened. The tooltips are CAPABILITY.brokerLine/gitPatLine/sshKeyLine
// (components/wardyn/copy.ts), not a second copy of the same three
// sentences: CAPABILITY is already this app's single source of truth for the
// identical github_token/git_pat/ssh_key honesty wording used elsewhere
// (Approvals, run review) — byte-for-byte the same text the design export
// hardcodes under different names (LANE_BROKERED/LANE_INSANDBOX/
// LANE_RESIDENT). Importing it means the two surfaces can't drift apart; a
// hand-retyped copy here could.
// patLaneMeta (#381) is the PAT lane's meta chosen from the operator's real
// WARDYN_GIT_PAT_BROKER switch (Config.DisableGitPATBroker), rather than a
// single frozen guess: on (the 0.7 default) the token is rewritten to a
// broker path and never enters the sandbox, so it reads like the App lane
// (brokered residency); off, it reverts to the pre-0.7 in-sandbox posture.
// LANE_META.pat below is this function's ON shape — the default a caller with
// no switch value in scope (secrets.tsx's chip, the Integrations rollup)
// should show, since that's what a fresh 0.7.10 install actually does.
export function patLaneMeta(brokered: boolean): LaneMeta {
  return brokered
    ? { label: "PAT · brokered", tooltip: CAPABILITY.gitPatLine, tone: "success", residency: "proxy_injected" }
    : { label: "PAT · in-sandbox", tooltip: CAPABILITY.gitPatLineResident, tone: "info", residency: "resident_env" };
}

export const LANE_META: Record<Lane, LaneMeta> = {
  app: { label: "App · brokered", tooltip: CAPABILITY.brokerLine, tone: "success", residency: "brokered_mint" },
  pat: patLaneMeta(true),
  ssh: { label: "SSH · resident", tooltip: CAPABILITY.sshKeyLine, tone: "warning", residency: "resident_mount" },
};

// Pre-convention secret names (github-pat, gitlab-pat, ado-pat, bitbucket-pat)
// remain fully usable — a git_pat grant can name any stored secret — they
// just can't be bucketed onto a host by name, so deriveProviders skips them.
// The skipping is in deriveProviders' own name-parsing and is pinned by
// scm-provider.test.ts.

export interface ProviderRow {
  host: string;
  brand: string;
  lanes: Lane[];
  /** Set when the host is NOT registered in scm_hosts and was reconstructed
   *  from this secret's name — a lossy guess (see deriveProviders), so the UI
   *  must label it as derived and keep it out of destructive copy. */
  derivedFrom?: string;
}

function brandFor(host: string): string {
  if (host === "github.com") return "GitHub";
  if (host === "dev.azure.com") return "Azure DevOps";
  if (host === "gitlab.com") return "GitLab";
  if (host === "bitbucket.org") return "Bitbucket";
  if (host.startsWith("ghes")) return "GitHub Enterprise";
  return "Git host";
}

// git-pat-<slug> or ssh-key-<slug>; group 1 is the slug.
const CRED_NAME_RE = /^(?:git-pat|ssh-key)-(.+)$/;

// Map.get-or-create, so every call site gets a real array to push onto
// instead of juggling `| undefined`.
function bucket(hosts: Map<string, Lane[]>, host: string): Lane[] {
  let lanes = hosts.get(host);
  if (!lanes) {
    lanes = [];
    hosts.set(host, lanes);
  }
  return lanes;
}

// Derives one row per known/credentialed host from data the console already
// has: every scmHosts entry seeds a (possibly empty) row; each git-pat-*/
// ssh-key-* secret contributes its lane to its host's row; githubApp=true
// gives github.com an "app" lane first, creating the row if it's absent.
//
// Host recovery is FORWARD, not reversed. For each scmHosts entry we compute
// slugHost(host) once and match secrets against that. The prototype this was
// ported from instead recovered a host BY REVERSING a secret's slug
// (`slug.replace(/-/g, ".")`, wardyn-proto.js:204) — lossy, since slugHost
// already turned any literal "-" in the host into "-" too, indistinguishable
// from a "." that became "-". A host with a real hyphen ("git-server.corp.com")
// slugs to "git-server-corp-com" and naively reverses to the DIFFERENT string
// "git.server.corp.com", double-listing it: an empty row from scmHosts plus a
// mis-named row from its own secret. Forward matching can't produce that
// split — a secret only ever lands on a host whose OWN slug it matches.
//
// The naive reverse survives only as a fallback for an ORPHAN secret (its
// slug matches no registered host), where a best-guess dotted host beats
// dropping the credential entirely — and the row it produces carries
// `derivedFrom` so the UI can label it as a guess and never put that invented
// hostname in destructive copy.
//
// slugHost is also many-to-one, so two REGISTERED hosts can share one slug
// ("git-server.corp.com" and "git.server.corp.com" both slug to
// "git-server-corp-com"). The secret's name cannot distinguish them, so its
// lane is added to EVERY host that matches — picking one would show the other
// a false "No credential".
export function deriveProviders(
  secretNames: string[],
  scmHosts: string[],
  githubApp: boolean,
): ProviderRow[] {
  const hosts = new Map<string, Lane[]>();
  const slugToHosts = new Map<string, string[]>();
  const derivedFrom = new Map<string, string>();
  for (const raw of scmHosts) {
    // Hosts are case-insensitive and the API stores what the operator typed
    // (validSiteHost lowercases only to CHECK it), so a "GitHub.com" entry —
    // reachable via the CLI, YAML, or a direct site-config PUT — has to bucket
    // onto github.com rather than open a second row beside it.
    const host = raw.trim().toLowerCase();
    bucket(hosts, host);
    const slug = slugHost(host);
    const matches = slugToHosts.get(slug);
    if (matches) matches.push(host);
    else slugToHosts.set(slug, [host]);
  }

  for (const name of secretNames) {
    const m = CRED_NAME_RE.exec(name);
    if (!m) continue;
    const matched = slugToHosts.get(m[1]);
    if (matched) {
      for (const host of matched) bucket(hosts, host).push(laneOfName(name));
      continue;
    }
    const guess = m[1].replace(/-/g, "."); // orphan fallback — a guess, flagged as one
    bucket(hosts, guess).push(laneOfName(name));
    if (!derivedFrom.has(guess)) derivedFrom.set(guess, name);
  }

  if (githubApp) {
    bucket(hosts, "github.com").unshift("app");
  }

  const rows = Array.from(hosts, ([host, lanes]) => {
    const from = derivedFrom.get(host);
    const row: ProviderRow = { host, brand: brandFor(host), lanes };
    if (from) row.derivedFrom = from;
    return row;
  });
  rows.sort((a, b) => (a.host < b.host ? -1 : a.host > b.host ? 1 : 0));
  return rows;
}

// ── Azure DevOps names in URLs (#485) ─────────────────────────────────────────
// A port of internal/adoscope/names.go's UnescapeName — the ONE rule the server
// reads a project or repository name out of a URL path segment with. Azure
// DevOps names carry spaces and most punctuation, so they reach the console as
// typed ("Card Auth (v2).Service"), as Azure DevOps' own %20 spelling, or
// escaped wholesale; each decodes to one name. Refused (null), exactly as the
// server refuses it: a malformed escape, an escape decoding to "/" or "\", a
// dot segment, edge whitespace, a trailing ".", a control character, and a
// segment that would decode AGAIN into any of those ("%252F").

const ADO_SEGMENT_MAX_DECODES = 4;

// percentDecode decodes every %XY in s; strict answers null on a malformed
// escape, lenient keeps it as literal text (the server's hidesStructure reads
// with the lenient one, the most dangerous decoder a request could meet).
function percentDecode(s: string, strict: boolean): string | null {
  const bytes: number[] = [];
  const enc = new TextEncoder();
  for (let i = 0; i < s.length; ) {
    if (s[i] === "%") {
      const hex = s.slice(i + 1, i + 3);
      if (/^[0-9a-fA-F]{2}$/.test(hex)) {
        bytes.push(parseInt(hex, 16));
        i += 3;
        continue;
      }
      if (strict) return null;
    }
    const ch = String.fromCodePoint(s.codePointAt(i) ?? 0);
    bytes.push(...enc.encode(ch));
    i += ch.length;
  }
  return new TextDecoder().decode(new Uint8Array(bytes));
}

function adoSegmentHazard(seg: string): boolean {
  return (
    seg === "." ||
    seg === ".." ||
    seg.trim() !== seg ||
    seg.endsWith(".") ||
    // eslint-disable-next-line no-control-regex
    /[\u0000-\u001f\u007f-\u009f]/.test(seg) ||
    /[/\\]/.test(seg)
  );
}

/** The name one URL path segment decodes to, or null when the server refuses it. */
export function unescapeADOName(raw: string): string | null {
  if (/%(2f|5c)/i.test(raw)) return null;
  const once = percentDecode(raw, true);
  if (once === null || adoSegmentHazard(once)) return null;
  let seg = once;
  for (let d = 0; d < ADO_SEGMENT_MAX_DECODES; d++) {
    const next = percentDecode(seg, false) ?? seg;
    if (next === seg) return once;
    if (adoSegmentHazard(next)) return null;
    seg = next;
  }
  return null;
}

/** Whether a repository address is an Azure DevOps one — its host, or the
 *  "_git" segment Azure DevOps Server paths carry (adoscope.CanonicalRepoURL). */
export function isADOAddress(locator: string): boolean {
  const m = /^(?:[a-z][a-z+.-]*:\/\/)?(?:[^@/]*@)?([^/:]+)/i.exec(locator.trim());
  const host = (m?.[1] ?? "").toLowerCase();
  return (
    host === "dev.azure.com" ||
    host.endsWith(".dev.azure.com") ||
    host.endsWith(".visualstudio.com") ||
    /\/_git(\/|$)/.test(locator)
  );
}

/** An Azure DevOps repository's own name from its address — "Card Auth
 *  (v2).Service", never its escapes — or null for any other address. */
export function adoRepoName(locator: string): string | null {
  if (!isADOAddress(locator)) return null;
  const leaf = locator.trim().replace(/\.git$/, "").replace(/\/+$/, "").split(/[/:]/).pop() ?? "";
  return leaf ? unescapeADOName(leaf) : null;
}
