/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { GrantSpec, RunPolicySpec } from "../../lib/types";
import {
  MAX_PUSH_RULE_PATH_BYTES,
  nonGitHubPushRuleHosts,
  pushRulePatternProblem,
  PUSH_RULES_WARN_SSH_ONLY,
  pushRulesUnenforceableBySSHOnly,
  pushRulesWarnNonGithub,
  PushRulesSection,
} from "./policy-push-rules";

const BASE: RunPolicySpec = {
  allowed_domains: [],
  first_use_approval: "always_deny",
  min_confinement_class: "CC2",
};

// push_rules is a SECURITY field (#57): the one that can hold or refuse a
// run's push. The mirror exists so the editor names what the server will
// refuse before Save — if it drifts from internal/api/policy.go's
// validatePushRulePaths/DenyPathSegments, the console starts promising writes
// the server rejects.
describe("pushRulePatternProblem — mirrors validatePushRulePaths + DenyPathSegments (#57, PR-2)", () => {
  it("accepts a well-formed pattern", () => {
    expect(pushRulePatternProblem(".github/workflows/**", false)).toBeNull();
    expect(pushRulePatternProblem("deploy/**", true)).toBeNull();
  });

  it("accepts an empty pattern — a freshly added row is not yet a mistake", () => {
    expect(pushRulePatternProblem("", false)).toBeNull();
    expect(pushRulePatternProblem("", true)).toBeNull();
  });

  it("refuses a pattern over the byte ceiling and names the actual count", () => {
    const long = "a/".repeat(200);
    const problem = pushRulePatternProblem(long, false);
    expect(problem).toMatch(/bytes\. Patterns are at most 256 bytes\./);
    expect(problem).toContain(String(new TextEncoder().encode(long).length));
  });

  it("measures the cap in bytes, not UTF-16 units — 120 characters is well under 256 but 360 bytes is not", () => {
    const cjk = "漢".repeat(120); // 120 UTF-16 units, 360 bytes (3 bytes/char)
    expect(cjk.length).toBeLessThan(MAX_PUSH_RULE_PATH_BYTES);
    expect(new TextEncoder().encode(cjk).length).toBeGreaterThan(MAX_PUSH_RULE_PATH_BYTES);
    expect(pushRulePatternProblem(cjk, false)).toMatch(/bytes/);
  });

  it("refuses a control character", () => {
    expect(pushRulePatternProblem("deploy/\u0007hook", false)).toBe(
      "This pattern has a control character in it, which no push path can contain.",
    );
  });

  // F1: unicode.IsControl (what the server's controlCharFree actually calls)
  // covers C1 (U+0080-U+009F) too, not just C0/DEL. A parity probe against the
  // real server found U+0080, U+0085 (NEL) and U+009F accepted live while the
  // server refused them.
  it("refuses a C1 control character (U+0080-U+009F), not just C0/DEL", () => {
    expect(pushRulePatternProblem("a\u0080b", false)).toMatch(/control character/);
    expect(pushRulePatternProblem("a\u0085b", false)).toMatch(/control character/);
    expect(pushRulePatternProblem("a\u009fb", false)).toMatch(/control character/);
    // The boundary just past C1 (U+00A0, NBSP) is NOT a control character.
    expect(pushRulePatternProblem("a\u00a0b", false)).toBeNull();
  });

  it("refuses leading or trailing whitespace", () => {
    expect(pushRulePatternProblem(" deploy/**", false)).toMatch(/leading or trailing space/);
    expect(pushRulePatternProblem("deploy/** ", false)).toMatch(/leading or trailing space/);
  });

  // F2: DenyPathSegments strips the leading "/" BEFORE checking whitespace —
  // "/ a" is a whitespace violation server-side even though its first
  // character is "/", not a space. A parity probe confirmed the server
  // refuses "/ a" while the unfixed live check accepted it.
  it("checks whitespace AFTER stripping the leading slash, matching the server's order", () => {
    expect(pushRulePatternProblem("/ a", false)).toMatch(/leading or trailing space/);
    expect(pushRulePatternProblem("/a ", false)).toMatch(/leading or trailing space/);
    // A lone leading slash with nothing after it strips to "", which has no
    // whitespace to flag.
    expect(pushRulePatternProblem("/", false)).toBeNull();
  });

  // F3: Go's strings.TrimSpace (unicode.IsSpace) does NOT treat U+FEFF
  // (BOM/ZWNBSP) as whitespace, unlike JavaScript's String.prototype.trim().
  // A parity probe confirmed the server accepts a leading/trailing BOM.
  it("does not flag a leading/trailing BOM (U+FEFF) as whitespace — the server accepts it", () => {
    expect(pushRulePatternProblem("﻿deploy/**", false)).toBeNull();
    expect(pushRulePatternProblem("deploy/**﻿", false)).toBeNull();
    // A REAL space beside the BOM is still caught.
    expect(pushRulePatternProblem(" ﻿deploy/**", false)).toMatch(/leading or trailing space/);
  });

  // F7/M3: the byte-cap comparison must be a strict `>`, not `>=` — exactly
  // 256 bytes is legal (validatePushRulePaths only refuses OVER the cap).
  it("accepts exactly the byte ceiling and refuses one byte over it", () => {
    const atCeiling = "a".repeat(MAX_PUSH_RULE_PATH_BYTES);
    const overCeiling = "a".repeat(MAX_PUSH_RULE_PATH_BYTES + 1);
    expect(pushRulePatternProblem(atCeiling, false)).toBeNull();
    expect(pushRulePatternProblem(overCeiling, false)).toMatch(/bytes/);
  });

  // F7/M7: hasEmptyPathSegment strips the leading "/" too (DenyPathSegments'
  // own first step) — without it, "/deploy/**" splits into ["", "deploy",
  // "**"], a leading empty segment that would falsely refuse a legal,
  // root-anchored review pattern.
  it("strips the leading slash before the segment check, so a root-anchored review pattern is not falsely flagged", () => {
    expect(pushRulePatternProblem("/deploy/**", true)).toBeNull();
  });

  // The packet's own scope call (PR-2): the empty/./.. segment check is
  // rendered live only for require_review_paths — deny_paths gets the
  // identical check, just server-side only, at Save.
  it("flags an empty/./.. path segment live only for review paths", () => {
    expect(pushRulePatternProblem("deploy/../etc", true)).toBe(
      'A path segment can\'t be empty, ".", or "..".',
    );
    expect(pushRulePatternProblem("deploy/./x", true)).toMatch(/path segment/);
    expect(pushRulePatternProblem("deploy//x", true)).toMatch(/path segment/);
    // The identical pattern on the DENY side is not flagged live.
    expect(pushRulePatternProblem("deploy/../etc", false)).toBeNull();
  });
});

