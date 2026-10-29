/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Bind namespace dialog (mock r16 T3-b): a pool, a Kubernetes namespace and a service identity. The help
// sentence is the safety claim, so it is shown here, not left to the docs. A refusal stays inline with the
// server's own sentence (a binding is a signed-in person's alone; a duplicate namespace on the pool is a 409).
import * as React from "react";
import { Loader2, TriangleAlert } from "lucide-react";
import { workloadBindings } from "../../../lib/api/workloads";
import { getErrorMessage } from "../../../lib/format";
import type { ServiceIdentity } from "../../../lib/types/service-identities";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { WORKLOAD_BINDINGS as B } from "../../wardyn/copy/workloads";
import { Field } from "../../wardyn/form-primitives";

export const servicePrincipal = (name: string) => `service:${name}`;

export function BindNamespaceDialog({
  pools,
  services,
  onClose,
  onBound,
}: {
  /** The organization pools a binding may name. */
  pools: { id: string; name: string }[];
  services: ServiceIdentity[];
  onClose: () => void;
  onBound: (namespace: string) => void;
}) {
  const [pool, setPool] = React.useState("");
  const [namespace, setNamespace] = React.useState("");
  const [service, setService] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || !pool || !service || !namespace.trim()) return;
    setBusy(true);
    setError("");
    try {
      const ns = namespace.trim();
      await workloadBindings.create({ pool, namespace: ns, service });
      onBound(ns);
    } catch (err) {
      setError(getErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogContent className="sm:max-w-md">
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>{B.DIALOG_TITLE}</DialogTitle>
          </DialogHeader>
          <Field label={B.POOL} htmlFor="bind-pool" required>
            <Select value={pool} onValueChange={setPool}>
              <SelectTrigger id="bind-pool" className="w-full">
                <SelectValue placeholder={pools.length ? B.POOL_PLACEHOLDER : B.NO_POOLS} />
              </SelectTrigger>
              <SelectContent>
                {pools.map((p) => (
                  <SelectItem key={p.id} value={p.id}>
                    {p.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <Field label={B.NAMESPACE} htmlFor="bind-namespace" required>
            <Input
              id="bind-namespace"
              required
              className="font-mono"
              autoComplete="off"
              spellCheck={false}
              value={namespace}
              onChange={(e) => setNamespace(e.target.value)}
            />
          </Field>
          <Field label={B.SERVICE} htmlFor="bind-service" required>
            <Select value={service} onValueChange={setService}>
              <SelectTrigger id="bind-service" className="w-full font-mono">
                <SelectValue placeholder={B.SERVICE_PLACEHOLDER} />
              </SelectTrigger>
              <SelectContent>
                {services.map((s) => (
                  <SelectItem key={s.name} value={s.name} className="font-mono">
                    {servicePrincipal(s.name)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle p-3 text-xs text-foreground">
            <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden />
            <span>{B.HELP}</span>
          </div>
          {error && (
            <p role="alert" className="text-sm text-danger">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" disabled={busy} onClick={onClose}>
              {B.CANCEL}
            </Button>
            <Button type="submit" disabled={busy || !pool || !service || !namespace.trim()}>
              {busy && <Loader2 className="size-3.5 animate-spin" />}
              {B.CONFIRM}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
