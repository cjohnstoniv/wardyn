/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import type { RunPolicySpec } from "../../../lib/types";
import { SUMMARY } from "../../screens/run-detail/policy-tab-copy";
import { WALL_PRESETS } from "../copy/wall-presets";
import { PolicySummary, specIsRedacted, type PolicyChangeMarks } from "./policy-summary";

// Every key a RunPolicySpec has, each carrying something the Summary shows.
const FULL = {
  min_confinement_class: "CC2",
  allowed_domains: ["api.example.com", "registry.example.org"],
  denied_domains: ["blocked.example.net"],
  first_use_approval: "deny_with_review",
  first_use_hold_seconds: 45,
  max_holds: 3,
  allowed_methods: ["GET", "POST"],
  llm_inspection: { mode: "block" },
  eligible_grants: [
    { kind: "github_token", scope: { repos: ["acme/api"] }, requires_approval: false },
    { kind: "github_token", scope: { repos: ["acme/web"] }, requires_approval: true },
    { kind: "git_pat", scope: { host: "git.example.com", access: "read", repos: ["team/app"] }, requires_approval: false },
    { kind: "env_secret", scope: { secret_name: "registry-token" }, requires_approval: false },
  ],
  azure_devops_capabilities: ["code_read"],
  workspace_mounts: [
    { source: "/srv/shared", target: "/mnt/shared", read_only: true },
    { source: "/srv/out", target: "/mnt/out", read_only: false },
  ],
  workspace_repos: [{ repo: "acme/api", ref: "main", target: "work/api" }],
  tool_rules: [
    { tool: "Bash", effect: "hold" },
    { tool: "Read", effect: "allow" },
    { tool: "*", effect: "deny" },
  ],
  push_rules: { deny_paths: [".github/**"], require_review_paths: ["payments/**"] },
  git_push_any_branch: false,
  ui_apps: [{ name: "web", port: 3000, path: "/app" }],
  resources: { cpu_millis: 2000, memory_mib: 4096, pids_limit: 512, disk_mib: 10240 },
  auto_stop_after_sec: 3600,
} as unknown as RunPolicySpec;

function section(heading: string): HTMLElement {
  return screen.getByRole("heading", { name: heading, level: 3 }).parentElement!;
}

// The value cell beside a label, found by the label's <dt>.
function row(heading: string, label: string): HTMLElement {
  const dt = within(section(heading)).getAllByText(label).find((el) => el.tagName === "DT");
  if (!dt) throw new Error(`no row "${label}" under "${heading}"`);
  return dt.nextElementSibling as HTMLElement;
}

