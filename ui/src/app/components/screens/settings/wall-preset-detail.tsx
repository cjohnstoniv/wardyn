/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What one preset narrows, knob by knob: the disclosure a built-in row opens (a built-in has no Edit, so this is how its
// knobs are read). A knob it leaves alone says so, so Read-only reads as what it leaves to the policy.
import type { PersonalWallsSpec } from "../../../lib/api/walls";
import { blockedTransfers, idleDisplay } from "../../../lib/wall-preset-words";
import { CC_META } from "../../wardyn/cc-meta";
import { NR_WALLS, YOUR_WALLS as T } from "../../wardyn/copy/personal-walls";
import { RECORDING_LEVEL } from "../../wardyn/copy/recording-level";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";
import { Chip } from "../../wardyn/primitives";

const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
const UNLISTED: Record<string, string> = { always_deny: T.UNLISTED_BLOCK, deny_with_review: T.UNLISTED_ASK, wait_for_review: T.UNLISTED_HOLD };

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[10rem_minmax(0,1fr)]">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="flex min-w-0 flex-col gap-1">{children}</dd>
    </div>
  );
}

const Codes = ({ list }: { list: string[] }) => (
  <span className="flex flex-wrap gap-1.5">
    {list.map((c) => (
      <Chip key={c} mono>
        {c}
      </Chip>
    ))}
  </span>
);

export function WallPresetDetail({ spec }: { spec: PersonalWallsSpec }) {
  const blocked = blockedTransfers(spec);
  const sites = spec.allowed_domains ? (spec.allowed_domains.length > 0 ? spec.allowed_domains.join(", ") : sentence(NR_WALLS.APPLY_SITES_SETUP_ONLY)) : undefined;
  return (
    <dl className="flex flex-col gap-2 border-t border-dashed border-border pt-2" data-testid="wall-preset-detail">
      <Row label={T.SITES}>{sites ?? W.DETAIL_NOT_NARROWED}</Row>
      {spec.denied_domains?.length ? <Row label={T.NEVER}>{spec.denied_domains.join(", ")}</Row> : null}
      <Row label={T.UNLISTED}>{(spec.first_use_approval && UNLISTED[spec.first_use_approval]) || W.DETAIL_NOT_NARROWED}</Row>
      {spec.min_confinement_class && <Row label={T.BARRIER}>{CC_META[spec.min_confinement_class]?.label ?? spec.min_confinement_class}</Row>}
      <Row label={T.CLIPBOARD}>{blocked.length > 0 ? NR_WALLS.APPLY_NO_TRANSFER(blocked.join(", ")) : W.DETAIL_NOT_NARROWED}</Row>
      <Row label={T.PUSHES}>
        {spec.push_rules?.deny_all === true || spec.push_rules?.deny_paths?.length ? (
          <>
            {spec.push_rules?.deny_all === true && (
              <>
                <strong className="font-semibold">{W.PUSH_EVERY_REFUSED}</strong>
                <span className="text-xs text-muted-foreground">{T.REFUSE_PUSH_HELP}</span>
              </>
            )}
            {spec.push_rules?.deny_paths?.length ? (
              <span className="flex flex-col gap-1">
                <span>{W.DETAIL_PATHS}:</span>
                <span className="break-all font-mono text-xs">{spec.push_rules.deny_paths.join(", ")}</span>
              </span>
            ) : null}
          </>
        ) : (
          W.DETAIL_NOT_NARROWED
        )}
      </Row>
      {spec.github_capabilities && (
        <Row label={W.DETAIL_GITHUB}>
          <Codes list={spec.github_capabilities} />
        </Row>
      )}
      {spec.azure_devops_capabilities && (
        <Row label={W.DETAIL_ADO}>
          <Codes list={spec.azure_devops_capabilities} />
        </Row>
      )}
      {spec.tool_rules?.length ? (
        <Row label={W.DETAIL_TOOL_RULES}>
          {spec.tool_rules.map((r) => (
            <span key={r.tool} className="break-all font-mono text-xs">
              {W.DETAIL_TOOL_RULE(r.tool, r.effect)}
            </span>
          ))}
        </Row>
      ) : null}
      {spec.recording && <Row label={W.DETAIL_RECORDING}>{RECORDING_LEVEL.LEVELS.find((l) => l.value === spec.recording)?.label ?? spec.recording}</Row>}
      {spec.auto_stop_after_sec !== undefined && <Row label={W.DETAIL_IDLE}>{idleLine(spec.auto_stop_after_sec)}</Row>}
    </dl>
  );
}

const idleLine = (sec: number): string => {
  const { n, unit } = idleDisplay(sec);
  return W.DETAIL_IDLE_AFTER(n, T.IDLE_UNIT(n, unit));
};
