/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { SUBSCRIPTION_OAUTH_SECRET } from "../../../lib/types";
import { DEMOS } from "./demo-catalog";

// Keyless, workspace-free sandboxes an operator drives by hand. DEMOS also
// carries a harness-aware demo (needsModel) — kept out of this subset since
// it trades the shared invariants below (empty allowed_domains, a pasted
// command) for a real, egress-scoped agent task.
const KEYLESS = DEMOS.filter((d) => !d.needsModel);

// The original showcase quartet — the first four keyless EGRESS demos, each
// covering a distinct first_use_approval / allow-all combo. Two later egress
// keyless demos sit on different axes entirely and deliberately REUSE an
// earlier combo rather than inventing a new mode to distinguish, so both are
// excluded from the distinctness checks below and asserted on separately (the
// section filter also excludes the secrets demos, which cover a different axis
// — value governance, not first_use_approval/allow-all — and would otherwise
// swallow this quartet check):
// - record-a-policy reuses the open-egress combo (Record Mode, not an
//   egress-approval-mode showcase — recording needs egress wide open, that's
//   the point).
// - denied-however-spelled reuses lines-that-cant-be-crossed's open-egress
//   always-deny combo (the deny-list/normalization axis, not a new approval
//   mode — what it showcases is denied_domains beating allow-all and host
//   spelling normalization).
// - once-or-for-good reuses fail-then-approve's deny_with_review (the
//   decision-SCOPE axis, not a new first_use_approval mode — scope is
//   orthogonal to FirstUseMode).
const SHOWCASE_QUARTET = KEYLESS.filter(
  (d) =>
    d.section === "egress" &&
    d.id !== "record-a-policy" &&
    d.id !== "once-or-for-good" &&
    d.id !== "denied-however-spelled",
);

