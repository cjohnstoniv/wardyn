// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"log/slog"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// Self-register the OCI/Docker substrate so a blank import (cmd/wardynd under
// `-tags docker`) makes "docker" selectable via -runner/WARDYN_RUNNER. Compiled
// ONLY under the docker tag, so a tagless wardynd fails `-runner docker` closed
// at registry resolve ("not registered") and carries no target-specific code.
//
// Record is enabled so Exec wraps the agent argv with wardyn-rec (PTY session
// recording). WARDYN_RECORDING_MOUNT names a Docker volume (or absolute host
// path) shared between agent containers and wardynd's -recording-dir, where
// wardyn-rec delivers finished casts (single-host delivery only).
//
// SECURITY (HIGH-finding): that shared mount is the REDUCED-ISOLATION fallback
// delivery path — casts written to it are UNMASKED (secret masking lives
// control-plane-side, on the brokered upload path) and it has NO cross-run
// isolation (all agent containers share one uid). The driver prefers the masked
// brokered upload whenever a run token exists; the startup warning below is how
// an operator who sets it anyway learns the tradeoff.
func init() {
	substrate.Register("docker", func(d substrate.Deps) (substrate.Substrate, error) {
		recordingMount := os.Getenv("WARDYN_RECORDING_MOUNT")
		if recordingMount != "" {
			slog.Warn("wardynd: WARDYN_RECORDING_MOUNT is set — this is the reduced-isolation recording fallback: casts delivered via the shared mount are UNMASKED and have NO cross-run isolation. The masked brokered upload path is preferred whenever available; prefer leaving WARDYN_RECORDING_MOUNT unset for viewer-exposed recordings.",
				slog.String("recording_mount", recordingMount),
			)
		}
		// WARDYN_INTERNAL_NETWORK names the control-plane bridge the proxy sidecar
		// joins. Empty => withDefaults() keeps "wardyn-internal" (single-tenant
		// default, unchanged). A shared multi-job host sets a per-project name so
		// concurrent stacks don't share one network — it MUST match the compose
		// network's name (deploy/compose: both derive from WARDYN_NS).
		s, err := New(Config{
			ProxyImage:      d.ProxyImage,
			Record:          true,
			RecordingMount:  recordingMount,
			InternalNetwork: os.Getenv("WARDYN_INTERNAL_NETWORK"),
			// Fail closed by default when the host can't enforce resource caps;
			// WARDYN_ALLOW_UNENFORCEABLE_CAPS=1 (trusted host) downgrades to a warn.
			AllowUnenforceableCaps: os.Getenv("WARDYN_ALLOW_UNENFORCEABLE_CAPS") == "1",
			ConfinementRuntimes:    d.ConfinementRuntimes,
		})
		if err != nil {
			return nil, err
		}
		return s, nil // avoid the typed-nil interface trap
	})
}
