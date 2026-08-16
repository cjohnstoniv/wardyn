/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations copy canon + structured metadata — verbatim transcription of
// the approved mock export's `T` (copy canon) and `CAPS` (capability-line
// notes) from mockup/wardyn-integrations.js, plus the structured metadata the
// later Integrations pages (list/add/detail) will render from: which category
// a type belongs to, what each AI type's picker card says, how a credential's
// residency reads as a chip, and which capabilities are flatly impossible for
// a given type (never a toggle — a stated fact, sourced from CAPS' own `fact`
// lines below). Pure TS — no React, no fetch, no DOM.
//
// Three layers, per the mock's own header: credential (dumb material) →
// integration (named, configured) → capabilities (ON / OFF / impossible-as-
// fact). There is no test-connect anywhere by design (T.FOOTNOTE).

// ============================ COPY CANON (verbatim) ============================
export const T = {
  LEDE: "Named connections to systems outside Wardyn — model providers, git hosts, package feeds, container registries, cloud providers, data stores, MCP servers, work tracking, observability, or anything else as an Other service. Wardyn runs without any of them.",
  // One exception now, not three: the Host proxy / Egress redirection Test
  // buttons left this page with their categories — Corporate network owns both,
  // and its gate is where a redirect proves itself (T.CORP_POINTER).
  FOOTNOTE:
    "Wardyn doesn't test-connect a stored credential. Everything here is what's stored and what Wardyn can see locally — the one exception is the GitHub App's ref-confinement row, which really asks GitHub.",
  // No "…or add it here" alternative any more — there is exactly one place a
  // proxy is configured, and the banner names it.
  PROXY_BANNER:
    "A corporate proxy was detected and isn't configured — set it up under Corporate network in Getting started, where the connectivity probe proves it.",
  CORP_LEDE:
    "First, and usually ten seconds: prove a sandbox on this host can reach the internet, and every step after this one can trust the answer. On most hosts that's one click — Test connectivity, see Reached, keep moving. Configure something here only if this machine reaches the internet through a corporate proxy, or has to fetch through internal mirrors — the proof then runs through that same path, exactly as a run would.",
  // The pair of pointers at the SAME consolidation, one per surface: the
  // Getting Started embed points one step back, the full page points forward
  // into Getting started. Both verbatim from the mock (round G).
  // UX-8: was "...shows the same two categories" — false (the embed renders
  // all ten). State what's actually true instead of a category count that
  // can go stale the next time a category is added.
  EMBED_SCOPE_NOTE:
    "This is the full Integrations page. Your corporate proxy and any egress redirects live one step back, in Corporate network.",
  CORP_POINTER:
    "Your corporate proxy and any egress redirects aren't integrations — they're network topology, and they live in Corporate network under Getting started, on the same screen as the probe that proves them.",
  EVIDENCE_HEAD: "What Wardyn found on this host",
  EVIDENCE_EXPLAIN:
    "Read from this machine's environment and git config. Wardyn does not use these automatically: a sandbox gets only what you configure below.",
  EVIDENCE_NONE: "Nothing found — no proxy variables in this machine's environment, none in git config.",
  NOPROXY_NOTE: "Not applied — Wardyn's own egress allowlist decides what a sandbox may reach.",
  CONFIG_HEAD: "What sandboxes will use",
  NOT_CONFIGURED: "Not configured — sandboxes go direct",
  PROXY_URL_HINT:
    "The URL sandboxes chain through. http:// only — https is not supported (the hop to your proxy is a plaintext CONNECT and can't be TLS-wrapped). Stored as plain configuration — it's topology, not a credential — unless it carries a username and password.",
  CRED_URL_NOTE:
    "This URL has a username and password in it. Wardyn will store it as a secret so it isn't displayed or logged; the sandbox never holds it either way.",
  SECRET_INSTEAD_HINT:
    "For a URL already in the store, or when policy requires it. The store is write-only — the value can't be read back here.",
  EGRESS_DESC:
    "The least-common thing in setup — most networks need nothing here. It exists for hosts that must fetch through an internal mirror or appliance: point outbound traffic — registries, container images, a specific host — somewhere else. Anything you do configure has to prove it works before this step hands off.",
  // TEST_OK / TEST_BLOCKED / TEST_OK_CUSTOM are FIXTURE mirrors of the
  // backend's own detail wording (classifyProxyProbe, site_config_probe.go) —
  // the real app renders whatever the server said; these exist so tests and
  // stories exercise the true shape. The mock renders the same sentences.
  TEST_OK:
    "Reached www.msftconnecttest.com/connecttest.txt and detectportal.firefox.com/success.txt through wardyn-proxy chained to http://proxy.corp.acme.com:8080 in 240ms — payloads matched, the full chain a run takes.",
  TEST_BLOCKED:
    "Could not reach either endpoint (www.msftconnecttest.com, detectportal.firefox.com): connection refused (or the host is unreachable) — probed through wardyn-proxy chained to http://proxy.corp.acme.com:8080.",
  TEST_BYPASS:
    "The mirror answered, but registry.npmjs.org is still reachable from a sandbox — runs can still bypass the mirror.",
  TEST_NORUNNER:
    "No runner is configured on this host — there's nothing to launch a probe with. Configure a barrier first, then test.",
  TEST_STANDING:
    "Tested from a throwaway sandbox on this host — the same path a run takes. Nothing else is inferred from the result.",
  TEST_PROXY_HINT:
    "Runs either way — through the configured proxy when there is one, direct when there isn't. Launches a throwaway confined probe and reports what actually happened; a real sandbox launch — seconds, not instant.",
  // The gate's own sentences (steps.ts's corpNetworkGate): a block reason
  // while Next is locked, a neutral standing note for the two states that
  // unlock it without the full builtin proof (no_runner / a custom pass).
  // Each state also carries a HEADLINE (GATE_HEAD_*) the footer renders bold
  // above the sentence. What is required is the PROOF, not configuration — no
  // rung demands a visit to Egress redirection.
  GATE_UNTESTED:
    "One probe, and this step is done — everything after it assumes the network works. A minute now instead of a fake credential failure two steps later.",
  GATE_HEAD_UNTESTED: "Connectivity isn't proven yet",
  GATE_HEAD_RUNNING: "Probe in flight",
  GATE_RUNNING:
    "A throwaway sandbox is reaching for the connectivity endpoints right now. The result decides whether this step can hand off.",
  GATE_HEAD_BLOCKED: "The probe came back blocked",
  GATE_HEAD_INTERCEPTED: "Something intercepted the probe",
  GATE_HEAD_EGRESS_UNTESTED: "Redirects aren't proven yet",
  GATE_HEAD_EGRESS_FAILING: "A redirect isn't being enforced",
  GATE_HEAD_NORUNNER: "Nothing to test with",
  GATE_HEAD_CUSTOM_ON: "Passing on a weaker proof",
  GATE_EGRESS_UNTESTED:
    "Every configured redirect has to prove reached before this step hands off — test the rows above, or remove them.",
  GATE_BLOCKED:
    "The probe came back blocked. Fix the proxy above and test again — or, if no public endpoint will ever answer here, test against a URL of your own.",
  GATE_INTERCEPTED:
    "Something intercepted the probe, so egress isn't open yet. Fix the proxy above, or test against a URL of your own if this network has no public egress by design.",
  GATE_CUSTOM_ON:
    "Passing on a custom endpoint — the request completed, which is weaker than the built-in check. Good enough to continue; worth re-running against the built-in endpoints if this host ever gets public egress.",
  NORUNNER_NOTE:
    "Nothing was proven here — there's no runner to launch a probe with, and Wardyn doesn't demand proof it can't collect. Configure a barrier, then come back and test.",
  // Intercepted is a variant of blocked, rendered apart — "nothing answered"
  // and "something answered and it wasn't the endpoint" send an operator to
  // different people.
  INTERCEPT_MEANS:
    "Different problem from a connection failure: the request left the host and something replied. Traffic on this network is being intercepted and inspected — talk to whoever runs the proxy, or point the check at an endpoint you know your network can reach.",
  PROBE_ENDPOINTS:
    "Two endpoints are tried — www.msftconnecttest.com/connecttest.txt (Windows NCSI) and detectportal.firefox.com/success.txt (Firefox) — so one blocked endpoint doesn't fail the probe. Each publishes a known fixed payload, and matching it is what catches a block page replying 200 OK; a vendor API has no such payload to match. Blocking these breaks the operating system's own network indicator, which is what makes them close to unblockable. api.anthropic.com and github.com are deliberately not among them: plenty of organisations block them, and a false “no internet” would stop setup dead on a healthy network.",
  // The custom-URL escape — revealed only after a failure, never on arrival.
  CUSTOM_URL_HINT:
    "Something on your network that answers — an internal service, a mirror, your own host. Not stored, and it changes nothing about how later runs reach the network.",
  CUSTOM_URL_WHY:
    "No public endpoint will answer on an internal-only or air-gapped host. Point the check at something yours instead, and it goes back to proving egress works rather than that the public internet does.",
  TEST_OK_CUSTOM:
    "The request to nexus.corp.internal/repository/health completed through wardyn-proxy chained to http://proxy.corp.acme.com:8080 in 90ms.",
  CUSTOM_CAVEAT:
    "Wardyn has no idea what that endpoint should return, so it can only report that the request completed — not that the internet was reached. A weaker proof than the built-in check, and it is recorded as one.",
  CUSTOM_REJECT_WHY:
    "Checked server-side before any sandbox starts — schemes, malformed hosts and shell metacharacters are refused there, and the server's own message is what you see.",
  EGRESS_SEEN_EMPTY:
    "Nothing redirected on this host — add one above if a run ever has to fetch through a mirror.",
  NET_ONLY_TIP:
    "Redirected at the network layer only — no tool config file is generated. An npm or pip row also gets a .npmrc / pip.conf written at run start; a container registry or bare host has no such file, and needs none: anything fetching this endpoint is rerouted.",
  X_KEY_CODEX: "Codex CLI speaks the OpenAI API only — an Anthropic key can't drive it. Not a setting.",
  X_SUB_CODEX: "Codex CLI speaks the OpenAI API only — a Claude login can't drive it. Not a setting.",
  X_BEDROCK_CODEX: "Codex CLI speaks the OpenAI API only — Bedrock can't drive it. Not a setting.",
  X_OPENAI_CLAUDE: "Claude Code speaks the Anthropic API only — an OpenAI key can't drive it. Not a setting.",
  X_SUB_DIRECT:
    "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting.",
  BEDROCK_FEATURES:
    "Wardyn's own features reach Bedrock through the AWS credential chain — the same lane this integration uses.",
  WRITE_ONLY: "The store is write-only: the value can't be read back.",
  MANAGED_LINE: "One login in a sandbox; the token is injected proxy-side and the sandbox holds only an inert sentinel.",
  HOSTCLI_LINE: "Uses the ~/.claude login on this host, mounted read-only into the run.",
  VIEWER_HINT: "Operator role required",
  TY_KEY: "Drives Claude Code, direct API calls, and Wardyn's features. Never resident.",
  TY_OPENAI: "Drives Codex CLI, direct API calls, and Wardyn's features.",
  // "Agent in the box" Getting-Started step (v0.5 local/design-prompts-v0.5/
  // prompt-v1-demo-step.md) — not a mock-export transcription like the rest of
  // this file (no mockup round covers this step yet); copy is verbatim from
  // that design prompt.
  // Claude Code only (H3, post-review fix): the catalog's task/policy are
  // Anthropic-specific, and the codex-cli agent image carries no claude
  // binary — an OpenAI-only deployment stays locked, honestly, with a reason.
};