describe("PolicySummary — plain names for every key", () => {
  it("shows every known key under its display name, in headed sections", () => {
    render(<PolicySummary spec={FULL} />);
    expect(screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent)).toEqual([
      "Barrier",
      "Network",
      "Credentials",
      "Azure DevOps access",
      "Files and code",
      "Tools and pushes",
      "Apps",
      "Limits",
    ]);
    expect(row("Barrier", "Minimum")).toHaveTextContent("Wall");
    expect(row("Network", "Allowed hosts")).toHaveTextContent("api.example.comregistry.example.org");
    expect(row("Network", "Blocked hosts")).toHaveTextContent("blocked.example.net");
    expect(row("Network", "Any other host")).toHaveTextContent(
      "Refused, then sent for approval" + "Held for up to 45 seconds while someone decides",
    );
    expect(row("Network", "Holds at once")).toHaveTextContent("3");
    expect(row("Network", "Request types")).toHaveTextContent("GET, POST");
    expect(row("Network", "Traffic checks")).toHaveTextContent("On");
    expect(row("Credentials", "Environment secret")).toHaveTextContent("registry-token");
    expect(row("Files and code", "Folders from the host")).toHaveTextContent(
      "/mnt/shared ← /srv/shared · Read-only" + "/mnt/out ← /srv/out",
    );
    expect(row("Files and code", "Repositories")).toHaveTextContent("acme/api at main → work/api");
    expect(row("Tools and pushes", "Tool rules")).toHaveTextContent("Bash — held" + "Read — allowed" + "Anything else is denied.");
    expect(row("Tools and pushes", "Deny")).toHaveTextContent(".github/**");
    expect(row("Tools and pushes", "Hold for review")).toHaveTextContent("payments/**");
    // The fixture's one capability list holds no code_write: that forge only is named, the other keeps its branch line.
    expect(row("Tools and pushes", "Pushes")).toHaveTextContent(/no pushes \(no write access\)/);
    expect(row("Apps", "UI apps")).toHaveTextContent("web → localhost:3000/app");
    expect(row("Limits", "CPU")).toHaveTextContent("2 CPU");
    expect(row("Limits", "Memory")).toHaveTextContent("4096 MiB");
    expect(row("Limits", "Processes")).toHaveTextContent("512");
    expect(row("Limits", "Disk")).toHaveTextContent("10240 MiB");
    expect(row("Limits", "When idle")).toHaveTextContent("Stops after 60 minutes");
    // The real one-line summary, under its one title.
    expect(within(section("Azure DevOps access")).getByTestId("ado-access-summary")).toBeInTheDocument();
  });

  it("never prints a raw key for a key it knows", () => {
    const { container } = render(<PolicySummary spec={FULL} />);
    for (const key of Object.keys(FULL)) expect(container).not.toHaveTextContent(key);
    expect(screen.queryByRole("heading", { name: "Other settings" })).toBeNull();
  });

  it("an unknown key shows under its raw name in Other settings, never hidden", () => {
    render(<PolicySummary spec={{ allowed_domains: [], future_knob: { level: 2 } } as unknown as RunPolicySpec} />);
    expect(row("Other settings", "future_knob")).toHaveTextContent('{"level":2}');
  });

  it("a known key holding the wrong shape is shown raw, not read as something else", () => {
    render(<PolicySummary spec={{ allowed_domains: "api.example.com", tool_rules: "all" } as unknown as RunPolicySpec} />);
    expect(row("Other settings", "allowed_domains")).toHaveTextContent('"api.example.com"');
    expect(row("Other settings", "tool_rules")).toHaveTextContent('"all"');
    expect(screen.queryByRole("heading", { name: "Network" })).toBeNull();
  });

  it("uses the body text size for labels and values", () => {
    render(<PolicySummary spec={FULL} />);
    const value = row("Barrier", "Minimum");
    expect(value.previousElementSibling).toHaveClass("text-sm");
    expect(within(value).getByText("Wall").closest("li")).toHaveClass("text-sm");
  });
});

