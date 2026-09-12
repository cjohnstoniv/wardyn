/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Git tab: one row per closed kind, always; the credential lanes render
// INSIDE the row (connection-cards.tsx's Lane/SecretLane, EXPORTED — never
// re-typed here).
import * as React from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { GitProvider } from "../../../lib/api/providers";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { GitTab } from "./git-tab";

vi.mock("../../../lib/api/secrets", () => ({
  secrets: { setSecret: vi.fn(), deleteSecret: vi.fn() },
}));

function Harness({ initial, onLatest }: { initial: GitProvider[]; onLatest?: (git: GitProvider[]) => void }) {
  const [git, setGit] = React.useState(initial);
  onLatest?.(git);
  return <GitTab git={git} onChange={setGit} present={[]} githubApp={false} operator />;
}

function CredHarness({ initial, present, githubApp }: { initial: GitProvider[]; present: string[]; githubApp: boolean }) {
  const [git, setGit] = React.useState(initial);
  return <GitTab git={git} onChange={setGit} present={present} githubApp={githubApp} operator />;
}

describe("GitTab", () => {
  it("renders the legacy-open banner when there are zero rows", () => {
    render(<Harness initial={[]} />);
    expect(screen.getByText(PROVIDERS.LEGACY_OPEN_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.LEGACY_OPEN_BODY)).toBeInTheDocument();
  });

  it("adding the first row clears the banner and renders both kind rows", async () => {
    render(<Harness initial={[]} />);
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }));
    expect(screen.queryByText(PROVIDERS.LEGACY_OPEN_TITLE)).not.toBeInTheDocument();
    expect(screen.getByTestId("provider-row-github")).toBeInTheDocument();
  });

  it("an absent row (Azure DevOps) shows the ROW_ABSENT_HINT and an Add provider button", () => {
    render(
      <Harness
        initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]}
      />,
    );
    expect(screen.getByText(PROVIDERS.ROW_ABSENT_HINT)).toBeInTheDocument();
  });

  // V1 lens A (medium): the SSH scoping CEILING, said where the policy is
  // written. An SSH clone URL carries no path, so an org-scoped row admits SSH
  // for the whole host — visible exactly when the row has a path AND permits ssh.
  it("an org-scoped row that permits ssh shows the host-level ceiling hint", () => {
    render(
      <Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"], lanes: ["ssh"] }]} />,
    );
    expect(screen.getByText(PROVIDERS.SSH_HOST_LEVEL_HINT)).toBeInTheDocument();
  });

  it("...and stays quiet when the row drops ssh, or bounds the whole host anyway", () => {
    const { unmount } = render(
      <Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"], lanes: ["pat"] }]} />,
    );
    expect(screen.queryByText(PROVIDERS.SSH_HOST_LEVEL_HINT)).not.toBeInTheDocument();
    unmount();
    render(
      <Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com"], lanes: ["ssh"] }]} />,
    );
    expect(screen.queryByText(PROVIDERS.SSH_HOST_LEVEL_HINT)).not.toBeInTheDocument();
  });

  it("a disabled row collapses to the ROW_DISABLED_HINT — never removed", () => {
    render(
      <Harness
        initial={[
          { id: "github", kind: "github", base_urls: ["https://github.com/acme"], disabled: true },
        ]}
      />,
    );
    expect(screen.getByText(PROVIDERS.ROW_DISABLED_HINT)).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.FIELD_BASE_URLS)).not.toBeInTheDocument();
    expect(screen.getByTestId("provider-row-github")).toBeInTheDocument();
  });

  it("an invalid base URL renders BASE_URL_INVALID under the textarea", () => {
    render(
      <Harness
        initial={[{ id: "github", kind: "github", base_urls: ["http://github.com/acme"] }]}
      />,
    );
    expect(screen.getByText(PROVIDERS.BASE_URL_INVALID)).toBeInTheDocument();
  });

  // V1 lens E: every form control on a row is reachable by its label — the
  // textarea through the Field's htmlFor/id pair, the lane boxes by their own
  // names inside a named group (a Field label cannot point at three controls).
  it("the base-URLs textarea and the lane checkboxes are reachable by name", () => {
    render(<Harness initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]} />);
    expect(screen.getByLabelText(PROVIDERS.FIELD_BASE_URLS).tagName).toBe("TEXTAREA");
    const group = screen.getByRole("group", { name: PROVIDERS.FIELD_LANES });
    expect(within(group).getAllByRole("checkbox")).toHaveLength(3);
    for (const box of within(group).getAllByRole("checkbox")) {
      expect(box).toHaveAccessibleName();
    }
  });

  // VL-22: a fresh Azure DevOps row's default address has no org segment, so
  // the row must say so before the admin ever reaches the server's 400.
  it("a freshly added Azure DevOps row shows the invalid hint until its org is appended", async () => {
    render(<Harness initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com"] }]} />);
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }));
    const row = screen.getByTestId("provider-row-azure_devops");
    expect(within(row).getByText(PROVIDERS.BASE_URL_INVALID)).toBeInTheDocument();
    const textarea = within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS);
    expect(textarea).toHaveAttribute("aria-invalid", "true");
    await userEvent.clear(textarea);
    await userEvent.type(textarea, "https://dev.azure.com/acme");
    expect(within(row).queryByText(PROVIDERS.BASE_URL_INVALID)).not.toBeInTheDocument();
    expect(textarea).toHaveAttribute("aria-invalid", "false");
  });

  it("the app lane is disabled with its reason on an Azure DevOps row", () => {
    render(
      <Harness
        initial={[{ id: "ado", kind: "azure_devops", base_urls: ["https://dev.azure.com/acme"] }]}
      />,
    );
    expect(screen.getByText(PROVIDERS.LANE_APP_UNAVAILABLE)).toBeInTheDocument();
  });

  it("the ssh lane is disabled with its reason on a self-hosted GHES row", () => {
    render(
      <Harness
        initial={[{ id: "ghes", kind: "github", base_urls: ["https://git.corp.example"] }]}
      />,
    );
    expect(screen.getByText(PROVIDERS.LANE_SSH_UNAVAILABLE)).toBeInTheDocument();
  });

  it("renders the credential lanes (PAT, and App for a github.com row) inside the row", () => {
    render(
      <Harness
        initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]}
      />,
    );
    expect(screen.getByRole("radio", { name: /Personal access token/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /GitHub App/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /SSH key/ })).toBeInTheDocument();
  });

  it("S.GIT_FOOTER renders once, as the plain note under the tab", () => {
    render(<Harness initial={[]} />);
    expect(
      screen.getByText(/Only the GitHub App lane keeps its token outside the sandbox/),
    ).toBeInTheDocument();
  });

  it("Remove opens a confirm dialog naming the kind, and clears the row on confirm", async () => {
    render(
      <Harness
        initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    // The only row is gone — back to true legacy open mode (the banner alone).
    expect(await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE)).toBeInTheDocument();
  });

  // HIGH fix: an empty `lanes` (wire "every lane the kind supports") must
  // never expand into a lane the kind cannot carry — the bug where toggling
  // `ssh` off an Azure DevOps row wrote `["app","pat"]`, which the server
  // 400s and the disabled `app` checkbox then leaves no way to un-write.
  describe("lane toggling never writes an unavailable lane (HIGH fix)", () => {
    it("toggling ssh off an Azure DevOps row (app unavailable) never writes app", async () => {
      let latest: GitProvider[] = [];
      render(
        <Harness
          initial={[{ id: "ado", kind: "azure_devops", base_urls: ["https://dev.azure.com/acme"] }]}
          onLatest={(g) => (latest = g)}
        />,
      );
      const row = screen.getByTestId("provider-row-azure_devops");
      await userEvent.click(within(row).getByRole("checkbox", { name: /SSH · resident/ }));
      expect(latest[0].lanes).toEqual(["pat"]);
      expect(latest[0].lanes).not.toContain("app");
    });

    it("toggling pat off a self-hosted GHES row (app + ssh unavailable) never writes app or ssh", async () => {
      let latest: GitProvider[] = [];
      render(
        <Harness
          initial={[{ id: "ghes", kind: "github", base_urls: ["https://git.corp.example"] }]}
          onLatest={(g) => (latest = g)}
        />,
      );
      const row = screen.getByTestId("provider-row-github");
      await userEvent.click(within(row).getByRole("checkbox", { name: /PAT · in-sandbox/ }));
      // pat was the only available lane; toggling it off leaves nothing
      // available permitted, and the empty-wire-convention never fires here
      // because it means "every AVAILABLE lane", not "every lane" — with pat
      // off, available (pat) != permitted (none), so it stays an explicit [].
      expect(latest[0].lanes).toEqual([]);
    });

    it("re-enabling every available lane on an Azure DevOps row stores [] (every lane the kind supports), never a 3-item array", async () => {
      let latest: GitProvider[] = [];
      render(
        <Harness
          initial={[{ id: "ado", kind: "azure_devops", base_urls: ["https://dev.azure.com/acme"], lanes: ["pat"] }]}
          onLatest={(g) => (latest = g)}
        />,
      );
      const row = screen.getByTestId("provider-row-azure_devops");
      await userEvent.click(within(row).getByRole("checkbox", { name: /SSH · resident/ }));
      expect(latest[0].lanes).toEqual([]);
    });
  });

  it("the row's own on/off chip reads Enabled when on", () => {
    render(<Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]} />);
    const row = screen.getByTestId("provider-row-github");
    expect(within(row).getByText(PROVIDERS.FIELD_ENABLED)).toBeInTheDocument();
  });

  it("the row's own on/off chip reads Off when disabled (mock State 3)", () => {
    render(
      <Harness
        initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"], disabled: true }]}
      />,
    );
    const row = screen.getByTestId("provider-row-github");
    expect(within(row).getByText("Off")).toBeInTheDocument();
  });

  describe("the credential lane opens on whatever is already connected (MEDIUM 4)", () => {
    it("opens on SSH when an ssh-key-<host> secret is present", () => {
      render(
        <CredHarness
          initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]}
          present={["ssh-key-github-com"]}
          githubApp={false}
        />,
      );
      expect(screen.getByRole("radio", { name: /SSH key/ })).toHaveAttribute("aria-checked", "true");
    });

    it("opens on App when a GitHub App is stored and SSH is not", () => {
      render(
        <CredHarness
          initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]}
          present={[]}
          githubApp={true}
        />,
      );
      expect(screen.getByRole("radio", { name: /GitHub App/ })).toHaveAttribute("aria-checked", "true");
    });

    it("opens on PAT (the shipped default) when nothing is connected", () => {
      render(
        <CredHarness
          initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]}
          present={[]}
          githubApp={false}
        />,
      );
      expect(screen.getByRole("radio", { name: /Personal access token/ })).toHaveAttribute("aria-checked", "true");
    });
  });

  it("the Remove confirm is outline, never the teal default (mock State 3)", async () => {
    render(
      <Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]} />,
    );
    const row = screen.getByTestId("provider-row-github");
    await userEvent.click(within(row).getByRole("button", { name: "Remove" }));
    const dialog = screen.getByRole("alertdialog");
    const confirm = within(dialog).getByRole("button", { name: "Remove" });
    expect(confirm.className.split(/\s+/)).not.toContain("bg-primary");
  });
});