// ============================ EGRESS REDIRECT SUGGESTIONS (verbatim) ============================
// The Corporate network / Egress redirection "From" combobox's suggested-source
// list (mockup's EGRESS_SUGGEST) — the label IS the URL; the ecosystem is only a
// muted secondary hint, not a selectable field (typing anything else — a full
// URL, a bare host, an IP — is just as valid; see T.EGRESS_DESC).
export const EGRESS_SUGGEST: ReadonlyArray<readonly [url: string, ecosystem: string]> = [
  ["https://registry.npmjs.org", "npm"],
  ["https://pypi.org/simple", "pip"],
  ["https://files.pythonhosted.org", "pip"],
  ["https://crates.io", "cargo"],
  ["https://static.crates.io", "cargo"],
  ["https://repo.maven.apache.org/maven2", "maven"],
  ["https://proxy.golang.org", "go"],
  ["https://sum.golang.org", "go"],
  ["https://api.nuget.org/v3/index.json", "nuget"],
  ["https://registry-1.docker.io", "container images"],
  ["https://ghcr.io", "container images"],
];

// The per-tool config file an ecosystem-tier redirect generates at run start
// (the mock's cfgFor) — display-only, for the expanded row's "Runs also get a
// generated <file> pointing at the mirror" line. A `from` outside this set
// has no entry and renders the network-only tip instead (T.NET_ONLY_TIP).
export const ECOSYSTEM_CONFIG_FILE: Readonly<Record<string, string>> = {
  npm: ".npmrc",
  pip: "pip.conf",
  cargo: "config.toml",
  maven: "settings.xml",
  go: "GOPROXY",
  nuget: "NuGet.config",
};