describe("PolicySummary — each repeated key is one title with a list", () => {
  it("lists hosts, grants of one kind, folders and tool rules under a single title each", () => {
    render(<PolicySummary spec={FULL} />);
    for (const [heading, label, n] of [
      ["Network", "Allowed hosts", 2],
      ["Credentials", "GitHub access", 2],
      ["Files and code", "Folders from the host", 2],
      ["Tools and pushes", "Tool rules", 3],
    ] as const) {
      expect(within(section(heading)).getAllByText(label)).toHaveLength(1);
      expect(within(row(heading, label)).getAllByRole("listitem")).toHaveLength(n);
    }
  });

  it("keeps what narrows a git access token and what needs approval", () => {
    render(<PolicySummary spec={FULL} />);
    const pat = row("Credentials", "Git access token");
    expect(pat).toHaveTextContent("git.example.com");
    expect(pat).toHaveTextContent("Read-only");
    expect(pat).toHaveTextContent("team/app");
    expect(row("Credentials", "GitHub access")).toHaveTextContent("acme/web" + "Needs approval");
  });

  it("an empty allow-list reads None, and allow-all reads in block-list terms", () => {
    const { unmount } = render(<PolicySummary spec={{ allowed_domains: [] } as unknown as RunPolicySpec} />);
    expect(row("Network", "Allowed hosts")).toHaveTextContent("None");
    unmount();
    render(<PolicySummary spec={{ allowed_domains: ["a.example"], allow_all_egress: true } as unknown as RunPolicySpec} />);
    expect(row("Network", "Allowed hosts")).toHaveTextContent("Allow-all egress (block-list only)");
    expect(row("Network", "Allowed hosts")).not.toHaveTextContent("a.example");
  });

  it("the * default shows only where the policy spells it out", () => {
    render(<PolicySummary spec={{ tool_rules: [{ tool: "Bash", effect: "deny" }] } as unknown as RunPolicySpec} />);
    expect(within(row("Tools and pushes", "Tool rules")).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "Bash — denied",
    ]);
  });

  it("reads the legacy boolean first_use_approval the way the wire does", () => {
    const { unmount } = render(<PolicySummary spec={{ first_use_approval: true } as unknown as RunPolicySpec} />);
    expect(row("Network", "Any other host")).toHaveTextContent("Refused, then sent for approval");
    unmount();
    render(<PolicySummary spec={{ first_use_approval: false } as unknown as RunPolicySpec} />);
    expect(row("Network", "Any other host")).toHaveTextContent("Refused");
    expect(screen.queryByRole("heading", { name: "Other settings" })).toBeNull();
  });

  // The hold has a display name of its own; without a mode it must not fall to its raw key.
  it("shows a hold with no first-use mode under Any other host, not under its raw key", () => {
    const { container } = render(<PolicySummary spec={{ first_use_hold_seconds: 45 } as unknown as RunPolicySpec} />);
    expect(within(row("Network", "Any other host")).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "Held for up to 45 seconds while someone decides",
    ]);
    expect(container).not.toHaveTextContent("first_use_hold_seconds");
    expect(screen.queryByRole("heading", { name: "Other settings" })).toBeNull();
  });

  it("a held connection states its own wait once", () => {
    render(
      <PolicySummary
        spec={{ first_use_approval: "wait_for_review", first_use_hold_seconds: 20 } as unknown as RunPolicySpec}
      />,
    );
    expect(within(row("Network", "Any other host")).getAllByRole("listitem")).toHaveLength(1);
    expect(row("Network", "Any other host")).toHaveTextContent("Held for up to 20 seconds while someone decides");
  });

  it("an empty policy says None rather than drawing empty sections", () => {
    render(<PolicySummary spec={{} as RunPolicySpec} />);
    expect(screen.getByText("None")).toBeInTheDocument();
    expect(screen.queryByRole("heading")).toBeNull();
  });
});

