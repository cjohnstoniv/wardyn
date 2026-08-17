/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  T,
  CAPS,
  AI_TYPES,
  RESIDENCY_META,
  SUBSCRIPTION_LANE_META,
  BEDROCK_LANE_META,
  IMPOSSIBLE,
  EGRESS_SUGGEST,
} from "./integrations";

// Sentinel byte-exact pins against the approved mock export
// (mockup2/wardyn-integrations.js `T`/`CAPS`/`EGRESS_SUGGEST`) — em-dashes and
// the middle dot in CAPS.azure()'s label are all significant and easy to
// flatten by hand-retyping. Full deep-equal +
// key-set verification against the mock's own literal lives in a one-off
// scratchpad script (real TS compile + eval, not a re-typed copy) — these
// sentinels are the fast, in-repo tripwire for the same class of drift.
describe("integrations — T canon sentinel pins", () => {
  it("pins plain entries verbatim, em-dashes included", () => {
    expect(T.X_SUB_DIRECT).toBe(
      "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting.",
    );
  });

  // The Corporate-network consolidation's changed/new copy. Corporate network
  // is now the SINGLE home for a corporate proxy and for egress redirection —
  // both categories left /integrations entirely — so the banner offers no
  // "…or add it here" alternative, the footnote names one exception rather
  // than three, and the two surfaces carry the two halves of one pointer
  // (EMBED_SCOPE_NOTE back a step, CORP_POINTER forward into Getting
  // started). Retired copy is DELETED, and asserting `in` pins each deletion.
  it("pins the consolidation copy (changed + new), and confirms the retired lines are deleted", () => {
    expect(T.LEDE).toBe(
      "Named connections to systems outside Wardyn — model providers, git hosts, package feeds, container registries, cloud providers, data stores, MCP servers, work tracking, observability, or anything else as an Other service. Wardyn runs without any of them.",
    );
    expect(T.PROXY_BANNER).toBe(
      "A corporate proxy was detected and isn't configured — set it up under Network in Getting started, where the connectivity probe proves it.",
    );
    expect(T.FOOTNOTE).toBe(
      "Wardyn doesn't test-connect a stored credential. Everything here is what's stored and what Wardyn can see locally — the one exception is the GitHub App's ref-confinement row, which really asks GitHub.",
    );
    expect(T.CORP_POINTER).toBe(
      "Your corporate proxy and any egress redirects aren't integrations — they're network topology, and they live in Network under Getting started, on the same screen as the probe that proves them.",
    );
    // Round E: the step must never CLAIM to be required — the proof is the
    // only required thing, and most hosts pass it in one click.
    expect(T.CORP_LEDE).toBe(
      "First, and usually ten seconds: prove a sandbox on this host can reach the internet, and every step after this one can trust the answer. On most hosts that's one click — Test connectivity, see Reached, keep moving. Configure something here only if this machine reaches the internet through a corporate proxy, or has to fetch through internal mirrors — the proof then runs through that same path, exactly as a run would.",
    );
    expect(T.CORP_LEDE).not.toContain("Required");
    expect(T.EMBED_SCOPE_NOTE).toBe(
      "This is the full Integrations page. Your corporate proxy and any egress redirects live one step back, in Network.",
    );
    // The Tools tab no longer exists anywhere — the embed note must not
    // resurrect it.
    expect(T.EMBED_SCOPE_NOTE).not.toContain("Tools tab");
    // …and NOT the old tail claiming the full page still shows all four.
    expect(T.EMBED_SCOPE_NOTE).not.toContain("all four categories");
    // …and not UX-8's false claim either — the embed renders all ten
    // categories (INTEGRATION_GROUPS), not "the same two".
    expect(T.EMBED_SCOPE_NOTE).not.toContain("same two categories");
    expect(T.NOPROXY_NOTE).toBe("Not applied — Wardyn's own egress allowlist decides what a sandbox may reach.");
    expect(T.NOT_CONFIGURED).toBe("Not configured — sandboxes go direct");
    expect(T.CRED_URL_NOTE).toBe(
      "This URL has a username and password in it. Wardyn will store it as a secret so it isn't displayed or logged; the sandbox never holds it either way.",
    );
    // Copy that lost its surface is deleted, not left lying around: EMPTY_MIRROR
    // went with the pre-Corporate-network rename, and these four went with the
    // two categories themselves.
    for (const gone of ["EMPTY_MIRROR", "EMPTY_EGRESS", "EMPTY_PROXY", "CAT_MIRROR", "CAT_PROXY"]) {
      expect(gone in T).toBe(false);
    }
  });

  it("pins the Test-probe verdict strings verbatim (T.TEST_STANDING makes clear these are honest, not inferred)", () => {
    // TEST_OK/TEST_BLOCKED/TEST_OK_CUSTOM mirror the BACKEND's own wording
    // (classifyProxyProbe, internal/api/site_config_probe.go) — the payload
    // check named on a pass, "either endpoint" + the real cause on a failure,
    // and a custom pass claiming only that the request completed.
    expect(T.TEST_OK).toBe(
      "Reached www.msftconnecttest.com/connecttest.txt and detectportal.firefox.com/success.txt through wardyn-proxy chained to http://proxy.corp.acme.com:8080 in 240ms — payloads matched, the full chain a run takes.",
    );
    expect(T.TEST_BLOCKED).toBe(
      "Could not reach either endpoint (www.msftconnecttest.com, detectportal.firefox.com): connection refused (or the host is unreachable) — probed through wardyn-proxy chained to http://proxy.corp.acme.com:8080.",
    );
    expect(T.TEST_OK_CUSTOM).toBe(
      "The request to nexus.corp.internal/repository/health completed through wardyn-proxy chained to http://proxy.corp.acme.com:8080 in 90ms.",
    );
    expect(T.TEST_BYPASS).toBe(
      "The mirror answered, but registry.npmjs.org is still reachable from a sandbox — runs can still bypass the mirror.",
    );
    expect(T.TEST_NORUNNER).toBe(
      "No runner is configured on this host — there's nothing to launch a probe with. Configure a barrier first, then test.",
    );
    expect(T.TEST_STANDING).toBe(
      "Tested from a throwaway sandbox on this host — the same path a run takes. Nothing else is inferred from the result.",
    );
  });

  // The gate/custom/intercepted strings the corp-network gating round added —
  // pinned because a state's SEMANTICS carry a safety claim here: a custom
  // pass must keep claiming less than a verified one, and intercepted must
  // never collapse into a generic failure.
  it("pins the gate ladder + custom-URL + intercepted copy verbatim", () => {
    expect(T.GATE_UNTESTED).toBe(
      "One probe, and this step is done — everything after it assumes the network works. A minute now instead of a fake credential failure two steps later.",
    );
    // The forced egress-tab visit died with round E — the mechanic that made
    // the least-common feature feel mandatory. Pinned deleted.
    expect("GATE_EGRESS_UNSEEN" in T).toBe(false);
    // Every gate state's bold headline (the footer's two-line treatment).
    expect(T.GATE_HEAD_UNTESTED).toBe("Connectivity isn't proven yet");
    expect(T.GATE_HEAD_RUNNING).toBe("Probe in flight");
    expect(T.GATE_RUNNING).toBe(
      "A throwaway sandbox is reaching for the connectivity endpoints right now. The result decides whether this step can hand off.",
    );
    expect(T.GATE_HEAD_BLOCKED).toBe("The probe came back blocked");
    expect(T.GATE_HEAD_INTERCEPTED).toBe("Something intercepted the probe");
    expect(T.GATE_HEAD_EGRESS_UNTESTED).toBe("Redirects aren't proven yet");
    expect(T.GATE_HEAD_EGRESS_FAILING).toBe("A redirect isn't being enforced");
    expect(T.GATE_HEAD_NORUNNER).toBe("Nothing to test with");
    expect(T.GATE_HEAD_CUSTOM_ON).toBe("Passing on a weaker proof");
    expect(T.GATE_EGRESS_UNTESTED).toBe(
      "Every configured redirect has to prove reached before this step hands off — test the rows above, or remove them.",
    );
    expect(T.GATE_BLOCKED).toBe(
      "The probe came back blocked. Fix the proxy above and test again — or, if no public endpoint will ever answer here, test against a URL of your own.",
    );
    expect(T.GATE_INTERCEPTED).toBe(
      "Something intercepted the probe, so egress isn't open yet. Fix the proxy above, or test against a URL of your own if this network has no public egress by design.",
    );
    expect(T.GATE_CUSTOM_ON).toBe(
      "Passing on a custom endpoint — the request completed, which is weaker than the built-in check. Good enough to continue; worth re-running against the built-in endpoints if this host ever gets public egress.",
    );
    expect(T.NORUNNER_NOTE).toBe(
      "Nothing was proven here — there's no runner to launch a probe with, and Wardyn doesn't demand proof it can't collect. Configure a barrier, then come back and test.",
    );
    expect(T.CUSTOM_CAVEAT).toBe(
      "Wardyn has no idea what that endpoint should return, so it can only report that the request completed — not that the internet was reached. A weaker proof than the built-in check, and it is recorded as one.",
    );
    expect(T.PROBE_ENDPOINTS).toContain("www.msftconnecttest.com/connecttest.txt (Windows NCSI)");
    expect(T.PROBE_ENDPOINTS).toContain("api.anthropic.com and github.com are deliberately not among them");
    expect(T.INTERCEPT_MEANS).toContain("the request left the host and something replied");
    expect(T.EGRESS_SEEN_EMPTY).toBe(
      "Nothing redirected on this host — add one above if a run ever has to fetch through a mirror.",
    );
    expect(T.EGRESS_DESC).toContain("The least-common thing in setup");
    expect(T.NET_ONLY_TIP).toContain("no tool config file is generated");
    // The one-string hint the custom block replaced — pinned deleted.
    expect("TEST_CUSTOM_HINT" in T).toBe(false);
  });

  it("pins EGRESS_SUGGEST verbatim — 11 entries, label IS the url, including both container-images rows", () => {
    expect(EGRESS_SUGGEST).toHaveLength(11);
    expect(EGRESS_SUGGEST[0]).toEqual(["https://registry.npmjs.org", "npm"]);
    expect(EGRESS_SUGGEST[9]).toEqual(["https://registry-1.docker.io", "container images"]);
    expect(EGRESS_SUGGEST[10]).toEqual(["https://ghcr.io", "container images"]);
    expect(EGRESS_SUGGEST.filter(([, eco]) => eco === "container images")).toHaveLength(2);
  });
});