describe("pushRulesUnenforceableBySSHOnly — mirrors composer/risk.go's pushRulesUnenforceable", () => {
  it("is true when ssh_key is the only git-capable grant", () => {
    const grants: GrantSpec[] = [{ kind: "ssh_key", requires_approval: true }];
    expect(pushRulesUnenforceableBySSHOnly(grants)).toBe(true);
  });

  it("is false once a brokered grant (git_pat or github_token) is also eligible", () => {
    const withPat: GrantSpec[] = [
      { kind: "ssh_key", requires_approval: true },
      { kind: "git_pat", requires_approval: true },
    ];
    expect(pushRulesUnenforceableBySSHOnly(withPat)).toBe(false);
    const withToken: GrantSpec[] = [
      { kind: "ssh_key", requires_approval: true },
      { kind: "github_token", requires_approval: true },
    ];
    expect(pushRulesUnenforceableBySSHOnly(withToken)).toBe(false);
  });

  it("is false with no ssh_key grant, or no grants at all", () => {
    expect(pushRulesUnenforceableBySSHOnly([{ kind: "git_pat", requires_approval: true }])).toBe(false);
    expect(pushRulesUnenforceableBySSHOnly(undefined)).toBe(false);
    expect(pushRulesUnenforceableBySSHOnly([])).toBe(false);
  });
});

describe("nonGitHubPushRuleHosts — mirrors composer/risk.go's nonGitHubPATHosts", () => {
  it("collects git_pat hosts other than github.com, sorted and de-duplicated", () => {
    const grants: GrantSpec[] = [
      { kind: "git_pat", requires_approval: true, scope: { host: "dev.azure.com" } },
      { kind: "git_pat", requires_approval: true, scope: { host: "DEV.AZURE.COM." } },
      { kind: "git_pat", requires_approval: true, scope: { host: "gitlab.example.com" } },
      { kind: "git_pat", requires_approval: true, scope: { host: "github.com" } },
      { kind: "ssh_key", requires_approval: true },
    ];
    expect(nonGitHubPushRuleHosts(grants)).toEqual(["dev.azure.com", "gitlab.example.com"]);
  });

  it("ignores non-git_pat grants and a missing/malformed scope", () => {
    expect(nonGitHubPushRuleHosts([{ kind: "github_token", requires_approval: true }])).toEqual([]);
    expect(nonGitHubPushRuleHosts([{ kind: "git_pat", requires_approval: true }])).toEqual([]);
    expect(nonGitHubPushRuleHosts([{ kind: "git_pat", requires_approval: true, scope: { host: 7 } }])).toEqual([]);
  });
});

