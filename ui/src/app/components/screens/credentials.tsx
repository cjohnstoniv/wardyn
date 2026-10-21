/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin view → Credentials (design F-1, packet F §4/§5): who holds a
// credential for each model provider, and erasing a person's whole namespace
// for offboarding. Both admin tiers read and act (K5-A, GET
// /model-providers/credentials and DELETE /people/{principal}/credentials —
// internal/api/credential_inventory.go, /credential_erase.go); a member is
// refused at the route (the generic admin-view refusal covers it, same as
// every other /admin/* screen).
import * as React from "react";
import { Link } from "react-router-dom";
import { ChevronDown, KeyRound } from "lucide-react";
import { credentials as credentialsApi, type CredentialInventory, type CredentialRow } from "../../lib/api/credentials";
import { getErrorMessage, relativeTime, absoluteTime } from "../../lib/format";
import { useOperator, useSecurityOperator } from "../wardyn/operator-context";
import { Button } from "../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../ui/dropdown-menu";
import { Chip } from "../wardyn/primitives";
import { EmptyState, ErrorState, TableSkeleton, loadFailStatus, type ScreenStatus } from "../wardyn/states";
import { PageHeader } from "../wardyn/page-header";
import { PageContainer } from "../wardyn/page-container";
import { SECURITY_ONLY_REASON } from "../wardyn/copy";
import { MODEL_PROVIDERS } from "../../lib/model-providers-copy";
import { INVENTORY, KEY_DOMAINS } from "../wardyn/copy/credentials";
import { EraseDataDialog, type ErasePerson } from "./erase-data-dialog";
import { AdminMintedTokens } from "./credentials-minted-tokens";
import { KeyDomainsSection } from "./credentials-key-domains";

// The server's canon 503 (credInventoryNoMeta, internal/api/credential_inventory.go)
// — its own empty card, no Retry, distinguished from every other read failure.
const NO_META_ERROR =
  "This deployment's secret store keeps no credential metadata, so there is nothing to list.";

function storeLabel(store: string): string {
  return INVENTORY.STORE[store] ?? store;
}

// A person's contiguous run of rows, in the order the inventory listed them —
// erase works per PERSON, not per credential (packet F
// §4 a colfoot: "The table is grouped by person because erase works per
// person"). The Person and Erase cells render once per group, with a rowSpan.
interface PersonGroup {
  person: string;
  email?: string;
  rows: CredentialRow[];
}
function groupByPerson(rows: CredentialRow[]): PersonGroup[] {
  const order: string[] = [];
  const groups = new Map<string, PersonGroup>();
  for (const r of rows) {
    let g = groups.get(r.person);
    if (!g) {
      g = { person: r.person, email: r.email, rows: [] };
      groups.set(r.person, g);
      order.push(r.person);
    }
    g.rows.push(r);
  }
  return order.map((p) => groups.get(p)!);
}

// M5 S3: "{domain} · {source}" for a person's next key, or a danger chip when two groups disagree.
function KeyDomainCell({ row }: { row: CredentialRow }) {
  switch (row.key_domain_source) {
    case undefined:
      return null;
    case "conflict":
      return (
        <Chip tone="danger" title={KEY_DOMAINS.CONFLICT} srLabel={KEY_DOMAINS.CONFLICT}>
          {KEY_DOMAINS.CONFLICT_CHIP}
        </Chip>
      );
    default: {
      const source =
        row.key_domain_source === "user"
          ? KEY_DOMAINS.SOURCE_USER
          : row.key_domain_source === "group"
            ? KEY_DOMAINS.SOURCE_GROUP(row.key_domain_group ?? "")
            : row.key_domain_source === "all"
              ? KEY_DOMAINS.SOURCE_ALL
              : KEY_DOMAINS.SOURCE_DEFAULT;
      return row.key_domain ? (
        <>
          <span className="font-mono">{row.key_domain}</span> <span className="text-muted-foreground">· {source}</span>
        </>
      ) : null;
    }
  }
}

