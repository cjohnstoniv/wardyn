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
  it("a write-capable github grant is an AMBER capability, and the broker line a guarantee", () => {
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
    expect(can.some((t) => /push branches and open prs/i.test(t))).toBe(true);
    // Mint-accurate: approval gates the token MINT (once), not every push.
    expect(can.some((t) => /you approve its token before it's minted/i.test(t))).toBe(true);
    // The broker guarantee lives in the can't column, verbatim from copy.ts.
    expect(cant).toContain(CAPABILITY.brokerLine);
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
