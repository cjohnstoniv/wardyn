/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Templates screen's reads and writes: GET /templates, the revision read a rename starts from, PUT,
// DELETE and the copy route that publishes to the organisation. Every route is a member route; the server
// checks the caller against the template's scope and answers the byte-identical 404 for one it hides.
import { asJson, asNoContent, wfetch } from "../../../lib/api/core";
import type { Template, TemplateList, TemplateSaveRequest } from "../../../lib/types/templates";

const path = (id: string) => `/templates/${encodeURIComponent(id)}`;

export const templatesApi = {
  list: async (): Promise<TemplateList> => asJson<TemplateList>(await wfetch("/templates")),

  /** The document of one revision: a rename sends back the revision the person saw, so a newer one conflicts. */
  getRevision: async (id: string, revision: number): Promise<Template> =>
    asJson<Template>(await wfetch(`${path(id)}/revisions/${revision}`)),

  update: async (id: string, req: TemplateSaveRequest): Promise<Template> =>
    asJson<Template>(await wfetch(path(id), { method: "PUT", asPerson: true, body: JSON.stringify(req) })),

  remove: async (id: string): Promise<void> => {
    await asNoContent(await wfetch(path(id), { method: "DELETE" }));
  },

  /** Publishes the template's current revision to the organisation (Admin view). */
  publishToOrg: async (id: string): Promise<Template> =>
    asJson<Template>(await wfetch(`${path(id)}/copy`, { method: "POST", asPerson: true, body: JSON.stringify({ scope: "org" }) })),
};
