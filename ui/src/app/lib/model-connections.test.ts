/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { connectionRowCopy, connectionRows, connectionsSummary } from "./model-connections";
import { CONNECTIONS } from "../components/wardyn/copy/door";
import { absoluteTime, relativeTime, shortDate } from "./format";
import { aheadByHours } from "./test-clock";
import { AGENTS } from "./workspace-providers-copy";
import { MODEL_PROVIDERS, baseStatus, providerStatus } from "./test-fixtures";
import type { SetupModelProvider, SetupProviderAccess, SetupStatus } from "./types";

// Canon pins (design §5.4, packet MP-D): every string this module renders is
// frozen verbatim in copy/door.ts's CONNECTIONS table. These tests exercise
// connectionRowCopy directly — the state table IS the feature — rather than
// through the component, mirroring model-access.test.ts's own split.

const status = (providers: { provider: SetupModelProvider; defaultFor?: string[]; state?: string; deadline?: string }[]) =>
  providerStatus(providers);

describe("connectionRows", () => {
  it("skips a disabled provider", () => {
    const s = providerStatus([{ provider: { ...MODEL_PROVIDERS.bedrock, disabled: true }, state: "live" }]);
    expect(connectionRows(s)).toEqual([]);
  });

  it("skips a provider with no access row (defensive — the server always pairs them)", () => {
    const s: SetupStatus = baseStatus({ model_providers: [MODEL_PROVIDERS.bedrock], provider_access: [] });
    expect(connectionRows(s)).toEqual([]);
  });

  it("pairs every enabled provider with its access row, in order", () => {
    const s = status([{ provider: MODEL_PROVIDERS.bedrock, state: "live" }, { provider: MODEL_PROVIDERS.gateway, state: "not_configured" }]);
    const rows = connectionRows(s);
    expect(rows.map((r) => r.provider.id)).toEqual([MODEL_PROVIDERS.bedrock.id, MODEL_PROVIDERS.gateway.id]);
  });

  it("absent model_providers is no rows, not a throw", () => {
    expect(connectionRows(baseStatus())).toEqual([]);
    expect(connectionRows(null)).toEqual([]);
    expect(connectionRows(undefined)).toEqual([]);
  });
});

describe("connectionsSummary", () => {
  it("no rows: Not set up by your admin (also the every-provider-disabled shape)", () => {
    expect(connectionsSummary([])).toEqual({ label: CONNECTIONS.SUMMARY_NOT_SET_UP, tone: "neutral" });
  });

  it("the default provider live: Ready", () => {
    const rows = connectionRows(status([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "live" }]));
    expect(connectionsSummary(rows)).toEqual({ label: CONNECTIONS.SUMMARY_READY, tone: "success" });
  });

  it("the default provider expiring: still Ready — it signs today", () => {
    const rows = connectionRows(status([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "expiring" }]));
    expect(connectionsSummary(rows)).toEqual({ label: CONNECTIONS.SUMMARY_READY, tone: "success" });
  });

  it("the default provider not_configured: Needs you", () => {
    const rows = connectionRows(status([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "not_configured" }]));
    expect(connectionsSummary(rows)).toEqual({ label: CONNECTIONS.SUMMARY_NEEDS_YOU, tone: "warning" });
  });

  // Packet MP-D case (d): the key row carries no alarm because it isn't the
  // default — only a DEFAULT's own state moves the summary.
  it("a non-default row unconnected does not turn Ready into Needs you", () => {
    const rows = connectionRows(
      status([
        { provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state: "live" },
        { provider: MODEL_PROVIDERS.anthropicKey, state: "not_configured" },
      ]),
    );
    expect(connectionsSummary(rows)).toEqual({ label: CONNECTIONS.SUMMARY_READY, tone: "success" });
  });

  // Case (c): one provider default for two harnesses — one row, one check.
  it("one provider default for two harnesses, connected: Ready", () => {
    const rows = connectionRows(
      status([{ provider: MODEL_PROVIDERS.gateway, defaultFor: ["claude-code", "codex-cli"], state: "live" }]),
    );
    expect(connectionsSummary(rows)).toEqual({ label: CONNECTIONS.SUMMARY_READY, tone: "success" });
  });
});

