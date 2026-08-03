/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { ComposeQuickReview, canLines, cantLines } from "./compose-quick-review";
import { CAPABILITY } from "../../wardyn/copy";
import type { RunPolicySpec } from "../../../lib/types";

// The CAN / CAN'T split is a PURE read-projection of the clamped inline_policy. It
// must render honest lines — never "unrestricted" for allow_all_egress, never
// dropping a raw egress host, grants amber (never a reassuring green check). The
// HIGH-risk acknowledgment gate (rendered inside the full ComposeReview) is
// covered by compose-review.test.tsx.

function renderQuick(policy: RunPolicySpec) {
  return render(<ComposeQuickReview inline_policy={policy} />);
}

describe("ComposeQuickReview — honest can/can't split", () => {
  it("(a) a read-only + no-egress proposal renders the safe guarantees in 'It can't'", () => {
    renderQuick({
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      workspace_mounts: [{ source: "/home/me/site", target: "/work", read_only: true }],
    });
    const cant = screen.getByRole("list", { name: /it can't/i });
    expect(within(cant).getByText(/mounted read-only/i)).toBeInTheDocument();
    expect(within(cant).getByText(/reach the internet — it has no network access/i)).toBeInTheDocument();
    expect(within(cant).getByText(/has no keys or tokens/i)).toBeInTheDocument();
  });

  it("(b) allow_all_egress:true renders the honest block-list copy, never 'unrestricted'", () => {
    const { container } = renderQuick({
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      allow_all_egress: true,
    });
    // The block-list phrasing appears in the CAN column.
    const can = screen.getByRole("list", { name: /this run can/i });
    expect(within(can).getByText(CAPABILITY.allowAllEgress)).toBeInTheDocument();
    // The critical security-copy invariant: "unrestricted" must never appear.
    expect(container.textContent).not.toMatch(/unrestricted/i);
    // The residual (block-list + SSRF guard) is the honest can't line.
    expect(screen.getByText(/block-list and SSRF guard still apply/i)).toBeInTheDocument();
  });

  it("(c) an unknown egress host renders verbatim in 'This run can' (never dropped)", () => {
    renderQuick({
      allowed_domains: ["internal.corp.example"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
    });
    const can = screen.getByRole("list", { name: /this run can/i });
    expect(within(can).getByText(/internal\.corp\.example/)).toBeInTheDocument();
  });

  it("renders a friendly-but-verbatim label for a well-known host", () => {
    renderQuick({
      allowed_domains: ["api.github.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC2",
    });
    const can = screen.getByRole("list", { name: /this run can/i });
    // hostLabel keeps the raw host in the string ("GitHub (api.github.com)").
    expect(within(can).getByText(/api\.github\.com/)).toBeInTheDocument();
  });

  it("always surfaces the shell capability and the always-true audit invariant", () => {
    renderQuick({ allowed_domains: [], first_use_approval: "always_deny", min_confinement_class: "CC2" });
    const can = screen.getByRole("list", { name: /this run can/i });
    expect(within(can).getByText(/run tests and shell commands/i)).toBeInTheDocument();
    const cant = screen.getByRole("list", { name: /it can't/i });
    expect(within(cant).getByText(/hide its activity from the audit log/i)).toBeInTheDocument();
  });
});

describe("canLines / cantLines — grant honesty (D2)", () => {
  it("a write-capable github grant pushes confined to its own branch, and can never open a PR", () => {
    const p: RunPolicySpec = {
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [
        {
          kind: "github_token",
          requires_approval: true,
          scope: { permissions: { contents: "write" } },
        },
      ],
    };
    const can = canLines(p).map((l) => l.text);
    const cant = cantLines(p).map((l) => l.text);
    // Honest capability: it pushes, but only into its own run branch — never an
    // unconstrained push, and never a claim it can open a PR (api.github.com is
    // broker-denied; see examples/workspaces/github-push/TASK.md).
    expect(can.some((t) => /push branches/i.test(t))).toBe(true);
    expect(can.some((t) => /refs\/heads\/wardyn\/<run-id>\//.test(t))).toBe(true);
    expect(can.some((t) => /\bpr\b|pull request/i.test(t))).toBe(false);
    // Mint-accurate: approval gates the token MINT (once), not every push.
    expect(can.some((t) => /you approve its token before it's minted/i.test(t))).toBe(true);
    // The broker guarantee lives in the can't column, verbatim from copy.ts —
    // alongside the PR-denial guarantee, which belongs there instead.
    expect(cant).toContain(CAPABILITY.brokerLine);
    expect(cant.some((t) => /open a pull request/i.test(t))).toBe(true);
  });

  it("a read-only github grant states the read-only guarantee, not the (redundant) PR-denial line", () => {
    const p: RunPolicySpec = {
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [
        { kind: "github_token", requires_approval: false, scope: { permissions: { contents: "read" } } },
      ],
    };
    const can = canLines(p).map((l) => l.text);
    const cant = cantLines(p).map((l) => l.text);
    // No push capability line at all for a read-only token.
    expect(can.some((t) => /push branches/i.test(t))).toBe(false);
    expect(cant.some((t) => /its github token is read-only/i.test(t))).toBe(true);
    expect(cant.some((t) => /open a pull request/i.test(t))).toBe(false);
  });

  it("a git_pat grant uses the gitPatLine exception (no reassuring broker claim)", () => {
    const p: RunPolicySpec = {
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [{ kind: "git_pat", requires_approval: false }],
    };
    const can = canLines(p).map((l) => l.text);
    const cant = cantLines(p).map((l) => l.text);
    // The honest exception is surfaced as an amber capability...
    expect(can).toContain(CAPABILITY.gitPatLine);
    // ...and the broker "can't see your keys" guarantee is NOT claimed for a PAT.
    expect(cant).not.toContain(CAPABILITY.brokerLine);
  });

  it("an ssh_key grant uses the sshKeyLine exception, same shape as git_pat", () => {
    const p: RunPolicySpec = {
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [{ kind: "ssh_key", requires_approval: false, scope: { host: "dev.azure.com" } }],
    };
    const can = canLines(p).map((l) => l.text);
    // Before this fix ssh_key fell through to the generic "short-lived
    // credential" line — wrong for a resident, disk-written key.
    expect(can).toContain(CAPABILITY.sshKeyLine);
    expect(can.some((t) => /short-lived/i.test(t))).toBe(false);
  });

  it("an api_key grant is never claimed short-lived — it's a long-lived key injected proxy-side", () => {
    const p: RunPolicySpec = {
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [{ kind: "api_key", requires_approval: true, scope: { host: "api.example.com" } }],
    };
    const can = canLines(p).map((l) => l.text);
    // Before this fix api_key fell through to the generic "short-lived
    // {noun}" line — contradicting this file's own header note that api_key is
    // a long-lived stored key injected proxy-side, unlike github_token's mint.
    expect(can.some((t) => /short-lived/i.test(t))).toBe(false);
    expect(can.some((t) => /injected proxy-side/i.test(t))).toBe(true);
    expect(can.some((t) => /asks you first/i.test(t))).toBe(true);
  });

  it("no leak: the barrier reads by label (Wall), never the wire class", () => {
    const cant = cantLines({
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
    }).map((l) => l.text);
    expect(cant.some((t) => /sealed behind a Wall/i.test(t))).toBe(true);
    expect(cant.join(" ")).not.toMatch(/CC2/);
  });
});
