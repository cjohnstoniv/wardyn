// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"log/slog"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// Self-registers the OCI/Docker substrate so a blank import makes "docker"
// selectable via -runner/WARDYN_RUNNER. Compiled ONLY under the docker tag, so
// a tagless wardynd fails `-runner docker` closed at registry resolve.
//
// Record follows the boot recording-store selection (Deps.Record): on, Exec
// wraps the agent argv with wardyn-rec for PTY recording, delivered to
// wardynd's -recording-dir via WARDYN_RECORDING_MOUNT (single-host only).
//
// SECURITY: that shared mount is the REDUCED-ISOLATION fallback delivery path
// — casts written to it are UNMASKED (masking happens control-plane-side, on
// the brokered upload path) and have NO cross-run isolation (all agent
// containers share one uid). The driver prefers the masked brokered upload
// whenever a run token exists; the startup warning below is how an operator
// who sets the mount anyway learns the tradeoff.
// buildConfig maps registration Deps and the resolved WARDYN_RECORDING_MOUNT
// to the docker driver's Config, kept pure so Record wiring is testable
// without a daemon: Deps.Record must reach Config.Record verbatim, never a
// hardcoded default.
func buildConfig(d substrate.Deps, recordingMount string) Config {
	return Config{
		ProxyImage:      d.ProxyImage,
		DriveProbeImage: d.DriveProbeImage,
		Record:          d.Record,
		RecordingMount:  recordingMount,
		InternalNetwork: os.Getenv("WARDYN_INTERNAL_NETWORK"),
		// SECURITY: fails closed by default when the host can't enforce resource
		// caps; WARDYN_ALLOW_UNENFORCEABLE_CAPS=true (trusted host) downgrades to
		// a warn. cliutil.EnvBool so a garbage value exits 2 at boot instead of
		// silently staying off.
		AllowUnenforceableCaps: cliutil.EnvBool("WARDYN_ALLOW_UNENFORCEABLE_CAPS", false),
		ConfinementRuntimes:    d.ConfinementRuntimes,
		// The deployment's host_path user-drive ceiling, parsed once at boot.
		UserDriveHostRoots: d.UserDriveHostRoots,
	}
}

func init() {
	substrate.Register("docker", func(d substrate.Deps) (substrate.Substrate, error) {
		recordingMount := os.Getenv("WARDYN_RECORDING_MOUNT")
		if recordingMount != "" {
			slog.Warn("wardynd: WARDYN_RECORDING_MOUNT is set — this is the reduced-isolation recording fallback: casts delivered via the shared mount are UNMASKED and have NO cross-run isolation. The masked brokered upload path is preferred whenever available; prefer leaving WARDYN_RECORDING_MOUNT unset for viewer-exposed recordings.",
				slog.String("recording_mount", recordingMount),
			)
		}
		// WARDYN_INTERNAL_NETWORK names the control-plane bridge the proxy sidecar
		// joins; empty keeps "wardyn-internal". A shared multi-job host sets a
		// per-project name so concurrent stacks don't share one network — it MUST
		// match the compose network's name (both derive from WARDYN_NS).
		s, err := New(buildConfig(d, recordingMount))
		if err != nil {
			return nil, err
		}
		// Pull images now, in the background, rather than make a live request pay
		// for the first pull.
		s.PrewarmImages()
		return s, nil // avoid the typed-nil interface trap
	})
}
