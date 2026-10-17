// Every save a person composes in the console names the person it was composed
// under, so a cookie another tab switched to someone else refuses the save
// instead of landing it on the wrong account. wfetch strips the option before
// sending, so the only place to pin it is the call sites: each module below
// must keep at least this many person-named writes.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, it } from "vitest";

const MIN_NAMED_WRITES: Record<string, number> = {
  "access.ts": 1, "branding.ts": 1, "components.ts": 3, "credential-delivery.ts": 1,
  "drives.ts": 3, "governance.ts": 3, "health.ts": 1, "key-domains.ts": 1,
  "model-provider-credentials.ts": 1, "model-provider-signin.ts": 1, "model-providers.ts": 1,
  "permissions.ts": 3, "run-layout.ts": 1, "secrets.ts": 1, "ssh-keys.ts": 1,
  "ado-pat.ts": 1, "me-drives.ts": 1, "people.ts": 1, "agent-providers.ts": 1,
  "workspaces.ts": 2, "providers.ts": 1, "policies.ts": 2, "user-types.ts": 2, "runs.ts": 1,
};

it.each(Object.entries(MIN_NAMED_WRITES))("%s keeps its saves person-named", (file, min) => {
  const src = readFileSync(join(__dirname, file), "utf8");
  expect(src.match(/asPerson: true/g)?.length ?? 0).toBeGreaterThanOrEqual(min);
});