export function CredentialsScreen() {
  const operator = useOperator();
  // The by-scope erase (POST /people/{principal}/erasure) is the security tier's.
  const securityOperator = useSecurityOperator();
  const [inv, setInv] = React.useState<CredentialInventory | null>(null);
  const [screenStatus, setStatus] = React.useState<ScreenStatus | "no_meta">("loading");
  const [errorMessage, setErrorMessage] = React.useState<string | undefined>(undefined);
  const [filter, setFilter] = React.useState<string | null>(null);
  // The one erase dialog: opened for a row's person, or with no person (the header's search).
  const [erase, setErase] = React.useState<{ person: ErasePerson | null } | null>(null);

  const load = React.useCallback(() => {
    setStatus("loading");
    credentialsApi
      .listInventory()
      .then((d) => {
        setInv(d);
        setStatus("ready");
      })
      .catch((e) => {
        const msg = getErrorMessage(e);
        setErrorMessage(msg);
        setStatus(msg === NO_META_ERROR ? "no_meta" : loadFailStatus(e));
      });
  }, []);
  React.useEffect(load, [load]);

  const rows = inv?.credentials ?? [];
  const providerNames = React.useMemo(() => {
    const out: Record<string, string> = {};
    for (const r of inv?.credentials ?? []) if (r.provider_name) out[r.provider] = r.provider_name;
    return out;
  }, [inv]);
  const byProvider = inv?.counts.by_provider ?? {};
  const noProvidersConfigured = Object.keys(byProvider).length === 0;
  const filtered = filter ? rows.filter((r) => r.provider === filter) : rows;
  const stores = new Set(rows.map((r) => r.store));
  const mixedStores = stores.size > 1;
  // The column only exists on a deployment that has the key-domain service (the server then names a domain for every row).
  const hasKeyDomains = rows.some((r) => r.key_domain_source !== undefined);
  const groups = React.useMemo(() => groupByPerson(filtered), [filtered]);
  // The footer's own store name: the EXTERNAL store present, if
  // any — the "Stored in" column (mixedStores) is the per-row distinction;
  // the footer's job is only to say where that store's OWN audit trail lives.
  const footerStore: "local" | "vault" | "key_vault" = stores.has("azurekv")
    ? "key_vault"
    : stores.has("vaultkv")
      ? "vault"
      : "local";

  return (
    <PageContainer width="list">
      <PageHeader
        title={INVENTORY.TITLE}
        description={INVENTORY.LEDE}
        actions={
          <>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline">
                  {INVENTORY.MORE}
                  <ChevronDown className="size-3.5" aria-hidden />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => setErase({ person: null })}>
                  {securityOperator ? INVENTORY.ERASE_MENU_DATA : INVENTORY.ERASE_MENU_CREDS}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </>
        }
      />

      {screenStatus === "forbidden" ? (
        <p className="mt-6 text-sm text-muted-foreground">{SECURITY_ONLY_REASON}</p>
      ) : (
        <div className="overflow-hidden rounded-xl border border-border bg-card">
          {screenStatus === "loading" ? (
            <TableSkeleton rows={5} cols={5} />
          ) : screenStatus === "no_meta" ? (
            <EmptyState icon={KeyRound} title={INVENTORY.NO_META_TITLE} description={errorMessage} />
          ) : screenStatus === "error" ? (
            <ErrorState onRetry={load} />
          ) : noProvidersConfigured ? (
            <EmptyState
              icon={KeyRound}
              title={MODEL_PROVIDERS.EMPTY_TITLE}
              description={INVENTORY.NO_PROVIDERS_BODY}
              action={
                operator ? (
                  <Button variant="outline" size="sm" asChild>
                    <Link to="/admin/settings">{INVENTORY.OPEN_SETTINGS}</Link>
                  </Button>
                ) : undefined
              }
            />
          ) : rows.length === 0 ? (
            <EmptyState icon={KeyRound} title={INVENTORY.EMPTY_TITLE} description={INVENTORY.EMPTY_BODY} />
          ) : (
            <div className="p-4">
              <p className="text-xs text-muted-foreground">{INVENTORY.SUMMARY(inv!.counts.people, inv!.counts.credentials)}</p>
              <div className="mt-3 flex flex-wrap gap-1.5">
                <button
                  type="button"
                  onClick={() => setFilter(null)}
                  className={`rounded-full border px-2.5 py-0.5 text-xs ${
                    filter === null ? "border-primary text-foreground" : "border-border text-muted-foreground"
                  }`}
                >
                  {INVENTORY.FILTER_ALL}
                </button>
                {Object.entries(byProvider).map(([id, n]) => (
                  <button
                    key={id}
                    type="button"
                    onClick={() => setFilter(id)}
                    className={`rounded-full border px-2.5 py-0.5 text-xs ${
                      filter === id ? "border-primary text-foreground" : "border-border text-muted-foreground"
                    }`}
                  >
                    {providerNames[id] ?? id} · {n}
                  </button>
                ))}
              </div>

              <div className="mt-3 overflow-x-auto">
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead>{INVENTORY.COL_PERSON}</TableHead>
                      <TableHead>{INVENTORY.COL_PROVIDER}</TableHead>
                      <TableHead>{INVENTORY.COL_STATE}</TableHead>
                      {mixedStores && <TableHead>{INVENTORY.COL_STORE}</TableHead>}
                      <TableHead>{INVENTORY.COL_ADDED}</TableHead>
                      <TableHead>{INVENTORY.COL_LAST_USED}</TableHead>
                      {hasKeyDomains && <TableHead>{INVENTORY.COL_DOMAIN}</TableHead>}
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {groups.flatMap((g) =>
                      g.rows.map((r, i) => (
                        <TableRow key={`${r.person}-${r.provider}`}>
                          {i === 0 && (
                            <TableCell rowSpan={g.rows.length} className="whitespace-nowrap align-top font-medium">
                              {g.email || g.person}
                              {g.email && <span className="ml-1.5 text-xs text-muted-foreground">{g.person}</span>}
                            </TableCell>
                          )}
                          <TableCell>{providerNames[r.provider] ?? r.provider}</TableCell>
                          <TableCell>
                            <Chip
                              tone={r.state === "expired" ? "warning" : "neutral"}
                              title={r.state === "expired" ? INVENTORY.EXPIRED_HINT : undefined}
                            >
                              {r.state === "expired" ? INVENTORY.STATE_EXPIRED : INVENTORY.STATE_STORED}
                            </Chip>
                          </TableCell>
                          {mixedStores && <TableCell className="text-muted-foreground">{storeLabel(r.store)}</TableCell>}
                          <TableCell className="text-muted-foreground" title={absoluteTime(r.added_at)}>
                            {relativeTime(r.added_at)}
                          </TableCell>
                          <TableCell
                            className="text-muted-foreground"
                            title={r.last_used_at ? absoluteTime(r.last_used_at) : undefined}
                          >
                            {r.last_used_at ? relativeTime(r.last_used_at) : INVENTORY.NEVER_USED}
                          </TableCell>
                          {i === 0 && hasKeyDomains && (
                            <TableCell rowSpan={g.rows.length} className="whitespace-nowrap align-top">
                              <KeyDomainCell row={r} />
                            </TableCell>
                          )}
                          {i === 0 && (
                            <TableCell rowSpan={g.rows.length} className="align-top">
                              <Button
                                size="sm"
                                variant="outline"
                                onClick={() => setErase({ person: { principal: g.person, label: g.email || g.person } })}
                              >
                                {INVENTORY.ERASE_ROW}
                              </Button>
                            </TableCell>
                          )}
                        </TableRow>
                      )),
                    )}
                  </TableBody>
                </Table>
              </div>

              <p className="mt-3 text-xs text-muted-foreground">
                {footerStore === "local" ? INVENTORY.FOOTER_LOCAL : INVENTORY.FOOTER(footerStore)}{" "}
                <Link to="/admin/audit" className="text-info hover:underline">
                  {INVENTORY.OPEN_AUDIT}
                </Link>
              </p>
            </div>
          )}
        </div>
      )}

      {/* Further admin-only reads: only once the screen has resolved as allowed. */}
      {screenStatus !== "forbidden" && screenStatus !== "loading" && <KeyDomainsSection />}
      {screenStatus !== "forbidden" && screenStatus !== "loading" && <AdminMintedTokens />}

      <EraseDataDialog
        open={erase !== null}
        person={erase?.person}
        onOpenChange={(open) => !open && setErase(null)}
        onErased={load}
      />
    </PageContainer>
  );
}
