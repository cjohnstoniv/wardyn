/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Your model connections" (design §5.4, packet MP-D, states C3-C10 and the
// summary chip) — pure, no React, mirroring model-access.ts's providerAttention
// / model-access-banner.tsx's providerStripLine split: the state table is the
// feature, so it is a function the tests can pin directly, and the component
// (screens/settings/model-connections-card.tsx) only renders what this
// returns. Not folded into model-access.ts: that module is on the shell's
// EAGER graph (its own file header explains why it avoids copy imports), and
// this page is reached only through the lazy Settings/Account chunk.
//
// NOT wired here, on purpose:
//  - C9b ("removed by an address change") — SetupProviderAccess carries only
//    `not_configured`, the same state a credential that was never stored
//    grades to; there is no field distinguishing the two. Its canon sentence
//    is left unwired rather than invented.
//  - C11 (the admin-token caller) — that principal never mounts the Member
//    view at all (model-access-context.tsx's own comment), so this page never
//    sees it.
import { absoluteTime } from "./format";
import { harnessDisplayNames } from "./model-access";
import type { SetupModelProvider, SetupProviderAccess, SetupStatus } from "./types";
import { AGENTS } from "./workspace-providers-copy";
import { CONNECTIONS } from "../components/wardyn/copy/door";

/** A provider this caller may connect, paired with their own state for it. */
export interface ConnectionRow {
  provider: SetupModelProvider;
  access: SetupProviderAccess;
}

/** The rows §5.4 draws: every ENABLED provider with a graded access row, in
 *  the order the server sent them. A disabled provider gets no row here — the
 *  design's own words ("One row per provider enabled for the person's granted
 *  harnesses"), mirroring providerAttention's identical skip. */
export function connectionRows(status: SetupStatus | null | undefined): ConnectionRow[] {
  const out: ConnectionRow[] = [];
  for (const p of status?.model_providers ?? []) {
    if (p.disabled) continue;
    const access = status?.provider_access?.find((a) => a.provider === p.id);
    if (!access) continue;
    out.push({ provider: p, access });
  }
  return out;
}

const CONNECTED_STATES = new Set(["live", "expiring"]);

export interface ConnectionsSummary {
  label: string;
  tone: "success" | "warning" | "neutral";
}

/**
 * connectionsSummary is §5.4's own chip: Ready when every granted harness's
 * DEFAULT provider is connected, Needs you when one is not, Not set up by
 * your admin with no rows at all. Only a harness's default counts — case (d)
 * (packet MP-D) draws an unconnected NON-default row with no alarm.
 * "Connected" includes `expiring`: the credential still signs today.
 *
 * No separate "granted harnesses" input: a row's own `default_for` (like its
 * `harnesses`) is already the server's answer to "this person's own agents"
 * — setupModelProviders filters both to what the caller may launch — so a
 * second filter against SetupStatus.harnesses would be a second, redundant
 * copy of that scoping, one an older or partial harnesses list could disagree
 * with.
 */
export function connectionsSummary(rows: ConnectionRow[]): ConnectionsSummary {
  if (rows.length === 0) return { label: CONNECTIONS.SUMMARY_NOT_SET_UP, tone: "neutral" };
  const defaultHarnesses = new Set<string>();
  for (const { provider } of rows) for (const h of provider.default_for ?? []) defaultHarnesses.add(h);
  const connected = (harnessId: string) =>
    rows.some((r) => (r.provider.default_for ?? []).includes(harnessId) && CONNECTED_STATES.has(r.access.state));
  const needsYou = [...defaultHarnesses].some((h) => !connected(h));
  return needsYou
    ? { label: CONNECTIONS.SUMMARY_NEEDS_YOU, tone: "warning" }
    : { label: CONNECTIONS.SUMMARY_READY, tone: "success" };
}

/**
 * legacySummary is Getting Started's OWN fallback (fix review on #541): with
 * no model-providers block at all, connectionsSummary's rows are always
 * empty — but "Not set up by your admin" is a real regression for every
 * install that predates provider records, which is every legacy shared or
 * per_user roster install main still carries (the admin funnel writes no
 * provider block until #548 lands). Read the SAME per-principal
 * `model_access` (and its `llm_ready` deployment-wide fallback) the retired
 * "Your model key" card used to, so a shared install with a working credential
 * still reads Ready rather than a false alarm.
 *
 * Used ONLY when `!status?.model_providers` — a real provider block always
 * takes connectionsSummary instead, even an empty one (§5.4's own "No
 * providers" state).
 */
export function legacySummary(status: SetupStatus | null | undefined): ConnectionsSummary {
  const state = status?.model_access?.state;
  if (state === "live" || state === "expiring") return { label: CONNECTIONS.SUMMARY_READY, tone: "success" };
  if (state === "not_configured" || state === "expired_signin") {
    return { label: CONNECTIONS.SUMMARY_NEEDS_YOU, tone: "warning" };
  }
  // Fix review (HIGH): shared_expired is a DEAD shared credential, not a
  // config fact — llm_ready is computed at the deployment/config level
  // (internal/api/setup.go), so it stays true for an install whose one
  // shared credential has expired, and the llmReady fallback below would
  // otherwise read that install as Ready.
  if (state === "shared_expired") return { label: AGENTS.MODEL_ACCESS_SHARED_EXPIRED, tone: "warning" };
  if (status?.llm_ready === true) return { label: CONNECTIONS.SUMMARY_READY, tone: "success" };
  return { label: CONNECTIONS.SUMMARY_NOT_SET_UP, tone: "neutral" };
}