describe("integrations — CAPS capability-line notes", () => {
  it("pins a fact line (impossible-as-fact, never a toggle) verbatim", () => {
    expect(CAPS.key()[1]).toEqual({ label: "Codex CLI", fact: T.X_KEY_CODEX });
  });

  it("sub(hostCli) is the only row set that varies by argument (Wardyn features)", () => {
    const off = CAPS.sub(false).find((r) => r.label === "Wardyn features");
    const on = CAPS.sub(true).find((r) => r.label === "Wardyn features");
    expect(off?.on).toBe(true);
    expect(on?.on).toBe(false);
  });
});

describe("integrations — structured metadata is grounded in the T/CAPS canon above", () => {

  // The two-category pin used to read CATEGORY_META, which existed for the
  // deleted /integrations page's section headers. The IntegrationCategory type
  // is what enforces the rule now — an integration is an account with a system
  // outside Wardyn, and Corporate network owns the network topology.

  // Two single-lane key types now, not three: azure_openai was the third and it
  // was removed in 0.5 — its one capability powered the deleted AI Run Composer.
  it("AI_TYPES.desc IS the matching T.TY_* for the single-lane key types", () => {
    expect(AI_TYPES.anthropic_api_key.desc).toBe(T.TY_KEY);
    expect(AI_TYPES.openai_api_key.desc).toBe(T.TY_OPENAI);
  });

  it("AI_TYPES.capabilityPreview wires straight to CAPS (no duplicated tables)", () => {
    expect(AI_TYPES.anthropic_api_key.capabilityPreview()).toEqual(CAPS.key());
    expect(AI_TYPES.bedrock.capabilityPreview()).toEqual(CAPS.bedrock());
    expect(AI_TYPES.anthropic_subscription.capabilityPreview(true)).toEqual(CAPS.sub(true));
  });

  it("IMPOSSIBLE reasons are the exact same T.X_* strings CAPS' fact rows use", () => {
    expect(IMPOSSIBLE.anthropic_api_key?.codex_cli).toBe(T.X_KEY_CODEX);
    expect(IMPOSSIBLE.anthropic_subscription?.direct_api).toBe(T.X_SUB_DIRECT);
  });

  it("SUBSCRIPTION_LANE_META tooltips ARE T.MANAGED_LINE / T.HOSTCLI_LINE verbatim", () => {
    expect(SUBSCRIPTION_LANE_META.managed.tooltip).toBe(T.MANAGED_LINE);
    expect(SUBSCRIPTION_LANE_META.resident_host.tooltip).toBe(T.HOSTCLI_LINE);
  });

  it("RESIDENCY_META covers all seven kinds with a label + tone + tooltip", () => {
    for (const kind of [
      "proxy_injected",
      "brokered_mint",
      "resident_mount",
      "resident_env",
      "control_plane",
      "notbuilt",
      "varies",
    ] as const) {
      expect(RESIDENCY_META[kind].label).toBeTruthy();
      expect(RESIDENCY_META[kind].tooltip).toBeTruthy();
      expect(["success", "warning", "neutral"]).toContain(RESIDENCY_META[kind].tone);
    }
  });

  // control_plane is distinct from `varies`: Azure has exactly ONE lane (the
  // mock's "cp" resChip kind), never "more than one lane" — the two kinds must
  // not collapse to the same label/tooltip.
  it("control_plane reads distinctly from varies (Azure has one lane, not several)", () => {
    expect(RESIDENCY_META.control_plane.label).toBe("control-plane side");
    expect(RESIDENCY_META.control_plane.label).not.toBe(RESIDENCY_META.varies.label);
  });

  it("BEDROCK_LANE_META covers all four lanes and points each at a residency kind", () => {
    for (const lane of ["bearer", "sso", "aws_dir", "static"] as const) {
      expect(RESIDENCY_META[BEDROCK_LANE_META[lane].residency]).toBeDefined();
    }
    // Raw access keys are env vars, not a mounted file — the one lane that
    // differs from its "resident" siblings.
    expect(BEDROCK_LANE_META.static.residency).toBe("resident_env");
    expect(BEDROCK_LANE_META.aws_dir.residency).toBe("resident_mount");
  });
});
