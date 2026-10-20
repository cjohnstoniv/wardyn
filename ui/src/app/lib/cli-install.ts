/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The card's "Get the CLI" lines (M-ATT-1; DESIGN-ATTACH §11.1, §11.10, §11.14).
// Pure functions of /healthz's `version` and `cli_artifact_base` so a vitest can
// pin every byte. The install script is a release asset, stamped with its own
// version; with an advertised mirror the same two assets are fetched from
// `<base>/v<version>/` and the mirror is passed back in WARDYN_ARTIFACT_BASE
// (the script reads it; `irm | iex` cannot take a flag).

export const CLI_RELEASE_BASE = "https://github.com/cjohnstoniv/wardyn/releases/download";

// A release build reports x.y.z (optionally a pre-release suffix). A source build
// reports x.y.z+<rev>[-dirty], which names no release asset, so it is not one.
const RELEASE_VERSION = /^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$/;
// The mirror is drawn into a shell line and a PowerShell line, so nothing that
// either would parse as syntax is let through (the daemon's boot check already
// requires https and no userinfo, query or fragment).
const SAFE_BASE = /^https:\/\/[A-Za-z0-9._~:/%+-]+$/;

export interface InstallLines {
  version: string;
  /** macOS, Linux and WSL2. */
  sh: string;
  /** Windows PowerShell. */
  ps: string;
  /** The lines fetch from the operator's mirror. */
  mirror: boolean;
}

/** null = the card cannot name a release (no version, a source build, or an unusable mirror). */
export function installLines(version: string | undefined, artifactBase: string | undefined): InstallLines | null {
  const m = RELEASE_VERSION.exec((version ?? "").trim());
  if (!m) return null;
  const v = m[1];
  const base = (artifactBase ?? "").trim().replace(/\/+$/, "");
  if (base === "") {
    const root = `${CLI_RELEASE_BASE}/v${v}`;
    return {
      version: v,
      sh: `curl -fsSL ${root}/install-cli.sh | sh`,
      ps: `irm ${root}/install-cli.ps1 | iex`,
      mirror: false,
    };
  }
  if (!SAFE_BASE.test(base)) return null;
  return {
    version: v,
    sh: `curl -fsSL ${base}/v${v}/install-cli.sh | WARDYN_ARTIFACT_BASE=${base} sh`,
    ps: `$env:WARDYN_ARTIFACT_BASE='${base}'; irm ${base}/v${v}/install-cli.ps1 | iex`,
    mirror: true,
  };
}

/** The viewer's OS picks which line leads; the other is folded. Anything but Windows leads with the sh line. */
export function viewerIsWindows(): boolean {
  return typeof navigator !== "undefined" && /Windows NT/i.test(navigator.userAgent);
}

export interface OrgCAFingerprint {
  /** The certificate's subject CN, when the operator published one. */
  subject?: string;
  fingerprint: string;
}

/**
 * /healthz.org_ca_fingerprints, in the order it lists them. The wire shape is
 * ATT-CA's (the Go field is not on main yet), so a plain string and an
 * `{subject, sha256|fingerprint}` object are both read; anything else is dropped.
 */
export function orgCAFingerprints(raw: unknown): OrgCAFingerprint[] {
  if (!Array.isArray(raw)) return [];
  const out: OrgCAFingerprint[] = [];
  for (const e of raw) {
    if (typeof e === "string" && e.trim() !== "") {
      out.push({ fingerprint: e.trim() });
    } else if (e && typeof e === "object") {
      const o = e as Record<string, unknown>;
      const fp = typeof o.sha256 === "string" ? o.sha256 : typeof o.fingerprint === "string" ? o.fingerprint : "";
      if (fp.trim() === "") continue;
      out.push({ fingerprint: fp.trim(), ...(typeof o.subject === "string" && o.subject !== "" ? { subject: o.subject } : {}) });
    }
  }
  return out;
}
