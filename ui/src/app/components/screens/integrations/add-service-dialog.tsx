/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add a service — the SEARCH-FIRST Add flow for the eight generic categories.
// Two panels: type what you're connecting (or browse the sections), then
// confirm where it lives, what credential it takes, and see how that credential
// will reach the request.
//
// The delivery mode is never a control. It follows from how the system
// authenticates: a header naming a stored secret is proxy-injected, and the two
// groups that authenticate outside HTTP honestly say "egress only" instead of
// showing a field that would do nothing.
import * as React from "react";
import { Search } from "lucide-react";
import {
  CATALOG_COPY,
  DELIVERY_META,
  INTEGRATION_GROUPS,
  integrationGroup,
  searchIntegrationTypes,
  typesInGroup,
  type IntegrationTypeMeta,
} from "../../../lib/integration-catalog";
import { genericIntegrationsApi } from "../../../lib/api/integrations";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Label } from "../../ui/label";
import { Textarea } from "../../ui/textarea";
import { Chip } from "../../wardyn/primitives";
import { Mono } from "../../wardyn/code-block";

/** A slug the API accepts as an integration id (secretNameRE: lowercase, dots/dashes/underscores). */
export function slugifyIntegrationId(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 128);
}

export function AddServiceDialog({
  open,
  onOpenChange,
  onAdded,
  onHandoff,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onAdded: () => void;
  /** Picking a model provider or a git host hands off to their own established
   *  flow — one Add button on the page, routed by what you picked, rather than
   *  two buttons an operator has to choose between before they know which. The
   *  picked TYPE travels with it: that flow must open on the thing that was
   *  clicked, never re-ask the coarser question this panel already answered. */
  onHandoff: (target: IntegrationTypeMeta) => void;
}) {
  const [picked, setPicked] = React.useState<IntegrationTypeMeta | null>(null);

  const pick = (t: IntegrationTypeMeta) => {
    if (t.addLane === "ai" || t.addLane === "scm") {
      onOpenChange(false);
      onHandoff(t);
      return;
    }
    setPicked(t);
  };

  // Reset to the search panel every time the dialog opens — a half-filled
  // second panel from a previous visit is never what someone wants next.
  React.useEffect(() => {
    if (open) setPicked(null);
  }, [open]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-[640px]">
        <DialogHeader>
          <DialogTitle>{picked ? `Connect ${picked.label}` : "Add integration"}</DialogTitle>
        </DialogHeader>
        {picked ? (
          <ConnectPanel
            type={picked}
            onBack={() => setPicked(null)}
            onSaved={() => {
              onOpenChange(false);
              onAdded();
            }}
          />
        ) : (
          <PickPanel onPick={pick} />
        )}
      </DialogContent>
    </Dialog>
  );
}

