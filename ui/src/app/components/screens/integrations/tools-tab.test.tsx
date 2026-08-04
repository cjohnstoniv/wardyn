/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { T } from "../../../lib/integrations";
import type { IntegrationsData } from "../../../lib/api/integrations";
import { ToolsTab } from "./tools-tab";

const EMPTY: IntegrationsData = { ai: [], scm: [] };

describe("ToolsTab — the six rows + LAW footer", () => {
  it("renders all six tool rows and the LAW footer, verbatim", () => {
    render(<ToolsTab data={EMPTY} redirects={[]} />);

    expect(screen.getByText("git")).toBeInTheDocument();
    expect(screen.getByText("Package managers")).toBeInTheDocument();
    // Claude Code / Codex CLI render via AgentBadge, not a plain name span.
    expect(screen.getByText("Claude Code")).toBeInTheDocument();
    expect(screen.getByText("Codex")).toBeInTheDocument();
    expect(screen.getByText("gh CLI")).toBeInTheDocument();
    expect(screen.getByText("Your own tools")).toBeInTheDocument();

    expect(screen.getAllByText("can drive a run")).toHaveLength(2);
    expect(screen.getByText(T.LAW)).toBeInTheDocument();
  });

  it("gh CLI and Your own tools are permanent facts: 'Powered by' is always the dash, never data-driven", () => {
    const data: IntegrationsData = {
      ...EMPTY,
      ai: [
        {
          id: "ai:anthropic_api_key",
          category: "ai_provider",
          name: "Anthropic (API key)",
          typeLabel: "anthropic · api key",
          chips: [{ label: "Claude Code · default", tone: "info" }],
          residency: "proxy_injected",
          posture: { kind: "configured" },
          secretNames: ["anthropic-api-key"],
          checkIds: [],
        },
      ],
    };
    render(<ToolsTab data={data} redirects={[]} />);
    // Claude Code is now wiring-derived (shows the integration's name)…
    expect(screen.getByText("Anthropic (API key) · default")).toBeInTheDocument();
    // …but gh CLI / Your own tools still read "—" regardless of what's configured.
    expect(screen.getAllByText("—")).toHaveLength(2);
  });

  it("git's Powered-by lists configured SCM integrations by name", () => {
    const data: IntegrationsData = {
      ...EMPTY,
      scm: [
        {
          id: "scm:github.com",
          category: "scm_host",
          name: "GitHub",
          typeLabel: "github.com",
          chips: [],
          residency: "resident_env",
          posture: { kind: "configured" },
          secretNames: ["git-pat-github-com"],
          checkIds: [],
        },
      ],
    };
    render(<ToolsTab data={data} redirects={[]} />);
    expect(screen.getByText("GitHub")).toBeInTheDocument();
  });

  // An egress redirect stopped being an integration row when Corporate network
  // took the category over, but it's still what a package manager fetches
  // THROUGH — so this row reads it straight off the site config rather than
  // falsely reporting "Nothing yet."
  it("Package managers' Powered-by names the mirror host from egress_redirects, not an integration row", () => {
    render(
      <ToolsTab
        data={EMPTY}
        redirects={[{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }]}
      />,
    );
    expect(screen.getByText("artifactory.corp.internal")).toBeInTheDocument();
  });
});
