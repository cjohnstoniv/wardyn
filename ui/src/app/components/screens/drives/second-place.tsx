/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where a drive mounts from (mock M10, #113 + #112): the label under Backend and
// the "Also reachable from" section. The label never refuses anything; it says
// what the backend allows. Only a share can have a second place, one per drive,
// and the only second place offered is a person's own runner. The server checks
// the runner and the path on save and answers with its own sentence.
//
// Every product string comes from user-drives-copy.ts. This file adds none.
import * as React from "react";
import { isManagedBackend, type DriveBackend } from "../../../lib/api/drives";
import { runners as runnersApi } from "../../../lib/api/runners";
import type { RunnerView } from "../../../lib/types/runners";
import { DRIVES } from "../../../lib/user-drives-copy";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Field } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { SELECT_CLASS } from "../new-run-flow/steps/tools/tool-dialog";
import { Note } from "./display";

/** A drive's second place as the editor holds it: a person's own runner and the share's path there. */
export interface SecondPlaceValue {
  runner: string;
  path: string;
}

/** The label under Backend: a share can mount from two places, a managed volume from one. */
export function PlacesLabel({ backend }: { backend: DriveBackend }) {
  const share = !isManagedBackend(backend);
  return (
    <span className="mt-2 flex flex-col items-start gap-1" data-testid="drive-places-label">
      <Chip tone="neutral">{share ? DRIVES.LABEL_TWO_PLACES : DRIVES.LABEL_ONE_PLACE}</Chip>
      <span className="text-meta text-muted-foreground">
        {share ? DRIVES.LABEL_TWO_PLACES_BODY : backend === "docker_volume" ? DRIVES.LABEL_ONE_PLACE_DOCKER : DRIVES.LABEL_ONE_PLACE_K8S}
      </span>
    </span>
  );
}

// The runners a second place can name: claimed runners of one person's own.
function useOwnRunners(open: boolean): RunnerView[] {
  const [list, setList] = React.useState<RunnerView[]>([]);
  React.useEffect(() => {
    if (!open) return;
    let current = true;
    runnersApi.list("active").then(
      (all) => current && setList(all.filter((r) => r.owner_kind === "person" && r.state === "claimed")),
      () => current && setList([]),
    );
    return () => { current = false; };
  }, [open]);
  return list;
}

export function SecondPlace({
  backend,
  value,
  onChange,
  adding,
  disabled,
}: {
  backend: DriveBackend;
  /** The second place, or null while the drive has one place. */
  value: SecondPlaceValue | null;
  onChange: (v: SecondPlaceValue | null) => void;
  /** The drive had no second place when the editor opened: the reach note shows. */
  adding: boolean;
  disabled: boolean;
}) {
  const runners = useOwnRunners(value !== null);
  if (isManagedBackend(backend)) {
    return <p className="text-body text-muted-foreground md:col-span-2">{DRIVES.SECOND_PLACE_SHARE_ONLY}</p>;
  }
  if (value === null) {
    return (
      <div className="md:col-span-2">
        <Button variant="outline" size="sm" onClick={() => onChange({ runner: "", path: "" })} disabled={disabled}>
          {DRIVES.SECOND_PLACE_ADD}
        </Button>
      </div>
    );
  }
  // A runner the list does not hold (the list could not be read) still shows as chosen.
  const options = runners.some((r) => r.id === value.runner) || value.runner === ""
    ? runners
    : [{ id: value.runner, name: value.runner } as RunnerView, ...runners];
  return (
    <section className="grid gap-4 rounded-md border border-border px-4 py-4 md:col-span-2 md:grid-cols-2" data-testid="drive-second-place">
      <div className="flex items-start justify-between gap-3 md:col-span-2">
        <div>
          <h4 className="text-sm font-medium text-foreground">{DRIVES.SECOND_PLACE_TITLE}</h4>
          <p className="mt-1 text-body text-muted-foreground">{DRIVES.SECOND_PLACE_LEAD}</p>
        </div>
        <Button variant="ghost" size="sm" onClick={() => onChange(null)} disabled={disabled}>
          {DRIVES.SECOND_PLACE_REMOVE}
        </Button>
      </div>
      <Field label={DRIVES.FIELD_WHERE}>
        <p className="text-body text-foreground">{DRIVES.WHERE_OWN_RUNNER}</p>
      </Field>
      <Field label={DRIVES.FIELD_RUNNER} htmlFor="drive-alt-runner" hint={DRIVES.RUNNER_HINT}>
        <select
          id="drive-alt-runner"
          className={SELECT_CLASS}
          value={value.runner}
          onChange={(e) => onChange({ ...value, runner: e.target.value })}
          disabled={disabled}
        >
          <option value="" disabled>{DRIVES.RUNNER_PLACEHOLDER}</option>
          {options.map((r) => (
            <option key={r.id} value={r.id}>{DRIVES.RUNNER_OPTION(r.name)}</option>
          ))}
        </select>
      </Field>
      <Field label={DRIVES.FIELD_ALT_HOST_ROOT} htmlFor="drive-alt-host-root" hint={DRIVES.ALT_HOST_ROOT_HINT}>
        <Input
          id="drive-alt-host-root"
          value={value.path}
          onChange={(e) => onChange({ ...value, path: e.target.value })}
          disabled={disabled}
          className="font-mono"
          autoComplete="off"
        />
      </Field>
      {adding && (
        <div className="md:col-span-2">
          <Note>{DRIVES.REACH_NOTE}</Note>
        </div>
      )}
    </section>
  );
}
