/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { RunPolicySpec } from "../../lib/types";
import { FIELD_HELP } from "./policy-field-help";
import { GIT_PAT_SCOPE as C } from "./copy/git-pat";
import { GitPATSection } from "./policy-git-pat";

const PAT = { host: "gitlab.example.com", secret_name: "git-pat-gitlab-example-com" };
const BASE: RunPolicySpec = {
  allowed_domains: [],
  first_use_approval: "always_deny",
  min_confinement_class: "CC2",
  eligible_grants: [{ kind: "git_pat", scope: { ...PAT }, requires_approval: false }],
};

function Harness({
  initial,
  seen,
  serverError,
}: {
  initial: RunPolicySpec;
  seen: RunPolicySpec[];
  serverError?: string;
}) {
  const [spec, setSpec] = React.useState(initial);
  return (
    <GitPATSection
      spec={spec}
      serverError={serverError}
      onSpecChange={(next) => {
        seen.push(next);
        setSpec(next);
      }}
    />
  );
}

const last = (seen: RunPolicySpec[]) => seen[seen.length - 1].eligible_grants![0].scope;

describe("GitPATSection (packet M7)", () => {
  it("renders nothing without a git_pat grant", () => {
    const { container } = render(<Harness initial={{ ...BASE, eligible_grants: [] }} seen={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("names each grant and shows the lead and the three honesty lines", () => {
    render(<Harness initial={BASE} seen={[]} />);
    expect(screen.getByText(C.SECTION_TITLE)).toBeInTheDocument();
    expect(screen.getByText(C.SECTION_LEAD)).toBeInTheDocument();
    expect(screen.getByText(`${PAT.host} · ${PAT.secret_name}`)).toBeInTheDocument();
    expect(screen.getByText(C.HONESTY_TOKEN)).toBeInTheDocument();
    expect(screen.getByText(C.HONESTY_API)).toBeInTheDocument();
    expect(screen.getByText(C.HONESTY_BROKER)).toBeInTheDocument();
  });

  it("writes all four fields back into the grant's scope, and drops a key that returns to its default", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={BASE} seen={seen} />);

    await userEvent.selectOptions(screen.getByLabelText(C.FORGE), "gitlab");
    expect(last(seen)).toMatchObject({ ...PAT, forge: "gitlab" });

    const repos = screen.getByLabelText(C.REPOS);
    await userEvent.type(repos, "group/app{Enter}group/libs/*");
    expect(last(seen)).toMatchObject({ repos: ["group/app", "group/libs/*"] });

    await userEvent.click(screen.getByRole("button", { name: "Read-only" }));
    expect(last(seen)).toMatchObject({ access: "read" });

    await userEvent.click(screen.getByRole("checkbox", { name: C.API }));
    expect(last(seen)).toEqual({ ...PAT, forge: "gitlab", repos: ["group/app", "group/libs/*"], access: "read", api: true });

    await userEvent.click(screen.getByRole("button", { name: C.ACCESS_WRITE }));
    expect(last(seen)).not.toHaveProperty("access");
    await userEvent.clear(repos);
    expect(last(seen)).not.toHaveProperty("repos");
  });

  it("reads a stored narrowing back into the fields", () => {
    render(
      <Harness
        initial={{
          ...BASE,
          eligible_grants: [
            { kind: "git_pat", requires_approval: false, scope: { ...PAT, forge: "gitea", repos: ["a/b"], access: "read", api: true } },
          ],
        }}
        seen={[]}
      />,
    );
    expect(screen.getByLabelText(C.FORGE)).toHaveValue("gitea");
    expect(screen.getByLabelText(C.REPOS)).toHaveValue("a/b");
    expect(screen.getByRole("button", { name: "Read-only" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("checkbox", { name: C.API })).toBeChecked();
  });

  it("cannot enable the API on the generic forge, and says why", async () => {
    const seen: RunPolicySpec[] = [];
    render(<Harness initial={BASE} seen={seen} />);
    expect(screen.getByLabelText(C.FORGE)).toHaveValue("generic");
    const api = screen.getByRole("checkbox", { name: C.API });
    expect(api).toBeDisabled();
    expect(screen.getByText(C.API_NEEDS_FORGE)).toBeInTheDocument();
    await userEvent.click(api);
    expect(seen).toHaveLength(0);

    await userEvent.selectOptions(screen.getByLabelText(C.FORGE), "gitlab");
    expect(screen.getByRole("checkbox", { name: C.API })).toBeEnabled();
    expect(screen.getByText(C.API_HINT)).toBeInTheDocument();
  });

  it("switching back to the generic forge takes a stored api out", async () => {
    const seen: RunPolicySpec[] = [];
    render(
      <Harness
        initial={{
          ...BASE,
          eligible_grants: [{ kind: "git_pat", requires_approval: false, scope: { ...PAT, forge: "gitlab", api: true } }],
        }}
        seen={seen}
      />,
    );
    await userEvent.selectOptions(screen.getByLabelText(C.FORGE), "generic");
    expect(last(seen)).toEqual(PAT);
  });

  it("shows a stored empty repos list as none", () => {
    render(
      <Harness
        initial={{ ...BASE, eligible_grants: [{ kind: "git_pat", requires_approval: false, scope: { ...PAT, repos: [] } }] }}
        seen={[]}
      />,
    );
    expect(screen.getByText(C.REPOS_NONE)).toBeInTheDocument();
    expect(screen.queryByText(C.REPOS_HINT)).not.toBeInTheDocument();
  });

  it("shows stored repos with surrounding whitespace without re-rendering forever", () => {
    render(
      <Harness
        initial={{
          ...BASE,
          eligible_grants: [
            { kind: "git_pat", requires_approval: false, scope: { ...PAT, repos: [" group/app", "group/libs/* "] } },
          ],
        }}
        seen={[]}
      />,
    );
    expect(screen.getByLabelText(C.REPOS)).toHaveValue(" group/app\ngroup/libs/* ");
  });

  it("shows a value the editor would not write, instead of hiding it", () => {
    render(
      <Harness
        initial={{
          ...BASE,
          eligible_grants: [{ kind: "git_pat", requires_approval: false, scope: { ...PAT, forge: "mystery", access: "banana" } }],
        }}
        seen={[]}
      />,
    );
    expect(screen.getByLabelText(C.FORGE)).toHaveValue("mystery");
    expect(screen.getByRole("button", { name: "Read-only" })).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByRole("button", { name: C.ACCESS_WRITE })).toHaveAttribute("aria-pressed", "false");
  });

  it.each([
    ["repos", 'eligible_grants[0]: git_pat scope invalid: git_pat scope repos entry "a/../b" is malformed (non-empty)', "TEXTAREA"],
    ["access", 'eligible_grants[0]: git_pat scope invalid: git_pat scope access "x" is not one of read, write', "DIV"],
    ["forge", 'eligible_grants[0]: git_pat scope invalid: git_pat scope forge "x" is not one of generic, gitlab', "SELECT"],
    ["api", "eligible_grants[0]: git_pat scope api: true on forge bitbucket_server needs the flag, which is off", "BUTTON"],
  ])("shows a server refusal naming %s on that field", (_axis, message, element) => {
    render(
      <Harness
        initial={{
          ...BASE,
          eligible_grants: [{ kind: "git_pat", requires_approval: false, scope: { ...PAT, forge: "bitbucket_server", api: true } }],
        }}
        seen={[]}
        serverError={message}
      />,
    );
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(message.replace(/^eligible_grants\[0\]: /, ""));
    const invalid = document.querySelectorAll('[aria-invalid="true"]');
    expect(invalid).toHaveLength(1);
    const owner = invalid[0] as HTMLElement;
    expect(owner.tagName).toBe(element);
    expect(owner.getAttribute("aria-describedby")).toContain(alert.id);
  });

  it("puts the refusal on the grant it names only", () => {
    const two: RunPolicySpec = {
      ...BASE,
      eligible_grants: [
        { kind: "api_key", requires_approval: false, scope: { host: "x" } },
        { kind: "git_pat", requires_approval: false, scope: { ...PAT } },
      ],
    };
    const { rerender } = render(<GitPATSection spec={two} onSpecChange={() => {}} serverError="eligible_grants[1]: git_pat scope invalid: git_pat scope access &quot;x&quot;" />);
    expect(screen.getByRole("alert")).toBeInTheDocument();
    rerender(<GitPATSection spec={two} onSpecChange={() => {}} serverError="eligible_grants[0]: git_pat scope invalid: git_pat scope access &quot;x&quot;" />);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("an error that names no axis stays out of the fields", () => {
    render(<Harness initial={BASE} seen={[]} serverError='eligible_grants[0]: git_pat scope invalid: json: unknown field "repo"' />);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(document.querySelector('[aria-invalid="true"]')).toBeNull();
  });
});

describe("eligible_grants field help", () => {
  it("carries the honesty copy", () => {
    const v = FIELD_HELP.eligible_grants.values;
    expect(v).toContain(C.HONESTY_TOKEN);
    expect(v).toContain(C.HONESTY_API);
    expect(v).toContain(C.HONESTY_BROKER);
  });
});
