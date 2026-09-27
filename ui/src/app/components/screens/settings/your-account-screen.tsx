/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Your account (M-5, #636 — the settings split, §4.3): a person's OWN
// connections and keys, reached at /account. Today: the model connection
// (S-4 — whichever section is on main; #1042/MP-22 isn't merged yet, so
// that's still ModelProviderCard, the same shared component Admin Settings
// mounts for the org's shared credential), Azure DevOps (a personal
// connection, #386) and Your SSH keys.
//
// Nothing here belongs to the deployment — Host, the admin's Model providers
// list, Providers and User drives all stayed in Admin Settings
// (admin-settings-screen.tsx), and with Host gone this page has no consumer
// left for GET /site-config (operatorOnly, R1) — ModelProviderCard's own `ai`
// derivation never reads it (only the SCM/git rows do, deriveIntegrations.ts),
// so this screen simply never fetches it and passes `null`, same as a member
// caller always read it before (integrations.ts's own comment).
//
// /ssh-keys is gone (deleted with no alias); SshKeysPane is mounted here
// instead, exactly as it always rendered on this route.
import * as React from "react";
import { setup as setupApi } from "../../../lib/api/setup";
import type { SetupStatus } from "../../../lib/types";
import { PageHeader } from "../../wardyn/page-header";
import { YOUR_ACCOUNT } from "../../wardyn/copy/console-view";
import { ModelProviderCard } from "./connection-cards";
import { AdoConnectionCard } from "./ado-connection";
import { SshKeysPane } from "../ssh-keys";
import { ErrorState, TableSkeleton } from "../../wardyn/states";

export function YourAccountScreen() {
  const [state, setState] = React.useState<"loading" | "error" | "ready">("loading");
  const [status, setStatus] = React.useState<SetupStatus | null>(null);

  const load = React.useCallback(() => {
    setupApi
      .getSetupStatus()
      .then((s) => {
        setStatus(s);
        setState("ready");
      })
      .catch(() => setState("error"));
  }, []);
  React.useEffect(load, [load]);

  return (
    <div className="mx-auto w-full max-w-[900px] px-6 py-8">
      <PageHeader title="Your account" description={YOUR_ACCOUNT.LEDE} />
      {state === "loading" && <TableSkeleton />}
      {state === "error" && <ErrorState onRetry={load} />}
      {state === "ready" && status && (
        <div className="space-y-4">
          {/* S-4 (#636): mounts whichever model section is on main when M-5
              merges. #1042 (MP-22, "Your model connections") is still open,
              so this is today's ModelProviderCard — the SAME component Admin
              Settings mounts for the org's shared credential; the card's own
              per-caller branches (operator vs. a per_user bearer/SSO row)
              already tell the two apart. */}
          <ModelProviderCard status={status} siteConfig={null} onChanged={load} />
          {/* #386, Q9: a personal connection. Renders nothing with no Azure
              DevOps row configured. */}
          <AdoConnectionCard status={status} onChanged={load} />
          <SshKeysPane heading="h3" />
        </div>
      )}
    </div>
  );
}