describe("PolicySummary — facts, launch checks, marks and hidden values", () => {
  it("a run's used class and a draft's requested class are separate rows", () => {
    const { unmount } = render(<PolicySummary spec={FULL} facts={{ usedClass: "CC3" }} />);
    expect(row("Barrier", "This run used")).toHaveTextContent("Vault");
    expect(within(section("Barrier")).queryByText("This run requests")).toBeNull();
    unmount();
    render(<PolicySummary spec={FULL} facts={{ requestedClass: "CC1" }} />);
    expect(row("Barrier", "This run requests")).toHaveTextContent("Fence");
    expect(within(section("Barrier")).queryByText("This run used")).toBeNull();
  });

  it("launch-only checks are one list under Checked at launch", () => {
    render(
      <PolicySummary
        spec={FULL}
        pending={[
          "task",
          "model_provider_selection",
          "credential_liveness",
          "autonomy",
          "tool_approvals",
          "runner_confinement",
          "drive_readiness",
          "dispatch_egress",
        ]}
      />,
    );
    const checks = section("Checked at launch");
    expect(within(checks).getAllByText("Not checked in this preview.")).toHaveLength(1);
    expect(within(checks).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "Task",
      "Model provider",
      "Credentials still valid",
      "Autonomy",
      "Tool approvals",
      "Barrier on the runner",
      "Drive ready",
      "Hosts added at dispatch",
    ]);
  });

  it("flags what launch added, and lists what it removed from the host lists", () => {
    const marks: PolicyChangeMarks = {
      of: (field, entry) => (field === "allowed_domains" && entry === "registry.example.org" ? "added" : undefined),
      removed: (field) => (field === "allowed_domains" ? ["packages.example.net"] : []),
    };
    render(<PolicySummary spec={FULL} marks={marks} />);
    const hosts = row("Network", "Allowed hosts");
    expect(within(hosts).getByText("registry.example.org").closest("li")).toHaveTextContent("Added at start");
    expect(within(hosts).getByText("api.example.com").closest("li")).not.toHaveTextContent("Added at start");
    expect(within(hosts).getByText("packages.example.net").closest("li")).toHaveTextContent("Removed at start");
  });

  it("a hidden folder source and a hidden secret read Hidden, with the reason reachable by keyboard", () => {
    const spec = {
      workspace_mounts: [{ source: "<redacted>", target: "/mnt/shared", read_only: true }],
      eligible_grants: [{ kind: "env_secret", scope: { secret_name: "<redacted>" }, requires_approval: false }],
    } as unknown as RunPolicySpec;
    const { container } = render(<PolicySummary spec={spec} />);
    const folder = row("Files and code", "Folders from the host");
    expect(folder).toHaveTextContent("/mnt/shared · Read-only" + "Hidden");
    // A host path is the super admin's alone; a secret name is hidden only below the security tier.
    const mount = within(folder).getByRole("note", { name: "Hidden. Only a super admin can see this." });
    expect(mount).toHaveAttribute("title", "Only a super admin can see this.");
    expect(mount).toHaveAttribute("tabindex", "0");
    const notes = screen.getAllByRole("note", { name: "Hidden. Only admins can see this." });
    expect(notes).toHaveLength(1);
    expect(notes[0]).toHaveAttribute("title", "Only admins can see this.");
    expect(notes[0]).toHaveAttribute("tabindex", "0");
    expect(container).not.toHaveTextContent("<redacted>");
    expect(specIsRedacted(spec)).toBe(true);
    expect(specIsRedacted(FULL)).toBe(false);
  });

  it("takes its heading level from where the document sits", () => {
    render(<PolicySummary spec={FULL} headingLevel={4} />);
    expect(screen.getByRole("heading", { name: "Network", level: 4 })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { level: 3 })).toBeNull();
  });
});

describe("PolicySummary — push limits (#2601)", () => {
  it("shows an enforced file-size limit, hold wait and inspected-pack size with no path rules", () => {
    const spec = { push_rules: { max_file_size_mib: 5, hold_seconds: 300, max_inspect_pack_mib: 32 } } as unknown as RunPolicySpec;
    render(<PolicySummary spec={spec} />);
    expect(row("Tools and pushes", "Largest file")).toHaveTextContent("5 MiB");
    expect(row("Tools and pushes", "Hold waits")).toHaveTextContent("300 seconds");
    expect(row("Tools and pushes", "Largest push inspected")).toHaveTextContent("32 MiB");
  });
});

