/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { apiURL, appURL, basePath, originWS, routerPath, signInURL, wsURL } from "./base-path";

function serveUnder(base: string) {
  document.documentElement.dataset.wardynBase = base;
}

afterEach(() => {
  delete document.documentElement.dataset.wardynBase;
});

describe("base-path", () => {
  it("is the host root when the page carries no base (dev server, tests, the default)", () => {
    expect(basePath()).toBe("");
    expect(appURL("/healthz")).toBe("/healthz");
    expect(apiURL("/runs")).toBe("/api/v1/runs");
    expect(routerPath("/runs/abc")).toBe("/runs/abc");
    serveUnder("");
    expect(apiURL("/me")).toBe("/api/v1/me");
  });

  it("prefixes every server URL with the base the daemon wrote into index.html", () => {
    serveUnder("/wardyn");
    expect(basePath()).toBe("/wardyn");
    expect(appURL("/auth/login")).toBe("/wardyn/auth/login");
    expect(appURL("/admin/runs")).toBe("/wardyn/admin/runs");
    expect(apiURL("/runs/1/recording/1")).toBe("/wardyn/api/v1/runs/1/recording/1");
    expect(apiURL("/audit/export?format=csv")).toBe("/wardyn/api/v1/audit/export?format=csv");
  });

  it("builds the attach WebSocket URL on this origin, under the base", () => {
    serveUnder("/team/wardyn");
    const host = window.location.host;
    const ws = window.location.protocol === "https:" ? "wss:" : "ws:";
    expect(wsURL("/runs/r1/attach")).toBe(`${ws}//${host}/team/wardyn/api/v1/runs/r1/attach`);
  });

  it("builds a WebSocket URL on this origin for a path that carries its own base, adding none", () => {
    serveUnder("/team/wardyn");
    const host = window.location.host;
    const ws = window.location.protocol === "https:" ? "wss:" : "ws:";
    expect(originWS("/gw/r/r1/__desktop/__rfb")).toBe(`${ws}//${host}/gw/r/r1/__desktop/__rfb`);
    expect(wsURL("/runs/r1/attach")).toBe(originWS(apiURL("/runs/r1/attach")));
  });

  it("takes the base off a browser pathname, and only a whole-segment match", () => {
    serveUnder("/wardyn");
    expect(routerPath("/wardyn/runs/abc")).toBe("/runs/abc");
    expect(routerPath("/wardyn")).toBe("/");
    expect(routerPath("/wardyn/")).toBe("/");
    expect(routerPath("/wardynx/runs")).toBe("/wardynx/runs");
    expect(routerPath("/runs")).toBe("/runs");
  });

  it("builds the sign-in link with the page to return to, under the base, and none from the root", () => {
    serveUnder("/wardyn");
    window.history.pushState({}, "", "/wardyn/runs/abc?x=1");
    try {
      expect(signInURL()).toBe("/wardyn/auth/login?return_to=%2Fruns%2Fabc%3Fx%3D1");
      window.history.pushState({}, "", "/wardyn/");
      expect(signInURL()).toBe("/wardyn/auth/login");
    } finally {
      window.history.pushState({}, "", "/");
    }
  });
});
