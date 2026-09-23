/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The type editor — the in-place form the User types screen opens over its
// list, the same idiom the Governance profile editor uses (one `default`
// button on the screen at any moment, CONSOLE-RULES §6).
//
// It authors ONE saved object: name, description, priority. Ceiling and run
// limits are NOT authored here — design §2.2 reads them off the governance
// profile assigned to this type (a Governance concern, read-only on this
// screen) — and "What this type gets" is a computed grid, not a form field.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { userTypes as api } from "../../../lib/api/user-types";
import type { GovernanceSnapshot } from "../../../lib/api/governance";
import { getErrorMessage } from "../../../lib/format";
import { USER_TYPES as UT } from "../../../lib/user-types-copy";
import type { UserType } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Field } from "../../wardyn/form-primitives";
import { SafetyMeter } from "../../wardyn/safety-meter";
import { Note } from "../governance/display";
import { ExplainGrid } from "./explain-grid";

// The profile bound to this type, by governance's own priority rule (highest
// wins) — the exact ordering ResolveGovernanceProfile applies, so a type with
// two assignments (a bug elsewhere, not something this screen writes) is never
// misreported by picking the wrong one.
function boundProfile(snapshot: GovernanceSnapshot | null, typeID: string) {
  if (!snapshot) return undefined;
  const assignment = snapshot.assignments
    .filter((a) => a.subject_type === "user_type" && a.subject === typeID)
    .sort((a, b) => b.priority - a.priority)[0];
  if (!assignment) return undefined;
  return snapshot.profiles.find((p) => p.id === assignment.profile_id);
}

export function UserTypeEditor({
  type,
  disabled,
  governance,
  onCancel,
  onSaved,
}: {
  /** The type being edited, or null for a new one. */
  type: UserType | null;
  disabled: boolean;
  /** null while still loading — the Ceiling/run-limits and "What this type
   *  gets" sections render only for an EXISTING type once this has landed,
   *  never a confident-but-wrong read against an unloaded snapshot. */
  governance: GovernanceSnapshot | null;
  onCancel: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = React.useState(type?.name ?? "");
  const [description, setDescription] = React.useState(type?.description ?? "");
  const [priority, setPriority] = React.useState(type ? String(type.priority) : "0");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const input = {
        name: name.trim(),
        description: description.trim(),
        priority: Number(priority) || 0,
      };
      if (type) {
        await api.updateUserType(type.id, input);
      } else {
        await api.createUserType(input);
      }
      onSaved();
    } catch (e) {
      setError(e instanceof HttpError ? e.message : getErrorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="border-t border-border px-6 py-5" data-testid="user-type-editor">
      <h3 className="text-sm font-medium text-foreground">
        {type ? UT.EDIT_TITLE(type.name) : UT.ADD_TITLE}
      </h3>

      <div className="mt-4 grid gap-4 md:grid-cols-2">
        <Field label={UT.FIELD_NAME} htmlFor="user-type-name" required>
          <Input
            id="user-type-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={disabled}
            autoComplete="off"
            required
          />
        </Field>
        <Field label={UT.FIELD_PRIORITY} htmlFor="user-type-priority" hint={UT.PRIORITY_HINT}>
          <Input
            id="user-type-priority"
            type="number"
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
            disabled={disabled || !!type?.built_in}
            className="font-mono"
          />
        </Field>
      </div>

      <div className="mt-4">
        <Field label={UT.FIELD_DESCRIPTION} htmlFor="user-type-description" hint={UT.DESCRIPTION_HINT}>
          <Textarea
            id="user-type-description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            disabled={disabled}
            rows={2}
          />
        </Field>
      </div>

      {error && <Note tone="red">{error}</Note>}

      {/* Read-only: both sections exist only once the type is saved (a new,
          unsaved type has no assignment and nothing for Explain to answer
          about) — design §2.2/§2.6. */}
      {type && governance && (
        <section className="mt-6 border-t border-border pt-5">
          <h4 className="text-body font-medium text-foreground">{UT.CEILING_TITLE}</h4>
          <p className="mt-0.5 max-w-[82ch] text-body text-muted-foreground">{UT.CEILING_LEAD}</p>
          {(() => {
            const profile = boundProfile(governance, type.id);
            return profile ? (
              <>
                <p className="mt-2 text-body text-foreground">{UT.CEILING_PROFILE(profile.name)}</p>
                <div className="mt-3 max-w-md">
                  <SafetyMeter spec={profile.ceiling} />
                </div>
              </>
            ) : (
              <Note>{UT.CEILING_NONE}</Note>
            );
          })()}
        </section>
      )}

      {type && (
        <section className="mt-6 border-t border-border pt-5">
          <h4 className="text-body font-medium text-foreground">{UT.EXPLAIN_TITLE}</h4>
          <p className="mt-0.5 max-w-[82ch] text-body text-muted-foreground">{UT.EXPLAIN_LEAD}</p>
          <div className="mt-3">
            <ExplainGrid subject={type.id} />
          </div>
        </section>
      )}

      <div className="mt-4 flex gap-2">
        <Button onClick={save} disabled={disabled || saving || !name.trim()}>
          {saving ? <Loader2 className="size-4 animate-spin" /> : null}
          {UT.SAVE}
        </Button>
        <Button variant="outline" onClick={onCancel} disabled={saving}>
          {UT.CANCEL}
        </Button>
      </div>
    </div>
  );
}