// ============================ CAPABILITY-LINE NOTES (verbatim) ============================
// mockup/wardyn-integrations.js's `CAPS` — one capability table per AI
// credential type, rendered as ON (note) / OFF-but-fixable (`note` with no
// `fact` — `sub`'s hostCli row below IS this case: the mock predicted it
// wouldn't occur for these five, but the sealed host-CLI lane is a real
// counter-example) / impossible-as-fact (`fact`, always one of T's X_* lines
// above). `sub`'s Wardyn-features row depends on whether the subscription is
// the sealed host-CLI lane (hostCli) — the only capability row that varies by
// anything other than the type itself.
export interface CapabilityRow {
  label: string;
  /** Present (true) when this capability works today. */
  on?: boolean;
  /** This is the type's CURRENT default lane for the capability (informational). */
  def?: boolean;
  /** Selecting this credential type WOULD become the default for the capability. */
  makeDefault?: boolean;
  /** Shown when `on` — what actually happens. With `on: false` and no `fact`,
   *  shown instead as why this instance doesn't have it (off, not impossible). */
  note?: string;
  /** Shown instead of on/note — a flat, unconditional impossibility (verbatim T.X_*). */
  fact?: string;
}

export const CAPS = {
  key(): CapabilityRow[] {
    return [
      { label: "Claude Code", on: true, def: true, note: "Claude Code runs call Anthropic with this key." },
      { label: "Codex CLI", fact: T.X_KEY_CODEX },
      { label: "Direct API calls", on: true, note: "The proxy injects x-api-key into calls the sandbox makes itself." },
      { label: "Wardyn features", on: true, def: true, note: "Composer and review use it from the control plane." },
    ];
  },
  // Optional (defaults falsy, like the managed/non-sealed lane) so this and the
  // other four CAPS.* functions share one capabilityPreview call shape below.
  sub(hostCli?: boolean): CapabilityRow[] {
    return [
      { label: "Claude Code", on: true, makeDefault: true, note: "Claude Code signs in with the captured session." },
      { label: "Codex CLI", fact: T.X_SUB_CODEX },
      { label: "Direct API calls", fact: T.X_SUB_DIRECT },
      {
        label: "Wardyn features",
        on: !hostCli,
        // Not "until you switch it on" — nothing in this console switches it;
        // enabling this lane's composer backend is a server-side config change
        // (internal/api/integrations.go's reasonHostCLIOptIn: ships disabled by
        // default). Say that it's off, not that a control is waiting to be found.
        note: hostCli
          ? "Off for this lane — no switch in this console turns it on."
          : "Composer sends Claude-Code-shaped requests, so the token is accepted.",
      },
    ];
  },
  bedrock(): CapabilityRow[] {
    return [
      { label: "Claude Code", on: true, makeDefault: true, note: "Claude Code calls Bedrock through the AWS chain." },
      { label: "Codex CLI", fact: T.X_BEDROCK_CODEX },
      { label: "Direct API calls", on: true, note: "SigV4-signed calls from the sandbox go out through the active lane." },
      { label: "Wardyn features", on: true, note: T.BEDROCK_FEATURES },
    ];
  },
  openai(): CapabilityRow[] {
    return [
      { label: "Claude Code", fact: T.X_OPENAI_CLAUDE },
      { label: "Codex CLI", on: true, note: "Codex CLI runs call OpenAI with this key." },
      { label: "Direct API calls", on: true, note: "The proxy injects Authorization into calls the sandbox makes itself." },
      { label: "Wardyn features", on: true, note: "Composer and review use it from the control plane." },
    ];
  },
};

