/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS parity for the constants and sentences the console mirrors from the server, read as TEXT from the Go source
// (wire-parity.test.ts does the same for the DTO shapes): the connection re-check sentence, the New Run app-name rule
// and the kept machine's closed sets.

import { it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { KEPT_START_FAILURES } from "../../components/screens/run-detail/widgets/machine-card";
import { CONNECTIONS } from "../../components/wardyn/copy/door";
import { APP_NAME_RE } from "../../components/screens/new-run-flow/lenses/apps-lens";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

it("connection re-check sentence matches the server", () => {
  const src = readFileSync(join(repoRoot(), "internal/api/provider_connection_changes.go"), "utf8");
  const sentence = src.match(/const providerAccessRecheck = ("[^"\n]*")/);
  expect(sentence).not.toBeNull();
  expect(JSON.parse(sentence![1])).toBe(CONNECTIONS.STORE_UNREADABLE);
  expect(CONNECTIONS.STORE_UNREADABLE).toContain("Your credential may still be stored");
});

it("the New Run app-name rule is the server's uiAppNameRE", () => {
  const src = readFileSync(join(repoRoot(), "internal/api/policy.go"), "utf8");
  const m = /var uiAppNameRE = regexp\.MustCompile\(`([^`]+)`\)/.exec(src);
  expect(m).not.toBeNull();
  expect(APP_NAME_RE.source).toBe(m![1]);
});

// The kept machine's state and the reason a delete is under way are closed sets the console words each of.
it("KeptMachineState and KeptMachineDeleteReason are the Go constants", () => {
  const root = repoRoot();
  const goState = readFileSync(join(root, "internal/types/kept_machine.go"), "utf8");
  const states = [...goState.matchAll(/KeptMachineState = "([a-z_]+)"/g)].map((m) => m[1]);
  const ts = readFileSync(join(root, "ui/src/app/lib/types/kept-machine.ts"), "utf8");
  const union = (name: string) => [...(new RegExp(`type ${name} = ([^;]+);`).exec(ts)?.[1] ?? "").matchAll(/"([a-z_]+)"/g)].map((m) => m[1]);
  expect(new Set(union("KeptMachineState"))).toEqual(new Set(states));
  const goReasons = readFileSync(join(root, "pkg/client/machines.go"), "utf8");
  const reasons = [...goReasons.matchAll(/KeptMachineDeletedBy\w+\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
  expect(new Set(union("KeptMachineDeleteReason"))).toEqual(new Set(reasons));
});

// The two sentences a Mac's runner fails a run with when its kept machine did not start: the Machine tile shows the run's
// failure line when it carries one of them.
it("the Machine tile's start failures are the runner's sentences", () => {
  const src = readFileSync(join(repoRoot(), "internal/runner/docker/macvm_kept.go"), "utf8");
  const said = (name: string) => new RegExp(`${name}\\s*=\\s*"([^"]+)"`).exec(src)?.[1];
  expect(said("keptSaidLocked")).toContain(KEPT_START_FAILURES[0]);
  expect(said("keptSaidTools")).toContain(KEPT_START_FAILURES[1]);
});
