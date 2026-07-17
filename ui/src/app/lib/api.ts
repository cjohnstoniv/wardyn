/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Compatibility barrel for the Wardyn API client.
//
// The client is split by domain under ./api/*.ts (each exports a per-domain
// object: runs, approvals, policies, secrets, workspaces, audit, recordings,
// composer, harnessAuth, health, setup). PREFER importing the specific domain
// directly — `import { runs } from "../lib/api/runs"` — so a route that never
// touches, say, the composer drops that module (and its SSE code) from its
// chunk. The aggregate `api` object below is kept only for direct-unit tests
// (lib/api.*.test.ts) and any straggler consumer; because it is one object
// spanning every domain, importing IT pulls the whole client and defeats
// per-route dead-code elimination — so screens must NOT use it.
import { audit, egressFromAudit } from "./api/audit";
import { approvals } from "./api/approvals";
import { composer, resolveComposeWorkspace } from "./api/compose";
import { harnessAuth } from "./api/harness-auth";
import { health } from "./api/health";
import { policies } from "./api/policies";
import { recordings } from "./api/recordings";
import { runs } from "./api/runs";
import { secrets } from "./api/secrets";
import { setup } from "./api/setup";
import { workspaces } from "./api/workspaces";

export {
  getToken,
  setToken,
  onUnauthorized,
  probeAuth,
  HttpError,
} from "./api/core";
export { egressFromAudit } from "./api/audit";
export { resolveComposeWorkspace } from "./api/compose";
export {
  approvals,
  audit,
  composer,
  harnessAuth,
  health,
  policies,
  recordings,
  runs,
  secrets,
  setup,
  workspaces,
};

// Aggregate surface — every method flattened onto one object (method names are
// unique across domains, so the spread never collides). Back-compat only; see
// the module comment above for why screens should import per-domain instead.
export const api = {
  ...setup,
  ...runs,
  ...approvals,
  ...policies,
  ...secrets,
  ...workspaces,
  ...audit,
  ...recordings,
  ...composer,
  ...harnessAuth,
  ...health,
  // Not a REST method but historically hung off `api.*`; kept for callers that
  // reach it through the aggregate.
  egressFromAudit,
  resolveComposeWorkspace,
};