describe("demo catalog", () => {
  it("ships exactly fifteen keyless demos with distinct ids/titles", () => {
    expect(KEYLESS).toHaveLength(15);
    expect(new Set(KEYLESS.map((d) => d.id)).size).toBe(15);
    expect(new Set(KEYLESS.map((d) => d.title)).size).toBe(15);
  });

  it("sections split the original egress demos from the new secrets demos", () => {
    const egressIds = [
      "sealed-box",
      "fail-then-approve",
      "held-at-the-door",
      "lines-that-cant-be-crossed",
      "denied-however-spelled",
      "agent-in-the-box",
      "record-a-policy",
      "once-or-for-good",
    ];
    for (const id of egressIds) expect(DEMOS.find((d) => d.id === id)?.section).toBe("egress");
    const secretsIds = [
      "write-only-by-design",
      "key-never-in-the-box",
      "authorized-not-issued",
      // The per-KIND cards, in the ladder the section teaches: header-injected
      // -> piped -> resident -> re-originated -> refused.
      "rest-api-token",
      "pat-stdout-only",
      "ssh-briefly-resident",
      "github-app-broker",
      "sts-fail-closed",
    ];
    for (const id of secretsIds) expect(DEMOS.find((d) => d.id === id)?.section).toBe("secrets");
    expect(DEMOS).toHaveLength(egressIds.length + secretsIds.length);
  });

  it("every demo is CC1, auto-stops, and mounts/repos nothing", () => {
    for (const d of DEMOS) {
      expect(d.policy.min_confinement_class).toBe("CC1");
      expect(d.policy.auto_stop_after_sec ?? 0).toBeGreaterThan(0);
      // Workspace-free by construction — these fields must be absent on every demo.
      expect(d.policy.workspace_mounts).toBeUndefined();
      expect(d.policy.workspace_repos).toBeUndefined();
    }
  });

  it("every egress-section demo grants nothing (the secrets demos are the only ones that do)", () => {
    for (const d of DEMOS.filter((d) => d.section === "egress")) {
      expect(d.policy.eligible_grants).toBeUndefined();
    }
  });

  it("every egress-section keyless demo pins an empty allowed_domains (deny-all base) and needs no model", () => {
    for (const d of KEYLESS.filter((d) => d.section === "egress")) {
      expect(d.policy.allowed_domains).toEqual([]);
      expect(d.needsModel).toBeFalsy();
    }
  });

  // The api_key cards are the ones the proxy injects a HEADER for, so they are
  // the only ones whose grant host must also be allowlisted (injection never
  // widens egress — the allowlist still has to name the host). The other kinds
  // reach the broker's LOCAL mint route instead and need no egress at all,
  // which is exactly why they are asserted separately below.
  it("the secrets-section api_key demos use api_key-only grants, a host inside allowed_domains, and a non-reserved secret_name", () => {
    const injecting = DEMOS.filter(
      (d) => d.section === "secrets" && d.policy.eligible_grants?.some((g) => g.kind === "api_key"),
    );
    expect(injecting.map((d) => d.id)).toEqual([
      "key-never-in-the-box",
      "authorized-not-issued",
      "rest-api-token",
    ]);
    for (const d of injecting) {
      expect(d.needsSecret).toBeTruthy();
      for (const g of d.policy.eligible_grants!) {
        expect(g.kind).toBe("api_key");
        const scope = g.scope as { host?: string; secret_name?: string };
        expect(scope.host).toBeTruthy();
        expect(d.policy.allowed_domains).toContain(scope.host);
        expect(scope.secret_name).toBeTruthy();
        expect(scope.secret_name).not.toBe(SUBSCRIPTION_OAUTH_SECRET);
        expect(scope.secret_name).not.toBe("anthropic-managed-oauth");
      }
    }
  });

  // rest-api-token is the REALISTIC sibling of key-never-in-the-box: same law,
  // the header a real third-party API actually wants. If that format ever loses
  // its "Bearer %s" the card stops being the thing it claims to teach.
  it("rest-api-token wires the standard Authorization: Bearer shape", () => {
    const d = DEMOS.find((x) => x.id === "rest-api-token")!;
    const scope = d.policy.eligible_grants![0].scope as { header?: string; format?: string };
    expect(scope.header).toBe("Authorization");
    expect(scope.format).toBe("Bearer %s");
    expect(d.needsSecret).toBe("wardyn-demo-api-token");
  });

  // Every non-api_key secrets card, pinned against the SERVER rule that would
  // otherwise 400/422 it — these scopes are validated at policy write, so a
  // drifted one is a card whose Start can never succeed.
  it("the per-kind grants match what the server accepts, and name a non-reserved secret", () => {
    const RESERVED = ["wardyn-signing-key", "wardyn-session-key", "github-app-key", "github-app-id"];
    const byId = (id: string) => DEMOS.find((d) => d.id === id)!;

    // git_pat: host + secret_name. gitlab.example.com deliberately, NOT a
    // brokered forge — validateGrantLaneExclusivity refuses a git_pat for one.
    const pat = byId("pat-stdout-only");
    const patGrant = pat.policy.eligible_grants![0];
    expect(patGrant.kind).toBe("git_pat");
    const patScope = patGrant.scope as { host?: string; secret_name?: string };
    expect(patScope.host).toBe("gitlab.example.com");
    expect(patScope.secret_name).toBe(pat.needsSecret);
    expect(RESERVED).not.toContain(patScope.secret_name);
    // No egress: the helper mints over the proxy's LOCAL route.
    expect(pat.policy.allowed_domains).toEqual([]);

    // ssh_key: the host MUST be a supported SSH-over-443 provider or the policy
    // write 400s AND the startup mint never runs (no audit row to point at).
    const ssh = byId("ssh-briefly-resident");
    const sshGrant = ssh.policy.eligible_grants![0];
    expect(sshGrant.kind).toBe("ssh_key");
    const sshScope = sshGrant.scope as { host?: string; key_secret_ref?: string };
    expect(["github.com", "dev.azure.com"]).toContain(sshScope.host);
    expect(sshScope.key_secret_ref).toBe(ssh.needsSecret);
    expect(RESERVED).not.toContain(sshScope.key_secret_ref);
    // run-create unions ssh.github.com:443 in for us, so the card must not
    // pre-declare it — and its copy has to say where the host came from.
    expect(ssh.policy.allowed_domains).toEqual([]);
    expect(ssh.setupUi.join(" ")).toContain("ssh.github.com:443");

    // github_token: TEACH+GATE, and gated on the App rather than a secret —
    // needsSecret would drop the card from the walk, which is the one thing a
    // teaching card must never do.
    const app = byId("github-app-broker");
    const appGrant = app.policy.eligible_grants![0];
    expect(appGrant.kind).toBe("github_token");
    expect(app.needsGitHubApp).toBe(true);
    expect(app.needsSecret).toBeUndefined();
    const appScope = appGrant.scope as { repos?: string[]; permissions?: Record<string, string> };
    expect(appScope.repos?.every((r) => /^[\w.-]+\/[\w.-]+$/.test(r))).toBe(true);
    // READ permissions only: a write-capable github_token would floor the run
    // at CC3 and the card's setupUi says it does not.
    expect(Object.values(appScope.permissions ?? {})).toEqual(["read"]);

    // cloud_sts: an empty object scope, no secret, and an ENABLED Start — the
    // create refusal is the demo, so a gate here would delete it.
    const sts = byId("sts-fail-closed");
    const stsGrant = sts.policy.eligible_grants![0];
    expect(stsGrant.kind).toBe("cloud_sts");
    expect(stsGrant.scope).toEqual({});
    expect(sts.needsSecret).toBeUndefined();
    expect(sts.needsGitHubApp).toBeUndefined();
  });

  // Exactly one TEACH+GATE card today. The gate does NOT drop the step
  // (setup/steps.ts's stepOrder reads needsModel/needsSecret only), so a new
  // needsGitHubApp demo has to be a deliberate decision, not a copy-paste.
  it("ships exactly one needsGitHubApp demo, and it is not also needsSecret/needsModel", () => {
    const gated = DEMOS.filter((d) => d.needsGitHubApp);
    expect(gated.map((d) => d.id)).toEqual(["github-app-broker"]);
    expect(gated[0].needsSecret).toBeUndefined();
    expect(gated[0].needsModel).toBeFalsy();
  });

  it("authorized-not-issued's steps pin the real mint 409 body codes", () => {
    const d = DEMOS.find((x) => x.id === "authorized-not-issued")!;
    const allText = d.steps.map((s) => `${s.text} ${s.cmd ?? ""}`).join(" ");
    expect(allText).toContain('"code":"pending"');
    expect(allText).toContain('"code":"already_minted"');
  });

  it("the original showcase quartet covers distinct first_use_approval / allow-all combos", () => {
    const combos = SHOWCASE_QUARTET.map((d) => `${d.policy.first_use_approval}:${d.policy.allow_all_egress ?? false}`);
    expect(new Set(combos).size).toBe(4);
    // The showcase quartet in order.
    expect(combos).toEqual([
      "always_deny:false",
      "deny_with_review:false",
      "wait_for_review:false",
      "always_deny:true",
    ]);
  });

  it("every wide-open-egress demo carries a caution", () => {
    const withCaution = KEYLESS.filter((d) => d.caution);
    const openEgress = KEYLESS.filter((d) => d.policy.allow_all_egress);
    // Every demo that opens egress wide carries the honest Fence-plus-open
    // caution — lines-that-cant-be-crossed AND record-a-policy both do.
    expect(withCaution.map((d) => d.id).sort()).toEqual(openEgress.map((d) => d.id).sort());
    expect(withCaution.length).toBeGreaterThan(0);
    for (const d of withCaution) expect(d.caution!.length).toBeGreaterThan(40);
  });

  // sts-fail-closed is the ONE exception and deliberately so: its run is
  // refused at create, so no sandbox and no terminal ever exist to paste into.
  // Pinning it by id (rather than loosening the rule) keeps every other card
  // honest about being hands-on.
  it("every keyless demo has at least one command step to paste — except the one with no sandbox", () => {
    for (const d of DEMOS) for (const s of d.steps) expect(s.text.length).toBeGreaterThan(0);
    const noCmd = KEYLESS.filter((d) => !d.steps.some((s) => s.cmd));
    expect(noCmd.map((d) => d.id)).toEqual(["sts-fail-closed"]);
  });

  it("ships exactly one harness demo — needs a model, drives the agent via a terminal command", () => {
    const harness = DEMOS.filter((d) => d.needsModel);
    expect(harness).toHaveLength(1);
    const [d] = harness;
    // Interactive like the rest (the operator runs `claude` in the attached
    // terminal) — a command step to paste, not an autonomous task.
    expect(d.steps.some((s) => s.cmd?.includes("claude"))).toBe(true);
    // Egress is scoped to Anthropic, not deny-all like the keyless demos.
    expect(d.policy.allowed_domains.length).toBeGreaterThan(0);
    expect(d.policy.allowed_domains.every((h) => h.includes("anthropic.com"))).toBe(true);
  });

  // Regression: setupUi copy pointed users at a nonexistent "Access → Egress"
  // subsection (Egress is its own sibling wizard step, not nested under Access).
  it("setupUi never claims Egress lives under the Access step", () => {
    for (const d of DEMOS) {
      for (const line of d.setupUi) {
        expect(line).not.toMatch(/Access\s*→\s*Egress/);
      }
    }
  });

  // setupUi tells an operator which control to click, so it must name what the
  // UI actually shows. It has quoted three different vocabularies now: the
  // retired wizard's Select label ("Deny + review"), then the New run page's
  // consequence titles ("Deny, but ask" — UNLISTED_RULES, which died with the
  // Edit-hosts dialog). /runs/new authors the spec JSON through the shared
  // Policy panel today, so there is no prose title left to quote: the WIRE KEY
  // is the vocabulary, and the panel's Fields rail is where it's documented.
  //
  // Pinning each demo's OWN first_use_approval is also a stronger check than
  // the two it replaces — it covers every confined demo rather than the first
  // match per mode, and it ties the instructions to the policy the demo really
  // launches with instead of to a string table.
  it("every confined demo's setupUi names its own first_use_approval value", () => {
    for (const d of KEYLESS.filter((x) => !x.policy.allow_all_egress)) {
      expect(
        d.setupUi.some((line) => line.includes(`"${d.policy.first_use_approval}"`)),
        `${d.id} should name ${d.policy.first_use_approval}`,
      ).toBe(true);
    }
  });

  // Under allow-all, first_use_approval is inert (the proxy never raises an
  // approval for a host it already allows), so quoting it there would teach a
  // setting that does nothing. Those demos name the key that IS doing the work.
  it("every allow-all demo's setupUi names allow_all_egress instead", () => {
    const openEgress = KEYLESS.filter((d) => d.policy.allow_all_egress);
    expect(openEgress.length).toBeGreaterThan(0);
    for (const d of openEgress) {
      expect(
        d.setupUi.some((line) => line.includes("allow_all_egress")),
        `${d.id} should name allow_all_egress`,
      ).toBe(true);
    }
  });

  // The steps must not send anyone to a control that no longer exists — the
  // five-step wizard's, nor the Network card / Edit-hosts dialog / Record radio
  // the shared Policy panel replaced on /runs/new.
  it("no setupUi line names a retired control", () => {
    for (const d of DEMOS) {
      for (const line of d.setupUi) {
        expect(line).not.toMatch(/Egress step|Basics step|Confinement step|Review step|wizard/i);
        expect(line).not.toMatch(/Edit hosts|In Network|Pick Record\b/i);
      }
    }
  });
});