export interface ConnectionRowCopy {
  chip: { label: string; tone: "success" | "warning" | "neutral" | "danger" };
  /** The "For {harness}" / "For {harness} and {harness}" line — always shown. */
  forLine: string;
  /** The state-specific second line; "" when the state draws none (C4, live
   *  Claude). */
  line: string;
  /** `line`'s absolute-time title, for the C5 hover the strip already gives
   *  its own deadline. "" when there is none. */
  title: string;
  /** The row's button label; undefined for a state with no button (C4, and a
   *  live Claude sign-in). */
  button?: string;
}

/** The server's action line adds nothing when it is byte-identical to the
 *  button's own label (mirrors model-access-banner.tsx's serverAction) — what
 *  survives is the pin-contradicted / wrong-portal pair (C7), which names
 *  what C6's own sentence cannot. */
function signInAction(access: SetupProviderAccess, name: string): string {
  return access.action && access.action !== AGENTS.SIGN_IN_AWS ? access.action : CONNECTIONS.C6_LINE(name);
}

/** connectionRowCopy is §5.4's per-row state table (C3-C10), as one function. */
export function connectionRowCopy(status: SetupStatus | null | undefined, row: ConnectionRow): ConnectionRowCopy {
  const { provider, access } = row;
  const name = provider.name || provider.id;
  const forLine = CONNECTIONS.FOR(harnessDisplayNames(status, provider.harnesses));

  if (provider.kind === "bedrock_sso") {
    switch (access.state) {
      case "live":
        return { chip: { label: CONNECTIONS.SIGNED_IN, tone: "success" }, forLine, line: "", title: "" };
      case "expiring": {
        // A clock time, not a relative offset (packet MP-D draws "Sign in
        // again before 17:30") — the same absoluteTime convention
        // modelAccessActionLine (workspace-providers-copy.ts) composes the
        // server's own expiring action line with. No separate hover title:
        // the line already carries the precise instant.
        const when = access.deadline ? absoluteTime(access.deadline) : "";
        return {
          chip: { label: CONNECTIONS.EXPIRING, tone: "warning" },
          forLine,
          line: when ? CONNECTIONS.EXPIRING_LINE(when) : "",
          title: "",
          button: AGENTS.SIGN_IN_AWS,
        };
      }
      case "expired_signin":
        return {
          chip: { label: CONNECTIONS.SIGNED_OUT, tone: "danger" },
          forLine,
          line: signInAction(access, name),
          title: "",
          button: AGENTS.SIGN_IN_AWS,
        };
      default:
        return {
          chip: { label: CONNECTIONS.NOT_SIGNED_IN, tone: "neutral" },
          forLine,
          line: "",
          title: "",
          button: AGENTS.SIGN_IN_AWS,
        };
    }
  }

  if (provider.kind === "anthropic_subscription") {
    switch (access.state) {
      case "live":
        return { chip: { label: CONNECTIONS.SIGNED_IN, tone: "success" }, forLine, line: "", title: "" };
      case "expiring":
        return {
          chip: { label: CONNECTIONS.EXPIRING, tone: "warning" },
          forLine,
          line: CONNECTIONS.CLAUDE_AGING,
          title: "",
          button: CONNECTIONS.SIGN_IN_CLAUDE,
        };
      default:
        return {
          chip: { label: CONNECTIONS.NOT_SIGNED_IN, tone: "neutral" },
          forLine,
          line: "",
          title: "",
          button: CONNECTIONS.SIGN_IN_CLAUDE,
        };
    }
  }

  // Every other kind is a typed key or token (door-dialog.tsx's own rule):
  // custom_endpoint says "token", every other closed kind "key".
  const token = provider.kind === "custom_endpoint";
  if (access.state === "live") {
    return {
      chip: { label: token ? CONNECTIONS.YOUR_TOKEN : CONNECTIONS.YOUR_KEY, tone: "success" },
      forLine,
      line: CONNECTIONS.SENT_TO(provider.host),
      title: "",
      button: CONNECTIONS.REPLACE,
    };
  }
  return {
    chip: { label: token ? CONNECTIONS.NO_TOKEN : CONNECTIONS.NO_KEY, tone: "neutral" },
    forLine,
    // D7: the destination shows on the row AND the dialog (packet MP-D, QD-4).
    line: token ? CONNECTIONS.TOKEN_GOES_TO(provider.host, "Anthropic") : CONNECTIONS.KEY_GOES_TO(provider.host),
    title: "",
    button: token ? CONNECTIONS.ADD_TOKEN : CONNECTIONS.ADD_KEY,
  };
}