describe("connectionRowCopy — bedrock_sso (C3-C7)", () => {
  const row = (access: SetupProviderAccess) => ({ provider: MODEL_PROVIDERS.bedrock, access });

  it("C3 not_configured: Not signed in, Sign in to AWS, no second line", () => {
    const copy = connectionRowCopy(baseStatus(), row({ provider: MODEL_PROVIDERS.bedrock.id, state: "not_configured" }));
    expect(copy.chip).toEqual({ label: CONNECTIONS.NOT_SIGNED_IN, tone: "neutral" });
    expect(copy.line).toBe("");
    expect(copy.button).toBe(AGENTS.SIGN_IN_AWS);
  });

  it("C4 live: Signed in, no button, no line", () => {
    const copy = connectionRowCopy(baseStatus(), row({ provider: MODEL_PROVIDERS.bedrock.id, state: "live" }));
    expect(copy.chip).toEqual({ label: CONNECTIONS.SIGNED_IN, tone: "success" });
    expect(copy.line).toBe("");
    expect(copy.button).toBeUndefined();
  });

  // A CLOCK TIME (packet MP-D draws "Sign in again before 17:30"), pinned as a
  // literal — never a recomputation through absoluteTime, which would let the
  // implementation and the test drift in lockstep and still pass. The
  // deadline is a fixed PAST instant (2020, so check-fixture-dates.sh never
  // counts it — an earlier year can't expire) under a fixed TZ, so the
  // literal is deterministic wherever this runs.
  describe("C5 expiring, a fixed clock time", () => {
    const priorTZ = process.env.TZ;
    beforeAll(() => {
      process.env.TZ = "UTC";
    });
    afterAll(() => {
      process.env.TZ = priorTZ;
    });

    it("Expiring, 'Sign in again before {when}', Sign in to AWS", () => {
      const deadline = "2020-01-15T09:30:00Z";
      const copy = connectionRowCopy(baseStatus(), row({ provider: MODEL_PROVIDERS.bedrock.id, state: "expiring", deadline }));
      expect(copy.chip).toEqual({ label: CONNECTIONS.EXPIRING, tone: "warning" });
      expect(copy.line).toBe(CONNECTIONS.EXPIRING_LINE("Jan 15, 2020, 09:30:00 AM"));
      expect(copy.title).toBe("");
      expect(copy.button).toBe(AGENTS.SIGN_IN_AWS);
    });
  });

  it("C6 expired_signin, ordinary dead session: Signed out, the C6 line, Sign in to AWS", () => {
    const copy = connectionRowCopy(
      baseStatus(),
      row({ provider: MODEL_PROVIDERS.bedrock.id, state: "expired_signin", action: AGENTS.SIGN_IN_AWS }),
    );
    expect(copy.chip).toEqual({ label: CONNECTIONS.SIGNED_OUT, tone: "danger" });
    expect(copy.line).toBe(CONNECTIONS.C6_LINE(MODEL_PROVIDERS.bedrock.name!));
    expect(copy.button).toBe(AGENTS.SIGN_IN_AWS);
  });

  it("C7 expired_signin, pin contradiction: the server's own action verbatim, never the C6 line", () => {
    const action =
      "Your stored AWS session is for account 111111111111 / role Old; this row now allows 222222222222 / New — sign in again.";
    const copy = connectionRowCopy(baseStatus(), row({ provider: MODEL_PROVIDERS.bedrock.id, state: "expired_signin", action }));
    expect(copy.chip).toEqual({ label: CONNECTIONS.SIGNED_OUT, tone: "danger" });
    expect(copy.line).toBe(action);
  });
});

describe("connectionRowCopy — key/token providers (C8-C9)", () => {
  it("C8 key, not configured: No key added, 'Your key goes to {host}', Add your key", () => {
    const copy = connectionRowCopy(
      baseStatus(),
      { provider: MODEL_PROVIDERS.anthropicKey, access: { provider: MODEL_PROVIDERS.anthropicKey.id, state: "not_configured" } },
    );
    expect(copy.chip).toEqual({ label: CONNECTIONS.NO_KEY, tone: "neutral" });
    expect(copy.line).toBe(CONNECTIONS.KEY_GOES_TO(MODEL_PROVIDERS.anthropicKey.host));
    expect(copy.button).toBe(CONNECTIONS.ADD_KEY);
  });

  it("C8 token (custom_endpoint), not configured: No token added, the D7 line naming Anthropic, Add your token", () => {
    const copy = connectionRowCopy(
      baseStatus(),
      { provider: MODEL_PROVIDERS.gateway, access: { provider: MODEL_PROVIDERS.gateway.id, state: "not_configured" } },
    );
    expect(copy.chip).toEqual({ label: CONNECTIONS.NO_TOKEN, tone: "neutral" });
    expect(copy.line).toBe(CONNECTIONS.TOKEN_GOES_TO(MODEL_PROVIDERS.gateway.host, "Anthropic"));
    expect(copy.button).toBe(CONNECTIONS.ADD_TOKEN);
  });

  it("C9 key, stored: Your key, 'Sent to {host}', Replace", () => {
    const copy = connectionRowCopy(
      baseStatus(),
      { provider: MODEL_PROVIDERS.anthropicKey, access: { provider: MODEL_PROVIDERS.anthropicKey.id, state: "live" } },
    );
    expect(copy.chip).toEqual({ label: CONNECTIONS.YOUR_KEY, tone: "success" });
    expect(copy.line).toBe(CONNECTIONS.SENT_TO(MODEL_PROVIDERS.anthropicKey.host));
    expect(copy.button).toBe(CONNECTIONS.REPLACE);
  });

  it("C9 token, stored: Your token, 'Sent to {host}', Replace", () => {
    const copy = connectionRowCopy(
      baseStatus(),
      { provider: MODEL_PROVIDERS.gateway, access: { provider: MODEL_PROVIDERS.gateway.id, state: "live" } },
    );
    expect(copy.chip).toEqual({ label: CONNECTIONS.YOUR_TOKEN, tone: "success" });
    expect(copy.line).toBe(CONNECTIONS.SENT_TO(MODEL_PROVIDERS.gateway.host));
    expect(copy.button).toBe(CONNECTIONS.REPLACE);
  });
});

