/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's Policy tab (GET /runs/{id}/policy): every state the read can
// land in, the source line per kind, the changes grouped by cause, the edited-
// since banner, hidden values and the Copy YAML output. The strings are the
// owner-approved mock's; policy-tab-copy.test.ts pins them character for
// character, so this file asserts through the copy module for the sentences
// and literally for the parts the mock fixes.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunDetail, RunPolicyChange, RunPolicySpec, RunPolicyView } from "../../../lib/types";

const getPolicyMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: { getPolicy: (...a: unknown[]) => getPolicyMock(...a) },
}));

import { toYaml } from "../../wardyn/code-block";
import { OperatorProvider } from "../../wardyn/operator-context";
import { PolicyTab } from "./policy-tab";
import { CHANGE_HEADING, POLICY_TAB } from "./policy-tab-copy";

const RUN = {
  id: "run-1",
  created_at: "2026-09-29T14:00:00Z",
  updated_at: "2026-09-29T14:00:00Z",
  created_by: "dana@acme.example",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "t",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
} as RunDetail;

const SPEC: RunPolicySpec = {
  allowed_domains: ["api.anthropic.com", "registry.npmjs.org"],
  denied_domains: ["corp.example"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
  eligible_grants: [
    { kind: "github_token", scope: { repos: ["acme/payments-api"] }, requires_approval: false },
    { kind: "git_pat", scope: { host: "dev.azure.com" }, requires_approval: true },
  ],
  auto_stop_after_sec: 3600,
  workspace_mounts: [{ source: "/srv/shared", target: "/home/agent/work/shared", read_only: true }],
  workspace_repos: [{ repo: "acme/payments-api", target: "work/payments-api" }],
  resources: { cpu_millis: 2000, memory_mib: 4096, disk_mib: 10240 },
  tool_rules: [
    { tool: "Bash", effect: "hold" },
    { tool: "WebFetch", effect: "deny" },
  ],
  push_rules: { deny_paths: [".github/workflows/**"] },
};

function view(over: Partial<RunPolicyView> = {}): RunPolicyView {
  return {
    run_id: "run-1",
    state: "recorded",
    source: { kind: "stored", policy_id: "p1", name: "ci" },
    spec: SPEC,
    redacted: false,
    changes: [],
    complete: true,
    ...over,
  };
}

function show(v: RunPolicyView | Error, opts: { principal?: string; run?: RunDetail } = {}) {
  if (v instanceof Error) getPolicyMock.mockRejectedValue(v);
  else getPolicyMock.mockResolvedValue(v);
  const tree = (run: RunDetail) => (
    <OperatorProvider operator={false} principal={opts.principal ?? "dana@acme.example"}>
      <PolicyTab run={run} />
    </OperatorProvider>
  );
  const utils = render(tree(opts.run ?? RUN));
  return { ...utils, rerunAs: (run: RunDetail) => utils.rerender(tree(run)) };
}

const change = (c: Partial<RunPolicyChange> & Pick<RunPolicyChange, "cause" | "field">): RunPolicyChange => c;

async function ready() {
  await screen.findByTestId("run-policy-tab");
}

beforeEach(() => {
  cleanup();
  getPolicyMock.mockReset();
});

describe("PolicyTab — states", () => {
  it("shows the lead, the source line and the sentence for a recorded run", async () => {
    show(view());
    await ready();
    expect(screen.getByText(POLICY_TAB.lead)).toBeInTheDocument();
    expect(screen.getByText('Started from the saved policy "ci".')).toBeInTheDocument();
    expect(getPolicyMock).toHaveBeenCalledWith("run-1");
  });

  it("not_yet: the source line and S-27, and no policy", async () => {
    show(view({ state: "not_yet", spec: undefined, redacted: false }));
    await ready();
    expect(screen.getByText('Started from the saved policy "ci".')).toBeInTheDocument();
    expect(screen.getByText("Wardyn records this run's policy when its sandbox is set up. This run hasn't reached that step.")).toBeInTheDocument();
    expect(screen.queryByText(POLICY_TAB.lead)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: POLICY_TAB.copyYaml })).not.toBeInTheDocument();
  });

  it("never: S-28 under the source line, with no policy and no scope sentence", async () => {
    show(view({ state: "never", spec: undefined, source: { kind: "inline" } }));
    await ready();
    expect(screen.getByText("Started from a policy written for this run.")).toBeInTheDocument();
    expect(screen.getByText("This run stopped before its sandbox was set up, so no policy was applied to it.")).toBeInTheDocument();
    expect(screen.queryByText(POLICY_TAB.scope)).not.toBeInTheDocument();
  });

  it("never with no derivable source states no source at all", async () => {
    show(view({ state: "never", spec: undefined, source: { kind: "unknown" } }));
    await ready();
    expect(screen.queryByText(POLICY_TAB.sourceUnknown)).not.toBeInTheDocument();
    expect(screen.getByText(POLICY_TAB.never)).toBeInTheDocument();
  });

  it("error: S-29 with Retry, and Retry reads again", async () => {
    const { container } = show(new Error("boom"));
    expect(await screen.findByText("Couldn't load this run's policy.")).toBeInTheDocument();
    expect(container.textContent).not.toContain("boom");
    getPolicyMock.mockResolvedValue(view());
    await userEvent.click(screen.getByRole("button", { name: /retry/i }));
    await ready();
    expect(getPolicyMock).toHaveBeenCalledTimes(2);
  });

  it("old run: keeps the recorded policy and its changes, then says some may be missing (S-30)", async () => {
    show(view({ complete: false, changes: [change({ cause: "workspace", field: "allowed_domains", added: ["registry.npmjs.org"] })] }));
    await ready();
    expect(screen.getByText("Changed when the run started")).toBeInTheDocument();
    expect(screen.getByText(CHANGE_HEADING.workspace)).toBeInTheDocument();
    expect(screen.getByText("This run started before Wardyn recorded each change, so some changes may not be listed.")).toBeInTheDocument();
  });

  it("old run with no changes never claims the run got the policy exactly as written", async () => {
    show(view({ complete: false, changes: [] }));
    await ready();
    expect(screen.queryByText(POLICY_TAB.nothingChanged)).not.toBeInTheDocument();
    expect(screen.queryByText(POLICY_TAB.changesHeading)).not.toBeInTheDocument();
    expect(screen.getByText(POLICY_TAB.incomplete)).toBeInTheDocument();
  });

  it("empty changes on a complete record: S-13, and S-30 is absent", async () => {
    show(view({ changes: [] }));
    await ready();
    expect(screen.getByText("Nothing was changed. The run got the policy exactly as written.")).toBeInTheDocument();
    expect(screen.queryByText(POLICY_TAB.incomplete)).not.toBeInTheDocument();
  });

  it("reads again when the run's state changes while the tab stays open, keeping the last answer on screen", async () => {
    const { rerunAs } = show(view());
    await ready();
    rerunAs({ ...RUN, state: "COMPLETED" });
    await waitFor(() => expect(getPolicyMock).toHaveBeenCalledTimes(2));
    expect(screen.getByTestId("run-policy-tab")).toBeInTheDocument();
  });
});

