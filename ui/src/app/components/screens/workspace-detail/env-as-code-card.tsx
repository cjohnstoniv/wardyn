/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Env as code — turn the scanned profile into committable files. "Generate
// files" fetches them fresh every time (GET /workspaces/{id}/env-as-code,
// never persisted client-side across profile/requirements changes — hence
// C.ENV_CAVEAT). A workspace with a local_dir source additionally gets
// "Write into the directory" (POST .../env-as-code/write, LOCAL-DIR ONLY
// server-side); a repo/ephemeral-only workspace has no host path, so it's
// told to copy-and-commit instead.
import * as React from "react";
import { FileCode2, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "../../ui/button";
import { Chip } from "../../wardyn/primitives";
import { CodeBlock } from "../../wardyn/code-block";
import { getErrorMessage } from "../../../lib/format";
import { workspaces as workspacesApi } from "../../../lib/api/workspaces";
import type { WorkspaceSourceInput } from "../../../lib/api/workspaces";
import type { Workspace } from "../../../lib/types";
import { C } from "../../../lib/workspace-copy";
import { SectionCard } from "./section-card";

// True when this workspace has somewhere on THIS host to write into — its
// legacy kind is local_dir, or (composed) at least one of its sources is.
// Mirrors the server's own gate (workspaceSourcesOfType(ws, LocalDir)) closely
// enough for a client-side show/hide decision; the server remains the real
// gate (a 422 there still surfaces if this guess is ever wrong).
function hasLocalDirSource(ws: Workspace): boolean {
  const sources = (ws as unknown as { sources?: WorkspaceSourceInput[] }).sources;
  if (sources && sources.length > 0) return sources.some((s) => s.type === "local_dir");
  return ws.kind === "local_dir";
}

export function EnvAsCodeCard({ ws }: { ws: Workspace }) {
  const [files, setFiles] = React.useState<Record<string, string> | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [writing, setWriting] = React.useState(false);
  const [written, setWritten] = React.useState(false);
  // Names a file writeEnvAsCode refused to overwrite (today, only ever
  // ".devcontainer/Dockerfile" when the operator already has one) — the
  // operator must learn it was PRESERVED, not silently overwritten or
  // silently dropped.
  const [skipped, setSkipped] = React.useState<string[]>([]);

  const generate = async () => {
    setLoading(true);
    setError(null);
    try {
      setFiles(await workspacesApi.getEnvAsCode(ws.id));
    } catch (e) {
      setError(getErrorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  const write = async () => {
    setWriting(true);
    try {
      const result = await workspacesApi.writeEnvAsCode(ws.id);
      setWritten(true);
      setSkipped(result.skipped);
      toast.success("Written into the directory", {
        description: result.skipped.length > 0 ? "Your existing .devcontainer/Dockerfile was left alone — see below." : undefined,
      });
    } catch (e) {
      toast.error("Failed to write env-as-code", { description: getErrorMessage(e) });
    } finally {
      setWriting(false);
    }
  };

  return (
    <SectionCard title="Env as code" subtitle="Turn the scanned profile into files you can commit.">
      {!files ? (
        <div className="flex flex-wrap items-center gap-3">
          <Button size="sm" variant="outline" onClick={() => void generate()} disabled={loading || !ws.profile}>
            {loading ? <Loader2 className="size-3.5 animate-spin" /> : <FileCode2 className="size-3.5" />}
            Generate files
          </Button>
          <p className="text-xs text-muted-foreground">
            {ws.profile
              ? "devcontainer.json (base image, language features, registry redirects) + AGENTS.md (detected recipe and declared services, as prose)."
              : "Needs a scanned profile — scan first."}
          </p>
          {/* UI-WS-12: this branch (files still null) is exactly where a
              FAILED generate leaves state — the `error` check below it was
              unreachable on a first failure, so the operator saw nothing at
              all, not even on a retry (a failure never sets `files`). */}
          {error && <p className="w-full text-sm text-danger">{error}</p>}
        </div>
      ) : error ? (
        <p className="text-sm text-danger">{error}</p>
      ) : (
        <div className="space-y-3">
          {Object.keys(files).length === 0 ? (
            <p className="text-sm text-muted-foreground">
              Nothing to emit — this workspace&apos;s profile produces no env-as-code files.
            </p>
          ) : (
            Object.entries(files).map(([name, content]) => (
              <div key={name} className="space-y-1.5">
                <div className="flex items-center gap-1.5 text-xs font-medium text-foreground">
                  <FileCode2 className="size-3.5 text-cyan" /> {name}
                </div>
                <CodeBlock text={content} />
              </div>
            ))
          )}
          <p className="text-[0.6875rem] leading-snug text-muted-foreground">{C.ENV_CAVEAT}</p>
          {hasLocalDirSource(ws) ? (
            <div className="flex items-center gap-2">
              <Button size="sm" onClick={() => void write()} disabled={writing}>
                {writing && <Loader2 className="size-3.5 animate-spin" />}
                Write into the directory
              </Button>
              {written && (
                <Chip tone="success" className="text-[0.6875rem]">
                  written
                </Chip>
              )}
            </div>
          ) : null}
          {written && skipped.length > 0 && (
            <p className="text-[0.6875rem] leading-snug text-warning">
              {skipped.includes(".devcontainer/Dockerfile")
                ? "Left your existing .devcontainer/Dockerfile alone — the devcontainer.json we wrote points build.dockerfile at it, so it won't include the agent CLI unless you add that yourself."
                : `Left ${skipped.join(", ")} alone — already existed.`}
            </p>
          )}
          {!hasLocalDirSource(ws) && (
            <p className="text-xs text-muted-foreground">Copy these and commit them — Wardyn doesn&apos;t push.</p>
          )}
        </div>
      )}
    </SectionCard>
  );
}
