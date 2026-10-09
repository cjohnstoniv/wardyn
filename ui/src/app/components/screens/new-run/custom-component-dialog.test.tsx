/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ComponentSaved } from "../../../lib/types";
import { setField } from "../../../../test/set-field";
import { CustomComponentDialog, type CustomComponentDialogProps } from "./custom-component-dialog";

// The real components client over a stubbed fetch: the routes, verbs and bodies
// below are what goes on the wire.
const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

const calls = () =>
  fetchMock.mock.calls.map(([path, init]) => ({
    path: String(path),
    method: String(init?.method),
    body: init?.body ? JSON.parse(String(init.body)) : undefined,
  }));

const SAVED: ComponentSaved = {
  id: "11111111-1111-1111-1111-111111111111",
  owner: "me",
  name: "Acme",
  definition: { hosts: ["api.acme.test"] },
  version: 1,
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:00:00Z",
  requirements: [],
};

function open(over: Partial<CustomComponentDialogProps> = {}) {
  const props: CustomComponentDialogProps = {
    context: "run",
    residentAllowed: true,
    secretsPath: "/secrets",
    onClose: vi.fn(),
    onAttach: vi.fn(),
    onSaved: vi.fn(),
    ...over,
  };
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  render(
    <MemoryRouter>
      <CustomComponentDialog {...props} />
    </MemoryRouter>,
  );
  return { props, user };
}

const type = async (_user: ReturnType<typeof userEvent.setup>, label: RegExp | string, text: string) => {
  setField(screen.getByLabelText(label), text);
};

