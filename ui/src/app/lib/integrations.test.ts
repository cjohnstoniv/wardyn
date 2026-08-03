/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  T,
  CAPS,
  CATEGORY_META,
  AI_TYPES,
  RESIDENCY_META,
  SUBSCRIPTION_LANE_META,
  BEDROCK_LANE_META,
  IMPOSSIBLE,
  TOOLS,
  EGRESS_SUGGEST,
} from "./integrations";

// Sentinel byte-exact pins against the approved mock export
// (mockup2/wardyn-integrations.js `T`/`CAPS`/`EGRESS_SUGGEST`) — em-dashes, the
// curly single quotes in BLAST[1], and the middle dot in CAPS.azure()'s label
// are all significant and easy to flatten by hand-retyping. Full deep-equal +
// key-set verification against the mock's own literal lives in a one-off
// scratchpad script (real TS compile + eval, not a re-typed copy) — these
// sentinels are the fast, in-repo tripwire for the same class of drift.
describe("integrations — T canon sentinel pins", () => {
  it("pins plain entries verbatim, em-dashes included", () => {
    expect(T.LEDE).toBe(
      "Named connections to systems outside Wardyn — model providers, git hosts, egress redirects, your corporate proxy. Wardyn runs without any of them.",
    );
    expect(T.LAW).toBe(
      "A tool is what the image carries. An integration is what it connects through. Wardyn never installs tools into your image — it only wires them at run time.",
    );
    expect(T.X_SUB_DIRECT).toBe(
      "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting.",
    );
  });

  // The 13->10 Corporate-network restructure's changed/new copy — a corporate
  // proxy is now ordered BEFORE integrations in Getting Started (steps.ts), so
  // the banner points there instead of just offering an inline add, and the
  // embed note explains why host proxy / egress redirection stop appearing
  // once that step exists. EMPTY_MIRROR is GONE (superseded by EMPTY_EGRESS) —
  // asserting `in` rather than a value pins the deletion itself.
  it("pins the corp-network-restructure copy (changed + new), and confirms EMPTY_MIRROR is deleted", () => {
    expect(T.PROXY_BANNER).toBe(
      "A corporate proxy was detected and isn't configured — set it up under Corporate network in Getting started, or add the Host proxy integration here.",
    );
    expect(T.CAT_MIRROR).toBe(
      "Points outbound traffic — registries, container images, a specific host — at an internal mirror or appliance. Skip if the public endpoints are reachable.",
    );
    expect(T.FOOTNOTE).toBe(
      "Wardyn doesn't test-connect a stored credential. Everything here is what's stored and what Wardyn can see locally — the exceptions: the GitHub App's ref-confinement row (really asks GitHub) and the Test buttons on Host proxy and Egress redirection (really launch a throwaway probe).",
    );
    // Round E: the step must never CLAIM to be required — the proof is the
    // only required thing, and most hosts pass it in one click.
    expect(T.CORP_LEDE).toBe(
      "First, and usually ten seconds: prove a sandbox on this host can reach the internet, and every step after this one can trust the answer. On most hosts that's one click — Test connectivity, see Reached, keep moving. Configure something here only if this machine reaches the internet through a corporate proxy, or has to fetch through internal mirrors — the proof then runs through that same path, exactly as a run would.",
    );
    expect(T.CORP_LEDE).not.toContain("Required");
    expect(T.EMBED_SCOPE_NOTE).toBe(
      "Host proxy and egress redirection live one step back — Corporate network. On the full Integrations page all four categories appear.",
    );
    expect(T.EMPTY_EGRESS).toBe("None. Outbound traffic goes to the public endpoints.");
    expect(T.NOPROXY_NOTE).toBe("Not applied — Wardyn's own egress allowlist decides what a sandbox may reach.");
    expect(T.NOT_CONFIGURED).toBe("Not configured — sandboxes go direct");
    expect(T.CRED_URL_NOTE).toBe(
      "This URL has a username and password in it. Wardyn will store it as a secret so it isn't displayed or logged; the sandbox never holds it either way.",
    );
    expect("EMPTY_MIRROR" in T).toBe(false);
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

  it("pins BLAST verbatim, including the curly quotes around 'Describe your task'", () => {
    expect(T.BLAST).toHaveLength(4);
    expect(T.BLAST[1]).toBe("Wardyn's Composer loses its backend — ‘Describe your task’ disappears from New Run.");
    expect(T.BLAST[3]).toBe("The stored secret anthropic-api-key is not deleted — remove it under Secrets.");
  });
});

describe("integrations — CAPS capability-line notes", () => {
  it("pins a fact line (impossible-as-fact, never a toggle) verbatim", () => {
    expect(CAPS.key()[1]).toEqual({ label: "Codex CLI", fact: T.X_KEY_CODEX });
  });

  it("pins the middle dot in azure()'s combined label", () => {
    expect(CAPS.azure()[0].label).toBe("Claude Code · Codex CLI");
  });

  it("sub(hostCli) is the only row set that varies by argument (Wardyn features)", () => {
    const off = CAPS.sub(false).find((r) => r.label === "Wardyn features");
    const on = CAPS.sub(true).find((r) => r.label === "Wardyn features");
    expect(off?.on).toBe(true);
    expect(on?.on).toBe(false);
  });
});

describe("integrations — structured metadata is grounded in the T/CAPS canon above", () => {
  it("CATEGORY_META's skip-if line IS T.CAT_* (same string, not a re-typed copy)", () => {
    expect(CATEGORY_META.ai_provider.skipIfLine).toBe(T.CAT_AI);
    expect(CATEGORY_META.scm_host.skipIfLine).toBe(T.CAT_SCM);
    expect(CATEGORY_META.artifact_mirror.skipIfLine).toBe(T.CAT_MIRROR);
    expect(CATEGORY_META.host_proxy.skipIfLine).toBe(T.CAT_PROXY);
  });

  it("AI_TYPES.desc IS the matching T.TY_* for the three single-lane key types", () => {
    expect(AI_TYPES.anthropic_api_key.desc).toBe(T.TY_KEY);
    expect(AI_TYPES.openai_api_key.desc).toBe(T.TY_OPENAI);
    expect(AI_TYPES.azure_openai.desc).toBe(`${T.TY_AZURE} Key or Entra.`);
  });

  it("AI_TYPES.capabilityPreview wires straight to CAPS (no duplicated tables)", () => {
    expect(AI_TYPES.anthropic_api_key.capabilityPreview()).toEqual(CAPS.key());
    expect(AI_TYPES.bedrock.capabilityPreview()).toEqual(CAPS.bedrock());
    expect(AI_TYPES.anthropic_subscription.capabilityPreview(true)).toEqual(CAPS.sub(true));
  });

  it("IMPOSSIBLE reasons are the exact same T.X_* strings CAPS' fact rows use", () => {
    expect(IMPOSSIBLE.anthropic_api_key?.codex_cli).toBe(T.X_KEY_CODEX);
    expect(IMPOSSIBLE.anthropic_subscription?.direct_api).toBe(T.X_SUB_DIRECT);
    expect(IMPOSSIBLE.azure_openai?.claude_code).toBe(T.X_AZURE_HARNESS);
    expect(IMPOSSIBLE.azure_openai?.codex_cli).toBe(T.X_AZURE_HARNESS);
  });

  it("SUBSCRIPTION_LANE_META tooltips ARE T.MANAGED_LINE / T.HOSTCLI_LINE verbatim", () => {
    expect(SUBSCRIPTION_LANE_META.managed.tooltip).toBe(T.MANAGED_LINE);
    expect(SUBSCRIPTION_LANE_META.resident_host.tooltip).toBe(T.HOSTCLI_LINE);
  });

  it("RESIDENCY_META covers all six kinds with a label + tone + tooltip", () => {
    for (const kind of [
      "proxy_injected",
      "brokered_mint",
      "resident_mount",
      "resident_env",
      "control_plane",
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

  it("TOOLS covers all six Tools-tab rows, verbatim against the mock's toolRow calls", () => {
    expect(TOOLS.git.wire).toBe(
      "Clone and push rerouted through the broker (App), a credential helper (PAT), or a key file (SSH) — set up at run start.",
    );
    expect(TOOLS.git.extra).toBe("In every Wardyn image.");
    expect(TOOLS.package_managers.wire).toBe(
      "Per-tool config files generated at run start (.npmrc, pip.conf, …); the mirror token is injected proxy-side.",
    );
    expect(TOOLS.gh_cli.wire).toBe(
      "Recognized on the host, never wired. Its token is broad; Wardyn never imports it — use an SCM host integration instead.",
    );
    for (const id of ["git", "package_managers", "claude_code", "codex_cli", "gh_cli", "own_tools"] as const) {
      expect(TOOLS[id].wire).toBeTruthy();
    }
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
