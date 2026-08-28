/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { TriangleAlert } from "lucide-react";

interface Props {
  children: React.ReactNode;
  // Optional label so a boundary can name the region it guards (e.g. "Runs").
  region?: string;
  // Optional custom fallback renderer.
  fallback?: (error: Error, reset: () => void) => React.ReactNode;
  // When this changes while the boundary is showing a caught error, the error
  // clears and the children mount fresh — e.g. a cockpit widget keyed on the
  // run id, so switching runs (or tabs) can't leave a stale crash from what
  // used to be here pinned on screen.
  resetKey?: string | number;
}

interface State {
  error: Error | null;
  resetKey?: string | number;
}

// A lazy-route chunk that fails to load almost always means the DEPLOYED BUILD
// CHANGED while this tab was open: the index.html in memory points at hashed
// chunks (assets/demos-step-<hash>.js) the server no longer has, so the first
// navigation to a not-yet-loaded route throws. Every console deploy does this to
// every open tab — and the generic "Try again" below cannot fix it, because
// resetting state re-runs the very same import against the very same missing
// file.
//
// The only real recovery is fetching the new index.html, so we do it once,
// automatically.
const CHUNK_ERROR_RE =
  /dynamically imported module|importing a module script failed|chunkloaderror|error loading dynamically imported/i;

function isStaleChunkError(error: Error): boolean {
  return CHUNK_ERROR_RE.test(error.message) || CHUNK_ERROR_RE.test(error.name);
}

// Self-limiting: at most one auto-reload per window, so a chunk error that
// SURVIVES the reload (a genuinely broken deploy, an offline server) falls
// through to the UI instead of pinning the browser in a refresh loop. No
// clean-up step to forget — the stamp simply ages out.
const RELOAD_STAMP_KEY = "wardyn-chunk-reload-at";
const RELOAD_COOLDOWN_MS = 10_000;

function reloadOnceForStaleChunk(): boolean {
  try {
    const last = Number(sessionStorage.getItem(RELOAD_STAMP_KEY) ?? 0);
    if (Date.now() - last < RELOAD_COOLDOWN_MS) return false;
    sessionStorage.setItem(RELOAD_STAMP_KEY, String(Date.now()));
  } catch {
    // Private mode / storage disabled: reloading without a stamp risks a loop,
    // so leave it to the operator and show the Reload button instead.
    return false;
  }
  window.location.reload();
  return true;
}

/**
 * ErrorBoundary stops a render-time exception in one subtree from unmounting
 * the entire React app. Before this existed, a single unmapped wire value
 * (e.g. a backend run state the UI did not know about) threw inside render and
 * blanked the whole console. Wrap screens/regions in this so a localized
 * failure degrades to an inline error card the operator can recover from.
 */
export class ErrorBoundary extends React.Component<Props, State> {
  constructor(props: Props) {
    super(props);
    this.state = { error: null, resetKey: props.resetKey };
  }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    if (props.resetKey === state.resetKey) return null;
    return { error: null, resetKey: props.resetKey };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    // Surface to the console for debugging; intentionally not swallowed.
    // eslint-disable-next-line no-console
    console.error(
      `[wardyn] render error${this.props.region ? ` in ${this.props.region}` : ""}:`,
      error,
      info.componentStack,
    );
    if (isStaleChunkError(error)) reloadOnceForStaleChunk();
  }

  reset = () => this.setState({ error: null });

  render() {
    const { error } = this.state;
    if (error) {
      if (this.props.fallback) return this.props.fallback(error, this.reset);
      // Reaching this branch for a chunk error means the auto-reload above
      // already ran (or storage refused it), so offer the action that CAN work
      // and drop the misleading "Try again", which would re-run the same import.
      const stale = isStaleChunkError(error);
      return (
        <div
          role="alert"
          className="m-4 rounded-lg border border-danger/25 bg-danger-subtle p-4 text-sm text-danger"
        >
          <div className="flex items-center gap-2 font-medium">
            <TriangleAlert className="size-4" />
            {stale ? (
              "Wardyn was updated while this tab was open."
            ) : (
              <>
                Something went wrong
                {this.props.region ? ` rendering ${this.props.region}` : ""}.
              </>
            )}
          </div>
          <p className="mt-1 text-danger/80">
            {stale
              ? "This page is running an older build whose files are no longer on the server. Reloading picks up the new one; nothing you have configured is affected."
              : error.message}
          </p>
          <button
            type="button"
            onClick={stale ? () => window.location.reload() : this.reset}
            className="mt-3 rounded-md border border-danger/30 px-2.5 py-1 text-xs font-medium hover:bg-danger/10"
          >
            {stale ? "Reload" : "Try again"}
          </button>
        </div>
      );
    }
    return this.props.children;
  }
}