describe("PolicySummary — the IP-address switch", () => {
  const ON = "When allow-all egress is on, refuse connections to IP addresses unless they are listed exactly in Allowed hosts.";
  const OFF = "This setting adds no IP-address restriction.";
  const withIp = (v: unknown) => ({ ...FULL, refuse_ip_literals: v }) as unknown as RunPolicySpec;

  it("reads a present true and a present false as a Network row, not as an Other setting", () => {
    const { unmount } = render(<PolicySummary spec={withIp(true)} />);
    expect(row("Network", "IP addresses")).toHaveTextContent(ON);
    expect(screen.queryByText("refuse_ip_literals")).toBeNull();
    unmount();
    render(<PolicySummary spec={withIp(false)} />);
    expect(row("Network", "IP addresses")).toHaveTextContent(OFF);
    expect(screen.queryByText("refuse_ip_literals")).toBeNull();
  });

  it("shows no row when the document omits it, and never invents an off value", () => {
    render(<PolicySummary spec={FULL} />);
    expect(screen.queryByText("IP addresses")).toBeNull();
    expect(screen.queryByText(OFF)).toBeNull();
    expect(screen.queryByText("refuse_ip_literals")).toBeNull();
  });

  it("flags an addition, and lists a removed value as a struck entry that makes no claim about the policy now", () => {
    const marks: PolicyChangeMarks = {
      of: (field, entry) => (field === "refuse_ip_literals" && entry === "false" ? "added" : undefined),
      removed: (field) => (field === "refuse_ip_literals" ? ["true"] : []),
    };
    render(<PolicySummary spec={withIp(false)} marks={marks} />);
    const r = row("Network", "IP addresses");
    expect(within(r).getByText(OFF).closest("li")).toHaveTextContent("Added at start");
    expect(within(r).getByText(ON).closest("li")).toHaveTextContent("Removed at start");
  });
});

