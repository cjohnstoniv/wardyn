/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy editor's "Git access tokens" section — repos, access, api and forge
// on each git_pat grant, one block per grant, the same shape as ToolRulesSection:
// it reads and writes the one document the textarea shows, so there is no second
// source of truth. Layout and copy are packet M7's (approved 2026-10-03); the
// strings are GIT_PAT_SCOPE, character for character.
//
// Every axis the server accepts is shown, including a value the editor would not
// write itself (an unknown forge, an access that is neither word): a field the
// editor hid while the server accepted it would let a pasted JSON policy carry
// it unseen. A refusal that names one axis of one grant (lib/git-pat-scope.ts's
// patAxisError) lands on that field, with aria-invalid and the server's sentence.
import * as React from "react";
import type { RunPolicySpec } from "../../lib/types";
import { gitPATGrants, patAxisError, readPATScope, type PATAxis } from "../../lib/git-pat-scope";
import { Checkbox } from "../ui/checkbox";
import { Textarea } from "../ui/textarea";
import { cn } from "../ui/utils";
import { Segmented } from "../screens/permissions";
import { SUMMARY } from "../screens/run-detail/policy-tab-copy";
import { GIT_PAT_SCOPE as C } from "./copy/git-pat";
import { SectionLabel } from "./primitives";

const FORGES = Object.keys(C.FORGE_OPTIONS);
const GENERIC = "generic";

// Write one grant's scope back into the spec. An edit that returns to a key's
// omission default drops the key, so a policy that never narrowed stays as short
// as it was.
function withScope(spec: RunPolicySpec, index: number, edit: (scope: Record<string, unknown>) => void): RunPolicySpec {
  const grants = (spec.eligible_grants ?? []).map((g, i) => {
    if (i !== index) return g;
    const scope = { ...(g.scope ?? {}) };
    edit(scope);
    return { ...g, scope };
  });
  return { ...spec, eligible_grants: grants };
}

const splitRepos = (text: string) =>
  text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

// The textarea's text is held here rather than derived from the list each
// render: splitting on every keystroke would drop a trailing newline the person
// just typed. It is re-seeded only when the list changed from outside.
function ReposField({
  id,
  repos,
  invalid,
  describedBy,
  onChange,
}: {
  id: string;
  repos: string[] | undefined;
  invalid: boolean;
  describedBy: string;
  onChange: (next: string[] | undefined) => void;
}) {
  const joined = (repos ?? []).join("\n");
  const [text, setText] = React.useState(joined);
  if (splitRepos(text).join("\n") !== joined) setText(joined);
  return (
    <Textarea
      id={id}
      value={text}
      rows={3}
      spellCheck={false}
      aria-invalid={invalid || undefined}
      aria-describedby={describedBy}
      className="font-mono text-xs"
      onChange={(e) => {
        setText(e.target.value);
        const list = splitRepos(e.target.value);
        onChange(list.length === 0 ? undefined : list);
      }}
    />
  );
}

function FieldError({ id, text }: { id: string; text?: string }) {
  if (!text) return null;
  return (
    <p id={id} role="alert" className="mt-1 text-xs text-danger">
      {text}
    </p>
  );
}