describe("PushRulesSection — reused warning text", () => {
  it("PUSH_RULES_WARN_SSH_ONLY matches risk.go's sentence byte for byte", () => {
    expect(PUSH_RULES_WARN_SSH_ONLY).toBe(
      "push_rules is set, but this run's only git-capable grant is ssh_key — the SSH transport has no broker seam, so these content rules cannot be enforced.",
    );
  });

  it("pushRulesWarnNonGithub fills in the host, matching risk.go's sentence", () => {
    expect(pushRulesWarnNonGithub("dev.azure.com")).toBe(
      "Content rules on dev.azure.com refuse any push whose tree still holds a path a deny pattern reaches, even one the push " +
        "leaves untouched: Wardyn can check what a push left unchanged on github.com only. On this forge, deny " +
        "only paths the repository does not hold yet.",
    );
  });
});

describe("PushRulesSection — the row editor (#57, PR-1)", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });

  // The panel is fully controlled; drive it through a tiny stateful harness so
  // typing several keystrokes actually accumulates (a controlled <input> whose
  // value prop never changes snaps back after every keystroke) — not just
  // proving the callback fired once. `capture` lets a test also read the
  // latest document without a second render pass of its own.
  function Harness({
    initial,
    capture,
  }: {
    initial: RunPolicySpec;
    capture?: (next: RunPolicySpec) => void;
  }) {
    const [spec, setSpec] = React.useState(initial);
    return (
      <PushRulesSection
        spec={spec}
        onSpecChange={(next) => {
          setSpec(next);
          capture?.(next);
        }}
      />
    );
  }

  // SectionLabel renders "Deny"/"Hold for review" as a <div> of its own — its
  // immediate parent is the section's wrapping div, which also holds that
  // section's rows and "Add path" button.
  function denySection() {
    return screen.getByText("Deny").parentElement as HTMLElement;
  }
  function reviewSection() {
    return screen.getByText("Hold for review").parentElement as HTMLElement;
  }

  it("shows the main path: add a review row, an invalid pattern shows the row error, fixing it clears it (#57, PR-2)", async () => {
    render(<Harness initial={BASE} />);
    await user.click(within(reviewSection()).getByRole("button", { name: "Add path" }));
    const row = screen.getByLabelText("Hold for review path 1");
    await user.type(row, "deploy/../etc");
    expect(screen.getByRole("alert")).toHaveTextContent(/path segment/);

    await user.clear(row);
    await user.type(row, "deploy/**");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("adds a deny path and persists it into the spec document", async () => {
    let current: RunPolicySpec = BASE;
    render(<Harness initial={BASE} capture={(next) => (current = next)} />);
    await user.click(within(denySection()).getByRole("button", { name: "Add path" }));
    await user.type(screen.getByLabelText("Deny path 1"), ".github/workflows/**");
    expect(current.push_rules).toEqual({ deny_paths: [".github/workflows/**"] });
  });

  it("removes a row and drops the key entirely when the document is back to nothing", async () => {
    let current: RunPolicySpec = BASE;
    render(
      <Harness
        initial={{ ...BASE, push_rules: { deny_paths: [".github/workflows/**"] } }}
        capture={(next) => (current = next)}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Remove Deny path 1" }));
    expect(screen.queryByLabelText("Deny path 1")).not.toBeInTheDocument();
    // Absent, not `{}` or `{deny_paths: []}`: a policy written before this
    // editor existed has no key, and "no rules" has to serialise back to
    // exactly that (types.PushRulesSpec.IsSet's contract).
    expect(Object.keys(current)).not.toContain("push_rules");
  });

  it("the review list states 'No paths held for review' only when it is empty", () => {
    // A prop-driven rerender (not Harness): Harness's own state would ignore a
    // changed `initial` after mount, the same reason it exists for the typing
    // tests above.
    const { rerender } = render(<PushRulesSection spec={BASE} onSpecChange={() => {}} />);
    expect(screen.getByText("No paths held for review.")).toBeInTheDocument();
    rerender(
      <PushRulesSection
        spec={{ ...BASE, push_rules: { require_review_paths: ["deploy/**"] } }}
        onSpecChange={() => {}}
      />,
    );
    expect(screen.queryByText("No paths held for review.")).not.toBeInTheDocument();
  });

  it("the numeric fields round-trip: 0/blank drops the key, a value writes it", async () => {
    let current: RunPolicySpec = { ...BASE, push_rules: { deny_paths: ["x"] } };
    render(
      <Harness
        initial={{ ...BASE, push_rules: { deny_paths: ["x"] } }}
        capture={(next) => (current = next)}
      />,
    );
    const ceiling = screen.getByLabelText("Inspection ceiling (MiB)");
    await user.type(ceiling, "48");
    expect(current.push_rules?.max_inspect_pack_mib).toBe(48);

    // Clearing it back to blank drops the key entirely (0 IS "use the default").
    await user.clear(ceiling);
    expect(current.push_rules?.max_inspect_pack_mib).toBeUndefined();
  });

  it("warns when this run's only git-capable grant is ssh_key, and only while push_rules is actually set", () => {
    const { rerender } = render(
      <PushRulesSection
        spec={{ ...BASE, eligible_grants: [{ kind: "ssh_key", requires_approval: true }] }}
        onSpecChange={() => {}}
      />,
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    rerender(
      <PushRulesSection
        spec={{
          ...BASE,
          eligible_grants: [{ kind: "ssh_key", requires_approval: true }],
          push_rules: { deny_paths: [".github/workflows/**"] },
        }}
        onSpecChange={() => {}}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent(PUSH_RULES_WARN_SSH_ONLY);
  });

  it("warns per non-GitHub git_pat host when deny_paths is set", () => {
    render(
      <PushRulesSection
        spec={{
          ...BASE,
          eligible_grants: [{ kind: "git_pat", requires_approval: true, scope: { host: "dev.azure.com" } }],
          push_rules: { deny_paths: [".github/workflows/**"] },
        }}
        onSpecChange={() => {}}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent(/dev\.azure\.com/);
  });

  // F7/M6: the non-GitHub warning's gate is `deny.length > 0` specifically
  // (mirroring risk.go's own `len(spec.PushRules.DenyPaths) > 0`), not "any
  // push_rules at all" — review-only rules on a non-GitHub host raise no
  // warning, because the forge-read limitation risk.go documents is about
  // what a DENY match refuses, not what a review hold pauses.
  it("does not warn about a non-GitHub host when only review-only rules are set (no deny_paths)", () => {
    render(
      <PushRulesSection
        spec={{
          ...BASE,
          eligible_grants: [{ kind: "git_pat", requires_approval: true, scope: { host: "dev.azure.com" } }],
          push_rules: { require_review_paths: ["deploy/**"] },
        }}
        onSpecChange={() => {}}
      />,
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  // F9: the mock's own header (packet script) carries this exact count next
  // to the title in every one of its five "Try it" scenarios.
  it("the header states the deny/review counts, and they track the document", () => {
    const { rerender } = render(<PushRulesSection spec={BASE} onSpecChange={() => {}} />);
    expect(screen.getByText("0 deny · 0 held for review")).toBeInTheDocument();
    rerender(
      <PushRulesSection
        spec={{
          ...BASE,
          push_rules: { deny_paths: [".github/workflows/**", "**/*.pem"], require_review_paths: ["deploy/**"] },
        }}
        onSpecChange={() => {}}
      />,
    );
    expect(screen.getByText("2 deny · 1 held for review")).toBeInTheDocument();
  });

  // F8 (a11y): a row's error is not just visually beside its input — an
  // assistive technology needs aria-invalid + aria-describedby to associate
  // the two, the same way a native form field/error pairing works.
  it("ties a row's error to its input with aria-invalid and aria-describedby", () => {
    render(
      <PushRulesSection
        spec={{ ...BASE, push_rules: { require_review_paths: ["deploy/../etc"] } }}
        onSpecChange={() => {}}
      />,
    );
    const row = screen.getByLabelText("Hold for review path 1");
    const alert = screen.getByRole("alert");
    expect(row).toHaveAttribute("aria-invalid", "true");
    expect(row.getAttribute("aria-describedby")).toBe(alert.id);
    expect(alert.id).toBeTruthy();
  });

  it("a row with no error carries no aria-invalid/aria-describedby", () => {
    render(
      <PushRulesSection
        spec={{ ...BASE, push_rules: { deny_paths: [".github/workflows/**"] } }}
        onSpecChange={() => {}}
      />,
    );
    const row = screen.getByLabelText("Deny path 1");
    expect(row).not.toHaveAttribute("aria-invalid");
    expect(row).not.toHaveAttribute("aria-describedby");
  });

  // F8 (a11y): "Add path" should move focus straight to the row it just
  // created — otherwise a keyboard/screen-reader user has to hunt for it.
  it("focuses the new row when Add path is clicked", async () => {
    render(<Harness initial={BASE} />);
    await user.click(within(denySection()).getByRole("button", { name: "Add path" }));
    expect(screen.getByLabelText("Deny path 1")).toHaveFocus();

    // Fill the first row before adding a second — clicking Add path while a
    // row is still blank blurs (and F10 drops) that blank row first, which is
    // its own behavior (covered below), not what this test is pinning.
    await user.type(screen.getByLabelText("Deny path 1"), "a/**");
    await user.click(within(denySection()).getByRole("button", { name: "Add path" }));
    expect(screen.getByLabelText("Deny path 2")).toHaveFocus();
  });

  // F8 (a11y): removing a row should not just drop focus to the page body —
  // it moves to the row that shifted into the removed one's place, or the
  // last remaining row, or the Add-path button once the list is empty.
  it("moves focus sensibly after removing a row", async () => {
    render(
      <Harness
        initial={{ ...BASE, push_rules: { deny_paths: ["a/**", "b/**", "c/**"] } }}
      />,
    );
    // Removing the MIDDLE row: focus lands on the row that shifted up into
    // its place (what was "c/**", now at index 1).
    await user.click(screen.getByRole("button", { name: "Remove Deny path 2" }));
    expect(screen.getByLabelText("Deny path 2")).toHaveFocus();
    expect(screen.getByLabelText("Deny path 2")).toHaveValue("c/**");

    // Removing the (now) LAST row: nothing shifts up, so focus lands on the
    // new last row.
    await user.click(screen.getByRole("button", { name: "Remove Deny path 2" }));
    expect(screen.getByLabelText("Deny path 1")).toHaveFocus();

    // Removing the only remaining row: the list is empty, so focus lands on
    // "Add path" itself.
    await user.click(screen.getByRole("button", { name: "Remove Deny path 1" }));
    expect(within(denySection()).getByRole("button", { name: "Add path" })).toHaveFocus();
  });

  // F10: an added row left blank must not reach Save and come back as the
  // server's "empty entry" 400 with no row-level hint (empty is deliberately
  // not flagged live — pushRulePatternProblem's own doc comment). Blur drops
  // it; a row the operator actually typed into survives blur untouched.
  it("drops a row on blur if it was left empty, but keeps one that was filled in", async () => {
    // Renders with an existing (non-freshly-added) blank row and clicks
    // straight into it — independent of whether "Add path" itself moves
    // focus, so this pins ONLY the blur-drop behavior.
    let current: RunPolicySpec = BASE;
    render(
      <Harness
        initial={{ ...BASE, push_rules: { deny_paths: [""] } }}
        capture={(next) => (current = next)}
      />,
    );
    const blank = screen.getByLabelText("Deny path 1");
    await user.click(blank);
    await user.click(document.body); // blur without typing anything
    expect(screen.queryByLabelText("Deny path 1")).not.toBeInTheDocument();
    expect(Object.keys(current)).not.toContain("push_rules");

    await user.click(within(denySection()).getByRole("button", { name: "Add path" }));
    await user.type(screen.getByLabelText("Deny path 1"), ".github/workflows/**");
    await user.click(document.body); // blur a FILLED row
    expect(screen.getByLabelText("Deny path 1")).toBeInTheDocument();
    expect(current.push_rules).toEqual({ deny_paths: [".github/workflows/**"] });
  });

  // The panel's parseSpec is a bare cast: whatever the textarea parses to
  // arrives here as a "RunPolicySpec". A malformed push_rules document must
  // never throw out of the section into the route's ErrorBoundary, taking the
  // operator's draft with it — it reads as empty instead.
  it.each([
    ["deny_paths not an array", { deny_paths: "not-an-array" }],
    ["a non-string entry", { deny_paths: [7] }],
    ["max_inspect_pack_mib not a number", { max_inspect_pack_mib: "32" }],
  ])("renders without throwing when push_rules holds %s", (_label, push_rules) => {
    render(
      <PushRulesSection
        spec={{ ...BASE, push_rules: push_rules as unknown as RunPolicySpec["push_rules"] }}
        onSpecChange={() => {}}
      />,
    );
    expect(screen.getByText("Push rules")).toBeInTheDocument();
  });
});
