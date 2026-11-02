/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Permissions row for the `inbound` feature value: who may declare a port callable from outside the run.
// The value is restricted (migration 0239), so nobody may until an allow names them. The row says so and lists the
// allows; "Add a grant" opens the add-grant form with this value filled in.
import * as React from "react";
import { INBOUND_ACCESS as T, SELF_HOST } from "../../lib/permissions-copy";
import type { CapabilityGrant } from "../../lib/types";
import { Button } from "../ui/button";
import { Mono } from "../wardyn/code-block";
import { Chip } from "../wardyn/primitives";

export const INBOUND_VALUE = "inbound";

const SERVICE_PREFIX = "service:";

/** The name of the service a grant's subject is, or null: a service identity is a `user` subject spelled service:<name>. */
export function serviceName(g: { subject_type: string; subject: string }): string | null {
  return g.subject_type === "user" && g.subject.startsWith(SERVICE_PREFIX) ? g.subject.slice(SERVICE_PREFIX.length) : null;
}

const isAllow = (g: CapabilityGrant) => g.capability === "feature" && g.value === INBOUND_VALUE && g.effect === "allow";

export function InboundAccessRow({
  grants,
  disabled,
  typeName,
  labels,
  onAddGrant,
}: {
  grants: CapabilityGrant[];
  /** The viewer is not a security admin: the row reads, and Add a grant is off. */
  disabled: boolean;
  typeName: (id: string) => string;
  /** The chip word of each subject type. */
  labels: Record<string, string>;
  onAddGrant: () => void;
}) {
  const allows = grants.filter(isAllow);
  const everyone = allows.some((g) => g.subject_type === "all");
  return (
    <div className="grid grid-cols-[1fr_auto] items-start gap-4 border-t border-border px-6 py-4" data-testid="inbound-access-row">
      <div>
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-body font-medium text-foreground">{T.LABEL}</h3>
          <Chip tone={everyone ? "warning" : "neutral"} dot>
            {everyone ? SELF_HOST.EVERYONE_CHIP : SELF_HOST.BY_GRANT_CHIP}
          </Chip>
        </div>
        <p className="mt-0.5 text-xs text-muted-foreground">{T.BLURB}</p>
        {allows.length > 0 && (
          <p className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-body text-foreground">
            <span>{T.ALLOWED_FOR}</span>
            {allows.map((g, i) => {
              const service = serviceName(g);
              return (
                <React.Fragment key={g.id}>
                  {i > 0 && <span aria-hidden="true">·</span>}
                  <span className="inline-flex items-center gap-1.5">
                    <Chip tone="neutral">{service !== null ? T.SUBJECT_SERVICE : labels[g.subject_type] ?? g.subject_type}</Chip>
                    {g.subject_type === "all" ? null : g.subject_type === "user_type" ? <span>{typeName(g.subject)}</span> : <Mono>{service ?? g.subject}</Mono>}
                  </span>
                </React.Fragment>
              );
            })}
          </p>
        )}
        {disabled && <p className="mt-2 text-xs text-muted-foreground">{T.REQUIRES_ROLE}</p>}
      </div>
      <Button type="button" variant="outline" size="sm" disabled={disabled} onClick={onAddGrant}>
        {T.ADD_GRANT}
      </Button>
    </div>
  );
}

