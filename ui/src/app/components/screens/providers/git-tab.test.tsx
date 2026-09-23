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

const setSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { setSecret: (...a: unknown[]) => setSecretMock(...a), deleteSecret: vi.fn() },
}));

function Harness({
  initial,
  onLatest,
  // What the screen passes: whether the LOADED snapshot was empty. Defaults to
  // the initial draft's own emptiness, which is what a fresh load looks like.
  loadedEmpty = initial.length === 0,
  onStatusRefresh = () => {},
  patBrokerEnabled,
}: {
  initial: GitProvider[];
  onLatest?: (git: GitProvider[]) => void;
  loadedEmpty?: boolean;
  onStatusRefresh?: () => void;
  patBrokerEnabled?: boolean;
}) {
  const [git, setGit] = React.useState(initial);
  onLatest?.(git);
  return (
    <GitTab
      git={git}
      onChange={setGit}
      present={[]}
      githubApp={false}
      operator
      loadedEmpty={loadedEmpty}
      patBrokerEnabled={patBrokerEnabled}
      onStatusRefresh={onStatusRefresh}
    />
  );
}

function CredHarness({
  initial,
  present,
  githubApp,
  onStatusRefresh = () => {},
}: {
  initial: GitProvider[];
  present: string[];
  githubApp: boolean;
  onStatusRefresh?: () => void;
}) {
  const [git, setGit] = React.useState(initial);
  return (
    <GitTab git={git} onChange={setGit} present={present} githubApp={githubApp} operator onStatusRefresh={onStatusRefresh} />
  );
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

  // #381 F8: the pat lane label/hint must actually flip when the loaded
  // switch is off — every earlier assertion in this file exercises the
  // default (on) shape only.
  it("shows the PAT lane's OFF-switch label and hint when patBrokerEnabled is false", () => {
    render(
      <Harness
        initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]}
        patBrokerEnabled={false}
      />,
    );
    const row = screen.getByTestId("provider-row-github");
    expect(within(row).getByText("PAT · in-sandbox")).toBeInTheDocument();
    expect(within(row).queryByText("PAT · brokered")).not.toBeInTheDocument();
    expect(
      within(row).getByText(
        "The simplest lane — stored once; a per-run helper hands it to git inside the sandbox at clone time.",
      ),
    ).toBeInTheDocument();
  });

  it("an absent row (Azure DevOps) shows the ROW_ABSENT_HINT and an Add provider button", () => {
    render(
      <Harness
        initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]}
      />,
    );
    expect(screen.getByText(PROVIDERS.ROW_ABSENT_HINT)).toBeInTheDocument();
  });

  // V1 lens A (medium) / #380 F5: the SSH scoping CEILING, said where the
  // policy is written. An SSH clone URL carries no path, so an org-scoped
  // row admits SSH for the whole host regardless of what it declares — the
  // console REFUSES the explicit selection outright now (the checkbox's own
  // disabled reason, LANE_SSH_PATH_SCOPED), matching the console door's 400,
  // rather than letting an admin pick it and hit a raw server error at Save.
  it("an org-scoped row disables the ssh checkbox with the path-scoped reason", () => {
    render(
      <Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"], lanes: ["ssh"] }]} />,
    );
    const row = screen.getByTestId("provider-row-github");
    expect(within(row).getByRole("checkbox", { name: /SSH · resident/ })).toBeDisabled();
    expect(within(row).getByText(PROVIDERS.LANE_SSH_PATH_SCOPED)).toBeInTheDocument();
  });

  it("...and stays available when the row bounds the whole host instead", () => {
    render(
      <Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com"], lanes: ["pat"] }]} />,
    );
    const row = screen.getByTestId("provider-row-github");
    expect(within(row).getByRole("checkbox", { name: /SSH · resident/ })).not.toBeDisabled();
    expect(screen.queryByText(PROVIDERS.LANE_SSH_PATH_SCOPED)).not.toBeInTheDocument();
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

  // Every form control on a row is reachable by its label — the textarea
  // through the Field's htmlFor/id pair, the lane boxes by their own names
  // inside a named group (a Field label cannot point at three controls).
  it("the base-URLs textarea and the lane checkboxes are reachable by name", () => {
    render(<Harness initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]} />);
    expect(screen.getByLabelText(PROVIDERS.FIELD_BASE_URLS).tagName).toBe("TEXTAREA");
    const group = screen.getByRole("group", { name: PROVIDERS.FIELD_LANES });
    expect(within(group).getAllByRole("checkbox")).toHaveLength(3);
    for (const box of within(group).getAllByRole("checkbox")) {
      expect(box).toHaveAccessibleName();
    }
  });

  // A fresh Azure DevOps row's default address has no org segment, so the row
  // must say so before the admin ever reaches the server's 400.
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

  // The base-URLs textarea must keep every line intact across keystrokes —
  // filtering the value and feeding it back on each change drops Enter before
  // a render, concatenating separate addresses into one garbage base URL and
  // breaking BASE_URLS_HINT's "one per line" promise.
  describe("the base-URLs textarea keeps its newlines (HIGH fix)", () => {
    it("typing a second line yields TWO base URLs, not one concatenation", async () => {
      let latest: GitProvider[] = [];
      render(
        <Harness
          initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]}
          onLatest={(g) => (latest = g)}
        />,
      );
      const textarea = screen.getByLabelText(PROVIDERS.FIELD_BASE_URLS) as HTMLTextAreaElement;
      await userEvent.type(textarea, "{Enter}https://git.corp.example/team");
      expect(textarea.value).toBe("https://github.com/acme\nhttps://git.corp.example/team");
      expect(latest[0].base_urls).toEqual(["https://github.com/acme", "https://git.corp.example/team"]);
    });

    it("the invalid hint stays live WHILE typing, on the text rather than the committed array", async () => {
      render(<Harness initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]} />);
      const row = screen.getByTestId("provider-row-github");
      const textarea = within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS) as HTMLTextAreaElement;
      await userEvent.type(textarea, "{Enter}http://plain.example");
      expect(within(row).getByText(PROVIDERS.BASE_URL_INVALID)).toBeInTheDocument();
      expect(textarea).toHaveAttribute("aria-invalid", "true");
      // Correcting the second line clears it, with both lines still there.
      await userEvent.clear(textarea);
      await userEvent.type(textarea, "https://github.com/acme{Enter}https://git.corp.example");
      expect(textarea.value).toBe("https://github.com/acme\nhttps://git.corp.example");
      expect(within(row).queryByText(PROVIDERS.BASE_URL_INVALID)).not.toBeInTheDocument();
    });

    it("a reload from outside re-seeds the textarea", () => {
      const { rerender } = render(
        <GitTab
          git={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]}
          onChange={() => {}}
          present={[]}
          githubApp={false}
          operator
          loadedEmpty={false}
          onStatusRefresh={() => {}}
        />,
      );
      rerender(
        <GitTab
          git={[{ id: "gh", kind: "github", base_urls: ["https://github.com/other"] }]}
          onChange={() => {}}
          present={[]}
          githubApp={false}
          operator
          loadedEmpty={false}
          onStatusRefresh={() => {}}
        />,
      );
      expect((screen.getByLabelText(PROVIDERS.FIELD_BASE_URLS) as HTMLTextAreaElement).value).toBe(
        "https://github.com/other",
      );
    });
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

  // F4-F13 (Appendix A V8): the credential-lane group needs roving tabindex
  // and arrow keys, and the selected lane's form must render OUTSIDE the
  // radiogroup — nesting a Save button inside it is an ARIA violation.
  describe("the credential-lane group has roving tabindex and arrow keys, and its body sits outside it", () => {
    // ticket: F4-F13
    it("only the checked lane is a Tab stop; ArrowRight moves selection and focus", async () => {
      render(<Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]} />);
      const row = screen.getByTestId("provider-row-github");
      const [pat, app, ssh] = within(row).getAllByRole("radio");
      expect(pat).toHaveAttribute("tabIndex", "0");
      expect(app).toHaveAttribute("tabIndex", "-1");
      expect(ssh).toHaveAttribute("tabIndex", "-1");

      pat.focus();
      await userEvent.keyboard("{ArrowRight}");
      expect(app).toHaveFocus();
      expect(app).toHaveAttribute("aria-checked", "true");
      expect(app).toHaveAttribute("tabIndex", "0");
      expect(pat).toHaveAttribute("tabIndex", "-1");

      await userEvent.keyboard("{ArrowLeft}{ArrowLeft}");
      expect(ssh).toHaveFocus(); // wraps
      expect(ssh).toHaveAttribute("aria-checked", "true");
    });

    it("the radiogroup contains ONLY the three radio buttons — no nested form", () => {
      render(<Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]} />);
      const row = screen.getByTestId("provider-row-github");
      const group = within(row).getByRole("radiogroup");
      // The PAT lane is selected by default (githubApp=false in this
      // Harness), so its Access-token field must not render inside the
      // radiogroup.
      expect(within(group).queryByLabelText("Access token")).not.toBeInTheDocument();
      expect(within(row).getByLabelText("Access token")).toBeInTheDocument();
    });

    it("selecting App by arrow key renders App's fields, not PAT's — the body follows the roving selection", async () => {
      render(<Harness initial={[{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }]} />);
      const row = screen.getByTestId("provider-row-github");
      const [pat] = within(row).getAllByRole("radio");
      pat.focus();
      await userEvent.keyboard("{ArrowRight}");
      expect(within(row).getByLabelText("App ID")).toBeInTheDocument();
      expect(within(row).queryByLabelText("Access token")).not.toBeInTheDocument();
    });
  });

  // The Git tab renders the three LEGACY lanes only; "entra" is configured
  // elsewhere. Rewriting `lanes` from the rendered set alone DROPPED it, and a
  // row left holding an entra block with no entra lane is refused by the
  // server as an orphaned block — a 400 on Save that no admin could trace back
  // to the checkbox they clicked.
  describe("a lane this tab does not render survives a toggle", () => {
    const entraRow: GitProvider = {
      id: "ado",
      kind: "azure_devops",
      base_urls: ["https://dev.azure.com/acme"],
      lanes: ["entra"],
      credential_source: "per_user",
      entra: {
        tenant_id: "0f2c1f1e-9d3a-4b8c-8f2d-1a2b3c4d5e6f",
        client_id: "7a6b5c4d-3e2f-4a1b-9c8d-7e6f5a4b3c2d",
        capability_ceiling: ["read", "code_write"],
      },
    };

    it("toggling pat on an entra row keeps entra", async () => {
      let latest: GitProvider[] = [];
      render(<Harness initial={[entraRow]} onLatest={(g) => (latest = g)} />);
      const row = screen.getByTestId("provider-row-azure_devops");
      await userEvent.click(within(row).getByRole("checkbox", { name: /PAT · brokered/ }));
      expect(latest[0].lanes).toContain("entra");
      expect(latest[0].lanes).toContain("pat");
      expect(latest[0].entra).toBeDefined();
    });

    it("and never collapses to the empty 'every legacy lane' form while entra is held", async () => {
      let latest: GitProvider[] = [];
      render(<Harness initial={[{ ...entraRow, lanes: ["entra"] }]} onLatest={(g) => (latest = g)} />);
      const row = screen.getByTestId("provider-row-azure_devops");
      // PAT is the only legacy lane an Azure DevOps row can tick (#380 makes ssh
      // unselectable on every legal row, and app has no ADO equivalent). Ticking
      // and then clearing it must leave entra held — never the empty list, which
      // the server reads as "every legacy lane" and which drops entra.
      const pat = within(row).getByRole("checkbox", { name: /PAT · brokered/ });
      await userEvent.click(pat);
      expect(latest[0].lanes).toEqual(expect.arrayContaining(["pat", "entra"]));
      await userEvent.click(pat);
      expect(latest[0].lanes).toEqual(["entra"]);
      expect(latest[0].lanes).not.toEqual([]);
    });

    it("an entra lane leaves the three legacy checkboxes unchecked — the field narrows", () => {
      render(<Harness initial={[entraRow]} />);
      const row = screen.getByTestId("provider-row-azure_devops");
      for (const box of within(row).getAllByRole("checkbox")) {
        expect(box).toHaveAttribute("aria-checked", "false");
      }
    });
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
    // Two canon keys, one per slot — never one sentence sliced on "? ".
    expect(within(dialog).getByText(PROVIDERS.REMOVE_CONFIRM_TITLE(PROVIDERS.KIND_GITHUB))).toBeInTheDocument();
    expect(within(dialog).getByText(PROVIDERS.REMOVE_CONFIRM_BODY)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    // The only row is gone — back to true legacy open mode (the banner alone).
    expect(await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE)).toBeInTheDocument();
  });

  // An empty `lanes` (wire "every lane the kind supports") must never expand
  // into a lane the kind cannot carry: toggling `ssh` off an Azure DevOps row
  // must not write `["app","pat"]`, since the server 400s that and the
  // disabled `app` checkbox leaves no way to un-write it.
  describe("lane toggling never writes an unavailable lane (HIGH fix)", () => {
    // #380 F5: an Azure DevOps base URL's org segment is MANDATORY
    // (validateProviderHostForKind), so no legal ADO row can ever present a
    // bare dev.azure.com entry — the explicit ssh lane is therefore never
    // selectable there (sshLaneExceedsPathScope is unconditionally true for
    // it), the same way app already never is. The checkbox stays DISABLED
    // with its own reason rather than 400ing on Save, and clicking it is a
    // no-op — the row is never touched.
    it("ssh is disabled with its own path-scoped reason on every Azure DevOps row, and clicking it is a no-op", async () => {
      let latest: GitProvider[] = [];
      render(
        <Harness
          initial={[{ id: "ado", kind: "azure_devops", base_urls: ["https://dev.azure.com/acme"] }]}
          onLatest={(g) => (latest = g)}
        />,
      );
      const row = screen.getByTestId("provider-row-azure_devops");
      const checkbox = within(row).getByRole("checkbox", { name: /SSH · resident/ });
      expect(checkbox).toBeDisabled();
      expect(within(row).getByText(PROVIDERS.LANE_SSH_PATH_SCOPED)).toBeInTheDocument();
      await userEvent.click(checkbox);
      expect(latest[0].lanes).toBeUndefined();
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
      await userEvent.click(within(row).getByRole("checkbox", { name: /PAT · brokered/ }));
      // pat was the only available lane; toggling it off leaves nothing
      // available permitted, and the empty-wire-convention never fires here
      // because it means "every AVAILABLE lane", not "every lane" — with pat
      // off, available (pat) != permitted (none), so it stays an explicit [].
      expect(latest[0].lanes).toEqual([]);
    });

    // #380 F5 moved this off Azure DevOps: with ssh now unconditionally
    // unavailable there (see above), ADO has only ONE ever-available lane and
    // can no longer exercise "re-enabling the last missing lane collapses
    // back to []" — a bare github.com row still can (all three lanes stay
    // available with no org path to scope any of them out).
    it("re-enabling every available lane on a bare github.com row stores [] (every lane the kind supports), never a 3-item array", async () => {
      let latest: GitProvider[] = [];
      render(
        <Harness
          initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com"], lanes: ["app", "pat"] }]}
          onLatest={(g) => (latest = g)}
        />,
      );
      const row = screen.getByTestId("provider-row-github");
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
    // The CHIP's own canon key, not ROW_DISABLED_HINT sliced at its colon: a
    // canon edit that drops the colon must not dump the whole sentence in here.
    expect(within(row).getByText(PROVIDERS.ROW_DISABLED_CHIP)).toBeInTheDocument();
    expect(PROVIDERS.ROW_DISABLED_CHIP).toBe("Off");
  });

  // With a row LOADED, an empty draft is a pending removal — the screen's Save
  // owns the affirmative, so the banner's Add steps down to outline rather
  // than showing a second teal (CONSOLE-RULES §2).
  it("the legacy-open banner's Add is teal on a loaded-empty snapshot and outline after a removal", () => {
    const { unmount } = render(<Harness initial={[]} />);
    expect(screen.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }).className.split(/\s+/)).toContain("bg-primary");
    unmount();
    render(<Harness initial={[]} loadedEmpty={false} />);
    expect(screen.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }).className.split(/\s+/)).not.toContain(
      "bg-primary",
    );
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

  // primaryHost() defaults to "github.com" whenever base_urls[0] is absent or
  // unparseable, and EVERY credential lane in the row keys off it — so an
  // Azure DevOps row with no address must not write the admin's ADO PAT to
  // git-pat-github-com, the secret the GitHub clone helper reads, from inside
  // a radiogroup labelled "Azure DevOps credentials".
  describe("the credential lanes never default to github.com (BLOCKER)", () => {
    it("a row with no parseable address disables every lane, names no secret, and can save nothing", async () => {
      setSecretMock.mockReset();
      render(<CredHarness initial={[{ id: "ado", kind: "azure_devops", base_urls: [] }]} present={[]} githubApp={false} />);
      const row = screen.getByTestId("provider-row-azure_devops");
      expect(within(row).getByText(PROVIDERS.LANES_NEED_ADDRESS)).toBeInTheDocument();
      const lanes = within(row).getAllByRole("radio");
      expect(lanes.length).toBeGreaterThan(0);
      for (const lane of lanes) {
        expect(lane).toBeDisabled();
        // Nothing is OPEN either: a lane body is where Save and the secret name live.
        expect(lane).toHaveAttribute("aria-checked", "false");
      }
      expect(within(row).queryByText(/git-pat-/)).not.toBeInTheDocument();
      expect(within(row).queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
      for (const lane of lanes) await userEvent.click(lane);
      expect(setSecretMock).not.toHaveBeenCalled();
    });

    it("an Azure DevOps row with its own address saves the PAT under ITS host", async () => {
      setSecretMock.mockReset();
      render(
        <CredHarness
          initial={[{ id: "ado", kind: "azure_devops", base_urls: ["https://dev.azure.com/acme"] }]}
          present={[]}
          githubApp={false}
        />,
      );
      const row = screen.getByTestId("provider-row-azure_devops");
      expect(within(row).queryByText(PROVIDERS.LANES_NEED_ADDRESS)).not.toBeInTheDocument();
      await userEvent.type(within(row).getByLabelText("Access token"), "ado-pat-value");
      await userEvent.click(within(row).getByRole("button", { name: "Save" }));
      expect(setSecretMock).toHaveBeenCalledWith("git-pat-dev-azure-com", "ado-pat-value");
      // The field must not suggest GitHub's `ghp_…` placeholder on the ADO row
      // — the cosmetic sibling of the very bug this screen exists to stop.
      expect(within(row).getByLabelText("Access token")).toHaveAttribute("placeholder", "Paste the token");
    });

    it("a github row still saves the PAT under github.com — the host is the row's, not a default", async () => {
      setSecretMock.mockReset();
      render(
        <CredHarness
          initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]}
          present={[]}
          githubApp={false}
        />,
      );
      const row = screen.getByTestId("provider-row-github");
      await userEvent.type(within(row).getByLabelText("Access token"), "ghp_x");
      await userEvent.click(within(row).getByRole("button", { name: "Save" }));
      expect(setSecretMock).toHaveBeenCalledWith("git-pat-github-com", "ghp_x");
      // …and github (including GHES, kind `github` on a corporate host) keeps it.
      expect(within(row).getByLabelText("Access token")).toHaveAttribute("placeholder", "ghp_…");
    });
  });

  // Every SecretLane in a provider row must fire something after Save or
  // Disconnect — a no-op onChanged would leave the lane saying not connected
  // after Save, or still showing Connected after Disconnect, until reload.
  // The fix threads a setup-status-only refresh through GitTab -> Row ->
  // SecretLane (four sites); an unsaved base-URL edit on a SIBLING field must
  // survive a credential save, so the fix must never become `load()`.
  describe("a credential save fires the setup-status-only refresh", () => {
    // ticket: F4-F2
    it("saving a PAT fires onStatusRefresh", async () => {
      setSecretMock.mockReset().mockResolvedValue(undefined);
      const onStatusRefresh = vi.fn();
      render(
        <CredHarness
          initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]}
          present={[]}
          githubApp={false}
          onStatusRefresh={onStatusRefresh}
        />,
      );
      const row = screen.getByTestId("provider-row-github");
      await userEvent.type(within(row).getByLabelText("Access token"), "ghp_x");
      await userEvent.click(within(row).getByRole("button", { name: "Save" }));
      await screen.findByText(/Stored as/);
      expect(onStatusRefresh).toHaveBeenCalledTimes(1);
    });

    it("the base-URL draft survives a credential save — the fix is never load()", async () => {
      setSecretMock.mockReset().mockResolvedValue(undefined);
      render(
        <Harness
          initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]}
          onStatusRefresh={() => {}}
        />,
      );
      const row = screen.getByTestId("provider-row-github");
      const urls = within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS);
      await userEvent.type(urls, "{Enter}https://git.corp.example/team");
      await userEvent.type(within(row).getByLabelText("Access token"), "ghp_x");
      await userEvent.click(within(row).getByRole("button", { name: "Save" }));
      await screen.findByText(/Stored as/);
      expect(within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS)).toHaveValue(
        "https://github.com/acme\nhttps://git.corp.example/team",
      );
    });
  });

  // baseURLError("") returns null by design (a blank LINE is not an error),
  // so an emptied textarea must flag itself rather than silently committing
  // `base_urls: []` — which the server refuses outright.
  it("clearing the addresses flags the row instead of silently committing an empty list", async () => {
    let latest: GitProvider[] = [];
    render(
      <Harness
        initial={[{ id: "gh", kind: "github", base_urls: ["https://github.com/acme"] }]}
        onLatest={(g) => (latest = g)}
      />,
    );
    const row = screen.getByTestId("provider-row-github");
    const textarea = within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS);
    await userEvent.clear(textarea);
    expect(latest[0].base_urls).toEqual([]);
    expect(textarea).toHaveAttribute("aria-invalid", "true");
    // Its own sentence, not BASE_URL_INVALID's "must be an https:// URL ...",
    // which diagnoses a typed line and reads wrong over an empty field.
    expect(within(row).getByText(PROVIDERS.BASE_URLS_REQUIRED)).toBeInTheDocument();
    expect(within(row).queryByText(PROVIDERS.BASE_URL_INVALID)).not.toBeInTheDocument();
    // ...and with no host there is no credential lane to save under.
    expect(within(row).getByText(PROVIDERS.LANES_NEED_ADDRESS)).toBeInTheDocument();

    await userEvent.type(textarea, "https://github.com/acme");
    expect(textarea).toHaveAttribute("aria-invalid", "false");
    expect(within(row).queryByText(PROVIDERS.BASE_URLS_REQUIRED)).not.toBeInTheDocument();
    expect(within(row).queryByText(PROVIDERS.LANES_NEED_ADDRESS)).not.toBeInTheDocument();
  });

  it("an off row with no addresses keeps the reason readable — its textarea is collapsed", () => {
    render(<Harness initial={[{ id: "gh", kind: "github", base_urls: [], disabled: true }]} />);
    const row = screen.getByTestId("provider-row-github");
    expect(within(row).getByText(PROVIDERS.ROW_DISABLED_HINT)).toBeInTheDocument();
    expect(within(row).getByText(PROVIDERS.BASE_URLS_REQUIRED)).toBeInTheDocument();
  });
});
