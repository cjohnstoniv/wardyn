/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Series ruling S6 — stale-stack hygiene: the stage must match the script, or
 * the script must own the mess.
 *
 * The series shares one long-lived stack (only video 01 resets), so every take
 * opens on whatever earlier takes left behind. Persona round 1 billed that
 * four ways: an Approvals badge stuck at 10 for three straight episodes, six
 * red "Killed" cards under a cold open claiming nothing had run, and
 * "workspace already in use by N active run(s) — proceeding anyway" toasts
 * firing mid-beat. Every video's beforeAll calls sweepStaleState() before
 * rolling; anything a video DELIBERATELY leaves behind (V01's undecided
 * re-raise) is either swept here by the next take or named in one clause on
 * camera.
 *
 * Leftover FAILED/untitled board cards have no delete API — V01's reset is the
 * only true board wipe; a later video that films the board owns naming what's
 * still visible.
 */

const API = (process.env.WARDYN_DEMO_BASE_URL || "http://localhost:8080") + "/api/v1";

// The demo stack is local mode (no auth); a token-bearing stack (V09's own
// disposable one) still sweeps when the env carries its bearer.
const HEADERS: Record<string, string> = {
  "content-type": "application/json",
  ...(process.env.WARDYN_DEMO_TOKEN ? { authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : {}),
};

// Inline rather than imported from ui/src (the demo specs deliberately don't
// reach into the app's source tree): a run in none of these states is live
// enough to hold a workspace or a badge.
const TERMINAL = new Set(["COMPLETED", "FAILED", "KILLED", "STOPPED", "ARCHIVED"]);

async function list(path: string): Promise<Array<Record<string, unknown>>> {
  const res = await fetch(API + path, { headers: HEADERS }).catch(() => null);
  if (!res?.ok) return [];
  const d = (await res.json().catch(() => null)) as unknown;
  if (Array.isArray(d)) return d as Array<Record<string, unknown>>;
  const items = (d as { items?: unknown })?.items;
  return Array.isArray(items) ? (items as Array<Record<string, unknown>>) : [];
}

/**
 * Deny every stale pending approval and kill live runs still holding a series
 * workspace. `workspacePaths` scopes the kill to runs whose workspace_path
 * contains one of the given fragments; with none given, every non-terminal
 * run goes (the demo stack's runs are all the series' own).
 *
 * Best-effort by design: a sweep that cannot reach the plane must not fail
 * the take — the take's own asserts will say what's actually wrong.
 */
export async function sweepStaleState(workspacePaths: string[] = []): Promise<void> {
  // 0.7: the setup gate force-lands ANY route on an un-onboarded install
  // (server-derived SiteConfig.OnboardingCompletedAt — a reset stack starts
  // un-onboarded). Every spec that sweeps is a post-setup script ("an operator
  // finished Getting Started before this film"), so marking the install
  // onboarded is stage hygiene exactly like the run/approval sweeps below:
  // the stage must match the script. Idempotent server-side; episode 02 films
  // the fresh-install story and deliberately does NOT sweep.
  await fetch(`${API}/setup/onboarding-complete`, { method: "POST", headers: HEADERS }).catch(() => {});

  for (const a of await list("/approvals?state=PENDING")) {
    await fetch(`${API}/approvals/${a.id}/deny`, {
      method: "POST",
      headers: HEADERS,
      body: JSON.stringify({ reason: "pre-take sweep (S6): stale hold from an earlier take" }),
    }).catch(() => {});
  }
  for (const r of await list("/runs")) {
    if (TERMINAL.has(String(r.state))) continue;
    const wsPath = String(r.workspace_path ?? "");
    if (workspacePaths.length && !workspacePaths.some((w) => wsPath.includes(w))) continue;
    await fetch(`${API}/runs/${r.id}/kill`, { method: "POST", headers: HEADERS }).catch(() => {});
  }
}
