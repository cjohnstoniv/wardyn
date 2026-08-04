/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The eight generic sections: what a row that carries its OWN hosts, header and
// secret says on screen. The claim under test throughout is honesty about
// delivery — a row must never look credentialed when nothing can present a
// credential for it.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { GenericSections, genericBlastRadius } from "./generic-sections";
import { genericIntegrations, type GenericIntegrationRow } from "../../../lib/api/integrations";
import type { SetupStatus, WireIntegration } from "../../../lib/types/setup";

function rowsFrom(integrations: WireIntegration[]): GenericIntegrationRow[] {
  return genericIntegrations({ integrations } as unknown as SetupStatus);
}

const FEED: WireIntegration = {
  id: "corp-artifactory",
  name: "Corp Artifactory",
  category: "package_feed",
  type: "artifactory",
  hosts: ["artifactory.corp.internal"],
  header: "Authorization",
  format: "Bearer %s",
  credentials: { token: "artifactory-token" },
  source: "stored",
};

const DB: WireIntegration = {
  id: "prod-postgres",
  name: "Prod Postgres",
  category: "data_store",
  type: "postgres",
  hosts: ["db.corp.internal:5432"],
  source: "stored",
};

function renderSections(integrations: WireIntegration[], operator = true) {
  return render(
    <GenericSections rows={rowsFrom(integrations)} operator={operator} onAdd={vi.fn()} onChanged={vi.fn()} />,
  );
}

describe("genericIntegrations", () => {
  it("selects only the generic categories, leaving the derived legacy ones alone", () => {
    const rows = rowsFrom([
      FEED,
      { id: "anthropic_api_key", category: "ai_provider", type: "anthropic_api_key" },
      { id: "github_app", category: "scm_host", type: "github_app" },
      { id: "artifact_mirror:x", category: "artifact_mirror", type: "artifact_mirror" },
    ]);
    expect(rows.map((r) => r.wire.id)).toEqual(["corp-artifactory"]);
  });

  it("states delivery from the ROW, not from the type's ideal", () => {
    // A header naming a stored secret is proxy-injected...
    expect(rowsFrom([FEED])[0].delivery).toBe("proxy");
    // ...and the same type WITHOUT one cannot be, however it usually works.
    const naked = { ...FEED, header: undefined, credentials: undefined };
    expect(rowsFrom([naked])[0].delivery).toBe("notbuilt");
    // A group that authenticates outside HTTP is egress-only by its own nature.
    expect(rowsFrom([DB])[0].delivery).toBe("notbuilt");
  });
});

describe("GenericSections", () => {
  it("renders a row under its category section with its hosts and delivery", () => {
    renderSections([FEED]);
    expect(screen.getByRole("region", { name: "Package & artifact feeds" })).toBeInTheDocument();
    expect(screen.getByText("Corp Artifactory")).toBeInTheDocument();
    expect(screen.getByText("artifactory.corp.internal")).toBeInTheDocument();
    expect(screen.getByText("proxy-injected")).toBeInTheDocument();
  });

  it("shows a data store as egress only — never as credentialed", () => {
    renderSections([DB]);
    expect(screen.getByText("egress only")).toBeInTheDocument();
    expect(screen.queryByText("proxy-injected")).not.toBeInTheDocument();
  });

  it("drops empty sections rather than listing eight empty headings", () => {
    renderSections([FEED]);
    expect(screen.queryByRole("region", { name: "Observability & incident" })).not.toBeInTheDocument();
  });

  it("offers no delete to a viewer", () => {
    renderSections([FEED], false);
    expect(screen.queryByRole("button", { name: /delete corp artifactory/i })).not.toBeInTheDocument();
  });
});

describe("genericBlastRadius", () => {
  it("names the hosts that stop being reachable and says secrets survive", () => {
    const lines = genericBlastRadius(rowsFrom([FEED])[0]);
    expect(lines.join(" ")).toContain("artifactory.corp.internal");
    expect(lines.join(" ")).toContain("artifactory-token is not deleted");
  });

  it("says nothing leaves the store when no credential is attached", () => {
    expect(genericBlastRadius(rowsFrom([DB])[0]).join(" ")).toContain("nothing leaves the secret store");
  });
});