describe("CustomComponentDialog: this run only", () => {
  it("adds an inline definition to the run and sends nothing to the server", async () => {
    const { props, user } = open();
    await type(user, /^Hosts it reaches/, "api.acme.test");
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    await type(user, /^Stored secret name/, "acme-key");
    await user.click(screen.getByRole("button", { name: "Add to this run" }));

    expect(props.onAttach).toHaveBeenCalledWith({
      inline: {
        hosts: ["api.acme.test"],
        secrets: [{ secret_name: "acme-key", delivery: { mode: "header", host: "api.acme.test" } }],
      },
    });
    expect(props.onClose).toHaveBeenCalled();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("labels the inline component when it has a name", async () => {
    const { props, user } = open();
    await type(user, /^Name/, "Acme API");
    await type(user, /^Hosts it reaches/, "api.acme.test");
    await user.click(screen.getByRole("button", { name: "Add to this run" }));
    expect(props.onAttach).toHaveBeenCalledWith({ inline: { hosts: ["api.acme.test"] }, name: "Acme API" });
  });

  it("offers no plain-HTTP control, and no shared choice, for a header secret (D6)", async () => {
    const { user } = open();
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    expect(screen.queryByText(/plain http|without tls|unencrypted/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(screen.queryByText(/shared|provided by your admin/i)).not.toBeInTheDocument();
  });

  it("a refused add names the field, focuses it and drops nothing the person typed", async () => {
    const { props, user } = open();
    await type(user, /^Name/, "Acme API");
    await type(user, /^Hosts it reaches/, "10.0.0.5\napi.acme.test");
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    await type(user, /^Stored secret name/, "acme-key");
    await user.click(screen.getByRole("button", { name: "Environment variable" }));
    await type(user, /^Variable name/, "PATH");
    await user.click(screen.getByRole("button", { name: "Add to this run" }));

    // Both fields are named, each beside its own control.
    const hosts = screen.getByLabelText(/^Hosts it reaches/);
    expect(hosts).toHaveAttribute("aria-invalid", "true");
    expect(document.getElementById(hosts.getAttribute("aria-describedby")!.split(" ").find((i) => i.endsWith("-error"))!)).toHaveTextContent(
      /"10\.0\.0\.5" must be a DNS name, not an IP address/,
    );
    const variable = screen.getByLabelText(/^Variable name/);
    expect(variable).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText(/"PATH" is reserved/)).toBeInTheDocument();

    // Nothing was sent or closed, and every typed value is still there.
    expect(props.onAttach).not.toHaveBeenCalled();
    expect(props.onClose).not.toHaveBeenCalled();
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Acme API");
    expect(hosts).toHaveValue("10.0.0.5\napi.acme.test");
    expect(screen.getByLabelText(/^Stored secret name/)).toHaveValue("acme-key");
    expect(variable).toHaveValue("PATH");
    await waitFor(() => expect(hosts).toHaveFocus());
  });
});

describe("CustomComponentDialog: env and file delivery switch", () => {
  it("hides both when the deployment refuses them", async () => {
    const { user } = open({ residentAllowed: false });
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    expect(screen.getByRole("button", { name: "Request header" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Environment variable" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "File" })).not.toBeInTheDocument();
  });

  it("offers all three when it allows them", async () => {
    const { user } = open();
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    for (const n of ["Request header", "Environment variable", "File"]) {
      expect(screen.getByRole("button", { name: n })).toBeInTheDocument();
    }
  });
});

describe("CustomComponentDialog: save to reuse", () => {
  const fill = async (user: ReturnType<typeof userEvent.setup>) => {
    await user.click(screen.getByRole("button", { name: "Save to reuse" }));
    await type(user, /^Name/, "Acme");
    await type(user, /^Hosts it reaches/, "api.acme.test");
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    await type(user, /^Stored secret name/, "acme-key");
  };

  it("POSTs the definition, attaches the saved row by id and closes when nothing is missing", async () => {
    fetchMock.mockResolvedValue(json(201, { ...SAVED, requirements: [{ kind: "secret", name: "acme-key", status: "present" }] }));
    const { props, user } = open();
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
    expect(calls()).toEqual([
      {
        path: "/api/v1/me/components",
        method: "POST",
        body: {
          name: "Acme",
          definition: {
            hosts: ["api.acme.test"],
            secrets: [{ secret_name: "acme-key", delivery: { mode: "header", host: "api.acme.test" } }],
          },
        },
      },
    ]);
    expect(props.onAttach).toHaveBeenCalledWith({ id: SAVED.id });
    expect(props.onSaved).toHaveBeenCalledWith(expect.objectContaining({ id: SAVED.id }));
  });

  it("renders the answer's missing secrets with a link to add them (D18)", async () => {
    fetchMock.mockResolvedValue(
      json(201, {
        ...SAVED,
        requirements: [
          { kind: "secret", name: "acme-key", status: "missing", fix: "add_secret" },
          { kind: "secret", name: "other-key", status: "present" },
        ],
      }),
    );
    const { props, user } = open({ secretsPath: "/admin/secrets" });
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));

    const needs = await screen.findByTestId("custom-component-needs");
    expect(screen.getByText("Needs input before a run can launch")).toBeInTheDocument();
    expect(within(needs).getByText("Secret acme-key isn't stored yet.")).toBeInTheDocument();
    expect(within(needs).queryByText(/other-key/)).not.toBeInTheDocument();
    expect(within(needs).getByRole("link", { name: "Add it on the Secrets page" })).toHaveAttribute("href", "/admin/secrets");
    // It stays open until the person is done, and the row is already on the run.
    expect(props.onClose).not.toHaveBeenCalled();
    expect(props.onAttach).toHaveBeenCalledWith({ id: SAVED.id });
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(props.onClose).toHaveBeenCalled();
  });

  it("a server refusal names the field it is about and keeps the whole form", async () => {
    fetchMock.mockResolvedValue(
      json(422, {
        error: "invalid component: definition.secrets[0].secret_name: \"acme-key\" is managed by Wardyn",
        reason: "component_definition_invalid",
      }),
    );
    const { props, user } = open();
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));

    const field = screen.getByLabelText(/^Stored secret name/);
    await waitFor(() => expect(field).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByText(/"acme-key" is managed by Wardyn/)).toBeInTheDocument();
    expect(field).toHaveValue("acme-key");
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Acme");
    expect(screen.getByLabelText(/^Hosts it reaches/)).toHaveValue("api.acme.test");
    expect(props.onClose).not.toHaveBeenCalled();
    expect(props.onAttach).not.toHaveBeenCalled();
  });

  it("a name already in use belongs to the name field", async () => {
    fetchMock.mockResolvedValue(json(409, { error: "you already have a component with that name", reason: "component_name_conflict" }));
    const { user } = open();
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.getByLabelText(/^Name/)).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByText("you already have a component with that name")).toBeInTheDocument();
  });

  it("a saved component must be named before anything is sent", async () => {
    const { user } = open();
    await user.click(screen.getByRole("button", { name: "Save to reuse" }));
    await type(user, /^Hosts it reaches/, "api.acme.test");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByLabelText(/^Name/)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Give it a name.")).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe("CustomComponentDialog: editing a saved component", () => {
  it("PUTs to its own id and does not attach it to a run", async () => {
    fetchMock.mockResolvedValue(json(200, { ...SAVED, name: "Acme v2", version: 2 }));
    const { props, user } = open({ context: "saved", component: SAVED, onAttach: undefined });
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Acme");
    expect(screen.getByLabelText(/^Hosts it reaches/)).toHaveValue("api.acme.test");
    await type(user, /^Name/, "Acme v2");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
    expect(calls()).toEqual([
      { path: `/api/v1/me/components/${SAVED.id}`, method: "PUT", body: { name: "Acme v2", definition: { hosts: ["api.acme.test"] } } },
    ]);
  });

  it("has no this-run-only choice: it always saves", () => {
    open({ context: "saved", component: SAVED });
    expect(screen.queryByRole("button", { name: "For this run only" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
  });
});