// The host marks (#2956, mock R13). The five strings are the approved copy, character for
// character; cmd/wardyn/run_policy.go prints the same five, so that file is read here too.
describe("PolicySummary host marks", () => {
  const INSPECTED = "Inspected";
  const TUNNEL = "Tunnel only";
  const INSPECTED_MEANING =
    "Wardyn opens this traffic to add a credential or check it, so the sandbox must trust Wardyn's certificate.";
  const TUNNEL_MEANING =
    "Wardyn passes this traffic through without reading it. It sees only the host name. Apps that pin their certificate work here.";
  const ALLOW_ALL_LINE = "Hosts not listed as Inspected pass through unread.";
  const ALLOW_ALL_TEXT = "Allow-all egress (block-list only)";
  const spec = { allowed_domains: ["artifacts.corp.example", "pypi.org"] } as RunPolicySpec;
  const row = (host: string) => screen.getByText(host).closest("li") as HTMLElement;

  it("pins the five strings verbatim, in the console copy and in the CLI", () => {
    expect(SUMMARY.inspected).toBe(INSPECTED);
    expect(SUMMARY.tunnelOnly).toBe(TUNNEL);
    expect(SUMMARY.inspectedMeaning).toBe(INSPECTED_MEANING);
    expect(SUMMARY.tunnelOnlyMeaning).toBe(TUNNEL_MEANING);
    expect(SUMMARY.tunnelOnlyAllowAll).toBe(ALLOW_ALL_LINE);
    const cli = readFileSync(join(__dirname, "../../../../../../cmd/wardyn/run_policy.go"), "utf8");
    for (const s of [INSPECTED, TUNNEL, INSPECTED_MEANING, TUNNEL_MEANING, ALLOW_ALL_LINE]) expect(cli).toContain(`"${s}"`);
  });

  it("puts each chip on its own host, and one meaning line per mark present", () => {
    render(<PolicySummary spec={spec} hostMarks={{ inspected: ["artifacts.corp.example"], terminated: ["artifacts.corp.example"], outside: [] }} />);
    expect(within(row("artifacts.corp.example")).getByText(INSPECTED)).toBeInTheDocument();
    expect(within(row("artifacts.corp.example")).queryByText(TUNNEL)).toBeNull();
    expect(within(row("pypi.org")).getByText(TUNNEL)).toBeInTheDocument();
    expect(within(row("pypi.org")).queryByText(INSPECTED)).toBeNull();
    expect(screen.getByText(INSPECTED_MEANING)).toBeInTheDocument();
    expect(screen.getByText(TUNNEL_MEANING)).toBeInTheDocument();
  });

  it("explains only the marks present", () => {
    render(<PolicySummary spec={spec} hostMarks={{ inspected: [], terminated: [], outside: [] }} />);
    expect(screen.getAllByText(TUNNEL)).toHaveLength(3); // two hosts and the meaning line's chip
    expect(screen.queryByText(INSPECTED)).toBeNull();
    expect(screen.queryByText(INSPECTED_MEANING)).toBeNull();
  });

  it("shows no marks without host_marks", () => {
    render(<PolicySummary spec={spec} />);
    for (const s of [INSPECTED, TUNNEL, INSPECTED_MEANING, TUNNEL_MEANING, ALLOW_ALL_LINE]) expect(screen.queryByText(s)).toBeNull();
  });

  it("lists a host the proxy opens outside the allowlist after the allowlist rows, as Inspected", () => {
    render(
      <PolicySummary
        spec={spec}
        hostMarks={{ inspected: ["artifacts.corp.example"], terminated: ["api.github.com", "artifacts.corp.example"], outside: ["api.github.com"] }}
      />,
    );
    const items = within(screen.getByText("artifacts.corp.example").closest("ul") as HTMLElement).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual([
      `artifacts.corp.example${INSPECTED}`,
      `pypi.org${TUNNEL}`,
      `api.github.com${INSPECTED}`,
    ]);
    expect(screen.getByText(INSPECTED_MEANING)).toBeInTheDocument();
    expect(screen.getByText(TUNNEL_MEANING)).toBeInTheDocument();
  });

  it("explains Inspected for an outside host even when every listed host is Tunnel only", () => {
    render(<PolicySummary spec={spec} hostMarks={{ inspected: [], terminated: ["api.github.com"], outside: ["api.github.com"] }} />);
    expect(within(row("api.github.com")).getByText(INSPECTED)).toBeInTheDocument();
    expect(screen.getByText(INSPECTED_MEANING)).toBeInTheDocument();
  });

  it("allow-all: the line, each opened host as Inspected, the Inspected meaning, then the allow-all line with no chip", () => {
    const { container } = render(
      <PolicySummary
        spec={{ ...spec, allow_all_egress: true } as RunPolicySpec}
        hostMarks={{ inspected: [], terminated: ["api.github.com", "api.anthropic.com", "api.openai.com"], outside: ["api.github.com", "api.anthropic.com"] }}
      />,
    );
    expect(screen.queryByText("pypi.org")).toBeNull();
    expect(screen.queryByText("api.openai.com")).toBeNull(); // terminated, but the proxy refuses it: not outside
    const text = container.textContent ?? "";
    const at = [ALLOW_ALL_TEXT, "api.github.com", "api.anthropic.com", INSPECTED_MEANING, ALLOW_ALL_LINE].map((s) => text.indexOf(s));
    expect(at.every((i) => i >= 0)).toBe(true);
    expect([...at].sort((a, b) => a - b)).toEqual(at);
    expect(within(row("api.github.com")).getByText(INSPECTED)).toBeInTheDocument();
    expect(screen.queryByText(TUNNEL)).toBeNull();
    expect(screen.queryByText(TUNNEL_MEANING)).toBeNull();
    const line = screen.getByText(ALLOW_ALL_LINE);
    expect(line.closest("p")?.children).toHaveLength(1); // the text only, no chip beside it
  });

  it("a null allowlist with an outside host still shows that host's Inspected row", () => {
    render(<PolicySummary spec={{ allowed_domains: null } as unknown as RunPolicySpec} hostMarks={{ inspected: [], terminated: ["api.github.com"], outside: ["api.github.com"] }} />);
    expect(within(row("api.github.com")).getByText(INSPECTED)).toBeInTheDocument();
    expect(screen.getByText(INSPECTED_MEANING)).toBeInTheDocument();
  });

  it("allow-all with nothing opened: the line and the allow-all line only", () => {
    render(<PolicySummary spec={{ ...spec, allow_all_egress: true } as RunPolicySpec} hostMarks={{ inspected: [], terminated: [], outside: [] }} />);
    expect(screen.getByText(ALLOW_ALL_TEXT)).toBeInTheDocument();
    expect(screen.getByText(ALLOW_ALL_LINE)).toBeInTheDocument();
    for (const s of [INSPECTED, TUNNEL, INSPECTED_MEANING, TUNNEL_MEANING]) expect(screen.queryByText(s)).toBeNull();
  });
});