describe("PolicyTab — the source line for each kind (S-3..S-10)", () => {
  const cases: [string, RunPolicyView["source"], string][] = [
    ["saved", { kind: "stored", name: "ci" }, 'Started from the saved policy "ci".'],
    ["saved, deleted", { kind: "stored", name: "ci", deleted: true }, 'Started from the saved policy "ci", which has since been deleted.'],
    ["saved, deleted, no name", { kind: "stored", deleted: true }, "Started from a saved policy that has since been deleted."],
    ["inline", { kind: "inline" }, "Started from a policy written for this run."],
    ["default", { kind: "default" }, "Started from your organization's default policy."],
    ["profile", { kind: "profile", name: "walled" }, "Started from the walled governance profile."],
    ["unknown", { kind: "unknown" }, "Wardyn set this policy for this run."],
  ];
  it.each(cases)("%s", async (_n, source, text) => {
    show(view({ source }));
    await ready();
    expect(screen.getByText(text)).toBeInTheDocument();
  });

  it("names the preset under the source line", async () => {
    show(view({ source: { kind: "stored", name: "ci", preset: "nightly-audit", preset_version: 3 } }));
    await ready();
    expect(screen.getByText('Launched from the preset "nightly-audit", version 3.')).toBeInTheDocument();
  });

  it("has no preset line when the run used none", async () => {
    show(view());
    await ready();
    expect(screen.queryByText(/Launched from the preset/)).not.toBeInTheDocument();
  });
});