function PickPanel({ onPick }: { onPick: (t: IntegrationTypeMeta) => void }) {
  const [query, setQuery] = React.useState("");
  const results = searchIntegrationTypes(query);
  const browsing = query.trim() === "";

  return (
    <div className="space-y-4">
      <p className="text-[0.8125rem] leading-snug text-muted-foreground">{CATALOG_COPY.ADD_DESC}</p>
      <div className="relative">
        <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          autoFocus
          className="pl-8"
          placeholder={CATALOG_COPY.SEARCH_PH}
          aria-label="Search integration types"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
      </div>

      {browsing ? (
        <div className="max-h-[340px] space-y-3 overflow-y-auto">
          {INTEGRATION_GROUPS.map((group) => (
            <div key={group.id}>
              <p className="text-xs font-medium text-foreground">{group.label}</p>
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">{group.desc}</p>
              <div className="mt-1.5 flex flex-wrap gap-1.5">
                {typesInGroup(group.id).map((t) => (
                  <Button key={t.id} size="sm" variant="outline" onClick={() => onPick(t)}>
                    {t.label}
                  </Button>
                ))}
              </div>
            </div>
          ))}
        </div>
      ) : results.length === 0 ? (
        <p className="text-[0.8125rem] leading-snug text-muted-foreground">{CATALOG_COPY.SEARCH_NONE}</p>
      ) : (
        <div className="space-y-1.5">
          {results.map((t) => (
            <button
              key={t.id}
              type="button"
              className="flex w-full items-center gap-2 rounded-lg border border-border px-3 py-2 text-left hover:bg-muted/40"
              onClick={() => onPick(t)}
            >
              <span className="min-w-0 flex-1">
                <span className="block text-sm text-foreground">{t.label}</span>
                <span className="block text-[0.6875rem] text-muted-foreground">
                  {integrationGroup(t.group).label}
                  {t.hosts.length > 0 ? ` · ${t.hosts[0]}` : ""}
                </span>
              </span>
              <Chip tone={DELIVERY_META[t.delivery].tone}>{DELIVERY_META[t.delivery].label}</Chip>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function ConnectPanel({
  type,
  onBack,
  onSaved,
}: {
  type: IntegrationTypeMeta;
  onBack: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = React.useState(type.label);
  const [hosts, setHosts] = React.useState(type.hosts.join("\n"));
  const [secret, setSecret] = React.useState(type.secret ?? "");
  const [docs, setDocs] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const group = integrationGroup(type.group);
  // A type with no backend home is listed because people look for it — never
  // offered as an Add that cannot work.
  const unsupported = type.addLane === "unsupported";
  // Cloud providers and data stores authenticate outside HTTP; there is no
  // header for the proxy to add, so the row is egress-only and honest about it.
  const takesHeader = !!type.header && type.delivery !== "notbuilt";
  const hostList = hosts
    .split("\n")
    .map((h) => h.trim())
    .filter(Boolean);
  const delivery = DELIVERY_META[takesHeader && secret.trim() ? "proxy" : type.delivery];

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      await genericIntegrationsApi.put(slugifyIntegrationId(name), {
        name: name.trim(),
        category: group.category,
        type: type.apiType ?? type.id,
        hosts: hostList,
        ...(takesHeader && secret.trim()
          ? {
              header: type.header,
              format: type.format,
              credentials: { token: secret.trim() },
            }
          : {}),
        ...(docs.trim() ? { docs: docs.trim() } : {}),
      });
      onSaved();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="space-y-4">
      <p className="text-[0.8125rem] leading-snug text-muted-foreground">{type.lead ?? CATALOG_COPY.CONNECT_DESC}</p>

      {unsupported && (
        <div className="rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2.5">
          <p className="text-[0.8125rem] leading-snug text-warning">{type.note}</p>
        </div>
      )}

      <div className="space-y-1.5">
        <Label htmlFor="int-name">Name</Label>
        <Input id="int-name" value={name} onChange={(e) => setName(e.target.value)} />
        <p className="text-[0.6875rem] text-muted-foreground">
          Stored as <Mono className="text-[0.6875rem]">{slugifyIntegrationId(name) || "—"}</Mono>
        </p>
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="int-hosts">{CATALOG_COPY.HOSTS_HEAD}</Label>
        <Textarea
          id="int-hosts"
          rows={3}
          value={hosts}
          placeholder={type.hostPlaceholder}
          onChange={(e) => setHosts(e.target.value)}
        />
        <p className="text-[0.6875rem] leading-snug text-muted-foreground">{CATALOG_COPY.HOSTS_HINT}</p>
      </div>

      {takesHeader ? (
        <div className="space-y-1.5">
          <Label htmlFor="int-secret">{CATALOG_COPY.CRED_HEAD}</Label>
          <Input id="int-secret" value={secret} placeholder="secret name" onChange={(e) => setSecret(e.target.value)} />
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">
            Presented as <Mono className="text-[0.6875rem]">{type.header}</Mono>. {CATALOG_COPY.CRED_WRITEONLY}
          </p>
        </div>
      ) : (
        <div className="rounded-lg border border-border px-3 py-2.5">
          <p className="text-[0.8125rem] leading-snug text-foreground">
            {type.why ?? DELIVERY_META[type.delivery].line}
          </p>
        </div>
      )}

      <div className="space-y-1.5">
        <Label htmlFor="int-docs">Docs link</Label>
        <Input id="int-docs" value={docs} onChange={(e) => setDocs(e.target.value)} />
        <p className="text-[0.6875rem] text-muted-foreground">{CATALOG_COPY.DOCS_HINT}</p>
      </div>

      <div className="flex items-center gap-2 rounded-lg border border-border px-3 py-2.5">
        <Chip tone={delivery.tone}>{delivery.label}</Chip>
        <p className="min-w-0 flex-1 text-[0.6875rem] leading-snug text-muted-foreground">
          {CATALOG_COPY.DELIV_STATED}
        </p>
      </div>

      <p className="text-[0.6875rem] leading-snug text-muted-foreground">{CATALOG_COPY.STORE_NOTE}</p>
      {error && <p className="text-[0.8125rem] leading-snug text-danger">{error}</p>}

      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={onBack} disabled={saving}>
          Back
        </Button>
        <Button onClick={save} disabled={saving || unsupported || !name.trim() || hostList.length === 0}>
          {saving ? "Adding…" : "Add integration"}
        </Button>
      </div>
    </div>
  );
}