// ============================ STRUCTURED METADATA (this module's own design) ============================
// Everything below is NOT a mock transcription — it's metadata later
// list/add/detail pages will index into, built ON TOP of the verbatim T/CAPS
// canon above (referenced, never re-typed) so the two can't drift apart.

// ---- Categories (mockup's AddCategory radio cards) ----
// TWO, not the mock's four. An integration is a named connection to a system
// OUTSIDE Wardyn — an account. A corporate proxy and an internal mirror are
// network topology, and a redirect carries a proof obligation (every one has to
// test "reached" before the step hands off), so both live on the Corporate
// network step instead. Nothing here can produce a row for them any more, and
// the type is what enforces that.
export type IntegrationCategory = "ai_provider" | "scm_host";

// CATEGORY_META (icon/title/skipIfLine per category) lived here for the deleted
// /integrations page's section headers. The Settings cards carry their own
// titles, so nothing consumes it. T.CAT_AI / T.CAT_SCM go with it.

// ---- AI credential types (mockup's AddTypeAI radio cards) ----
export type AiType = "anthropic_api_key" | "anthropic_subscription" | "bedrock" | "openai_api_key";

export interface AiTypeMeta {
  title: string;
  /** T.TY_* verbatim where the mock defines one; otherwise the mock's own
   *  collapsed-card description (still verbatim, just not a T.* constant). */
  desc: string;
  /** The capability-table rows a later page renders as a preview. `sub`'s
   *  hostCli param only matters for anthropic_subscription; the others ignore
   *  any argument. */
  capabilityPreview: (hostCli?: boolean) => CapabilityRow[];
}