describe("PolicyTab — the edited-since banner (saved policies only)", () => {
  it("changed: S-11a, with the saved policy's current name", async () => {
    show(view({ stored_policy_now: { state: "changed", name: "ci-renamed" } }));
    await ready();
    expect(screen.getByTestId("policy-since-banner")).toHaveTextContent(
      'The saved policy "ci-renamed" has changed since this run started. This page shows what the run got.',
    );
  });

  it("updated (an older run): the softer S-11b", async () => {
    show(view({ stored_policy_now: { state: "updated" } }));
    await ready();
    expect(screen.getByTestId("policy-since-banner")).toHaveTextContent(
      'The saved policy "ci" was updated after this run started, so it may read differently now. This page shows what the run got.',
    );
  });

  it.each(["same", "deleted"] as const)("%s: no banner", async (state) => {
    show(view({ stored_policy_now: { state } }));
    await ready();
    expect(screen.queryByTestId("policy-since-banner")).not.toBeInTheDocument();
  });

  it("a run from the default or a profile never gets one", async () => {
    show(view({ source: { kind: "default" } }));
    await ready();
    expect(screen.queryByTestId("policy-since-banner")).not.toBeInTheDocument();
  });
});

describe("PolicyTab — changes grouped by cause", () => {
  it("names each cause with its own heading and marks entries Added or Removed at start", async () => {
    show(
      view({
        changes: [
          change({ cause: "workspace", field: "allowed_domains", added: ["registry.npmjs.org"] }),
          change({ cause: "source_control", field: "allowed_domains", added: ["ghes.acme.example:443"] }),
          change({ cause: "model_access", field: "allowed_domains", added: ["llm-gateway.acme.example:443"] }),
          change({
            cause: "mirror",
            field: "allowed_domains",
            added: ["npm.mirror.acme.example:443"],
            removed: ["registry.npmjs.org"],
          }),
          change({ cause: "git_broker", field: "denied_domains", added: ["github.com", "api.github.com"] }),
          change({ cause: "launch", field: "allowed_domains", added: ["status.acme.example"] }),
        ],
      }),
    );
    await ready();
    for (const h of [
      "Added for the workspace",
      "Added so the run can reach its code",
      "Added so the agent can reach its model",
      "Switched to your organization's package mirror",
      "Routed through Wardyn's GitHub connection",
      "Set by Wardyn when the run started",
    ]) {
      expect(screen.getByRole("heading", { name: h })).toBeInTheDocument();
    }
    const mirror = screen.getByRole("heading", { name: "Switched to your organization's package mirror" }).parentElement!;
    expect(within(mirror).getByText("npm.mirror.acme.example:443").parentElement).toHaveTextContent("Added at start");
    expect(within(mirror).getByText("registry.npmjs.org").parentElement).toHaveTextContent("Removed at start");
  });

  it("an unrecognised cause falls under the generic heading", async () => {
    show(view({ changes: [change({ cause: "something_new", field: "allowed_domains", added: ["x.example"] })] }));
    await ready();
    expect(screen.getByRole("heading", { name: CHANGE_HEADING.launch })).toBeInTheDocument();
  });

  it("names the profile (S-20) and keeps two profiles' walls apart", async () => {
    show(
      view({
        changes: [
          change({ cause: "profile", field: "denied_domains", added: ["corp.example"], profile: "walled" }),
          change({ cause: "profile", field: "denied_domains", added: ["paste.example"], profile: "strict" }),
        ],
      }),
    );
    await ready();
    expect(screen.getByRole("heading", { name: "Limited by the walled governance profile" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Limited by the strict governance profile" })).toBeInTheDocument();
  });

  it("restart (S-21): dated, and its entries carry no chip", async () => {
    show(view({ changes: [change({ cause: "restart", field: "denied_domains", added: ["paste.example"], at: "2026-09-29T12:00:00Z" })] }));
    await ready();
    const h = screen.getByRole("heading", { name: /^Blocked when the run was restarted on / });
    expect(h.textContent).toMatch(/Sep 29, 2026/);
    expect(within(h.parentElement!).getByText("paste.example").parentElement).not.toHaveTextContent("Added at start");
  });

  it("disk size (S-22): a bare number reads as MiB, with no chip", async () => {
    show(view({ changes: [change({ cause: "org_disk", field: "resources.disk_mib", added: ["20480"] })] }));
    await ready();
    const g = screen.getByRole("heading", { name: "Disk size set from your organization's default" }).parentElement!;
    expect(within(g).getByText("20480 MiB").parentElement).not.toHaveTextContent("Added at start");
  });

  describe("limits: whose limits", () => {
    const limits = view({
      changes: [
        change({
          cause: "limits",
          field: "allowed_domains",
          removed: ["pastebin.com"],
          detail: ["dropped 1 egress domain(s) not in operator allowlist: pastebin.com", 'confinement raised from "CC1" to operator minimum "CC2"'],
        }),
      ],
    });

    it("the run's owner reads S-14a", async () => {
      show(limits, { principal: "dana@acme.example" });
      await ready();
      expect(screen.getByRole("heading", { name: "Narrowed to fit your limits" })).toBeInTheDocument();
    });

    it("any other reader reads S-14b, naming the person, and the server's lines verbatim", async () => {
      show(limits, { principal: "admin@acme.example" });
      await ready();
      expect(screen.getByRole("heading", { name: "Narrowed to fit the limits set for dana@acme.example" })).toBeInTheDocument();
      expect(screen.getByText("dropped 1 egress domain(s) not in operator allowlist: pastebin.com")).toBeInTheDocument();
      expect(screen.getByText('confinement raised from "CC1" to operator minimum "CC2"')).toBeInTheDocument();
    });
  });

  // The server keys a repo with a ref as `repo@ref`.
  it("keeps the Added chip on a repo that carries a ref", async () => {
    show(
      view({
        spec: { ...SPEC, workspace_repos: [{ repo: "acme/api", ref: "main", target: "work/api" }] },
        changes: [change({ cause: "workspace", field: "workspace_repos", added: ["acme/api@main"] })],
      }),
    );
    await ready();
    expect(rowValue("Files and code", "Repositories")).toHaveTextContent("acme/api at main → work/api" + "Added at start");
  });

  it("reads single-value changes in the console's own words, never as wire codes", async () => {
    show(
      view({
        changes: [
          change({ cause: "limits", field: "min_confinement_class", added: ["CC2"], removed: ["CC1"] }),
          change({ cause: "limits", field: "first_use_approval", added: ["deny_with_review"], removed: ["wait_for_review"] }),
          change({ cause: "limits", field: "resources.memory_mib", added: ["4096"] }),
          change({ cause: "limits", field: "resources.disk_mib", added: ["9000"] }),
          change({ cause: "limits", field: "resources.cpu_millis", added: ["1500"] }),
        ],
      }),
    );
    await ready();
    const g = screen.getByRole("heading", { name: CHANGE_HEADING.limitsOwn }).parentElement!;
    for (const t of ["Wall", "Fence", "Refused, then sent for approval", "4096 MiB", "9000 MiB", "1.5 CPU"]) {
      expect(within(g).getByText(t)).toBeInTheDocument();
    }
    for (const raw of ["CC2", "CC1", "deny_with_review", "wait_for_review"]) {
      expect(within(g).queryByText(raw)).not.toBeInTheDocument();
    }
  });

  it("a change to wait_for_review names the policy's own hold time, not the default", async () => {
    show(
      view({
        spec: { ...SPEC, first_use_approval: "wait_for_review", first_use_hold_seconds: 45 },
        changes: [change({ cause: "limits", field: "first_use_approval", added: ["wait_for_review"] })],
      }),
    );
    await ready();
    const g = screen.getByRole("heading", { name: CHANGE_HEADING.limitsOwn }).parentElement!;
    expect(within(g).getByText("Held for up to 45 seconds while someone decides")).toBeInTheDocument();
    expect(within(g).queryByText(/30 seconds/)).not.toBeInTheDocument();
  });

  it("flags a changed entry in the Summary too", async () => {
    show(view({ changes: [change({ cause: "workspace", field: "allowed_domains", added: ["registry.npmjs.org"] })] }));
    await ready();
    const row = rowValue("Network", "Allowed hosts");
    expect(within(row).getByText("registry.npmjs.org").parentElement).toHaveTextContent("Added at start");
    expect(within(row).getByText("api.anthropic.com").parentElement).not.toHaveTextContent("Added at start");
  });
});

function sectionFor(title: string): HTMLElement {
  return screen.getByRole("heading", { name: title, level: 4 }).parentElement!;
}

function rowValue(section: string, label: string): HTMLElement {
  // A section can share its title with its only row (Credentials, Traffic checks).
  const dt = within(sectionFor(section)).getAllByText(label).find((el) => el.tagName === "DT");
  return dt!.nextElementSibling as HTMLElement;
}

describe("PolicyTab — the Summary", () => {
  it("renders every section from the policy, reusing the console's own wording", async () => {
    show(view());
    await ready();
    expect(rowValue("Network", "Allowed hosts")).toHaveTextContent("api.anthropic.comregistry.npmjs.org");
    expect(rowValue("Network", "Blocked hosts")).toHaveTextContent("corp.example");
    expect(rowValue("Network", "Any other host")).toHaveTextContent("Refused, then sent for approval");
    expect(rowValue("Network", "Request types")).toHaveTextContent("All");
    expect(rowValue("Barrier", "Minimum")).toHaveTextContent("Wall");
    expect(rowValue("Barrier", "This run used")).toHaveTextContent("Wall");
    expect(rowValue("Credentials", "GitHub access")).toHaveTextContent("acme/payments-api");
    const pat = rowValue("Credentials", "Git access token");
    expect(pat).toHaveTextContent("dev.azure.com");
    expect(pat).toHaveTextContent("Needs approval");
    expect(rowValue("Files and code", "Folders from the host")).toHaveTextContent("/srv/shared→/home/agent/work/sharedRead-only");
    expect(rowValue("Files and code", "Repositories")).toHaveTextContent("acme/payments-api → work/payments-api");
    expect(rowValue("Tools and pushes", "Tool rules")).toHaveTextContent(
      "2 rules · Bash held, WebFetch denied. Anything else is held.",
    );
    expect(rowValue("Tools and pushes", "Pushes")).toHaveTextContent("Only this run's own branch");
    expect(rowValue("Tools and pushes", "Deny")).toHaveTextContent(".github/workflows/**");
    expect(rowValue("Tools and pushes", "Hold for review")).toHaveTextContent("None");
    expect(rowValue("Apps", "UI apps")).toHaveTextContent("None declared");
    expect(rowValue("Limits", "CPU")).toHaveTextContent("2 CPU");
    expect(rowValue("Limits", "Memory")).toHaveTextContent("4096 MiB");
    expect(rowValue("Limits", "Processes")).toHaveTextContent("Standard limit");
    expect(rowValue("Limits", "Disk")).toHaveTextContent("10240 MiB");
    expect(rowValue("Limits", "When idle")).toHaveTextContent("Auto-stop: 60 min idle");
    expect(rowValue("Traffic checks", "Traffic checks")).toHaveTextContent("Off");
    expect(screen.queryByRole("heading", { name: "Azure DevOps access" })).not.toBeInTheDocument();
  });

  it("describes allow-all in block-list terms, and an empty policy as None / Standard limit", async () => {
    show(
      view({
        spec: {
          allowed_domains: null,
          allow_all_egress: true,
          first_use_approval: "always_deny",
          min_confinement_class: "CC1",
          allowed_methods: ["GET", "POST"],
        },
      }),
    );
    await ready();
    expect(rowValue("Network", "Allowed hosts")).toHaveTextContent("Can reach almost any site (except a block-list).");
    expect(rowValue("Network", "Blocked hosts")).toHaveTextContent("None");
    expect(rowValue("Network", "Any other host")).toHaveTextContent("Refused");
    expect(rowValue("Network", "Request types")).toHaveTextContent("GET, POST");
    expect(rowValue("Barrier", "Minimum")).toHaveTextContent("Fence");
    expect(rowValue("Credentials", "Credentials")).toHaveTextContent("None");
    expect(rowValue("Files and code", "Folders from the host")).toHaveTextContent("None");
    expect(rowValue("Files and code", "Repositories")).toHaveTextContent("None");
    expect(rowValue("Tools and pushes", "Tool rules")).toHaveTextContent("None");
    expect(rowValue("Limits", "CPU")).toHaveTextContent("Standard limit");
    expect(rowValue("Limits", "Disk")).toHaveTextContent("Standard limit");
    expect(rowValue("Limits", "When idle")).toHaveTextContent("Runs until stopped");
  });

  it("says how long a held connection waits, and shows apps, refs, any-branch pushes and traffic checks On", async () => {
    show(
      view({
        spec: {
          ...SPEC,
          first_use_approval: "wait_for_review",
          first_use_hold_seconds: 45,
          git_push_any_branch: true,
          ui_apps: [{ name: "vscode", port: 8080, path: "/" }],
          workspace_repos: [{ repo: "acme/api", ref: "main", target: "work/api" }],
          llm_inspection: { mode: "alert" },
        },
      }),
    );
    await ready();
    expect(rowValue("Network", "Any other host")).toHaveTextContent("Held for up to 45 seconds while someone decides");
    expect(rowValue("Tools and pushes", "Pushes")).toHaveTextContent("Any branch");
    expect(rowValue("Apps", "UI apps")).toHaveTextContent("vscode → localhost:8080/");
    expect(rowValue("Files and code", "Repositories")).toHaveTextContent("acme/api at main → work/api");
    expect(rowValue("Traffic checks", "Traffic checks")).toHaveTextContent("On");
  });

  it("shows Azure DevOps access only when the policy set capabilities", async () => {
    show(view({ spec: { ...SPEC, azure_devops_capabilities: ["work_items.read"] } }));
    await ready();
    expect(screen.getByRole("heading", { name: "Azure DevOps access" })).toBeInTheDocument();
  });
});

describe("PolicyTab — hidden values", () => {
  const redactedSpec: RunPolicySpec = {
    ...SPEC,
    workspace_mounts: [{ source: "<redacted>", target: "/home/agent/work/shared", read_only: true }],
  };

  it("a member sees Hidden, with the reason on hover, in place of the folder source", async () => {
    show(view({ spec: redactedSpec, redacted: true }));
    await ready();
    const hidden = within(sectionFor("Files and code")).getByText("Hidden");
    expect(hidden).toHaveAttribute("title", "Only admins can see this.");
    expect(sectionFor("Files and code")).not.toHaveTextContent("<redacted>");
    expect(sectionFor("Files and code")).toHaveTextContent("/home/agent/work/shared");
  });

  it("a blanked source reads Hidden too", async () => {
    show(view({ spec: { ...SPEC, workspace_mounts: [{ source: "", target: "/t" }] }, redacted: true }));
    await ready();
    expect(within(sectionFor("Files and code")).getByText("Hidden")).toBeInTheDocument();
  });

  it("an admin sees the folder, and no Hidden marker", async () => {
    show(view());
    await ready();
    expect(within(sectionFor("Files and code")).queryByText("Hidden")).not.toBeInTheDocument();
    expect(within(sectionFor("Files and code")).getByText("/srv/shared")).toBeInTheDocument();
  });

  it("S-33 sits above the YAML only when values are hidden", async () => {
    const user = userEvent.setup();
    show(view({ spec: redactedSpec, redacted: true }));
    await ready();
    expect(screen.queryByText(POLICY_TAB.redacted)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "YAML" }));
    expect(screen.getByText("Values shown as <redacted> are hidden from you. Fill them in before using this as a policy.")).toBeInTheDocument();
    expect(screen.getByText(/<redacted>/, { selector: "code *" })).toBeInTheDocument();
  });

  it("no S-33 for a reader who sees everything", async () => {
    const user = userEvent.setup();
    show(view());
    await ready();
    await user.click(screen.getByRole("button", { name: "YAML" }));
    expect(screen.queryByText(POLICY_TAB.redacted)).not.toBeInTheDocument();
  });
});

describe("PolicyTab — Summary / YAML switch and Copy YAML", () => {
  it("opens on Summary, and the switch swaps it for the YAML block", async () => {
    const user = userEvent.setup();
    show(view());
    await ready();
    expect(screen.getByRole("button", { name: "Summary" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("heading", { name: "Network", level: 4 })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "YAML" }));
    expect(screen.getByRole("button", { name: "YAML" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByRole("heading", { name: "Network", level: 4 })).not.toBeInTheDocument();
    // The block renders one div per line; joined back they are the document.
    const lines = Array.from(document.querySelectorAll("pre code > div"), (d) => d.textContent);
    expect(lines.join("\n")).toBe(toYaml(SPEC));
    await user.click(screen.getByRole("button", { name: "Summary" }));
    expect(screen.getByRole("heading", { name: "Network", level: 4 })).toBeInTheDocument();
  });

  it("Copy YAML writes exactly toYaml(spec), from either view, and confirms it", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    // userEvent.setup() (earlier tests) leaves a getter-only clipboard stub behind.
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    show(view());
    await ready();
    fireEvent.click(screen.getByRole("button", { name: "Copy YAML" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText).toHaveBeenCalledWith(toYaml(SPEC));
    expect(await screen.findByText("Copied")).toHaveAttribute("aria-live", "polite");
  });

  it("the scope sentence (S-32) foots both views", async () => {
    const user = userEvent.setup();
    show(view());
    await ready();
    expect(screen.getByText(POLICY_TAB.scope)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "YAML" }));
    expect(screen.getByText(POLICY_TAB.scope)).toBeInTheDocument();
  });
});
