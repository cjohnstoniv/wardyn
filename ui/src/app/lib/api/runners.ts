/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runners management routes (internal/api/runner_inventory.go, runner_registration.go,
// runner_revoke.go): the administrators' inventory, a person's own runners, registration tokens, the
// claim, revoke and the runners switch. A refusal throws the HttpError of core.ts, so the console shows
// the server's own sentence and can match on `reason`.
import type {
  RunnerClaimRequest,
  RunnerFilter,
  RunnerRegistration,
  RunnerRegistrationToken,
  RunnerLimits,
  RunnerRevokeAnswer,
  RunnerSettingsRequest,
  RunnerTokenRequest,
  RunnerUpdateRequest,
  RunnerView,
} from "../types/runners";
import type { RunnerSettings } from "../types/site";
import { asJson, asNoContent, LIST_LIMIT, unwrapList, wfetch } from "./core";

const JSON_HEADERS = { "Content-Type": "application/json" };
const seg = (id: string) => encodeURIComponent(id);

/** The reason of the 403 a person's own routes answer while runners are off (internal/api/reasons_routes.go). */
export const RUNNERS_DISABLED = "runners_disabled";

// A paged list read whole: the routes page newest first and set X-Wardyn-Truncated while more remain.
async function readAll<T>(path: string): Promise<T[]> {
  const all: T[] = [];
  for (;;) {
    const res = await wfetch(`${path}${path.includes("?") ? "&" : "?"}limit=${LIST_LIMIT}&offset=${all.length}`, { method: "GET" });
    const page = unwrapList<T>(await asJson<unknown>(res));
    all.push(...page);
    if (page.length === 0 || res.headers.get("X-Wardyn-Truncated") !== "true") return all;
  }
}

const post = <T>(path: string, body?: unknown) =>
  wfetch(path, { method: "POST", headers: JSON_HEADERS, body: JSON.stringify(body ?? {}) }).then((r) => asJson<T>(r));

export const runners = {
  /** GET /runners?state= (security tier): every person's runners, owner named. Default filter is active. */
  list: (state: RunnerFilter = "active") => readAll<RunnerView>(`/runners?state=${state}`),
  get: (id: string) => wfetch(`/runners/${seg(id)}`, { method: "GET" }).then((r) => asJson<RunnerView>(r)),
  /** GET /me/runners: the caller's own runners, and any another person revoked this week. 403 `runners_disabled` while off. */
  listMine: () => readAll<RunnerView>("/me/runners"),
  /** POST /runners/tokens (super admin): a 72 h token for a named person. The answer carries the token once. */
  mint: (owner: string) => post<RunnerRegistrationToken>("/runners/tokens", { owner } satisfies RunnerTokenRequest),
  /** POST /me/runners/tokens: the caller's own one-hour token. */
  mintMine: () => post<RunnerRegistrationToken>("/me/runners/tokens"),
  /** POST /me/runners/{id}/claim: `fingerprint` is the one read on the person's own computer. */
  claim: (id: string, fingerprint: string) =>
    post<RunnerRegistration>(`/me/runners/${seg(id)}/claim`, { fingerprint } satisfies RunnerClaimRequest),
  /** PUT /me/runners/{id}: the owner declares its limits; a total past what the computer reports is refused 422. */
  updateMine: (id: string, limits: RunnerLimits) =>
    wfetch(`/me/runners/${seg(id)}`, { method: "PUT", headers: JSON_HEADERS, body: JSON.stringify({ limits } satisfies RunnerUpdateRequest) }).then((r) =>
      asJson<RunnerView>(r),
    ),
  /** DELETE /runners/{id} (security tier). */
  revoke: (id: string) => wfetch(`/runners/${seg(id)}`, { method: "DELETE" }).then((r) => asJson<RunnerRevokeAnswer>(r)),
  /** DELETE /me/runners/{id}; `notMine` marks a runner the person did not register, so security tooling can tell it apart. */
  revokeMine: (id: string, notMine = false) =>
    wfetch(`/me/runners/${seg(id)}${notMine ? "?reason=not_mine" : ""}`, { method: "DELETE" }).then((r) => asJson<RunnerRevokeAnswer>(r)),
  /** GET /runners/tokens (security tier): unused, unexpired tokens. A value is never listed. */
  tokens: () => readAll<RunnerRegistrationToken>("/runners/tokens"),
  revokeToken: (id: string) => wfetch(`/runners/tokens/${seg(id)}`, { method: "DELETE" }).then(asNoContent),
  settings: () => wfetch("/runners/settings", { method: "GET" }).then((r) => asJson<RunnerSettings>(r)),
  /** PUT /runners/settings (super admin). Turning on is refused 409 until the organisation URL is HTTPS. */
  setEnabled: (enabled: boolean) =>
    wfetch("/runners/settings", { method: "PUT", headers: JSON_HEADERS, body: JSON.stringify({ enabled } satisfies RunnerSettingsRequest) }).then((r) =>
      asJson<RunnerSettings>(r),
    ),
};