describe("PolicySummary — traffic checks left to a model gateway", () => {
  const spec = (inspection: Record<string, unknown>) => ({ allowed_domains: [], llm_inspection: inspection }) as unknown as RunPolicySpec;

  it("names the gateway, and says Wardyn still checks the run's secret values when the policy has that check", () => {
    render(<PolicySummary spec={spec({ mode: "block", detect_secrets: true })} facts={{ inspectionLeftTo: ["Corp gateway"] }} />);
    expect(row("Network", "Traffic checks")).toHaveTextContent(
      "On, except prompts to Corp gateway" +
        "Your organisation has assigned these checks to Corp gateway. Wardyn still checks them for the secret values the run holds.",
    );
  });

  it("says Wardyn does not check them when the policy has no secret-value check", () => {
    render(<PolicySummary spec={spec({ mode: "alert", detect_pii: true })} facts={{ inspectionLeftTo: ["Corp gateway", "Lab gateway"] }} />);
    expect(row("Network", "Traffic checks")).toHaveTextContent(
      "On, except prompts to Corp gateway, Lab gateway" +
        "Your organisation has assigned these checks to Corp gateway, Lab gateway. Wardyn does not check them.",
    );
  });

  it("adds nothing when the policy turns traffic checks off, or no gateway checks prompts", () => {
    const { unmount } = render(<PolicySummary spec={spec({ mode: "off" })} facts={{ inspectionLeftTo: ["Corp gateway"] }} />);
    expect(row("Network", "Traffic checks").textContent).toBe("Off");
    unmount();
    render(<PolicySummary spec={spec({ mode: "block", detect_secrets: true })} facts={{ inspectionLeftTo: [] }} />);
    expect(row("Network", "Traffic checks").textContent).toBe("On");
  });
});

describe("PolicySummary — the Pushes row", () => {
  const pushes = (partial: Record<string, unknown>) => {
    render(<PolicySummary spec={{ git_push_any_branch: false, ...partial } as unknown as RunPolicySpec} />);
    return row("Tools and pushes", "Pushes");
  };

  it("says every push is refused for push_rules.deny_all, ahead of where a push could go", () => {
    expect(pushes({ push_rules: { deny_all: true }, git_push_any_branch: true, github_capabilities: ["code_write"] })).toHaveTextContent(WALL_PRESETS.PUSH_EVERY_REFUSED);
  });

  const GH_READ = ["code_read", "issues_read"];
  const ADO_READ = ["code_read", "work_read"];
  const GH_WRITE = ["code_read", "code_write"];
  // Table: the global sentence needs BOTH lists present and neither naming code_write. One list names its forge and the
  // other forge keeps the branch line, because its provider default (an absent list) can still push.
  it.each([
    ["both lists, neither writes", { github_capabilities: GH_READ, azure_devops_capabilities: ADO_READ }, [WALL_PRESETS.PUSH_NO_WRITE]],
    ["GitHub list only, no write: the ADO default can push", { github_capabilities: GH_READ }, ["GitHub: no pushes (no write access)", "Azure DevOps: only this run's own branch"]],
    ["ADO list only, no write: the GitHub default can push", { azure_devops_capabilities: ADO_READ, git_push_any_branch: true }, ["Azure DevOps: no pushes (no write access)", "GitHub: any branch"]],
    ["GitHub no write beside an ADO list that writes", { github_capabilities: GH_READ, azure_devops_capabilities: ["code_write"] }, ["GitHub: no pushes (no write access)", "Azure DevOps: only this run's own branch"]],
    ["both lists write", { github_capabilities: GH_WRITE, azure_devops_capabilities: ["code_write"] }, [SUMMARY.ownBranch]],
    ["no lists", {}, [SUMMARY.ownBranch]],
  ])("Pushes: %s", (_name, spec, want) => {
    const cell = pushes(spec);
    expect(cell).toHaveTextContent(want.join(""));
    if (want[0] !== WALL_PRESETS.PUSH_NO_WRITE) expect(cell).not.toHaveTextContent(/^No pushes/);
  });

  it("a stored policy whose ADO provider default can push is never read as no pushes at all", () => {
    // A stored policy (Policies screen): a GitHub read list and no ADO list.
    expect(pushes({ github_capabilities: GH_READ })).not.toHaveTextContent(WALL_PRESETS.PUSH_NO_WRITE);
  });

  it("keeps the run's own branch, or any branch, when a list names code_write or none is named", () => {
    expect(pushes({ github_capabilities: ["code_read", "code_write"] })).toHaveTextContent(SUMMARY.ownBranch);
    cleanup();
    expect(pushes({ git_push_any_branch: true })).toHaveTextContent(SUMMARY.anyBranch);
  });

  it("shows no Pushes row when the spec says nothing of pushing", () => {
    render(<PolicySummary spec={{ allowed_domains: ["a.example.com"] } as unknown as RunPolicySpec} />);
    expect(screen.queryByText("Pushes")).toBeNull();
  });
});

