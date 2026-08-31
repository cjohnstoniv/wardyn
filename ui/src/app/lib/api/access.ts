/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Access / role-mapping CRUD (0.7 SSO Phase 3) — the People step's four
// operatorOnly routes behind the acting surface. Mirrors internal/api/access.go;
// modeled on lib/api/permissions.ts's shape.
import type {
  AccessPostureFlipBody,
  AccessPreviewRequest,
  AccessPreviewResponse,
  AccessResponse,
  RoleMappingWriteInput,
} from "../types";
import { asJson, errText, HttpError, wfetch } from "./core";

// What an upsert actually did — same 201-created/200-updated split
// permissions.ts's GrantUpsert reports (access.go's handleUpsertRoleMapping
// compares the returned row's id against the one it built).
export interface RoleMappingUpsert {
  mapping: { id: string; value: string; role: string; created_by?: string; created_at?: string };
  created: boolean;
}

// Best-effort parse of a POST/DELETE 400 body as the structured posture-flip
// shape (access.go's accessPostureFlipBody) — required_acknowledgement is the
// discriminator; a plain {"error":"..."} (collision/lockout/email-refusal)
// parses fine as JSON but never carries that field, so it falls through as
// undefined rather than a wrongly-typed value.
async function parsePostureFlip(res: Response): Promise<AccessPostureFlipBody | null> {
  try {
    const body = (await res.clone().json()) as Partial<AccessPostureFlipBody>;
    return body.required_acknowledgement === true &&
      typeof body.before === "string" &&
      typeof body.after === "string"
      ? (body as AccessPostureFlipBody)
      : null;
  } catch {
    return null;
  }
}

// Thrown instead of a plain HttpError when a write 400s carrying the
// structured posture-flip body — carries before/after so access-panel.tsx can
// render the SAME parameterized GUARD copy it shows pre-emptively (from GET
// /access's own posture field), reactively, for the rare race where a second
// admin's write changed the map between this client's last fetch and this
// attempt.
export class AccessPostureFlipRequiredError extends HttpError {
  before: string;
  after: string;
  constructor(status: number, body: AccessPostureFlipBody) {
    super(status, body.error);
    this.name = "AccessPostureFlipRequiredError";
    this.before = body.before;
    this.after = body.after;
  }
}

async function throwAccessWriteError(res: Response): Promise<never> {
  const flip = await parsePostureFlip(res);
  if (flip) throw new AccessPostureFlipRequiredError(res.status, flip);
  throw new HttpError(res.status, await errText(res));
}

export const access = {
  // GET /api/v1/access -> the whole People-step data need in one call. A 503
  // (writeError, SSO not configured) surfaces as HttpError(503, "SSO is not
  // configured") — the caller discriminates on .status, per requireOIDC.
  async getAccess(): Promise<AccessResponse> {
    const res = await wfetch("/access", { method: "GET" });
    return asJson<AccessResponse>(res);
  },

  // POST /api/v1/access/mappings -> the saved row. A non-2xx response is
  // thrown as HttpError with the RAW server message (errText) — the caller
  // (access-panel.tsx) classifies it (posture-flip / email-refused /
  // collision / lockout) since the response body carries no discriminator
  // field for the plain-message cases.
  async upsertMapping(input: RoleMappingWriteInput): Promise<RoleMappingUpsert> {
    const res = await wfetch("/access/mappings", { method: "POST", body: JSON.stringify(input) });
    if (!res.ok) await throwAccessWriteError(res);
    const created = res.status === 201;
    return { mapping: await asJson<RoleMappingUpsert["mapping"]>(res), created };
  },

  // DELETE /api/v1/access/mappings/{id}?acknowledge_access_change=true — the
  // ack rides the QUERY STRING here, not a JSON body (access.go's
  // handleDeleteRoleMapping reads r.URL.Query(), unlike the POST route).
  async deleteMapping(id: string, acknowledgeAccessChange = false): Promise<void> {
    const qs = acknowledgeAccessChange ? "?acknowledge_access_change=true" : "";
    const res = await wfetch(`/access/mappings/${encodeURIComponent(id)}${qs}`, { method: "DELETE" });
    if (!res.ok) await throwAccessWriteError(res);
  },

  // POST /api/v1/access/preview -> a 200 always (a preview failure is an
  // OUTCOME — response.error — never a thrown HttpError; see
  // handlePreviewRole's own doc).
  async previewRole(input: AccessPreviewRequest): Promise<AccessPreviewResponse> {
    const res = await wfetch("/access/preview", { method: "POST", body: JSON.stringify(input) });
    return asJson<AccessPreviewResponse>(res);
  },
};
