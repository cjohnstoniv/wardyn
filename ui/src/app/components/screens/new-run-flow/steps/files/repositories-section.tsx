/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Repositories: every repository of the workspaces the person may launch, each
// included on its own, at its own branch, at the path the server fixes. A
// repository the person has not onboarded is added from Workspaces.

import * as React from "react";
import { Link } from "react-router-dom";
import { Checkbox } from "../../../../ui/checkbox";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { excludeRepo, includeRepo, repoIncluded, setRepoRef, setStartsHere, startsAt } from "../../lenses/files-lens";
import { useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { BranchPicker } from "./branch-picker";
import type { RepoRow } from "./files-seams";
import { StartsHere } from "./starts-here";

function Row({ row, path, startsId }: { row: RepoRow; path?: string; startsId?: string }) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const nameId = React.useId();
  const entry = draft.intent.workspaces?.find((w) => w.workspace_id === row.workspaceId);
  const included = repoIncluded(draft.intent, row.workspaceId, row.repo);
  const ref = entry?.repos?.find((r) => r.repo === row.repo)?.ref;
  const edit = (apply: (i: typeof draft.intent) => typeof draft.intent) => dispatch({ type: "edit", apply });

  return (
    <li className="flex flex-wrap items-center gap-x-4 gap-y-2 py-3">
      <Checkbox
        checked={included}
        aria-label={NR_FILES.INCLUDE_ARIA(row.name)}
        onCheckedChange={(on) => edit((i) => (on === true ? includeRepo(i, row.workspaceId, row.repo) : excludeRepo(i, row.workspaceId, row.repo, row.siblings)))}
      />
      <span id={nameId} className="font-medium">{row.name}</span>
      <Chip>{NR_FILES.REPOSITORY_BADGE}</Chip>
      {path && <code className="font-mono text-xs text-muted-foreground">{path}</code>}
      <div className="ml-auto flex flex-wrap items-center gap-3">
        <BranchPicker
          workspaceId={row.workspaceId}
          repo={row.repo}
          name={row.name}
          included={included}
          sourceRef={row.ref}
          value={ref}
          typed={draft.guided.refText[row.key] ?? ref ?? ""}
          onPick={(next) => edit((i) => setRepoRef(i, row.workspaceId, row.repo, next, row.siblings))}
          onType={(text) => {
            dispatch({ type: "guided", patch: { refText: { ...draft.guided.refText, [row.key]: text } } });
            edit((i) => setRepoRef(i, row.workspaceId, row.repo, text.trim() || undefined, row.siblings));
          }}
        />
        <StartsHere
          nameId={nameId}
          id={startsId}
          included={included}
          checked={included && startsAt(draft.intent, row.workspaceId, row.repo, row.siblings.length)}
          onSelect={() => edit((i) => setStartsHere(i, row.workspaceId, row.repo))}
        />
      </div>
    </li>
  );
}

export function RepositoriesSection({ rows, pathOf, startsFocusKey, focusSelf }: {
  rows: readonly RepoRow[];
  /** The path the run's own preflight gives a row, falling back to the workspace's `clone_target`. */
  pathOf: (row: RepoRow) => string | undefined;
  /** The row whose Starts here radio a held Continue focuses. */
  startsFocusKey?: string;
  /** No row can take the failed-Continue focus (only a drive is included): the section does. */
  focusSelf?: boolean;
}) {
  return (
    <section id={focusSelf ? "nr-starts-here" : undefined} tabIndex={focusSelf ? -1 : undefined} aria-labelledby="nr-repos-title" className="flex flex-col gap-2 rounded-lg border border-border p-4">
      <div>
        <h2 id="nr-repos-title" className="font-semibold">{NR_FILES.REPOS_TITLE}</h2>
        <p className="text-sm text-muted-foreground">{NR_FILES.REPOS_BODY}</p>
      </div>
      <ul className="divide-y divide-border">
        {rows.map((row) => <Row key={row.key} row={row} path={pathOf(row)} startsId={row.key === startsFocusKey ? "nr-starts-here" : undefined} />)}
      </ul>
      <Link to="/workspaces" className="w-fit text-sm font-medium text-foreground underline underline-offset-4">{NR_FILES.ADD_REPOSITORY}</Link>
    </section>
  );
}