describe("connectionRowCopy — Claude subscription (C10)", () => {
  const row = (access: SetupProviderAccess) => ({ provider: MODEL_PROVIDERS.claude, access });

  it("not signed in: Not signed in, Sign in to Claude, no line", () => {
    const copy = connectionRowCopy(baseStatus(), row({ provider: MODEL_PROVIDERS.claude.id, state: "not_configured" }));
    expect(copy.chip).toEqual({ label: CONNECTIONS.NOT_SIGNED_IN, tone: "neutral" });
    expect(copy.line).toBe("");
    expect(copy.button).toBe(CONNECTIONS.SIGN_IN_CLAUDE);
  });

  it("live: Signed in — C4's chip, reused (§5.4 draws no separate live chip for Claude)", () => {
    const copy = connectionRowCopy(baseStatus(), row({ provider: MODEL_PROVIDERS.claude.id, state: "live" }));
    expect(copy.chip).toEqual({ label: CONNECTIONS.SIGNED_IN, tone: "success" });
    expect(copy.button).toBeUndefined();
  });

  it("aging (expiring): Expiring, the fixed 11-months sentence, Sign in to Claude", () => {
    const copy = connectionRowCopy(baseStatus(), row({ provider: MODEL_PROVIDERS.claude.id, state: "expiring" }));
    expect(copy.chip).toEqual({ label: CONNECTIONS.EXPIRING, tone: "warning" });
    expect(copy.line).toBe(CONNECTIONS.CLAUDE_AGING);
    expect(copy.button).toBe(CONNECTIONS.SIGN_IN_CLAUDE);
  });
});

// #592 (CS-8) — the meta line every row that HOLDS a credential gets, from
// THIS caller's own provider_access row (added_at/last_used_at). Independent
// of provider kind: bedrock is enough to exercise all three shapes.
//
// Clock-relative fixtures (aheadByHours, #210) rather than literal dates: a
// literal date this suite compared to "now" would start failing the day the
// wall clock catches up to it, for reasons with nothing to do with the code
// under test. The expected strings are composed through the SAME shortDate/
// relativeTime/absoluteTime helpers the module under test uses — the
// mutation proof (not a hardcoded string) is what proves the composition
// itself, not just that a formatter was called.
describe("connectionRowCopy — the meta line (#592)", () => {
  const row = (access: Partial<SetupProviderAccess> = {}) => ({
    provider: MODEL_PROVIDERS.bedrock,
    access: { provider: MODEL_PROVIDERS.bedrock.id, state: "live", ...access } as SetupProviderAccess,
  });

  it("nothing stored (no added_at): no meta line at all", () => {
    const copy = connectionRowCopy(baseStatus(), row());
    expect(copy.meta).toBe("");
    expect(copy.metaTitle).toBe("");
  });

  it("added_at, never used: Added {date} · Not used by a run yet", () => {
    const addedAt = aheadByHours(-24 * 10);
    const copy = connectionRowCopy(baseStatus(), row({ added_at: addedAt }));
    expect(copy.meta).toBe(`${CONNECTIONS.ADDED(shortDate(addedAt))} · ${CONNECTIONS.NOT_USED}`);
    expect(copy.metaTitle).toBe("");
  });

  it("added_at and last_used_at: Added {date} · Last used {relative}, hover = the exact instant", () => {
    const addedAt = aheadByHours(-24 * 10);
    const lastUsedAt = aheadByHours(-2);
    const copy = connectionRowCopy(baseStatus(), row({ added_at: addedAt, last_used_at: lastUsedAt }));
    expect(copy.meta).toBe(
      `${CONNECTIONS.ADDED(shortDate(addedAt))} · ${CONNECTIONS.LAST_USED(relativeTime(lastUsedAt))}`,
    );
    expect(copy.metaTitle).toBe(absoluteTime(lastUsedAt));
  });
});

describe("connectionRowCopy — the 'For …' line", () => {
  it("one harness: 'For Claude Code'", () => {
    const s = baseStatus({ harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true }] });
    const copy = connectionRowCopy(s, {
      provider: MODEL_PROVIDERS.bedrock,
      access: { provider: MODEL_PROVIDERS.bedrock.id, state: "live" },
    });
    expect(copy.forLine).toBe(CONNECTIONS.FOR("Claude Code"));
  });

  it("two harnesses: 'For Claude Code and Codex CLI'", () => {
    const s = baseStatus({
      harnesses: [
        { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true },
        { id: "codex-cli", display: "Codex CLI", has_gateway: true, has_login: true },
      ],
    });
    const copy = connectionRowCopy(s, {
      provider: MODEL_PROVIDERS.gateway,
      access: { provider: MODEL_PROVIDERS.gateway.id, state: "live" },
    });
    expect(copy.forLine).toBe(CONNECTIONS.FOR("Claude Code and Codex CLI"));
  });
});

