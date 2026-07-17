// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-scan is Wardyn's in-sandbox workspace scanner. It runs INSIDE
// a throwaway governed scan run (a later wave's launcher), walks the mounted
// workspace, and ships the raw ScanFacts back to the control plane so the
// profile is DERIVED control-plane-side (facts-out, not profile-out — the
// sandbox never carries authority; see internal/workspacescan DeriveProfile).
//
// Upload contract (mirrors wardyn-rec's brokered upload — the proxy injects the
// run token, so the sandbox NEVER holds it):
//
//	PUT  ${WARDYN_PROXY_URL}/wardyn/v1/scan-results/${WARDYN_RUN_ID}
//	Content-Type: application/json
//	body: json(workspacescan.ScanFacts)
//
// The wardyn-proxy local route /wardyn/v1/scan-results/ (handleBrokerScanResult)
// forwards this to POST-authenticated /api/v1/internal/scan-results/{runID} with
// the run token injected; the control plane rejects a runID that doesn't match
// the token's run (cross-run pollution guard). No Authorization header is set
// here on purpose — any sandbox-supplied one is stripped by the proxy.
//
// Env:
//
//	WARDYN_WORKSPACE_DIR  dir to scan (default /home/agent/work)
//	WARDYN_PROXY_URL      proxy base URL (required)
//	WARDYN_RUN_ID         this run's id (required)
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/sidecar"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// defaultWorkspaceDir matches the agent images' workspace mount point.
const defaultWorkspaceDir = "/home/agent/work"

func main() {
	if err := run(); err != nil {
		// Fail loud only on setup errors (missing required env). Delivery
		// failures are handled non-fatally inside run() (logged, exit 0) — a
		// scan that can't upload leaves the workspace in pending_scan, an honest
		// signal, without crashing the throwaway run.
		fmt.Fprintln(os.Stderr, "wardyn-scan:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := cliutil.EnvOr("WARDYN_WORKSPACE_DIR", defaultWorkspaceDir)
	url, err := sidecar.ProxyRunURL("scan")
	if err != nil {
		return err
	}

	// CollectFacts is bounded/read-only/never-errors (manifest-count + per-file
	// caps in internal/workspacescan), so the marshaled body is inherently small
	// — no separate size cap needed on the producing side.
	facts := workspacescan.CollectFacts(dir)
	body, err := json.Marshal(facts)
	if err != nil {
		return fmt.Errorf("marshal scan facts: %w", err)
	}

	if derr := sidecar.Upload(url, body); derr != nil {
		// Non-fatal (see main): the run already ran; a failed upload is a scan
		// problem, not a crash. Log it and exit 0 so the completion watcher tears
		// the throwaway run down cleanly.
		fmt.Fprintln(os.Stderr, "wardyn-scan: scan-result upload failed (non-fatal):", derr)
	}
	return nil
}
