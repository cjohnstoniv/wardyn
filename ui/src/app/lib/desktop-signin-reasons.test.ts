/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Every desktop sign-in reason of the Go source has a console sentence, and the
// map holds no reason the server does not send. Run:
//   cd ui && pnpm vitest run src/app/lib/desktop-signin-reasons.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { DESKTOP_SIGNIN_REASON_LINE, PROXY_INTERNAL_REASONS, desktopSigninReasonLine } from "./desktop-signin-reasons";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const goSource = readFileSync(join(repoRoot(), "internal/api/reasons_routes.go"), "utf8");
const goReasons = [...goSource.matchAll(/^\s*reasonDesktopSignin\w*\s*=\s*"(desktop_signins?_\w+)"/gm)].map((m) => m[1]);

// The server reasons a person can meet that have no sentence: none may.
const unmapped = (server: string[], map: Record<string, string>, exempt: readonly string[]): string[] =>
  server.filter((r) => !(r in map) && !exempt.includes(r));

describe("desktop sign-in reasons", () => {
  it("reads the Go constants", () => {
    expect(goReasons.length).toBeGreaterThanOrEqual(16);
    expect(goReasons).toContain("desktop_signin_credential_refused");
  });

  it("every reason of internal/api/reasons_routes.go has a console sentence, bar the named proxy-internal one", () => {
    expect(unmapped(goReasons, DESKTOP_SIGNIN_REASON_LINE, PROXY_INTERNAL_REASONS)).toEqual([]);
  });

  it("a new server reason that is neither mapped nor named as exempt fails", () => {
    expect(unmapped([...goReasons, "desktop_signin_new_reason"], DESKTOP_SIGNIN_REASON_LINE, PROXY_INTERNAL_REASONS)).toEqual([
      "desktop_signin_new_reason",
    ]);
  });

  it("holds no reason the server does not send, none of the exempt ones, and no sentence that is a code", () => {
    expect(Object.keys(DESKTOP_SIGNIN_REASON_LINE).sort()).toEqual(goReasons.filter((r) => !PROXY_INTERNAL_REASONS.includes(r as never)).sort());
    for (const [reason, line] of Object.entries(DESKTOP_SIGNIN_REASON_LINE)) {
      expect(line, reason).not.toMatch(/desktop_signin/);
      expect(line, reason).toMatch(/\.$/);
    }
  });

  it("is silent on any other value, including Object.prototype names", () => {
    expect(desktopSigninReasonLine("run_owner_only")).toBeUndefined();
    expect(desktopSigninReasonLine("desktop_signin_credential_refused")).toBeUndefined();
    expect(desktopSigninReasonLine("constructor")).toBeUndefined();
    expect(desktopSigninReasonLine(undefined)).toBeUndefined();
  });
});