describe("PolicySummary: the wall preset's notes (P-63, P-62b)", () => {
  const spec = {
    allowed_domains: ["a.example.com"], denied_domains: ["paste.example.net"], first_use_approval: "always_deny",
    push_rules: { deny_all: true }, git_push_any_branch: false,
  } as unknown as RunPolicySpec;
  const chip = { label: "Wall preset: Sealed", fields: new Set(["allowed_domains", "push_rules"]) };
  const chipsIn = (group: string, label: string) => within(row(group, label)).queryAllByText("Wall preset: Sealed");

  it("puts the preset's chip on the rows built from fields it decided, and only those", () => {
    render(<PolicySummary spec={spec} notes={{ chip }} />);
    expect(chipsIn("Network", "Allowed hosts")).toHaveLength(1);
    expect(chipsIn("Tools and pushes", "Pushes")).toHaveLength(1);
    expect(chipsIn("Network", "Blocked hosts")).toHaveLength(0);
    expect(chipsIn("Network", "Any other host")).toHaveLength(0);
  });

  it("draws no chip without notes, or for a chip with no fields", () => {
    render(<PolicySummary spec={spec} notes={{ chip: { ...chip, fields: new Set() } }} />);
    expect(screen.queryByText("Wall preset: Sealed")).toBeNull();
  });

  it("reads Pushes can't be stopped on this install when the server says so, ahead of the no-write and branch lines", () => {
    render(<PolicySummary spec={{ git_push_any_branch: false, github_capabilities: ["code_read"] } as unknown as RunPolicySpec} notes={{ pushesUnstoppable: true }} />);
    expect(row("Tools and pushes", "Pushes")).toHaveTextContent(WALL_PRESETS.PUSHES_UNSTOPPABLE);
    expect(row("Tools and pushes", "Pushes")).not.toHaveTextContent(/no write access/);
  });

  it("under deny_all never says Pushes can't be stopped", () => {
    render(<PolicySummary spec={spec} notes={{ pushesUnstoppable: true }} />);
    const pushes = row("Tools and pushes", "Pushes");
    expect(pushes).toHaveTextContent(WALL_PRESETS.PUSH_EVERY_REFUSED);
    expect(pushes).not.toHaveTextContent(WALL_PRESETS.PUSHES_UNSTOPPABLE);
  });
});
