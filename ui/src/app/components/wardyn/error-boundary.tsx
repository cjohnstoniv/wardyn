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
}

interface State {
  error: Error | null;
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
    this.state = { error: null };
  }

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    // Surface to the console for debugging; intentionally not swallowed.
    // eslint-disable-next-line no-console
    console.error(
      `[wardyn] render error${this.props.region ? ` in ${this.props.region}` : ""}:`,
      error,
      info.componentStack,
    );
  }

  reset = () => this.setState({ error: null });

  render() {
    const { error } = this.state;
    if (error) {
      if (this.props.fallback) return this.props.fallback(error, this.reset);
      return (
        <div
          role="alert"
          className="m-4 rounded-lg border border-danger/25 bg-danger-subtle p-4 text-sm text-danger"
        >
          <div className="flex items-center gap-2 font-medium">
            <TriangleAlert className="size-4" />
            Something went wrong
            {this.props.region ? ` rendering ${this.props.region}` : ""}.
          </div>
          <p className="mt-1 text-danger/80">{error.message}</p>
          <button
            type="button"
            onClick={this.reset}
            className="mt-3 rounded-md border border-danger/30 px-2.5 py-1 text-xs font-medium hover:bg-danger/10"
          >
            Try again
          </button>
        </div>
      );
    }
    return this.props.children;
  }
}