export const AI_TYPES: Record<AiType, AiTypeMeta> = {
  anthropic_api_key: { title: "Anthropic API key", desc: T.TY_KEY, capabilityPreview: CAPS.key },
  anthropic_subscription: {
    title: "Claude subscription",
    desc: "A claude.ai login instead of a key.",
    capabilityPreview: CAPS.sub,
  },
  bedrock: {
    title: "AWS Bedrock",
    desc: "Claude models through your AWS account.",
    capabilityPreview: CAPS.bedrock,
  },
  openai_api_key: { title: "OpenAI API key", desc: T.TY_OPENAI, capabilityPreview: CAPS.openai },
};

// ---- Residency (mockup's resChip kinds, generalized into a label+tone+tooltip
// dictionary any integration's credential can point at) ----
// control_plane is the mock's 4th resChip kind ("cp" — "control-plane side"),
// used only by Azure OpenAI (AddAzure's residencyBlock): distinct from `varies`
// (which means "more than one lane exists") — Azure has exactly one lane, and
// it simply never touches the sandbox at all.
export type ResidencyKind =
  | "proxy_injected"
  | "brokered_mint"
  | "resident_mount"
  | "resident_env"
  | "control_plane"
  | "notbuilt"
  | "varies";

export interface ResidencyMeta {
  label: string;
  tone: "success" | "warning" | "neutral";
  tooltip: string;
}

