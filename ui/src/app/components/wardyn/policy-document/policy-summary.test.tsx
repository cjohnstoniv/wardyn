/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { RunPolicySpec } from "../../../lib/types";
import { SUMMARY } from "../../screens/run-detail/policy-tab-copy";
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
    expect(row("Tools and pushes", "Pushes")).toHaveTextContent("Only this run's own branch");
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
