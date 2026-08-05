/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";

const setSecretMock = vi.fn();
vi.mock("../../../lib/api/sources", () => ({
  sourcesApi: { listSources: () => Promise.resolve([]) },
}));
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { setSecret: (...a: unknown[]) => setSecretMock(...a) },
}));

import { StepSources } from "./step-sources";
import { newSourceRow, removeSource, seedFloor, type SourceRow, type WorkspaceSourceKind } from "./wizard-types";
import { C, V2C } from "../../../lib/workspace-copy";

// A thin stateful wrapper standing in for the slice of wizard.tsx's state
// StepSources is a controlled view over — wires the same pure helpers
// wizard.tsx itself uses (wizard-types.ts), so this exercises the real
// remove/re-seed contract, not a test-only reimplementation of it.
function Harness({
  initialSources,
  initialSecretNames = [],
  githubApp = false,
}: {
  initialSources?: SourceRow[];
  initialSecretNames?: string[];
  githubApp?: boolean;
}) {
  const [name, setName] = React.useState("");
  const [sources, setSources] = React.useState<SourceRow[]>(initialSources ?? seedFloor());
  const [secretNames, setSecretNames] = React.useState<string[]>(initialSecretNames);
  return (
    <StepSources
      name={name}
      onNameChange={setName}
      sources={sources}
      onAddSource={(type: WorkspaceSourceKind) => setSources((prev) => [...prev, newSourceRow(type)])}
      onAttachLibrarySource={() => {}}
      onUpdateSource={(id, patch) => setSources((prev) => prev.map((r) => (r.id === id ? { ...r, ...patch } : r)))}
      onRemoveSource={(id) => setSources((prev) => removeSource(prev, id))}
      secretNames={secretNames}
      githubApp={githubApp}
      onSecretStored={(n) => setSecretNames((prev) => [...prev, n])}
    />
  );
}

beforeEach(() => {
  setSecretMock.mockReset();
  setSecretMock.mockResolvedValue(undefined);
});

describe("StepSources — the ephemeral floor", () => {
  it("starts with one seeded ephemeral row whose remove button is disabled", () => {
    render(<Harness />);
    expect(screen.getByText(V2C.FLOOR)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /remove ephemeral directory/i })).toBeDisabled();
  });

  it("enables removal once a second source exists, and the floor returns when the last other source is removed", () => {
    render(<Harness />);
    // Add a local directory — now the floor is no longer alone.
    fireEvent.click(screen.getByRole("button", { name: /local directory/i }));
    expect(screen.getAllByTestId("source-row")).toHaveLength(2);
    expect(screen.getByRole("button", { name: /remove ephemeral directory/i })).not.toBeDisabled();

    // Remove the local dir we just added.
    fireEvent.click(screen.getByRole("button", { name: /remove local directory/i }));

    // Back to exactly one row, and it's the (re-seeded) floor again.
    expect(screen.getAllByTestId("source-row")).toHaveLength(1);
    expect(screen.getByText(V2C.FLOOR)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /remove ephemeral directory/i })).toBeDisabled();
  });
});

describe("StepSources — a composed multi-source workspace", () => {
  it("adds a local dir and a repo, accepts input into each, and auto-derives distinct mount targets", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: "Add Local directory" }));
    fireEvent.click(screen.getByRole("button", { name: "Add Repository" }));
    // Floor + local_dir + repo.
    expect(screen.getAllByTestId("source-row")).toHaveLength(3);

    const pathInput = screen.getByPlaceholderText("/home/me/projects/payments");
    fireEvent.change(pathInput, { target: { value: "/home/me/projects/payments" } });
    expect(pathInput).toHaveValue("/home/me/projects/payments");

    const sourceInput = screen.getByPlaceholderText("acme/payments-service");
    fireEvent.change(sourceInput, { target: { value: "acme/payments-service" } });
    expect(sourceInput).toHaveValue("acme/payments-service");

    // With 3 sources, each non-default target is derived from its own base
    // name rather than collapsing onto the same /home/agent/work.
    expect(screen.getByPlaceholderText("/home/agent/work/payments")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("/home/agent/work/payments-service")).toBeInTheDocument();
  });
});

describe("StepSources — the SSH hard gate blocks only its own row", () => {
  const rows: SourceRow[] = [
    { ...newSourceRow("repo"), source: "git@ghes.corp.internal:acme/payments.git" },
    { ...newSourceRow("repo"), source: "acme/other-repo" },
  ];

  it("gates the SSH-remote row with C.SSH_GATE while the plain-slug row stays fully usable", () => {
    render(<Harness initialSources={rows} />);
    expect(screen.getByText("SSH key needed first")).toBeInTheDocument();
    expect(screen.getByText(C.SSH_GATE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add ssh key/i })).toBeInTheDocument();

    // The other row has no gate, and its own Source input is present/editable.
    const otherSourceInput = screen.getByDisplayValue("acme/other-repo");
    expect(otherSourceInput).not.toBeDisabled();
    fireEvent.change(otherSourceInput, { target: { value: "acme/other-repo-renamed" } });
    expect(otherSourceInput).toHaveValue("acme/other-repo-renamed");
  });

  it("does not wedge the wizard — the ungated row can still be edited and removed while the gate is up", () => {
    render(<Harness initialSources={rows} />);
    // Two rows share the "Remove repository" label; removing either one must
    // work — the SSH gate on the other row must never disable this control.
    const removeButtons = screen.getAllByRole("button", { name: "Remove repository" });
    expect(removeButtons).toHaveLength(2);
    fireEvent.click(removeButtons[0]);
    expect(screen.getAllByTestId("source-row")).toHaveLength(1);
  });

  it("the gate clears once the ssh key is actually added through the dialog", async () => {
    render(<Harness initialSources={rows} />);
    expect(screen.getByText("SSH key needed first")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /add ssh key/i }));
    const dialog = screen.getByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText(/value/i), {
      target: { value: "-----BEGIN OPENSSH PRIVATE KEY-----" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /save secret/i }));

    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("ssh-key-ghes-corp-internal", expect.any(String)));
    await waitFor(() => expect(screen.queryByText("SSH key needed first")).not.toBeInTheDocument());
  });

  it("Add SSH key opens the locked, host-aware AddSecretDialog for the gated row's conventional name", () => {
    render(<Harness initialSources={rows} />);
    fireEvent.click(screen.getByRole("button", { name: /add ssh key/i }));
    const dialog = screen.getByRole("dialog");
    const nameInput = within(dialog).getByLabelText(/name/i) as HTMLInputElement;
    expect(nameInput).toHaveValue("ssh-key-ghes-corp-internal");
    expect(nameInput).toHaveAttribute("readonly");
  });
});