export function GitPATSection({
  spec,
  onSpecChange,
  serverError,
}: {
  spec: RunPolicySpec;
  onSpecChange: (next: RunPolicySpec) => void;
  /** The editor's last save refusal. One that names an axis of a grant is shown on that field. */
  serverError?: string | null;
}) {
  const uid = React.useId();
  const grants = gitPATGrants(spec.eligible_grants);
  if (grants.length === 0) return null;
  const refused = patAxisError(serverError);

  return (
    <div className="rounded-lg border border-border p-3">
      <SectionLabel>{C.SECTION_TITLE}</SectionLabel>
      <p className="mt-1 text-xs leading-snug text-muted-foreground">{C.SECTION_LEAD}</p>
      <div className="mt-2.5 space-y-3">
        {grants.map(({ index, grant }) => {
          const s = readPATScope(grant.scope);
          const base = `${uid}-pat-${index}`;
          const errorFor = (axis: PATAxis) => (refused?.index === index && refused.axis === axis ? refused.sentence : undefined);
          const errId = (axis: PATAxis) => `${base}-${axis}-error`;
          const describe = (axis: PATAxis, hintId?: string) =>
            [hintId, errorFor(axis) ? errId(axis) : undefined].filter(Boolean).join(" ") || undefined;
          const generic = s.forge === "" || s.forge === GENERIC;
          // A stored api: true on the generic forge can be unchecked but not
          // checked: the person can take it out, and cannot put it in.
          const apiLocked = generic && !s.api;
          const accessValue = s.access === "read" ? "read" : s.access === "" || s.access === "write" ? "write" : "";
          return (
            <fieldset key={index} className="space-y-3 rounded-lg border border-border p-3" data-testid="git-pat-block">
              <legend className="px-1 font-mono text-xs">
                {s.host} · {s.secretName}
              </legend>

              <div className="space-y-1.5">
                <label htmlFor={`${base}-forge`} className="text-sm font-medium">
                  {C.FORGE}
                </label>
                <select
                  id={`${base}-forge`}
                  value={generic ? GENERIC : s.forge}
                  aria-invalid={errorFor("forge") ? true : undefined}
                  aria-describedby={describe("forge", `${base}-forge-hint`)}
                  onChange={(e) =>
                    onSpecChange(
                      withScope(spec, index, (scope) => {
                        if (e.target.value === GENERIC) {
                          delete scope.forge;
                          delete scope.api; // the generic forge has no API door
                        } else scope.forge = e.target.value;
                      }),
                    )
                  }
                  className="border-input bg-input-background focus-visible:border-ring focus-visible:ring-ring aria-invalid:border-destructive dark:bg-input/30 block h-9 w-full max-w-xs rounded-md border px-2 text-body outline-none focus-visible:ring-[3px]"
                >
                  {FORGES.map((f) => (
                    <option key={f} value={f}>
                      {C.FORGE_OPTIONS[f]}
                    </option>
                  ))}
                  {!generic && !FORGES.includes(s.forge) && <option value={s.forge}>{s.forge}</option>}
                </select>
                <p id={`${base}-forge-hint`} className="text-xs leading-snug text-muted-foreground">
                  {C.FORGE_HINT}
                </p>
                <FieldError id={errId("forge")} text={errorFor("forge")} />
              </div>

              <div className="space-y-1.5">
                <label htmlFor={`${base}-repos`} className="text-sm font-medium">
                  {C.REPOS}
                </label>
                <ReposField
                  id={`${base}-repos`}
                  repos={s.repos}
                  invalid={!!errorFor("repos")}
                  describedBy={describe("repos", `${base}-repos-hint`) ?? ""}
                  onChange={(next) =>
                    onSpecChange(
                      withScope(spec, index, (scope) => {
                        if (next === undefined) delete scope.repos;
                        else scope.repos = next;
                      }),
                    )
                  }
                />
                <p id={`${base}-repos-hint`} className="text-xs leading-snug text-muted-foreground">
                  {s.repos?.length === 0 ? C.REPOS_NONE : C.REPOS_HINT}
                </p>
                <FieldError id={errId("repos")} text={errorFor("repos")} />
              </div>

              <div className="space-y-1.5">
                <span id={`${base}-access-label`} className="text-sm font-medium">
                  {C.ACCESS}
                </span>
                <div
                  role="group"
                  aria-labelledby={`${base}-access-label`}
                  aria-invalid={errorFor("access") ? true : undefined}
                  aria-describedby={describe("access")}
                  className={cn("w-fit rounded-lg", errorFor("access") && "ring-2 ring-destructive/40")}
                >
                  <Segmented
                    value={accessValue}
                    options={[
                      { value: "write", label: C.ACCESS_WRITE },
                      { value: "read", label: SUMMARY.readOnly },
                    ]}
                    onChange={(v) =>
                      onSpecChange(
                        withScope(spec, index, (scope) => {
                          if (v === "read") scope.access = "read";
                          else delete scope.access;
                        }),
                      )
                    }
                  />
                </div>
                <FieldError id={errId("access")} text={errorFor("access")} />
              </div>

              <div className="space-y-1.5">
                <div className="flex items-start gap-2.5">
                  <Checkbox
                    id={`${base}-api`}
                    className="mt-0.5"
                    checked={s.api}
                    disabled={apiLocked}
                    aria-invalid={errorFor("api") ? true : undefined}
                    aria-describedby={describe("api", `${base}-api-hint`)}
                    onCheckedChange={(v) =>
                      onSpecChange(
                        withScope(spec, index, (scope) => {
                          if (v === true) scope.api = true;
                          else delete scope.api;
                        }),
                      )
                    }
                  />
                  <div className="min-w-0">
                    <label htmlFor={`${base}-api`} className="text-sm font-medium">
                      {C.API}
                    </label>
                    <p id={`${base}-api-hint`} className="text-xs leading-snug text-muted-foreground">
                      {apiLocked ? C.API_NEEDS_FORGE : C.API_HINT}
                    </p>
                  </div>
                </div>
                <FieldError id={errId("api")} text={errorFor("api")} />
              </div>
            </fieldset>
          );
        })}
      </div>
      <div className="mt-2.5 space-y-1 text-xs leading-snug text-muted-foreground" data-testid="git-pat-honesty">
        <p>{C.HONESTY_TOKEN}</p>
        <p>{C.HONESTY_API}</p>
        <p>{C.HONESTY_BROKER}</p>
      </div>
    </div>
  );
}
