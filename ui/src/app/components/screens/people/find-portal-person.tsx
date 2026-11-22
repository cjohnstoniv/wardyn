/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The exact lookup of a portal person, beside the ordinary search: the operator knows the provider's issuer
// and person ID, not the principal Wardyn made of them (GET /people/portal-person). A hit opens the drawer.
import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import { people as peopleApi } from "../../../lib/api/people";
import type { PersonSummary } from "../../../lib/types";
import { PEOPLE_PAGE as P } from "../../wardyn/copy/people";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { answeredSentence } from "../../wardyn/states";

// The two 404s mean different things to the operator: a mis-spelt issuer, or an ID no portal has introduced.
function notFoundSentence(e: unknown): string {
  if (e instanceof HttpError && e.reason === "portal_issuer_not_bound") return P.FIND_ISSUER_NOT_BOUND;
  if (e instanceof HttpError && e.reason === "person_not_found") return P.FIND_PERSON_NOT_FOUND;
  return answeredSentence(e, P.FIND_FAILED) ?? P.FIND_FAILED;
}

// intent is the screen's counter of what the operator last chose: every row open, every drawer dismissal and
// every lookup bumps it. A lookup that finishes after a later choice has been overtaken and changes nothing.
export function FindPortalPerson({ onFound, intent }: { onFound: (p: PersonSummary) => void; intent: React.MutableRefObject<number> }) {
  const [issuer, setIssuer] = React.useState("");
  const [personId, setPersonId] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const mine = ++intent.current;
    setBusy(true);
    setError("");
    try {
      const found = await peopleApi.portalPerson(issuer.trim(), personId.trim());
      const row = await peopleApi.get(found.principal);
      if (intent.current !== mine) return;
      if (row) onFound(row);
      else setError(P.FIND_NOT_LISTED);
    } catch (err) {
      if (intent.current === mine) setError(notFoundSentence(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2 border-b border-border p-3" aria-label={P.FIND_TITLE}>
      <div className="flex flex-wrap items-center gap-2">
        <Input
          aria-label={P.FIND_ISSUER}
          placeholder={P.FIND_ISSUER_PLACEHOLDER}
          className="max-w-sm"
          autoComplete="off"
          spellCheck={false}
          value={issuer}
          onChange={(e) => setIssuer(e.target.value)}
        />
        <Input
          aria-label={P.FIND_PERSON_ID}
          placeholder={P.FIND_PERSON_ID_PLACEHOLDER}
          className="max-w-xs"
          autoComplete="off"
          spellCheck={false}
          value={personId}
          onChange={(e) => setPersonId(e.target.value)}
        />
        <Button type="submit" variant="outline" disabled={busy || !issuer.trim() || !personId.trim()}>
          {busy ? P.FIND_BUSY : P.FIND_BUTTON}
        </Button>
      </div>
      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
    </form>
  );
}