export const RESIDENCY_META: Record<ResidencyKind, ResidencyMeta> = {
  proxy_injected: {
    label: "proxy-injected",
    tone: "success",
    tooltip: "Held by Wardyn's store; the egress proxy injects it into outbound calls. The sandbox never holds it.",
  },
  brokered_mint: {
    label: "brokered",
    tone: "success",
    tooltip: "A short-lived, scoped credential is minted per run. Nothing long-lived is ever stored in the sandbox.",
  },
  resident_mount: {
    label: "resident",
    tone: "warning",
    tooltip: "Mounted into the sandbox as a file — the process running there can read it.",
  },
  resident_env: {
    label: "resident",
    tone: "warning",
    tooltip: "Set as an environment variable in the sandbox — the process running there can read it.",
  },
  control_plane: {
    label: "control-plane side",
    tone: "neutral",
    tooltip: "Called from Wardyn's control plane only — no sandbox lane exists for it.",
  },
  notbuilt: {
    label: "egress only",
    tone: "warning",
    tooltip:
      "Wardyn can open the path to the host — which is the difference between a run reaching it and not reaching it at all. Delivering this system's credential into the sandbox isn't built yet, and the row says so rather than showing an empty field.",
  },
  varies: {
    label: "varies by lane",
    tone: "neutral",
    tooltip: "This integration has more than one lane; residency depends on which one is active.",
  },
};

// Lane sub-choices for the two multi-lane AI types. Named to match the wire
// vocabulary already in use elsewhere (WorkspaceLLMCredMode uses "managed" for
// this exact subscription case — lib/types/workspaces.ts).
export type SubscriptionLane = "managed" | "resident_host";
export interface SubscriptionLaneMeta {
  title: string;
  residency: ResidencyKind;
  /** Verbatim T.MANAGED_LINE / T.HOSTCLI_LINE. */
  tooltip: string;
}
export const SUBSCRIPTION_LANE_META: Record<SubscriptionLane, SubscriptionLaneMeta> = {
  managed: { title: "Managed by Wardyn (container login)", residency: "proxy_injected", tooltip: T.MANAGED_LINE },
  resident_host: { title: "Host CLI login", residency: "resident_mount", tooltip: T.HOSTCLI_LINE },
};

export type BedrockLane = "bearer" | "sso" | "aws_dir" | "static";
export interface BedrockLaneMeta {
  title: string;
  residency: ResidencyKind;
  extra?: string;
}
export const BEDROCK_LANE_META: Record<BedrockLane, BedrockLaneMeta> = {
  bearer: { title: "Bearer token", residency: "proxy_injected" },
  sso: { title: "AWS SSO", residency: "resident_mount", extra: "containerized login" },
  aws_dir: { title: "Host ~/.aws profile", residency: "resident_mount", extra: "boot config" },
  // Raw access keys are exported as environment variables, not a mounted file.
  static: { title: "Access keys", residency: "resident_env" },
};

// ---- Impossible-as-fact map (never a toggle) ----
// Derived straight from CAPS' own `fact` rows above (type → capability →
// verbatim T.X_* reason) so the impossibility text can't drift from the
// capability table it's read out of.
export type AiCapability = "claude_code" | "codex_cli" | "direct_api" | "wardyn_features";

export const IMPOSSIBLE: Partial<Record<AiType, Partial<Record<AiCapability, string>>>> = {
  anthropic_api_key: { codex_cli: T.X_KEY_CODEX },
  anthropic_subscription: { codex_cli: T.X_SUB_CODEX, direct_api: T.X_SUB_DIRECT },
  bedrock: { codex_cli: T.X_BEDROCK_CODEX },
  openai_api_key: { claude_code: T.X_OPENAI_CLAUDE },
};
